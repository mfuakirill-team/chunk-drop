package api

import (
	"net"
	"net/http"

	"chunkdrop/internal/syncmesh"
	"chunkdrop/internal/transfer"
)

// Engine-side extras: native pick dialogs (#1), LAN peers view (#7),
// syncmesh serving endpoints (#6). All group-key gated (authStart) —
// they act on the engine's desktop/filesystem, receivers never see
// them (the dashboard is same-origin and sends the group key).

// handlePickFiles opens a native multi-file dialog on the engine.
func (s *Server) handlePickFiles(w http.ResponseWriter, r *http.Request) {
	if s.Picker == nil {
		http.Error(w, `{"error":"picker unavailable"}`, http.StatusNotFound)
		return
	}
	paths, err := s.Picker.PickFiles()
	if err != nil {
		http.Error(w, `{"error":"picker failed"}`, http.StatusInternalServerError)
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"paths": paths})
}

// handlePickFolder opens a native folder dialog on the engine.
func (s *Server) handlePickFolder(w http.ResponseWriter, r *http.Request) {
	if s.Picker == nil {
		http.Error(w, `{"error":"picker unavailable"}`, http.StatusNotFound)
		return
	}
	paths, err := s.Picker.PickFolder()
	if err != nil {
		http.Error(w, `{"error":"picker failed"}`, http.StatusInternalServerError)
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"paths": paths})
}

// handlePeers lists discovered LAN engines (#7 dashboard panel).
func (s *Server) handlePeers(w http.ResponseWriter, r *http.Request) {
	self := map[string]any{"id": "", "name": "", "ip": "", "port": 0, "self": true}
	var peers []map[string]any
	if s.Discovery != nil {
		b := s.Discovery.Self()
		self = map[string]any{
			"id": b.ID, "name": b.Name, "ip": localIP(), "port": b.Port, "self": true,
		}
		for _, m := range s.Discovery.Table().List() {
			peers = append(peers, map[string]any{
				"id":   m.ID,
				"name": m.Name,
				"ip":   m.IP,
				"port": m.Port,
			})
		}
	}
	if peers == nil {
		peers = []map[string]any{}
	}
	writeJSON(w, http.StatusOK, map[string]any{"self": self, "peers": peers})
}

// handleSyncState returns this engine's shared-folder announcement
// (#6): what files we have, with size/mtime/xxhash.
func (s *Server) handleSyncState(w http.ResponseWriter, r *http.Request) {
	if s.Sync == nil {
		http.Error(w, `{"error":"sync unavailable"}`, http.StatusNotFound)
		return
	}
	_ = s.Sync.Scan() // fresh state per call; scan is cache-accelerated
	writeJSON(w, http.StatusOK, s.Sync.State())
}

// handleSyncStart serves a file from the shared folder to a pulling
// peer: body {"rels": ["a/b.jpg"]} → session (id+key+manifest) the
// peer fetches chunks from via /api/chunk — same wire as receivers.
func (s *Server) handleSyncStart(w http.ResponseWriter, r *http.Request) {
	if s.Sync == nil || s.Sessions == nil {
		http.Error(w, `{"error":"sync unavailable"}`, http.StatusNotFound)
		return
	}
	var body struct {
		Rels []string `json:"rels"`
	}
	if err := decodeBody(r, w, &body, 1<<20); err != nil {
		return
	}
	if len(body.Rels) == 0 {
		http.Error(w, `{"error":"no paths"}`, http.StatusBadRequest)
		return
	}
	sess, m, err := s.Sync.StartSession(body.Rels, syncRegistrar{s.newSyncSession})
	if err != nil {
		// Distinguish "we don't have it anymore" (peer retries others)
		// from real internal trouble; both stay generic on the wire.
		http.Error(w, `{"error":"sync start failed"}`, http.StatusBadRequest)
		return
	}
	writeJSON(w, http.StatusOK, syncStartResponse{
		SessionID: sess.ID,
		Key:       sess.KeyEncoded,
		Manifest:  m,
	})
}

// newSyncSession registers a manifest as a live session (wire values
// only: id + encoded key — syncmesh must not import session).
func (s *Server) newSyncSession(m *transfer.Manifest) (syncmesh.ServedSession, error) {
	sess, err := s.Sessions.NewSession(m)
	if err != nil {
		return syncmesh.ServedSession{}, err
	}
	return syncmesh.ServedSession{ID: sess.ID, KeyEncoded: sess.KeyEncoded()}, nil
}

// syncRegistrar adapts newSyncSession to syncmesh.SessionRegistrar.
type syncRegistrar struct {
	new func(*transfer.Manifest) (syncmesh.ServedSession, error)
}

func (r syncRegistrar) NewSession(m *transfer.Manifest) (syncmesh.ServedSession, error) {
	return r.new(m)
}

type syncStartResponse struct {
	SessionID string             `json:"sessionId"`
	Key       string             `json:"key"`
	Manifest  *transfer.Manifest `json:"manifest"`
}

func localIP() string {
	// Best-effort display value for the peers panel.
	conn, err := net.Dial("udp", "8.8.8.8:80")
	if err == nil {
		defer conn.Close()
		if a := conn.LocalAddr(); a != nil {
			if host, _, err := net.SplitHostPort(a.String()); err == nil {
				return host
			}
		}
	}
	return ""
}
