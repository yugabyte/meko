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
- **Deployment** — one container running the supervisor + 5 services. PGDATA is
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
  internal HTTP APIs when `service.exposeInternalHttp=true`).

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
| `service.type` | `ClusterIP` | Service type |
| `service.exposeInternalHttp` | `false` | also publish pageserver/safekeeper/endpoint_storage HTTP |
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
