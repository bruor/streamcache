//go:build !windows
package main

import "golang.org/x/sys/unix"

func GetDiskFreeSpace(path string) (uint64, error) {
	var stat unix.Statfs_t
	err := unix.Statfs(path, &stat)
	if err != nil {
		return 0, err
	}
	// Bavail is the number of free blocks available to non-privileged users
	// Bsize is the size of each filesystem block
	return stat.Bavail * uint64(stat.Bsize), nil
}
