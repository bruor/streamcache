//go:build !linux
package main

// SyncFileRangeWrite is a no-op on non-Linux platforms
func SyncFileRangeWrite(fd uintptr, offset int64, length int64) {}

// SyncFileRangeWaitAndEvict is a no-op on non-Linux platforms
func SyncFileRangeWaitAndEvict(fd uintptr, offset int64, length int64) {}

// FadviseDontNeed is a no-op on non-Linux platforms
func FadviseDontNeed(fd uintptr, offset int64, length int64) {}

// PunchHole is a no-op on non-Linux platforms
func PunchHole(fd uintptr, offset int64, length int64) error {
	return nil
}
