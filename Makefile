GO ?= go
BIN := mcpsnoop
PKG := ./...

.PHONY: all build test vet staticcheck fmt fmt-check lint check clean

all: check build

build:
	$(GO) build -o $(BIN) ./cmd/mcpsnoop

test:
	$(GO) test $(PKG)

vet:
	$(GO) vet $(PKG)

# staticcheck catches non-idiomatic code (e.g. interface{} over any, dead code).
staticcheck:
	@command -v staticcheck >/dev/null 2>&1 || $(GO) install honnef.co/go/tools/cmd/staticcheck@latest
	staticcheck $(PKG)

fmt:
	gofmt -s -w .

# fmt-check fails (for CI) if any file is not simplified/gofmt'd.
fmt-check:
	@out="$$(gofmt -s -l .)"; if [ -n "$$out" ]; then echo "gofmt -s needed:"; echo "$$out"; exit 1; fi

lint: vet staticcheck

check: fmt-check lint test

clean:
	rm -f $(BIN)
	rm -rf dist
