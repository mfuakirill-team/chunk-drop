// Package hub is the coordination server (M2, docs/ARCHITECTURE.md §1):
// a stateless WSS service behind nginx on the VPS. It answers "which
// of my devices are online" (presence per group tag), relays WebRTC
// signaling, and (later steps) pipes opaque bytes for the relay
// fallback. It writes nothing to disk and holds state in RAM only.
//
// The group key itself never crosses the wire: clients authenticate
// with a group tag — SHA-256(group_key), knowledge of which requires
// the key — plus a signature from their device key over the hello.
// The hub sees real device IDs (the group owner's choice) but never
// the group key, and never user file data (relay frames are the
// engine's own WSS-level TLS-protected bytes; the direct WebRTC path
// bypasses the hub entirely).
package hub

// Wire protocol (JSON frames over WSS). Kept in this package so the
// server and the engine-side client (internal/hubclient) agree on the
// exact shapes — both sides live in this repo but must stay
// independently deployable (old hub, newer engine: presence and
// hello are frozen; signaling/relay types below are additive).

// Hello is the first message a client sends after upgrading.
type Hello struct {
	Type string `json:"type"` // "hello"
	Tag  string `json:"tag"`  // hex(sha256(group_key)) — group membership
	ID   string `json:"id"`   // device ID: hex(sha256(device public key))
	Name string `json:"name"` // human name (hostname)
	Pub  string `json:"pub"`  // base64 Ed25519 public key (32 bytes)
	TS   int64  `json:"ts"`   // unix seconds of the signature
	Sig  string `json:"sig"`  // base64 signature over HelloCanonical(tag,id,pub,ts)
}

// HelloCanonical is the exact byte string signed by the hello: no
// ambiguity about which fields are covered.
func HelloCanonical(tag, id, pub string, ts int64) []byte {
	return []byte("chunkdrop-hello|" + tag + "|" + id + "|" + pub + "|" + itoa(ts))
}

// Peer is one online device as announced by the hub.
type Peer struct {
	ID   string `json:"id"`
	Name string `json:"name"`
	Pub  string `json:"pub"`
}

// Presence is the hub's full-list broadcast: sent to a group's members
// on every join/leave and to a fresh client right after its hello.
// A full list keeps clients trivially consistent (no diff to replay).
type Presence struct {
	Type    string `json:"type"` // "presence"
	Devices []Peer `json:"devices"`
}

// Error is sent before the hub closes a connection it rejects.
type Error struct {
	Type  string `json:"type"` // "error"
	Error string `json:"error"`
}

// itoa avoids importing strconv in a hot path for one call site.
func itoa(n int64) string {
	if n == 0 {
		return "0"
	}
	neg := n < 0
	if neg {
		n = -n
	}
	var b [20]byte
	i := len(b)
	for n > 0 {
		i--
		b[i] = byte('0' + n%10)
		n /= 10
	}
	if neg {
		i--
		b[i] = '-'
	}
	return string(b[i:])
}
