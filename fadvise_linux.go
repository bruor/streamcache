//go:build linux
package main

import (
	"golang.org/x/sys/unix"
)

// SyncFileRangeWrite starts asynchronous background writeout of dirty pages for a block
func SyncFileRangeWrite(fd uintptr, offset int64, length int64) {
	_ = unix.SyncFileRange(int(fd), offset, length, unix.SYNC_FILE_RANGE_WRITE)
}

// SyncFileRangeWaitAndEvict waits for writeout of a block to complete and evicts it from page cache
func SyncFileRangeWaitAndEvict(fd uintptr, offset int64, length int64) {
	_ = unix.SyncFileRange(int(fd), offset, length, unix.SYNC_FILE_RANGE_WAIT_BEFORE|unix.SYNC_FILE_RANGE_WRITE|unix.SYNC_FILE_RANGE_WAIT_AFTER)
	_ = unix.Fadvise(int(fd), offset, length, unix.FADV_DONTNEED)
}

// FadviseDontNeed evicts clean pages from OS page cache (e.g. during player reads)
func FadviseDontNeed(fd uintptr, offset int64, length int64) {
	_ = unix.Fadvise(int(fd), offset, length, unix.FADV_DONTNEED)
}
