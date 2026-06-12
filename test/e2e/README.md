# End-to-end test suite

A Rust harness (`e2e_tests` crate) that brings up a real `yb_light_local`
cluster against a real YugabyteDB and exercises it with scenario tests
and golden `.sql` workloads, scanning the pageserver log for corruption
markers after every test. This is the verification gate the
`e2e-before-corruption-claim` skill points at.

## Running

```sh
test/e2e/run.sh                 # fast suite (default)
test/e2e/run.sh fast            # explicit
test/e2e/run.sh slow            # slow suite only
test/e2e/run.sh all             # fast then slow (one shared cluster)
test/e2e/run.sh smoke           # one test by name (substring also matches)
test/e2e/run.sh -n 20 restart_wal_replay_yb   # repeat 20x, cumulative pass/fail
test/e2e/run.sh --release all   # build + run in release mode
test/e2e/run.sh -h              # full help
```

`run.sh` is a thin wrapper: it `cargo build`s the harness +
`yb_light_local` (incremental — a no-op when nothing changed), then
execs the harness. Default builds are debug; `--release` builds and runs
the release binaries.

- **`-n <count>`** repeats the selection and prints a cumulative
  `passed / failed` count. It runs *all* selected tests each iteration
  (failures are tallied, not fatal) — for hunting flakiness. Without
  `-n` the run is fail-fast: the first failure aborts.
- By default both passing and failing runs tear the cluster + DB down but
  keep their `*.log` files under `.e2e-tests/run_<id>/`; the run dir is
  GC'd after 7 days. Pass `--no-fail-clean` to leave a *failed* run's
  cluster + DB up for postmortem (it prints how to tear it down).

### Prerequisites

- A YugabyteDB reachable at `127.0.0.1:5433` (override with
  `YB_HOST`/`YB_PORT`).
- `psql` on `PATH` (used for the `.sql` workloads and the background
  write/read loops).
- `/tmp/neon-init.toml` — the base init config the per-run config is
  derived from (the ephemeral DB name is swapped in).

## How it works

- **One shared cluster.** A single `yb_light_local` cluster + tenant +
  `main` endpoint is set up once; all tests share it. Between tests the
  cluster is recycled — any component the prior test left down is
  restarted and the compute endpoint is recreated — so each test starts
  from a clean compute state. The tenant + timeline persist, so **tests
  must use unique table names**.
- **Ephemeral DB.** Each run uses its own `yb_storage_e2e_*` database,
  dropped on success.
- **Corruption gate.** After every test the new tail of the pageserver
  log is scanned for corruption markers (`MATERIALIZER REDO ERROR`,
  `WRITE-PATH ROUNDTRIP`, …); the first hit aborts the run. The final
  verdict also requires the `pageserver_yb_correctness_errors_total`
  metric to have no exported series.
- **Sequential by design.** The shared cluster means tests run in a
  fixed order, not in parallel — which is why this is a driver binary,
  not a `cargo test` target.

## Adding a test

Test name = source file stem; the subdirectory/category is fast vs slow.
Nothing central to edit — tests are auto-discovered.

- **A scenario** (typed assertions): drop
  `src/scenarios/fast/<name>.rs` (or `slow/`) with:

  ```rust
  use crate::cluster::{Cluster, step};
  use crate::db;

  pub async fn run(c: &Cluster) -> anyhow::Result<()> {
      step("do the thing");
      let n = db::query_i64(&db::compute_conn(c.compute_port()),
                            "SELECT count(*) FROM …").await?;
      anyhow::ensure!(n == 42, "got {n}, want 42");
      Ok(())
  }
  ```

  The build script (`build.rs`) auto-registers it; no list to edit.
  Drive cluster operations with `c.yb(&["endpoint", "start", …])`; make
  assertions with the typed `db` helpers (or `psql` for background load
  loops). Don't print a `PASS` line — the runner does that.

- **A golden `.sql` workload**: drop `<name>.sql` + `<name>.out` into
  `fast/` or `slow/`. The runner execs `psql -f`, normalizes stdout, and
  diffs against the `.out` golden. Generate/refresh the golden with
  `E2E_REGEN_EXPECTED=1 test/e2e/run.sh <name>` and review the diff
  before committing. `.sql` workloads are discovered at runtime — no
  recompile needed to add one.

## Layout

```
test/e2e/
  run.sh                  wrapper: build + exec the harness
  clean.sh                teardown (kill procs, drop DB, wipe run dir)
  build.rs                auto-registers scenarios from the subdirs
  src/
    main.rs               CLI + run loop (fast/slow/all/<name>/-n/--release)
    cluster.rs            shared-cluster fixture + corruption gate + teardown
    db.rs                 typed tokio-postgres assertion helpers
    psql.rs               psql wrappers (.sql workloads + background loops)
    workload.rs           .sql runner: exec + normalize + golden diff
    scenarios/
      fast/*.rs           fast scenarios (one `run` fn each)
      slow/*.rs           slow scenarios
  fast/  *.sql *.out      fast golden workloads
  slow/  *.sql *.out      slow golden workloads
```

## Useful env vars

`E2E_REGEN_EXPECTED=1` (regenerate goldens), `SOAK_SECS`,
`PS_HTTP_TIMEOUT`, `ENDPOINT_START_TIMEOUT` (restart-scenario timing),
`YB_HOST`/`YB_PORT`/`COMPUTE_PORT`, `YB_DBNAME`/`YB_LIGHT_REPO_DIR`/`TEST_DIR`
(override the ephemeral DB / run dir), `YB_LIGHT` (path to the binary),
`YB_PS_VERIFY_WAL_ROUNDTRIP` (defaults to 1).
