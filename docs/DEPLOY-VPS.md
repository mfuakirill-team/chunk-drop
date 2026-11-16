# VPS Deployment (hub) — M2

**What the VPS runs:** the **hub** (presence, WebRTC signaling — the
WSS relay fallback comes in a later M2 step) and a **STUN server**
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
docker compose logs hub | head               # → hub 0.1.0-m2.2 listening on :8080
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

In `engine.toml` (created by `engine keygen`):

```toml
hub_url  = "wss://drop.404.mn/hub/ws"   # empty = internet mesh off (LAN-only, M0/M1 behavior)
stun_url = "stun:drop.404.mn:3478"
```

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
  memory, capped at 80 Mbit/s of relay throughput.
- CPU: negligible — all crypto lives in the clients by design.
- Disk: the image itself; **zero user data, ever**.
- Bandwidth: STUN binding requests are tiny; P2P WebRTC transfers cost
  the VPS nothing. Only the relay fallback (step ③) consumes traffic.
