# Meko Local's Go backend. cgo is required: sqlite-vec and SQLite are compiled in, and FTS5
# needs the sqlite_fts5 tag. On RHEL 8 set CC=/opt/rh/gcc-toolset-13/root/usr/bin/gcc.

export CGO_ENABLED := 1
TAGS    := sqlite_fts5
VERSION ?= dev
COMMIT  ?= $(shell git rev-parse --short HEAD 2>/dev/null || echo unknown)
PKG     := github.com/yugabyte/meko-local/backend/internal/version
LDFLAGS := -X $(PKG).Version=$(VERSION) -X $(PKG).Commit=$(COMMIT)

.PHONY: build test lint fmt clean

build:
	go build -tags $(TAGS) -ldflags "$(LDFLAGS)" -o bin/meko-local ./cmd/meko-local

test:
	go test -race -tags $(TAGS) ./...

# golangci-lint v2 (https://golangci-lint.run) must be installed; CI pins its version.
lint:
	go vet -tags $(TAGS) ./...
	@test -z "$$(gofmt -l .)" || { gofmt -l .; echo "run make fmt"; exit 1; }
	golangci-lint run

fmt:
	gofmt -w .

clean:
	rm -rf bin
