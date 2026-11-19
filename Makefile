# ChunkDrop — M0 build targets.
# Every milestone ends in something usable (docs/PLAN.md §3).

GO ?= go
NODE ?= npm

# Static binaries, no cgo, stripped (SmartScreen note in docs/PLAN.md §5).
# -buildvcs=false: `go build` stamps VCS info by invoking git, which
# fails (exit 128, dubious ownership) when the repo is mounted into a
# container owned by another uid — CI does exactly that. Nothing reads
# the VCS stamp (the version string is set in code), so it's off.
LDFLAGS := -s -w
# HUBPIN=<sha256/...> bakes the hub certificate pin into every binary
# built in this invocation (internal/hubclient.DefaultPin). Get the
# value with `make pin HOST=drop.404.mn`. Config hub_cert_pin wins
# when set.
ifneq ($(strip $(HUBPIN)),)
LDFLAGS += -X chunkdrop/internal/hubclient.DefaultPin=$(HUBPIN)
endif
BUILDFLAGS := -trimpath -buildvcs=false -ldflags="$(LDFLAGS)"

.PHONY: all web engine hub engine-win engine-linux engine-mac test clean pin help

all: engine hub

## web: build the Preact app into web/dist and sync for go:embed
web:
	cd web && $(NODE) ci --silent 2>/dev/null || $(NODE) install --silent
	cd web && $(NODE) run build
	@# go:embed can't reach web/dist from internal/webdist — sync it.
	@# The glob keeps .gitkeep (the fresh-clone placeholder that makes
	@# `go build`/`go test` work before the first `make web`).
	@rm -rf internal/webdist/dist/*
	@cp -a web/dist/. internal/webdist/dist/

## engine: build the engine binary for the current platform
engine: web
	$(GO) build $(BUILDFLAGS) -o bin/engine ./cmd/engine

## hub: build the coordination server (M2, VPS/Docker)
hub:
	$(GO) build $(BUILDFLAGS) -o bin/hub ./cmd/hub

## engine-win: cross-compile Windows amd64 (the group's daily driver)
engine-win: web
	GOOS=windows GOARCH=amd64 CGO_ENABLED=0 $(GO) build $(BUILDFLAGS) -o bin/engine-windows-amd64.exe ./cmd/engine

## engine-linux: cross-compile Linux amd64 + arm64 (NAS, later)
engine-linux: web
	GOOS=linux GOARCH=amd64 CGO_ENABLED=0 $(GO) build $(BUILDFLAGS) -o bin/engine-linux-amd64 ./cmd/engine
	GOOS=linux GOARCH=arm64 CGO_ENABLED=0 $(GO) build $(BUILDFLAGS) -o bin/engine-linux-arm64 ./cmd/engine

## engine-mac: untested, but free (docs/PLAN.md §4)
engine-mac: web
	GOOS=darwin GOARCH=arm64 CGO_ENABLED=0 $(GO) build $(BUILDFLAGS) -o bin/engine-darwin-arm64 ./cmd/engine

## test: unit tests (transfer rules, session, api, lanip)
test:
	$(GO) test ./...

## pin: fetch the hub certificate's SPKI pin (HOST=drop.404.mn).
## Prints "sha256/<base64>" — put that in engine.toml (hub_cert_pin)
## or pass HUBPIN=<that> to make engine.
pin:
	@echo "pin for $(HOST) (leaf certificate public key):"
	@echo "sha256/$$(openssl s_client -servername $(HOST) -connect $(HOST):443 </dev/null 2>/dev/null \
		| openssl x509 -pubkey -noout \
		| openssl pkey -pubin -outform DER \
		| openssl dgst -sha256 -binary | base64)"

## clean: build artifacts
clean:
	rm -rf bin internal/webdist/dist
	mkdir -p internal/webdist/dist

help:
	@grep -E '^##' $(MAKEFILE_LIST) | sed 's/## //'