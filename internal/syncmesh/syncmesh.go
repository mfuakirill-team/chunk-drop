// Package syncmesh implements PC↔PC sync of the shared folder
// ("chunk-drop-files" inside Downloads): engines announce their files
// (xxhash, cached in "hashes/"), pull missing/newer ones from the
// swarm, newest-mtime wins, deletes propagate.
//
// Wire: plain HTTP against the existing chunk API (PROTOCOL.md §4)
// plus two sync endpoints (GET /api/sync/state, POST /api/sync/start),
// group-key gated. The puller fetches chunk c of a file from
// peers[(c) % len(peers)] — torrent-like load balancing — and
// verifies every chunk's SHA-256 (identical on every peer, since the
// file is the same bytes) before writing. Remote mtime is preserved
// (os.Chtimes) so newest-mtime comparisons stay stable and never
// ping-pong between peers.
package syncmesh

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"runtime"
	"sort"
	"strings"
	"sync"
	"time"

	"chunkdrop/internal/activity"
	"chunkdrop/internal/transfer"

	"github.com/cespare/xxhash/v2"
)

// SyncDirName is the synced folder created inside Downloads.
const SyncDirName = "chunk-drop-files"

// CacheDirName is the sync metadata subfolder ("hashes").
const CacheDirName = "hashes"

// PartSuffix marks an in-progress sync pull's temp file ("x.iso.part").
// Temp files are engine-internal state: never scanned, never announced,
// never planned from remote announcements.
const PartSuffix = ".part"

// TombstoneTTL bounds how long a locally-deleted file stays
// tombstoned (hashes/cache.json "vanished" entries). Must comfortably
// exceed any peer's offline time, or its stale copy would resurrect
// the file when it returns.
const TombstoneTTL = 30 * 24 * time.Hour

// ScanInterval is how often the local folder is rescanned.
const ScanInterval = 5 * time.Second

// DeleteGrace is how long a *previously announced* local file must be
// absent from every reachable peer's announcement before delete
// propagation fires. A peer rescanning/restarting mid-announce must not
// cost us data (the "file deleted itself during transfer" bug).
const DeleteGrace = 2 * time.Minute

// LocalFile is one file in the local sync folder.
type LocalFile struct {
	Rel   string // slash path, relative to the sync dir
	Path  string // absolute
	Size  int64
	Mtime time.Time
	Xxh   uint64 // whole-file xxhash
}

// FileMeta is a file as announced over the wire.
type FileMeta struct {
	Size  int64     `json:"size"`
	Mtime time.Time `json:"mtime"`
	Xxh   uint64    `json:"xxh"`
}

// Announcement is one engine's folder state (GET /api/sync/state).
type Announcement struct {
	Engine string               `json:"engine"`
	Name   string               `json:"name"`
	Files  map[string]*FileMeta `json:"files"`
}

// Local is this engine's sync folder state.
type Local struct {
	Dir      string
	Name     string
	EngineID string

	mu    sync.Mutex
	files map[string]*LocalFile
	cache *HashCache
}

// SyncDir returns the chunk-drop-files path (Downloads or the OS
// default download dir), creating it when missing.
func SyncDir() (string, error) {
	base, err := downloadsDir()
	if err != nil {
		return "", err
	}
	dir := filepath.Join(base, SyncDirName)
	if err := os.MkdirAll(filepath.Join(dir, CacheDirName), 0o755); err != nil {
		return "", err
	}
	return dir, nil
}

func downloadsDir() (string, error) {
	home, err := os.UserHomeDir()
	if err != nil {
		return "", err
	}
	switch runtimeOS() {
	case "windows", "darwin":
		return filepath.Join(home, "Downloads"), nil
	}
	if p := os.Getenv("XDG_DOWNLOAD_DIR"); p != "" && !strings.Contains(p, "$") {
		return p, nil
	}
	d := filepath.Join(home, "Downloads")
	if _, err := os.Stat(d); err == nil {
		return d, nil
	}
	return filepath.Join(home, ".local", "share", "chunkdrop", "downloads"), nil
}

// NewLocal scans the sync folder (with the hash cache) and returns
// the engine-side state.
func NewLocal(dir, name, engineID string) (*Local, error) {
	l := &Local{Dir: dir, Name: name, EngineID: engineID, files: map[string]*LocalFile{}}
	if err := l.Scan(); err != nil {
		return nil, err
	}
	return l, nil
}

// Scan walks the sync folder, reusing cached xxhash values for
// unchanged files (size+mtime match) and re-hashing the rest.
func (l *Local) Scan() error {
	cache, err := LoadHashCache(l.Dir)
	if err != nil {
		cache = NewHashCache(l.Dir)
	}
	files := map[string]*LocalFile{}
	err = walkFiles(l.Dir, func(rel, abs string, info os.FileInfo) {
		lf := &LocalFile{Rel: rel, Path: abs, Size: info.Size(), Mtime: info.ModTime()}
		if c, ok := cache.Get(rel); ok && c.Size == info.Size() && c.Mtime.Equal(info.ModTime()) {
			lf.Xxh = c.Xxh
		} else {
			if h, herr := xxFile(abs); herr == nil {
				lf.Xxh = h
				cache.Put(rel, &CacheEntry{Size: info.Size(), Mtime: info.ModTime(), Xxh: h})
			}
		}
		files[rel] = lf
	})
	if err != nil {
		return err
	}
	cache.Prune(files)
	l.mu.Lock()
	l.files = files
	l.cache = cache
	l.mu.Unlock()
	return nil
}

func walkFiles(root string, fn func(rel, abs string, info os.FileInfo)) error {
	return filepath.WalkDir(root, func(p string, d os.DirEntry, err error) error {
		if err != nil {
			if p == root {
				return err
			}
			return nil // unreadable subtree: skip, keep the rest
		}
		if d.IsDir() {
			if p == root {
				return nil
			}
			if rel, _ := filepath.Rel(root, p); rel == CacheDirName {
				return filepath.SkipDir // never sync the cache itself
			}
			return nil
		}
		info, ierr := d.Info()
		if ierr != nil || !info.Mode().IsRegular() {
			return nil
		}
		// In-progress pulls ("x.iso.part") are engine-internal temp
		// files, never content. Announcing them made peers pull each
		// other's half-finished downloads back ("downloaded
		// debian.iso.part from linux") — a self-feeding loop that
		// doubled traffic and littered sync folders with junk.
		if strings.HasSuffix(d.Name(), PartSuffix) {
			return nil
		}
		rel, _ := filepath.Rel(root, p)
		fn(filepath.ToSlash(rel), p, info)
		return nil
	})
}

func xxFile(path string) (uint64, error) {
	f, err := os.Open(path)
	if err != nil {
		return 0, err
	}
	defer f.Close()
	h := xxhash.New()
	if _, err := io.Copy(h, f); err != nil {
		return 0, err
	}
	return h.Sum64(), nil
}

func runtimeOS() string { return runtime.GOOS }

var (
	httpClient     = defaultClient
	httpNewRequest = http.NewRequest
)

func defaultClient() *http.Client { return &http.Client{Timeout: 30 * time.Second} }

// Files returns a copy of the current local file map.
func (l *Local) Files() map[string]*LocalFile {
	l.mu.Lock()
	defer l.mu.Unlock()
	out := make(map[string]*LocalFile, len(l.files))
	for k, v := range l.files {
		c := *v
		out[k] = &c
	}
	return out
}

// State returns the wire announcement for this engine.
func (l *Local) State() *Announcement {
	a := &Announcement{Engine: l.EngineID, Name: l.Name, Files: map[string]*FileMeta{}}
	for rel, f := range l.Files() {
		a.Files[rel] = &FileMeta{Size: f.Size, Mtime: f.Mtime, Xxh: f.Xxh}
	}
	return a
}

// Resolve maps a synced rel path to its absolute local path, refusing
// escapes (PROTOCOL.md §8 receiver-side belt-and-suspenders).
func (l *Local) Resolve(rel string) (string, error) {
	clean, err := transfer.SanitizePath(rel)
	if err != nil {
		return "", err
	}
	abs := filepath.Join(l.Dir, filepath.FromSlash(clean))
	root := filepath.Clean(l.Dir)
	if !strings.HasPrefix(filepath.Clean(abs), root+string(os.PathSeparator)) {
		return "", fmt.Errorf("sync path escapes the sync dir: %q", rel)
	}
	return abs, nil
}

// StartSession builds a manifest of the requested rels (resolved
// inside the sync dir) and registers it as a live session the pulling
// peer fetches chunks from via the normal /api/chunk endpoint.
func (l *Local) StartSession(rels []string, sessions SessionRegistrar) (*ServedSession, *transfer.Manifest, error) {
	files := l.Files()
	pairs := make([]transfer.SourcePair, 0, len(rels))
	for _, rel := range rels {
		f, ok := files[rel]
		if !ok {
			return nil, nil, fmt.Errorf("sync: we no longer have %q", rel)
		}
		pairs = append(pairs, transfer.SourcePair{Rel: f.Rel, Abs: f.Path})
	}
	m := transfer.NewManifest(sessionID())
	if err := m.BuildFromPairs(pairs, transfer.BuildOptions{}); err != nil {
		return nil, nil, err
	}
	sm, err := sessions.NewSession(m)
	if err != nil {
		return nil, nil, err
	}
	return &ServedSession{ID: sm.ID, KeyEncoded: sm.KeyEncoded, Manifest: m}, m, nil
}

// ServedSession is the wire-visible part of a registered sync session
// (id + encoded key; syncmesh must not import session internals).
type ServedSession struct {
	ID         string
	KeyEncoded string
	Manifest   *transfer.Manifest
}

// SessionRegistrar registers a manifest as a live transfer session.
// Satisfied by an adapter over *session.Manager in the api package
// (which imports syncmesh; syncmesh must not import back).
type SessionRegistrar interface {
	NewSession(m *transfer.Manifest) (ServedSession, error)
}

// Session is the wire-visible part of a registered sync session
// (mirrors what *session.Manager returns; kept as a plain struct so
// the puller stays decoupled from session internals).
type Session struct {
	ID         string
	KeyEncoded string
	Manifest   *transfer.Manifest
}

// SessionManager registers a manifest as a live transfer session.
// Satisfied by a tiny adapter over *session.Manager in the api
// package (which imports syncmesh; syncmesh must not import back).
type SessionManager interface {
	NewSession(m *transfer.Manifest) (*Session, error)
}

// RecordPulled notes a completed pull in local state + cache.
func (l *Local) RecordPulled(rel string, abs string, size int64, mtime time.Time, xxh uint64) {
	l.mu.Lock()
	l.files[rel] = &LocalFile{Rel: rel, Path: abs, Size: size, Mtime: mtime, Xxh: xxh}
	if l.cache == nil {
		l.cache = NewHashCache(l.Dir)
	}
	l.cache.Put(rel, &CacheEntry{Size: size, Mtime: mtime, Xxh: xxh})
	l.cache.ClearVanished(rel) // it's back: tombstone off
	l.mu.Unlock()
	// Persist immediately: Scan reloads the cache from disk, so an
	// unsaved entry would be forgotten before its disappearance could
	// be tombstoned.
	_ = l.cache.Save()
}

// VanishedTime reports when rel was last noticed missing from the sync
// folder though previously known (the user deleted it locally). This
// tombstone keeps the puller from re-downloading the file while the
// deletion still propagates to peers. See HashCache.Vanished.
func (l *Local) VanishedTime(rel string) (time.Time, bool) {
	l.mu.Lock()
	defer l.mu.Unlock()
	if l.cache == nil {
		return time.Time{}, false
	}
	l.cache.Mu.Lock()
	defer l.cache.Mu.Unlock()
	t, ok := l.cache.Vanished[rel]
	return t, ok
}

// DeleteLocal removes a local file (delete propagation) and drops its
// cache entry.
func (l *Local) DeleteLocal(rel string) {
	l.mu.Lock()
	f, ok := l.files[rel]
	delete(l.files, rel)
	if l.cache != nil {
		l.cache.Drop(rel)
	}
	l.cache.Save()
	l.mu.Unlock()
	if ok {
		_ = os.Remove(f.Path)
	}
}

var sessionIDFn = func() string { return fmt.Sprintf("sync-%d", time.Now().UnixNano()) }

func sessionID() string { return sessionIDFn() }

// ---------------------------------------------------------------------------
// Hash cache: <syncdir>/hashes/cache.json — one index file, atomic
// rewrite per scan, entry per file {size, mtime, xxh} so unchanged
// files are never re-hashed (xxhash: the fastest usable hash).
// ---------------------------------------------------------------------------

// CacheEntry is one cached hash record.
type CacheEntry struct {
	Size  int64     `json:"size"`
	Mtime time.Time `json:"mtime"`
	Xxh   uint64    `json:"xxh"`
}

// HashCache is the on-disk hash index.
type HashCache struct {
	Dir string

	Mu   sync.Mutex
	Data map[string]*CacheEntry
	// Vanished are delete tombstones: rel → when we first noticed the
	// file gone from the sync folder though previously known. A user
	// deletion must propagate to every peer; without tombstones, a
	// peer still inside its own delete grace keeps announcing the file
	// and the deleter instantly re-pulls it — the "removed here,
	// re-downloaded seconds later" livelock. Persisted in cache.json
	// so a restart doesn't forget deletions either.
	Vanished map[string]time.Time
}

// cacheFile is the persisted shape of the hash cache. The pre-tombstone
// format was a bare map[string]*CacheEntry; LoadHashCache still reads
// that for existing installs (missing entries just re-hash once).
type cacheFile struct {
	Entries  map[string]*CacheEntry `json:"entries"`
	Vanished map[string]time.Time   `json:"vanished"`
}

// NewHashCache builds an empty cache bound to a sync dir.
func NewHashCache(dir string) *HashCache {
	return &HashCache{
		Dir:      filepath.Join(dir, CacheDirName),
		Data:     map[string]*CacheEntry{},
		Vanished: map[string]time.Time{},
	}
}

func (hc *HashCache) indexFile() string { return filepath.Join(hc.Dir, "cache.json") }

// LoadHashCache reads <dir>/hashes/cache.json (missing → empty).
func LoadHashCache(dir string) (*HashCache, error) {
	hc := NewHashCache(dir)
	b, err := os.ReadFile(hc.indexFile())
	if err != nil {
		if os.IsNotExist(err) {
			return hc, nil
		}
		return nil, err
	}
	var cf cacheFile
	if err := json.Unmarshal(b, &cf); err != nil {
		return hc, nil // corrupt: start fresh, files re-hash once
	}
	if cf.Entries == nil && cf.Vanished == nil {
		// Old (pre-tombstone) format: a bare map[string]*CacheEntry.
		var data map[string]*CacheEntry
		if json.Unmarshal(b, &data) == nil && data != nil {
			hc.Data = data
		}
		return hc, nil
	}
	if cf.Entries == nil {
		cf.Entries = map[string]*CacheEntry{}
	}
	if cf.Vanished == nil {
		cf.Vanished = map[string]time.Time{}
	}
	hc.Data, hc.Vanished = cf.Entries, cf.Vanished
	return hc, nil
}

// Get returns a cached entry.
func (hc *HashCache) Get(rel string) (*CacheEntry, bool) {
	hc.Mu.Lock()
	defer hc.Mu.Unlock()
	e, ok := hc.Data[rel]
	return e, ok
}

// Put stores an entry in memory (Save persists).
func (hc *HashCache) Put(rel string, e *CacheEntry) {
	hc.Mu.Lock()
	hc.Data[rel] = e
	hc.Mu.Unlock()
}

// Drop removes an entry (delete propagation).
func (hc *HashCache) Drop(rel string) {
	hc.Mu.Lock()
	delete(hc.Data, rel)
	hc.Mu.Unlock()
}

// ClearVanished removes a tombstone (the file is back).
func (hc *HashCache) ClearVanished(rel string) {
	hc.Mu.Lock()
	delete(hc.Vanished, rel)
	hc.Mu.Unlock()
}

// Prune drops entries for files that no longer exist (turning them
// into tombstones), clears tombstones for files that came back, drops
// expired tombstones, then Save()s.
func (hc *HashCache) Prune(files map[string]*LocalFile) {
	hc.Mu.Lock()
	now := time.Now()
	for rel := range hc.Data {
		if _, ok := files[rel]; ok {
			continue
		}
		delete(hc.Data, rel)
		if _, ok := hc.Vanished[rel]; !ok {
			hc.Vanished[rel] = now // keep the FIRST vanish time
		}
	}
	for rel, t := range hc.Vanished {
		if _, ok := files[rel]; ok {
			delete(hc.Vanished, rel) // re-added locally: tombstone off
			continue
		}
		if now.Sub(t) > TombstoneTTL {
			delete(hc.Vanished, rel) // stale beyond any realistic peer absence
		}
	}
	hc.Mu.Unlock()
	_ = hc.Save()
}

// Save atomically rewrites the index file.
func (hc *HashCache) Save() error {
	hc.Mu.Lock()
	b, err := json.MarshalIndent(cacheFile{Entries: hc.Data, Vanished: hc.Vanished}, "", " ")
	hc.Mu.Unlock()
	if err != nil {
		return err
	}
	if err := os.MkdirAll(hc.Dir, 0o755); err != nil {
		return err
	}
	tmp := hc.indexFile() + ".tmp"
	if err := os.WriteFile(tmp, b, 0o644); err != nil {
		return err
	}
	return os.Rename(tmp, hc.indexFile())
}

// ---------------------------------------------------------------------------
// Diff + swarm planning
// ---------------------------------------------------------------------------

// Needed describes what to pull: a file (missing or a peer has newer)
// and every peer that announced it (the swarm).
type Needed struct {
	Rel   string
	Size  int64
	Mtime time.Time
	Xxh   uint64
	Peers []string // engine ids that announced it
}

// syncableRel filters rels that must never enter the pull plan from
// remote announcements: in-progress ".part" temp files (peers on older
// builds still announce theirs) and the engine's own metadata folder.
func syncableRel(rel string) bool {
	if strings.HasSuffix(rel, PartSuffix) {
		return false
	}
	return rel != CacheDirName && !strings.HasPrefix(rel, CacheDirName+"/")
}

// ComputeNeeded merges peer announcements into a pull plan keyed by
// rel, with the newest mtime as the target and its swarm attached.
func ComputeNeeded(local map[string]*LocalFile, anns []*Announcement) map[string]*Needed {
	type cand struct {
		meta *FileMeta
		who  []string
	}
	cands := map[string]*cand{}
	for _, a := range anns {
		for rel, meta := range a.Files {
			if !syncableRel(rel) {
				continue
			}
			c, ok := cands[rel]
			if !ok {
				c = &cand{meta: meta, who: []string{a.Engine}}
				cands[rel] = c
				continue
			}
			if meta.Mtime.After(c.meta.Mtime) {
				c.meta = meta
				c.who = []string{a.Engine}
			} else if meta.Mtime.Equal(c.meta.Mtime) && c.meta.Xxh == meta.Xxh {
				c.who = append(c.who, a.Engine)
			}
		}
	}
	out := map[string]*Needed{}
	for rel, c := range cands {
		if lf, ok := local[rel]; ok && !c.meta.Mtime.After(lf.Mtime) {
			continue // we have same-age or newer
		}
		out[rel] = &Needed{
			Rel: rel, Size: c.meta.Size, Mtime: c.meta.Mtime, Xxh: c.meta.Xxh,
			Peers: c.who,
		}
	}
	return out
}

// deleteEpochs tracks, per rel, when we *last saw a peer announce* a
// local file (or, for never-announced files, when we first scanned
// it). Delete propagation fires only for files absent from every
// announcement for a full DeleteGrace. This guards against the
// classic races that silently ate files ("remote thinks it's
// deleted → local deletes it while the pull of that very file is
// still in flight"): a peer restarting/rescanning announces an empty
// or partial folder for a few seconds, and without epochs every other
// box would delete everything older than the grace instantly.
type deleteEpochs struct {
	mu       sync.Mutex
	lastSeen map[string]time.Time // rel → last time any peer announced it (or first local scan)
}

func newDeleteEpochs() *deleteEpochs {
	return &deleteEpochs{lastSeen: map[string]time.Time{}}
}

// sync seeds/advances lastSeen from this pass's announcements and
// returns the deletion candidates. Call AFTER a successful full
// announcement round.
func (d *deleteEpochs) sync(local map[string]*LocalFile, anns []*Announcement) []string {
	now := time.Now()
	d.mu.Lock()
	defer d.mu.Unlock()
	// 1. Every locally-present file must exist in the map (seed on
	//    first sight so brand-new files get the full grace too).
	for rel := range local {
		if _, ok := d.lastSeen[rel]; !ok {
			d.lastSeen[rel] = now
		}
	}
	// 2. Advance lastSeen for everything any peer announces.
	for _, a := range anns {
		for rel := range a.Files {
			if t, ok := d.lastSeen[rel]; !ok || now.After(t) {
				d.lastSeen[rel] = now
			}
		}
	}
	// 3. Deletion candidates: local files whose lastSeen is stale
	//    AND that are absent from every current announcement.
	announced := map[string]bool{}
	for _, a := range anns {
		for rel := range a.Files {
			announced[rel] = true
		}
	}
	cut := now.Add(-DeleteGrace)
	var out []string
	for rel, last := range d.lastSeen {
		if _, alive := local[rel]; !alive {
			delete(d.lastSeen, rel) // file's gone locally anyway: forget it
			continue
		}
		if !announced[rel] && last.Before(cut) {
			out = append(out, rel)
		}
	}
	sort.Strings(out)
	return out
}

// ---------------------------------------------------------------------------
// Puller — HTTP client side, swarm round-robin per chunk
// ---------------------------------------------------------------------------

// PeerRef addresses one sync-serving engine.
type PeerRef struct {
	Host     string // host:port
	EngineID string
	Name     string
}

// Puller fetches files from the swarm.
type Puller struct {
	GroupKey string
	Logf     func(string, ...any)
	// Act receives concise activity events (nil = disabled).
	Act *activity.Log
}

// State fetches a peer's announcement (GET /api/sync/state).
func (p *Puller) State(peer PeerRef) (*Announcement, error) {
	var a Announcement
	if err := p.do(peer, "GET", "/api/sync/state", nil, &a); err != nil {
		return nil, err
	}
	return &a, nil
}

func (p *Puller) do(peer PeerRef, method, path string, body, out any) error {
	var rdr io.Reader
	if body != nil {
		b, err := json.Marshal(body)
		if err != nil {
			return err
		}
		rdr = strings.NewReader(string(b))
	}
	req, err := httpNewRequest(method, "http://"+peer.Host+path, rdr)
	if err != nil {
		return err
	}
	req.Header.Set("X-ChunkDrop-Key", p.GroupKey)
	if body != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	resp, err := httpClient().Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	b, err := io.ReadAll(io.LimitReader(resp.Body, 64<<20))
	if err != nil {
		return err
	}
	if resp.StatusCode != 200 {
		return fmt.Errorf("peer %s: %s → HTTP %d", peer.Name, path, resp.StatusCode)
	}
	if out == nil {
		return nil
	}
	return json.Unmarshal(b, out)
}

// startResp mirrors POST /api/sync/start.
type startResp struct {
	SessionID string             `json:"sessionId"`
	Key       string             `json:"key"`
	Manifest  *transfer.Manifest `json:"manifest"`
}

// StartSession creates a one-file sync session on a peer.
func (p *Puller) StartSession(peer PeerRef, rel string) (*startResp, error) {
	var r startResp
	if err := p.do(peer, "POST", "/api/sync/start", map[string]any{"rels": []string{rel}}, &r); err != nil {
		return nil, err
	}
	return &r, nil
}

// ErrNoPeer marks a file no live peer currently has.
var ErrNoPeer = errors.New("sync: no peer has this file")

// Pull downloads one file from its swarm: chunk c comes from
// peers[c % len(peers)] (round-robin), falling over to the next peer
// on error, every chunk SHA-256-verified against the manifest before
// it is written. Only peers that returned a session key participate.
func (p *Puller) Pull(l *Local, n *Needed, peers []PeerRef) error {
	var man *transfer.Manifest
	keys := map[string]string{} // engineID → session key
	var lastErr error
	for _, pe := range peers {
		r, err := p.StartSession(pe, n.Rel)
		if err != nil {
			lastErr = err
			continue
		}
		keys[pe.EngineID] = r.Key
		if man == nil {
			man = r.Manifest
		}
	}
	if man == nil {
		return fmt.Errorf("sync: no peer could start a session for %q: %v", n.Rel, lastErr)
	}
	if len(man.Files) != 1 {
		return fmt.Errorf("sync: expected single-file manifest for %q", n.Rel)
	}
	f0 := man.Files[0]

	// Swarming peers: those that gave us a session for this file.
	var swarm []PeerRef
	for _, pe := range peers {
		if _, ok := keys[pe.EngineID]; ok {
			swarm = append(swarm, pe)
		}
	}

	abs, err := l.Resolve(n.Rel)
	if err != nil {
		return err
	}
	if err := os.MkdirAll(filepath.Dir(abs), 0o755); err != nil {
		return err
	}
	// Temp file lives in the excluded metadata folder (hashes/tmp),
	// never beside the target: a half-written pull must not show up in
	// the sync folder where Scan would announce it to peers (the
	// ".part ping-pong" bug) or litter the user's folder after a
	// crash. Same filesystem → the final rename is still atomic.
	tmpDir := filepath.Join(l.Dir, CacheDirName, "tmp")
	if err := os.MkdirAll(tmpDir, 0o755); err != nil {
		return err
	}
	part := filepath.Join(tmpDir, filepath.Base(n.Rel)+PartSuffix)
	f, err := os.OpenFile(part, os.O_CREATE|os.O_WRONLY|os.O_TRUNC, 0o644)
	if err != nil {
		return err
	}
	defer f.Close()

	for c := range f0.ChunkCount {
		var bytes []byte
		ok := false
		for try := range swarm {
			pe := swarm[(c+try)%len(swarm)]
			b, _, cerr := p.chunk(pe, keys[pe.EngineID], 0, c)
			if cerr != nil {
				continue
			}
			sum := sha256.Sum256(b)
			if hex.EncodeToString(sum[:]) != f0.Hashes[c] {
				continue // bad bytes from this peer: next one
			}
			bytes, ok = b, true
			break
		}
		if !ok {
			_ = os.Remove(part)
			return fmt.Errorf("sync: chunk %d of %q failed on every peer", c, n.Rel)
		}
		if _, err := f.WriteAt(bytes, int64(c)*int64(man.ChunkSize)); err != nil {
			_ = os.Remove(part)
			return err
		}
	}
	if err := f.Sync(); err != nil {
		_ = os.Remove(part)
		return err
	}
	if err := f.Close(); err != nil {
		_ = os.Remove(part)
		return err
	}
	if err := os.Rename(part, abs); err != nil {
		_ = os.Remove(part)
		return err
	}
	// Preserve remote mtime (stable newest-wins comparisons) and
	// refresh the local hash cache for the new content.
	if err := os.Chtimes(abs, time.Now(), n.Mtime); err == nil {
		if xxh, xerr := xxFile(abs); xerr == nil {
			st, _ := os.Stat(abs)
			l.RecordPulled(n.Rel, abs, st.Size(), n.Mtime, xxh)
		}
	}
	if p.Logf != nil {
		p.Logf("sync: pulled %s (%d B) from %d peer(s)", n.Rel, n.Size, len(swarm))
	}
	names := make([]string, len(swarm))
	for i, pe := range swarm {
		names[i] = pe.Name
	}
	p.Act.Add(activity.KindDown, fmt.Sprintf("downloaded %s (%s) from %s", n.Rel, humanBytes(n.Size), strings.Join(names, ", ")))
	return nil
}

// chunk fetches chunk c of file f from one peer session.
func (p *Puller) chunk(peer PeerRef, key string, f, c int) ([]byte, string, error) {
	req, err := httpNewRequest("GET", fmt.Sprintf("http://%s/api/chunk/%d/%d", peer.Host, f, c), nil)
	if err != nil {
		return nil, "", err
	}
	req.Header.Set("X-ChunkDrop-Key", key)
	resp, err := httpClient().Do(req)
	if err != nil {
		return nil, "", err
	}
	defer resp.Body.Close()
	if resp.StatusCode != 200 {
		return nil, "", fmt.Errorf("peer %s: chunk %d/%d → HTTP %d", peer.Name, f, c, resp.StatusCode)
	}
	b, err := io.ReadAll(io.LimitReader(resp.Body, 64<<20))
	if err != nil {
		return nil, "", err
	}
	return b, resp.Header.Get("X-Chunk-Hash"), nil
}

// filterTombstoned drops planned pulls for locally-deleted files (see
// HashCache.Vanished): a peer still inside its own delete grace keeps
// announcing the file, and without this the deleter instantly
// re-downloads it — the deletion never propagates. A remote mtime
// NEWER than the vanish time beats the tombstone: the file was
// genuinely re-created or edited elsewhere after our deletion.
func filterTombstoned(l *Local, plan map[string]*Needed) {
	for rel, n := range plan {
		if t, ok := l.VanishedTime(rel); ok && !n.Mtime.After(t) {
			delete(plan, rel)
		}
	}
}

// NeededRels gives a deterministic pull order (small files first).
func NeededRels(plan map[string]*Needed) []string {
	out := make([]string, 0, len(plan))
	for k := range plan {
		out = append(out, k)
	}
	sort.Slice(out, func(i, j int) bool {
		if plan[out[i]].Size != plan[out[j]].Size {
			return plan[out[i]].Size < plan[out[j]].Size
		}
		return out[i] < out[j]
	})
	return out
}

// humanBytes renders a size for activity lines (1.5 MB style).
func humanBytes(n int64) string {
	const k = 1024
	switch {
	case n >= k*k*k*k*k: // TiB is plenty for a private LAN
		return fmt.Sprintf("%.1f TB", float64(n)/(k*k*k*k*k))
	case n >= k*k*k*k:
		return fmt.Sprintf("%.1f GB", float64(n)/(k*k*k*k))
	case n >= k*k*k:
		return fmt.Sprintf("%.1f MB", float64(n)/(k*k*k))
	case n >= k*k:
		return fmt.Sprintf("%.1f KB", float64(n)/(k*k))
	default:
		return fmt.Sprintf("%d B", n)
	}
}

// ---------------------------------------------------------------------------
// Runner — the auto-transfer loop (#6): rescan, fetch every peer's
// announcement, compute the plan, pull from swarms, propagate deletes.
// ---------------------------------------------------------------------------

// Runner drives folder sync against discovered peers.
type Runner struct {
	Local    *Local
	Puller   *Puller
	GroupKey string
	Peers    func() []PeerRef // discovered peers (host:port + engine id)
	Logf     func(string, ...any)
	// Act receives concise activity events (nil = disabled).
	Act *activity.Log

	// deleting tracks per-file delete-epoch state (race guard).
	deleting *deleteEpochs
}

// SyncOnce runs one full planning + pull + delete-propagation pass.
// Called by the engine's ticker; safe to call manually (tests).
func (r *Runner) SyncOnce() error {
	if err := r.Local.Scan(); err != nil {
		return err
	}
	peers := r.Peers()
	if len(peers) == 0 {
		return nil // nobody else on the LAN yet
	}
	// 1. Announcements from every peer.
	anns := make([]*Announcement, 0, len(peers))
	for _, pe := range peers {
		a, err := r.Puller.State(pe)
		if err != nil {
			r.logf("sync: peer %s: %v", pe.Name, err)
			r.Act.Add(activity.KindError, fmt.Sprintf("peer %s unreachable: %v", pe.Name, err))
			continue
		}
		anns = append(anns, a)
	}
	if len(anns) == 0 {
		return nil
	}
	// 2. Plan + pull (per file: its swarm — every peer that has it).
	// Pulls are serial within this loop, so any temp file still in
	// hashes/tmp is a crash leftover from a previous run — clear it.
	_ = os.RemoveAll(filepath.Join(r.Local.Dir, CacheDirName, "tmp"))
	plan := ComputeNeeded(r.Local.Files(), anns)
	filterTombstoned(r.Local, plan) // user-deleted here: never re-pull
	engineByName := map[string]PeerRef{}
	for _, pe := range peers {
		engineByName[pe.EngineID] = pe
	}
	for _, rel := range NeededRels(plan) {
		n := plan[rel]
		var swarm []PeerRef
		for _, id := range n.Peers {
			if pe, ok := engineByName[id]; ok {
				swarm = append(swarm, pe)
			}
		}
		if len(swarm) == 0 {
			continue // stale announcement edge: next pass re-plans
		}
		if err := r.Puller.Pull(r.Local, n, swarm); err != nil {
			r.logf("sync: pull %s: %v", rel, err)
			r.Act.Add(activity.KindError, fmt.Sprintf("pull %s failed: %v", rel, err))
		}
	}
	// 3. Delete propagation (files no peer announces for a full grace,
	//    per-file last-seen epochs — see deleteEpochs for the race
	//    this closes).
	if r.deleting == nil {
		r.deleting = newDeleteEpochs()
	}
	for _, rel := range r.deleting.sync(r.Local.Files(), anns) {
		r.Local.DeleteLocal(rel)
		r.logf("sync: deleted %s (deleted on a peer)", rel)
		r.Act.Add(activity.KindDown, fmt.Sprintf("deleted %s (deleted on a peer)", rel))
	}
	return nil
}

func (r *Runner) logf(f string, args ...any) {
	if r.Logf != nil {
		r.Logf(f, args...)
	}
}

// Run blocks running SyncOnce every ScanInterval until stop closes.
func (r *Runner) Run(stop <-chan struct{}) {
	t := time.NewTicker(ScanInterval)
	defer t.Stop()
	for {
		select {
		case <-stop:
			return
		case <-t.C:
			_ = r.SyncOnce()
		}
	}
}
