package proto

import (
	"errors"
	"fmt"
	"hash/crc32"
	"math/bits"
)

// Bitmap marks which clusters of the source volume are stored in the image.
// Bit i (byte i/8, bit i%8) corresponds to cluster i. Padding bits past the
// last cluster are always zero.
type Bitmap struct {
	data   []byte
	blocks uint64
}

// NewBitmap allocates a zeroed bitmap for the given number of clusters.
func NewBitmap(blocks uint64) (Bitmap, error) {
	if blocks == 0 {
		return Bitmap{}, errors.New("proto: empty bitmap")
	}
	n := bitmapBytes(blocks)
	if n > uint64(maxInt) {
		return Bitmap{}, fmt.Errorf("proto: bitmap of %d blocks is too large", blocks)
	}
	return Bitmap{data: make([]byte, int(n)), blocks: blocks}, nil
}

// ParseBitmap wraps data as a bitmap of the given number of clusters without
// copying it. It rejects a wrong length and set padding bits.
func ParseBitmap(data []byte, blocks uint64) (Bitmap, error) {
	if blocks == 0 {
		return Bitmap{}, errors.New("proto: empty bitmap")
	}
	if want := bitmapBytes(blocks); uint64(len(data)) != want {
		return Bitmap{}, fmt.Errorf("proto: bitmap is %d bytes, want %d", len(data), want)
	}
	if r := blocks % 8; r != 0 && data[len(data)-1]>>r != 0 {
		return Bitmap{}, errors.New("proto: bitmap has padding bits set")
	}
	return Bitmap{data: data, blocks: blocks}, nil
}

// bitmapBytes returns the bytes needed for blocks bits, without overflowing.
func bitmapBytes(blocks uint64) uint64 {
	n := blocks / 8
	if blocks%8 != 0 {
		n++
	}
	return n
}

// Blocks returns how many clusters the bitmap describes.
func (b Bitmap) Blocks() uint64 { return b.blocks }

// Bytes returns the encoded bitmap. The slice is shared with b.
func (b Bitmap) Bytes() []byte { return b.data }

// CRC32 returns the checksum stored in Header.BitmapCRC32.
func (b Bitmap) CRC32() uint32 { return crc32.ChecksumIEEE(b.data) }

// Has reports whether cluster i is marked as stored.
func (b Bitmap) Has(i uint64) bool {
	if i >= b.blocks {
		return false
	}
	return b.data[i/8]&(1<<(i%8)) != 0
}

// Set marks cluster i as stored. It panics on an out-of-range index, which is
// a programming error rather than bad input.
func (b Bitmap) Set(i uint64) {
	if i >= b.blocks {
		panic(fmt.Sprintf("proto: bitmap index %d out of range [0, %d)", i, b.blocks))
	}
	b.data[i/8] |= 1 << (i % 8)
}

// Count returns the number of clusters marked as stored.
func (b Bitmap) Count() uint64 {
	var n uint64
	for _, v := range b.data {
		n += uint64(bits.OnesCount8(v))
	}
	return n
}
