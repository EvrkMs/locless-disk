package proto

import (
	"encoding/json"
	"errors"
	"fmt"
	"strings"
)

// ManifestVersion is the current manifest format version.
const ManifestVersion = 1

// ErrUnsupportedManifest is returned for a manifest version this build cannot read.
var ErrUnsupportedManifest = errors.New("proto: unsupported manifest version")

// SizePolicy says how a partition is sized when an image is deployed.
type SizePolicy string

const (
	// PolicyFixed keeps the size recorded in the manifest.
	PolicyFixed SizePolicy = "fixed"
	// PolicyFill takes all the space the fixed partitions leave on the disk.
	PolicyFill SizePolicy = "fill"
)

// Content says what an image keeps of a partition.
type Content string

const (
	// ContentBlocks stores the partition as blocks.
	ContentBlocks Content = "blocks"
	// ContentNone only creates the partition on deploy, as with MSR.
	ContentNone Content = "none"
)

// Manifest describes the GPT disk an image was captured from.
type Manifest struct {
	Version    int         `json:"version"`
	DiskSize   uint64      `json:"diskSize"`   // source disk size in bytes
	SectorSize uint32      `json:"sectorSize"` // logical sector size in bytes
	DiskGUID   string      `json:"diskGuid"`
	Partitions []Partition `json:"partitions"` // in disk order
}

// Partition is one GPT entry of the source disk.
type Partition struct {
	Number       uint32     `json:"number"`
	TypeGUID     string     `json:"typeGuid"`
	GUID         string     `json:"guid"`
	Name         string     `json:"name,omitempty"`
	Attributes   uint64     `json:"attributes,omitempty"`
	Offset       uint64     `json:"offset"`                 // bytes from the start of the disk
	Size         uint64     `json:"size"`                   // bytes, as captured
	OriginalSize uint64     `json:"originalSize,omitempty"` // before the shrink, if there was one
	Policy       SizePolicy `json:"policy"`
	Content      Content    `json:"content"`
}

// Validate reports whether the manifest describes a deployable disk.
func (m Manifest) Validate() error {
	if m.Version != ManifestVersion {
		return fmt.Errorf("%w: %d", ErrUnsupportedManifest, m.Version)
	}
	if m.SectorSize < 512 || m.SectorSize&(m.SectorSize-1) != 0 {
		return fmt.Errorf("proto: bad sector size %d", m.SectorSize)
	}
	if m.DiskSize == 0 {
		return errors.New("proto: disk size is zero")
	}
	if !validGUID(m.DiskGUID) {
		return fmt.Errorf("proto: bad disk guid %q", m.DiskGUID)
	}
	if len(m.Partitions) == 0 {
		return errors.New("proto: manifest has no partitions")
	}

	numbers := make(map[uint32]bool, len(m.Partitions))
	fills := 0
	var end uint64
	for _, p := range m.Partitions {
		if err := p.validate(m); err != nil {
			return fmt.Errorf("proto: partition %d: %w", p.Number, err)
		}
		if numbers[p.Number] {
			return fmt.Errorf("proto: duplicate partition number %d", p.Number)
		}
		numbers[p.Number] = true
		if p.Offset < end {
			return fmt.Errorf("proto: partition %d overlaps the previous one or is out of order", p.Number)
		}
		end = p.Offset + p.Size
		if p.Policy == PolicyFill {
			fills++
		}
	}
	if fills > 1 {
		return fmt.Errorf("proto: %d partitions have the fill policy, at most one may", fills)
	}
	return nil
}

// validate checks the fields of one partition against the disk.
func (p Partition) validate(m Manifest) error {
	if !validGUID(p.TypeGUID) {
		return fmt.Errorf("bad type guid %q", p.TypeGUID)
	}
	if !validGUID(p.GUID) {
		return fmt.Errorf("bad guid %q", p.GUID)
	}
	switch p.Policy {
	case PolicyFixed, PolicyFill:
	default:
		return fmt.Errorf("unknown size policy %q", p.Policy)
	}
	switch p.Content {
	case ContentBlocks, ContentNone:
	default:
		return fmt.Errorf("unknown content %q", p.Content)
	}
	sector := uint64(m.SectorSize)
	if p.Size == 0 || p.Offset%sector != 0 || p.Size%sector != 0 {
		return fmt.Errorf("offset %d and size %d must be non-zero multiples of the sector size", p.Offset, p.Size)
	}
	if p.Size > m.DiskSize || p.Offset > m.DiskSize-p.Size {
		return fmt.Errorf("extends past the end of a %d-byte disk", m.DiskSize)
	}
	return nil
}

// Marshal validates the manifest and encodes it as JSON.
func (m Manifest) Marshal() ([]byte, error) {
	if err := m.Validate(); err != nil {
		return nil, err
	}
	return json.MarshalIndent(m, "", "  ")
}

// ParseManifest decodes and validates a manifest.
func ParseManifest(b []byte) (Manifest, error) {
	var m Manifest
	if err := json.Unmarshal(b, &m); err != nil {
		return Manifest{}, fmt.Errorf("proto: bad manifest: %w", err)
	}
	if err := m.Validate(); err != nil {
		return Manifest{}, err
	}
	return m, nil
}

// validGUID reports whether s has the form 00000000-0000-0000-0000-000000000000.
func validGUID(s string) bool {
	if len(s) != 36 {
		return false
	}
	for i := 0; i < len(s); i++ {
		switch i {
		case 8, 13, 18, 23:
			if s[i] != '-' {
				return false
			}
		default:
			if !strings.ContainsRune("0123456789abcdefABCDEF", rune(s[i])) {
				return false
			}
		}
	}
	return true
}
