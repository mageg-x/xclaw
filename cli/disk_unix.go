//go:build !windows

package main

import (
	"strings"
	"syscall"
)

func diskFreeBytesPlatform(path string) uint64 {
	path = strings.TrimSpace(path)
	if path == "" {
		path = "."
	}
	var st syscall.Statfs_t
	if err := syscall.Statfs(path, &st); err != nil {
		return 0
	}
	return st.Bavail * uint64(st.Bsize)
}
