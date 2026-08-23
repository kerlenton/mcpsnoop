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
# Pinned for the third time for the same reason, one layer out. `make check` was
# green on shellcheck 0.11.0 while CI failed on the ubuntu image's older one,
# which still reports SC2015 for a pattern 0.11 accepts. An analyser the two
# sides do not share is an analyser that finds things only on somebody else's
# pull request. Downloaded when the local one is a different version, the way
# staticcheck is run through `go run` at a fixed version.
SHELLCHECK_VERSION ?= v0.11.0

.PHONY: all build test vet vet-cross staticcheck fmt fmt-check lint check clean action-test shellcheck-bin

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
action-test: shellcheck-bin
	@$(GO) build -o "$(CURDIR)/.action-bin/mcpsnoop" ./cmd/mcpsnoop
	@"$(CURDIR)/.action-bin/shellcheck" action/*.sh action/tests/*.sh
	@PATH="$(CURDIR)/.action-bin:$$PATH" bash action/tests/install_test.sh
	@PATH="$(CURDIR)/.action-bin:$$PATH" bash action/tests/check_test.sh
	@bash action/tests/docs_test.sh
	@rm -rf "$(CURDIR)/.action-bin"

# Resolve a shellcheck of exactly SHELLCHECK_VERSION: the one already installed
# if it is that version, otherwise a downloaded copy. It is a Haskell binary
# rather than a Go module, so there is no `go run` equivalent to lean on.
shellcheck-bin:
	@mkdir -p "$(CURDIR)/.action-bin"
	@want="$(SHELLCHECK_VERSION)"; want="$${want#v}"; \
	if command -v shellcheck >/dev/null 2>&1 && \
		shellcheck --version | grep -qx "version: $$want"; then \
		ln -sf "$$(command -v shellcheck)" "$(CURDIR)/.action-bin/shellcheck"; \
	elif [ ! -x "$(CURDIR)/.action-bin/shellcheck" ]; then \
		case "$$(uname -s)/$$(uname -m)" in \
			Darwin/arm64) plat=darwin.aarch64 ;; \
			Darwin/*) plat=darwin.x86_64 ;; \
			Linux/aarch64|Linux/arm64) plat=linux.aarch64 ;; \
			Linux/*) plat=linux.x86_64 ;; \
			*) echo "no pinned shellcheck for $$(uname -s)/$$(uname -m)"; exit 1 ;; \
		esac; \
		echo "fetching shellcheck $(SHELLCHECK_VERSION) for $$plat"; \
		curl -fsSL "https://github.com/koalaman/shellcheck/releases/download/$(SHELLCHECK_VERSION)/shellcheck-$(SHELLCHECK_VERSION).$$plat.tar.xz" \
			| tar -xJ -C "$(CURDIR)/.action-bin" --strip-components=1 "shellcheck-$(SHELLCHECK_VERSION)/shellcheck"; \
	fi

lint: vet vet-cross staticcheck

# check is the pre-commit/CI gate, formatting, static analysis, and the full
# test suite under the race detector (matching CI).
check: fmt-check lint action-test
	TZ=$(CHECK_TZ) $(GO) test -race $(PKG)

clean:
	rm -f $(BIN)
	rm -rf dist .action-bin
