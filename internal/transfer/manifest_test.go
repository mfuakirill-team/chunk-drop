package transfer

import (
	"encoding/json"
	"os"
	"path/filepath"
	"testing"
)

// Manifest + chunking tests (docs/M0-TASKS.md step 4):
// last-short-chunk, empty file, unicode names, 10k files.

func TestChunkCountFor(t *testing.T) {
	cases := []struct {
		n, cs int64
		want  int
	}{
		{0, 4 << 20, 0},
		{1, 4 << 20, 1},
		{4 << 20, 4 << 20, 1},
		{4<<20 + 1, 4 << 20, 2},
		{8 << 20, 4 << 20, 2},
	}
	for _, c := range cases {
		if got := ChunkCountFor(c.n, c.cs); got != c.want {
			t.Errorf("ChunkCountFor(%d, %d) = %d, want %d", c.n, c.cs, got, c.want)
		}
	}
}

func testTree(t *testing.T) string {
	t.Helper()
	dir := t.TempDir()
	files := map[string]string{
		"root.txt":            "hello chunkdrop", // < 1 chunk
		"photos/2026/cat.jpg": "A",               // last chunk short
		"photos/2026/dog.jpg": "B",
		"photos/2026/big.bin": "C",
		"docs/report.pdf":     "D",
		"näme ünïcode.txt":    "E", // NFC-sensitive name
		"empty.dat":           "",  // empty file → 0 chunks
		"last-short.dat":      "z", // exactly 1 byte → 1 short chunk
	}
	for rel, content := range files {
		abs := filepath.Join(dir, filepath.FromSlash(rel))
		if err := os.MkdirAll(filepath.Dir(abs), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(abs, []byte(content), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	return dir
}

func TestBuildFromPathsFolder(t *testing.T) {
	dir := testTree(t)
	m := NewManifest("sess-test")
	if err := m.BuildFromPaths([]string{dir}, BuildOptions{}); err != nil {
		t.Fatal(err)
	}
	if m.PackedAsZip {
		t.Error("PackedAsZip should be false")
	}
	if m.ChunkSize != DefaultChunkSize {
		t.Errorf("chunk size = %d, want default", m.ChunkSize)
	}
	// BuildFromPaths names paths relative to the picked folder, so every
	// manifest path starts with the temp dir's base name.
	root := filepath.ToSlash(filepath.Base(dir))
	var (
		gotEmpty     bool
		gotLast      bool
		gotUnicode   bool
		gotShortByte bool
		totalFiles   int
	)
	for _, f := range m.Files {
		totalFiles++
		if f.Size == 0 {
			gotEmpty = true
			if f.ChunkCount != 0 || len(f.Hashes) != 0 {
				t.Errorf("empty file %q: chunkCount=%d hashes=%d, want 0/0", f.Path, f.ChunkCount, len(f.Hashes))
			}
		}
		switch f.Path {
		case root + "/last-short.dat":
			gotLast = true
			if f.ChunkCount != 1 {
				t.Errorf("1-byte file chunkCount = %d, want 1", f.ChunkCount)
			}
		case root + "/root.txt":
			gotShortByte = true
			if f.ChunkCount != 1 {
				t.Errorf("small file chunkCount = %d, want 1", f.ChunkCount)
			}
		case root + "/näme ünïcode.txt":
			gotUnicode = true
		}
		for i, h := range f.Hashes {
			if len(h) != 64 {
				t.Errorf("file %q hash %d: len=%d, want 64", f.Path, i, len(h))
			}
		}
	}
	if !gotEmpty || !gotLast || !gotUnicode || !gotShortByte {
		t.Errorf("missing fixture coverage: empty=%v last=%v unicode=%v small=%v (total %d)",
			gotEmpty, gotLast, gotUnicode, gotShortByte, totalFiles)
	}
	if totalFiles != 8 {
		t.Errorf("total files = %d, want 8", totalFiles)
	}
	if err := ValidateManifest(m); err != nil {
		t.Errorf("ValidateManifest: %v", err)
	}
}

func TestBuildSingleFile(t *testing.T) {
	dir := testTree(t)
	abs := filepath.Join(dir, "photos", "2026", "cat.jpg")
	m := NewManifest("s")
	if err := m.BuildFromPaths([]string{abs}, BuildOptions{}); err != nil {
		t.Fatal(err)
	}
	if len(m.Files) != 1 || m.Files[0].Path != "cat.jpg" {
		t.Fatalf("manifest = %+v", m.Files)
	}
	if m.Files[0].ChunkCount != 1 {
		t.Errorf("chunkCount = %d", m.Files[0].ChunkCount)
	}
}

func TestBuildEmptySelectionFails(t *testing.T) {
	dir := t.TempDir()
	m := NewManifest("s")
	if err := m.BuildFromPaths([]string{dir}, BuildOptions{}); err == nil {
		t.Fatal("expected error for empty dir")
	}
}

func TestChunkReadLastShort(t *testing.T) {
	dir := t.TempDir()
	data := make([]byte, DefaultChunkSize+7)
	for i := range data {
		data[i] = byte(i)
	}
	if err := os.WriteFile(filepath.Join(dir, "big.bin"), data, 0o644); err != nil {
		t.Fatal(err)
	}
	m := NewManifest("s")
	if err := m.BuildFromPaths([]string{filepath.Join(dir, "big.bin")}, BuildOptions{}); err != nil {
		t.Fatal(err)
	}
	// last chunk must be exactly the 7 remainder bytes
	got, err := m.ChunkRead(0, 1)
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 7 {
		t.Fatalf("last chunk len = %d, want 7", len(got))
	}
	want := data[DefaultChunkSize:]
	for i, b := range want {
		if got[i] != b {
			t.Fatalf("byte %d: got %d want %d", i, got[i], b)
		}
	}
	// out of range
	if _, err := m.ChunkRead(0, 2); err != ErrChunkRange {
		t.Errorf("chunk 2 err = %v, want ErrChunkRange", err)
	}
	if _, err := m.ChunkRead(5, 0); err != ErrFileRange {
		t.Errorf("file 5 err = %v, want ErrFileRange", err)
	}
}

func TestChunkReadEmptyFile(t *testing.T) {
	dir := t.TempDir()
	p := filepath.Join(dir, "empty.dat")
	if err := os.WriteFile(p, nil, 0o644); err != nil {
		t.Fatal(err)
	}
	m := NewManifest("s")
	if err := m.BuildFromPaths([]string{p}, BuildOptions{}); err != nil {
		t.Fatal(err)
	}
	if m.Files[0].ChunkCount != 0 {
		t.Fatalf("chunkCount = %d, want 0", m.Files[0].ChunkCount)
	}
	if _, err := m.ChunkRead(0, 0); err == nil {
		t.Error("chunk 0 of empty file should error out of range")
	}
}

func TestUnicodeNamesNFC(t *testing.T) {
	dir := t.TempDir()
	// "é" written as NFD (e + combining acute) must come out NFC (single rune).
	nfd := "e\u0301.txt"
	if err := os.WriteFile(filepath.Join(dir, nfd), []byte("x"), 0o644); err != nil {
		t.Fatal(err)
	}
	root := filepath.ToSlash(filepath.Base(dir))
	m := NewManifest("s")
	if err := m.BuildFromPaths([]string{dir}, BuildOptions{}); err != nil {
		t.Fatal(err)
	}
	found := false
	for _, f := range m.Files {
		if f.Path == root+"/é.txt" {
			found = true
		}
	}
	if !found {
		t.Errorf("NFC-normalized path missing, got %v", m.Files)
	}
}

func TestTenThousandFiles(t *testing.T) {
	if testing.Short() {
		t.Skip("10k fixture is slow")
	}
	dir := t.TempDir()
	const n = 10000
	// one level of subfolders to keep directory reads fast
	for i := 0; i < n; i++ {
		sub := filepath.Join(dir, "d"+string(rune('a'+i%26)))
		if err := os.MkdirAll(sub, 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(filepath.Join(sub, "f"+string(rune('0'+i%10))+"-"+string(rune('a'+i%26))+itoa(i)+".txt"), []byte("x"), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	m := NewManifest("s")
	if err := m.BuildFromPaths([]string{dir}, BuildOptions{}); err != nil {
		t.Fatal(err)
	}
	if m.FileCount() != n {
		t.Errorf("files = %d, want %d", m.FileCount(), n)
	}
	if err := ValidateManifest(m); err != nil {
		t.Fatal(err)
	}
	b, err := m.JSON()
	if err != nil {
		t.Fatal(err)
	}
	var round Manifest
	if err := json.Unmarshal(b, &round); err != nil {
		t.Fatalf("roundtrip unmarshal: %v", err)
	}
}

func itoa(i int) string {
	if i == 0 {
		return ""
	}
	digits := ""
	for i > 0 {
		digits = string(rune('0'+i%10)) + digits
		i /= 10
	}
	return digits
}

func TestManifestJSONRoundtrip(t *testing.T) {
	dir := testTree(t)
	m := NewManifest("round")
	if err := m.BuildFromPaths([]string{dir}, BuildOptions{}); err != nil {
		t.Fatal(err)
	}
	b, err := m.JSON()
	if err != nil {
		t.Fatal(err)
	}
	var back Manifest
	if err := json.Unmarshal(b, &back); err != nil {
		t.Fatal(err)
	}
	if back.SessionID != "round" || back.Version != ManifestVersion || back.ChunkSize != m.ChunkSize {
		t.Errorf("roundtrip mismatch: %+v", back)
	}
	if len(back.Files) != len(m.Files) {
		t.Errorf("file count mismatch after roundtrip")
	}
	for i := range back.Files {
		if back.Files[i].Path != m.Files[i].Path {
			t.Errorf("file %d path %q != %q", i, back.Files[i].Path, m.Files[i].Path)
		}
	}
	// SourcePath must never be serialized
	if s := string(b); contains(s, `"SourcePath"`) || contains(s, filepath.ToSlash(dir)) {
		t.Error("SourcePath or absolute path leaked into JSON")
	}
}

func contains(s, sub string) bool {
	return len(s) >= len(sub) && (s == sub || len(sub) == 0 || indexOf(s, sub) >= 0)
}

func indexOf(s, sub string) int {
	for i := 0; i+len(sub) <= len(s); i++ {
		if s[i:i+len(sub)] == sub {
			return i
		}
	}
	return -1
}

func TestBuildRejectsAboveManifestCapWithoutZip(t *testing.T) {
	// (fast cousin of TestTenThousandFiles via BuildOptions.ChunkSize and cap)
	// We can't easily fake 10001 files cheaply here; instead assert the cap
	// exists and is enforced by constructing a manifest directly.
	m := NewManifest("s")
	for i := 0; i < MaxManifestFiles+1; i++ {
		m.Files = append(m.Files, File{Index: i, Path: "f.txt", Size: 1, ChunkCount: 1, Hashes: []string{"00"}})
	}
	if err := ValidateManifest(m); err == nil {
		t.Error("ValidateManifest should reject > MaxManifestFiles entries")
	}
}

func TestHumanBytes(t *testing.T) {
	cases := []struct {
		n    int64
		want string
	}{
		{0, "0 B"},
		{1023, "1023 B"},
		{1024, "1.0 KiB"},
		{4 << 20, "4.0 MiB"},
		{1 << 30, "1.0 GiB"},
	}
	for _, c := range cases {
		if got := HumanBytes(c.n); got != c.want {
			t.Errorf("HumanBytes(%d) = %q, want %q", c.n, got, c.want)
		}
	}
}
