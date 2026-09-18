# Temporal LRU findings

Phase 0 log. Observed facts, dated, newest at the bottom. Inferences are marked.

## 2026-09-03 bring-up

- `up.sh` created pool `lru-temporal`, cluster `temporal` (PG 18), endpoint
  `a310d544` at 2 vCPU / 16 GiB in under 2 minutes. Endpoint pod ready on first try.
- Temporal `auto-setup` 1.29.7 created `temporal_lru` and `temporal_lru_visibility`
  and ran both schema tools as `cloud_admin` on the first boot. 39 tables, 40
  connections open. No amp-specific error during schema setup.
- Three deploy fixes needed, all on the Temporal side, none on amp: dynamic config
  file missing from the image (ConfigMap added), Nexus callback template and
  `component.callbacks.allowedAddresses` unset (every Nexus operation failed until
  set), `enableServiceLinks` collision crashing the UI.
- Omes `throughput_stress`, 4 concurrent, 2 iterations/s: in the first 4 minutes of a
  clean cycle 1262 workflows, 878 completed, 12 failed. Endpoint pod 650 mCPU /
  810 MiB, Temporal pod 530 mCPU / 395 MiB, Omes 110 mCPU.
- Temporal logged 27 `Nexus StartOperation request failed: context deadline exceeded`
  in those 4 minutes, all against its own HTTP port 7243. Inferred: single-process
  Temporal under load, not the database. Watch whether the rate grows.
- `Queue reader unable to retrieve tasks: shard status unknown` 5 times at startup
  only. Inferred: shard acquisition during boot.

## 2026-09-03 monitoring up

- Prometheus, Grafana (`http://grafana-lru.tail5955.ts.net`) and the checker run in
  `amp-lru`. 27 amp endpoint scrape targets in `amp-dev` (9 endpoints x 3 exporters).
- After 20 minutes at 4 concurrent / 2 iterations per s: endpoint commits about
  490/s on `temporal_lru`, persistence p99 16 ms, persistence error ratio 0.03 %
  (all `CurrentWorkflowConditionFailedError`, which is Temporal's expected
  already-started path), workflow failure ratio 0, service error ratio 0,
  0.73 kitchen-sink workflows completing per s. No deadlocks.
- Omes SDK client metrics (`temporal_request*`) come out with scrambled label
  values (operation, namespace, task_queue rotate between series). Inferred: an
  Omes or Go SDK label ordering bug. The dashboard uses server-side metrics instead.

## 2026-09-03 dashboard load time

- Cold visit pulled **8.3 MB** of Grafana boot assets uncompressed; the two big
  chunks alone took 5.4 s and 4.6 s over the tailnet. `GF_SERVER_ENABLE_GZIP` is
  off by default. With it on the same eight assets are **2.5 MB** and the worst
  chunk drops from 5.4 s to 1.05 s.
- Warm load was already fine and stays so: Grafana boots in 0.47 s, the first
  panel query fires at 0.85 s, all seven above-the-fold panels finish at 2.2 s.
  Panel queries run 0.7 s median, 1.2 s worst.
- Assets carry `cache-control: max-age=31536000`, so the 8 MB was a first-visit
  cost only, which is exactly when someone forms an opinion of the page.
- Server side is idle: Grafana 6 mCPU / 116 MiB against a 500 mCPU / 512 MiB
  limit, Prometheus 11 mCPU / 192 MiB against 1 core / 2 GiB, 49k series. Tailnet
  RTT from a laptop is about 90 ms.

## 2026-09-03 restructure

- Directory renamed to `small_temporal`; every resource is now `small-temporal-*`
  and labelled `lru=small_temporal`. Prometheus and Grafana moved to the shared
  `../monitoring` base and discover LRUs by pod annotation and dashboard
  ConfigMap. Same Grafana URL. The Temporal memory limit went from 4 to 8 GiB,
  since the pod sat at 99 % of 4 GiB, the shape of the two earlier OOMs.
- Omes rebuilt on the new pod as expected; the portal verdict came back `ok`
  through the new `lru="small_temporal"` selector on the first sync.

## 2026-09-04 review fixes

- `lru_overall_status` counted only rules that ran. A rule whose query returned
  nothing became ERR and dropped out of the minimum, so an unreadable endpoint
  could read as green. It is now the minimum over every rule.
- The checker resolves its endpoint from the controller by pool instead of taking
  a configured id, which removes the sed that rewrote a tracked file and the
  leading-comma PromQL error when the id was unset. New rule
  `amp_endpoint_resolved` reports when the pool has no endpoint.
- Prometheus RBAC narrowed from a cluster-wide pods read to a namespace Role in
  each of the two namespaces it discovers. Grafana's admin password moved from a
  manifest constant to a generated Secret.
- Checked against the bot's other two claims: the `sed` on `kustomization.yaml`
  was idempotent (removed anyway), and the endpoint's pull secret is
  `quay-pull-secret`, a long-lived quay credential from 2026-07-08, not a ~12 h
  ECR token.

## 2026-09-04 the endpoint alias was never patched

The LRU was down for its whole first day: Temporal could not acquire a single
shard and Omes died on every cycle, both on
`lookup small-temporal-endpoint ... no such host`. All four pods stayed `Running`,
so only the portal verdict showed it.

`svc/small-temporal-endpoint` still carried the `placeholder.amp-dev.svc.cluster.local`
that `endpoint.yaml` shipped through kustomize. `up.sh` guarded its patch on
`get svc amp-endpoint`, a different service that the else branch was supposed to
create. It is not in the namespace today, so the guard never passed and the alias
the manifests actually resolve was never touched.

The deeper problem was that the alias was a kustomize resource at all. A tracked
manifest can only carry a placeholder, so every `kubectl apply -k .` — a path
`README.md` documents for changing an Omes knob — reset the alias and would have
reproduced this outage even with the guard fixed. `up.sh` now applies the alias
imperatively with the resolved externalName and it is no longer a kustomize
resource. Nothing detects the failure on its own: the rollout status passes because
the pod is Running, and the checker resolves the endpoint from the controller API
rather than through the alias, so `amp_endpoint_resolved` stays green.

The amp side was healthy throughout: pool `lru-temporal`, endpoint
`a310d544-39a4-45c5-bfaf-b5f6d8c0b592`, its deployment 1/1 the whole time.

## Open

- 12 failed workflows per 4 minutes: confirm they are the Nexus deadline cases and not
  persistence errors.
- Endpoint memory curve over 24 h at this rate.
- `amp_storage.pages` and `wals` growth for branch `ae980ead-a32c-4404-8188-11c1a1424d86`.
- Mark's Omes command line on `vmload2a`, to align scenario and rate.

## 2026-09-11 - 99.98% of the endpoint file cache, hours of headroom left

Stable for six days on every application measure: 5.8 workflows/s, persistence
p99 flat at 18.8 ms, zero service errors, no restarts. Nothing in the workload
suggests a problem.

The capacity rules added in this commit say otherwise. Logical size is **42.94 GB
against a 42.95 GB file-cache limit**, growing 1.4 GB/day, so
`days_until_file_cache_full` reads **0.007 days**. It is at the boundary now.

The transferwise LRU crossed the same line on 09-11 and lost 44% of its
throughput over the following fourteen hours, with the same 8 GiB
`shared_buffers` and the same 1 GiB `max_wal_size` underneath. This one writes
far less WAL (1.4 MB/s, zero forced checkpoints so far), so the same collapse is
not certain. It is the obvious thing to watch.

Recorded before anything degrades, which is the point: the prediction is on
record and either the mechanism generalises or it does not.

## 2026-09-17 `omes_up` was our rule, not an outage

`omes_up` FAILed 13 times in 48h, one minute each, spaced 2h00m18s apart and
drifting a minute later each time. Nothing was wrong.

omes runs `throughput_stress` in two-hour blocks. Between blocks it stops the
worker, rebuilds the Go binary and starts a new one, leaving both metrics ports
closed for about 32 seconds:

```
16:50:31 Run completed in 2h0m18.245282923s
16:50:37 Sending interrupt to worker, PID 18871 ... Worker has been stopped
16:51:07 Building go program at /work/src/workers/go/omes-temp-4287630458
16:51:09 Starting worker ... Process metrics sidecar started on 0.0.0.0:9091
```

Both targets, `:9090` and `:9091`, failed the same single scrape and no other.
At each one `workflow_success` held 1.64-1.75/s, persistence p99 18.3-19.2 ms,
error ratio 0.0002, and every amp-endpoint scrape was up, so the database was
never involved. Scrape durations are 2-29 ms typical, 211 ms worst over 48h,
so nothing was timing out either.

The defect was reading one instantaneous `up` sample and calling a zero a FAIL.
Eight rules across the four checkers did this: `omes_up`, `ledger_up`,
`bomber_up`, `card_up` and `amp_endpoint_up`. All now go through `liveness()`,
which asks whether the target answered any scrape in a window: 5m for the
workload drivers, which restart on their own schedule, 3m for the endpoint.

The cost: a roll that takes an endpoint down for about a minute no longer FAILs
`amp_endpoint_up`. `persistence_error_ratio` and `persistence_p99_s` are what
actually caught the 15 Sep roll, and they still do.

## 2026-09-17 first branch probe: warm reads cost ~1.4x on a depth-1 fork

First cycle of `branch-probe`. Fork took 5.1s, its endpoint 15.0s, teardown 10.1s.

| probe | parent warm | branch warm | ratio | parent cold | branch cold | ratio |
|---|---|---|---|---|---|---|
| `executions_point` | 0.163 ms | 0.220 ms | 1.35 | 0.217 ms | 122.8 ms | 566 |
| `history_node_range` | 0.197 ms | 0.264 ms | 1.34 | 1.47 ms | 17.3 ms | 11.7 |
| `history_tree_scan` | 0.145 ms | 0.212 ms | 1.46 | 0.957 ms | 7.21 ms | 7.5 |
| `shards_point` | 0.134 ms | 0.209 ms | 1.56 | 0.149 ms | 4.66 ms | 31.3 |

The cold column is not a fork measurement. The parent has weeks of file cache and
the fork's endpoint was seconds old, so 566x is mostly cache warmth. Read it as an
upper bound on what a fresh endpoint costs, not as fork overhead.

These particular cold numbers overstate it further: this cycle sampled its
predicates on the parent about a minute before reading it, so the parent's cold
pass was a second touch of pages the probe had just faulted in.

**The warm row above is wrong, and the next entry retracts it.**

The warm column is the real number, and it is consistent: 1.34-1.56x across four
unrelated access patterns, all at sub-millisecond absolute latency. Same session,
second execution of the same statement, so both sides are serving from
`shared_buffers`. The parent is also carrying live Temporal traffic while the fork
is idle, which biases against the fork being the slower one, so 1.4x is if anything
conservative.

One cycle. The point of running it daily is to find out whether 1.4x is stable.

## 2026-09-17 retraction: warm reads on a depth-1 fork cost nothing measurable

The 1.34-1.56x warm ratio above does not survive fixing the probe's own
methodology. Sampling moved to the fork and the parent is now read first, so
nothing touches the parent before its own pass and the fork gets the length of
the parent's pass to settle. Same four probes, next cycle:

| probe | warm ratio before | warm ratio after | cold ratio after |
|---|---|---|---|
| `executions_point` | 1.35 | 0.99 | 61.0 |
| `history_node_range` | 1.34 | 0.99 | 13.6 |
| `history_tree_scan` | 1.46 | 1.00 | 1.12 |
| `shards_point` | 1.56 | 0.96 | 1.01 |

Warm is 0.96-1.00 across all four, so once the pages are in `shared_buffers` a
depth-1 fork costs nothing this probe can measure. That matches the depth sweep
in the branch-depth thread, where warm p50 was flat at 0.007 ms from depth 0 to
20, and it is the earlier 1.4x that was the outlier. The most likely cause of
the old number is that the fork's endpoint was seconds old when it was measured
first; reading the parent first gives it the parent's pass to finish its own
startup work.

Cold still splits by access path, and the split is informative. The two probes
that go through an index are 61x and 13.6x; the two that do not are ~1.0. The
sampler runs on the fork and `TABLESAMPLE SYSTEM` reads heap blocks, so the
fork's heap is warm for exactly these rows while its index pages are not. What
is left in the cold column is therefore mostly index page misses on a fresh
endpoint, which is the read-across-levels path that is not pushed down today.

One cycle each way. The daily run is what decides whether this holds.
