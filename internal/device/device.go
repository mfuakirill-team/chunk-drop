// Package device is the engine's persistent identity (M2): an Ed25519
// key pair generated on first run and kept at <configDir>/device.key
// (mode 0600). The device ID is the fingerprint of the public key —
// stable across restarts, so the same machine is the same device on
// the LAN (discovery beacons) and at the hub (M2 presence).
//
// The private key must never live in the synced folder (it would
// replicate to every mesh device); the config dir is local per
// machine. Peers learn the public key from hello messages and pin it
// on first sight — a later key change is a warning, not silent trust.
package device

import (
	"crypto/ed25519"
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"fmt"
	"os"
	"path/filepath"
)

// KeyFile is the identity file's name inside the config dir.
const KeyFile = "device.key"

// Identity is one engine device: its stable ID, human name, and keys.
type Identity struct {
	ID   string             // hex(sha256(public key)) — 64 hex chars
	Name string             // human-facing name (hostname)
	Priv ed25519.PrivateKey // 64 bytes (seed + public)
	Pub  ed25519.PublicKey  // 32 bytes
}

// LoadOrCreate loads the identity from dir/device.key, creating the
// key pair on first run (dir created as needed, file written 0600).
// name is the human-facing device name (the engine passes its
// hostname, the same one discovery already advertises).
func LoadOrCreate(dir, name string) (*Identity, error) {
	path := filepath.Join(dir, KeyFile)
	raw, err := os.ReadFile(path)
	switch {
	case os.IsNotExist(err):
		id, gerr := create(path, name)
		if gerr != nil {
			return nil, gerr
		}
		return id, nil
	case err != nil:
		return nil, fmt.Errorf("read %s: %w", path, err)
	}
	priv, perr := parseKey(raw)
	if perr != nil {
		return nil, fmt.Errorf("%s: %w (delete the file to mint a new identity)", path, perr)
	}
	return fromPriv(priv, name), nil
}

// create generates a key pair and persists it.
func create(path, name string) (*Identity, error) {
	_, priv, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		return nil, fmt.Errorf("generate key: %w", err)
	}
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return nil, fmt.Errorf("create config dir: %w", err)
	}
	// Base64 of the full 64-byte private key, one line — hand-checkable
	// with `base64 -d`/Go, and the file mode is the real protection.
	data := base64.StdEncoding.EncodeToString(priv)
	if err := os.WriteFile(path, []byte(data+"\n"), 0o600); err != nil {
		return nil, fmt.Errorf("write %s: %w", path, err)
	}
	return fromPriv(priv, name), nil
}

// parseKey decodes a base64 line into an Ed25519 private key.
func parseKey(raw []byte) (ed25519.PrivateKey, error) {
	s := raw
	if i := len(raw); i > 0 && raw[i-1] == '\n' {
		s = raw[:i-1]
	}
	b, err := base64.StdEncoding.DecodeString(string(s))
	if err != nil {
		return nil, fmt.Errorf("bad base64: %w", err)
	}
	if len(b) != ed25519.PrivateKeySize {
		return nil, fmt.Errorf("want %d bytes, got %d", ed25519.PrivateKeySize, len(b))
	}
	return ed25519.PrivateKey(b), nil
}

func fromPriv(priv ed25519.PrivateKey, name string) *Identity {
	pub := priv.Public().(ed25519.PublicKey)
	return &Identity{ID: Fingerprint(pub), Name: name, Priv: priv, Pub: pub}
}

// Fingerprint is the device ID for a public key: hex(SHA-256(pub)).
func Fingerprint(pub ed25519.PublicKey) string {
	sum := sha256.Sum256(pub)
	return hex.EncodeToString(sum[:])
}

// Sign signs payload with the device's private key.
func (id *Identity) Sign(payload []byte) []byte {
	return ed25519.Sign(id.Priv, payload)
}

// Verify checks a signature against a device public key.
func Verify(pub ed25519.PublicKey, msg, sig []byte) bool {
	return ed25519.Verify(pub, msg, sig)
}

// VerifyID checks a signature and that pub is the key behind the
// claimed device ID — one call for hello-style proofs.
func VerifyID(idHex string, pub ed25519.PublicKey, msg, sig []byte) bool {
	return Fingerprint(pub) == idHex && Verify(pub, msg, sig)
}
