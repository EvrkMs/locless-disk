package proto

import (
	"fmt"
	"math/bits"
)

// Bitmap marks which clusters of the source volume are stored in the image.
// Bit i (byte i/8, bit i%8) corresponds to cluster i.
type Bitmap []byte

// NewBitmap allocates a zeroed bitmap for the given number of clusters.
func NewBitmap(blocks uint64) (Bitmap, error) {
	if blocks == 0 {
		return nil, fmt.Errorf("proto: empty bitmap")
	}
	n := (blocks + 7) / 8
	if n > uint64(maxInt) {
		return nil, fmt.Errorf("proto: bitmap of %d blocks is too large", blocks)
	}
	return make(Bitmap, int(n)), nil
}

// Blocks returns how many clusters the bitmap can describe.
func (b Bitmap) Blocks() uint64 { return uint64(len(b)) * 8 }

// Has reports whether cluster i is marked as stored.
func (b Bitmap) Has(i uint64) bool {
	if i >= b.Blocks() {
		return false
	}
	return b[i/8]&(1<<(i%8)) != 0
}

// Set marks cluster i as stored. It panics on an out-of-range index, which is
// a programming error rather than bad input.
func (b Bitmap) Set(i uint64) {
	if i >= b.Blocks() {
		panic(fmt.Sprintf("proto: bitmap index %d out of range [0, %d)", i, b.Blocks()))
	}
	b[i/8] |= 1 << (i % 8)
}

// Count returns the number of clusters marked as stored.
func (b Bitmap) Count() uint64 {
	var n uint64
	for _, v := range b {
		n += uint64(bits.OnesCount8(v))
	}
	return n
}
