//go:build !windows

package main

import "errors"

// openSystemDisk exists so the package builds everywhere.
func openSystemDisk() (source, error) {
	return nil, errors.New("reading a disk needs Windows")
}
