package main

import (
	"context"
	"errors"
	"net"
	"testing"
	"time"
)

// fakeTunnel / fakeRelay record dial attempts and hand back pipe ends.
type fakeTunnel struct {
	dials int
	err   error // Dial fails when set
	pcUp  bool  // PeerConnected
}

func (f *fakeTunnel) Dial(ctx context.Context, peer string) (net.Conn, error) {
	f.dials++
	if f.err != nil {
		return nil, f.err
	}
	c1, _ := net.Pipe()
	return c1, nil
}
func (f *fakeTunnel) PeerConnected(string) bool { return f.pcUp }

type fakeRelay struct {
	dials int
	err   error
}

func (f *fakeRelay) Dial(ctx context.Context, peer string) (net.Conn, error) {
	f.dials++
	if f.err != nil {
		return nil, f.err
	}
	c1, _ := net.Pipe()
	return c1, nil
}

const testPeer = "aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa"

func newTestDialer(t *testing.T, tun tunnelDialer, rel relayDialer) (*remoteDialer, *routeState) {
	t.Helper()
	rs := &routeState{}
	d := &remoteDialer{
		tun:  tun,
		rel:  rel,
		rs:   rs,
		logf: func(string, ...any) {},
	}
	return d, rs
}

// routeState: mode vocabulary, auto default, off blocks, snapshots.
func TestRouteState(t *testing.T) {
	rs := &routeState{}
	if got := rs.Mode(testPeer); got != "auto" {
		t.Errorf("default mode = %q, want auto", got)
	}
	if rs.Blocked(testPeer) {
		t.Error("default state must not block")
	}

	if err := rs.SetMode(testPeer, "turbo"); err == nil {
		t.Error("unknown mode accepted")
	}
	if err := rs.SetMode("", "off"); err == nil {
		t.Error("empty id accepted")
	}
	for _, m := range []string{"auto", "tunnel", "relay", "off"} {
		if err := rs.SetMode(testPeer, m); err != nil {
			t.Errorf("mode %q rejected: %v", m, err)
		}
	}

	if err := rs.SetMode(testPeer, "off"); err != nil {
		t.Fatal(err)
	}
	if !rs.Blocked(testPeer) {
		t.Error("off must block")
	}
	if err := rs.SetMode(testPeer, "auto"); err != nil {
		t.Fatal(err)
	}
	if rs.Blocked(testPeer) {
		t.Error("auto must unblock")
	}

	if err := rs.SetMode(testPeer, "relay"); err != nil {
		t.Fatal(err)
	}
	modes := rs.Modes()
	if len(modes) != 1 || modes[testPeer] != "relay" {
		t.Errorf("Modes() = %v", modes)
	}
	rs.setRoute(testPeer, "relay")
	routes := rs.Routes()
	if len(routes) != 1 || routes[testPeer] != "relay" {
		t.Errorf("Routes() = %v", routes)
	}
}

// dialPeer, mode auto: tunnel first, relay on tunnel failure, and the
// route taken is recorded.
func TestDialPeerAuto(t *testing.T) {
	t.Run("tunnel works", func(t *testing.T) {
		tun, rel := &fakeTunnel{}, &fakeRelay{}
		d, rs := newTestDialer(t, tun, rel)
		conn, err := d.dialPeer(context.Background(), testPeer)
		if err != nil {
			t.Fatal(err)
		}
		conn.Close()
		if tun.dials != 1 || rel.dials != 0 {
			t.Errorf("dials tunnel=%d relay=%d", tun.dials, rel.dials)
		}
		if rs.Routes()[testPeer] != "tunnel" {
			t.Errorf("route = %q, want tunnel", rs.Routes()[testPeer])
		}
	})

	t.Run("tunnel fails — relay takes over, cooldown set", func(t *testing.T) {
		tun, rel := &fakeTunnel{err: errors.New("ice failed")}, &fakeRelay{}
		d, rs := newTestDialer(t, tun, rel)
		conn, err := d.dialPeer(context.Background(), testPeer)
		if err != nil {
			t.Fatal(err)
		}
		conn.Close()
		if tun.dials != 1 || rel.dials != 1 {
			t.Errorf("dials tunnel=%d relay=%d", tun.dials, rel.dials)
		}
		if rs.Routes()[testPeer] != "relay" {
			t.Errorf("route = %q, want relay", rs.Routes()[testPeer])
		}
		d.mu.Lock()
		_, cooled := d.cooldown[testPeer]
		d.mu.Unlock()
		if !cooled {
			t.Error("failed tunnel dial did not set the cooldown")
		}
		if d.tunnelUsable(testPeer) {
			t.Error("cooled peer with PC down must not be tunnel-usable")
		}
	})
}

// mode "off" refuses the dial without touching either transport.
func TestDialPeerOff(t *testing.T) {
	tun, rel := &fakeTunnel{}, &fakeRelay{}
	d, rs := newTestDialer(t, tun, rel)
	if err := rs.SetMode(testPeer, "off"); err != nil {
		t.Fatal(err)
	}
	if _, err := d.dialPeer(context.Background(), testPeer); err == nil {
		t.Fatal("off peer dialed")
	}
	if tun.dials != 0 || rel.dials != 0 {
		t.Errorf("blocked peer dialed: tunnel=%d relay=%d", tun.dials, rel.dials)
	}
}

// mode "tunnel" forces the tunnel: no relay fallback on failure.
func TestDialPeerTunnelMode(t *testing.T) {
	t.Run("tunnel ok", func(t *testing.T) {
		tun, rel := &fakeTunnel{}, &fakeRelay{}
		d, rs := newTestDialer(t, tun, rel)
		if err := rs.SetMode(testPeer, "tunnel"); err != nil {
			t.Fatal(err)
		}
		conn, err := d.dialPeer(context.Background(), testPeer)
		if err != nil {
			t.Fatal(err)
		}
		conn.Close()
		if rel.dials != 0 {
			t.Errorf("forced tunnel still dialed relay (%d)", rel.dials)
		}
		if rs.Routes()[testPeer] != "tunnel" {
			t.Errorf("route = %q", rs.Routes()[testPeer])
		}
	})

	t.Run("tunnel fails — error surfaces, relay untouched", func(t *testing.T) {
		tun, rel := &fakeTunnel{err: errors.New("ice failed")}, &fakeRelay{}
		d, rs := newTestDialer(t, tun, rel)
		if err := rs.SetMode(testPeer, "tunnel"); err != nil {
			t.Fatal(err)
		}
		if _, err := d.dialPeer(context.Background(), testPeer); err == nil {
			t.Fatal("forced tunnel swallowed the failure")
		}
		if rel.dials != 0 {
			t.Errorf("forced tunnel fell back to relay (%d dials)", rel.dials)
		}
	})

	t.Run("tunnel off entirely — refuses", func(t *testing.T) {
		rel := &fakeRelay{}
		d, rs := newTestDialer(t, nil, rel) // nil interface = no tunnel
		if err := rs.SetMode(testPeer, "tunnel"); err != nil {
			t.Fatal(err)
		}
		if _, err := d.dialPeer(context.Background(), testPeer); err == nil {
			t.Fatal("tunnel mode accepted with no tunnel")
		}
		if rel.dials != 0 {
			t.Errorf("relay dialed despite forced tunnel mode (%d)", rel.dials)
		}
	})
}

// mode "relay" skips the tunnel entirely (the CHUNKDROP_RELAY_ONLY
// shape, but per peer).
func TestDialPeerRelayMode(t *testing.T) {
	tun, rel := &fakeTunnel{}, &fakeRelay{}
	d, rs := newTestDialer(t, tun, rel)
	if err := rs.SetMode(testPeer, "relay"); err != nil {
		t.Fatal(err)
	}
	conn, err := d.dialPeer(context.Background(), testPeer)
	if err != nil {
		t.Fatal(err)
	}
	conn.Close()
	if tun.dials != 0 {
		t.Errorf("forced relay dialed the tunnel (%d)", tun.dials)
	}
	if rs.Routes()[testPeer] != "relay" {
		t.Errorf("route = %q", rs.Routes()[testPeer])
	}
}

// after a tunnel failure the cooldown suppresses tunnel attempts for
// tunnelCooldown (the PC-up escape hatch excepted).
func TestCooldownSuppressesTunnel(t *testing.T) {
	tun, rel := &fakeTunnel{err: errors.New("ice failed")}, &fakeRelay{}
	d, _ := newTestDialer(t, tun, rel)
	ctx := context.Background()
	if _, err := d.dialPeer(ctx, testPeer); err != nil {
		t.Fatal(err)
	}
	if _, err := d.dialPeer(ctx, testPeer); err != nil {
		t.Fatal(err)
	}
	if tun.dials != 1 {
		t.Errorf("tunnel dials = %d, want 1 (cooldown must suppress the retry)", tun.dials)
	}
	if rel.dials != 2 {
		t.Errorf("relay dials = %d, want 2", rel.dials)
	}
}

// HTTPClient caches per peer (transport pooling survives peerRefs
// rebuilds) and serves only its own peer's host.
func TestHTTPClientCache(t *testing.T) {
	tun, rel := &fakeTunnel{}, &fakeRelay{}
	d, _ := newTestDialer(t, tun, rel)
	c1 := d.HTTPClient(testPeer)
	c2 := d.HTTPClient(testPeer)
	if c1 != c2 {
		t.Error("clients not cached per peer")
	}
	other := "b" + testPeer[1:]
	c3 := d.HTTPClient(other)
	if c3 == c1 {
		t.Error("different peers share a client")
	}
}

// tunnelUsable honors the cooldown but lets a connected PC through.
func TestTunnelUsable(t *testing.T) {
	tun := &fakeTunnel{err: errors.New("ice failed")}
	rel := &fakeRelay{}
	d, _ := newTestDialer(t, tun, rel)
	d.setCooldown(testPeer)
	if d.tunnelUsable(testPeer) {
		t.Error("cooled peer reported usable")
	}
	tun.pcUp = true
	if !d.tunnelUsable(testPeer) {
		t.Error("connected PC must bypass the cooldown")
	}
	d.mu.Lock()
	d.cooldown[testPeer] = time.Now().Add(-2 * tunnelCooldown)
	d.mu.Unlock()
	if !d.tunnelUsable(testPeer) {
		t.Error("expired cooldown must reopen the tunnel")
	}
}
