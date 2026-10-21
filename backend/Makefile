# Meko Local's Go backend. cgo is required: sqlite-vec and SQLite are compiled in, and FTS5
# needs the sqlite_fts5 tag. On RHEL 8 set CC=/opt/rh/gcc-toolset-13/root/usr/bin/gcc.
# The memory engine (engine/) is TypeScript and needs Node 22.13 or newer and npm.

export CGO_ENABLED := 1
TAGS    := sqlite_fts5
VERSION ?= dev
COMMIT  ?= $(shell git rev-parse --short HEAD 2>/dev/null || echo unknown)
PKG     := github.com/yugabyte/meko-local/backend/internal/version
LDFLAGS := -X $(PKG).Version=$(VERSION) -X $(PKG).Commit=$(COMMIT)

ENGINE_SRC := $(wildcard engine/src/*.ts) engine/tsconfig.json engine/tsconfig.build.json

.PHONY: build engine test lint fmt clean

build:
	go build -tags $(TAGS) -ldflags "$(LDFLAGS)" -o bin/meko-local ./cmd/meko-local

# The engine's dependencies, installed again only when the lockfile changes.
engine/node_modules/.package-lock.json: engine/package.json engine/package-lock.json
	cd engine && npm ci --no-audit --no-fund

engine/dist/main.js: engine/node_modules/.package-lock.json $(ENGINE_SRC)
	cd engine && npm run build

engine: engine/dist/main.js

# The engine is built first, then both suites run.
test: engine
	go test -race -tags $(TAGS) ./...
	cd engine && npm test

# golangci-lint v2 (https://golangci-lint.run) must be installed; CI pins its version.
lint: engine/node_modules/.package-lock.json
	go vet -tags $(TAGS) ./...
	@test -z "$$(gofmt -l .)" || { gofmt -l .; echo "run make fmt"; exit 1; }
	golangci-lint run
	cd engine && npm run typecheck

fmt:
	gofmt -w .

clean:
	rm -rf bin engine/dist
