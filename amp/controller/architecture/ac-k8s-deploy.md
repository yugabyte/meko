# controller — Kubernetes endpoint deploy

**Status:** Current (demo-grade — see Assumptions)
**Last updated:** 2026-07-01

How the **amp controller** runs on Kubernetes and deploys compute **endpoints**
there ([#108](https://github.com/yugabyte/amp/issues/108)). The k8s deployer
is one concrete implementation behind the **provisioning interface** the design
mandates (amp-controller.md principles 2–3): runtime-specific "create a pod
vs. a VM" logic lives behind `deploy.Deployer` (`CreateEndpoint`/`DeleteEndpoint`),
not in the core, so other runtimes slot in behind the same interface. The driver
is chosen by `--deploy-driver`: **`docker`** (default — one container per
endpoint, for single-host integration) or **`kubernetes`** (this doc, the
`KubernetesDeployer`). On Kubernetes the deploy mechanism is **Helm**
(amp-controller.md §11): the controller shells out to the `helm` CLI rather
than embedding a Helm SDK — the same "shell out to a managed binary" posture used
for the controller (§11.1).

## Deploying the controller

The controller is a **single bundled container** — `controller` (the control-plane API
+ task worker) plus the **controller** it runs as a managed subprocess
(deploy/controller, [#94](https://github.com/yugabyte/amp/issues/94)) — with the
`helm` CLI and the `yb-amp-endpoint` chart baked into the image.

[`deploy/controller/redeploy.sh`](../../deploy/controller/redeploy.sh) is the canonical
(re-)deploy. It is idempotent (`helm upgrade --install` of the
[`yb-amp-controller`](../../deploy/helm/yb-amp-controller) chart, mapping its env
interface onto chart values) and lays down:

- a **ServiceAccount** + namespaced **Role/RoleBinding** — the controller drives
  Helm to create/delete endpoint Deployments, Services, ConfigMaps, Secrets (and
  Helm's own release Secrets), and reads pods/services for address lookups — plus
  a small **ClusterRole** for `namespaces` (Helm's `--create-namespace` probes it)
  and `nodes` (the NodePort address resolver reads a node IP);
- the **Deployment**, wired for the Kubernetes driver (`AMP_DEPLOY_DRIVER=kubernetes`)
  and internal-LoadBalancer endpoints;
- an in-cluster **Service** (`amp-controller`, ClusterIP) exposing the API (`8080`)
  and the controller port (`50051`) that endpoints register against; and
- an **internal LoadBalancer** Service (`amp-controller-lb`) for the UI/API on a
  private VPC IP (the SC port stays on the ClusterIP only);
- the **MCP server** as its own pod (`amp-controller-mcp` Deployment + Service) —
  a stateless HTTP wrapper of the controller REST API — gated by `mcp.enabled`; and
- the **`amp_ui` UI** as its own pod (`amp-controller-ui` Deployment + Service) —
  nginx serving the SPA and reverse-proxying `/api/` to the in-cluster controller
  Service — gated by `ui.enabled`.

The MCP and UI Services are exposed the same way as the controller (one
`CONTROLLER_EXPOSE` knob adds a matching `-np` / `-lb` Service to each).

For reaching the control plane and endpoints over a Tailscale tailnet (and the
`tailnet_address` the controller reports alongside the LB address), see
[`ac-tailnet.md`](ac-tailnet.md).

It does **not** create the namespace or the backing YugabyteDB — those pre-exist
(see Assumptions) — but it **does (re)mint the image pull secret** each run.
Everything is env-overridable (`NS`, `CONTROLLER_TAG`, `ENDPOINT_TAG`,
`YB_HOST_PORTS`, `LB_ANNOTATIONS`, …).

## The shape — one endpoint = one Helm release = one pod

```
POST …/storage-pools/{pool}/clusters/{proj}/branches/{br}/endpoints
   │  (handlers.go: CreateEndpoint)
   ├─ core.Backend.CreateEndpoint              → inserts the endpoint row (address empty)
   ├─ deploy.KubernetesDeployer.CreateEndpoint → helm upgrade --install --wait
   └─ core.Backend.SetEndpointAddress(<LB IP / pod IP / Service DNS>:<port>)
                                                              → 201 {endpoint}
```

The deploy is **synchronous**: `helm upgrade --install --wait` blocks until the
release is ready, then the resolved client address is written back and the call
returns `201`. The row is inserted first so the single-writer rule is enforced
before a Helm install is spent; if the deploy then fails, the just-inserted row
is removed so it does not linger undeployed.

`--wait` covers the pod, not the Service: Helm's readiness check for a Service
verifies it has a ClusterIP (and, for a LoadBalancer, an ingress address) and
never looks at its endpoints. So a release is ready while the ClusterIP still
answers a REJECT — and the controller reaches an endpoint's pageserver/safekeeper
over exactly that Service, which is how it came to report an endpoint ready and
then refuse its own follow-up call (#724).

So the deploy is `helm --wait` **and** `verifyReady`: a `GET /v1/status` against
the endpoint's safekeeper, over the same Service DNS the controller's follow-up
calls use, must answer 2xx. Only then does the task succeed.

It is a round trip rather than a read of the Service's `Endpoints`, because
routable and ready are different facts and only the first is visible there — an
address is listed ready because the *pod's* readiness probe passed (that probe is
on `pg-compute`) and the port is listed because the Service declares it, so
neither observes the safekeeper. The GET subsumes the routing check anyway: a
Service with no backend REJECTs. `/v1/status` is on the safekeeper's
unauthenticated allow-list, so the probe carries no token.

It retries every second for 30s, so neither a Service a beat behind nor a
safekeeper still binding its port fails an otherwise good deploy. A failure leaves
the release installed and the endpoint row addressless, the same shape as any
other failed deploy; the create is idempotent, so re-issuing it converges.

Retire is the mirror (`DELETE …/endpoints/{id}`): `helm uninstall
--ignore-not-found --wait` then drop the row → `204`. This is the design's
kill-before-create retire (ac-highlvl.md §4); `--wait` blocks until the
resources are gone.

## Materializing a branch: the creation record

Cluster/branch creation and endpoint creation are **separate** steps, joined by
the branch's **creation record** (full detail in
[controller-cluster-create.md](controller-cluster-create.md)):

- **create-cluster** (and **create-branch**) records the branch in the
  control-plane DB (`branch`) **and** a creation record in the storage
  controller, keyed by `branch_id`. It does **not** deploy anything or
  eagerly materialize the branch's storage.
- **create-endpoint** deploys the endpoint. On first materialization the pod's
  pageserver reads its init mode off the branch's `amp_storage.branch_state` row
  — a NULL `ancestor_branch_id` means `initdb` + import, a set one means fork
  that ancestor — then registers + attaches to the controller (below). The
  creation record is not consumed: it stays as the branch's durable record. There
  is no separate create-database call — the first endpoint is what turns the
  declared branch into real storage.

The storage layer and the control plane identify a branch the same way — a SQL
`uuid`, rendered in the uuid's own spelling — so chart values carry it verbatim.
(The endpoint image receives it as the `BRANCH_ID` env var, #168.)

### Extensions (ImageVolumes)

Endpoints mount externalized PostgreSQL extensions (pgvector, …) as read-only
Kubernetes ImageVolumes rather than baking them into the endpoint image. The
extension set is opted into **per branch** (catalog defaults ∪ the branch's
opted-in set): seeded on the default branch at `createCluster`, inherited by a
fork, and extended on a single branch with `addBranchExtensions`. The controller
resolves it to pinned images via
[`internal/extensions`](../internal/extensions/catalog.go) and renders them into
the chart's `endpoint.extensions`. The full design — build pipeline, the two
PG18 path GUCs, and per-branch storage — is in
[`compute/architecture/extensions.md`](../../compute/architecture/extensions.md).

## Controller coordination + addressing

With `controller.url` set (the controller path), the endpoint pod runs its branch
bookkeeping against the controller. **Write fencing is not part of that**: the
pod's supervisor holds a YSQL lease on the branch and passes it to its services
as their write deadline (compute/architecture/compute-fencing.md), so a
replacement pod waits out the outgoing one's lease rather than being ranked
against it by the controller. (The safekeeper is endpoint-owned and not
registered with the controller.)

> **Planned direction:** the remaining calls move *out of the pod and into amp's
> deploy code*, so the pod stops self-registering. This evolves toward a
> **create-then-configure / pre-warm** model: pods are created unassigned and
> bound to a branch on demand via
> a push `configure` (amp mints `branch_id`; the pod returns
> `start_lsn`). See
> [`sc-integration.md`](sc-integration.md).

Two distinct network paths:

- **controller → endpoint** (heartbeats, branch ops): the controller dials the
  endpoint's **own stable Service DNS** — `CONTROLLER_REACHABLE_HOST =
  <release>.<ns>.svc.cluster.local` — and the SC-needed HTTP port
  (pageserver) is published on that Service. Because the address is
  the Service, not the pod IP, a **rescheduled pod (new IP) re-registers the same
  address** and the controller's node-register stays `UpToDate` instead of
  returning `409` — the fix for the reschedule crash-loop
  ([#121](https://github.com/yugabyte/amp/issues/121)).
- **client → endpoint** (SQL): the address recorded on the endpoint row, resolved
  from the Kubernetes API once the release is ready, by `--deploy-service-type`:
  - `LoadBalancer` → the external **LB ingress IP** `<lbIP>:<computePort>` (the
    deployer polls until the cloud LB assigns it, ~2 min, then falls back);
  - `NodePort` → **`<nodeIP>:<nodePort>`** — the nodePort allocated for the compute
    port on any **Ready** node's address (an `ExternalIP` if the nodes have one,
    else an `InternalIP`/VPC address). No cloud LB to provision; reaching it may
    require a firewall rule allowing the node-port range to the nodes. Reading node
    IPs needs the cluster-scoped `nodes` get/list grant in the controller's
    ClusterRole (`redeploy.sh`). NB: a NodePort Service applies to *all* its ports,
    so with SC coordination on (the default) the pageserver/safekeeper
    HTTP ports are exposed on node ports too — demo-grade, like the rest of this path;
  - `ClusterIP` → the **pod IP** `<podIP>:<computePort>` (connect in-cluster).

  All fall back to the stable **Service DNS** when the lookup isn't possible. The
  LB/node/pod IP is **not** stable across pod (or node) restarts; the Service DNS is.

The deployed endpoints reach their **backing YugabyteDB** by its in-cluster
Service DNS (`--deploy-yb-host-ports`), which may differ from the controller's own
`--database-url` path to its state. The `branch_id` is the shared key.

## Endpoint data volume (`--deploy-storage-class`)

Each endpoint's `/data` (the compute's `pgdata`, the LFC cache, logs and
pidfiles) is provisioned from a storage class when `--deploy-storage-class` is
set: the chart renders a `ReadWriteOnce` **PersistentVolumeClaim** of that class
(a network block volume — **EBS** on AWS, **Persistent Disk** on GCP) and mounts
it at `/data`. Its size is **proportional to the endpoint's CPU** — **20 GiB per
vCPU**, so a 2-vCPU endpoint gets 40 GiB — with `--deploy-disk-size`
acting as a **floor** rather than the size itself.

That ratio is the **policy**, and it is **uncapped**: a 32-vCPU perf endpoint asks
for the full 640 GiB. Nothing clamps it, because the failure mode of a ratio a
pool cannot back belongs to the scheduler — the pod stays `Pending` and says so —
rather than to the controller quietly handing an endpoint less disk than its CPU
allocation will fill. The corollary is a **provisioning requirement**: whatever
backs `/data` has to be able to supply 20 GiB per vCPU for the largest endpoint
the deployment intends to run, or that endpoint never schedules.
`--deploy-disk-gib-per-vcpu` / `AMP_DEPLOY_DISK_GIB_PER_VCPU` overrides the ratio
for a pool deliberately built to a different disk-per-core shape.

`--deploy-disk-size-override` / `AMP_DEPLOY_DISK_SIZE_OVERRIDE` (chart:
`controller.endpoint.diskSizeOverride`) abandons the ratio: **every** endpoint's
`/data` is that flat size whatever its CPU, bypassing both the ratio and the
`--deploy-disk-size` floor. It is for **test clusters**, where the provisioning
requirement above is the obstacle rather than something to satisfy. A GKE node on
the default 100GB boot disk reports ~43.8 GiB *allocatable* ephemeral storage on
every machine shape, so a 2-vCPU endpoint's 40 GiB request packs **one endpoint
per node**, and under a cluster-wide auto-provisioning quota the endpoint that
cannot get a fresh node stays `Pending` indefinitely (#602). `10Gi` fits four.
Empty by default, and it should stay empty in prod: a flat size promises nothing
will fill the disk, which holds for a test's scratch data and not for a real
workload's. The Deployment always uses the **`Recreate`** strategy (stop the old
pod before starting the new one): an endpoint is a single-writer compute, so a
redeploy must not surge a second pod that would fight the old one for the storage
writer fence (and a ReadWriteOnce PVC independently attaches to one pod at a
time). The pod's
**`podSecurityContext.fsGroup`** (default `1000`, the endpoint image's `yugabyte`
gid) makes the otherwise root-owned provisioned volume writable by the non-root
process — a single source for the fsGroup, applied whether or not a PVC is used (an
`emptyDir` is world-writable and unaffected). The PVC is part of the Helm release,
so `helm uninstall` on retire frees the volume. With the flag **empty**, `/data`
falls back to the current ephemeral **`emptyDir`** (no persistent volume). All durable state still lives in YugabyteDB; the volume is
local scratch + cache, so a pod (or PVC) reset is safe.

The `emptyDir` case takes the **same** per-vCPU size, and differs only in who
enforces it. It is carved out of the **node's** ephemeral storage, which every
pod on that node shares, so the size is declared twice: as the volume's
`sizeLimit`, and as an `ephemeral-storage` **request/limit** on the endpoint
container. The `sizeLimit` is only an eviction ceiling — it reserves nothing —
whereas the request is what the **scheduler** accounts against the node's
capacity, so it will not place more endpoints on a node than its disk can back.
That is what keeps co-tenants from oversubscribing it.

This is also where the 20 GiB/vCPU provisioning requirement lands in practice.
When the endpoint pool is a GKE node pool created with
`--ephemeral-storage-local-ssd` (the `cloud.google.com/gke-ephemeral-storage-local-ssd`
label `--deploy-node-selector` pins endpoints to), the node's local NVMe SSDs
*are* its ephemeral storage — so the local-SSD count has to be sized against the
pool's vCPU count, not left at whatever the machine type defaults to. A 32-vCPU
node needs **2 local SSDs** (750 GiB raw) to seat one 32-vCPU endpoint; a single
375 GiB SSD leaves the pod `Pending`. It also means the volume is local NVMe
rather than network-attached, which is why `emptyDir` is the right backing for
this pool and a PVC/generic-ephemeral volume — provisioned by a CSI driver
against a storage class — would silently move `/data` onto a network disk.

The controller deliberately does **not** inspect nodes to size this. Deriving
the ratio from a node's own allocatable disk needs a cluster-scoped
`nodes: [get, list]` read, which BYOC deployments will not grant (#584), and it
could only ever be an estimate: the controller knows neither which node the pod
will land on nor which endpoints will land there afterwards. The scheduler knows
both, and `ephemeral-storage` is how it is told. Reading it from inside a pod is
no better — an `emptyDir`'s `sizeLimit` is enforced by the kubelet watching disk
usage rather than by a filesystem quota, so `statvfs` there reports the whole
node filesystem.

Both sizes are fixed at create: a PVC and an `emptyDir` `sizeLimit` cannot be
changed in place, so a later CPU resize does not resize the volume.

`createEndpoint` carries a per-endpoint **`disk_type`** (`network` default,
`local`). `network` uses the configured storage class above; `local` (a
node-local instance SSD, typically NVMe) is **not yet supported** and is rejected
with `400` before any row is committed — tracked as a follow-up (the deployer has
no local-disk path yet).

## Packages

- **`internal/helm`** — a thin, one-shot wrapper over the `helm` CLI
  (`UpgradeInstall`, `Uninstall`). Per-endpoint values are marshaled to a temp
  file passed with `--values` (JSON is valid YAML, sidestepping `--set` escaping).
- **`internal/deploy`** — the `Deployer` interface and its `KubernetesDeployer` /
  `DockerDeployer` implementations + the `EndpointSpec` the handler hands them.
  The Kubernetes deployer maps an endpoint to chart values, looks up the client
  address via the in-cluster service account (`k8sAPIGet`), and sets the Service
  type/annotations. `EndpointSpec.CPU`/`Memory` (defaulted to 2 cores / 4Gi) map
  to `endpoint.resources.{cpu,memory}`, which the chart renders as **both**
  `requests` and `limits` on the endpoint container so the pod gets the
  **Guaranteed** QoS class. NB: Guaranteed is a *pod*-level property — every
  container in the pod must set request == limit. If a future change adds a
  sidecar (e.g. the planned `postgres_exporter` metrics sidecar, #254), that
  container must also carry a request == limit block (a small fixed reservation
  is fine) or the pod silently drops to Burstable.

  It also derives the pod's **bootstrap budget** from `--deploy-timeout`:
  `startupProbe.failureThreshold` and the Deployment's `progressDeadlineSeconds`.
  Both exist so the two clocks that can kill a still-starting compute outlast the
  one the deployer itself waits on. Without the startupProbe the liveness probe
  owns the startup window (90s) and a slower bootstrap crash-loops; without the
  progress deadline the Deployment reports `ProgressDeadlineExceeded` at
  Kubernetes' 600s default while the kubelet is still legitimately waiting
  (#960). The kubelet's budget is `periodSeconds * (failureThreshold - 1)`, not
  the product — the prober probes once immediately, then on the period.

Wired in `cmd/controller/main.go` (`buildProvisioning`): with `--deploy`, a
`KubernetesDeployer` or `DockerDeployer` is built per `--deploy-driver` and passed
to the API as `api.Provisioning.Deployer`. With `--deploy` off, the endpoint
create/retire routes refuse with `501` (read routes stay).

## Resizing an endpoint in place

`resizeEndpoint` changes a running endpoint's CPU/memory **without a restart when
the node has room**, via the `resize_endpoint` task → `deploy.ResizeEndpoint`.
Kubernetes only does in-place resize through the **pod `resize` subresource** — it
is a Pod-only operation; workload controllers are not resize-aware (through k8s
1.35). So a resize is **two writes**, in order:

1. **Database config** — `UpdateEndpointResources` merges the new cpu/memory into
   the endpoint `config` jsonb (the (re)create spec). A controller-driven redeploy
   re-renders from this, so the new size is durable across redeploys.
2. **In-place resize** — a strategic-merge `PATCH` of the running pod's
   `.../pods/{pod}/resize` subresource, so the current pod adopts the new size
   live. The deployer reads back the pod's resize conditions and reports the
   outcome (`completed` / `pending` / `infeasible` / `unsupported`) on the task
   result; the API and UI surface it.

**Why we do *not* patch the Deployment template.** Editing the Deployment's
`spec.template` resources changes the pod-template-hash, which makes the
Deployment controller start an ordinary **rollout and recreate the pod** — the
exact opposite of an in-place resize. So the size lives on the pod (via the
`resize` subresource) and in the DB `config` (the durable desired state), not on
the Deployment template. **Known gap:** an *involuntary* recreate (node loss /
eviction) before the next controller-driven redeploy brings the pod back at the
**old** size — the size stays durable in the DB and converges on the next
redeploy, but there is no periodic reconcile that closes the window automatically.
Endpoints don't use Deployment rollouts (single replica, `Recreate` strategy), so
the clean fix is to move the endpoint workload to a bare **ReplicaSet** (a
template PATCH there doesn't roll) — tracked as a follow-up.

A resize is **first-writer-wins**: a partial unique index on the task table
(`tasks_one_active_resize_per_endpoint`) admits at most one in-flight
`resize_endpoint` per endpoint, so a concurrent resize is rejected with `409`
until the first reaches a terminal state (the lock self-releases; a crashed task
is reclaimed by its lease). CPU/memory are set as request == limit, preserving
the pod's **Guaranteed** QoS class (in-place resize cannot change QoS). The
`resize` PATCH goes through the same raw in-cluster REST path as the read lookups
(`k8sAPIPatch`, sibling of `k8sAPIGet`) — **no client-go dependency, no bundled
`kubectl`**. The `pods/resize` subresource is a *separate* RBAC resource, granted
explicitly in the controller Role (`resources: [pods/resize], verbs: [patch]`).
The endpoint chart declares a per-container `resizePolicy`
(`endpoint.resizePolicy`, default cpu+memory `NotRequired`) so the kubelet adjusts
the cgroup without a container restart. Requires k8s ≥ 1.33 (in-place resize beta,
on by default); on older clusters the subresource PATCH is rejected and the
outcome surfaces as an error. The Docker deployer returns `unsupported`.

## Idempotency

Deploy/retire converge on retry rather than duplicating or wedging:

- `helm upgrade --install` converges; a prior attempt killed mid-flight can leave
  the release `pending-*`, which would make the next install refuse — so the
  `internal/helm` wrapper first clears a stuck pending release (`helm status` →
  `uninstall`) before installing.
- `SetEndpointAddress` is an absolute `UPDATE … WHERE id = $1`.
- `helm uninstall --ignore-not-found` tolerates an already-gone release (helm ≥ 3.13).
- `DeleteEndpoint` is `DELETE … WHERE id = $1` (absent row = no-op).
- `RecordBranch` is idempotent: it fills a branch's creation group once and
  leaves an existing record untouched. A *newly registered* fork is also
  validated against the ancestor's GC cutoff (below it → 400, the insert
  rolls back); a re-record skips that check, so a retry after the cutoff
  advanced stays a no-op success.

## Configuration (§9)

The `--deploy*` block (env `AMP_DEPLOY*`), validated only when `--deploy` is set.

Common to both drivers: `--deploy`, `--deploy-driver`, `--deploy-yb-host-ports` /
`-yb-user` / `-yb-dbname` / `-yb-password` (or `--deploy-yb-existing-secret`),
`--deploy-controller-url`, `--deploy-timeout`.

`--deploy-timeout` is not only the caller's patience: on the Kubernetes driver it
also sets the pod's startupProbe budget and `progressDeadlineSeconds`, so raising
it genuinely gives a slow bootstrap more room rather than just waiting longer for
the same kill (#960).

Kubernetes driver:

| Flag (env) | Purpose |
|---|---|
| `--deploy-namespace` | namespace endpoints are deployed into |
| `--helm-bin` / `--deploy-chart` / `--kubeconfig` | helm binary, chart path, KUBECONFIG (empty = in-cluster) |
| `--endpoint-image-repo` / `--endpoint-image-tag` | endpoint image (tag must be set) |
| `--deploy-pull-secret` | `imagePullSecrets` name in the namespace |
| `--deploy-compute-port` | compute Postgres port the Service publishes |
| `--deploy-service-type` | endpoint Service type: `ClusterIP` (default), `NodePort`, or `LoadBalancer` |
| `--deploy-service-annotations` | `k=v,k=v` annotations on the endpoint Service (e.g. internal-LB) |
| `--deploy-storage-class` | storage class for the endpoint `/data` PVC (EBS/PD); empty = ephemeral `emptyDir` |
| `--deploy-disk-size` | **floor** for the `/data` PVC when `--deploy-storage-class` is set (default `10Gi`); the size itself is 20 GiB × the endpoint's vCPU |
| `--deploy-disk-gib-per-vcpu` | GiB of `/data` per endpoint vCPU (`0` = the built-in 20; for a pool built to a different disk-per-core shape) |
| `--deploy-memory-gib-per-vcpu` | GiB of memory per endpoint vCPU (`0` = the built-in 8). Fills in the memory when a request names only CPU, and is the floor every request is held to — a pair below it is a `400`. The escape hatch for a pool whose nodes cannot back 8 GiB/core |
| `--deploy-disk-size-override` | flat `/data` size for **every** endpoint whatever its CPU, bypassing the ratio and the floor; empty (default) = CPU-proportional. Test clusters only |

`--deploy-service-type` and `--deploy-service-annotations` above are the
deployment-wide default. **Client reachability is also a per-endpoint choice**,
set with `connectivity` on `POST .../endpoints` and reported back on the
endpoint:

| `connectivity.mode` | What the endpoint gets |
|---|---|
| `cluster_ip` (default) | the ClusterIP Service only — in-cluster, plus the tailnet when tailnet-exposed |
| `nlb_internal` | additionally a Postgres-only Service behind a VPC-routable AWS NLB |
| `nlb_external` | the same, internet-facing |

`connectivity.allowed_cidrs` becomes that Service's `loadBalancerSourceRanges`;
empty is unrestricted, which on `nlb_external` leaves password authentication as
the only barrier. Each `nlb_*` endpoint provisions its own load balancer — a
billed resource that counts against the account's per-region quota.

**Portable across clouds.** The mode is the concept; the annotations that
implement it are per-provider, so the deployer detects the cloud from a node's
`spec.providerID` (`aws://` / `gce://` / `azure://`) and passes it to the chart
as `service.cloud` — no configuration, and it cannot drift from the cluster it
describes. `KubernetesConfig.Cloud` overrides it.

| Cloud | Internal | External |
|---|---|---|
| AWS | NLB, `scheme: internal`, pod-IP targets, cross-zone on | the same, `internet-facing` |
| GKE | `networking.gke.io/load-balancer-type: Internal` (add `…-allow-global-access` for clients in another region) | no annotation — the default |
| AKS | `service.beta.kubernetes.io/azure-load-balancer-internal: "true"` | no annotation — the default |

`allowed_cidrs` needs no per-cloud handling: `loadBalancerSourceRanges` is a core
Service field. An **internal** LB whose cloud is unknown **fails the render**
rather than falling back — every provider defaults a LoadBalancer Service to
internet-facing, so a silent fallback would publish exactly what the caller asked
to keep private. An external one renders fine without knowing the cloud, since it
needs no annotation anywhere.

Three properties are deliberate:

- **A second Service, not a type change.** The main endpoint Service also carries
  `sup-http` (9899), the supervisor's settings API — unauthenticated, and able to
  reload config and restart Postgres. The load-balanced Service publishes the
  Postgres port alone.
- **Pod-IP target group (AWS)** (`nlb-target-type: ip`), so there is no kube-proxy hop
  and the load balancer controller re-registers the pod when it is replaced. That
  is what makes this address durable where the stored pod IP is not.
- **Cross-zone enabled (AWS).** An endpoint is one pod in one AZ while the NLB
  takes an interface per subnet; with cross-zone off (the AWS load balancer
  controller's default) interfaces in the other AZs have no target and clients
  resolving to them fail outright. GCP and Azure L4 load balancers are regional
  and need no equivalent.

Cloud load balancers often take longer to provision than
`lbAddressPollTimeout`, so `CreateEndpoint` falls back to the Service-type
address rather than failing the create; the endpoint is up and serving either
way. Nothing re-polls afterwards — the recorded address stays the fallback until
the endpoint's next controller-driven redeploy re-runs the resolution. Recovery:
"Endpoint stuck on a pod IP after an `nlb_*` create" in
[`amp/runbooks/controller_runbook.md`](../../runbooks/controller_runbook.md).

A redeploy re-renders the release from `core.EndpointDeploy` alone, so that record
carries `Connectivity`/`AllowedCIDRs`: without them `service.connectivity` renders
empty, helm prunes the `-lb` Service, and the load balancer is deleted out from
under every client holding its address.

Docker driver: `--deploy-endpoint-image`, `--deploy-network`,
`--deploy-external-host`.

`--deploy` requires helm ≥ 3.13 on `PATH`.

## Assumptions (current)

The deploy is **demo-grade**. What it currently takes for granted:

- **External, pre-existing YugabyteDB.** controller does not run or create YB; the
  controller, the controller, and every endpoint connect to an existing
  universe (`yb_storage`, schemas `controller` / `controller` / `pageserver` /
  `safekeeper`). State **persists across re-deploys** — `redeploy.sh` re-attaches
  to it (migrations are idempotent); deleting the controller does not lose data.
- **Pre-existing namespace; the pull secret is (re)minted.** The namespace must exist
  before `redeploy.sh` (it warns if absent). The `pull-secret` is **(re)minted on
  every run** by both `up.sh` and `redeploy.sh` from the `dshubin_bot` bot account
  (its ECR token expires ~12h; creds resolved via `deploy/ecr-creds.sh` — env keys >
  `AWS_BOT_JSON` > SSO; see `deploy/README.md` "ECR access & the pull secret"). A
  stale/missing secret is the usual cause of `ImagePullBackOff`. Images come from
  `amp/controller` + `amp/endpoint` at `454529406029.dkr.ecr.us-west-2.amazonaws.com`.
  Everything lives in a **single namespace** (no cross-namespace deploys).
- **No authentication.** The control-plane API + UI are unauthenticated. Mitigated only by **internal
  LoadBalancers** (private VPC IPs, GKE `…/load-balancer-type: Internal`) — not by
  any authn/authz. Do not expose publicly.
- **Shared, well-known endpoint credentials.** Every endpoint's compute uses the
  same `cloud_admin` / `cloud_admin` superuser (the `COMPUTE_PASSWORD` default);
  there is no per-endpoint credential generation or cluster isolation at the SQL
  layer.
- **Postgres v18 only.** create-cluster rejects any other `pg_version`.
- **One pageserver+safekeeper per endpoint pod**, registered with a **unique SC
  node id derived from the branch id** (a 31-bit hash). Collision-resistant, not
  collision-proof. One `read_write` endpoint per branch (single-writer).
- **Bootstrap-on-first-endpoint.** Branches are materialized by the first
  endpoint's `initdb`, driven by the branch's seeded `branch_state` row — there
  is no standalone create-cluster/branch flow yet.
- **Synchronous, non-atomic provisioning.** create-cluster / create-endpoint run
  inline on the request (no executing write-ahead task framework —
  [#84](https://github.com/yugabyte/amp/issues/84) /
  [#113](https://github.com/yugabyte/amp/issues/113)); a crash mid-flow can
  orphan committed state ([#106](https://github.com/yugabyte/amp/issues/106)).
- **Client address is a point-in-time value.** The LB/pod IP recorded at create
  time is not refreshed; the stable Service DNS is the durable address. The
  LoadBalancer poll (~2 min) can time out for a slow internal LB and fall back.

## Known edges / not yet

- **No write-ahead task framework.** Deploy/retire run inline, so the HTTP request
  stays open for the whole `helm --wait` (up to `--deploy-timeout`); moving onto a
  durable, resumable task is the follow-up (#84 / #113).
- **No reconciliation / drift detection** (intent vs. live k8s state,
  amp-controller.md principle 6); deploy/retire are one-shot reconciles.
- **No attach/detach handoff.** Retire is kill-before-create, correct absent a
  network partition (ac-highlvl.md §4 partition gap). Write fencing is the
  endpoint supervisor's own lease (compute/architecture/compute-fencing.md),
  which is what makes kill-before-create safe under a partition; live migration
  of a branch between endpoints is still not built.
- **Pre-fix endpoints don't self-heal.** An endpoint registered before the stable
  Service-DNS change (#121) has a stale node address in the SC and will still
  `409` on reschedule; redeploy it (new endpoints are unaffected).
