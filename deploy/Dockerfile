# ChunkDrop hub — multi-stage build (deploy ONLY the hub, DEPLOY-VPS.md).
#
# The hub serves no web pages (the PWA is embedded in the engine
# binary, not here), so there is no Node stage: Go builds ./cmd/hub
# straight from source. Nothing is configured via env vars — group
# membership is authenticated per connection (group tag + device-key
# signature, SECURITY §5), the hub holds no secrets and writes nothing
# to disk (RAM-only, PLAN.md D5).
#
# Build:  docker build -f deploy/Dockerfile -t chunkdrop-hub .
# Run:    docker run -d --name chunkdrop-hub -p 127.0.0.1:8085:8080 \
#            chunkdrop-hub --addr :8080
# (nginx terminates TLS in front of it — deploy/nginx-site.conf.example)

# Stage 1 — build the hub binary
FROM golang:1.27-alpine AS build
WORKDIR /src
COPY go.mod go.sum ./
RUN go mod download
COPY cmd/ cmd/
COPY internal/ internal/
RUN CGO_ENABLED=0 GOOS=linux go build -trimpath -ldflags="-s -w" -o /out/hub ./cmd/hub

# Stage 2 — distroless runtime: no shell, no package manager, no
# writable anything; drops every capability and runs as nonroot.
FROM gcr.io/distroless/static-debian12:nonroot
COPY --from=build /out/hub /hub
USER nonroot
EXPOSE 8080/tcp
ENTRYPOINT ["/hub"]
