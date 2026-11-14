// Package lanip enumerates the machine's LAN candidate addresses for
// the QR link (docs/ARCHITECTURE.md §1: "VPN adapters are the classic
// trap" — multiple candidates go to a UI picker).
package lanip

import (
	"net"
	"sort"
)

// Candidate is one non-loopback IPv4 address the engine could advertise.
type Candidate struct {
	IP        string `json:"ip"`
	Interface string `json:"interface"` // interface name, e.g. "en0", "eth3"
}

// Candidates enumerates non-loopback IPv4 unicast addresses.
// Loopback, link-local (169.254.*) and IPv6 are excluded — QR links
// need a routable LAN address.
func Candidates() ([]Candidate, error) {
	ifaces, err := net.Interfaces()
	if err != nil {
		return nil, err
	}
	var out []Candidate
	seen := map[string]bool{}
	for _, ifi := range ifaces {
		if ifi.Flags&net.FlagLoopback != 0 || ifi.Flags&net.FlagUp == 0 {
			continue
		}
		addrs, err := ifi.Addrs()
		if err != nil {
			continue // one bad interface must not kill the enumeration
		}
		for _, a := range addrs {
			ipn, ok := a.(*net.IPNet)
			if !ok {
				continue
			}
			ip := ipn.IP
			if ip.IsLoopback() || ip.IsLinkLocalUnicast() || ip.To4() == nil {
				continue
			}
			s := ip.String()
			if !seen[s] {
				seen[s] = true
				out = append(out, Candidate{IP: s, Interface: ifi.Name})
			}
		}
	}
	sort.Slice(out, func(i, j int) bool {
		if out[i].IP != out[j].IP {
			return out[i].IP < out[j].IP
		}
		return out[i].Interface < out[j].Interface
	})
	return out, nil
}

// Pick returns the single candidate if exactly one exists — the
// auto-pick path in the engine — or nil when a UI picker is needed.
func Pick(cs []Candidate) *Candidate {
	if len(cs) == 1 {
		return &cs[0]
	}
	return nil
}

// BindAddr returns the host:port to bind the HTTP server to: the chosen
// candidate IP when practical (SECURITY.md §6), else all interfaces.
func BindAddr(cs []Candidate, chosen string, port int) string {
	if chosen != "" {
		for _, c := range cs {
			if c.IP == chosen {
				return net.JoinHostPort(c.IP, itoa(port))
			}
		}
	}
	return net.JoinHostPort("0.0.0.0", itoa(port))
}

func itoa(p int) string {
	if p == 0 {
		return "0"
	}
	digits := ""
	for p > 0 {
		digits = string(rune('0'+p%10)) + digits
		p /= 10
	}
	return digits
}
