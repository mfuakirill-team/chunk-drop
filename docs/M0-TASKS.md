# M0 Build Checklist — "LAN one-shot send"

Goal: **first version the group can actually use.** No VPS, no internet mode,
no Docker — engine + phone browser only. Build in this order; each step ends
in something testable.

Definition of done (from `PLAN.md`):

> Windows engine → QR → iPhone Safari saves a 4 GB file; a WiFi hiccup
> mid-transfer doesn't restart it; wrong group/session key is rejected.

## Step list

### 1. Repo scaffold
- [ ] `go mod init` (module name = repo), dirs per `ARCHITECTURE.md` §5
- [ ] `web/` with `npm create vite@latest -- --template preact-ts`;
      add `hash-wasm`, `qrcode`; output goes to `web/dist`
- [ ] `//go:embed` of `web/dist` into a shared `internal/webui` package
      (served by both engine now and hub later)
- [ ] `Makefile`: `make web` (vite build), `make engine-win`
      (`GOOS=windows GOARCH=amd64`), `make engine-linux` (amd64 + arm64)

### 2. Config + keys
- [ ] TOML config: group key, port (default 8090), receive dir, size caps
- [ ] `engine keygen` subcommand: prints fresh 32-byte base64url group key
- [ ] Paths: `~/.config/chunkdrop/engine.toml` / `%APPDATA%\chunkdrop\engine.toml`

### 3. LAN IP detection
- [ ] Enumerate non-loopback IPv4 interfaces
- [ ] Multiple candidates (VPN adapters!) → UI picker; single candidate → auto
- [ ] Bind the HTTP server to the chosen interface when practical

### 4. Manifest + chunking (`internal/transfer`)
- [ ] Walk picked files/folder → manifest per `PROTOCOL.md` §3 (4 MiB chunks,
      SHA-256 per chunk, gzip on serve)
- [ ] Never load whole files into RAM — stream reads by chunk offsets
- [ ] **Store-mode ZIP packer**: single-file manifest when > 50 files and
      receiver is mobile (`PROTOCOL.md` §3 rules) — archive/zip `Store` method,
      no compression
- [ ] Unit tests: last-short-chunk, empty file, unicode names, 10k files

### 5. Session + auth (`internal/session`, `internal/api`)
- [ ] Session: id + 16-byte key, RAM-only, 24 h expiry, cancel
- [ ] Middleware: `X-ChunkDrop-Key` constant-time compare; 401 generic
- [ ] Endpoints: `/r/`, `/api/manifest`, `/api/chunk/{f}/{c}`,
      `/api/progress`, `/api/cancel`, `/api/session` per `PROTOCOL.md` §4

### 6. Filename safety (`internal/transfer/sanitize.go`)
- [ ] All 7 rules of `PROTOCOL.md` §8, each with a unit test:
      traversal, absolute paths, reserved device names, trailing dots/spaces,
      NFC normalization, length/depth caps, case-collision rename
- [ ] Re-check on the receiver side before any disk write (upload path lands
      in M1, but the module ships tested now)

### 7. Web app — receive page (Preact)
- [ ] Read `#k=` fragment → key in `X-ChunkDrop-Key` on all calls
- [ ] Manifest view: tree, sizes, per-file progress
- [ ] Chunk pipeline: sequential per file, 2–3 in flight, hash-wasm verify,
      3 retries with backoff, per-chunk error surface (never restart the file)
- [ ] Save: desktop Chromium → File System Access API (real folder tree);
      others → `<a download>` per file
- [ ] Progress POST every ~2 s (chunk bitmap)

### 8. Web app — sender dashboard (same app, served on `/`)
- [ ] File/folder picker (engine side, native dialog or page)
- [ ] QR screen with the receive URL (tiny QR lib; big, quiet zone — phones)
- [ ] Live receiver progress (poll `/api/session`)
- [ ] "Copy link" fallback for when QR can't be scanned

### 9. Cross-compile + manual test matrix
- [ ] Windows 10/11 (amd64): firewall prompt on first run (expected), engine runs
- [ ] Ubuntu/Debian amd64 (+ arm64 build for later NAS use)
- [ ] iPhone Safari: scan QR from camera → save 4 GB file → verify in Files app
- [ ] Android Chrome: same
- [ ] Desktop Firefox + Chrome as receivers
- [ ] **Hiccup test**: airplane-mode the phone for 5 s mid-transfer → resumes
- [ ] **Honest failure test**: close the tab mid-transfer → restarts cleanly
      (resume-after-close is M3, not M0 — must not corrupt anything)
- [ ] Wrong/absent session key → 401, no listing, no path hints

### 10. Distribution (zero cost)
- [ ] GitHub Releases with the two binaries (or a `dl/` route on the VPS
      behind the group key once M2 exists)
- [ ] One-page "install & first send" note for the group (this doc's §M0 DoD
      as the acceptance script)

## After M0

Go straight to `M1` (upload direction phone→PC, folder trees, mDNS PC↔PC,
ZIP default-on for mobile) — still no server needed. M2 (VPS/Docker/internet
mode) only after LAN mode has carried real files for the group.
