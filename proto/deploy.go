package proto

import (
	"encoding/binary"
	"errors"
	"fmt"
	"io"
)

// TaskKindDeploy writes an image over the disk of a PC, destroying what is there.
const TaskKindDeploy = "deploy"

// deployEntrySize is the wire size of one entry of the deploy list.
const deployEntrySize = 40

// DeployInfo answers the start of a deploy.
type DeployInfo struct {
	BlockSize uint32   `json:"blockSize"`
	Image     string   `json:"image"`
	Version   int      `json:"version"`
	Manifest  Manifest `json:"manifest"`
}

// DeployEntry is one place of the disk to write. Its number in the list is how
// the agent asks for its data.
type DeployEntry struct {
	Partition uint32
	Position  uint32
	Hash      Sum // empty for a block of zeros
}

// Zero reports whether the place is filled with zeros, with no data to fetch.
func (e DeployEntry) Zero() bool { return e.Hash == Sum{} }

// AppendDeployEntry encodes one entry.
func AppendDeployEntry(dst []byte, e DeployEntry) []byte {
	dst = binary.LittleEndian.AppendUint32(dst, e.Partition)
	dst = binary.LittleEndian.AppendUint32(dst, e.Position)
	return append(dst, e.Hash[:]...)
}

// ParseDeployEntries decodes the deploy list.
func ParseDeployEntries(b []byte) ([]DeployEntry, error) {
	if len(b)%deployEntrySize != 0 {
		return nil, errors.New("proto: truncated deploy list")
	}
	entries := make([]DeployEntry, len(b)/deployEntrySize)
	for i := range entries {
		e := &entries[i]
		e.Partition = binary.LittleEndian.Uint32(b[0:])
		e.Position = binary.LittleEndian.Uint32(b[4:])
		copy(e.Hash[:], b[8:])
		b = b[deployEntrySize:]
	}
	return entries, nil
}

// AppendNumbers encodes the entry numbers an agent asks for.
func AppendNumbers(dst []byte, numbers []uint32) []byte {
	for _, n := range numbers {
		dst = binary.LittleEndian.AppendUint32(dst, n)
	}
	return dst
}

// ParseNumbers decodes entry numbers.
func ParseNumbers(b []byte) ([]uint32, error) {
	if len(b)%4 != 0 {
		return nil, errors.New("proto: truncated number list")
	}
	numbers := make([]uint32, len(b)/4)
	for i := range numbers {
		numbers[i] = binary.LittleEndian.Uint32(b[i*4:])
	}
	return numbers, nil
}

// WriteStored sends one stored block.
func WriteStored(w io.Writer, h BlockHeader, payload []byte) error {
	if _, err := w.Write(h.MarshalBinary()); err != nil {
		return err
	}
	_, err := w.Write(payload)
	return err
}

// ReadStored receives one stored block. The caller checks it with DecodeBlock.
func ReadStored(r io.Reader) (BlockHeader, []byte, error) {
	var hb [BlockHeaderSize]byte
	if _, err := io.ReadFull(r, hb[:]); err != nil {
		return BlockHeader{}, nil, unexpected(err)
	}
	h, err := ParseBlockHeader(hb[:])
	if err != nil {
		return BlockHeader{}, nil, err
	}
	if h.Stored > maxStored {
		return BlockHeader{}, nil, fmt.Errorf("proto: block payload of %d bytes", h.Stored)
	}
	payload := make([]byte, h.Stored)
	if _, err := io.ReadFull(r, payload); err != nil {
		return BlockHeader{}, nil, unexpected(err)
	}
	return h, payload, nil
}
