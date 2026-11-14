# Master Plan

Project: **ChunkDrop** (working name) — private, chunked, resumable file transfer.
Companion docs: `ARCHITECTURE.md`, `PROTOCOL.md`, `SECURITY.md`, `DEPLOY-VPS.md`, `M0-TASKS.md`.

## 1. Problem and constraints

A small private group needs to move files between Windows/Linux PCs and
Android/iOS phones:

| Constraint | Value |
|---|---|
| Usage pattern | One-shot transfers; phones never run in background |
| Location | Mostly same room (QR from PC screen); occasionally different cities |
| Content | "Always different" — multi-GB files *and* many-small-file batches |
| Budget | **0** — VPS with dedicated public IP is already owned and paid; own domain; existing nginx on that VPS |
| VPS policy | Runs in **Docker**; **never stores user files**; relay/signaling only |
| Stores/reviews | Out: no App Store / Play Store; phones use browser or installed PWA |
| Effort | Solo developer, evenings — plan is staged so every milestone is shippable alone |

## 2. Locked decisions

These were settled during the design discussion. Change them only consciously.

| # | Topic | Decision | Reason |
|---|---|---|---|
| D1 | Transfer model | Torrent-*like*: chunked, hash-verified, resumable; multi-peer swarm later. **No BitTorrent compatibility** (no DHT, trackers, magnet links) | Closed private group; avoids Apple's torrent ban entirely, no ISP shaping, no piracy optics; we keep full control of UX |
| D2 | Platforms | Native engine: **Windows + Linux** (single static binary each). Phones: **browser/PWA only** (Safari/Chrome). No store distribution | Zero budget, zero Apple account; one-shot foreground use fits browser limits |
| D3 | Priority | **LAN/QR first** (M0–M1), internet mode second (M2) | Group usage is mostly "scan QR from the PC screen"; M0 then needs no VPS work at all |
| D4 | Server | Own VPS; **hub runs in Docker**; existing **nginx stays on the host** for TLS; own domain, subdomain + certbot | User requirement; nginx already deployed and known; see `DEPLOY-VPS.md` |
| D5 | Data policy | Hub never writes user files to disk — signaling + encrypted-byte relay only | User requirement; also minimizes trust and liability |
| D6 | Stack | **Go** for engine + hub (stdlib `net/http`, `go:embed`, `pion/webrtc`); **Preact + Vite + TS** web app; **hash-wasm** SHA-256 in browser | Single-binary deploys, cross-compilation, one UI codebase; hash-wasm works on plain-HTTP LAN pages where WebCrypto is unavailable |
| D7 | Chunking | 4 MiB default chunk, SHA-256 per chunk; **store-mode ZIP packing** offered when a batch has many small files and the receiver is mobile | Group sends both extremes; store-mode ZIP is ~free CPU and turns N downloads into 1 |
| D8 | Auth | Long-term **group key** (engine/hub config) + per-transfer **session key** (QR/short-link URL fragment, never on the wire). UA/version checks are metadata only | A spoofable header is not a lock; keys are free and real |
| D9 | NAT traversal | Free public STUN + **WSS relay fallback on 443** through the VPS. No coturn initially | Relay-on-443 passes every firewall and is simpler to operate; add coturn only if measurements demand it |
| D10 | Cost lines | Binaries: GitHub Releases and/or VPS-hosted downloads behind the group key. STUN: free public servers. Certs: Let's Encrypt via certbot. | 0 €/month total; electricity only |

## 3. Milestones

Every milestone ends in something the group can actually use.

| Phase | Delivers | "Done" when | Rough solo effort |
|---|---|---|---|
| **M0 — LAN one-shot** | Engine (Win+Linux) + web receive page; QR pairing; chunked SHA-256 transfer with per-chunk retry | Windows engine → QR → iPhone Safari saves a 4 GB file; WiFi hiccup mid-transfer doesn't restart it; group key required | 1–2 weeks |
| **M1 — Full LAN** | Folder trees; phone→PC push (upload direction); mDNS PC↔PC; store-mode ZIP packing for many-small→mobile | 800-photo folder lands as a tree on Chrome/Edge desktop and as one ZIP on iPhone; phone pushes photos to PC; two PCs see each other without QR | 1–2 weeks |
| **M2 — Internet mode** | Hub in Docker on VPS (existing nginx in front); short-code rooms; WebRTC DataChannel (pion ↔ browser); WSS relay fallback; PWA installable from the VPS page | Windows PC → iPhone on LTE via 6-char code; bytes go P2P (VPS only signals); transfer completes via relay when hole-punch fails | 2–3 weeks (WebRTC glue is the trickiest part) |
| **M3 — Swarm + browser resume** | Multiple receivers pull missing chunks from *each other*; OPFS chunk store so a closed tab reopens and continues | 3 receivers on 3 networks finish faster than 1:1; tab closed mid-transfer → reopen → resumes | ~2 weeks |
| **M4 — USB sneakernet** | "Export to USB" writes marker + chunks to a stick (exFAT); any engine auto-detects the stick, verifies hashes, completes the file | Transfer a 30 GB file to an offline PC via USB stick, verified, zero manual copying | 1–2 weeks |
| **M5 — Polish (optional)** | Tray icon; LAN encryption (TLS with pinned self-signed cert or DTLS); Android APK wrapper; auto-update check; i18n | Filled in as the group demands | ongoing |

M0 and M1 need **no server at all** — the engine serves the web page itself.

## 4. Explicitly out of scope

- Real BitTorrent interop (magnet links, DHT, public trackers) — see D1.
- App stores of any kind; no Apple/Google developer accounts.
- Cloud storage / async drop-box on the VPS (rejected in discussion: "VPS never keeps user files").
- Continuous folder synchronization (that's Syncthing's problem, not ours).
- macOS engine build (nothing blocks it — Go cross-compiles — it's just untested demand; community contribution welcome later).
- coturn/TURN until measurements show hole-punch success is too low.

## 5. Risks and accepted risks

| Risk | Status | Handling |
|---|---|---|
| iOS suspends the tab when the app is backgrounded mid-transfer | accepted until M3 | M0: per-chunk retry keeps transfers alive through hiccups while the tab is open; M3: OPFS resume across tab close |
| LAN traffic is plain HTTP in M0–M4 (sniffable on hostile WiFi) | accepted until M5 | Fine on home/trusted WiFi; documented in `SECURITY.md`; M5 adds TLS-with-pinned-fingerprint (the QR can carry the pin) or DTLS |
| Windows SmartScreen warns on unsigned binaries | accepted | One-time "More info → Run anyway" per group member; a cheap OSS-developer signing cert is a later option, not now |
| WebRTC hole-punch failure | mitigated | WSS relay fallback on 443 always works; STUN is free; success rate measured in M2 |
| Group member leaves / phone stolen | process | Rotate the group key (one config change + redistribute — `SECURITY.md` has the procedure) |
| Solo-dev bandwidth across 5 platforms | staged | Milestones are each shippable alone; phones need zero native code |

## 6. Open questions (decide before/at M0)

- [ ] Final name and repository (public on GitHub gives free Releases + CI; private is fine if binaries are served from the VPS).
- [ ] License (recommend Apache-2.0; MIT acceptable).
- [ ] Subdomain on the existing domain, e.g. `xfer.example.com` (needed only at M2).
- [ ] Engine config format: TOML vs JSON (either; recommend TOML for hand-editing).

## 7. Next action

Start **M0** — the ordered, concrete checklist is in `M0-TASKS.md`.
