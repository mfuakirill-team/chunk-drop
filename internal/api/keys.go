package api

import (
	"net/http"
	"net/url"
	"strings"

	"chunkdrop/internal/config"
	"chunkdrop/internal/logx"
)

// Dashboard group-key management (M2): the owner sets the key through
// the dashboard, never by hand-editing engine.toml. POST /api/key
// applies a key immediately (mesh rekeys on the spot) and the receiver
// (cmd/engine) defers only the engine.toml write by keySaveSeconds.
//
// Auth: local-only (the server's localOnly wrapper) plus a same-page
// stamp — HeaderUI must be present, and an Origin header, when the
// browser sent one, must name this engine's own host:port. That is
// what stops a drive-by webpage on this device from rekeying the
// engine: it can reach the API (its requests look local) but cannot
// add the custom header (CORS preflight) and its Origin names its own
// site. The current key is deliberately NOT the credential here: the
// owner at the dashboard replaces it freely, and a browser that lost
// its saved copy (hand-edited toml, wiped PWA storage) just types the
// key again. Typing the key that is already set is the harmless no-op
// path below — it reveals nothing a wrong key wouldn't (local-only
// surface).
//
// The group key keeps every other role: it authenticates mesh traffic
// between engines, stays the receiver-link secret and is validated
// (config.ValidKey) before anything is applied.

// dashboardStampOK verifies the request plausibly comes from the
// dashboard's own script: the UI stamp header plus, when present, a
// matching Origin (same host:port as the request — scheme-insensitive,
// so an eventual TLS front-end keeps working). No Origin = non-browser
// client (curl, scripts) — the local-only wrapper already gates those.
func dashboardStampOK(r *http.Request) bool {
	if r.Header.Get(HeaderUI) == "" {
		return false
	}
	origin := r.Header.Get("Origin")
	if origin == "" {
		return true
	}
	u, err := url.Parse(origin)
	if err != nil || u.Host == "" {
		return false
	}
	return strings.EqualFold(u.Host, r.Host)
}

// handleKeySet applies a new group key. Body: {"key": "..."}.
func (s *Server) handleKeySet(w http.ResponseWriter, r *http.Request) {
	if s.OnKey == nil {
		http.Error(w, `{"error":"key setting unavailable"}`, http.StatusNotFound)
		return
	}
	if !dashboardStampOK(r) {
		logx.Warn("auth", "401 on /api/key: not the local dashboard (from %s, origin %q)",
			r.RemoteAddr, r.Header.Get("Origin"))
		http.Error(w, ErrNoSession, http.StatusUnauthorized)
		return
	}
	var body struct {
		Key string `json:"key"`
	}
	if err := decodeBody(r, w, &body, 1<<20); err != nil {
		return
	}
	key := strings.TrimSpace(body.Key)
	if !config.ValidKey(key) {
		http.Error(w, `{"error":"invalid group key (1–256 printable chars, no quotes/backslash/control chars)"}`, http.StatusBadRequest)
		return
	}
	if key == s.GroupKey() {
		// Same key re-submitted: nothing to apply, nothing to rekey.
		writeJSON(w, http.StatusOK, map[string]any{"applied": false, "saveInSeconds": 0})
		return
	}
	// Dashboard auth swaps immediately — the dashboard has already saved
	// the new key and polls with it; the mesh rekeys now (OnKey). Only
	// the engine.toml write waits for the receiver's save window.
	s.SetGroupKey(key)
	s.OnKey(key)
	writeJSON(w, http.StatusOK, map[string]any{"applied": true, "saveInSeconds": keySaveSeconds})
}

// keySaveSeconds mirrors cmd/engine's save delay (advertised to the
// UI); defined here so the API answer stays honest. The engine is the
// authority — keep the two in sync.
const keySaveSeconds = 20
