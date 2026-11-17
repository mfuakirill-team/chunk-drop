// Package tunnel carries the engine's existing HTTP mesh protocol
// unchanged inside WebRTC DataChannels (M2 step ②). The design rule
// is "no new wire format": LAN and internet peers speak the exact
// same requests; only the bytes' way between the engines differs.
//
// Shape per peer pair:
//
//	offerer side (engine A)                     answerer side (engine B)
//	http.Transport.DialContext                  net.Listener (m.Listener)
//	  → new DataChannel (CreateDataChannel)       ← OnDataChannel
//	  → dcConn (net.Conn over DC messages)        → dcConn
//	http.Client ↔ http.Server (mesh handler)
//
// Each HTTP connection maps to one DataChannel on a per-direction
// PeerConnection: A offers the PC it dials through, B offers the PC
// it dials through — two independent PCs, so neither side ever waits
// on the other to renegotiate, and there is no glare by construction.
// Signaling (non-trickle SDP: all candidates are inside the
// description) goes through the hub, tagged with which PC it belongs
// to: "offer" = the sender's PC, "answer" = the PC the sender is
// answering on. An ID-ordering scheme is unnecessary because both
// directions are separate sessions.
//
// Security: the tunnel itself is an unauthenticated transport — the
// mesh protocol's group-key checks carry the actual trust boundary,
// exactly like the LAN listener. The dashboard stays local-only: the
// http.Server sees a tunnel RemoteAddr that is not a parseable IP
// (api.isLocalRequest fails), so control routes remain closed to
// remote engines.
package tunnel

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"strings"
	"sync"
	"time"

	"chunkdrop/internal/logx"

	"github.com/pion/webrtc/v4"
)

// Tuning constants. DataChannel messages cap at 64 KiB (SCTP
// default), so writes are chunked; everything else keeps a quiet
// local mesh responsive without tuning-by-feeling.
const (
	// MsgChunk splits one net.Conn Write into DataChannel messages.
	MsgChunk = 16 << 10
	// highWater / lowThreshold: SCTP write backpressure. Above the
	// high-water mark, Write waits for the buffer to drain below
	// lowThreshold (OnBufferedAmountLow), bounding RAM per channel.
	highWater    = 1 << 20 // 1 MiB buffered before Write blocks
	lowThreshold = 256 << 10
	// msgQueue bounds buffered inbound messages per channel (16 KiB
	// each). The HTTP layer always drains bodies, so this is ample.
	msgQueue = 64

	// gatherTimeout bounds the ICE gathering wait before an SDP is
	// sent (host candidates appear in ms; STUN reflexive adds a bit).
	gatherTimeout = 5 * time.Second
	// sigTimeout bounds waiting for one SDP answer to arrive.
	sigTimeout = 20 * time.Second
	// dialTimeout bounds one HTTP-conn dial: ensure PC + SDP +
	// DTLS/SCTP + channel open. Generous first-connect budget; later
	// dials reuse the live PC and finish in milliseconds.
	dialTimeout = 25 * time.Second
)

// Addr is the net.Addr on tunnel conns. It is deliberately not a
// parseable IP: the engine's local-only dashboard guard rejects
// unparseable RemoteAddrs, so control routes stay closed to remote
// engines while group-key-authed mesh routes work as on the LAN.
type Addr struct{ Peer string }

func (a Addr) Network() string { return "tunnel" }
func (a Addr) String() string  { return "tunnel:" + a.Peer }

// SendFunc delivers one signaling frame to a hub peer. side is
// "offer" (this engine's PC) or "answer" (the PC this engine answers
// on); typ is "sdp" or "ice"; body is the pion JSON payload.
type SendFunc func(to, side, typ string, body any) error

// Manager owns every tunnel PeerConnection of this engine.
type Manager struct {
	SelfID  string
	StunURL string
	Send    SendFunc
	Logf    func(string, ...any)

	dialTimeout time.Duration

	mu      sync.Mutex
	out     map[string]*outPC // peer → PC I offered (my dial direction)
	in      map[string]*inPC  // peer → PC they offered (my answer side)
	clients map[string]*http.Client
	listenC chan net.Conn
	closed  chan struct{}
}

// New builds a manager. send is the hub's SendSignal; wire
// hubclient.OnSignal to SignalIn.
func New(selfID, stunURL string, send SendFunc) *Manager {
	return &Manager{
		SelfID:      selfID,
		StunURL:     stunURL,
		Send:        send,
		dialTimeout: dialTimeout,
		out:         map[string]*outPC{},
		in:          map[string]*inPC{},
		clients:     map[string]*http.Client{},
		listenC:     make(chan net.Conn, 64),
		closed:      make(chan struct{}),
	}
}

func (m *Manager) logf(format string, args ...any) {
	if m.Logf != nil {
		m.Logf(format, args...)
	}
}

// isClosed reports whether Close has run.
func (m *Manager) isClosed() bool {
	select {
	case <-m.closed:
		return true
	default:
		return false
	}
}

// Close tears down every PC and stops the listener. Idempotent.
func (m *Manager) Close() {
	m.mu.Lock()
	outs := make([]*outPC, 0, len(m.out))
	for _, p := range m.out {
		outs = append(outs, p)
	}
	m.out = map[string]*outPC{}
	ins := make([]*inPC, 0, len(m.in))
	for _, p := range m.in {
		ins = append(ins, p)
	}
	m.in = map[string]*inPC{}
	m.mu.Unlock()
	for _, p := range outs {
		p.killPC()
	}
	for _, p := range ins {
		p.killPC()
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	select {
	case <-m.closed:
		return // already closed: PCs were torn down on the first call
	default:
		close(m.closed)
	}
}

// Listener accepts incoming tunnel conns (remote engines' HTTP
// connections) for http.Serve.
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

// pushConn feeds one accepted DataChannel conn to the listener.
func (m *Manager) pushConn(c net.Conn) {
	select {
	case m.listenC <- c:
	case <-m.closed:
		_ = c.Close()
	}
}

// ---------------------------------------------------------------------------
// HTTP client side (the dial direction)
// ---------------------------------------------------------------------------

// HostFor renders the PeerRef.Host for a tunnel peer: a syntactically
// valid host:port that the transport's DialContext decodes back into
// the device ID (no DNS involved — DialContext replaces resolution).
func HostFor(deviceID string) string {
	return net.JoinHostPort("tunnel-"+deviceID, "9")
}

// Dial opens one HTTP connection to peer (a fresh DataChannel on the
// engine's own offerer PC). Exported for the engine's tunnel-first
// fallback dialer: it dials here first, then falls back to the hub
// relay (step ③) when this fails.
func (m *Manager) Dial(ctx context.Context, peer string) (net.Conn, error) {
	return m.dialConn(ctx, peer)
}

// PeerConnected reports whether the outgoing PC to peer is currently
// connected — the fallback dialer uses it to retry the tunnel
// immediately after an ICE recovery instead of waiting out a
// cooldown.
func (m *Manager) PeerConnected(peer string) bool {
	m.mu.Lock()
	defer m.mu.Unlock()
	p, ok := m.out[peer]
	return ok && p.pc != nil && p.pc.ConnectionState() == webrtc.PeerConnectionStateConnected
}

// PeerFromHost decodes HostFor's output back into a device ID.
// Anything without the tunnel- prefix is rejected — no real DNS name
// can slip into a tunnel dial.
func PeerFromHost(hostPort string) (string, error) {
	host, _, err := net.SplitHostPort(hostPort)
	if err != nil {
		return "", fmt.Errorf("tunnel: bad peer address %q", hostPort)
	}
	id, ok := strings.CutPrefix(host, "tunnel-")
	if !ok || id == "" {
		return "", fmt.Errorf("tunnel: not a tunnel address %q", hostPort)
	}
	return id, nil
}

// HTTPClient returns (and caches) the HTTP client for one tunnel
// peer. Conns are DataChannels: transport-level pooling applies, with
// IdleConnTimeout disabled so an idle keep-alive channel is reused
// rather than torn down. A per-peer client also isolates pooling per
// destination, mirroring the LAN client's host:port addressing.
func (m *Manager) HTTPClient(peer string) *http.Client {
	m.mu.Lock()
	defer m.mu.Unlock()
	if c, ok := m.clients[peer]; ok {
		return c
	}
	tr := &http.Transport{
		DialContext: func(ctx context.Context, network, addr string) (net.Conn, error) {
			// addr is HostFor(peer) — nothing here does DNS.
			if got, err := PeerFromHost(addr); err != nil || got != peer {
				return nil, fmt.Errorf("tunnel: dial for %q, client serves %q", addr, peer)
			}
			dctx, cancel := context.WithTimeout(ctx, m.dialTimeout)
			defer cancel()
			return m.dialConn(dctx, peer)
		},
		MaxConnsPerHost:       2, // announcement + chunk stream headroom
		MaxIdleConnsPerHost:   2,
		IdleConnTimeout:       0, // keep-alive channels live as long as the PC
		ResponseHeaderTimeout: 30 * time.Second,
	}
	c := &http.Client{Transport: tr, Timeout: 120 * time.Second}
	m.clients[peer] = c
	return c
}

// dialConn opens one HTTP connection to peer: a fresh DataChannel on
// the engine's own (offerer) PC for that peer.
func (m *Manager) dialConn(ctx context.Context, peer string) (net.Conn, error) {
	p := m.ensureOut(peer)
	if p.pc == nil {
		return nil, fmt.Errorf("tunnel: no peer connection to %s", peer)
	}
	dc, err := p.pc.CreateDataChannel("", nil)
	if err != nil {
		return nil, fmt.Errorf("tunnel: create channel to %s: %w", peer, err)
	}
	open := make(chan struct{})
	dc.OnOpen(func() { close(open) })
	select {
	case <-open:
	case <-p.dead:
		_ = dc.Close()
		return nil, fmt.Errorf("tunnel: connection to %s died while opening", peer)
	case <-ctx.Done():
		_ = dc.Close()
		return nil, fmt.Errorf("tunnel: dial %s: %w", peer, ctx.Err())
	}
	return newConn(dc, peer), nil
}

// ensureOut returns the engine's own PC toward peer, creating it on
// first use. The PC's OnNegotiationNeeded drives the SDP offer; dial
// waits for the channel's OnOpen, which implies the exchange finished.
func (m *Manager) ensureOut(peer string) *outPC {
	m.mu.Lock()
	if m.isClosed() {
		m.mu.Unlock()
		return &outPC{m: m, peer: peer, dead: closedChan()}
	}
	if p, ok := m.out[peer]; ok {
		m.mu.Unlock()
		return p
	}
	p := &outPC{
		m:       m,
		peer:    peer,
		dead:    make(chan struct{}),
		answers: make(chan *webrtc.SessionDescription, 1),
	}
	p.mu.Lock() // block handleAnswer until the PC is wired
	pc, err := newPC(m.StunURL, peer+" (out)", &p.gather)
	if err != nil {
		p.mu.Unlock()
		m.mu.Unlock()
		p.markDead()
		m.logf("peer connection to %s: %v", peer, err)
		return p // pc nil → dialConn fails fast
	}
	p.pc = pc
	m.out[peer] = p
	m.mu.Unlock()

	pc.OnNegotiationNeeded(func() { go p.renegotiate() })
	pc.OnConnectionStateChange(func(st webrtc.PeerConnectionState) {
		logx.Debug("tunnel", "%s: connection state %s", peer+" (out)", st)
		switch st {
		case webrtc.PeerConnectionStateFailed, webrtc.PeerConnectionStateClosed:
			if st == webrtc.PeerConnectionStateFailed {
				logx.Warn("tunnel", "%s: ICE failed — no reachable candidate pair (STUN unreachable → no srflx candidates; or symmetric NAT → needs relay)", peer)
			}
			p.kill()
		case webrtc.PeerConnectionStateConnected:
			m.logf("connected to %s (outgoing)", peer)
		}
	})
	p.mu.Unlock() // PC ready: signaling may proceed
	return p
}

// ---------------------------------------------------------------------------
// outPC — the PC this engine offered (its dial direction)
// ---------------------------------------------------------------------------

type outPC struct {
	m    *Manager
	peer string
	pc   *webrtc.PeerConnection

	// mu serializes SDP exchanges: renegotiate holds it from offer
	// creation to answer application (pion needs a stable signaling
	// state), and ensureOut briefly holds it while the PC is wired.
	mu sync.Mutex

	// answers carries one pending SDP answer per exchange; renegotiate
	// is serialized by mu, so one slot suffices.
	answers chan *webrtc.SessionDescription
	gather  chan struct{} // closed on ICE gathering complete
	dead    chan struct{}
	deadSet sync.Once
}

// kill removes this PC from the manager, marks it dead, and closes
// the underlying PC; a later dial builds a fresh one.
func (p *outPC) kill() {
	p.m.mu.Lock()
	if cur, ok := p.m.out[p.peer]; ok && cur == p {
		delete(p.m.out, p.peer)
	}
	p.m.mu.Unlock()
	p.markDead()
	p.killPC()
}

func (p *outPC) killPC() {
	if p.pc != nil {
		_ = p.pc.Close()
	}
	p.markDead()
}

func (p *outPC) markDead() { p.deadSet.Do(func() { close(p.dead) }) }

// handleAnswer applies the peer's SDP answer (side="answer").
func (p *outPC) handleAnswer(body json.RawMessage) {
	var sd webrtc.SessionDescription
	if err := json.Unmarshal(body, &sd); err != nil || sd.SDP == "" {
		p.m.logf("bad answer from %s", p.peer)
		return
	}
	select {
	case p.answers <- &sd:
	default:
		p.m.logf("unexpected answer from %s (no offer pending)", p.peer)
	}
}

// renegotiate runs one full SDP exchange (initial offer included —
// pion fires NegotiationNeeded for the first channel too). Serialized
// by mu: pion requires a stable signaling state between exchanges,
// and concurrent CreateDataChannel calls fire NegotiationNeeded once
// per channel.
func (p *outPC) renegotiate() {
	if p.pc == nil {
		return
	}
	p.mu.Lock()
	defer p.mu.Unlock()
	if p.isDead() {
		return
	}
	offer, err := p.pc.CreateOffer(nil)
	if err != nil {
		p.m.logf("offer to %s: %v", p.peer, err)
		return
	}
	if err := p.pc.SetLocalDescription(offer); err != nil {
		p.m.logf("local offer to %s: %v", p.peer, err)
		return
	}
	desc, err := waitGathered(p.pc, p.gather)
	if err != nil {
		p.m.logf("gather for %s: %v", p.peer, err)
		return
	}
	if p.m.Send == nil {
		p.m.logf("no signaling path to %s", p.peer)
		return
	}
	if err := p.m.Send(p.peer, "offer", "sdp", desc); err != nil {
		p.m.logf("send offer to %s: %v", p.peer, err)
		return
	}
	select {
	case ans := <-p.answers:
		if err := p.pc.SetRemoteDescription(*ans); err != nil {
			p.m.logf("answer from %s: %v", p.peer, err)
		}
	case <-time.After(sigTimeout):
		p.m.logf("no answer from %s in %s", p.peer, sigTimeout)
	case <-p.dead:
	}
}

func (p *outPC) isDead() bool {
	select {
	case <-p.dead:
		return true
	default:
		return false
	}
}

// ---------------------------------------------------------------------------
// inPC — the PC this engine answers (the peer's dial direction)
// ---------------------------------------------------------------------------

type inPC struct {
	m    *Manager
	peer string
	pc   *webrtc.PeerConnection

	mu      sync.Mutex // serializes offer → answer exchanges
	gather  chan struct{}
	dead    chan struct{}
	deadSet sync.Once
}

// kill removes this PC from the manager, marks it dead, and closes
// the underlying PC.
func (p *inPC) kill() {
	p.m.mu.Lock()
	if cur, ok := p.m.in[p.peer]; ok && cur == p {
		delete(p.m.in, p.peer)
	}
	p.m.mu.Unlock()
	p.markDead()
	p.killPC()
}

func (p *inPC) killPC() {
	if p.pc != nil {
		_ = p.pc.Close()
	}
	p.markDead()
}

func (p *inPC) markDead() { p.deadSet.Do(func() { close(p.dead) }) }

// handleOffer applies the peer's SDP offer (side="offer") and answers.
// Called on the hubclient read loop: offers are processed in order.
func (p *inPC) handleOffer(body json.RawMessage) {
	var sd webrtc.SessionDescription
	if err := json.Unmarshal(body, &sd); err != nil || sd.SDP == "" {
		p.m.logf("bad offer from %s", p.peer)
		return
	}
	p.mu.Lock()
	defer p.mu.Unlock()
	if p.isDead() {
		return
	}
	if err := p.pc.SetRemoteDescription(sd); err != nil {
		p.m.logf("remote offer from %s: %v", p.peer, err)
		return
	}
	ans, err := p.pc.CreateAnswer(nil)
	if err != nil {
		p.m.logf("answer for %s: %v", p.peer, err)
		return
	}
	if err := p.pc.SetLocalDescription(ans); err != nil {
		p.m.logf("local answer for %s: %v", p.peer, err)
		return
	}
	desc, err := waitGathered(p.pc, p.gather)
	if err != nil {
		p.m.logf("gather for %s: %v", p.peer, err)
		return
	}
	if p.m.Send == nil {
		p.m.logf("no signaling path to %s", p.peer)
		return
	}
	if err := p.m.Send(p.peer, "answer", "sdp", desc); err != nil {
		p.m.logf("send answer to %s: %v", p.peer, err)
	}
}

func (p *inPC) isDead() bool {
	select {
	case <-p.dead:
		return true
	default:
		return false
	}
}

// ---------------------------------------------------------------------------
// signaling dispatch
// ---------------------------------------------------------------------------

// SignalIn receives one relayed signaling frame from the hub. The
// side tag routes the payload: "offer" frames belong to the PC I
// answer (the sender offered it), "answer" frames belong to the PC I
// offered. Frames arrive on the hubclient read loop, in order — the
// per-PC state machines rely on that ordering.
func (m *Manager) SignalIn(from, typ, side string, body json.RawMessage) {
	switch typ {
	case "sdp", "ice":
	default:
		return
	}
	switch side {
	case "offer", "answer":
	default:
		m.logf("signal %q from %s: bad side %q", typ, from, side)
		return
	}
	if typ == "sdp" && side == "offer" {
		m.handleRemoteOffer(from, body)
		return
	}
	if typ == "sdp" && side == "answer" {
		if p := m.lookupOut(from); p != nil {
			p.handleAnswer(body)
		} else {
			m.logf("answer from %s with no outgoing PC", from)
		}
		return
	}
	// ICE (trickle) — unused today (signaling is non-trickle) but
	// routed the same way for forward compatibility.
	if side == "offer" {
		if p := m.lookupIn(from); p != nil {
			m.addICE(p.pc, body, from)
		}
		return
	}
	if p := m.lookupOut(from); p != nil {
		m.addICE(p.pc, body, from)
	}
}

func (m *Manager) addICE(pc *webrtc.PeerConnection, body json.RawMessage, from string) {
	if pc == nil {
		return
	}
	var c webrtc.ICECandidateInit
	if err := json.Unmarshal(body, &c); err != nil {
		m.logf("bad ICE from %s", from)
		return
	}
	_ = pc.AddICECandidate(c)
}

func (m *Manager) handleRemoteOffer(from string, body json.RawMessage) {
	p := m.ensureIn(from)
	p.handleOffer(body)
}

// ensureIn builds the answer-side PC for a peer's first offer.
func (m *Manager) ensureIn(peer string) *inPC {
	m.mu.Lock()
	if m.isClosed() {
		m.mu.Unlock()
		return &inPC{m: m, peer: peer, dead: closedChan()}
	}
	if p, ok := m.in[peer]; ok {
		m.mu.Unlock()
		return p
	}
	p := &inPC{m: m, peer: peer, dead: make(chan struct{})}
	p.mu.Lock() // block handleOffer until the PC is wired
	pc, err := newPC(m.StunURL, peer+" (in)", &p.gather)
	if err != nil {
		p.mu.Unlock()
		m.mu.Unlock()
		p.markDead()
		m.logf("peer connection from %s: %v", peer, err)
		return p
	}
	p.pc = pc
	m.in[peer] = p
	m.mu.Unlock()

	// Every channel the remote opens is one HTTP connection.
	pc.OnDataChannel(func(dc *webrtc.DataChannel) {
		m.pushConn(newConn(dc, peer))
	})
	pc.OnConnectionStateChange(func(st webrtc.PeerConnectionState) {
		logx.Debug("tunnel", "%s (in): connection state %s", peer, st)
		switch st {
		case webrtc.PeerConnectionStateFailed, webrtc.PeerConnectionStateClosed:
			if st == webrtc.PeerConnectionStateFailed {
				logx.Warn("tunnel", "%s (in): ICE failed — no reachable candidate pair (STUN unreachable → no srflx candidates; or symmetric NAT → needs relay)", peer)
			}
			p.kill()
		case webrtc.PeerConnectionStateConnected:
			m.logf("connected to %s (incoming)", peer)
		}
	})
	p.mu.Unlock() // PC ready: offers may be handled
	return p
}

func (m *Manager) lookupOut(peer string) *outPC {
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.out[peer]
}

func (m *Manager) lookupIn(peer string) *inPC {
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.in[peer]
}

// newPC builds one PeerConnection with the tunnel's common settings.
// gather is created here and closed by the OnICECandidate wiring when
// ICE gathering completes. label names the PC in log lines
// ("<peer> (out)" / "<peer> (in)") — the only way to tell which
// direction a debug line belongs to.
func newPC(stunURL, label string, gather *chan struct{}) (*webrtc.PeerConnection, error) {
	se := webrtc.SettingEngine{}
	// Loopback candidates let engines on the same machine (tests,
	// VMs) still connect; ICE ranks them below LAN routes anyway and
	// the mesh protocol stays group-key gated.
	se.SetIncludeLoopbackCandidate(true)
	api := webrtc.NewAPI(webrtc.WithSettingEngine(se))
	cfg := webrtc.Configuration{}
	if stunURL != "" {
		cfg.ICEServers = []webrtc.ICEServer{{URLs: []string{stunURL}}}
	}
	pc, err := api.NewPeerConnection(cfg)
	if err != nil {
		return nil, err
	}
	*gather = make(chan struct{})
	var once sync.Once
	// Candidate tally: with --debug this answers the first question of
	// any remote-NAT diagnosis — were server-reflexive candidates
	// gathered at all? Zero srflx means STUN was unreachable (server
	// down, firewall, VPN) and only private host candidates went out;
	// srflx present but no connection means the NATs won't punch
	// (symmetric — relay fallback territory).
	var nHost, nSrflx, nOther int
	pc.OnICECandidate(func(c *webrtc.ICECandidate) {
		if c == nil { // nil = gathering complete
			once.Do(func() {
				close(*gather)
				logx.Debug("tunnel", "%s: gather complete: %d host, %d srflx, %d other candidates",
					label, nHost, nSrflx, nOther)
			})
			return
		}
		switch c.Typ {
		case webrtc.ICECandidateTypeHost:
			nHost++
		case webrtc.ICECandidateTypeSrflx:
			nSrflx++
		default:
			nOther++
		}
		logx.Debug("tunnel", "%s: candidate %s %s:%d", label, c.Typ, c.Address, c.Port)
	})
	return pc, nil
}

// waitGathered waits for ICE gathering to complete (bounded) and
// returns the local description with every candidate embedded —
// non-trickle signaling: one SDP frame per exchange, no ICE frames.
// After the first exchange, gathering is done for the PC's lifetime,
// so later renegotiations return immediately.
func waitGathered(pc *webrtc.PeerConnection, gather chan struct{}) (webrtc.SessionDescription, error) {
	if gather != nil {
		select {
		case <-gather:
		case <-time.After(gatherTimeout):
			// Diagnosability: SRFLX candidates come from STUN — a
			// gather that never completes means STUN is unreachable
			// (server down, firewall, VPN) and the SDP will carry host
			// candidates only.
			logx.Debug("tunnel", "gather incomplete after %v — STUN slow or unreachable? (SDP carries host candidates only)", gatherTimeout)
		}
	}
	desc := pc.LocalDescription()
	if desc == nil {
		return webrtc.SessionDescription{}, errors.New("no local description")
	}
	return *desc, nil
}

func closedChan() chan struct{} {
	ch := make(chan struct{})
	close(ch)
	return ch
}

// ---------------------------------------------------------------------------
// dcConn — net.Conn over one DataChannel (ordered + reliable)
// ---------------------------------------------------------------------------

// newConn wires the message pump and returns the net.Conn. OnMessage
// is set here — before the exchange can deliver data — so nothing is
// lost if the channel is already open.
func newConn(dc *webrtc.DataChannel, peer string) net.Conn {
	c := &dcConn{
		dc:     dc,
		peer:   peer,
		msgs:   make(chan []byte, msgQueue),
		closed: make(chan struct{}),
	}
	dc.OnMessage(func(m webrtc.DataChannelMessage) {
		select {
		case c.msgs <- m.Data:
		case <-c.closed:
		}
	})
	dc.SetBufferedAmountLowThreshold(lowThreshold)
	low := make(chan struct{}, 1)
	dc.OnBufferedAmountLow(func() {
		select {
		case low <- struct{}{}:
		default:
		}
	})
	c.low = low
	return c
}

// dcConn adapts one DataChannel into net.Conn for net/http on both
// sides of the tunnel.
type dcConn struct {
	dc     *webrtc.DataChannel
	peer   string
	low    chan struct{}
	msgs   chan []byte
	closed chan struct{}
	once   sync.Once

	rmu sync.Mutex
	cur []byte // remainder of the current inbound message
}

// Read returns the inbound byte stream. Message boundaries are not
// preserved (net/http wants a stream) but bytes are ordered and
// complete — the WebRTC channel is ordered+reliable.
func (c *dcConn) Read(p []byte) (int, error) {
	c.rmu.Lock()
	defer c.rmu.Unlock()
	for {
		if len(c.cur) > 0 {
			n := copy(p, c.cur)
			c.cur = c.cur[n:]
			return n, nil
		}
		select {
		case msg := <-c.msgs:
			c.cur = msg
		case <-c.closed:
			// Drain anything already queued before reporting EOF.
			select {
			case msg := <-c.msgs:
				c.cur = msg
			default:
				return 0, io.EOF
			}
		}
	}
}

// Write sends p as one or more DataChannel messages, applying SCTP
// backpressure between chunks.
func (c *dcConn) Write(p []byte) (int, error) {
	select {
	case <-c.closed:
		return 0, net.ErrClosed
	default:
	}
	total := 0
	for len(p) > 0 {
		if err := c.waitDrain(); err != nil {
			return total, err
		}
		n := len(p)
		if n > MsgChunk {
			n = MsgChunk
		}
		if err := c.dc.Send(p[:n]); err != nil {
			return total, err
		}
		p = p[n:]
		total += n
	}
	return total, nil
}

// waitDrain blocks while the SCTP send buffer exceeds the high-water
// mark. The low-water callback may be missed under races, so the wait
// is also polled — cheap, and keeps a stalled callback from hanging a
// transfer forever.
func (c *dcConn) waitDrain() error {
	for c.dc.BufferedAmount() > highWater {
		select {
		case <-c.low:
		case <-time.After(100 * time.Millisecond):
		case <-c.closed:
			return net.ErrClosed
		}
	}
	return nil
}

// Close closes the DataChannel; the peer sees EOF on its side.
func (c *dcConn) Close() error {
	c.once.Do(func() { close(c.closed) })
	if c.dc != nil {
		_ = c.dc.Close()
	}
	return nil
}

func (c *dcConn) LocalAddr() net.Addr  { return Addr{Peer: c.peer} }
func (c *dcConn) RemoteAddr() net.Addr { return Addr{Peer: c.peer} }

// SetDeadline/SetReadDeadline/SetWriteDeadline: deadlines cannot be
// honored on a WebRTC channel without aborting in-flight reads, and
// net/http only uses them when explicitly configured — the tunnel's
// HTTP server/client does not. Documented no-ops.
func (c *dcConn) SetDeadline(t time.Time) error      { return nil }
func (c *dcConn) SetReadDeadline(t time.Time) error  { return nil }
func (c *dcConn) SetWriteDeadline(t time.Time) error { return nil }
