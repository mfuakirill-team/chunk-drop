# Device acceptance script — M0/M1 (run on real hardware)

Everything below is built and unit-tested; this is the human pass on
real devices. ~30 minutes with two laptops + a phone. Tick as you go.

## Setup (5 min)

- [x] Copy the right binary to each machine: `engine` (Linux),
      `engine-windows-amd64.exe` (Windows). `bin/` after `make engine*`.
- [x] First run on each machine: `engine keygen` → paste the same key
      into every machine's `engine.toml` (`USAGE.md` §2.1).
- [ ] Windows only: allow the firewall prompt for private networks.
- [x] Both engines running → each dashboard's **LAN clients** card
      lists both (beacons, UDP :8091).

## A. Send → receive (M0 core)

- [x] **Desktop → desktop**: pick files on PC-A, scan/open link on
      PC-B → files land in a chosen folder, tree structure intact
      (`USAGE.md` §4).
- [ ] **Desktop → iPhone Safari**: scan QR → save a file; the
      downloaded file opens from Files (per-file `<a download>`).
- [x] **Desktop → Android Chrome**: same.
- [x] **Many-small batch (≥ 60 files) → phone**: pick a photo folder,
      start; the phone should get **one ZIP** (auto per-receiver
      packing, M1) — check the download is `bundle.zip` with the tree
      inside, while a desktop receiver of the *same session* still
      gets the tree (`USAGE.md` §4.2).
- [x] **4 GB single file → desktop receiver**: completes; spot-check
      the last chunk (file size exact).

## B. Robustness hiccups

- [x] **Airplane-mode the phone 5 s mid-transfer** → resumes, finishes
      (20 s network-patience window — if it ever gives up, the phone
      shows the real error and re-picking restarts the link cleanly).
- [ ] **Close the receiver tab mid-transfer, reopen the link** → clean
      restart, nothing corrupt on disk (resume-after-close is M3 by
      design).
- [ ] **Ctrl+C the sending engine mid-transfer** → receiver surfaces
      the error, no partial junk beside the target.
- [x] **Wrong session key**: open a `/r/` link with an edited key →
      generic `401` body, no listing, no path hints.

## C. Receive from a phone (M1 upload)

- [x] Dashboard **Open a phone upload link** → scan with the phone →
      pick a few photos → **progress on both screens** → done.
- [x] Photos appear in the engine's **sync folder**
      (`~/Downloads/chunk-drop-files`), Activity feed logs them.
- [x] **Propagation**: a second engine with the same group key picks
      the photos up within ~5 s (sync mesh).
- [x] **Close link** right after → the phone's next chunk/finish/start
      gets `401` (or a clean error), a *new* slot rejects the old key,
      and the dashboard card stays closed (mint button — it must NOT
      re-mint a QR on its own after you pressed Close).
- [x] **One link, several batches**: after a transfer finishes, "Send
      more" on the *same* link starts a fresh batch (the dashboard's
      QR is unchanged — no re-scan needed), and the Activity line
      shows `✓ phone sent N file(s) … batch done, link still open`.
- [x] **Names while pushing**: the phone shows which file is being
      sent (and the chosen names while hashing), and the dashboard's
      ▼ line lists the incoming name(s) — verify a wrong pick is
      noticeable on both screens.
- [x] **Phone mtime**: photos' **modified** date is their original date
      (not "now"); on Windows the **created** date is original too
      (on Linux/macOS the "created"/birth column is always "now" —
      filesystem limitation, `USAGE.md` §5.11).

## D. PC↔PC sync folder

- [x] Drop a file in PC-A's sync folder → appears on PC-B ≤ 10 s.
- [x] **Delete** a file on PC-A → gone on PC-B within a sync round;
      bring PC-B offline first, delete, restart PC-B → deletion still
      propagates (tombstone replay, 24 h).
- [x] **Older build check** (if any machine still runs a pre-`8bb4a28`
      binary): upgrade it — mixed meshes can resurrect deleted files.

## E. Engine-side extras (local-only surface)

- [x] `Pick files…`/`Pick folder…` open a native dialog **on the
      engine machine**.
- [x] From another device: any dashboard API (`/api/peers`,
      `/api/upload/new`) returns the generic **404** even with the
      group key (`USAGE.md` §5.0) — remote `/r/` and `/u/` pages still
      work.
- [x] **Disk-space gate**: start a batch declaring more bytes than the
      destination drive has free (curl `/api/upload/start` with a big
      declared size — verified live: 40 GiB batch against ~45 GiB free
      with a 30 GiB batch in flight → 400 `not enough disk space:
      need… free… reserved… available…`; dashboard gets the error
      line; after closing the 30 GiB link the 40 GiB batch starts).
      (`SECURITY.md` §6, `USAGE.md` §5.11)

## F. Restart behavior

- [x] `Ctrl+C` engine → dashboard tab shows dead API; restart → new QR
      for new transfers, **sync folder intact**, no leftover
      `hashes/uploads/` staging.
- [ ] 24 h later (or if you skip waiting: edit the TTL in code) — old
      session keys rejected with `401`.

---

**When all boxes tick**: M0 + M1 device acceptance is done. After
that: cut the `v0.1.0` release (owner, in the GitHub UI — the Release
workflow builds and attaches the binaries automatically) and M2 (hub,
needs explicit go-ahead).
