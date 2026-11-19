//go:build !windows
package main

import "golang.org/x/sys/unix"

// GetDiskFreeSpace returns available free bytes for unprivileged callers on path's filesystem.
func GetDiskFreeSpace(path string) (uint64, error) {
	free, _, err := GetDiskCapacity(path)
	return free, err
}

// GetDiskCapacity returns available free bytes and total filesystem bytes for path.
func GetDiskCapacity(path string) (uint64, uint64, error) {
	var stat unix.Statfs_t
	if err := unix.Statfs(path, &stat); err != nil {
		return 0, 0, err
	}
	// Bavail is the number of free blocks available to non-privileged users
	// Bsize is the size of each filesystem block
	return stat.Bavail * uint64(stat.Bsize), stat.Blocks * uint64(stat.Bsize), nil
}
