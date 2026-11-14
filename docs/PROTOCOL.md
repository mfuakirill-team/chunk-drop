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
- Recommended sender behavior: if receiver is mobile and the batch has > 50
  files, offer (or auto-apply) store-mode ZIP packing.

## 4. Engine HTTP API (LAN, normative)

| Method & path | Purpose |
|---|---|
| `GET /r/` | Receive web page (embedded web app) |
| `GET /api/manifest` | Manifest (gzip) |
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

| Method & path | Purpose |
|---|---|
| `POST /api/start` | Create a transfer session from engine-local paths; body `{"paths":["…"],"zip":bool}` |
| `POST /api/pick/files` | Open a native multi-file dialog **on the engine**, return chosen paths |
| `POST /api/pick/folder` | Same, for a recursive folder pick |
| `GET /api/peers` | LAN discovery view: `{"self":{…},"peers":[{id,name,ip,port}…]}` |
| `GET /api/sync/state` | This engine's shared-folder announcement (rel → size/mtime/xxh) |
| `POST /api/sync/start` | Body `{"rels":["…"]}` → session serving those files for a peer |
| `GET /api/activity` | Concise feed: `{"events":[{time,kind,msg}],"uploads":[live session summaries]}` |

Sync normative details:
- **In-progress pull temp files are never announced or planned.** A
  pulling engine writes its half-finished copy under the sync folder's
  excluded `hashes/tmp/` metadata dir (never beside the target), and
  rels ending in `.part` (or inside `hashes/`) are skipped by scan and
  pull planning — so peers can never pull each other's partial
  downloads back.
- **Local deletions propagate; they are never undone by stale
  announcements.** When a known file disappears from the sync folder,
  the engine records a tombstone (persisted in
  `hashes/cache.json` → `vanished`, kept 30 days) and refuses to
  re-pull that rel while any peer still announces it — otherwise the
  deleter would instantly re-download the file from a peer still inside
  its own delete grace, and the deletion would never propagate.
  A remote mtime **newer** than the vanish time beats the tombstone
  (the file was genuinely edited/re-created elsewhere after our
  deletion). Re-adding the file locally clears the tombstone. Deletion
  reaches every reachable peer within `DeleteGrace` (2 min) of
  consistent absence from announcements.

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

| Method & path | Purpose |
|---|---|
| `POST /api/upload/start` | Body: manifest (browser-built); engine validates paths/sizes/caps → `uploadId` |
| `PUT /api/upload/{uploadId}/chunk/{f}/{c}` | Body: chunk bytes + `X-Chunk-Hash` header; engine verifies hash before ack |
| `POST /api/upload/{uploadId}/finish` | Engine finalizes into the configured receive directory |

The engine verifies every uploaded chunk hash — never trust a client's
manifest claims.

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
