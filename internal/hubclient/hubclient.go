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
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"sync"
	"time"

	"chunkdrop/internal/device"
	"chunkdrop/internal/hub"

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

// Peer mirrors hub.Peer — one online remote device.
type Peer = hub.Peer

// Status is the dashboard-facing connection state.
type Status struct {
	URL       string `json:"url"`
	Connected bool   `json:"connected"`
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
	// Logf renders a line; nil = silent (the caller wires logx).
	Logf func(format string, args ...any)

	mu        sync.Mutex
	conn      *websocket.Conn
	wmu       sync.Mutex // one writer at a time (hello, frames, pings)
	connected bool
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

// serve runs one connection attempt to completion.
func (c *Client) serve(ctx context.Context) error {
	dialCtx, cancel := context.WithTimeout(ctx, 10*time.Second)
	ws, _, err := websocket.Dial(dialCtx, c.URL, &websocket.DialOptions{
		HTTPHeader: nil, // nothing sensitive travels in headers
	})
	cancel()
	if err != nil {
		return fmt.Errorf("dial: %w", err)
	}
	// Always hard-close on the way out, before the loop redials:
	// without this, a dead-but-unclosed socket would linger and the
	// hub's replace path would have to kill it for us.
	defer ws.CloseNow()
	ws.SetReadLimit(8 << 10) // same cap as the hub's

	if err := c.hello(ws); err != nil {
		return err
	}
	c.setConnected(true, nil)

	// Keepalive: ping the hub on a timer; pong silence → close the
	// socket, which also unblocks the read loop below.
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
				err := ws.Ping(pctx)
				cancel()
				if err != nil {
					_ = ws.CloseNow()
					return
				}
			}
		}
	}()

	// Serve until the socket dies (or Stop cancels ctx — Read
	// unblocks on both). No read deadline: control frames do not
	// terminate Read, so a deadline would kill quiet-but-healthy
	// presence connections every couple of minutes.
	for {
		typ, data, err := ws.Read(serveCtx)
		if err != nil {
			return fmt.Errorf("read: %w", err)
		}
		if typ == websocket.MessageText {
			c.handleFrame(data)
		}
	}
}

// hello sends the authenticated first frame. The signature binds
// tag+id+pub+ts — the hub can verify device-key possession and that
// the ID is the key's fingerprint, without learning the group key
// (the tag is only a hash of it).
func (c *Client) hello(ws *websocket.Conn) error {
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
	c.wmu.Lock()
	wctx, cancel := context.WithTimeout(context.Background(), WriteTimeout)
	err = ws.Write(wctx, websocket.MessageText, frame)
	cancel()
	c.wmu.Unlock()
	return err
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
	case "error":
		var e hub.Error
		if err := json.Unmarshal(data, &e); err == nil {
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
