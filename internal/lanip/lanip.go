// Package lanip enumerates LAN IPv4 candidates, picks one, binds, and
// answers "is this a desktop session?" for browser/dialog opening.
package lanip

import (
	"fmt"
	"net"
	"os"
	"runtime"
	"strings"
	"time"
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
//
// Windows machines routinely expose extra "local only" adapters
// (Hyper-V/WSL virtual switches, TAP devices, docker bridges) that
// have no default gateway and cannot reach anything. To avoid picking
// one of those, the OS default route is probed (DefaultRouteIP) — the
// interface the OS itself would use to reach other devices wins with
// a wide margin — and virtual-sounding adapter names are penalized.
func Pick(cands []Candidate) *Candidate {
	return pick(cands, DefaultRouteIP())
}

func pick(cands []Candidate, via string) *Candidate {
	if len(cands) == 0 {
		return nil
	}
	pref := func(c Candidate) int {
		p := net.ParseIP(c.IP)
		if p == nil || p.To4() == nil {
			return 0
		}
		b := p.To4()
		s := 0
		switch {
		case b[0] == 10, b[0] == 192 && b[1] == 168:
			s = 2
		case b[0] == 172 && b[1] >= 16 && b[1] <= 31:
			s = 1
		}
		if via != "" && c.IP == via {
			// The adapter the OS routes internet traffic through —
			// this is the "configured, reaches other devices" one.
			s += 100
		}
		if virtualAdapter(c.Interface) {
			// Local-only virtual adapters: demote, don't drop — on a
			// machine with ONLY a virtual adapter it must still win.
			s -= 10
		}
		return s
	}
	best := -1
	for i := range cands {
		if best == -1 || pref(cands[i]) > pref(cands[best]) {
			best = i
		}
	}
	return &cands[best]
}

// virtualAdapters are name fragments of adapters that are typically
// local-only (no gateway): virtual switches, VPN/TAP devices and
// container bridges. Matched case-insensitively against the adapter
// name; a match only demotes a candidate (see pick).
var virtualAdapters = []string{
	"vethernet", "wsl", "hyper-v", "virtual", "vbox", "vmware",
	"docker", "veth", "virbr", "loopback", "tap", "tun", "vpn",
	"tailscale", "zerotier", "hamachi", "ppp",
}

func virtualAdapter(name string) bool {
	n := strings.ToLower(name)
	for _, frag := range virtualAdapters {
		if strings.Contains(n, frag) {
			return true
		}
	}
	return false
}

// DefaultRouteIP returns the local IPv4 address the OS would use to
// reach the internet — established via a UDP "connection" to public
// resolvers (no packet is actually sent; UDP connect just asks the
// routing table). Empty when there is no working default route. This
// is the adapter that is *configured* — it has a gateway — unlike the
// local-only virtual adapters Windows likes to expose.
func DefaultRouteIP() string {
	for _, target := range []string{"8.8.8.8:80", "1.1.1.1:80", "9.9.9.9:80"} {
		conn, err := net.DialTimeout("udp", target, time.Second)
		if err != nil {
			continue // no route to this resolver; try the next
		}
		host, _, err := net.SplitHostPort(conn.LocalAddr().String())
		_ = conn.Close()
		if err == nil && host != "" && net.ParseIP(host) != nil {
			return host
		}
	}
	return ""
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
