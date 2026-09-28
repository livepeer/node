GO ?= go
VERSION ?= dev
COMMIT ?= $(shell git rev-parse --short=12 HEAD 2>/dev/null || echo unknown)
LDFLAGS = -X github.com/livepeer/node/internal/version.Name=$(VERSION) -X github.com/livepeer/node/internal/version.Commit=$(COMMIT)

.PHONY: build test
build:
	mkdir -p bin
	$(GO) build -ldflags '$(LDFLAGS)' -o bin/livepeer ./cmd/livepeer
	$(GO) build -ldflags '$(LDFLAGS)' -o bin/livepeer-orchestrator ./cmd/livepeer-orchestrator
	$(GO) build -ldflags '$(LDFLAGS)' -o bin/livepeer-signer ./cmd/livepeer-signer
	$(GO) build -ldflags '$(LDFLAGS)' -o bin/livepeer-chain ./cmd/livepeer-chain

test:
	$(GO) test -race ./...
