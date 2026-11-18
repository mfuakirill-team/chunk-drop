package api

import (
	"net/http"
	"strings"

	"chunkdrop/internal/config"
	"chunkdrop/internal/logx"
)

// Global mesh mode (M2 step ④ rework): ONE switch that says how THIS
// engine interacts with all other peers — not per-peer settings.
//
//   - "auto"     — everything: LAN peers direct, internet peers via
//     the hub (tunnel first, relay fallback).
//   - "local"    — the local network only: the hub connection is
//     dropped entirely (no presence, no hub peers).
//   - "internet" — the global internet only: LAN-discovered peers are
//     skipped; peers join via hub_url/stun_url.
//
// The mode is engine state (engine.toml mesh_mode, default auto): the
// dashboard reads it (GET /api/mode) and switches it (POST /api/mode →
// OnMode, which rebuilds the mesh and saves the toml).

// handleModeGet returns the current mode:
// {"mode":"auto","modes":["auto","local","internet"]}.
func (s *Server) handleModeGet(w http.ResponseWriter, r *http.Request) {
	mode := s.MeshMode()
	if mode == "" {
		mode = config.MeshModeAuto
	}
	writeJSON(w, http.StatusOK, map[string]any{
		"mode":  mode,
		"modes": []string{"auto", "local", "internet"},
	})
}

// handleModeSet applies a new global mode: body
// {"mode":"auto|local|internet"} → {"ok":true,"mode":…}. OnMode (the
// engine) rebuilds the mesh and persists engine.toml; an error refuses
// the switch. Local-only + group-key gated like every dashboard
// control: the dashboard drives this PC's mesh.
func (s *Server) handleModeSet(w http.ResponseWriter, r *http.Request) {
	if s.OnMode == nil {
		http.Error(w, `{"error":"mode changes unavailable"}`, http.StatusNotImplemented)
		return
	}
	var body struct {
		Mode string `json:"mode"`
	}
	if err := decodeBody(r, w, &body, 1<<20); err != nil {
		return
	}
	body.Mode = strings.TrimSpace(body.Mode)
	if !config.ValidMeshMode(body.Mode) {
		http.Error(w, `{"error":"unknown mode (auto|local|internet)"}`, http.StatusBadRequest)
		return
	}
	if err := s.OnMode(body.Mode); err != nil {
		logx.Error("mesh", "mode switch to %s refused: %v", body.Mode, err)
		http.Error(w, `{"error":"mode change failed"}`, http.StatusInternalServerError)
		return
	}
	s.SetMeshMode(body.Mode)
	writeJSON(w, http.StatusOK, map[string]any{"ok": true, "mode": body.Mode})
}
