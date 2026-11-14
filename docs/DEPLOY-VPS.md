# VPS Deployment (hub)

**Not needed until M2.** M0–M1 are engine-only and touch no server at all.
This doc exists now so the M2 build has nothing left to invent.

**What the VPS is for (locked):** the container there is the **hub** —
a *relay and PWA host only*. It never joins the LAN (no host network
mode, no UDP discovery), holds no files, and lets transfers work over
the **global network** unless the client opts out — the engine gets a
config flag (`internet_sync = false` to disable reaching the hub) so
the feature can be turned off on the client's side (M2 scope).

## 1. Topology (recommended — matches your setup)

```
Internet ──443──► nginx (already on the VPS host, certbot/TLS)
                    │  proxy_pass http://127.0.0.1:8085
                    ▼
                 Docker: chunkdrop-hub (publishes 127.0.0.1:8085:8080)
                    │  stateless, read-only, no volumes, never stores files
```

The hub is published **only on localhost** — the outside world can only reach
it through nginx with TLS. Existing nginx stays on the host as-is; we add one
site config for a subdomain (e.g. `xfer.yourdomain.com`).

Alternative if you ever want the whole stack containerized: run nginx inside
the compose file too, publish 80/443, mount the certbot certs read-only. The
recommended variant avoids touching your working nginx; both are fine.

## 2. Files (live in `deploy/` in the repo)

### `deploy/Dockerfile`

```dockerfile
# 1. Build the web app
FROM node:22-alpine AS web
WORKDIR /src/web
COPY web/package*.json ./
RUN npm ci
COPY web/ ./
RUN npm run build          # outputs web/dist

# 2. Build the hub binary (web/dist is embedded via go:embed)
FROM golang:1.25-alpine AS build
WORKDIR /src
COPY go.mod go.sum ./
RUN go mod download
COPY . .
COPY --from=web /src/web/dist ./web/dist
RUN CGO_ENABLED=0 go build -trimpath -ldflags="-s -w" -o /out/hub ./cmd/hub

# 3. Distroless runtime: no shell, no package manager, no writable anything
FROM gcr.io/distroless/static-debian12:nonroot
COPY --from=build /out/hub /hub
USER nonroot
ENTRYPOINT ["/hub"]
```

### `deploy/docker-compose.yml`

```yaml
services:
  hub:
    build: ..
    # or, later: image: ghcr.io/<you>/chunkdrop-hub:latest
    container_name: chunkdrop-hub
    restart: unless-stopped
    ports:
      - "127.0.0.1:8085:8080"      # localhost only — nginx is the front door
    environment:
      GROUP_KEY: "REPLACE_WITH_engine-keygen-OUTPUT"
      STUN_URLS: "stun:stun.l.google.com:19302"
      ROOM_TTL_MINUTES: "1440"
    read_only: true
    cap_drop: [ALL]
    security_opt: [no-new-privileges:true]
    healthcheck:
      test: ["CMD", "/hub", "-healthcheck"]   # or wget-free HTTP probe binary
      interval: 30s
      timeout: 3s
      retries: 3
    # NOTE: no volumes. The hub is stateless BY DESIGN (PLAN.md D5).
```

### `deploy/nginx-site.conf.example`

The three classic proxy traps are marked — all three were identified in the
planning discussion and are mandatory.

```nginx
# /etc/nginx/sites-available/xfer.yourdomain.com
server {
    listen 443 ssl;
    server_name xfer.yourdomain.com;

    # certbot manages these:
    ssl_certificate     /etc/letsencrypt/live/xfer.yourdomain.com/fullchain.pem;
    ssl_certificate_key /etc/letsencrypt/live/xfer.yourdomain.com/privkey.pem;

    location / {
        proxy_pass http://127.0.0.1:8085;

        # TRAP 1 — WebSockets do not work without these:
        proxy_http_version 1.1;
        proxy_set_header Upgrade $http_upgrade;
        proxy_set_header Connection "upgrade";

        proxy_set_header Host $host;
        proxy_set_header X-Real-IP $remote_addr;

        # TRAP 2 — idle signaling connections get silently cut without this:
        proxy_read_timeout 3600s;
        proxy_send_timeout 3600s;

        # TRAP 3 — nginx buffering strangles relay throughput:
        proxy_buffering off;
    }
}
server {
    listen 80;
    server_name xfer.yourdomain.com;
    return 301 https://$host$request_uri;
}
```

## 3. First install (on the VPS)

```bash
# DNS: A record  xfer.yourdomain.com  →  <dedicated VPS IP>

# cert for the subdomain (your nginx is already set up for certbot)
sudo certbot --nginx -d xfer.yourdomain.com

# run the hub
git clone <repo> ~/chunkdrop && cd ~/chunkdrop
# put the group key into deploy/docker-compose.yml (from `engine keygen`)
cd deploy && docker compose up -d --build
docker compose ps        # healthy?
curl -s http://127.0.0.1:8085/healthz
```

Firewall: only 22/80/443 need to be open. The hub port is not published
publicly.

## 4. Updates

```bash
cd ~/chunkdrop
git pull
cd deploy && docker compose up -d --build
```

In-flight rooms die on restart (stateless); worst case users re-enter a
6-char code. Rollback = `git checkout <previous> && docker compose up -d --build`.

## 5. Resource expectations

- RAM: tens of MB idle; a relayed multi-GB transfer adds only buffer memory.
- CPU: negligible (crypto is in clients' hands by design — §5 of SECURITY.md).
- Disk: the image itself; **zero user data, ever**.
- Bandwidth: only fallback-relayed transfers consume VPS traffic; P2P
  successes cost nothing.
