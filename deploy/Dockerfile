# ChunkDrop engine — multi-stage build.
#
# The engine serves the embedded web app (Preact) from inside the binary
# (go:embed), so "docker run" needs no volumes and no external assets.
# Ports: 8090/tcp (HTTP), 8091/udp (discovery beacons).
#
# Build:  docker build -t chunkdrop-engine .
# Run:     docker run -d --name chunkdrop \
#            -p 8090:8090 -p 8091:8091/udp chunkdrop-engine
#
# Config: engine.toml (group_key) — mount read-only:
#           -v ~/.config/chunkdrop/engine.toml:/root/.config/chunkdrop/engine.toml:ro
#         (or pass env CHUNKDROP_GROUP_KEY=<key> as a fallback).

# Stage 1 — build the web app (Vite → web/dist)
FROM node:22-alpine AS web
WORKDIR /src/web
COPY web/package.json web/package-lock.json ./
RUN npm ci --silent
COPY web/ ./
RUN npm run build

# Stage 2 — build the engine with the freshly built web app embedded
FROM golang:1.27-alpine AS build
WORKDIR /src
COPY go.mod go.sum ./
RUN go mod download
COPY cmd/ cmd/
COPY internal/ internal/
COPY --from=web /src/web/dist internal/webdist/dist/
# webdist/dist is go:embed'ed; the placeholder dot-file keeps the tree non-empty in git,
# the real build overwrites it above.
RUN CGO_ENABLED=0 GOOS=linux go build -trimpath -ldflags="-s -w" -o /out/engine ./cmd/engine

# Stage 3 — minimal runtime (no shell, no package manager)
FROM alpine:3.20
COPY --from=build /out/engine /engine
# Non-root, unprivileged.
USER 65534:65534
EXPOSE 8090/tcp 8091/udp
VOLUME /config
ENV HOME /config
ENTRYPOINT ["/engine"]