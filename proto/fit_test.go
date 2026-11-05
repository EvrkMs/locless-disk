package proto

import (
	"errors"
	"testing"
)

const gib = 1024 * mib

// fitManifest is a captured disk: EFI, MSR, Windows shrunk to 40 GiB, Recovery.
func fitManifest() Manifest {
	return Manifest{
		Version: ManifestVersion, DiskSize: 240 * gib, SectorSize: 512, DiskGUID: testGUID,
		Partitions: []Partition{
			{Number: 1, TypeGUID: testGUID, GUID: testGUID, Offset: 1 * mib, Size: 100 * mib, Policy: PolicyFixed, Content: ContentBlocks},
			{Number: 2, TypeGUID: testGUID, GUID: testGUID, Offset: 101 * mib, Size: 16 * mib, Policy: PolicyFixed, Content: ContentNone},
			{Number: 3, TypeGUID: testGUID, GUID: testGUID, Offset: 117 * mib, Size: 40 * gib, Policy: PolicyFill, Content: ContentBlocks},
			{Number: 4, TypeGUID: testGUID, GUID: testGUID, Offset: 200 * gib, Size: 500 * mib, Policy: PolicyFixed, Content: ContentBlocks},
		},
	}
}

// TestFitIsForTheRealDisk covers disks larger and smaller than the source, and
// sizes that differ by a few bytes: the table must always match the target.
func TestFitIsForTheRealDisk(t *testing.T) {
	for _, end := range []uint64{128 * gib, 128*gib - 17408, 240 * gib, 240*gib + 4096, 512 * gib, 41 * gib} {
		m := fitManifest()
		got, err := m.Fit(1*mib, end)
		if err != nil {
			t.Fatalf("end %d: %v", end, err)
		}
		p := got.Partitions
		if p[0].Offset != 1*mib || p[1].Offset != 101*mib || p[2].Offset != 117*mib {
			t.Fatalf("end %d: head moved: %+v", end, p)
		}
		if p[2].Size < 40*gib {
			t.Fatalf("end %d: fill shrank to %d", end, p[2].Size)
		}
		if p[2].Offset+p[2].Size != p[3].Offset {
			t.Fatalf("end %d: gap before the tail: %+v", end, p)
		}
		if p[3].Size != 500*mib || p[3].Offset+p[3].Size > end || end-(p[3].Offset+p[3].Size) >= partitionAlign+1 {
			t.Fatalf("end %d: tail: %+v", end, p[3])
		}
		if p[3].Offset%partitionAlign != 0 {
			t.Fatalf("end %d: tail is not aligned: %d", end, p[3].Offset)
		}
		if m.Partitions[3].Offset != 200*gib || m.Partitions[2].Size != 40*gib {
			t.Fatal("Fit changed the original manifest")
		}
	}
}

func TestFitRefusesADiskTooSmall(t *testing.T) {
	var small *TooSmallError
	if _, err := fitManifest().Fit(1*mib, 40*gib); !errors.As(err, &small) {
		t.Fatalf("got %v", err)
	}
	if small.Need <= 40*gib || small.Have != 40*gib {
		t.Fatalf("error: %+v", small)
	}
}

func TestFitWithoutFillKeepsTheLayout(t *testing.T) {
	m := fitManifest()
	m.Partitions[2].Policy = PolicyFixed
	got, err := m.Fit(1*mib, 240*gib)
	if err != nil {
		t.Fatalf("Fit: %v", err)
	}
	if got.Partitions[3].Offset != 200*gib {
		t.Fatalf("moved: %+v", got.Partitions[3])
	}
	var small *TooSmallError
	if _, err := m.Fit(1*mib, 100*gib); !errors.As(err, &small) {
		t.Fatalf("got %v", err)
	}
}

func TestFitRefusesAStartBeforeTheUsableArea(t *testing.T) {
	if _, err := fitManifest().Fit(2*mib, 240*gib); err == nil {
		t.Fatal("Fit accepted a partition before the usable area")
	}
}

// TestFitKeepsAVolumeUpToTheEnd: a volume that was not shrunk may end past the last
// 1 MiB boundary, and a disk of the same size still holds it.
func TestFitKeepsAVolumeUpToTheEnd(t *testing.T) {
	m := Manifest{Partitions: []Partition{{Number: 1, Offset: 1 * mib, Size: 10*mib + 4096, Policy: PolicyFill}}}
	end := uint64(11*mib + 4096)
	got, err := m.Fit(1*mib, end)
	if err != nil {
		t.Fatalf("Fit: %v", err)
	}
	if got.Partitions[0].Size != 10*mib+4096 {
		t.Fatalf("size %d", got.Partitions[0].Size)
	}
	var small *TooSmallError
	if _, err := m.Fit(1*mib, end-1); !errors.As(err, &small) {
		t.Fatalf("one byte less: %v", err)
	}
}
