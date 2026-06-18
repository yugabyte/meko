# End-to-end test suite

A Rust harness (`e2e_tests` crate) that brings up a real `yb_light_local`
cluster against a real YugabyteDB and exercises it with scenario tests
and golden `.sql` workloads, scanning the pageserver log for corruption
markers after every test. This is the verification gate the
`e2e-before-corruption-claim` skill points at.

There are two entry points. Most of this doc is the **`yb_light_local`** suite
(`run.sh`, below). The **amp-on-Docker** suite (`run-amp.sh`) is summarized next.

## amp-on-Docker suite (`run-amp.sh`)

`test/e2e/run-amp.sh` exercises the **amp** control plane on Docker: it starts the
`amp` container (with its Docker endpoint provisioner enabled) against the host's
YugabyteDB, then runs a *thin* test that calls amp's REST API
(`user → storage pool → timeline → endpoint`) and runs `psql` against the endpoint
amp returns. **amp owns all provisioning** — create-timeline records the tenant +
its main timeline, and create-endpoint deploys the compute container synchronously
via amp's `deploy.Deployer` (the Docker driver; yblight-controller.md §5.6),
returning its address. The harness contains no orchestration. The `e2e_tests amp`
selector is the harness side (`src/amp.rs`).

It runs the SC-free self-bootstrap path: amp deploys the endpoint with
`--deploy-bootstrap`, so it provisions the storage schema and initdb's the
recorded tenant/timeline on first start (there is no create-database flow yet).
Storage-controller-coordinated deploy is future work.

```sh
test/e2e/run-amp.sh           # create timeline + endpoint, psql the endpoint
test/e2e/run-amp.sh -n 3      # repeat the suite 3x
```

Prerequisites: Docker; a YugabyteDB on the host at `:5433` (yugabyte/yugabyte);
the `yb-light/neon-endpoint:dev` image present (build it once with
`docker build -t yb-light/neon-endpoint:dev -f deploy/endpoint/Dockerfile .`).
`run-amp.sh` builds `yb-light/amp:dev` itself and tears everything down on exit.

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
`yb_light_local` + every cluster server binary `yb_light_local` spawns
(`storage_controller`, `yb-pageserver`, `safekeeper`, `endpoint_storage`,
`compute_ctl`) + the diagnostic tools (incremental — a no-op when nothing
changed), then execs the harness. Building the server binaries here is
essential: `yb_light_local` launches them from `target/<profile>/` at
runtime without rebuilding, so a missing one silently runs a stale binary.
Default builds are debug; `--release` builds and runs the release binaries.

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
  diffs against the `.out` golden. The `psql` used is the one built from
  yb-light's own Postgres (`pg_install/v17/bin/psql`), not whatever is on
  `PATH` — its meta-commands (e.g. `\d`) must match the compute's major
  version. Generate/refresh the golden with
  `E2E_REGEN_EXPECTED=1 test/e2e/run.sh <name>` and review the diff
  before committing. `.sql` workloads are discovered at runtime — no
  recompile needed to add one.
- **Disabling a workload**: rename both files to `<name>.sql.disabled`
  and `<name>.out.disabled`. The runner skips them but prints them as
  `disabled (skipped)` so they stay visible; re-enable by stripping the
  `.disabled` suffix. Used for tests that pass but destabilize the suite —
  e.g. `clog`/`subxacts`, whose heavy commit bursts make the next endpoint
  recycle time out (see issue #115).

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
`YB_PS_VERIFY_WAL_ROUNDTRIP` (defaults to 1), `E2E_SKIP=<substr>[,…]`
(drop matching tests from the run — e.g. a known-flaky one — printing
what was skipped).

**Concurrency:** every run is isolated by construction — it synthesizes
its own config on a free port block (auto-picked in 20000..60000, pin
with `E2E_PORT_BASE=<n>`), uses an ephemeral DB
(`yb_storage_e2e_<pid>_<rand>`) and a unique `run_<id>/` dir, and scopes
its teardown to exactly those. So suites can run concurrently and never
tear down each other (or a developer's local cluster). (The shared
`target/debug` still serializes concurrent `cargo build`s on the cargo
lock — harmless, just waits.)
