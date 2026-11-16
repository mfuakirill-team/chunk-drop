// Disk-space accounting: the manager reserves the declared batch size
// at start (against the destination dir's filesystem), the reservation
// shrinks as chunks land, and every terminal path releases it.
package upload

import (
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"chunkdrop/internal/diskspace"
)

type fakeLedger struct {
	noSpace  error         // non-nil: Reserve fails with this
	reserves []fakeReserve // every Reserve call
	written  []fakeWritten // every Written call
	released []string      // every Release call (ids)
}

type fakeReserve struct {
	dir, id string
	size    int64
}

type fakeWritten struct {
	id string
	n  int64
}

func (f *fakeLedger) Reserve(dir, id string, size int64) error {
	f.reserves = append(f.reserves, fakeReserve{dir, id, size})
	return f.noSpace
}

func (f *fakeLedger) Written(dir, id string, n int64) {
	f.written = append(f.written, fakeWritten{id, n})
}

func (f *fakeLedger) Release(dir, id string) {
	f.released = append(f.released, id)
}

func TestStartReservedAgainstDisk(t *testing.T) {
	dest := t.TempDir()
	led := &fakeLedger{}
	mgr := NewManager()
	mgr.Space = led
	up, err := mgr.NewPending(dest)
	if err != nil {
		t.Fatal(err)
	}
	mkSpec := func(name string, payload []byte) FileSpec {
		sum := sha256.Sum256(payload)
		cc, hashes := 1, []string{hex.EncodeToString(sum[:])}
		if len(payload) == 0 {
			cc, hashes = 0, nil
		}
		return FileSpec{
			Path: name, Size: int64(len(payload)), ChunkCount: cc,
			Hashes: hashes, MtimeMs: 1700000000000,
		}
	}

	// Ledger says "does not fit" → start refused with that error, and
	// nothing is staged.
	led.noSpace = &diskspace.NoSpaceError{Need: 100, Free: 10, Reserved: 5}
	_, err = mgr.Start(up.KeyEncoded(), []FileSpec{mkSpec("a.txt", []byte("hello"))}, 1<<30)
	var nse *diskspace.NoSpaceError
	if !errors.As(err, &nse) {
		t.Fatalf("start err = %v, want *NoSpaceError", err)
	}
	if !strings.Contains(err.Error(), "not enough disk space") ||
		!strings.Contains(err.Error(), "destination") {
		t.Errorf("error text lacks the numbers/destination: %v", err)
	}
	if up.Started || len(up.Files) != 0 {
		t.Errorf("slot mutated by the refused start (started=%v)", up.Started)
	}
	if _, statErr := os.Stat(filepath.Join(dest, CacheDirName, UploadsTmpDir, up.ID)); !os.IsNotExist(statErr) {
		t.Errorf("staging created for a refused start: %v", statErr)
	}

	// Ledger accepts → start proceeds, reserve got the declared size.
	led.noSpace = nil
	payload := []byte("hello")
	if _, err := mgr.Start(up.KeyEncoded(), []FileSpec{mkSpec("a.txt", payload), mkSpec("b.txt", nil)}, 1<<30); err != nil {
		t.Fatalf("start: %v", err)
	}
	if len(led.reserves) != 2 {
		t.Fatalf("reserves = %d, want 2 (refused + accepted)", len(led.reserves))
	}
	last := led.reserves[len(led.reserves)-1]
	if last.dir != dest || last.id != "upload:"+up.ID || last.size != int64(len(payload)) {
		t.Errorf("reserve args = %+v, want dir=dest id=upload:%s size=%d", last, up.ID, len(payload))
	}

	// A chunk landing moves its bytes out of the reservation.
	if err := up.StoreChunk(0, 0, payload, up.Files[0].Hashes[0]); err != nil {
		t.Fatalf("storeChunk: %v", err)
	}
	if len(led.written) != 1 || led.written[0].n != int64(len(payload)) || led.written[0].id != "upload:"+up.ID {
		t.Errorf("written = %+v, want one n=%d for the upload", led.written, len(payload))
	}

	// Finish releases the reservation.
	if _, err := up.Finish(); err != nil {
		t.Fatalf("finish: %v", err)
	}
	if len(led.released) != 1 || led.released[0] != "upload:"+up.ID {
		t.Errorf("released = %v, want one release for the upload", led.released)
	}
}

func TestCloseAndReapReleaseReservation(t *testing.T) {
	dest := t.TempDir()
	led := &fakeLedger{}
	mgr := NewManager()
	mgr.Space = led
	mkSpec := func(name string, payload []byte) FileSpec {
		sum := sha256.Sum256(payload)
		cc, hashes := 1, []string{hex.EncodeToString(sum[:])}
		if len(payload) == 0 {
			cc, hashes = 0, nil
		}
		return FileSpec{
			Path: name, Size: int64(len(payload)), ChunkCount: cc,
			Hashes: hashes, MtimeMs: 1700000000000,
		}
	}

	// Close-link path.
	up, _ := mgr.NewPending(dest)
	if _, err := mgr.Start(up.KeyEncoded(), []FileSpec{mkSpec("a.txt", []byte("x"))}, 1<<30); err != nil {
		t.Fatal(err)
	}
	if !mgr.Close(up.KeyEncoded()) {
		t.Fatal("close failed")
	}
	if len(led.released) != 1 {
		t.Fatalf("close did not release: %v", led.released)
	}

	// Expiry path (Reap).
	up2, _ := mgr.NewPending(dest)
	if _, err := mgr.Start(up2.KeyEncoded(), []FileSpec{mkSpec("b.txt", []byte("y"))}, 1<<30); err != nil {
		t.Fatal(err)
	}
	up2.ExpiresAt = time.Now().Add(-time.Second)
	mgr.Reap()
	if len(led.released) != 2 {
		t.Fatalf("reap did not release: %v", led.released)
	}

	// A restarted slot re-declares: one reserve per Start, no leak.
	led2 := &fakeLedger{}
	mgr2 := NewManager()
	mgr2.Space = led2
	up3, _ := mgr2.NewPending(dest)
	if _, err := mgr2.Start(up3.KeyEncoded(), []FileSpec{mkSpec("c.txt", []byte("z"))}, 1<<30); err != nil {
		t.Fatal(err)
	}
	if _, err := mgr2.Start(up3.KeyEncoded(), []FileSpec{mkSpec("c2.txt", []byte("zz"))}, 1<<30); err != nil {
		t.Fatal(err)
	}
	if len(led2.reserves) != 2 {
		t.Fatalf("restart reserves = %d, want 2", len(led2.reserves))
	}
	if len(led2.released) != 0 {
		t.Fatalf("restart released = %v, want none (reserve replaced, not released)", led2.released)
	}
}
