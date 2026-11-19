# Transfer Protocol (v1)

The protocol is deliberately boring: JSON + plain HTTP on the LAN, the same
chunk framing tunneled through WebRTC/WSS for internet mode. M0–M1 parts are
**normative** (build them exactly as written); M2+ parts are **provisional**
(finalize when the phase starts).

Terms: **engine** (desktop Go app), **hub** (VPS Go service), **sender**,
**receiver**, **session** (one transfer attempt), **manifest**, **chunk**.

## 1. Keys and identifiers

| Name | Size / form | Lifetime | Where it lives |
|---|---|---|---|
| Group key | 32 bytes, base64url (43 chars) | Long-term | Engine + hub config; distributed once out-of-band |
| Session key | 16 bytes, base64url (22 chars) | One transfer (≤ 24 h) | Engine RAM; carried to receivers in the QR/link fragment |
| Session id | UUIDv4 | One transfer | Engine RAM, manifest |
| Room code (M2) | 6 chars, alphabet `23456789ABCDEFGHJKMNPQRSTUVWXYZ` (no 0/O/1/I) | One transfer | Hub RAM |

- Every authenticated request carries `X-ChunkDrop-Key: <session-key>` and is
  compared in **constant time**. Wrong key → `401`, generic body.
- The group key protects engine↔engine and engine↔hub traffic; receivers on
  the LAN never need it (possession of the session key via QR is the proof).
- `User-Agent` / version strings may be recorded as metadata; they are never
  consulted for authorization.

## 2. Pairing: the QR link (LAN)

```
http://192.168.1.20:8090/r/#k=<22-char-session-key>
```

- The key is in the **URL fragment** (`#k=`). Fragments are never sent in HTTP
  requests — the receiver page reads it in JS and presents it as the
  `X-ChunkDrop-Key` header on subsequent API calls.
- The engine picks the LAN IP automatically; with multiple candidate NICs
  (VPN adapters!) the UI shows a picker.
- Plain HTTP on the LAN is an **accepted risk until M5** (`SECURITY.md` §4):
  on hostile WiFi both file bytes and the session-key header are sniffable.
  M5 pins a self-signed cert fingerprint inside the QR instead.

## 3. Manifest (normative)

```jsonc
{
  "version": 1,
  "sessionId": "b0e6…",
  "createdAt": "2026-11-14T18:03:11Z",
  "chunkSize": 4194304,          // default 4 MiB; per-manifest constant
  "packedAsZip": false,          // true when engine packed many small files
  "files": [
    {
      "index": 0,
      "path": "photos/2026/cat.jpg",   // relative, sanitized, UTF-8 NFC
      "size": 5242880,
      "chunkCount": 2,
      "hashes": ["sha256hex…", "sha256hex…"]   // one per chunk
    }
  ]
}
```

Served as `GET /api/manifest` with `Content-Encoding: gzip` (a 10 000-file
manifest is ~1 MB JSON → ~150 KB gzipped).

Rules:
- One chunk size per manifest; the last chunk of each file may be short.
- `packedAsZip: true` → manifest describes a single file `bundle.zip`
  (store-mode ZIP: no compression, structure preserved inside).
- **Auto-ZIP is per receiver (M1)**: the session's manifest is the tree;
  a receiver whose User-Agent is mobile **and** whose selection is a
  many-small batch (> 50 files, average file < 64 MiB) gets a lazily
  built packed variant instead — built once per session and cached, so
  desktop receivers keep the tree while phones keep one download.
  `GET /api/manifest?zip=1` forces the packed variant, `?zip=0` opts
  out; `GET /api/chunk/{f}/{c}` serves bytes from the same variant the
  receiver was shown (indices refer to it).

## 4. Engine HTTP API (LAN, normative)

| Method & path | Purpose |
|---|---|
| `GET /r/` | Receive web page (embedded web app) |
| `GET /u/` | Phone→PC push page (M1) — public like `/r/`; the upload key lives in the fragment |
| `GET /api/manifest` | Manifest (gzip); `?zip=1|0` forces/opts out of the packed variant (§3) |
| `GET /api/chunk/{fileIndex}/{chunkIndex}` | Raw chunk bytes; `X-Chunk-Hash: <sha256hex>` header as a convenience cross-check |
| `POST /api/progress` | Body: `{"have":{"0":[0,1,2,17,…], …}}` — chunk bitmap per file; sender UI polls session state to display it |
| `POST /api/cancel` | Close the session |
| `GET /api/session` | Session metadata + latest receiver progress (for the sender dashboard, same origin) |

Chunk size and count are validated against the manifest; unknown indices →
`404`, session key invalid → `401`, session expired → `410`.

### 4.1 Engine-side (dashboard/sync) routes — **group-key gated** (`X-ChunkDrop-Key: <group_key>`)

These act on the engine's own desktop/filesystem, so they require the
long-term **group key** (constant-time compare, fail closed when no
key is configured). Wrong key → `401` with the same generic body.

**Dashboard routes are also local-only by default** (marked 🔒): the
request's source address must be this machine — loopback or one of its
own interface addresses (a local browser reaches the engine via its LAN
IP). Anything else gets the generic `404` before authentication runs,
even with the correct group key: the dashboard can point a session at
arbitrary paths and open native dialogs, so exposing it to other
devices is an attack vector. `allow_remote_dashboard = true` in
engine.toml opts out (still group-key gated). The engine↔engine sync
routes are *not* local-only — they are the mesh and are gated by the
group key alone; receiver routes (§4.2) are reachable from other
devices by design.

| Method & path | Purpose |
|---|---|
| 🔒 `GET /` | Dashboard web app (local-only) |
| `GET /assets/*`, `/icons/*`, `/manifest.webmanifest` | Static bundle for both pages — **public**: the receive page references it by absolute path and must render on other devices (no secrets inside; also reachable via the unguarded `/r/` mount) |
| 🔒 `POST /api/start` | Create a transfer session from engine-local paths; body `{"paths":["…"],"zip":bool}` |
| 🔒 `POST /api/pick/files` | Open a native multi-file dialog **on the engine**, return chosen paths |
| 🔒 `POST /api/pick/folder` | Same, for a recursive folder pick |
| 🔒 `GET /api/peers` | Mesh client view: `{"self":{…},"peers":[{id,name,ip,port,hub,down,up}…]}` — `hub` marks internet reachability; `down`/`up` are live transfer rates in bytes/s (see below). LAN rows are omitted entirely while the mesh mode is `internet` |
| `GET /api/sync/state` | This engine's shared-folder announcement: `{"files":{rel:{size,mtime,xxh}},"deleted":{rel:mtime}}` |
| `POST /api/sync/start` | Body `{"rels":["…"]}` → session serving those files for a peer |
| 🔒 `GET /api/activity` | Concise feed: `{"events":[{time,kind,msg}],"uploads":[live session summaries],"incoming":[phone pushes in progress]}` |
| 🔒 `POST /api/upload/new` | Mint a phone-upload slot: `{"id","key"}` (§4 upload direction) |
| 🔒 `POST /api/upload/close` | Kill a phone-upload link: body `{"key":"<upload-key>"}` |
| 🔒 `POST /api/key` | Set the group key from the dashboard: body `{"key":"…"}` → `{"applied":bool,"saveInSeconds":20}` — see below for its auth (no `X-ChunkDrop-Key`) |

**`POST /api/key` is gated differently from every other dashboard route**:
the group key cannot be its own credential (a browser that lost its saved
copy — hand-edited toml, wiped PWA storage — could then never re-enter).
Instead the request carries a same-page stamp, and the engine applies the
key with **no current-key comparison**:

- `X-ChunkDrop-UI: dashboard` header must be present. Cross-site pages
  cannot add custom headers (CORS preflight), so this says "sent by the
  dashboard's own script".
- An `Origin` header, when the browser sent one, must name the engine's
  own `host:port` (scheme-insensitive). Empty `Origin` (curl, scripts) is
  allowed — the local-only wrapper already gates those.

A drive-by webpage on this device can still send blind requests (they
look local) but can supply neither stamp, so it gets `401`. The key
applies instantly (mesh rekeys); only the engine.toml write is deferred
(~20 s, flushed on shutdown). Typing the key that is already set is a
harmless no-op (`{"applied":false}`).

| 🔒 `GET /api/mode` | Current global mesh mode: `{"mode":"auto","modes":["auto","local","internet"]}` — see below |
| 🔒 `POST /api/mode` | Switch the global mesh mode: body `{"mode":"auto|local|internet"}` → `{"ok":true,"mode":…}` — see below |

**The global mesh mode (M2 step ④).** ONE switch that says how THIS
engine interacts with all other peers — there is nothing per-peer to
configure. The dashboard reads it (`GET /api/mode`) and switches it
(`POST /api/mode`); the engine rebuilds the mesh immediately (same
stop/start as a group-key rekey) and persists the choice in
`engine.toml` (`mesh_mode`, default `auto`) after a short debounce, so
it survives a restart:

- `auto` — everything: LAN peers direct, internet peers via the hub
  (tunnel first, relay fallback). A peer visible both ways uses the
  direct LAN route.
- `local` — the local network only. The hub connection is **dropped
  entirely** (no presence, no hub peers, no relay); the engine logs
  `mesh mode local — hub off, local network only`.
- `internet` — the global internet only: LAN-discovered peers are
  skipped (they do not appear in `/api/peers` and are not synced
  with); peers join via `hub_url`/`stun_url` (tunnel first, relay
  fallback).

Discovery stays on in every mode — announcing is passive (beacons
carry no traffic); the mode only gates the mesh itself.

**Live transfer rates.** `/api/peers` rows also carry `down` (bytes/s
this engine pulls FROM that client, metered at verified chunk writes
in the syncmesh puller) and `up` (bytes/s it serves TO that client,
metered in the chunk handler). Both are rolling 5 s averages; idle
peers report `0` — the dashboard shows "–", not 0. Phones (session-key
receivers) have no mesh identity and are not attributed.

Sync normative details:
- **In-progress pull temp files are never announced or planned.** A
  pulling engine writes its half-finished copy under the sync folder's
  excluded `hashes/tmp/` metadata dir (never beside the target), and
  rels ending in `.part` (or inside `hashes/`) are skipped by scan and
  pull planning — so peers can never pull each other's partial
  downloads back.
- **Deletions are explicit signals, never inferred from absence.**
  Deleting a file in the sync folder records a tombstone — file name +
  mtime of the deleted version — in `hashes/cache.json` (`deleted`)
  and announces it in `GET /api/sync/state` → `deleted:{rel:mtime}`.
  Absence from an announcement is **not** a delete signal (a peer
  restarting/rescanning looks identical, and inferring deletes from
  that is what caused files to vanish and deletions to never
  propagate).
- **Identity is name + mtime.** A peer that receives a tombstone
  deletes its copy when the local mtime is the same or older, then
  re-announces the record (gossip) — so an engine that was offline
  during the delete still receives the signal when it returns. A
  **newer** mtime under the same name is a new file: it is kept, syncs
  normally, and the stale record is dropped. Tombstones are kept and
  re-announced for `TombstoneTTL` (24 h) and survive engine restarts;
  a newer deleted version (same name, newer mtime) replaces the record.
- `cache.json` is rewritten only when its content changes — the 5 s
  scan loop keeps the file untouched otherwise. Only the *contents* of
  `hashes/tmp/` are cleared per round; the directory itself stays.

The PWA surface (manifest, icons) is served from the same static web
app (`/manifest.webmanifest`, `/icons/*`) — no service worker by
design (offline caching is out of scope until it stops fighting the
"engine is the source of truth" model).

Receiver behavior (normative):
- Fetch chunks **sequentially per file, 2–3 in flight** (keep phone RAM flat).
- Verify every chunk with SHA-256 (hash-wasm) before keeping it. Hash mismatch
  → refetch (max 3 attempts with backoff, then surface the error).
- A failed/slow fetch is retried independently — one bad moment on WiFi never
  restarts the transfer.
- Save: desktop Chromium → File System Access API writes the real tree;
  everything else → per-file `<a download>` (and recommend ZIP mode for
  many-small batches on phones).
- M0 resume scope: **within the open tab** (retries + refetch). Resume after
  tab close arrives with OPFS persistence in M3.

Upload direction (phone → PC, M1, normative):

The dashboard mints the slot and hands the key to the phone; the phone
then drives the whole upload with that key. Possession of the upload
key is the authorization — the same model as receive links, direction
reversed. Staged bytes live under the destination's excluded
`hashes/uploads/<uploadId>/` tree: invisible to the sync scan, never
announced to peers, and cleared at startup (an engine restart kills
every upload).

| Method & path | Purpose |
|---|---|
| 🔒 `POST /api/upload/new` | Dashboard mints a slot: `{"id","key"}` (RAM-only, 24 h TTL) |
| 🔒 `POST /api/upload/close` | Dashboard kills a link: body `{"key":"<upload-key>"}`; wipes its staging; the phone's next chunk/finish/start gets `401` immediately — closed is terminal, the dashboard never re-opens a closed link |
| `POST /api/upload/start` | Body: `{"files":[{path,size,chunkCount,hashes,mtime}…]}` (browser-built manifest); engine validates §8 paths, chunk math, hash shape, caps → `{"id","files","bytes"}`. A started slot may be started **again** — mid-transfer (phone lost its connection, re-picked files) or after a finished batch (phone's "send more"): state resets, staging is wiped, everything re-uploads. One link serves several batches until it expires or the dashboard closes it — the QR's key lives until one of those, and the card never mints a second slot for the same QR, so a closed key has no lingering twin |
| `PUT /api/upload/{uploadId}/chunk/{f}/{c}` | Body: chunk bytes + `X-Chunk-Hash` header; engine verifies against **both** the header and the declared manifest hash before ack; body capped at exactly one chunk |
| `POST /api/upload/{uploadId}/finish` | Engine checks completeness + sizes, renames each staged file into place (atomic, same filesystem), stamps the phone's mtime, refreshes the sync state; idempotent on retry |

Normative details:
- **Disk-space gate at start**: the declared batch size must fit on the
  destination directory's own filesystem (sync folder / receive_dir /
  ramdisk — never "the disk" in general) once the bytes already
  reserved by other transfers in flight are subtracted. A batch that
  does not fit gets `400` with a `not enough disk space` message
  carrying need / free / reserved / available, and the dashboard's
  activity feed logs the same numbers. Reservations are RAM-only and
  count not-yet-written bytes (they shrink as chunks land, release on
  finish / close / expire / fail; an engine restart clears them) —
  mesh pulls reserve the same way on the sync folder's filesystem and
  fail the pull (logged, activity line) when they do not fit. A failed
  `statfs` (destination unreachable) also refuses the transfer with a
  clear error rather than writing blind.
- Upload sessions are RAM-only with a 24 h TTL — same lifetime as
  receive sessions; a duplicate finish after a lost response still
  returns `200` (the finished upload authenticates until expiry).
- A restarted slot is safe: staging lives only under the excluded
  `hashes/uploads/` tree, a restart wipes it, and finish re-verifies
  every staged byte (size + SHA-256 against the current manifest) —
  stale bytes from the aborted attempt can never land. This covers
  both a mid-transfer restart and a fresh batch on a finished link.
- The activity listing includes finished (but unexpired) slots flagged
  `finished: true`: the phone still holds that QR and "send more"
  reuses it, so the dashboard's card must not re-mint while the slot
  exists. **Closing the link is the only early kill**: it deletes the
  slot instantly and the same key the phone holds stops working for
  start/chunk/finish (401) — there is no second slot a closed key
  could fall back to.
- Finish stamps each landed file's **modification** time from the
  phone's `mtime`; on Windows the **creation** time is stamped too.
  (Linux/macOS filesystems have no userspace API for birth time.)
- Finished-but-unexpired slots stay in `/api/activity` "incoming",
  flagged `finished: true` (batch done, key still live) — the
  dashboard's card keeps the same QR and never re-mints while the slot
  exists; only an expired/deleted slot's absence triggers a re-mint
  (and never after a manual close).
- The `{uploadId}` in the URL path must belong to the authenticated
  key; a mismatch gets the generic `404` (nothing about other slots).
- Uploads land in the **sync folder** by default so they propagate to
  the other engines through the existing mesh; `receive_dir` in
  engine.toml overrides (then they stay on this PC).
- The engine verifies every uploaded chunk hash — never trust a
  client's manifest claims. Phone mtimes are preserved
  (`os.Chtimes`) so mesh newest-wins stays meaningful.
- The `/u/` page is a public mount like `/r/` and never offers a
  dashboard-back affordance.

## 5. Internet mode (M2 — hub signaling + tunneled mesh)

The provisional M2 design (rooms with codes, custom binary framing on
DataChannels) was replaced during implementation by a simpler, strictly
better scheme: the hub is a **signaling + presence server only**, and
the transfer itself is the **existing HTTP mesh protocol tunneled
unchanged** through WebRTC DataChannels — no second wire format to
maintain, one code path for LAN and internet.

### Hub wire protocol (JSON frames over WSS, normative)

Endpoint: the engine config's `hub_url` (e.g. `wss://drop.404.mn/hub/ws`).
Frames (`internal/hub/protocol.go`, shared with the client by import —
old hub / newer engine must stay compatible: `hello`/`presence` frozen,
the rest additive):

- `hello` (client→hub, first frame): `tag` = hex(SHA-256(group_key)),
  `id` = device ID, `name`, `pub` (base64 Ed25519), `ts`, `sig` =
  signature over `"chunkdrop-hello|tag|id|pub|ts"`. The group key
  itself never crosses the wire; a wrong tag/signature is rejected
  before the connection is used.
- `presence` (hub→client, full list on every join/leave + after hello):
  `devices: [{id, name, pub}]`. A full list, not diffs — clients stay
  trivially consistent.
- `sdp` / `ice` (signaling relay, M2 step ②): `to` = target device ID,
  `side` = `"offer" | "answer"`, `body` = opaque pion SDP/ICE JSON. The
  hub overwrites `from` with the authenticated sender (no spoofing) and
  delivers only to members of the same group; an offline target gets an
  `error` frame. Non-trickle ICE: candidates ride inside the SDP, one
  exchange per direction — no ICE candidate flood.
- `error` (hub→client): sent before the hub closes a connection it
  rejects (connection-level, `sid` unset). Also stream-scoped for the
  relay fallback: the hub could not forward a `relay` frame (offline
  or stalled target) — `sid` names the stream to abort, the
  connection stays up.

The hub is stateless and RAM-only: no disk writes, no secrets, sessions
expire after 24 h. Device identity is a per-device Ed25519 key at
`~/.config/chunkdrop/device.key` (0600); the ID is
hex(SHA-256(public key)).

### Hub channels (M3 — TLS when the network intercepts TLS)

Some networks (corporate/hotel/campus proxies) terminate the outer
`:443` TLS handshake with their own certificate: the standard wss
channel dies with `x509: certificate signed by unknown authority` (or,
worse, "succeeds" against a proxy whose root CA is already in the
system trust store — a silent MITM). The M3 channels deal with that by
carrying a **real TLS handshake inside a plaintext websocket**:

```
client                     nginx :80              hub
  │ ws://…/hub/ws  (ordinary upgrade, no TLS outside)  │
  │── binary message "chunkdrop-itls1" (marker) ──────►│ ITLS on?
  │── binary messages = raw TLS records (≤16 KiB) ────►│ tls.Server
  │◄══ inner TLS handshake: REAL hub certificate ═════►│
  │   verified: system roots + hub_cert_pin            │
  │   then: the same JSON frames as NDJSON (line-based)│
```

- The marker (`itls1`) must be the **first** message. A hub without
  `--inner-tls-cert/--inner-tls-key` rejects it (`error` frame, then
  close) — legacy clients and behavior are untouched.
- One TLS record per binary message; after the handshake the hub
  protocol continues **NDJSON over the inner TLS connection** (one
  JSON frame per `\n`-terminated line, same schema as above). The
  outer websocket's ping/pong stays the keepalive on both sides.
- Verification is identical to any TLS connection — **chain against
  the system roots, plus `hub_cert_pin` (SPKI pin) when set**. The
  genuine certificate is presented live by the real hub *inside* the
  tunnel; nothing cert-shaped is ever baked into the binary (the pin
  is a public fingerprint, `make pin` prints it).
- Threat-model honesty: this is **obfuscation, not invisibility** — a
  middlebox that scans payloads can fingerprint the inner ClientHello.
  What it defeats is handshake interception, not traffic analysis.

Channel selection (`hub_channel`, M3):

| `hub_channel` | Behavior |
|---|---|
| `auto` *(default)* | wss first; on **dial/verification failure only**, fall back to ws-tls (ws:// URLs, e.g. local tests, go ws-tls → plain `ws` legacy). Whichever channel last connected is tried first (RAM, not persisted). |
| `tls` | Classic wss only. |
| `ws-tls` | Inner-TLS channel only. |

`/api/hub` reports the active channel in `channel`: `"tls"` (plain
wss), `"ws-tls"` (inner TLS), `"ws"` (legacy plaintext, local tests),
or empty until the first connection.

### Transfer path (implemented, step ②)

Once signaling completes, each direction gets its own PeerConnection
(offerer dials through its own PC — no glare) with one DataChannel per
HTTP connection. The DataChannel carries plain HTTP/1.1 requests and
responses (chunked bodies) between the two engines' mesh APIs — the
exact same endpoints LAN peers use. SCTP message size (65536) bounds
the write granularity (16 KiB chunks); backpressure applies above
1 MiB of unacked channel data. The hub sees none of this traffic.

### Relay fallback (implemented, step ③)

If ICE fails (symmetric NAT, blocked UDP — anywhere the hub's own
socket gets through), transfers fall back to the hub's own WSS relay,
again carrying the same HTTP protocol. New `relay` frames
(`internal/hub/protocol.go`): `to` = target device ID, `sid` = stream
ID, and either `data` (JSON base64 payload, ≤ 32 KiB per frame) / `eof`
(send direction closed) or `ack` (cumulative "receiver consumed N
bytes" flow-control report). The hub is a dumb stateless forwarder: it
validates the target, overwrites `from` with the authenticated sender,
and re-sends the frame inside the sender's group — no per-stream
state, no storage. A write to a dead/stalled target answers a
stream-scoped `error` frame (`sid` set; `sid` 0 = connection-level
error, e.g. a hello reject).

Engine side (`internal/relay`), the tunnel's shape mirrored one more
time: `http.Transport.DialContext` → relay stream on the dialer;
`net.Listener` → relay streams on the accepter, both served by the
same mesh handler. Stream IDs come from two disjoint namespaces (the
lexicographically larger device ID dials on odd `sid`s), so
simultaneous opposite dials cannot collide. Flow control is
end-to-end: the sender keeps at most 512 KiB un-acked per stream, the
receiver acks what its consumer has actually read — that bounds RAM at
every hop (dialer ≤ window, hub ≈ socket buffers, accepter ≤ a window
of un-acked bytes) and caps a stream near window/RTT: **512 KiB at a
50 ms hub RTT ≈ 80 Mbit/s**, the documented relay ceiling. Failsafes:
a stream whose acks stop for 15 s is aborted; idle streams are reaped
after 3 min.

Routing: the engine always dials the tunnel first (P2P — the hub sees
nothing of a working tunnel); a failed tunnel dial cools that peer
down for 60 s and the relay takes over, with an instant tunnel retry
once its peer connection reports connected again.
`CHUNKDROP_RELAY_ONLY=1` skips the tunnel entirely (diagnosis:
"hub reachable, tunnel broken" without touching the NAT question).
The relay is an unauthenticated transport exactly like the tunnel —
the mesh protocol's group-key checks carry the trust boundary; the
bytes stay under the engine's TLS session to the hub (`SECURITY.md`
§5).

## 6. Resume state (M3 — provisional)

- Receiver persists received chunks in OPFS: `session-<id>/f<f>-c<c>.bin`,
  bitmap in IndexedDB keyed by session id.
- Reopening the same `#k=` link (or M2 short link) → manifest → diff against
  bitmap → fetch only missing chunks.
- Engine keeps the session alive for 24 h, then expires it (`410`).

## 7. USB marker (M4 — provisional)

```
/usb-root
├── .chunkdrop.json      → {"version":1,"sessionId":"…","manifestSha256":"…",
│                            "chunkSize":…,"exportedAt":"…"}
└── .chunks/
    ├── f0-c000123.bin   → exactly the chunk bytes
    └── f0-c000124.bin
```

Sticks must be exFAT (FAT32's 4 GB limit). Engines poll mount points
(`/media/*`, `/run/media/$USER/*`, Windows drive letters) every ~2 s. On
detection: verify manifest hash, import available chunks (verify each), then
finish from the network if the original sender is reachable.

## 8. Filename safety rules (normative, applies to every path in every manifest)

1. No absolute paths, no `..` segments, no drive letters — reject and log.
2. Normalize to UTF-8 **NFC**; reject control characters and NUL.
3. Strip trailing dots/spaces per component (Windows quirk).
4. Reject Windows reserved device names: `CON, PRN, AUX, NUL, COM1–9, LPT1–9`
   (case-insensitive, with or without extension).
5. Cap component length at 255 bytes and total depth at 16 levels.
6. Case-insensitivity collision check (`A.jpg` vs `a.jpg` in one manifest) →
   rename with numeric suffix, never overwrite silently.
7. Receiver-side belt-and-suspenders: repeat all checks before writing to disk.
