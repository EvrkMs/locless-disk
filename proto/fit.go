package proto

import "fmt"

// partitionAlign keeps partitions on 1 MiB, as Windows does, for SSD alignment.
const partitionAlign = 1 << 20

// Fit lays the partitions of an image out on a disk whose usable area is
// [first, end). The disk may be larger or smaller than the source: the table is
// built for this disk, so it only has to hold the captured data.
//
// Partitions before the fill partition keep their place. Partitions after it,
// like Recovery, move to the end of the disk. The fill partition takes what is
// between them and must not end up smaller than what was captured. Sizes in the
// result are the new ones; blocks stay addressed from the start of a partition,
// so moving a partition does not change them.
func (m Manifest) Fit(first, end uint64) (Manifest, error) {
	out := m
	out.Partitions = append([]Partition(nil), m.Partitions...)
	if len(out.Partitions) == 0 {
		return Manifest{}, fmt.Errorf("proto: manifest has no partitions")
	}
	if out.Partitions[0].Offset < first {
		return Manifest{}, fmt.Errorf("proto: first partition starts before the usable area")
	}

	fill := -1
	for i, p := range out.Partitions {
		if p.Policy == PolicyFill {
			fill = i
		}
	}
	if fill < 0 {
		last := out.Partitions[len(out.Partitions)-1]
		if need := last.Offset + last.Size; need > end {
			return Manifest{}, &TooSmallError{Need: need, Have: end}
		}
		return out, nil
	}

	cursor := end / partitionAlign * partitionAlign
	var tail uint64
	for i := len(out.Partitions) - 1; i > fill; i-- {
		p := &out.Partitions[i]
		aligned := (p.Size + partitionAlign - 1) / partitionAlign * partitionAlign
		tail += aligned
		if aligned > cursor {
			return Manifest{}, &TooSmallError{Need: tail, Have: end}
		}
		cursor -= aligned
		p.Offset = cursor
	}
	p := &out.Partitions[fill]
	if tail == 0 && cursor < p.Offset+p.Size && p.Offset+p.Size <= end {
		return out, nil // a volume up to the very end of a disk of the same size
	}
	if need := p.Offset + p.Size + tail; cursor < p.Offset+p.Size {
		return Manifest{}, &TooSmallError{Need: need, Have: end}
	}
	p.Size = cursor - p.Offset
	return out, nil
}

// TooSmallError says the disk cannot hold the image.
type TooSmallError struct {
	Need, Have uint64
}

func (e *TooSmallError) Error() string {
	return fmt.Sprintf("proto: image needs %d bytes, the disk has %d", e.Need, e.Have)
}
