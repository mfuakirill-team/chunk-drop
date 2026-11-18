package api

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"chunkdrop/internal/discovery"
	"chunkdrop/internal/hubclient"
)

// POST /api/key (M2): the dashboard sets the group key. Local-only
// plus the same-page stamp (HeaderUI + matching Origin) — the current
// key is NOT the credential, the owner replaces it freely. Applied
// instantly; only the engine.toml write is deferred.
func TestKeySet(t *testing.T) {
	const oldKey = "old-secret-old-secret-old-secret!"
	const newKey = "new-secret-new-secret-new-secret!"

	t.Run("first run — no key configured", func(t *testing.T) {
		srv, _, _ := newTestServer(t)
		srv.SetGroupKey("")
		var applied string
		srv.OnKey = func(key string) { applied = key }
		h := srv.Handler()
		rec := reqKeyStamp(t, h, "", `{"key":"`+newKey+`"}`)
		if rec.Code != http.StatusOK {
			t.Fatalf("status = %d: %s", rec.Code, rec.Body.String())
		}
		if applied != newKey {
			t.Errorf("OnKey = %q, want %q", applied, newKey)
		}
		// The dashboard auth swaps immediately.
		if srv.GroupKey() != newKey {
			t.Errorf("GroupKey = %q, want the applied key", srv.GroupKey())
		}
		var out struct {
			Applied       bool `json:"applied"`
			SaveInSeconds int  `json:"saveInSeconds"`
		}
		if err := json.Unmarshal(rec.Body.Bytes(), &out); err != nil {
			t.Fatal(err)
		}
		if !out.Applied || out.SaveInSeconds != keySaveSeconds {
			t.Errorf("response = applied=%v saveInSeconds=%d", out.Applied, out.SaveInSeconds)
		}
	})

	t.Run("fresh install — placeholder is not a credential", func(t *testing.T) {
		srv, _, _ := newTestServer(t)
		srv.SetGroupKey("<paste `engine keygen` output here>")
		var applied string
		srv.OnKey = func(key string) { applied = key }
		h := srv.Handler()
		rec := reqKeyStamp(t, h, "", `{"key":"test"}`)
		if rec.Code != http.StatusOK {
			t.Fatalf("status = %d: %s", rec.Code, rec.Body.String())
		}
		if applied != "test" || srv.GroupKey() != "test" {
			t.Errorf("applied=%q groupKey=%q, want \"test\"", applied, srv.GroupKey())
		}
	})

	t.Run("no UI stamp — rejected (the old drive-by shape)", func(t *testing.T) {
		srv, _, _ := newTestServer(t)
		srv.SetGroupKey(oldKey)
		called := false
		srv.OnKey = func(string) { called = true }
		h := srv.Handler()
		rec := reqKeyBody(t, h, http.MethodPost, "/api/key", "", `{"key":"`+newKey+`"}`)
		if rec.Code != http.StatusUnauthorized {
			t.Fatalf("status = %d, want 401", rec.Code)
		}
		if called {
			t.Error("OnKey called despite missing stamp")
		}
		if srv.GroupKey() != oldKey {
			t.Errorf("GroupKey changed on rejected request: %q", srv.GroupKey())
		}
	})

	t.Run("foreign Origin — rejected (drive-by webpage)", func(t *testing.T) {
		srv, _, _ := newTestServer(t)
		srv.SetGroupKey(oldKey)
		called := false
		srv.OnKey = func(string) { called = true }
		h := srv.Handler()
		rec := reqKeyOrigin(t, h, "https://evil.example", `{"key":"`+newKey+`"}`)
		if rec.Code != http.StatusUnauthorized {
			t.Fatalf("status = %d, want 401", rec.Code)
		}
		if called {
			t.Error("OnKey called despite foreign Origin")
		}
		if srv.GroupKey() != oldKey {
			t.Errorf("GroupKey changed on rejected request: %q", srv.GroupKey())
		}
	})

	t.Run("own Origin — accepted", func(t *testing.T) {
		srv, _, _ := newTestServer(t)
		srv.SetGroupKey(oldKey)
		var applied string
		srv.OnKey = func(key string) { applied = key }
		h := srv.Handler()
		rec := reqKeyOrigin(t, h, "http://192.168.1.5:8090", `{"key":"`+newKey+`"}`)
		if rec.Code != http.StatusOK {
			t.Fatalf("status = %d: %s", rec.Code, rec.Body.String())
		}
		if applied != newKey || srv.GroupKey() != newKey {
			t.Errorf("applied=%q groupKey=%q", applied, srv.GroupKey())
		}
	})

	t.Run("no Origin (curl, script) — accepted", func(t *testing.T) {
		srv, _, _ := newTestServer(t)
		srv.SetGroupKey(oldKey)
		var applied string
		srv.OnKey = func(key string) { applied = key }
		h := srv.Handler()
		rec := reqKeyStamp(t, h, "", `{"key":"`+newKey+`"}`)
		if rec.Code != http.StatusOK {
			t.Fatalf("status = %d: %s", rec.Code, rec.Body.String())
		}
		if applied != newKey || srv.GroupKey() != newKey {
			t.Errorf("applied=%q groupKey=%q", applied, srv.GroupKey())
		}
	})

	t.Run("key already set — browser that lost its copy just retypes it", func(t *testing.T) {
		srv, _, _ := newTestServer(t)
		srv.SetGroupKey(oldKey)
		rekeyed := false
		srv.OnKey = func(string) { rekeyed = true }
		h := srv.Handler()
		rec := reqKeyStamp(t, h, "", `{"key":"`+oldKey+`"}`)
		if rec.Code != http.StatusOK {
			t.Fatalf("status = %d: %s", rec.Code, rec.Body.String())
		}
		var out struct {
			Applied bool `json:"applied"`
		}
		if err := json.Unmarshal(rec.Body.Bytes(), &out); err != nil {
			t.Fatal(err)
		}
		if out.Applied || rekeyed || srv.GroupKey() != oldKey {
			t.Errorf("same-key resubmit must be a no-op: applied=%v rekeyed=%v key=%q",
				out.Applied, rekeyed, srv.GroupKey())
		}
	})

	t.Run("short key accepted — no format restrictions", func(t *testing.T) {
		srv, _, _ := newTestServer(t)
		srv.SetGroupKey("")
		var applied string
		srv.OnKey = func(key string) { applied = key }
		h := srv.Handler()
		rec := reqKeyStamp(t, h, "", `{"key":"test"}`)
		if rec.Code != http.StatusOK {
			t.Fatalf("status = %d: %s", rec.Code, rec.Body.String())
		}
		if applied != "test" {
			t.Errorf("applied = %q, want \"test\"", applied)
		}
	})

	t.Run("invalid key rejected", func(t *testing.T) {
		srv, _, _ := newTestServer(t)
		srv.SetGroupKey("")
		srv.OnKey = func(string) { t.Error("OnKey called for invalid key") }
		h := srv.Handler()
		rec := reqKeyStamp(t, h, "", `{"key":"bad\tkey"}`)
		if rec.Code != http.StatusBadRequest {
			t.Fatalf("status = %d, want 400", rec.Code)
		}
	})

	t.Run("no OnKey wired — endpoint disabled", func(t *testing.T) {
		srv, _, _ := newTestServer(t)
		h := srv.Handler()
		rec := reqKeyStamp(t, h, "", `{"key":"`+newKey+`"}`)
		if rec.Code != http.StatusNotFound {
			t.Fatalf("status = %d, want 404", rec.Code)
		}
	})
}

// reqKeyStamp POSTs /api/key with the dashboard's stamp headers but no
// Origin header (the curl/script shape — and the browser shape when
// the engine URL was opened without a referring site).
func reqKeyStamp(t *testing.T, h http.Handler, origin, body string) *httptest.ResponseRecorder {
	t.Helper()
	return reqKeyOrigin(t, h, origin, body)
}

// reqKeyOrigin POSTs /api/key with the UI stamp; origin "" sends no
// Origin header, anything else sets it verbatim (own or foreign). The
// request Host is fixed to the engine's LAN address so "own Origin"
// values can be formed exactly like the browser would.
func reqKeyOrigin(t *testing.T, h http.Handler, origin, body string) *httptest.ResponseRecorder {
	t.Helper()
	req := httptest.NewRequest(http.MethodPost, "/api/key", strings.NewReader(body))
	req.RemoteAddr = testLocalAddr
	req.Host = "192.168.1.5:8090"
	req.Header.Set(HeaderUI, "dashboard")
	if origin != "" {
		req.Header.Set("Origin", origin)
	}
	req.Header.Set("Content-Type", "application/json")
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	return rec
}

// GET /api/peers (M2 clients card): LAN beacons and hub devices join
// by the persistent device ID — LAN+hub clients get hub=true but keep
// their LAN ip; hub-only clients carry no ip (UI shows the ID prefix).
func TestPeersMergesHubDevices(t *testing.T) {
	srv, _, _ := newTestServer(t)
	srv.SetGroupKey("group-secret-group-secret-group!!")

	srv.SetDiscovery(&staticPeers{
		self: discovery.Beacon{V: 1, ID: "aaaaaaaaaaaaaaaa", Name: "lan-box", Port: 8090, TS: 1},
		members: []discovery.Member{
			{Beacon: discovery.Beacon{V: 1, ID: "bbbbbbbbbbbbbbbb", Name: "other-box", Port: 8090, TS: 1}, IP: "192.168.1.9"},
		},
	})
	// Remote device bbbb is ALSO on the LAN (same ID — beacons carry
	// the device ID); cccc is hub-only.
	srv.SetHub(hubStub{devices: []hubclient.Peer{
		{ID: "bbbbbbbbbbbbbbbb", Name: "other-box"},
		{ID: "cccccccccccccccc", Name: "far-box"},
	}})

	h := srv.Handler()
	rec := reqKey(t, h, http.MethodGet, "/api/peers", srv.GroupKey())
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d: %s", rec.Code, rec.Body.String())
	}
	var out struct {
		Peers []struct {
			ID   string `json:"id"`
			IP   string `json:"ip"`
			Port int    `json:"port"`
			Hub  bool   `json:"hub"`
		} `json:"peers"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &out); err != nil {
		t.Fatal(err)
	}
	type row struct {
		IP  string
		Hub bool
	}
	byID := map[string]row{}
	for _, p := range out.Peers {
		byID[p.ID] = row{p.IP, p.Hub}
	}
	lanHub := byID["bbbbbbbbbbbbbbbb"]
	if lanHub.IP != "192.168.1.9" || !lanHub.Hub {
		t.Errorf("LAN+hub client = %+v, want LAN ip with hub=true", lanHub)
	}
	remote := byID["cccccccccccccccc"]
	if remote.IP != "" || !remote.Hub {
		t.Errorf("hub-only client = %+v, want no ip with hub=true", remote)
	}
	if _, ok := byID["aaaaaaaaaaaaaaaa"]; ok {
		t.Error("self device leaked into the peer list via the hub")
	}
}

// staticPeers is a fixed discovery snapshot (test stub).
type staticPeers struct {
	self    discovery.Beacon
	members []discovery.Member
}

func (s *staticPeers) Self() discovery.Beacon { return s.self }

func (s *staticPeers) Table() *discovery.Table {
	t := &discovery.Table{}
	for _, m := range s.members {
		t.Observe(m.Beacon, m.IP)
	}
	return t
}

// hubStub is a fixed hub status (test stub for HubSource).
type hubStub struct {
	devices []hubclient.Peer
}

func (h hubStub) Status() hubclient.Status {
	return hubclient.Status{URL: "ws://hub.example/hub/ws", Devices: h.devices}
}
