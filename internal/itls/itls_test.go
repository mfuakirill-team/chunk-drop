package itls

import (
	"bytes"
	"context"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/tls"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/pem"
	"errors"
	"io"
	"math/big"
	"net"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/coder/websocket"
)

// testCert mints a throwaway self-signed certificate (ECDSA P-256:
// valid in every TLS version Go speaks) with the httptest-friendly
// SANs.
func testCert(t *testing.T) (tls.Certificate, *x509.CertPool) {
	t.Helper()
	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	tmpl := x509.Certificate{
		SerialNumber:          big.NewInt(1),
		Subject:               pkix.Name{CommonName: "chunkdrop-test"},
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

// itlsEchoServer serves one inner-TLS connection that echoes every
// NDJSON frame back (full handshake + framing over a real ws pair).
func itlsEchoServer(t *testing.T, cert tls.Certificate) *httptest.Server {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		ws, err := websocket.Accept(w, r, nil)
		if err != nil {
			return
		}
		defer ws.CloseNow()
		typ, data, err := ws.Read(r.Context())
		if err != nil || !IsMarker(typ, data) {
			return
		}
		tr := NewTransport(r.Context(), ws)
		tlsConn, err := Server(tr, &tls.Config{Certificates: []tls.Certificate{cert}})
		if err != nil {
			return
		}
		sc := NewFrameScanner(tlsConn, 64<<10)
		for {
			line, err := sc.Next()
			if err != nil {
				return
			}
			if err := WriteFrame(tlsConn, line); err != nil {
				return
			}
		}
	}))
	t.Cleanup(srv.Close)
	return srv
}

func dialWS(t *testing.T, url string) *websocket.Conn {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	ws, _, err := websocket.Dial(ctx, url, nil)
	if err != nil {
		t.Fatal(err)
	}
	return ws
}

// TestClientMarkerRejected: a hub that answers the marker with a TEXT
// error frame on the plaintext channel (feature off or an older hub)
// yields ErrMarkerRejected — the sentinel callers may fall back on.
// The client's ClientHello races the rejection (the hub may answer
// before or after it); both orders must end in the same sentinel.
func TestClientMarkerRejected(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		ws, err := websocket.Accept(w, r, nil)
		if err != nil {
			return
		}
		defer ws.CloseNow()
		typ, data, err := ws.Read(r.Context())
		if err != nil || !IsMarker(typ, data) {
			return
		}
		// Reject exactly like the hub does: text error frame, close.
		wctx, cancel := context.WithTimeout(r.Context(), 2*time.Second)
		_ = ws.Write(wctx, websocket.MessageText,
			[]byte(`{"type":"error","error":"inner TLS disabled on this hub"}`))
		cancel()
		_ = ws.CloseNow()
	}))
	defer srv.Close()

	ws := dialWS(t, "ws"+strings.TrimPrefix(srv.URL, "http")+"/hub/ws")
	defer ws.CloseNow()
	_, err := Client(context.Background(), ws, &tls.Config{ServerName: "127.0.0.1"})
	if !errors.Is(err, ErrMarkerRejected) {
		t.Fatalf("err = %v, want ErrMarkerRejected", err)
	}
	if !strings.Contains(err.Error(), "inner TLS disabled") {
		t.Fatalf("error should carry the hub's message: %v", err)
	}
}

// TestHandshakeAndNDJSON: marker → inner TLS handshake → NDJSON
// round-trip, with the client verifying the certificate against a
// pin (the real verification path, not InsecureSkipVerify).
func TestHandshakeAndNDJSON(t *testing.T) {
	cert, pool := testCert(t)
	srv := itlsEchoServer(t, cert)

	ws := dialWS(t, "ws"+strings.TrimPrefix(srv.URL, "http")+"/hub/ws")
	defer ws.CloseNow()

	// Pin: sha256 of the certificate's SPKI.
	leaf, err := x509.ParseCertificate(cert.Certificate[0])
	if err != nil {
		t.Fatal(err)
	}
	spki, err := x509.MarshalPKIXPublicKey(leaf.PublicKey)
	if err != nil {
		t.Fatal(err)
	}

	cfg := &tls.Config{
		ServerName: "127.0.0.1",
		RootCAs:    pool,
		MinVersion: tls.VersionTLS12,
		VerifyPeerCertificate: func(rawCerts [][]byte, _ [][]*x509.Certificate) error {
			parsed, err := x509.ParseCertificate(rawCerts[0])
			if err != nil {
				return err
			}
			got, err := x509.MarshalPKIXPublicKey(parsed.PublicKey)
			if err != nil {
				return err
			}
			if !bytes.Equal(got, spki) {
				return io.EOF // stand-in for "pin mismatch"
			}
			return nil
		},
	}

	tlsConn, err := Client(context.Background(), ws, cfg)
	if err != nil {
		t.Fatalf("client handshake: %v", err)
	}
	defer tlsConn.Close()

	if err := WriteFrame(tlsConn, []byte(`{"type":"hello","n":1}`)); err != nil {
		t.Fatalf("write: %v", err)
	}
	line, err := NewFrameScanner(tlsConn, 64<<10).Next()
	if err != nil {
		t.Fatalf("read: %v", err)
	}
	if string(line) != `{"type":"hello","n":1}` {
		t.Fatalf("echo = %s", line)
	}

	// Same handshake, wrong pin: must abort, not connect.
	ws2 := dialWS(t, "ws"+strings.TrimPrefix(srv.URL, "http")+"/hub/ws")
	defer ws2.CloseNow()
	badCfg := &tls.Config{
		ServerName: "127.0.0.1",
		RootCAs:    pool,
		MinVersion: tls.VersionTLS12,
		VerifyPeerCertificate: func(rawCerts [][]byte, _ [][]*x509.Certificate) error {
			return io.EOF // stand-in for "pin mismatch" — never accept
		},
	}
	if _, err := Client(context.Background(), ws2, badCfg); err == nil {
		t.Fatal("handshake with a mismatching pin succeeded")
	}
}

// TestTransportByteStream: the transport is an exact, in-order byte
// stream — several writes of different sizes come out whole, and one
// large write (many internal reads on the receiver) arrives complete.
func TestTransportByteStream(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		ws, err := websocket.Accept(w, r, nil)
		if err != nil {
			return
		}
		defer ws.CloseNow()
		typ, data, err := ws.Read(r.Context())
		if err != nil || !IsMarker(typ, data) {
			return
		}
		tr := NewTransport(r.Context(), ws)
		defer tr.Close()
		_, _ = io.Copy(tr, tr) // raw echo (no TLS) — pure transport test
	}))
	defer srv.Close()

	ws := dialWS(t, "ws"+strings.TrimPrefix(srv.URL, "http")+"/hub/ws")
	defer ws.CloseNow()
	ctx := context.Background()
	if err := SendMarker(ctx, ws); err != nil {
		t.Fatal(err)
	}
	tr := NewTransport(ctx, ws)
	defer tr.Close()

	small := []string{"alpha", "beta", "gamma-longer-piece"}
	var want []byte
	for _, s := range small {
		if _, err := tr.Write([]byte(s)); err != nil {
			t.Fatalf("write: %v", err)
		}
		want = append(want, s...)
	}
	// TLS records are ≤ ~16 KiB and crypto/tls emits one underlying
	// Write per record — the realistic max message size. Reassemble a
	// multi-record stream in order.
	for i := 0; i < 20; i++ {
		rec := bytes.Repeat([]byte("chunkdrop"), 900) // ~8.1 KiB
		if _, err := tr.Write(rec); err != nil {
			t.Fatalf("record write: %v", err)
		}
		want = append(want, rec...)
	}

	got := make([]byte, len(want))
	if _, err := io.ReadFull(tr, got); err != nil {
		t.Fatalf("read: %v", err)
	}
	if !bytes.Equal(got, want) {
		t.Fatalf("stream mismatch: got %d bytes", len(got))
	}
}

// TestFrameScanner: blank lines skipped, oversized frames rejected
// instead of buffering without bound.
func TestFrameScanner(t *testing.T) {
	in := "\n{\"a\":1}\n{\"a\":2}\n"
	sc := NewFrameScanner(strings.NewReader(in), 64<<10)
	l1, err := sc.Next()
	if err != nil || string(l1) != `{"a":1}` {
		t.Fatalf("first = %s, %v", l1, err)
	}
	l2, err := sc.Next()
	if err != nil || string(l2) != `{"a":2}` {
		t.Fatalf("second = %s, %v", l2, err)
	}
	if _, err := sc.Next(); err == nil {
		t.Fatal("scanner should end at EOF")
	}

	big := strings.Repeat("x", 20000) + "\n"
	sc2 := NewFrameScanner(strings.NewReader(big), 4096) // limit below the line length
	if _, err := sc2.Next(); err == nil || !strings.Contains(err.Error(), "exceeds") {
		t.Fatalf("oversized frame = %v, want a limit error", err)
	}
}
