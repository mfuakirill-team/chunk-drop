# M1 Build Checklist — "Full LAN"

Goal per `PLAN.md` §3 M1: **folder trees; phone→PC push (upload
direction); mDNS PC↔PC; store-mode ZIP packing for many-small→mobile.**

Done-line: *800-photo folder lands as a tree on Chrome/Edge desktop and
as one ZIP on iPhone; phone pushes photos to PC; two PCs see each other
without QR.*

Already satisfied by M0/M0.1/M0.2 work:

- [x] **Folder trees** — folder picker + tree manifest
      (`internal/transfer`), File System Access tree save on desktop
      Chromium (`web/src/saver.ts`).
- [x] **PC↔PC discovery without QR** — shipped as UDP broadcast beacons
      :8091 (HMAC of the group key, replay window) + peers panel +
      syncmesh (`internal/discovery`, `internal/syncmesh`). This is the
      M1 "mDNS" item fulfilled by our own beacons: same home-LAN scope,
      no extra dependency, already authenticated. Real mDNS/zeroconf
      stays out until a device proves broadcast is filtered.
- [x] **Store-mode ZIP packer exists** (`internal/transfer/zip.go`) —
      but opt-in only (dashboard checkbox at `/api/start`).

## Remaining M1 work

### 1. ZIP per-receiver (auto for mobile)
Mobile receiver + many small files → the *same session* serves a tree
to desktops and one store-ZIP to phones.

- [x] `transfer.BuildPackedVariant(m, zipPath)` — build the packed
      manifest variant from an unpacked one (pairs = Files' Path/SourcePath).
- [x] Auto rule (PROTOCOL §3 refined): receiver UA is mobile **and**
      session not packed **and** > 50 files **and** average file < 64 MiB
      (many-small, not "many huge"). `?zip=1|0` on `GET /api/manifest`
      forces/opts out.
- [x] Lazy build, once per session (`session.Session` caches the packed
      manifest; mutex-guarded). Chunk endpoint serves chunks from the
      same per-receiver variant.
- [x] Tests: mobile UA → packed, desktop UA → tree, built once (pointer
      identity), `?zip=0` opt-out, already-packed session untouched.

### 2. Upload direction (phone→PC push) — the main build
Protocol per `PROTOCOL.md` §4 upload table. Decisions: uploads land in
the **sync folder** (`~/Downloads/chunk-drop-files`) unless
`receive_dir` is set in engine.toml — so they propagate to other group
PCs through the existing sync mesh.

- [x] `internal/upload`: RAM-only upload sessions (16-byte keys, 24 h
      TTL, reap), per-file/chunk state, tmp files under
      `<dest>/hashes/uploads/<id>/` (excluded from sync scan, *not*
      wiped by the sync round's `hashes/tmp` cleanup).
- [x] Routes (`internal/api/upload.go`):
      `POST /api/upload/new` (🔒 local-only + group key; dashboard makes
      the QR/link), `POST /api/upload/start`,
      `PUT /api/upload/{uploadId}/chunk/{f}/{c}`,
      `POST /api/upload/{uploadId}/finish` — upload-key gated,
      constant-time compare, generic 401/404, chunk bodies capped at
      4 MiB + slack, every chunk hash-verified server-side.
- [x] Engine verifies + applies all PROTOCOL §8 rules on upload paths
      (`SanitizePath`, case-collision rename exported as
      `transfer.ResolveCaseCollisions`), caps: `MaxManifestFiles`,
      `MaxUploadBytes`.
- [x] Finish: size check, atomic rename → destination, `os.Chtimes`
      preserves phone mtime, `syncmesh.Local.RecordUpload` refreshes
      cache so peers pick the file up, activity feed events.
- [x] `/u/` mount (public like `/r/`): `web/src/upload.tsx` — file/folder
      pick, client-built manifest (hash-wasm SHA-256, 4 MiB chunks),
      2 chunks in flight, 3 retries with backoff, progress, no
      dashboard-back affordance.
- [x] Dashboard "Receive from phone" card: QR of `/u/#k=<upload-key>`,
      copy link, close-link button.
- [x] Tests: full flow (start → chunk → finish), wrong key 401, hash
      mismatch rejection, size-cap reject, sanitize/collision on paths,
      tmp files invisible to sync scan, finish populates sync state.

### 3. M0 leftovers (not blocking M1)
- [ ] Device test matrix (M0-TASKS step 9) — needs real hardware.
- [ ] Distribution (step 10) — blocked on repo name / public-private /
      license decisions (PLAN §6).

### Docs
- [x] PROTOCOL §3 (auto-ZIP rule), §4 (upload routes + `/api/upload/new`).
- [x] USAGE: receive-from-phone how-to + troubleshooting.
- [x] SECURITY: upload-surface threat-model row.
- [x] ARCHITECTURE: `internal/upload`, `/u/` mount, beacons-as-mDNS note.
