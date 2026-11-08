package main

import "testing"

// TestDescribeDevice asks the first disk of this machine what it is. The queries
// only read; a machine without such a disk skips.
func TestDescribeDevice(t *testing.T) {
	d := describeDevice(0)
	if d == "" {
		t.Skip("no description of PhysicalDrive0")
	}
	t.Logf("PhysicalDrive0: %s", d)
	if mem := physicalMemory(); mem == 0 {
		t.Fatal("no physical memory")
	} else {
		t.Logf("memory: %s", formatBytes(mem))
	}
}
