package diskspace

import (
	"errors"
	"os"
	"path/filepath"
	"testing"
)

// The ledger's arithmetic is unit-tested with the real filesystem
// under a temp dir: reservations shrink as bytes land, release clears,
// a re-reserve replaces, and a too-big transfer is refused with the
// full numbers.

func TestReserveWrittenRelease(t *testing.T) {
	dir := t.TempDir()
	r := New()

	free, err := Free(dir)
	if err != nil || free <= 0 {
		t.Fatalf("Free(%s) = %d, %v", dir, free, err)
	}

	// Reserve half the free space, then write it all down.
	half := free / 2
	if err := r.Reserve(dir, "t1", half); err != nil {
		t.Fatalf("Reserve t1: %v", err)
	}
	if got := r.Reserved(dir); got != half {
		t.Fatalf("Reserved = %d, want %d", got, half)
	}
	r.Written(dir, "t1", half/2)
	if got := r.Reserved(dir); got != half-half/2 {
		t.Fatalf("Reserved after Written = %d, want %d", got, half-half/2)
	}
	r.Written(dir, "t1", half) // over-write clamps at 0
	if got := r.Reserved(dir); got != 0 {
		t.Fatalf("Reserved after full write = %d, want 0", got)
	}
	r.Release(dir, "t1")
	if got := r.Reserved(dir); got != 0 {
		t.Fatalf("Reserved after Release = %d, want 0", got)
	}
}

func TestReserveRejectsOverflow(t *testing.T) {
	dir := t.TempDir()
	r := New()
	free, err := Free(dir)
	if err != nil || free <= 0 {
		t.Fatalf("Free = %d, %v", free, err)
	}

	// One transfer reserves half; a second asking for the whole disk
	// must not fit, whatever the OS's free space does meanwhile.
	if err := r.Reserve(dir, "t1", free/2); err != nil {
		t.Fatalf("Reserve t1: %v", err)
	}
	err = r.Reserve(dir, "t2", free)
	var nse *NoSpaceError
	if !errors.As(err, &nse) {
		t.Fatalf("Reserve t2 err = %v, want *NoSpaceError", err)
	}
	if nse.Need != free {
		t.Errorf("Need = %d, want %d", nse.Need, free)
	}
	if nse.Reserved != free/2 {
		t.Errorf("Reserved = %d, want %d", nse.Reserved, free/2)
	}
	if nse.Available() >= free {
		t.Errorf("Available = %d, want < free %d", nse.Available(), free)
	}
	// The refused transfer must not have booked anything.
	if got := r.Reserved(dir); got != free/2 {
		t.Fatalf("Reserved after refusal = %d, want %d", got, free/2)
	}
	// The failed check's error message carries the numbers.
	t.Log(nse.Error())
}

func TestReserveReplaces(t *testing.T) {
	dir := t.TempDir()
	r := New()
	if err := r.Reserve(dir, "t1", 100); err != nil {
		t.Fatalf("Reserve: %v", err)
	}
	// A restarted transfer re-declares: replace, not add.
	if err := r.Reserve(dir, "t1", 40); err != nil {
		t.Fatalf("re-Reserve: %v", err)
	}
	if got := r.Reserved(dir); got != 40 {
		t.Fatalf("Reserved = %d, want 40 (replaced)", got)
	}
}

func TestSameFilesystemSharesBucket(t *testing.T) {
	dir := t.TempDir()
	sub := filepath.Join(dir, "sub", "deeper")
	if err := os.MkdirAll(sub, 0o755); err != nil {
		t.Fatalf("mkdir: %v", err)
	}
	r := New()
	// Different dirs, same filesystem: one bucket — reserving under
	// dir is visible under sub and vice versa.
	if err := r.Reserve(dir, "t1", 100); err != nil {
		t.Fatalf("Reserve dir: %v", err)
	}
	if got := r.Reserved(sub); got != 100 {
		t.Fatalf("Reserved(sub) = %d, want 100 (same filesystem bucket)", got)
	}
}
