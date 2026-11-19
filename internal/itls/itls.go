// Package itls carries the hub's TLS INSIDE a plaintext websocket
// connection (M3 "inner TLS").
//
// Why: on MITM networks the outer wss handshake is intercepted — the
// client sees a foreign certificate and aborts (x509 unknown
// authority, or a pin mismatch). A ws:// connection on port 80 sees
// no TLS on the outside: a middlebox forwards the websocket frames as
// opaque binary payloads. So the real TLS handshake rides INSIDE the
// websocket: the client sends a marker frame, both sides wrap the
// connection in a real tls.Conn, and the genuine hub certificate
// arrives (live, from the real server) inside that tunnel — verified
// against the system roots and hub_cert_pin as usual. The TLS keys
// are negotiated end-to-end, over the middlebox's head.
//
// Framing: after the marker, each raw TLS record travels as one
// websocket binary message (a record is written with a single Write,
// never split across messages; websocket messages arrive in order, so
// the byte stream is exact). Once the handshake completes, the hub
// protocol continues over the inner connection as NDJSON — one JSON
// frame per '\n'-terminated line — instead of websocket text frames.
//
// Threat-model honesty (docs/ARCHITECTURE.md): this is obfuscation,
// not invisibility — a payload-scanning middlebox can still
// fingerprint the inner ClientHello. v1 accepts that.
package itls

import (
	"bufio"
	"context"
	"crypto/tls"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"sync"
	"time"

	"github.com/coder/websocket"
)

// Marker is the first websocket message a client sends to switch the
// connection into inner-TLS mode. Binary frame, exact bytes.
const Marker = "chunkdrop-itls1"

// ErrMarkerRejected means the hub answered the marker on the PLAIN
// websocket — the itls channel is unavailable (disabled, or an older
// hub). This is the only ws-tls failure from which falling back to a
// legacy plaintext channel is safe: it happened before any TLS
// traffic, so nothing was tampered with. A TLS-level failure (pin
// mismatch, handshake tampering) is NOT this error — callers must
// never fall back to plaintext then.
var ErrMarkerRejected = errors.New("itls: hub rejected the inner-TLS marker")

// HandshakeTimeout bounds the inner TLS handshake (both sides). The
// transport's deadlines are no-ops (see Transport), so the timeout is
// enforced with a watchdog instead of a conn deadline.
const HandshakeTimeout = 10 * time.Second

// WriteTimeout bounds one ws message write inside the transport —
// including each TLS record during the handshake and app data.
const WriteTimeout = 10 * time.Second

// SendMarker sends the mode-switch marker (client side, before
// NewTransport).
func SendMarker(ctx context.Context, ws *websocket.Conn) error {
	wctx, cancel := context.WithTimeout(ctx, WriteTimeout)
	defer cancel()
	return ws.Write(wctx, websocket.MessageBinary, []byte(Marker))
}

// IsMarker reports whether a freshly read first message is the
// mode-switch marker.
func IsMarker(typ websocket.MessageType, data []byte) bool {
	return typ == websocket.MessageBinary && string(data) == Marker
}

// Transport adapts one upgraded websocket connection into a
// net.Conn-shaped byte stream:
//
//   - Read: one background goroutine pumps every binary message into
//     an io.Pipe, in order. Non-binary messages are skipped — ws
//     ping/pong keepalive keeps working here (coder/websocket
//     answers pings inside Read), and the protocol data stays binary.
//   - Write: one binary message per Write call. A TLS record is
//     written with a single Write (16 KiB max record, far below the
//     hub's 64 KiB frame limit), so a record is never split across
//     messages.
//   - Deadlines are no-ops: a read deadline would kill
//     quiet-but-healthy connections (liveness is the outer ws
//     pinger's job), and write stalls are already bounded by the ws
//     Write context. This is why the handshake timeout is a watchdog,
//     not a conn deadline — crypto/tls's ctx watcher sets conn
//     deadlines that would silently do nothing here.
//   - ctx is the connection's lifetime (request context on the hub,
//     dial/serve context on the client): cancelling it unblocks both
//     directions.
type Transport struct {
	ws    *websocket.Conn
	ctx   context.Context
	wmu   sync.Mutex
	pr    *io.PipeReader
	pw    *io.PipeWriter
	first chan firstMsg // the pump's one-shot report of the first message
}

// firstMsg is the pump's report of the first message after the marker
// (or of the read error, if the connection died before one arrived).
type firstMsg struct {
	typ  websocket.MessageType
	data []byte
	err  error
}

// NewTransport wraps ws. The marker frame (if any) must already be
// consumed by the caller.
func NewTransport(ctx context.Context, ws *websocket.Conn) *Transport {
	t := &Transport{ws: ws, ctx: ctx, first: make(chan firstMsg, 1)}
	t.pr, t.pw = io.Pipe()
	go t.pump()
	// Un-block the pump if ctx dies while it is stuck in a Pipe write
	// (the TLS side stopped reading): closing the pipe makes that
	// write fail and the goroutine exit.
	go func() {
		<-ctx.Done()
		t.pw.CloseWithError(io.ErrClosedPipe)
	}()
	return t
}

func (t *Transport) pump() {
	reported := false
	for {
		typ, data, err := t.ws.Read(t.ctx)
		if err != nil {
			if !reported {
				t.first <- firstMsg{err: err}
				reported = true
			}
			t.pw.CloseWithError(err)
			return
		}
		if !reported {
			// The FIRST message decides the client channel's fate
			// (text = the hub answered the marker on the plaintext
			// channel → rejection). Reported before the pipe write:
			// the watcher must not wait for a TLS-side reader.
			t.first <- firstMsg{typ: typ, data: data}
			reported = true
		}
		if typ != websocket.MessageBinary {
			continue
		}
		if _, err := t.pw.Write(data); err != nil {
			return
		}
	}
}

// Read implements net.Conn (fed by the pump goroutine).
func (t *Transport) Read(p []byte) (int, error) { return t.pr.Read(p) }

// Write implements net.Conn: one binary ws message per call.
func (t *Transport) Write(p []byte) (int, error) {
	t.wmu.Lock()
	defer t.wmu.Unlock()
	wctx, cancel := context.WithTimeout(t.ctx, WriteTimeout)
	defer cancel()
	if err := t.ws.Write(wctx, websocket.MessageBinary, p); err != nil {
		return 0, err
	}
	return len(p), nil
}

// Close implements net.Conn. tls.Conn.Close (close_notify) lands
// here; the real kill is the ws CloseNow, which also unblocks the
// pump's Read.
func (t *Transport) Close() error {
	t.pr.CloseWithError(io.EOF)
	return t.ws.CloseNow()
}

func (t *Transport) LocalAddr() net.Addr  { return noopAddr{} }
func (t *Transport) RemoteAddr() net.Addr { return noopAddr{} }
func (t *Transport) SetDeadline(time.Time) error {
	return nil // see struct doc: no-op by design
}
func (t *Transport) SetReadDeadline(time.Time) error  { return nil }
func (t *Transport) SetWriteDeadline(time.Time) error { return nil }

// noopAddr is a placeholder address: tls.Conn never dials anything,
// and the ws library does not expose the peer address.
type noopAddr struct{}

func (noopAddr) Network() string { return "itls" }
func (noopAddr) String() string  { return "itls" }

// Server runs the server side of the inner handshake over tr. The
// marker must already be consumed by the caller; cfg must carry the
// hub certificate.
func Server(tr *Transport, cfg *tls.Config) (*tls.Conn, error) {
	c := tls.Server(tr, cfg)
	if err := handshake(c, tr.ctx); err != nil {
		return nil, fmt.Errorf("itls: server handshake: %w", err)
	}
	return c, nil
}

// Client runs the client side of the inner handshake over ws: sends
// the marker, then upgrades to TLS. ctx is the CONNECTION LIFETIME
// (the caller's serve-loop context that outlives the dial — not the
// short dial-deadline context, which is usually cancelled the moment
// the connection is up). cfg must carry ServerName and the trust
// material (roots and/or hub_cert_pin via VerifyPeerCertificate).
//
// TLS semantics: the client speaks FIRST (ClientHello), so the
// handshake starts immediately after the marker; the peer's first
// message is watched concurrently — a TEXT message means the hub
// answered the marker on the plaintext channel (feature off or an
// older hub): the handshake is aborted and the error wraps
// ErrMarkerRejected, the ONLY client failure a caller may fall back
// to legacy channels on (nothing TLS-shaped happened before it). Any
// other failure (pin mismatch, tampering, timeout) is NOT.
func Client(ctx context.Context, ws *websocket.Conn, cfg *tls.Config) (*tls.Conn, error) {
	if err := SendMarker(ctx, ws); err != nil {
		return nil, fmt.Errorf("itls: marker: %w", err)
	}
	tr := NewTransport(ctx, ws)
	c := tls.Client(tr, cfg)

	hsCtx, cancel := context.WithTimeout(ctx, HandshakeTimeout)
	defer cancel()
	hsDone := make(chan error, 1)
	go func() { hsDone <- c.HandshakeContext(hsCtx) }()

	// Watch the peer's first message while the handshake runs. The
	// pump reports it BEFORE piping, so a rejection is seen even
	// though no TLS reader ever consumes bytes.
	var m firstMsg
	select {
	case m = <-tr.first:
	case err := <-hsDone:
		// Handshake ended without any peer message (bad config,
		// instant close). Its result is the answer.
		if err != nil {
			return nil, fmt.Errorf("itls: handshake: %w", err)
		}
		return c, nil
	case <-hsCtx.Done():
		_ = c.Close() // unblocks the handshake goroutine
		<-hsDone
		return nil, fmt.Errorf("itls: handshake timed out after %s", HandshakeTimeout)
	}

	if m.err != nil {
		// The connection died before the peer said anything; the
		// handshake goroutine unblocks on the closed pipe on its own
		// (hsDone is buffered — no leak).
		return nil, fmt.Errorf("itls: after marker: %w", m.err)
	}
	if m.typ != websocket.MessageBinary {
		// The hub answered on the PLAIN websocket: it does not speak
		// inner TLS. Surface its error frame (if parseable).
		_ = c.Close()
		return nil, fmt.Errorf("%w: %s", ErrMarkerRejected, peerError(m.data))
	}
	// Binary = the ServerHello (already piped to the TLS reader).
	if err := <-hsDone; err != nil {
		return nil, fmt.Errorf("itls: handshake: %w", err)
	}
	return c, nil
}

// peerError extracts a readable message from the hub's rejection
// frame ({"type":"error","error":...}) or falls back to a truncated
// raw view.
func peerError(data []byte) string {
	var frame struct {
		Error string `json:"error"`
	}
	if err := json.Unmarshal(data, &frame); err == nil && frame.Error != "" {
		return frame.Error
	}
	s := string(data)
	if len(s) > 120 {
		s = s[:120] + "…"
	}
	return s
}

// handshake runs the TLS handshake with a watchdog: the transport's
// deadlines are no-ops, so ctx-driven deadline plumbing inside
// HandshakeContext cannot work — a goroutine does the handshake and a
// hard Close unblocks it when the watchdog fires.
func handshake(c *tls.Conn, parent context.Context) error {
	ctx, cancel := context.WithTimeout(parent, HandshakeTimeout)
	defer cancel()
	done := make(chan error, 1)
	go func() { done <- c.HandshakeContext(ctx) }()
	select {
	case err := <-done:
		return err
	case <-ctx.Done():
		_ = c.Close() // unblocks the handshake goroutine
		<-done
		return fmt.Errorf("timed out after %s", HandshakeTimeout)
	}
}

// FrameScanner reads NDJSON frames (one JSON object per line) with a
// hard length cap. bufio.Reader.ReadSlice keeps this allocation-free
// in the common case; oversized frames are an error, never unbounded
// memory.
type FrameScanner struct {
	br    *bufio.Reader
	limit int
}

func NewFrameScanner(r io.Reader, limit int) *FrameScanner {
	return &FrameScanner{br: bufio.NewReaderSize(r, 16<<10), limit: limit}
}

// Next returns the next frame's bytes (line, without the trailing
// '\n'). Empty lines are skipped.
func (s *FrameScanner) Next() ([]byte, error) {
	var acc []byte
	for {
		frag, err := s.br.ReadSlice('\n')
		switch {
		case err == bufio.ErrBufferFull:
			acc = append(acc, frag...)
			if len(acc) > s.limit {
				return nil, fmt.Errorf("frame exceeds %d bytes", s.limit)
			}
		case err != nil:
			return nil, err
		default:
			acc = append(acc, frag...)
			line := acc[:len(acc)-1] // strip '\n'
			if len(line) == 0 {
				acc = acc[:0]
				continue // tolerate blank lines
			}
			return line, nil
		}
	}
}

// WriteFrame writes one NDJSON frame (JSON + '\n') in a single Write,
// serialized by the caller's write mutex (tls.Conn is not safe for
// concurrent writes).
func WriteFrame(w io.Writer, data []byte) error {
	buf := make([]byte, len(data)+1)
	copy(buf, data)
	buf[len(data)] = '\n'
	_, err := w.Write(buf)
	return err
}
