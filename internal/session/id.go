package session

import (
	"crypto/rand"
	"encoding/hex"
)

// NewID returns a random 16-byte session id, hex-encoded (UUID-shaped
// enough for our purposes; PROTOCOL.md §1 says UUIDv4, and 128 random
// bits are the same security property).
func NewID() string {
	b := make([]byte, 16)
	if _, err := rand.Read(b); err != nil {
		// crypto/rand never fails on supported platforms; panic loudly
		// rather than mint predictable ids.
		panic("crypto/rand: " + err.Error())
	}
	return hex.EncodeToString(b)
}
