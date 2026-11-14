// Package lanip enumerates LAN IPv4 candidates, picks one, binds, and
// answers "is this a desktop session?" for browser/dialog opening.
package lanip

import (
	"fmt"
	"net"
	"os"
	"runtime"
	"strings"
)

// BroadcastPort is the UDP port discovery beacons are sent to (fixed,
// private protocol — ground rule #3: not BitTorrent, nothing to
// interoperate with, nothing for ISPs to fingerprint).
const BroadcastPort = 8091

// Candidate is one LAN IPv4 address on an interface.
type Candidate struct {
	IP        string
	Interface string
}

// Candidates lists non-loopback IPv4 addresses of up interfaces.
// Loopback is never a useful advertise target on the LAN.
func Candidates() ([]Candidate, error) {
	ifaces, err := net.Interfaces()
	if err != nil {
		return nil, err
	}
	var out []Candidate
	for _, ifc := range ifaces {
		if ifc.Flags&net.FlagUp == 0 || ifc.Flags&net.FlagLoopback != 0 {
			continue
		}
		addrs, err := ifc.Addrs()
		if err != nil {
			continue
		}
		for _, a := range addrs {
			ipn, ok := a.(*net.IPNet)
			if !ok {
				continue
			}
			ip := ipn.IP
			if ip.To4() == nil || ip.IsLoopback() || ip.IsLinkLocalUnicast() || ip.IsMulticast() {
				continue
			}
			out = append(out, Candidate{IP: ip.String(), Interface: ifc.Name})
		}
	}
	return out, nil
}

// Pick returns the most plausible single LAN candidate (RFC1918
// 10/192.168 first, then 172.16-31, then any IPv4), or nil when none.
func Pick(cands []Candidate) *Candidate {
	if len(cands) == 0 {
		return nil
	}
	pref := func(ip string) int {
		p := net.ParseIP(ip)
		if p == nil || p.To4() == nil {
			return 0
		}
		b := p.To4()
		switch {
		case b[0] == 10, b[0] == 192 && b[1] == 168:
			return 2
		case b[0] == 172 && b[1] >= 16 && b[1] <= 31:
			return 1
		}
		return 0
	}
	best := -1
	for i := range cands {
		if best == -1 || pref(cands[i].IP) > pref(cands[best].IP) {
			best = i
		}
	}
	return &cands[best]
}

func isPrivateLAN(ip string) bool {
	p := net.ParseIP(ip)
	if p == nil || p.To4() == nil {
		return false
	}
	b := p.To4()
	return b[0] == 10 || b[0] == 192 && b[1] == 168 || b[0] == 172 && b[1] >= 16 && b[1] <= 31
}

// BindAddr chooses the address the HTTP server should bind: the picked
// interface when unambiguous, else 0.0.0.0 (all interfaces).
func BindAddr(cands []Candidate, advertise string, port int) string {
	if advertise != "" {
		return net.JoinHostPort(advertise, fmt.Sprintf("%d", port))
	}
	if pick := Pick(cands); pick != nil && len(cands) == 1 {
		return net.JoinHostPort(pick.IP, fmt.Sprintf("%d", port))
	}
	return net.JoinHostPort("0.0.0.0", fmt.Sprintf("%d", port))
}

// BroadcastAddrs returns IPv4 broadcast addresses of LAN interfaces,
// deduplicated — discovery beacons go there via UDP.
func BroadcastAddrs() []string {
	ifaces, err := net.Interfaces()
	if err != nil {
		return nil
	}
	seen := map[string]bool{}
	var out []string
	for _, ifc := range ifaces {
		if ifc.Flags&net.FlagUp == 0 || ifc.Flags&net.FlagLoopback != 0 {
			continue
		}
		addrs, err := ifc.Addrs()
		if err != nil {
			continue
		}
		for _, a := range addrs {
			ipn, ok := a.(*net.IPNet)
			if !ok {
				continue
			}
			ip4 := ipn.IP.To4()
			if ip4 == nil || ipn.IP.IsLoopback() {
				continue
			}
			bc := make(net.IP, len(ip4))
			copy(bc, ip4)
			for i := range bc {
				bc[i] |= ^ipn.Mask[i]
			}
			s := bc.String()
			if !seen[s] {
				seen[s] = true
				out = append(out, s)
			}
		}
	}
	return out
}

// HasGUI reports whether this engine runs inside a desktop session —
// i.e. browser auto-open (#4) and native pick dialogs (#1) can work.
// Headless (SSH, service, container) → false; callers degrade to
// printed URLs and paste-a-path picking.
func HasGUI() bool {
	if os.Getenv("SSH_TTY") != "" || os.Getenv("SSH_CONNECTION") != "" {
		return false
	}
	if runtime.GOOS == "windows" {
		return true // auto-open just fails silently if truly headless
	}
	return os.Getenv("DISPLAY") != "" || os.Getenv("WAYLAND_DISPLAY") != ""
}

// Hostname returns a short machine name for beacons and panels.
func Hostname() string {
	h, err := os.Hostname()
	if err != nil {
		return "unknown"
	}
	if i := strings.IndexByte(h, '.'); i > 0 {
		h = h[:i]
	}
	return h
}
