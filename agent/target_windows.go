package main

import (
	"encoding/binary"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"
	"time"
	"unicode/utf16"

	"fogs/proto"
)

// Control codes that change a disk. This file is the only place that uses them.
const (
	ioctlSetDriveLayoutEx = 0x0007c054 // IOCTL_DISK_SET_DRIVE_LAYOUT_EX
	ioctlCreateDisk       = 0x0007c058 // IOCTL_DISK_CREATE_DISK
	ioctlUpdateProperties = 0x00070140 // IOCTL_DISK_UPDATE_PROPERTIES
	fsctlLockVolume       = 0x00090018 // FSCTL_LOCK_VOLUME
	fsctlDismountVolume   = 0x00090020 // FSCTL_DISMOUNT_VOLUME

	ioctlStorageQuery = 0x002d1400 // IOCTL_STORAGE_QUERY_PROPERTY, a read-only query

	maxTablePartitions = 128 // GPT entries on a standard disk

	busISCSI   = 0x09
	busUSB     = 0x07
	busSD      = 0x0c
	busVirtual = 0x0f // BusTypeFileBackedVirtual, a VHD
)

// busType asks how a disk is connected.
func busType(number int) (uint32, error) {
	h, err := openHandle(fmt.Sprintf(`\\.\PhysicalDrive%d`, number))
	if err != nil {
		return 0, err
	}
	defer syscall.CloseHandle(h)
	out, err := control(h, ioctlStorageQuery, make([]byte, 12), 1024)
	if err != nil {
		return 0, err
	}
	if len(out) < 32 {
		return 0, errors.New("короткий ответ о диске")
	}
	return binary.LittleEndian.Uint32(out[28:]), nil
}

// writeDisk is the disk an image is deployed to.
type writeDisk struct {
	number      int
	size        uint64
	sector      uint32
	existing    []uint32            // partitions found before anything was changed
	layout      []byte              // table as read, for its GPT usable area
	fresh       bool                // not GPT: it gets a new, empty GPT first
	allowFormat bool                // tests on a VHD; otherwise only WinPE may format
	parts       map[uint32]*os.File // partitions kept locked while the disk is written
	disk        *os.File            // writes go here, past the size Windows remembers for a partition
	offsets     map[uint32]uint64   // where each partition starts on this disk
}

// openTarget finds the disk to overwrite, see chooseTarget. Only a disk inside the
// PC may be wiped: a network disk of CCBoot, a USB stick or a mounted VHD is
// somebody else's data.
func openTarget() (target, error) {
	number, err := chooseTarget(scanDisks())
	if err != nil {
		return nil, err
	}
	return newWriteDisk(number)
}

// scanDisks lists the disks of the PC. A disk that does not answer, like a card
// reader without a card, is left out.
func scanDisks() []diskInfo {
	var disks []diskInfo
	for n := range maxDisks {
		bus, err := busType(n)
		if err != nil {
			continue
		}
		table, err := readTable(n)
		if err != nil {
			continue
		}
		style, entries, err := parseLayout(table)
		if err != nil {
			continue
		}
		d := diskInfo{number: n, empty: len(entries) == 0}
		d.internal = bus != busISCSI && bus != busUSB && bus != busSD && bus != busVirtual
		for _, e := range entries {
			d.hasESP = d.hasESP || style == partitionStyleGPT && e.typeGUID == typeESP
		}
		disks = append(disks, d)
	}
	return disks
}

// readTable reads the partition table of a disk of any style.
func readTable(number int) ([]byte, error) {
	h, err := openHandle(fmt.Sprintf(`\\.\PhysicalDrive%d`, number))
	if err != nil {
		return nil, err
	}
	defer syscall.CloseHandle(h)
	return control(h, ioctlDriveLayoutEx, nil, layoutEntriesAt+layoutEntrySize*maxTablePartitions)
}

// openTargetNumber opens one exact disk, for tests on virtual disks.
func openTargetNumber(n int) (*writeDisk, error) {
	return newWriteDisk(n)
}

// newWriteDisk reads a disk without changing it. Any disk will do: a new one has no
// table at all, and one from another PC may have an MBR.
func newWriteDisk(number int) (*writeDisk, error) {
	h, err := openHandle(fmt.Sprintf(`\\.\PhysicalDrive%d`, number))
	if err != nil {
		return nil, err
	}
	defer syscall.CloseHandle(h)
	geometry, err := control(h, ioctlGeometryEx, nil, 256)
	if err != nil {
		return nil, fmt.Errorf("геометрия диска %d: %w", number, err)
	}
	w := &writeDisk{
		number: number, size: binary.LittleEndian.Uint64(geometry[24:]), sector: binary.LittleEndian.Uint32(geometry[20:]),
		parts: map[uint32]*os.File{}, offsets: map[uint32]uint64{},
	}
	if w.layout, err = control(h, ioctlDriveLayoutEx, nil, layoutEntriesAt+layoutEntrySize*maxTablePartitions); err != nil {
		return nil, fmt.Errorf("таблица разделов диска %d: %w", number, err)
	}
	style, entries, err := parseLayout(w.layout)
	if err != nil {
		return nil, err
	}
	w.fresh = style != partitionStyleGPT
	for _, e := range entries {
		w.existing = append(w.existing, e.number)
	}
	return w, nil
}

func (w *writeDisk) Describe() string {
	s := fmt.Sprintf("диск №%d, %s", w.number, formatBytes(w.size))
	if w.fresh {
		s += ", без GPT"
	}
	if device := describeDevice(w.number); device != "" {
		s += ", " + device
	}
	return s
}

// openWritable opens a device so it can be changed.
func openWritable(path string) (syscall.Handle, error) {
	name, err := syscall.UTF16PtrFromString(path)
	if err != nil {
		return 0, err
	}
	return syscall.CreateFile(name, syscall.GENERIC_READ|syscall.GENERIC_WRITE,
		syscall.FILE_SHARE_READ|syscall.FILE_SHARE_WRITE, nil, syscall.OPEN_EXISTING, 0, 0)
}

func partitionPath(disk int, partition uint32) string {
	return fmt.Sprintf(`\\?\GLOBALROOT\Device\Harddisk%d\Partition%d`, disk, partition)
}

// takeVolume locks a partition and detaches its file system. Windows refuses
// writes to a mounted volume, and only the handle holding the lock may write.
func takeVolume(h syscall.Handle) error {
	var lastErr error
	for range 20 { // something may still be closing files on it
		if _, lastErr = control(h, fsctlLockVolume, nil, 1); lastErr == nil {
			break
		}
		time.Sleep(250 * time.Millisecond)
	}
	if lastErr != nil {
		return fmt.Errorf("том занят: %w", lastErr)
	}
	if _, err := control(h, fsctlDismountVolume, nil, 1); err != nil {
		return fmt.Errorf("том не отключается: %w", err)
	}
	return nil
}

// holdPartitions takes partitions from Windows until the returned function is
// called. It may be called more than once.
func holdPartitions(disk int, numbers []uint32) (func(), error) {
	var held []syscall.Handle
	release := func() {
		for _, h := range held {
			syscall.CloseHandle(h)
		}
		held = nil
	}
	for _, number := range numbers {
		h, err := openWritable(partitionPath(disk, number))
		if err != nil {
			continue // a partition without a device has nothing mounted
		}
		held = append(held, h)
		if err := takeVolume(h); err != nil {
			release()
			return nil, fmt.Errorf("раздел %d: %w", number, err)
		}
	}
	return release, nil
}

// writeTable puts a saved table back on the disk, with the volumes taken from
// Windows so that nothing sees the partitions change under it.
func (d *physicalDisk) writeTable(table []byte, entries []tableEntry) error {
	numbers := make([]uint32, len(entries))
	for i, e := range entries {
		numbers[i] = e.number
	}
	release, err := holdPartitions(d.number, numbers)
	if err != nil {
		return err
	}
	defer release()
	disk, err := openWritable(fmt.Sprintf(`\\.\PhysicalDrive%d`, d.number))
	if err != nil {
		return err
	}
	defer syscall.CloseHandle(disk)

	out := append([]byte(nil), table...)
	for i := range int(binary.LittleEndian.Uint32(out[4:])) {
		out[layoutEntriesAt+i*layoutEntrySize+28] = 1 // RewritePartition
	}
	if _, err := control2(disk, ioctlSetDriveLayoutEx, out); err != nil {
		return fmt.Errorf("таблица разделов: %w", err)
	}
	if _, err := control2(disk, ioctlUpdateProperties, nil); err != nil {
		return fmt.Errorf("обновление диска: %w", err)
	}
	return nil
}

// emptyGPT writes a GPT with no partitions and returns the table Windows then reports.
func emptyGPT(disk syscall.Handle, id []byte) ([]byte, error) {
	in := make([]byte, 24) // CREATE_DISK with CREATE_DISK_GPT
	binary.LittleEndian.PutUint32(in[0:], partitionStyleGPT)
	copy(in[4:20], id)
	binary.LittleEndian.PutUint32(in[20:], maxTablePartitions)
	if _, err := control2(disk, ioctlCreateDisk, in); err != nil {
		return nil, fmt.Errorf("создание GPT: %w", err)
	}
	if _, err := control2(disk, ioctlUpdateProperties, nil); err != nil {
		return nil, fmt.Errorf("обновление диска: %w", err)
	}
	layout, err := control(disk, ioctlDriveLayoutEx, nil, layoutEntriesAt+layoutEntrySize*maxTablePartitions)
	if err != nil {
		return nil, err
	}
	if binary.LittleEndian.Uint32(layout[0:]) != partitionStyleGPT {
		return nil, errors.New("создание GPT: диск остался без GPT")
	}
	return layout, nil
}

// Format wipes the disk. The GPT comes from a request and not from diskpart, whose
// convert gpt adds a reserved partition: there must be one partition and no other.
func (w *writeDisk) Format(label string) error {
	if !w.allowFormat && !strings.EqualFold(os.Getenv("SystemDrive"), "X:") {
		return errors.New("форматирование работает только в WinPE")
	}
	if err := runDiskpart(fmt.Sprintf("select disk %d", w.number), "clean"); err != nil {
		return err
	}
	disk, err := openWritable(fmt.Sprintf(`\\.\PhysicalDrive%d`, w.number))
	if err != nil {
		return err
	}
	id := proto.NewID()
	_, err = emptyGPT(disk, id[:])
	syscall.CloseHandle(disk)
	if err != nil {
		return err
	}
	return runDiskpart(fmt.Sprintf("select disk %d", w.number), "rescan", "create partition primary",
		"format fs=ntfs quick label="+label)
}

// createGPT puts an empty GPT on a disk that has no such table, and reads the
// usable area Windows gave it.
func (w *writeDisk) createGPT(disk syscall.Handle, m proto.Manifest) error {
	layout, err := emptyGPT(disk, parseGUID(m.DiskGUID))
	if err != nil {
		return err
	}
	first := binary.LittleEndian.Uint64(layout[24:])
	end := first + binary.LittleEndian.Uint64(layout[32:])
	for _, p := range m.Partitions {
		if p.Offset < first || p.Offset+p.Size > end {
			return fmt.Errorf("раздел %d не лёг в область GPT", p.Number)
		}
	}
	w.layout, w.fresh = layout, false
	return nil
}

// Prepare takes every partition from Windows, writes the table of the image and
// locks the new partitions for writing.
func (w *writeDisk) Prepare(m proto.Manifest) error {
	release, err := holdPartitions(w.number, w.existing)
	if err != nil {
		return err
	}
	defer release()

	disk, err := openWritable(fmt.Sprintf(`\\.\PhysicalDrive%d`, w.number))
	if err != nil {
		return err
	}
	defer syscall.CloseHandle(disk)
	if w.fresh {
		if err := w.createGPT(disk, m); err != nil {
			return err
		}
	}
	if _, err := control2(disk, ioctlSetDriveLayoutEx, buildLayout(w.layout, m)); err != nil {
		return fmt.Errorf("таблица разделов: %w", err)
	}
	if _, err := control2(disk, ioctlUpdateProperties, nil); err != nil {
		return fmt.Errorf("обновление диска: %w", err)
	}
	release()

	if err := w.lockNew(disk, m); err != nil {
		return err
	}
	// Windows keeps the old size of a partition that starts where it did before,
	// and refuses writes past it. The disk itself has no such limit, and writing
	// to it is allowed because the partitions are locked and dismounted.
	h, err := openWritable(fmt.Sprintf(`\\.\PhysicalDrive%d`, w.number))
	if err != nil {
		return err
	}
	w.disk = os.NewFile(uintptr(h), fmt.Sprintf("PhysicalDrive%d", w.number))
	for _, p := range m.Partitions {
		w.offsets[p.Number] = p.Offset
	}
	return nil
}

// control2 runs a request that returns nothing.
func control2(h syscall.Handle, code uint32, in []byte) ([]byte, error) {
	var inPtr *byte
	if len(in) > 0 {
		inPtr = &in[0]
	}
	var n uint32
	return nil, syscall.DeviceIoControl(h, code, inPtr, uint32(len(in)), nil, 0, &n, nil)
}

// lockNew finds the partitions of the new table, which Windows may have
// renumbered, and locks those the image fills.
func (w *writeDisk) lockNew(disk syscall.Handle, m proto.Manifest) error {
	layout, err := control(disk, ioctlDriveLayoutEx, nil, layoutEntriesAt+layoutEntrySize*maxTablePartitions)
	if err != nil {
		return err
	}
	byOffset := map[uint64]uint32{}
	count := int(binary.LittleEndian.Uint32(layout[4:]))
	for i := range count {
		e := layout[layoutEntriesAt+i*layoutEntrySize:]
		if n := binary.LittleEndian.Uint32(e[24:]); n != 0 {
			byOffset[binary.LittleEndian.Uint64(e[8:])] = n
		}
	}
	for _, p := range m.Partitions {
		if p.Content != proto.ContentBlocks {
			continue
		}
		number, ok := byOffset[p.Offset]
		if !ok {
			return fmt.Errorf("раздел со смещением %d не появился в таблице", p.Offset)
		}
		f, err := w.openPartition(number)
		if err != nil {
			return fmt.Errorf("раздел %d: %w", p.Number, err)
		}
		w.parts[p.Number] = f
	}
	return nil
}

// openPartition waits for the device of a new partition and takes it.
func (w *writeDisk) openPartition(number uint32) (*os.File, error) {
	var h syscall.Handle
	var err error
	for range 40 { // Windows creates the devices shortly after the table changes
		if h, err = openWritable(partitionPath(w.number, number)); err == nil {
			break
		}
		time.Sleep(250 * time.Millisecond)
	}
	if err != nil {
		return nil, err
	}
	if err := takeVolume(h); err != nil {
		syscall.CloseHandle(h)
		return nil, err
	}
	return os.NewFile(uintptr(h), "Partition"+strconv.Itoa(int(number))), nil
}

// buildLayout makes the table of the image, keeping the usable area of the disk.
// GUIDs come from the image: the boot configuration refers to them.
func buildLayout(current []byte, m proto.Manifest) []byte {
	b := make([]byte, layoutEntriesAt+layoutEntrySize*len(m.Partitions))
	binary.LittleEndian.PutUint32(b[0:], partitionStyleGPT)
	binary.LittleEndian.PutUint32(b[4:], uint32(len(m.Partitions)))
	copy(b[8:24], parseGUID(m.DiskGUID))
	copy(b[24:40], current[24:40]) // StartingUsableOffset, UsableLength
	binary.LittleEndian.PutUint32(b[40:], maxTablePartitions)
	for i, p := range m.Partitions {
		e := b[layoutEntriesAt+i*layoutEntrySize:]
		binary.LittleEndian.PutUint32(e[0:], partitionStyleGPT)
		binary.LittleEndian.PutUint64(e[8:], p.Offset)
		binary.LittleEndian.PutUint64(e[16:], p.Size)
		binary.LittleEndian.PutUint32(e[24:], uint32(i+1))
		e[28] = 1 // RewritePartition
		copy(e[32:48], parseGUID(p.TypeGUID))
		copy(e[48:64], parseGUID(p.GUID))
		binary.LittleEndian.PutUint64(e[64:], p.Attributes)
		for j, u := range utf16.Encode([]rune(p.Name)) {
			if j == 36 {
				break
			}
			binary.LittleEndian.PutUint16(e[72+j*2:], u)
		}
	}
	return b
}

// parseGUID is the reverse of formatGUID.
func parseGUID(s string) []byte {
	hex := strings.ReplaceAll(s, "-", "")
	b := make([]byte, 16)
	for i := range b {
		v, _ := strconv.ParseUint(hex[i*2:i*2+2], 16, 8)
		b[i] = byte(v)
	}
	b[0], b[1], b[2], b[3] = b[3], b[2], b[1], b[0]
	b[4], b[5] = b[5], b[4]
	b[6], b[7] = b[7], b[6]
	return b
}

func (w *writeDisk) WriteBlock(partition uint32, offset uint64, data []byte) error {
	if w.parts[partition] == nil || w.disk == nil {
		return fmt.Errorf("раздел %d не подготовлен", partition)
	}
	_, err := w.disk.WriteAt(data, int64(w.offsets[partition]+offset))
	return err
}

// Finish flushes the disk. Close then gives the partitions back to Windows.
func (w *writeDisk) Finish() error {
	if w.disk == nil {
		return errors.New("диск не подготовлен")
	}
	return w.disk.Sync()
}

// windowsLetter is where MakeBootable mounts the Windows partition.
const windowsLetter = "W"

// SetName writes the computer name into the registry of the Windows on the disk,
// through the letter MakeBootable gave it. Like MakeBootable it acts only inside WinPE.
func (w *writeDisk) SetName(name string) (err error) {
	if !strings.EqualFold(os.Getenv("SystemDrive"), "X:") {
		return nil
	}
	const hive = `HKLM\LOCLESS`
	if err := runReg("load", hive, windowsLetter+`:\Windows\System32\config\SYSTEM`); err != nil {
		return err
	}
	defer func() { err = errors.Join(err, runReg("unload", hive)) }()

	out, err := exec.Command("reg", "query", hive+`\Select`, "/v", "Current").CombinedOutput()
	if err != nil {
		return fmt.Errorf("reg query: %w: %s", err, lastLine(out))
	}
	set, err := controlSet(string(out))
	if err != nil {
		return err
	}
	for _, v := range [][2]string{
		{`Control\ComputerName\ComputerName`, "ComputerName"},
		{`Services\Tcpip\Parameters`, "Hostname"},
		{`Services\Tcpip\Parameters`, "NV Hostname"},
	} {
		if err := runReg("add", hive+`\`+set+`\`+v[0], "/v", v[1], "/t", "REG_SZ", "/d", name, "/f"); err != nil {
			return err
		}
	}
	return nil
}

func runReg(args ...string) error {
	out, err := exec.Command("reg", args...).CombinedOutput()
	if err != nil {
		return fmt.Errorf("reg %s: %w: %s", args[0], err, lastLine(out))
	}
	return nil
}

// MakeBootable runs bcdboot, which adds Windows Boot Manager to the firmware. It
// acts only inside WinPE: on a working Windows it would change that PC's boot.
func (w *writeDisk) MakeBootable(m proto.Manifest) error {
	if !strings.EqualFold(os.Getenv("SystemDrive"), "X:") {
		return nil
	}
	const windows, efi = windowsLetter, "S"
	var script []string
	script = append(script, fmt.Sprintf("select disk %d", w.number), "rescan")
	found := 0
	for i, p := range m.Partitions {
		letter := ""
		switch {
		case p.TypeGUID == typeESP:
			letter = efi
		case p.Policy == proto.PolicyFill:
			letter = windows
		default:
			continue
		}
		found++
		// buildLayout numbered the partitions in table order.
		script = append(script, fmt.Sprintf("select partition %d", i+1), "assign letter="+letter+" noerr")
	}
	if found != 2 {
		return errors.New("не найдены разделы EFI и Windows")
	}
	if err := runDiskpart(script...); err != nil {
		return err
	}
	out, err := exec.Command("bcdboot", windows+`:\Windows`, "/s", efi+":", "/f", "UEFI").CombinedOutput()
	if err != nil {
		return fmt.Errorf("bcdboot: %w: %s", err, lastLine(out))
	}
	return nil
}

// lastLine keeps an error short enough for the screen.
func lastLine(out []byte) string {
	lines := strings.Split(strings.TrimSpace(string(out)), "\n")
	return strings.TrimSpace(lines[len(lines)-1])
}

func (w *writeDisk) Close() error {
	var errs []error
	if w.disk != nil {
		errs = append(errs, w.disk.Close())
		w.disk = nil
	}
	for number, f := range w.parts {
		errs = append(errs, f.Close())
		delete(w.parts, number)
	}
	return errors.Join(errs...)
}

// Fit builds the table of the image for this disk and refuses a disk that cannot
// hold the data. Nothing is written yet. The usable area comes from the GPT of the
// disk itself, so a difference of a few bytes between disks does not matter.
func (w *writeDisk) Fit(m proto.Manifest) (proto.Manifest, error) {
	if m.SectorSize != w.sector {
		return proto.Manifest{}, fmt.Errorf("размер сектора диска %d, в образе %d", w.sector, m.SectorSize)
	}
	if len(m.Partitions) > maxTablePartitions {
		return proto.Manifest{}, fmt.Errorf("в образе %d разделов", len(m.Partitions))
	}
	first := binary.LittleEndian.Uint64(w.layout[24:])
	end := first + binary.LittleEndian.Uint64(w.layout[32:])
	if w.fresh {
		first, end = gptArea(w.size, w.sector)
	}
	fitted, err := m.Fit(first, end)
	var small *proto.TooSmallError
	if errors.As(err, &small) {
		return proto.Manifest{}, fmt.Errorf("образу нужно %s, а на диске доступно %s", formatBytes(small.Need), formatBytes(small.Have))
	}
	if err != nil {
		return proto.Manifest{}, err
	}
	fitted.DiskSize = w.size
	return fitted, nil
}

// runDiskpart runs a script and returns its last line on failure.
func runDiskpart(lines ...string) error {
	_, err := diskpartOutput(lines...)
	return err
}

// diskpartOutput runs a script and returns what diskpart printed.
func diskpartOutput(lines ...string) (string, error) {
	file := filepath.Join(os.TempDir(), "locless-diskpart.txt")
	if err := os.WriteFile(file, []byte(strings.Join(lines, "\r\n")+"\r\n"), 0o644); err != nil {
		return "", err
	}
	defer os.Remove(file)
	out, err := exec.Command("diskpart", "/s", file).CombinedOutput()
	if err != nil {
		return string(out), fmt.Errorf("diskpart: %w: %s", err, lastLine(out))
	}
	return string(out), nil
}

// Grow stretches the file system over the partition, which the table made larger
// than the captured volume. Like ntfsresize in FOG, it runs after the data is
// written, with the partition back in Windows.
func (w *writeDisk) Grow(m proto.Manifest) error {
	for i, p := range m.Partitions {
		if p.Policy == proto.PolicyFill {
			// buildLayout numbered the partitions in table order.
			return runDiskpart(fmt.Sprintf("select disk %d", w.number), "rescan",
				fmt.Sprintf("select partition %d", i+1), "extend filesystem")
		}
	}
	return nil
}
