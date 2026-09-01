# ac — Logs

**Status:** Draft
**Last updated:** 2026-08-17

How amp's container logs leave the node and reach a searchable store. The
guiding finding mirrors [`ac-metrics.md`](ac-metrics.md): **every amp component
already writes structured-enough logs to stdout** — the Rust services through
`tracing` (`utils::logging::init`), the Go controller through its own logger,
Postgres through its stderr — so this is a *collect-and-export* problem, not a
re-instrument-everything one.

The gap it closes: those logs live only in the container runtime's node-local
files. `kubectl logs` reaches them while the pod exists and loses them the moment
it is rescheduled — exactly when a compute crash-loop or a daemon exiting on an
expired write lease most needs explaining — and it cannot search across pods, which a getpage
failure spanning compute_ctl, pageserver and safekeeper requires.

## Pipeline

```
 sources (stdout)                collect                     store        visualize
 ┌──────────────────────┐
 │ amp controller       │
 │ compute pod          │   ┌────────────────────────┐  ┌──────────────┐  ┌───────┐
 │   pageserver         │──►│ OTel Collector         │─►│ VictoriaLogs │─►│Grafana│
 │   safekeeper         │   │ (daemonset, per node)  │  │              │  │       │
 │   compute_ctl        │   │ filelog → k8sattributes│  └──────────────┘  └───────┘
 │   postgres           │   │        → otlphttp      │
 │   postgres_exporter  │   └────────────────────────┘
 └──────────────────────┘         reads /var/log/pods
```

The kubelet already writes every container's stdout/stderr to
`/var/log/pods/<ns>_<pod>_<uid>/<container>/N.log`. The collector tails those
files, re-attaches the Kubernetes identity, and exports OTLP to the store.

### Why a daemonset tailing files, not in-process OTLP

The alternative is linking an OTLP log exporter into each service —
`opentelemetry-appender-tracing` bridged into the existing `tracing` subscriber,
alongside the OTLP *trace* export `compute/libs/tracing-utils` already does.
Rejected as the primary mechanism:

- **Coverage.** Postgres itself, `postgres_exporter`, and any sidecar or init
  container are not Rust and do not use `tracing`. A compute pod that fails
  before `compute_ctl` initialises its subscriber produces exactly the logs worth
  having, and an in-process exporter is not running yet to ship them.
- **Backpressure.** An in-process exporter makes the log store's availability a
  property of the service: a slow or down store either blocks or silently drops
  inside the pageserver's hot path. A node-local collector absorbs that with its
  own queue and retry, and the worst case is a lagging tail.
- **No code to change.** This is deployment configuration, so it ships and
  reverts independently of a service release.

The in-process bridge is still the right way to get **trace-correlated,
structured** logs, and remains open as a follow-on for the Rust services
specifically; it composes with this (it would export to the same collector)
rather than replacing it.

## Implementation

One `OpenTelemetryCollector` CR, `<release>-logs`, in the
[`yb-amp-controller`](../../deploy/helm/yb-amp-controller) chart alongside the
two metrics collectors
([`templates/otel-collectors.yaml`](../../deploy/helm/yb-amp-controller/templates/otel-collectors.yaml)).
Gated on `otel.enabled` and `otel.collectors.logs.enabled` (both on by default
once `otel.enabled`). **Requires the OTel Operator** and the **contrib**
collector image — the `filelog` receiver and `k8sattributes` processor are not
in the core distro.

**`mode: daemonset`** is not a preference. Container logs are node-local files,
so a deployment-mode collector would collect only from whichever nodes its
replicas happened to land on, and would do so silently.

### Receiver — `filelog`

- `include: /var/log/pods/<release-namespace>_*/*/*.log` — scoped to amp's own
  namespace. The daemonset can read every pod on the node; shipping the node's
  other tenants is both a volume and a privacy problem.
- `exclude` covers `<release>-*-collector-*`, or the collector's own stdout feeds
  back into its pipeline and compounds.
- `start_at: beginning`, not `end`. A compute pod that dies during startup is
  the case these logs exist to explain; the checkpoint below is what stops that
  from meaning replay.
- `storage: file_storage/checkpoints` — read offsets are persisted to a hostPath
  (`/var/lib/otelcol/<namespace>-<release>-logs`), so a collector restart resumes
  where it stopped instead of re-shipping or skipping the file. The namespace is
  in the path because the dir is node-shared and bbolt holds it under an
  exclusive lock. Unprefixed, the second release to open it on a node never
  starts its filelog receiver (#889).
- The **`container` operator** does the parsing: it strips the CRI/containerd
  (or docker) envelope, stitches back together lines the runtime split at 16 KiB
  — which the Rust services' multi-line backtraces routinely exceed — and lifts
  `k8s.namespace.name` / `k8s.pod.name` / `k8s.container.name` off the file path.

### Processors

- **`memory_limiter` first.** One pod logging in a loop must not OOM the node's
  collector; put anything ahead of it and the limiter never sees the pressure in
  time.
- **`k8sattributes`** adds what the file path cannot carry — pod uid, node, and
  the owning Deployment/StatefulSet — and copies two pod labels:
  `app.kubernetes.io/instance` → **`endpoint`** and
  `app.kubernetes.io/component` → `component`. `endpoint` is deliberately the
  same label the metrics collectors relabel onto every per-endpoint series, so
  the filter on a Grafana panel is the filter in a LogsQL query.
  `filter.node_from_env_var: KUBE_NODE_NAME` (fed from `spec.nodeName`) keeps
  each replica watching only its own node's pods rather than every pod in the
  cluster.
- **`batch`** last, as with the metrics collectors.

### Exporter

`otlphttp` to `otel.logsEndpoint`, defaulting to the VictoriaLogs OTLP ingest
path served by the [`amp-monitoring`](../../deploy/helm/amp-monitoring) chart
(`http://victoria-logs-server:9428/insert/opentelemetry/v1/logs`). TLS follows
the scheme, matching the metrics exporters: plaintext for in-cluster `http://`,
verified for `https://`. Any OTLP/HTTP logs receiver works — the store is not
baked in.

### RBAC — namespaced, deliberately

`k8sattributes` resolves a pod's workload through its ReplicaSet, so the
collectors' namespaced `Role` gains `apps/replicasets` (get/list/watch) next to
the `pods` rule the metrics scrape jobs already use. Service account and binding
are shared across all three collectors.

Nothing here is cluster-scoped, and that is a design constraint rather than an
accident: `k8sattributes` defaults to watching pods cluster-wide, which would
force a `ClusterRole` and break the chart's namespace-scoped install. Setting
`filter.namespace` to the release namespace keeps it inside the existing `Role`.
The one cluster-scoped piece in this chart remains `otel.podMetrics` (cAdvisor
via the kubelet), which the log pipeline does not need.

## Store

**VictoriaLogs**, deployed by the `amp-monitoring` chart next to VictoriaMetrics
— same vendor, same operational model, native OTLP ingest, and stream-oriented
indexing that suits amp's high-volume repetitive lines. Rationale and the
rejected alternatives (Loki, CloudWatch Logs) are in that chart's
[`DESIGN.md`](../../deploy/helm/amp-monitoring/DESIGN.md).

## Deploying

```sh
helm upgrade --install amp-monitoring deploy/helm/amp-monitoring -n amp-dev

helm upgrade --install amp deploy/helm/yb-amp-controller -n amp-dev \
  --set otel.enabled=true \
  --set otel.logsEndpoint=http://victoria-logs-server:9428/insert/opentelemetry/v1/logs
```

Then Grafana → **Dashboards → amp — Endpoint logs** (uid `amp-endpoint-logs`,
shipped by the `amp-monitoring` chart). Endpoint / container / free-text
dropdowns over a live tail, an errors-only panel, and per-container log volume —
the intended entry point for "what is this endpoint doing".

Explore → **VictoriaLogs** is there for raw LogsQL:

```logsql
{k8s.namespace.name="amp-dev", k8s.container.name="endpoint"} error
{endpoint="yb-ep-abc123"} | stats by (k8s.container.name) count()
```

**Field names keep the OTLP dotted form.** VictoriaLogs stores the resource
attributes verbatim (`k8s.container.name`), where the metrics side underscores
them through Prometheus normalization (`k8s_namespace_name`) — the same
attribute, spelled differently per store. `endpoint` and `component` come from
`k8sattributes`' label extraction and are identical on both sides, which is why
they are the labels to join on.

Containers are `endpoint`, `controller`, `mcp`, `ui` and `postgres-exporter`.
The endpoint pod runs pageserver, safekeeper and compute_ctl inside the single
`endpoint` container, so separating those is a message-level filter rather than
a container one.

## Operational notes

- **Tainted nodes.** The daemonset only lands where it is schedulable. If amp
  endpoints run on tainted nodes, set `otel.logsTolerations` to match — otherwise
  those nodes' logs are missing with no error anywhere.
- **Stream cardinality** is the log analogue of metric cardinality. Keep
  per-request identifiers (branch id, LSN, tenant) in the log *body*; promoting
  them to stream fields degrades VictoriaLogs the same way label churn degrades
  a TSDB.
- **Volume.** The pageserver at debug level is by far the loudest source. The
  log level is still per-service (`RUST_LOG` / the service config), and lowering
  it there remains the first lever — this pipeline ships whatever is written.
- **Retention** is the store's (`victorialogs.server.retentionPeriod`, 1 month by
  default), not the collector's.
- **hostPath.** The collector mounts `/var/log/pods` read-only plus a writable
  checkpoint dir. Clusters enforcing a restricted PodSecurity policy need the
  collector namespace exempted or the operator's security context adjusted.

## Not covered yet

- **Trace correlation.** Traces export in-process via `tracing-utils`; logs go
  out over the file path with no `trace_id`. Joining them needs the in-process
  log bridge described above.
- **`yb_amp_local`.** This is the Kubernetes path only. Local dev still reads
  the per-service log files under `.yb_amp/`.
- **Alerting** on log patterns. The `critical!` macro already drives a pageable
  *metric* (`libmetrics_tracing_event_count{level="critical"}`), which remains
  the alerting path; log-based alerting is not wired.
