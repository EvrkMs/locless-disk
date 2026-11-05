package proto

import (
	"errors"
	"reflect"
	"strings"
	"testing"
)

const (
	testGUID = "12345678-9abc-def0-1234-56789abcdef0"
	mib      = 1 << 20
)

func testManifest() Manifest {
	return Manifest{
		Version:    ManifestVersion,
		DiskSize:   1000 * mib,
		SectorSize: 512,
		DiskGUID:   testGUID,
		Partitions: []Partition{
			{Number: 1, TypeGUID: testGUID, GUID: testGUID, Name: "EFI", Offset: 1 * mib, Size: 100 * mib, Policy: PolicyFixed, Content: ContentBlocks},
			{Number: 2, TypeGUID: testGUID, GUID: testGUID, Offset: 101 * mib, Size: 16 * mib, Policy: PolicyFixed, Content: ContentNone},
			{Number: 3, TypeGUID: testGUID, GUID: testGUID, Offset: 117 * mib, Size: 500 * mib, Policy: PolicyFill, Content: ContentBlocks},
			{Number: 4, TypeGUID: testGUID, GUID: testGUID, Attributes: 0x8000000000000001, Offset: 617 * mib, Size: 300 * mib, Policy: PolicyFixed, Content: ContentBlocks},
		},
	}
}

func TestManifestRoundTrip(t *testing.T) {
	m := testManifest()

	b, err := m.Marshal()
	if err != nil {
		t.Fatalf("Marshal: %v", err)
	}
	got, err := ParseManifest(b)
	if err != nil {
		t.Fatalf("ParseManifest: %v", err)
	}
	if !reflect.DeepEqual(got, m) {
		t.Fatalf("round trip mismatch:\n got %+v\nwant %+v", got, m)
	}
}

// TestManifestKeys pins the JSON field names: these are the on-disk format.
func TestManifestKeys(t *testing.T) {
	b, err := testManifest().Marshal()
	if err != nil {
		t.Fatalf("Marshal: %v", err)
	}
	for _, key := range []string{
		"version", "diskSize", "sectorSize", "diskGuid", "partitions",
		"number", "typeGuid", "guid", "name", "attributes", "offset", "size", "policy", "content",
	} {
		if !strings.Contains(string(b), `"`+key+`":`) {
			t.Errorf("encoded manifest has no key %q", key)
		}
	}
}

func TestManifestValidate(t *testing.T) {
	tests := []struct {
		name   string
		mutate func(*Manifest)
	}{
		{"sector too small", func(m *Manifest) { m.SectorSize = 256 }},
		{"sector not power of two", func(m *Manifest) { m.SectorSize = 1000 }},
		{"no disk size", func(m *Manifest) { m.DiskSize = 0 }},
		{"bad disk guid", func(m *Manifest) { m.DiskGUID = "not-a-guid" }},
		{"no partitions", func(m *Manifest) { m.Partitions = nil }},
		{"bad type guid", func(m *Manifest) { m.Partitions[0].TypeGUID = "" }},
		{"bad partition guid", func(m *Manifest) { m.Partitions[0].GUID = strings.Repeat("g", 36) }},
		{"unknown policy", func(m *Manifest) { m.Partitions[0].Policy = "grow" }},
		{"no policy", func(m *Manifest) { m.Partitions[0].Policy = "" }},
		{"unknown content", func(m *Manifest) { m.Partitions[0].Content = "files" }},
		{"no content", func(m *Manifest) { m.Partitions[0].Content = "" }},
		{"zero size", func(m *Manifest) { m.Partitions[0].Size = 0 }},
		{"unaligned offset", func(m *Manifest) { m.Partitions[0].Offset = 1000 }},
		{"unaligned size", func(m *Manifest) { m.Partitions[0].Size = 1000 }},
		{"past end of disk", func(m *Manifest) { m.Partitions[3].Size = 1000 * mib }},
		{"offset overflow", func(m *Manifest) { m.Partitions[3].Offset = ^uint64(0) - 511 }},
		{"overlap", func(m *Manifest) { m.Partitions[1].Offset = 100 * mib }},
		{"out of order", func(m *Manifest) { m.Partitions[0], m.Partitions[3] = m.Partitions[3], m.Partitions[0] }},
		{"duplicate number", func(m *Manifest) { m.Partitions[1].Number = 1 }},
		{"two fills", func(m *Manifest) { m.Partitions[3].Policy = PolicyFill }},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			m := testManifest()
			tc.mutate(&m)
			if err := m.Validate(); err == nil {
				t.Fatal("Validate accepted an invalid manifest")
			}
		})
	}
}

func TestManifestWithoutFillIsValid(t *testing.T) {
	m := testManifest()
	m.Partitions[2].Policy = PolicyFixed
	if err := m.Validate(); err != nil {
		t.Fatalf("Validate: %v", err)
	}
}

func TestParseManifestRejects(t *testing.T) {
	if _, err := ParseManifest([]byte("{")); err == nil {
		t.Fatal("ParseManifest accepted broken JSON")
	}
	if _, err := ParseManifest([]byte(`{"version":99}`)); !errors.Is(err, ErrUnsupportedManifest) {
		t.Fatalf("got %v, want %v", err, ErrUnsupportedManifest)
	}
}
