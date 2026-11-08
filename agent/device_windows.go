package main

import (
	"bytes"
	"encoding/binary"
	"fmt"
	"strings"
	"syscall"
)

// Properties IOCTL_STORAGE_QUERY_PROPERTY can ask about.
const (
	storageDeviceProperty      = 0 // StorageDeviceProperty: model and bus
	storageDeviceSeekPenalty   = 7 // StorageDeviceSeekPenaltyProperty: spinning or not
	storagePropertyStandardQry = 0 // PropertyStandardQuery
)

// busNames are the STORAGE_BUS_TYPE values a club PC has.
var busNames = map[uint32]string{
	0x01: "SCSI", 0x03: "ATA", 0x07: "USB", 0x08: "RAID", 0x0a: "SAS", 0x0b: "SATA",
	0x0c: "SD", 0x0f: "VHD", 0x11: "NVMe",
}

// describeDevice names what is behind a physical disk: its model, the bus and
// whether it is an SSD or a spinning disk. Each part is left out if Windows does
// not say; the result is for people and the metrics, nothing relies on it.
func describeDevice(number int) string {
	// No access is asked: the questions about a device need none, so this works
	// without the rights that reading the disk takes.
	name, err := syscall.UTF16PtrFromString(fmt.Sprintf(`\\.\PhysicalDrive%d`, number))
	if err != nil {
		return ""
	}
	h, err := syscall.CreateFile(name, 0, syscall.FILE_SHARE_READ|syscall.FILE_SHARE_WRITE, nil, syscall.OPEN_EXISTING, 0, 0)
	if err != nil {
		return ""
	}
	defer syscall.CloseHandle(h)

	var parts []string
	if out, err := control(h, ioctlStorageQuery, storageQuery(storageDeviceProperty), 1024); err == nil && len(out) >= 32 {
		if model := descriptorString(out, binary.LittleEndian.Uint32(out[16:])); model != "" {
			parts = append(parts, model)
		}
		if bus, ok := busNames[binary.LittleEndian.Uint32(out[28:])]; ok {
			parts = append(parts, bus)
		}
	}
	// DEVICE_SEEK_PENALTY_DESCRIPTOR: Version, Size, then IncursSeekPenalty.
	if out, err := control(h, ioctlStorageQuery, storageQuery(storageDeviceSeekPenalty), 12); err == nil && len(out) >= 9 {
		if out[8] != 0 {
			parts = append(parts, "HDD")
		} else {
			parts = append(parts, "SSD")
		}
	}
	return strings.Join(parts, ", ")
}

// storageQuery builds STORAGE_PROPERTY_QUERY for a property.
func storageQuery(property uint32) []byte {
	q := make([]byte, 12)
	binary.LittleEndian.PutUint32(q[0:], property)
	binary.LittleEndian.PutUint32(q[4:], storagePropertyStandardQry)
	return q
}

// descriptorString reads a NUL-terminated string at offset of a descriptor.
func descriptorString(out []byte, offset uint32) string {
	if offset == 0 || int(offset) >= len(out) {
		return ""
	}
	s := out[offset:]
	if end := bytes.IndexByte(s, 0); end >= 0 {
		s = s[:end]
	}
	return strings.TrimSpace(string(s))
}
