// Package relay carries the engine's existing HTTP mesh protocol
// unchanged through the hub's WSS relay (M2 step ③): the fallback
// transport for peers the WebRTC tunnel cannot reach (symmetric NAT,
// blocked UDP — anywhere the hub's own socket gets through). The
// design rule is the tunnel's: no new wire format. The relay moves
// plain HTTP/1.1 requests and responses byte-for-byte; only the way
// the bytes travel differs.
//
// Shape per stream (one HTTP connection):
//
//	dialer side (engine A)                      accepter side (engine B)
//	http.Transport.DialContext                  net.Listener (m.Listener)
//	  → stream (my sid namespace)                 ← first data frame
//	relay frames: {sid, data, eof, ack}  ⇄ hub ⇄ relay frames
//	http.Client ↔ http.Server (mesh handler)
//
// Frames travel as opaque `relay` frames inside the group
// (hub.Relay) — the hub re-addresses and forwards them with no
// per-stream state. Flow control is end-to-end (A ↔ B): the sender
// keeps at most `window` payload bytes un-acked; the receiver acks
// what its consumer has actually read. That bounds RAM at every hop
// (A ≤ window, hub ≈ socket buffers, B ≤ a window of not-yet-acked
// bytes) and caps a stream near window/RTT — 512 KiB at a 50 ms hub
// RTT is ~80 Mbit/s, the documented relay ceiling.
//
// Stream IDs come from two disjoint namespaces so simultaneous A→B
// and B→A dials cannot collide: the lexicographically larger device
// ID dials on odd sids, the smaller on even sids (dirParity). Acks
// echo the data frame's sid, so each side routes frames to its own
// streams by parity alone.
//
// Security: the relay is an unauthenticated transport exactly like
// the tunnel — the mesh protocol's group-key checks carry the trust
// boundary; the relay listener is served by the same mesh handler.
// Control routes stay closed: the relay's RemoteAddr is not a
// parseable IP, so the local-only dashboard guard rejects it.
package relay

import (
	"context"
	"fmt"
	"io"
	"net"
	"sync"
	"time"

	"chunkdrop/internal/hub"
)

// Tuning constants.
const (
	// chunkSize caps one frame's payload. JSON base64 inflates it to
	// ~41 KiB, safely under the hub's 64 KiB read limit (30 KiB
	// chosen to leave headroom for frame overhead).
	chunkSize = 30 << 10
	// window is the per-stream in-flight byte budget: Write blocks
	// once un-acked bytes reach it. It also bounds the receiver's
	// not-yet-acked buffering. At a 50 ms hub RTT ≈ 80 Mbit/s.
	window = 512 << 10
	// ackEvery: the receiver sends a cumulative ack after consuming
	// this many new bytes — or immediately when pending drains.
	ackEvery = 16 << 10
	// waitTick bounds one Write wait between ack checks (the ack
	// signal channel may be missed under races; the tick re-checks).
	waitTick = 100 * time.Millisecond
	// stallTimeout: a stream whose acks stop entirely for this long
	// is aborted (hub socket died, peer vanished, acks dropped under
	// load). Generous: a healthy stream drains a full window in well
	// under a second at even 1 Mbit/s.
	stallTimeout = 15 * time.Second
	// reapEvery / reapIdle: the reaper aborts streams with no
	// activity at all for reapIdle — a dead hub socket or a vanished
	// peer must not leak handler goroutines (the tunnel gets conn
	// death for free from the DataChannel; the relay has to reap).
	reapEvery = 30 * time.Second
	reapIdle  = 3 * time.Minute
	// listenBacklog bounds un-accepted server-side streams; over
	// that, new streams are dropped rather than blocking the hub
	// read loop (never block the read loop: it also carries
	// presence and signaling).
	listenBacklog = 64
	// ackQueue bounds queued ack sends; overflow drops an ack (the
	// sender's stall failsafe recovers).
	ackQueue = 256
)

// Addr is the net.Addr on relay conns. Deliberately not a parseable
// IP: the engine's local-only dashboard guard rejects unparseable
// RemoteAddrs, so control routes stay closed to remote engines while
// group-key-authed mesh routes work as on the LAN.
type Addr struct{ Peer string }

func (a Addr) Network() string { return "relay" }
func (a Addr) String() string  { return "relay:" + a.Peer }

// dirParity is the sid parity bit a dialer uses toward peer: the two
// engines allocate stream IDs from disjoint namespaces, or a
// simultaneous A→B and B→A dial with the same numeric sid would
// collide. The lexicographically larger device ID dials on odd sids.
// Deterministic on both ends (they compare the same two IDs).
func dirParity(self, peer string) uint64 {
	if self > peer {
		return 1
	}
	return 0
}

// Manager owns this engine's relay streams. Wire Send to
// hubclient.SendRelay and hubclient.OnRelay to Deliver.
type Manager struct {
	SelfID string
	// Send delivers one frame to a peer through the hub.
	Send func(to string, f hub.Relay) error
	// Logf renders a line; nil = silent (the caller wires logx).
	Logf func(string, ...any)

	mu      sync.Mutex
	closed  chan struct{}
	nextSid uint64 // dial sid counter; sid = n<<1 | dirParity
	out     map[uint64]*stream
	in      map[inKey]*stream
	acks    chan hub.Relay // ack frames awaiting the pump goroutine
	listenC chan net.Conn
}

type inKey struct {
	from string
	sid  uint64
}

// New builds a manager and starts its two housekeeping goroutines
// (ack pump + idle reaper).
func New(selfID string, send func(to string, f hub.Relay) error) *Manager {
	m := &Manager{
		SelfID:  selfID,
		Send:    send,
		closed:  make(chan struct{}),
		out:     map[uint64]*stream{},
		in:      map[inKey]*stream{},
		acks:    make(chan hub.Relay, ackQueue),
		listenC: make(chan net.Conn, listenBacklog),
	}
	go m.pump()
	go m.reap()
	return m
}

func (m *Manager) logf(format string, args ...any) {
	if m.Logf != nil {
		m.Logf(format, args...)
	}
}

func (m *Manager) isClosed() bool {
	select {
	case <-m.closed:
		return true
	default:
		return false
	}
}

// Close aborts every stream and stops the listener. Idempotent.
func (m *Manager) Close() {
	m.mu.Lock()
	select {
	case <-m.closed:
		m.mu.Unlock()
		return
	default:
		close(m.closed)
	}
	outs := make([]*stream, 0, len(m.out))
	for _, s := range m.out {
		outs = append(outs, s)
	}
	ins := make([]*stream, 0, len(m.in))
	for _, s := range m.in {
		ins = append(ins, s)
	}
	m.out = map[uint64]*stream{}
	m.in = map[inKey]*stream{}
	m.mu.Unlock()
	for _, s := range outs {
		s.finish() // maps already purged; just unblock the streams
	}
	for _, s := range ins {
		s.finish()
	}
}

// ---------------------------------------------------------------------------
// Frame routing (hub → engine, on the hubclient read loop)
// ---------------------------------------------------------------------------

// Deliver routes one inbound frame. It must never block and never
// panic — the read loop also carries presence and signaling.
// Malformed frames are dropped silently (the hub already validated
// the target and payload size).
//
// Routing is by sid parity against the sender: a stream lives in the
// namespace of whoever DIALED it, and both directions of a stream
// (request data, response data, and the acks of each) carry that one
// sid. So from the receiver's view:
//
//	sid parity == my dial parity     → a stream I dialed (out map):
//	                                   acks for my requests, response
//	                                   data coming back.
//	sid parity == the peer's parity  → a stream they dialed (in map):
//	                                   request data/EOF, and acks for
//	                                   my responses.
func (m *Manager) Deliver(from string, f hub.Relay) {
	switch f.Type {
	case "error":
		// The hub could not deliver a frame for one of my streams
		// (target offline/stalled). Abort exactly that stream.
		if f.Sid == 0 {
			return // conn-level error (hello rejects and the like)
		}
		m.mu.Lock()
		s := m.out[f.Sid]
		m.mu.Unlock()
		if s != nil {
			s.abortStream("relay: hub could not deliver stream %d — aborted", f.Sid)
		}
	case "relay":
		if f.Sid == 0 {
			return
		}
		switch f.Sid & 1 {
		case dirParity(m.SelfID, from):
			// My dial namespace: response data for / acks of a stream
			// I dialed. The stream must exist (my dial created it).
			m.mu.Lock()
			s := m.out[f.Sid]
			m.mu.Unlock()
			if s == nil || s.peer != from {
				return
			}
			if f.Ack > 0 {
				s.gotAck(f.Ack)
			} else {
				s.deliver(f)
			}
		case dirParity(from, m.SelfID):
			// The peer's namespace: a stream they dialed (accept side).
			key := inKey{from: from, sid: f.Sid}
			if f.Ack > 0 {
				// Ack for this stream's response data (my writes).
				m.mu.Lock()
				s := m.in[key]
				m.mu.Unlock()
				if s != nil {
					s.gotAck(f.Ack)
				}
				return
			}
			m.deliverStream(from, f)
		}
	}
}

// deliverStream appends payload to (or closes) the peer-dialed
// stream, creating it — and offering it to the listener — on the
// first frame. Runs on the hub read loop: registration under the
// manager mutex, payload under the stream mutex, wakeups
// non-blocking.
func (m *Manager) deliverStream(from string, f hub.Relay) {
	key := inKey{from: from, sid: f.Sid}
	var fresh bool
	m.mu.Lock()
	if m.isClosed() {
		m.mu.Unlock()
		return
	}
	s := m.in[key]
	if s == nil {
		if len(m.listenC) >= listenBacklog {
			m.mu.Unlock()
			m.logf("relay: listener backlog full — dropping stream from %s", short(from))
			return
		}
		s = newStream(m, from, f.Sid)
		s.key, s.inOk = key, true
		m.in[key] = s
		fresh = true
	}
	m.mu.Unlock()
	s.deliver(f)
	if fresh {
		// Offer the conn to the http.Server. Non-blocking (backlog
		// checked above); a closed manager finishes the stream.
		select {
		case m.listenC <- s:
		case <-m.closed:
			s.finish()
		}
	}
}

// removeIn drops a finished accept-side stream from the map.
func (m *Manager) removeIn(key inKey) {
	m.mu.Lock()
	delete(m.in, key)
	m.mu.Unlock()
}

// removeOut drops a finished dial-side stream.
func (m *Manager) removeOut(sid uint64) {
	m.mu.Lock()
	delete(m.out, sid)
	m.mu.Unlock()
}

// pump sends ack frames so the hub read loop never blocks on a hub
// write. Overflow drops an ack; the sender's stall failsafe recovers.
func (m *Manager) pump() {
	for {
		select {
		case f := <-m.acks:
			if m.Send == nil {
				continue
			}
			if err := m.Send(f.To, f); err != nil {
				m.logf("relay: ack send failed: %v", err)
			}
		case <-m.closed:
			return
		}
	}
}

// reap aborts streams with no activity for reapIdle (dead hub
// socket, vanished peer — nothing else tells an accept-side stream).
func (m *Manager) reap() {
	t := time.NewTicker(reapEvery)
	defer t.Stop()
	for {
		select {
		case <-t.C:
		case <-m.closed:
			return
		}
		m.mu.Lock()
		var stale []*stream
		for key, s := range m.in {
			if s.idleFor() > reapIdle {
				stale = append(stale, s)
				delete(m.in, key)
			}
		}
		for sid, s := range m.out {
			if s.idleFor() > reapIdle {
				stale = append(stale, s)
				delete(m.out, sid)
			}
		}
		m.mu.Unlock()
		for _, s := range stale {
			s.abortStream("relay: stream idle %v — aborted", reapIdle)
		}
	}
}

// ---------------------------------------------------------------------------
// Dial side (the HTTP client direction)
// ---------------------------------------------------------------------------

// Dial opens one relay stream to peer as a net.Conn for an
// http.Transport DialContext. Exported so the engine's tunnel-first
// fallback dialer can use it too.
func (m *Manager) Dial(ctx context.Context, peer string) (net.Conn, error) {
	if m.isClosed() {
		return nil, net.ErrClosed
	}
	if peer == m.SelfID || len(peer) != 64 || !isHex(peer) {
		return nil, fmt.Errorf("relay: bad peer id %q", short(peer))
	}
	m.mu.Lock()
	if m.isClosed() {
		m.mu.Unlock()
		return nil, net.ErrClosed
	}
	m.nextSid++
	sid := m.nextSid<<1 | dirParity(m.SelfID, peer)
	s := newStream(m, peer, sid)
	s.out = sid
	m.out[sid] = s
	m.mu.Unlock()
	return s, nil
}

// ---------------------------------------------------------------------------
// Accept side (the HTTP server direction)
// ---------------------------------------------------------------------------

// Listener accepts incoming relay streams (remote engines' HTTP
// connections) for http.Serve — the accept-side twin of the tunnel's
// listener; both serve the same mesh handler.
func (m *Manager) Listener() net.Listener { return &listener{m: m} }

type listener struct{ m *Manager }

func (l *listener) Accept() (net.Conn, error) {
	select {
	case c := <-l.m.listenC:
		return c, nil
	case <-l.m.closed:
		return nil, net.ErrClosed
	}
}

func (l *listener) Close() error {
	l.m.Close()
	return nil
}

func (l *listener) Addr() net.Addr { return Addr{} }

// ---------------------------------------------------------------------------
// stream — one net.Conn over relay frames
// ---------------------------------------------------------------------------

// stream adapts relay frames into net.Conn for net/http on both
// sides. Both directions are symmetric: Write applies the sliding
// window, Read consumes and acks.
type stream struct {
	mgr  *Manager
	peer string
	sid  uint64

	mu sync.Mutex
	// inbound
	pending      [][]byte  // frames not yet handed to the reader
	cur          []byte    // remainder of the current frame
	eofIn        bool      // peer closed its send direction
	consumed     uint64    // payload bytes handed to the reader (ack basis)
	ackSent      uint64    // last cumulative ack queued
	lastActivity time.Time // reaper staleness basis (any frame/ack/send)
	// outbound
	written uint64    // payload bytes sent
	acked   uint64    // last cumulative ack from the peer
	ackAt   time.Time // last ack progress (Write stall failsafe)
	eofOut  bool      // we sent eof (no more writes allowed)
	// lifecycle
	closed bool
	once   sync.Once
	dead   chan struct{}
	notify chan struct{} // reader wakeup (cap 1, non-blocking)
	ackC   chan struct{} // ack-progress wakeup for Write (cap 1)
	wmu    sync.Mutex    // serializes frame sends (byte order)

	key  inKey  // accept side only
	inOk bool   // registered in mgr.in
	out  uint64 // dial side: the registered sid (never 0)
}

func newStream(m *Manager, peer string, sid uint64) *stream {
	return &stream{
		mgr:          m,
		peer:         peer,
		sid:          sid,
		dead:         make(chan struct{}),
		notify:       make(chan struct{}, 1),
		ackC:         make(chan struct{}, 1),
		lastActivity: time.Now(),
	}
}

// deliver applies one data/eof frame (called from the hub read loop
// via deliverStream — never blocks).
func (s *stream) deliver(f hub.Relay) {
	s.mu.Lock()
	if s.closed {
		s.mu.Unlock()
		return
	}
	s.lastActivity = time.Now()
	if len(f.Data) > 0 {
		s.pending = append(s.pending, f.Data)
	}
	if f.Eof {
		s.eofIn = true
	}
	s.mu.Unlock()
	select {
	case s.notify <- struct{}{}:
	default:
	}
}

// gotAck applies a cumulative ack (called from Deliver — never
// blocks).
func (s *stream) gotAck(ack uint64) {
	s.mu.Lock()
	if ack > s.acked {
		s.acked = ack
		s.ackAt = time.Now()
	}
	s.lastActivity = time.Now()
	s.mu.Unlock()
	select {
	case s.ackC <- struct{}{}:
	default:
	}
}

// Read returns the inbound byte stream. Frame boundaries are not
// preserved (net/http wants a stream) but bytes are ordered and
// complete — each direction rides one ordered+reliable WSS.
func (s *stream) Read(p []byte) (int, error) {
	for {
		s.mu.Lock()
		if len(s.cur) == 0 && len(s.pending) > 0 {
			s.cur = s.pending[0]
			s.pending[0] = nil
			s.pending = s.pending[1:]
		}
		if len(s.cur) > 0 {
			n := copy(p, s.cur)
			s.cur = s.cur[n:]
			s.consumed += uint64(n)
			need := s.consumed-s.ackSent >= ackEvery
			empty := len(s.cur) == 0 && len(s.pending) == 0
			s.mu.Unlock()
			if need || empty {
				s.sendAck()
			}
			return n, nil
		}
		eof := s.eofIn || s.closed
		s.mu.Unlock()
		if eof {
			return 0, io.EOF
		}
		select {
		case <-s.notify:
		case <-s.dead:
			// Aborted: loop once more — finish() sets closed, so the
			// next pass drains any late arrivals and returns EOF.
		}
	}
}

// sendAck queues one cumulative ack for the pump goroutine.
func (s *stream) sendAck() {
	s.mu.Lock()
	ack := s.consumed
	if ack == s.ackSent {
		s.mu.Unlock()
		return
	}
	s.ackSent = ack
	s.mu.Unlock()
	select {
	case s.mgr.acks <- hub.Relay{Type: "relay", To: s.peer, Sid: s.sid, Ack: ack}:
	default:
		// Dropped under load; the sender's stall failsafe covers it.
	}
}

// Write sends p as one or more relay frames, applying the sliding
// window: never more than `window` payload bytes un-acked. The
// window is what turns the hub's stateless forwarding into real
// backpressure — without it the sender could pump a whole transfer
// into buffers nobody is draining.
func (s *stream) Write(p []byte) (int, error) {
	s.wmu.Lock()
	defer s.wmu.Unlock()
	total := 0
	for len(p) > 0 {
		s.mu.Lock()
		if s.closed || s.eofOut {
			s.mu.Unlock()
			return total, net.ErrClosed
		}
		inflight := s.written - s.acked
		n := len(p)
		if avail := int64(window) - int64(inflight); avail < int64(n) {
			n = int(avail)
		}
		if n > chunkSize {
			n = chunkSize
		}
		if n <= 0 {
			// Window exhausted: wait for ack progress.
			s.mu.Unlock()
			if err := s.waitAck(); err != nil {
				return total, err
			}
			continue
		}
		s.written += uint64(n)
		s.mu.Unlock()
		// Copy: the frame outlives this call (the hub client serializes
		// it, the peer buffers it), and p belongs to the caller —
		// http transports reuse their copy buffers immediately.
		data := make([]byte, n)
		copy(data, p[:n])
		if err := s.send(hub.Relay{Type: "relay", To: s.peer, Sid: s.sid, Data: data}); err != nil {
			return total, err
		}
		p = p[n:]
		total += n
	}
	return total, nil
}

// waitAck blocks for ack progress, stream death, or the stall
// failsafe (no ack progress at all for stallTimeout — see
// stallTimeout for why that must not hang forever).
func (s *stream) waitAck() error {
	s.mu.Lock()
	base, ackAt := s.acked, s.ackAt
	s.mu.Unlock()
	deadline := ackAt.Add(stallTimeout)
	for {
		select {
		case <-s.ackC:
			return nil
		case <-s.dead:
			return net.ErrClosed
		case <-time.After(waitTick):
		}
		s.mu.Lock()
		progress := s.acked
		s.mu.Unlock()
		if progress != base {
			return nil
		}
		if !time.Now().Before(deadline) {
			s.abortStream("relay: no ack progress for %v — stream aborted", stallTimeout)
			return net.ErrClosed
		}
	}
}

// send delivers one frame to the hub. Send errors mean the hub
// socket is gone — abort the stream (callers return the error).
func (s *stream) send(f hub.Relay) error {
	if s.mgr.Send == nil {
		s.abortStream("relay: no send path (hub off)")
		return net.ErrClosed
	}
	if err := s.mgr.Send(s.peer, f); err != nil {
		s.abortStream("relay: send failed: %v", err)
		return err
	}
	s.mu.Lock()
	s.lastActivity = time.Now()
	s.mu.Unlock()
	return nil
}

// Close ends the send direction gracefully: an eof frame goes out
// (the peer drains and sees EOF) and the stream unwinds. Idempotent.
func (s *stream) Close() error {
	// eofOut + the eof frame go out under wmu, so a concurrent Write
	// can never slip a data frame in after the eof frame.
	s.wmu.Lock()
	s.mu.Lock()
	closed, eofOut := s.closed, s.eofOut
	if !closed && !eofOut {
		s.eofOut = true
	}
	s.mu.Unlock()
	if !closed && !eofOut {
		// Best effort — a failed eof send leaves the peer to its own
		// reaper, and a truncated body is what closing early means.
		_ = s.mgr.Send(s.peer, hub.Relay{Type: "relay", To: s.peer, Sid: s.sid, Eof: true})
	}
	s.wmu.Unlock()
	s.finish()
	return nil
}

// abortStream logs why the stream died and tears it down (no eof
// frame — the peer's reaper/stall failsafe handles its side).
func (s *stream) abortStream(format string, args ...any) {
	s.mgr.logf(format, args...)
	s.finish()
}

// finish unregisters the stream and wakes everyone. The once guards
// the dead channel (Read/Write wake exactly once); map removal is
// keyed by role.
func (s *stream) finish() {
	s.once.Do(func() {
		s.mu.Lock()
		s.closed = true
		key, inOk, out := s.key, s.inOk, s.out
		s.mu.Unlock()
		close(s.dead)
		if inOk {
			s.mgr.removeIn(key)
		}
		if out != 0 {
			s.mgr.removeOut(out)
		}
	})
}

// idleFor is the time since the last inbound frame, ack or send —
// the reaper's staleness measure.
func (s *stream) idleFor() time.Duration {
	s.mu.Lock()
	defer s.mu.Unlock()
	return time.Since(s.lastActivity)
}

// LocalAddr/RemoteAddr mirror the tunnel's Addr trick: not parseable
// IPs, so the engine's local-only guard keeps control routes closed.
func (s *stream) LocalAddr() net.Addr  { return Addr{Peer: s.peer} }
func (s *stream) RemoteAddr() net.Addr { return Addr{Peer: s.peer} }

// SetDeadline/SetReadDeadline/SetWriteDeadline: deadlines cannot be
// honored on a relay stream without tearing it down, and net/http
// only uses them when explicitly configured — the relay's HTTP
// server/client does not. Documented no-ops, like the tunnel's.
func (s *stream) SetDeadline(t time.Time) error      { return nil }
func (s *stream) SetReadDeadline(t time.Time) error  { return nil }
func (s *stream) SetWriteDeadline(t time.Time) error { return nil }

// short renders a device ID for log lines.
func short(id string) string {
	if len(id) <= 12 {
		return id
	}
	return id[:12] + "…"
}

// isHex reports whether s is lowercase hex of even length (mirrors
// hub.isHex without importing the server package's internals).
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
