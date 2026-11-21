# Meko's Go backend. cgo is required: SQLite and sqlite-vec are compiled in, from the
# yugabyte/go-sqlite3 fork (go.mod's replace). FTS5 needs the sqlite_fts5 tag and sqlite-vec the
# sqlite_vec tag. On RHEL 8 set CC=/opt/rh/gcc-toolset-13/root/usr/bin/gcc.
# The context engine (context_engine/) and the web UI (web_ui/) are TypeScript and need Node 22.13 or
# newer and npm.

export CGO_ENABLED := 1
TAGS    := sqlite_fts5,sqlite_vec
VERSION ?= dev
COMMIT  ?= $(shell git rev-parse --short HEAD 2>/dev/null || echo unknown)
PKG     := github.com/yugabyte/meko/src/meko/internal/version
LDFLAGS := -X $(PKG).Version=$(VERSION) -X $(PKG).Commit=$(COMMIT)

CONTEXT_ENGINE_SRC := $(wildcard context_engine/src/*.ts) context_engine/tsconfig.json context_engine/tsconfig.build.json
WEB_UI_SRC := $(shell find web_ui/src web_ui/public -type f 2>/dev/null) web_ui/index.html web_ui/vite.config.ts \
	web_ui/tsconfig.json

.PHONY: build context_engine web_ui test bench fuzz lint fmt clean

# bin/meko-cli is meko under its other name (cmd/meko picks the commands by it).
build:
	go build -tags $(TAGS) -ldflags "$(LDFLAGS)" -o bin/meko ./cmd/meko
	ln -sf meko bin/meko-cli

# The context engine's dependencies, installed again only when the lockfile changes. onnxruntime-node's
# install script would download a CUDA provider on linux-x64 (and time out on CI runners); Meko runs
# on the CPU, so it's skipped.
context_engine/node_modules/.package-lock.json: context_engine/package.json context_engine/package-lock.json
	cd context_engine && ONNXRUNTIME_NODE_INSTALL=skip npm ci --no-audit --no-fund

context_engine/dist/main.js: context_engine/node_modules/.package-lock.json $(CONTEXT_ENGINE_SRC)
	cd context_engine && npm run build

context_engine: context_engine/dist/main.js

# The web UI meko serve serves at /, built into web_ui/dist.
web_ui/node_modules/.package-lock.json: web_ui/package.json web_ui/package-lock.json
	cd web_ui && npm ci --no-audit --no-fund

web_ui/dist/index.html: web_ui/node_modules/.package-lock.json $(WEB_UI_SRC)
	cd web_ui && npm run build

web_ui: web_ui/dist/index.html

# The Go tests start the built context engine, so it's built first. The web UI's tests run its pages in
# happy-dom against a faked API, so they need its dependencies, not a build.
test: context_engine web_ui/node_modules/.package-lock.json
	go test -race -tags $(TAGS) ./...
	cd context_engine && npm test
	cd web_ui && npm test

# The search budget at 200 MB of content (a few minutes; not in CI).
bench: context_engine
	go test -tags $(TAGS),bench -run 'TestSearchBudget|TestKnowledgeBudget' -v -timeout 90m ./internal/memory/

# Markdown's heading finder: windowed parsing against whole-file parsing, for 5 minutes (not in CI;
# go test runs its seeds). An input it fails on is saved under internal/knowledge/testdata/fuzz.
fuzz:
	go test -run '^$$' -fuzz FuzzMarkdownHeadings -fuzztime 5m ./internal/knowledge/

# golangci-lint v2 (https://golangci-lint.run) must be installed; CI pins its version.
lint: context_engine/node_modules/.package-lock.json web_ui/node_modules/.package-lock.json
	go vet -tags $(TAGS) ./...
	@test -z "$$(gofmt -l .)" || { gofmt -l .; echo "run make fmt"; exit 1; }
	golangci-lint run
	cd context_engine && npm run typecheck
	cd web_ui && npm run check

fmt:
	gofmt -w .

clean:
	rm -rf bin context_engine/dist web_ui/dist
