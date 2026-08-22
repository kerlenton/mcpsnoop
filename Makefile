GO ?= go
BIN := mcpsnoop
PKG := ./...
VERSION ?= $(shell git describe --tags --always --dirty 2>/dev/null || echo dev)
# Pinned so `make check` and CI run the same analyser. They did not: the Makefile
# reused whatever staticcheck was already on PATH while CI installed @latest, so
# a deprecation a newer release had learned about passed locally and failed in
# CI. Bump this deliberately rather than discovering it on an unrelated pull
# request.
STATICCHECK_VERSION ?= v0.8.0
# Pinned for the same reason, one layer down. CI runners are UTC and a
# contributor's machine is whatever they live in, so a test that renders a local
# timestamp passes in one and fails in the other. It has: an inventory test took
# the last six characters of an RFC3339 stamp as its offset, which is the offset
# at +03:00 and part of the minutes at UTC. `check` claims to match CI, so it
# pins the zone the way it pins the analyser. Override it to reproduce something
# zone-specific.
CHECK_TZ ?= UTC

.PHONY: all build test vet staticcheck fmt fmt-check lint check clean

all: check build

build:
	$(GO) build -ldflags "-X main.version=$(VERSION)" -o $(BIN) ./cmd/mcpsnoop

test:
	$(GO) test $(PKG)

vet:
	$(GO) vet $(PKG)

# staticcheck catches non-idiomatic code (e.g. interface{} over any, dead code).
# Run through `go run` at the pinned version rather than whatever binary is on
# PATH, so the result does not depend on when a contributor last installed it.
staticcheck:
	$(GO) run honnef.co/go/tools/cmd/staticcheck@$(STATICCHECK_VERSION) $(PKG)

fmt:
	gofmt -s -w .

# fmt-check fails (for CI) if any file is not simplified/gofmt'd.
fmt-check:
	@out="$$(gofmt -s -l .)"; if [ -n "$$out" ]; then echo "gofmt -s needed:"; echo "$$out"; exit 1; fi

lint: vet staticcheck

# check is the pre-commit/CI gate, formatting, static analysis, and the full
# test suite under the race detector (matching CI).
check: fmt-check lint
	TZ=$(CHECK_TZ) $(GO) test -race $(PKG)

clean:
	rm -f $(BIN)
	rm -rf dist
