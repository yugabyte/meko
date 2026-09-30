# The LRU environment: what runs where

The map of the always-on amp workloads and everything they stand on, for whoever
has to find, read or fix one without having been in the room. `deploy/lru/README.md`
is the authority on the *shape* an LRU has, this is the authority on *where the
pieces are*, and each LRU's `FINDINGS.md` is the authority on what it has shown.

No endpoint ids, ELB hostnames or account numbers here. Those change under us and
the portal row is where they live.

## Three repositories, one system

| Repo | Holds |
|---|---|
| `yugabyte/meko` | amp itself (`amp/`), the LRU manifests and checkers (`amp/deploy/lru/`), the QA framework (`amp/test/`) |
| `yugabyte/meko-devops` | the Jenkins pipelines: `amp-deploy` (promotion), `amp-lru-sync` (hourly verdict), `amp-build`, `amp-run`, `amp-reap` |
| `yugabyte/qa-services` | the amp report portal (`amp-report-service`: backend, Angular frontend, MCP server) and the `yb` CLI |

A change to an LRU touches the first, a change to how verdicts reach the portal
the second, a change to how the portal shows them the third.

## The cluster

One EKS cluster, `meko_dev_eks`, in `us-east-2`, 29 nodes on Kubernetes 1.34.
Kube context of the same name. Everything below is on it.

```
namespace        what
amp-dev          the amp control plane and every endpoint pod (yb-ep-<id>)
amp-lru          the LRU workloads, checkers, Prometheus, Grafana, amp-time
yb-dev-amp2-db   the YugabyteDB universe amp stores into: universe amp2-db,
                 3 masters, 3 tservers, managed by YBA
tailscale        the operator that puts services on the tailnet
```

The other `amp-*` and `meko-*` namespaces are individual people's environments and
are not part of this. `amp-bench` is a **different cluster** entirely, used for
benchmark stacks, not for LRUs.

Reach into the cluster with `kubectl --context meko_dev_eks`. Reach the UIs through
the tailnet, `*.tail5955.ts.net`. Nothing here is on the public internet.

## The amp deployment

`amp-dev` is the standing dev deployment: the one a release is promoted onto and the
one every LRU's endpoint lives in.

**Control plane.** `amp-controller` on `:8080` (`/api/v1`, basic auth `admin:admin`),
with `amp-controller-ui` and `amp-controller-mcp` beside it, all three on the tailnet
under those names. Deployed by the `yb-amp-controller` Helm chart from `amp/deploy/helm/`.

**Storage.** The controller keeps its registry and every endpoint's pages in the
`amp2-db` universe, database `yb_storage`, through
`ybamp2-db-dfac-yb-tservers.yb-dev-amp2-db.svc.cluster.local:5433`.

**Endpoints.** One pod per endpoint, `yb-ep-<id>`, image `quay.io/yugabyte/amp-endpoint:<tag>`.
The pod's single `endpoint` container runs compute (`:55433`), the pageserver (`:9898`)
and the safekeeper (`:7676`) together, with a `postgres-exporter` sidecar on `:9187`.
Rolling the endpoint image therefore rolls all three, and the image tag is the
pageserver version too.

**Versions, and the trap.** Promotion through the portal runs the `amp-deploy` job,
which helm-upgrades the control plane and sets `AMP_ENDPOINT_IMAGE_TAG`. That tag is
only the default for endpoints created *afterwards*. **Existing endpoints keep the
version on their row until an explicit roll:**

```
POST /api/v1/endpoint-upgrades
{"version": "<tag>", "endpoint_ids": ["<id>", ...], "max_parallel": 1}
```

Omit `endpoint_ids` to roll the whole fleet, which on a shared deployment means other
people's endpoints too. Named ids are never skipped, a fleet roll skips endpoints
already at `version`. Once a roll leaves every endpoint at the controller's version,
further rolls are refused (`409` `fleet_at_version`, `GET /api/v1/fleet-version`
shows it). One roll at a time (`409` otherwise), fail-fast, each pod
replaced so each endpoint is down about a minute. Nothing in the portal, the `yb` CLI
or the MCP server calls this route; it is the controller API directly. Spec:
`amp/controller/internal/api/openapi_spec.yaml`, https://github.com/yugabyte/meko/issues/809.

Nothing tracks this drift. On 15 Sep the fleet had ten distinct endpoint versions,
the oldest from 18 Aug, and it still does. The four LRU endpoints were rolled that
day and are the only ones anybody keeps current.

## The LRUs

Four, all following the shape in `deploy/lru/README.md`: their own pool, an
`ExternalName` Service `<name>-endpoint` pointing at the endpoint, pods labelled
`lru=<name>`, a checker, a dashboard, a portal row.

| LRU | pool | endpoint | workload | manifests |
|---|---|---|---|---|
| `small_temporal` | `lru-temporal` | 2 vCPU / 16 GiB | Temporal server 1.29 persisting to amp, Omes load | `deploy/lru/small_temporal/` |
| `newblack` | `lru-newblack` | 4 vCPU / 32 GiB | anonymised NewBlack checkout, C# bomber | `deploy/lru/newblack/` |
| `solidgate` | `lru-solidgate` | 2 vCPU / 16 GiB | anonymised Solidgate card payment, Go, paced at 50 txn/s | `deploy/lru/solidgate/` |
| `transferwise` | `lru-transferwise` | 2 vCPU / 16 GiB | anonymised TransferWise ledger, `ledger_bench`, one write thread | `deploy/lru/transferwise/` |

Databases `temporal_lru`, `newblack_lru`, `solidgate_lru`, `transferwise_lru`, all as
`cloud_admin`. Each `up.sh` is idempotent and creates the pool, cluster and endpoint
through the controller before applying the manifests.

Three of them are shaped unusually:

- `small_temporal` is the only one running a real server rather than a benchmark
  driver, so it is the one that notices an endpoint going away. It also carries the
  **branch probe**, which forks its branch daily and compares query latency on the
  fork against the parent. Its omes load generator restarts its scenario every two
  hours by design, which is why liveness rules use a window rather than one sample.
- `transferwise` carries a `retention` container deleting exactly what the customer
  deletes (the outbox and processed jobs), and has been the source of most incidents.
  Its `FINDINGS.md` is the longest.
- `newblack` is the only 4 vCPU endpoint, and the only workload whose dataset is a
  small fraction of its file cache.

`kubectl -n amp-lru get all -l lru=<name>` is the whole of one LRU.

## Shared observability, all in `amp-lru`

| | where | what |
|---|---|---|
| Prometheus | `prometheus:9090` in-cluster | scrapes `amp-lru` pods by annotation, every endpoint pod in `amp-dev`, and per-container memory from each node's kubelet; 30 days / 15 GB |
| Grafana | `grafana-lru.tail5955.ts.net` | one dashboard per LRU, provisioned from ConfigMaps; anonymous viewer, `grafana-admin` Secret to edit |
| amp-time | `amp-time-<lru>.tail5955.ts.net` | Mark's `pg_stat_statements` browser, one instance per LRU, 7 days of 10s timeslices in SQLite on a shared PVC; vendored under `deploy/lru/amp-time/` |
| `pgstats` | sidecar in each checker pod, `:9096` | the Postgres internals amp's exporter omits: checkpointer, per-table size, analyze age |

`monitoring/up.sh` applies Prometheus and Grafana, `amp-time/up.sh` applies amp-time.

Two things about the Grafana dashboards that cost time when they are not known. The
sidecar names each file on disk after the **ConfigMap key**, and every LRU writes into
one shared directory, so keys must be unique per LRU or the dashboards overwrite each
other once a minute. And each dashboard resolves its endpoint through a hidden
variable from `amp_endpoint_info{default="true",pool="..."}` — the `default` filter
matters, because a fork's endpoint sits in the same pool and would otherwise be a
candidate whenever the pinned endpoint id no longer exists.

## The verdict path

Each checker evaluates its rules once a minute and exports `lru_check_status{lru,check}`
and `lru_overall_status{lru}`, where **1 is OK, 0.5 WARN, 0 FAIL and -1 ERR**, and the
overall status is the minimum. Once an hour the `amp-lru-sync` Jenkins job
(`meko-devops/jenkins/amp/`) reads every active portal row, execs into that LRU's
checker, queries Prometheus for the status, and posts `ok` / `not_ok` / `error` with
the failing and warning rule names.

The exec target is whichever pod the row names, and it only has to be able to reach
Prometheus. Three rows name `deploy/<name>-checker`; small_temporal still names
`deploy/small-temporal-omes`, from before the checkers existed. Both work.

The portal is https://amp-qa.dev.yugabyte.com/lru, its MCP server at `/mcp`, the CLI is
`yb amp lru`. A row stores the definition and one verdict per hour, nothing else. The
metrics stay in Prometheus.

Rules are **advisory** (WARN, never FAIL) when they compare an LRU with its own past or
measure capacity, because they cannot tell a regression from a deliberate change. FAIL
is reserved for the workload not doing its job. One consequence worth knowing before reading a
green badge: an LRU can sit at WARN for days and still report `ok` every hour.

ERR goes the other way, and the direction is deliberate. A rule that *should* have a
measurement and does not is ERR, and ERR drags `lru_overall_status` down, because a
rule that cannot run is evidence of a problem rather than evidence of health. That is
what `amp_endpoint_resolved` is for. A rule with no *history* yet is simply left out of
the rule set instead.

Liveness rules (`ledger_up`, `bomber_up`, `card_up`, `omes_up`, `amp_endpoint_up`) ask
whether the target answered **any** scrape in a window, 5m for workload drivers and 3m
for the endpoint, not whether it answered the last one. A single missed scrape is not an
outage, and treating it as one produced 13 false FAILs in 48h.

## Operating it

```bash
# what is running
kubectl --context meko_dev_eks -n amp-lru get pods
yb amp lru list
yb amp lru get <name>

# one LRU
kubectl -n amp-lru get all -l lru=transferwise
kubectl -n amp-lru logs deploy/transferwise-runner -c ledger --tail=30
yb amp lru sync transferwise --yes

# change one: edit under deploy/lru/<name>/, then
(cd amp/deploy/lru/<name> && kubectl --context meko_dev_eks apply -k .)
kubectl -n amp-lru rollout restart deploy/<name>-checker   # ConfigMap names are stable

# which version is each endpoint on (the tag is also the pageserver version)
kubectl --context meko_dev_eks -n amp-dev get pods -l app.kubernetes.io/name=yb-amp-endpoint \
  -o custom-columns='POD:.metadata.name,IMAGE:.spec.containers[0].image'

# ad-hoc PromQL without leaving the cluster
kubectl -n amp-lru exec deploy/transferwise-checker -c checker -- \
  curl -sG http://prometheus:9090/api/v1/query --data-urlencode 'query=lru_check_status < 1'

# psql against an LRU's endpoint, from the checker's pgstats sidecar
kubectl -n amp-lru exec -i deploy/small-temporal-checker -c pgstats -- sh -c \
  'PGPASSWORD=cloud_admin psql -h small-temporal-endpoint -p 55433 -U cloud_admin -d temporal_lru -f -'
```

**Merging a PR does not deploy anything.** Nothing watches git. After a merge, re-apply
the kustomizations and roll the checkers by hand, or the cluster and `main` drift apart
silently.

## Reading the state quickly

The portal badge answers "is it broken". These answer "what is it doing", and are the
first three things to run when picking this up cold:

```bash
# every check not at OK right now, across all four
... --data-urlencode 'query=lru_check_status < 1'

# how many minutes each check spent below OK in the last day
... --data-urlencode 'query=count_over_time((lru_check_status < 1)[24h:1m])'

# the drift numbers: >1 means slower or bigger than the reference
... --data-urlencode 'query=lru_check_value{check=~"throughput_vs_24h_ago|latency_vs_7d_baseline"}'
```

A standing WARN is normal and is not the same as a new one. `count_over_time` at 1440
means it warned for the entire day, which is a steady state somebody already knows
about; a number in the tens is the one worth chasing.

## What has happened, briefly

The record is in each LRU's `FINDINGS.md`. This is the index.

- **3 Sep** — `small_temporal` first, then `newblack`.
- **10 Sep** — `transferwise` and `solidgate` join.
- **11 Sep** — transferwise loses 44% of its throughput over fourteen hours while green
  on the portal. Cause: dataset growth crossing the endpoint's file cache at a 4-thread
  write rate, not `max_wal_size` as first reported. Out of it came the drift rules, the
  `pgstats` sidecar and the capacity rules.
- **12 Sep** — transferwise goes to one write thread with the customer's own retention.
  A pool sized to one connection then starves the driver behind ANALYZE for a day, fixed
  by giving the pool a connection per worker. amp-time deployed for all four. ANALYZE
  measured at 25-31s per 20M-row table.
- **14 Sep** — pageserver GC lag on transferwise, rising 11:27 to a 20.3 GB peak and
  back to zero by 05:47 the next day: a long duty cycle, not the stall I first reported.
  Two of our own rules found wrong and replaced, one measuring
  `checkpoint_completion_target` as if it were load, the other comparing against a
  window shorter than the decay it was meant to catch.
- **15 Sep** — all four LRU endpoints rolled to `0.260914.0-5f4bc418e`; the fleet had
  drifted to ten versions. Data invariants held across the roll.
- **17 Sep** — the `omes_up` blink explained: omes runs its scenario in two-hour blocks
  and rebuilds its worker between them, ~32s with no metrics listener. Eight liveness
  rules were reading one sample and calling a scheduled gap a FAIL. amp-time found
  OOMKilling at 1Gi, raised to 2Gi, and a cAdvisor scrape added because nothing measured
  per-container memory. The branch probe landed, and its first result was retracted a
  day later when the probe's own sampling order turned out to be the thing it measured.

Standing results worth knowing: `small_temporal` has run past its file cache since
12 Sep with no measurable effect, and transferwise runs at ~4x its cache at full
throughput. Crossing the cache is what moved across the 11 Sep
inflection, but it is not sufficient on its own, and what else is required has not
been established: small_temporal crossed with no effect, at a far lower write rate.
solidgate crossing at a fixed rate is the next data point. A depth-1 fork costs
nothing measurable on warm reads.

## Open, with no owner

- **Endpoint version drift.** Promotion does not touch endpoints and nothing reports the
  spread. The checkers already resolve their endpoint from the controller every five
  minutes, so comparing its version against the controller's default is a few lines away.
- **transferwise latency is drifting up.** `latency_vs_7d_baseline` went 1.87 (14 Sep) to
  2.28 (17 Sep) with no step change and no other rule moving. Slow, unexplained, and the
  reason that rule exists.
- **solidgate is about to cross its file cache**, roughly a week out at the 17 Sep growth
  rate. It is the cleanest chance to test the 11 Sep hypothesis on a workload with a
  fixed rate, since crossing alone should do nothing.
- **The GC drain-lag threshold is unmeasured.** The rule fails above 10 GB, chosen
  before any cycle was seen, and the one full cycle peaked at 20.3. Nothing has cycled
  since 15 Sep 05:47 against a prior cadence of about a day, and the endpoint roll that
  evening is not evidence either way: the drain had already finished before it.
- **amp-time memory.** Whether it leaks or just tracks a filling 7-day store is now
  measurable through cAdvisor and was not before; the stores first hit their retention
  cutoff around 19 Sep.
