package transfer

import (
	"archive/zip"
	"io"
	"os"
)

// Store-mode ZIP packer (docs/PROTOCOL.md §3, PLAN.md D7):
// structure is preserved inside one archive, entries are stored with
// method Store (no compression) — ~zero CPU on the engine side and it
// turns N phone downloads into 1.

// WriteStoreZip packs files (srcFile entries: manifest-relative path +
// absolute source path) into a store-mode ZIP at dstPath.
// Directory structure is preserved; no compression is applied.
func WriteStoreZip(dstPath string, files []srcFile) error {
	zf, err := os.Create(dstPath)
	if err != nil {
		return err
	}
	defer zf.Close()

	zw := zip.NewWriter(zf)
	defer zw.Close() // error surfaced via zw.Close ordering below

	for _, f := range files {
		// Normalize each stored name (PROTOCOL.md §8 rule 2/7) —
		// names come from the sender's own filesystem here, but the
		// belt-and-suspenders rule applies on every path we emit, too.
		name, err := SanitizePath(f.relPath)
		if err != nil {
			return err
		}
		src, err := os.Open(f.absPath)
		if err != nil {
			return err
		}
		hdr := &zip.FileHeader{
			Name:   name,
			Method: zip.Store, // THE point: no compression
		}
		w, err := zw.CreateHeader(hdr)
		if err != nil {
			src.Close()
			return err
		}
		if _, err := io.Copy(w, src); err != nil {
			src.Close()
			return err
		}
		src.Close()
	}
	return zw.Close()
}
