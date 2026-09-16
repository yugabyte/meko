# yb-amp-endpoint Helm chart

Runs a [yb-amp endpoint](../../README.md) as a single pod backed by an
external YugabyteDB. Connection config is passed in via a ConfigMap.

## Install

```sh
helm install ep deploy/helm/yb-amp-endpoint \
  --set image.repository=yb-amp/neon-endpoint --set image.tag=dev \
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
  PersistentVolumeClaim instead. The Deployment always uses the `Recreate`
  strategy: an endpoint is a single-writer compute, so a redeploy must stop the
  old pod before starting the new one rather than surging a second pod that would
  fight the old for the storage writer fence (and, with a PVC, the RWO volume
  attaches to one pod at a time anyway). The pod's `podSecurityContext.fsGroup`
  makes the root-owned PVC writable by the non-root process. Rolls on config
  changes.
- **PersistentVolumeClaim** — only when `persistence.storageClass` is set: a
  `ReadWriteOnce` claim of that class (EBS/PD) backing `/data`. Part of the
  release, so `helm uninstall` frees it.
- **LoadBalancer Service** `<fullname>-lb` (when `service.connectivity` is
  `nlb_internal` or `nlb_external`) — a **second**, Postgres-only Service behind
  an AWS Network Load Balancer, so clients outside the cluster get a stable
  address that survives pod replacement (the load balancer controller re-targets
  the new pod IP). `nlb_internal` is VPC-routable, `nlb_external` internet-facing;
  restrict either with `service.allowedCidrs`.

  Portable across clouds: the mode is the concept, and `service.cloud` selects
  the annotations — AWS gets an NLB with pod-IP targets and cross-zone on, GKE
  gets `networking.gke.io/load-balancer-type: Internal`, AKS gets
  `service.beta.kubernetes.io/azure-load-balancer-internal`. `allowedCidrs` is a
  core Service field and works the same everywhere. An **internal** LB with no
  known cloud fails the render rather than defaulting to internet-facing, which
  is what every provider would otherwise do.

  It is a separate Service rather than a type change on the one below because
  that one also carries `sup-http` (9899) — the supervisor's settings API, which
  is unauthenticated and can restart Postgres. Publishing it on a load balancer
  would put a restart button on the network.
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
replication. Query-level metrics come from the `stat_statements` collector,
which the template passes whenever `endpoint.sharedPreloadLibraries` contains
`pg_stat_statements` — it does by default. See
[`controller/architecture/ac-metrics.md`](../../../controller/architecture/ac-metrics.md).

## Key values

| Value | Default | Meaning |
|---|---|---|
| `image.repository` / `image.tag` | `yb-amp/neon-endpoint` / chart appVersion | endpoint image |
| `yugabyte.hostPorts` | `yb-tserver:5433` | CSV of YB tservers `host:port[,...]` |
| `yugabyte.user` / `yugabyte.dbname` | `yugabyte` / `yb_storage` | YB role / database |
| `yugabyte.sslmode` / `yugabyte.sslrootcert` | _(empty)_ | TLS mode / CA bundle path |
| `yugabyte.sslcert` / `yugabyte.sslkey` | _(empty)_ | client certificate and key paths, for certificate (`cert` HBA) authentication as `yugabyte.user` |
| `tls.enabled` | `false` | render a cert-manager `Certificate` (CN = `yugabyte.user`, from `tls.issuerRef`) and mount its Secret at `/etc/amp/tls/client` and `tls.caBundleConfigMap` at `/etc/amp/tls/ca`; the controller sets it together with the four `yugabyte.ssl*` values |
| `tls.issuerRef.kind` / `.name` / `.group` | `ClusterIssuer` / `amp-ca` / `cert-manager.io` | issuer of the certificate |
| `tls.caBundleConfigMap` | `amp-ca-bundle` | ConfigMap (key `ca.crt`) holding the trusted roots |
| `tls.certificate.duration` / `.renewBefore` | `720h` / `240h` | certificate lifetime and renewal lead |
| `yugabyte.password` | _(empty)_ | inline password → chart-managed Secret |
| `yugabyte.existingSecret` / `existingSecretKey` | _(empty)_ / `password` | use an existing Secret instead |
| `endpoint.pgVersion` | `18` | compute Postgres version (the image ships v18 only) |
| `endpoint.computePort` | `55433` | compute Postgres port (exposed by the Service) |
| `endpoint.branchId` | _(empty)_ | serve an existing branch; empty ⇒ bootstrap |
| `endpoint.sharedPreloadLibraries` | `yb_amp,pg_stat_statements` | compute `shared_preload_libraries`; `pg_stat_statements` here also turns on the exporter's `stat_statements` collector and compute_ctl's `/insights` |
| `endpoint.endpointId` | _(empty)_ | the endpoint's `amp_registry.endpoints` id; lets the supervisor read its own and its branch's `pg_settings` (and the pageserver/safekeeper override columns) from the registry. Empty ⇒ derived defaults only |
| `endpoint.resources.cpu` / `endpoint.resources.memory` | `2` / `16Gi` | compute reserved for the pod; rendered as request == limit ⇒ Guaranteed QoS. 16Gi is the 8 GiB per vCPU the control plane holds every endpoint to — a core without the memory to feed it gives Postgres a cache it cannot keep |
| `service.type` | `ClusterIP` | Service type |
| `service.exposeInternalHttp` | `false` | also publish pageserver/safekeeper HTTP |
| `service.connectivity` | `cluster_ip` | client reachability: `cluster_ip`, `nlb_internal`, `nlb_external` |
| `service.cloud` | _(set by the controller)_ | whose LB annotations to render: `aws`, `gcp`, `azure`. The controller passes what it detected from node provider IDs, or its `--deploy-cloud` override; required for `nlb_internal` |
| `service.allowedCidrs` | `[]` | `loadBalancerSourceRanges` for the `nlb_*` modes; empty = unrestricted |
| `metrics.postgresExporter.enabled` | `true` | run the `postgres_exporter` sidecar (`:9187`) |
| `metrics.postgresExporter.image.tag` | `v0.17.1` | exporter image tag |
| `metrics.postgresExporter.extraArgs` | `[]` | extra exporter flags, beyond the `--no-collector.database` / `--collector.stat_statements` the template sets |
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
helm unittest deploy/helm/yb-amp-endpoint
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
bash deploy/helm/yb-amp-endpoint/tests/guaranteed-qos.sh
```
