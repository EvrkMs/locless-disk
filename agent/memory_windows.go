package main

import (
	"syscall"
	"unsafe"
)

var procGetProcessMemoryInfo = kernel32.NewProc("K32GetProcessMemoryInfo")

// processMemoryCounters is PROCESS_MEMORY_COUNTERS.
type processMemoryCounters struct {
	cb                         uint32
	pageFaultCount             uint32
	peakWorkingSetSize         uintptr
	workingSetSize             uintptr
	quotaPeakPagedPoolUsage    uintptr
	quotaPagedPoolUsage        uintptr
	quotaPeakNonPagedPoolUsage uintptr
	quotaNonPagedPoolUsage     uintptr
	pagefileUsage              uintptr
	peakPagefileUsage          uintptr
}

// processPeakMemory returns the peak working set of the agent: the most physical
// memory it ever held, as Windows counts it, which is what WinPE runs short of.
func processPeakMemory() (uint64, bool) {
	if procGetProcessMemoryInfo.Find() != nil {
		return 0, false
	}
	h, err := syscall.GetCurrentProcess()
	if err != nil {
		return 0, false
	}
	c := processMemoryCounters{}
	c.cb = uint32(unsafe.Sizeof(c))
	if r, _, _ := procGetProcessMemoryInfo.Call(uintptr(h), uintptr(unsafe.Pointer(&c)), uintptr(c.cb)); r == 0 {
		return 0, false
	}
	return uint64(c.peakWorkingSetSize), true
}
