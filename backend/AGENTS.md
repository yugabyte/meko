# AGENTS.md: Meko backend

Meko's Go backend: one binary, `meko`, that is both the CLI and the server, the way
yugabyted runs YugabyteDB, and its Node memory engine. Its design is [docs/design.md](docs/design.md)
(the foundation), [docs/memory-engine.md](docs/memory-engine.md) (the memory engine and
conversations) and [docs/api.md](docs/api.md) (MCP and REST). The rules for the whole repo are in the root [AGENTS.md](../AGENTS.md).

## Map

| Path | What it is |
|---|---|
| `cmd/meko/` | The entry point: hands the arguments to `internal/cli`. |
| `internal/cli/` | The commands: `start` (re-runs the binary as a detached `serve`), `stop`, `status`, `serve`, `version`, `token`, and `search`, `add` and `datapacks` (MCP calls to the running server). The data folder's `flock` says whether a server runs. |
| `internal/config/` | Settings: flags, then `MEKO_*`, then `config.toml`, then defaults (port 7778). |
| `internal/paths/` | The data folder's layout and permissions, and moving it off a network filesystem. |
| `internal/store/` | The only door to SQLite: one writer and a read pool, embedded append-only migrations, typed queries. |
| `internal/sqlitevec/` | cgo glue that compiles `third_party/sqlite-vec` in and registers it. |
| `internal/server/` | The HTTP server on 127.0.0.1: the Host and Origin checks, the per-route credential check (admin token, client token, or none with `[server] auth = "none"`), `/healthz` and `/api/v1` (status includes the engine's and the auth mode). |
| `internal/mcp/` | MCP at `/mcp`: Meko Cloud's tools on the official MCP Go SDK (stateless, JSON replies, raw arguments read loosely), recording each call. |
| `internal/wire/` | Meko Cloud's reply shapes, which MCP and REST both build their replies with. |
| `internal/testenv/` | A whole server for tests: store, store API, the engine with the hashing embedder, the service and the HTTP server. |
| `internal/envelope/` | The `{data, error}` reply every API uses, and the error handler that keeps internal errors generic. |
| `internal/storeapi/` | The store API the engine calls on `run/store.sock`: mem0's VectorStore and HistoryManager over `internal/store`, and the engine's hello. Internal token only. |
| `internal/engine/` | Finding Node and the built engine, the engine's child spec, and the Go client of its API on `run/engine.sock`. |
| `internal/memory/` | The service MCP and REST call: memories (through the engine), learnings (keep and promote), artifacts, conversations, conversation search, the sweep that embeds turns, and the call log (recording and pruning). |
| `engine/` | The memory engine: TypeScript on Node 22, public `mem0ai` with Meko's providers (store, history, a transformers.js embedder in a worker thread, a refusing LLM), its API, its tests. |
| `internal/auth/` | The admin token the CLI uses (new at each start), and the client token MCP and REST clients send (`client.token`, kept across restarts). |
| `internal/supervisor/` | Child processes (the engine): process groups, health checks, backoff, stop, and the pid records (`run/children/`) that let `stop` kill a hung server's children. |
| `internal/logging/` | slog to a rotated JSON file and the terminal. |
| `internal/version/` | Version and commit, set with `-ldflags`. |
| `third_party/` | Vendored C code, never edited. |

## Commands

Run them in `backend/`:

```sh
export CC=/opt/rh/gcc-toolset-13/root/usr/bin/gcc   # RHEL 8 only; on macOS the Xcode command line tools are enough
make build                                          # bin/meko
make engine                                         # npm ci (when the lockfile changes) and build engine/dist
make test                                           # builds the engine, then go test -race and the engine's tests
make lint                                           # go vet, gofmt, golangci-lint v2 (.golangci.yml), tsc on the engine
make bench                                          # the search budget at 200 MB (minutes; not in CI)
```

It needs Go 1.25 (an older `go` downloads it on first use; `go.mod`'s `go` line is a hard minimum)
and a C compiler, because cgo is on; the Makefile sets it and the build tags. The engine needs
Node 22.13+ and npm. The first engine test run downloads the embedding model (23 MB) into
`engine/.models` (git-ignored; `MEKO_TEST_MODELS` moves it). SQLite and sqlite-vec
are compiled into the binary, so the first build compiles SQLite and takes about a minute. CI runs build, lint
and test on Linux and macOS when a PR changes `backend/` (the `backend` job in
`.github/workflows/ci.yml`; `scripts/ci-scope.sh` decides), and the required `CI gate` fails when any of them fails.

## Rules

- Commits and PR titles are `chore(meko): ...` (or `chore(meko/<area>): ...`). Nothing here
  ships in AgentK Local's release yet, and any `feat`, `fix`, `perf` or `deps` commit on `main`
  cuts an AgentK release.
- Keep `docs/design.md` and this file current in the same commit as the code, and check the
  change against [REVIEW.md](REVIEW.md).
- Every database call goes through `internal/store`. The only other package that imports the
  SQLite driver is `internal/sqlitevec`, which links SQLite's C code; only its test runs a query.
  The engine has no database: it stores through the store API. Migrations are append-only: never
  edit one that has shipped.
- Engine dependencies are pinned exactly in `engine/package.json`; `engine/.npmrc` keeps npm from
  installing mem0ai's peers. Prod Meko's code is private: copy none of it and cite none of its
  paths (state the behavior instead).
- Neither log ever holds memory or turn text: only ids, counts and timings. The engine never logs
  an error's message.
- Tests use temporary data folders (short `/tmp` paths: unix sockets have a length limit) and free
  ports, never `~/.meko` or port 7778. Tests that need no model start the engine with the
  hashing embedder (`engine.Options.TestEmbedder`; the CLI's tests set it through their own
  `MEKO_TEST_EMBEDDER`, which only the test binary reads).
- `third_party/` is vendored code, never edited (sqlite-vec and SQLite's headers; see
  `third_party/sqlite-vec/README.md`). It moves to yugabyte forks once those exist.
- Lint is `.golangci.yml`: prod Meko's config with the changes listed at its top. Fix findings
  rather than adding to its exclusions; a `nolint` names the rule and says why.
