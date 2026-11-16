//go:build unix

package diskspace

import (
	"fmt"
	"os"
	"syscall"

	"golang.org/x/sys/unix"
)

// freeOn returns the bytes available to unprivileged users on path's
// filesystem (statfs Bavail — blocks reserved for root are excluded).
func freeOn(path string) (int64, error) {
	var st unix.Statfs_t
	if err := unix.Statfs(path, &st); err != nil {
		return 0, err
	}
	free := int64(st.Bavail) * int64(st.Bsize)
	if free < 0 {
		free = 0
	}
	return free, nil
}

// fsKey identifies the filesystem holding dir (device id): two dirs on
// the same drive share one reservation bucket, a ramdisk or a second
// drive gets its own. Falls back to the path when the device id is
// unavailable — the bucket is then just coarser, never wrong.
func fsKey(dir string) string {
	a := existingAncestor(dir)
	if fi, err := os.Stat(a); err == nil {
		if st, ok := fi.Sys().(*syscall.Stat_t); ok {
			return fmt.Sprintf("dev:%d", st.Dev)
		}
	}
	return a
}
