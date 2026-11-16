package api

import (
	"net/http"
)

// Hub connection status for the dashboard (M2 step ①): whether the
// engine is online at the hub, and which remote devices are currently
// there. Group-key gated + local-only like the rest of the dashboard.
func (s *Server) handleHub(w http.ResponseWriter, r *http.Request) {
	if s.Hub == nil {
		// No hub configured: not an error — the internet mesh is
		// simply off (engine.toml: hub_url empty).
		writeJSON(w, http.StatusOK, map[string]any{"enabled": false})
		return
	}
	st := s.Hub.Status()
	writeJSON(w, http.StatusOK, map[string]any{
		"enabled":   true,
		"url":       st.URL,
		"connected": st.Connected,
		"lastError": st.LastError,
		"devices":   st.Devices,
	})
}
