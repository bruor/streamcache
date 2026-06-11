//go:build !linux
package main

func FadviseDontNeed(fd uintptr, offset int64, length int64) {
	// No-op on Windows/macOS
}
