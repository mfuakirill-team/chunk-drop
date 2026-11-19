package hubclient

import (
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/tls"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/base64"
	"encoding/pem"
	"math/big"
	"net"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"chunkdrop/internal/hub"
)

// Channel selection (M3 step C): auto fallback between the classic
// wss channel and the inner-TLS ws:// channel, forced modes, and the
// RAM lastGood reorder.

// testCertPair mints a throwaway self-signed cert with httptest-
// friendly SANs (same shape as the hub-side test pair).
func testCertPair(t *testing.T) (tls.Certificate, *x509.CertPool) {
	t.Helper()
	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	tmpl := x509.Certificate{
		SerialNumber:          big.NewInt(1),
		Subject:               pkix.Name{CommonName: "chunkdrop-client-test"},
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
	pair, err := tls.X509KeyPair(
		pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: der}),
		pem.EncodeToMemory(&pem.Block{Type: "EC PRIVATE KEY", Bytes: keyDER}))
	if err != nil {
		t.Fatal(err)
	}
	pool := x509.NewCertPool()
	pool.AddCert(&x509.Certificate{Raw: der})
	return pair, pool
}

func certPin(t *testing.T, pair tls.Certificate) string {
	t.Helper()
	leaf, err := x509.ParseCertificate(pair.Certificate[0])
	if err != nil {
		t.Fatal(err)
	}
	der, err := x509.MarshalPKIXPublicKey(leaf.PublicKey)
	if err != nil {
		t.Fatal(err)
	}
	sum := sha256Sum(der)
	return "sha256/" + base64.StdEncoding.EncodeToString(sum)
}

// innerTLSHub serves the inner-TLS channel on a PLAIN http server
// (exactly the real topology: TLS lives inside the tunnel).
func innerTLSHub(t *testing.T) (wsURL string, pair tls.Certificate, pool *x509.CertPool) {
	t.Helper()
	pair, pool = testCertPair(t)
	hub1 := hub.New()
	hub1.ITLS = &tls.Config{Certificates: []tls.Certificate{pair}, MinVersion: tls.VersionTLS12}
	srv := httptest.NewServer(hub1.Handler())
	t.Cleanup(func() { hub1.Close(); srv.Close() })
	return "ws" + strings.TrimPrefix(srv.URL, "http") + "/hub/ws", pair, pool
}

// TestFallbackToInnerTLS: hub_url says wss://, but the outer TLS
// handshake cannot succeed against this endpoint (here: a plain HTTP
// server — in the field: a MITM proxy). auto must fall back to the
// inner-TLS channel and connect WITH the pin enforced inside the
// tunnel.
func TestFallbackToInnerTLS(t *testing.T) {
	wsURL, pair, pool := innerTLSHub(t)
	// Same host:port, but wss:// — the outer handshake fails.
	wssURL := "wss" + strings.TrimPrefix(wsURL, "ws")

	id := testIdentity(t, "engine-fallback")
	c := New(wssURL, "group-fallback", id, t.Logf)
	c.TLSRoots = pool
	c.Pins = []string{certPin(t, pair)}
	c.Start()
	defer c.Stop()

	waitFor(t, 5*time.Second, "connection via fallback", c.Connected)
	if st := c.Status(); st.Channel != ChanWSTLS {
		t.Fatalf("channel = %q, want ws-tls", st.Channel)
	}

	// lastGood must now front-run the broken channel.
	plan := c.plan()
	if len(plan) != 2 || plan[0] != ChanWSTLS || plan[1] != ChanTLS {
		t.Fatalf("plan after fallback = %v", plan)
	}
}

// TestAutoOnPlainWSFallsBackToLegacy: a plain ws:// hub_url (local
// smoke, no inner TLS configured) tries the inner-TLS channel first,
// gets rejected by the marker-less hub, and lands on the legacy
// plaintext channel.
func TestAutoOnPlainWSFallsBackToLegacy(t *testing.T) {
	url, srv, hub1 := testHub(t)
	defer func() { hub1.Close(); srv.Close() }()

	id := testIdentity(t, "engine-plain")
	c := New(url, "group-plain", id, t.Logf)
	c.Start()
	defer c.Stop()

	waitFor(t, 5*time.Second, "connection on legacy channel", c.Connected)
	if st := c.Status(); st.Channel != ChanWS {
		t.Fatalf("channel = %q, want ws", st.Channel)
	}
	if plan := c.plan(); len(plan) != 2 || plan[0] != ChanWS {
		t.Fatalf("plan after fallback = %v", c.plan())
	}
}

// TestForcedChannel: forced modes never try the other channel.
func TestForcedChannel(t *testing.T) {
	url, srv, hub1 := testHub(t)
	defer func() { hub1.Close(); srv.Close() }()

	id := testIdentity(t, "engine-forced")

	// Forced ws-tls against a marker-less hub: every attempt fails,
	// nothing ever connects, the error is loud.
	c := New(url, "group-forced", id, t.Logf)
	c.Channel = ChanWSTLS
	c.Start()
	time.Sleep(1500 * time.Millisecond)
	if c.Connected() {
		t.Fatal("forced ws-tls connected to a plain hub")
	}
	if st := c.Status(); st.LastError == "" {
		t.Fatal("no lastError for the failing forced channel")
	}
	c.Stop()

	// Forced tls on a ws:// URL: a misconfiguration — the tls channel
	// needs TLS on the outside, and it fails loudly instead of
	// silently degrading to plaintext.
	c2 := New(url, "group-forced", id, t.Logf)
	c2.Channel = ChanTLS
	c2.Start()
	time.Sleep(1500 * time.Millisecond)
	if c2.Connected() {
		t.Fatal("forced tls connected to a plain ws:// endpoint")
	}
	if st := c2.Status(); st.LastError == "" {
		t.Fatal("no lastError for the misconfigured forced channel")
	}
	c2.Stop()
}

// TestWrongPinDoesNotFallBackToPlaintext: a TLS-level failure (pin
// mismatch inside the tunnel) must NOT drop the client to the legacy
// plaintext ws channel — that would hand the traffic to whoever
// interfered with the TLS handshake. Only a marker rejection
// (ErrMarkerRejected — nothing TLS happened) may fall back.
func TestWrongPinDoesNotFallBackToPlaintext(t *testing.T) {
	wsURL, _, pool := innerTLSHub(t)
	otherPair, _ := testCertPair(t) // an unrelated key → wrong pin

	id := testIdentity(t, "engine-pinfall")
	c := New(wsURL, "group-pinfall", id, t.Logf)
	c.TLSRoots = pool
	c.Pins = []string{certPin(t, otherPair)}
	// Channel stays auto: [ws-tls, ws] — the plaintext channel is
	// right there in the plan, and must stay unreachable.
	c.Start()
	defer c.Stop()

	deadline := time.Now().Add(3 * time.Second)
	for time.Now().Before(deadline) {
		if st := c.Status(); strings.Contains(st.LastError, "pin mismatch") {
			break
		}
		time.Sleep(50 * time.Millisecond)
	}
	st := c.Status()
	if !strings.Contains(st.LastError, "pin mismatch") {
		t.Fatalf("LastError = %q, want a pin mismatch", st.LastError)
	}
	if c.Connected() {
		t.Fatal("client connected despite a wrong pin")
	}
	if st.Channel == ChanWS {
		t.Fatalf("fell back to the plaintext channel after a pin mismatch: %+v", st)
	}
}

// TestPlanUnit: the plan orders by forced channel, URL scheme, and
// the RAM lastGood.
func TestPlanUnit(t *testing.T) {
	id := testIdentity(t, "engine-plan")

	t.Run("auto wss url", func(t *testing.T) {
		c := New("wss://drop.404.mn/hub/ws", "k", id, nil)
		got := c.plan()
		if got[0] != ChanTLS || got[1] != ChanWSTLS {
			t.Fatalf("plan = %v", got)
		}
	})
	t.Run("auto ws url", func(t *testing.T) {
		c := New("ws://127.0.0.1:8085/hub/ws", "k", id, nil)
		got := c.plan()
		if got[0] != ChanWSTLS || got[1] != ChanWS {
			t.Fatalf("plan = %v", got)
		}
	})
	t.Run("forced", func(t *testing.T) {
		c := New("wss://drop.404.mn/hub/ws", "k", id, nil)
		c.Channel = ChanWSTLS
		if got := c.plan(); len(got) != 1 || got[0] != ChanWSTLS {
			t.Fatalf("plan = %v", got)
		}
	})
	t.Run("lastGood first", func(t *testing.T) {
		c := New("wss://drop.404.mn/hub/ws", "k", id, nil)
		c.mu.Lock()
		c.lastGood = ChanWSTLS
		c.mu.Unlock()
		if got := c.plan(); got[0] != ChanWSTLS {
			t.Fatalf("plan = %v", got)
		}
	})
}

// TestWSURLOverrideAndDerivation: ws:// endpoint derivation from
// hub_url and the explicit override.
func TestWSURLOverrideAndDerivation(t *testing.T) {
	id := testIdentity(t, "engine-wsurl")

	c := New("wss://drop.404.mn/hub/ws", "k", id, nil)
	if got := c.wsURL(); got != "ws://drop.404.mn/hub/ws" {
		t.Fatalf("derived = %q", got)
	}
	c2 := New("wss://drop.404.mn:443/hub/ws", "k", id, nil)
	if got := c2.wsURL(); got != "ws://drop.404.mn/hub/ws" {
		t.Fatalf(":443 dropped = %q", got)
	}
	c3 := New("wss://drop.404.mn/hub/ws", "k", id, nil)
	c3.WSURL = "ws://127.0.0.1:8085/hub/ws"
	if got := c3.wsURL(); got != "ws://127.0.0.1:8085/hub/ws" {
		t.Fatalf("override = %q", got)
	}
	// TLS identity is the hub_url host even when the ws endpoint is
	// elsewhere.
	if got := c3.tlsName(); got != "drop.404.mn" {
		t.Fatalf("tlsName = %q", got)
	}
}

// dialContextIsolation: Stop must unblock an in-flight inner-TLS dial
// (the transport rides the loop ctx, not the dial ctx).
func TestStopCancelsItlsDial(t *testing.T) {
	wsURL, pair, pool := innerTLSHub(t)

	id := testIdentity(t, "engine-stop")
	c := New(wsURL, "group-stop", id, t.Logf)
	c.TLSRoots = pool
	c.Pins = []string{certPin(t, pair)}
	c.Channel = ChanWSTLS
	c.Start()
	waitFor(t, 5*time.Second, "connection", c.Connected)
	c.Stop()
	waitFor(t, 2*time.Second, "stop", func() bool { return !c.Connected() })
}
