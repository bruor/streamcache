//go:build linux
package main

import (
	"golang.org/x/sys/unix"
)

func FadviseDontNeed(fd uintptr, offset int64, length int64) {
	// 1. Asynchronously start writeout of dirty pages to physical storage in the background
	_ = unix.SyncFileRange(int(fd), offset, length, unix.SYNC_FILE_RANGE_WRITE)
	// 2. POSIX_FADV_DONTNEED tells the kernel we no longer need these pages in memory,
	// prompting the kernel to evict them from the OS page cache once clean.
	_ = unix.Fadvise(int(fd), offset, length, unix.FADV_DONTNEED)
}
