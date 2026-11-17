package api

import (
	"crypto/subtle"
	"net/http"
	"strings"

	"chunkdrop/internal/config"
	"chunkdrop/internal/logx"
)

// Dashboard group-key management (M2): the owner sets the key through
// the dashboard, never by hand-editing engine.toml. POST /api/key
// applies a key immediately (mesh rekeys on the spot) and the receiver
// (cmd/engine) defers only the engine.toml write by keySaveSeconds.
//
// Auth: local-only always. When a real key is already configured it is
// also the dashboard credential — replacing it requires presenting it
// (the dashboard sends the key it knows in X-ChunkDrop-Key). A fresh
// engine.toml holds the placeholder, which is NOT a configured key:
// there is nothing to present yet, local-only is the gate (exactly the
// first-run situation).

// handleKeySet applies a new group key. Body: {"key": "..."}.
func (s *Server) handleKeySet(w http.ResponseWriter, r *http.Request) {
	if s.OnKey == nil {
		http.Error(w, `{"error":"key setting unavailable"}`, http.StatusNotFound)
		return
	}
	if cur := s.GroupKey(); config.UsableKey(cur) {
		if subtle.ConstantTimeCompare([]byte(r.Header.Get(HeaderKey)), []byte(cur)) != 1 {
			logx.Warn("auth", "401 on /api/key: wrong group key (from %s)", r.RemoteAddr)
			http.Error(w, ErrNoSession, http.StatusUnauthorized)
			return
		}
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
