package proto

import (
	"bytes"
	"encoding/hex"
	"errors"
	"hash/crc32"
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

func TestChunkRoundTrip(t *testing.T) {
	payload := []byte("compressed-bytes")
	stream := AppendChunk(nil, payload, 4096)

	h, got, err := ReadChunk(bytes.NewReader(stream), 4096)
	if err != nil {
		t.Fatalf("ReadChunk: %v", err)
	}
	if h.Raw != 4096 {
		t.Fatalf("Raw: got %d, want 4096", h.Raw)
	}
	if !bytes.Equal(got, payload) {
		t.Fatalf("payload: got %q, want %q", got, payload)
	}
	if err := h.Verify(got); err != nil {
		t.Fatalf("Verify: %v", err)
	}
}

func TestChunkGolden(t *testing.T) {
	h := ChunkHeader{Compressed: 5, Raw: 4096, CRC32: 0x11223344}
	const want = "050000000010000044332211"
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
	payload := []byte("test")
	h := ChunkHeader{Compressed: uint32(len(payload)), Raw: 4, CRC32: crc32.ChecksumIEEE(payload)}
	if err := h.Verify(payload); err != nil {
		t.Fatalf("Verify on clean payload: %v", err)
	}

	corrupt := append([]byte(nil), payload...)
	corrupt[0] ^= 0xff
	if err := h.Verify(corrupt); err == nil {
		t.Fatal("Verify accepted a corrupted payload")
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
}
