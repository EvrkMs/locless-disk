//go:build !windows

package scenario

import "syscall"

// freeSpace returns the bytes this process may still write on the disk of path.
func freeSpace(path string) (uint64, error) {
	var s syscall.Statfs_t
	if err := syscall.Statfs(path, &s); err != nil {
		return 0, err
	}
	return s.Bavail * uint64(s.Bsize), nil
}
