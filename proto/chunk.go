package proto

import (
	"encoding/binary"
	"errors"
	"fmt"
	"hash/crc32"
	"io"
)

// ChunkHeaderSize is the wire size of ChunkHeader in bytes.
const ChunkHeaderSize = 12

// chunkOverhead bounds how much larger a compressed payload may be than the
// raw data it replaces. It protects the reader from corrupt or hostile headers.
const chunkOverhead = 4096

// ErrShortChunk is returned when fewer than ChunkHeaderSize bytes are available.
var ErrShortChunk = errors.New("proto: short chunk header")

// ChunkHeader precedes every chunk payload on the wire.
//
// Raw is the uncompressed size in bytes and Compressed the payload size. CRC32
// covers the payload as stored, so a chunk can be verified before it is
// decompressed.
type ChunkHeader struct {
	Compressed uint32
	Raw        uint32
	CRC32      uint32
}

// MarshalBinary encodes the chunk header.
func (c ChunkHeader) MarshalBinary() []byte {
	b := make([]byte, ChunkHeaderSize)
	binary.LittleEndian.PutUint32(b[0:], c.Compressed)
	binary.LittleEndian.PutUint32(b[4:], c.Raw)
	binary.LittleEndian.PutUint32(b[8:], c.CRC32)
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
		CRC32:      binary.LittleEndian.Uint32(b[8:]),
	}, nil
}

// Verify checks the payload against the header.
func (c ChunkHeader) Verify(payload []byte) error {
	if uint32(len(payload)) != c.Compressed {
		return fmt.Errorf("proto: chunk payload is %d bytes, header says %d", len(payload), c.Compressed)
	}
	if got := crc32.ChecksumIEEE(payload); got != c.CRC32 {
		return fmt.Errorf("proto: chunk crc mismatch: %#08x != %#08x", got, c.CRC32)
	}
	return nil
}

// AppendChunk appends a chunk header and payload to dst and returns the result.
// raw is the uncompressed size of the data the payload encodes.
func AppendChunk(dst, payload []byte, raw uint32) []byte {
	h := ChunkHeader{
		Compressed: uint32(len(payload)),
		Raw:        raw,
		CRC32:      crc32.ChecksumIEEE(payload),
	}
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
	if h.Compressed > maxRaw+chunkOverhead {
		return ChunkHeader{}, nil, fmt.Errorf("proto: chunk payload size %d exceeds limit %d", h.Compressed, maxRaw+chunkOverhead)
	}
	payload := make([]byte, h.Compressed)
	if _, err := io.ReadFull(r, payload); err != nil {
		return ChunkHeader{}, nil, err
	}
	return h, payload, nil
}
