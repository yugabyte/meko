# amp-monitoring Helm chart

The observability stack for amp: **VictoriaMetrics** and **VictoriaLogs** stores
plus **Grafana** with the amp dashboards. It's the store + visualize end of both
the metrics and the log pipeline; the collect end is the
`OpenTelemetryCollector` CRs in the [`yb-amp-controller`](../yb-amp-controller)
chart. Design + rationale in [`DESIGN.md`](DESIGN.md),
[`controller/architecture/ac-metrics.md`](../../../controller/architecture/ac-metrics.md)
and [`controller/architecture/ac-logs.md`](../../../controller/architecture/ac-logs.md).

## Prerequisites

For dashboards to show data, the collect step must be running:

1. **OpenTelemetry Operator** in the cluster (provides the `OpenTelemetryCollector`
   CRD):
   ```sh
   helm repo add open-telemetry https://open-telemetry.github.io/opentelemetry-helm-charts
   helm upgrade --install opentelemetry-operator open-telemetry/opentelemetry-operator \
     -n opentelemetry-operator-system --create-namespace \
     --set admissionWebhooks.certManager.enabled=false \
     --set admissionWebhooks.autoGenerateCert.enabled=true \
     --set manager.collectorImage.repository=otel/opentelemetry-collector-contrib
   ```
2. **`yb-amp-controller` deployed with `otel.enabled=true`** (the default),
   pointed at both stores:
   ```sh
   --set otel.remoteWriteEndpoint=http://victoria-metrics-server:8428/api/v1/write \
   --set otel.logsEndpoint=http://victoria-logs-server:9428/insert/opentelemetry/v1/logs
   ```

This chart is valid and installable without them — the dashboards and the log
view are just empty until data arrives.

The **Cache hit/miss** dashboard's LFC row needs the `amp-compute-lfc` scrape
job, which a collector deployed before it does not have. Either `helm upgrade`
the `yb-amp-controller` release, or patch the running collector in place — the
operator rolls only the collector pod:

```sh
NS=amp-dev   # the namespace amp runs in
kubectl -n "$NS" get opentelemetrycollector yb-amp-controller-compute -o json \
  | jq '.spec.config.receivers.prometheus.config.scrape_configs as $s
        | if any($s[]; .job_name == "amp-compute-lfc") then {} else
            {spec: {config: {receivers: {prometheus: {config: {scrape_configs:
              ($s + [($s[] | select(.job_name == "amp-compute-ctl"))
                     + {job_name: "amp-compute-lfc", metrics_path: "/autoscaling_metrics"}])}}}}}}
          end' \
  | kubectl -n "$NS" patch opentelemetrycollector yb-amp-controller-compute --type=merge --patch-file=/dev/stdin
```

It clones the existing `amp-compute-ctl` job (same pod discovery, same `:3080`)
and changes only the path, so it stays correct on a deploy that overrode the
port; re-running it patches `{}`, which changes nothing. Confirm with `lfc_hits`
in Grafana Explore within one scrape interval.

Grafana downloads the `victoriametrics-logs-datasource` plugin from grafana.com
at pod start, so its pod needs egress. Without it, Grafana comes up but the
VictoriaLogs datasource fails to load.

## Install

Install into the amp namespace so the short Service DNS resolves:

```sh
helm dependency update deploy/helm/amp-monitoring   # once, fetches VM + VictoriaLogs + Grafana
helm upgrade --install amp-monitoring deploy/helm/amp-monitoring -n amp-dev
```

## Access Grafana

Exposed on the tailnet (Tailscale operator) at **http://grafana-amp.<tailnet>.ts.net**
(plain HTTP — the operator L4-forwards port 80; `https://` will not connect).
Admin user `admin`; the password is chart-generated:

```sh
kubectl -n amp-dev get secret amp-monitoring-grafana -o jsonpath='{.data.admin-password}' | base64 -d
```

Or port-forward: `kubectl -n amp-dev port-forward svc/amp-monitoring-grafana 3000:80`.

## What it renders

- **VictoriaMetrics** (`victoria-metrics-single`) — Service `victoria-metrics-server:8428`;
  remote-write target `…/api/v1/write`, PromQL/datasource on `:8428`. Backed by a
  `gp2-csi` PVC, 1-month retention (see `values.yaml`).
- **VictoriaLogs** (`victoria-logs-single`) — Service `victoria-logs-server:9428`;
  OTLP ingest at `…/insert/opentelemetry/v1/logs`, LogsQL/datasource on `:9428`.
  Backed by a `gp2-csi` PVC, 1-month retention.
- **Grafana** — VictoriaMetrics (default) and VictoriaLogs datasources + nine
  dashboards (Postgres, Controller, Storage layer, Pod resources, Endpoint logs,
  Cache hit/miss, Storage engine, WAL/LSN lag, getpage SLO) loaded from
  ConfigMaps via the dashboard sidecar.

## Viewing endpoint logs

Grafana → **Dashboards → amp — Endpoint logs**. Pick an endpoint and a container
from the dropdowns; the panels show the live tail plus an error-rate graph. The
dashboard is the intended entry point — Explore is there when you need raw LogsQL:

```logsql
{k8s.namespace.name="amp-dev", k8s.container.name="endpoint"} error
{endpoint="yb-ep-abc123"} | stats by (k8s.container.name) count()
```

Field names keep the OTLP form with dots (`k8s.container.name`), unlike the
metrics side where Prometheus normalization underscores them
(`k8s_namespace_name`). Containers are `endpoint`, `controller`, `mcp`, `ui` and
`postgres-exporter` — the endpoint pod runs pageserver, safekeeper and
compute_ctl inside the single `endpoint` container, so separating those means
filtering on the message, not the container.

The collector sets the same `endpoint` label the metrics carry, so a chart and
its logs filter identically.

## Key values

| Value | Default | Meaning |
|---|---|---|
| `victoriametrics.server.retentionPeriod` | `1` | metrics retention, months |
| `victoriametrics.server.persistentVolume.storageClassName` | `gp2-csi` | metrics PVC class |
| `victoriametrics.server.persistentVolume.size` | `10Gi` | metrics PVC size |
| `victorialogs.server.retentionPeriod` | `1` | log retention, months |
| `victorialogs.server.persistentVolume.size` | `20Gi` | log PVC size — raise it first when logs fill up |
| `grafana.plugins` | `[victoriametrics-logs-datasource]` | LogsQL datasource plugin (needs egress at pod start) |
| `grafana.service.annotations` | tailnet expose (`grafana-amp`, `tag:k8s-meko`) | Grafana exposure; override for internal-LB etc. |
| `grafana.persistence.enabled` | `false` | Grafana is stateless (provisioned datasources + dashboards) |

Dashboards live in [`dashboards/`](dashboards) as JSON; add one and it's picked up
automatically. See [`values.yaml`](values.yaml) for the full set.
