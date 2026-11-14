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
}

// Default returns the built-in defaults (used when no config file exists —
// the engine then prints a fresh group key hint on first run).
func Default() Config {
	return Config{
		Port:           DefaultPort,
		MaxUploadBytes: 100 << 30,
		ReceiveDir:     "",
	}
}

// Load reads the engine config, creating a template on first run.
func Load() (Config, string, error) {
	path, err := ConfigPath()
	if err != nil {
		return Default(), "", err
	}
	cfg := Default()
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

// ChunkSizeDefault re-exports the transfer default for config UIs.
func ChunkSizeDefault() int { return transfer.DefaultChunkSize }
