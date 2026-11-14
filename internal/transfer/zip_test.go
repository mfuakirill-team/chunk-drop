package transfer

import (
	"archive/zip"
	"io"
	"os"
	"path/filepath"
	"testing"
)

func TestWriteStoreZipPreservesStructureNoCompression(t *testing.T) {
	dir := testTree(t)
	var files []srcFile
	err := filepath.WalkDir(dir, func(p string, d os.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if d.IsDir() {
			return nil
		}
		rel, err := filepath.Rel(dir, p)
		if err != nil {
			return err
		}
		info, err := d.Info()
		if err != nil {
			return err
		}
		files = append(files, srcFile{relPath: filepath.ToSlash(rel), absPath: p, size: info.Size()})
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	zipPath := filepath.Join(t.TempDir(), "bundle.zip")
	if err := WriteStoreZip(zipPath, files); err != nil {
		t.Fatal(err)
	}

	zr, err := zip.OpenReader(zipPath)
	if err != nil {
		t.Fatal(err)
	}
	defer zr.Close()
	if len(zr.File) != len(files) {
		t.Fatalf("zip entries = %d, want %d", len(zr.File), len(files))
	}
	seen := map[string]bool{}
	for _, f := range zr.File {
		seen[f.Name] = true
		if f.Method != zip.Store {
			t.Errorf("entry %q method = %d, want Store(0)", f.Name, f.Method)
		}
		rc, err := f.Open()
		if err != nil {
			t.Fatal(err)
		}
		_, _ = io.Copy(io.Discard, rc)
		rc.Close()
	}
	for _, f := range files {
		if !seen[f.relPath] {
			t.Errorf("missing zip entry %q", f.relPath)
		}
	}
}

func TestBuildPackedAsZip(t *testing.T) {
	dir := testTree(t)
	zipPath := filepath.Join(t.TempDir(), "bundle.zip")
	m := NewManifest("zip")
	if err := m.BuildFromPaths([]string{dir}, BuildOptions{ZipPath: zipPath}); err != nil {
		t.Fatal(err)
	}
	if !m.PackedAsZip {
		t.Error("PackedAsZip not set")
	}
	if m.FileCount() != 1 {
		t.Fatalf("packed manifest files = %d, want 1", m.FileCount())
	}
	if m.Files[0].Path != "bundle.zip" {
		t.Errorf("packed path = %q", m.Files[0].Path)
	}
	if m.Files[0].ChunkCount < 1 {
		t.Errorf("packed chunkCount = %d", m.Files[0].ChunkCount)
	}
	if err := ValidateManifest(m); err != nil {
		t.Fatal(err)
	}
	// Chunks of the zip must be readable.
	got, err := m.ChunkRead(0, 0)
	if err != nil {
		t.Fatal(err)
	}
	if len(got) > DefaultChunkSize {
		t.Errorf("chunk 0 len = %d, exceeds chunk size", len(got))
	}
}
