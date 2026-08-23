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

.PHONY: all build test vet vet-cross staticcheck fmt fmt-check lint check clean action-test

all: check build

build:
	$(GO) build -ldflags "-X main.version=$(VERSION)" -o $(BIN) ./cmd/mcpsnoop

test:
	$(GO) test $(PKG)

vet:
	$(GO) vet $(PKG)

# The platforms CI compiles on. The release ships Windows and macOS binaries and
# the CI matrix vets Windows, but a local `go vet` only ever sees the host, so
# platform-specific code is compiled here for the first time on somebody else's
# pull request.
CROSS_GOOS ?= windows darwin linux

# vet-cross typechecks every package *and its tests* for each of those. Building
# the binaries is not enough and was not: `GOOS=windows go build ./...` skips
# test files entirely, so a test calling syscall.Mkfifo behind a runtime
# GOOS check passed every local gate and broke the Windows job, because a
# runtime skip does not stop the compiler needing the symbol. Guard that code
# with a build tag, the way console_unix.go does, and this target proves it.
vet-cross:
	@for os in $(CROSS_GOOS); do \
		echo "GOOS=$$os go vet $(PKG)"; \
		GOOS=$$os $(GO) vet $(PKG) || exit 1; \
	done

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

# The GitHub Action is shell, so the Go gate above cannot see it, and it is the
# one part of this repository that runs on other people's machines. Its tests
# drive the real scripts against a stand-in releases page and a stand-in binary,
# so they need mcpsnoop built but no network. shellcheck is run when it is here,
# since a contributor working on Go has no reason to have installed it.
action-test:
	@$(GO) build -o "$(CURDIR)/.action-bin/mcpsnoop" ./cmd/mcpsnoop
	@if command -v shellcheck >/dev/null 2>&1; then \
		shellcheck action/*.sh action/tests/*.sh || exit 1; \
	else \
		echo "shellcheck not installed, skipping the shell lint"; \
	fi
	@PATH="$(CURDIR)/.action-bin:$$PATH" bash action/tests/install_test.sh
	@PATH="$(CURDIR)/.action-bin:$$PATH" bash action/tests/check_test.sh
	@rm -rf "$(CURDIR)/.action-bin"

lint: vet vet-cross staticcheck

# check is the pre-commit/CI gate, formatting, static analysis, and the full
# test suite under the race detector (matching CI).
check: fmt-check lint action-test
	TZ=$(CHECK_TZ) $(GO) test -race $(PKG)

clean:
	rm -f $(BIN)
	rm -rf dist .action-bin
