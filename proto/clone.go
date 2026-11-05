package proto

import (
	"encoding/binary"
	"errors"
	"fmt"
	"io"
	"regexp"
)

// TaskKindClone reads the disk of a PC into a new version of an image. It only
// reads: the disk is not changed.
const TaskKindClone = "clone"

// maxStored bounds a payload on the wire; zstd can slightly grow bad input, but
// EncodeBlock stores such blocks raw.
const maxStored = MaxBlockSize

var imageName = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9_-]{0,63}$`)

// ValidImageName reports whether name can be an image. Names become directory
// names on the server, so nothing that could leave the image directory passes.
func ValidImageName(name string) bool { return imageName.MatchString(name) }

// CloneInfo answers the start of a clone.
type CloneInfo struct {
	BlockSize uint32 `json:"blockSize"`
	Image     string `json:"image"`
	Version   int    `json:"version"` // number of the version being created
}

// CloneResult says what a finished clone produced.
type CloneResult struct {
	File string `json:"file"`
	Size int64  `json:"size"`
}

// PutRecord places one block on the disk. Block is nil if the server already
// has the data, which it said in its answer to the check.
type PutRecord struct {
	Partition uint32
	Position  uint32
	Hash      Sum
	Block     *BlockHeader
	Payload   []byte
}

// AppendHashes encodes a list of hashes for a check.
func AppendHashes(dst []byte, hashes []Sum) []byte {
	for i := range hashes {
		dst = append(dst, hashes[i][:]...)
	}
	return dst
}

// ParseHashes decodes a list of hashes.
func ParseHashes(b []byte) ([]Sum, error) {
	if len(b)%len(Sum{}) != 0 {
		return nil, errors.New("proto: truncated hash list")
	}
	hashes := make([]Sum, len(b)/len(Sum{}))
	for i := range hashes {
		copy(hashes[i][:], b[i*len(Sum{}):])
	}
	return hashes, nil
}

// AppendPut encodes one record.
func AppendPut(dst []byte, r PutRecord) []byte {
	dst = binary.LittleEndian.AppendUint32(dst, r.Partition)
	dst = binary.LittleEndian.AppendUint32(dst, r.Position)
	dst = append(dst, r.Hash[:]...)
	if r.Block == nil {
		return append(dst, 0)
	}
	dst = append(dst, 1)
	dst = append(dst, r.Block.MarshalBinary()...)
	return append(dst, r.Payload...)
}

// ReadPut decodes one record. It returns io.EOF after the last one.
func ReadPut(r io.Reader) (PutRecord, error) {
	var head [41]byte
	if _, err := io.ReadFull(r, head[:]); err != nil {
		return PutRecord{}, err // a clean end of the body is io.EOF
	}
	p := PutRecord{
		Partition: binary.LittleEndian.Uint32(head[0:]),
		Position:  binary.LittleEndian.Uint32(head[4:]),
	}
	copy(p.Hash[:], head[8:])
	if head[40] == 0 {
		return p, nil
	}
	var hb [BlockHeaderSize]byte
	if _, err := io.ReadFull(r, hb[:]); err != nil {
		return PutRecord{}, unexpected(err)
	}
	h, err := ParseBlockHeader(hb[:])
	if err != nil {
		return PutRecord{}, err
	}
	if h.Stored > maxStored {
		return PutRecord{}, fmt.Errorf("proto: block payload of %d bytes", h.Stored)
	}
	p.Block = &h
	p.Payload = make([]byte, h.Stored)
	if _, err := io.ReadFull(r, p.Payload); err != nil {
		return PutRecord{}, unexpected(err)
	}
	return p, nil
}

// unexpected keeps a body cut inside a record from looking like a clean end.
func unexpected(err error) error {
	if errors.Is(err, io.EOF) {
		return io.ErrUnexpectedEOF
	}
	return err
}
