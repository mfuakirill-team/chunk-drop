# Security Model

## 1. Assets

- File **contents** being transferred (private group data).
- **Integrity**: files must arrive unmodified and complete.
- Group **membership**: only group members may use the engine/hub at all.
- VPS operator (us): must never be able to read group files even in relay mode.

## 2. Threat model

| Adversary | Capability | Mitigation | Status |
|---|---|---|---|
| Stranger on the LAN | Sees QR-less traffic, probes engine port | Session key required on every endpoint (constant-time compare); no directory listing; unknown session → `401`/`404` generic | M0 |
| Stranger on the LAN (or a compromised device) | Wants to *control* the engine PC: start sessions from arbitrary paths, open dialogs, read local file names/peers | **Dashboard/control surface is local-only**: only requests whose source address is this machine (loopback or its own interface addresses) reach `/`, `/api/start`, `/api/pick/*`, `/api/peers`, `/api/activity` — everyone else gets the generic `404`, even with the group key. Receiving links (`/r/`, session endpoints) stay remote by design; the engine↔engine mesh (`/api/sync/*`) stays remote behind the group key. `allow_remote_dashboard` is an explicit opt-out (still key-gated) | M0 |
| Stranger on the LAN with a sniffer | Reads WiFi frames (plain HTTP) | **Accepted risk until M5** (home WiFi: fine). M5: TLS with self-signed cert, fingerprint pinned inside the QR | accepted → M5 |
| Stranger on the internet | Hits the VPS hub | Rooms require the group key at creation; codes are 30 bits + a room secret; rate-limited creation; hub stores nothing | M2 |
| Malicious "receiver" with a valid QR link | Gets exactly the files that session offers — nothing more | Sessions are scoped per transfer; engine exposes no filesystem beyond the manifest | M0 |
| Malicious sender (M1 uploads) | Sends crafted manifests/paths | Path rules in `PROTOCOL.md` §8 enforced receiver-side; per-chunk hash verification before disk write (against header **and** declared hash); total-size caps; staging inside the excluded `hashes/uploads/` tree, so half-uploads are never announced or synced; finished uploads stamp phone mtimes, so a crafted old/new mtime can only reorder mesh newest-wins, never bypass tombstones | M1 |
| Anyone with the `/u/` link (M1) | Pushes files onto the engine PC while the link is open | Link = 16-byte key in the URL fragment (same possession model as receive links); dashboard **Close link** kills it instantly and wipes staging; slots expire after 24 h; RAM-only — engine restart kills everything; uploads land only in the destination dir, never elsewhere | M1 |
| VPS compromise (hub container) | Runs our hub code, sees relayed ciphertext, sees IPs/timing | Relay frames are AES-GCM under `HKDF(roomSecret)` — hub pipes bytes it cannot decrypt; **no files at rest ever**; metadata exposure (IPs, session timing, sizes) accepted | M2 |
| Departed group member | Still holds the group key | Rotate: generate new key, update engine+hub configs, redistribute | procedure below |
| Path traversal / zip-slip | Crafted `../../` or reserved names | `PROTOCOL.md` §8 rules 1–7, with tests | M0 |

## 3. Authentication (summary of `PROTOCOL.md` §1)

- **Group key** (32 B) — gates engine↔engine and engine↔hub usage.
- **Session key** (16 B) — gates one transfer; travels only via the QR/link
  fragment, then as a header, compared in constant time.
- **Upload key** (16 B) — same shape, opposite direction (M1): gates one
  phone→PC push; the dashboard mints it, the `/u/` page carries it in the
  fragment, every upload endpoint compares it constant-time.
- **A User-Agent/version check is explicitly not an auth check** — decided in
  the design discussion: anyone can type `curl -H "User-Agent: MyApp/1.0"`.
  It may be logged as metadata; nothing more.

## 4. Accepted risks (deliberate, documented)

1. **LAN traffic is plaintext in M0–M4** (plain HTTP). Anyone sniffing the
   same WiFi can read the file bytes. Home/trusted networks: acceptable.
   Fixed in M5 (pinned self-signed TLS or DTLS-on-LAN).
2. **iOS tab suspension** can pause a receive mid-file. Mitigated by chunked
   retry while open, OPFS resume in M3. Not a confidentiality risk.
3. **SmartScreen warning** on unsigned Windows binaries — one-time click,
   accepted to keep cost at zero.
4. **Metadata visibility at the hub** (participant IPs, transfer sizes/timing)
   in internet mode. Content is not exposed; metadata exposure accepted.

## 5. Hub cannot read relayed bytes (M2 design invariant)

The DataChannel path is DTLS-encrypted end-to-end by WebRTC (hub never sees
it at all). The fallback relay path terminates TLS *at the hub*, so frames
would be plaintext to the hub — therefore the spec requires the client to
wrap every relayed frame in AES-256-GCM under `HKDF-SHA256(roomSecret,
"relay-e2e")` **before** it reaches the WSS socket. The hub is a dumb,
stateless pipe over ciphertext. Combined with the no-volumes container
(`DEPLOY-VPS.md`), the invariant holds: **the VPS never stores and never
reads user files.**

## 6. Hardening checklist

Engine:
- [x] Constant-time key comparison on every authenticated endpoint
- [x] All `PROTOCOL.md` §8 path rules, unit-tested (traversal, reserved
      names, unicode, collisions, trailing dots)
- [x] Per-chunk hash verification on upload (M1) before anything hits disk
- [x] Total-size cap in config (`max_upload_bytes`, default 100 GiB)
- [ ] Disk-space check before accepting an upload (not implemented yet —
      a full disk currently fails the chunk write mid-transfer)
- [x] Session auto-expiry (24 h) and manual cancel; sessions RAM-only
- [x] Bind only to the chosen LAN interface, not `0.0.0.0`, when possible

Hub container:
- [ ] `read_only: true`, `cap_drop: [ALL]`, `no-new-privileges`, no volumes
- [ ] Room creation rate-limit (e.g. 10/min per group key) + per-IP throttle
- [ ] No request body persistence anywhere; logs contain no session keys
- [ ] `/healthz` for the Docker healthcheck; nothing else exposed besides
      signaling/relay/web routes behind group-key or room-secret auth

## 7. Key rotation procedure (when someone leaves the group)

1. Run `engine keygen` → new group key.
2. Update the hub container env var, restart (`docker compose up -d`).
3. Send the new key to remaining members out-of-band (messenger of choice).
4. Old sessions die on hub restart; LAN sessions die when engines restart.
5. Done — no revocation list needed because keys are the only credential.
