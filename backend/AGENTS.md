# AGENTS.md: Meko backend

Meko's Go backend: one binary, `meko`, that is both the CLI and the server, the way
yugabyted runs YugabyteDB, and its Node memory engine. Its design is [docs/design.md](docs/design.md)
(the foundation), [docs/memory-engine.md](docs/memory-engine.md) (the memory engine and
conversations), [docs/api.md](docs/api.md) (MCP and REST), [docs/knowledge.md](docs/knowledge.md)
(knowledge files), [docs/install.md](docs/install.md) (release builds, releases, the installer,
the service, extensions, updates and secrets) and [docs/web-ui.md](docs/web-ui.md) (the web UI). The rules for the whole repo are in the root [AGENTS.md](../AGENTS.md).

## Map

| Path | What it is |
|---|---|
| `cmd/meko/` | The entry point: hands the arguments to `internal/cli`. |
| `internal/cli/` | The commands: `start`, `stop` and `restart` (through the login service once installed, else a detached `serve`; `start` and `stop` also run the extensions'), `status`, `logs`, `serve` (which also checks for updates and rolls back a failing one), `version`, `token`, `ui` (opens the web UI with the client token), `destroy` (stops the server and removes the data folder), `install`, `uninstall`, `install_extension`, `uninstall_extension`, `update`, and `search`, `add_memory`, `datapacks`, `create_datapack`, `delete_datapack`, `upload` and `upload_status` (MCP calls to the running server). The data folder's `flock` says whether a server runs. |
| `internal/config/` | Settings: flags, then `MEKO_*`, then `config.toml`, then defaults (port 7778). |
| `internal/paths/` | The data folder's layout and permissions, and moving it off a network filesystem. |
| `internal/store/` | The only door to SQLite: one writer and a read pool, embedded append-only migrations, typed queries. |
| `internal/server/` | The HTTP server on 127.0.0.1: the Host and Origin checks, the per-route credential check (admin token, client token, or none with `[server] auth = "none"`; the knowledge-file links' ticket), the Origin check that lets in only the web UI's own pages, the web UI's route, `/healthz` and `/api/v1` (status includes the memory engine's and the auth mode). |
| `internal/mcp/` | MCP at `/mcp`: Meko Cloud's tools on the official MCP Go SDK (stateless, JSON replies, raw arguments read loosely), recording each call. |
| `internal/rest/` | The `/api/v1` routes meko_ui calls: the local user, datapacks and metrics, artifacts, memories and learnings, conversations and their threads, knowledge files, and the upload and download links. |
| `internal/knowledge/` | Knowledge files before the database: the types Meko takes and the check of a file's first bytes, text extraction (PDF, HTML, Markdown, CSV, text), chunking, and the `knowledgebase-files/` folder their bytes live in. No queries, no memory engine. |
| `internal/webui/` | Finding the built web UI and serving it: its files, `index.html` for the app's own routes, and the headers that keep other sites from framing it. |
| `internal/wire/` | Meko Cloud's reply shapes, which MCP and REST both build their replies with. |
| `internal/testenv/` | A whole server for tests: store, store API, the memory engine with the hashing embedder, the service and the HTTP server. |
| `internal/envelope/` | The `{data, error}` reply every API uses, and the error handler that keeps internal errors generic. |
| `internal/storeapi/` | The store API the memory engine calls on `run/store.sock`: mem0's VectorStore and HistoryManager over `internal/store`, and the memory engine's hello. Internal token only. |
| `internal/memoryengine/` | Finding Node and the built memory engine, its child spec, and the Go client of its API on `run/mem_eng.sock`. |
| `internal/memory/` | The service MCP and REST call: memories (through the memory engine), learnings (keep and promote), artifacts, conversations, conversation search, knowledge files (upload and download links, the indexer, hybrid search with the reranker), the sweep that embeds turns and knowledge chunks, the call log (recording, pruning, and the stats metrics read), and the local user's profile. |
| `assets/` | The Meko logo, dark and white, as Meko Cloud's web app has it; the repo's README shows it. |
| `memory_engine/` | The memory engine: TypeScript on Node 22, public `mem0ai` with Meko's providers (store, history, a transformers.js embedder in a worker thread, a refusing LLM), the knowledge base's cross-encoder in a worker thread of its own, its API, its tests. |
| `web_ui/` | The web UI: React, Vite and Tailwind, built into `web_ui/dist`, which `serve` serves at `/`. Its pages call only `/api/v1` and `/mcp`. |
| `internal/auth/` | The admin token the CLI uses (new at each start), and the client token MCP and REST clients send (`client.token`, kept across restarts). |
| `internal/service/` | The login service: a launchd LaunchAgent or a systemd user unit from embedded templates, run with launchctl or systemctl. |
| `internal/secrets/` | The secret store: the login Keychain through `/usr/bin/security` (macOS), or 0600 files in the data folder. |
| `internal/extensions/` | `extensions.json`: the extensions `meko start` and `stop` run, and the ones `install_extension` knows (agentk). |
| `internal/update/` | Release installs (`~/.local/share/meko/app/<version>`, `app/current`, outside the data folder), GitHub releases, staging and checking an update, probation and rollback. |
| `internal/supervisor/` | Child processes (the memory engine): process groups, health checks, backoff, stop, and the pid records (`run/children/`) that let `stop` kill a hung server's children. |
| `internal/logging/` | slog to a rotated JSON file and the terminal. |
| `internal/version/` | Version and commit, set with `-ldflags`. |
| `scripts/` | `install.sh` (Meko's installer, published with each release), `package-release.sh` and `smoke-release.sh` (the release build and its check), and their tests. |
| `memory_engine/scripts/fetch-model.mjs` | Downloads the pinned models (the embedder and the reranker) for a release to bundle, and checks they run offline. |

## Commands

Run them in `backend/`:

```sh
export CC=/opt/rh/gcc-toolset-13/root/usr/bin/gcc   # RHEL 8 only; on macOS the Xcode command line tools are enough
make build                                          # bin/meko
make memory_engine                                  # npm ci (when the lockfile changes) and build memory_engine/dist
make web_ui                                         # the same for the web UI, into web_ui/dist
make test                                           # builds the memory engine, then go test -race and the memory engine's tests
make lint                                           # go vet, gofmt, golangci-lint v2 (.golangci.yml), tsc on the memory engine and the web UI
make bench                                          # the search budget at 200 MB (minutes; not in CI)
go test ./scripts/                                  # the installer, against a fake gh
```

It needs Go 1.26 (an older `go` downloads it on first use; `go.mod`'s `go` line is a hard minimum,
and a dependency bump can raise it, as golang.org/x/sys 0.49 did in #212)
and a C compiler, because cgo is on; the Makefile sets it and the build tags. The memory engine needs
Node 22.13+ and npm. The first memory engine test run downloads the embedding and reranking models (23 MB
each) into
`memory_engine/.models` (git-ignored; `MEKO_TEST_MODELS` moves it). SQLite and sqlite-vec
are compiled into the binary from the `yugabyte/go-sqlite3` fork (`go.mod`'s `replace`), with the
`sqlite_fts5` and `sqlite_vec` tags the Makefile and `.golangci.yml` set; the first build compiles
SQLite and takes about a minute. CI runs build, lint
and test on Linux and macOS when a PR changes `backend/` (the `backend` job in
`.github/workflows/ci.yml`; `scripts/ci-scope.sh` decides), and the required `CI gate` fails when any of them fails.

## Rules

- Commits and PR titles name the `meko` component (`fix(meko/cli): ...`). Meko releases on its
  own train (`.github/workflows/meko-release.yml`, tags `meko-v<version>`): a `feat`, `fix`,
  `perf` or `deps` commit with component `meko` on `main` cuts a Meko release, and never an AgentK
  one. Use `chore` for what doesn't change what ships ([install.md](docs/install.md)).
- Keep `docs/design.md` and this file current in the same commit as the code, and the repo
  root's `README.md` too when the commands, their flags or the build requirements change. Check
  the change against [REVIEW.md](REVIEW.md).
- Every database call goes through `internal/store`, the only package that imports the SQLite
  driver. The memory engine has no database: it stores through the store API. Migrations are append-only: never
  edit one that has shipped.
- Memory engine and web UI dependencies are pinned exactly in their `package.json`; `memory_engine/.npmrc` keeps npm from
  installing mem0ai's peers. Prod Meko's code is private: copy none of it and cite none of its
  paths (state the behavior instead).
- Dependabot (the repo root's `.github/dependabot.yml`) bumps the Go modules (`deps(meko): ...`)
  and the memory engine's npm packages (`deps(meko/memory-engine): ...`, or
  `chore(meko/memory-engine): ...` for dev-only ones) every Monday, with minor and patch
  bumps grouped and `mem0ai` in a PR of its own. `mattn/go-sqlite3` is left out: it's replaced by the
  fork, so upgrade the fork's tag by hand.
- Neither log ever holds memory, turn or knowledge-file text: only ids, counts and timings. The
  memory engine never logs an error's message.
- Knowledge files' bytes live in `knowledgebase-files/`, only through `internal/knowledge`'s
  `Files`; a file and its `kb_documents` row change together under the service's `fileMu`.
- Tests use temporary data folders (short `/tmp` paths: unix sockets have a length limit) and free
  ports, never `~/.meko` or port 7778. They never touch the user's own service, Keychain or
  extensions: the CLI's tests set `HOME`, `MEKO_SECRETS=file`, a fake service manager and a PATH
  without the machine's `agentk` (or `agent-k-local`, its old name). Tests that need no model start the memory engine with the
  hashing embedder (`memoryengine.Options.TestEmbedder`; the CLI's tests set it through their own
  `MEKO_TEST_EMBEDDER`, which only the test binary reads).
- No third-party C code is copied here. SQLite and sqlite-vec come from a tag of the
  `yugabyte/go-sqlite3` fork (its `README.yugabyte.md` says how to sync sqlite-vec from
  `yugabyte/sqlite-vec` or rebase on mattn). To upgrade, cut a new `-yb.<n>` tag there (never move
  one), bump the `replace` in `go.mod`, and update the versions `TestLibVersions` expects.
- Lint is `.golangci.yml`: prod Meko's config with the changes listed at its top. Fix findings
  rather than adding to its exclusions; a `nolint` names the rule and says why.
- The lines the CLI and `scripts/install.sh` print end without a period, like Go's error strings,
  so a version, path or command at the end isn't read with one (`Installing meko 0.20261022.1`).
  Periods between sentences stay. `TestOutputLinesEndWithoutPeriod` checks every line of the
  strings `internal/cli` prints or formats with `fmt` and of the strings its functions return.
