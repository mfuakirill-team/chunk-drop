package hub

import (
	"context"
	"crypto/ed25519"
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/coder/websocket"
)

// Hub tests use real websocket clients against an httptest server —
// the hello/presence wire is the contract.

// testDevice mints a throwaway device identity.
type testDevice struct {
	id   string
	name string
	priv ed25519.PrivateKey
	pub  string // base64
	tag  string // hex sha256 of the group key
}

func newTestDevice(t *testing.T, group, name string) *testDevice {
	t.Helper()
	_, priv, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	sum := sha256.Sum256([]byte(group))
	return &testDevice{
		id:   hex.EncodeToString(mustSum(priv.Public().(ed25519.PublicKey))),
		name: name,
		priv: priv,
		pub:  base64.StdEncoding.EncodeToString(priv.Public().(ed25519.PublicKey)),
		tag:  hex.EncodeToString(sum[:]),
	}
}

func mustSum(b []byte) []byte {
	sum := sha256.Sum256(b)
	return sum[:]
}

// dial connects and performs the hello. The first presence frame is
// NOT consumed here — callers assert it explicitly.
func (d *testDevice) dial(t *testing.T, url string, mutate func(*Hello)) *websocket.Conn {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	ws, _, err := websocket.Dial(ctx, url, nil)
	if err != nil {
		t.Fatalf("dial: %v", err)
	}
	ws.SetReadLimit(MaxReadLimit)

	h := Hello{
		Type: "hello", Tag: d.tag, ID: d.id, Name: d.name,
		Pub: d.pub, TS: time.Now().Unix(),
	}
	h.Sig = base64.StdEncoding.EncodeToString(
		ed25519.Sign(d.priv, HelloCanonical(h.Tag, h.ID, h.Pub, h.TS)))
	if mutate != nil {
		mutate(&h)
	}
	frame, _ := json.Marshal(h)
	if err := ws.Write(ctx, websocket.MessageText, frame); err != nil {
		t.Fatalf("hello write: %v", err)
	}
	return ws
}

// dialExpectReject performs a hello that must fail: returns the hub's
// error text (or a read failure, which also means rejection).
func dialExpectReject(t *testing.T, url string, mutate func(*Hello)) string {
	t.Helper()
	ws := newTestDevice(t, "group-reject", "pc").dial(t, url, mutate)
	defer ws.Close(websocket.StatusNormalClosure, "")
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	typ, data, err := ws.Read(ctx)
	if err != nil {
		return "connection closed: " + err.Error()
	}
	if typ != websocket.MessageText {
		return "non-text reply"
	}
	var m Error
	if json.Unmarshal(data, &m) != nil || m.Type != "error" {
		return "unexpected reply (not an error frame)"
	}
	return m.Error
}

// expectPresence reads frames until one presence frame matches want
// exactly (skipping pings/other frames). Order-insensitive: view()
// iterates a map, so the protocol makes no ordering promise.
func expectPresence(t *testing.T, ws *websocket.Conn, want ...string) {
	t.Helper()
	wantSet := map[string]bool{}
	for _, id := range want {
		wantSet[id] = true
	}
	start := time.Now()
	for i := 0; i < 20; i++ {
		ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
		typ, data, err := ws.Read(ctx)
		cancel()
		if err != nil {
			t.Fatalf("presence read (after %s): %v", time.Since(start), err)
		}
		if typ != websocket.MessageText {
			continue
		}
		var p Presence
		if json.Unmarshal(data, &p) != nil || p.Type != "presence" {
			continue
		}
		if len(p.Devices) != len(want) {
			continue
		}
		match := true
		for _, d := range p.Devices {
			if !wantSet[d.ID] {
				match = false
				break
			}
		}
		if match {
			return
		}
	}
	t.Fatalf("never saw presence with devices %v", want)
}

func hubURL(t *testing.T, srv *httptest.Server) string {
	t.Helper()
	return "ws" + strings.TrimPrefix(srv.URL, "http") + "/hub/ws"
}

func TestHelloAndPresence(t *testing.T) {
	hub := New()
	srv := httptest.NewServer(hub.Handler())
	defer srv.Close()
	url := hubURL(t, srv)

	d1 := newTestDevice(t, "group-key-one", "pc-one")
	d2 := newTestDevice(t, "group-key-one", "pc-two")
	d3 := newTestDevice(t, "group-key-two", "pc-three") // other group

	ws1 := d1.dial(t, url, nil)
	defer ws1.Close(websocket.StatusNormalClosure, "")
	expectPresence(t, ws1, d1.id) // initial presence: itself only

	ws2 := d2.dial(t, url, nil)
	defer ws2.Close(websocket.StatusNormalClosure, "")
	expectPresence(t, ws1, d1.id, d2.id) // d1 learns about d2
	expectPresence(t, ws2, d1.id, d2.id) // d2's initial view

	// A different group is invisible to d1/d2.
	ws3 := d3.dial(t, url, nil)
	defer ws3.Close(websocket.StatusNormalClosure, "")
	expectPresence(t, ws3, d3.id)

	// d2 drops: d1's final presence is itself only — which also
	// proves d3 (the other group) never leaked into d1's feed.
	ws2.Close(websocket.StatusAbnormalClosure, "gone")
	expectPresence(t, ws1, d1.id)
}

// expectSignal reads frames until one matches (type, from, side),
// skipping other frames; returns the raw body.
func expectSignal(t *testing.T, ws *websocket.Conn, typ, from, side string) Signal {
	t.Helper()
	for i := 0; i < 20; i++ {
		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		frametyp, data, err := ws.Read(ctx)
		cancel()
		if err != nil {
			t.Fatalf("signal read: %v", err)
		}
		if frametyp != websocket.MessageText {
			continue
		}
		var s Signal
		if json.Unmarshal(data, &s) != nil || s.Type != typ {
			continue
		}
		if s.From != from || s.Side != side {
			t.Fatalf("signal = from %q side %q, want %q %q", s.From, s.Side, from, side)
		}
		return s
	}
	t.Fatalf("never saw a %s frame from %s", typ, from)
	return Signal{}
}

// TestSignalRelay: signaling flows between same-group devices with
// the hub-authoritative From, and is invisible across groups.
func TestSignalRelay(t *testing.T) {
	h := New()
	srv := httptest.NewServer(h.Handler())
	defer srv.Close()
	url := hubURL(t, srv)

	d1 := newTestDevice(t, "group-sig", "pc-one")
	d2 := newTestDevice(t, "group-sig", "pc-two")
	ws1 := d1.dial(t, url, nil)
	defer ws1.Close(websocket.StatusNormalClosure, "")
	expectPresence(t, ws1, d1.id)
	ws2 := d2.dial(t, url, nil)
	defer ws2.Close(websocket.StatusNormalClosure, "")
	expectPresence(t, ws2, d1.id, d2.id)
	expectPresence(t, ws1, d1.id, d2.id)

	// d1 → d2: a sdp offer.
	body, _ := json.Marshal(map[string]any{"type": "offer", "sdp": "v=0"})
	frame, _ := json.Marshal(Signal{Type: "sdp", To: d2.id, Side: "offer", Body: body})
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	if err := ws1.Write(ctx, websocket.MessageText, frame); err != nil {
		t.Fatalf("write: %v", err)
	}
	cancel()

	sig := expectSignal(t, ws2, "sdp", d1.id, "offer")
	if string(sig.Body) == "" || string(sig.Body) == "null" {
		t.Fatal("empty relayed body")
	}

	// d2 → d1: the answer back.
	body, _ = json.Marshal(map[string]any{"type": "answer", "sdp": "v=0"})
	frame, _ = json.Marshal(Signal{Type: "sdp", To: d1.id, Side: "answer", Body: body})
	ctx, cancel = context.WithTimeout(context.Background(), 5*time.Second)
	if err := ws2.Write(ctx, websocket.MessageText, frame); err != nil {
		t.Fatalf("write: %v", err)
	}
	cancel()
	expectSignal(t, ws1, "sdp", d2.id, "answer")
}

// TestSignalSpoofAndOffline: From is overwritten by the hub, a bad
// target id is dropped, and an offline target answers with an error
// frame instead of silence.
func TestSignalSpoofAndOffline(t *testing.T) {
	h := New()
	srv := httptest.NewServer(h.Handler())
	defer srv.Close()
	url := hubURL(t, srv)

	d1 := newTestDevice(t, "group-spoof", "pc-one")
	d2 := newTestDevice(t, "group-spoof", "pc-two")
	ws1 := d1.dial(t, url, nil)
	defer ws1.Close(websocket.StatusNormalClosure, "")
	expectPresence(t, ws1, d1.id)
	ws2 := d2.dial(t, url, nil)
	defer ws2.Close(websocket.StatusNormalClosure, "")
	expectPresence(t, ws2, d1.id, d2.id)
	expectPresence(t, ws1, d1.id, d2.id)

	// Spoof: d1 claims the frame came from d2 — the hub rewrites From.
	body, _ := json.Marshal(map[string]any{"sdp": "x"})
	frame, _ := json.Marshal(Signal{Type: "sdp", To: d2.id, From: "fake", Side: "offer", Body: body})
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	if err := ws1.Write(ctx, websocket.MessageText, frame); err != nil {
		t.Fatalf("write: %v", err)
	}
	cancel()
	expectSignal(t, ws2, "sdp", d1.id, "offer") // From = real sender

	// Offline target: error frame back to the sender.
	frame, _ = json.Marshal(Signal{Type: "sdp", To: strings.Repeat("a", 64), Side: "offer", Body: body})
	ctx, cancel = context.WithTimeout(context.Background(), 5*time.Second)
	if err := ws1.Write(ctx, websocket.MessageText, frame); err != nil {
		t.Fatalf("write: %v", err)
	}
	cancel()
	var gotErr Error
	found := false
	for i := 0; i < 20 && !found; i++ {
		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		typ, data, err := ws1.Read(ctx)
		cancel()
		if err != nil {
			break
		}
		if typ == websocket.MessageText && json.Unmarshal(data, &gotErr) == nil && gotErr.Type == "error" {
			found = true
		}
	}
	if !found {
		t.Fatal("no error frame for offline signaling target")
	}

	// Bad target shape: dropped silently (no crash, connection stays).
	frame, _ = json.Marshal(Signal{Type: "sdp", To: "zz", Side: "offer", Body: body})
	ctx, cancel = context.WithTimeout(context.Background(), 5*time.Second)
	if err := ws1.Write(ctx, websocket.MessageText, frame); err != nil {
		t.Fatalf("write: %v", err)
	}
	cancel()
}

func TestHelloRejections(t *testing.T) {
	hub := New()
	srv := httptest.NewServer(hub.Handler())
	defer srv.Close()
	url := hubURL(t, srv)

	d := newTestDevice(t, "group-key-x", "pc")

	cases := []struct {
		name   string
		mutate func(*Hello)
	}{
		{"bad signature", func(h *Hello) {
			h.Sig = base64.StdEncoding.EncodeToString(make([]byte, 64))
		}},
		{"spoofed id", func(h *Hello) {
			h.ID = strings.Repeat("a", 64)
			// The signature still covers the real id, so verification
			// fails — claiming someone else's ID needs their key.
		}},
		{"id/key mismatch", func(h *Hello) {
			other := newTestDevice(t, d.tag, "other") // same tag text, fresh keys
			h.ID = other.id                           // pub stays d's → fingerprint mismatch
		}},
		{"bad tag", func(h *Hello) { h.Tag = "zz" }},
		{"stale ts", func(h *Hello) { h.TS = time.Now().Add(-2 * time.Hour).Unix() }},
		{"short pub", func(h *Hello) { h.Pub = base64.StdEncoding.EncodeToString(make([]byte, 31)) }},
		{"empty name", func(h *Hello) { h.Name = "   " }},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			errText := dialExpectReject(t, url, tc.mutate)
			if !strings.Contains(errText, "hello rejected") {
				t.Errorf("error text = %q, want a hello rejection reason", errText)
			}
		})
	}
}

func TestReplaceRejoin(t *testing.T) {
	hub := New()
	srv := httptest.NewServer(hub.Handler())
	defer srv.Close()
	url := hubURL(t, srv)

	d := newTestDevice(t, "group-rejoin", "pc")
	ws1 := d.dial(t, url, nil)
	defer ws1.Close(websocket.StatusNormalClosure, "")
	expectPresence(t, ws1, d.id)

	// Same device reconnects: the old connection is replaced, not
	// duplicated — the replacement's presence list is still [d].
	ws2 := d.dial(t, url, nil)
	defer ws2.Close(websocket.StatusNormalClosure, "")
	expectPresence(t, ws2, d.id)
}

func TestCleanName(t *testing.T) {
	cases := []struct {
		in, want string
		ok       bool
	}{
		{"  desk-pc  ", "desk-pc", true},
		{"a\x01b", "ab", true}, // control chars are stripped, not fatal
		{"\x01\x02", "", false},
		{"", "", false},
		{strings.Repeat("x", 97), "", false},
		{strings.Repeat("x", 96), strings.Repeat("x", 96), true},
	}
	for _, tc := range cases {
		got := tc.in
		err := cleanName(&got)
		if err != tc.ok || (tc.ok && got != tc.want) {
			t.Errorf("cleanName(%q) = %q ok=%v, want %q ok=%v", tc.in, got, err, tc.want, tc.ok)
		}
	}
}

func TestFingerprintB64(t *testing.T) {
	pub := make([]byte, 32)
	sum := sha256.Sum256(pub)
	want := hex.EncodeToString(sum[:])
	if got := FingerprintB64(pub); got != want {
		t.Errorf("FingerprintB64 = %s, want %s", got, want)
	}
}
