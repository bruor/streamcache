//go:build linux
package main

import (
	"golang.org/x/sys/unix"
)

// SyncFileRangeWrite starts asynchronous background writeout of dirty pages for a range
func SyncFileRangeWrite(fd uintptr, offset int64, length int64) {
	_ = unix.SyncFileRange(int(fd), offset, length, unix.SYNC_FILE_RANGE_WRITE)
}

// SyncFileRangeWaitAndEvict waits for writeout of a range to complete and evicts it from page cache
func SyncFileRangeWaitAndEvict(fd uintptr, offset int64, length int64) {
	_ = unix.SyncFileRange(int(fd), offset, length, unix.SYNC_FILE_RANGE_WAIT_BEFORE|unix.SYNC_FILE_RANGE_WRITE|unix.SYNC_FILE_RANGE_WAIT_AFTER)
	_ = unix.Fadvise(int(fd), offset, length, unix.FADV_DONTNEED)
}

// FadviseDontNeed evicts clean pages from OS page cache
func FadviseDontNeed(fd uintptr, offset int64, length int64) {
	if fd == 0 || fd == ^uintptr(0) {
		return
	}
	_ = unix.Fadvise(int(fd), offset, length, unix.FADV_DONTNEED)
}

// PunchHole deallocates physical disk blocks while preserving file offsets (ext4, xfs, btrfs)
func PunchHole(fd uintptr, offset int64, length int64) error {
	if fd == 0 || fd == ^uintptr(0) || length <= 0 {
		return nil
	}
	return unix.Fallocate(int(fd), unix.FALLOC_FL_PUNCH_HOLE|unix.FALLOC_FL_KEEP_SIZE, offset, length)
}
