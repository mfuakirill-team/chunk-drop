package api

import (
	"net/http"
)

// Hub connection status for the dashboard (M2 step ①): whether the
// engine is online at the hub, and which remote devices are currently
// there. Group-key gated + local-only like the rest of the dashboard.
func (s *Server) handleHub(w http.ResponseWriter, r *http.Request) {
	c := s.hubSnapshot()
	if c == nil {
		// No hub configured: not an error — the internet mesh is
		// simply off (engine.toml: hub_url empty).
		writeJSON(w, http.StatusOK, map[string]any{"enabled": false})
		return
	}
	st := c.Status()
	writeJSON(w, http.StatusOK, map[string]any{
		"enabled":   true,
		"url":       st.URL,
		"connected": st.Connected,
		"lastError": st.LastError,
		"devices":   st.Devices,
		// Step ③ diagnosis knob, surfaced so the hub card can say
		// "relay forced" when transfers are slow on purpose.
		"relayOnly": s.RelayOnly,
	})
}
