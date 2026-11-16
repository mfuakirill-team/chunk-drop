package tunnel

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strings"
	"sync"
	"testing"
	"time"
)

// relay is the fake hub signaling service: Send frames are routed by
// target ID, OnSignal callbacks fire on the receiving manager.
type relay struct {
	mu      sync.Mutex
	mgrs    map[string]*Manager
	offline map[string]bool
}

func newRelay() *relay {
	return &relay{mgrs: map[string]*Manager{}, offline: map[string]bool{}}
}

func (r *relay) register(id string, m *Manager) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.mgrs[id] = m
}

func (r *relay) setOffline(id string, off bool) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.offline[id] = off
}

// send implements SendFunc for a manager with self=id.
func (r *relay) send(self string) SendFunc {
	return func(to, side, typ string, body any) error {
		r.mu.Lock()
		m := r.mgrs[to]
		off := r.offline[to] || to == self
		r.mu.Unlock()
		if m == nil || off {
			return fmt.Errorf("relay: %s offline", to)
		}
		raw, err := json.Marshal(body)
		if err != nil {
			return err
		}
		go m.SignalIn(self, typ, side, raw)
		return nil
	}
}

// newManager builds a manager wired to the relay, registering it for
// signal delivery both ways.
func newManager(t *testing.T, r *relay, id, stun string) *Manager {
	t.Helper()
	m := New(id, stun, r.send(id))
	m.Logf = t.Logf
	r.register(id, m)
	t.Cleanup(m.Close)
	return m
}

// pair bridges a→b and b→a and returns both managers.
func pair(t *testing.T) (a, b *Manager, r *relay) {
	t.Helper()
	r = newRelay()
	a = newManager(t, r, "1111", "")
	b = newManager(t, r, "2222", "")
	return a, b, r
}

// TestTunnelHTTPEndToEnd pushes a real HTTP request/response through
// two managers: chunked response bodies, request bodies, headers —
// the whole net/http surface the mesh protocol uses.
func TestTunnelHTTPEndToEnd(t *testing.T) {
	a, b, _ := pair(t)

	// b serves like the mesh handler: reads a body, answers big.
	payload := make([]byte, 4<<20) // 4 MiB: many SCTP messages each way
	if _, err := rand.Read(payload); err != nil {
		t.Fatal(err)
	}
	sum := sha256.Sum256(payload)
	wantHash := hex.EncodeToString(sum[:])

	ln := b.Listener()
	srv := &http.Server{Handler: http.HandlerFunc(func(w http.ResponseWriter, req *http.Request) {
		body, err := io.ReadAll(req.Body)
		if err != nil {
			t.Errorf("server read body: %v", err)
			return
		}
		if string(body) != "ping-body" {
			t.Errorf("server got body %q", body)
		}
		if req.Header.Get("X-Test") != "hi" {
			t.Errorf("server missing header")
		}
		w.Header().Set("X-Echo", "ok")
		w.Header().Set("Content-Length", fmt.Sprintf("%d", len(payload)))
		w.Write(payload)
	})}
	go srv.Serve(ln)
	t.Cleanup(func() { _ = srv.Close() })

	// a dials b over the tunnel and does one request (the http.Client
	// dials internally — this mirrors the mesh's real call path).
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	req, err := http.NewRequest("POST", "http://tunnel-2222:9/test", strings.NewReader("ping-body"))
	if err != nil {
		t.Fatal(err)
	}
	req.Header.Set("X-Test", "hi")
	req = req.WithContext(ctx)
	resp, err := a.HTTPClient("2222").Do(req)
	if err != nil {
		t.Fatalf("do: %v", err)
	}
	defer resp.Body.Close()
	if resp.Header.Get("X-Echo") != "ok" {
		t.Fatalf("missing echo header: %v", resp.Header)
	}
	got, err := io.ReadAll(resp.Body)
	if err != nil {
		t.Fatalf("read body: %v", err)
	}
	if len(got) != len(payload) {
		t.Fatalf("body %d bytes, want %d", len(got), len(payload))
	}
	gsum := sha256.Sum256(got)
	if hex.EncodeToString(gsum[:]) != wantHash {
		t.Fatal("payload hash mismatch over the tunnel")
	}
}

// TestTunnelConcurrentDials exercises renegotiation: many channels
// created at once on a fresh PC must all open (serialized SDP
// exchanges), and each must carry data independently.
func TestTunnelConcurrentDials(t *testing.T) {
	a, b, _ := pair(t)

	ln := b.Listener()
	srv := &http.Server{Handler: http.HandlerFunc(func(w http.ResponseWriter, req *http.Request) {
		fmt.Fprintf(w, "peer=%s", req.Header.Get("X-Client"))
	})}
	go srv.Serve(ln)
	t.Cleanup(func() { _ = srv.Close() })

	const n = 5
	var wg sync.WaitGroup
	for i := range n {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			req, err := http.NewRequest("GET", "http://tunnel-2222:9/x", nil)
			if err != nil {
				t.Errorf("req: %v", err)
				return
			}
			req.Header.Set("X-Client", fmt.Sprintf("c%d", i))
			resp, err := a.HTTPClient("2222").Do(req)
			if err != nil {
				t.Errorf("do c%d: %v", i, err)
				return
			}
			defer resp.Body.Close()
			body, _ := io.ReadAll(resp.Body)
			if string(body) != fmt.Sprintf("peer=c%d", i) {
				t.Errorf("c%d got %q", i, body)
			}
		}(i)
	}
	wg.Wait()
}

// TestSignalRouting checks the side-tag dispatch: offers create
// answer-side PCs, answers are delivered to offer-side PCs.
func TestSignalRouting(t *testing.T) {
	a, b, _ := pair(t)

	// a offers by dialing; b must end up with an answer-side PC and
	// serve the request; a must have its offer-side PC connected.
	srv := &http.Server{Handler: http.HandlerFunc(func(w http.ResponseWriter, req *http.Request) {
		w.Write([]byte("ok"))
	})}
	go srv.Serve(b.Listener())
	t.Cleanup(func() { _ = srv.Close() })

	req, err := http.NewRequest("GET", "http://tunnel-2222:9/x", nil)
	if err != nil {
		t.Fatal(err)
	}
	resp, err := a.HTTPClient("2222").Do(req)
	if err != nil {
		t.Fatalf("do: %v", err)
	}
	resp.Body.Close()

	a.mu.Lock()
	outOK := a.out["2222"] != nil
	a.mu.Unlock()
	b.mu.Lock()
	inOK := b.in["1111"] != nil
	b.mu.Unlock()
	if !outOK || !inOK {
		t.Fatalf("routing: a.out=%v b.in=%v", outOK, inOK)
	}
}

// TestOfflineTarget checks that signaling to an offline peer fails
// fast (relay error) rather than hanging the dialer.
func TestOfflineTarget(t *testing.T) {
	a, _, r := pair(t)
	r.setOffline("2222", true)
	ctx, cancel := context.WithTimeout(context.Background(), 8*time.Second)
	defer cancel()
	tr := a.HTTPClient("2222").Transport.(*http.Transport)
	// dialTimeout is 25s; ctx caps the wait. Must fail, not hang.
	if _, err := tr.DialContext(ctx, "tcp", HostFor("2222")); err == nil {
		t.Fatal("dial to offline peer unexpectedly succeeded")
	}
}

// TestHostForRoundTrip checks the fake-address encoding.
func TestHostForRoundTrip(t *testing.T) {
	id := strings.Repeat("ab", 32)
	if got, err := PeerFromHost(HostFor(id)); err != nil || got != id {
		t.Fatalf("roundtrip: %q %v", got, err)
	}
	if _, err := PeerFromHost("127.0.0.1:8080"); err == nil {
		t.Fatal("plain host accepted")
	}
}
