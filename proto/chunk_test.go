package proto

import (
	"bytes"
	"encoding/hex"
	"errors"
	"io"
	"testing"
)

func TestBitmapSetHasCount(t *testing.T) {
	b, err := NewBitmap(1000)
	if err != nil {
		t.Fatalf("NewBitmap: %v", err)
	}
	if got := b.Blocks(); got != 1000 {
		t.Fatalf("Blocks: got %d, want 1000", got)
	}
	for _, i := range []uint64{0, 1, 7, 8, 999} {
		b.Set(i)
	}
	for _, i := range []uint64{0, 1, 7, 8, 999} {
		if !b.Has(i) {
			t.Fatalf("Has(%d) = false after Set", i)
		}
	}
	if b.Has(1000) {
		t.Fatal("Has(1000) = true on a 1000-block bitmap")
	}
	if got := b.Count(); got != 5 {
		t.Fatalf("Count: got %d, want 5", got)
	}
}

// TestBitmapKeepsExactSize uses a block count that is not a multiple of 8: the
// padding bits of the last byte must not count as clusters.
func TestBitmapKeepsExactSize(t *testing.T) {
	b, err := NewBitmap(1001)
	if err != nil {
		t.Fatalf("NewBitmap: %v", err)
	}
	if got := b.Blocks(); got != 1001 {
		t.Fatalf("Blocks: got %d, want 1001", got)
	}
	if got := len(b.Bytes()); got != 126 {
		t.Fatalf("Bytes: got %d bytes, want 126", got)
	}
	b.Set(1000)
	defer func() {
		if recover() == nil {
			t.Fatal("Set(1001) did not panic on a 1001-block bitmap")
		}
	}()
	b.Set(1001)
}

func TestBitmapSetPanicsOutOfRange(t *testing.T) {
	b, err := NewBitmap(8)
	if err != nil {
		t.Fatalf("NewBitmap: %v", err)
	}
	defer func() {
		if recover() == nil {
			t.Fatal("Set(8) did not panic on an 8-block bitmap")
		}
	}()
	b.Set(8)
}

func TestNewBitmapRejectsZero(t *testing.T) {
	if _, err := NewBitmap(0); err == nil {
		t.Fatal("NewBitmap(0) returned no error")
	}
}

func TestParseBitmap(t *testing.T) {
	b, err := ParseBitmap([]byte{0xff, 0x01}, 9)
	if err != nil {
		t.Fatalf("ParseBitmap: %v", err)
	}
	if got := b.Count(); got != 9 {
		t.Fatalf("Count: got %d, want 9", got)
	}

	tests := []struct {
		name   string
		data   []byte
		blocks uint64
	}{
		{"no blocks", nil, 0},
		{"too short", []byte{0xff}, 9},
		{"too long", []byte{0xff, 0x01, 0x00}, 9},
		{"padding set", []byte{0xff, 0x03}, 9},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			if _, err := ParseBitmap(tc.data, tc.blocks); err == nil {
				t.Fatal("ParseBitmap accepted a bad bitmap")
			}
		})
	}
}

func TestChunkRoundTrip(t *testing.T) {
	payload := []byte("compressed-bytes")
	stream := AppendChunk(nil, payload, 7, 4096)

	h, got, err := ReadChunk(bytes.NewReader(stream), 4096)
	if err != nil {
		t.Fatalf("ReadChunk: %v", err)
	}
	if h.Raw != 4096 {
		t.Fatalf("Raw: got %d, want 4096", h.Raw)
	}
	if h.Index != 7 {
		t.Fatalf("Index: got %d, want 7", h.Index)
	}
	if !bytes.Equal(got, payload) {
		t.Fatalf("payload: got %q, want %q", got, payload)
	}
	if err := h.Verify(got); err != nil {
		t.Fatalf("Verify: %v", err)
	}
}

func TestChunkGolden(t *testing.T) {
	h := ChunkHeader{Compressed: 5, Raw: 4096, Index: 7, CRC32: 0x11223344}
	const want = "05000000001000000700000000000000" + "44332211"
	if got := hex.EncodeToString(h.MarshalBinary()); got != want {
		t.Fatalf("chunk header:\n got %s\nwant %s", got, want)
	}

	parsed, err := ParseChunkHeader(h.MarshalBinary())
	if err != nil {
		t.Fatalf("ParseChunkHeader: %v", err)
	}
	if parsed != h {
		t.Fatalf("round trip mismatch: got %+v, want %+v", parsed, h)
	}
}

func TestChunkVerifyRejectsCorruption(t *testing.T) {
	stream := AppendChunk(nil, []byte("test"), 3, 4)
	h, payload, err := ReadChunk(bytes.NewReader(stream), 4)
	if err != nil {
		t.Fatalf("ReadChunk: %v", err)
	}
	if err := h.Verify(payload); err != nil {
		t.Fatalf("Verify on clean payload: %v", err)
	}

	corrupt := append([]byte(nil), payload...)
	corrupt[0] ^= 0xff
	if err := h.Verify(corrupt); err == nil {
		t.Fatal("Verify accepted a corrupted payload")
	}

	wrongIndex := h
	wrongIndex.Index++
	if err := wrongIndex.Verify(payload); err == nil {
		t.Fatal("Verify accepted a corrupted index")
	}

	wrongRaw := h
	wrongRaw.Raw++
	if err := wrongRaw.Verify(payload); err == nil {
		t.Fatal("Verify accepted a corrupted raw size")
	}
}

func TestReadChunkBoundsHeader(t *testing.T) {
	const maxRaw = 1 << 20

	tests := []struct {
		name string
		h    ChunkHeader
	}{
		{"raw above limit", ChunkHeader{Compressed: 4, Raw: maxRaw + 1, CRC32: 0}},
		{"payload above limit", ChunkHeader{Compressed: maxRaw + chunkOverhead + 1, Raw: 4, CRC32: 0}},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			_, _, err := ReadChunk(bytes.NewReader(tc.h.MarshalBinary()), maxRaw)
			if err == nil {
				t.Fatal("ReadChunk accepted an oversized header")
			}
		})
	}
}

func TestReadChunkTruncated(t *testing.T) {
	if _, _, err := ReadChunk(bytes.NewReader(nil), 1024); !errors.Is(err, io.EOF) {
		t.Fatalf("got %v, want io.EOF", err)
	}

	stream := AppendChunk(nil, []byte("test"), 0, 4)
	_, _, err := ReadChunk(bytes.NewReader(stream[:len(stream)-1]), 1024)
	if !errors.Is(err, io.ErrUnexpectedEOF) {
		t.Fatalf("got %v, want io.ErrUnexpectedEOF", err)
	}
}
