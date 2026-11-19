// Package hubclient is the engine's side of the M2 hub connection:
// one outbound WSS to the hub, hello-authenticated (group tag +
// device-key signature), then presence updates in both directions.
// Everything the engine learns from the hub — who is online with the
// same group key, whether the socket is up — is exposed for the
// dashboard and (later steps) for routing file transfers.
//
// The client is deliberately dumb about backoff: reconnect forever
// with capped exponential delay. The hub is optional infrastructure;
// a missing/unreachable hub only means "no internet mesh", LAN keeps
// working untouched.
package hubclient

import (
	"context"
	"crypto/sha256"
	"crypto/tls"
	"crypto/x509"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/url"
	"strings"
	"sync"
	"time"

	"chunkdrop/internal/device"
	"chunkdrop/internal/hub"
	"chunkdrop/internal/itls"

	"github.com/coder/websocket"
)

// Backoff bounds for the reconnect loop.
const (
	backoffStart = 1 * time.Second
	backoffMax   = 60 * time.Second
	// Keepalive: the client pings the hub so a dead hub is noticed
	// even when nothing else is flowing (pong silence = dead). The
	// hub pings too, so the wire stays busy in both directions.
	PingEvery   = 25 * time.Second
	PingTimeout = 10 * time.Second
	// WriteTimeout bounds every single frame write.
	WriteTimeout = 10 * time.Second
)

// Channel names (Status.Channel, logs, config hub_channel values).
const (
	// ChanTLS is the classic channel: plain wss (TLS on the outside).
	ChanTLS = "tls"
	// ChanWSTLS is the M3 channel: REAL TLS inside a plaintext ws://
	// connection (for MITM networks that intercept the outer
	// handshake).
	ChanWSTLS = "ws-tls"
	// ChanWS is the legacy channel: plaintext ws, no TLS anywhere
	// (local smoke tests against a plain hub).
	ChanWS = "ws"
)

// Peer mirrors hub.Peer — one online remote device.
type Peer = hub.Peer

// Status is the dashboard-facing connection state.
type Status struct {
	URL       string `json:"url"`
	Connected bool   `json:"connected"`
	Channel   string `json:"channel,omitempty"` // active/last-used channel
	LastError string `json:"last_error,omitempty"`
	Devices   []Peer `json:"devices"`
}

// Client is one hub connection with its reconnect loop. Start it and
// forget it: all methods are safe from any goroutine, Stop unwinds.
type Client struct {
	URL      string           // ws:// or wss:// drop.404.mn/hub/ws
	Tag      string           // hex(sha256(group_key))
	Identity *device.Identity // device keys + name
	// OnPresence fires (outside locks) whenever the online-device
	// list changes; nil = ignored.
	OnPresence func([]Peer)
	// OnSignal fires (on the read loop, in order) for each relayed
	// signaling frame: from = authenticated sender device ID, typ =
	// "sdp"/"ice", side = "offer"/"answer", body = opaque pion JSON.
	// nil = dropped. Handlers must not block on signaling replies —
	// the read loop serializes all inbound frames.
	OnSignal func(from, typ, side string, body json.RawMessage)
	// OnRelay fires (on the read loop, in order) for each relayed
	// stream frame (step ③ fallback): from = authenticated sender
	// device ID ("" when the frame is a hub error scoped to a stream),
	// f = the frame with Data/Eof/Ack. nil = dropped. The handler must
	// never block: the read loop also carries presence and signaling.
	OnRelay func(from string, f hub.Relay)
	// Logf renders a line; nil = silent (the caller wires logx).
	Logf func(format string, args ...any)

	// Pins is the hub certificate SPKI pin list ("sha256/<base64>"
	// entries, docs in pin.go). Empty = fall back to the build-time
	// DefaultPin; both empty = standard TLS verification only. Enforced
	// on wss dials in addition to chain verification.
	Pins []string
	// TLSRoots replaces the system root store for hub TLS verification
	// when set (test hook; production dials use the system pool).
	TLSRoots *x509.CertPool
	// Channel is the hub_channel policy: "tls"/"ws-tls" force one
	// channel; empty or "auto" = try the natural channel first, fall
	// back to the other on dial/verification failure.
	Channel string
	// WSURL overrides the derived ws:// endpoint for the inner-TLS
	// channel (config hub_ws_url).
	WSURL string

	mu        sync.Mutex
	lnk       cLink
	connected bool
	activeCh  string // channel in use / last used (dashboard)
	lastGood  string // RAM: channel that last connected (plan order)
	lastErr   string
	devices   []Peer
	cancel    context.CancelFunc
	done      chan struct{}
}

// New builds a client (not started).
func New(url, groupKey string, id *device.Identity, logf func(string, ...any)) *Client {
	sum := sha256.Sum256([]byte(groupKey))
	return &Client{
		URL:      url,
		Tag:      hex.EncodeToString(sum[:]),
		Identity: id,
		Logf:     logf,
	}
}

// Start launches the reconnect loop. Calling Start twice is a no-op.
func (c *Client) Start() {
	c.mu.Lock()
	if c.done != nil {
		c.mu.Unlock()
		return
	}
	ctx, cancel := context.WithCancel(context.Background())
	c.cancel = cancel
	c.done = make(chan struct{})
	c.mu.Unlock()
	go c.loop(ctx)
}

// Stop closes the connection and ends the loop. Idempotent.
func (c *Client) Stop() {
	c.mu.Lock()
	cancel, done := c.cancel, c.done
	c.mu.Unlock()
	if cancel != nil {
		cancel()
	}
	if done != nil {
		<-done
	}
}

// Status snapshots the connection state for the dashboard.
func (c *Client) Status() Status {
	c.mu.Lock()
	defer c.mu.Unlock()
	devs := make([]Peer, len(c.devices))
	copy(devs, c.devices)
	return Status{
		URL:       c.URL,
		Connected: c.connected,
		Channel:   c.activeCh,
		LastError: c.lastErr,
		Devices:   devs,
	}
}

// Connected reports whether the hub socket is currently up.
func (c *Client) Connected() bool {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.connected
}

func (c *Client) logf(format string, args ...any) {
	if c.Logf != nil {
		c.Logf(format, args...)
	}
}

// loop connects, serves the connection, and retries with capped
// exponential backoff until Stop.
func (c *Client) loop(ctx context.Context) {
	defer close(c.done)
	backoff := time.Duration(0)
	for {
		if ctx.Err() != nil {
			return
		}
		if backoff > 0 {
			select {
			case <-ctx.Done():
				return
			case <-time.After(backoff):
			}
		}
		err := c.serve(ctx)
		c.setConnected(false, err)
		if ctx.Err() != nil {
			return
		}
		if err != nil {
			c.logf("hub connection lost: %v", err)
		}
		backoff *= 2
		if backoff < backoffStart {
			backoff = backoffStart
		}
		if backoff > backoffMax {
			backoff = backoffMax
		}
	}
}

// serve runs one connection attempt to completion: walk the channel
// plan (auto: fallback on dial/verification failure only), serve the
// first channel that connects.
func (c *Client) serve(ctx context.Context) error {
	dialCtx, cancel := context.WithTimeout(ctx, 10*time.Second)
	var lnk cLink
	var active string
	var dialErr error
	for _, ch := range c.plan() {
		l, err := c.dialChannel(dialCtx, ctx, ch)
		if err == nil {
			lnk, active = l, ch
			break
		}
		if dialErr == nil {
			dialErr = err
		} else {
			c.logf("hub channel %s failed: %v", ch, err)
		}
		if !mayFallback(ch, err) {
			break
		}
	}
	cancel()
	if lnk == nil {
		return fmt.Errorf("dial: %w", dialErr)
	}
	return c.serveLink(ctx, lnk, active)
}

// mayFallback decides whether a failed channel may hand over to the
// next one. The rule is security-first: never drop to a WEAKER
// channel because something TLS-shaped went wrong — that is exactly
// what an active attacker wants.
//
//   - tls (outer wss) → any failure may try ws-tls: the tunnel
//     enforces the same roots and pins, so the fallback cannot
//     weaken verification.
//   - ws-tls → ONLY a marker rejection (ErrMarkerRejected: the hub
//     answered on the plaintext channel — old hub or feature off,
//     nothing TLS ever happened). A pin mismatch or any other
//     TLS-level failure stops here and surfaces loudly: falling back
//     to plaintext ws would hand the traffic to whoever interfered.
//   - ws (legacy plaintext) is the last entry already.
func mayFallback(ch string, err error) bool {
	switch ch {
	case ChanTLS:
		return true
	case ChanWSTLS:
		return errors.Is(err, itls.ErrMarkerRejected)
	}
	return false
}

// plan returns the ordered channel attempts for one connection. A
// forced Channel is a single entry; auto tries the natural channel
// first (wss URLs: outside TLS; ws:// URLs: the inner-TLS channel),
// with the channel that last worked (RAM only) moved to the front so
// a healthy connection never double-dials.
func (c *Client) plan() []string {
	switch c.Channel {
	case ChanTLS:
		return []string{ChanTLS}
	case ChanWSTLS:
		return []string{ChanWSTLS}
	}
	var order []string
	if strings.HasPrefix(strings.ToLower(c.URL), "wss://") {
		order = []string{ChanTLS, ChanWSTLS}
	} else {
		order = []string{ChanWSTLS, ChanWS}
	}
	c.mu.Lock()
	good := c.lastGood
	c.mu.Unlock()
	for i := 1; i < len(order); i++ {
		if order[i] == good {
			order[0], order[i] = order[i], order[0]
			break
		}
	}
	return order
}

// dialChannel performs one channel's dial: dialCtx bounds the dial
// (and, for the inner-TLS channel, the handshake), lifetime is the
// connection-lifetime context the transport keeps riding on.
func (c *Client) dialChannel(dialCtx, lifetime context.Context, ch string) (cLink, error) {
	switch ch {
	case ChanTLS:
		if !strings.HasPrefix(strings.ToLower(c.URL), "wss://") {
			return nil, fmt.Errorf("tls channel needs a wss:// hub_url, got %s", c.URL)
		}
		ws, err := c.dial(dialCtx)
		if err != nil {
			return nil, err
		}
		return &clientWsLink{ws: ws}, nil
	case ChanWSTLS:
		return c.dialItls(dialCtx, lifetime)
	case ChanWS:
		ws, _, err := websocket.Dial(dialCtx, c.wsURL(), &websocket.DialOptions{
			HTTPHeader: nil,
		})
		if err != nil {
			return nil, err
		}
		ws.SetReadLimit(hub.MaxReadLimit)
		return &clientWsLink{ws: ws}, nil
	}
	return nil, fmt.Errorf("unknown hub channel %q", ch)
}

// dialItls opens the M3 inner-TLS channel: plain ws:// to the derived
// endpoint, the itls1 marker, then a REAL TLS handshake inside the
// tunnel — the genuine hub certificate arrives through it and is
// verified like any TLS connection (roots + hub_cert_pin). The TLS
// identity (ServerName) is the hub_url host, not the ws endpoint
// host.
func (c *Client) dialItls(dialCtx, lifetime context.Context) (cLink, error) {
	wsURL := c.wsURL()
	ws, _, err := websocket.Dial(dialCtx, wsURL, &websocket.DialOptions{
		HTTPHeader: nil,
	})
	if err != nil {
		return nil, fmt.Errorf("ws dial %s: %w", wsURL, err)
	}
	ws.SetReadLimit(hub.MaxReadLimit)
	defer func() {
		if err != nil {
			_ = ws.CloseNow()
		}
	}()

	pins, err := c.activePins()
	if err != nil {
		return nil, err
	}
	cfg := &tls.Config{
		// ServerName is the hub_url host: the certificate is issued
		// for the site, even though the tunnel rides ws:// on :80.
		ServerName: c.tlsName(),
		MinVersion: tls.VersionTLS12,
	}
	if c.TLSRoots != nil {
		cfg.RootCAs = c.TLSRoots
	}
	if len(pins) > 0 {
		cfg.VerifyPeerCertificate = pinVerifier(pins)
	}
	tlsConn, err := itls.Client(lifetime, ws, cfg)
	if err != nil {
		return nil, err
	}
	return &clientNDJSONLink{
		tlsConn: tlsConn,
		sc:      itls.NewFrameScanner(tlsConn, hub.MaxReadLimit),
		outer:   ws,
	}, nil
}

// wsURL derives the plaintext ws endpoint for the inner-TLS channel:
// explicit override (WSURL / hub_ws_url) wins; otherwise the same
// host and path as hub_url with the scheme swapped and :443 dropped
// (ws implies port 80).
func (c *Client) wsURL() string {
	if c.WSURL != "" {
		return c.WSURL
	}
	u, err := url.Parse(c.URL)
	if err != nil {
		return c.URL
	}
	u.Scheme = "ws"
	if u.Port() == "443" {
		// Rebuild the host without the port (keep IPv6 brackets).
		host := u.Hostname()
		if strings.Contains(host, ":") {
			host = "[" + host + "]"
		}
		u.Host = host
	}
	return u.String()
}

// tlsName is the TLS identity the hub certificate must match: the
// host of hub_url, never the (possibly overridden) ws endpoint host.
func (c *Client) tlsName() string {
	if u, err := url.Parse(c.URL); err == nil {
		return u.Hostname()
	}
	return ""
}

// serveLink runs one live connection to completion on the given
// channel: hello, presence loop, keepalive.
func (c *Client) serveLink(ctx context.Context, lnk cLink, ch string) error {
	defer lnk.closeNow()

	if err := c.hello(lnk); err != nil {
		return err
	}
	c.mu.Lock()
	c.lnk = lnk
	c.connected = true
	c.lastErr = ""
	c.activeCh = ch
	c.lastGood = ch
	c.mu.Unlock()
	c.logf("hub: connected via %s", ch)

	// Keepalive: ping the hub so a dead hub is noticed even when
	// nothing else is flowing (pong silence = dead). Both channels
	// ping the OUTER websocket — on the inner-TLS channel the hub's
	// transport keeps answering pongs inside its ws.Read.
	serveCtx, cancel := context.WithCancel(ctx)
	defer cancel()
	go func() {
		t := time.NewTicker(PingEvery)
		defer t.Stop()
		for {
			select {
			case <-serveCtx.Done():
				return
			case <-t.C:
				pctx, cancel := context.WithTimeout(serveCtx, PingTimeout)
				err := lnk.ping(pctx)
				cancel()
				if err != nil {
					lnk.closeNow()
					return
				}
			}
		}
	}()

	// Serve until the socket dies (or Stop cancels ctx — Read
	// unblocks on both). No read deadline: control frames do not
	// terminate Read, so a deadline would kill quiet-but-healthy
	// presence connections every couple of minutes.
	defer func() {
		c.mu.Lock()
		c.lnk = nil
		c.mu.Unlock()
	}()
	for {
		data, err := lnk.read(serveCtx)
		if err != nil {
			return fmt.Errorf("read: %w", err)
		}
		c.handleFrame(data)
	}
}

// dial opens the websocket. On wss endpoints it applies certificate
// pins (hub_cert_pin / build-time DefaultPin) in addition to the
// standard chain verification, and honors TLSRoots instead of the
// system pool when set (test hook). Plain ws:// dials (local smoke,
// later the M3 inner-TLS channel) carry no outer TLS — pins cannot
// apply there, so a pin configured against a ws:// URL is logged and
// ignored rather than silently verified-against-nothing.
func (c *Client) dial(ctx context.Context) (*websocket.Conn, error) {
	opts := &websocket.DialOptions{
		HTTPHeader: nil, // nothing sensitive travels in headers
	}
	if !strings.HasPrefix(strings.ToLower(c.URL), "wss://") {
		if pins, _ := c.activePins(); len(pins) > 0 {
			c.logf("hub_cert_pin set but hub_url is not wss — pin ignored on %s", c.URL)
		}
		ws, _, err := websocket.Dial(ctx, c.URL, opts)
		return ws, err
	}

	pins, err := c.activePins()
	if err != nil {
		return nil, err
	}
	if len(pins) > 0 || c.TLSRoots != nil {
		// Clone the default transport so the proxy behavior of
		// http.DefaultTransport (ProxyFromEnvironment) survives: a
		// pinned dial must still work through HTTPS_PROXY/NO_PROXY.
		tr := cloneDefaultTransport()
		tc := tr.TLSClientConfig
		if tc == nil {
			tc = &tls.Config{}
		} else {
			tc = tc.Clone()
		}
		tc.MinVersion = tls.VersionTLS12
		if c.TLSRoots != nil {
			tc.RootCAs = c.TLSRoots
		}
		if len(pins) > 0 {
			tc.VerifyPeerCertificate = pinVerifier(pins)
		}
		tr.TLSClientConfig = tc
		opts.HTTPClient = &http.Client{Transport: tr}
	}
	ws, _, err := websocket.Dial(ctx, c.URL, opts)
	return ws, err
}

// cloneDefaultTransport copies http.DefaultTransport when it is the
// usual *http.Transport (it always is, but do not assume).
func cloneDefaultTransport() *http.Transport {
	if def, ok := http.DefaultTransport.(*http.Transport); ok {
		return def.Clone()
	}
	return &http.Transport{}
}

// cLink is the client's transport abstraction (mirror of the hub's
// link): either a plain websocket (tls / legacy ws channels) or NDJSON
// over the inner-TLS connection (ws-tls channel). The protocol is
// identical on both — same JSON frames, different framing.
type cLink interface {
	// read returns the next JSON frame. Control/keepalive frames are
	// consumed internally.
	read(ctx context.Context) ([]byte, error)
	// write sends one JSON frame; serialized by the implementation
	// (websocket and tls.Conn both allow one writer at a time).
	write(ctx context.Context, data []byte) error
	// ping sends the keepalive (the outer ws ping on every channel).
	ping(ctx context.Context) error
	// closeNow hard-closes the transport (no close handshake).
	closeNow()
}

// clientWsLink is the plaintext-websocket channel: the tls channel
// (TLS terminated outside) and the legacy ws channel share it.
type clientWsLink struct {
	ws  *websocket.Conn
	wmu sync.Mutex
}

func (l *clientWsLink) read(ctx context.Context) ([]byte, error) {
	for {
		typ, data, err := l.ws.Read(ctx)
		if err != nil {
			return nil, err
		}
		if typ == websocket.MessageText {
			return data, nil
		}
		// Binary after hello: never part of the protocol — ignored.
	}
}

func (l *clientWsLink) write(ctx context.Context, data []byte) error {
	l.wmu.Lock()
	defer l.wmu.Unlock()
	return l.ws.Write(ctx, websocket.MessageText, data)
}

func (l *clientWsLink) ping(ctx context.Context) error { return l.ws.Ping(ctx) }
func (l *clientWsLink) closeNow()                      { _ = l.ws.CloseNow() }

// clientNDJSONLink is the inner-TLS channel: JSON frames as
// newline-delimited lines over the tls.Conn that rides inside the
// outer websocket. Keepalive rides the outer socket (ws ping/pong).
type clientNDJSONLink struct {
	tlsConn *tls.Conn
	sc      *itls.FrameScanner
	outer   *websocket.Conn
	wmu     sync.Mutex
}

func (l *clientNDJSONLink) read(ctx context.Context) ([]byte, error) {
	// The scanner read has no context: cancel unblocks via a hard
	// Close (the transport's deadlines are no-ops — see itls).
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

func (l *clientNDJSONLink) write(_ context.Context, data []byte) error {
	l.wmu.Lock()
	defer l.wmu.Unlock()
	// tls.Conn.Write goes through the itls.Transport, whose ws Write
	// carries its own timeout — a stalled peer cannot wedge this.
	return itls.WriteFrame(l.tlsConn, data)
}

func (l *clientNDJSONLink) ping(ctx context.Context) error { return l.outer.Ping(ctx) }
func (l *clientNDJSONLink) closeNow() {
	_ = l.tlsConn.Close()
	_ = l.outer.CloseNow()
}

// hello sends the authenticated first frame. The signature binds
// tag+id+pub+ts — the hub can verify device-key possession and that
// the ID is the key's fingerprint, without learning the group key
// (the tag is only a hash of it).
func (c *Client) hello(lnk cLink) error {
	pub := base64.StdEncoding.EncodeToString(c.Identity.Pub)
	ts := time.Now().Unix()
	sig := c.Identity.Sign(hub.HelloCanonical(c.Tag, c.Identity.ID, pub, ts))
	frame, err := json.Marshal(hub.Hello{
		Type: "hello", Tag: c.Tag, ID: c.Identity.ID, Name: c.Identity.Name,
		Pub: pub, TS: ts, Sig: base64.StdEncoding.EncodeToString(sig),
	})
	if err != nil {
		return err
	}
	wctx, cancel := context.WithTimeout(context.Background(), WriteTimeout)
	defer cancel()
	return lnk.write(wctx, frame)
}

// SendSignal sends one signaling frame to a device via the hub:
// to = target device ID, side = "offer"/"answer" (which of the
// sender's PCs the payload belongs to), typ = "sdp"/"ice", body =
// opaque pion JSON. Safe from any goroutine; fails when the socket
// is down (callers retry through their own state machines).
func (c *Client) SendSignal(to, side, typ string, body any) error {
	raw, err := json.Marshal(body)
	if err != nil {
		return fmt.Errorf("hub: signal body: %w", err)
	}
	frame, err := json.Marshal(hub.Signal{Type: typ, To: to, Side: side, Body: raw})
	if err != nil {
		return err
	}
	c.mu.Lock()
	lnk := c.lnk
	c.mu.Unlock()
	if lnk == nil {
		return fmt.Errorf("hub: not connected")
	}
	wctx, cancel := context.WithTimeout(context.Background(), WriteTimeout)
	defer cancel()
	return lnk.write(wctx, frame)
}

// SendRelay sends one stream frame to a device via the hub (step ③
// fallback relay): to = target device ID, f = the frame (Sid, Data/
// Eof/Ack). Serialized with every other client write (one writer per
// socket); fails when the socket is down — stream callers treat that
// as stream death.
func (c *Client) SendRelay(to string, f hub.Relay) error {
	f.Type = "relay"
	f.To = to
	f.From = "" // hub-authoritative; never client-claimed
	frame, err := json.Marshal(f)
	if err != nil {
		return err
	}
	c.mu.Lock()
	lnk := c.lnk
	c.mu.Unlock()
	if lnk == nil {
		return fmt.Errorf("hub: not connected")
	}
	wctx, cancel := context.WithTimeout(context.Background(), WriteTimeout)
	defer cancel()
	return lnk.write(wctx, frame)
}

// handleFrame processes one server frame. Unknown types are ignored
// (forward compatibility: newer hub + older engine).
func (c *Client) handleFrame(data []byte) {
	var m struct {
		Type string `json:"type"`
	}
	if err := json.Unmarshal(data, &m); err != nil {
		return
	}
	switch m.Type {
	case "presence":
		var p hub.Presence
		if err := json.Unmarshal(data, &p); err != nil {
			return
		}
		c.mu.Lock()
		c.devices = p.Devices
		c.mu.Unlock()
		if c.OnPresence != nil {
			c.OnPresence(p.Devices)
		}
	case "sdp", "ice":
		if c.OnSignal == nil {
			return
		}
		var s hub.Signal
		if err := json.Unmarshal(data, &s); err != nil {
			return
		}
		c.OnSignal(s.From, s.Type, s.Side, s.Body)
	case "relay":
		if c.OnRelay == nil {
			return
		}
		var f hub.Relay
		if err := json.Unmarshal(data, &f); err != nil {
			return
		}
		c.OnRelay(f.From, f)
	case "error":
		var e hub.Error
		if err := json.Unmarshal(data, &e); err == nil {
			if e.Sid != 0 && c.OnRelay != nil {
				// Stream-scoped: the hub could not deliver (target
				// offline/stalled). Route it to the stream layer.
				c.OnRelay("", hub.Relay{Type: "error", Sid: e.Sid})
				return
			}
			c.logf("hub error: %s", e.Error)
		}
	}
}

func (c *Client) setConnected(v bool, lastErr error) {
	c.mu.Lock()
	c.connected = v
	if lastErr != nil {
		c.lastErr = lastErr.Error()
	} else if v {
		c.lastErr = ""
	}
	c.mu.Unlock()
}
