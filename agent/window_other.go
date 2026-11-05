//go:build !windows

package main

import (
	"errors"
	"image"
)

// runWindow exists so the package builds everywhere; the agent only runs on Windows.
func runWindow(func(*image.RGBA), func(int) bool) error {
	return errors.New("the agent screen needs Windows")
}
