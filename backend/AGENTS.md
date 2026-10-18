# AGENTS.md: Meko Local backend

Meko Local's Go backend: one binary, `meko-local`, that is both the CLI and the server, the way
yugabyted runs YugabyteDB. Its design is [docs/design.md](docs/design.md). The rules for the whole
repo are in the root [AGENTS.md](../AGENTS.md).

## Commands

Run them in `backend/`:

```sh
export CC=/opt/rh/gcc-toolset-13/root/usr/bin/gcc   # RHEL 8 only; on macOS the Xcode command line tools are enough
make build                                          # bin/meko-local
make test                                           # go test -race
make lint                                           # go vet, gofmt, golangci-lint v2 (.golangci.yml)
```

It needs Go 1.25 (an older `go` downloads it on first use) and a C compiler: cgo is on, because
SQLite and sqlite-vec are compiled into the binary, and FTS5 needs the `sqlite_fts5` build tag. The
Makefile sets both. The first build compiles SQLite and takes about a minute. CI runs build, lint
and test on Linux and macOS when `backend/` changes (`.github/workflows/backend.yml`).

## Rules

- Commits and PR titles are `chore(backend): ...`. Nothing here ships in AgentK Local's release
  yet, and any `feat`, `fix`, `perf` or `deps` commit on `main` cuts an AgentK release.
- Keep `docs/design.md` and this file current in the same commit as the code.
- Lint is `.golangci.yml`: prod Meko's config with the changes listed at its top. Fix findings
  rather than adding to its exclusions; a `nolint` names the rule and says why.
