// Package web exposes the built web app directory (web/dist) to Go.
package web

import (
	"io/fs"
	"os"
)

// BuildDir is the vite output directory relative to the repo root.
const BuildDir = "web/dist"

// BuildFS is web/dist as an fs.FS, or nil if it doesn't exist yet
// (fresh clone, `make web` not run).
var BuildFS fs.FS = openBuild()

func openBuild() fs.FS {
	if st, err := os.Stat(BuildDir); err == nil && st.IsDir() {
		return os.DirFS(BuildDir)
	}
	return nil
}
