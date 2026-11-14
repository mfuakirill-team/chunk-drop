// Package webdist embeds the built web app (web/dist) so both the
// engine and the hub serve the same UI (docs/ARCHITECTURE.md §1).
//
// The embed pattern uses a child directory: `//go:embed all:dist` with
// the dist/ directory copied/symlinked here by `make web` (Go's embed
// cannot reference paths outside the module subtree of this package).
package webdist

import (
	"embed"
	"io/fs"

	"chunkdrop/web"
)

//go:embed all:dist
var embedded embed.FS

// FS returns the embedded build output of the web app.
//
// `make web` syncs web/dist → internal/webdist/dist before `go build`;
// if that step was skipped, FS falls back to an empty tree and the
// engine logs that the UI is missing.
var FS fs.FS = embedded

func init() {
	// Prefer the live web/dist when running from the source tree via
	// `go run` (dev), so UI changes show up without a re-embed.
	if _, err := fs.Stat(web.BuildFS, "."); err == nil {
		FS = web.BuildFS
	}
}
