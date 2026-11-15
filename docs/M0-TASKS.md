# M0 Build Checklist — "LAN one-shot send"

Goal: **first version the group can actually use.** No VPS, no internet mode,
no Docker — engine + phone browser only. Build in this order; each step ends
in something testable.

Definition of done (from `PLAN.md`):

> Windows engine → QR → iPhone Safari saves a 4 GB file; a WiFi hiccup
> mid-transfer doesn't restart it; wrong group/session key is rejected.

## Step list

### 1. Repo scaffold
- [x] `go mod init` (module name = repo), dirs per `ARCHITECTURE.md` §5
- [x] `web/` with `npm create vite@latest -- --template preact-ts`;
      add `hash-wasm`, `qrcode`; output goes to `web/dist`
- [x] `//go:embed` of `web/dist` into a shared `internal/webui` package
      (served by both engine now and hub later)
- [x] `Makefile`: `make web` (vite build), `make engine-win`
      (`GOOS=windows GOARCH=amd64`), `make engine-linux` (amd64 + arm64)

### 2. Config + keys
- [x] TOML config: group key, port (default 8090), receive dir, size caps
- [x] `engine keygen` subcommand: prints fresh 32-byte base64url group key
- [x] Paths: `~/.config/chunkdrop/engine.toml` / `%APPDATA%\chunkdrop\engine.toml`

### 3. LAN IP detection
- [x] Enumerate non-loopback IPv4 interfaces
- [x] Multiple candidates (VPN adapters!) → UI picker; single candidate → auto
- [x] Bind the HTTP server to the chosen interface when practical

### 4. Manifest + chunking (`internal/transfer`)
- [x] Walk picked files/folder → manifest per `PROTOCOL.md` §3 (4 MiB chunks,
      SHA-256 per chunk, gzip on serve)
- [x] Never load whole files into RAM — stream reads by chunk offsets
- [x] **Store-mode ZIP packer**: single-file manifest when > 50 files and
      receiver is mobile (`PROTOCOL.md` §3 rules) — archive/zip `Store` method,
      no compression
- [x] Unit tests: last-short-chunk, empty file, unicode names, 10k files

### 5. Session + auth (`internal/session`, `internal/api`)
- [x] Session: id + 16-byte key, RAM-only, 24 h expiry, cancel
- [x] Middleware: `X-ChunkDrop-Key` constant-time compare; 401 generic
- [x] Endpoints: `/r/`, `/api/manifest`, `/api/chunk/{f}/{c}`,
      `/api/progress`, `/api/cancel`, `/api/session` per `PROTOCOL.md` §4

### 6. Filename safety (`internal/transfer/sanitize.go`)
- [x] All 7 rules of `PROTOCOL.md` §8, each with a unit test:
      traversal, absolute paths, reserved device names, trailing dots/spaces,
      NFC normalization, length/depth caps, case-collision rename
- [x] Re-check on the receiver side before any disk write (upload path lands
      in M1, but the module ships tested now)

### 7. Web app — receive page (Preact)
- [x] Read `#k=` fragment → key in `X-ChunkDrop-Key` on all calls
- [x] Manifest view: tree, sizes, per-file progress
- [x] Chunk pipeline: sequential per file, 2–3 in flight, hash-wasm verify,
      3 retries with backoff, per-chunk error surface (never restart the file)
- [x] Save: desktop Chromium → File System Access API (real folder tree);
      others → `<a download>` per file
- [x] Progress POST every ~2 s (chunk bitmap)

### 8. Web app — sender dashboard (same app, served on `/`)
- [x] File/folder picker (engine side, native dialog or page)
- [x] QR screen with the receive URL (tiny QR lib; big, quiet zone — phones)
- [x] Live receiver progress (poll `/api/session`)
- [x] "Copy link" fallback for when QR can't be scanned

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
- [x] GitHub Releases are automated (`.github/workflows/release.yml`):
      create a `v*` tag (or a release with a tag) in the GitHub UI →
      the Release workflow builds the four engines + `SHA256SUMS` on
      Debian Trixie and attaches them to the release. `.github/
      workflows/build.yml` builds artifacts on every push (Actions →
      Build → "Run workflow" for a manual rerun); tag pushes skip it.
- [ ] One-page "install & first send" note for the group (this doc's §M0 DoD
      as the acceptance script)

### 9 note — real-device matrix still open
Steps 1–8 are done in code; the hardware test matrix (Windows engine,
iPhone Safari, Android Chrome, hiccup/failure tests) still needs real
devices — see `USAGE.md` §5.8. Treat the checkboxes above as "built",
not "device-tested".

## M0.1 / M0.2 additions (shipped)

### M0.1 — engine remote-control + sync
- [x] Native pick dialogs on the engine (`internal/picker`, `/api/pick/*`)
- [x] Group-key persistence in the dashboard (`localStorage`)
- [x] UDP discovery beacons :8091 (HMAC, replay window, peers panel)
- [x] Sync folder `~/Downloads/chunk-drop-files` (PC↔PC, xxhash cache,
      `/api/sync/state`, `/api/sync/start`)

### M0.2 — dashboard UX + phones + Docker
- [x] Selected paths in a visible frame + **Reset selection** (`send.tsx`)
- [x] **Cancel** next to **Start transfer** (sender-side session end)
- [x] **Activity** feed below LAN clients (`/api/activity`, ring buffer,
      live upload summaries; receiver progress moved to the side panel)
- [x] PWA: manifest + generated icons (`scripts/genicons`), iOS/Android
      install instructions (`USAGE.md` §4.9); **no service worker** (design)
- [x] `GET /api/peers → 404` from phone diagnosed: stale binary — routes
      exist since M0.1; wrong key would be 401 (`USAGE.md` §5.0)
- [x] Engine Docker image (`deploy/Dockerfile`, `docker-compose.yml`,
      non-root, ~15 MB, 8090/tcp + 8091/udp, `CHUNKDROP_GROUP_KEY` env)
      — **for LAN machines (NAS/home server)**; the VPS container
      (hub: relay + PWA host, client-side opt-out) stays M2 per
      `DEPLOY-VPS.md`
- [x] Sync delete-propagation race fixed: per-file "last seen
      announced" epochs — a peer restarting/rescanning no longer makes
      other engines delete files mid-transfer

## After M0

Go straight to `M1` (upload direction phone→PC, folder trees, mDNS PC↔PC,
ZIP default-on for mobile) — still no server needed. M2 (VPS/Docker/internet
mode) only after LAN mode has carried real files for the group.
