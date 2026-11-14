package syncmesh

import (
	"os"
	"path/filepath"
	"testing"
	"time"
)

func testLocal(t *testing.T, files map[string]string) (*Local, string) {
	t.Helper()
	dir := t.TempDir()
	for name, content := range files {
		p := filepath.Join(dir, filepath.FromSlash(name))
		if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(p, []byte(content), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	l, err := NewLocal(dir, "testbox", "engine-1")
	if err != nil {
		t.Fatal(err)
	}
	return l, dir
}

func TestScanHashesAndCaches(t *testing.T) {
	l, dir := testLocal(t, map[string]string{
		"a/b.txt":              "hello",
		"big.bin":              "some bytes",
		CacheDirName + "/junk": "must be ignored",
	})
	files := l.Files()
	if _, ok := files["a/b.txt"]; !ok {
		t.Fatal("a/b.txt missing from scan")
	}
	if _, ok := files[CacheDirName+"/junk"]; ok {
		t.Error("hashes/ subfolder must never be synced")
	}
	// cache written?
	c, err := LoadHashCache(dir)
	if err != nil {
		t.Fatal(err)
	}
	if len(c.Data) != len(files) {
		t.Errorf("cache entries %d != files %d", len(c.Data), len(files))
	}
	// unchanged second scan must reuse (same xxh) — verify via state
	st := l.State()
	if st.Engine != "engine-1" || st.Name != "testbox" {
		t.Errorf("state identity = %+v", st)
	}
	if len(st.Files) != len(files) {
		t.Errorf("state files %d != %d", len(st.Files), len(files))
	}
}

func TestScanRescanAfterChange(t *testing.T) {
	l, dir := testLocal(t, map[string]string{"f.txt": "one"})
	first := l.Files()["f.txt"]
	// change content + mtime → cache miss → re-hash
	time.Sleep(2 * time.Millisecond)
	if err := os.WriteFile(filepath.Join(dir, "f.txt"), []byte("two!"), 0o644); err != nil {
		t.Fatal(err)
	}
	newMtime := time.Now().Add(time.Hour)
	if err := os.Chtimes(filepath.Join(dir, "f.txt"), newMtime, newMtime); err != nil {
		t.Fatal(err)
	}
	if err := l.Scan(); err != nil {
		t.Fatal(err)
	}
	second := l.Files()["f.txt"]
	if second.Xxh == first.Xxh {
		t.Error("xxhash did not change after content change")
	}
}

func TestResolveRefusesEscape(t *testing.T) {
	l, _ := testLocal(t, nil)
	for _, rel := range []string{"../../etc/passwd", "/abs/path", "a/../../b", "CON"} {
		if _, err := l.Resolve(rel); err == nil {
			t.Errorf("Resolve(%q) must fail", rel)
		}
	}
	good, err := l.Resolve("photos/cat.jpg")
	if err != nil {
		t.Errorf("good path rejected: %v", err)
	}
	if filepath.Base(good) != "cat.jpg" {
		t.Errorf("Resolve mangled path: %q", good)
	}
}

func TestComputeNeededNewestWins(t *testing.T) {
	local := map[string]*LocalFile{
		"old.txt": {Rel: "old.txt", Mtime: time.Unix(1000, 0), Xxh: 1},
		"sam.txt": {Rel: "sam.txt", Mtime: time.Unix(2000, 0), Xxh: 9},
	}
	anns := []*Announcement{
		{Engine: "e1", Name: "a", Files: map[string]*FileMeta{
			"new.txt": {Size: 5, Mtime: time.Unix(3000, 0), Xxh: 2},
			"old.txt": {Size: 3, Mtime: time.Unix(1000, 0), Xxh: 1}, // same: not pulled
		}},
		{Engine: "e2", Name: "b", Files: map[string]*FileMeta{
			"new.txt": {Size: 5, Mtime: time.Unix(3000, 0), Xxh: 2},
			"sam.txt": {Size: 4, Mtime: time.Unix(1500, 0), Xxh: 8}, // older: ignored
		}},
	}
	plan := ComputeNeeded(local, anns)
	if len(plan) != 1 {
		t.Fatalf("plan = %+v", plan)
	}
	n, ok := plan["new.txt"]
	if !ok {
		t.Fatal("missing new.txt")
	}
	if len(n.Peers) != 2 {
		t.Errorf("swarm = %v, want both engines", n.Peers)
	}
	if _, pulled := plan["old.txt"]; pulled {
		t.Error("same-mtime file must not be pulled")
	}
	if _, pulled := plan["sam.txt"]; pulled {
		t.Error("older remote must not win over newer local")
	}
}

func TestDeletionsGrace(t *testing.T) {
	old := time.Now().Add(-time.Hour)
	fresh := time.Now()
	local := map[string]*LocalFile{
		"gone.txt":  {Rel: "gone.txt", Mtime: old},
		"fresh.txt": {Rel: "fresh.txt", Mtime: fresh},
	}
	anns := []*Announcement{{Engine: "e", Name: "n", Files: map[string]*FileMeta{
		"other.txt": {Size: 1, Mtime: old, Xxh: 1},
	}}}
	del := Deletions(local, anns)
	if len(del) != 1 || del[0] != "gone.txt" {
		t.Fatalf("deletions = %v", del)
	}
}

func TestDeleteLocal(t *testing.T) {
	l, dir := testLocal(t, map[string]string{"x.txt": "data"})
	if err := os.Remove(filepath.Join(dir, "x.txt")); err != nil {
		t.Fatal(err)
	}
	l.DeleteLocal("x.txt") // must not fail even though disk file is gone
	if _, ok := l.Files()["x.txt"]; ok {
		t.Error("file still listed after DeleteLocal")
	}
}
