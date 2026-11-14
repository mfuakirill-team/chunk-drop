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
	"sync/atomic"
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

// Deletion identity is name+mtime: the same name at the same or an
// older mtime is the deleted version; a newer mtime is a new file.
func TestDeletedCovers(t *testing.T) {
	m := time.Unix(1700000000, 0)
	d := Deleted{Mtime: m}
	if !d.Covers(m) {
		t.Error("same mtime must be covered")
	}
	if !d.Covers(m.Add(-time.Minute)) {
		t.Error("older mtime must be covered")
	}
	if d.Covers(m.Add(time.Second)) {
		t.Error("newer mtime must survive")
	}
}

// Deletion records: same/older deleted version never rewrites the
// record (keeps its age), a newer deleted version replaces it, the
// record is announced in State() and survives a restart.
func TestDeleteRecordGossip(t *testing.T) {
	l, dir := testLocal(t, map[string]string{"a.txt": "v1"})
	m := time.Unix(1700000000, 0)
	l.RecordDeleted("gone.iso", m)
	if d, ok := l.DeleteInfo("gone.iso"); !ok || !d.Mtime.Equal(m) {
		t.Fatalf("record = %+v ok=%v", d, ok)
	}
	l.RecordDeleted("gone.iso", m.Add(-time.Hour)) // older deletion: ignored
	if d, _ := l.DeleteInfo("gone.iso"); !d.Mtime.Equal(m) {
		t.Errorf("older deleted version overwrote record: %v", d.Mtime)
	}
	l.RecordDeleted("gone.iso", m.Add(time.Hour)) // newer deleted version
	if d, _ := l.DeleteInfo("gone.iso"); !d.Mtime.Equal(m.Add(time.Hour)) {
		t.Errorf("newer deleted version not recorded: %v", d.Mtime)
	}
	if got := l.State().Deleted["gone.iso"]; !got.Equal(m.Add(time.Hour)) {
		t.Errorf("record not gossiped in State(): %v", l.State().Deleted)
	}
	l2, _ := NewLocal(dir, "x", "e")
	if _, ok := l2.DeleteInfo("gone.iso"); !ok {
		t.Error("record lost on restart")
	}
	l.DropDeleted("gone.iso")
	if _, ok := l.DeleteInfo("gone.iso"); ok {
		t.Error("record not dropped")
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

// The deletion resurrection livelock: user deletes a file here, but a
// peer still inside its own delete grace keeps announcing it, so the
// planner re-pulled it within seconds — undoing the deletion, and the
// user's next deletion started the loop again. Tombstones (persisted
// vanish records) must block re-pulling until the deletion has
// propagated, while a genuinely newer remote edit still syncs.
// Full deletion lifecycle: a user delete becomes a gossiped record
// (name+mtime), old announcements can't resurrect it, a newer version
// wins, and restoring the old version is re-deleted by policy.
func TestDeleteLifecycle(t *testing.T) {
	l, dir := testLocal(t, map[string]string{"a.txt": "v1"})
	oldMtime := l.Files()["a.txt"].Mtime

	// User deletes the file → next scan records the deletion identity.
	if err := os.Remove(filepath.Join(dir, "a.txt")); err != nil {
		t.Fatal(err)
	}
	if err := l.Scan(); err != nil {
		t.Fatal(err)
	}
	d, ok := l.DeleteInfo("a.txt")
	if !ok || !d.Mtime.Equal(oldMtime) {
		t.Fatalf("deletion record = %+v ok=%v (want mtime %v)", d, ok, oldMtime)
	}
	// The record survives an engine restart (persisted in cache.json).
	l2, _ := NewLocal(dir, "x", "e")
	if d2, ok := l2.DeleteInfo("a.txt"); !ok || !d2.Mtime.Equal(oldMtime) {
		t.Fatalf("record lost on restart: %+v %v", d2, ok)
	}

	// A peer still announcing the deleted version must NOT resurrect it.
	ann := &Announcement{Engine: "e2", Name: "n2", Files: map[string]*FileMeta{
		"a.txt": {Size: 2, Mtime: oldMtime, Xxh: 1},
	}}
	plan := ComputeNeeded(l.Files(), []*Announcement{ann})
	filterTombstoned(l, plan)
	if len(plan) != 0 {
		t.Fatalf("deleted version planned for re-pull: %v", plan)
	}

	// A NEWER mtime under the same name is a new file: it syncs, and
	// the stale deletion record is dropped.
	live := &Announcement{Engine: "e2", Name: "n2", Files: map[string]*FileMeta{
		"a.txt": {Size: 9, Mtime: oldMtime.Add(time.Hour), Xxh: 7},
	}}
	plan2 := ComputeNeeded(l.Files(), []*Announcement{live})
	filterTombstoned(l, plan2)
	if len(plan2) != 1 {
		t.Fatal("newer remote version must sync")
	}
	if _, ok := l.DeleteInfo("a.txt"); ok {
		t.Error("stale deletion record not dropped for the newer version")
	}

	// Restoring the OLD version locally (same name, old mtime) is
	// re-deleted by the name+mtime policy.
	l3, dir3 := testLocal(t, map[string]string{"b.txt": "v1"})
	bMtime := l3.Files()["b.txt"].Mtime
	if err := os.Remove(filepath.Join(dir3, "b.txt")); err != nil {
		t.Fatal(err)
	}
	if err := l3.Scan(); err != nil {
		t.Fatal(err)
	}
	bPath := filepath.Join(dir3, "b.txt")
	if err := os.WriteFile(bPath, []byte("old copy"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.Chtimes(bPath, bMtime, bMtime); err != nil {
		t.Fatal(err)
	}
	if err := l3.Scan(); err != nil {
		t.Fatal(err)
	}
	if _, ok := l3.DeleteInfo("b.txt"); !ok {
		t.Fatal("record for same/older re-add must remain")
	}
	r := &Runner{Local: l3, Puller: &Puller{GroupKey: "k", Act: activity.NewLog(8)}, Logf: t.Logf}
	r.enforceLocalDeletes()
	if _, err := os.Stat(bPath); !os.IsNotExist(err) {
		t.Error("restored old version was not re-deleted")
	}

	// A genuinely NEWER local version survives and clears the record.
	if err := os.WriteFile(bPath, []byte("new copy"), 0o644); err != nil {
		t.Fatal(err)
	}
	newM := bMtime.Add(time.Hour)
	if err := os.Chtimes(bPath, newM, newM); err != nil {
		t.Fatal(err)
	}
	if err := l3.Scan(); err != nil {
		t.Fatal(err)
	}
	if _, ok := l3.DeleteInfo("b.txt"); ok {
		t.Error("newer local version did not clear the deletion record")
	}
}

// Full SyncOnce loop: the peer announces a file the local user deleted;
// the tombstone must suppress the pull entirely (no session request at
// all) and the file must stay gone.
func TestSyncOnceSuppressesTombstonedPull(t *testing.T) {
	src := bytes.Repeat([]byte("chunkdrop "), 100)
	var starts int32
	oldMtime := time.Now().Add(-time.Hour)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case r.Method == http.MethodGet && r.URL.Path == "/api/sync/state":
			w.Header().Set("Content-Type", "application/json")
			_ = json.NewEncoder(w).Encode(Announcement{Engine: "fake", Name: "fake", Files: map[string]*FileMeta{
				"big.bin": {Size: int64(len(src)), Mtime: oldMtime, Xxh: 1},
			}})
		case r.Method == http.MethodPost && r.URL.Path == "/api/sync/start":
			atomic.AddInt32(&starts, 1)
			man := transfer.NewManifest("fake-session")
			if err := man.BuildFromPairs([]transfer.SourcePair{
				{Rel: "big.bin", Abs: writeSrc(t, src)},
			}, transfer.BuildOptions{ChunkSize: 1024}); err != nil {
				http.Error(w, "boom", 500)
				return
			}
			w.Header().Set("Content-Type", "application/json")
			_ = json.NewEncoder(w).Encode(startResp{SessionID: "fake-session", Key: "fake-key", Manifest: man})
		default:
			http.Error(w, "not found", 404)
		}
	}))
	t.Cleanup(srv.Close)
	peer := PeerRef{Host: strings.TrimPrefix(srv.URL, "http://"), EngineID: "fake", Name: "fake"}

	l, dir := testLocal(t, nil)
	// The file was pulled earlier (registered locally + cached), then
	// the user deleted it off disk.
	disk := filepath.Join(dir, "big.bin")
	if err := os.WriteFile(disk, src, 0o644); err != nil {
		t.Fatal(err)
	}
	l.RecordPulled("big.bin", disk, int64(len(src)), oldMtime, 1)
	if err := os.Remove(disk); err != nil {
		t.Fatal(err)
	}
	if err := l.Scan(); err != nil {
		t.Fatal(err)
	}
	if _, ok := l.DeleteInfo("big.bin"); !ok {
		t.Fatal("deletion record not created")
	}

	r := &Runner{
		Local:  l,
		Puller: &Puller{GroupKey: "k", Act: activity.NewLog(16), Logf: t.Logf},
		Peers:  func() []PeerRef { return []PeerRef{peer} },
	}
	if err := r.SyncOnce(); err != nil {
		t.Fatal(err)
	}
	if atomic.LoadInt32(&starts) != 0 {
		t.Errorf("deleted file re-pulled: %d session request(s)", starts)
	}
	if _, err := os.Stat(disk); !os.IsNotExist(err) {
		t.Error("deleted file resurrected on disk")
	}
	// The record is re-announced, so peers (including ones that come
	// back later) keep learning about the deletion.
	if _, ok := l.State().Deleted["big.bin"]; !ok {
		t.Error("deletion record not gossiped in the announcement")
	}
}

// A peer's delete signal removes our matching copy (name + same/older
// mtime), and the record is re-announced onward (gossip) — this is the
// "device X was offline" path: it gets the signal from any peer that
// still remembers it.
func TestSyncOnceAppliesPeerDeleteSignal(t *testing.T) {
	deletedMtime := time.Now().Add(-time.Hour)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method == http.MethodGet && r.URL.Path == "/api/sync/state" {
			w.Header().Set("Content-Type", "application/json")
			_ = json.NewEncoder(w).Encode(Announcement{
				Engine:  "fake",
				Name:    "fake",
				Files:   map[string]*FileMeta{},
				Deleted: map[string]time.Time{"old.iso": deletedMtime},
			})
			return
		}
		http.Error(w, "not found", 404)
	}))
	t.Cleanup(srv.Close)
	peer := PeerRef{Host: strings.TrimPrefix(srv.URL, "http://"), EngineID: "fake", Name: "fake"}

	l, dir := testLocal(t, nil)
	oldPath := filepath.Join(dir, "old.iso")
	if err := os.WriteFile(oldPath, []byte("deleted version"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.Chtimes(oldPath, deletedMtime, deletedMtime); err != nil {
		t.Fatal(err)
	}
	// A newer file under a different name must be untouched.
	keepPath := filepath.Join(dir, "keep.bin")
	if err := os.WriteFile(keepPath, []byte("keep me"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := l.Scan(); err != nil {
		t.Fatal(err)
	}

	r := &Runner{
		Local:  l,
		Puller: &Puller{GroupKey: "k", Act: activity.NewLog(16), Logf: t.Logf},
		Peers:  func() []PeerRef { return []PeerRef{peer} },
	}
	if err := r.SyncOnce(); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(oldPath); !os.IsNotExist(err) {
		t.Error("matching file not deleted by the peer's signal")
	}
	if _, err := os.Stat(keepPath); err != nil {
		t.Errorf("unrelated file disturbed: %v", err)
	}
	if d, ok := l.DeleteInfo("old.iso"); !ok || !d.Mtime.Equal(deletedMtime) {
		t.Error("peer's deletion record not stored for re-announcement")
	}
	if _, ok := l.State().Deleted["old.iso"]; !ok {
		t.Error("peer's deletion record not gossiped onward")
	}
}

// A NEWER local version beats an incoming delete signal for the same
// name: the file is kept and the stale record dropped.
func TestPeerDeleteSignalLosesToNewerLocal(t *testing.T) {
	older := time.Now().Add(-2 * time.Hour)
	newer := time.Now()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(Announcement{
			Engine:  "fake",
			Name:    "fake",
			Files:   map[string]*FileMeta{},
			Deleted: map[string]time.Time{"doc.txt": older},
		})
	}))
	t.Cleanup(srv.Close)
	peer := PeerRef{Host: strings.TrimPrefix(srv.URL, "http://"), EngineID: "fake", Name: "fake"}

	l, dir := testLocal(t, nil)
	path := filepath.Join(dir, "doc.txt")
	if err := os.WriteFile(path, []byte("edited after the delete"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.Chtimes(path, newer, newer); err != nil {
		t.Fatal(err)
	}
	if err := l.Scan(); err != nil {
		t.Fatal(err)
	}
	r := &Runner{
		Local:  l,
		Puller: &Puller{GroupKey: "k", Act: activity.NewLog(16), Logf: t.Logf},
		Peers:  func() []PeerRef { return []PeerRef{peer} },
	}
	if err := r.SyncOnce(); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(path); err != nil {
		t.Errorf("newer local version deleted by an old signal: %v", err)
	}
	if _, ok := l.State().Deleted["doc.txt"]; ok {
		t.Error("stale deletion record kept despite newer local file")
	}
}

// cache.json must not be rewritten when nothing changed (the scan loop
// runs every 5 s), but must be written when it did change.
func TestCacheQuietWhenUnchanged(t *testing.T) {
	l, dir := testLocal(t, map[string]string{"a.txt": "v1"})
	path := filepath.Join(dir, CacheDirName, "cache.json")
	st1, err := os.Stat(path)
	if err != nil {
		t.Fatal(err)
	}
	time.Sleep(10 * time.Millisecond)
	if err := l.Scan(); err != nil {
		t.Fatal(err)
	}
	st2, err := os.Stat(path)
	if err != nil {
		t.Fatal(err)
	}
	if !st1.ModTime().Equal(st2.ModTime()) {
		t.Errorf("cache.json rewritten with no changes (%v → %v)", st1.ModTime(), st2.ModTime())
	}
	if err := os.WriteFile(filepath.Join(dir, "b.txt"), []byte("x"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := l.Scan(); err != nil {
		t.Fatal(err)
	}
	st3, err := os.Stat(path)
	if err != nil {
		t.Fatal(err)
	}
	if st3.ModTime().Equal(st2.ModTime()) {
		t.Error("cache.json not written after a real change")
	}
}
