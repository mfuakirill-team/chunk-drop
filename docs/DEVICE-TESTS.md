# Device acceptance script — M0/M1 (run on real hardware)

Everything below is built and unit-tested; this is the human pass on
real devices. ~30 minutes with two laptops + a phone. Tick as you go.

## Setup (5 min)

- [ ] Copy the right binary to each machine: `engine` (Linux),
      `engine-windows-amd64.exe` (Windows). `bin/` after `make engine*`.
- [ ] First run on each machine: `engine keygen` → paste the same key
      into every machine's `engine.toml` (`USAGE.md` §2.1).
- [ ] Windows only: allow the firewall prompt for private networks.
- [ ] Both engines running → each dashboard's **LAN clients** card
      lists both (beacons, UDP :8091).

## A. Send → receive (M0 core)

- [ ] **Desktop → desktop**: pick files on PC-A, scan/open link on
      PC-B → files land in a chosen folder, tree structure intact
      (`USAGE.md` §4).
- [ ] **Desktop → iPhone Safari**: scan QR → save a file; the
      downloaded file opens from Files (per-file `<a download>`).
- [ ] **Desktop → Android Chrome**: same.
- [ ] **Many-small batch (≥ 60 files) → phone**: pick a photo folder,
      start; the phone should get **one ZIP** (auto per-receiver
      packing, M1) — check the download is `bundle.zip` with the tree
      inside, while a desktop receiver of the *same session* still
      gets the tree (`USAGE.md` §4.2).
- [ ] **4 GB single file → desktop receiver**: completes; spot-check
      the last chunk (file size exact).

## B. Robustness hiccups

- [ ] **Airplane-mode the phone 5 s mid-transfer** → resumes, finishes
      (chunk retries + backoff).
- [ ] **Close the receiver tab mid-transfer, reopen the link** → clean
      restart, nothing corrupt on disk (resume-after-close is M3 by
      design).
- [ ] **Ctrl+C the sending engine mid-transfer** → receiver surfaces
      the error, no partial junk beside the target.
- [ ] **Wrong session key**: open a `/r/` link with an edited key →
      generic `401` body, no listing, no path hints.

## C. Receive from a phone (M1 upload)

- [ ] Dashboard **Open a phone upload link** → scan with the phone →
      pick a few photos → **progress on both screens** → done.
- [ ] Photos appear in the engine's **sync folder**
      (`~/Downloads/chunk-drop-files`), Activity feed logs them.
- [ ] **Propagation**: a second engine with the same group key picks
      the photos up within ~5 s (sync mesh).
- [ ] **Close link** right after → the phone's next chunk/finish gets
      `401` (or a clean error), and a *new* slot rejects the old key.
- [ ] **Phone mtime**: photos show their original date, not "now".

## D. PC↔PC sync folder

- [ ] Drop a file in PC-A's sync folder → appears on PC-B ≤ 10 s.
- [ ] **Delete** a file on PC-A → gone on PC-B within a sync round;
      bring PC-B offline first, delete, restart PC-B → deletion still
      propagates (tombstone replay, 24 h).
- [ ] **Older build check** (if any machine still runs a pre-`8bb4a28`
      binary): upgrade it — mixed meshes can resurrect deleted files.

## E. Engine-side extras (local-only surface)

- [ ] `Pick files…`/`Pick folder…` open a native dialog **on the
      engine machine**.
- [ ] From another device: any dashboard API (`/api/peers`,
      `/api/upload/new`) returns the generic **404** even with the
      group key (`USAGE.md` §5.0) — remote `/r/` and `/u/` pages still
      work.

## F. Restart behavior

- [ ] `Ctrl+C` engine → dashboard tab shows dead API; restart → new QR
      for new transfers, **sync folder intact**, no leftover
      `hashes/uploads/` staging.
- [ ] 24 h later (or if you skip waiting: edit the TTL in code) — old
      session keys rejected with `401`.

---

**When all boxes tick**: M0 + M1 device acceptance is done. Then the
only open items are distribution (GitHub Releases — needs repo
name/public-private/license decisions, `PLAN.md` §6) and M2 (hub,
needs explicit go-ahead).
