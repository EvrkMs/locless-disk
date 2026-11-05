package proto

import (
	"encoding/binary"
	"encoding/hex"
	"errors"
	"hash/crc32"
	"testing"
)

func testHeader() Header {
	return Header{
		Compression: CompressionZstd,
		ClusterSize: 4096,
		ChunkSize:   1 << 20,
		BlockCount:  1000,
		UsedBlocks:  900,
		DeviceSize:  4096000,
	}
}

func TestHeaderRoundTrip(t *testing.T) {
	h := testHeader()

	b, err := h.MarshalBinary()
	if err != nil {
		t.Fatalf("MarshalBinary: %v", err)
	}
	if len(b) != HeaderSize {
		t.Fatalf("encoded %d bytes, want %d", len(b), HeaderSize)
	}

	got, err := ParseHeader(b)
	if err != nil {
		t.Fatalf("ParseHeader: %v", err)
	}
	if got != h {
		t.Fatalf("round trip mismatch:\n got %+v\nwant %+v", got, h)
	}
}

// TestHeaderGolden pins the byte layout so a format change cannot slip in
// unnoticed: these bytes are the on-disk v1 format.
func TestHeaderGolden(t *testing.T) {
	b, err := testHeader().MarshalBinary()
	if err != nil {
		t.Fatalf("MarshalBinary: %v", err)
	}

	const wantBody = "464f4753010001000010000000001000e803000000000000840300000000000000803e0000000000"
	if got := hex.EncodeToString(b[:40]); got != wantBody {
		t.Fatalf("header body:\n got %s\nwant %s", got, wantBody)
	}
	if got, want := binary.LittleEndian.Uint32(b[40:]), crc32.ChecksumIEEE(b[:40]); got != want {
		t.Fatalf("header crc: got %#08x, want %#08x", got, want)
	}
}

func TestParseHeaderRejects(t *testing.T) {
	valid := func(t *testing.T) []byte {
		t.Helper()
		b, err := testHeader().MarshalBinary()
		if err != nil {
			t.Fatalf("MarshalBinary: %v", err)
		}
		return b
	}

	tests := []struct {
		name    string
		mutate  func([]byte) []byte
		wantErr error
	}{
		{
			name:    "short",
			mutate:  func(b []byte) []byte { return b[:HeaderSize-1] },
			wantErr: ErrShortHeader,
		},
		{
			name: "bad magic",
			mutate: func(b []byte) []byte {
				b[0] = 'X'
				return reseal(b)
			},
			wantErr: ErrBadMagic,
		},
		{
			name: "bad crc",
			mutate: func(b []byte) []byte {
				b[20] ^= 0xff
				return b
			},
			wantErr: ErrBadHeaderCRC,
		},
		{
			name: "unsupported version",
			mutate: func(b []byte) []byte {
				binary.LittleEndian.PutUint16(b[4:], Version+1)
				return reseal(b)
			},
			wantErr: ErrUnsupportedVersion,
		},
		{
			name: "invalid values",
			mutate: func(b []byte) []byte {
				binary.LittleEndian.PutUint64(b[24:], 2000) // used > total
				return reseal(b)
			},
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			_, err := ParseHeader(tc.mutate(valid(t)))
			if err == nil {
				t.Fatal("ParseHeader accepted a corrupt header")
			}
			if tc.wantErr != nil && !errors.Is(err, tc.wantErr) {
				t.Fatalf("got %v, want %v", err, tc.wantErr)
			}
		})
	}
}

func TestHeaderValidate(t *testing.T) {
	tests := []struct {
		name   string
		mutate func(*Header)
	}{
		{"unknown compression", func(h *Header) { h.Compression = 42 }},
		{"cluster too small", func(h *Header) { h.ClusterSize = 256 }},
		{"cluster not power of two", func(h *Header) { h.ClusterSize = 3000 }},
		{"chunk smaller than cluster", func(h *Header) { h.ChunkSize = 1024 }},
		{"chunk not multiple of cluster", func(h *Header) { h.ChunkSize = 1<<20 + 1 }},
		{"no blocks", func(h *Header) { h.BlockCount = 0 }},
		{"used over total", func(h *Header) { h.UsedBlocks = h.BlockCount + 1 }},
		{"no device size", func(h *Header) { h.DeviceSize = 0 }},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			h := testHeader()
			tc.mutate(&h)
			if err := h.Validate(); err == nil {
				t.Fatal("Validate accepted an invalid header")
			}
		})
	}
}

func TestBitmapLen(t *testing.T) {
	tests := []struct {
		blocks uint64
		want   int
	}{
		{1, 1},
		{7, 1},
		{8, 1},
		{9, 2},
		{1000, 125},
	}
	for _, tc := range tests {
		h := testHeader()
		h.BlockCount = tc.blocks
		got, err := h.BitmapLen()
		if err != nil {
			t.Fatalf("%d blocks: %v", tc.blocks, err)
		}
		if got != tc.want {
			t.Fatalf("%d blocks: got %d bytes, want %d", tc.blocks, got, tc.want)
		}
	}
}

// reseal recomputes the header checksum after a test mutated the body.
func reseal(b []byte) []byte {
	binary.LittleEndian.PutUint32(b[40:], crc32.ChecksumIEEE(b[:40]))
	return b
}
