//go:build windows

package main

import (
	"path/filepath"
	"strings"

	"golang.org/x/sys/windows"
)

func diskFreeBytesPlatform(path string) uint64 {
	path = strings.TrimSpace(path)
	if path == "" {
		path = "."
	}
	clean := filepath.Clean(path)
	ptr, err := windows.UTF16PtrFromString(clean)
	if err != nil {
		return 0
	}
	var free uint64
	var total uint64
	var totalFree uint64
	if err := windows.GetDiskFreeSpaceEx(ptr, &free, &total, &totalFree); err != nil {
		return 0
	}
	return free
}
