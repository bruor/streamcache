//go:build !linux
package main

func SyncFileRangeWrite(fd uintptr, offset int64, length int64) {}
func SyncFileRangeWaitAndEvict(fd uintptr, offset int64, length int64) {}

func FadviseDontNeed(fd uintptr, offset int64, length int64) {
	// No-op on Windows/macOS
}
