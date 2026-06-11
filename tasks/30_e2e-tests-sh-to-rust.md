---
status: pending
severity: medium
area: yb-pageserver
opened: 2026-06-10
---

# Convert the e2e suite from shell scripts to Rust

## Motivation

The e2e suite (`test/e2e/run.sh` + the `fast/`/`slow/` test scripts) is
all Bash today. It works, but the assertion style is the weak point: every
check is a `[[ $X == $Y ]] || fail "..."` against a string captured from
`psql -qtA`, and the cluster lifecycle (`yb_light_local` CLI calls, pidfile
polling, log-marker scans) is hand-rolled shell. That buys us:

- **Fragile assertions.** Comparisons are stringly-typed; a stray space or
  a locale change in `psql` output silently changes a comparison. No typed
  values, no structured diffs beyond the `.sql`/`.out` golden path.
- **Poor failure ergonomics.** A failing `[[ ]]` prints one `fail` line;
  there's no backtrace, no captured query, no automatic context. Debugging
  means re-running with `bash -x`.
- **Duplicated boilerplate.** Each script re-declares `step`/`fail`,
  re-derives `$REPO`, re-defines `psql_main`/`psql_at` connection helpers
  with the same `PGPASSWORD= … -U cloud_admin -v ON_ERROR_STOP=1 -qtA`
  incantation (see `fast/branch-ancestor.sh`, `fast/multi-timeline.sh`).
- **No reuse of the code under test.** `yb_light_local` is driven as an
  opaque subprocess, parsing tenant ids out of `awk '/successfully
  created/{print $2}'` instead of calling the `control_plane` library that
  already produces them.

We're a Rust shop with a Cargo workspace already in place; the test harness
should be Rust too.

## Goal

Port the e2e suite to a Rust integration-test harness that preserves
**exactly** the current coverage and the one-command UX, while replacing
stringly-typed shell assertions with typed Rust ones and real connection
clients.

Non-goal: changing *what* the tests check. This is a faithful port — same
scenarios, same golden `.sql`/`.out` validation, same corruption-marker
gate, same shared-cluster-across-tests model. Behavior changes belong in
their own tasks.

## What exists today (the port target)

`test/e2e/run.sh` (318 lines) is the orchestrator:

- **Cluster lifecycle:** `setup_cluster` (clean → `yb_light_local init` →
  `start` → one shared tenant + `main` endpoint), `prep_for_next_test`
  (restart any component the prior test left down, recycle the compute),
  `clean.sh` teardown via an EXIT trap. Ephemeral YB db per run
  (`yb_storage_e2e_$$_$RANDOM`), per-run init TOML derived from
  `/tmp/neon-init.toml` with the dbname swapped.
- **Two test kinds:**
  - `*.sh` scripts — drive `$NEON` (the `yb_light_local` bin) + `psql`,
    assert inline. Current fast set: `branch-ancestor`, `branch-endpoint`,
    `going-away`, `layout`, `multi-timeline`, `restart-correctness`,
    `restart-wal-replay-yb`. Slow: `restart-wal-replay`.
  - `*.sql` workloads — run via `psql -f`, normalize stdout
    (`normalize_psql_output`), diff against a checked-in `<name>.out`
    golden; `E2E_REGEN_EXPECTED=1` regenerates. Current set: fast
    `smoke`, `update-churn`; slow `btree-splits`, `full-workload`.
- **Corruption gate:** after every test, scan the new tail of
  `pageserver.log` for `MARKERS` (`MATERIALIZER REDO ERROR|ABORT|PANIC|early
  eof|specified item offset|image row at lsn|deserialize error|WRITE-PATH
  ROUNDTRIP`); first hit aborts. Final verdict also checks the
  `pageserver_yb_correctness_errors_total` metric is 0.

All of that has to survive the port.

## Plan / sketch

1. **Harness crate.** Add a `test/e2e` (or `e2e_tests`) workspace member
   with the harness in `src/` and one Rust test file per current scenario
   under `tests/`. Reuse the workspace's existing deps — `tokio-postgres`
   (replaces every `psql` invocation with a typed client), `reqwest` (the
   metrics + storage-controller HTTP checks), and ideally the
   `control_plane` crate's library API for cluster/tenant/endpoint
   operations instead of shelling to the binary.

2. **Cluster fixture.** A `Cluster` struct that owns the lifecycle
   `run.sh` hand-rolls: ephemeral-db setup, `init`/`start`, the shared
   tenant + `main` endpoint, `prep_for_next_test`-equivalent recycling, and
   a `Drop` impl that runs the `clean.sh` teardown. Decide
   shared-vs-per-test cluster:
   - **Match today (shared cluster, sequential tests):** cheapest, but
     Rust's default test harness runs tests in parallel threads — either
     force `--test-threads=1`, or put all scenarios in a single `#[test]`
     that drives them in sequence (mirrors the current single
     `setup_cluster` + loop), or use a `OnceCell`/`Mutex` global fixture.
   - **Per-test cluster:** cleaner isolation, parallelizable, but N× the
     bring-up cost (5–10 min each per [task 12](12_e2e-test-infra.md)). Too
     slow. Recommend keeping the shared-cluster model.

3. **Typed assertions.** Replace `[[ $CNT == 2000 ]]` with
   `assert_eq!(count, 2000, "...")` over values pulled through
   `tokio-postgres` `query_one(...).get::<_, i64>(0)`. Whole-table
   checksums (`md5(string_agg(...))`) stay SQL-side; the harness just
   compares the returned strings.

4. **Corruption-marker gate as a fixture guard.** Port the `MARKERS`
   log-tail scan into a helper the fixture runs after each scenario (track
   the PS log byte offset, scan only the new tail, fail on first hit). Keep
   the marker list in one place.

5. **Golden `.sql`/`.out` workloads.** Two options:
   - Keep `.sql` + `.out` files and have a Rust runner execute + normalize
     + diff them (smallest change; preserves `E2E_REGEN_EXPECTED`).
   - Or fold each workload into a Rust test asserting on typed results.
   The `.sql` ones are pure-SQL golden captures with deterministic
   projections — keeping them as files (option 1) is the faithful port;
   only the *driver* moves to Rust.

6. **Entry point.** `cargo test -p e2e_tests` (or a thin `test/e2e/run.sh`
   wrapper that calls it, to preserve the documented `test/e2e/run.sh
   fast|slow|all` UX and the `e2e-before-corruption-claim` skill's
   command). Map `fast`/`slow` to a feature flag, an env var, or test-name
   filters.

## Open questions

- **Library vs subprocess for `yb_light_local`.** Calling `control_plane`
  directly is cleaner and gives typed tenant/timeline ids, but the binary
  also does process supervision (spawns PS/SK/compute, writes pidfiles).
  May still need subprocess for `init`/`start`; decide per-command.
- **Parallelism.** If we keep one shared cluster, the suite stays
  sequential — fine, matches today. Worth a comment so nobody "speeds it
  up" with `--test-threads` and corrupts the shared state.
- **Where the crate lives.** `test/e2e` as a path-only workspace member
  (not published) keeps the files where they are; the `.sql`/`.out`
  goldens can stay in `test/e2e/{fast,slow}/`.

## Notes / links

- Faithful port only — see the **Goal** non-goal. Coverage parity is the
  acceptance bar: every scenario in `fast/` and `slow/` today must have a
  Rust equivalent asserting the same things.
- Relates to [task 12](12_e2e-test-infra.md) (the original e2e infra goal)
  and [task 25](25_e2e-sql-output-validation.md) (the `.sql`/`.out` golden
  validation this must preserve).
- The `e2e-before-corruption-claim` skill points at `test/e2e/run.sh`;
  keep that command working (wrapper or rename the skill in the same
  change).
- Verification: the ported suite must pass `test/e2e/run.sh fast`
  (and `slow`) against real YugabyteDB with identical pass/fail behavior to
  the shell version on the same builds.
