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
		t.Error("no candidates must not auto-pick")
	}
	one := []Candidate{{IP: "192.168.1.5", Interface: "eth0"}}
	if p := Pick(one); p == nil || p.IP != "192.168.1.5" {
		t.Errorf("single candidate not auto-picked: %v", p)
	}
	two := []Candidate{{IP: "192.168.1.5"}, {IP: "10.0.0.3"}}
	if Pick(two) != nil {
		t.Error("two candidates must force the picker")
	}
}

func TestBindAddr(t *testing.T) {
	cs := []Candidate{{IP: "192.168.1.5", Interface: "eth0"}}
	if got := BindAddr(cs, "192.168.1.5", 8090); got != "192.168.1.5:8090" {
		t.Errorf("BindAddr = %q", got)
	}
	// Unknown chosen IP → fall back to all interfaces.
	if got := BindAddr(cs, "10.9.9.9", 8090); got != "0.0.0.0:8090" {
		t.Errorf("BindAddr fallback = %q", got)
	}
}
