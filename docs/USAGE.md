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
| `receive_dir` | *(M1)* | Where received uploads land (upload direction ships in M1). |
| `max_upload_bytes` | `106253862144` | Per-transfer upload cap (M1). Ignore for now. |

### 2.3 Delete everything

Nothing is written outside the config file (sessions are RAM-only, files
stream straight from disk). To uninstall: delete the binary and the config
directory.

---

## 3. Run the engine

```bash
./engine                    # port from config (default 8090)
./engine -port 8091         # one-off port override
./engine -bind 192.168.1.20  # pin the advertised LAN IP (see §5.4)
./engine help               # all flags
./engine version
./engine keygen             # fresh group key (see §2.1)
```

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

---

## 4. Send → receive walkthrough

### 4.1 On the sending machine

1. Open the **dashboard** URL from the startup banner in a desktop browser.
   (Chrome/Edge/Firefox — any modern one.)
2. **Pick files…** (multi-select) or **Pick folder…** (recursive walk).
   - "pack many small files as store-mode ZIP" is optional; for folders
     it can be handy on phones (one file to save instead of a tree).
     It's a ZIP with **no compression** (store mode) — near-instant pack.
3. **Start transfer** → the engine hashes chunks on the fly (SHA-256 per
   4 MiB chunk) → a **QR code** appears.
4. Below the QR: **Copy link** (fallback) and receiver progress (live %,
   polled from the phone).

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
   - Files stream chunk-by-chunk, each chunk SHA-256-verified, retried
     up to 3× with backoff on network hiccups.
4. Per-file ✓ marks and a log appear live; sender sees the same progress.
5. Done → "Done — files saved." Open the Files app / Downloads folder.

**Receiving on the sending machine itself** ("Open on this device"
button): works, but saving needs a desktop browser with the File System
Access API (Chrome/Edge/Chromium on Windows/Linux).

### 4.3 Cancel / stop / close mid-transfer

- Receiver: **Cancel transfer** → session ends, sender sees it. Nothing
  corrupts on the phone (partial files are discarded).
- Sender: closing the dashboard tab is fine (chunks are served by the
  engine, not the tab). To end a session, `Ctrl+C` the engine.
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

---

## 5. Troubleshooting

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
./engine -bind 192.168.1.20        # the actual Wi-Fi IP
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
| Wi-Fi blip mid-transfer | Chunk retries (up to 3, backoff). Transfer continues. |
| Close receiver tab mid-transfer | In M0: transfer stops. Reopen link → session gone → fresh transfer. (Resume is M3.) |
| `Ctrl+C` engine | Sessions die (RAM-only). Restart → new QR. |
| Engine machine reboot | Same as `Ctrl+C`. |
| 24 h since session start | Session expires → 401/410 → fresh QR. |

---

## 6. What's safe / what to keep private

- **The QR link is the transfer's secret.** Anyone on the LAN with the
  link can fetch (only) that session's files. It lives in the URL
  *fragment* (`#/`), so it never appears in server logs or HTTP
  requests to other hosts.
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
engine -port <n>        bind a different port (default 8090 / config)
engine -bind <ip>       advertise a specific LAN IP in the QR
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
| Session keys, 24 h expiry, cancel | ✅ M0 |
| Upload direction (phone → PC) | ⬜ M1 |
| Internet mode via VPS hub | ⬜ M2 |
| Resume-after-close for receivers | ⬜ M3 |

See `docs/PLAN.md` §3 for the full roadmap and `docs/M0-TASKS.md` for
the build checklist this release passed.