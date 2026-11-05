package main

import (
	"bytes"
	"encoding/binary"
	"os"
	"testing"
	"time"

	"fogs/proto"
)

func TestFormatGUID(t *testing.T) {
	// The EFI system partition type as Windows stores it.
	b := []byte{0x28, 0x73, 0x2a, 0xc1, 0x1f, 0xf8, 0xd2, 0x11, 0xba, 0x4b, 0x00, 0xa0, 0xc9, 0x3e, 0xc9, 0x3b}
	if got := formatGUID(b); got != typeESP {
		t.Fatalf("got %s", got)
	}
}

func TestUTF16Name(t *testing.T) {
	b := make([]byte, 72)
	for i, r := range []rune("Основной") {
		binary.LittleEndian.PutUint16(b[i*2:], uint16(r))
	}
	if got := utf16Name(b); got != "Основной" {
		t.Fatalf("got %q", got)
	}
}

func testVolume(t *testing.T) (*volumeMap, *physicalDisk) {
	t.Helper()
	bits, err := proto.NewBitmap(8)
	if err != nil {
		t.Fatalf("NewBitmap: %v", err)
	}
	bits.Set(1)
	v := &volumeMap{cluster: 4096, total: 8, bitmap: bits}
	return v, &physicalDisk{maps: map[uint32]*volumeMap{3: v}}
}

func TestVolumeMapUsed(t *testing.T) {
	v, _ := testVolume(t)
	tests := []struct {
		offset, length uint64
		want           bool
	}{
		{0, 4096, false},   // cluster 0 is free
		{4096, 4096, true}, // cluster 1 is used
		{0, 8192, true},    // spans into it
		{2 * 4096, 6 * 4096, false},
		{8 * 4096, 512, true},  // past the clusters: the backup boot sector
		{7 * 4096, 8192, true}, // ends past the clusters
	}
	for _, tc := range tests {
		if got := v.used(tc.offset, tc.length); got != tc.want {
			t.Fatalf("used(%d, %d) = %v", tc.offset, tc.length, got)
		}
	}
}

func TestReadBlockSkipsFreeSpaceWithoutReading(t *testing.T) {
	_, d := testVolume(t)
	dst := make([]byte, 4096)
	used, err := d.ReadBlock(3, 0, dst) // d.file is nil: a read would panic
	if err != nil || used {
		t.Fatalf("used %v, err %v", used, err)
	}
}

// TestRealBlocks measures real data, read-only: how fast blocks are read, how well
// they compress, and that each one survives encoding.
//
//	LOCLESS_REAL_DISK=1 go test ./agent -run RealBlocks -v
func TestRealBlocks(t *testing.T) {
	if os.Getenv("LOCLESS_REAL_DISK") == "" {
		t.Skip("LOCLESS_REAL_DISK is not set")
	}
	src, err := openSystemDisk()
	if err != nil {
		t.Fatalf("openSystemDisk: %v", err)
	}
	defer src.Close()

	const want = 3000
	var main proto.Partition
	for _, p := range src.Manifest().Partitions {
		if p.Policy == proto.PolicyFill {
			main = p
		}
	}
	block := make([]byte, proto.DefaultBlockSize)
	var raw, stored, count, empty, unique int
	seen := map[proto.Sum]bool{}
	start := time.Now()
	for offset := uint64(0); offset+proto.DefaultBlockSize <= main.Size && count < want; offset += proto.DefaultBlockSize {
		used, err := src.ReadBlock(main.Number, offset, block)
		if err != nil {
			t.Fatalf("ReadBlock: %v", err)
		}
		if !used {
			empty++
			continue
		}
		h, payload, err := proto.EncodeBlock(block)
		if err != nil {
			t.Fatalf("EncodeBlock: %v", err)
		}
		back, err := proto.DecodeBlock(h, payload)
		if err != nil || !bytes.Equal(back, block) {
			t.Fatalf("block at %d did not survive encoding: %v", offset, err)
		}
		if !seen[h.Hash] {
			seen[h.Hash] = true
			unique++
			stored += len(payload)
		}
		raw += len(block)
		count++
	}
	took := time.Since(start).Seconds()
	t.Logf("%d blocks with data, %d empty skipped, %d unique, %.1fs", count, empty, unique, took)
	t.Logf("raw %s, stored %s (%.0f%%), one thread: %.0f MB/s", formatBytes(uint64(raw)), formatBytes(uint64(stored)),
		float64(stored)*100/float64(raw), float64(raw)/took/(1<<20))
}

// TestRealDisk reads the disk of this machine, read-only. It needs administrator
// rights, so it runs only on request:
//
//	LOCLESS_REAL_DISK=1 go test ./agent -run RealDisk -v
func TestRealDisk(t *testing.T) {
	if os.Getenv("LOCLESS_REAL_DISK") == "" {
		t.Skip("LOCLESS_REAL_DISK is not set")
	}
	src, err := openSystemDisk()
	if err != nil {
		t.Fatalf("openSystemDisk: %v", err)
	}
	defer src.Close()
	t.Log(src.Describe())

	const block = proto.DefaultBlockSize
	for _, p := range src.Manifest().Partitions {
		used := src.UsedBlocks(p.Number, block)
		t.Logf("partition %d: %q type %s size %s policy %s content %s, blocks with data: %d of %d",
			p.Number, p.Name, p.TypeGUID, formatBytes(p.Size), p.Policy, p.Content, used, (p.Size+block-1)/block)
		if p.Content != proto.ContentBlocks {
			continue
		}
		raw := make([]byte, min(uint64(block), p.Size))
		ok, err := src.ReadBlock(p.Number, 0, raw)
		if err != nil {
			t.Fatalf("ReadBlock: %v", err)
		}
		t.Logf("  first block: used %v, starts %x", ok, raw[:16])
	}
}
