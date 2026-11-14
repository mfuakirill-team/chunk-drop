package api

import (
	"net"
	"net/http"
)

// Dashboard access control: the dashboard can point a session at
// arbitrary filesystem paths and open native dialogs on this PC, so it
// is reachable **only from this machine** by default. Receiving (the
// /r/ page and the session endpoints behind its key) is the only
// remote surface. `allow_remote_dashboard` in engine.toml opts out for
// headless engines (e.g. a NAS whose dashboard you open from a laptop),
// still behind the group key.

// interfaceAddrs is swappable in tests.
var interfaceAddrs = net.InterfaceAddrs

// isLocalRequest reports whether r comes from this machine: loopback,
// or any address configured on a local interface. A browser on the
// same PC reaches the engine via its LAN IP (source = that LAN IP);
// another device would present its own source address instead.
func isLocalRequest(r *http.Request) bool {
	host, _, err := net.SplitHostPort(r.RemoteAddr)
	if err != nil {
		return false
	}
	ip := net.ParseIP(host)
	if ip == nil {
		return false
	}
	if ip.IsLoopback() {
		return true
	}
	addrs, err := interfaceAddrs()
	if err != nil {
		return false
	}
	for _, a := range addrs {
		var own net.IP
		switch v := a.(type) {
		case *net.IPNet:
			own = v.IP
		case *net.IPAddr:
			own = v.IP
		default:
			continue
		}
		if own != nil && own.Equal(ip) {
			return true
		}
	}
	return false
}

// localOnly wraps a dashboard/control handler with the local-only
// check. Non-local requests get the generic 404 (same body as any
// unknown route — the response confirms nothing; SECURITY.md §2).
// The allow flag is read per request so it can be flipped in tests and
// by future config reloads.
func (s *Server) localOnly(h http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if !s.AllowRemoteDashboard && !isLocalRequest(r) {
			http.Error(w, generic404, http.StatusNotFound)
			return
		}
		h.ServeHTTP(w, r)
	})
}
