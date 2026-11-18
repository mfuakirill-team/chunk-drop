package api

import (
	"net/http"
	"strings"
)

// Per-peer transport controls (M2 step ④): the dashboard shows which
// route each mesh peer actually used and lets the owner steer it.
//
// RouteControl is the engine-side state behind that: the transport an
// internet peer last used (WebRTC tunnel or hub relay — recording is
// the dialer's job) and the owner's per-peer mode. Satisfied by the
// engine's route state (cmd/engine); stubbed in tests. Nil server
// field = controls unavailable (404 on the POST, empty fields on
// /api/peers) — a mesh-less engine has nothing to steer.
//
// Modes:
//   - "auto"    — default routing: LAN direct for LAN peers; tunnel
//     first, hub-relay fallback for internet peers.
//   - "tunnel"  — force the WebRTC tunnel (no relay fallback).
//   - "relay"   — force the hub relay (diagnosis: "does the hub path
//     work" without touching the NAT question).
//   - "off"     — pause sync with this peer, two-sided: the peer is
//     dropped from this engine's mesh peer list (no pulls, no
//     announcements to it) and, wherever its identity is knowable
//     (tunnel/relay RemoteAddr, LAN via discovery), it is refused
//     when it pulls from this engine.
//
// State is RAM-only by design: toggles describe this run's debugging
// and pausing; a restart (or a group-key rekey, which rebuilds the
// mesh) returns every peer to auto.
type RouteControl interface {
	// Routes snapshots deviceID → last route used ("tunnel"/"relay").
	// Absent = not dialed yet; LAN peers are always "lan" and are not
	// in the map.
	Routes() map[string]string
	// Modes snapshots deviceID → mode ("auto" is reported even when
	// unset, so the UI can bind a select to it).
	Modes() map[string]string
	// SetMode applies a per-peer mode; the error names the problem
	// (unknown mode, empty id).
	SetMode(id, mode string) error
}

// peerModes is the accepted SetMode vocabulary.
var peerModes = map[string]bool{
	"auto":   true,
	"tunnel": true,
	"relay":  true,
	"off":    true,
}

// routesSnapshot returns the current route control (nil-safe).
func (s *Server) routesSnapshot() RouteControl {
	s.mu.RLock()
	defer s.mu.RUnlock()
	return s.routes
}

// handlePeerMode applies a per-peer mode (step ④): body
// {"id":"<deviceID>","mode":"auto|tunnel|relay|off"} → {"ok":true}.
// Local-only + group-key gated like every dashboard control: the
// dashboard drives this PC's mesh.
func (s *Server) handlePeerMode(w http.ResponseWriter, r *http.Request) {
	rc := s.routesSnapshot()
	if rc == nil {
		http.Error(w, `{"error":"route controls unavailable"}`, http.StatusNotFound)
		return
	}
	var body struct {
		ID   string `json:"id"`
		Mode string `json:"mode"`
	}
	if err := decodeBody(r, w, &body, 1<<20); err != nil {
		return
	}
	body.ID = strings.TrimSpace(body.ID)
	body.Mode = strings.TrimSpace(body.Mode)
	if body.ID == "" {
		http.Error(w, `{"error":"missing peer id"}`, http.StatusBadRequest)
		return
	}
	if !peerModes[body.Mode] {
		http.Error(w, `{"error":"unknown mode (auto|tunnel|relay|off)"}`, http.StatusBadRequest)
		return
	}
	if err := rc.SetMode(body.ID, body.Mode); err != nil {
		http.Error(w, `{"error":"mode change refused"}`, http.StatusInternalServerError)
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"ok": true, "id": body.ID, "mode": body.Mode})
}
