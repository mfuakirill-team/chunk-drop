package upload

import (
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// Engine-side checks for the browser-built manifest (PROTOCOL.md §4
// upload start + §8): sanitize every path, chunk math, hash shape,
// caps, case-collision rename.
func TestValidateSpecs(t *testing.T) {
	good := FileSpec{
		Path: "pics/2026/cat.jpg", Size: 5, ChunkCount: 1,
		Hashes: []string{strings.Repeat("ab", 32)}, MtimeMs: 1700000000000,
	}
	specs := func(ss ...FileSpec) []FileSpec { return ss }

	if _, err := validateSpecs(specs(good), 1<<30); err != nil {
		t.Fatalf("good specs rejected: %v", err)
	}

	// PROTOCOL.md §8 rule 1: traversal is rejected.
	bad := good
	bad.Path = "../etc/passwd"
	if _, err := validateSpecs(specs(bad), 1<<30); err == nil {
		t.Error("traversal path accepted")
	}
	// Chunk math must match the size.
	bad = good
	bad.ChunkCount = 2
	if _, err := validateSpecs(specs(bad), 1<<30); err == nil {
		t.Error("inconsistent chunkCount accepted")
	}
	// Hash shape.
	bad = good
	bad.Hashes = []string{"AB"} // wrong length + uppercase
	if _, err := validateSpecs(specs(bad), 1<<30); err == nil {
		t.Error("malformed hash accepted")
	}
	// Total-size cap.
	if _, err := validateSpecs(specs(good, good), 5); err == nil {
		t.Error("size cap not enforced")
	}
	// Manifest cap.
	many := make([]FileSpec, 0, 10001)
	for i := 0; i < 10001; i++ {
		s := good
		s.Path = fmt.Sprintf("f%d", i)
		many = append(many, s)
	}
	if _, err := validateSpecs(many, 1<<40); err == nil {
		t.Error("file-count cap not enforced")
	}
	// Case collisions: rule 6 — renamed, never silently overwritten.
	files, err := validateSpecs(specs(
		good,
		FileSpec{Path: "pics/A.jpg", Size: 1, ChunkCount: 1, Hashes: []string{strings.Repeat("00", 32)}},
		FileSpec{Path: "pics/a.jpg", Size: 1, ChunkCount: 1, Hashes: []string{strings.Repeat("00", 32)}},
	), 1<<30)
	if err != nil {
		t.Fatalf("collision specs rejected: %v", err)
	}
	paths := map[string]bool{}
	for _, f := range files {
		if paths[strings.ToLower(f.Path)] {
			t.Errorf("collision survived: %q", f.Path)
		}
		paths[strings.ToLower(f.Path)] = true
	}
	if !strings.Contains(files[2].Path, "(1)") {
		t.Errorf("collision not renamed: %q", files[2].Path)
	}
}

// storeChunk verifies against BOTH the header and the declared manifest
// hash; finish renames staged files into place and stamps mtimes.
func TestStoreChunkAndFinish(t *testing.T) {
	dest := t.TempDir()
	mgr := NewManager()
	up, err := mgr.NewPending(dest)
	if err != nil {
		t.Fatal(err)
	}

	payload := []byte("hello chunkdrop")
	sum := sha256.Sum256(payload)
	hash := hex.EncodeToString(sum[:])
	specs := []FileSpec{
		{Path: "docs/readme.txt", Size: int64(len(payload)), ChunkCount: 1, Hashes: []string{hash}, MtimeMs: 1700000000000},
		{Path: "docs/empty.bin", Size: 0, ChunkCount: 0, Hashes: nil, MtimeMs: 0},
	}
	if _, err := mgr.Start("wrong-key-wrong-key-wrong-", specs, 1<<30); err == nil {
		t.Fatal("start with a wrong key succeeded")
	}
	if _, err := mgr.Start(up.KeyEncoded(), specs, 1<<30); err != nil {
		t.Fatalf("start: %v", err)
	}
	if up.Staging == "" || !strings.Contains(up.Staging, filepath.Join(dest, CacheDirName, UploadsTmpDir)) {
		t.Fatalf("staging dir %q outside the excluded hashes/ tree", up.Staging)
	}

	f0 := up.Files[0]
	if err := up.storeChunk(f0, 0, []byte("hello chunkdRop"), hash); err != ErrHashMismatch {
		t.Errorf("mismatched bytes accepted (err=%v)", err)
	}
	if err := up.storeChunk(f0, 0, payload, strings.Repeat("ff", 32)); err != ErrHashMismatch {
		t.Errorf("wrong header hash accepted (err=%v)", err)
	}
	if err := up.storeChunk(f0, 0, payload, hash); err != nil {
		t.Fatalf("storeChunk: %v", err)
	}
	if err := up.storeChunk(f0, 1, payload, hash); err != ErrBadChunk {
		t.Errorf("out-of-range chunk accepted (err=%v)", err)
	}
	if f0.Have() != 1 || !f0.HaveAll() {
		t.Errorf("have=%d all=%v", f0.Have(), f0.HaveAll())
	}

	// Finish before completeness is refused — but the empty file has
	// nothing to receive, so only f0 gates it.
	// A second start RESETS the slot (restart allowed): have-bitmaps
	// cleared, staging wiped — finish now requires a fresh upload.
	if _, err := mgr.Start(up.KeyEncoded(), specs, 1<<30); err != nil {
		t.Fatalf("restart start: %v", err)
	}
	f0 = up.Files[0] // Start rebuilds the file list (fresh *File values)
	if f0.Have() != 0 || f0.HaveAll() {
		t.Errorf("restart did not reset have-state: have=%d all=%v", f0.Have(), f0.HaveAll())
	}
	if f0.TmpPath == "" {
		t.Fatal("restart lost the staging path")
	}
	if st, err := os.Stat(f0.TmpPath); err != nil || st.Size() != 0 {
		t.Errorf("restarted staging file: %v (size %d)", err, st.Size())
	}
	if err := up.storeChunk(f0, 0, payload, hash); err != nil {
		t.Fatalf("re-storeChunk: %v", err)
	}
	landed, err := up.Finish()
	if err != nil {
		t.Fatalf("finish: %v", err)
	}
	if len(landed) != 2 {
		t.Fatalf("landed = %+v", landed)
	}
	for _, l := range landed {
		if _, err := os.Stat(l.Abs); err != nil {
			t.Errorf("%s missing after finish: %v", l.Abs, err)
		}
	}
	got, _ := os.ReadFile(filepath.Join(dest, "docs", "readme.txt"))
	if string(got) != string(payload) {
		t.Errorf("landed content = %q", got)
	}
	if st, err := os.Stat(filepath.Join(dest, "docs", "readme.txt")); err != nil ||
		!st.ModTime().Equal(time.UnixMilli(1700000000000)) {
		t.Errorf("phone mtime not preserved: %v %v", st, err)
	}
	if _, err := os.Stat(filepath.Join(dest, "docs", "empty.bin")); err != nil {
		t.Errorf("empty file not created: %v", err)
	}
	// Duplicate finish is a no-op.
	if again, err := up.Finish(); err != nil || again != nil {
		t.Errorf("duplicate finish = %+v, %v", again, err)
	}
	// Finished uploads still authenticate (consistent duplicate 200s)
	// and the staging dir cleans up.
	if _, ok := mgr.ByKey(up.KeyEncoded()); !ok {
		t.Error("finished upload no longer authenticates")
	}
	up.Cleanup()
	if _, err := os.Stat(up.Staging); !os.IsNotExist(err) {
		t.Errorf("staging dir survived cleanup: %v", err)
	}
}

// One link, several batches: a finished slot stays listed (flagged
// Finished, so the dashboard's card keeps its QR instead of re-minting
// a second slot — the old key must never outlive a Close), "send more"
// restarts the same slot, a stray chunk after finish fails cleanly,
// and Close deletes the slot so the key goes dead for every purpose.
func TestSendMoreAfterFinish(t *testing.T) {
	dest := t.TempDir()
	mgr := NewManager()
	up, err := mgr.NewPending(dest)
	if err != nil {
		t.Fatal(err)
	}
	mkSpec := func(name string, payload []byte) FileSpec {
		sum := sha256.Sum256(payload)
		return FileSpec{
			Path: name, Size: int64(len(payload)), ChunkCount: 1,
			Hashes: []string{hex.EncodeToString(sum[:])}, MtimeMs: 1700000000000,
		}
	}

	// Batch 1 lands.
	b1 := []byte("first batch")
	if _, err := mgr.Start(up.KeyEncoded(), []FileSpec{mkSpec("b1.txt", b1)}, 1<<30); err != nil {
		t.Fatal(err)
	}
	if err := up.StoreChunk(0, 0, b1, up.Files[0].Hashes[0]); err != nil {
		t.Fatal(err)
	}
	if _, err := up.Finish(); err != nil {
		t.Fatal(err)
	}

	// The finished slot stays listed, flagged Finished — the card's
	// watcher keys on presence, so the same QR survives for batch 2
	// (a re-mint here is what once let a closed key keep working).
	list := mgr.List()
	if len(list) != 1 || list[0].ID != up.ID || !list[0].Finished {
		t.Fatalf("finished slot not listed as finished: %+v", list)
	}

	// Batch 2 on the same link ("send more").
	b2 := []byte("second batch!")
	if _, err := mgr.Start(up.KeyEncoded(), []FileSpec{mkSpec("b2.txt", b2)}, 1<<30); err != nil {
		t.Fatalf("send-more start rejected: %v", err)
	}
	list = mgr.List()
	if len(list) != 1 || list[0].Finished {
		t.Fatalf("restarted slot not listed as in-flight: %+v", list)
	}
	if err := up.StoreChunk(0, 0, b2, up.Files[0].Hashes[0]); err != nil {
		t.Fatalf("batch-2 chunk: %v", err)
	}
	landed, err := up.Finish()
	if err != nil {
		t.Fatalf("batch-2 finish: %v", err)
	}
	if len(landed) != 1 || landed[0].Rel != "b2.txt" {
		t.Fatalf("batch-2 landed = %+v", landed)
	}
	got, err := os.ReadFile(filepath.Join(dest, "b2.txt"))
	if err != nil || string(got) != string(b2) {
		t.Errorf("b2.txt = %q, %v", got, err)
	}
	got1, err := os.ReadFile(filepath.Join(dest, "b1.txt"))
	if err != nil || string(got1) != string(b1) {
		t.Errorf("b1.txt clobbered by batch 2: %q, %v", got1, err)
	}

	// Stray chunk between batches (staging cleaned at finish) fails
	// cleanly — a client error, not a 500.
	b3 := []byte("third!")
	if err := up.StoreChunk(0, 0, b3, up.Files[0].Hashes[0]); err != ErrBadChunk {
		t.Errorf("stray chunk after finish: got %v, want ErrBadChunk", err)
	}

	// Close link kills the key for everything: no more ByKey (the
	// phone's chunk/finish/start get 401) and nothing listed.
	if !mgr.Close(up.KeyEncoded()) {
		t.Fatal("close after finish returned false")
	}
	if _, ok := mgr.ByKey(up.KeyEncoded()); ok {
		t.Error("closed finished slot still authenticates by key")
	}
	if n := len(mgr.List()); n != 0 {
		t.Errorf("closed slot still listed: %d", n)
	}
}

// Staging must be invisible to the sync scan (it lives inside hashes/)
// — a half-uploaded file must never be announced to peers.
func TestStagingInvisibleToWalk(t *testing.T) {
	dest := t.TempDir()
	mgr := NewManager()
	up, err := mgr.NewPending(dest)
	if err != nil {
		t.Fatal(err)
	}
	payload := []byte("x")
	sum := sha256.Sum256(payload)
	if _, err := mgr.Start(up.KeyEncoded(), []FileSpec{{
		Path: "half.jpg", Size: 1, ChunkCount: 1,
		Hashes: []string{hex.EncodeToString(sum[:])},
	}}, 1<<30); err != nil {
		t.Fatal(err)
	}
	if err := up.storeChunk(up.Files[0], 0, payload, up.Files[0].Hashes[0]); err != nil {
		t.Fatal(err)
	}
	// Mirror the sync scan's exclusion rules: nothing under hashes/.
	var found []string
	err = filepath.WalkDir(dest, func(p string, d os.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if d.IsDir() && d.Name() == CacheDirName && filepath.Dir(p) == dest {
			return filepath.SkipDir
		}
		if !d.IsDir() {
			found = append(found, p)
		}
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	if len(found) != 0 {
		t.Errorf("staged file visible outside hashes/: %v", found)
	}
}
