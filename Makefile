GO      ?= go
BIN     ?= bin
VERSION ?= $(shell git describe --tags --always --dirty 2>/dev/null || echo dev)
LDFLAGS := -s -w -X github.com/salehsayyadi/tuunel/internal/daemon.Version=$(VERSION)

.PHONY: all build test race vet fmt staticcheck check lab bench microbench clean

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

bench: build
	sudo bash tests/lab/netns-bench.sh $(CURDIR)/$(BIN) 50

microbench:
	$(GO) test -run '^$$' -bench . -benchtime 2s ./tests/bench ./internal/session

clean:
	rm -rf $(BIN)
