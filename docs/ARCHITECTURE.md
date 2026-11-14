# Architecture

Three artifacts, one protocol.

## 1. Components

### Engine — `cmd/engine` (Go, Windows + Linux, one static binary per OS)

The desktop program group members install. Responsibilities:

- **Sender and receiver** of transfers; runs the transfer state machine.
- **HTTP server on the LAN** (default `:8090`) that serves the web app and the
  chunk API — this is what phones talk to in LAN mode.
- **LAN IP detection**: enumerates non-loopback IPv4 interfaces; if several
  exist (VPN adapters are the classic trap), the UI asks which one to put in
  the QR code.
- **QR code generation** for the receive link (see `PROTOCOL.md` §2).
- mDNS/zeroconf announcement so engines find each other PC↔PC (M1).
- USB stick watching + marker import/export (M4).
- No background daemon-ness required: you launch it, you transfer, you close it.

Config lives in `~/.config/chunkdrop/engine.toml` (Linux) /
`%APPDATA%\chunkdrop\engine.toml` (Windows): group key, port, receive
directory, caps. `engine keygen` prints a fresh group key.

### Hub — `cmd/hub` (Go, runs in Docker on the VPS)

A stateless coordination service. Responsibilities:

- Serves the same web app (embedded via `go:embed`) for internet mode.
- **WebSocket signaling**: pairs sender and receiver into short-code "rooms",
  relays SDP/ICE candidates between them.
- **Fallback relay**: when WebRTC hole-punching fails, both sides open a WSS
  connection and the hub pipes opaque (end-to-end encrypted) frames between
  them. See `SECURITY.md` §5 for why the hub still cannot read the bytes.
- **Never writes user files to disk.** No volume mounts, read-only root
  filesystem. A `/healthz` endpoint for Docker healthchecks.

Everything else — TLS, compression, firewall-facing 443 — is handled by the
nginx already running on the VPS host (`DEPLOY-VPS.md`).

### Web app — `web/` (Preact + Vite + TypeScript)

One codebase, embedded into **both** binaries with `go:embed`. Served:

- from the **engine** at `http://<lan-ip>:8090/r/` (LAN mode, QR-scanned), and
- from the **hub** at `https://xfer.example.com/` (internet mode, installable PWA).

Two modes in the UI:
- **Receive mode** — file list, per-file/per-chunk progress, download &
  "save all", chunk-hash verification, retry handling.
- **Sender dashboard** — file/folder picker, QR screen, live receiver progress
  (receivers POST their chunk bitmaps; the sender page polls session state).

Browser capability matrix we design around:

| Capability | Chrome/Edge desktop | Firefox desktop | iOS Safari | Android Chrome |
|---|---|---|---|---|
| Download chunked + verify (hash-wasm) | ✅ | ✅ | ✅ | ✅ |
| Save many files as a real folder tree (File System Access API) | ✅ | ❌ → ZIP fallback | ❌ → ZIP fallback | ❌ → ZIP fallback |
| PWA install | ✅ | n/a | ✅ (Add to Home Screen) | ✅ |
| WebRTC DataChannel (M2) | ✅ | ✅ | ✅ | ✅ |
| OPFS persistent chunk store (M3) | ✅ | ✅ | ✅ (15.2+) | ✅ |
| WebCrypto | ✅ (HTTPS only) | ✅ (HTTPS only) | ✅ (HTTPS only) | ✅ (HTTPS only) |

**Why hash-wasm instead of WebCrypto for hashing:** `crypto.subtle` is only
available in secure contexts; the LAN page is plain `http://192.168.x.x`.
hash-wasm (WASM SHA-256, ~GB/s) works everywhere. WebCrypto is used only on
the HTTPS-served internet mode (M2 E2E encryption).

## 2. Data flows

### LAN send (M0) — the primary flow

```
Engine (PC)                                   Phone browser
   │                                                │
   │ 1. user picks files → manifest built            │
   │    (4 MiB chunks, SHA-256 each, gzip)          │
   │ 2. session created (id + 16-byte key)          │
   │ 3. QR rendered:                                │
   │    http://192.168.1.20:8090/r/#k=<key>         │
   │                                    ┌───────────┤
   │ 4a. GET /r/          (page)  ◄─────│ camera scan
   │ 4b. GET /api/manifest        ◄─────│ X-Session-Key header
   │ 4c. GET /api/chunk/0/17      ◄─────│ per chunk, 3 retries, hash-wasm verify
   │ 4d. POST /api/progress       ◄─────│ chunk bitmap every ~2 s
   │ 5. "save file" via <a download> /       │
   │    File System Access (desktop)         │
   │ 6. engine UI shows live %  (poll)       │
```

No discovery protocol involved at all — **the QR is both discovery and
authentication**. This is why M0 works on a phone that has nothing installed.

### Internet send (M2) — the secondary flow

```
Engine (sender PC)          VPS (nginx → hub in Docker)          Receiver browser
   │  WSS: create room          │                                    │
   │───────────────────────────►│── 6-char code + room secret ──►  │
   │                            │        (engine shows QR/short URL)│
   │◄──────────── WS signaling: SDP / ICE relayed both ways ──────►│
   │                                                                  │
   │════════════ WebRTC DataChannel (direct, DTLS) ════════════════►│
   │         bytes never touch the VPS                                │
   │                                                                  │
   │   if hole-punch fails:                                           │
   │═══ WSS relay via hub:443, frames AES-GCM end-to-end ═══════════►│
```

### USB sneakernet (M4)

```
Engine A: "export to USB" → marker `.chunkdrop.json` + .chunks/f0-c000123.bin …
Stick travels in a pocket.
Engine B: mount polling detects marker → hashes verified → chunks imported
into session state → only missing chunks are fetched from the network.
```

## 3. Technology choices and rationale

| Choice | Why this, not that |
|---|---|
| Go | One static binary per OS (cross-compile from any machine, incl. linux/arm64 for NAS), superb stdlib HTTP, `go:embed`, pion ecosystem; Rust offered smaller binaries at a much steeper solo-dev cost |
| pion/webrtc | Production-grade Go WebRTC (LiveKit et al.) — the engine must speak DataChannel for internet mode |
| Preact + Vite | Tiny (~4 KB) framework is enough for lists/progress/QR/code entry; TS built in; no React-sized bundle for a tool page |
| hash-wasm | SHA-256 that works on plain-HTTP LAN pages where WebCrypto is blocked (secure-context rule) |
| Plain HTTP REST for LAN (no gRPC/WebSockets in M0) | Zero-friction: chunk GETs are retryable, cache-friendly, trivially debuggable; WebSockets enter at M2 for signaling only |
| STUN + WSS-relay-443 instead of coturn | Relay on 443 passes every corporate/hotel firewall; one Go codebase we control; coturn stays in the back pocket (D9) |
| Hub in Docker, nginx on host | User's VPS already runs nginx with certs; Docker isolates our code, keeps the host clean; hub is stateless so the container has no volumes at all |
| Store-mode ZIP for many-small→mobile | ZIP "store" (no compression) costs ~zero CPU on the engine and collapses 800 downloads into 1 on an iPhone |

## 4. State and storage

- **Engine**: sessions live in RAM + a small state directory
  (`~/.local/share/chunkdrop/` / `%APPDATA%`) holding resume bitmaps and the
  receive directory. Transfers expire after 24 h or cancel.
- **Hub**: entirely stateless. Rooms are in-memory, vanish on restart (worst
  case: re-enter a 6-char code). No database, no volumes, nothing persistent.
- **VPS disk**: nothing of the users' ever. Only the container image.

## 5. Repository layout

```
chunkdrop/
├── cmd/
│   ├── engine/main.go        # desktop binary
│   └── hub/main.go           # VPS binary
├── internal/
│   ├── api/                  # REST endpoints, auth middleware
│   ├── transfer/             # manifest, chunking, SHA-256, resume bitmaps
│   ├── session/              # session lifecycle, keys, expiry
│   ├── lanip/                # interface enumeration for QR
│   ├── mdns/                 # engine↔engine discovery (M1)
│   ├── webrtc/               # pion glue (M2)
│   └── usb/                  # marker, import/export, mount polling (M4)
├── web/                      # Preact + Vite + TS, built to web/dist,
│                             #   embedded via go:embed in both binaries
├── deploy/
│   ├── Dockerfile            # node→go→distroless multi-stage (hub)
│   ├── docker-compose.yml
│   └── nginx-site.conf.example
└── docs/                     # these documents
```
