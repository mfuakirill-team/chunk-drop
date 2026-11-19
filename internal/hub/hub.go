package hub

import (
	"context"
	"crypto/ed25519"
	"crypto/sha256"
	"crypto/tls"
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

	"chunkdrop/internal/itls"

	"github.com/coder/websocket"
)

// Limits — a personal hub serving a handful of devices, but every
// limit here is what stops one bad client from costing RAM or CPU.
const (
	// MaxDevicesPerGroup caps presence per group tag (a family mesh:
	// far below this; anything bigger is abuse or a bug).
	MaxDevicesPerGroup = 64
	// MaxReadLimit caps one inbound frame. Hellos are tiny; SDP
	// descriptions with gathered candidates can reach a few KiB.
	MaxReadLimit = 64 << 10
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

	// ITLS, when set, enables the inner-TLS channel (M3): a client
	// that sends the chunkdrop-itls1 marker first gets its plaintext
	// ws connection wrapped in TLS (server side, this certificate) and
	// continues the same protocol as NDJSON over the inner connection.
	// nil (default) = feature off; marker clients are rejected.
	ITLS *tls.Config

	mu      sync.Mutex
	clients map[string]*group // group tag → group
}

// group is one group tag's live connections plus that group's
// broadcast lock. The lock serializes presence broadcasts so the wire
// order on every member's connection matches the event order (join,
// leave, replace): two handler goroutines broadcasting concurrently
// would otherwise only race a connection's write mutex — the newer
// list could land BEFORE the older one, and the client's "last frame
// wins" rule would keep the stale list forever (seen as a flaky CI
// failure in TestSendSignalAndDispatch: a device stuck on a 1-device
// presence list that arrived after the 2-device one).
type group struct {
	bmu   sync.Mutex
	conns map[string]*conn
}

// New builds an empty hub.
func New() *Server {
	return &Server{clients: map[string]*group{}}
}

// Close drops every live connection (SIGTERM shutdown, and the
// "restart the hub" case in tests). Connections still waiting for
// their hello get dropped by their own deadline. Nothing to persist.
func (s *Server) Close() {
	s.mu.Lock()
	var all []*conn
	for _, g := range s.clients {
		for _, c := range g.conns {
			all = append(all, c)
		}
	}
	s.clients = map[string]*group{}
	s.mu.Unlock()
	for _, c := range all {
		c.lnk.kill()
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
	lnk  link
	tag  string
	id   string
	name string
	pub  string
	// replaced is set when a newer connection takes this device's
	// presence slot: the loser stops broadcasting and lets its read
	// loop wind down silently.
	replaced bool
}

// link is the transport abstraction behind one connection: either a
// plain websocket (legacy channel: text frames) or NDJSON over an
// inner-TLS connection (M3 channel). The hub protocol is identical on
// both — same JSON frames, different framing.
type link interface {
	// send writes one JSON frame; serialized by the implementation
	// (websocket and tls.Conn both allow one writer at a time).
	send(data []byte) error
	// read returns the next JSON frame. Binary/control frames are
	// consumed internally (keepalive lives in the transports).
	read(ctx context.Context) ([]byte, error)
	// kill hard-closes the transport without a close handshake (used
	// for replaced connections and rejections).
	kill()
}

// wsLink is the legacy channel: one websocket connection, frames as
// text messages. Write serialization lives here (one writer per
// socket); read tolerates binary frames after hello (ignored, as
// always).
type wsLink struct {
	ws  *websocket.Conn
	wmu sync.Mutex

	// peeked carries the first frame when the handler already
	// consumed it sniffing for the inner-TLS marker.
	peeked  []byte
	hasPeek bool
}

func (l *wsLink) send(data []byte) error {
	l.wmu.Lock()
	defer l.wmu.Unlock()
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	return l.ws.Write(ctx, websocket.MessageText, data)
}

func (l *wsLink) read(ctx context.Context) ([]byte, error) {
	if l.hasPeek {
		l.hasPeek = false
		d := l.peeked
		l.peeked = nil
		return d, nil
	}
	for {
		typ, data, err := l.ws.Read(ctx)
		if err != nil {
			return nil, err
		}
		if typ == websocket.MessageText {
			return data, nil
		}
		// Binary after hello: never part of the protocol — ignored
		// (forward compatibility), exactly like the old read loop.
	}
}

func (l *wsLink) kill() { _ = l.ws.CloseNow() }

// ndjsonLink is the inner-TLS channel (M3): the hub protocol's JSON
// frames as newline-delimited lines over a tls.Conn that itself rides
// inside a websocket (internal/itls).
type ndjsonLink struct {
	tlsConn *tls.Conn
	sc      *itls.FrameScanner
	wmu     sync.Mutex // tls.Conn: one writer at a time
}

func (l *ndjsonLink) send(data []byte) error {
	l.wmu.Lock()
	defer l.wmu.Unlock()
	// tls.Conn.Write goes through the itls.Transport, whose ws Write
	// carries its own timeout — a stalled peer cannot wedge this
	// forever.
	return itls.WriteFrame(l.tlsConn, data)
}

func (l *ndjsonLink) read(ctx context.Context) ([]byte, error) {
	// The scanner read has no context: cancel unblocks via a hard
	// Close (the deadline no-op makes ctx-driven conn deadlines
	// useless here — see itls.Transport).
	done := make(chan struct{})
	defer close(done)
	go func() {
		select {
		case <-ctx.Done():
			_ = l.tlsConn.Close()
		case <-done:
		}
	}()
	return l.sc.Next()
}

func (l *ndjsonLink) kill() { _ = l.tlsConn.Close() }

// write sends one JSON frame through the connection's link.
func (c *conn) write(data []byte) error { return c.lnk.send(data) }

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

// handleWS upgrades, sniffs the first frame for the inner-TLS marker
// (M3), reads exactly one hello, validates it, then serves presence
// until the connection dies.
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
	// In inner-TLS mode this pings the OUTER ws as before; pongs are
	// answered inside the transport's ws.Read.
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

	// Sniff the first frame: a binary chunkdrop-itls1 marker switches
	// the connection into inner-TLS mode; anything else is the legacy
	// channel, starting with the hello itself.
	sniffCtx, cancelSniff := context.WithTimeout(r.Context(), 10*time.Second)
	typ, first, err := ws.Read(sniffCtx)
	cancelSniff()
	if err != nil {
		return
	}
	var lnk link
	switch {
	case itls.IsMarker(typ, first):
		lnk = s.itlsLink(ws, r.Context())
		if lnk == nil {
			return
		}
	case typ == websocket.MessageBinary:
		// Some other binary payload as the first frame: invalid in
		// both channels.
		s.reject(&wsLink{ws: ws}, "hello must be a text frame")
		return
	default:
		lnk = &wsLink{ws: ws, peeked: first, hasPeek: true}
	}

	c, ok := s.readHello(lnk)
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
		data, err := lnk.read(r.Context())
		if err != nil {
			s.logf("leave: %s (%s) read error: %v", c.id, c.name, err)
			s.leave(c)
			return
		}
		s.handleFrame(c, data)
	}
}

// itlsLink upgrades a marker-sending ws connection into the M3 inner
// TLS channel and returns its NDJSON link (nil after a logged
// failure; the outer ws closes via the handler's defer).
func (s *Server) itlsLink(ws *websocket.Conn, ctx context.Context) link {
	if s.ITLS == nil {
		// Feature off: answer on the plain ws the client is still on.
		s.reject(&wsLink{ws: ws}, "inner TLS disabled on this hub")
		return nil
	}
	tr := itls.NewTransport(ctx, ws)
	tlsConn, err := itls.Server(tr, s.ITLS)
	if err != nil {
		s.logf("inner TLS handshake failed: %v", err)
		return nil
	}
	return &ndjsonLink{tlsConn: tlsConn, sc: itls.NewFrameScanner(tlsConn, MaxReadLimit)}
}

// readHello waits for and validates the first frame. On failure it
// sends {"type":"error",...} and closes.
func (s *Server) readHello(lnk link) (*conn, bool) {
	// The hello must arrive promptly: no idle connection squatting.
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	data, err := lnk.read(ctx)
	if err != nil {
		return nil, false
	}
	var h Hello
	if err := json.Unmarshal(data, &h); err != nil {
		s.reject(lnk, "bad hello: not valid JSON")
		return nil, false
	}
	if h.Type != "hello" {
		s.reject(lnk, "first message must be hello")
		return nil, false
	}
	if err := validateHello(&h); err != nil {
		s.reject(lnk, "hello rejected: "+err.Error())
		return nil, false
	}
	return &conn{lnk: lnk, tag: h.Tag, id: h.ID, name: h.Name, pub: h.Pub}, true
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

// handleFrame dispatches one client frame. Presence needs no client
// input; "sdp"/"ice" are WebRTC signaling (M2 step ②) relayed inside
// the sender's group; "relay" pipes one fallback-stream frame (step
// ③); unknown types are dropped, never fatal (forward compatibility:
// older hub + newer engine).
func (s *Server) handleFrame(c *conn, data []byte) {
	var m struct {
		Type string `json:"type"`
	}
	if err := json.Unmarshal(data, &m); err != nil {
		return
	}
	switch m.Type {
	case "sdp", "ice":
		s.relay(c, data, m.Type)
	case "relay":
		s.relayStream(c, data)
	default:
		s.logf("frame %q from %s: not handled yet", m.Type, c.id)
	}
}

// relay forwards one signaling frame to its target device inside the
// sender's group. The hub overwrites From with the authenticated
// sender — a client cannot spoof another device — and answers with an
// error frame when the target is offline (the sender's dialer also
// times out; the frame just fails fast).
func (s *Server) relay(c *conn, data []byte, typ string) {
	var sig Signal
	if err := json.Unmarshal(data, &sig); err != nil {
		return
	}
	// Shape checks only: the body stays opaque (pion JSON, capped by
	// the frame limit).
	if len(sig.To) != 64 || !isHex(sig.To) {
		s.logf("relay: %s from %s: bad target", typ, c.id)
		return
	}
	s.mu.Lock()
	g := s.clients[c.tag]
	var target *conn
	if g != nil {
		target = g.conns[sig.To]
	}
	s.mu.Unlock()

	if target == nil || target == c {
		errData, _ := json.Marshal(Error{Type: "error", Error: "signaling target offline"})
		_ = c.write(errData)
		return
	}
	// Rebuild the frame instead of passing the client's bytes through:
	// From is authoritative (hub-verified), never client-claimed.
	fwd, err := json.Marshal(Signal{Type: typ, From: c.id, Side: sig.Side, Body: sig.Body})
	if err != nil {
		return
	}
	_ = target.write(fwd)
	s.logf("relay: %s %s→%s", typ, c.id, sig.To)
}

// maxRelayData caps one relay frame's payload (see internal/relay:
// streams chunk at 30 KiB, so 32 KiB has headroom). The wire frame
// (JSON base64) stays far below MaxReadLimit either way.
const maxRelayData = 32 << 10

// relayStream forwards one fallback-stream frame to its target device
// inside the sender's group. Fully stateless: the hub forwards the
// frame verbatim (From rewritten authoritatively) and forgets it —
// flow control lives end-to-end in the engines (internal/relay
// sliding window), so the hub never queues or stores stream data. A
// write error to the target (stalled or dead socket) is reported back
// to the sender as a stream-scoped error frame: the sender aborts
// that one stream, everything else keeps flowing.
func (s *Server) relayStream(c *conn, data []byte) {
	var f Relay
	if err := json.Unmarshal(data, &f); err != nil {
		return
	}
	if len(f.To) != 64 || !isHex(f.To) || len(f.Data) > maxRelayData {
		s.logf("relay stream from %s: bad frame", c.id)
		return
	}
	s.mu.Lock()
	g := s.clients[c.tag]
	var target *conn
	if g != nil {
		target = g.conns[f.To]
	}
	s.mu.Unlock()
	if target == nil || target == c {
		errData, _ := json.Marshal(Error{Type: "error", Error: "relay target offline", Sid: f.Sid})
		_ = c.write(errData)
		return
	}
	fwd, err := json.Marshal(Relay{Type: "relay", To: f.To, From: c.id, Sid: f.Sid, Data: f.Data, Eof: f.Eof, Ack: f.Ack})
	if err != nil {
		return
	}
	if err := target.write(fwd); err != nil {
		errData, _ := json.Marshal(Error{Type: "error", Error: "relay target stalled", Sid: f.Sid})
		_ = c.write(errData)
		return
	}
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
		g = &group{conns: map[string]*conn{}}
		s.clients[c.tag] = g
	}
	if old, ok := g.conns[c.id]; ok && old != c {
		old.replaced = true
		// kill, not a graceful close: a graceful close waits up to 5 s
		// for the old client's echo, and that client is gone (it is
		// the one redialing as c). Under the hub lock that stall
		// would freeze every join in the group.
		old.lnk.kill()
	} else if len(g.conns) >= MaxDevicesPerGroup {
		s.mu.Unlock()
		s.reject(c.lnk, "device limit reached for this group")
		return
	}
	g.conns[c.id] = c
	// Copy targets + peers under the lock: the map must never be
	// iterated after unlock (a concurrent join/leave would mutate it).
	peers, targets := view(g)
	s.mu.Unlock()
	// Serialize against the group's other broadcasts: see group.bmu.
	g.bmu.Lock()
	broadcast(targets, peers)
	g.bmu.Unlock()
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
	if g == nil || g.conns[c.id] != c {
		s.mu.Unlock()
		return
	}
	delete(g.conns, c.id)
	if len(g.conns) == 0 {
		delete(s.clients, c.tag)
	}
	peers, targets := view(g)
	s.mu.Unlock()
	// Serialize against the group's other broadcasts: see group.bmu.
	// (The group may already be deleted from s.clients; the lock is
	// still ours to hold — a re-created group would be newer state
	// anyway, and its join broadcast serializes behind this one only
	// if it grabbed this same lock object, which it does whenever the
	// two events overlap on the same connection set.)
	g.bmu.Lock()
	broadcast(targets, peers)
	g.bmu.Unlock()
}

// view copies a group's peers and their connections under the lock.
func view(g *group) ([]Peer, []*conn) {
	peers := make([]Peer, 0, len(g.conns))
	targets := make([]*conn, 0, len(g.conns))
	for _, c := range g.conns {
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
		_ = c.write(data)
	}
}

// reject sends an error frame and closes without a handshake wait
// (used before hello success and for cap violations) — the rejected
// client may not be reading, and a graceful close would stall this
// connection's goroutine for its full echo timeout.
func (s *Server) reject(lnk link, msg string) {
	data, _ := json.Marshal(Error{Type: "error", Error: msg})
	_ = lnk.send(data)
	lnk.kill()
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
