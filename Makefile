GO      ?= go
BIN     ?= bin
VERSION ?= $(shell git describe --tags --always --dirty 2>/dev/null || echo dev)
LDFLAGS := -s -w -X github.com/salehsayyadi/tuunel/internal/daemon.Version=$(VERSION)

.PHONY: all build test race vet fmt staticcheck check lab lab-extended soak release bench microbench clean

all: build

build:
	CGO_ENABLED=0 $(GO) build -trimpath -ldflags '$(LDFLAGS)' -o $(BIN)/tuunel ./cmd/tuunel
	CGO_ENABLED=0 $(GO) build -trimpath -ldflags '$(LDFLAGS)' -o $(BIN)/tunnelctl ./cmd/tunnelctl

test:
	$(GO) test ./...

race:
	$(GO) test -race ./...

vet:
	$(GO) vet ./...

fmt:
	@test -z "$$(gofmt -l .)" || (gofmt -l .; exit 1)

staticcheck:
	staticcheck ./...

check: fmt vet test race staticcheck

# Real-TUN two-namespace acceptance lab (root required).
lab: build
	sudo bash tests/lab/netns-acceptance.sh $(CURDIR)/$(BIN)

# Extended lab: every carrier, failover cycles, endpoints, shutdown, reverse, MTU matrix.
lab-extended: build
	sudo bash tests/lab/netns-extended.sh $(CURDIR)/$(BIN)

# Soak: SOAK_SECONDS of traffic with periodic carrier failure, leak sampling.
SOAK_SECONDS ?= 360
soak: build
	sudo bash tests/lab/netns-soak.sh $(CURDIR)/$(BIN) $(SOAK_SECONDS)

# Release archives + installer + SHA256SUMS in dist/.
VERSION ?= dev
release:
	bash scripts/release.sh $(VERSION)

bench: build
	sudo bash tests/lab/netns-bench.sh $(CURDIR)/$(BIN) 50

microbench:
	$(GO) test -run '^$$' -bench . -benchtime 2s ./tests/bench ./internal/session

clean:
	rm -rf $(BIN)
