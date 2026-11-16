//go:build windows

package diskspace

import (
	"path/filepath"

	"golang.org/x/sys/windows"
)

// freeOn returns the bytes available to this user on path's volume.
func freeOn(path string) (int64, error) {
	p, err := windows.UTF16PtrFromString(path)
	if err != nil {
		return 0, err
	}
	var callerFree, total, free uint64
	if err := windows.GetDiskFreeSpaceEx(p, &callerFree, &total, &free); err != nil {
		return 0, err
	}
	if callerFree > 1<<62 {
		callerFree = 1 << 62
	}
	return int64(callerFree), nil
}

// fsKey identifies the volume holding dir (drive letter): two dirs on
// the same drive share one reservation bucket, a ramdisk or a second
// drive gets its own.
func fsKey(dir string) string {
	if vol := filepath.VolumeName(dir); vol != "" {
		return vol
	}
	return dir
}
