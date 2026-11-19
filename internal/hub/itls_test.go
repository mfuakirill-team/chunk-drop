package hub

import (
	"context"
	"crypto/ecdsa"
	"crypto/ed25519"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/sha256"
	"crypto/tls"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"encoding/pem"
	"errors"
	"math/big"
	"net"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"chunkdrop/internal/itls"

	"github.com/coder/websocket"
)

var (
	errNoCert      = errors.New("no certificate presented")
	errPinMismatch = errors.New("inner TLS pin mismatch")
)

// spkiHex is the pin fingerprint used by the test verifier:
// hex(sha256(SPKI DER)).
func spkiHex(der []byte) string {
	sum := sha256.Sum256(der)
	return hex.EncodeToString(sum[:])
}

// spkiPin computes the same fingerprint from a test certificate.
func spkiPin(t *testing.T, pair tls.Certificate) string {
	t.Helper()
	leaf, err := x509.ParseCertificate(pair.Certificate[0])
	if err != nil {
		t.Fatal(err)
	}
	der, err := x509.MarshalPKIXPublicKey(leaf.PublicKey)
	if err != nil {
		t.Fatal(err)
	}
	return spkiHex(der)
}

// testPair mints a throwaway self-signed cert (ECDSA P-256: valid in
// every TLS version Go speaks) with httptest-friendly SANs.
func testCertShared(t *testing.T) (tls.Certificate, *x509.CertPool) {
	t.Helper()
	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	tmpl := x509.Certificate{
		SerialNumber:          big.NewInt(1),
		Subject:               pkix.Name{CommonName: "chunkdrop-hub-test"},
		NotBefore:             time.Now().Add(-time.Hour),
		NotAfter:              time.Now().Add(24 * time.Hour),
		KeyUsage:              x509.KeyUsageDigitalSignature | x509.KeyUsageCertSign,
		ExtKeyUsage:           []x509.ExtKeyUsage{x509.ExtKeyUsageServerAuth},
		BasicConstraintsValid: true,
		IsCA:                  true,
		DNSNames:              []string{"localhost"},
		IPAddresses:           []net.IP{net.IPv4(127, 0, 0, 1), net.IPv6loopback},
	}
	der, err := x509.CreateCertificate(rand.Reader, &tmpl, &tmpl, &key.PublicKey, key)
	if err != nil {
		t.Fatal(err)
	}
	keyDER, err := x509.MarshalECPrivateKey(key)
	if err != nil {
		t.Fatal(err)
	}
	certPEM := pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: der})
	keyPEM := pem.EncodeToMemory(&pem.Block{Type: "EC PRIVATE KEY", Bytes: keyDER})
	pair, err := tls.X509KeyPair(certPEM, keyPEM)
	if err != nil {
		t.Fatal(err)
	}
	pool := x509.NewCertPool()
	pool.AddCert(&x509.Certificate{Raw: der})
	return pair, pool
}

// The inner-TLS channel (M3): a client that sends the itls1 marker
// gets REAL TLS inside its plaintext websocket and continues the
// identical hub protocol as NDJSON. The full stack is tested: cert +
// pin verification, hello, presence, and signal relay over the
// tunnel.

func itlsHub(t *testing.T) (url string, srv *httptest.Server, hub1 *Server, pool *x509.CertPool, pin string) {
	t.Helper()
	pair, pool := testCertShared(t)
	hub1 = New()
	hub1.ITLS = &tls.Config{Certificates: []tls.Certificate{pair}, MinVersion: tls.VersionTLS12}
	srv = httptest.NewServer(hub1.Handler())
	t.Cleanup(func() { hub1.Close(); srv.Close() })
	return "ws" + strings.TrimPrefix(srv.URL, "http") + "/hub/ws", srv, hub1, pool, spkiPin(t, pair)
}

// itlsClient is one inner-TLS test client: the TLS connection plus a
// single NDJSON scanner (a per-call scanner would buffer ahead and
// lose frames between reads).
type itlsClient struct {
	conn *tls.Conn
	sc   *itls.FrameScanner
}

func (c *itlsClient) read(t *testing.T) []byte {
	t.Helper()
	type res struct {
		line []byte
		err  error
	}
	ch := make(chan res, 1)
	go func() {
		line, err := c.sc.Next()
		ch <- res{line, err}
	}()
	select {
	case r := <-ch:
		if r.err != nil {
			t.Fatalf("read frame: %v", r.err)
		}
		return r.line
	case <-time.After(5 * time.Second):
		t.Fatal("read frame: timeout")
		return nil
	}
}

func (c *itlsClient) write(t *testing.T, frame []byte) {
	t.Helper()
	if err := itls.WriteFrame(c.conn, frame); err != nil {
		t.Fatalf("write frame: %v", err)
	}
}

// itlsDial connects over the inner-TLS channel and sends the hello
// (marker + TLS + NDJSON).
func (d *testDevice) itlsDial(t *testing.T, url, pin string, pool *x509.CertPool) *itlsClient {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	ws, _, err := websocket.Dial(ctx, url, nil)
	if err != nil {
		t.Fatalf("dial: %v", err)
	}
	t.Cleanup(func() { _ = ws.CloseNow() })
	cfg := &tls.Config{
		ServerName: "127.0.0.1",
		RootCAs:    pool,
		MinVersion: tls.VersionTLS12,
		VerifyPeerCertificate: func(rawCerts [][]byte, _ [][]*x509.Certificate) error {
			if len(rawCerts) == 0 {
				return errNoCert
			}
			c, err := x509.ParseCertificate(rawCerts[0])
			if err != nil {
				return err
			}
			der, err := x509.MarshalPKIXPublicKey(c.PublicKey)
			if err != nil {
				return err
			}
			if spkiHex(der) != pin {
				return errPinMismatch
			}
			return nil
		},
	}
	tlsConn, err := itls.Client(context.Background(), ws, cfg)
	if err != nil {
		t.Fatalf("inner TLS handshake: %v", err)
	}
	t.Cleanup(func() { _ = tlsConn.Close() })

	h := Hello{
		Type: "hello", Tag: d.tag, ID: d.id, Name: d.name,
		Pub: d.pub, TS: time.Now().Unix(),
	}
	h.Sig = base64.StdEncoding.EncodeToString(
		ed25519.Sign(d.priv, HelloCanonical(h.Tag, h.ID, h.Pub, h.TS)))
	frame, _ := json.Marshal(h)
	if err := itls.WriteFrame(tlsConn, frame); err != nil {
		t.Fatalf("hello write: %v", err)
	}
	return &itlsClient{conn: tlsConn, sc: itls.NewFrameScanner(tlsConn, MaxReadLimit)}
}

// readFrame reads one NDJSON frame with a deadline.
func readFrame(t *testing.T, c *itlsClient) []byte { return c.read(t) }

// TestInnerTLSFullStack: hello + presence + signal relay over the
// inner-TLS channel, with pin verification on the client.
func TestInnerTLSFullStack(t *testing.T) {
	url, _, hub1, pool, pin := itlsHub(t)

	d1 := newTestDevice(t, "group-itls", "engine-itls-a")
	d2 := newTestDevice(t, "group-itls", "engine-itls-b")

	c1 := d1.itlsDial(t, url, pin, pool)
	line := readFrame(t, c1)
	var p Presence
	if err := json.Unmarshal(line, &p); err != nil {
		t.Fatalf("presence frame: %v (%s)", err, line)
	}
	if len(p.Devices) != 1 || p.Devices[0].ID != d1.id {
		t.Fatalf("own presence = %s", line)
	}

	// A bad pin must abort the handshake, loudly.
	ws, _, err := websocket.Dial(context.Background(), url, nil)
	if err != nil {
		t.Fatal(err)
	}
	defer ws.CloseNow()
	badCfg := &tls.Config{
		ServerName: "127.0.0.1",
		RootCAs:    pool,
		MinVersion: tls.VersionTLS12,
		VerifyPeerCertificate: func(rawCerts [][]byte, _ [][]*x509.Certificate) error {
			return errPinMismatch // never accept
		},
	}
	if _, err := itls.Client(context.Background(), ws, badCfg); err == nil {
		t.Fatal("inner handshake with a mismatching pin succeeded")
	}

	// Second device joins; c1 sees both via presence.
	c2 := d2.itlsDial(t, url, pin, pool)
	line = readFrame(t, c1)
	if err := json.Unmarshal(line, &p); err != nil {
		t.Fatalf("presence frame 2: %v (%s)", err, line)
	}
	if len(p.Devices) != 2 {
		t.Fatalf("presence after join = %s", line)
	}
	// c2's own join broadcast precedes anything relayed to it.
	line2 := readFrame(t, c2)
	if err := json.Unmarshal(line2, &p); err != nil || len(p.Devices) != 2 {
		t.Fatalf("c2 presence = %s (%v)", line2, p)
	}

	// Signaling relay through the tunnel.
	sig := Signal{Type: "sdp", To: d2.id, Side: "offer", Body: json.RawMessage(`{"sdp":"x"}`)}
	frame, _ := json.Marshal(sig)
	c1.write(t, frame)
	var s Signal
	if err := json.Unmarshal(readFrame(t, c2), &s); err != nil {
		t.Fatalf("relay frame: %v", err)
	}
	if s.From != d1.id || s.Side != "offer" {
		t.Fatalf("relay = %+v", s)
	}
	if hub1.ITLS == nil {
		t.Fatal("ITLS lost")
	}
}

// TestInnerTLSDisabled: without the hub's ITLS config, a marker
// client gets a plain-ws error frame and a close — legacy behavior
// untouched.
func TestInnerTLSDisabled(t *testing.T) {
	hub1 := New()
	srv := httptest.NewServer(hub1.Handler())
	defer func() { hub1.Close(); srv.Close() }()
	url := hubURL(t, srv)

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	ws, _, err := websocket.Dial(ctx, url, nil)
	if err != nil {
		t.Fatal(err)
	}
	defer ws.CloseNow()
	if err := itls.SendMarker(ctx, ws); err != nil {
		t.Fatal(err)
	}
	typ, data, err := ws.Read(ctx)
	if err != nil {
		t.Fatalf("expected an error frame before close: %v", err)
	}
	if typ != websocket.MessageText || !strings.Contains(string(data), "inner TLS disabled") {
		t.Fatalf("got %v %s", typ, data)
	}
	// Then the connection must die (reject = kill).
	if _, _, err := ws.Read(ctx); err == nil {
		t.Fatal("connection stayed open after the rejection")
	}
}
