package main

import (
	"bytes"
	"encoding/binary"
	"errors"
	"os"
	"path/filepath"
	"slices"
	"strconv"
	"testing"

	"fogs/proto"
)

// mustBeVirtual stops the test unless the disk is a VHD. This test wipes a disk.
func mustBeVirtual(t *testing.T, number int) {
	t.Helper()
	bus, err := busType(number)
	if err != nil {
		t.Fatalf("query disk %d: %v", number, err)
	}
	if bus != busVirtual {
		t.Fatalf("disk %d has bus type %d, this is not a VHD: refusing", number, bus)
	}
}

func diskFromEnv(t *testing.T, name string) int {
	t.Helper()
	n, err := strconv.Atoi(os.Getenv(name))
	if err != nil {
		t.Skip(name + " is not set")
	}
	mustBeVirtual(t, n)
	return n
}

// checkRestored reads a disk afresh: the table must be the saved one and the file
// system must fill its partition.
func checkRestored(t *testing.T, number int, table []byte) {
	t.Helper()
	d, err := openDisk(number)
	if err != nil {
		t.Fatalf("openDisk: %v", err)
	}
	defer d.Close()
	_, want, err := parseLayout(table)
	if err != nil {
		t.Fatalf("parseLayout: %v", err)
	}
	current, err := d.Table()
	if err != nil {
		t.Fatalf("Table: %v", err)
	}
	_, now, err := parseLayout(current)
	if err != nil {
		t.Fatalf("parseLayout: %v", err)
	}
	if !slices.Equal(want, now) {
		t.Fatalf("table %+v, want %+v", now, want)
	}
	if fill := largestVolume(want); !d.filled(fill) {
		t.Fatalf("file system of partition %d does not fill it: %+v", fill.number, d.maps[fill.number])
	}
}

// grownTable returns a table in which the volume is larger by mib. For Repair, a
// disk with the table of now and such a saved table is a disk after a shrink: the
// partition and its file system are smaller than the table says, and the space
// behind them is free.
func grownTable(t *testing.T, table []byte, mib uint64) []byte {
	t.Helper()
	entries := mustEntries(t, table)
	fill := largestVolume(entries)
	out := append([]byte(nil), table...)
	for i := range entries {
		if entries[i].number == fill.number {
			e := out[layoutEntriesAt+i*layoutEntrySize:]
			binary.LittleEndian.PutUint64(e[16:], fill.size+mib<<20)
		}
	}
	return out
}

// testRepair gives a volume its size back the ways a real PC can need it. The
// repair always runs from a fresh start, as from another boot after a power cut,
// knowing nothing but the table saved on the server. It needs free space behind the
// volume: scripts/vhd-test.ps1 -SourceData leaves some.
func testRepair(t *testing.T, d *physicalDisk, table []byte) {
	t.Helper()
	entries := mustEntries(t, table)
	fill := largestVolume(entries)
	usable := binary.LittleEndian.Uint64(table[24:]) + binary.LittleEndian.Uint64(table[32:])
	if fill.start+fill.size+128<<20 > usable {
		t.Log("no free space behind the volume, repair skipped")
		return
	}
	fresh := func() *physicalDisk {
		f, err := openDisk(d.number)
		if err != nil {
			t.Fatalf("openDisk: %v", err)
		}
		t.Cleanup(func() { f.Close() })
		f.allowShrink = true
		return f
	}

	// Power lost with the volume shrunk.
	first := grownTable(t, table, 64)
	if err := fresh().Repair(first); err != nil {
		t.Fatalf("Repair of a smaller volume: %v", err)
	}
	checkRestored(t, d.number, first)
	t.Log("repaired from another boot")

	// A repair with nothing left to do changes nothing.
	if err := fresh().Repair(first); err != nil {
		t.Fatalf("Repair of a whole volume: %v", err)
	}
	checkRestored(t, d.number, first)

	// Power lost between the two steps: the table is back, the file system is not.
	second := grownTable(t, table, 128)
	if err := fresh().writeTable(second, mustEntries(t, second)); err != nil {
		t.Fatalf("writeTable: %v", err)
	}
	if err := fresh().Repair(second); err != nil {
		t.Fatalf("Repair after a table write: %v", err)
	}
	checkRestored(t, d.number, second)
	t.Log("repaired after a half-done repair")

	// A table of another layout is refused and writes nothing.
	other := append([]byte(nil), second...)
	other[layoutEntriesAt+8+1]++ // the first partition starts elsewhere
	if err := fresh().Repair(other); !errors.Is(err, errStaleTable) {
		t.Fatalf("Repair with a foreign table: %v", err)
	}
	// The saved table is smaller than the disk now: it must not cut the volume.
	if err := fresh().Repair(table); !errors.Is(err, errStaleTable) {
		t.Fatalf("Repair with an older table: %v", err)
	}
	checkRestored(t, d.number, second)
	t.Log("foreign tables refused")
}

func mustEntries(t *testing.T, table []byte) []tableEntry {
	t.Helper()
	_, entries, err := parseLayout(table)
	if err != nil {
		t.Fatalf("parseLayout: %v", err)
	}
	return entries
}

// TestVHDCloneAndDeploy runs the real disk code on virtual disks: it clones one
// and writes the image over another, wiping it. scripts/vhd-test.ps1 sets it up.
func TestVHDCloneAndDeploy(t *testing.T) {
	src := diskFromEnv(t, "LOCLESS_VHD_SRC")
	dst := diskFromEnv(t, "LOCLESS_VHD_DST")

	d, err := openDisk(src)
	if err != nil {
		t.Fatalf("openDisk: %v", err)
	}
	defer d.Close()
	d.allowShrink = true
	d.slack = 64 << 20 // the test volume is smaller than the usual slack
	table, err := d.Table()
	if err != nil {
		t.Fatalf("Table: %v", err)
	}
	// diskpart needs the defragmentation service, which may be off on the PC running
	// this test. Then the image is taken as it is, and testRepair covers the way back.
	shrunk := d.Shrink(func(s string) { t.Log(s) })
	m := d.Manifest()
	for _, p := range m.Partitions {
		t.Logf("source partition %d %q: %s, %s, data blocks %d", p.Number, p.Name, formatBytes(p.Size), p.Content, d.UsedBlocks(p.Number, proto.DefaultBlockSize))
	}

	path := filepath.Join(t.TempDir(), "v1.ldisk")
	f, err := os.Create(path)
	if err != nil {
		t.Fatalf("Create: %v", err)
	}
	defer f.Close()
	h := proto.Header{Kind: proto.KindDisk, BlockSize: proto.DefaultBlockSize, ID: proto.NewID()}
	w, err := proto.NewWriter(f, h, nil)
	if err != nil {
		t.Fatalf("NewWriter: %v", err)
	}
	const size = uint64(proto.DefaultBlockSize)
	for _, p := range m.Partitions {
		if p.Content != proto.ContentBlocks {
			continue
		}
		for offset := uint64(0); offset < p.Size; offset += size {
			raw := make([]byte, min(size, p.Size-offset))
			used, err := d.ReadBlock(p.Number, offset, raw)
			if err != nil {
				t.Fatalf("ReadBlock: %v", err)
			}
			if used {
				if err := w.Add(p.Number, uint32(offset/size), raw); err != nil {
					t.Fatalf("Add: %v", err)
				}
			}
		}
	}
	if _, err := w.Close(m); err != nil {
		t.Fatalf("Close: %v", err)
	}
	info, _ := f.Stat()
	t.Logf("image: %s", formatBytes(uint64(info.Size())))
	if shrunk {
		if err := d.Repair(table); err != nil {
			t.Fatalf("Repair: %v", err)
		}
		checkRestored(t, src, table)
		t.Log("source volume restored")
	}
	testRepair(t, d, table)
	file, err := proto.OpenFile(f, info.Size())
	if err != nil {
		t.Fatalf("OpenFile: %v", err)
	}
	chain, err := proto.NewChain(file)
	if err != nil {
		t.Fatalf("NewChain: %v", err)
	}

	target, err := openTargetNumber(dst)
	if err != nil {
		t.Fatalf("openTargetNumber: %v", err)
	}
	defer target.Close()
	t.Log(target.Describe())
	fitted, err := target.Fit(m)
	if err != nil {
		t.Fatalf("Fit: %v", err)
	}
	for _, p := range fitted.Partitions {
		t.Logf("target partition %d: offset %s, size %s", p.Number, formatBytes(p.Offset), formatBytes(p.Size))
	}
	if err := target.Prepare(fitted); err != nil {
		t.Fatalf("Prepare: %v", err)
	}
	sizes := map[uint32]uint64{}
	for _, p := range m.Partitions {
		sizes[p.Number] = p.Size
	}
	for _, e := range chain.Index().Entries {
		offset := uint64(e.Position) * size
		data := make([]byte, min(size, sizes[e.Partition]-offset))
		if !e.Zero() {
			if data, err = chain.Read(e); err != nil {
				t.Fatalf("Read: %v", err)
			}
		}
		if err := target.WriteBlock(e.Partition, offset, data); err != nil {
			t.Fatalf("WriteBlock partition %d offset %d: %v", e.Partition, offset, err)
		}
	}
	if err := target.Finish(); err != nil {
		t.Fatalf("Finish: %v", err)
	}
	if err := target.Close(); err != nil { // MakeBootable is a no-op outside WinPE
		t.Fatalf("Close: %v", err)
	}
	if err := target.Grow(fitted); err != nil {
		t.Fatalf("Grow: %v", err)
	}
	t.Log("deployed")

	// The table on the target must be the one of the source, GUIDs included: the
	// boot configuration of Windows refers to them.
	after, err := openDisk(dst)
	if err != nil {
		t.Fatalf("reopen target: %v", err)
	}
	defer after.Close()
	got := after.Manifest()
	if got.DiskGUID != m.DiskGUID {
		t.Fatalf("disk guid %s, want %s", got.DiskGUID, m.DiskGUID)
	}
	if len(got.Partitions) != len(fitted.Partitions) {
		t.Fatalf("%d partitions, want %d", len(got.Partitions), len(fitted.Partitions))
	}
	for i, p := range fitted.Partitions {
		g := got.Partitions[i]
		if g.Offset != p.Offset || g.Size != p.Size || g.GUID != p.GUID || g.TypeGUID != p.TypeGUID || g.Name != p.Name {
			t.Fatalf("partition %d: got %+v, want %+v", i, g, p)
		}
	}
	t.Log("partition table matches")

	// Partitions copied whole must match byte for byte, read back from the disk.
	for i, p := range m.Partitions {
		if p.Content != proto.ContentBlocks || d.maps[p.Number] != nil {
			continue
		}
		number := got.Partitions[i].Number
		want := make([]byte, size)
		have := make([]byte, size)
		for offset := uint64(0); offset < p.Size; offset += size {
			n := min(size, p.Size-offset)
			if _, err := d.ReadBlock(p.Number, offset, want[:n]); err != nil {
				t.Fatalf("read source: %v", err)
			}
			if _, err := after.ReadBlock(number, offset, have[:n]); err != nil {
				t.Fatalf("read target: %v", err)
			}
			if !bytes.Equal(want[:n], have[:n]) {
				t.Fatalf("partition %d differs at offset %d", p.Number, offset)
			}
		}
		t.Logf("partition %d (%s) matches byte for byte", p.Number, formatBytes(p.Size))
	}
}

// TestVHDFormat wipes a virtual disk into one volume and reads the table back.
func TestVHDFormat(t *testing.T) {
	n := diskFromEnv(t, "LOCLESS_VHD_FORMAT")
	d, err := newWriteDisk(n)
	if err != nil {
		t.Fatalf("newWriteDisk: %v", err)
	}
	d.allowFormat = true
	if err := d.Format(proto.FormatLabel); err != nil {
		t.Fatalf("Format: %v", err)
	}
	table, err := readTable(n)
	if err != nil {
		t.Fatalf("readTable: %v", err)
	}
	style, entries, err := parseLayout(table)
	if err != nil {
		t.Fatalf("parseLayout: %v", err)
	}
	if style != partitionStyleGPT || len(entries) != 1 {
		t.Fatalf("style %d, partitions %+v", style, entries)
	}
	if e := entries[0]; e.typeGUID == typeESP || e.typeGUID == typeMSR {
		t.Fatalf("partition %+v", e)
	}
	t.Logf("one partition of %s on a disk of %s", formatBytes(entries[0].size), formatBytes(d.size))
}
