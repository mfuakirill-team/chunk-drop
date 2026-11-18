package api

import (
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"chunkdrop/internal/config"
	"chunkdrop/internal/discovery"
	"chunkdrop/internal/hubclient"
	"chunkdrop/internal/ratemeter"
)

// stringReader is strings.NewReader with a short local name (the
// request bodies in this file are POST /api/mode JSON).
func stringReader(s string) *strings.Reader { return strings.NewReader(s) }

// GET /api/mode (M2 step ④ rework): the current global mesh mode.
func TestModeGet(t *testing.T) {
	const key = "group-secret-group-secret-group!!"
	srv, _, _ := newTestServer(t)
	srv.SetGroupKey(key)
	srv.SetMeshMode("local")
	h := srv.Handler()

	rec := reqKey(t, h, http.MethodGet, "/api/mode", key)
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d: %s", rec.Code, rec.Body.String())
	}
	var out struct {
		Mode  string   `json:"mode"`
		Modes []string `json:"modes"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &out); err != nil {
		t.Fatal(err)
	}
	if out.Mode != "local" {
		t.Errorf("mode = %q, want local", out.Mode)
	}
	if len(out.Modes) != 3 {
		t.Errorf("modes = %v", out.Modes)
	}

	// Unset (legacy config) reads as auto.
	srv.SetMeshMode("")
	rec = reqKey(t, h, http.MethodGet, "/api/mode", key)
	out = struct {
		Mode  string   `json:"mode"`
		Modes []string `json:"modes"`
	}{}
	if err := json.Unmarshal(rec.Body.Bytes(), &out); err != nil {
		t.Fatal(err)
	}
	if out.Mode != "auto" {
		t.Errorf("empty mode reads = %q, want auto", out.Mode)
	}

	// Wrong key → 401.
	if rec := reqKey(t, h, http.MethodGet, "/api/mode", "wrong-key-wrong-key-wrong"); rec.Code != http.StatusUnauthorized {
		t.Errorf("wrong key = %d, want 401", rec.Code)
	}
}

// POST /api/mode applies via OnMode (the engine rebuilds the mesh and
// persists engine.toml); errors refuse the switch.
func TestModeSet(t *testing.T) {
	const key = "group-secret-group-secret-group!!"

	newSrv := func(t *testing.T, onMode func(string) error) http.Handler {
		t.Helper()
		srv, _, _ := newTestServer(t)
		srv.SetGroupKey(key)
		srv.OnMode = onMode
		return srv.Handler()
	}

	post := func(t *testing.T, h http.Handler, body string) *httptest.ResponseRecorder {
		t.Helper()
		req := httptest.NewRequest(http.MethodPost, "/api/mode", stringReader(body))
		req.RemoteAddr = testLocalAddr
		req.Header.Set(HeaderKey, key)
		rec := httptest.NewRecorder()
		h.ServeHTTP(rec, req)
		return rec
	}

	t.Run("applies a valid mode", func(t *testing.T) {
		var applied []string
		h := newSrv(t, func(m string) error { applied = append(applied, m); return nil })
		rec := post(t, h, `{"mode":"internet"}`)
		if rec.Code != http.StatusOK {
			t.Fatalf("status = %d: %s", rec.Code, rec.Body.String())
		}
		if len(applied) != 1 || applied[0] != "internet" {
			t.Errorf("OnMode calls = %v", applied)
		}
	})

	t.Run("unknown mode → 400, not applied", func(t *testing.T) {
		var applied []string
		h := newSrv(t, func(m string) error { applied = append(applied, m); return nil })
		if rec := post(t, h, `{"mode":"turbo"}`); rec.Code != http.StatusBadRequest {
			t.Fatalf("status = %d, want 400", rec.Code)
		}
		if len(applied) != 0 {
			t.Errorf("OnMode called: %v", applied)
		}
	})

	t.Run("engine refuses → 500", func(t *testing.T) {
		h := newSrv(t, func(string) error { return errors.New("nope") })
		if rec := post(t, h, `{"mode":"local"}`); rec.Code != http.StatusInternalServerError {
			t.Fatalf("status = %d, want 500", rec.Code)
		}
	})

	t.Run("no OnMode → 501 (read-only engine)", func(t *testing.T) {
		h := newSrv(t, nil)
		if rec := post(t, h, `{"mode":"local"}`); rec.Code != http.StatusNotImplemented {
			t.Fatalf("status = %d, want 501", rec.Code)
		}
	})

	t.Run("wrong key → 401", func(t *testing.T) {
		h := newSrv(t, func(string) error { return nil })
		req := httptest.NewRequest(http.MethodPost, "/api/mode", stringReader(`{"mode":"local"}`))
		req.RemoteAddr = testLocalAddr
		req.Header.Set(HeaderKey, "wrong-key-wrong-key-wrong")
		rec := httptest.NewRecorder()
		h.ServeHTTP(rec, req)
		if rec.Code != http.StatusUnauthorized {
			t.Errorf("status = %d, want 401", rec.Code)
		}
	})
}

// config.ValidMeshMode covers the full accepted vocabulary.
func TestMeshModeVocabulary(t *testing.T) {
	for _, m := range []string{"auto", "local", "internet"} {
		if !config.ValidMeshMode(m) {
			t.Errorf("%q rejected", m)
		}
	}
	for _, m := range []string{"", "tunnel", "relay", "off", "Auto"} {
		if config.ValidMeshMode(m) {
			t.Errorf("%q accepted", m)
		}
	}
}

// /api/peers rows carry the live rates: "up" from the chunk handler's
// meter (attributed by transport), "down" from the puller's meter.
func TestPeersRateFields(t *testing.T) {
	const key = "group-secret-group-secret-group!!"
	const peerID = "bbbbbbbbbbbbbbbb"
	srv, _, _ := newTestServer(t)
	srv.SetGroupKey(key)
	srv.SetDiscovery(&staticPeers{
		self: discovery.Beacon{V: 1, ID: "aaaaaaaaaaaaaaaa", Name: "lan-box", Port: 8090, TS: 1},
		members: []discovery.Member{
			{Beacon: discovery.Beacon{V: 1, ID: peerID, Name: "other-box", Port: 8090, TS: 1}, IP: "192.168.1.9"},
		},
	})
	srv.SetHub(hubStub{devices: []hubclient.Peer{
		{ID: peerID, Name: "other-box"},   // LAN + hub
		{ID: "cccccccccccc", Name: "far"}, // hub-only
	}})
	down := ratemeter.New(ratemeter.DefaultWindow)
	srv.DownloadRates = down
	h := srv.Handler()

	// Serve 2000 bytes to the LAN peer (chunk handler's meter).
	srv.UploadRates() // sanity: no panic before any bytes
	srv.uploadRates.Add(peerID, 2000)
	down.Add("cccccccccccc", 1000)

	rec := reqKey(t, h, http.MethodGet, "/api/peers", key)
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d: %s", rec.Code, rec.Body.String())
	}
	var out struct {
		Peers []struct {
			ID   string  `json:"id"`
			Down float64 `json:"down"`
			Up   float64 `json:"up"`
		} `json:"peers"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &out); err != nil {
		t.Fatal(err)
	}
	if len(out.Peers) != 2 {
		t.Fatalf("peers = %d rows, want 2", len(out.Peers))
	}
	byID := map[string]struct{ down, up float64 }{}
	for _, p := range out.Peers {
		byID[p.ID] = struct{ down, up float64 }{p.Down, p.Up}
	}
	if p := byID[peerID]; p.up <= 0 || p.down != 0 {
		t.Errorf("LAN+hub peer rates = %+v, want up>0, down 0", p)
	}
	if p := byID["cccccccccccc"]; p.down <= 0 || p.up != 0 {
		t.Errorf("hub-only peer rates = %+v, want down>0, up 0", p)
	}
}

// requestPeerID names the mesh peer behind a request by transport:
// tunnel/relay RemoteAddr prefixes, LAN via the discovery table.
func TestRequestPeerID(t *testing.T) {
	d := &staticPeers{
		self: discovery.Beacon{V: 1, ID: "aaaaaaaaaaaaaaaa", Name: "lan-box", Port: 8090, TS: 1},
		members: []discovery.Member{
			{Beacon: discovery.Beacon{V: 1, ID: "bbbbbbbbbbbbbbbb", Name: "other-box", Port: 8090, TS: 1}, IP: "192.168.1.9"},
		},
	}
	cases := []struct {
		remote, want string
	}{
		{"tunnel:bbbbccccdddd", "bbbbccccdddd"},
		{"relay:bbbbccccdddd", "bbbbccccdddd"},
		{"192.168.1.9:5555", "bbbbbbbbbbbbbbbb"},
		{"10.9.9.9:1234", ""}, // unknown LAN host
		{"tunnelx", ""},       // prefix lookalike → falls through to LAN lookup
	}
	for _, c := range cases {
		req := httptest.NewRequest(http.MethodGet, "/api/chunk/0/0", nil)
		req.RemoteAddr = c.remote
		if got := requestPeerID(req, d); got != c.want {
			t.Errorf("remote %q → id %q, want %q", c.remote, got, c.want)
		}
	}
	req := httptest.NewRequest(http.MethodGet, "/api/chunk/0/0", nil)
	req.RemoteAddr = "192.168.1.9:1"
	if got := requestPeerID(req, nil); got != "" {
		t.Errorf("nil discovery → %q, want empty", got)
	}
}
