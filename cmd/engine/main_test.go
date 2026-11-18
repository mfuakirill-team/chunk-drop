package main

import (
	"context"
	"errors"
	"net"
	"testing"
	"time"

	"chunkdrop/internal/discovery"
	"chunkdrop/internal/ratemeter"
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

func newTestDialer(t *testing.T, tun tunnelDialer, rel relayDialer) *remoteDialer {
	t.Helper()
	return &remoteDialer{
		tun:  tun,
		rel:  rel,
		logf: func(string, ...any) {},
	}
}

// dialPeer, default routing: tunnel first, relay on tunnel failure
// (with the cooldown recorded).
func TestDialPeerAuto(t *testing.T) {
	t.Run("tunnel works", func(t *testing.T) {
		tun, rel := &fakeTunnel{}, &fakeRelay{}
		d := newTestDialer(t, tun, rel)
		conn, err := d.dialPeer(context.Background(), testPeer)
		if err != nil {
			t.Fatal(err)
		}
		conn.Close()
		if tun.dials != 1 || rel.dials != 0 {
			t.Errorf("dials tunnel=%d relay=%d", tun.dials, rel.dials)
		}
	})

	t.Run("tunnel fails — relay takes over, cooldown set", func(t *testing.T) {
		tun, rel := &fakeTunnel{err: errors.New("ice failed")}, &fakeRelay{}
		d := newTestDialer(t, tun, rel)
		conn, err := d.dialPeer(context.Background(), testPeer)
		if err != nil {
			t.Fatal(err)
		}
		conn.Close()
		if tun.dials != 1 || rel.dials != 1 {
			t.Errorf("dials tunnel=%d relay=%d", tun.dials, rel.dials)
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

// after a tunnel failure the cooldown suppresses tunnel attempts for
// tunnelCooldown (the PC-up escape hatch excepted).
func TestCooldownSuppressesTunnel(t *testing.T) {
	tun, rel := &fakeTunnel{err: errors.New("ice failed")}, &fakeRelay{}
	d := newTestDialer(t, tun, rel)
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
	d := newTestDialer(t, &fakeTunnel{}, &fakeRelay{})
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
	d := newTestDialer(t, tun, &fakeRelay{})
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

// peerRefs honors the global mesh mode: auto includes LAN-discovered
// peers, internet skips them (interact via the global internet only).
func TestPeerRefsModeFilter(t *testing.T) {
	selfID := "aaaaaaaaaaaaaaaa"
	peerID := "bbbbbbbbbbbbbbbb"

	build := func() (*meshRuntime, meshDeps) {
		m := &meshRuntime{disc: inactiveDisc(selfID, "self", 8090)}
		m.disc.Table().Observe(discovery.Beacon{V: 1, ID: peerID, Name: "other-box", Port: 8090, TS: 1}, "192.168.1.9")
		d := meshDeps{deviceID: selfID}
		return m, d
	}

	t.Run("auto — LAN peers in", func(t *testing.T) {
		m, d := build()
		d.mode = "auto"
		refs := peerRefs(m, d)
		if len(refs) != 1 || refs[0].EngineID != peerID {
			t.Errorf("refs = %+v", refs)
		}
	})

	t.Run("internet — LAN peers skipped", func(t *testing.T) {
		m, d := build()
		d.mode = "internet"
		if refs := peerRefs(m, d); len(refs) != 0 {
			t.Errorf("refs = %+v, want none (no hub in this fixture)", refs)
		}
	})

	t.Run("local — hub nil, LAN peers still in", func(t *testing.T) {
		m, d := build()
		d.mode = "local"
		if refs := peerRefs(m, d); len(refs) != 1 {
			t.Errorf("refs = %+v, want the LAN peer", refs)
		}
	})

	t.Run("self never in the list", func(t *testing.T) {
		m, d := build()
		m.disc.Table().Observe(discovery.Beacon{V: 1, ID: selfID, Name: "self", Port: 8090, TS: 1}, "127.0.0.1")
		if refs := peerRefs(m, d); len(refs) != 1 {
			t.Errorf("refs = %+v, want only the other peer", refs)
		}
	})
}

// The puller meters verified chunk bytes per peer engine ID (the
// Clients card's download column).
func TestPullerRateMetering(t *testing.T) {
	m := ratemeter.New(ratemeter.DefaultWindow)
	m.Add("peer-a", 4096)
	m.Add("peer-b", 512)
	r := m.Rates()
	if len(r) != 2 || r["peer-a"] <= 0 {
		t.Errorf("rates = %v", r)
	}
}
