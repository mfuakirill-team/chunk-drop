package hubclient

import (
	"context"
	"encoding/json"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"chunkdrop/internal/device"
	"chunkdrop/internal/hub"

	"github.com/coder/websocket"
)

// The client is tested against the REAL hub (full stack): hello →
// presence, reconnect with backoff, and status reporting.

func testHub(t *testing.T) (url string, srv *httptest.Server, hub1 *hub.Server) {
	t.Helper()
	hub1 = hub.New()
	srv = httptest.NewUnstartedServer(hub1.Handler())
	srv.Start()
	return "ws" + strings.TrimPrefix(srv.URL, "http") + "/hub/ws", srv, hub1
}

func testIdentity(t *testing.T, name string) *device.Identity {
	t.Helper()
	id, err := device.LoadOrCreate(t.TempDir(), name)
	if err != nil {
		t.Fatal(err)
	}
	return id
}

// waitFor spins until cond is true (or fails after the deadline).
func waitFor(t *testing.T, d time.Duration, what string, cond func() bool) {
	t.Helper()
	deadline := time.Now().Add(d)
	for time.Now().Before(deadline) {
		if cond() {
			return
		}
		time.Sleep(10 * time.Millisecond)
	}
	t.Fatalf("timeout waiting for %s", what)
}

func TestConnectPresenceStatus(t *testing.T) {
	url, srv, hub1 := testHub(t)
	defer func() { hub1.Close(); srv.Close() }()

	id := testIdentity(t, "engine-one")
	var mu sync.Mutex
	var seen [][]Peer
	c := New(url, "group-key-alpha", id, t.Logf)
	c.OnPresence = func(peers []Peer) {
		mu.Lock()
		seen = append(seen, peers)
		mu.Unlock()
	}
	c.Start()
	defer c.Stop()

	// Connected + own presence within a second (no backoff on first dial).
	waitFor(t, 3*time.Second, "connection", c.Connected)
	waitFor(t, 3*time.Second, "presence", func() bool {
		mu.Lock()
		defer mu.Unlock()
		return len(seen) > 0 && len(seen[len(seen)-1]) == 1 &&
			seen[len(seen)-1][0].ID == id.ID
	})

	st := c.Status()
	if !st.Connected || st.URL != url || len(st.Devices) != 1 {
		t.Errorf("status = %+v", st)
	}

	// Stop: connected flips false promptly.
	c.Stop()
	waitFor(t, 2*time.Second, "disconnect", func() bool { return !c.Connected() })
}

func TestReconnectAfterHubRestart(t *testing.T) {
	url, srv, hub1 := testHub(t)
	defer srv.Close()

	id := testIdentity(t, "engine-two")
	c := New(url, "group-key-beta", id, t.Logf)
	c.Start()
	defer c.Stop()
	waitFor(t, 3*time.Second, "first connection", c.Connected)

	// Restart the hub underneath the client: Server.Close drops every
	// connection AND wipes the RAM state (presence) — from the mesh's
	// perspective that is exactly a hub restart. The listener keeps
	// serving, so the client's reconnect loop must bring it back.
	hub1.Close()

	waitFor(t, 10*time.Second, "disconnect after hub death", func() bool {
		return !c.Connected()
	})
	waitFor(t, 15*time.Second, "reconnect after backoff", c.Connected)
	waitFor(t, 3*time.Second, "presence after reconnect", func() bool {
		return len(c.Status().Devices) == 1
	})
}

func TestPresenceAcrossTwoClients(t *testing.T) {
	url, srv, hub1 := testHub(t)
	defer func() { hub1.Close(); srv.Close() }()

	id1 := testIdentity(t, "engine-a")
	id2 := testIdentity(t, "engine-b")

	c1 := New(url, "group-key-gamma", id1, t.Logf)
	c2 := New(url, "group-key-gamma", id2, t.Logf)
	c1.Start()
	defer c1.Stop()
	waitFor(t, 3*time.Second, "c1 connected", c1.Connected)

	c2.Start()
	defer c2.Stop()
	// c1 must learn about c2 via presence; c2's own list has both.
	waitFor(t, 3*time.Second, "c1 sees c2", func() bool {
		devs := c1.Status().Devices
		return len(devs) == 2
	})
	waitFor(t, 3*time.Second, "c2 sees both", func() bool {
		devs := c2.Status().Devices
		return len(devs) == 2
	})
}

// TestSendSignalAndDispatch: SendSignal delivers through the hub to a
// second client, and OnSignal fires with the hub-authoritative from
// id and the exact side/body payload.
func TestSendSignalAndDispatch(t *testing.T) {
	url, srv, hub1 := testHub(t)
	defer func() { hub1.Close(); srv.Close() }()

	id1 := testIdentity(t, "engine-sig-a")
	id2 := testIdentity(t, "engine-sig-b")

	c1 := New(url, "group-key-delta", id1, t.Logf)
	c2 := New(url, "group-key-delta", id2, t.Logf)

	type sig struct {
		from, typ, side string
		body            string
	}
	var mu sync.Mutex
	var got []sig
	c2.OnSignal = func(from, typ, side string, body json.RawMessage) {
		mu.Lock()
		got = append(got, sig{from, typ, side, string(body)})
		mu.Unlock()
	}
	c1.Start()
	defer c1.Stop()
	c2.Start()
	defer c2.Stop()
	waitFor(t, 3*time.Second, "c1 connected", c1.Connected)
	waitFor(t, 3*time.Second, "both present", func() bool {
		return len(c1.Status().Devices) == 2 && len(c2.Status().Devices) == 2
	})

	// c1 → c2 offer.
	desc := map[string]any{"type": "offer", "sdp": "v=0\r\no=- 1 2 IN IP4 127.0.0.1"}
	if err := c1.SendSignal(id2.ID, "offer", "sdp", desc); err != nil {
		t.Fatalf("send: %v", err)
	}
	waitFor(t, 3*time.Second, "signal dispatched", func() bool {
		mu.Lock()
		defer mu.Unlock()
		return len(got) == 1
	})
	mu.Lock()
	s := got[0]
	mu.Unlock()
	if s.from != id1.ID || s.typ != "sdp" || s.side != "offer" {
		t.Errorf("signal = %+v", s)
	}
	var back map[string]any
	if err := json.Unmarshal([]byte(s.body), &back); err != nil || back["sdp"] != desc["sdp"] {
		t.Errorf("body roundtrip: %v (%v)", s.body, err)
	}

	// Error path: signaling to an offline device returns an error
	// frame (logged) and SendSignal itself still succeeds.
	if err := c1.SendSignal(strings.Repeat("a", 64), "offer", "sdp", desc); err != nil {
		t.Errorf("send to offline: %v", err)
	}
}

// A group key the hub "doesn't know" is not a rejection — by design
// the hub cannot distinguish an unused key from a private one: any
// well-formed tag+signature simply forms its own (empty) group. So
// there is no "wrong key" case from the client's side; malformed
// hellos are covered by the hub package's tests.

// A raw websocket client that sends a non-hello first frame gets an
// error frame and is closed.
func TestHubRejectsWrongFirstFrame(t *testing.T) {
	url, srv, hub1 := testHub(t)
	defer func() { hub1.Close(); srv.Close() }()

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	ws, _, err := websocket.Dial(ctx, url, nil)
	if err != nil {
		t.Fatal(err)
	}
	defer ws.CloseNow()
	bad := `{"type":"sdp"}`
	if err := ws.Write(ctx, websocket.MessageText, []byte(bad)); err != nil {
		t.Fatal(err)
	}
	// First read: the hub's error frame (or an immediate close).
	typ, data, err := ws.Read(ctx)
	if err == nil {
		if typ != websocket.MessageText || !strings.Contains(string(data), `"error"`) {
			t.Fatalf("expected an error frame, got type=%v data=%s", typ, data)
		}
		_, _, err = ws.Read(ctx)
	}
	if err == nil {
		t.Fatal("connection stayed open after a bad first frame")
	}
}
