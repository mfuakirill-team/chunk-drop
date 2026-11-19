// Package config loads the engine's TOML config from
// ~/.config/chunkdrop/engine.toml (Linux) or %APPDATA%\chunkdrop\engine.toml
// (Windows), with `engine keygen` generating a fresh group key.
package config

import (
	"crypto/rand"
	"encoding/base64"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"regexp"
	"runtime"
	"strings"

	"chunkdrop/internal/transfer"

	"github.com/BurntSushi/toml"
)

// DefaultGroupKeyEnv lets Docker/other env-based setups pass the group
// key without mounting a config file (deploy/Dockerfile): the engine
// reads it as a fallback when the config file has no key.
const DefaultGroupKeyEnv = "CHUNKDROP_GROUP_KEY"

// Defaults for the engine config.
const (
	DefaultPort    = 8090
	DefaultMaxBody = 512 << 20 // progress/manifest POST cap: generous but finite

	// MeshModeAuto is the default mesh mode: LAN and internet both on.
	MeshModeAuto = "auto"

	// HubChannelAuto is the default hub channel: wss first, inner-TLS
	// over ws:// on failure.
	HubChannelAuto = "auto"

	// DefaultHubURL / DefaultStunURL preconfigure the internet mesh in
	// fresh installs and first-run templates: the user runs the engine
	// and the hub is already wired up. An engine.toml that *explicitly*
	// sets hub_url = "" still turns the internet mesh off — empty means
	// off, the preconfigured default is only what a fresh install gets.
	DefaultHubURL  = "wss://drop.404.mn/hub/ws"
	DefaultStunURL = "stun:drop.404.mn:3478"
)

// Config is the engine's TOML file.
type Config struct {
	// GroupKey is the long-term group key (32 bytes, base64url) — gates
	// engine↔engine/engine↔hub usage (M1/M2); LAN receivers only need the
	// per-transfer session key.
	GroupKey string `toml:"group_key"`

	// Port the HTTP server binds to (default 8090).
	Port int `toml:"port"`

	// BindIP forces the LAN address to advertise in QR links. Empty = auto.
	BindIP string `toml:"bind_ip"`

	// ReceiveDir is where M1 uploads land. Empty = default (see DefaultReceiveDir).
	ReceiveDir string `toml:"receive_dir"`

	// MaxUploadBytes caps a single upload transfer (M1); 0 = default 100 GiB.
	MaxUploadBytes int64 `toml:"max_upload_bytes"`

	// AllowRemoteDashboard opens the dashboard/control surface (session
	// creation, file pickers, peers, activity) to other devices. Off by
	// default: the dashboard controls this PC, so only this machine may
	// use it; receiving links always work from other devices.
	AllowRemoteDashboard bool `toml:"allow_remote_dashboard"`

	// HubURL is the M2 hub's websocket endpoint, e.g.
	// "wss://drop.404.mn/hub/ws". Empty = the internet mesh is off:
	// the engine behaves exactly as in M0/M1 (LAN only). The hub is
	// optional infrastructure; LAN keeps working without it.
	HubURL string `toml:"hub_url"`

	// StunURL is the STUN server (e.g. "stun:drop.404.mn:3478") used
	// for WebRTC server-reflexive candidates when punching through
	// NATs. Empty is fine on a plain LAN (host candidates suffice).
	StunURL string `toml:"stun_url"`

	// MeshMode is the global mesh mode (M2 step ④): how THIS engine
	// interacts with all other peers. One of:
	//
	//   - "auto"      — everything: LAN peers direct, internet peers
	//     via the hub (tunnel first, relay fallback).
	//   - "local"     — the local network only: the hub connection is
	//     dropped entirely; the Clients card shows LAN engines.
	//   - "internet"  — the global internet only: LAN-discovered peers
	//     are skipped; peers join via hub_url/stun_url (tunnel first,
	//     relay fallback).
	//
	// Persisted (the dashboard's mode switch writes it) so the choice
	// survives a restart. Empty/unknown → auto.
	MeshMode string `toml:"mesh_mode"`

	// HubCertPin is an optional SPKI pin for the hub's TLS certificate:
	// comma-separated list of "sha256/<base64>" fingerprints of the
	// certificate's public key (`make pin` prints the current value).
	// Non-empty → the hub's presented certificate must match one entry
	// IN ADDITION to normal chain verification — a MITM whose root CA
	// is installed in the trust store still fails. Empty (default) =
	// standard verification only. Empty here but a pin baked into the
	// binary at build time (hubclient.DefaultPin) → the baked pin wins.
	HubCertPin string `toml:"hub_cert_pin"`

	// HubChannel selects the hub connection channel (M3):
	//
	//   - "auto"    — default: plain wss first; on dial/verification
	//     failure (e.g. a MITM network aborting the TLS handshake),
	//     fall back to the inner-TLS channel (real TLS inside a
	//     plaintext ws:// connection). Whichever channel last worked
	//     is tried first until it stops working.
	//   - "tls"     — force plain wss only (never ws://).
	//   - "ws-tls"  — force the inner-TLS channel only.
	//
	// Unknown/empty → auto.
	HubChannel string `toml:"hub_channel"`

	// HubWSURL overrides the ws:// endpoint used by the inner-TLS
	// channel. Empty = derive it from hub_url: same host and path,
	// ws:// scheme, port 443 dropped (80 implied). Only useful when
	// the ws endpoint lives on a nonstandard port or path.
	HubWSURL string `toml:"hub_ws_url"`
}

// Hub channel vocabulary (HubChannel above; validated at load).
var HubChannels = map[string]bool{
	"auto":   true,
	"tls":    true,
	"ws-tls": true,
}

// ValidHubChannel reports whether s is an accepted hub_channel value.
func ValidHubChannel(s string) bool { return HubChannels[s] }

// Mesh mode vocabulary (MeshMode above; validated at load and at the
// /api/mode endpoint).
var MeshModes = map[string]bool{
	"auto":     true,
	"local":    true,
	"internet": true,
}

// ValidMeshMode reports whether s is an accepted mesh mode value.
func ValidMeshMode(s string) bool { return MeshModes[s] }

// Default returns the built-in defaults (used when no config file exists —
// the engine then prints a fresh group key hint on first run).
func Default() Config {
	return Config{
		Port:           DefaultPort,
		MaxUploadBytes: 100 << 30,
		ReceiveDir:     "",
		HubURL:         DefaultHubURL,
		StunURL:        DefaultStunURL,
		MeshMode:       MeshModeAuto,
		HubChannel:     HubChannelAuto,
	}
}

// Load reads the engine config, creating a template on first run.
func Load() (Config, string, error) {
	path, err := ConfigPath()
	if err != nil {
		return Default(), "", err
	}
	cfg := Default()
	if cfg.MeshMode == "" {
		cfg.MeshMode = MeshModeAuto
	}
	if _, err := os.Stat(path); os.IsNotExist(err) {
		if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
			return cfg, path, fmt.Errorf("create config dir: %w", err)
		}
		tmpl := Default()
		tmpl.GroupKey = "<paste `engine keygen` output here>"
		b, err := toml.Marshal(tmpl)
		if err != nil {
			return cfg, path, err
		}
		if err := os.WriteFile(path, b, 0o600); err != nil {
			return cfg, path, fmt.Errorf("write config template: %w", err)
		}
		return cfg, path, nil
	}
	if _, err := toml.DecodeFile(path, &cfg); err != nil {
		if errors.Is(err, fs.ErrPermission) {
			// The common case is a 0600 engine.toml in a container that
			// runs as another uid — say how to fix it, not just "denied".
			return cfg, path, fmt.Errorf("parse %s: %w\n"+
				"  the engine user cannot read the config (engine keygen writes it 0600).\n"+
				"  Docker: run the container as the file owner — PUID=$(id -u) PGID=$(id -g) docker compose up -d;\n"+
				"  alternatively chmod 644 the file, or pass CHUNKDROP_GROUP_KEY=<key> and mount nothing.", path, err)
		}
		return cfg, path, fmt.Errorf("parse %s: %w", path, err)
	}
	if cfg.Port == 0 {
		cfg.Port = DefaultPort
	}
	if cfg.MaxUploadBytes == 0 {
		cfg.MaxUploadBytes = 100 << 30
	}
	// Mesh mode: empty (old config files) or a typo → auto.
	if !ValidMeshMode(cfg.MeshMode) {
		cfg.MeshMode = MeshModeAuto
	}
	// Hub channel: empty (old config files) or a typo → auto.
	if !ValidHubChannel(cfg.HubChannel) {
		cfg.HubChannel = HubChannelAuto
	}
	// Env fallback (Docker): a key in the environment beats "no key yet".
	if cfg.GroupKey == "" || strings.Contains(cfg.GroupKey, "<paste") {
		if k := strings.TrimSpace(os.Getenv(DefaultGroupKeyEnv)); k != "" {
			cfg.GroupKey = k
		}
	}
	return cfg, path, nil
}

// ConfigPath returns the OS-specific engine config path.
func ConfigPath() (string, error) {
	home := homeDir()
	if runtime.GOOS == "windows" {
		appdata := os.Getenv("APPDATA")
		if appdata == "" {
			appdata = filepath.Join(home, "AppData", "Roaming")
		}
		return filepath.Join(appdata, "chunkdrop", "engine.toml"), nil
	}
	return filepath.Join(home, ".config", "chunkdrop", "engine.toml"), nil
}

// DefaultReceiveDir returns where received uploads land by default.
func DefaultReceiveDir() (string, error) {
	home := homeDir()
	if runtime.GOOS == "windows" {
		appdata := os.Getenv("APPDATA")
		if appdata == "" {
			appdata = filepath.Join(home, "AppData", "Roaming")
		}
		return filepath.Join(appdata, "chunkdrop", "received"), nil
	}
	// Follow XDG when present, like every desktop tool should.
	if xdd := strings.TrimSpace(os.Getenv("XDG_DOWNLOAD_DIR")); xdd != "" && !strings.Contains(xdd, "$") {
		return filepath.Join(xdd, "chunkdrop"), nil
	}
	return filepath.Join(home, ".local", "share", "chunkdrop", "received"), nil
}

// homeDir swallows the home-dir error — empty home degrades to a
// relative path, which callers can still work with.
func homeDir() string {
	h, _ := os.UserHomeDir()
	return h
}

// Keygen prints a fresh 32-byte base64url group key (the `engine keygen`
// subcommand; SECURITY.md §7 rotation procedure).
func Keygen() (string, error) {
	k := make([]byte, 32)
	if _, err := rand.Read(k); err != nil {
		return "", err
	}
	return base64.RawURLEncoding.EncodeToString(k), nil
}

// ValidKey reports whether s can be stored as a group key: it must be
// representable in a TOML basic string and an HTTP header — printable
// characters, no quotes or backslashes, no control characters. There is
// deliberately no strength requirement: a key either matches the group
// or it does not, and the owner may choose any value (a 4-character
// "test" is a valid — if weak — group key).
func ValidKey(s string) bool {
	if s == "" || len(s) > 256 {
		return false
	}
	for _, r := range s {
		if r < ' ' || r > '~' || r == '"' || r == '\\' {
			return false
		}
	}
	return true
}

// UsableKey reports whether k can actually gate the mesh: a real key —
// not empty and not the first-run placeholder. The placeholder in a
// fresh engine.toml is NOT a configured key: the dashboard must be
// allowed to set the first key without presenting it as a credential,
// and no dashboard route may treat it as one.
func UsableKey(k string) bool {
	return k != "" && !strings.Contains(k, "<paste")
}

// SaveGroupKey rewrites the group_key line in the engine.toml at path,
// preserving everything else (comments, field order) — the dashboard
// sets the key (M2), and the file must stay human-editable. Missing
// group_key line → appended at the end; missing file → a fresh
// template with the key filled in. Written 0600 like the rest.
func SaveGroupKey(path, key string) error {
	if !ValidKey(key) {
		return fmt.Errorf("refusing to save invalid group key")
	}
	return saveTOMLLine(path, "group_key", `"`+key+`"`)
}

// SaveMeshMode rewrites the mesh_mode line in the engine.toml at path
// (M2 step ④): the dashboard's global mode switch persists here so the
// choice survives a restart. Same file rules as SaveGroupKey.
func SaveMeshMode(path, mode string) error {
	if !ValidMeshMode(mode) {
		return fmt.Errorf("refusing to save unknown mesh mode %q", mode)
	}
	return saveTOMLLine(path, "mesh_mode", `"`+mode+`"`)
}

// saveTOMLLine rewrites `key = <value>` in the TOML file at path,
// preserving everything else (comments, field order). Missing key line
// → appended at the end; missing file → a fresh Default() template
// with the field set. Written 0600.
func saveTOMLLine(path, key, value string) error {
	data, err := os.ReadFile(path)
	if errors.Is(err, fs.ErrNotExist) {
		if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
			return fmt.Errorf("create config dir: %w", err)
		}
		tmpl := Default()
		if m, ok := map[string]*string{
			"group_key": &tmpl.GroupKey,
			"mesh_mode": &tmpl.MeshMode,
		}[key]; ok {
			*m = strings.Trim(value, `"`)
		}
		b, err := toml.Marshal(tmpl)
		if err != nil {
			return err
		}
		return os.WriteFile(path, b, 0o600)
	}
	if err != nil {
		return fmt.Errorf("read %s: %w", path, err)
	}
	line := []byte(key + " = " + value)
	re := regexp.MustCompile(`(?m)^[ \t]*` + regexp.QuoteMeta(key) + `[ \t]*=.*$`)
	if re.Match(data) {
		data = re.ReplaceAll(data, line)
	} else {
		if len(data) > 0 && data[len(data)-1] != '\n' {
			data = append(data, '\n')
		}
		data = append(data, line...)
		data = append(data, '\n')
	}
	return os.WriteFile(path, data, 0o600)
}

// ChunkSizeDefault re-exports the transfer default for config UIs.
func ChunkSizeDefault() int { return transfer.DefaultChunkSize }
