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
| 🔒 `GET /api/peers` | LAN discovery view: `{"self":{…},"peers":[{id,name,ip,port}…]}` |
| `GET /api/sync/state` | This engine's shared-folder announcement: `{"files":{rel:{size,mtime,xxh}},"deleted":{rel:mtime}}` |
| `POST /api/sync/start` | Body `{"rels":["…"]}` → session serving those files for a peer |
| 🔒 `GET /api/activity` | Concise feed: `{"events":[{time,kind,msg}],"uploads":[live session summaries],"incoming":[phone pushes in progress]}` |
| 🔒 `POST /api/upload/new` | Mint a phone-upload slot: `{"id","key"}` (§4 upload direction) |
| 🔒 `POST /api/upload/close` | Kill a phone-upload link: body `{"key":"<upload-key>"}` |

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
| `POST /api/upload/start` | Body: `{"files":[{path,size,chunkCount,hashes,mtime}…]}` (browser-built manifest); engine validates §8 paths, chunk math, hash shape, caps → `{"id","files","bytes"}`. A started-but-**not-finished** slot may be started again (phone lost its connection mid-transfer): state resets, staging is wiped, everything re-uploads. A **finished** slot refuses further starts → `410 Gone` `{"error":"upload already finished"}`: one link is one batch; the dashboard's card renews its QR after each finish and the next batch needs a scan of that fresh link |
| `PUT /api/upload/{uploadId}/chunk/{f}/{c}` | Body: chunk bytes + `X-Chunk-Hash` header; engine verifies against **both** the header and the declared manifest hash before ack; body capped at exactly one chunk |
| `POST /api/upload/{uploadId}/finish` | Engine checks completeness + sizes, renames each staged file into place (atomic, same filesystem), stamps the phone's mtime, refreshes the sync state; idempotent on retry |

Normative details:
- Upload sessions are RAM-only with a 24 h TTL — same lifetime as
  receive sessions; a duplicate finish after a lost response still
  returns `200` (the finished upload authenticates until expiry).
- A restarted slot is safe: staging lives only under the excluded
  `hashes/uploads/` tree, a restart wipes it, and finish re-verifies
  every staged byte (size + SHA-256 against the current manifest) —
  stale bytes from the aborted attempt can never land. This restart
  window covers only a started-not-finished slot; a finished slot
  refuses starts (`410`), so a lingering finished slot can never
  accept another batch — one link is one batch.
- Finish stamps each landed file's **modification** time from the
  phone's `mtime`; on Windows the **creation** time is stamped too.
  (Linux/macOS filesystems have no userspace API for birth time.)
- Finished slots are not reported as live (`/api/activity` "incoming"
  lists in-flight uploads only) — the dashboard's upload card treats
  its slot id disappearing from that list as "link spent" and mints a
  fresh QR automatically.
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

## 5. Chunk framing off-HTTP (M2 internet mode — provisional)

The same logical operations, framed as binary messages on a WebRTC
DataChannel (ordered/reliable initially) or on the WSS relay:

```
frame = [1-byte type][4-byte LE fileIndex][4-byte LE chunkIndex]
        [4-byte LE length][length bytes payload]

type 0x01 = MANIFEST (payload = gzip JSON)
type 0x02 = CHUNK (payload = raw bytes)
type 0x03 = PROGRESS (payload = compact bitmap)
type 0x04 = DONE / CANCEL
```

Room lifecycle via hub signaling (WSS, JSON messages: `hello`, `sdp`, `ice`,
`bye`): sender engine creates a room with the group key, gets code + room
secret, shows `https://xfer.example.com/#c=<code>&s=<secret>` as QR or text.
Receiver joins, SDP/ICE exchange, DataChannel opens.

**Relay fallback:** if ICE fails after ~8 s, both sides open
`wss://xfer.example.com/relay/<code>`. Frames relayed through the hub are
additionally wrapped in AES-256-GCM with `HKDF-SHA256(roomSecret, "relay-e2e")`
— the hub pipes ciphertext it cannot decrypt (`SECURITY.md` §5).

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
