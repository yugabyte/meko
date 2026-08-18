# Design — amp observability stack (`amp-monitoring`)

**Status:** Draft
**Last updated:** 2026-08-11

The **store + visualize** half of amp's metrics and log pipelines. The collect
half — the sources that expose Prometheus `/metrics`, the collectors that scrape
them, and the daemonset that tails container logs — is designed in
[`controller/architecture/ac-metrics.md`](../../../controller/architecture/ac-metrics.md)
and [`controller/architecture/ac-logs.md`](../../../controller/architecture/ac-logs.md),
and implemented as the controller `/metrics` endpoint, the endpoint
`postgres_exporter` sidecar, and the `OpenTelemetryCollector` CRs in the
[`yb-amp-controller`](../yb-amp-controller) chart. This chart is where the
telemetry lands and how humans look at it.

## Pipeline (this chart = the last two boxes)

```
 sources (/metrics)          collect                    store            visualize
 ┌───────────────┐
 │ amp controller│─┐
 │ storage ctrl  │ │   OpenTelemetryCollector CRs   ┌──────────────┐   ┌─────────┐
 │ pageserver    │ ├──►(yb-amp-controller chart)───►│ VictoriaMetrics├──►│ Grafana │
 │ safekeeper    │ │    prometheus receiver →        │  (this chart) │   │ (this   │
 │ compute_ctl   │ │    prometheus_remote_write      └──────────────┘   │  chart) │
 │ postgres_exp. │─┘                                                    │         │
 └───────────────┘                                                      │         │
 stdout (every container)                                               │         │
 ┌───────────────┐    logs daemonset CR             ┌──────────────┐    │         │
 │ /var/log/pods │───►(yb-amp-controller chart)────►│ VictoriaLogs ├───►│         │
 └───────────────┘     filelog → otlphttp           │ (this chart) │    └─────────┘
                                                    └──────────────┘
```

## Component choices

### Store — VictoriaMetrics (single node)
- **Prometheus-compatible** both ways: `prometheus_remote_write` in, PromQL out.
  Every amp source is already Prometheus-shaped (Neon heritage), so it maps 1:1
  with no translation, and upstream PromQL dashboards work unchanged.
- **Dense / cheap at high label cardinality** (per-endpoint, per-tenant,
  per-`queryid`) versus vanilla Prometheus, with better long-term retention.
- Single-node `victoria-metrics-single` is right for a placement group's scale;
  swap in the VM cluster chart if a fleet outgrows one node.
- **Alternative — CloudWatch** (`awsemf` exporter on the collector side): zero new
  infra, AWS-native, but custom-metric pricing explodes with high-cardinality
  labels and PromQL/Grafana fit these Neon metrics far better. Documented as a
  stopgap, not the home. The store is pluggable — it's just the collectors'
  `otel.remoteWriteEndpoint`, so nothing upstream is committed to VM.

### Store (logs) — VictoriaLogs (single node)
- **Same operational model as the metrics store** — same vendor, same chart repo,
  same single-node/PVC shape, same retention knob. One store to learn, one to
  back up, one to scale.
- **Native OTLP ingest** (`/insert/opentelemetry/v1/logs`), so the collector
  exports without a translation layer or a sidecar agent.
- **Cheap at amp's shape of data.** amp's logs are high-volume and highly
  repetitive (per-getpage, per-WAL-segment lines); VictoriaLogs indexes by
  stream rather than full-text by default, so disk stays proportional to
  distinct streams rather than to line count.
- **Alternative — Loki:** the better-known pairing with Grafana and a first-party
  datasource (no plugin install). Rejected on operational weight — its
  single-binary mode still wants object storage for anything beyond a demo, and
  it would add a second storage vendor next to VictoriaMetrics for no gain.
- **Alternative — CloudWatch Logs:** zero new infra, but ingest pricing scales
  with raw log volume, which is the wrong shape for a chatty storage engine, and
  it splits logs away from the metrics in a second UI.
- The store is pluggable the same way the metrics store is — it's just the
  collector's `otel.logsEndpoint`, so nothing upstream is committed to
  VictoriaLogs.

### Visualize — Grafana
- VictoriaMetrics provisioned as the default datasource (Prometheus type, fixed
  uid `victoriametrics` so dashboards bind deterministically).
- VictoriaLogs provisioned alongside it (uid `victorialogs`) via the
  `victoriametrics-logs-datasource` plugin — LogsQL has no built-in Grafana type.
  This is the stack's **one internet dependency at runtime**: Grafana fetches the
  plugin from grafana.com at pod start. Air-gapped installs bake it into the
  image instead.
- Dashboards shipped as **ConfigMaps** (`templates/dashboards.yaml`, rendered from
  `dashboards/*.json`) and auto-loaded by the chart's **dashboard sidecar** — no
  gnetId fetch, no internet dependency, versioned in-repo.
- Exposed on the **tailnet** via the Tailscale operator (annotations on the
  Service), the same pattern as the amp control plane. The operator does L4
  forwarding on port 80, so Grafana is reached over **HTTP** at
  `http://<hostname>.<tailnet>.ts.net` (no serve/cert). Override the Service
  annotations for a different exposure.

## Dashboards

| Dashboard | uid | Covers |
|---|---|---|
| amp — Postgres | `amp-postgres` | `pg_up`, connections, txn/s, tuples/s, cache-hit, block I/O, deadlocks |
| amp — Controller | `amp-controller` | request rate/latency (p50/95/99), task throughput & duration, in-flight |
| amp — Storage layer | `amp-storage` | controller (branches, nodes, HTTP, DB latency), safekeeper (timelines, WAL, disk), pageserver ops, compute_ctl |
| amp — Pod resources | `amp-pods` | per-pod CPU / memory / network (from cAdvisor) |
| amp — Endpoint logs | `amp-endpoint-logs` | live tail, errors-only, and per-container log volume for a selected endpoint (from VictoriaLogs) |

The log dashboard is the deliberate entry point for reading endpoint logs, rather
than leaving Explore as the only door: it templates `endpoint` off the same label
the metrics carry, so moving from a spike on *Storage layer* to the lines behind
it is a dashboard switch, not a hand-written LogsQL query. It is the only
dashboard bound to the `victorialogs` datasource; the other four use
`victoriametrics`.

## Label conventions (must match the collectors)
- Counter series carry a **`_total`** suffix from OTel Prometheus normalization
  (e.g. `pg_stat_database_xact_commit_total`) — dashboards use the suffixed names.
- `resource_to_telemetry_conversion` on the collector promotes k8s resource
  attributes to labels (`k8s_namespace_name`, `k8s_pod_name`, …). Per-component
  identity is carried in an `endpoint` label set by collector relabeling.
- **Logs carry the same attributes, spelled differently.** `k8sattributes` emits
  the same Kubernetes resource attributes, but VictoriaLogs keeps the OTLP dotted
  form (`k8s.container.name`) where Prometheus normalization underscores it
  (`k8s_namespace_name`) — so a query cannot be copied verbatim between the two.
  The extracted labels `endpoint` and `component` are identical on both sides,
  which is what makes them the join keys between a metrics panel and its logs.

## Deployment model
- **Namespace:** install into the amp namespace (e.g. `amp-dev`) so the short
  Service DNS (`victoria-metrics-server`, `victoria-logs-server`) and the
  dashboard sidecar's in-namespace search resolve with no extra config. The log
  collector also scopes its tail to its own release namespace, so a cross-namespace
  install collects nothing.
- **Prerequisites:** telemetry only flows once the **OpenTelemetry Operator** is
  installed and the `yb-amp-controller` chart is deployed with `otel.enabled=true`
  pointed at these stores. This chart is inert-but-valid without them (empty
  dashboards, empty log view).
- **Umbrella chart:** VictoriaMetrics, VictoriaLogs and Grafana are upstream
  subchart dependencies (pinned in `Chart.yaml`); `helm dependency update` before
  install.

## Operational notes
- **Retention** defaults to 1 month for both stores
  (`victoriametrics.server.retentionPeriod`, `victorialogs.server.retentionPeriod`);
  both PVCs use `gp2-csi` (meko/EKS has no default StorageClass). The log PVC is
  the larger of the two (20Gi) — a log byte compresses far worse than a sample,
  and it is the first thing to fill.
- **Cardinality** is bounded by keeping high-cardinality series (per-`queryid`
  `pg_stat_statements`) opt-in and relabeling route/endpoint labels to templates.
  The log equivalent is **stream count**: keep per-request identifiers in the log
  *body*, never promote them to stream fields, or VictoriaLogs degrades the same
  way a TSDB does under label churn.
- **Admin password** is chart-generated into the Grafana Secret (not baked into
  values); retrieve it from the Secret.
- **Scaling:** for many placement groups, move to the VM cluster chart and the
  VictoriaLogs cluster chart, and/or a central pair the per-group collectors
  ship to.
