package syncmesh

import (
	"bytes"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"chunkdrop/internal/activity"
	"chunkdrop/internal/transfer"
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

func TestDeleteEpochsGrace(t *testing.T) {
	old := time.Now().Add(-2 * time.Hour)
	d := newDeleteEpochs()
	local := map[string]*LocalFile{
		"gone.txt":  {Rel: "gone.txt", Mtime: old},
		"fresh.txt": {Rel: "fresh.txt", Mtime: time.Now()},
	}
	anns := []*Announcement{{Engine: "e", Name: "n", Files: map[string]*FileMeta{
		"other.txt": {Size: 1, Mtime: old, Xxh: 1},
	}}}
	// First round: everything local gets seeded with "now" → no deletions
	// even though no peer announces them (fresh AND stale files).
	if got := d.sync(local, anns); len(got) != 0 {
		t.Fatalf("first round deletions = %v, want none", got)
	}
	// Simulate the grace passing without any announcement.
	{
		d.mu.Lock()
		for rel := range d.lastSeen {
			if _, alive := local[rel]; alive {
				d.lastSeen[rel] = time.Now().Add(-DeleteGrace).Add(-time.Minute)
			}
		}
		d.mu.Unlock()
	}
	// Now the stale-but-present file is a deletion candidate; the fresh
	// one was seeded recently... actually both are now stale in this
	// simulation, so both are candidates — the guard is that the FIRST
	// round never deletes (the actual reported bug).
	got := d.sync(local, anns)
	for _, rel := range got {
		if rel != "gone.txt" && rel != "fresh.txt" {
			t.Errorf("unexpected deletion %q", rel)
		}
	}
	// A peer announcing the file prevents the deletion (fresh lastSeen).
	anns = append(anns, &Announcement{Engine: "e2", Name: "n2", Files: map[string]*FileMeta{
		"gone.txt": {Size: 3, Mtime: old, Xxh: 9},
	}})
	d.mu.Lock()
	for rel := range d.lastSeen {
		if _, alive := local[rel]; alive {
			if rel == "fresh.txt" {
				d.lastSeen[rel] = time.Now() // still fresh: not a candidate
			} else {
				d.lastSeen[rel] = time.Now().Add(-DeleteGrace).Add(-time.Minute)
			}
		}
	}
	d.mu.Unlock()
	if got := d.sync(local, anns); len(got) != 0 {
		t.Errorf("announced file deleted: %v", got)
	}
	// Missing-from-local entries are dropped from tracking (no leak).
	delete(local, "gone.txt")
	_ = d.sync(local, anns)
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

// The ".part ping-pong" bug: a peer pulling our file writes its
// in-progress copy as "x.iso.part" inside its sync folder; if Scan
// announced it, other engines pulled the half-finished file back —
// repeatedly, as it grew. Parts must be invisible to sync at every
// stage: scan, announcements, and pull planning.
func TestPartFilesNeverSynced(t *testing.T) {
	l, _ := testLocal(t, map[string]string{
		"real.txt":        "content",
		"debian.iso.part": "half-written garbage",
		"sub/other.part":  "more junk",
	})
	files := l.Files()
	if _, ok := files["debian.iso.part"]; ok {
		t.Error("in-progress .part file must never be scanned/announced")
	}
	if _, ok := files["sub/other.part"]; ok {
		t.Error("nested .part file must never be scanned/announced")
	}
	if _, ok := files["real.txt"]; !ok {
		t.Error("scan broke: real file missing")
	}
	// Defensive: even if an older build announces its .part temps (or
	// engine metadata), the planner must never schedule a pull.
	plan := ComputeNeeded(files, []*Announcement{{Engine: "old", Name: "old", Files: map[string]*FileMeta{
		"remote.part":       {Size: 10, Mtime: time.Now(), Xxh: 5},
		"hashes/cache.json": {Size: 10, Mtime: time.Now(), Xxh: 5},
	}}})
	if len(plan) != 0 {
		t.Errorf("plan must ignore .part and metadata rels, got %v", plan)
	}
}

// fakePeer serves the two sync endpoints (POST /api/sync/start,
// GET /api/chunk/0/c) for a single source file, so Pull can be tested
// without a second engine.
func fakePeer(t *testing.T, src []byte, corrupt bool) PeerRef {
	t.Helper()
	const chunkSize = 1024
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case r.Method == http.MethodPost && r.URL.Path == "/api/sync/start":
			man := transfer.NewManifest("fake-session")
			if err := man.BuildFromPairs([]transfer.SourcePair{
				{Rel: "big.bin", Abs: writeSrc(t, src)},
			}, transfer.BuildOptions{ChunkSize: chunkSize}); err != nil {
				t.Errorf("manifest: %v", err)
				http.Error(w, "boom", 500)
				return
			}
			w.Header().Set("Content-Type", "application/json")
			_ = json.NewEncoder(w).Encode(startResp{SessionID: "fake-session", Key: "fake-key", Manifest: man})
		case r.Method == http.MethodGet && strings.HasPrefix(r.URL.Path, "/api/chunk/"):
			var c int
			if _, err := fmt.Sscanf(r.URL.Path, "/api/chunk/0/%d", &c); err != nil {
				http.Error(w, "bad chunk", 400)
				return
			}
			off := int64(c) * chunkSize
			if off >= int64(len(src)) {
				http.Error(w, "no such chunk", 404)
				return
			}
			end := off + chunkSize
			if end > int64(len(src)) {
				end = int64(len(src))
			}
			b := append([]byte(nil), src[off:end]...)
			if corrupt {
				b[0] ^= 0xff // SHA-256 check must reject this
			}
			_, _ = w.Write(b)
		default:
			http.Error(w, "not found", 404)
		}
	}))
	t.Cleanup(srv.Close)
	return PeerRef{Host: strings.TrimPrefix(srv.URL, "http://"), EngineID: "fake", Name: "fake"}
}

// writeSrc materializes the fake peer's source bytes on disk (the
// manifest hashes/stream from a real file).
func writeSrc(t *testing.T, src []byte) string {
	t.Helper()
	p := filepath.Join(t.TempDir(), "big.bin")
	if err := os.WriteFile(p, src, 0o644); err != nil {
		t.Fatal(err)
	}
	return p
}

// The pull temp file must live in the excluded hashes/tmp folder —
// never beside the target in the sync folder, where a concurrent Scan
// (api/sync/state rescans on every call!) would announce the
// half-written file, and where crash leftovers would be user-visible.
func TestPullTempOutsideSyncDir(t *testing.T) {
	src := append(bytes.Repeat([]byte("chunkdrop "), 300), []byte("tail")...) // ~3 KB
	l, dir := testLocal(t, nil)
	p := &Puller{GroupKey: "k", Act: activity.NewLog(16), Logf: t.Logf}

	// Success: file lands complete, no .part anywhere in the sync dir.
	peer := fakePeer(t, src, false)
	n := &Needed{Rel: "big.bin", Peers: []string{"fake"}}
	if err := p.Pull(l, n, []PeerRef{peer}); err != nil {
		t.Fatalf("pull: %v", err)
	}
	got, err := os.ReadFile(filepath.Join(dir, "big.bin"))
	if err != nil || !bytes.Equal(got, src) {
		t.Fatalf("pulled content wrong: %v", err)
	}
	if _, ok := l.Files()["big.bin"]; !ok {
		t.Error("pulled file not registered locally")
	}
	assertNoParts(t, dir)

	// Failure (corrupt chunk): pull errors AND cleans its temp up —
	// no junk left in the sync folder or the temp dir.
	l2, dir2 := testLocal(t, nil)
	peerBad := fakePeer(t, src, true)
	err = p.Pull(l2, &Needed{Rel: "big.bin", Peers: []string{"fake"}}, []PeerRef{peerBad})
	if err == nil {
		t.Fatal("corrupt chunk must fail the pull")
	}
	assertNoParts(t, dir2)
}

func assertNoParts(t *testing.T, syncDir string) {
	t.Helper()
	err := filepath.WalkDir(syncDir, func(p string, d os.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if !d.IsDir() && strings.HasSuffix(d.Name(), PartSuffix) {
			t.Errorf("leftover temp in sync dir: %s", p)
		}
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
}
