package api

import (
	"encoding/json"
	"net/http"
	"testing"

	"chunkdrop/internal/discovery"
	"chunkdrop/internal/hubclient"
)

// POST /api/key (M2): the dashboard sets the group key. Local-only;
// with a key already configured the current key is the credential.
func TestKeySet(t *testing.T) {
	const oldKey = "old-secret-old-secret-old-secret!"
	const newKey = "new-secret-new-secret-new-secret!"

	t.Run("first run — no key configured", func(t *testing.T) {
		srv, _, _ := newTestServer(t)
		srv.SetGroupKey("")
		var staged string
		srv.OnKey = func(key string) { staged = key }
		h := srv.Handler()
		rec := reqKeyBody(t, h, http.MethodPost, "/api/key", "", `{"key":"`+newKey+`"}`)
		if rec.Code != http.StatusOK {
			t.Fatalf("status = %d: %s", rec.Code, rec.Body.String())
		}
		if staged != newKey {
			t.Errorf("OnKey = %q, want %q", staged, newKey)
		}
		// The dashboard auth swaps immediately.
		if srv.GroupKey() != newKey {
			t.Errorf("GroupKey = %q, want the staged key", srv.GroupKey())
		}
	})

	t.Run("wrong current key rejected", func(t *testing.T) {
		srv, _, _ := newTestServer(t)
		srv.SetGroupKey(oldKey)
		called := false
		srv.OnKey = func(string) { called = true }
		h := srv.Handler()
		rec := reqKeyBody(t, h, http.MethodPost, "/api/key", "wrong-wrong-wrong", `{"key":"`+newKey+`"}`)
		if rec.Code != http.StatusUnauthorized {
			t.Fatalf("status = %d, want 401", rec.Code)
		}
		if called {
			t.Error("OnKey called despite failed auth")
		}
		if srv.GroupKey() != oldKey {
			t.Errorf("GroupKey changed on rejected request: %q", srv.GroupKey())
		}
	})

	t.Run("correct current key accepted", func(t *testing.T) {
		srv, _, _ := newTestServer(t)
		srv.SetGroupKey(oldKey)
		var staged string
		srv.OnKey = func(key string) { staged = key }
		h := srv.Handler()
		rec := reqKeyBody(t, h, http.MethodPost, "/api/key", oldKey, `{"key":"`+newKey+`"}`)
		if rec.Code != http.StatusOK {
			t.Fatalf("status = %d: %s", rec.Code, rec.Body.String())
		}
		if staged != newKey || srv.GroupKey() != newKey {
			t.Errorf("staged=%q groupKey=%q", staged, srv.GroupKey())
		}
	})

	t.Run("invalid key rejected", func(t *testing.T) {
		srv, _, _ := newTestServer(t)
		srv.SetGroupKey("")
		srv.OnKey = func(string) { t.Error("OnKey called for invalid key") }
		h := srv.Handler()
		rec := reqKeyBody(t, h, http.MethodPost, "/api/key", "", `{"key":"bad key with spaces"}`)
		if rec.Code != http.StatusBadRequest {
			t.Fatalf("status = %d, want 400", rec.Code)
		}
	})

	t.Run("no OnKey wired — endpoint disabled", func(t *testing.T) {
		srv, _, _ := newTestServer(t)
		h := srv.Handler()
		rec := reqKeyBody(t, h, http.MethodPost, "/api/key", "", `{"key":"`+newKey+`"}`)
		if rec.Code != http.StatusNotFound {
			t.Fatalf("status = %d, want 404", rec.Code)
		}
	})
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
