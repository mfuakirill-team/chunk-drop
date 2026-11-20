# VPS Deployment (hub) — M2

**What the VPS runs:** the **hub** (presence, WebRTC signaling, and
the WSS relay fallback — all stateless) and a **STUN server**
(coturn, STUN-only). Both are stateless: the hub holds no files, keeps
sessions only in RAM (24 h TTL), and writes nothing to disk. There is
**no engine on the VPS** — engines run on the devices that hold files.

**No secrets on the VPS.** The hub needs **zero configuration**: group
membership is authenticated per connection (group tag + device-key
signature, SECURITY §5), the group key itself never crosses the wire.
The Docker stack therefore has no env vars, no volumes, no mounts.

## 1. Topology

```
Internet ──443/tcp──► nginx (already on the VPS host, certbot/TLS)
                       │  proxy_pass http://127.0.0.1:8085
                       ▼
                    Docker: chunkdrop-hub   (127.0.0.1:8085 → :8080)
                       │  stateless, read-only, no volumes, no env
Internet ──3478/udp──► Docker: chunkdrop-stun (coturn, host network)
```

- The hub is published **only on localhost** — the outside world reaches
  it exclusively through nginx with TLS. Your existing nginx stays on
  the host untouched; we add one site config for `drop.404.mn`.
- coturn runs on the **host network** (`network_mode: host`): STUN
  responses must carry the VPS's real public IP, and inside a bridge
  network coturn would report Docker-internal addresses. It is
  STUN-only (`deploy/turnserver.conf` switches every relaying/allocation
  feature off) — the transfer fallback is the hub's own WSS relay, not
  TURN.

## 2. Files (live in `deploy/` in the repo)

| File | Purpose |
|---|---|
| `deploy/Dockerfile` | Go build → distroless. No Node stage: the hub serves no web pages (the PWA is embedded in the **engine** binary). |
| `deploy/docker-compose.yml` | `hub` (localhost-published, read-only, cap-drop ALL, no env/volumes) + `stun` (coturn, host network). |
| `deploy/turnserver.conf` | coturn STUN-only config — answers binding requests, refuses everything else. |
| `deploy/nginx-site.conf.example` | nginx site for `drop.404.mn` with the three mandatory proxy traps. |

The three proxy traps (marked in the example config) are all mandatory:

1. **WebSocket upgrade headers** — without them the engine's WSS
   connection never establishes.
2. **Long read/send timeouts (`3600s`)** — presence connections are
   long-lived and quiet (the engine pings every 25 s); nginx's default
   60 s idle cut would kill them.
3. **`proxy_buffering off`** — signaling frames and relayed bytes must
   not queue in nginx buffers.

## 3. First install (on the VPS)

```bash
# 0. DNS: A record  drop.404.mn  →  <VPS IP>   (your side)

# 1. cert for the subdomain (nginx + certbot already set up)
sudo certbot --nginx -d drop.404.mn
#    (or copy deploy/nginx-site.conf.example, adjust cert paths,
#     symlink into sites-enabled, nginx -t && systemctl reload nginx)

# 2. run the stack
git clone <repo> ~/chunkdrop && cd ~/chunkdrop/deploy
docker compose up -d --build
docker compose ps

# 3. verify
curl -s http://127.0.0.1:8085/healthz        # → ok
docker compose logs hub | head               # → hub 0.1.0-m2.3 listening on :8080
docker compose logs stun | head              # coturn banner + "Listening on port 3478" —
                                             # NOT "Operation not permitted" (exit 126 =
                                             # the image's file-capped binary vs cap rules;
                                             # see the comment in docker-compose.yml)
curl -s https://drop.404.mn/healthz          # → ok (through nginx + TLS)
```

**Firewall:** open `22/tcp`, `80/tcp`, `443/tcp` and **`3478/udp`**.
Nothing else — the hub port is not published publicly, and coturn runs
STUN-only (no relay port range to open).

## 4. Engine-side config (each device)

Fresh installs are preconfigured — the first-run template already
contains the defaults:

```toml
hub_url  = "wss://drop.404.mn/hub/ws"   # set = internet mesh on
stun_url = "stun:drop.404.mn:3478"
```

An **older** `engine.toml` was written with an explicit empty
`hub_url = ""` — empty means off, so those engines stay LAN-only until
you set the two lines above (or set the group key via the dashboard,
which rewrites the file without touching the hub lines).

Empty `hub_url` keeps the engine exactly as in M0/M1. With both set,
engines connect to the hub for presence/signaling and sync over the
internet when LAN is not good enough (routing policy: `lan_min_mbit`,
`probe_seconds`). Verify from the dashboard or `GET /api/hub` →
connected. The engine shows up on the hub as its device ID
(`hex(SHA-256(device.key))`) — no further registration.

## 5. Updates

```bash
cd ~/chunkdrop && git pull
cd deploy && docker compose up -d --build
```

Restarting is harmless: the hub is stateless, devices reconnect on
their next keepalive cycle and re-announce themselves. Rollback =
`git checkout <previous> && docker compose up -d --build`.

## 6. Resource expectations

- RAM: tens of MB idle; relayed transfers (step ③) add only buffer
  memory (the hub holds no stream state), capped at 80 Mbit/s of
  relay throughput.

## 7. Inner-TLS channel (M3) — for TLS-intercepting client networks

Some client networks (corporate/hotel proxies) terminate the outer
`:443` TLS handshake with their own certificate: engines there fail
with `x509: certificate signed by unknown authority`, or worse,
"succeed" against a proxy CA their OS trusts (a silent MITM). The M3
inner-TLS channel gives them **real TLS inside a plaintext `ws://`
connection** (docs/PROTOCOL.md §5): the cert is presented live by the
hub *inside* the tunnel and verified against the system roots +
`hub_cert_pin` — nothing cert-shaped is baked into engine binaries.

> **REQUIRED, not optional.** On intercepting networks the engine's
> first hop is the `:80 /hub/ws` endpoint — without the port-80
> location block below the inner-TLS channel cannot work anywhere, and
> a proxy that answers `:80` on the client's network answers with a
> redirect into its own TLS (engine dials must never follow redirects,
> so such a network fails loudly with the 30x status instead of a
> misleading x509 error). Do not skip step 1.

Steps (this is the one part that cannot be tested from a dev box —
it needs the VPS):

```bash
# 0. Host-side cert copy the nonroot distroless hub can read
#    (certbot's archive files are 0600 root — mounting them directly
#    would be unreadable AND stale after the first renewal).
install -d -m 0755 /etc/chunkdrop/certs
install -m 0644 /etc/letsencrypt/live/drop.404.mn/fullchain.pem /etc/chunkdrop/certs/
install -m 0640 -o 65532 -g 65532 /etc/letsencrypt/live/drop.404.mn/privkey.pem /etc/chunkdrop/certs/

# 1. Port 80: forward ONLY /hub/ws as a plain websocket (the example
#    site config already has the location block — deploy/nginx-site.conf.example
#    §"M3 inner-TLS channel"). Then:
nginx -t && systemctl reload nginx

# 2. Recreate the stack with the new compose (cert mount + hub flags)
cd ~/chunkdrop && git pull && cd deploy && docker compose up -d --build
docker compose logs hub | head
#    → "inner-TLS channel on: itls1 over ws:// terminates with /certs/fullchain.pem"

# 3. Renewals: never hand-copied certs go stale silently — add a
#    certbot deploy hook (certbot runs as root, so it may read the
#    privkey):
cat >/etc/letsencrypt/renewal-hooks/deploy/chunkdrop-certs.sh <<'EOF'
#!/bin/sh
install -m 0644 /etc/letsencrypt/live/drop.404.mn/fullchain.pem /etc/chunkdrop/certs/
install -m 0640 -o 65532 -g 65532 /etc/letsencrypt/live/drop.404.mn/privkey.pem /etc/chunkdrop/certs/
cd /root/chunkdrop/deploy && docker compose restart hub
EOF
chmod +x /etc/letsencrypt/renewal-hooks/deploy/chunkdrop-certs.sh

# 4. KEY REUSE — required for the pin baked into engine binaries:
#    the engines pin the LEAF PUBLIC KEY (SPKI). By default certbot
#    generates a new key at every renewal, which would silently
#    invalidate every baked pin at the next renewal. Turn key reuse on
#    once (Certbot ≥ 2.3); combined with the deploy hook above the pin
#    then survives renewals indefinitely (the cert rotates, the key
#    does not):
certbot reconfigure --cert-name drop.404.mn --reuse-key
#    (Older certbot: add `reuse_key = True` under [renewalparams] in
#    /etc/letsencrypt/renewal/drop.404.mn.conf, or run one
#    `certbot renew --cert-name drop.404.mn --reuse-key --force-renewal`,
#    which persists the option. If the key ever DOES rotate
#    deliberately — e.g. a compromise — the new pin arrives from
#    `make pin HOST=drop.404.mn`; engines accept old+new pins
#    comma-separated in hub_cert_pin during the transition.)
```

Smoke tests (from a normal network — the classic wss channel must keep
working; then from an MITM'd network if you have one):

```bash
curl -s https://drop.404.mn/healthz          # → ok  (443 unchanged)
# wss still connects: engine starts, GET /api/hub → "channel": "tls"
# inner-TLS endpoint answers (marker handling lives in the ws layer):
npx wscat -c ws://drop.404.mn/hub/ws
#   send text "hi"  → {"type":"error","error":"first message must be hello"}  (legacy path intact)
#   the binary marker "chunkdrop-itls1" on an itls-enabled hub starts
#   a TLS server handshake — wscat prints a binary frame (the server hello)
# from an engine on the intercepting network:
#   GET /api/hub → "channel": "ws-tls" and connected: true
```

Hardening: engines shipped from this repo **bake the hub leaf-certificate
pin in at build time** (Makefile `HUBPIN_FALLBACK` / auto-fetched
`HUBPIN`; see the Makefile header). With the pin set — baked or per
engine in `engine.toml` (`hub_cert_pin`, comma-separated old+new across
a key rotation) — even a proxy whose root CA the OS trusts cannot
silently read the hub connection: the mismatch aborts loudly. The
`--reuse-key` step above is what keeps a baked pin valid across
renewals.
- CPU: negligible — all crypto lives in the clients by design.
- Disk: the image itself; **zero user data, ever**.
- Bandwidth: STUN binding requests are tiny; P2P WebRTC transfers cost
  the VPS nothing. Only the relay fallback (step ③) consumes traffic.
