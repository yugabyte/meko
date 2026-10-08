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
  `yugabyte.existingSecret` is given), injected as the `YB_PASSWORD` env var,
  and the compute default user's password (`endpoint.password`), injected as
  `COMPUTE_PASSWORD`. The chart renders it when either is set, so the compute
  password still gets a Secret alongside a `yugabyte.existingSecret`.
- **Deployment** — one container running the supervisor + 5 services, plus (by
  default) a `postgres_exporter` sidecar for Postgres metrics, and a
  `documentdb-gateway` sidecar on any endpoint whose branch mounts `documentdb`
  (`documentdbGateway.enabled` overrides that either way). The gateway also adds
  a `documentdb` port to the Service and a Secret holding its backend URL, which
  names the `documentdb` database compute_ctl creates the extension in. It serves
  the compute's certificate when `computeTls.enabled`, so with a private CA that
  names the Service a client connects with
  `mongodb://<service-dns>:10260/?tls=true&tlsCAFile=<ca.crt>`. Otherwise it
  self-signs, and only `tlsAllowInvalidCertificates=true` connects. PGDATA is
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

- **IngressRouteTCP / MiddlewareTCP / DNSEndpoint** (when `service.connectivity`
  is `ingress_internal` or `ingress_external`) — the endpoint's route onto a
  **shared** load balancer: a port on an ingress shard rather than a load
  balancer of its own. Carries `amp.yugabyte.com/ingress-shard` so only that
  shard's proxy serves it. The `DNSEndpoint` is what external-dns publishes the
  endpoint's own name from. Needs the Traefik CRDs installed
  (`controller/architecture/ac-ingress.md`).

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
| `computeTls.enabled` | `false` | give the compute's Postgres listener a **server** certificate, so a client can connect with `sslmode=verify-full`. The documentdb gateway serves the same one on its port. The opposite direction from `tls` above and independent of it; TLS stays optional on the wire (pg_hba is still `md5`, not `hostssl`) |
| `computeTls.mode` | `issue` | `issue` = one cert-manager `Certificate` for this endpoint from `computeTls.issuerRef` (a private CA); `existingSecret` = mount `computeTls.existingSecret`, one wildcard the fleet shares (Let's Encrypt, where a per-endpoint order spends the 50-new-certificates-per-registered-domain weekly limit on 50 endpoint **creates**; renewals are exempt, so it caps create rate, not fleet size) |
| `computeTls.issuerRef.kind` / `.name` / `.group` | `ClusterIssuer` / `amp-ca` / `cert-manager.io` | issuer for `mode: issue` |
| `computeTls.existingSecret` | _(empty)_ | the `tls.crt` + `tls.key` Secret to mount for `mode: existingSecret`; must already exist in the namespace |
| `computeTls.dnsNames` | `[]` | extra names, on top of the compute Service's DNS name and the published name (`service.ingress.dnsName` or `service.dns.name`) — which the chart adds itself |
| `computeTls.includeServiceDns` | `true` | cover the in-cluster Service name. Turn **off** for an ACME issuer: no public CA can validate `.svc.cluster.local` and one unvalidatable name fails the whole order |
| `computeTls.certificate.duration` / `.renewBefore` | `2160h` / `720h` | lifetime and renewal lead. Leave the duration **empty** for ACME, which fixes it at 90 days and only warns that a request was ignored |
| `yugabyte.password` | _(empty)_ | inline password → chart-managed Secret |
| `yugabyte.existingSecret` / `existingSecretKey` | _(empty)_ / `password` | use an existing Secret instead |
| `endpoint.pgVersion` | `18` | compute Postgres version (the image ships v18 only) |
| `endpoint.computePort` | `55433` | compute Postgres port (exposed by the Service) |
| `endpoint.branchId` | _(empty)_ | serve an existing branch; empty ⇒ bootstrap |
| `endpoint.sharedPreloadLibraries` | `yb_amp,pg_stat_statements,pgsentinel` | compute `shared_preload_libraries`; `pg_stat_statements` here also turns on the exporter's `stat_statements` collector and compute_ctl's `/insights` |
| `endpoint.endpointId` | _(empty)_ | the endpoint's `amp_registry.endpoints` id; lets the supervisor read its own and its branch's `pg_settings` (and the pageserver/safekeeper override columns) from the registry. Empty ⇒ derived defaults only |
| `endpoint.resources.cpu` / `endpoint.resources.memory` | `2` / `16Gi` | compute reserved for the pod; rendered as request == limit ⇒ Guaranteed QoS. 16Gi is the 8 GiB per vCPU the control plane holds every endpoint to — a core without the memory to feed it gives Postgres a cache it cannot keep |
| `service.type` | `ClusterIP` | Service type |
| `service.exposeInternalHttp` | `false` | also publish pageserver/safekeeper HTTP |
| `service.connectivity` | `cluster_ip` | client reachability: `cluster_ip`, `nlb_internal`, `nlb_external`, `ingress_internal`, `ingress_external` |
| `service.cloud` | _(set by the controller)_ | whose LB annotations to render: `aws`, `gcp`, `azure`. The controller passes what it detected from node provider IDs, or its `--deploy-cloud` override; required for `nlb_internal` |
| `service.allowedCidrs` | `[]` | `loadBalancerSourceRanges` for the `nlb_*` modes; empty = unrestricted. On the `ingress_*` modes it renders a `MiddlewareTCP` instead, since a shared load balancer cannot carry a per-endpoint allowlist — the API refuses it there for now |
| `service.ingress` | _(set by the controller)_ | the allocated shard slot: `shard`, `entryPoint`, `port`, `dnsName`, `shardHost`, `recordType`. Required for the `ingress_*` modes and rendered by nothing else |
| `service.dns` | _(set by the controller)_ | the `nlb_*` endpoint's published name: `name`, `target`, `recordType`. The `name` goes in the certificate. A `DNSEndpoint` for it is rendered only once `target` is set |
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
