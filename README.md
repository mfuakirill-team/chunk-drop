# ChunkDrop

**Chunked, resumable, torrent-style file transfer for a private group of people.**
A small desktop program ("engine") sends files to any phone or PC through a browser
("just scan the QR"), over the local network today and over the internet later.
Optional USB-stick "sneakernet" mode for carrying chunks physically.

> Status: **M0 built** — LAN one-shot send works end-to-end (engine + web app).
> Internet mode (VPS hub) is M2; see `docs/PLAN.md` §3 for the roadmap.

## Quick start (5 minutes)

**What you need:** the sending machine runs the engine (Windows or Linux);
every receiving device only needs a browser on the same network.

### 1. Get a binary

Build it from this checkout (Go 1.27+ and Node 20.19+ once, for the web app):

```bash
make engine-win       # → bin/engine-windows-amd64.exe
make engine-linux     # → bin/engine-linux-amd64 + -arm64
```

### 2. Set the group key (once per machine)

```bash
./bin/engine-linux-amd64 keygen          # prints a fresh key
nano ~/.config/chunkdrop/engine.toml     # paste it → group_key = "…"
```

On Windows the config is `%APPDATA%\chunkdrop\engine.toml` (use Notepad).
The engine tells you this on first run — it writes the template itself.

### 3. Run it and send

```bash
./bin/engine-linux-amd64
```

It prints the dashboard URL (`http://<lan-ip>:8090/`):

1. Open that URL in a browser on the sending machine.
2. Paste the group key once (the browser remembers it).
3. **Pick files…** → selection shows in its own frame → **Start transfer**
   → a QR code appears (**Cancel** ends it any time).
4. On the phone, scan the QR (or open the link / "Copy link").
5. The phone shows the file list → **Save all…** → done.
   (Phones can also install the page as an app: "Add to Home Screen"
   — see `docs/USAGE.md` §4.9.)

4 GB or 400 small files — same flow; a dropped Wi-Fi moment resumes,
nothing is ever uploaded to a server. All options, browser differences,
firewall/unboxing troubleshooting: **`docs/USAGE.md`**.

Prefer running the engine in Docker (home server / NAS)? `deploy/`
has a lightweight image + compose file — `docs/USAGE.md` §3.1.
(The VPS gets the *hub* — relay + PWA host — in M2: `docs/DEPLOY-VPS.md`.)

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
| `docs/USAGE.md` | **Full usage guide** — troubleshooting, options, details behind every quick-start step |

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
