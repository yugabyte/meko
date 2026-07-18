# Compute integration suite

A Rust harness (`integration_tests` crate) that brings up a real `yb_amp_local`
cluster against a real YugabyteDB and exercises it with scenario tests
and golden `.sql` workloads, scanning the pageserver log for corruption
markers after every test. This is the verification gate the
`integration-before-corruption-claim` skill points at.

> The companion **control-plane (controller-on-Docker)** suite lives in
> [`../control_plane`](../control_plane); Postgres assertion helpers
> shared by both suites are in [`../libs/integration_common`](../libs/integration_common).

## Running

```sh
test/integration/compute/run.sh                 # unit then fast (default)
test/integration/compute/run.sh unit            # only the live-DB cargo-test modules
test/integration/compute/run.sh fast            # only the fast suite
test/integration/compute/run.sh slow            # only the slow suite
test/integration/compute/run.sh all             # unit, fast, then slow (one shared cluster)
test/integration/compute/run.sh smoke           # one test by name (substring also matches)
test/integration/compute/run.sh -n 20 restart_wal_replay_yb   # repeat 20x, cumulative pass/fail
test/integration/compute/run.sh --release all   # build + run in release mode
test/integration/compute/run.sh -h              # full help
```

`run.sh` is a thin wrapper: it `cargo build`s the harness +
`yb_amp_local` + every cluster server binary `yb_amp_local` spawns
(`controller`, `yb-pageserver`, `safekeeper`, `endpoint_storage`,
`compute_ctl`) + the diagnostic tools (incremental — a no-op when nothing
changed), then execs the harness. Building the server binaries here is
essential: `yb_amp_local` launches them from `target/<profile>/` at
runtime without rebuilding, so a missing one silently runs a stale binary.
Default builds are debug; `--release` builds and runs the release binaries.

`run.sh` also rebuilds the **`yb_amp` Postgres extension**
(`make yb-amp-pg-ext-v18`) when its C sources changed — `cargo` does not build
it, and the compute loads `pg_install/v18/lib/postgresql/yb_amp.so` at runtime,
so a stale `.so` runs an old walproposer/compute against fresh cluster binaries
(a wire/ABI skew that looks like a hang or segfault). Staleness is tracked via
`build/.yb_amp_ext.stamp`; because the PGXS build has no header-dependency
tracking, any C change forces a **clean** extension rebuild (~1–2 min), while
Rust-only edits skip it entirely.

The **`unit`** selection (part of the default and of `all`) runs the
live-DB tests via `cargo test`, pointed at this run's cluster: the
pageserver/safekeeper modules (`pageserver::page_store::tests`,
`pageserver::page_writer::tests`, `safekeeper::yb_wal_storage::tests`,
with `YB_TEST_HOST/PORT/DB`) and the `yb_tsclient` live suite
(`tests/live.rs`, with `YB_LIVE_TSERVERS`/`YB_LIVE_YSQL`/`YB_LIVE_DB` —
its tserver RPC endpoint lives on `:9100`). These need a real YB with the
production schema migrated; out of the box they assume a developer-managed
cluster on `5433`/`yb_storage`, which `unit` replaces with the run's
isolated ephemeral database (this is how CI runs them — see
`.github/workflows/pr-build.yml`).

- **`-n <count>`** repeats the selection and prints a cumulative
  `passed / failed` count. It runs *all* selected tests each iteration
  (failures are tallied, not fatal) — for hunting flakiness. Without
  `-n` the run is fail-fast: the first failure aborts.
- A passing run tears the cluster + DB + run dir down. A failing run stops
  the cluster + drops the DB but keeps its `*.log` files under
  `.integration-tests/run_<id>/` (harness + compute/pageserver/safekeeper/
  controller); the run dir is GC'd after 7 days. Pass
  `--no-fail-stop` to instead leave a *failed* run's cluster + DB running for
  postmortem (it prints how to tear it down).

### Prerequisites

- A YugabyteDB reachable at `127.0.0.1:5433` (override with
  `YB_HOST`/`YB_PORT`). If you don't already run one, start the same
  image CI uses with `scripts/yb-local.sh start` (Docker; `stop` to tear
  it down) — see that script's header for details.
- `psql` on `PATH` (used for the `.sql` workloads and the background
  write/read loops).

## How it works

- **One shared cluster.** A single `yb_amp_local` cluster + tenant +
  `main` endpoint is set up once; all tests share it. Between tests the
  cluster is recycled — any component the prior test left down is
  restarted, a pageserver that is alive but not accepting connections is
  force-restarted (a failed config-restore restart can wedge it, #241),
  and the compute endpoint is recreated — so each test starts from a
  clean compute state. The tenant + timeline persist, so **tests must
  use unique table names**.
- **Ephemeral DB.** Each run uses its own `yb_storage_integration_*` database,
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

- **A scenario** (typed assertions): drop a Rust
  `fast/<name>.rs` (or `slow/`) — in the same category dir as the `.sql`
  workloads — with:

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
  `fast/` or `slow/`. The runner execs `psql -f`, captures stdout and
  stderr **interleaved** (like pg_regress's `2>&1`, so `NOTICE`/`WARNING`/
  `ERROR` text is part of the golden), normalizes it, and diffs against the
  `.out` golden. A workload with *intentional* errors should `\set
  ON_ERROR_STOP 0` in its `.sql` (the runner otherwise aborts on the first
  error); see `fast/event-triggers.sql`. The `psql` used is the one built from
  yb-amp's own Postgres (`pg_install/v18/bin/psql`), not whatever is on
  `PATH` — its meta-commands (e.g. `\d`) must match the compute's major
  version. Generate/refresh the golden with
  `INTEGRATION_REGEN_EXPECTED=1 test/integration/compute/run.sh <name>` and review the diff
  before committing. `.sql` workloads are discovered at runtime — no
  recompile needed to add one.
- **Disabling a workload**: rename both files to `<name>.sql.disabled`
  and `<name>.out.disabled`. The runner skips them but prints them as
  `disabled (skipped)` so they stay visible; re-enable by stripping the
  `.disabled` suffix. Used for tests that pass but destabilize the suite —
  e.g. `clog`/`subxacts`, whose heavy commit bursts make the next endpoint
  recycle time out (see issue #115).

## Full upstream pg_regress suite (`pg_regress.sh`)

`run.sh` runs this crate's own checked-in golden workloads. To instead run
the **full upstream PostgreSQL regression suite** against a yb-amp
compute, use `pg_regress.sh` — a separate driver that bypasses the Rust
harness entirely:

```sh
test/integration/compute/pg_regress.sh            # full suite, release build (default)
test/integration/compute/pg_regress.sh --debug    # debug build (slower)
test/integration/compute/pg_regress.sh -j 16      # more pg_regress connections
test/integration/compute/pg_regress.sh --keep     # leave the cluster up for postmortem
test/integration/compute/pg_regress.sh -h         # full help
```

It drives the ~200-test suite straight from the vendored Postgres source
(`compute/vendor/postgres-v18/src/test/regress`) via the stock `pg_regress`
binary: it builds + installs the suite's C support modules (regress,
refint, autoinc), brings up a throwaway `yb_amp_local` cluster, creates
the `regression` database, runs the parallel schedule, and tears down on
exit (unless `--keep`). The by-design `CREATE TABLESPACE`-unsupported
diffs are blessed via the expected-output variants in
[`regress_yb_expected/`](regress_yb_expected) (overlaid onto pg_regress's
`expected/<test>_N.out` alternative-file mechanism, not by editing the
upstream goldens), so a clean tree is a genuine green (0 failures on PG18).
See the header of `pg_regress.sh` and issue #119 for details.

> Defaults to a **release** build — a debug build of the full suite is
> painfully slow. Honours `YB_HOST`/`YB_PORT`, `INTEGRATION_PORT_BASE`,
> `YB_AMP_REPO_DIR`, `YB_DBNAME`.

## Layout

```
test/integration/
  compute/                 this suite (the `integration_tests` crate)
    run.sh                 wrapper: build + run the harness (signal-trap cleanup)
    pg_regress.sh          driver for the FULL upstream pg_regress suite (see above)
    regress_yb_expected/   yb-amp expected-output variants for pg_regress.sh
    clean.sh               teardown (kill procs, drop DB, wipe run dir)
    build.rs               auto-registers scenarios from fast/ and slow/
    Cargo.toml             the `integration_tests` crate
    main.rs                CLI + run loop (fast/slow/all/<name>/-n/--release)
    cluster.rs             shared-cluster fixture + corruption gate + teardown
    psql.rs                psql wrappers (.sql workloads + background loops)
    workload.rs            .sql runner: exec + normalize + golden diff
    scenarios.rs           scenario registry (includes build.rs codegen)
    fast/  *.rs *.sql *.out  fast scenarios + golden workloads (same dir)
    slow/  *.rs *.sql *.out  slow scenarios + golden workloads (same dir)
  control_plane/           the controller-on-Docker suite (`integration_control_plane`)
  libs/integration_common/         shared `db` helpers used by both suites
```

## Useful env vars

`INTEGRATION_REGEN_EXPECTED=1` (regenerate goldens), `SOAK_SECS`,
`PS_HTTP_TIMEOUT`, `ENDPOINT_START_TIMEOUT` (restart-scenario timing),
`YB_HOST`/`YB_PORT`/`COMPUTE_PORT`, `YB_DBNAME`/`YB_AMP_REPO_DIR`/`TEST_DIR`
(override the ephemeral DB / run dir), `YB_AMP` (path to the binary),
`YB_PS_VERIFY_WAL_ROUNDTRIP` (defaults to 1), `INTEGRATION_SKIP=<substr>[,…]`
(drop matching tests from the run — e.g. a known-flaky one — printing
what was skipped).

**Concurrency:** every run is isolated by construction — it synthesizes
its own config on a free port block (auto-picked in 20000..60000, pin
with `INTEGRATION_PORT_BASE=<n>`), uses an ephemeral DB
(`yb_storage_integration_<pid>_<rand>`) and a unique `run_<id>/` dir, and scopes
its teardown to exactly those. So suites can run concurrently and never
tear down each other (or a developer's local cluster). (The shared
`target/debug` still serializes concurrent `cargo build`s on the cargo
lock — harmless, just waits.)
