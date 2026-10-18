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

It needs Go 1.25 (an older `go` downloads it on first use; `go.mod`'s `go` line is a hard minimum)
and a C compiler, because cgo is on; the Makefile sets it and the build tags. CI runs build, lint
and test on Linux and macOS when `backend/` changes (`.github/workflows/backend.yml`).

## Rules

- Commits and PR titles are `chore(backend): ...`. Nothing here ships in AgentK Local's release
  yet, and any `feat`, `fix`, `perf` or `deps` commit on `main` cuts an AgentK release.
- Keep `docs/design.md` and this file current in the same commit as the code.
- `third_party/` is vendored code, never edited (sqlite-vec and SQLite's headers; see
  `third_party/sqlite-vec/README.md`). It moves to yugabyte forks once those exist.
- Lint is `.golangci.yml`: prod Meko's config with the changes listed at its top. Fix findings
  rather than adding to its exclusions; a `nolint` names the rule and says why.
