package api

import (
	"fmt"
	"net"
	"net/http"
	"strings"
	"time"

	"chunkdrop/internal/activity"
	"chunkdrop/internal/hubclient"
	"chunkdrop/internal/lanip"
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

// handlePeers lists mesh clients for the dashboard "Clients" panel
// (#7): LAN-discovered engines (beacons carry the persistent device
// ID, so the two sources join by it) merged with the hub's online
// devices. The "hub" flag marks clients that are reachable over the
// internet — the UI shows them with a globe; hub-only ones have no
// LAN ip, so they are shown by the first characters of their ID.
// With route controls wired (M2 step ④) each row also carries the
// owner's mode ("auto"/"tunnel"/"relay"/"off") and — for internet
// peers — the route last used ("tunnel"/"relay"; LAN peers are
// always "lan").
func (s *Server) handlePeers(w http.ResponseWriter, r *http.Request) {
	rc := s.routesSnapshot()
	var routes, modes map[string]string
	if rc != nil {
		routes, modes = rc.Routes(), rc.Modes()
	}
	modeOf := func(id string) string {
		if m, ok := modes[id]; ok {
			return m
		}
		return "auto"
	}
	self := map[string]any{"id": "", "name": "", "ip": "", "port": 0, "self": true}
	peers := []map[string]any{}
	idx := map[string]map[string]any{}
	if d := s.discoverySnapshot(); d != nil {
		b := d.Self()
		self = map[string]any{
			"id": b.ID, "name": b.Name, "ip": localIP(), "port": b.Port, "self": true,
		}
		for _, m := range d.Table().List() {
			row := map[string]any{
				"id":    m.ID,
				"name":  m.Name,
				"ip":    m.IP,
				"port":  m.Port,
				"hub":   false,
				"route": "lan", // LAN peers always take the direct route
				"mode":  modeOf(m.ID),
			}
			idx[m.ID] = row
			peers = append(peers, row)
		}
	}
	for _, dev := range s.hubDevices() {
		if dev.ID == self["id"] {
			continue // the engine itself — covered by the self row
		}
		if row, ok := idx[dev.ID]; ok {
			row["hub"] = true // reachable BOTH ways; UI shows only the LAN ip + 🌐
			continue
		}
		peers = append(peers, map[string]any{
			"id":    dev.ID,
			"name":  dev.Name,
			"ip":    "",
			"port":  0,
			"hub":   true,
			"route": routes[dev.ID], // "tunnel"/"relay"; "" = not dialed yet
			"mode":  modeOf(dev.ID),
		})
	}
	writeJSON(w, http.StatusOK, map[string]any{"self": self, "peers": peers})
}

// discoverySnapshot returns the current discovery source (nil-safe).
func (s *Server) discoverySnapshot() PeersSource {
	s.mu.RLock()
	defer s.mu.RUnlock()
	return s.discovery
}

// hubSnapshot returns the current hub client (nil-safe).
func (s *Server) hubSnapshot() HubSource {
	s.mu.RLock()
	defer s.mu.RUnlock()
	return s.hub
}

// hubDevices snapshots the hub's online device list (nil-safe).
func (s *Server) hubDevices() []hubclient.Peer {
	c := s.hubSnapshot()
	if c == nil {
		return nil
	}
	return c.Status().Devices
}

// peerPaused reports whether the request comes from a peer the
// dashboard paused (mode "off", step ④). The pulling peer's identity:
// tunnel/relay conns carry `tunnel:<deviceID>` / `relay:<deviceID>`
// RemoteAddrs; LAN conns carry an IP:port the discovery table can
// name. Unknown identity (phones, unidentified conns) → not paused —
// those are not mesh engines and the group key already gated them.
func (s *Server) peerPaused(r *http.Request) bool {
	rc := s.routesSnapshot()
	if rc == nil {
		return false
	}
	peer := requestPeerID(r, s.discoverySnapshot())
	if peer == "" {
		return false
	}
	return rc.Modes()[peer] == "off"
}

// requestPeerID identifies the engine behind a mesh request ("" when
// unknown). See peerPaused for the transport shapes.
func requestPeerID(r *http.Request, d PeersSource) string {
	ra := r.RemoteAddr
	for _, p := range []string{"tunnel:", "relay:"} {
		if strings.HasPrefix(ra, p) {
			return strings.TrimPrefix(ra, p)
		}
	}
	if d == nil {
		return ""
	}
	host, _, err := net.SplitHostPort(ra)
	if err != nil {
		host = ra
	}
	for _, m := range d.Table().List() {
		if m.IP == host {
			return m.ID
		}
	}
	return ""
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
// A peer the dashboard put "off" (M2 step ④) is refused here: pause
// is two-sided wherever the pulling peer is identifiable (tunnel and
// relay conns carry the device ID in RemoteAddr; LAN conns are named
// by the discovery table). An in-flight pull that already holds a
// session key finishes — the gate closes the next pull.
func (s *Server) handleSyncStart(w http.ResponseWriter, r *http.Request) {
	if s.Sync == nil || s.Sessions == nil {
		http.Error(w, `{"error":"sync unavailable"}`, http.StatusNotFound)
		return
	}
	if s.peerPaused(r) {
		// Same generic body as any other sync-start failure: the
		// paused peer learns nothing about why.
		http.Error(w, `{"error":"sync start failed"}`, http.StatusBadRequest)
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
	s.Activity.Add(activity.KindUp, fmt.Sprintf("serving %q to peer %s", strings.Join(body.Rels, ", "), r.RemoteAddr))
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

// handleActivity serves the concise activity feed (#1.3): what is
// uploading (live sessions serving receivers), what downloaded or was
// deleted (sync mesh), what failed. Ring buffer, RAM-only.
func (s *Server) handleActivity(w http.ResponseWriter, r *http.Request) {
	events := s.Activity.List()
	if events == nil {
		events = []activity.Event{}
	}
	resp := map[string]any{
		"events":  events,
		"uploads": s.liveUploads(),
	}
	if s.Uploads != nil {
		// Phone→PC pushes in progress (M1) — the feed shows their
		// progress the same way it shows live receive sessions.
		resp["incoming"] = s.Uploads.List()
	}
	writeJSON(w, http.StatusOK, resp)
}

// uploadInfo summarizes one currently-served session for the feed.
type uploadInfo struct {
	Session  string         `json:"session"`
	Files    int            `json:"files"`
	Bytes    int64          `json:"bytes"`
	Receiver string         `json:"receiver,omitempty"`
	Have     map[string]any `json:"have,omitempty"`
	Since    string         `json:"since"`
}

func (s *Server) liveUploads() []uploadInfo {
	if s.Sessions == nil {
		return []uploadInfo{}
	}
	out := []uploadInfo{}
	for _, sess := range s.Sessions.ByKeyAll() {
		u := uploadInfo{
			Session: sess.ID,
			Files:   len(sess.Manifest.Files),
			Bytes:   sess.Manifest.TotalSize(),
			Since:   sess.CreatedAt.UTC().Format(time.RFC3339),
		}
		if sess.HasProgress() {
			u.Receiver = sess.Progress.Receiver
			u.Have = sess.Progress.Have
		}
		out = append(out, u)
	}
	return out
}

func localIP() string {
	// Best-effort display value for the peers panel: the address the
	// OS would use to reach the internet (routing-table probe, no
	// packets sent).
	return lanip.DefaultRouteIP()
}
