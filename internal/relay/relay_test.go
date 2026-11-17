package relay

import (
	"bytes"
	"context"
	"crypto/rand"
	"fmt"
	"io"
	"net"
	"net/http"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"chunkdrop/internal/hub"
)

// fakeHub routes Send frames to the receiving manager's Deliver —
// the real hub forwards synchronously on the sender's read loop and
// asynchronously overall, but order per direction is preserved on
// the wire either way; direct synchronous calls model that exactly.
type fakeHub struct {
	mu      sync.Mutex
	mgrs    map[string]*Manager
	offline map[string]bool
}

func newFakeHub() *fakeHub {
	return &fakeHub{mgrs: map[string]*Manager{}, offline: map[string]bool{}}
}

func (h *fakeHub) register(id string, m *Manager) {
	h.mu.Lock()
	defer h.mu.Unlock()
	h.mgrs[id] = m
}

func (h *fakeHub) setOffline(id string, off bool) {
	h.mu.Lock()
	defer h.mu.Unlock()
	h.offline[id] = off
}

func (h *fakeHub) send(self string) func(to string, f hub.Relay) error {
	return func(to string, f hub.Relay) error {
		h.mu.Lock()
		m := h.mgrs[to]
		off := h.offline[to] || to == self
		h.mu.Unlock()
		if m == nil || off {
			return fmt.Errorf("relay: %s offline", to)
		}
		m.Deliver(self, f)
		return nil
	}
}

func newManager(t *testing.T, h *fakeHub, id string) *Manager {
	t.Helper()
	m := New(id, h.send(id))
	m.Logf = t.Logf
	h.register(id, m)
	t.Cleanup(m.Close)
	return m
}

// TestDirParity pins the namespace split: the two directions of a
// pair never share a parity, and each end computes the same value.
func TestDirParity(t *testing.T) {
	a, b := "aaaa", "bbbb"
	if dirParity(a, b) != 0 || dirParity(b, a) != 1 {
		t.Errorf("parity split wrong: %d/%d", dirParity(a, b), dirParity(b, a))
	}
	if dirParity(a, b) == dirParity(a, b) && dirParity(a, b) == dirParity(b, a) {
		t.Error("both directions share a namespace — simultaneous dials would collide")
	}
}

// TestHTTPOverRelay is the end-to-end contract: a real http.Server
// behind one manager's listener and a real http.Client dialing
// through the other's Dial — large bodies both ways, several
// concurrent streams, echo round-trips byte-exact.
func TestHTTPOverRelay(t *testing.T) {
	h := newFakeHub()
	// "1111" < "2222": the client dials on even sids, the server on
	// odd — both directions active at once by construction.
	a := newManager(t, h, strings.Repeat("a", 64))
	b := newManager(t, h, strings.Repeat("b", 64))

	// Server side: echo POST bodies, serve a 2 MiB payload on GET.
	payload := make([]byte, 2<<20)
	if _, err := rand.Read(payload); err != nil {
		t.Fatal(err)
	}
	srv := &http.Server{Handler: http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.Method {
		case http.MethodGet:
			w.Header().Set("Content-Length", fmt.Sprint(len(payload)))
			_, _ = w.Write(payload)
		case http.MethodPost:
			got, err := io.ReadAll(r.Body)
			if err != nil {
				http.Error(w, err.Error(), http.StatusInternalServerError)
				return
			}
			_, _ = w.Write(got)
		default:
			http.Error(w, "no", http.StatusMethodNotAllowed)
		}
	})}
	go srv.Serve(b.Listener())
	t.Cleanup(func() { _ = srv.Close() })

	client := &http.Client{Transport: &http.Transport{
		DialContext: func(ctx context.Context, network, addr string) (net.Conn, error) {
			return a.Dial(ctx, b.SelfID)
		},
		MaxConnsPerHost: 2,
	}}

	// GET: 2 MiB through the relay, byte-exact (exercises window +
	// acks across ~70 frames).
	resp, err := client.Get("http://relay/")
	if err != nil {
		t.Fatalf("GET: %v", err)
	}
	got, err := io.ReadAll(resp.Body)
	resp.Body.Close()
	if err != nil {
		t.Fatalf("GET body: %v", err)
	}
	if !bytes.Equal(got, payload) {
		t.Fatalf("GET payload mismatch: got %d bytes", len(got))
	}

	// POST: request window (the body flows client→server, the ack
	// direction server→client).
	body := make([]byte, 3<<20)
	if _, err := rand.Read(body); err != nil {
		t.Fatal(err)
	}
	resp, err = client.Post("http://relay/", "application/octet-stream", bytes.NewReader(body))
	if err != nil {
		t.Fatalf("POST: %v", err)
	}
	got, err = io.ReadAll(resp.Body)
	resp.Body.Close()
	if err != nil {
		t.Fatalf("POST body: %v", err)
	}
	if !bytes.Equal(got, body) {
		t.Fatalf("POST echo mismatch: got %d bytes", len(got))
	}

	// Concurrency: four GETs at once (over MaxConnsPerHost — the
	// transport will serialize two at a time, fine) must all arrive
	// intact; interleaved frames must not cross streams.
	var wg sync.WaitGroup
	errs := make([]error, 4)
	for i := range errs {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			resp, err := client.Get("http://relay/")
			if err != nil {
				errs[i] = err
				return
			}
			got, err := io.ReadAll(resp.Body)
			resp.Body.Close()
			if err != nil {
				errs[i] = err
				return
			}
			if !bytes.Equal(got, payload) {
				errs[i] = fmt.Errorf("concurrent GET %d: payload mismatch (%d bytes)", i, len(got))
			}
		}(i)
	}
	wg.Wait()
	for i, err := range errs {
		if err != nil {
			t.Errorf("concurrent GET %d: %v", i, err)
		}
	}
}

// TestRelayBothDirectionsDial: each side dials the other at the same
// time — the disjoint sid namespaces must keep the two streams
// independent.
func TestRelayBothDirectionsDial(t *testing.T) {
	h := newFakeHub()
	a := newManager(t, h, strings.Repeat("a", 64))
	b := newManager(t, h, strings.Repeat("b", 64))

	echo := func(name string, m, peer *Manager) {
		conn, err := m.Dial(context.Background(), peer.SelfID)
		if err != nil {
			t.Errorf("%s: dial: %v", name, err)
			return
		}
		defer conn.Close()
		msg := []byte("hello from " + name + " " + strings.Repeat("x", 100<<10))
		if _, err := conn.Write(msg); err != nil {
			t.Errorf("%s: write: %v", name, err)
			return
		}
		got := make([]byte, len(msg))
		if _, err := io.ReadFull(conn, got); err != nil {
			t.Errorf("%s: read: %v", name, err)
			return
		}
		if !bytes.Equal(got, msg) {
			t.Errorf("%s: echo mismatch", name)
		}
	}
	// The peer side mirrors the bytes back (the listener here is a
	// raw accept loop, not http.Serve).
	go func() {
		for {
			conn, err := b.Listener().Accept()
			if err != nil {
				return
			}
			go func(c interface {
				io.ReadWriteCloser
			}) {
				_, _ = io.Copy(c, c)
				_ = c.Close()
			}(conn)
		}
	}()
	go func() {
		for {
			conn, err := a.Listener().Accept()
			if err != nil {
				return
			}
			go func(c interface {
				io.ReadWriteCloser
			}) {
				_, _ = io.Copy(c, c)
				_ = c.Close()
			}(conn)
		}
	}()

	var wg sync.WaitGroup
	wg.Add(2)
	go func() { defer wg.Done(); echo("a→b", a, b) }()
	go func() { defer wg.Done(); echo("b→a", b, a) }()
	wg.Wait()
}

// TestRelayBackpressure is the RAM-bound proof: a writer pumping
// megabytes at a stream nobody reads must stall at the window (plus
// one in-flight frame), not buffer the whole transfer.
func TestRelayBackpressure(t *testing.T) {
	h := newFakeHub()
	a := newManager(t, h, strings.Repeat("a", 64))
	b := newManager(t, h, strings.Repeat("b", 64))

	conn, err := a.Dial(context.Background(), b.SelfID)
	if err != nil {
		t.Fatal(err)
	}
	defer conn.Close()

	// Server side accepts but never reads — the writer must stall at
	// the window.
	stop := make(chan struct{})
	defer close(stop)
	go func() {
		c, err := b.Listener().Accept()
		if err != nil {
			return
		}
		<-stop
		_ = c.Close()
	}()

	chunk := make([]byte, 64<<10)
	var total atomic.Int64
	go func() {
		for {
			n, err := conn.Write(chunk)
			total.Add(int64(n))
			if err != nil {
				return
			}
		}
	}()

	deadline := time.After(3 * time.Second)
	sampled := false
	for !sampled {
		select {
		case <-deadline:
			t.Fatal("writer never stalled at the window")
		case <-time.After(500 * time.Millisecond):
			// The writer has been running for a while: it may not have
			// sent more than the window plus one in-flight frame
			// (frames queued in the peer's socket buffer before the
			// hub-side backpressure bites).
			sampled = true
		}
	}
	if total.Load() > int64(window+3*chunkSize) {
		t.Fatalf("writer buffered %d bytes — window not enforced", total.Load())
	}
	if total.Load() < int64(window) {
		t.Fatalf("writer stalled too early at %d bytes", total.Load())
	}
}

// TestRelayPeerOffline: dialing/writing to an offline peer errors
// promptly (the hub's error frame routes back through OnRelay), and
// the aborted stream does not wedge later dials.
func TestRelayPeerOffline(t *testing.T) {
	h := newFakeHub()
	a := newManager(t, h, strings.Repeat("a", 64))
	ghost := strings.Repeat("f", 64) // never registered

	conn, err := a.Dial(context.Background(), ghost)
	if err != nil {
		t.Fatal(err)
	}
	defer conn.Close()
	if _, err := conn.Write([]byte("ping")); err == nil {
		t.Fatal("write to offline peer succeeded")
	}

	// Recovery: a later dial works fine once a real peer exists.
	b := newManager(t, h, strings.Repeat("b", 64))
	conn2, err := a.Dial(context.Background(), b.SelfID)
	if err != nil {
		t.Fatalf("dial after failure: %v", err)
	}
	defer conn2.Close()
	if _, err := conn2.Write([]byte("ping")); err != nil {
		t.Fatalf("write after failure: %v", err)
	}
}

// TestRelayManagerClose: in-flight streams unblock with errors when
// the manager closes (mesh rekey/shutdown must not hang).
func TestRelayManagerClose(t *testing.T) {
	h := newFakeHub()
	a := newManager(t, h, strings.Repeat("a", 64))
	b := newManager(t, h, strings.Repeat("b", 64))

	conn, err := a.Dial(context.Background(), b.SelfID)
	if err != nil {
		t.Fatal(err)
	}
	go func() {
		c, err := b.Listener().Accept()
		if err != nil {
			return
		}
		// Held, never read: no acks flow, the writer must stall in
		// the window before close.
		_ = c
	}()

	// Fill past the window with no reader draining on b — the Write
	// blocks in the window, then a.Close() must unblock it with an
	// error (mesh rekey/shutdown must not hang).
	go func() {
		time.Sleep(300 * time.Millisecond)
		a.Close()
	}()
	errCh := make(chan error, 1)
	go func() {
		_, err := conn.Write(make([]byte, 8<<20))
		errCh <- err
	}()
	select {
	case err := <-errCh:
		if err == nil {
			t.Fatal("write succeeded through a closed manager")
		}
	case <-time.After(5 * time.Second):
		t.Fatal("write still blocked 5 s after manager close")
	}
}
