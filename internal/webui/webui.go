// Package webui embeds the built web app (web/dist) into the engine
// and hub binaries (docs/ARCHITECTURE.md §1: one codebase, two servers).
package webui

import (
	"io/fs"
	"net/http"

	"chunkdrop/internal/webdist"
)

// FS returns the embedded web app files.
func FS() fs.FS { return webdist.FS }

// Handler serves the embedded web app (index.html fallback included by
// http.FileServerFS, correct content types from the embed FS names).
func Handler() http.Handler {
	sub, err := fs.Sub(webdist.FS, ".")
	if err != nil {
		panic("webui: embedded dist broken: " + err.Error())
	}
	return http.FileServerFS(sub)
}
