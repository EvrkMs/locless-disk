// Package proto defines the image format and the wire protocol shared by the
// FOGs server and the WinPE agent.
//
// An image file is a fixed 44-byte header, a bitmap of occupied clusters and a
// sequence of independent compressed chunks:
//
//	magic(4) version(2) compression(2) clusterSize(4) chunkSize(4)
//	blockCount(8) usedBlocks(8) deviceSize(8) headerCRC32(4)
//	bitmap(ceil(blockCount/8) bytes, bit i = block i is stored)
//	chunk{ compLen(4) rawLen(4) crc32(4) payload(compLen) } ...
//
// All integers are little-endian. Every chunk decompresses on its own, so
// chunks can be compressed in parallel and sent independently over multicast.
package proto

import (
	"encoding/binary"
	"errors"
	"fmt"
	"hash/crc32"
)

// Magic is "FOGS" read as a little-endian uint32.
const Magic uint32 = 0x53474F46

// Version is the current format version.
const Version uint16 = 1

// HeaderSize is the exact wire size of Header in bytes.
const HeaderSize = 44

// Limits for the values stored in Header.
const (
	MinClusterSize = 512
	MaxClusterSize = 1 << 20
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
	return nil
}

// BitmapLen returns the number of bytes the block bitmap occupies.
func (h Header) BitmapLen() (int, error) {
	n := (h.BlockCount + 7) / 8
	if n > uint64(maxInt) {
		return 0, fmt.Errorf("proto: bitmap of %d blocks is too large", h.BlockCount)
	}
	return int(n), nil
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
	binary.LittleEndian.PutUint32(b[40:], crc32.ChecksumIEEE(b[:40]))
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
	if got, want := binary.LittleEndian.Uint32(b[40:]), crc32.ChecksumIEEE(b[:40]); got != want {
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
	}
	if err := h.Validate(); err != nil {
		return Header{}, err
	}
	return h, nil
}

// maxInt is the largest value an int can hold on this platform.
const maxInt = int(^uint(0) >> 1)
