//go:build linux
package main

import (
	"golang.org/x/sys/unix"
)

// SyncFileRangeWrite starts asynchronous background writeout of dirty pages for a block
func SyncFileRangeWrite(fd uintptr, offset int64, length int64) {
	_ = unix.SyncFileRange(int(fd), offset, length, unix.SYNC_FILE_RANGE_WRITE)
}

// FadviseDontNeed evicts clean pages from OS page cache (e.g. during player reads or after writeout)
func FadviseDontNeed(fd uintptr, offset int64, length int64) {
	_ = unix.Fadvise(int(fd), offset, length, unix.FADV_DONTNEED)
}
