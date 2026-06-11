---
status: done
severity: medium
area: yb-pageserver
opened: 2026-06-10
---

# Convert the e2e suite from shell scripts to Rust

## Outcome

Done. The e2e suite is now a Rust harness — the `e2e_tests` crate at
`test/e2e/` (path-only workspace member, `publish = false`). The shell
assertion style is gone: scenario checks run through typed
`tokio-postgres` queries and `reqwest` HTTP calls instead of
`[[ $X == $Y ]]` against `psql -qtA` strings. Coverage is a faithful
1:1 port — same scenarios, same `.sql`/`.out` goldens, same
corruption-marker gate, same shared-cluster model.

`test/e2e/run.sh` is now a thin wrapper (`cargo build` + exec the
binary) so the documented `test/e2e/run.sh fast|slow|all` UX and the
`e2e-before-corruption-claim` skill's command keep working unchanged.

## What landed

- **Driver binary, not `cargo test`.** The shared-cluster + fixed-order
  + gate-after-each + fast-then-slow-on-one-cluster model can't be
  expressed with the parallel/unordered default test harness, so the
  crate is a `[[bin]]` parsing `fast|slow|all` exactly like the old
  `run.sh`. `test/e2e/run.sh` wraps it.
- **Subprocess for cluster ops, typed clients for assertions.**
  `yb_light_local` is still driven as a subprocess (it owns process
  supervision + pidfiles); only the assertions moved to typed clients.
- **`Cluster` fixture** (`src/cluster.rs`) owns the lifecycle the old
  `run.sh` hand-rolled: ephemeral-db setup, the derived init TOML
  (kept outside `repo_dir` since `clean.sh` wipes it), `init`/`start`,
  the shared tenant + `main` endpoint, `prep_for_next_test` recycle,
  the corruption-marker log gate, and a `Drop` that runs `clean.sh` on
  success / leaves the cluster up for postmortem on failure.
- **PASS reporting is the runner's job.** Scenarios just assert and
  return `Ok(())`; `run_scenario`/`run_sql` print the uniform
  `PASS — <name>  (<n>s)`.
- **Filesystem auto-registration — zero edits to add a test.** The
  build script (`test/e2e/build.rs`) scans `src/scenarios/{fast,slow}/`
  and generates the `pub mod` declarations + the `fast()`/`slow()`
  registries; `.sql` workloads are globbed at runtime from
  `test/e2e/{fast,slow}/`. Adding a test is just dropping a file: a
  `<name>.rs` (with `pub async fn run(&Cluster)`) into the right
  scenario subdir, or a `<name>.sql`/`<name>.out` pair into the
  category dir. The subdir is the category; the file stem is the test
  name.
- **Single-test selection.** `test/e2e/run.sh <name>` (or a substring)
  runs just that test against the shared cluster — e.g.
  `test/e2e/run.sh branch_ancestor` or `… smoke`.
- **`.sql` goldens kept verbatim.** The `.sql`/`.out` files stay under
  `test/e2e/{fast,slow}/`; the Rust runner execs `psql -f`, normalizes
  (the same `Time:`/version-warning strip), and diffs (`E2E_REGEN_EXPECTED=1`
  still regenerates).

## Scenarios ported (coverage parity)

Fast scenarios (`src/scenarios/fast/`): `branch_ancestor`,
`branch_endpoint`, `going_away`, `layout`, `multi_timeline`,
`restart_correctness`, `restart_wal_replay_yb`; fast goldens `smoke`,
`update-churn`. Slow scenario (`src/scenarios/slow/`):
`restart_wal_replay`; slow goldens `btree-splits`, `full-workload`. The
eight `*.sh` scripts were deleted; the `.sql`/`.out` goldens and
`clean.sh` were kept. Test name = source file stem; run one with
`test/e2e/run.sh <name>`.

## Verification

Against real YugabyteDB (2026-06-10), on the final auto-discovered
harness:

- `test/e2e/run.sh fast` — all 7 scenarios + `smoke`/`update-churn`
  goldens green, corruption gate clean.
- `test/e2e/run.sh slow` — `restart_wal_replay` (n advanced 5225 across
  the WAL gap) + `btree-splits` + `full-workload` (678s) green.
- Both end with `pageserver_yb_correctness_errors_total` verdict at 0.
- `test/e2e/run.sh -n 3 smoke` — single-test selection + cumulative
  tally verified (`3 passed, 0 failed`).

`--release` is wired in the wrapper (builds `--release`, points
`YB_LIGHT` at the release binary) but a full release e2e was not run —
release builds are heavy and need the special `PROTOC` on this box (see
AGENTS Build notes); the run path is identical to the verified debug
path, only the profile + binary dir differ.

## Notes / links

- Subsumes [task 12](12_e2e-test-infra.md) (the original one-command
  e2e infra goal) and preserves the golden validation from
  [task 25](25_e2e-sql-output-validation.md).
