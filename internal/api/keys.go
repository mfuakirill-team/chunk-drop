package api

import (
	"crypto/subtle"
	"net/http"

	"chunkdrop/internal/config"
	"chunkdrop/internal/logx"
)

// Dashboard group-key management (M2): the owner sets the key through
// the dashboard, never by hand-editing engine.toml. POST /api/key
// stages a key; the receiver (cmd/engine) owns the 20 s change-of-mind
// debounce, the engine.toml write and the live mesh rekey.
//
// Auth: local-only always. When a key is already configured it is also
// the dashboard credential — replacing it requires presenting it (the
// dashboard sends the key it knows in X-ChunkDrop-Key). With no key
// configured there is nothing to present yet; local-only is the gate
// (this is exactly the first-run situation).

// handleKeySet stages a new group key. Body: {"key": "..."}.
func (s *Server) handleKeySet(w http.ResponseWriter, r *http.Request) {
	if s.OnKey == nil {
		http.Error(w, `{"error":"key setting unavailable"}`, http.StatusNotFound)
		return
	}
	if cur := s.GroupKey(); cur != "" {
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
	key := body.Key
	if !config.ValidKey(key) {
		http.Error(w, `{"error":"invalid group key (8+ printable chars, no spaces/quotes)"}`, http.StatusBadRequest)
		return
	}
	if key == s.GroupKey() {
		// Same key re-submitted: nothing to stage, nothing to rekey.
		writeJSON(w, http.StatusOK, map[string]any{"staged": false, "applyInSeconds": 0})
		return
	}
	// Dashboard auth swaps immediately — the dashboard has already
	// saved the new key and polls with it; only the mesh rebuild and
	// the engine.toml write wait for the receiver's debounce.
	s.SetGroupKey(key)
	s.OnKey(key)
	writeJSON(w, http.StatusOK, map[string]any{"staged": true, "applyInSeconds": keyApplySeconds})
}

// keyApplySeconds mirrors cmd/engine's debounce (advertised to the UI);
// defined here so the API answer stays honest. The engine is the
// authority — keep the two in sync.
const keyApplySeconds = 20
