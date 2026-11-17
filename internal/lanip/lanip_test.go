package lanip

import (
	"net"
	"testing"
)

func TestCandidatesExcludesLoopback(t *testing.T) {
	cs, err := Candidates()
	if err != nil {
		t.Fatal(err)
	}
	for _, c := range cs {
		ip := net.ParseIP(c.IP)
		if ip == nil {
			t.Errorf("bad candidate IP %q", c.IP)
			continue
		}
		if ip.IsLoopback() {
			t.Errorf("loopback candidate %q", c.IP)
		}
		if ip.To4() == nil {
			t.Errorf("non-IPv4 candidate %q", c.IP)
		}
		if ip.IsLinkLocalUnicast() {
			t.Errorf("link-local candidate %q", c.IP)
		}
	}
}

func TestPick(t *testing.T) {
	if Pick(nil) != nil {
		t.Fatal("no candidates must not auto-pick")
	}
	one := []Candidate{{IP: "192.168.1.5", Interface: "eth0"}}
	if p := Pick(one); p == nil || p.IP != "192.168.1.5" {
		t.Fatalf("single candidate not auto-picked: %v", p)
	}
	// Two candidates: the RFC1918 one wins (VPN adapters lose).
	two := []Candidate{{IP: "172.20.1.9", Interface: "tun0"}, {IP: "192.168.1.5", Interface: "eth0"}}
	if p := Pick(two); p == nil || p.IP != "192.168.1.5" {
		t.Fatalf("private LAN not preferred over VPN: %v", p)
	}
}

// The Windows bug this fixes: a local-only virtual adapter (Hyper-V /
// WSL switch, no gateway) beat the real adapter because both were
// RFC1918. The OS default route must win with a wide margin, and
// virtual-sounding names must lose even against a real-looking one.
func TestPickDefaultRouteWinsOverVirtual(t *testing.T) {
	// Real adapter vs virtual switch, no route probe (via == ""):
	// same RFC1918 class — without the probe the first one would win,
	// which is exactly how the wrong adapter got picked.
	cands := []Candidate{
		{IP: "172.24.80.1", Interface: "vEthernet (WSL (Hyper-V firewall))"},
		{IP: "192.168.1.23", Interface: "Wi-Fi"},
	}
	if p := pick(cands, ""); p == nil || p.IP != "192.168.1.23" {
		t.Fatalf("virtual switch must lose to a real adapter: %+v", p)
	}
	// Even a "more private" virtual adapter loses to the routed one.
	cands = []Candidate{
		{IP: "192.168.137.1", Interface: "vEthernet (Default Switch)"},
		{IP: "10.0.0.5", Interface: "Ethernet"},
	}
	if p := pick(cands, "10.0.0.5"); p == nil || p.IP != "10.0.0.5" {
		t.Fatalf("default-route adapter must win: %+v", p)
	}
	// Only-virtual setups still pick something (demoted, not dropped).
	cands = []Candidate{{IP: "172.17.0.1", Interface: "docker0"}}
	if p := pick(cands, ""); p == nil || p.IP != "172.17.0.1" {
		t.Fatalf("sole virtual adapter must still be picked: %+v", p)
	}
}

func TestVirtualAdapter(t *testing.T) {
	yes := []string{"vEthernet (WSL)", "Hyper-V Virtual Ethernet Adapter", "TAP-Windows Adapter", "docker0", "virbr0", "tun0", "Tailscale"}
	for _, n := range yes {
		if !virtualAdapter(n) {
			t.Errorf("%q should be treated as a virtual/VPN adapter", n)
		}
	}
	no := []string{"Wi-Fi", "Ethernet", "Ethernet 2", "WLAN", "eth0", "en0"}
	for _, n := range no {
		if virtualAdapter(n) {
			t.Errorf("%q should NOT be treated as a virtual/VPN adapter", n)
		}
	}
}

func TestBindAddr(t *testing.T) {
	cs := []Candidate{{IP: "192.168.1.5", Interface: "eth0"}}
	if got := BindAddr(cs, "192.168.1.5", 8090); got != "192.168.1.5:8090" {
		t.Errorf("BindAddr = %q", got)
	}
	// Pinned advertise IP wins even when not enumerated (VPN edge).
	if got := BindAddr(cs, "10.9.9.9", 8090); got != "10.9.9.9:8090" {
		t.Errorf("BindAddr fallback = %q", got)
	}
}
