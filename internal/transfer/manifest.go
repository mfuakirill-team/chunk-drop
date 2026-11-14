// Package transfer implements the ChunkDrop manifest format, chunked
// streaming reads and the store-mode ZIP packer (docs/PROTOCOL.md §3, §4).
package transfer

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"
)

const (
	// DefaultChunkSize is the default chunk size (4 MiB, PROTOCOL.md §3).
	DefaultChunkSize = 4 * 1024 * 1024

	// ManifestVersion is the manifest format version we emit and accept.
	ManifestVersion = 1

	// MaxManifestFiles caps how many entries one manifest may contain.
	// Above this the sender must pack a ZIP.
	MaxManifestFiles = 10000
)

// File describes one file in a manifest.
type File struct {
	Index      int      `json:"index"`
	Path       string   `json:"path"` // relative, sanitized, UTF-8 NFC
	Size       int64    `json:"size"`
	ChunkCount int      `json:"chunkCount"`
	Hashes     []string `json:"hashes"` // sha256 hex, one per chunk

	// SourcePath is the absolute source path on the sender. It never
	// leaves the engine (json:"-") but lets chunk serving stream from disk.
	SourcePath string `json:"-"`
}

// Manifest is the wire manifest (PROTOCOL.md §3).
type Manifest struct {
	Version     int       `json:"version"`
	SessionID   string    `json:"sessionId"`
	CreatedAt   time.Time `json:"createdAt"`
	ChunkSize   int       `json:"chunkSize"`
	PackedAsZip bool      `json:"packedAsZip"`
	Files       []File    `json:"files"`
}

// NewManifest returns an empty manifest with the default chunk size.
func NewManifest(sessionID string) *Manifest {
	return &Manifest{
		Version:   ManifestVersion,
		SessionID: sessionID,
		CreatedAt: time.Now().UTC(),
		ChunkSize: DefaultChunkSize,
	}
}

// ChunkCountFor returns how many chunks of size chunkSize cover n bytes.
func ChunkCountFor(n, chunkSize int64) int {
	if chunkSize <= 0 {
		panic("chunkSize <= 0")
	}
	return int((n + chunkSize - 1) / chunkSize)
}

// srcFile is plain metadata collected during a walk (no open handles).
type srcFile struct {
	relPath string // manifest-relative path
	absPath string // absolute source path
	size    int64
}

// BuildOptions controls BuildFromPaths.
type BuildOptions struct {
	// ChunkSize defaults to DefaultChunkSize.
	ChunkSize int
	// ZipPath, when non-empty, packs the whole selection into one
	// store-mode ZIP at this path; the manifest then describes that
	// single file (PROTOCOL.md §3 "packedAsZip").
	ZipPath string
}

// SourcePair is an explicit (relative manifest path, absolute source)
// pair — used by callers that resolve paths themselves (syncmesh),
// bypassing the filesystem walk.
type SourcePair struct {
	Rel string // slash-separated, sanitized by BuildFromPairs
	Abs string // absolute source path to hash/stream from
}

// BuildFromPairs builds a manifest from explicit pairs. Rels are
// sanitized (PROTOCOL.md §8), size is taken from stat of Abs.
func (m *Manifest) BuildFromPairs(pairs []SourcePair, opt BuildOptions) error {
	chunkSize := int64(opt.ChunkSize)
	if chunkSize <= 0 {
		chunkSize = DefaultChunkSize
	}
	m.ChunkSize = int(chunkSize)
	m.PackedAsZip = false
	m.Files = make([]File, 0, len(pairs))
	for i, p := range pairs {
		rel, err := SanitizePath(p.Rel)
		if err != nil {
			return fmt.Errorf("path %q: %w", p.Rel, err)
		}
		info, err := os.Stat(p.Abs)
		if err != nil {
			return fmt.Errorf("stat %s: %w", p.Abs, err)
		}
		if !info.Mode().IsRegular() {
			return fmt.Errorf("%s: not a regular file", p.Abs)
		}
		mf := File{
			Index:      i,
			Path:       rel,
			Size:       info.Size(),
			SourcePath: p.Abs,
			ChunkCount: ChunkCountFor(info.Size(), chunkSize),
		}
		if err := mf.hashSource(mf.SourcePath, chunkSize); err != nil {
			return fmt.Errorf("hash %s: %w", p.Abs, err)
		}
		m.Files = append(m.Files, mf)
	}
	if len(m.Files) == 0 {
		return errors.New("nothing to send: no files given")
	}
	return nil
}

// BuildFromPaths walks the picked files/folders and fills the manifest.
// Files are streamed later (ChunkRead), never held whole in RAM.
//
// If opt.ZipPath is set, everything is packed into one store-mode ZIP
// (no compression) and the manifest describes just that archive.
func (m *Manifest) BuildFromPaths(paths []string, opt BuildOptions) error {
	chunkSize := int64(opt.ChunkSize)
	if chunkSize <= 0 {
		chunkSize = DefaultChunkSize
	}
	m.ChunkSize = int(chunkSize)

	seen := map[string]bool{}
	var files []srcFile
	var total int64
	roots := make([]string, 0, len(paths))
	for _, p := range paths {
		if p == "" {
			continue
		}
		cp := filepath.Clean(p)
		if seen[cp] {
			continue // ignore double-picked roots
		}
		seen[cp] = true
		roots = append(roots, cp)
	}
	sort.Strings(roots) // deterministic order across runs

	count := 0
	for _, root := range roots {
		var err error
		var rootTotal int64
		files, count, rootTotal, err = m.collectRoot(root, files, count)
		if err != nil {
			return err
		}
		total += rootTotal
	}
	if count == 0 {
		return errors.New("nothing to send: no regular files found")
	}
	if opt.ZipPath == "" && count > MaxManifestFiles {
		return fmt.Errorf("%d files exceeds the %d-file manifest cap — pack a ZIP instead",
			count, MaxManifestFiles)
	}

	if opt.ZipPath != "" {
		return m.buildPacked(files, opt.ZipPath, chunkSize)
	}
	return m.buildUnpacked(files, chunkSize)
}

// collectRoot appends one picker root (a single file, or a walked folder)
// and returns the updated slice + counters.
func (m *Manifest) collectRoot(root string, files []srcFile, count int) ([]srcFile, int, int64, error) {
	info, err := os.Lstat(root)
	if err != nil {
		return files, count, 0, fmt.Errorf("stat %s: %w", root, err)
	}
	if !info.IsDir() {
		if !info.Mode().IsRegular() {
			return files, count, 0, fmt.Errorf("%s: not a regular file", root)
		}
		return append(files, srcFile{relPath: filepath.ToSlash(filepath.Base(root)), absPath: root, size: info.Size()}),
			count + 1, info.Size(), nil
	}
	return walkDir(root, filepath.ToSlash(filepath.Base(root)), files, count)
}

func walkDir(root, relDir string, files []srcFile, count int) ([]srcFile, int, int64, error) {
	entries, err := os.ReadDir(root)
	if err != nil {
		return files, count, 0, fmt.Errorf("read dir %s: %w", root, err)
	}
	names := make([]string, 0, len(entries))
	byName := make(map[string]os.DirEntry, len(entries))
	for _, e := range entries {
		names = append(names, e.Name())
		byName[e.Name()] = e
	}
	sort.Strings(names) // deterministic tree order

	var total int64
	for _, name := range names {
		e := byName[name]
		full := filepath.Join(root, name)
		if e.IsDir() {
			var err error
			files, count, _, err = walkDir(full, relDir+"/"+name, files, count)
			if err != nil {
				return files, count, total, err
			}
			continue
		}
		if !e.Type().IsRegular() {
			continue // skip symlinks, sockets, fifos, devices
		}
		info, err := e.Info()
		if err != nil {
			return files, count, total, fmt.Errorf("stat %s: %w", full, err)
		}
		files = append(files, srcFile{relPath: relDir + "/" + name, absPath: full, size: info.Size()})
		count++
		total += info.Size()
	}
	return files, count, total, nil
}

func (m *Manifest) buildUnpacked(files []srcFile, chunkSize int64) error {
	m.PackedAsZip = false
	m.Files = make([]File, 0, len(files))
	for i, f := range files {
		path, err := SanitizePath(f.relPath)
		if err != nil {
			return fmt.Errorf("path %q: %w", f.relPath, err)
		}
		mf := File{
			Index:      i,
			Path:       path,
			Size:       f.size,
			SourcePath: f.absPath,
			ChunkCount: ChunkCountFor(f.size, chunkSize),
		}
		if err := mf.hashSource(mf.SourcePath, chunkSize); err != nil {
			return fmt.Errorf("hash %s: %w", f.absPath, err)
		}
		m.Files = append(m.Files, mf)
	}
	return nil
}

func (m *Manifest) buildPacked(files []srcFile, zipPath string, chunkSize int64) error {
	if err := WriteStoreZip(zipPath, files); err != nil {
		return fmt.Errorf("pack zip: %w", err)
	}
	info, err := os.Stat(zipPath)
	if err != nil {
		return fmt.Errorf("stat zip: %w", err)
	}
	path, err := SanitizePath(filepath.ToSlash(filepath.Base(zipPath)))
	if err != nil {
		return fmt.Errorf("zip name: %w", err)
	}
	m.PackedAsZip = true
	m.Files = []File{{
		Index:      0,
		Path:       path,
		Size:       info.Size(),
		SourcePath: zipPath,
		ChunkCount: ChunkCountFor(info.Size(), chunkSize),
	}}
	if err := m.Files[0].hashSource(zipPath, chunkSize); err != nil {
		return fmt.Errorf("hash zip: %w", err)
	}
	return nil
}

// BuildPackedVariant packs an already-built (unpacked) manifest into a
// store-mode ZIP at zipPath and returns the variant manifest: one file
// (the archive), same session id and chunk size. Source paths are taken
// from the built manifest (File.SourcePath), so no second walk happens.
// M1 per-receiver ZIP (PROTOCOL.md §3): the session keeps serving the
// tree to desktop receivers while phones get this variant.
func BuildPackedVariant(m *Manifest, zipPath string) (*Manifest, error) {
	if m == nil {
		return nil, errors.New("nil manifest")
	}
	if m.PackedAsZip {
		return m, nil // already packed: nothing to build
	}
	pairs := make([]srcFile, 0, len(m.Files))
	for _, f := range m.Files {
		if f.SourcePath == "" {
			return nil, fmt.Errorf("file %q has no source path", f.Path)
		}
		pairs = append(pairs, srcFile{relPath: f.Path, absPath: f.SourcePath, size: f.Size})
	}
	out := NewManifest(m.SessionID)
	out.ChunkSize = m.ChunkSize
	if err := out.buildPacked(pairs, zipPath, int64(m.ChunkSize)); err != nil {
		return nil, err
	}
	return out, nil
}

// hashSource streams absPath chunk by chunk and fills f.Hashes.
// Whole files are never loaded into RAM.
func (f *File) hashSource(absPath string, chunkSize int64) error {
	src, err := os.Open(absPath)
	if err != nil {
		return err
	}
	defer src.Close()

	f.Hashes = make([]string, 0, f.ChunkCount)
	buf := make([]byte, chunkSize)
	for range f.ChunkCount {
		n, rerr := io.ReadFull(src, buf)
		if rerr != nil && rerr != io.EOF && rerr != io.ErrUnexpectedEOF {
			return rerr
		}
		if n == 0 {
			if f.ChunkCount > 0 && len(f.Hashes) == 0 {
				return fmt.Errorf("unexpected empty read (size=%d, chunkSize=%d)", f.Size, chunkSize)
			}
			break // unexpected EOF while size promised more — caught below
		}
		sum := sha256.Sum256(buf[:n])
		f.Hashes = append(f.Hashes, hex.EncodeToString(sum[:]))
	}
	if len(f.Hashes) != f.ChunkCount {
		return fmt.Errorf("short file: hashed %d of %d chunks", len(f.Hashes), f.ChunkCount)
	}
	return nil
}

// JSON returns the manifest marshalled to indented JSON.
func (m *Manifest) JSON() ([]byte, error) { return json.MarshalIndent(m, "", "  ") }

// TotalSize returns the summed byte size of all files in the manifest.
func (m *Manifest) TotalSize() int64 {
	var t int64
	for _, f := range m.Files {
		t += f.Size
	}
	return t
}

// Find returns the manifest file with the given index.
func (m *Manifest) Find(index int) (File, bool) {
	if index < 0 || index >= len(m.Files) {
		return File{}, false
	}
	return m.Files[index], true
}

// FileCount returns how many files the manifest describes.
func (m *Manifest) FileCount() int { return len(m.Files) }

// ValidateManifest performs receiver-side sanity checks before a download
// pipeline trusts a manifest: version, per-file chunk counts vs sizes,
// hash shape and path safety (PROTOCOL.md §8 rule 7).
func ValidateManifest(m *Manifest) error {
	if m == nil {
		return errors.New("nil manifest")
	}
	if m.Version != ManifestVersion {
		return fmt.Errorf("unsupported manifest version %d", m.Version)
	}
	if m.ChunkSize <= 0 || m.ChunkSize > 64*1024*1024 {
		return fmt.Errorf("bad chunk size %d", m.ChunkSize)
	}
	if len(m.Files) == 0 {
		return errors.New("manifest has no files")
	}
	if len(m.Files) > MaxManifestFiles {
		return fmt.Errorf("manifest too large: %d files", len(m.Files))
	}
	for i, f := range m.Files {
		if f.Index != i {
			return fmt.Errorf("file entry %d has index %d", i, f.Index)
		}
		if want := ChunkCountFor(f.Size, int64(m.ChunkSize)); f.ChunkCount != want {
			return fmt.Errorf("file %q: chunkCount %d, expected %d", f.Path, f.ChunkCount, want)
		}
		if len(f.Hashes) != f.ChunkCount {
			return fmt.Errorf("file %q: %d hashes for %d chunks", f.Path, len(f.Hashes), f.ChunkCount)
		}
		if _, err := SanitizePath(f.Path); err != nil {
			return fmt.Errorf("file %q: %w", f.Path, err)
		}
		for _, h := range f.Hashes {
			if len(h) != sha256.Size*2 || strings.ToLower(h) != h {
				return fmt.Errorf("file %q: malformed chunk hash %q", f.Path, h)
			}
		}
	}
	return nil
}

// HumanBytes formats n bytes in KiB/MiB/GiB/TiB for UI display.
func HumanBytes(n int64) string {
	const k = 1024
	switch {
	case n >= k*k*k*k:
		return fmt.Sprintf("%.1f TiB", float64(n)/(k*k*k*k))
	case n >= k*k*k:
		return fmt.Sprintf("%.1f GiB", float64(n)/(k*k*k))
	case n >= k*k:
		return fmt.Sprintf("%.1f MiB", float64(n)/(k*k))
	case n >= k:
		return fmt.Sprintf("%.1f KiB", float64(n)/k)
	default:
		return fmt.Sprintf("%d B", n)
	}
}
