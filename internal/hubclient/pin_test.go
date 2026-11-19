package hubclient

import (
	"context"
	"crypto/ed25519"
	"crypto/rand"
	"crypto/sha256"
	"crypto/x509"
	"encoding/base64"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/coder/websocket"
)

// sha256Sum is the byte-slice form of sha256.Sum256 (test helper).
func sha256Sum(b []byte) []byte {
	s := sha256.Sum256(b)
	return s[:]
}

// Pins are an integrity check, so every malformed entry must fail
// loudly (ParsePins) rather than degrade to "no pinning".
func TestParsePins(t *testing.T) {
	good := "sha256/" + base64.StdEncoding.EncodeToString(make([]byte, 32))
	raw, err := ParsePins([]string{good, "  ", ""})
	if err != nil || len(raw) != 1 {
		t.Fatalf("ParsePins(valid) = %v, %v", raw, err)
	}

	for _, bad := range []string{
		"not-a-pin",
		"md5/AAAA",
		"sha256/!!!",
		"sha256/" + base64.StdEncoding.EncodeToString(make([]byte, 16)), // truncated
	} {
		if _, err := ParsePins([]string{bad}); err == nil {
			t.Errorf("ParsePins(%q) accepted a malformed pin", bad)
		}
	}
}

func TestSplitPins(t *testing.T) {
	got := SplitPins(" sha256/AAA ,,, sha256/BBB , ")
	want := []string{"sha256/AAA", "sha256/BBB"}
	if len(got) != len(want) {
		t.Fatalf("SplitPins = %q", got)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Fatalf("SplitPins = %q", got)
		}
	}
}

// echoHandler accepts the upgrade and holds the connection open; the
// tests below only exercise the dial (TLS + pin), not the protocol.
func echoHandler(w http.ResponseWriter, r *http.Request) {
	c, err := websocket.Accept(w, r, nil)
	if err != nil {
		return
	}
	defer c.CloseNow()
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	_, _, _ = c.Read(ctx)
}

// TestPinWSSDial: a pinned wss dial against the real TLS server.
// The pin is checked IN ADDITION to chain verification:
//   - trusted roots + matching pin           → dial succeeds
//   - trusted roots + wrong pin              → loud pin-mismatch abort
//   - untrusted chain (no TLSRoots) + pin    → standard verification fails
func TestPinWSSDial(t *testing.T) {
	srv := httptest.NewTLSServer(http.HandlerFunc(echoHandler))
	defer srv.Close()

	cert := srv.Certificate()
	pool := x509.NewCertPool()
	pool.AddCert(cert)

	sum, err := SPKIHash(cert)
	if err != nil {
		t.Fatal(err)
	}
	goodPin := pinPrefix + base64.StdEncoding.EncodeToString(sum)

	// A wrong pin: hash of some unrelated public key.
	_, priv, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	otherDER, err := x509.MarshalPKIXPublicKey(priv.Public())
	if err != nil {
		t.Fatal(err)
	}
	badPin := pinPrefix + base64.StdEncoding.EncodeToString(sha256Sum(otherDER))

	url := "wss" + strings.TrimPrefix(srv.URL, "https") + "/hub/ws"

	t.Run("matching pin + trusted root", func(t *testing.T) {
		c := New(url, "pin-group", testIdentity(t, "pin-ok"), t.Logf)
		c.Pins = []string{goodPin}
		c.TLSRoots = pool
		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		ws, err := c.dial(ctx)
		if err != nil {
			t.Fatalf("dial with matching pin: %v", err)
		}
		ws.CloseNow()
	})

	t.Run("wrong pin aborts", func(t *testing.T) {
		c := New(url, "pin-group", testIdentity(t, "pin-bad"), t.Logf)
		c.Pins = []string{badPin}
		c.TLSRoots = pool
		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		_, err := c.dial(ctx)
		if err == nil {
			t.Fatal("dial with a wrong pin succeeded")
		}
		if !strings.Contains(err.Error(), "pin mismatch") {
			t.Fatalf("expected a pin-mismatch error, got: %v", err)
		}
	})

	t.Run("untrusted chain still fails", func(t *testing.T) {
		c := New(url, "pin-group", testIdentity(t, "pin-chain"), t.Logf)
		c.Pins = []string{goodPin} // right pin, but the chain is self-signed
		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		if _, err := c.dial(ctx); err == nil {
			t.Fatal("dial against an untrusted chain succeeded despite a matching pin")
		}
	})
}

// TestDefaultPinFallback: with no config pins, the build-time
// DefaultPin applies.
func TestDefaultPinFallback(t *testing.T) {
	srv := httptest.NewTLSServer(http.HandlerFunc(echoHandler))
	defer srv.Close()
	cert := srv.Certificate()
	sum, err := SPKIHash(cert)
	if err != nil {
		t.Fatal(err)
	}

	old := DefaultPin
	defer func() { DefaultPin = old }()

	// Correct baked pin → dial succeeds (roots still needed: the test
	// server's cert is self-signed).
	DefaultPin = pinPrefix + base64.StdEncoding.EncodeToString(sum)
	c := New("wss"+strings.TrimPrefix(srv.URL, "https")+"/hub/ws", "pin-group", testIdentity(t, "pin-baked"), t.Logf)
	c.TLSRoots = func() *x509.CertPool {
		p := x509.NewCertPool()
		p.AddCert(cert)
		return p
	}()
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	ws, err := c.dial(ctx)
	if err != nil {
		t.Fatalf("dial with baked DefaultPin: %v", err)
	}
	ws.CloseNow()

	// Config pins override the baked pin: a mismatched config pin must
	// win over the (correct) baked one.
	DefaultPin = pinPrefix + base64.StdEncoding.EncodeToString(sum)
	_, priv, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	otherDER, err := x509.MarshalPKIXPublicKey(priv.Public())
	if err != nil {
		t.Fatal(err)
	}
	c = New("wss"+strings.TrimPrefix(srv.URL, "https")+"/hub/ws", "pin-group", testIdentity(t, "pin-override"), t.Logf)
	c.Pins = []string{pinPrefix + base64.StdEncoding.EncodeToString(sha256Sum(otherDER))}
	c.TLSRoots = func() *x509.CertPool {
		p := x509.NewCertPool()
		p.AddCert(cert)
		return p
	}()
	ctx2, cancel2 := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel2()
	if _, err := c.dial(ctx2); err == nil || !strings.Contains(err.Error(), "pin mismatch") {
		t.Fatalf("config pins should override the baked pin: %v", err)
	}
}
