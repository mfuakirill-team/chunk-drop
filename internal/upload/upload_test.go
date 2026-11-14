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
	if _, err := mgr.Start(up.KeyEncoded(), specs, 1<<30); err == nil {
		t.Error("double start accepted")
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
