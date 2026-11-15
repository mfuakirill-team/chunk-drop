// Package upload implements the engine side of phone→PC pushes (M1,
// docs/PROTOCOL.md §4 upload table). An upload session is RAM-only
// state keyed by a 16-byte key carried in the /u/ link fragment — the
// same possession-proves-access model as receive sessions, just with
// the direction reversed.
//
// Bytes arrive as chunk PUTs, are verified server-side (SHA-256 per
// chunk, against both the client's X-Chunk-Hash and the declared
// manifest hash), and are staged under <dest>/hashes/uploads/<id>/ —
// inside the excluded hashes/ metadata folder, so a half-uploaded file
// is invisible to the sync scan and never announced to peers. finish
// verifies completeness, then renames each staged file into place
// (atomic, same filesystem) and stamps the phone's mtime.
package upload

import (
	"crypto/sha256"
	"crypto/subtle"
	"encoding/hex"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"

	"chunkdrop/internal/logx"
	"chunkdrop/internal/session"
	"chunkdrop/internal/transfer"
)

const (
	// DefaultTTL is how long an upload session stays alive — mirror
	// receive sessions (PROTOCOL.md §1): 24 h, RAM-only, restart kills.
	DefaultTTL = 24 * time.Hour

	// UploadsTmpDir is where staged upload bytes live, relative to the
	// destination dir: <dest>/hashes/uploads/<uploadId>/. Inside the
	// excluded "hashes" metadata folder (the same name syncmesh uses:
	// never scanned, never announced, never touched by the sync
	// round's hashes/tmp cleanup).
	UploadsTmpDir = "uploads"
	// CacheDirName is that excluded metadata folder's name ("hashes").
	CacheDirName = "hashes"
)

// FileSpec is one uploaded file as declared by the browser-built
// manifest (PROTOCOL.md §4: the engine verifies every claim).
type FileSpec struct {
	Path       string   `json:"path"`       // relative, "/", UTF-8 NFC
	Size       int64    `json:"size"`       // total bytes
	ChunkCount int      `json:"chunkCount"` // == ChunkCountFor(size, 4 MiB)
	Hashes     []string `json:"hashes"`     // sha256 hex per chunk
	MtimeMs    int64    `json:"mtime"`      // lastModified, ms since epoch (0 = now)
}

// File is one accepted upload file's runtime state.
type File struct {
	Index      int
	Path       string // sanitized (possibly collision-renamed) rel path
	Size       int64
	ChunkCount int
	Hashes     []string
	Mtime      time.Time
	TmpPath    string // staged file under <dest>/hashes/uploads/<id>/

	mu   sync.Mutex
	have map[int]bool // received chunks
}

// Have returns the count of received chunks of f.
func (f *File) Have() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return len(f.have)
}

// HaveAll reports whether every declared chunk arrived (or the file is
// empty and nothing needed to arrive).
func (f *File) HaveAll() bool {
	f.mu.Lock()
	defer f.mu.Unlock()
	return len(f.have) >= f.ChunkCount
}

// Upload is one RAM-only upload session.
type Upload struct {
	ID      string
	Key     []byte // 16 random bytes, base64url on the wire
	Dest    string // absolute final destination dir
	Staging string // absolute staging dir for this upload
	Files   []*File

	CreatedAt time.Time
	ExpiresAt time.Time
	Started   bool
	Finished  bool

	mu sync.Mutex
}

// BytesDone returns the received-bytes total across all files (each
// arrived chunk counts its declared size — the last may be short).
func (u *Upload) BytesDone() int64 {
	var n int64
	for _, f := range u.Files {
		f.mu.Lock()
		for c := range f.have {
			n += chunkExpectedSize(f, c)
		}
		f.mu.Unlock()
	}
	return n
}

// BytesTotal returns the declared total across all files.
func (u *Upload) BytesTotal() int64 {
	var n int64
	for _, f := range u.Files {
		n += f.Size
	}
	return n
}

// validateSpecs applies the engine-side checks (PROTOCOL.md §4 upload
// start + §8): sanitize every path, consistent chunk math, hash shape,
// manifest cap, total-size cap, case-collision rename.
func validateSpecs(specs []FileSpec, maxTotal int64) ([]*File, error) {
	if len(specs) == 0 {
		return nil, errors.New("no files")
	}
	if len(specs) > transfer.MaxManifestFiles {
		return nil, fmt.Errorf("%d files exceeds the %d-file cap",
			len(specs), transfer.MaxManifestFiles)
	}
	var total int64
	files := make([]transfer.File, 0, len(specs))
	for _, sp := range specs {
		if sp.Size < 0 {
			return nil, fmt.Errorf("negative size for %q", sp.Path)
		}
		if want := transfer.ChunkCountFor(sp.Size, transfer.DefaultChunkSize); sp.ChunkCount != want {
			return nil, fmt.Errorf("file %q: chunkCount %d, expected %d", sp.Path, sp.ChunkCount, want)
		}
		if len(sp.Hashes) != sp.ChunkCount {
			return nil, fmt.Errorf("file %q: %d hashes for %d chunks", sp.Path, len(sp.Hashes), sp.ChunkCount)
		}
		for _, h := range sp.Hashes {
			if len(h) != sha256.Size*2 || strings.ToLower(h) != h {
				return nil, fmt.Errorf("file %q: malformed chunk hash", sp.Path)
			}
		}
		rel, err := transfer.SanitizePath(sp.Path)
		if err != nil {
			return nil, fmt.Errorf("path %q: %w", sp.Path, err)
		}
		total += sp.Size
		files = append(files, transfer.File{Path: rel, Size: sp.Size, ChunkCount: sp.ChunkCount})
	}
	if total > maxTotal {
		return nil, fmt.Errorf("total %d bytes exceeds the upload cap", total)
	}
	files = transfer.ResolveCaseCollisions(files) // PROTOCOL.md §8 rule 6

	out := make([]*File, 0, len(specs))
	for i, f := range files {
		mtime := time.Now()
		if specs[i].MtimeMs > 0 {
			mtime = time.UnixMilli(specs[i].MtimeMs)
		}
		out = append(out, &File{
			Index:      i,
			Path:       f.Path,
			Size:       specs[i].Size,
			ChunkCount: specs[i].ChunkCount,
			Hashes:     specs[i].Hashes,
			Mtime:      mtime,
			have:       map[int]bool{},
		})
	}
	return out, nil
}

// Manager owns all live upload sessions (RAM-only; an engine restart
// kills every upload — stale staging dirs are cleared at startup).
type Manager struct {
	mu      sync.Mutex
	uploads map[string]*Upload
	ttl     time.Duration
}

// NewManager creates an upload manager with the default 24 h TTL.
func NewManager() *Manager {
	return &Manager{uploads: map[string]*Upload{}, ttl: DefaultTTL}
}

// NewPending mints an upload slot: an id + key waiting for the phone's
// manifest. The dashboard creates it when showing the QR/link.
func (m *Manager) NewPending(dest string) (*Upload, error) {
	key, err := session.NewKey()
	if err != nil {
		return nil, err
	}
	u := &Upload{
		ID:        session.NewID(),
		Key:       key,
		Dest:      dest,
		CreatedAt: time.Now(),
		ExpiresAt: time.Now().Add(m.ttl),
	}
	m.mu.Lock()
	m.uploads[u.ID] = u
	m.mu.Unlock()
	return u, nil
}

// Start fills a pending upload with the browser-built manifest,
// validating every spec. Keyed by the upload key (the phone's proof),
// not the id — ids are only ever used in URL paths after authentication.
//
// A started slot may be started again — mid-transfer (the phone lost
// its connection and re-picked files) or after a finished batch (the
// phone's "send more"): the slot resets and everything re-uploads from
// scratch. Staging lives only under the excluded hashes/uploads tree
// and is re-verified by finish, so a restart can never land stale or
// half-verified bytes. One link is therefore reusable for several
// batches until it expires or the dashboard closes it.
func (m *Manager) Start(presentedKey string, specs []FileSpec, maxTotal int64) (*Upload, error) {
	files, err := validateSpecs(specs, maxTotal)
	if err != nil {
		return nil, err
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	u, ok := m.byKeyLocked(presentedKey)
	if !ok {
		return nil, session.ErrNotFound
	}
	u.mu.Lock()
	defer u.mu.Unlock()
	wasFinished := u.Finished
	u.Finished = false // "send more" on a finished link starts a new batch
	wasStarted := u.Started
	u.Started = true
	u.Files = files
	u.Staging = filepath.Join(u.Dest, CacheDirName, UploadsTmpDir, u.ID)
	// A restart wipes the previous staging dir — the new manifest is
	// the only source of truth (mixed old/new .part files would be
	// unverifiable against the new declared hashes).
	if wasStarted {
		_ = os.RemoveAll(u.Staging)
	}
	if err := os.MkdirAll(u.Staging, 0o755); err != nil {
		u.Started = false
		return nil, fmt.Errorf("create staging dir: %w", err)
	}
	for _, f := range files {
		f.TmpPath = filepath.Join(u.Staging, fmt.Sprintf("f%d.part", f.Index))
		// Create the staged file now (empty for zero-chunk files) so
		// finish's size check can trust what it stats.
		fh, err := os.OpenFile(f.TmpPath, os.O_CREATE|os.O_WRONLY, 0o644)
		if err != nil {
			return nil, fmt.Errorf("stage %s: %w", f.Path, err)
		}
		fh.Close()
	}
	if wasStarted {
		if wasFinished {
			logx.Warn("upload", "slot %s: new batch on a finished link (phone tapped \"send more\")", u.ID)
		} else {
			logx.Warn("upload", "slot %s restarted by the phone: state reset, staging wiped", u.ID)
		}
	}
	return u, nil
}

// byKeyLocked finds the live-or-finished upload whose encoded key
// matches, constant-time compare per candidate (mirrors
// session.Manager.ByKey). Finished slots are found too: a duplicate
// finish must authenticate (idempotent 200s), and "send more" must be
// able to start a new batch on the same link.
func (m *Manager) byKeyLocked(presentedKey string) (*Upload, bool) {
	for _, u := range m.uploads {
		if time.Now().After(u.ExpiresAt) {
			continue
		}
		if subtle.ConstantTimeCompare([]byte(presentedKey), []byte(session.KeyEncoding(u.Key))) == 1 {
			return u, true
		}
	}
	return nil, false
}

// ByKey authenticates a request key. Presenting the key of a *finished*
// upload keeps returning it until expiry so a duplicate finish/retry
// gets a consistent response instead of a generic 401.
func (m *Manager) ByKey(presentedKey string) (*Upload, bool) {
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.byKeyLocked(presentedKey)
}

// Get returns the live upload for id — or ErrNotFound.
func (m *Manager) Get(id string) (*Upload, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	u, ok := m.uploads[id]
	if !ok {
		return nil, session.ErrNotFound
	}
	if time.Now().After(u.ExpiresAt) {
		return nil, session.ErrExpired
	}
	return u, nil
}

// Close removes an upload (dashboard "close link") and wipes its
// staging dir. The key identifies the upload from the dashboard.
func (m *Manager) Close(presentedKey string) bool {
	m.mu.Lock()
	var victim *Upload
	for id, u := range m.uploads {
		if subtle.ConstantTimeCompare([]byte(presentedKey), []byte(session.KeyEncoding(u.Key))) == 1 {
			victim = u
			delete(m.uploads, id)
			break
		}
	}
	m.mu.Unlock()
	if victim == nil {
		return false
	}
	if victim.Staging != "" {
		_ = os.RemoveAll(victim.Staging)
	}
	return true
}

// Reap deletes expired uploads and their staging dirs (called from the
// engine's reap ticker; safe to call manually in tests).
func (m *Manager) Reap() {
	m.mu.Lock()
	var expired []*Upload
	for id, u := range m.uploads {
		if time.Now().After(u.ExpiresAt) {
			expired = append(expired, u)
			delete(m.uploads, id)
		}
	}
	m.mu.Unlock()
	for _, u := range expired {
		if u.Staging != "" {
			_ = os.RemoveAll(u.Staging)
		}
	}
}

// CleanStale removes leftover staging dirs from a previous engine run
// (no upload can be in flight while the engine is starting).
func (m *Manager) CleanStale(dest string) {
	root := filepath.Join(dest, CacheDirName, UploadsTmpDir)
	ents, err := os.ReadDir(root)
	if err != nil {
		return
	}
	for _, e := range ents {
		_ = os.RemoveAll(filepath.Join(root, e.Name()))
	}
}

// Info is the dashboard-facing summary of one upload session.
type Info struct {
	ID    string   `json:"id"`
	Files int      `json:"files"`
	Done  int64    `json:"bytesDone"`
	Total int64    `json:"bytesTotal"`
	Since string   `json:"since"`
	Names []string `json:"names,omitempty"` // first NamesCap paths (dashboard display)
}

// NamesCap bounds the names list in Info — a 1000-photo push must not
// bloat the polled activity JSON; the dashboard shows "+N more" using
// the Files count.
const NamesCap = 8

// List summarizes all live uploads for the activity feed. Finished
// slots are not listed — they are no longer in flight (and the
// dashboard's upload card uses the absence of its slot id here to
// notice a spent link and re-mint a fresh one).
func (m *Manager) List() []Info {
	m.mu.Lock()
	defer m.mu.Unlock()
	out := []Info{}
	for _, u := range m.uploads {
		if time.Now().After(u.ExpiresAt) || u.Finished {
			continue
		}
		info := Info{
			ID:    u.ID,
			Files: len(u.Files),
			Done:  u.BytesDone(),
			Total: u.BytesTotal(),
			Since: u.CreatedAt.UTC().Format(time.RFC3339),
		}
		for i, f := range u.Files {
			if i >= NamesCap {
				break
			}
			info.Names = append(info.Names, f.Path)
		}
		out = append(out, info)
	}
	return out
}

// Count returns the number of live uploads (tests).
func (m *Manager) Count() int {
	m.mu.Lock()
	defer m.mu.Unlock()
	return len(m.uploads)
}

// KeyEncoded returns the wire form of this upload's key (QR fragment).
func (u *Upload) KeyEncoded() string { return session.KeyEncoding(u.Key) }

// MatchesID reports whether id is this upload's id — used to bind the
// URL path to the authenticated key (constant-time, like every compare
// of a secret-derived value).
func (u *Upload) MatchesID(id string) bool {
	return subtle.ConstantTimeCompare([]byte(id), []byte(u.ID)) == 1
}

// StoreChunk verifies and writes one chunk of file index i (handler
// entry point; the per-file variant below does the actual work).
func (u *Upload) StoreChunk(i, c int, chunk []byte, declaredHash string) error {
	if i < 0 || i >= len(u.Files) {
		return ErrBadChunk
	}
	return u.storeChunk(u.Files[i], c, chunk, declaredHash)
}

// ExpectedChunkSize returns the declared byte length of chunk c of
// file index i (0 when out of range) — for logging and validation.
func (u *Upload) ExpectedChunkSize(i, c int) int64 {
	if i < 0 || i >= len(u.Files) {
		return 0
	}
	return chunkExpectedSize(u.Files[i], c)
}

// chunkExpectedSize returns the declared byte length of chunk c.
func chunkExpectedSize(f *File, c int) int64 {
	size := int64(transfer.DefaultChunkSize)
	if rem := f.Size - int64(c)*int64(transfer.DefaultChunkSize); rem < size {
		size = rem
	}
	return size
}

// storeChunk verifies and writes one chunk. The hash is checked against
// the header AND the declared manifest hash — a matching header alone
// proves nothing (PROTOCOL.md §4: never trust client claims).
func (u *Upload) storeChunk(f *File, c int, chunk []byte, declaredHash string) error {
	if c < 0 || c >= f.ChunkCount {
		return ErrBadChunk
	}
	if int64(len(chunk)) != chunkExpectedSize(f, c) {
		return ErrBadChunk
	}
	sum := sha256.Sum256(chunk)
	got := hex.EncodeToString(sum[:])
	if declaredHash != "" && subtle.ConstantTimeCompare([]byte(declaredHash), []byte(got)) != 1 {
		return ErrHashMismatch
	}
	if subtle.ConstantTimeCompare([]byte(declaredHash), []byte(f.Hashes[c])) != 1 {
		return ErrHashMismatch
	}
	fh, err := os.OpenFile(f.TmpPath, os.O_WRONLY, 0o644)
	if err != nil {
		return err
	}
	defer fh.Close()
	if _, err := fh.WriteAt(chunk, int64(c)*int64(transfer.DefaultChunkSize)); err != nil {
		return err
	}
	f.mu.Lock()
	f.have[c] = true
	f.mu.Unlock()
	return nil
}

// Landed is one finalized file: rel path inside the destination, its
// absolute path on disk, and its size (for activity lines).
type Landed struct {
	Rel  string
	Abs  string
	Size int64
}

// Finish verifies completeness, renames every staged file into the
// destination (atomic same-filesystem rename), stamps the phone's
// mtime, and returns the landed files for the caller's syncmesh /
// activity bookkeeping.
func (u *Upload) Finish() ([]Landed, error) {
	u.mu.Lock()
	defer u.mu.Unlock()
	if u.Finished {
		return nil, nil // idempotent duplicate finish
	}
	if !u.Started {
		return nil, errors.New("upload not started")
	}
	out := make([]Landed, 0, len(u.Files))
	for _, f := range u.Files {
		if !f.HaveAll() {
			return nil, fmt.Errorf("file %q: %d of %d chunks received", f.Path, f.Have(), f.ChunkCount)
		}
		st, err := os.Stat(f.TmpPath)
		if err != nil {
			return nil, fmt.Errorf("stage %s: %w", f.Path, err)
		}
		if st.Size() != f.Size {
			return nil, fmt.Errorf("file %q: staged %d bytes, declared %d", f.Path, st.Size(), f.Size)
		}
	}
	// All complete: move everything into place.
	for _, f := range u.Files {
		abs := filepath.Join(u.Dest, filepath.FromSlash(f.Path))
		root := filepath.Clean(u.Dest)
		if !strings.HasPrefix(filepath.Clean(abs), root+string(os.PathSeparator)) {
			return nil, fmt.Errorf("path escapes the destination dir: %q", f.Path)
		}
		if err := os.MkdirAll(filepath.Dir(abs), 0o755); err != nil {
			return nil, fmt.Errorf("mkdir for %s: %w", f.Path, err)
		}
		if err := os.Rename(f.TmpPath, abs); err != nil {
			return nil, fmt.Errorf("finalize %s: %w", f.Path, err)
		}
		_ = os.Chtimes(abs, time.Now(), f.Mtime) // phone mtime preserved
		_ = setBirthTime(abs, f.Mtime)           // Windows: "Date created" too
		out = append(out, Landed{Rel: f.Path, Abs: abs, Size: f.Size})
	}
	u.Finished = true
	return out, nil
}

// Cleanup removes the staging dir (after a successful finish nothing is
// left in it; after a failure it stops junk from accumulating).
func (u *Upload) Cleanup() {
	if u.Staging != "" {
		_ = os.RemoveAll(u.Staging)
	}
}

var (
	ErrBadChunk     = errors.New("bad chunk")
	ErrHashMismatch = errors.New("chunk hash mismatch")
)
