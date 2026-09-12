BINARIES := kitbash-mcp kitbashd
# The one version source is pkgver in packaging/apk/APKBUILD (PLAN.md 4.8).
PKGVER := $(shell sed -n 's/^pkgver=//p' packaging/apk/APKBUILD)
VERSION := v$(PKGVER)
LDFLAGS := -s -w -X main.version=$(VERSION)

.PHONY: all build test test-race lint build-linux check-version bump clean

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

## check-version: every version mention in the repo equals pkgver
check-version:
	sh packaging/release/check.sh

## bump: set a new version everywhere and commit the release commit, e.g. make bump VERSION=0.2.0
bump:
	sh packaging/release/bump.sh $(VERSION)

clean:
	rm -rf bin
