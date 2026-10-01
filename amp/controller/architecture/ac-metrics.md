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

Logs are the sibling pipeline, sharing the collector deployment, the RBAC and the
`endpoint` label convention but nothing else — see [`ac-logs.md`](ac-logs.md).

## Pipeline

```
 sources (expose Prometheus /metrics)        collect            store          visualize
 ┌───────────────────────────────┐
 │ amp controller        :8080    │─┐
 │ controller    (http)   │ │        ┌──────────────┐   ┌──────────────┐   ┌─────────┐
 │ compute pod                    │ ├──scrape┤ OTel         ├──►│ VictoriaMetrics├──►│ Grafana │
 │   pageserver        :9898      │ │        │ Collector    │   │ (remote-write) │   │         │
 │   safekeeper        :7676      │ │        │ (Operator)   │   └──────────────┘   └─────────┘
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
| `amp_controller_tasks_reclaimed_total` | counter | — | Tasks this process took over that another controller process left `running`: it crashed or lost its lease, or shut down with them in flight, so a restart during work counts too. Unlabelled, so it reads 0 from startup. **Read it alongside `process_start_time_seconds`, not alone:** the controller runs one replica, so a reclaim lands in a process that has just started, often before its first scrape, and `increase()` has no earlier sample to measure it from. |
| `amp_controller_tasks_step_retries_total` | counter | `step_kind` | Step re-runs after losing a write interlock (SQLSTATE 40001 — see the interlock-retry invariant in `ac-tasks.md`). Contention, not breakage: a fork racing a GC cutoff advance or a sibling fork is expected to lose sometimes. The label is **`step_kind`** (a `task_step.kind`, e.g. `record_branch_recipe`), *not* the `kind` of `tasks_total` (a task kind, e.g. `branch`) — the two are different name spaces, so joining them yields nonsense. What to watch is this counter's rate against `tasks_total{status="failed"}`: retries rising while failures stay flat is contention being absorbed, which is the mechanism working. |
| `amp_controller_registry_branches` | gauge | — | Branches currently recorded in the registry. |
| `amp_controller_build_info` | gauge | `version`, `commit`, `build_date` | Constant 1; carries the build identity (from [`internal/version`](../internal/version), see `ac-versioning.md`) for joining onto other series. |
| `amp_controller_gc_sweeps_total` | counter | — | Completed failed-create GC sweeps, one per tick regardless of how many resource kinds it swept (`ac-tasks.md`). |
| `amp_controller_gc_sweep_duration_seconds` | histogram | — | Sweep duration, covering every kind's selection plus each per-orphan precheck and enqueue. |
| `amp_controller_gc_orphans_total` | counter | `resource`, `outcome` | Failed-create orphans the sweeper saw, by `resource` (`cluster`, `endpoint`) and `outcome` (`enqueued`, `skipped`, `failed`). A flat `orphans_total` against a rising `sweeps_total` means the sweeper is running and idle rather than stuck. |

Plus the standard Go runtime (`go_*`) and process (`process_*`) collectors.

The `registry_branches` gauge is **collected at scrape time**: a
`prometheus.Collector` ([`internal/metrics/registry_stats.go`](../internal/metrics/registry_stats.go))
runs `count(*)` over the live (not tombstoned) rows of `amp_registry.branches` when `/metrics` is
scraped, so there is no cached value to go stale and no background poller. If the
count query fails it logs and emits nothing for that scrape rather than a wrong
value. Everything else is recorded inline by the HTTP middleware and the task
worker.

## Step 2 — OTel Collector (implemented)

Two **`OpenTelemetryCollector` CRs** (reconciled by the **OpenTelemetry
Operator**) ship with the control-plane chart
([`deploy/helm/yb-amp-controller`](../../deploy/helm/yb-amp-controller),
`templates/otel-collectors.yaml`), both `mode: deployment`, remote-writing to a
Prometheus-compatible store. They render when `otel.enabled` (**on by default**)
**and** the cluster has the `opentelemetry.io` CRDs — so the default cannot fail
an install where the operator is absent; there the collectors are skipped and
`NOTES.txt` says they were:

- **`<release>-controller`** — scrapes the amp controller (`:8080`) and its in-pod
  controller (`:50051`).
- **`<release>-compute`** — scrapes every endpoint pod: the postgres_exporter
  sidecar (`:9187`), pageserver (`:9898`), safekeeper (`:7676`), compute_ctl
  (`:3080`) twice — `/metrics` for compute_ctl's own counters and
  `/autoscaling_metrics` for the LFC gauges it proxies from the communicator's
  unix socket — and (with `otel.podMetrics`) per-pod cAdvisor resource metrics
  from each node's kubelet.

The chart also creates the collector `ServiceAccount` and RBAC sized to what is
actually scraped: a namespaced **`Role`** on `pods` for discovery — every app
scrape job restricts SD to the release namespace — plus a **`ClusterRole`** on
`nodes`/`nodes/proxy`/`nodes/metrics` **only when `otel.podMetrics` is on**,
because cAdvisor lives in the kubelet and has no namespaced equivalent. That pair
is named `<namespace>-<release>-otel` — unprefixed, two installs on one cluster
share it and either `uninstall` revokes the other's node access (#888). With
`podMetrics` off the collectors need no cluster-scoped rights at all. Store is set
via `otel.remoteWriteEndpoint` (VictoriaMetrics by default); an explicit `""` drops
that exporter entirely, and `otel.inClusterStores.enabled=false` drops it along with
the logs one, which is what a BYOC install does. **Requires the OTel
Operator** (opentelemetry.io CRDs) in the cluster.

## BYOC: shipping to yugabyte-cloud

`otel.byoc.enabled` adds an `otlphttp/byoc` exporter to both metrics collectors —
BasicAuth over the BYOC PrivateLink door — and a `datadog` exporter to the logs
daemonset. A BYOC install pairs it with `otel.inClusterStores.enabled=false`, so
yugabyte-cloud is the only destination; a cloud install leaves the stores on and
ships to both.

Three things make that hold in a cluster we do not own.

**The chart tells the operator, and does not stop them.** `agent.enabled` marks the
environment as YCP-managed, and `yb-amp-controller.telemetryWarnings` then collects
everything wrong — otel off, byoc off, no operator, nothing scraping, the logs
collector off, a plaintext or empty endpoint, an unnamed credential — into a block
NOTES.txt prints after the install. It collects *all* of them rather than stopping at
the first, because nothing interrupts the operator to make them read it. These were
`fail`s; they are warnings while the BYOC door is rolled out fleet by fleet, since an
install that cannot ship is as likely to be a fleet problem as an operator mistake.

**A post-install hook reports whether it works, not just that it is configured.**
The same file's Job — running the reconcile-agent image with `preflight` — POSTs a
gzipped OTLP payload to the door (401/403 is a wrong password, a transport error is
PrivateLink not wired) and then polls the **controller** collector's own
`otelcol_exporter_sent_metric_points{exporter="otlphttp/byoc"}` until it is non-zero.
Only that collector: the compute one scrapes endpoint pods, so a freshly installed
environment owns none and its counter is legitimately flat.

It exits 0 whatever it finds, so a successful install does **not** mean metrics are
flowing — the Job's log is the verdict, and NOTES.txt says so. There is no flag
controlling that; making it fatal later is a `return err` in `runPreflight`.

**The control plane watches, because the install runs from a checkout the customer
has**: every rule inside the chart is theirs to edit. The agent reports its effective
telemetry config — read from the same values the collectors render from — on enroll
and on every status tick, and YCP compares the endpoint against its own
`--byoc-metrics-url`, the same value it printed in the install hint.

A mismatch **degrades the environment, never refuses it**: one that cannot ship
metrics is still one an operator needs to see, and refusing it would hide exactly
what the refusal is about. (It would also be a restart-time lockout — enrolment
repeats at every agent startup.) The reason lands on a synthetic `telemetry`
component the console already renders.

The agent additionally reports whether metrics are still *flowing*, not just
configured: it reads the controller collector's
`otelcol_exporter_sent_metric_points{exporter="otlphttp/byoc"}` each tick and reports
how long it has sat still, so an environment that breaks after install is degraded
rather than going quietly dark. The controller collector specifically — the compute
one scrapes endpoint pods, so an environment with none has a legitimately flat
counter.

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
(`:7676`) and compute_ctl (`:3080`) already serve
`/metrics`**, but their container ports are only exposed when
`service.exposeInternalHttp=true` or the endpoint is SC-coordinated. To scrape
them, the Collector's pod service-discovery must target those ports (the
`postgres_exporter` `prometheus.io/*` annotations cover only `:9187`); a
multi-port scrape config or per-port annotations is the step-2 work. The
`controller` and the amp `controller` (`:8080`) are scraped directly.

### Tenancy labels

Both metrics collectors stamp **`environment_id`** and **`account_id`** on every
datapoint they scrape, via a `transform/identity` processor fed by
`otel.environmentId` / `otel.accountId`. That covers every source at once — the Go
controller, the Rust services, `postgres_exporter`, cAdvisor:
labelling at the collector is one setting, where labelling at the source would be
~230 metric declarations across two languages plus the exporters amp does not own.
They are datapoint attributes rather than resource attributes so Prometheus's OTLP
receiver promotes them to labels without the receiving control plane setting
`promote_resource_attributes` (same reasoning as the BYOC `source` label).

The values come from YCP, which owns both: `environment_id` is the environment
slug (`acme-prod` — the same value as `agent.envName`), and the install hint
the console prints fills both in. Unset, nothing is stamped. The hint omits
`account_id` for a standalone YCP, whose all-zeros sentinel account would label
every series identically.

One source this does **not** reach: the reconcile-agent, which exposes no
`/metrics` at all. The backing YugabyteDB is reached by a different pipeline
entirely, and **only on the operator-managed foundation** — see below. (The QA
framework also samples the DB into the report portal, which is a third one.)

### Endpoint identity labels

The compute collector's six endpoint jobs copy the pod's `amp.yugabyte.com/`
identity labels ([`ac-k8s-deploy.md`](ac-k8s-deploy.md#labels-identity-and-tags))
onto every series they scrape, as `endpoint_id`, `branch_id`, `cluster_id`,
`storage_pool_id` and `ep_type`, so a query can group by cluster, pool or role
without a join. The relabel rules are one helper,
`yb-amp-controller.endpointIdentityRelabels`.

The pageserver and safekeeper need one extra step for `branch_id`. Each serves
exactly the pod's one branch and already puts that same id on its branch-scoped
series, but most of their series have none (WAL ingest, WAL redo, the YugabyteDB
read and write paths, GC). A target label named `branch_id` would make Prometheus
rename theirs to `exported_branch_id`, so those two jobs add the pod's branch as
`pod_branch_id` and a `metric_relabel_configs` pair
(`yb-amp-controller.endpointBranchFill`) moves it into `branch_id` only on a
series whose own `branch_id` is empty, then drops it. Every series ends up with
`branch_id`, and the daemon's value is never overwritten.

User tags are not copied: their keys are the caller's, so they can collide with
an exporter's label names or the ones above.

## YugabyteDB: exported by YBA, not scraped by us

**Only where YBA runs.** These are CRs the YugabyteDB Kubernetes operator reads,
so this covers the operator-managed foundation and nothing else: `up-gke.sh`
gates the step on `FOUNDATION=operator`, and `FOUNDATION` defaults to
`yugabyted`. Every other bring-up — `deploy/up.sh`, the default `up-gke.sh`, the
ycp agent's BYOC data plane — runs a hand-rolled `yugabyted` StatefulSet with no
YBA to accept these, and its database still reports nothing. That gap is real and
open; do not read this section as covering it.

The universe under an operator-managed deployment is not in the pipeline above.
Its metrics are YugabyteDB Anywhere's to export, configured declaratively on the
CRs the foundation renders (meko#1405):

```
foundation spec
  TelemetryProvider          ─┐
  YBUniverse.spec.telemetry  ─┤
                              ▼
   operator ─▶ YBA ─▶ collector config ─▶ otelCollector.* on the DB chart
                                             │
                                             ▼
                          sidecar in the yb-master and yb-tserver pods,
                          scraping pod-locally ─▶ the BYOC OTLP door
```

Nothing in this repo writes a scrape job or a relabel rule for it, and the amp
collectors never see it — it reaches the same door by its own route.

**Why not just scrape it with a collector of ours.** Two different obstacles,
depending on the shape:

- A **deployment-mode** collector, like the three above, discovers pods with
  `role: pod` SD restricted to the release namespace, backed by a namespaced
  `Role`. The universe lands in the provider zones' `kubeNamespace`, which need
  not be that namespace — so it would need either a grant in a namespace the
  chart does not own or cluster-scoped discovery, which #753 exists to prevent.
- A **sidecar** of ours avoids discovery entirely, but injection happens at pod
  *admission*: `foundation up` creates the universe long before the amp chart
  creates the collector CR, so the webhook has nothing to resolve and never
  retries. Verified on a live universe — pods annotated, CR present, no
  `otc-container`.

And writing `otelCollector.config` by hand through `kubernetesOverrides` fights
YBA for the same field, which it sets itself from the universe's telemetry
config.

**It is turned on after the universe, not with it.** The exporter references the
`byoc-metrics-auth` Secret, which the environment's install hint creates, and the
identity tags come from that same step — so a foundation comes up with no
telemetry and the hint prints the blocks that enable it. An empty `exporters`
list is not "collect but ship nowhere": YBA gates the sidecar on there being an
exporter, so nothing is collected until it is applied, and no series can reach
the door without an identity on it.

**`collectionLevel: MINIMAL`, and deliberately not `TABLE_OFF`.** This is the one
piece of the configuration that is a trap. YBA has two collection-level
implementations: the swamper iterates every parameter in the level's file, so
`TABLE_OFF`'s `table_allowlist=NONE` reaches the database; the **metrics export
path sends only `priority_regex`, `metrics` and `show_help`**, and `TABLE_OFF`
defines neither of the first two. Declaring it on an exporting universe therefore
sends no filter at all — the v1 endpoint's default `priority_regex=.*` — and
collects *every* per-table series, the opposite of its name and worse than the
`NORMAL` default. The CRD accepts it and documents it as dropping per-table
metrics, so nothing says otherwise until the bill does. `MINIMAL` defines both
parameters, so both are sent and it takes effect. No level yields zero per-table
series here; that needs the export generator to pass every parameter the way the
swamper already does.

`deploy/yb-metrics-smoke.sh` checks the links rather than the intent, because
each of them fails quietly: the provider reaching `Ready` with a UUID, the sidecar
actually being in both pods, the *generated* config carrying the level's
parameters and the identity tags, and the collector accepting datapoints. Reading
the generated config is the only place an under- or over-collecting level becomes
visible before the storage bill. It runs in the foundation nightly, the only path
with a real operator-managed universe.

**Known limitation.** `additionalTags` is fixed per exporter, so one environment
per universe is assumed; two sharing a foundation would tag the same series with
one identity. And a `TelemetryProvider` spec is immutable in every field, so
rotating the BYOC password means deleting and recreating it, which YBA refuses
while a universe references it.

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

### Certificate expiry — from the pod, not from cert-manager

Three gauges the **supervisor** serves on its own port (`:9899`, scrape job
`amp-supervisor`), set by the watcher that already polls the mounted certificate
(`compute/endpoint_supervisor/src/compute_tls.rs`):

| Series | Meaning |
|---|---|
| `compute_cert_not_after_seconds` | `notAfter` of the certificate the compute presents to clients (0 = none) |
| `compute_ca_not_after_seconds` | `notAfter` of the CA that issued it, from the leaf Secret's `ca.crt` (0 = none, which is what an ACME issuer gives) |
| `compute_cert_reload_pending` | `1` while the mounted certificate is not the one the postmaster is serving, confirmed by a handshake |

cert-manager publishes `certmanager_certificate_expiration_timestamp_seconds` for
every `Certificate` it manages, and that is the better source for *issuance*
health across a cluster. These exist because they answer a different
question: what the postmaster is **actually serving**. The two diverge exactly
when a renewal has not reached the pod — the Secret rewritten, the volume not yet
propagated, or a mount that never arrived — which is the failure the watcher
exists to catch and which no cluster-level metric can see.

Alert on `compute_cert_not_after_seconds` approaching `time()`, and on `0` for an
endpoint whose chart has `computeTls.enabled`: zero means no certificate is being
served at all, while the pod stays healthy and every `sslmode=verify-full` client
fails.

**And alert on `compute_cert_reload_pending == 1` sustained** — say, ten minutes,
comfortably past the 60s poll. The two expiry gauges read the mounted *file*, so
a renewal that reached the volume moves them whether or not the postmaster ever
re-read it: they would report a healthy new `notAfter` while the compute went on
serving the old certificate. The pending gauge is the difference between those two
states, and it is the only one of the three that can show it.

It is **confirmed, not inferred**: the watcher opens a TLS handshake to the
compute's own port and compares the leaf the server presents against the leaf in
the mount, so `0` means the postmaster is serving the file rather than that a
signal was sent. That distinction is the whole point — a `SIGHUP` is dropped by
`compute_ctl` when its status is neither `Empty` nor `Running`, and a Postgres
reload whose new pair fails to load keeps the old `SSL_context` while logging and
returning normally. Neither is visible from the signal. A momentary `1` is the
normal path (the confirmation runs for up to 10s after each signal, and the poll
retries every 60s); a sustained `1` means the compute is not taking the
certificate.

Served only when the endpoint has a controller (the supervisor's HTTP listener is
gated on `SUPERVISOR_HTTP_PORT`), which a controller-driven install always sets.

### Query-level metrics — `pg_stat_statements`

The richest perf signal (per-normalized statement `calls`, total/mean/max exec
time, rows, block hits vs reads, WAL bytes) comes from the exporter's built-in
`stat_statements` collector, **on by default**. Three pieces hang off one value,
`endpoint.sharedPreloadLibraries` (`yb_amp,pg_stat_statements,pgsentinel`):

1. The library is preloaded from the first boot. It is loadable only at postmaster
   start, so leaving it out costs a compute restart to add later.
2. `compute_ctl` creates the extension in the main database, from
   `post_apply_config` — which runs whether or not the spec sets
   `skip_pg_catalog_updates`, and every amp spec does. It is a background task, so
   the extension lands a moment after the endpoint reports ready.
3. The chart passes `--collector.stat_statements` only when that value carries the
   library — without it the view is absent and the collector errors every scrape.

So dropping `pg_stat_statements` from that one value turns the whole path off
coherently, extension and collector included. It also gates `compute_ctl`'s
`GET /insights` (top-100 statements by mean time).

`queryid` is high-cardinality, so scope/cap it in the Collector's relabel rules
if the series count is a concern.

## Rollout order

All three steps are implemented:

1. **Controller `/metrics`** — ✅ (`controller/internal/metrics`).
2. **OTel Collector** — ✅ (`OpenTelemetryCollector` CRs in the control-plane
   chart; requires the OTel Operator + a store, VictoriaMetrics by default).
3. **Compute `postgres_exporter` sidecar** — ✅
   (`deploy/helm/yb-amp-endpoint`), `pg_stat_statements` included.

Steps 1 and 3 are store-agnostic (they only expose `/metrics`); step 2 picks
the store.
