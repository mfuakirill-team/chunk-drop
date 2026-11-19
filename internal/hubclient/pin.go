// Certificate pinning for the hub connection (M3 step A).
//
// A pin is the sha256 hash of the hub certificate's public key
// (SubjectPublicKeyInfo, DER) — the "pin-sha256" form — written as
// "sha256/<base64>". The engine compares it against the certificate
// the hub presents, IN ADDITION to normal chain verification:
//
//   - chain verification answers "signed by someone I've been told to
//     trust" (the MITM with its own untrusted CA already fails here —
//     the x509 unknown-authority error);
//   - the pin answers "is this THE key" — it converts a SILENT MITM
//     (corporate/hotel proxy whose root CA is already in the system
//     trust store, so the chain verifies) into the same loud failure.
//
// Pins travel in the engine config (hub_cert_pin, comma-separated to
// allow old+new during key rotation) and/or baked into the binary at
// build time (DefaultPin below, stamped by `make engine HUBPIN=...`).
// Config entries win when present; the baked pin is the fallback.
package hubclient

import (
	"crypto/sha256"
	"crypto/x509"
	"encoding/base64"
	"errors"
	"fmt"
	"strings"
)

// DefaultPin is the hub certificate SPKI pin baked into the binary at
// build time: `make engine HUBPIN=sha256/...` stamps it via -ldflags
// -X (Makefile). Empty (the default build) = no baked pin, standard
// verification only. Config (hub_cert_pin) overrides it when set.
var DefaultPin = ""

// pinPrefix is the only supported pin scheme.
const pinPrefix = "sha256/"

// SplitPins splits a comma-separated hub_cert_pin value into trimmed
// entries (empties dropped). It does not validate the entries —
// ParsePins does that, so a malformed pin surfaces as a dial error,
// not as a config-load failure.
func SplitPins(s string) []string {
	var out []string
	for _, e := range strings.Split(s, ",") {
		if e = strings.TrimSpace(e); e != "" {
			out = append(out, e)
		}
	}
	return out
}

// ParsePins validates pin entries into their raw SPKI hashes for
// byte comparison. An error names the bad entry: a pin is an
// integrity guarantee, so a typo must fail loudly, never silently
// verify nothing.
func ParsePins(entries []string) ([][]byte, error) {
	var out [][]byte
	for _, e := range entries {
		e = strings.TrimSpace(e)
		if e == "" {
			continue
		}
		b64, ok := strings.CutPrefix(e, pinPrefix)
		if !ok {
			return nil, fmt.Errorf("pin %q: want %q + base64", e, pinPrefix)
		}
		raw, err := base64.StdEncoding.DecodeString(b64)
		if err != nil {
			return nil, fmt.Errorf("pin %q: bad base64: %w", e, err)
		}
		if len(raw) != sha256.Size {
			return nil, fmt.Errorf("pin %q: want %d bytes, got %d", e, sha256.Size, len(raw))
		}
		out = append(out, raw)
	}
	return out, nil
}

// SPKIHash returns the pin fingerprint of a certificate: sha256 over
// the DER of its public key (SubjectPublicKeyInfo).
func SPKIHash(cert *x509.Certificate) ([]byte, error) {
	der, err := x509.MarshalPKIXPublicKey(cert.PublicKey)
	if err != nil {
		return nil, fmt.Errorf("marshal public key: %w", err)
	}
	sum := sha256.Sum256(der)
	return sum[:], nil
}

// activePins resolves the pin list for one dial: config entries when
// present, otherwise the build-time default. Empty result = no
// pinning (standard verification only).
func (c *Client) activePins() ([][]byte, error) {
	entries := c.Pins
	if len(entries) == 0 && DefaultPin != "" {
		entries = SplitPins(DefaultPin)
	}
	return ParsePins(entries)
}

// pinVerifier returns a VerifyPeerCertificate callback enforcing the
// pin list on the leaf (first) certificate. crypto/tls calls it after
// the normal chain verification, so both checks hold.
func pinVerifier(pins [][]byte) func(rawCerts [][]byte, _ [][]*x509.Certificate) error {
	return func(rawCerts [][]byte, _ [][]*x509.Certificate) error {
		if len(rawCerts) == 0 {
			return errors.New("no certificate presented")
		}
		cert, err := x509.ParseCertificate(rawCerts[0])
		if err != nil {
			return fmt.Errorf("parse peer certificate: %w", err)
		}
		sum, err := SPKIHash(cert)
		if err != nil {
			return err
		}
		for _, want := range pins {
			if string(sum) == string(want) {
				return nil
			}
		}
		return fmt.Errorf(
			"hub certificate pin mismatch: got sha256/%s — the network may be intercepting TLS; see docs/USAGE.md (troubleshooting)",
			base64.StdEncoding.EncodeToString(sum))
	}
}
