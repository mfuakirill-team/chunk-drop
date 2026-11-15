//go:build !windows

package upload

import "time"

// setBirthTime is a no-op off Windows: Linux and macOS offer no
// portable userspace API to set a file's birth time. The modification
// time (stamped in Finish) is what file managers and photo tools show
// by default, so the photo's original date is what users see there.
func setBirthTime(_ string, _ time.Time) error { return nil }
