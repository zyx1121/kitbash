BINARIES := kitbash-mcp kitbashd
VERSION ?= $(shell git describe --tags --always 2>/dev/null || echo dev)
LDFLAGS := -s -w -X main.version=$(VERSION)

.PHONY: all build test test-race lint build-linux clean

all: lint test build

## build: static binaries for this host
build:
	@for b in $(BINARIES); do \
		CGO_ENABLED=0 go build -trimpath -ldflags "$(LDFLAGS)" -o bin/$$b ./cmd/$$b || exit 1; \
	done

## test: every package
test:
	go test ./...

## test-race: the concurrent packages under the race detector (needs cgo)
test-race:
	CGO_ENABLED=1 go test -race -count=1 ./internal/daemon/... ./internal/store/... ./internal/sysusers/... ./internal/otlp/... ./internal/telemetry/... ./internal/bridge/... ./internal/proc/...

## lint: formatting and correctness
lint:
	@unformatted=$$(gofmt -l .); \
	if [ -n "$$unformatted" ]; then \
		echo "gofmt needed:"; echo "$$unformatted"; exit 1; \
	fi
	go vet ./...

## build-linux: the binaries kitbash hosts run
build-linux:
	@for b in $(BINARIES); do \
		CGO_ENABLED=0 GOOS=linux GOARCH=amd64 go build -trimpath -ldflags "$(LDFLAGS)" -o bin/$$b-linux-amd64 ./cmd/$$b || exit 1; \
	done

clean:
	rm -rf bin
