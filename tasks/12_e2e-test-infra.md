---
status: done
severity: high
area: yb-pageserver
opened: 2026-06-02
---

# End-to-end test infra for corruption-class fixes

## Resolution

Done — subsumed by the e2e suite. The "one command from clean repo to
verdict" goal is realized by `test/e2e/run.sh fast|slow|all`: it brings
up a yb_light_local cluster against an ephemeral YB, runs the
workloads, scans the pageserver log for corruption markers after each,
and exits non-zero with a one-line verdict. The marker list, the
write-heavy golden workloads (`btree-splits.sql`, `full-workload.sql`),
and the `YB_PS_VERIFY_WAL_ROUNDTRIP=1` strict mode (on by default in
the harness) all live there. The harness was ported from shell to Rust
in [task 30](30_e2e-tests-sh-to-rust.md); the golden-output validation
is [task 25](25_e2e-sql-output-validation.md). The
`e2e-before-corruption-claim` skill is the operational rule that points
at this harness.

The remaining open questions below (local-vs-AWS timing reproduction,
CI integration) were never load-bearing for this task and are out of
scope; file them fresh if they bite again.

## Motivation (original)

Two builds (`2345eb514`, `f6f71bc96`) shipped to AWS on 2026-06-02
claiming to address the Btree/INSERT_UPPER walredo PANIC. Both
reproduced identical corruption on AWS, byte-for-byte. The fix
authors' verification was unit tests + a Cargo workspace build; the
unit tests didn't exercise the actual failure mode and the bug only
surfaces under a multi-MB ingest workload that walks btree pages
through INSERT_UPPER (page split + downlink insertion).

We need a one-command local harness so every corruption-class fix
gets exercised against the real failure mode before AWS time is
burned. Right now the dev-box bring-up alone takes 5–10 min of
manual steps (see [`4.k`](4.k_yb-light-local-start-wedge.md)).

## Goals

- **One command from clean repo to verdict.** `./scripts/e2e.sh`
  (name TBD) starts a YB cluster, brings up `yb_light_local`, creates
  a tenant + timeline, starts a compute endpoint, runs the
  corruption-repro workload, scans the PS log for the corruption
  markers, and exits non-zero with a one-line verdict if any fired.
- **Reuse not rebuild.** The harness should accept an
  already-running YB and an already-built pageserver binary so the
  edit-build-test loop is fast.
- **Deterministic seed.** The workload SQL needs a seeded `random()`
  so the failing block IDs are stable across runs — easier
  bisection.
- **Captures the full diagnostic.** On failure, dumps the last 100
  lines of `pageserver.log`, the relevant `psql` output, and the
  `pg_stat_activity` snapshot to a per-run output dir.

## Shape

- `scripts/e2e.sh`: orchestrates the steps below. Bash, no python
  deps; uses `psql` / `target/release/yb_light_local` / `curl`.
- `test/repro/workload.sql`: the canonical write-heavy workload
  (CREATE TABLE big_table + 2M-row INSERT + secondary index +
  UPDATE×N + range SELECTs). Already exists at
  `/tmp/repro-workload.sql` from the 2026-06-02 session; promote
  it into the repo with a fixed seed.
- `test/repro/expected_markers.txt`: list of log substrings whose
  presence indicates regression — the corruption-fix list:
  ```
  MATERIALIZER REDO ERROR
  WRITE-PATH ROUNDTRIP
  image row at lsn
  deserialize error
  PANIC: failed to add new item
  specified item offset is too large
  ```
- Optional: `--smoke` flag (small 50k-row workload, ~30 s) for fast
  smoke-test runs; default to the full 2M-row workload that
  actually triggers btree page splits.

## Open questions

- Does the local YB cluster have enough load tolerance to reproduce
  the AWS timing-dependent failures? The 2026-06-02 local run
  passed cleanly on `8c4208b85` while AWS failed; the e2e harness
  needs a way to either turn up the workload until it reproduces
  locally, or document the gap honestly so we don't false-pass.
- Should we run under `YB_PS_VERIFY_WAL_ROUNDTRIP=1` by default?
  It's slow but catches ser/des regressions early. Probably:
  `--strict` flag enables it.
- Integration with CI: out of scope for v1; e2e is operator-run
  during fix verification.

## Notes

- Closing the loop here also closes the dev-box vs AWS divergence
  the user flagged after the 2345eb514/f6f71bc96 shipments. Without
  this harness we'll keep shipping local-verified builds that fail
  on AWS.
- Cross-link to memory entry `run-e2e-before-claiming-fix.md` —
  this task is the tooling backing for that workflow rule.
