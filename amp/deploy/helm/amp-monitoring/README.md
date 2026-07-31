# amp-monitoring Helm chart

The observability stack for amp: a **VictoriaMetrics** store and **Grafana** with
the amp dashboards. It's the store + visualize end of the metrics pipeline; the
scrape + collect end is the `OpenTelemetryCollector` CRs in the
[`yb-amp-controller`](../yb-amp-controller) chart. Design + rationale in
[`DESIGN.md`](DESIGN.md) and
[`controller/architecture/ac-metrics.md`](../../../controller/architecture/ac-metrics.md).

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
2. **`yb-amp-controller` deployed with `otel.enabled=true`**, pointed at this
   store (`otel.remoteWriteEndpoint=http://victoria-metrics-server:8428/api/v1/write`).

This chart is valid and installable without them — the dashboards are just empty
until metrics arrive.

## Install

Install into the amp namespace so the short Service DNS resolves:

```sh
helm dependency update deploy/helm/amp-monitoring        # once, fetches VM + Grafana
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
- **Grafana** — VictoriaMetrics as the default datasource + four dashboards
  (Postgres, Controller, Storage layer, Pod resources) loaded from ConfigMaps via
  the dashboard sidecar.

## Key values

| Value | Default | Meaning |
|---|---|---|
| `victoriametrics.server.retentionPeriod` | `1` | retention, months |
| `victoriametrics.server.persistentVolume.storageClassName` | `gp2-csi` | store PVC class |
| `victoriametrics.server.persistentVolume.size` | `10Gi` | store PVC size |
| `grafana.service.annotations` | tailnet expose (`grafana-amp`, `tag:k8s-meko`) | Grafana exposure; override for internal-LB etc. |
| `grafana.persistence.enabled` | `false` | Grafana is stateless (provisioned datasource + dashboards) |

Dashboards live in [`dashboards/`](dashboards) as JSON; add one and it's picked up
automatically. See [`values.yaml`](values.yaml) for the full set.
