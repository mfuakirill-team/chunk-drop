package hub

import (
	"context"
	"crypto/ed25519"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"log"
	"net/http"
	"strings"
	"sync"
	"time"
	"unicode"
	"unicode/utf8"

	"github.com/coder/websocket"
)

// Limits — a personal hub serving a handful of devices, but every
// limit here is what stops one bad client from costing RAM or CPU.
const (
	// MaxDevicesPerGroup caps presence per group tag (a family mesh:
	// far below this; anything bigger is abuse or a bug).
	MaxDevicesPerGroup = 64
	// MaxReadLimit caps one inbound frame (hello/signaling are tiny).
	MaxReadLimit = 8 << 10
	// TSWindow is how far a hello's timestamp may drift — presence
	// replay is harmless (the same device re-announcing itself), so
	// the window only needs to cover clock skew between machines.
	TSWindow = 15 * time.Minute
	// PingEvery is the keepalive interval on both sides. Liveness is
	// carried entirely by ping/pong: a quiet-but-healthy connection
	// must never be killed by a read deadline (control frames do not
	// terminate Read — a deadline would murder presence-only
	// connections every few minutes). The pinger closes the socket
	// when pongs stop arriving; the read loop then unblocks.
	PingEvery = 25 * time.Second
	// PingTimeout bounds a single ping wait.
	PingTimeout = 10 * time.Second
)

// Server is the hub. RAM-only: presence is a nested map guarded by a
// mutex, nothing is persisted — a hub restart just drops everyone
// online (clients reconnect with backoff and the group is whole again
// within seconds).
type Server struct {
	// Logf renders one log line; defaults to [hub] log.Printf.
	Logf func(format string, args ...any)

	mu      sync.Mutex
	clients map[string]map[string]*conn // group tag → device ID → conn
}

// New builds an empty hub.
func New() *Server {
	return &Server{clients: map[string]map[string]*conn{}}
}

// Close drops every live connection (SIGTERM shutdown, and the
// "restart the hub" case in tests). Connections still waiting for
// their hello get dropped by their own deadline. Nothing to persist.
func (s *Server) Close() {
	s.mu.Lock()
	var all []*conn
	for _, g := range s.clients {
		for _, c := range g {
			all = append(all, c)
		}
	}
	s.clients = map[string]map[string]*conn{}
	s.mu.Unlock()
	for _, c := range all {
		_ = c.ws.CloseNow()
	}
}

func (s *Server) logf(format string, args ...any) {
	if s.Logf != nil {
		s.Logf(format, args...)
		return
	}
	log.Printf("[hub] "+format, args...)
}

// conn is one live client connection with its validated hello state.
type conn struct {
	ws   *websocket.Conn
	tag  string
	id   string
	name string
	pub  string
	// replaced is set when a newer connection takes this device's
	// presence slot: the loser stops broadcasting and lets its read
	// loop wind down silently.
	replaced bool
}

// Handler returns the hub's HTTP tree: /hub/ws (upgrade) + /healthz.
// nginx terminates TLS in front of it (DEPLOY-VPS.md); the hub itself
// is protocol-agnostic.
func (s *Server) Handler() http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("GET /hub/ws", s.handleWS)
	mux.HandleFunc("GET /healthz", func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte("ok"))
	})
	return mux
}

// handleWS upgrades, reads exactly one hello, validates it, then
// serves presence until the connection dies.
func (s *Server) handleWS(w http.ResponseWriter, r *http.Request) {
	ws, err := websocket.Accept(w, r, &websocket.AcceptOptions{
		// The dashboard never talks to the hub directly (the engine
		// connects server-side), so origin echo is safe and handy for
		// tests and curl-style debugging tools.
		OriginPatterns: []string{"*"},
	})
	if err != nil {
		return
	}
	ws.SetReadLimit(MaxReadLimit)
	defer ws.CloseNow()

	// Dead-connection detection: ping the peer regularly and close
	// the socket when pongs stop; the read loop (no idle deadline —
	// see PingEvery) unblocks on the close and runs leave() below.
	pingCtx, cancelPing := context.WithCancel(r.Context())
	defer cancelPing()
	go func() {
		t := time.NewTicker(PingEvery)
		defer t.Stop()
		for {
			select {
			case <-pingCtx.Done():
				return
			case <-t.C:
				pctx, cancel := context.WithTimeout(pingCtx, PingTimeout)
				err := ws.Ping(pctx)
				cancel()
				if err != nil {
					_ = ws.CloseNow()
					return
				}
			}
		}
	}()

	c, ok := s.readHello(ws)
	if !ok {
		cancelPing()
		return
	}
	s.logf("join: %s (%s) in group %s", c.id, c.name, shortTag(c.tag))
	s.join(c)

	// Read loop: presence needs no client input; any other frame type
	// is signaling/relay in later steps — unknown types are ignored
	// (forward compatibility: older hub + newer engine). No read
	// deadline here: liveness is the ping goroutine's job (control
	// frames never terminate Read, so a deadline would kill quiet
	// but healthy connections).
	for {
		typ, data, err := ws.Read(r.Context())
		if err != nil {
			s.leave(c)
			return
		}
		if typ == websocket.MessageText {
			s.handleFrame(c, data)
		}
	}
}

// readHello waits for and validates the first frame. On failure it
// sends {"type":"error",...} and closes.
func (s *Server) readHello(ws *websocket.Conn) (*conn, bool) {
	// The hello must arrive promptly: no idle connection squatting.
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	typ, data, err := ws.Read(ctx)
	if err != nil {
		return nil, false
	}
	if typ != websocket.MessageText {
		s.reject(ws, "hello must be a text frame")
		return nil, false
	}
	var h Hello
	if err := json.Unmarshal(data, &h); err != nil {
		s.reject(ws, "bad hello: not valid JSON")
		return nil, false
	}
	if h.Type != "hello" {
		s.reject(ws, "first message must be hello")
		return nil, false
	}
	if err := validateHello(&h); err != nil {
		s.reject(ws, "hello rejected: "+err.Error())
		return nil, false
	}
	return &conn{ws: ws, tag: h.Tag, id: h.ID, name: h.Name, pub: h.Pub}, true
}

// validateHello checks shapes and both proofs: group-key knowledge
// (the tag — preimage of sha256(group_key)) and device-key possession
// (the signature binding tag+id+pub+ts). It also binds the ID to the
// key, which is what makes ID spoofing impossible.
func validateHello(h *Hello) error {
	if !isHex(h.Tag) || len(h.Tag) != 64 {
		return fmt.Errorf("bad group tag")
	}
	pub, err := base64.StdEncoding.DecodeString(h.Pub)
	if err != nil || len(pub) != ed25519.PublicKeySize {
		return fmt.Errorf("bad public key")
	}
	if !isHex(h.ID) || len(h.ID) != 64 {
		return fmt.Errorf("bad device id")
	}
	if fp := FingerprintB64(pub); fp != h.ID {
		return fmt.Errorf("device id does not match the key")
	}
	if !cleanName(&h.Name) {
		return fmt.Errorf("bad name")
	}
	if d := time.Since(time.Unix(h.TS, 0)); d > TSWindow || d < -TSWindow {
		return fmt.Errorf("stale hello timestamp")
	}
	sig, err := base64.StdEncoding.DecodeString(h.Sig)
	if err != nil || len(sig) != ed25519.SignatureSize {
		return fmt.Errorf("bad signature")
	}
	if !ed25519.Verify(ed25519.PublicKey(pub), HelloCanonical(h.Tag, h.ID, h.Pub, h.TS), sig) {
		return fmt.Errorf("bad signature")
	}
	return nil
}

// handleFrame dispatches one client frame. Step ① knows presence
// only; signaling (sdp/ice) and relay control arrive in later steps —
// unknown frames are dropped, never fatal.
func (s *Server) handleFrame(c *conn, data []byte) {
	var m struct {
		Type string `json:"type"`
	}
	if err := json.Unmarshal(data, &m); err != nil {
		return
	}
	s.logf("frame %q from %s: not handled yet", m.Type, c.id)
}

// join inserts/updates the device and broadcasts the new presence to
// the whole group. A re-connecting device replaces its old connection
// (one device = one presence entry). Replacement of a stale entry
// skips the cap check: MaxDevicesPerGroup counts real devices, and a
// re-join should never fail.
func (s *Server) join(c *conn) {
	s.mu.Lock()
	g := s.clients[c.tag]
	if g == nil {
		g = map[string]*conn{}
		s.clients[c.tag] = g
	}
	if old, ok := g[c.id]; ok && old != c {
		old.replaced = true
		// CloseNow, not Close: a graceful close waits up to 5 s for
		// the old client's echo, and that client is gone (it is the
		// one redialing as c). Under the hub lock that stall would
		// freeze every join in the group.
		_ = old.ws.CloseNow()
	} else if len(g) >= MaxDevicesPerGroup {
		s.mu.Unlock()
		s.reject(c.ws, "device limit reached for this group")
		return
	}
	g[c.id] = c
	// Copy targets + peers under the lock: the map must never be
	// iterated after unlock (a concurrent join/leave would mutate it).
	peers, targets := view(g)
	s.mu.Unlock()
	broadcast(targets, peers)
}

// leave removes the device (if it still owns the slot) and
// broadcasts. A replaced connection leaves without a broadcast — the
// new one already announced itself.
func (s *Server) leave(c *conn) {
	s.mu.Lock()
	if c.replaced {
		s.mu.Unlock()
		return
	}
	g := s.clients[c.tag]
	if g == nil || g[c.id] != c {
		s.mu.Unlock()
		return
	}
	delete(g, c.id)
	if len(g) == 0 {
		delete(s.clients, c.tag)
	}
	peers, targets := view(g)
	s.mu.Unlock()
	broadcast(targets, peers)
}

// view copies a group's peers and their connections under the lock.
func view(g map[string]*conn) ([]Peer, []*conn) {
	peers := make([]Peer, 0, len(g))
	targets := make([]*conn, 0, len(g))
	for _, c := range g {
		if c.replaced {
			continue
		}
		peers = append(peers, Peer{ID: c.id, Name: c.name, Pub: c.pub})
		targets = append(targets, c)
	}
	return peers, targets
}

// broadcast pushes one presence frame to every group member. Write
// failures are ignored — the failed peer's read loop notices and
// triggers its own leave.
func broadcast(targets []*conn, peers []Peer) {
	if peers == nil {
		peers = []Peer{}
	}
	data, _ := json.Marshal(Presence{Type: "presence", Devices: peers})
	for _, c := range targets {
		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		err := c.ws.Write(ctx, websocket.MessageText, data)
		cancel()
		_ = err
	}
}

// reject sends an error frame and closes without a handshake wait
// (used before hello success and for cap violations) — the rejected
// client may not be reading, and a graceful close would stall this
// connection's goroutine for its full echo timeout.
func (s *Server) reject(ws *websocket.Conn, msg string) {
	data, _ := json.Marshal(Error{Type: "error", Error: msg})
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	_ = ws.Write(ctx, websocket.MessageText, data)
	cancel()
	_ = ws.CloseNow()
	s.logf("reject: %s", msg)
}

// cleanName sanitizes the display name: trims, strips control/
// invisible characters, caps the length (a name is shown on other
// people's dashboards). Empty after sanitizing → rejected.
func cleanName(name *string) bool {
	n := strings.Map(func(r rune) rune {
		if r < 0x20 || r == 0x7f || unicode.Is(unicode.Cf, r) {
			return -1
		}
		return r
	}, strings.TrimSpace(*name))
	if n == "" || len(n) > 96 || !utf8.ValidString(n) {
		return false
	}
	*name = n
	return true
}

// isHex reports whether s is lowercase hex of even length.
func isHex(s string) bool {
	if len(s) == 0 || len(s)%2 != 0 {
		return false
	}
	for _, r := range s {
		if !('0' <= r && r <= '9' || 'a' <= r && r <= 'f') {
			return false
		}
	}
	return true
}

// FingerprintB64 is the device ID derived from a public key:
// hex(SHA-256(pub)). Mirrors internal/device.Fingerprint for the hub,
// which cannot import the engine's identity package (separate
// binaries stay independent).
func FingerprintB64(pub []byte) string {
	sum := sha256.Sum256(pub)
	return hex.EncodeToString(sum[:])
}

func shortTag(tag string) string {
	if len(tag) < 8 {
		return tag
	}
	return tag[:8] + "…"
}
