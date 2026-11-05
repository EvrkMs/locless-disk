//go:build !windows

package main

import "errors"

// openTarget exists so the package builds everywhere.
func openTarget() (target, error) {
	return nil, errors.New("writing a disk needs Windows")
}
