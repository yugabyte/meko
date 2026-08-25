# Test tiers: `regression`, `stress`, `perf` and `nightly`

Four tier tags — and **`nightly` is a superset of the other three**:

- **`regression`** — the fast gate on every PR / push to `master`. Checks that the
  basic functions still work, that the main use case is intact, and that the PR
  didn't break anything important. Must stay green to merge.
- **`stress`** — all eleven `suites/stress` scenarios: `load_sustain` (workloadgen
  soak), `combined_load` (four parallel workloads under chaos on a co-located YB
  backing), `backing_maintenance` (workloads through a rolling restart, a gflag
  change, a data move and a YugabyteDB upgrade), `backing_backup_restore`
  (workloads through a YBA backup, restore, PITR rewind and scheduled backup),
  `multitenant_load` and
  `branch_churn` (many tenants, many branches), the nemesis / durability
  scenarios (`amp_nemesis`, `backing_nemesis`, `storage_proc_nemesis`,
  `commit_durability`) and `bulk_load_kill` (one large write killed mid-flight).
  Scheduled as its own tier so the report portal gets a Stress
  column; the `Category: core.Stress` tests in `suites/features` are **not** in it
  (they belong to `regression`/`nightly` and would double-report).
- **`perf`** — the baseline-gated benchmarks in `suites/perf` (`tpcc`, `pgbench`,
  `sysbench`), with version/image comparison + bisect + history/trend.
- **`nightly`** — the extended sweep, run on Jenkins against real deployments. It
  **re-runs** every tagged test (across the real backings) *plus* what regression
  can't afford: real k8s / GCP / YBA / yugabyted / raw-YB backings, chaos/nemesis,
  and (planned) negative/red-path cases.

So a regression test carries **both** `regression` + `nightly`, a stress test
`stress` + `nightly`, a perf test `perf` + `nightly`; the extended-only tests carry
just `nightly`.

`ampqa collect --tag regression` and `--tag nightly` are the source of truth for
what's in each. The `canary` suite (engine self-test) and `presets` (setup-only
dev-env menus) are deliberately untagged — they're selected by `--suite` / name,
not by tier.

---

## `regression` — every PR / master change (11 tests)

Fast, deterministic, each asserting a distinct core guarantee. Env-portable: the
tests depend on env *capabilities* (`pgEnv` to reach the endpoint's Postgres, plus
`computeRestarter` for the restart tests), not a concrete env — so the same tests
run against a self-built or a standing control plane alike:

```
# self-building k8s (default): stands up its own amp control plane off a bare
# kube-context, cluster-per-test, tears it all down. No config needed.
ampqa run --tag regression

# an already-standing control plane, cluster-per-test against it
ampqa run --tag regression -c <standing-cp.yaml>
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
| `pg_read_cold_cache` | cold read path broken — after a compute restart (the only thing that empties the instance-scoped LFC) a full scan+lookup can't be rebuilt from the Pageserver, or the reads never missed the cache at all |
| `pg_ddl_survives_restart` | catalog/index durability broken — table, index, index-backed lookup, or data don't survive the `consistent_lsn` boundary |
| `pg_multi_db_workload` | **the main use case** — separate databases on one endpoint don't run correct, distinct, isolated workloads |

The PR gate is: all `regression` tests pass. Exit `0` = green; non-zero blocks the
merge (note exit `4` = all-skipped, which the gate must treat as a failure).

> Add to the gate by tagging a test `regression` — keep it fast and make it assert
> something a real regression would break, not a tautology; depend only on `pgEnv`
> (or `computeRestarter`) so it stays env-portable.

---

## `nightly` — extended sweep on Jenkins (the regression tests plus the extended ones)

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
- **Stress** (also `--tag stress`) — `combined_load` (parallel workloads +
  chaos/nemesis against an exclusive universe), `backing_maintenance` (the same
  co-located backing under PLANNED maintenance instead of chaos),
  `backing_backup_restore` (the same backing under YBA's data-protection
  operations — backup, restore, PITR, scheduled backups), `load_sustain`
  (workloadgen soak), `multitenant_load` and `branch_churn`, the nemesis /
  durability scenarios (`amp_nemesis`, `backing_nemesis`, `storage_proc_nemesis`,
  `commit_durability`) and `bulk_load_kill`, which deletes an endpoint's compute
  pod — gracefully and by killing the container outright — in the middle of one
  large COPY / INSERT ... SELECT / batched-insert transaction / million-row
  UPDATE and holds it to all-or-nothing.
- **Perf** (also `--tag perf`) — `tpcc` (BenchBase), `pgbench` and `sysbench`,
  baseline-gated, with version/image comparison + bisect + history/trend. All
  three also sample the backing's own numbers while the benchmark runs
  (`apps/metrics`: per-pod CPU/memory plus the `apps.metrics.collect` prometheus
  set) and report them as series metrics under human keys — `[yb tserver] Write
  latency`, not a raw prometheus name. `microbench` samples the amp compute
  endpoint pod instead (`apps.metrics.endpoint_collect`, section `[endpoint]`),
  since no YB backing is in its request path. Set
  `apps.metrics.enabled=bool:false` to turn that off.

Example (one env at a time):

```
ampqa run --tag nightly --testr '^features/amp_on_yb_k8s$'          -c gke.yaml
# cross-cloud/VM foundation (untagged — on demand, needs the cloud infra):
ampqa run --testr '^features/amp_on_(yugabyted|raw_yugabyte)_aws$'   -c aws.yaml
ampqa run --tag stress -c gke.yaml -o stress.combined.enabled=bool:true
ampqa run --tag perf   -c gke.yaml -o perf.enabled=bool:true
```

`--tag stress` selects the whole tier. `load_sustain` needs no config beyond a
`pgEnv`-capable env (it skips otherwise) and honours `-o stress.duration`.
`combined_load` self-provisions, so it skips unless `stress.combined.enabled` is
set *and* a kube-context (`apps.ybk8s.kubecontext` / `apps.ybk8s.kubeconfig` /
`env.ampstack.context`), a chart version (`apps.ybk8s.chart_version` /
`versions.yugabytedb`) and the private sample-app jar secret
(`stress.combined.jar_secret`, default `gh_package`) are all resolvable.
`backing_maintenance` and `backing_backup_restore` do NOT self-provision: they
operate on the universe the env already stood up, so they need
`env.yb_deploy=yba_provisioned` (kubernetes) and skip on any env that gives them
no YBA universe. Both are ON by default (`stress.backing_maintenance.enabled`,
`stress.backing_backup_restore.enabled`) — and since every tagged test also
carries `nightly`, that is roughly an hour of backing-mutating work added to the
`stress` lane and to the nightly sweep, not just to `--tag stress`.

They still skip, with the reason on the result, when the backing cannot carry
them:

- `backing_maintenance` needs 3 tservers, because it takes one out under load.
  The default universe is RF1 (`apps.yba.k8s.num_nodes`), so it skips there; the
  scheduled stress lane passes `num_nodes=3` / `replication_factor=3`.
- `backing_backup_restore` needs YBC on the universe. `apps.yba.k8s.enable_ybc`
  is on, so a kubernetes universe has it; a VM universe needs
  `apps.yba.universe.enable_ybc=bool:true`, which is off by default because
  `WaitForYbcServer` there pings a yb-controller nothing installs.
- Either one skips when the private sample-app jar secret is unresolvable.

`backing_maintenance` defaults to `operations: ["gflag_change"]`. The
`software_upgrade` operation is not in that list: the target defaults to the
YBA's own version, and a universe already at or past it fails the run, so it
needs a universe CREATED a train behind:

```
ampqa run --testr '^stress/backing_maintenance$' -c gke.yaml \
  -o stress.backing_maintenance.operations=list:gflag_change,software_upgrade \
  -o apps.yba.universe.yb_version=<previous-train-release>
```

> **Planned, not yet present:** dedicated negative / red-path cases (error paths,
> failure injection with asserted failure) belong in `nightly`. Add them tagged
> `nightly`; there are none today.

Nightly failures are triaged, not auto-merge-blocking — except a `regression`-tagged
test failing there too, which is a real regression.

---

## Conventions

- **Four tier tags:** `regression`, `stress`, `perf`, `nightly`. Don't introduce
  others; `canary`/`presets` stay untagged (selected by `--suite` / name).
- **Every tagged test also carries `nightly`** — the tier tags partition what the
  nightly sweep re-runs, they don't opt out of it.
- **PR gate = `--tag regression` only** — fast, deterministic, no cloud provisioning.
- **Nightly = `--tag nightly`**, one Jenkins job per env overlay; collect reports
  centrally. `--tag stress` / `--tag perf` are the scheduled per-tier lanes, each
  needing its own env overlay and enable flag (see below).
- `ampqa describe <test>` shows a test's parameters; `ampqa collect --tag <t>` lists
  a tier; `ampqa manifest` is the machine-readable catalog for CI.
