# Design — amp observability stack (`amp-monitoring`)

**Status:** Draft
**Last updated:** 2026-07-15

The **store + visualize** half of the amp metrics pipeline. The scrape + collect
half — the sources that expose Prometheus `/metrics` and the collectors that
scrape them — is designed in
[`controller/architecture/ac-metrics.md`](../../../controller/architecture/ac-metrics.md)
and implemented as the controller `/metrics` endpoint, the endpoint
`postgres_exporter` sidecar, and the `OpenTelemetryCollector` CRs in the
[`yb-amp-controller`](../yb-amp-controller) chart. This chart is where the
metrics land and how humans look at them.

## Pipeline (this chart = the last two boxes)

```
 sources (/metrics)          collect                    store            visualize
 ┌───────────────┐
 │ amp controller│─┐
 │ storage ctrl  │ │   OpenTelemetryCollector CRs   ┌──────────────┐   ┌─────────┐
 │ pageserver    │ ├──►(yb-amp-controller chart)───►│ VictoriaMetrics├──►│ Grafana │
 │ safekeeper    │ │    prometheus receiver →        │  (this chart) │   │ (this   │
 │ compute_ctl   │ │    prometheus_remote_write      └──────────────┘   │  chart) │
 │ postgres_exp. │─┘                                                    └─────────┘
 └───────────────┘
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

### Visualize — Grafana
- VictoriaMetrics provisioned as the default datasource (Prometheus type, fixed
  uid `victoriametrics` so dashboards bind deterministically).
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

## Label conventions (must match the collectors)
- Counter series carry a **`_total`** suffix from OTel Prometheus normalization
  (e.g. `pg_stat_database_xact_commit_total`) — dashboards use the suffixed names.
- `resource_to_telemetry_conversion` on the collector promotes k8s resource
  attributes to labels (`k8s_namespace_name`, `k8s_pod_name`, …). Per-component
  identity is carried in an `endpoint` label set by collector relabeling.

## Deployment model
- **Namespace:** install into the amp namespace (e.g. `amp-dev`) so the short
  Service DNS (`victoria-metrics-server`) and the dashboard sidecar's in-namespace
  search resolve with no extra config.
- **Prerequisites:** the metrics only flow once the **OpenTelemetry Operator** is
  installed and the `yb-amp-controller` chart is deployed with `otel.enabled=true`
  pointed at this store. This chart is inert-but-valid without them (empty
  dashboards).
- **Umbrella chart:** VictoriaMetrics and Grafana are upstream subchart
  dependencies (pinned in `Chart.yaml`); `helm dependency update` before install.

## Operational notes
- **Retention** defaults to 1 month (`victoriametrics.server.retentionPeriod`);
  the PVC uses `gp2-csi` (meko/EKS has no default StorageClass).
- **Cardinality** is bounded by keeping high-cardinality series (per-`queryid`
  `pg_stat_statements`) opt-in and relabeling route/endpoint labels to templates.
- **Admin password** is chart-generated into the Grafana Secret (not baked into
  values); retrieve it from the Secret.
- **Scaling:** for many placement groups, move to the VM cluster chart and/or a
  central store the per-group collectors remote-write to.
