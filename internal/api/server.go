package api

import (
	"net/http"

	"chunkdrop/internal/session"
)

// Wire constants (docs/PROTOCOL.md §1, §4).
const (
	// HeaderKey carries the session key (or group key for /api/start).
	HeaderKey = "X-ChunkDrop-Key"
	// HeaderSession optionally carries the session id (dashboard calls).
	HeaderSession = "X-ChunkDrop-Session"
	// ChunkHashHeader is the convenience hash cross-check on chunk GETs.
	ChunkHashHeader = "X-Chunk-Hash"
)

// Generic error bodies — identical for every failure mode, so
// responses never leak which ids/files exist (SECURITY.md §2).
const (
	ErrNoSession = `{"error":"unauthorized"}`
	ErrGone      = `{"error":"session expired"}`
	generic404   = `{"error":"not found"}`
)

// Server holds the engine's HTTP handlers.
type Server struct {
	Sessions *session.Manager

	// GroupKey (engine config) gates session creation (sender side).
	GroupKey string

	// UI serves the embedded web app (nil = no embedded UI yet).
	UI http.Handler
}

// NewServer builds the handler tree.
func NewServer(sm *session.Manager, ui http.Handler) *Server {
	return &Server{Sessions: sm, UI: ui}
}

// Handler returns the fully wired http.Handler.
func (s *Server) Handler() http.Handler {
	mux := http.NewServeMux()

	// The receive web app (embedded web/dist).
	mux.Handle("/r/", s.uiHandler())

	// Authenticated receiver endpoints (PROTOCOL.md §4).
	mux.HandleFunc("GET /api/manifest", s.auth(s.handleManifest))
	mux.HandleFunc("GET /api/chunk/{f}/{c}", s.auth(s.handleChunk))
	mux.HandleFunc("POST /api/progress", s.auth(s.handleProgress))
	mux.HandleFunc("POST /api/cancel", s.auth(s.handleCancel))
	mux.HandleFunc("GET /api/session", s.auth(s.handleSession))

	// Sender-side (same-origin dashboard): create a session from
	// picked paths, gated by the group key.
	mux.HandleFunc("POST /api/start", s.authStart(s.handleStart))

	return mux
}

// uiHandler serves the embedded web app under /r/. The page itself is
// public — the session key lives in the URL *fragment* and only ever
// appears on API calls as a header (PROTOCOL.md §2).
func (s *Server) uiHandler() http.Handler {
	if s.UI == nil {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			http.Error(w, "web app not embedded (build web/dist first)", http.StatusNotFound)
		})
	}
	return http.StripPrefix("/r/", s.UI)
}
