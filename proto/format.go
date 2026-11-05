// Package proto defines the image format and the wire protocol shared by the
// FOGs server and the WinPE agent.
//
// A disk image is a directory holding a JSON manifest (see Manifest) with the
// partition table, and one image file per stored partition.
//
// An image file is a fixed 48-byte header, a bitmap of occupied clusters and a
// sequence of independent compressed chunks:
//
//	magic(4) version(2) compression(2) clusterSize(4) chunkSize(4)
//	blockCount(8) usedBlocks(8) deviceSize(8) bitmapCRC32(4) headerCRC32(4)
//	bitmap(ceil(blockCount/8) bytes, bit i = block i is stored)
//	chunk{ compLen(4) rawLen(4) index(8) crc32(4) payload(compLen) } ...
//
// All integers are little-endian. Every chunk decompresses on its own, so
// chunks can be compressed in parallel and sent independently over multicast.
//
// The stored clusters, taken in bitmap order and concatenated, form one data
// stream cut into ChunkSize pieces; only the last piece may be shorter. Chunk
// index i therefore holds stream bytes [i*ChunkSize, (i+1)*ChunkSize), which
// lets a receiver place a chunk, or ask for a lost one, by its index alone.
package proto

import (
	"encoding/binary"
	"errors"
	"fmt"
	"hash/crc32"
	"math/bits"
)

// Magic is "FOGS" read as a little-endian uint32.
const Magic uint32 = 0x53474F46

// Version is the current format version.
const Version uint16 = 1

// HeaderSize is the exact wire size of Header in bytes.
const HeaderSize = 48

// headerBodySize is the part of the header covered by its checksum.
const headerBodySize = HeaderSize - 4

// Limits for the values stored in Header.
const (
	MinClusterSize = 512
	MaxClusterSize = 2 << 20
	MaxChunkSize   = 64 << 20
)

// Compression identifies the codec used for every chunk of an image.
type Compression uint16

const (
	CompressionNone Compression = 0
	CompressionZstd Compression = 1
)

// ErrShortHeader is returned when fewer than HeaderSize bytes are available.
var ErrShortHeader = errors.New("proto: short header")

// ErrBadMagic is returned when the file is not a FOGs image.
var ErrBadMagic = errors.New("proto: bad magic")

// ErrBadHeaderCRC is returned when the header checksum does not match.
var ErrBadHeaderCRC = errors.New("proto: bad header crc")

// ErrUnsupportedVersion is returned for a format version this build cannot read.
var ErrUnsupportedVersion = errors.New("proto: unsupported version")

// ErrBadBitmapCRC is returned when the bitmap checksum does not match.
var ErrBadBitmapCRC = errors.New("proto: bad bitmap crc")

// Header is the fixed part of an image file.
//
// BlockCount clusters are described by the bitmap; UsedBlocks of them are
// actually stored and appear as chunks in the file.
type Header struct {
	Compression Compression // codec of every chunk
	ClusterSize uint32      // bytes per cluster on the source volume
	ChunkSize   uint32      // uncompressed bytes per chunk
	BlockCount  uint64      // clusters on the source volume
	UsedBlocks  uint64      // clusters stored in the image
	DeviceSize  uint64      // source volume size in bytes
	BitmapCRC32 uint32      // checksum of the encoded bitmap
}

// Validate reports whether the header describes a readable image.
func (h Header) Validate() error {
	switch h.Compression {
	case CompressionNone, CompressionZstd:
	default:
		return fmt.Errorf("proto: unknown compression %d", h.Compression)
	}
	if h.ClusterSize < MinClusterSize || h.ClusterSize > MaxClusterSize {
		return fmt.Errorf("proto: cluster size %d out of range [%d, %d]", h.ClusterSize, MinClusterSize, MaxClusterSize)
	}
	if h.ClusterSize&(h.ClusterSize-1) != 0 {
		return fmt.Errorf("proto: cluster size %d is not a power of two", h.ClusterSize)
	}
	if h.ChunkSize < h.ClusterSize || h.ChunkSize > MaxChunkSize {
		return fmt.Errorf("proto: chunk size %d out of range [%d, %d]", h.ChunkSize, h.ClusterSize, MaxChunkSize)
	}
	if h.ChunkSize%h.ClusterSize != 0 {
		return fmt.Errorf("proto: chunk size %d is not a multiple of cluster size %d", h.ChunkSize, h.ClusterSize)
	}
	if h.BlockCount == 0 {
		return errors.New("proto: block count is zero")
	}
	if h.UsedBlocks > h.BlockCount {
		return fmt.Errorf("proto: used blocks %d exceed block count %d", h.UsedBlocks, h.BlockCount)
	}
	if h.DeviceSize == 0 {
		return errors.New("proto: device size is zero")
	}
	// The device may be larger than its clusters: NTFS keeps a backup boot
	// sector past the last cluster. It can never be smaller.
	if hi, lo := bits.Mul64(h.BlockCount, uint64(h.ClusterSize)); hi != 0 || lo > h.DeviceSize {
		return fmt.Errorf("proto: %d clusters of %d bytes exceed device size %d", h.BlockCount, h.ClusterSize, h.DeviceSize)
	}
	return nil
}

// BitmapLen returns the number of bytes the block bitmap occupies.
func (h Header) BitmapLen() (int, error) {
	n := bitmapBytes(h.BlockCount)
	if n > uint64(maxInt) {
		return 0, fmt.Errorf("proto: bitmap of %d blocks is too large", h.BlockCount)
	}
	return int(n), nil
}

// SetBitmap records the block counts and the checksum of b in the header.
func (h *Header) SetBitmap(b Bitmap) {
	h.BlockCount = b.Blocks()
	h.UsedBlocks = b.Count()
	h.BitmapCRC32 = b.CRC32()
}

// ParseBitmap verifies an encoded bitmap against the header and wraps it
// without copying. Writing clusters from a damaged bitmap would put data at
// the wrong disk offsets, so nothing may be restored until this succeeds.
func (h Header) ParseBitmap(data []byte) (Bitmap, error) {
	if got := crc32.ChecksumIEEE(data); got != h.BitmapCRC32 {
		return Bitmap{}, fmt.Errorf("%w: %#08x != %#08x", ErrBadBitmapCRC, got, h.BitmapCRC32)
	}
	b, err := ParseBitmap(data, h.BlockCount)
	if err != nil {
		return Bitmap{}, err
	}
	if n := b.Count(); n != h.UsedBlocks {
		return Bitmap{}, fmt.Errorf("proto: bitmap marks %d blocks, header says %d", n, h.UsedBlocks)
	}
	return b, nil
}

// ChunkCount returns how many chunks the image holds. The header must be valid.
func (h Header) ChunkCount() uint64 {
	used := h.UsedBlocks * uint64(h.ClusterSize)
	n := used / uint64(h.ChunkSize)
	if used%uint64(h.ChunkSize) != 0 {
		n++
	}
	return n
}

// CheckChunk reports whether a chunk header fits this image: its index must be
// in range and its raw size must match that position in the data stream.
func (h Header) CheckChunk(c ChunkHeader) error {
	n := h.ChunkCount()
	if c.Index >= n {
		return fmt.Errorf("proto: chunk index %d out of range [0, %d)", c.Index, n)
	}
	want := h.ChunkSize
	if c.Index == n-1 {
		if tail := h.UsedBlocks * uint64(h.ClusterSize) % uint64(h.ChunkSize); tail != 0 {
			want = uint32(tail)
		}
	}
	if c.Raw != want {
		return fmt.Errorf("proto: chunk %d raw size is %d, want %d", c.Index, c.Raw, want)
	}
	if h.Compression == CompressionNone && c.Compressed != c.Raw {
		return fmt.Errorf("proto: uncompressed chunk %d stores %d bytes, want %d", c.Index, c.Compressed, c.Raw)
	}
	return nil
}

// MarshalBinary encodes the header with its checksum.
func (h Header) MarshalBinary() ([]byte, error) {
	if err := h.Validate(); err != nil {
		return nil, err
	}
	b := make([]byte, HeaderSize)
	binary.LittleEndian.PutUint32(b[0:], Magic)
	binary.LittleEndian.PutUint16(b[4:], Version)
	binary.LittleEndian.PutUint16(b[6:], uint16(h.Compression))
	binary.LittleEndian.PutUint32(b[8:], h.ClusterSize)
	binary.LittleEndian.PutUint32(b[12:], h.ChunkSize)
	binary.LittleEndian.PutUint64(b[16:], h.BlockCount)
	binary.LittleEndian.PutUint64(b[24:], h.UsedBlocks)
	binary.LittleEndian.PutUint64(b[32:], h.DeviceSize)
	binary.LittleEndian.PutUint32(b[40:], h.BitmapCRC32)
	binary.LittleEndian.PutUint32(b[headerBodySize:], crc32.ChecksumIEEE(b[:headerBodySize]))
	return b, nil
}

// ParseHeader decodes and validates a header, checksum included.
func ParseHeader(b []byte) (Header, error) {
	if len(b) < HeaderSize {
		return Header{}, ErrShortHeader
	}
	if got := binary.LittleEndian.Uint32(b[0:]); got != Magic {
		return Header{}, fmt.Errorf("%w: %#08x", ErrBadMagic, got)
	}
	if got, want := binary.LittleEndian.Uint32(b[headerBodySize:]), crc32.ChecksumIEEE(b[:headerBodySize]); got != want {
		return Header{}, fmt.Errorf("%w: %#08x != %#08x", ErrBadHeaderCRC, got, want)
	}
	if v := binary.LittleEndian.Uint16(b[4:]); v != Version {
		return Header{}, fmt.Errorf("%w: %d", ErrUnsupportedVersion, v)
	}
	h := Header{
		Compression: Compression(binary.LittleEndian.Uint16(b[6:])),
		ClusterSize: binary.LittleEndian.Uint32(b[8:]),
		ChunkSize:   binary.LittleEndian.Uint32(b[12:]),
		BlockCount:  binary.LittleEndian.Uint64(b[16:]),
		UsedBlocks:  binary.LittleEndian.Uint64(b[24:]),
		DeviceSize:  binary.LittleEndian.Uint64(b[32:]),
		BitmapCRC32: binary.LittleEndian.Uint32(b[40:]),
	}
	if err := h.Validate(); err != nil {
		return Header{}, err
	}
	return h, nil
}

// maxInt is the largest value an int can hold on this platform.
const maxInt = int(^uint(0) >> 1)
