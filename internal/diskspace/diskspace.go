// Package diskspace answers "how much room is left on the filesystem
// that holds THIS directory" — the sync folder, receive_dir, a ramdisk,
// a second drive: the check always targets the destination directory's
// own filesystem, never "the disk" in general. It also keeps a
// RAM-only ledger of space reserved by transfers in flight, so several
// concurrent transfers cannot together overflow the filesystem:
//
//	available = actually free (statfs) − already reserved
//
// A reservation counts only bytes not yet on disk: as chunks land they
// are moved from "reserved" to "really used" (Written), so the OS's
// free space and the ledger never double-count the same bytes. When a
// transfer ends — finished, closed, expired, failed — its reservation
// is released; an engine restart clears the ledger naturally, since
// nothing is in flight and RAM is empty.
package diskspace

import (
	"fmt"
	"os"
	"path/filepath"
	"sync"
)

// Ledger is the disk-accounting surface the transfer paths use;
// *Reservations is the real implementation (tests swap in fakes).
type Ledger interface {
	// Reserve declares that size bytes are about to be written under
	// dir. It fails with *NoSpaceError when they would not fit. A
	// re-reserve with the same id replaces the old number (a
	// restarted transfer re-declares its size).
	Reserve(dir, id string, size int64) error
	// Written accounts bytes that have actually hit the disk under
	// dir (they no longer count against the reservation — the OS's
	// free space already reflects them).
	Written(dir, id string, n int64)
	// Release drops the reservation entirely (transfer finished,
	// closed, expired, or failed for good).
	Release(dir, id string)
}

// NoSpaceError is returned when a transfer does not fit; it carries
// the numbers for the log line and the dashboard message.
type NoSpaceError struct {
	Need     int64 // bytes the transfer asked for
	Free     int64 // actually free on that filesystem right now
	Reserved int64 // reserved by other transfers in flight
}

// Available is what is left once in-flight reservations are subtracted
// from the real free space.
func (e *NoSpaceError) Available() int64 { return e.Free - e.Reserved }

func (e *NoSpaceError) Error() string {
	return fmt.Sprintf("not enough disk space: need %s, %s free, %s reserved by transfers in flight → %s available",
		humanBytes(e.Need), humanBytes(e.Free), humanBytes(e.Reserved), humanBytes(e.Available()))
}

// entry is one in-flight transfer's not-yet-written byte count.
type entry struct {
	dir       string
	remaining int64
}

// Reservations is the RAM-only ledger. Bookkeeping is keyed by
// filesystem, not by directory: two destination dirs on the same drive
// share one bucket, so neither can pass the check while together
// exceeding the disk.
type Reservations struct {
	mu   sync.Mutex
	byFS map[string]map[string]*entry // fs key → reservation id → entry
}

// New creates an empty ledger.
func New() *Reservations {
	return &Reservations{byFS: map[string]map[string]*entry{}}
}

// Reserve checks the destination filesystem and records the transfer's
// not-yet-written bytes under id. Returns *NoSpaceError (or a wrapped
// statfs failure) without touching any state when it does not fit.
func (r *Reservations) Reserve(dir, id string, size int64) error {
	if size < 0 {
		size = 0
	}
	key := fsKey(dir)
	r.mu.Lock()
	defer r.mu.Unlock()
	fs := r.byFS[key]
	if fs == nil {
		fs = map[string]*entry{}
		r.byFS[key] = fs
	}
	delete(fs, id) // replace: a restarted transfer re-declares its size
	var reserved int64
	for _, e := range fs {
		reserved += e.remaining
	}
	free, err := Free(dir)
	if err != nil {
		return fmt.Errorf("free-space check on %s: %w", dir, err)
	}
	if size > free-reserved {
		return &NoSpaceError{Need: size, Free: free, Reserved: reserved}
	}
	fs[id] = &entry{dir: dir, remaining: size}
	return nil
}

// Written moves n bytes from reserved to on-disk.
func (r *Reservations) Written(dir, id string, n int64) {
	key := fsKey(dir)
	r.mu.Lock()
	defer r.mu.Unlock()
	if e, ok := r.byFS[key][id]; ok {
		e.remaining -= n
		if e.remaining < 0 {
			e.remaining = 0
		}
	}
}

// Release drops the reservation entirely.
func (r *Reservations) Release(dir, id string) {
	key := fsKey(dir)
	r.mu.Lock()
	defer r.mu.Unlock()
	if fs := r.byFS[key]; fs != nil {
		delete(fs, id)
		if len(fs) == 0 {
			delete(r.byFS, key)
		}
	}
}

// Reserved reports how much is currently reserved on dir's filesystem
// (0 when nothing is in flight) — for logs and tests.
func (r *Reservations) Reserved(dir string) int64 {
	key := fsKey(dir)
	r.mu.Lock()
	defer r.mu.Unlock()
	var n int64
	for _, e := range r.byFS[key] {
		n += e.remaining
	}
	return n
}

// Free returns the bytes available to this user on the filesystem of
// dir. A missing dir is fine: the nearest existing ancestor answers
// (creating the dir later does not change the filesystem).
func Free(dir string) (int64, error) {
	return freeOn(existingAncestor(dir))
}

// existingAncestor returns the deepest existing ancestor of dir.
func existingAncestor(dir string) string {
	d := filepath.Clean(dir)
	for {
		if _, err := os.Stat(d); err == nil {
			return d
		}
		p := filepath.Dir(d)
		if p == d {
			break
		}
		d = p
	}
	return filepath.Clean(dir)
}

// humanBytes renders a size for error messages (mirrors the style used
// across the engine's activity lines).
func humanBytes(n int64) string {
	if n < 0 {
		n = 0
	}
	const unit = 1 << 10
	switch {
	case n >= unit<<30:
		return fmt.Sprintf("%.1f TiB", float64(n)/(unit<<30))
	case n >= unit<<20:
		return fmt.Sprintf("%.1f GiB", float64(n)/(unit<<20))
	case n >= unit<<10:
		return fmt.Sprintf("%.1f MiB", float64(n)/(unit<<10))
	case n >= unit:
		return fmt.Sprintf("%.1f KiB", float64(n)/unit)
	default:
		return fmt.Sprintf("%d B", n)
	}
}
