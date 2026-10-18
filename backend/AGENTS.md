# AGENTS.md: Meko Local backend

Meko Local's Go backend: one binary, `meko-local`, that is both the CLI and the server, the way
yugabyted runs YugabyteDB. Its design is [docs/design.md](docs/design.md). The rules for the whole
repo are in the root [AGENTS.md](../AGENTS.md).

## Map

| Path | What it is |
|---|---|
| `cmd/meko-local/` | The entry point: hands the arguments to `internal/cli`. |
| `internal/cli/` | The commands: `start` (re-runs the binary as a detached `serve`), `stop`, `status`, `serve`, `version`. The data folder's `flock` says whether a server runs. |
| `internal/config/` | Settings: flags, then `MEKO_LOCAL_*`, then `config.toml`, then defaults (port 7778). |
| `internal/paths/` | The data folder's layout and permissions, and moving it off a network filesystem. |
| `internal/store/` | The only door to SQLite: one writer and a read pool, embedded append-only migrations, typed queries. |
| `internal/sqlitevec/` | cgo glue that compiles `third_party/sqlite-vec` in and registers it. |
| `internal/server/` | The HTTP server on 127.0.0.1: the `{data, error}` envelope, the Host, Origin and admin-token checks, `/healthz` and `/api/v1`. |
| `internal/auth/` | The admin token the CLI uses (API keys come with MCP). |
| `internal/supervisor/` | Child processes (the Node engine, later): process groups, health checks, backoff, stop. |
| `internal/logging/` | slog to a rotated JSON file and the terminal. |
| `internal/version/` | Version and commit, set with `-ldflags`. |
| `third_party/` | Vendored C code, never edited. |

## Commands

Run them in `backend/`:

```sh
export CC=/opt/rh/gcc-toolset-13/root/usr/bin/gcc   # RHEL 8 only; on macOS the Xcode command line tools are enough
make build                                          # bin/meko-local
make test                                           # go test -race
make lint                                           # go vet, gofmt, golangci-lint v2 (.golangci.yml)
```

It needs Go 1.25 (an older `go` downloads it on first use; `go.mod`'s `go` line is a hard minimum)
and a C compiler, because cgo is on; the Makefile sets it and the build tags. SQLite and sqlite-vec
are compiled into the binary, so the first build compiles SQLite and takes about a minute. CI runs build, lint
and test on Linux and macOS when `backend/` changes (`.github/workflows/backend.yml`).

## Rules

- Commits and PR titles are `chore(backend): ...`. Nothing here ships in AgentK Local's release
  yet, and any `feat`, `fix`, `perf` or `deps` commit on `main` cuts an AgentK release.
- Keep `docs/design.md` and this file current in the same commit as the code, and check the
  change against [REVIEW.md](REVIEW.md).
- Every database call goes through `internal/store`; nothing else imports the SQLite driver.
  Migrations are append-only: never edit one that has shipped.
- Tests use temporary data folders and free ports, never `~/.meko-local` or port 7778.
- `third_party/` is vendored code, never edited (sqlite-vec and SQLite's headers; see
  `third_party/sqlite-vec/README.md`). It moves to yugabyte forks once those exist.
- Lint is `.golangci.yml`: prod Meko's config with the changes listed at its top. Fix findings
  rather than adding to its exclusions; a `nolint` names the rule and says why.
