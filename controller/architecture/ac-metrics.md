# ac — Metrics

**Status:** Draft
**Last updated:** 2026-07-14

How amp emits metrics for its compute instances, the controller, and
the amp controller, and how they reach a metrics store. The guiding finding:
**most of the stack already emits Prometheus-shaped metrics** (the Neon-derived
Rust services), so this is largely a *collect-and-export* problem, not a
re-instrument-everything one. The one real instrumentation gap is the Go amp
controller — closed by step 1 below. This repo now implements the two source-side
steps (step 1, the controller `/metrics`; step 3, the compute `postgres_exporter`
sidecar); the collector (step 2) is the remaining deployment step.

## Pipeline

```
 sources (expose Prometheus /metrics)        collect            store          visualize
 ┌───────────────────────────────┐
 │ amp controller        :8080    │─┐
 │ controller    (http)   │ │        ┌──────────────┐   ┌──────────────┐   ┌─────────┐
 │ compute pod                    │ ├──scrape┤ OTel         ├──►│ VictoriaMetrics├──►│ Grafana │
 │   pageserver        :9898      │ │        │ Collector    │   │ (remote-write) │   │         │
 │   safekeeper        :7676      │ │        │ (Operator)   │   └──────────────┘   └─────────┘
 │   endpoint_storage  :9993      │ │        └──────────────┘
 │   compute_ctl       :3080      │ │
 │   postgres_exporter :9187      │─┘
 └───────────────────────────────┘
```

The Collector **scrapes** each component's `/metrics` and **remote-writes** to
the store. Because every source is Prometheus-shaped, it maps 1:1 to a
Prometheus-compatible store with no translation.

### Store choice

**VictoriaMetrics** (Prometheus remote-write) + Grafana is the recommendation:
the components already emit Prometheus with upstream PromQL dashboards, VM is
dense/cheap at the per-endpoint / per-tenant label cardinality expected here,
and a VM stack already runs in the environment. CloudWatch (`awsemf` exporter)
is the documented alternative — zero new infra, but custom-metric pricing grows
with high-cardinality labels, so it is a stopgap, not the long-term home. The
controller's instrumentation (step 1) is **store-agnostic**: it just exposes
`/metrics`; nothing here commits to a store.

## Step 1 — amp controller `/metrics` (implemented)

The Go controller had no metrics. It now exposes a Prometheus scrape endpoint on
the **main HTTP listener** (`--listen`, default `:8080`) at **`GET /metrics`**,
alongside `/healthz` and `/readyz` and outside the versioned API and the
OpenAPI spec. It is served in-cluster and scraped by the Collector; there is no
separate metrics port.

- **Owner:** [`internal/metrics`](../internal/metrics/metrics.go) holds a private
  `prometheus.Registry`, the series definitions, and nil-safe recording helpers.
- **HTTP:** [`internal/api`](../internal/api/server.go) adds a middleware that
  records every request, and mounts the `promhttp` handler at `/metrics`.
- **Tasks:** [`internal/task`](../internal/task/worker.go) reports task-lifecycle
  events through a small `MetricsRecorder` interface (so the task package keeps
  no dependency on `internal/metrics`); the worker's claim loop instruments each
  task it runs.
- **Toggle:** `--metrics` / `AMP_METRICS` (default **true**). When false the
  registry is nil, `/metrics` is not mounted, and all recording is a no-op.

### Series exposed

All under the `amp_controller_` namespace, following the Prometheus
`namespace_subsystem_name` convention.

| Metric | Type | Labels | Meaning |
|---|---|---|---|
| `amp_controller_http_requests_total` | counter | `method`, `route`, `status` | Requests handled. `route` is the matched route **template** (e.g. `/api/v1/pools/:poolId`), never the raw URI, so path parameters don't explode cardinality. The `/metrics` scrape itself is excluded. |
| `amp_controller_http_request_duration_seconds` | histogram | `method`, `route` | Request latency (default buckets). |
| `amp_controller_tasks_total` | counter | `kind`, `status` | Tasks the worker finished, by outcome (`succeeded`, `failed`, `interrupted`). |
| `amp_controller_task_duration_seconds` | histogram | `kind` | Task execution time (terminal outcomes only; an interrupted task did partial work and stays reclaimable, so its partial time is not observed). |
| `amp_controller_tasks_inflight` | gauge | — | Tasks currently executing. |
| `amp_controller_registry_branches` | gauge | — | Branches currently recorded in the registry. |
| `amp_controller_build_info` | gauge | `version`, `commit`, `build_date` | Constant 1; carries the build identity (from [`internal/version`](../internal/version), see `ac-versioning.md`) for joining onto other series. |

Plus the standard Go runtime (`go_*`) and process (`process_*`) collectors.

The `registry_branches` gauge is **collected at scrape time**: a
`prometheus.Collector` ([`internal/metrics/registry_stats.go`](../internal/metrics/registry_stats.go))
runs `count(*)` over `registry.branches` when `/metrics` is
scraped, so there is no cached value to go stale and no background poller. If the
count query fails it logs and emits nothing for that scrape rather than a wrong
value. Everything else is recorded inline by the HTTP middleware and the task
worker.

## Step 2 — OTel Collector (implemented)

Two **`OpenTelemetryCollector` CRs** (reconciled by the **OpenTelemetry
Operator**) ship with the control-plane chart
([`deploy/helm/yb-amp-controller`](../../deploy/helm/yb-amp-controller),
`templates/otel-collectors.yaml`, gated by `otel.enabled`), both `mode:
deployment`, remote-writing to a Prometheus-compatible store:

- **`<release>-controller`** — scrapes the amp controller (`:8080`) and its in-pod
  controller (`:50051`).
- **`<release>-compute`** — scrapes every endpoint pod: the postgres_exporter
  sidecar (`:9187`), pageserver (`:9898`), safekeeper (`:7676`), compute_ctl
  (`:3080`), and (with `otel.podMetrics`) per-pod cAdvisor resource metrics from
  each node's kubelet.

The chart also creates the collector `ServiceAccount` + a `ClusterRole`/binding
for pod and node discovery. Store is set via `otel.remoteWriteEndpoint`
(VictoriaMetrics by default). **Requires the OTel Operator** (opentelemetry.io
CRDs) in the cluster.

Implementation notes learned deploying this on EKS:
- Use the **contrib** collector image (the `prometheus` receiver +
  `prometheus_remote_write` exporter aren't in the core distro).
- cAdvisor: scrape the kubelet **directly** at `nodeIP:10250/metrics/cadvisor` —
  the OTel prometheus receiver silently drops everything when scraping via the
  API-server proxy path.
- Counter metric names gain a `_total` suffix through OTel normalization
  (`pg_stat_database_xact_commit_total`); dashboards must use the suffixed names.
- pageserver `/metrics` emits some duplicate series per scrape → benign
  "different value, same timestamp" warnings (those samples are dropped).

Note on the endpoint pod's internal services: **pageserver (`:9898`), safekeeper
(`:7676`), endpoint_storage (`:9993`), and compute_ctl (`:3080`) already serve
`/metrics`**, but their container ports are only exposed when
`service.exposeInternalHttp=true` or the endpoint is SC-coordinated. To scrape
them, the Collector's pod service-discovery must target those ports (the
`postgres_exporter` `prometheus.io/*` annotations cover only `:9187`); a
multi-port scrape config or per-port annotations is the step-2 work. The
`controller` and the amp `controller` (`:8080`) are scraped directly.

## Step 3 — compute Postgres metrics (implemented)

For downstream performance tooling, the **`postgres_exporter`** sidecar now runs
in the k8s endpoint pod (the exporter already ships in the compute VM image at
`v0.17.1`, but the pod did not run it). Implemented in the endpoint Helm chart,
[`deploy/helm/yb-amp-endpoint`](../../deploy/helm/yb-amp-endpoint):

- **Sidecar** (`deployment.yaml`, `metrics.postgresExporter` in `values.yaml`) —
  `quay.io/prometheuscommunity/postgres-exporter:v0.17.1`, serving `/metrics` on
  `:9187`. It connects to the compute's Postgres over the pod's **shared
  loopback** (`host=localhost port=<computePort>`) as `cloud_admin` via **trust
  auth** — the same connection the VM-image exporter uses — and runs with
  `--no-collector.database` (that collector errors against the storage layer).
  **On by default**; disable with `metrics.postgresExporter.enabled=false`.
- **Scrape discovery** — the pod is annotated `prometheus.io/scrape` +
  `prometheus.io/port` (not published on the endpoint Service, which may be a
  LoadBalancer / tailnet-exposed), so the Collector scrapes it via pod
  service-discovery. Because the controller deploys endpoints with a values file
  that omits the `metrics` block, controller-provisioned endpoints inherit this
  default automatically.
- **Built-in collectors** give `pg_stat_database` (commits/rollbacks, cache-hit,
  tuples, deadlocks, temp files), bgwriter/checkpointer, connections, locks, DB
  size, and replication with no extra config.

### Query-level metrics — `pg_stat_statements` (opt-in)

The richest perf signal (per-normalized statement `calls`, total/mean/max exec
time, rows, block hits vs reads, WAL bytes) comes from the exporter's built-in
`stat_statements` collector, enabled with
`metrics.postgresExporter.extraArgs: ["--collector.stat_statements"]`. It is
**off by default** because it requires the extension, which the compute does not
load by default (it preloads `yb_amp,pgaudit`). To turn it on:

1. Add `pg_stat_statements` to `endpoint.sharedPreloadLibraries` (a preload lib →
   needs a compute restart).
2. `CREATE EXTENSION pg_stat_statements` in the target database.
3. Set the `--collector.stat_statements` exporter arg above.

`queryid` is high-cardinality, so scope/cap it in the Collector's relabel rules
if the series count is a concern.

## Rollout order

All three steps are implemented:

1. **Controller `/metrics`** — ✅ (`controller/internal/metrics`).
2. **OTel Collector** — ✅ (`OpenTelemetryCollector` CRs in the control-plane
   chart; requires the OTel Operator + a store, VictoriaMetrics by default).
3. **Compute `postgres_exporter` sidecar** — ✅
   (`deploy/helm/yb-amp-endpoint`); `pg_stat_statements` is opt-in.

Steps 1 and 3 are store-agnostic (they only expose `/metrics`); step 2 picks
the store.
