# Test tiers: `regression` and `nightly`

Two tiers, two tags — and **`nightly` is a superset of `regression`**:

- **`regression`** — the fast gate on every PR / push to `master`. Checks that the
  basic functions still work, that the main use case is intact, and that the PR
  didn't break anything important. Must stay green to merge.
- **`nightly`** — the extended sweep, run on Jenkins against real deployments. It
  **re-runs the regression tests** (across the real backings) *plus* what
  regression can't afford: real k8s / GCP / YBA / yugabyted / raw-YB backings,
  chaos/nemesis, perf, and (planned) negative/red-path cases.

So the regression tests carry **both** tags (`regression` + `nightly`); the
extended-only tests carry just `nightly`. `--tag regression` = the fast 11;
`--tag nightly` = everything (23).

`ampqa collect --tag regression` and `--tag nightly` are the source of truth for
what's in each. The `canary` suite (engine self-test) and `presets` (setup-only
dev-env menus) are deliberately untagged — they're selected by `--suite` / name,
not by tier.

---

## `regression` — every PR / master change (11 tests)

Fast, deterministic, each asserting a distinct core guarantee. Env-portable: the
tests depend on env *capabilities* (`pgEnv` to reach the endpoint's Postgres, plus
`computeRestarter` for the restart tests), not a concrete env — so the same tests
run on either:

```
# self-building k8s (default): stands up its own amp control plane off a bare
# kube-context, project-per-test, tears it all down. No config needed.
ampqa run --tag regression

# docker env on a remote host (e.g. a GH Actions VM), no k8s
ampqa run --tag regression --env-provider docker -c <docker-env.yaml>
```

Under `--fake-env` every regression test skips cleanly.

| Test | What a failure catches |
|---|---|
| `pg_roundtrip` | compute↔storage broken — a row written through amp compute can't be read back with the right count+checksum |
| `pg_restart_compute` | the headline guarantee: data does NOT survive a compute restart (WAL replay / storage-compute separation regressed) |
| `workloadgen_verify` | data corruption — the recomputed deterministic checksum no longer matches the database |
| `pg_parallel_isolation_alpha` / `_beta` | multitenancy leak — a co-tenant sees another tenant's rows |
| `pg_mutations` | non-insert WAL broken — UPDATE/DELETE deltas don't materialise |
| `pg_transaction` | transaction semantics broken — commit not durable, rollback leaks, or a statement error fails to abort the tx |
| `pg_index` | index pages don't reconstruct — a point lookup / range count over a secondary index disagrees |
| `pg_read_cold_cache` | cold read path broken — after reconnect (empty LFC) a full scan+lookup can't be rebuilt from the Pageserver |
| `pg_ddl_survives_restart` | catalog/index durability broken — table, index, index-backed lookup, or data don't survive the `consistent_lsn` boundary |
| `pg_multi_db_workload` | **the main use case** — separate databases on one endpoint don't run correct, distinct, isolated workloads |

The PR gate is: all `regression` tests pass. Exit `0` = green; non-zero blocks the
merge (note exit `4` = all-skipped, which the gate must treat as a failure).

> Add to the gate by tagging a test `regression` — keep it fast and make it assert
> something a real regression would break, not a tautology; depend only on `pgEnv`
> (or `computeRestarter`) so it stays env-portable.

---

## `nightly` — extended sweep on Jenkins (19 tests = the 11 regression + 8 extended)

The wide, heavy tier: it re-runs the regression tests against real backings, and
adds real deployments across clouds/backings, chaos, and long runs. Selected by
`--tag nightly`; on Jenkins it's typically split into several jobs, each with the
env overlay its tests need (a single run would demand every cloud credential at
once). The extended-only tests (beyond regression) are:

- **Foundation on k8s** — end-to-end (backing → amp → workloads → teardown) on the
  k8s-hosted backing: `amp_on_yb_k8s`. The cross-cloud / VM / YBA foundation tests
  (`amp_on_yugabyted_aws`, `amp_on_raw_yugabyte_aws`, `amp_on_yba_universe`,
  `amp_on_yba_provisioned`) are **untagged** — run on demand by name when that
  cloud infra is available, not part of the automated nightly tier for now. (A
  second k8s check — yugabyted in k8s pods, via the `ybpod` backing — is planned
  but not yet a registered test.)
- **amp features on real k8s** — `amp_branch_isolation`, `amp_endpoint_lifecycle`,
  `amp_compute_restart`.
- **Stress** — `combined_load` (parallel workloads + chaos/nemesis against an
  exclusive universe) and `load_sustain` (workloadgen soak).
- **Perf** — `tpcc` (BenchBase) and `pgbench`, baseline-gated, with version/image
  comparison + bisect + history/trend.

Example (one env at a time):

```
ampqa run --tag nightly --testr '^features/amp_on_yb_k8s$'          -c gke.yaml
# cross-cloud/VM foundation (untagged — on demand, needs the cloud infra):
ampqa run --testr '^features/amp_on_(yugabyted|raw_yugabyte)_aws$'   -c aws.yaml
ampqa run --tag nightly --suite stress -c gke.yaml -o stress.combined.enabled=bool:true
ampqa run --tag nightly --suite perf   -c gke.yaml -o perf.enabled=bool:true
```

> **Planned, not yet present:** dedicated negative / red-path cases (error paths,
> failure injection with asserted failure) belong in `nightly`. Add them tagged
> `nightly`; there are none today.

Nightly failures are triaged, not auto-merge-blocking — except a `regression`-tagged
test failing there too, which is a real regression.

---

## Conventions

- **Exactly two tier tags:** `regression` and `nightly`. Don't introduce others;
  `canary`/`presets` stay untagged (selected by `--suite` / name).
- **PR gate = `--tag regression` only** — fast, deterministic, no cloud provisioning.
- **Nightly = `--tag nightly`**, one Jenkins job per env overlay; collect reports
  centrally.
- `ampqa describe <test>` shows a test's parameters; `ampqa collect --tag <t>` lists
  a tier; `ampqa manifest` is the machine-readable catalog for CI.
