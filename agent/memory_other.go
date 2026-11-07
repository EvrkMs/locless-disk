//go:build !windows

package main

// processPeakMemory has no portable source; the metrics fall back to what the Go
// runtime was seen holding.
func processPeakMemory() (uint64, bool) { return 0, false }
