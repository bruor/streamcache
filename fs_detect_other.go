//go:build !linux

package main

func getDeviceID(path string) (uint64, bool) {
	return 0, false
}

func detectFSType(path string) string {
	return "unknown"
}
