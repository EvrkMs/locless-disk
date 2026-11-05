package proto

import (
	"encoding/binary"
	"errors"
	"fmt"
	"hash/crc32"
	"io"
)

// ChunkHeaderSize is the wire size of ChunkHeader in bytes.
const ChunkHeaderSize = 20

// chunkCRCOffset is where the checksum sits in the encoded chunk header. The
// bytes before it are covered by the checksum.
const chunkCRCOffset = 16

// chunkOverhead bounds how much larger a compressed payload may be than the
// raw data it replaces. It protects the reader from corrupt or hostile headers.
const chunkOverhead = 4096

// ErrShortChunk is returned when fewer than ChunkHeaderSize bytes are available.
var ErrShortChunk = errors.New("proto: short chunk header")

// ChunkHeader precedes every chunk payload on the wire.
//
// Raw is the uncompressed size in bytes and Compressed the payload size. Index
// is the position of the chunk in the image, counted from zero. CRC32 covers
// the header fields and the payload as stored, so a chunk can be verified
// before it is decompressed and a damaged size or index cannot go unnoticed.
type ChunkHeader struct {
	Compressed uint32
	Raw        uint32
	Index      uint64
	CRC32      uint32
}

// MarshalBinary encodes the chunk header.
func (c ChunkHeader) MarshalBinary() []byte {
	b := make([]byte, ChunkHeaderSize)
	binary.LittleEndian.PutUint32(b[0:], c.Compressed)
	binary.LittleEndian.PutUint32(b[4:], c.Raw)
	binary.LittleEndian.PutUint64(b[8:], c.Index)
	binary.LittleEndian.PutUint32(b[chunkCRCOffset:], c.CRC32)
	return b
}

// ParseChunkHeader decodes a chunk header.
func ParseChunkHeader(b []byte) (ChunkHeader, error) {
	if len(b) < ChunkHeaderSize {
		return ChunkHeader{}, ErrShortChunk
	}
	return ChunkHeader{
		Compressed: binary.LittleEndian.Uint32(b[0:]),
		Raw:        binary.LittleEndian.Uint32(b[4:]),
		Index:      binary.LittleEndian.Uint64(b[8:]),
		CRC32:      binary.LittleEndian.Uint32(b[chunkCRCOffset:]),
	}, nil
}

// checksum computes the CRC32 of the header fields followed by the payload.
func (c ChunkHeader) checksum(payload []byte) uint32 {
	sum := crc32.ChecksumIEEE(c.MarshalBinary()[:chunkCRCOffset])
	return crc32.Update(sum, crc32.IEEETable, payload)
}

// Verify checks the header and the payload against the stored checksum.
func (c ChunkHeader) Verify(payload []byte) error {
	if uint64(len(payload)) != uint64(c.Compressed) {
		return fmt.Errorf("proto: chunk payload is %d bytes, header says %d", len(payload), c.Compressed)
	}
	if got := c.checksum(payload); got != c.CRC32 {
		return fmt.Errorf("proto: chunk crc mismatch: %#08x != %#08x", got, c.CRC32)
	}
	return nil
}

// AppendChunk appends a chunk header and payload to dst and returns the result.
// index is the chunk position in the image and raw the uncompressed size of the
// data the payload encodes.
func AppendChunk(dst, payload []byte, index uint64, raw uint32) []byte {
	h := ChunkHeader{
		Compressed: uint32(len(payload)),
		Raw:        raw,
		Index:      index,
	}
	h.CRC32 = h.checksum(payload)
	dst = append(dst, h.MarshalBinary()...)
	return append(dst, payload...)
}

// ReadChunk reads one chunk from r. maxRaw bounds the payload the header may
// claim to describe, so a corrupt file cannot trigger a huge allocation.
func ReadChunk(r io.Reader, maxRaw uint32) (ChunkHeader, []byte, error) {
	var hb [ChunkHeaderSize]byte
	if _, err := io.ReadFull(r, hb[:]); err != nil {
		return ChunkHeader{}, nil, err
	}
	h, err := ParseChunkHeader(hb[:])
	if err != nil {
		return ChunkHeader{}, nil, err
	}
	if h.Raw > maxRaw {
		return ChunkHeader{}, nil, fmt.Errorf("proto: chunk raw size %d exceeds limit %d", h.Raw, maxRaw)
	}
	limit := uint64(maxRaw) + chunkOverhead
	if uint64(h.Compressed) > limit {
		return ChunkHeader{}, nil, fmt.Errorf("proto: chunk payload size %d exceeds limit %d", h.Compressed, limit)
	}
	payload := make([]byte, h.Compressed)
	if _, err := io.ReadFull(r, payload); err != nil {
		return ChunkHeader{}, nil, err
	}
	return h, payload, nil
}
