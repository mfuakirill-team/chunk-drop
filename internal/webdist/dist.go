// Package webdist embeds the built web app (web/dist) so both the
// engine and the hub serve the same UI (docs/ARCHITECTURE.md §1).
//
// The embed pattern uses a child directory: `//go:embed all:dist` with
// the dist/ directory copied/symlinked here by `make web` (Go's embed
// cannot reference paths outside the module subtree of this package).
//
// The engine serves the embedded assets unconditionally: the binary is
// portable and must behave identically from any working directory.
// (A previous build preferred a live web/dist relative to the CWD for
// `go run` dev builds; that CWD sniffing crashed or silently changed
// what was served when launched from elsewhere, so it was removed.
// `make web` syncs web/dist → internal/webdist/dist before the build,
// which is the sanctioned way to get fresh UI into the binary.)
package webdist

import (
	"embed"
	"io/fs"
)

//go:embed all:dist
var embedded embed.FS

// FS returns the embedded build output of the web app, rooted at the
// dist/ contents (index.html at the FS root). `all:dist` keeps the
// directory name as a prefix, so unwrap it.
var FS fs.FS = mustSub(embedded, "dist")

func mustSub(fsys fs.FS, dir string) fs.FS {
	sub, err := fs.Sub(fsys, dir)
	if err != nil {
		panic("webdist: embedded dist broken: " + err.Error())
	}
	return sub
}
