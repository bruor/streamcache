//go:build linux

package main

import (
	"os"
	"path/filepath"
	"syscall"

	"golang.org/x/sys/unix"
)

func getDeviceID(path string) (uint64, bool) {
	info, err := os.Stat(path)
	if err != nil {
		return 0, false
	}
	st, ok := info.Sys().(*syscall.Stat_t)
	if !ok || st == nil {
		return 0, false
	}
	return uint64(st.Dev), true
}

func detectFSType(path string) string {
	absPath := path
	if abs, err := filepath.Abs(path); err == nil {
		absPath = abs
	}
	if eval, err := filepath.EvalSymlinks(absPath); err == nil {
		absPath = eval
	}

	if raw, err := os.ReadFile("/proc/self/mountinfo"); err == nil {
		if fs := parseMountInfoFSType(string(raw), absPath); fs != "" {
			return fs
		}
	}

	var stat unix.Statfs_t
	if err := unix.Statfs(absPath, &stat); err != nil {
		return "unknown"
	}

	switch uint64(stat.Type) {
	case 0xFF534D42:
		return "cifs"
	case 0xFE534D42:
		return "smb3"
	case 0x6969:
		return "nfs"
	case 0x65735546:
		return "fuse"
	case 0x01021994:
		return "tmpfs"
	case 0xEF53:
		return "ext4"
	case 0x58465342:
		return "xfs"
	case 0x9123683E:
		return "btrfs"
	case 0x794C7630:
		return "overlay"
	default:
		return "unknown"
	}
}
