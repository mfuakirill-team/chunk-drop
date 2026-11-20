# ChunkDrop — M0 build targets.
# Every milestone ends in something usable (docs/PLAN.md §3).

GO ?= go
NODE ?= npm

# Static binaries, no cgo, stripped (SmartScreen note in docs/PLAN.md §5).
# -buildvcs=false: `go build` stamps VCS info by invoking git, which
# fails (exit 128, dubious ownership) when the repo is mounted into a
# container owned by another uid — CI does exactly that. Nothing reads
# the VCS stamp (the version string is set in code), so it's off.

# ── Hub pin: baked by default ──────────────────────────────────────
# The hub domain is fixed; the leaf certificate's SPKI pin is baked
# into every linked binary (internal/hubclient.DefaultPin) so shipped
# builds verify the hub out of the box. A config hub_cert_pin still
# wins when set.

# Hub host (fixed in practice; override only for smoke setups).
HUBHOST ?= drop.404.mn

# HUBPIN_FALLBACK: committed fallback pin, used when the auto-fetch
# below yields nothing (hub unreachable from the build machine, or its
# certificate fails verification). Fill it from a trusted network —
# ideally the VPS itself — and paste the "sha256/..." line here:
#
#     make pin HOST=drop.404.mn
#
# NOTE: certbot renewals must reuse the key (deploy step: certbot
# --reuse-key) or a new leaf key invalidates this pin. See
# docs/DEPLOY-VPS.md §7.
HUBPIN_FALLBACK ?=

# HUBPIN: auto-fetched from the live hub at build time, with FULL
# chain + hostname verification (-verify_hostname -verify_return_error).
# A MITM'd build network makes verification fail and yields an EMPTY
# pin — which falls back to HUBPIN_FALLBACK — never a poisoned one.
# An explicit `make engine HUBPIN=<sha256/... or bare base64>`
# overrides the live fetch. Recursive (?=) assignment: openssl runs
# only when a binary is actually linked, not on every make invocation.
# The fetch emits the prefixed runtime form ("sha256/<base64>") —
# the -X stamp is the raw DefaultPin string, and the engine's pin
# parser REQUIRES the prefix; a bare base64 baked here fails every
# dial with "want sha256/ + base64" (the regression this guards).
HUBPIN ?= $(shell pem=$$(openssl s_client -connect $(HUBHOST):443 -servername $(HUBHOST) -verify_hostname $(HUBHOST) -verify_return_error </dev/null 2>/dev/null \
	| openssl x509 -pubkey -noout 2>/dev/null); \
	if [ -n "$$pem" ]; then printf '%s\n' "$$pem" \
		| openssl pkey -pubin -outform DER 2>/dev/null \
		| openssl dgst -sha256 -binary | base64; fi)

# pin_norm: one entry → the runtime form. Accepts bare base64 (what
# the hash pipeline produces) or already-prefixed values (an explicit
# HUBPIN=sha256/...). filter gives an exact prefix test — no
# substring games.
pin_norm = $(if $(filter sha256/%,$(1)),$(1),$(if $(strip $(1)),sha256/$(1),))

# The pins actually baked: auto-fetched HUBPIN (explicit override or
# verified live fetch) FIRST, the committed fallback SECOND, joined
# with a comma when both exist (the engine's DefaultPin accepts a
# comma list — so a certbot key rotation with a refreshed fallback
# keeps the old key accepted). Nothing → no baked pin (pin comes from
# config only).
comma := ,
pin_auto = $(strip $(call pin_norm,$(HUBPIN)))
pin_fall = $(strip $(call pin_norm,$(HUBPIN_FALLBACK)))
bakedpin = $(pin_auto)$(if $(pin_auto),$(if $(pin_fall),$(comma)))$(pin_fall)

LDFLAGS = -s -w $(if $(strip $(bakedpin)),-X chunkdrop/internal/hubclient.DefaultPin=$(bakedpin))
BUILDFLAGS = -trimpath -buildvcs=false -ldflags="$(LDFLAGS)"

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
## Prints "sha256/<base64>" — put that in HUBPIN_FALLBACK in this
## Makefile (bakes it into every build), in engine.toml (hub_cert_pin),
## or pass HUBPIN=<that> to make engine. The certificate is verified
## (chain + hostname) first — against a MITM'd network this prints
## nothing and fails, rather than handing you an attacker's pin.
pin: HOST ?= $(HUBHOST)
pin:
	@echo "pin for $(HOST) (leaf certificate public key, verified):"
	@p=$$(pem=$$(openssl s_client -connect $(HOST):443 -servername $(HOST) -verify_hostname $(HOST) -verify_return_error </dev/null 2>/dev/null \
		| openssl x509 -pubkey -noout 2>/dev/null); \
	if [ -n "$$pem" ]; then printf '%s\n' "$$pem" \
		| openssl pkey -pubin -outform DER 2>/dev/null \
		| openssl dgst -sha256 -binary | base64; fi); \
	if [ -n "$$p" ]; then echo "sha256/$$p"; \
	else echo "EMPTY — certificate verification failed against $(HOST); not printing an untrusted pin" >&2; exit 1; fi

## clean: build artifacts
clean:
	rm -rf bin internal/webdist/dist
	mkdir -p internal/webdist/dist

help:
	@grep -E '^##' $(MAKEFILE_LIST) | sed 's/## //'