package api

import (
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"chunkdrop/internal/discovery"
	"chunkdrop/internal/hubclient"
	"chunkdrop/internal/syncmesh"
)

// fakeRoutes is a RouteControl stub recording SetMode calls.
type fakeRoutes struct {
	modes  map[string]string
	routes map[string]string
	set    []string // "id=mode" in call order
	fail   error    // SetMode always fails when set
}

func (f *fakeRoutes) Routes() map[string]string { return f.routes }
func (f *fakeRoutes) Modes() map[string]string  { return f.modes }
func (f *fakeRoutes) SetMode(id, mode string) error {
	if f.fail != nil {
		return f.fail
	}
	f.set = append(f.set, id+"="+mode)
	return nil
}

// POST /api/peer/mode (M2 step ④): the dashboard steers per-peer
// routing (auto/tunnel/relay/off). Local-only + group-key gated.
func TestPeerMode(t *testing.T) {
	const key = "group-secret-group-secret-group!!"

	t.Run("applies a valid mode", func(t *testing.T) {
		srv, _, _ := newTestServer(t)
		srv.SetGroupKey(key)
		rc := &fakeRoutes{}
		srv.SetRoutes(rc)
		h := srv.Handler()
		rec := post(t, h, "/api/peer/mode", key, `{"id":"abc123","mode":"relay"}`)
		if rec.Code != http.StatusOK {
			t.Fatalf("status = %d: %s", rec.Code, rec.Body.String())
		}
		if len(rc.set) != 1 || rc.set[0] != "abc123=relay" {
			t.Errorf("SetMode calls = %v", rc.set)
		}
		var out struct {
			OK   bool   `json:"ok"`
			ID   string `json:"id"`
			Mode string `json:"mode"`
		}
		if err := json.Unmarshal(rec.Body.Bytes(), &out); err != nil {
			t.Fatal(err)
		}
		if !out.OK || out.ID != "abc123" || out.Mode != "relay" {
			t.Errorf("response = %s", rec.Body.String())
		}
	})

	t.Run("unknown mode — rejected, nothing applied", func(t *testing.T) {
		srv, _, _ := newTestServer(t)
		srv.SetGroupKey(key)
		rc := &fakeRoutes{}
		srv.SetRoutes(rc)
		h := srv.Handler()
		rec := post(t, h, "/api/peer/mode", key, `{"id":"abc123","mode":"turbo"}`)
		if rec.Code != http.StatusBadRequest {
			t.Fatalf("status = %d, want 400", rec.Code)
		}
		if len(rc.set) != 0 {
			t.Errorf("SetMode called: %v", rc.set)
		}
	})

	t.Run("missing id — rejected", func(t *testing.T) {
		srv, _, _ := newTestServer(t)
		srv.SetGroupKey(key)
		rc := &fakeRoutes{}
		srv.SetRoutes(rc)
		h := srv.Handler()
		rec := post(t, h, "/api/peer/mode", key, `{"id":"  ","mode":"off"}`)
		if rec.Code != http.StatusBadRequest {
			t.Fatalf("status = %d, want 400", rec.Code)
		}
	})

	t.Run("engine refuses — 500", func(t *testing.T) {
		srv, _, _ := newTestServer(t)
		srv.SetGroupKey(key)
		rc := &fakeRoutes{fail: errors.New("nope")}
		srv.SetRoutes(rc)
		h := srv.Handler()
		rec := post(t, h, "/api/peer/mode", key, `{"id":"abc123","mode":"off"}`)
		if rec.Code != http.StatusInternalServerError {
			t.Fatalf("status = %d, want 500", rec.Code)
		}
	})

	t.Run("no route state — 404 (mesh-less engine)", func(t *testing.T) {
		srv, _, _ := newTestServer(t)
		srv.SetGroupKey(key)
		h := srv.Handler()
		rec := post(t, h, "/api/peer/mode", key, `{"id":"abc123","mode":"off"}`)
		if rec.Code != http.StatusNotFound {
			t.Fatalf("status = %d, want 404", rec.Code)
		}
	})

	t.Run("wrong group key — 401", func(t *testing.T) {
		srv, _, _ := newTestServer(t)
		srv.SetGroupKey(key)
		srv.SetRoutes(&fakeRoutes{})
		h := srv.Handler()
		rec := post(t, h, "/api/peer/mode", "wrong-key-wrong-key-wrong", `{"id":"abc123","mode":"off"}`)
		if rec.Code != http.StatusUnauthorized {
			t.Fatalf("status = %d, want 401", rec.Code)
		}
	})
}

// GET /api/peers (step ④): rows carry the owner's mode and the route
// last used — "lan" for LAN peers, the dialer's observation for
// internet peers; unset modes report as "auto".
func TestPeersRouteFields(t *testing.T) {
	const key = "group-secret-group-secret-group!!"
	srv, _, _ := newTestServer(t)
	srv.SetGroupKey(key)
	srv.SetDiscovery(&staticPeers{
		self: discovery.Beacon{V: 1, ID: "aaaaaaaaaaaaaaaa", Name: "lan-box", Port: 8090, TS: 1},
		members: []discovery.Member{
			{Beacon: discovery.Beacon{V: 1, ID: "bbbbbbbbbbbbbbbb", Name: "other-box", Port: 8090, TS: 1}, IP: "192.168.1.9"},
		},
	})
	srv.SetHub(hubStub{devices: []hubclient.Peer{
		{ID: "bbbbbbbbbbbbbbbb", Name: "other-box"}, // LAN + hub
		{ID: "cccccccccccccccc", Name: "far-box"},   // hub-only
	}})
	srv.SetRoutes(&fakeRoutes{
		modes:  map[string]string{"bbbbbbbbbbbbbbbb": "off", "cccccccccccccccc": "relay"},
		routes: map[string]string{"cccccccccccccccc": "relay"},
	})
	h := srv.Handler()

	type row struct {
		ID    string `json:"id"`
		Hub   bool   `json:"hub"`
		Route string `json:"route"`
		Mode  string `json:"mode"`
	}
	var out struct {
		Peers []row `json:"peers"`
	}

	snapshot := func() {
		t.Helper()
		rec := reqKey(t, h, http.MethodGet, "/api/peers", key)
		if rec.Code != http.StatusOK {
			t.Fatalf("status = %d: %s", rec.Code, rec.Body.String())
		}
		out.Peers = nil
		if err := json.Unmarshal(rec.Body.Bytes(), &out); err != nil {
			t.Fatal(err)
		}
	}
	byID := func() map[string]row {
		t.Helper()
		m := map[string]row{}
		for _, p := range out.Peers {
			m[p.ID] = p
		}
		return m
	}

	snapshot()
	got := byID()
	if len(got) != 2 {
		t.Fatalf("peers = %v, want the LAN + hub-only rows", got)
	}
	if p := got["bbbbbbbbbbbbbbbb"]; p.Route != "lan" || p.Mode != "off" {
		t.Errorf("LAN peer row = %+v, want route=lan mode=off", p)
	}
	if p := got["cccccccccccccccc"]; p.Route != "relay" || p.Mode != "relay" {
		t.Errorf("hub-only row = %+v, want route=relay mode=relay", p)
	}

	// Unset modes report as auto (the UI binds a select to it); an
	// internet peer without a dial yet has an empty route.
	srv.SetRoutes(&fakeRoutes{})
	snapshot()
	got = byID()
	for _, p := range got {
		if p.Mode != "auto" {
			t.Errorf("peer %s mode = %q, want auto", p.ID, p.Mode)
		}
	}
	if p := got["cccccccccccccccc"]; p.Route != "" {
		t.Errorf("undialed internet peer route = %q, want empty", p.Route)
	}
}

// compile-time interface check.
var _ RouteControl = (*fakeRoutes)(nil)

// Serve side of the pause (step ④): a peer put "off" cannot pull from
// this engine either — tunnel/relay conns are named by their
// RemoteAddr prefix, LAN conns by the discovery table.
func TestSyncStartPausedPeer(t *testing.T) {
	const key = "group-secret-group-secret-group!!"
	const pausedID = "pppppppppppppppp"

	syncDir := t.TempDir()
	if err := os.WriteFile(filepath.Join(syncDir, "note.txt"), []byte("hi"), 0o644); err != nil {
		t.Fatal(err)
	}
	local, err := syncmesh.NewLocal(syncDir, "testbox", "eng1")
	if err != nil {
		t.Fatal(err)
	}

	srv, _, _ := newTestServer(t)
	srv.SetGroupKey(key)
	srv.Sync = local
	srv.SetDiscovery(&staticPeers{
		self: discovery.Beacon{V: 1, ID: "aaaaaaaaaaaaaaaa", Name: "lan-box", Port: 8090, TS: 1},
		members: []discovery.Member{
			{Beacon: discovery.Beacon{V: 1, ID: pausedID, Name: "paused-box", Port: 8090, TS: 1}, IP: "10.0.0.9"},
		},
	})
	srv.SetRoutes(&fakeRoutes{modes: map[string]string{pausedID: "off"}})
	h := srv.Handler()

	post := func(remote string) int {
		req := httptest.NewRequest(http.MethodPost, "/api/sync/start", strings.NewReader(`{"rels":["note.txt"]}`))
		req.RemoteAddr = remote
		req.Header.Set(HeaderKey, key)
		rec := httptest.NewRecorder()
		h.ServeHTTP(rec, req)
		return rec.Code
	}

	if got := post("10.0.0.9:5555"); got != http.StatusBadRequest {
		t.Errorf("LAN pull from paused peer = %d, want 400", got)
	}
	if got := post("tunnel:" + pausedID); got != http.StatusBadRequest {
		t.Errorf("tunnel pull from paused peer = %d, want 400", got)
	}
	if got := post("relay:" + pausedID); got != http.StatusBadRequest {
		t.Errorf("relay pull from paused peer = %d, want 400", got)
	}

	// Same peer unpaused → the pull goes through.
	srv.SetRoutes(&fakeRoutes{})
	if got := post("10.0.0.9:5555"); got != http.StatusOK {
		t.Errorf("unpaused peer pull = %d, want 200", got)
	}
	// An unidentified conn (phone receiver shape) is never paused.
	srv.SetRoutes(&fakeRoutes{modes: map[string]string{pausedID: "off"}})
	if got := post("203.0.113.7:9999"); got != http.StatusOK {
		t.Errorf("unidentified conn = %d, want 200 (not a mesh engine)", got)
	}
}
