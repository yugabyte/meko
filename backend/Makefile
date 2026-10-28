# Meko's Go backend. cgo is required: SQLite and sqlite-vec are compiled in, from the
# yugabyte/go-sqlite3 fork (go.mod's replace). FTS5 needs the sqlite_fts5 tag and sqlite-vec the
# sqlite_vec tag. On RHEL 8 set CC=/opt/rh/gcc-toolset-13/root/usr/bin/gcc.
# The memory engine (memory_engine/) and the web UI (web_ui/) are TypeScript and need Node 22.13 or
# newer and npm.

export CGO_ENABLED := 1
TAGS    := sqlite_fts5,sqlite_vec
VERSION ?= dev
COMMIT  ?= $(shell git rev-parse --short HEAD 2>/dev/null || echo unknown)
PKG     := github.com/yugabyte/meko/backend/internal/version
LDFLAGS := -X $(PKG).Version=$(VERSION) -X $(PKG).Commit=$(COMMIT)

MEMORY_ENGINE_SRC := $(wildcard memory_engine/src/*.ts) memory_engine/tsconfig.json memory_engine/tsconfig.build.json
WEB_UI_SRC := $(shell find web_ui/src web_ui/public -type f 2>/dev/null) web_ui/index.html web_ui/vite.config.ts \
	web_ui/tsconfig.json

.PHONY: build memory_engine web_ui test bench lint fmt clean

# bin/mekosh is meko under its other name (cmd/meko picks the commands by it).
build:
	go build -tags $(TAGS) -ldflags "$(LDFLAGS)" -o bin/meko ./cmd/meko
	ln -sf meko bin/mekosh

# The memory engine's dependencies, installed again only when the lockfile changes. onnxruntime-node's
# install script would download a CUDA provider on linux-x64 (and time out on CI runners); Meko runs
# on the CPU, so it's skipped.
memory_engine/node_modules/.package-lock.json: memory_engine/package.json memory_engine/package-lock.json
	cd memory_engine && ONNXRUNTIME_NODE_INSTALL=skip npm ci --no-audit --no-fund

memory_engine/dist/main.js: memory_engine/node_modules/.package-lock.json $(MEMORY_ENGINE_SRC)
	cd memory_engine && npm run build

memory_engine: memory_engine/dist/main.js

# The web UI meko serve serves at /, built into web_ui/dist.
web_ui/node_modules/.package-lock.json: web_ui/package.json web_ui/package-lock.json
	cd web_ui && npm ci --no-audit --no-fund

web_ui/dist/index.html: web_ui/node_modules/.package-lock.json $(WEB_UI_SRC)
	cd web_ui && npm run build

web_ui: web_ui/dist/index.html

# The Go tests start the built memory engine, so it's built first. The web UI's tests run its pages in
# happy-dom against a faked API, so they need its dependencies, not a build.
test: memory_engine web_ui/node_modules/.package-lock.json
	go test -race -tags $(TAGS) ./...
	cd memory_engine && npm test
	cd web_ui && npm test

# The search budget at 200 MB of content (a few minutes; not in CI).
bench: memory_engine
	go test -tags $(TAGS),bench -run 'TestSearchBudget|TestKnowledgeBudget' -v -timeout 90m ./internal/memory/

# golangci-lint v2 (https://golangci-lint.run) must be installed; CI pins its version.
lint: memory_engine/node_modules/.package-lock.json web_ui/node_modules/.package-lock.json
	go vet -tags $(TAGS) ./...
	@test -z "$$(gofmt -l .)" || { gofmt -l .; echo "run make fmt"; exit 1; }
	golangci-lint run
	cd memory_engine && npm run typecheck
	cd web_ui && npm run check

fmt:
	gofmt -w .

clean:
	rm -rf bin memory_engine/dist web_ui/dist
