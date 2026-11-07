package main

import (
	"syscall"
	"time"
)

// processCPU returns the processor time the agent has used, kernel and user, on
// all cores together.
func processCPU() time.Duration {
	h, err := syscall.GetCurrentProcess()
	if err != nil {
		return 0
	}
	var created, exited, kernel, user syscall.Filetime
	if err := syscall.GetProcessTimes(h, &created, &exited, &kernel, &user); err != nil {
		return 0
	}
	ticks := func(t syscall.Filetime) int64 { return int64(t.HighDateTime)<<32 | int64(t.LowDateTime) }
	return time.Duration(ticks(kernel)+ticks(user)) * 100 // FILETIME counts 100 ns
}
