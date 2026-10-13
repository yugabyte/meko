# Test tiers: `regression`, `stress` and `perf`

Three tier tags, each a scheduled lane (`scale` exists in `suites/perf` but is
untagged and run on demand — see the Perf bullet):

- **`regression`** — the fast gate on every PR / push to `master`. Checks that the
  basic functions still work, that the main use case is intact, and that the PR
  didn't break anything important. Must stay green to merge.
- **`stress`** — all twenty `suites/stress` scenarios: `load_sustain` (workloadgen
  soak), `combined_load` (four parallel workloads under chaos on a co-located YB
  backing), `backing_maintenance` (workloads through a rolling restart, a gflag
  change, a data move and a YugabyteDB upgrade), `backing_backup_restore`
  (workloads through a YBA backup, restore, PITR rewind and scheduled backup),
  `multitenant_load` and
  `branch_churn` (many tenants, many branches), the nemesis / durability
  scenarios (`amp_nemesis`, `backing_nemesis`, `storage_proc_nemesis`,
  `wal_tail_nemesis`, `control_plane_nemesis`,
  `commit_durability`, `backing_node_partition`, `endpoint_backing_partition`,
  `backing_unreachable_api`),
  `fence_stall` and `fence_stall_supervisor` (the writer stalled past its write
  lease and resumed),
  `bulk_load_kill` (one large write killed mid-flight) and
  `backing_disk_full` (the backing's data disks filled, every INSERT held to an
  answer and every endpoint to losing its write token, then the volumes grown
  and amp held to recovering), `sqlancer` (SQLancer's logic-bug oracles on a quiet
  stack), plus the two `suites/features` tests below: twenty-two in all.
  Every `suites/stress` test that drives amp runs on a stack of its own
  (`Shape: DedicatedStack`: its own YugabyteDB and amp control plane, torn
  down with the test), so a fault one injects, or a wedge it leaves behind,
  never reaches the next test. `load_sustain` (`SingleContainer`) and
  `combined_load` (`Universe`) already stood on their own.
  Scheduled as its own tier so the report portal gets a Stress
  column. Membership is by cost, not by category. The `Category: core.Stress`
  tests in `suites/features` are **not** in it (they belong to `regression` and
  would double-report), while `amp_failed_create_reclaim` and
  `amp_cluster_failed_create_reclaim` are: each waits out the controller's 35m
  deploy timeout and then the sweeper's 30m gc-grace, over an hour before either
  can assert anything. Both grace periods are control-plane-wide settings, and
  both tests stay `Shape: SharedControlPlane`, so they cannot be shortened for
  themselves alone and they are the two tests in this tier without a stack of
  their own.
- **`perf`** — the baseline-gated benchmarks in `suites/perf` (`tpcc`, `pgbench`,
  `sysbench`), with version/image comparison + bisect + history/trend, plus the
  measured-number tests that gate against portal history rather than a workload
  baseline (`lifecycle`, `ops_latency`, `chain_depth_read`, `branch_depth`,
  `idle_footprint`, `latency_topology`). Those six each provision a stack of
  their own.

The `scale` tests in `suites/perf` fit no scheduled lane and run on demand by
name or `--testr`.

`ampqa collect --tag <tier>` is the source of truth for what's in each. The `canary` suite (engine self-test) and `presets` (setup-only
dev-env menus) are deliberately untagged — they're selected by `--suite` / name,
not by tier.

---

## `regression` — every PR / master change (87 tests)

Fast, deterministic, each asserting a distinct core guarantee. The table below is
the core-guarantee subset; the tier also carries the amp-feature tests
(`amp_branch_isolation`, `amp_endpoint_lifecycle`, `amp_compute_restart`), the
control-plane upgrade tests (`amp_upgrade`, `amp_upgrade_chain` — the chain never
calls `POST /endpoint-upgrades`, so it does not subsume the single hop and both
stay here, plus `amp_format_compat`, `amp_mixed_versions` and
`amp_endpoint_rollback`), the control-plane
contract tests (`amp_pool_lifecycle`, `amp_endpoint_upgrade_roll`, the resize and
race tests) and the extension / PG-compatibility matrix. The two failed-create
reclaim tests are `stress`, not here. Env-portable: the
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

---

## Extended tests — on demand, by name

The `stress` and `perf` tiers also run on demand, each with the env overlay its
tests need (a single run would demand every cloud credential at once):

- **Stress** (also `--tag stress`) — `combined_load` (parallel workloads +
  chaos/nemesis against an exclusive universe), `backing_maintenance` (the same
  co-located backing under PLANNED maintenance instead of chaos),
  `backing_backup_restore` (the same backing under YBA's data-protection
  operations — backup, restore, PITR, scheduled backups), `load_sustain`
  (workloadgen soak), `multitenant_load` and `branch_churn`, the nemesis /
  durability scenarios (`amp_nemesis`, `backing_nemesis`, `storage_proc_nemesis`,
  `wal_tail_nemesis` — safekeeper restarts under a degraded backing, the
  meko#777/meko#941 WAL-loss class, `control_plane_nemesis` — the controller pod
  deleted mid-task, holding the meko#578/meko#971 task re-claim contract,
  `commit_durability`, `backing_node_partition` — one backing tserver pod cut
  off the network (iptables in its own netns) while a quorum serves, every
  INSERT held to an answer, `endpoint_backing_partition` — one endpoint pod cut off the backing,
  held to fencing itself, coming back, and every acknowledged id in storage) and `bulk_load_kill`, which deletes an endpoint's compute
  pod — gracefully and by killing the container outright — in the middle of one
  large COPY / INSERT ... SELECT / batched-insert transaction / million-row
  UPDATE and holds it to all-or-nothing. `fence_stall` and
  `fence_stall_supervisor` take nothing away: they stall the writer of a branch
  past its write lease (the whole pod through cgroup v2, or the supervisor alone
  under SIGSTOP), attempt a second writer while it is stalled, and resume it.
  `sqlancer` installs yugabyte/sqlancer from its `master-yb` release in a
  client pod and runs each oracle through its amp provider for 10 minutes,
  4 oracles at once on 4 endpoints (`stress.sqlancer.endpoints`), with 16
  threads each. Nothing faults the stack, because SQLancer reports a
  dropped connection as a bug. The install step records the commit the release
  was built from and the tarball's sha256. Opt-in (`stress.sqlancer.enabled`).
  The tier also carries the two `suites/features` failed-create reclaim tests,
  which are here for their runtime rather than for any fault they inject.
  `backing_node_partition_multi_region` is `backing_node_partition` with the
  tservers and the tenants' endpoints first placed a multi-region deployment's
  round trips apart (`apps/netshape`, tc netem in the pods' own network
  namespaces, `stress.backing_node_partition.multi_region_topology`): one
  region's tserver cut off while the surviving quorum is tens of milliseconds
  apart.
- **Perf** (also `--tag perf`) — `tpcc` (BenchBase), `pgbench`, `sysbench` and
  `pgbench_uncached`, each gated against a LIVE baseline by default
  (`perf.baseline.live`): a second set of stacks on the prior release runs the
  same workload concurrently, and the verdict is the median of the per-pair
  deltas (provision i against provision i), with a sign test on the pairs.
  Throughput is gated at saturation and latency on the median of
  `perf.bench.fixed_rate_iterations` fixed-rate windows per stack at
  `perf.bench.fixed_rate_fraction` of the lowest arm's throughput. Every stack
  is pinned to `perf.arm_node_selector` and, with `perf.pin_zone`, to one of
  that pool's zones, the same one for provision i of both arms. Its tservers, masters and client
  pod run with limits equal to requests (`perf.isolation.*`); the endpoint is
  Guaranteed already, by the product chart. A datapoint's allowance is its A/A
  band where `perf.baseline.aa` runs have built one. A band past
  `perf.live_arm.max_band_pct` (live verdict) or `perf.gate.max_band_pct`
  (history gate) is reported `noisy` and never failed on. A benchmark fails on
  one datapoint past its allowance; the lifecycle and microbench arms, one stack
  each, need `perf.live_arm.min_violations` and a measured band (A/A or history),
  and report `no_band` until one exists. A benchmark with its live arm holds 6 of the
  cluster's `sched.cluster_slots.capacity` (12) stack slots, so at most two run
  at once across every run on the cluster; the rest wait before their body. The
  portal-history comparison stays informational (its band/verdict are
  framework-side, `perf.gate.*`). Version/image comparison + bisect +
  history/trend remain. All four also sample the backing's own numbers while the benchmark runs
  (`apps/metrics`: per-pod CPU/memory plus the `apps.metrics.collect` prometheus
  set) and report them as series metrics under human keys — `[yb tserver] Write
  latency`, not a raw prometheus name. `microbench` samples the amp compute
  endpoint pod instead (`apps.metrics.endpoint_collect`, section `[endpoint]`),
  since no YB backing is in its request path, and runs its phase list
  `perf.microbench.rounds` times, each round on a table of its own, gating each
  phase's median. Set
  `apps.metrics.enabled=bool:false` to turn that off. `lifecycle` times the data
  lifecycle on a loaded tenant (fork off millions of rows, endpoint up on the
  fork, cold reads, pause/resume once `perf.lifecycle.pause_resume` is on, big
  delete, drop, branch and cluster delete), run over
  `perf.lifecycle.iterations` concurrent fresh tenants with each phase's median
  as the portal-gated datapoint. Four more take a stack of their own too:
  `ops_latency` times every control-plane operation on fresh objects over
  `perf.ops_latency.repeats` repeats (an operation with fewer samples than
  five, or than the best-sampled operation when repeats is below five, such as a
  cold endpoint create on a small stack, is reported and not gated),
  `chain_depth_read` times cold single-page
  reads at each delta-chain depth, gates each rung's p50 over the shallowest
  rung's in the same run (absolute latency follows pod placement), and also asserts the `image_chain_limit`
  contract outright, `branch_depth` measures cold and warm point reads, a cold
  scan, an insert, an update and a delete at each depth of a 20-deep fork chain,
  each write and the scan split into `perf.branch_depth.repeats` disjoint slices
  timed one statement each, a gated rung's cold scan run `perf.branch_depth.scan_passes`
  times on a rolled pod, and asserts the first fork's cold p50 within `perf.branch_depth.max_ratio` of
  the root's, and `idle_footprint` reads /proc inside an untouched
  endpoint's container for its per-process idle CPU and RSS, gating the median
  over the hold's intervals. `client_window` runs every operation
  `perf.client_window.repeats` times round-robin and gates the medians.
  `latency_topology` benchmarks one sysbench (or pgbench,
  `perf.latency_topology.workload`) stack unshaped and then under each
  `perf.latency_topology.topologies` entry, `perf.latency_topology.iterations`
  windows each, with tc netem delay injected into
  the tservers' and the endpoint's network namespaces so the sites are the
  configured round trips apart (multi-AZ, multi-region, the endpoint remote
  from its universe), every round trip verified with ping first.
  `stream_compression` stands one RF3 stack per `perf.stream_compression.modes`
  entry (gzip and none), each backing created with that mode, and runs a
  compressible seed, ANALYZE at each statistics target, cache-capped
  primary-key reads and an insert ramp on all of them in step. It gates every
  arm's numbers on history and reports gzip's ratio to none (meko#1856, #1857).

What one run can resolve, measured on the portal before the stacks were
isolated (40 runs per series, 50 or so live-arm pairs; 80% power, one-sided 5%):

| datapoint | run-to-run CV | arm ratio sd | smallest regression one run detects |
|---|---|---|---|
| `pgbench.tps` | 13% | 15% | ~36% |
| `tpcc.throughput` | 16% | 48% | ~120% |
| `lifecycle.fork_loaded` | 23% | 34% | ~85% |
| `idle.rss_mib_sum` | 0.6% | n/a | ~2% |

Isolation, pairing and the A/A band exist to shrink the middle column.
Re-measure it from the `.arm_delta_pct` series before trusting a green run to
mean less than the last column.

Example (one env at a time):

```
ampqa run --tag stress -c gke.yaml -o stress.combined.enabled=bool:true
ampqa run --tag perf   -c gke.yaml -o perf.enabled=bool:true
# A/A: both arms on the image under test, builds the noise model
ampqa run --tag perf   -c gke.yaml -o perf.baseline.aa=bool:true
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
`stress.backing_backup_restore.enabled`) — roughly an hour of backing-mutating
work in the `stress` lane.

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
> failure injection with asserted failure); there are none today.

Scheduled-lane failures are triaged, not auto-merge-blocking — except a
`regression`-tagged test failing there too, which is a real regression.

---

## Conventions

- **Three tier tags:** `regression`, `stress`, `perf`. Don't introduce others;
  `canary`/`presets`/`scale` and the extended tests stay untagged (selected by
  `--suite` / name / `--testr`).
- **PR gate = `--tag regression` only** — fast, deterministic, no cloud provisioning.
- `--tag regression` / `--tag stress` / `--tag perf` are the scheduled daily lanes,
  each needing its own env overlay and enable flag (see below).
- `ampqa describe <test>` shows a test's parameters; `ampqa collect --tag <t>` lists
  a tier; `ampqa manifest` is the machine-readable catalog for CI.
