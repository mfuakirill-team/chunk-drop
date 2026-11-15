// Package api wires the engine's HTTP handlers: receiver endpoints
// (PROTOCOL.md §4), the sender dashboard's start endpoint, native pick
// dialogs (#1), LAN peer discovery view (#7) and the syncmesh
// endpoints (#6). All /api/sync* and /api/pick routes are gated by
// the group key — engines are group members, receivers are not.
package api

import (
	"net/http"
	"time"

	"chunkdrop/internal/activity"
	"chunkdrop/internal/discovery"
	"chunkdrop/internal/logx"
	"chunkdrop/internal/session"
	"chunkdrop/internal/syncmesh"
	"chunkdrop/internal/upload"
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

	// GroupKey (engine config) gates session creation and all
	// engine-to-engine routes (pick, sync, discovery view).
	GroupKey string

	// UI serves the embedded web app (nil = no embedded UI yet).
	UI http.Handler

	// Picker opens native file dialogs on the engine (#1). Injected,
	// nil disables the pick endpoints (paste-paths still works).
	Picker Picker

	// Discovery lists LAN peers for the dashboard panel (#7).
	Discovery PeersSource

	// Sync is this engine's shared folder (auto-transfer, #6).
	Sync *syncmesh.Local

	// Uploads is the phone→PC push state (M1, PROTOCOL.md §4 upload
	// table); nil disables the upload routes. UploadDest is where
	// finished uploads land (the sync folder by default), and
	// MaxUploadBytes caps one upload (engine.toml, 100 GiB default).
	Uploads        *upload.Manager
	UploadDest     string
	MaxUploadBytes int64

	// SyncStart registers a sync serving session (#6 pull side).
	SyncStart func(*syncmesh.Session) error

	// Activity is the concise feed for the dashboard status panel
	// (#1.3): uploads (serving), downloads (sync), deletes, errors.
	// Nil = events are dropped (callers need no guards).
	Activity *activity.Log

	// AllowRemoteDashboard disables the local-only guard on the
	// dashboard/control routes (engine.toml: allow_remote_dashboard).
	// Default false: only this machine may control the PC; receiving
	// links from other devices keep working either way.
	AllowRemoteDashboard bool
}

// Picker abstracts the native dialog surface (internal/picker).
type Picker interface {
	PickFiles() ([]string, error)
	PickFolder() ([]string, error)
}

// PeersSource is the read side of discovery the server needs (#7).
// Satisfied by *discovery.Service; stubbed in tests.
type PeersSource interface {
	Self() discovery.Beacon
	Table() *discovery.Table
}

// NewServer builds the handler tree.
func NewServer(sm *session.Manager, ui http.Handler) *Server {
	return &Server{Sessions: sm, UI: ui}
}

// Handler returns the fully wired http.Handler.
func (s *Server) Handler() http.Handler {
	mux := http.NewServeMux()

	// The web app (embedded web/dist): sender dashboard at /,
	// receive page at /r/ (one page, two modes — app.tsx). The
	// dashboard entry is local-only (below); the receive page is the
	// remote surface and stays public (the session key lives in the
	// URL fragment).
	//
	// Static files are public: the receive page's HTML references them
	// by absolute path (/assets/…, /icons/…, /manifest.webmanifest), and
	// it must render on other devices. The bundle holds no secrets, and
	// /r/assets/… could already reach it through the unguarded /r/ mount.
	// Only the dashboard *entry* and the control APIs are local-only.
	mux.Handle("/assets/", s.uiHandler(false))
	mux.Handle("/icons/", s.uiHandler(false))
	mux.Handle("/manifest.webmanifest", s.uiHandler(false))
	mux.Handle("/", s.localOnly(s.uiHandler(false)))
	mux.Handle("/r/", s.uiHandler(true))
	// /u/ is the phone→PC push page (M1): public like /r/ — the upload
	// key lives in the fragment; no dashboard-back affordance there.
	mux.Handle("/u/", s.uiStrip("/u/"))

	// Authenticated receiver endpoints (PROTOCOL.md §4) — must work
	// from other devices: they carry the per-transfer session key.
	mux.HandleFunc("GET /api/manifest", s.auth(s.handleManifest))
	mux.HandleFunc("GET /api/chunk/{f}/{c}", s.auth(s.handleChunk))
	mux.HandleFunc("POST /api/progress", s.auth(s.handleProgress))
	mux.HandleFunc("POST /api/cancel", s.auth(s.handleCancel))
	mux.HandleFunc("GET /api/session", s.auth(s.handleSession))

	// Sender-side control (create a session from picked paths) —
	// local-only + group key: the dashboard controls this PC.
	mux.Handle("POST /api/start", s.localOnly(s.authStart(s.handleStart)))

	// Native pick dialogs on the engine machine (#1) — local-only +
	// group key: no remote dialog popping on our desktop.
	mux.Handle("POST /api/pick/files", s.localOnly(s.authStart(s.handlePickFiles)))
	mux.Handle("POST /api/pick/folder", s.localOnly(s.authStart(s.handlePickFolder)))

	// LAN peers panel (#7) + activity feed (#1.3) — local-only: they
	// name local files and peers. Sync endpoints (#6) stay reachable by
	// peer engines (that is the mesh; group key is the gate).
	mux.Handle("GET /api/peers", s.localOnly(s.authStart(s.handlePeers)))
	mux.Handle("GET /api/activity", s.localOnly(s.authStart(s.handleActivity)))
	mux.HandleFunc("GET /api/sync/state", s.authStart(s.handleSyncState))
	mux.HandleFunc("POST /api/sync/start", s.authStart(s.handleSyncStart))

	// Phone→PC push (M1, PROTOCOL.md §4 upload table). Slot creation
	// is dashboard control (local-only + group key); the upload itself
	// is gated by the upload key the dashboard handed out, exactly like
	// a receive session.
	mux.Handle("POST /api/upload/new", s.localOnly(s.authStart(s.handleUploadNew)))
	mux.Handle("POST /api/upload/close", s.localOnly(s.authStart(s.handleUploadClose)))
	mux.HandleFunc("POST /api/upload/start", s.authUpload(s.handleUploadStart))
	mux.HandleFunc("PUT /api/upload/{uploadId}/chunk/{f}/{c}", s.authUpload(s.handleUploadChunk))
	mux.HandleFunc("POST /api/upload/{uploadId}/finish", s.authUpload(s.handleUploadFinish))

	return requestLog(mux)
}

// statusWriter records the response code so the debug middleware can
// log method/path/status/duration per request.
type statusWriter struct {
	http.ResponseWriter
	code int
}

func (w *statusWriter) WriteHeader(code int) {
	w.code = code
	w.ResponseWriter.WriteHeader(code)
}

func (w *statusWriter) Write(b []byte) (int, error) {
	if w.code == 0 {
		w.code = http.StatusOK
	}
	return w.ResponseWriter.Write(b)
}

// requestLog logs every request at debug level — method, path, status,
// duration and source. With -debug this is the "each step" record of
// what the engine actually served; at info and above it stays silent.
func requestLog(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		sw := &statusWriter{ResponseWriter: w}
		start := time.Now()
		defer func() {
			if rec := recover(); rec != nil {
				logx.Error("http", "panic serving %s %s: %v", r.Method, r.URL.Path, rec)
				if sw.code == 0 {
					http.Error(sw, `{"error":"internal error"}`, http.StatusInternalServerError)
				}
			}
			logx.Debug("http", "%s %s -> %d (%s) from %s",
				r.Method, r.URL.Path, sw.code, time.Since(start).Round(time.Millisecond), r.RemoteAddr)
		}()
		next.ServeHTTP(sw, r)
		if sw.code >= 500 {
			logx.Error("http", "%s %s -> %d", r.Method, r.URL.Path, sw.code)
		}
	})
}

// uiHandler serves the embedded web app. strip selects the /r/ mount
// (receive links), where the prefix is removed so index.html resolves.
// The page itself is public — the session key lives in the URL
// *fragment* and only ever appears on API calls as a header
// (PROTOCOL.md §2).
func (s *Server) uiHandler(strip bool) http.Handler {
	if s.UI == nil {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			http.Error(w, "web app not embedded (build web/dist first)", http.StatusNotFound)
		})
	}
	if strip {
		return http.StripPrefix("/r/", s.UI)
	}
	return s.UI
}

// uiStrip serves the app under a subpath mount (e.g. /u/) with the
// prefix removed so index.html resolves. The mount itself is public —
// its key lives in the URL fragment.
func (s *Server) uiStrip(prefix string) http.Handler {
	if s.UI == nil {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			http.Error(w, "web app not embedded (build web/dist first)", http.StatusNotFound)
		})
	}
	return http.StripPrefix(prefix, s.UI)
}
