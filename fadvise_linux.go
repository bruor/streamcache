//go:build linux
package main

import (
	"golang.org/x/sys/unix"
)

func FadviseDontNeed(fd uintptr, offset int64, length int64) {
	// POSIX_FADV_DONTNEED tells the kernel we no longer need these pages in memory,
	// prompting the kernel to evict them from the OS page cache immediately.
	_ = unix.Fadvise(int(fd), offset, length, unix.FADV_DONTNEED)
}
