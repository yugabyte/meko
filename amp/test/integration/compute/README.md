# Compute integration suite

A Rust harness (`integration_tests` crate) that brings up a real `yb_amp_local`
cluster against a real YugabyteDB and exercises it with scenario tests
and golden `.sql` workloads, scanning the pageserver log for corruption
markers after every test. This is the verification gate the
`integration-before-corruption-claim` skill points at.

> The companion **control-plane (controller-on-Docker)** suite lives in
> [`../controller`](../controller); Postgres assertion helpers
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

`run.sh` is a thin wrapper: it `cargo build`s the harness + the Rust
cluster server binaries `yb_amp_local` spawns (`pageserver`,
`safekeeper`, `compute_ctl`) + the diagnostic tools,
and `go build`s the Go binaries (`yb_amp_local` and `controller`) —
incremental, a no-op when nothing changed — then execs the harness. Building the server binaries here is
essential: `yb_amp_local` launches them from `target/<profile>/` at
runtime without rebuilding, so a missing one silently runs a stale binary.
Default builds are debug; `--release` builds and runs the release binaries.

`run.sh` also rebuilds the **compute Postgres extensions** (`make
yb-amp-pg-ext-v18`: `yb_amp` and the vendored `pgsentinel`) when their sources
changed (C, headers, Makefiles, SQL scripts, control files) or a library the
compute preloads is missing. `cargo` does not build them, and the compute loads
`<install>/lib/postgresql/yb_amp.so` at runtime,
so a stale `.so` runs an old walproposer/compute against fresh cluster binaries
(a wire/ABI skew that looks like a hang or segfault). Staleness is tracked via
`.yb_amp_ext.stamp` in the build dir; because the PGXS build has no
header-dependency tracking, any such change forces a **clean** extension rebuild
(~1–2 min), while Rust-only edits skip it entirely.

### Which PostgreSQL the suites run

By default `run.sh` and `pg_regress.sh` run against an **assertion-enabled**
Postgres (`--enable-cassert`): these suites exist to catch corruption and lost-write bugs,
and an `Assert` firing on the spot is the point — a release build runs the same
tests blind. It is the default `BUILD_TYPE=debug` tree, `pg_install-debug/v18`
(build artifacts under `build-debug/`), selected in the [`Makefile`](../../../Makefile) and
resolved by [`pg_tree.sh`](pg_tree.sh) — the same tree a plain `./build.sh`
produces, so the two share one build. The release Postgres the shipped bundle is
staged from lives in `pg_install-release/`, so neither can clobber the other.
Every run prints which one it got, and refuses to start on a release build.

The assertions are compiled in at `-O2`: the optimisation level is independent
of `--enable-cassert`, and `-O0` cost 1.5–2.2x for nothing.

`--release` selects the release tree instead, meaning what it means to
`build.sh` — release cluster binaries, release extension, release PostgreSQL with
assertions off. Every run prints which one it got. CI runs the default on every
push, so that is where the assertions are guaranteed.

Note what that costs the first time: the first `--release` builds a **whole
second PostgreSQL** into `pg_install-release/` (~5 min, and a second tree on
disk) before the run starts. It is paid once and the tree is reused after.

`build.sh --pg-only [--release]` is the only thing that builds either tree: the
suites pass `PG_INSTALL_CACHED=1` to their own `make` so it cannot configure
Postgres behind that call.

The **`unit`** selection (part of the default and of `all`) runs the
live-DB tests via `cargo test`, pointed at this run's cluster: the
pageserver/safekeeper modules (`pageserver::page_store::tests`,
`pageserver::page_store::gc::tests`, `pageserver::page_writer::tests`,
`safekeeper::yb_wal_storage::tests`, with `YB_TEST_HOST/PORT/DB`) and the `yb_thin_client` live suite
(`tests/live.rs`, with `YB_LIVE_TSERVERS`/`YB_LIVE_YSQL`/`YB_LIVE_DB` —
its tserver RPC endpoint lives on `:9100`). These need a real YB with the
production schema migrated; out of the box they assume a developer-managed
cluster on `5433`/`yb_storage`, which `unit` replaces with the run's
isolated ephemeral database (this is how CI runs them — see
`.github/workflows/amp-pr-build.yml`).

- **`-n <count>`** repeats the selection and prints a cumulative
  `passed / failed` count. It runs *all* selected tests each iteration
  (failures are tallied, not fatal) — for hunting flakiness. Without
  `-n` the run is fail-fast: the first failure aborts.
  All iterations share one cluster, so the harness isolates them: the
  cluster and branch names a scenario provisions carry the iteration
  number (`catchup_race_2`, `gc_dm_2`), and the previous iteration's
  scenario endpoints are destroyed on the way in, freeing their ids,
  compute ports and pageserver/safekeeper pairs. Their `*.log` files are
  copied to `iteration_<n>_logs/` in the run dir first, so a failing
  iteration is still postmortem-able. Only the logs survive the destroy,
  so to keep a failing iteration's pgdata and configs as well, run with
  `INTEGRATION_STOP_ON_FAIL=1 ... --no-fail-stop`: the first flips `-n`
  back to fail-fast, so the run stops at the failing iteration before
  anything is swept, and the second keeps teardown from running `clean.sh`,
  which deletes everything that is not a `*.log`.
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
  Drive cluster operations with `c.yb(&["endpoint", "start", …])`, and give an
  endpoint of the scenario's own the pg port `c.endpoint_port()` returns; make
  assertions with the typed `db` helpers (or `psql` for background load
  loops). Don't print a `PASS` line — the runner does that.

- **A golden `.sql` workload**: drop `<name>.sql` + `<name>.out` into
  `fast/` or `slow/`. The runner execs `psql -f`, captures stdout and
  stderr **interleaved** (like pg_regress's `2>&1`, so `NOTICE`/`WARNING`/
  `ERROR` text is part of the golden), normalizes it, and diffs against the
  `.out` golden. A workload with *intentional* errors should `\set
  ON_ERROR_STOP 0` in its `.sql` (the runner otherwise aborts on the first
  error); see `fast/event-triggers.sql`. The `psql` used is the one built from
  yb-amp's own Postgres (`$POSTGRES_DISTRIB_DIR/v18/bin/psql`), not whatever is
  on `PATH` — its meta-commands (e.g. `\d`) must match the compute's major
  version. Generate/refresh the golden with
  `INTEGRATION_REGEN_EXPECTED=1 test/integration/compute/run.sh <name>` and review the diff
  before committing. `.sql` workloads are discovered at runtime — no
  recompile needed to add one.
- **Provisioning a cluster or branch of your own**: name it through the
  `Cluster` helpers (`cluster_create`, `branch_create_named`,
  `branch_create_at`), not a raw `c.yb(&["cluster", "create", …])`. They
  route the name through `Cluster::iteration_name`, which is what keeps `-n
  <count>` off the rows the previous iteration left live. To use the name
  again (in a SQL assertion, say) call `c.iteration_name(<base>)` — always
  on the base name, never on one a helper or an earlier call already
  resolved, which would suffix it twice. Endpoints need no such care:
  `Cluster::begin_iteration` destroys the previous iteration's.
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
test/integration/compute/run.sh pg_regress         # same thing, via the one entry point
test/integration/compute/pg_regress.sh            # full suite, release build (default)
test/integration/compute/pg_regress.sh --debug    # debug build (slower)
test/integration/compute/pg_regress.sh -j 16      # more pg_regress connections
test/integration/compute/pg_regress.sh --keep     # leave the cluster up for postmortem
test/integration/compute/pg_regress.sh -h         # full help
```

It drives the 230-test suite straight from the vendored Postgres source
(`compute/vendor/postgres-v18/src/test/regress`) via the stock `pg_regress`
binary: it builds + installs the suite's C support modules (regress,
refint, autoinc), brings up a throwaway `yb_amp_local` cluster, creates
the `regression` database, runs the parallel schedule, and tears down on
exit (unless `--keep`). Expect 230/230 against the upstream goldens — there
are no yb-amp-specific expected files, because the compute runs with
`yb_amp.regress_test_mode=on`, the escape hatch `yb_amp_ddl_handler.c`
defines for this suite (it lets `CREATE TABLESPACE` through, which
`test_setup`, `tablespace` and `stats` need).

In CI it runs as a second step of the integration job (`amp-pr-build.yml`), right
after `run.sh`, as `pg_regress.sh --no-build`: it builds nothing and runs on what
that step produced, taking the profile from whatever is in `target/`.

> Defaults to **debug**, matching `run.sh`, so running both does not recompile
> the workspace twice. `--release` is faster over 230 tests but builds release
> throughout, PostgreSQL included, so assertions are off. Honours
> `YB_HOST`/`YB_PORT`, `INTEGRATION_PORT_BASE`, `YB_AMP_REPO_DIR`, `YB_DBNAME`.

## Local extension test (`extensions.sh`)

`extensions.sh` runs an externalized-extension end-to-end against a **local**
`yb_amp_local` deploy — the local-deploy twin of the `SMOKE_EXTENSIONS` block in
[`deploy/smoke-test.sh`](../../../deploy/smoke-test.sh) (which runs the same
check against a GKE cluster). Local deploy has no Kubernetes ImageVolume, so the
extension is "mounted" the way the chart's config does it — the two PG18 path
GUCs — set here via `--pg-setting`:

```sh
test/integration/compute/extensions.sh                 # pgvector smoke, release
test/integration/compute/extensions.sh --ext pgvector  # explicit
test/integration/compute/extensions.sh --regress       # run pgvector's upstream suite
test/integration/compute/extensions.sh --registry my   # override image registry
test/integration/compute/extensions.sh --cache         # layer-cache the base build
test/integration/compute/extensions.sh --keep          # leave the cluster up
test/integration/compute/extensions.sh -h              # full help
```

What it does:

1. Builds the extension image with the real docker pipeline
   ([`deploy/extensions/build.sh`](../../../deploy/extensions/build.sh) →
   `quay.io/.../amp-ext-<name>:<version>`, tag resolved from
   [`extensions.yaml`](../../../deploy/extensions/extensions.yaml)).
2. Extracts its `FROM scratch` tree (`share/` + `lib/`) to a host temp dir.
3. Brings up a throwaway `yb_amp_local` cluster and creates an endpoint whose
   `extension_control_path` / `dynamic_library_path` point at that tree.
4. `CREATE EXTENSION` + a real query (for pgvector, the nearest-neighbour `<->`
   assertions from `smoke-test.sh`), then tears the cluster down (`--keep` to
   leave it up).

The docker-built `.so` loads into the local compute because both compile from
the same `compute/vendor/postgres-v18` submodule (matching `PG_MODULE_MAGIC`).

### `--regress`: the extension's own upstream suite (#608)

`--regress` swaps the hand-written smoke in step 4 for the extension's **own**
upstream regression suite — the extension analogue of what `pg_regress.sh` does
for core PostgreSQL. The suite ships in the extension's source tree, which the
`FROM scratch` ImageVolume deliberately omits, so the driver re-fetches the
source the same way [`extension.Dockerfile`](../../../deploy/extensions/extension.Dockerfile)
does, and runs its suite against the compute that mounts the built tree. The
URL is verified against its pin in
[`downloads_checksum.txt`](../../../downloads_checksum.txt), `AMP_EXT_GH_TOKEN`
authenticates a private source, and `prep` runs with `$SAFE_CURL` set, so what it
fetches is verified too. `patch` then applies from the manifest.

Which suite is per the manifest row's `regress:` block:

- `driver: pgxs` (pgvector, pg_cron, age, hexabridge_mssql) — reads `REGRESS` /
  `REGRESS_OPTS` from the extension's Makefile and runs `pg_regress` straight at
  the live compute,
  stripping any `--temp-instance` / `--temp-config` (pg_cron's Makefile would
  otherwise start a private server).
- `driver: script` (hexabridge) — runs the extension's own runner
  (`scripts/regression.sh`) pointed at the compute via `PG*` env.

By-design amp diffs are blessed as alternative expected outputs under
[`ext_regress_expected/<name>/`](ext_regress_expected/README.md) (overlaid at run
time, upstream goldens untouched), so a clean tree is a genuine green. An
extension with no `regress:` block errors under `--regress`.

> **Requires docker** + `python3`/PyYAML, a YugabyteDB (`YB_HOST`/`YB_PORT`,
> default `127.0.0.1:5433`), and a built Postgres (`pg_install-debug/v18` — the script
> runs `./build.sh` if it is missing). Heavy: the first run builds the
> ABI-anchor PG18 base image in docker (several minutes). Primarily a
> manual/local driver; it is not part of the per-PR CI, but the opt-in
> [`/ci amp_extensions_local`](../../../docs/ci-directives.md) suite runs it with
> `--regress` (and `--cache`, so the base restores from the shared registry layer
> cache).
>
> **Preload caveat:** a preload extension (e.g. `pg_cron`) is also appended to
> `shared_preload_libraries` (the local base is just `yb_amp`); the library base
> name is guessed from the extension's `control` name, so a preload extension
> whose `.so` differs may need that adjusted. pgvector needs no preload, so it
> is the clean first case.

## Layout

```
test/integration/
  compute/                 this suite (the `integration_tests` crate)
    run.sh                 wrapper: build + run the harness (signal-trap cleanup)
    pg_regress.sh          driver for the FULL upstream pg_regress suite (see above)
    extensions.sh          driver: externalized extension smoke + --regress suite on local deploy (see above)
    ext_regress_expected/  amp expected-output variants for extensions.sh --regress
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
  controller/              the controller-on-Docker suite (`integration_control_plane`)
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

**Garbage collection:** every pageserver the suite provisions is *born*
with GC enabled and tuned for prompt reclaim on small test workloads
(`enable_gc`, `gc_period="1s"`, `gc_queue_floor_reset_interval="5s"`,
`pitr_interval="0s"`, a small `gc_horizon`, `gc_candidate_every_n_bases=1`),
injected into each generated `pageserver.toml` via the
`YB_PAGESERVER_TEST_EXTRA_TOML` boot seam (set in `cluster.rs`; born-with
because `enable_gc` is read once at attach, so changing it needs a
restart). So GC runs under
*every* scenario — the whole suite doubles as a live check that trimming
stays invisible to reads — and the `gc_*` scenarios exercise it directly
(reclaim, branch-point pinning, teardown, crash recovery). GC scenarios
run on their own child branches so their reclaim signal is isolated.

**Branch-drop GC** is the *controller's* reaper, not the pageserver's: it
reclaims a dropped branch's `amp_storage` rows after
`--branch-gc-retention` (a day in production, matching the default PITR
window). The suite runs the controller with `AMP_BRANCH_GC_RETENTION=0s`
and `AMP_BRANCH_GC_INTERVAL=3s` (both overridable in the environment;
`cluster.rs` forwards them, and the launcher passes every `AMP_`-prefixed
var through to the controller it spawns), so reclamation happens inside a
test rather than a day later. That is on for *every* scenario — any test
that drops a branch as teardown also exercises the reap —
and `gc_branch_drop_reclaims` asserts it directly.

**Concurrency:** every run is isolated by construction — it synthesizes
its own config on a free port block (auto-picked in 20000..60000, pin
with `INTEGRATION_PORT_BASE=<n>`), uses an ephemeral DB
(`yb_storage_integration_<pid>_<rand>`) and a unique `run_<id>/` dir, and scopes
its teardown to exactly those. So suites can run concurrently and never
tear down each other (or a developer's local cluster). (The shared
`target/debug` still serializes concurrent `cargo build`s on the cargo
lock — harmless, just waits.)
