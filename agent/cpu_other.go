//go:build !windows

package main

import (
	"syscall"
	"time"
)

// processCPU returns the processor time the agent has used, kernel and user, on
// all cores together.
func processCPU() time.Duration {
	var u syscall.Rusage
	if err := syscall.Getrusage(syscall.RUSAGE_SELF, &u); err != nil {
		return 0
	}
	return time.Duration(u.Utime.Nano() + u.Stime.Nano())
}
