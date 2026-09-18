# SAN Network — common development and deployment tasks.
#
#   make build     compile sannode/sanup/sancli/tools into bin/ (memory backend)
#   make lmdb      compile them with the cgo LMDB backend (-tags lmdb)
#   make test      gofmt check, go vet, go test ./... -count=1
#   make cross     cross-compile every package for linux/amd64 and linux/arm64
#   make e2e       run the 3-node end-to-end check (cmd/sane2e)
#   make soak      run the short CI soak (cmd/sansoak --short)
#   make bench     run the fast benchmark suite (cmd/sanbench)
#   make run       build and start a local node with sanup
#   make stop      stop the node started by `make run`
#   make install   install as a systemd service (delegates to deploy/install.sh)
#   make docker    build the LMDB container image (deploy/Dockerfile)
#   make clean     remove bin/

GO      ?= go
PREFIX  ?= /usr/local
BIN     := bin
GOFLAGS ?=
LDFLAGS := -s -w

.PHONY: help build lmdb test fmt vet cross e2e soak bench run stop install docker clean

help:
	@sed -n '2,14p' Makefile

build:
	@mkdir -p $(BIN)
	$(GO) build $(GOFLAGS) -trimpath -ldflags="$(LDFLAGS)" -o $(BIN)/sannode ./cmd/sannode
	$(GO) build $(GOFLAGS) -trimpath -ldflags="$(LDFLAGS)" -o $(BIN)/sanup ./cmd/sanup
	$(GO) build $(GOFLAGS) -trimpath -ldflags="$(LDFLAGS)" -o $(BIN)/sancli ./cmd/sancli
	$(GO) build $(GOFLAGS) -trimpath -ldflags="$(LDFLAGS)" -o $(BIN)/sane2e ./cmd/sane2e
	$(GO) build $(GOFLAGS) -trimpath -ldflags="$(LDFLAGS)" -o $(BIN)/sangenesis ./cmd/sangenesis
	$(GO) build $(GOFLAGS) -trimpath -ldflags="$(LDFLAGS)" -o $(BIN)/sansoak ./cmd/sansoak
	$(GO) build $(GOFLAGS) -trimpath -ldflags="$(LDFLAGS)" -o $(BIN)/sanbench ./cmd/sanbench

lmdb:
	@mkdir -p $(BIN)
	CGO_ENABLED=1 $(GO) build $(GOFLAGS) -tags lmdb -trimpath -ldflags="$(LDFLAGS)" -o $(BIN)/sannode ./cmd/sannode
	CGO_ENABLED=1 $(GO) build $(GOFLAGS) -tags lmdb -trimpath -ldflags="$(LDFLAGS)" -o $(BIN)/sanup ./cmd/sanup
	CGO_ENABLED=1 $(GO) build $(GOFLAGS) -tags lmdb -trimpath -ldflags="$(LDFLAGS)" -o $(BIN)/sancli ./cmd/sancli

fmt:
	@unformatted="$$(gofmt -l .)"; \
	if [ -n "$$unformatted" ]; then echo "not gofmt-clean:"; echo "$$unformatted"; exit 1; fi

vet:
	$(GO) vet ./...

test: fmt vet
	$(GO) test ./... -count=1
	@if command -v gcc >/dev/null 2>&1; then \
		CGO_ENABLED=1 $(GO) test -tags lmdb ./internal/ledger/store/ -count=1; \
	else \
		echo "gcc not found; skipping the LMDB store test"; \
	fi

cross:
	GOOS=linux GOARCH=amd64 $(GO) build ./...
	GOOS=linux GOARCH=arm64 $(GO) build ./...

e2e:
	$(GO) run ./cmd/sane2e

soak:
	$(GO) run ./cmd/sansoak --short

bench:
	$(GO) run ./cmd/sanbench

run: build
	$(BIN)/sanup --host 0.0.0.0 --api-host 0.0.0.0

stop: build
	$(BIN)/sanup --stop

install:
	sudo PREFIX=$(PREFIX) ./deploy/install.sh

docker:
	docker build -f deploy/Dockerfile -t san-network-go:local .

clean:
	rm -rf $(BIN)
