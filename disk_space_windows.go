//go:build windows
package main

import (
	"golang.org/x/sys/windows"
)

// GetDiskFreeSpace returns available free bytes for path on Windows.
func GetDiskFreeSpace(path string) (uint64, error) {
	free, _, err := GetDiskCapacity(path)
	return free, err
}

// GetDiskCapacity returns available free bytes and total volume bytes for path on Windows.
func GetDiskCapacity(path string) (uint64, uint64, error) {
	var freeBytesAvailable, totalNumberOfBytes, totalNumberOfFreeBytes uint64
	err := windows.GetDiskFreeSpaceEx(
		windows.StringToUTF16Ptr(path),
		&freeBytesAvailable,
		&totalNumberOfBytes,
		&totalNumberOfFreeBytes,
	)
	if err != nil {
		return 0, 0, err
	}
	return freeBytesAvailable, totalNumberOfBytes, nil
}
