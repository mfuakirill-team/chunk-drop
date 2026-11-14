# ChunkDrop

**Chunked, resumable, torrent-style file transfer for a private group of people.**
A small desktop program ("engine") sends files to any phone or PC through a browser
("just scan the QR"), over the local network today and over the internet later.
Optional USB-stick "sneakernet" mode for carrying chunks physically.

> Status: **planning complete, ready to build M0.** No code exists yet.
> Everything below is the agreed plan, distilled from the design discussions.

## Why

The group needs to move files between Windows/Linux PCs and Android/iOS phones:

- one-shot transfers (receive a file, save it, done — nothing runs 24/7 on phones),
- mixed content: sometimes one multi-GB file, sometimes hundreds of small ones,
- mostly in the same room (scan a QR from the PC screen), sometimes across cities,
- **zero additional budget** — everything runs on hardware/services already owned.

Nothing on the market fits: LocalSend (LAN-only, no chunked resume), Syncthing
(sync-oriented, weak on iOS), Resilio Sync (closed source, paid tiers), croc
(no GUI, no swarm). See `docs/PLAN.md` for the full landscape analysis.

## Shape in one picture

```
LAN (primary, M0)                          Internet (M2)
─────────────────                          ─────────────
┌────────────────┐    QR code              ┌────────────────┐
│ Engine (Go)    │────────────────┐        │ Engine (Go)    │
│ Windows/Linux  │                │        │ Windows/Linux  │
│ single binary  │                ▼        └───────┬────────┘
│                │      ┌──────────────┐          │ signaling only
│ serves web UI  │◄─────│ Phone browser│          ▼ (bytes go P2P)
│ on :8090       │ HTTP │ / installed  │   ┌───────────────┐
└────────────────┘      │ PWA          │   │ VPS hub (Go)  │
                        └──────────────┘   │ Docker, no    │
                                           │ file storage  │
                                           └───────────────┘
```

- Files travel as **4 MiB chunks, SHA-256-verified, individually retried** —
  a dropped WiFi moment doesn't restart a 4 GB transfer.
- **Pairing = the QR code itself**: it contains a per-transfer secret that never
  appears on the wire.
- The VPS **never stores user files**; it only pairs endpoints and relays
  encrypted bytes when a direct P2P connection fails.
- Later phases: multi-receiver swarms (receivers fetch missing chunks from each
  other) and USB sticks as a "transport peer".

## Documentation index

| File | Contents |
|---|---|
| `docs/PLAN.md` | Master plan: locked decisions, milestones, scope, risks |
| `docs/ARCHITECTURE.md` | Components, data flows, tech choices and why |
| `docs/PROTOCOL.md` | Wire protocol: manifest format, endpoints, chunking, pairing |
| `docs/SECURITY.md` | Threat model, auth model, sanitization rules, accepted risks |
| `docs/DEPLOY-VPS.md` | VPS deployment: Docker (hub) + existing nginx, configs included |
| `docs/M0-TASKS.md` | Concrete build checklist for the first milestone |

## Ground rules (from the requirements discussion)

1. **Zero monthly cost.** Own VPS (already paid, dedicated IP, own domain),
   free public STUN, binaries hosted on GitHub Releases or the VPS itself.
   Windows SmartScreen warning: accepted (one-time "Run anyway" per person).
2. **No app stores.** Phones use the browser / installed PWA. iOS needs no
   Apple developer account this way.
3. **Not BitTorrent.** Torrent-*like* semantics (chunks, hashes, resume, later
   swarms) but a closed, private protocol — no DHT, no trackers, no piracy
   association, nothing for ISPs to shape.
4. **The VPS is a pipe, not a disk.** It relays encrypted traffic and stores
   nothing.
5. **Auth = keys, not labels.** Group key + per-transfer session key. A
   User-Agent/version check is treated as metadata, never as a gate.
