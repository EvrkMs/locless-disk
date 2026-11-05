package main

import (
	"encoding/binary"
	"errors"
	"fmt"
	"os"
	"syscall"
	"unicode/utf16"
	"unsafe"

	"fogs/proto"
)

// Device control codes, all read-only queries.
const (
	ioctlDriveLayoutEx  = 0x00070050 // IOCTL_DISK_GET_DRIVE_LAYOUT_EX
	ioctlGeometryEx     = 0x000700a0 // IOCTL_DISK_GET_DRIVE_GEOMETRY_EX
	fsctlNTFSVolumeData = 0x00090064 // FSCTL_GET_NTFS_VOLUME_DATA
	fsctlVolumeBitmap   = 0x0009006f // FSCTL_GET_VOLUME_BITMAP

	partitionStyleGPT = 1

	layoutEntriesAt = 48  // offset of the entries in DRIVE_LAYOUT_INFORMATION_EX with GPT
	layoutEntrySize = 144 // sizeof(PARTITION_INFORMATION_EX)

	maxDisks = 16
)

// GPT partition types the capture treats specially.
const (
	typeESP = "c12a7328-f81f-11d2-ba4b-00a0c93ec93b"
	typeMSR = "e3c9e316-0b5c-4db8-817d-f92df00215ae"
)

// volumeMap says which clusters of an NTFS partition are in use.
type volumeMap struct {
	cluster uint64
	total   uint64 // clusters; anything past them, like the backup boot sector, is kept
	bitmap  proto.Bitmap
}

// physicalDisk reads one disk. The handle is opened for reading only; the only
// change to the disk comes from Shrink and Repair, see shrink_windows.go.
type physicalDisk struct {
	number   int
	file     *os.File
	size     uint64
	manifest proto.Manifest
	maps     map[uint32]*volumeMap // nil entry: read the partition whole
	starts   map[uint32]uint64

	allowShrink bool   // tests on a VHD; otherwise only WinPE may shrink
	original    uint64 // size of the fill partition before the shrink
	slack       uint64 // free space to leave, 0 means the default
}

// openSystemDisk finds the disk Windows boots from: the first GPT disk with an
// EFI system partition.
func openSystemDisk() (source, error) {
	var last error
	for n := range maxDisks {
		d, err := openDisk(n)
		if err != nil {
			if last == nil && !errors.Is(err, os.ErrNotExist) {
				last = err
			}
			continue
		}
		return d, nil
	}
	if last != nil {
		return nil, last
	}
	return nil, errors.New("нет GPT-диска с разделом EFI")
}

func openHandle(path string) (syscall.Handle, error) {
	name, err := syscall.UTF16PtrFromString(path)
	if err != nil {
		return 0, err
	}
	return syscall.CreateFile(name, syscall.GENERIC_READ, syscall.FILE_SHARE_READ|syscall.FILE_SHARE_WRITE,
		nil, syscall.OPEN_EXISTING, 0, 0)
}

// control runs a query into a fresh buffer of the given size.
func control(h syscall.Handle, code uint32, in []byte, size int) ([]byte, error) {
	out := make([]byte, size)
	var inPtr *byte
	if len(in) > 0 {
		inPtr = &in[0]
	}
	var n uint32
	if err := syscall.DeviceIoControl(h, code, inPtr, uint32(len(in)), &out[0], uint32(size), &n, nil); err != nil {
		return nil, err
	}
	return out[:n], nil
}

func openDisk(number int) (*physicalDisk, error) {
	h, err := openHandle(fmt.Sprintf(`\\.\PhysicalDrive%d`, number))
	if err != nil {
		return nil, err
	}
	d := &physicalDisk{
		number: number,
		file:   os.NewFile(uintptr(h), fmt.Sprintf("PhysicalDrive%d", number)),
		maps:   map[uint32]*volumeMap{},
		starts: map[uint32]uint64{},
	}
	if err := d.load(h); err != nil {
		d.file.Close()
		return nil, err
	}
	return d, nil
}

// load reads the size and the partition table.
func (d *physicalDisk) load(h syscall.Handle) error {
	geometry, err := control(h, ioctlGeometryEx, nil, 256)
	if err != nil {
		return fmt.Errorf("геометрия диска %d: %w", d.number, err)
	}
	d.size = binary.LittleEndian.Uint64(geometry[24:])
	sector := binary.LittleEndian.Uint32(geometry[20:])

	layout, err := control(h, ioctlDriveLayoutEx, nil, layoutEntriesAt+layoutEntrySize*128)
	if err != nil {
		return fmt.Errorf("таблица разделов диска %d: %w", d.number, err)
	}
	if binary.LittleEndian.Uint32(layout[0:]) != partitionStyleGPT {
		return fmt.Errorf("диск %d не GPT: %w", d.number, os.ErrNotExist)
	}
	count := int(binary.LittleEndian.Uint32(layout[4:]))
	d.manifest = proto.Manifest{
		Version:    proto.ManifestVersion,
		DiskSize:   d.size,
		SectorSize: sector,
		DiskGUID:   formatGUID(layout[8:24]),
	}

	hasESP := false
	var largest *proto.Partition
	for i := range count {
		e := layout[layoutEntriesAt+i*layoutEntrySize:]
		number := binary.LittleEndian.Uint32(e[24:])
		size := binary.LittleEndian.Uint64(e[16:])
		if number == 0 || size == 0 {
			continue // unused slot of the table
		}
		start := binary.LittleEndian.Uint64(e[8:])
		p := proto.Partition{
			Number:     number,
			TypeGUID:   formatGUID(e[32:48]),
			GUID:       formatGUID(e[48:64]),
			Attributes: binary.LittleEndian.Uint64(e[64:]),
			Name:       utf16Name(e[72:144]),
			Offset:     start,
			Size:       size,
			Policy:     proto.PolicyFixed,
			Content:    proto.ContentBlocks,
		}
		d.starts[number] = start
		switch p.TypeGUID {
		case typeESP:
			hasESP = true // FAT clusters do not start at offset 0: read it whole
		case typeMSR:
			p.Content = proto.ContentNone
		default:
			d.maps[number] = d.volumeMap(number)
		}
		d.manifest.Partitions = append(d.manifest.Partitions, p)
	}
	if !hasESP {
		return fmt.Errorf("на диске %d нет раздела EFI: %w", d.number, os.ErrNotExist)
	}
	sortPartitions(d.manifest.Partitions)
	for i := range d.manifest.Partitions {
		p := &d.manifest.Partitions[i]
		if d.maps[p.Number] != nil && (largest == nil || p.Size > largest.Size) {
			largest = p
		}
	}
	if largest != nil {
		largest.Policy = proto.PolicyFill
	}
	return d.manifest.Validate()
}

// tableEntry is one partition of a table in the form Windows returns it.
type tableEntry struct {
	number      uint32
	start, size uint64
	typeGUID    string
	guid        string
}

// parseLayout reads a DRIVE_LAYOUT_INFORMATION_EX, which may have come from the
// server. The entries of an MBR disk carry no GUIDs but sit in the same place.
func parseLayout(b []byte) (uint32, []tableEntry, error) {
	if len(b) < layoutEntriesAt {
		return 0, nil, errors.New("короткая таблица разделов")
	}
	count := int(binary.LittleEndian.Uint32(b[4:]))
	if count > maxTablePartitions || len(b) < layoutEntriesAt+count*layoutEntrySize {
		return 0, nil, errors.New("повреждённая таблица разделов")
	}
	var entries []tableEntry
	for i := range count {
		e := b[layoutEntriesAt+i*layoutEntrySize:]
		number := binary.LittleEndian.Uint32(e[24:])
		size := binary.LittleEndian.Uint64(e[16:])
		if number == 0 || size == 0 {
			continue // unused slot of the table
		}
		entries = append(entries, tableEntry{
			number: number, start: binary.LittleEndian.Uint64(e[8:]), size: size,
			typeGUID: formatGUID(e[32:48]), guid: formatGUID(e[48:64]),
		})
	}
	return binary.LittleEndian.Uint32(b[0:]), entries, nil
}

// Table returns the partition table as Windows gives it, ready to be written back.
func (d *physicalDisk) Table() ([]byte, error) {
	layout, err := control(syscall.Handle(d.file.Fd()), ioctlDriveLayoutEx, nil, layoutEntriesAt+layoutEntrySize*maxTablePartitions)
	if err != nil {
		return nil, fmt.Errorf("таблица разделов диска %d: %w", d.number, err)
	}
	if len(layout) < layoutEntriesAt {
		return nil, errors.New("короткая таблица разделов")
	}
	count := int(binary.LittleEndian.Uint32(layout[4:]))
	return layout[:min(len(layout), layoutEntriesAt+layoutEntrySize*count)], nil
}

// sortPartitions orders partitions by their place on the disk, as the manifest requires.
func sortPartitions(p []proto.Partition) {
	for i := 1; i < len(p); i++ {
		for j := i; j > 0 && p[j].Offset < p[j-1].Offset; j-- {
			p[j], p[j-1] = p[j-1], p[j]
		}
	}
}

// volumeMap asks NTFS which clusters it uses. It returns nil if the partition is
// not readable NTFS, so an unknown file system is copied whole and nothing is lost.
func (d *physicalDisk) volumeMap(partition uint32) *volumeMap {
	h, err := openHandle(fmt.Sprintf(`\\?\GLOBALROOT\Device\Harddisk%d\Partition%d`, d.number, partition))
	if err != nil {
		return nil
	}
	defer syscall.CloseHandle(h)

	data, err := control(h, fsctlNTFSVolumeData, nil, 512)
	if err != nil || len(data) < 48 {
		return nil
	}
	total := binary.LittleEndian.Uint64(data[16:])
	cluster := uint64(binary.LittleEndian.Uint32(data[44:]))
	if total == 0 || cluster == 0 {
		return nil
	}

	start := make([]byte, 8) // STARTING_LCN_INPUT_BUFFER: from cluster 0
	out, err := control(h, fsctlVolumeBitmap, start, 16+int(total/8)+64)
	if err != nil || len(out) < 16 {
		return nil
	}
	if first := binary.LittleEndian.Uint64(out[0:]); first != 0 {
		return nil
	}
	bits := make([]byte, (total+7)/8)
	if copy(bits, out[16:]) != len(bits) {
		return nil
	}
	if r := total % 8; r != 0 {
		bits[len(bits)-1] &= byte(1<<r) - 1 // Windows may set the padding bits
	}
	bitmap, err := proto.ParseBitmap(bits, total)
	if err != nil {
		return nil
	}
	return &volumeMap{cluster: cluster, total: total, bitmap: bitmap}
}

// used reports whether any data lies in a range of the partition.
func (v *volumeMap) used(offset, length uint64) bool {
	first := offset / v.cluster
	last := (offset + length - 1) / v.cluster
	for c := first; c <= last; c++ {
		if c >= v.total || v.bitmap.Has(c) {
			return true
		}
	}
	return false
}

func (d *physicalDisk) Describe() string {
	return fmt.Sprintf("№%d, %s, разделов: %d", d.number, formatBytes(d.size), len(d.manifest.Partitions))
}

func (d *physicalDisk) Manifest() proto.Manifest { return d.manifest }

func (d *physicalDisk) Close() error { return d.file.Close() }

func (d *physicalDisk) UsedBlocks(partition uint32, blockSize uint32) uint64 {
	for _, p := range d.manifest.Partitions {
		if p.Number != partition {
			continue
		}
		size := uint64(blockSize)
		blocks := (p.Size + size - 1) / size
		v := d.maps[partition]
		if v == nil {
			return blocks
		}
		var n uint64
		for i := range blocks {
			offset := i * size
			if v.used(offset, min(size, p.Size-offset)) {
				n++
			}
		}
		return n
	}
	return 0
}

func (d *physicalDisk) ReadBlock(partition uint32, offset uint64, dst []byte) (bool, error) {
	v := d.maps[partition]
	length := uint64(len(dst))
	if v != nil && !v.used(offset, length) {
		return false, nil
	}
	if _, err := d.file.ReadAt(dst, int64(d.starts[partition]+offset)); err != nil {
		return false, err
	}
	if v == nil {
		return true, nil
	}
	// Free clusters hold leftovers that would make equal blocks look different.
	for pos := uint64(0); pos < length; {
		c := (offset + pos) / v.cluster
		end := min((c+1)*v.cluster-offset, length)
		if c < v.total && !v.bitmap.Has(c) {
			clear(dst[pos:end])
		}
		pos = end
	}
	return true, nil
}

// formatGUID prints a Windows GUID, whose first three fields are little-endian.
func formatGUID(b []byte) string {
	return fmt.Sprintf("%08x-%04x-%04x-%02x%02x-%02x%02x%02x%02x%02x%02x",
		binary.LittleEndian.Uint32(b[0:]), binary.LittleEndian.Uint16(b[4:]), binary.LittleEndian.Uint16(b[6:]),
		b[8], b[9], b[10], b[11], b[12], b[13], b[14], b[15])
}

// utf16Name decodes the fixed-size GPT name field.
func utf16Name(b []byte) string {
	units := unsafe.Slice((*uint16)(unsafe.Pointer(&b[0])), len(b)/2)
	for i, u := range units {
		if u == 0 {
			units = units[:i]
			break
		}
	}
	return string(utf16.Decode(units))
}
