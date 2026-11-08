package main

import (
	"errors"
	"fmt"
	"os"
	"strings"
	"syscall"

	"fogs/proto"
)

// shrinkSlack is the free space left in a shrunk volume. FOG leaves at least 500 MB.
const (
	shrinkSlackMin     = 1 << 30
	shrinkSlackPercent = 10
)

// Files Windows recreates by itself. FOG removes them too: they are large, useless
// in an image, and cannot be moved out of the way of a shrink.
var swapFiles = []string{"pagefile.sys", "hiberfil.sys", "swapfile.sys"}

// Shrink repeats what FOG does before a capture: it makes the Windows volume as
// small as its data, so the image fits any disk that can hold that data. It runs
// only in WinPE, never on a live Windows. A volume that cannot be shrunk is
// captured as it is, as in FOG: the clone still works, but needs a larger disk.
// Repair puts the volume back.
func (d *physicalDisk) Shrink(log func(string)) bool {
	if !d.mayChange() {
		return false
	}
	var fill *proto.Partition
	for i := range d.manifest.Partitions {
		if p := &d.manifest.Partitions[i]; p.Policy == proto.PolicyFill {
			fill = p
		}
	}
	if fill == nil || d.maps[fill.Number] == nil {
		log("сжатие: нет тома NTFS, диск снимается как есть")
		return false
	}
	number, original := fill.Number, fill.Size

	root := partitionPath(d.number, number)
	for _, name := range swapFiles {
		if err := os.Remove(root + `\` + name); err == nil {
			log("удалён " + name)
		} else if !errors.Is(err, os.ErrNotExist) {
			log("не удалён " + name + ": " + err.Error())
		}
	}
	if err := d.settle(number); err != nil {
		log("сжатие: " + err.Error())
		return false
	}
	if err := d.reload(number); err != nil {
		log("сжатие: " + err.Error())
		return false
	}

	v := d.maps[number]
	if v == nil {
		log("сжатие: том не читается, диск снимается как есть")
		return false
	}
	used := v.bitmap.Count() * v.cluster
	slack := uint64(shrinkSlackMin)
	if d.slack != 0 {
		slack = d.slack
	}
	target := used + max(slack, used/100*shrinkSlackPercent)
	if target >= original {
		log("сжатие: том уже почти полон, сжимать нечего")
		return false
	}
	d.original = original
	size, summary, err := shrinkVolume(diskVolume{d, number}, target, log)
	if err != nil {
		log("сжатие: " + err.Error())
		return true // nobody knows what the disk looks like: Repair will find out
	}
	if size >= original {
		log("сжатие не удалось, диск снимается как есть: " + summary)
		return false
	}
	log("том сжат: " + summary)
	return true
}

// diskVolume is the Windows volume of a partition, shrunk by diskpart.
type diskVolume struct {
	d      *physicalDisk
	number uint32
}

// size reads the result from the disk. A shrink changes file system data still
// held in the cache, and a clone reads past the cache, so the volume is settled
// first.
func (v diskVolume) size() (uint64, error) {
	if err := v.d.settle(v.number); err != nil {
		return 0, err
	}
	if err := v.d.reload(v.number); err != nil {
		return 0, err
	}
	for _, p := range v.d.manifest.Partitions {
		if p.Number == v.number {
			return p.Size, nil
		}
	}
	return 0, fmt.Errorf("раздел %d пропал", v.number)
}

func (v diskVolume) reclaimable() (uint64, bool, error) {
	out, err := diskpartOutput(fmt.Sprintf("select disk %d", v.d.number), fmt.Sprintf("select partition %d", v.number),
		"shrink querymax")
	if err != nil {
		return 0, false, err
	}
	bytes, ok := parseReclaimable(out)
	return bytes, ok, nil
}

// shrinkBy asks for mib and accepts less: minimum lets diskpart shrink by what it
// can instead of failing outright.
func (v diskVolume) shrinkBy(mib uint64) error {
	return runDiskpart(fmt.Sprintf("select disk %d", v.d.number), fmt.Sprintf("select partition %d", v.number),
		fmt.Sprintf("shrink desired=%d minimum=1", mib))
}

// mayChange says whether this code may touch the disk: only WinPE may, never a
// working Windows.
func (d *physicalDisk) mayChange() bool {
	return d.allowShrink || strings.EqualFold(os.Getenv("SystemDrive"), "X:")
}

// settle flushes a volume to the disk by dismounting it. Windows mounts it again
// on the next access.
func (d *physicalDisk) settle(number uint32) error {
	h, err := openWritable(partitionPath(d.number, number))
	if err != nil {
		return err
	}
	defer syscall.CloseHandle(h)
	return takeVolume(h)
}

// reload reads the table and the cluster map again, keeping the chosen partition
// as the one that fills the disk and recording its size before the shrink.
func (d *physicalDisk) reload(fill uint32) error {
	d.maps = map[uint32]*volumeMap{}
	d.starts = map[uint32]uint64{}
	d.manifest = proto.Manifest{}
	if err := d.load(syscall.Handle(d.file.Fd())); err != nil {
		return err
	}
	for i := range d.manifest.Partitions {
		p := &d.manifest.Partitions[i]
		p.Policy = proto.PolicyFixed
		if p.Number == fill {
			p.Policy = proto.PolicyFill
			p.OriginalSize = d.original
		}
	}
	return nil
}

// Repair gives the volume back its size in two steps, as FOG does after a capture:
// the saved table goes back on the disk as it was, to the byte, and then the file
// system is stretched over the partition. Both are checked by reading the disk, and
// a step already done is skipped, so this also finishes a repair cut short.
func (d *physicalDisk) Repair(table []byte) error {
	if !d.mayChange() {
		return nil
	}
	_, want, err := parseLayout(table)
	if err != nil {
		return fmt.Errorf("%w: %v", errStaleTable, err)
	}
	current, err := d.Table()
	if err != nil {
		return err
	}
	_, now, err := parseLayout(current)
	if err != nil {
		return err
	}
	changed, err := compareTables(want, now)
	if err != nil {
		return err
	}
	fill := largestVolume(want)
	if fill == nil {
		return fmt.Errorf("%w: в ней нет тома", errStaleTable)
	}
	d.original = fill.size

	if changed {
		if err := d.writeTable(table, want); err != nil {
			return err
		}
	}
	if err := d.reload(fill.number); err != nil {
		return err
	}
	if d.filled(fill) {
		return nil
	}
	err = runDiskpart(fmt.Sprintf("select disk %d", d.number), "rescan",
		fmt.Sprintf("select partition %d", fill.number), "extend filesystem")
	if err != nil {
		return err
	}
	if err := d.reload(fill.number); err != nil {
		return err
	}
	if !d.filled(fill) {
		return fmt.Errorf("файловая система не заняла раздел %d", fill.number)
	}
	return nil
}

// compareTables reports whether the disk differs from the saved table. Only sizes
// may differ, and a partition may only have got smaller: anything else means the
// table is about some other layout, and writing it would destroy that layout.
func compareTables(want, now []tableEntry) (bool, error) {
	if len(want) != len(now) {
		return false, errStaleTable
	}
	changed := false
	for i, w := range want {
		n := now[i]
		if w.number != n.number || w.start != n.start || w.guid != n.guid || w.typeGUID != n.typeGUID || w.size < n.size {
			return false, errStaleTable
		}
		changed = changed || w.size != n.size
	}
	return changed, nil
}

// largestVolume finds the partition a shrink works on, as the capture chooses it.
func largestVolume(entries []tableEntry) *tableEntry {
	var largest *tableEntry
	for i := range entries {
		e := &entries[i]
		if e.typeGUID != typeESP && e.typeGUID != typeMSR && (largest == nil || e.size > largest.size) {
			largest = e
		}
	}
	return largest
}

// filled reports whether the file system reaches the end of the partition. NTFS
// leaves the last sectors for the copy of its boot sector.
func (d *physicalDisk) filled(p *tableEntry) bool {
	v := d.maps[p.number]
	return v != nil && p.size <= (v.total+2)*v.cluster
}
