# ChunkDrop — Usage guide

*The quick start in the [README](../README.md) gets you sending in 5 minutes.
This guide covers everything else: install details, options, what the receiver
needs, troubleshooting. Same doc order, deeper answers.*

The engine is a **single self-contained binary** — no installer, no runtime
dependencies, no assets next to it. Copy it anywhere and run it. Config and
keys live in your home directory (paths below).

---

## 1. Install (per sending machine)

You need the binary for the sending machine's OS:

- **Windows 10/11 x64** → `engine-windows-amd64.exe`
- **Linux x64** → `engine-linux-amd64`
- **Linux ARM64** (Raspberry Pi, NAS) → `engine-linux-arm64`

### Build from this checkout (any machine)

Requirements (only for building, never for running):

- Go 1.27+
- Node.js 20.19+ (`npm`)

```bash
make engine-win       # → bin/engine-windows-amd64.exe
make engine-linux     # → bin/engine-linux-amd64 + bin/engine-linux-arm64
make engine-mac       # → bin/engine-darwin-arm64 (untested, community)
```

Prefer a plain build for *this* machine? `make engine` → `bin/engine`.
The web app is compiled in (`go:embed`); **the binary has no external files**.

### Or copy an existing binary

Windows ↔ Linux builds cross-compile from anywhere: build once on your dev
box, ship `bin/` contents to the others (USB stick, scp, whatever).

### Windows unboxing

1. Double-click `engine-windows-amd64.exe` (or run from PowerShell).
2. **SmartScreen** may show "Windows protected your PC" → **More info** →
   **Run anyway** (unverified publisher, no code-signing cert — accepted
   risk, `docs/PLAN.md` §5). This appears once per machine.
3. **Firewall prompt** (first run): allow on **private networks** — that's
   your LAN. Do **not** tick "public networks".

A console window opens — that's the engine. Keep it open while sending.

---

## 2. Configure (once per machine)

The engine stores its config in your home directory:

| OS | Config file |
|---|---|
| Linux/macOS | `~/.config/chunkdrop/engine.toml` |
| Windows | `%APPDATA%\chunkdrop\engine.toml` (Explorer paste works) |

First run creates the file as a template and tells you so. You only ever
need to set one line — the group key — then optionally the port.

### 2.1 Group key

The **group key** is the shared secret of your group. It authorizes the
*sender side* — creating a transfer session from picked paths. Receivers
(phone) never need it; they get per-transfer keys via the QR code.

```bash
# Linux / macOS
./engine keygen                       # prints e.g. "b3f9…-style 43-char key"
nano ~/.config/chunkdrop/engine.toml  # set: group_key = "<paste output>"
```

```powershell
# Windows
.\engine-windows-amd64.exe keygen     # prints a fresh key
notepad %APPDATA%\chunkdrop\engine.toml
```

Paste the keygen output into the `group_key = "…"` line and save.

**Where does the key show up?** In the web dashboard (sender side) you'll
paste it once per browser; the browser remembers it in `localStorage`.
Phone receivers never type it.

**Rotation** (`docs/SECURITY.md` §7): when someone leaves the group, run
`engine keygen` again and hand the new key to the machines you trust.

### 2.2 Optional settings

Edit `engine.toml`; restart the engine after changes.

| Key | Default | Meaning |
|---|---|---|
| `port` | `8090` | TCP port the server binds. |
| `bind_ip` | *(auto)* | LAN address to advertise in QR links. Only set it if auto-detect picks the wrong adapter (common with VPNs — see §5.4). |
| `allow_remote_dashboard` | `false` | Dashboard/control access from **other devices**. Off: the dashboard works only on the engine machine itself (loopback or its own LAN address); other devices get a 404 even with the group key. Turn on only for headless engines (e.g. a NAS whose dashboard you open from a laptop). Receiving links always work from other devices regardless. |
| `receive_dir` | *(M1)* | Where received uploads land (upload direction ships in M1). |
| `max_upload_bytes` | `106253862144` | Per-transfer upload cap (M1). Ignore for now. |

### 2.3 Delete everything

Nothing is written outside the config file (sessions are RAM-only, files
stream straight from disk). To uninstall: delete the binary and the config
directory.

---

## 3. Run the engine

```bash
./engine                     # port from config (default 8090)
./engine --port 8091         # one-off port override
./engine --bind 192.168.1.20 # pin the advertised LAN IP (see §5.4)
./engine --debug             # verbose logs: every request, chunk, step
./engine --log-level warn    # quiet: info hidden (debug|info|warn|error)
./engine help                # all flags
./engine version
./engine keygen              # fresh group key (see §2.1)
```

**Log levels** (`--log-level`, or `--debug` as shorthand for `debug`):

| Level    | Shows                                                                    |
|----------|--------------------------------------------------------------------------|
| `debug`  | everything — every HTTP request (method/path/status/duration), every stored chunk, every auth decision |
| `info`   | the default — transfer milestones: slot minted/started/restarted/finished, files landed, sync events |
| `warn`   | info + recovered problems: rejected chunks, wrong keys (401), finish refusals (400), restarts |
| `error`  | failures only — panics, staging errors, 5xx responses                    |

Lines look like `engine: [warn] upload: slot <id> restarted by the phone: state reset, staging wiped`.
**When something misbehaves, run `./engine --debug` and retry** — every step
and every error reason ends up on stderr.

Startup output:

```
engine: LAN address: 192.168.16.2 (eth0) — auto-picked

  ChunkDrop engine ready
  dashboard:  http://192.168.16.2:8090/
  receive:    http://192.168.16.2:8090/r/#k=<session-key>
```

Two URLs:

- **`dashboard:`** — open this on the **sending machine** in a desktop
  browser. This is where you pick files and see receiver progress.
- **`receive:`** — what the QR encodes. Don't type it; scan it (or copy
  the link). `<session-key>` is a per-transfer secret.

The engine serves the web app itself (embedded at build time) — **no
Node, no web server needed on the receiving or sending machine.**

What the engine does while running:

- Binds the LAN address it auto-detected (or `bind_ip`/`-bind`).
- Sessions live **only in RAM** — reboot/close loses them (nothing
  corrupts; just restart the transfer).
- Sessions expire after **24 h**.
- `Ctrl+C` (or window close) = clean shutdown, cancels sessions.

### 3.1 The engine runs as a plain binary

The engine is a single static binary — no Docker needed on the devices
that hold files. Run it directly (§3) on Windows/Linux/macOS; it
auto-detects the LAN, serves the dashboard on 8090/tcp and answers LAN
discovery beacons on 8091/udp.

The only Docker deployment in this repo is the **VPS hub** (a different
program: presence + WebRTC signaling, no LAN, no files) — see
`docs/DEPLOY-VPS.md`.

---

## 4. Send → receive walkthrough

### 4.1 On the sending machine

1. Open the **dashboard** URL from the startup banner in a desktop browser.
   (Chrome/Edge/Firefox — any modern one.)
2. **Pick files…** (multi-select) or **Pick folder…** (recursive walk).
   - Selected paths appear in their own **frame** (blue border) with a
     **Reset selection** button — obvious what's about to be sent.
   - "pack many small files as store-mode ZIP" is optional; for folders
     it can be handy on phones (one file to save instead of a tree).
     It's a ZIP with **no compression** (store mode) — near-instant pack.
3. **Start transfer** → the engine hashes chunks on the fly (SHA-256 per
   4 MiB chunk) → a **QR code** appears. **Cancel** (right of Start)
   ends the session.
4. Below the QR: **Copy link** (fallback). Live receiver progress moved
   to the **LAN clients / Activity** panel on the right (§4.1.1).

### 4.2 On the receiving device

Requirements: a **browser**, on the **same network** (same Wi-Fi/LAN
subnet). Nothing installed, no account, no app.

1. Scan the QR with the phone camera (or open the copied link on any
   device — desktops included).
2. The page shows the file list: paths, sizes, chunk counts. Verify what
   you're about to save matches what you expected.
3. Tap **Save all…**
   - Phone asks *where* to save (iOS: Files app location; Android:
     Downloads or a folder you pick) — confirm once.
   - Files stream chunk-by-chunk, each chunk SHA-256-verified; network
     hiccups are retried patiently (20 s patience window) instead of
     killing the transfer.
4. Per-file ✓ marks and a log appear live; sender sees the same progress.
5. Done → "Done — files saved." Open the Files app / Downloads folder.

**Receiving on the sending machine itself** ("Open on this device"
button): works, but saving needs a desktop browser with the File System
Access API (Chrome/Edge/Chromium on Windows/Linux).

### 4.2.1 The Activity frame (sender side)
Below the **LAN clients** card sits its own **Activity** card showing:

- **Activity** — a concise feed of what's happening: ▲ uploading
  (sessions started, receiver progress), ▼ downloading (sync folder
  pulls from other engines), • info, ! errors. Newest on top, last 12
  lines, refreshed every 3 s.
- **Live receiver progress** of your running transfer (which files the
  phone already has) — moved here from the sender card (M0.2).
- **File names while in flight**: the ▲ line lists which files the
  receiver is pulling right now, and each ▼ phone-push line lists the
  incoming file name(s) (`▼ phone pushing 2 file(s) — a.jpg, b.jpg +
  1 more — 2.1 GiB / 3.9 GiB`) — so you can verify the right files are
  moving, not just the byte count. When a phone batch completes, the
  line settles into `✓ phone sent N file(s) — … — batch done, link
  still open` (the QR stays valid for "Send more").

Both panels are read-only views; nothing here changes transfer state.

### 4.3 Cancel / stop / close mid-transfer

- Receiver: **Cancel transfer** → session ends, sender sees it. Nothing
  corrupts on the phone (partial files are discarded).
- Sender: **Cancel** (next to **Start transfer**) ends the current
  session immediately — same effect as the receiver cancelling.
- Sender: closing the dashboard tab is fine (chunks are served by the
  engine, not the tab). To end everything, `Ctrl+C` the engine.
- **Resume-after-close** (closing the phone tab mid-transfer, reopening
  the same link) is deliberately **M3** — in M0 the receiver restarts the
  *chunks*, not from scratch: it skips nothing (fresh session), so a
  full re-download happens. Design decision, not a bug; see
  `docs/PLAN.md` §3.

### 4.4 Chunk math (why it's robust)

Files travel as 4 MiB chunks (1024/2^22 bytes), each SHA-256-verified
individually by the receiver and retried with backoff. A 4 GB file =
~1024 chunks; a 5-second Wi-Fi dropout costs one 4 MiB re-request, not a
restart. The `X-Chunk-Hash` response header is a cross-check: the
receiver hashes the bytes itself and compares both.

### 4.5 Receive from a phone (phone → PC push, M1)

The reverse direction: the phone pushes files **into** the PC.

1. On the dashboard click **Open a phone upload link** (the *Receive
   from a phone* card). A QR appears.
2. Scan it (or copy the link) with the phone — it opens the `/u/`
   page. Pick **Choose files** or **Choose a folder**.
3. The phone builds the manifest (hashes every 4 MiB chunk), streams
   the chunks, and the engine verifies **every** chunk server-side
   before keeping it. The phone shows **which file is being sent**,
   and the dashboard's ▼ line lists the incoming name(s) — verify on
   either screen that the right files are moving.
4. Done — the files land in the **sync folder**
   (`~/Downloads/chunk-drop-files`) and propagate to your other
   engines automatically. (Set `receive_dir` in engine.toml to keep
   uploads on this PC instead.)

Notes:

- The link is dangerous-ish while open: **anyone with it can upload**
  to this PC until you press **Close link** (or it expires, 24 h).
  Close it as soon as the phone is done — closing is instant and
  terminal: the phone's next chunk/start gets a 401, and the card
  never re-opens a link you closed (you press the button again for
  the next transfer).
- One link, several batches: the phone's **Send more** starts a fresh
  batch on the same QR (the dashboard keeps showing it — the card only
  re-mints if the link ends on its own, e.g. expired or engine
  restarted). **Close link** kills the key instantly for every
  purpose: no start, chunk, or finish goes through afterwards.
- Upload sessions are RAM-only: restarting the engine kills a
  half-finished upload (the phone page shows an error; start a new
  link).
- Phone mtimes are preserved, so mesh newest-wins and sorting stay
  meaningful.

---

## 4.9 Install as an app (PWA) on the phone

The receiver page is an installable **PWA** (Add to Home Screen) — an
icon on the phone's home screen that opens ChunkDrop full-screen, like
a native app. Nothing is downloaded from an app store; the phone
fetches the app from the engine the first time.

> **The 404 note:** a `404` on `/api/peers` from a phone is **normal**
> — the dashboard is local-only by default (SECURITY.md §2), so other
> devices get the generic `404` there even with the group key. Phones
> use the receive page (`/r/` QR links) and never need the dashboard;
> the receive link shows no "back to sender dashboard" button for that
> reason. Receiving is unaffected, and so are the public static assets
> (`/assets/*`, `/icons/*`). A `404` on a *receive* page means an old
> engine binary: rebuild/redeploy (`make engine` /
> `docker compose up -d --build`).

### Android (Chrome, Chromium, Firefox)

1. Phone on the **same Wi-Fi** as the engine; open the engine URL
   (`http://<engine-ip>:8090/`) — any page of it works (dashboard or
   a receive link).
2. Menu **⋮ → Add to Home screen / Install app** → confirm.
   - Chrome shows a native install banner on second visit.
3. The icon (blue chunk-arrow) appears with the app name — launch
   opens it standalone, no browser bars.

### iOS/iPadOS (Safari — required for installs)

1. Open the engine URL in **Safari** (PWA install only works from
   Safari; the page itself runs fine in any browser).
2. **Share button → Add to Home Screen → Add**.
3. Launch from the home screen — standalone, full screen.

### What the installed app does

- Opens the dashboard (or last page) at engine quality — manifest
  view, chunked download with SHA-256 verify, retries.
- **No offline mode**: it's a LAN tool; without the engine reachable
  it shows the connection error. (Caching/offline is not a goal — see
  `docs/PLAN.md`.)
- Update = reload the page; the engine always serves the newest
  embedded build.

### PWA tech notes (for the curious)

- Manifest: `web/public/manifest.webmanifest` — name, standalone
  display, 192/512 PNG icons + maskable variants, `theme_color`.
- Icons: generated by `go run ./scripts/genicons` → `web/public/icons/`
  (reproducible, no binary assets in git).
- **No service worker (deliberate)**: install prompts work without one
  on Android Chrome and iOS Safari; a SW would add offline caching
  complexity that fights the "engine is the source of truth" model.
  If install prompts misbehave on some Chromium build later, a
  minimal cache-first-on-static SW is the fallback plan.

### 5.0 "404" on `/api/peers` (or any dashboard API) from a device

First check: **are you on another device?** The dashboard/control
surface is local-only by default (the dashboard can point a session at
any path and open dialogs on the engine PC — a remote control surface
is an attack vector). Another device gets the generic `404` even with
the correct group key, and that includes `/api/peers`, `/api/activity`,
`/api/start`, `/api/pick/*`, and the dashboard at `/`. Receiving (`/r/`
links) is unaffected — the page and its static bundle (`/assets/*`,
`/icons/*`) stay public, only the dashboard entry is guarded. If you
really need the dashboard from another device (headless NAS), set
`allow_remote_dashboard = true` in `engine.toml` and restart — it stays
group-key gated.

Otherwise: you're talking to an **old engine binary**. All dashboard
routes shipped in M0.1–M0.2; a `404` there means the route doesn't
exist. A wrong *key* answers `401`, never `404`. Fix: rebuild and
restart the engine, and hard-refresh the page (`Ctrl+Shift+R`) to drop
the cached old web app.

### 5.1 "Receiver's browser says it can't reach / connection refused"

- Are both devices on the **same network**? Guest Wi-Fi isolation and
  "AP isolation" on routers block device-to-device traffic — this is the
  #1 cause. Try the sender's URL in the *receiver's* browser: if it
  doesn't load, it's network, not ChunkDrop.
- **Firewall on the sender**: first Windows run shows the allow prompt
  (see §1). Linux: `sudo ufw allow 8090/tcp` (or your distro's tool);
  `firewalld`: `sudo firewall-cmd --add-port=8090/tcp`.

### 5.2 "401 / group key rejected" (sender dashboard)

- The group key in the dashboard doesn't match `engine.toml`. Re-copy
  from `engine keygen` output → paste into both places.
- **First-run template**: if the config still says
  `group_key = "<paste … here>"`, the engine correctly refuses to
  start sessions (fail closed). Set a real key (§2.1).

### 5.3 "Session key rejected (401)" (receiver page)

The QR/session is gone: session expired (24 h), sender pressed
`Ctrl+C`, or receiver hit **Cancel**. Ask the sender for a fresh QR —
that's the whole recovery procedure.

### 5.4 QR points at the wrong IP (VPN / Docker / virtual adapters)

The engine auto-picks a LAN IPv4, but VPNs (WireGuard, OpenVPN, corporate)
create adapters that look like LAN. Symptoms: QR contains an IP the phone
can't reach (e.g. `10.x` VPN range), while both devices are on Wi-Fi.

```bash
./engine --bind 192.168.1.20       # the actual Wi-Fi IP
# or persistent, in engine.toml:   bind_ip = "192.168.1.20"
```

The CLI prompt also offers a numbered picker when several adapters exist.

### 5.5 "Pick folder…" does nothing on the sending machine

`showDirectoryPicker()` needs a secure context and Chromium. On Firefox
the page falls back to multi-file pick — pick the files instead, or run
the dashboard in Chrome/Edge. (Folder receive-saves have the same
caveat, §4.2.)

### 5.6 Where did my file save on the phone?

- **iOS Safari**: the Files app opens a location picker; look in
  **On My iPhone** or the folder you picked, not Downloads.
- **Android Chrome**: the default is **Downloads** (or the folder the
  picker offered).

### 5.7 Windows: engine window closes instantly

Run it from PowerShell (`cd` to the folder, `.\engine-windows-amd64.exe`)
so output and errors stay visible. SmartScreen refusals close the window
silently — run via right-click → **Run anyway** once.

### 5.8 Restart / resume behavior — quick table

| Event | What happens |
|---|---|
| Wi-Fi blip / 5 s airplane-mode mid-transfer | Chunk retries patiently: network failures keep retrying for **20 s** (backoff up to 4 s per pause) — the phone waits out the blackout and resumes where it stopped. Engine answers (400/401) are final after 3 tries. |
| Phone gave up anyway (network lost > 20 s) | The transfer is **not** marked done; the phone shows the real error. Pick the files again on the phone — the same link restarts cleanly (slot state resets, staging wiped, everything re-uploads). |
| Close receiver tab mid-transfer | In M0: transfer stops. Reopen link → session gone → fresh transfer. (Resume is M3.) |
| `Ctrl+C` engine | Sessions die (RAM-only). Restart → new QR. |
| Engine machine reboot | Same as `Ctrl+C`. |
| 24 h since session start | Session expires → 401/410 → fresh QR. |

### 5.9 Stray "*.part" files in the sync folder (older build)

Engines built before this fix briefly announced their in-progress sync
pulls (`debian.iso.part`), so a peer could pull the half-finished file
back — sometimes repeatedly, as it grew. Fixed: temp files now live in
the excluded `hashes/tmp/` folder and are invisible to sync everywhere
(scan, announcements, planning). Any leftover `*.part` files still
sitting in `chunk-drop-files/` from the old build are plain junk — the
engine ignores them; delete them by hand whenever you like.

### 5.10 "I deleted it, and it came back"

Deleting a file in the sync folder removes it on **every** engine.
This is an explicit signal, not a guess: the engine records the
deletion (file name + mtime of the deleted version) in
`hashes/cache.json` and broadcasts it in every announcement. Peers
delete their copy right away (within one 5 s sync round) and re-announce
the record, so a device that was **offline** during the deletion still
receives it when it returns — records are kept and replayed for 24 h
(`TombstoneTTL`) and survive engine restarts.

Name + mtime decide identity: the same name with the same or an older
mtime is the deleted version (removed); a **newer** mtime is a new file
(kept, synced, stale record dropped). Older builds inferred deletions
from a file merely *not* being announced — which is also what a peer
restarting looks like — so deletions ping-ponged and never converged.
Update all engines; a mixed mesh with old builds can still resurrect
files.

### 5.11 Phone upload problems (M1)

| Symptom | Cause / fix |
|---|---|
| "Open a phone upload link" is greyed out | Set the group key in the dashboard first (the link is minted by the engine, which needs the key). |
| Phone says the computer rejected the transfer | The engine validates names/paths (`docs/PROTOCOL.md` §8) and size caps — rename the file (no `../`, no Windows-reserved names) or raise `max_upload_bytes`. |
| "not enough disk space: need X, Y free, Z reserved → A available" | The batch wouldn't fit the destination **directory's own filesystem** once other transfers in flight are counted. Free up space (or move `receive_dir` to a bigger drive in `engine.toml`) and send again — nothing was written, the phone just retries. The dashboard's Activity feed logs the same numbers. Mesh pulls refuse
the same way — the pull is retried next sync round, so free up space
and it lands then. |
| "Session key rejected"/401 on the phone after it worked | The link was closed (or the engine restarted — upload state is RAM-only). Open a new link. |
| "Send more" after a finished transfer | Works — the same link takes another batch (each batch re-uploads from scratch; staged bytes are re-verified by finish, so nothing stale can land). If the phone reports 401 instead, the link was closed, expired, or the engine restarted — rescan the QR. |
| Dashboard QR dead / re-scanning gives errors | The card keeps the QR while the link lives — through any number of batches. It **mints a fresh one automatically** only when the link ends on its own (24 h expiry, engine restart) or you closed it (closing is terminal; press the mint button for a new link). |
| Phone says "upload incomplete — N of M files abandoned" | The network dropped longer than the 20 s patience window. Stay connected and pick the files again; with `--debug` the engine log shows exactly which chunk failed and why. |
| Photos land with "created today" | The **modified** date is the photo's original date (that's what file managers and photo tools sort/show by default). On Windows the **created** date is stamped correctly too; on Linux/macOS filesystems there is no API to set a file's birth time, so that column stays "now" — a platform limitation, not a transfer bug. |
| Files didn't arrive but the phone says "Sent" | Check the destination: the **sync folder** by default, or `receive_dir` if set. The dashboard's Activity feed logs every landed file. |
| Phone picks a folder but files arrive flat | Firefox (some versions) ignores folder picks — it sends files without their folder prefix. Use Chrome/Safari on the phone, or accept the flat batch. |
| A `hashes/uploads/` folder in the sync folder | That's upload staging — never synced, never announced, auto-cleared. Leave it. |

---

## 6. What's safe / what to keep private

- **The QR link is the transfer's secret.** Anyone on the LAN with the
  link can fetch (only) that session's files. It lives in the URL
  *fragment* (`#/`), so it never appears in server logs or HTTP
  requests to other hosts.
- **An upload link works the other way: anyone with it can push files
  onto that PC** until you close the link (24 h max). Close it when the
  phone is done; the files land in the sync folder, so treat it like
  any other way a file can appear there.
- **The group key opens the sender side** (session creation). Keep it
  out of chats you don't trust; rotate on member exit (§2.1).
- The engine binds LAN interfaces and serves plain HTTP — fine for the
  LAN threat model, **not** for exposure to the internet (M2 brings TLS
  via the hub; `docs/SECURITY.md` §2 accepted risks).
- The receiver-side engine writes nothing to disk except its config; the
  sender streams from disk, never caching copies of what you send.

---

## 7. CLI reference

```
engine                  start the sender dashboard server
engine keygen           print a fresh 32-byte group key
engine version
engine help
engine --port <n>       bind a different port (default 8090 / config)
engine --bind <ip>      advertise a specific LAN IP in the QR
```

Exit codes: 0 clean shutdown (`Ctrl+C`/SIGTERM), 1 fatal startup error
(port busy, config unreadable). A busy port retries binding all
interfaces once before giving up.

---

## 8. FAQ

**Does the sender upload anything anywhere?** No. Bytes stream from the
sender's disk to the receiver's browser over your LAN. No server, cloud,
or relay sees file content in M0.

**Multiple receivers at once?** Same QR works for several devices; each
gets its own progress in the sender dashboard. (True multi-receiver
*sourcing* from each other is a later milestone.)

**Can I receive on the sender machine?** Yes — "Open on this device" or
the copied link in a local browser (§4.2).

**Big file limit?** RAM holds hashes + session state (a few MB per
session regardless of file size); files stream chunk-by-chunk. No
practical size cap.

**Does it work offline?** Both devices need the LAN, nothing else. No
internet required — airplane-mode Wi-Fi hotspot works (§ who uses this:
field teams).

**Where do I report bugs?** Open an issue with engine version, OS,
browser, and the exact step from this guide.

---

## 9. Milestone status (what's implemented)

| Area | Status |
|---|---|
| LAN send: files/folder → QR → browser save | ✅ M0 |
| Store-mode ZIP packing (many small files) | ✅ M0 |
| Session keys, 24 h expiry, cancel (sender **and** receiver) | ✅ M0/M0.2 |
| Native pick dialogs on the engine (dashboard remote control) | ✅ M0.1 |
| LAN discovery + peers panel (who's online) | ✅ M0.1 |
| Sync folder `~/Downloads/chunk-drop-files` (PC↔PC) | ✅ M0.1 |
| Dashboard: selection frame + Reset, Cancel, Activity feed | ✅ M0.2 |
| PWA install (iOS Safari, Android Chrome/Firefox) | ✅ M0.2 |
| Activity as a separate frame below LAN clients | ✅ M0.2 |
| Engine in Docker (LAN machine, deploy/) | ❌ removed in M2 — engine is a plain binary (§3.1) |
| VPS hub: presence + WebRTC signaling (relay fallback ⬜) | ✅ step ② (`DEPLOY-VPS.md`) |
| Upload direction (phone → PC) | ⬜ M1 |
| Internet mode via VPS hub | ⬜ M2 |
| Resume-after-close for receivers | ⬜ M3 |
| Real-device test matrix (iPhone/Android/Windows engines) | ⬜ needs hardware |
| Service worker / offline caching | ⬜ deliberately none (§4.9) |

See `docs/PLAN.md` §3 for the full roadmap and `docs/M0-TASKS.md` for
the build checklist this release passed.