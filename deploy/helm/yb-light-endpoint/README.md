# yb-light-endpoint Helm chart

Runs a [yb-light endpoint](../../README.md) as a single pod backed by an
external YugabyteDB. Connection config is passed in via a ConfigMap.

## Install

```sh
helm install ep deploy/helm/yb-light-endpoint \
  --set image.repository=yb-light/neon-endpoint --set image.tag=dev \
  --set yugabyte.hostPorts=yb-tserver.default.svc:5433 \
  --set yugabyte.user=yugabyte \
  --set yugabyte.password=$YB_PASSWORD
```

The chart renders:

- **ConfigMap** — the YugabyteDB connection block (`host_ports`, `user`,
  `dbname`, `sslmode`, …) plus endpoint/compute settings and the internal
  service ports. Consumed by the pod via `envFrom`.
- **Secret** — the YB password (only when `yugabyte.password` is set and no
  `yugabyte.existingSecret` is given). Injected as the `YB_PASSWORD` env var.
- **Deployment** — one container running the supervisor + 5 services, plus (by
  default) a `postgres_exporter` sidecar for Postgres metrics. PGDATA is
  ephemeral and durable state lives in YugabyteDB, so `/data` is an `emptyDir`
  scratch volume by default; when `persistence.storageClass` is set it is a
  PersistentVolumeClaim instead and the Deployment uses the `Recreate` strategy
  (the RWO volume attaches to one pod at a time). The pod's
  `podSecurityContext.fsGroup` makes the root-owned PVC writable by the non-root
  process. Rolls on config changes.
- **PersistentVolumeClaim** — only when `persistence.storageClass` is set: a
  `ReadWriteOnce` claim of that class (EBS/PD) backing `/data`. Part of the
  release, so `helm uninstall` frees it.
- **Service** — exposes the compute Postgres port (and, optionally, the
  internal HTTP APIs when `service.exposeInternalHttp=true`). The
  `postgres_exporter` metrics port is deliberately *not* published here — the
  pod is annotated for Prometheus pod-discovery instead (see below).

### Metrics

When `metrics.postgresExporter.enabled` (default), the pod runs a
`postgres_exporter` sidecar that connects to the compute's Postgres over
loopback (as `cloud_admin`, trust auth — the same path the compute VM-image
exporter uses) and serves Prometheus on `:9187`. The pod carries
`prometheus.io/scrape` + `prometheus.io/port` annotations so a collector with
pod service-discovery scrapes it directly; the port stays off the Service so
metrics aren't published on a LoadBalancer/tailnet-exposed endpoint. Built-in
collectors cover `pg_stat_database`, bgwriter, connections, locks, and
replication. For query-level metrics, enable the `stat_statements` collector
(`metrics.postgresExporter.extraArgs: ["--collector.stat_statements"]`) once
`pg_stat_statements` is preloaded and the extension is created. See
[`controller/architecture/ac-metrics.md`](../../../controller/architecture/ac-metrics.md).

## Key values

| Value | Default | Meaning |
|---|---|---|
| `image.repository` / `image.tag` | `yb-light/neon-endpoint` / chart appVersion | endpoint image |
| `yugabyte.hostPorts` | `yb-tserver:5433` | CSV of YB tservers `host:port[,...]` |
| `yugabyte.user` / `yugabyte.dbname` | `yugabyte` / `yb_storage` | YB role / database |
| `yugabyte.sslmode` / `yugabyte.sslrootcert` | _(empty)_ | TLS mode / CA bundle path |
| `yugabyte.password` | _(empty)_ | inline password → chart-managed Secret |
| `yugabyte.existingSecret` / `existingSecretKey` | _(empty)_ / `password` | use an existing Secret instead |
| `endpoint.pgVersion` | `17` | compute Postgres version |
| `endpoint.computePort` | `55433` | compute Postgres port (exposed by the Service) |
| `endpoint.branchId` | _(empty)_ | serve an existing branch; empty ⇒ bootstrap |
| `endpoint.sharedPreloadLibraries` | `yb_amp` | compute `shared_preload_libraries` |
| `endpoint.extraSettings` | `{}` | extra Postgres GUCs (`name: value`) |
| `endpoint.resources.cpu` / `endpoint.resources.memory` | `2` / `4Gi` | compute reserved for the pod; rendered as request == limit ⇒ Guaranteed QoS |
| `service.type` | `ClusterIP` | Service type |
| `service.exposeInternalHttp` | `false` | also publish pageserver/safekeeper/endpoint_storage HTTP |
| `metrics.postgresExporter.enabled` | `true` | run the `postgres_exporter` sidecar (`:9187`) |
| `metrics.postgresExporter.image.tag` | `v0.17.1` | exporter image tag |
| `metrics.postgresExporter.extraArgs` | `[]` | extra exporter flags (e.g. `--collector.stat_statements`) |
| `persistence.storageClass` | _(empty)_ | back `/data` with a PVC of this class (EBS/PD); empty ⇒ `emptyDir` |
| `persistence.size` | `10Gi` | PVC size when `persistence.storageClass` is set |
| `podSecurityContext.fsGroup` | `1000` | pod fsGroup (the image's run-as gid) so a provisioned PVC is writable |
| `supervisor.maxRestarts` / `restartWindowSeconds` / `restartBackoffSeconds` | `5` / `60` / `2` | in-pod restart policy |

See [`values.yaml`](values.yaml) for the full set.

## Unit tests

Tests use [helm-unittest](https://github.com/helm-unittest/helm-unittest)
(same approach as `yugabyte/charts/stable/yugaware/tests`):

```sh
helm plugin install https://github.com/helm-unittest/helm-unittest.git   # once
helm unittest deploy/helm/yb-light-endpoint
```

Suites live in [`tests/`](tests) (`*_test.yaml`), one per template.

### Guaranteed-QoS guardrail

[`tests/guaranteed-qos.sh`](tests/guaranteed-qos.sh) renders the chart and
asserts that **every** container in the compute pod sets
`resources.requests == resources.limits` for cpu and memory, so the pod gets
Kubernetes Guaranteed QoS. Unlike the per-container helm-unittest assertions,
this catches a *newly added* container (e.g. a sidecar) that forgets its
request==limit block — QoS is pod-level, so one such container drops the whole
pod to Burstable. Needs `helm` + [`yq`](https://github.com/mikefarah/yq).

```sh
make endpoint-chart-test          # helm unittest + the guardrail
# or just the guardrail:
bash deploy/helm/yb-light-endpoint/tests/guaranteed-qos.sh
```
