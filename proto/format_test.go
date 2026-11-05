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
		BitmapCRC32: 0xAABBCCDD,
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

	const wantBody = "464f4753010001000010000000001000e803000000000000840300000000000000803e0000000000ddccbbaa"
	if got := hex.EncodeToString(b[:headerBodySize]); got != wantBody {
		t.Fatalf("header body:\n got %s\nwant %s", got, wantBody)
	}
	if got, want := binary.LittleEndian.Uint32(b[headerBodySize:]), crc32.ChecksumIEEE(b[:headerBodySize]); got != want {
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
		{"cluster too large", func(h *Header) { h.ClusterSize = 4 << 20; h.ChunkSize = 4 << 20; h.DeviceSize = 1 << 40 }},
		{"cluster not power of two", func(h *Header) { h.ClusterSize = 3000 }},
		{"chunk smaller than cluster", func(h *Header) { h.ChunkSize = 1024 }},
		{"chunk not multiple of cluster", func(h *Header) { h.ChunkSize = 1<<20 + 1 }},
		{"no blocks", func(h *Header) { h.BlockCount = 0 }},
		{"used over total", func(h *Header) { h.UsedBlocks = h.BlockCount + 1 }},
		{"no device size", func(h *Header) { h.DeviceSize = 0 }},
		{"device smaller than clusters", func(h *Header) { h.DeviceSize = 4095999 }},
		{"cluster bytes overflow", func(h *Header) { h.BlockCount = 1 << 62 }},
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

func TestHeaderValidateAccepts(t *testing.T) {
	tests := []struct {
		name   string
		mutate func(*Header)
	}{
		{"device larger than clusters", func(h *Header) { h.DeviceSize += 512 }},
		{"2 MiB clusters", func(h *Header) { h.ClusterSize = 2 << 20; h.ChunkSize = 4 << 20; h.DeviceSize = 1 << 40 }},
		{"nothing stored", func(h *Header) { h.UsedBlocks = 0 }},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			h := testHeader()
			tc.mutate(&h)
			if err := h.Validate(); err != nil {
				t.Fatalf("Validate: %v", err)
			}
		})
	}
}

// bitmapFixture returns a 1001-block bitmap and a header sealed for it. The
// block count leaves padding bits in the last byte.
func bitmapFixture(t *testing.T) (Header, []byte) {
	t.Helper()
	b, err := NewBitmap(1001)
	if err != nil {
		t.Fatalf("NewBitmap: %v", err)
	}
	b.Set(0)
	b.Set(1000)

	h := testHeader()
	h.DeviceSize = 1001 * 4096
	h.SetBitmap(b)
	if err := h.Validate(); err != nil {
		t.Fatalf("Validate: %v", err)
	}
	return h, b.Bytes()
}

func TestHeaderParseBitmap(t *testing.T) {
	h, data := bitmapFixture(t)
	if h.BlockCount != 1001 || h.UsedBlocks != 2 {
		t.Fatalf("SetBitmap: got %d blocks, %d used", h.BlockCount, h.UsedBlocks)
	}
	b, err := h.ParseBitmap(data)
	if err != nil {
		t.Fatalf("ParseBitmap: %v", err)
	}
	if !b.Has(1000) || b.Has(999) {
		t.Fatal("parsed bitmap lost its contents")
	}
}

func TestHeaderParseBitmapRejects(t *testing.T) {
	t.Run("flipped bit", func(t *testing.T) {
		h, data := bitmapFixture(t)
		data[10] ^= 0x01
		if _, err := h.ParseBitmap(data); !errors.Is(err, ErrBadBitmapCRC) {
			t.Fatalf("got %v, want %v", err, ErrBadBitmapCRC)
		}
	})
	t.Run("truncated", func(t *testing.T) {
		h, data := bitmapFixture(t)
		if _, err := h.ParseBitmap(data[:len(data)-1]); err == nil {
			t.Fatal("ParseBitmap accepted a truncated bitmap")
		}
	})
	t.Run("wrong length with matching crc", func(t *testing.T) {
		h, data := bitmapFixture(t)
		short := data[:len(data)-1]
		h.BitmapCRC32 = crc32.ChecksumIEEE(short)
		if _, err := h.ParseBitmap(short); err == nil {
			t.Fatal("ParseBitmap accepted a bitmap of the wrong length")
		}
	})
	t.Run("count differs from header", func(t *testing.T) {
		h, data := bitmapFixture(t)
		h.UsedBlocks++
		if _, err := h.ParseBitmap(data); err == nil {
			t.Fatal("ParseBitmap accepted a wrong used block count")
		}
	})
	t.Run("padding bit with matching crc", func(t *testing.T) {
		h, data := bitmapFixture(t)
		data[len(data)-1] |= 0x80
		h.BitmapCRC32 = crc32.ChecksumIEEE(data)
		if _, err := h.ParseBitmap(data); err == nil {
			t.Fatal("ParseBitmap accepted a set padding bit")
		}
	})
}

func TestChunkCount(t *testing.T) {
	tests := []struct {
		used uint64
		want uint64
	}{
		{0, 0},
		{1, 1},
		{256, 1}, // exactly one 1 MiB chunk of 4 KiB clusters
		{257, 2},
		{900, 4},
	}
	for _, tc := range tests {
		h := testHeader()
		h.UsedBlocks = tc.used
		if got := h.ChunkCount(); got != tc.want {
			t.Fatalf("%d used blocks: got %d chunks, want %d", tc.used, got, tc.want)
		}
	}
}

func TestCheckChunk(t *testing.T) {
	const (
		full = 1 << 20
		tail = 900*4096 - 3*full // the last of 4 chunks
	)

	tests := []struct {
		name    string
		c       ChunkHeader
		wantErr bool
	}{
		{"first", ChunkHeader{Index: 0, Raw: full}, false},
		{"last", ChunkHeader{Index: 3, Raw: tail}, false},
		{"index past end", ChunkHeader{Index: 4, Raw: full}, true},
		{"short middle chunk", ChunkHeader{Index: 1, Raw: tail}, true},
		{"last chunk too long", ChunkHeader{Index: 3, Raw: full}, true},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			err := testHeader().CheckChunk(tc.c)
			if (err != nil) != tc.wantErr {
				t.Fatalf("got %v, want error: %v", err, tc.wantErr)
			}
		})
	}
}

func TestCheckChunkEdges(t *testing.T) {
	h := testHeader()

	h.UsedBlocks = 512 // two full chunks: the last one is not short
	if err := h.CheckChunk(ChunkHeader{Index: 1, Raw: 1 << 20}); err != nil {
		t.Fatalf("full last chunk: %v", err)
	}

	h.UsedBlocks = 0
	if err := h.CheckChunk(ChunkHeader{}); err == nil {
		t.Fatal("CheckChunk accepted a chunk in an empty image")
	}

	h = testHeader()
	h.Compression = CompressionNone
	if err := h.CheckChunk(ChunkHeader{Index: 0, Raw: 1 << 20, Compressed: 1 << 20}); err != nil {
		t.Fatalf("stored chunk: %v", err)
	}
	if err := h.CheckChunk(ChunkHeader{Index: 0, Raw: 1 << 20, Compressed: 1}); err == nil {
		t.Fatal("CheckChunk accepted a stored chunk with a wrong payload size")
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
	binary.LittleEndian.PutUint32(b[headerBodySize:], crc32.ChecksumIEEE(b[:headerBodySize]))
	return b
}
