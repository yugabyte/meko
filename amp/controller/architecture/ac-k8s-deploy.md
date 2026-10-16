# controller — Kubernetes endpoint deploy

**Status:** Current (demo-grade — see Assumptions)
**Last updated:** 2026-09-29

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
- the endpoints' own **ServiceAccount** (`amp-endpoint`), bound to nothing;
- the **Deployment**, wired for the Kubernetes driver (`AMP_DEPLOY_DRIVER=kubernetes`)
  and internal-LoadBalancer endpoints;
- an in-cluster **Service** (`amp-controller`, ClusterIP) exposing the API (`8080`)
  and the controller port (`50051`) that endpoints register against; and
- an **internal LoadBalancer** Service (`amp-controller-lb`) for the API on a
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
   │  (handlers.go: CreateEndpoint)        → plans a create_endpoint task → 202 {task}
   ⋮
task worker, create_endpoint step (internal/task/step_create_endpoint.go)
   ├─ EnsureEndpointRecord                     → inserts the endpoint row (address empty)
   ├─ deploy.KubernetesDeployer.CreateEndpoint → helm upgrade --install --wait, then verifyReady
   └─ SetEndpointAddress(<LB / NodePort / Service DNS>:<port>)
```

The API returns `202` with the task as soon as the work is planned; the caller
polls `GET /api/v1/tasks/{id}` (the `Location` header). Everything else happens in
the worker's `create_endpoint` step, where `helm upgrade --install --wait` blocks
until the release is ready and the resolved client address is then written back.
The row is inserted first so the single-writer rule is enforced before a Helm
install is spent; a duplicate `read_write` endpoint is a failed task, not a `409`.
A deploy that fails leaves the row addressless for the failed-create GC sweeper
to reclaim (`ac-tasks.md`).

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

### A failed deploy carries the pod's reason, not helm's timeout

When `--wait` runs out helm says only `context deadline exceeded` — the same
sentence for unschedulable, `ImagePullBackOff`, a crash loop and a wedged
supervisor alike, and the reason was on the pod all along (#635). So the deployer
reads the pod and puts its words in front of helm's: the `PodScheduled=False`
message (where `Insufficient ephemeral-storage` and the node counts live), else a
container's waiting reason and image, else its last exit code, else the pod's
recent `Warning` events.

It has to read **during** the wait, because helm tears the Deployment and pod
down when it gives up and there is nothing left to read afterwards
(`controller_runbook.md`, "A `helm --wait` pod is ephemeral"). `watchDeploy`
therefore snapshots the pod every 15s while helm blocks and keeps the most
specific answer; the failure path prefers a fresh read and falls back to that
snapshot. Namespaced pods and events only — the Role already grants both, and the
cluster-scoped node reads that would size the shortfall are deliberately not part
of this (#584, #602).

Only a **wait expiry** is enriched (`helm.ErrWaitTimeout`). Every other way an
upgrade fails — a chart render refusal, an unwritable values file, a cancelled
context — already carries its cause in helm's own message, and usually has no pod
at all; leading those with a pod diagnosis would bury the real error in the tail,
which is the position this whole mechanism exists to keep clear.

Events are read only on that failure path, never on a snapshot. A pod that is
merely still starting is inconclusive too, so consulting events per tick would put
a namespace-wide event LIST (the `involvedObject` fieldSelector is unindexed) on
every tick of every healthy deploy.

Two details about which events count. A **scheduling** failure asks for them even
though it is conclusive: the scheduler names the nodes it rejected, but only
cluster-autoscaler says why no node can be *added*. And the filter is not
Warning-only, because autoscaler records that as **Normal**
(`NotTriggerScaleUp: … 9 exceeded quota: "cluster-wide", resources: cpu`) — the
single line #635 was filed about, and the reason raising the deploy timeout
20m→35m never helped.

Retire is the mirror (`DELETE …/endpoints/{id}`): `helm uninstall
--ignore-not-found --wait` then drop the row → `204`. This is the design's
kill-before-create retire (ac-highlvl.md §4); `--wait` blocks until the
resources are gone.

### Endpoint health is read live

An endpoint's `address` is recorded at create and never cleared, so it cannot say
whether the endpoint is serving. `GET`/list endpoints therefore report `health`
from the pod itself: one pod LIST per request, selected by the release's
`app.kubernetes.io/instance` label (which every chart version sets, unlike the
newer identity labels). `ready` is the pod's `Ready` condition, which covers the
sidecars too; otherwise the reason is the same pod diagnosis a failed deploy
carries. A terminating pod is ignored, since under `Recreate` it is the old one. A
failed read reports no `health` rather than `not_ready`. The Docker and local
deployers do not implement it.

### Labels: identity and tags

Every endpoint's objects carry five identity labels, from `deploy.IdentityLabels`:

| Label | Value |
|---|---|
| `amp.yugabyte.com/endpoint-id` | the endpoint id |
| `amp.yugabyte.com/branch-id` | its branch id |
| `amp.yugabyte.com/cluster-id` | its cluster id |
| `amp.yugabyte.com/storage-pool-id` | its cluster's storage pool id |
| `amp.yugabyte.com/ep-type` | its role, `read_write` or `read_only` |

So `kubectl get pods -l amp.yugabyte.com/cluster-id=<id>` lists a cluster's
computes. With them come the endpoint's tags (`EndpointSpec.Labels`, its cluster's
merged with its own, see [`ac-apis.md`](ac-apis.md#tags-are-kubernetes-labels)).
Both render as the chart's `commonLabels`. The `yb-amp-endpoint.labels` helper puts them on every object of
the release, and the pod template gets them too. They never reach a selector: a
Deployment's selector is immutable, and the controller finds the pod by
`app.kubernetes.io/instance`. The chart drops any key it sets itself rather than
emitting it twice.

The pod also carries the pool's, cluster's, branch's and endpoint's names as the
annotations `amp.yugabyte.com/storage-pool-name`, `amp.yugabyte.com/cluster-name`,
`amp.yugabyte.com/branch-name` and `amp.yugabyte.com/endpoint-name`
(`deploy.IdentityAnnotations`, the chart's `podAnnotations`). They are annotations
because a name is free text, which a label value need not accept. An unnamed
endpoint has no `amp.yugabyte.com/endpoint-name`. A pool rename reaches the pod on
its next redeploy, not before.

The identity labels and the annotations are on every endpoint, so the first
redeploy of one created before them changes its pod template and restarts its pod. That is free on an
upgrade or an extension redeploy, which restart it anyway, and costs a restart
on a plain `POST /endpoint-rerenders`. A resize patches the pod in place and runs
no helm, so it is unaffected.

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
  - `ClusterIP` (the default) → the endpoint's own **Service DNS**
    `<release>.<ns>.svc.cluster.local:<computePort>` — in-cluster reachability only.

  All fall back to the Service DNS when the lookup isn't possible, and none of them
  is the **pod** address: nothing re-resolves a recorded address until the
  endpoint's next controller-driven redeploy, so a pod IP would let an extension add
  or an eviction silently invalidate a connection string a client already holds
  (CLOUDGA-36993). An LB ingress survives pod replacement, a node IP does not
  survive node replacement, and the Service DNS survives both.

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

Which disk that ephemeral storage *is* is a property of the node, not of the pod,
and nothing in the node API reports it: `emptyDir` lives under the kubelet's root,
which is the local NVMe on that GKE pool but the **EBS boot volume** on a stock EKS
node. An endpoint scheduled there works and is merely slow — the LFC runs at the
boot volume's IOPS (#1612). So the controller makes the operator say which, with
`--deploy-scratch-backing` / `AMP_DEPLOY_SCRATCH_BACKING` (chart:
`controller.endpoint.scratchBacking`):

- **`local-nvme`** — `/data` must be local NVMe. `--deploy-node-selector` is then
  **required** and must name the pool whose ephemeral storage is the instance NVMe
  (GKE labels its local-SSD pools itself; on EKS the operator labels the nodeadm
  `localStorage` group, `amp/local-nvme=true` by convention —
  `deploy/README.md`). A `--deploy-storage-class` alongside is refused: a PVC is
  a network volume, not scratch.
- **`node-disk`** — `/data` rides whatever the node has, and the operator has
  accepted that (kind, CI, a dev cluster).
- **empty** — undeclared. It stays the default because a controller that stops
  starting on upgrade is worse than a slow LFC; instead, when the `emptyDir` is in
  use and no selector pins it either, the controller **warns at startup** naming
  the three ways out. A selector on its own is a choice and silences it.

Neither whether a matching node really has NVMe under its kubelet (not visible
from the API) nor whether any node matches is checked: the recipe pools autoscale
from zero, and a selector no pool satisfies already surfaces per deploy as the
scheduler's `Pending` reason.

The controller deliberately does **not** inspect nodes to size this. `rbac.nodeRead`
does grant a cluster-scoped `nodes: [get, list]` since #1166, so the read is
available where it once was not (#584) — but sizing from it could only ever be an
estimate: the controller knows neither which node the pod
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
  is fine) or the pod silently drops to Burstable. Two sidecars exist today: the
  `postgres_exporter` metrics sidecar (#254) and the `documentdb-gateway`, which
  renders only on an endpoint whose branch mounts `documentdb` and publishes a
  second Service port (`documentdb`, 10260). Both carry request == limit, which
  `tests/guaranteed-qos.sh` asserts per scenario.

  The API reports the gateway as `documentdb_address`: the endpoint's own host on
  10260, so only where whatever fronts the endpoint publishes 10260 on that host
  (`DocumentDBGatewayReachable`). The ClusterIP Service does; so does the `-lb`
  Service of the `nlb_*` modes, and a shard for an `ingress_*` endpoint with a
  published name, which routes it by SNI ([`ac-ingress.md`](ac-ingress.md)
  §The documentdb gateway). A NodePort Service does not, since it gives 10260 a
  nodePort of its own, and neither does an `ingress_*` endpoint addressed by its
  shard's shared host. A `cluster_ip` endpoint under a LoadBalancer Service type
  is not reported either, nor an `nlb_*` endpoint whose address fell back to a
  nodePort because its load balancer was not ready in time. The rule reads the endpoint row, not the release, so an
  endpoint last deployed before its mode carried the gateway reports the address
  before its release publishes it, until it is redeployed
  (`runbooks/controller_runbook.md`).

  The gateway's probes are `tcpSocket` on its own port, deliberately not its
  `check` subcommand, since readiness is pod-wide. The port still waits on the
  extension: the gateway binds only once the documentdb schema exists, retrying
  every 10s until then. compute_ctl creates it after the compute is up: a
  `documentdb` database, then `CREATE EXTENSION documentdb CASCADE` in it,
  whenever `pg_documentdb` is preloaded, retried until it succeeds
  (compute/architecture/extensions.md, "documentdb creates itself"). So the
  gateway's startupProbe budget follows the endpoint container's own, derived
  below from `--deploy-timeout`, rather than a short one of its own.

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
- An uninstall that overlaps another uninstall of the same release (a cluster
  delete right behind its endpoint's delete) can lose the final purge: helm
  reports `Failed to purge the release: release: not found`, either as the sole
  error of `uninstallation completed with 1 error(s)` or alone when the release
  was already marked uninstalled. `--ignore-not-found` does not cover it, since
  the release existed when that uninstall started. The wrapper treats it as
  success: the release is gone. Any other uninstall error still fails the step.
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
| `--endpoint-image-repo` / `--endpoint-image-tag` | endpoint image (tag must be set). The tag is the **default**: an endpoint carrying its own `version` overrides it (`ac-endpoint-upgrade.md` §3) |
| `--deploy-pull-secret` | `imagePullSecrets` name in the namespace |
| `--deploy-compute-port` | compute Postgres port the Service publishes |
| `--deploy-service-type` | endpoint Service type: `ClusterIP` (default), `NodePort`, or `LoadBalancer` |
| `--deploy-service-annotations` | `k=v,k=v` annotations on the endpoint Service (e.g. internal-LB) |
| `--deploy-cloud` | override the cloud whose LB annotations the `nlb_*` connectivity modes render: `aws`, `gcp` or `azure`. Empty (normal) = detect from a node's `spec.providerID`, which `rbac.nodeRead` grants by default. Chart value: `controller.endpoint.cloud` |
| `--deploy-storage-class` | storage class for the endpoint `/data` PVC (EBS/PD); empty = ephemeral `emptyDir` |
| `--deploy-scratch-backing` | what the `emptyDir` `/data` sits on when no storage class is set: `local-nvme` (then `--deploy-node-selector` is required, naming the local-NVMe pool) or `node-disk` (the node's own disk, accepted). Empty (default) = the controller warns at startup when nothing pins `/data` (#1612). Chart value: `controller.endpoint.scratchBacking` |
| `--deploy-node-selector` | `k=v,k=v` node labels every endpoint pod must schedule onto. Chart value: `controller.endpoint.nodeSelector` |
| `--deploy-endpoint-service-account` | ServiceAccount every endpoint pod runs as; empty = the namespace's `default`. The chart creates `amp-endpoint`, bound to nothing, and endpoint pods mount no token either way, so code inside Postgres cannot reach the Kubernetes API. Chart value: `controller.endpoint.serviceAccount` |
| `--deploy-disk-size` | **floor** for the `/data` PVC when `--deploy-storage-class` is set (default `10Gi`); the size itself is 20 GiB × the endpoint's vCPU |
| `--deploy-disk-gib-per-vcpu` | GiB of `/data` per endpoint vCPU (`0` = the built-in 20; for a pool built to a different disk-per-core shape) |
| `--deploy-memory-gib-per-vcpu` | GiB of memory per endpoint vCPU (`0` = the built-in 8). Fills in the memory when a request names only CPU, and is the floor every request is held to — a pair below it is a `400`. The escape hatch for a pool whose nodes cannot back 8 GiB/core |
| `--deploy-lfc-offload-interval-seconds` | seconds between saves of each endpoint's LFC contents, which a restarted endpoint reloads in the background (`lfc.offloadIntervalSeconds` and `lfc.autoprewarm`, set together). Default `300`; `0` = neither. Chart value: `controller.endpoint.lfcOffloadIntervalSeconds` |
| `--deploy-disk-size-override` | flat `/data` size for **every** endpoint whatever its CPU, bypassing the ratio and the floor; empty (default) = CPU-proportional. Test clusters only |
| `--deploy-yb-tls-mode` | `require` \| `verify-ca` \| `verify-full`: render a cert-manager `Certificate` (CN = `--deploy-yb-user`) and the CA bundle into every endpoint and connect with them; empty (default) = plaintext. Endpoints created before it is set are brought onto it by `POST /api/v1/endpoint-rerenders` |
| `--deploy-yb-tls-issuer-kind` / `-name` / `-group` | issuer the endpoint certificates come from (default `ClusterIssuer` / `amp-ca` / `cert-manager.io`) |
| `--deploy-yb-tls-ca-configmap` | ConfigMap (key `ca.crt`) holding the trusted roots, mounted as the endpoints' `sslrootcert` (default `amp-ca-bundle`) |
| `--deploy-yb-tls-cert-duration` / `-renew-before` | endpoint certificate lifetime and renewal lead (default `720h` / `240h`) |
| `--deploy-compute-tls-mode` | `issue` \| `existing-secret`: the certificate each compute presents to its OWN clients, so `sslmode=verify-full` against a published endpoint name succeeds (`ac-compute-tls.md`). Empty or `disable` (default) = the compute offers no TLS. The opposite direction from `--deploy-yb-tls-*` above and independent of it |
| `--deploy-compute-tls-issuer-kind` / `-name` / `-group` | issuer for `mode=issue`, one `Certificate` per endpoint (default `ClusterIssuer` / `amp-ca` / `cert-manager.io`) |
| `--deploy-compute-tls-secret` | Secret (`tls.crt` + `tls.key`) every compute mounts under `mode=existing-secret`, typically one wildcard for the endpoint DNS zone |
| `--deploy-compute-tls-cert-duration` / `-renew-before` | compute certificate lifetime and renewal lead (default `2160h` / `720h`). Leave the duration **empty** for an ACME issuer, which fixes it at 90 days and only warns that the request was ignored |
| (no flag) | the endpoint's own client-facing address is added to its certificate automatically — as an IP SAN for an `nlb_*` load balancer or an `ingress_*` shard in a zone-less deployment, as a name otherwise (`ac-compute-tls.md` §The two addresses the chart cannot derive) |
| `--deploy-compute-tls-include-service-dns` | put the endpoint's in-cluster Service DNS name in its certificate (default true). Must be **false** for `mode=issue` against an ACME issuer: no public CA can validate a `.svc.cluster.local` name and one unvalidatable name fails the whole order. No effect under `mode=existing-secret`, which renders no `Certificate` |
| `--deploy-endpoint-user-split` | run each endpoint's pageserver and safekeeper and its compute as different Unix users (the endpoint chart's `userSplit`), so a compromised Postgres cannot read the YugabyteDB client key (default false). Needs the foundation to enforce client certificates; see `compute/architecture/single-container-endpoint.md` |

`--deploy-service-type` and `--deploy-service-annotations` above are the
deployment-wide default. **Client reachability is also a per-endpoint choice**,
set with `connectivity` on `POST .../endpoints` and reported back on the
endpoint:

| `connectivity.mode` | What the endpoint gets |
|---|---|
| `cluster_ip` (default) | the ClusterIP Service only — in-cluster, plus the tailnet when tailnet-exposed |
| `nlb_internal` | additionally a Service behind a VPC-routable load balancer of its own, carrying Postgres and, where it runs, the documentdb gateway, and a published name where there is a zone (§Published names) |
| `nlb_external` | the same, internet-facing |
| `ingress_internal` | a port on a **shared** VPC-routable load balancer instead of one of its own ([`ac-ingress.md`](ac-ingress.md)) |
| `ingress_external` | the same, internet-facing |

The `ingress_*` pair is refused with `400` unless the deployment has
`--deploy-ingress-enabled` set **and** this controller can place an endpoint on a
shard; the error says which is missing. `allowed_cidrs` on them is a per-endpoint
middleware in the shard's proxy, refused unless `--deploy-ingress-source-filtering`
says the shards see client addresses ([`ac-ingress.md`](ac-ingress.md#per-endpoint-allowlists)).

`connectivity.allowed_cidrs` becomes that Service's `loadBalancerSourceRanges`,
which covers the gateway port as well as Postgres's; empty is unrestricted, which on `nlb_external` leaves password authentication as
the only barrier. Each `nlb_*` endpoint provisions its own load balancer — a
billed resource that counts against the account's per-region quota. The mode is
**create-time only**: changing it replaces the load balancer and the address. The
allowlist is not — `POST …/endpoints/{endpointId}/connectivity` plans an
`update_endpoint_connectivity` task that patches `loadBalancerSourceRanges` on the live
`<release>-lb` Service and persists the list for the next render. It does not
helm-upgrade: a re-render applies everything else the stored record says too, and after
a resize (which patches only the pod) that changes the pod template and rolls the pod.
Reaching one from another VPC needs a service attachment the customer creates:
[`runbooks/psc-endpoint-publishing.md`](../../runbooks/psc-endpoint-publishing.md).

**Portable across clouds.** The mode is the concept; the annotations that
implement it are per-provider, so the deployer detects the cloud from a node's
`spec.providerID` (`aws://` / `gce://` / `azure://`) and passes it to the chart
as `service.cloud` — it cannot drift from the cluster it describes, and it needs
no configuration. Reading a node needs `nodes get, list`, granted by the
controller chart's `rbac.nodeRead` (**default true**): a read-only ClusterRole,
cluster-wide only because Node is a cluster-scoped kind, and deliberately
separate from `rbac.clusterScoped`, which carries the `namespaces create` grant
and stays off. Detection memoizes only a successful answer; a failed node list is
retried on the next create.

`--deploy-cloud` (chart: `controller.endpoint.cloud`) overrides it, for an
install that may hold no cluster-scoped binding at all, or a cluster whose
provider IDs the controller reads wrongly. Without either, an `nlb_internal`
create fails in the chart rather than defaulting to internet-facing.

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
  Postgres port and, on a documentdb endpoint, the gateway's — both of which
  make the client log in with SCRAM.
- **Pod-IP target group (AWS)** (`nlb-target-type: ip`), so there is no kube-proxy hop
  and the load balancer controller re-registers the pod when it is replaced. That
  is what keeps this address correct across a pod replacement, with no redeploy.
- **Cross-zone enabled (AWS).** An endpoint is one pod in one AZ while the NLB
  takes an interface per subnet; with cross-zone off (the AWS load balancer
  controller's default) interfaces in the other AZs have no target and clients
  resolving to them fail outright. GCP and Azure L4 load balancers are regional
  and need no equivalent.

Cloud load balancers often take longer to provision than
`lbAddressPollTimeout`, so `CreateEndpoint` falls back to the Service-type
address rather than failing the create; the endpoint is up and serving either
way. Nothing re-polls afterwards — the recorded address stays the fallback until
the endpoint's next controller-driven redeploy re-runs the resolution, so the
endpoint is reachable in-cluster throughout and only the external address waits.
Recovery: "Endpoint stuck on the in-cluster address after an `nlb_*` create" in
[`amp/runbooks/controller_runbook.md`](../../runbooks/controller_runbook.md).

A redeploy re-renders the release from `core.EndpointDeploy` alone, so that record
carries `Connectivity`/`AllowedCIDRs`: without them `service.connectivity` renders
empty, helm prunes the `-lb` Service, and the load balancer is deleted out from
under every client holding its address.

### Published names

A load balancer's own address is a bare IP on GCP and Azure and an ELB hostname
on AWS, and either dies with the load balancer. So where the deployment has a DNS
zone for the endpoint's exposure, an `nlb_*` endpoint is given a name in it,
`yb-ep-<id>.<zone>`, and `Endpoint.address` reports `<name>:<port>`. The zones are
the shared ingress's (`--deploy-ingress-zone-internal` for `nlb_internal`,
`-external` for `nlb_external`) and work with the ingress itself off. One
external-dns per zone publishes both modes' records.

- **The name is decided once.** `create_endpoint` mints it into the endpoint's
  `config.dns_name` and every deploy reads it from there. A zone set later
  therefore names no existing endpoint, and a zone changed later renames none,
  for the same reason as on the ingress path (`ac-ingress.md` §Config).
- **The full release name, not the ingress path's short `ep-xxxxxxxx`.** The
  short form stays unique only through the allocation table's index. This one is
  unique by construction, can never equal an ingress name in the same zone, and
  tells an operator which Service a record belongs to.
- **The record is a `DNSEndpoint` in the endpoint's release**, beside the `-lb`
  Service, so `helm uninstall` withdraws it. Its target is the load balancer's
  address, which the cloud assigns after the Service exists. A create therefore
  renders it on the second pass, together with the certificate SAN. A redeploy
  reads the target before rendering instead: rendering without it would have
  helm delete the live record, and external-dns withdraw the name, until the
  second pass. If that read fails the redeploy fails, rather than withdraw a
  live name over a transient error.
- **The name is in the certificate from the first render.** The chart adds
  `service.dns.name` to the SANs itself, so no reissue waits on the load
  balancer.
- **No record, no name.** If the load balancer has no address in time, the
  fallback above applies unchanged. If the second pass fails to publish the
  record, the endpoint reports the load balancer's own address. Either way the
  next redeploy publishes the name.
- **Rolling the controller back withdraws the names.** An older controller
  ignores `config.dns_name`, so its next redeploy of an endpoint renders no
  record. external-dns then deletes the name, and the address reverts to the
  load balancer's. Clients holding the name lose it, so a deployment that may
  roll back past this release should leave the zones unset.

external-dns: `amp/deploy/external-dns/install.sh`, once per zone. Credentials to
write the zone are the zone owner's to grant (on BYOC, the customer's) and are not
scripted.

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
- **`cloud_admin` is no longer well-known, but it is still shared.** Its
  password lives in the branch's catalog. A root branch's first writer sets it
  from the `password` its caller supplied, delivered as `COMPUTE_PASSWORD`; a
  fork inherits it with the rest of its parent's catalog, which is why a fork
  below the parent's [floor](controller-cluster-create.md#the-fork-floor) is
  refused. So the API requires a `password` only where `password_required` is
  true, and drops one sent anywhere else. There is no default to fall back to: a
  deploy that renders no verifier — a redeploy, a fleet roll, a `read_only`
  endpoint, a writer on a fork or on a root that already has a password — leaves
  the role's password exactly as the branch's catalog has it, and an endpoint
  created before any of this keeps whatever it was given then. What has not
  changed is that it is ONE superuser per branch, shared by every endpoint on it
  and inherited by forks — there is still no cluster isolation at the SQL layer,
  and no way for a customer to choose the user's name (yugabyte/meko#1354).

  **One exception: a Voyager migration target.** Its plan builds the
  create-endpoint step directly rather than through the handler that requires a
  password, and the credential the migrate job connects with is a hardcoded
  `cloud_admin` / `cloud_admin` — so the plan passes the verifier for exactly
  that, to keep the two halves agreeing. The target is a `cluster_ip` endpoint
  (the plan requests no other connectivity), so it is reachable in-cluster and
  over the tailnet, not from the internet. Giving it a real credential is
  yugabyte/meko#1473.

  The control plane stores no password and generates none: what travels to the
  pod (and sits in the release Secret and the create task's step row) is the
  verifier `pg_authid` itself holds — **SCRAM-SHA-256**, so it is not
  password-equivalent and neither artifact can be used to log in. The caller
  brings either the plaintext, which exists only in that request and must be
  printable ASCII (SASLprep — see `createEndpoint`'s `password`), or a verifier
  it computed itself, which is passed through as is.

  The supervisor applies that verifier only on a role that has no password yet,
  and does nothing at all when the pod carries no `COMPUTE_PASSWORD`. So a
  redeploy or a fleet roll — neither of which renders a verifier — leaves the
  credential alone instead of resetting it, and a password a customer set
  themselves survives a restart.

  **Migration.** The `pg_hba` rule is `scram-sha-256` and ships in the endpoint
  image, so an endpoint created before this keeps working until it is rolled
  onto an image carrying it. At that roll its md5-era `cloud_admin` can no
  longer authenticate — every remote client gets `password authentication
  failed` — while loopback stays `trust` and the data is untouched. The fix is
  one statement per endpoint, run **after** the roll (before it,
  `password_encryption` is still md5 and this just writes another md5 verifier):

  ```bash
  # deploy/… , not a bare name: yb-ep-<endpoint-id> is the Deployment, and the
  # pod carries a generated suffix.
  kubectl -n "$NS" exec -it deploy/yb-ep-<endpoint-id> -- \
    /usr/local/v18/bin/psql -h 127.0.0.1 -p 55433 -U cloud_admin -d postgres \
    -v ON_ERROR_STOP=1 -c "ALTER ROLE cloud_admin PASSWORD '<original password>'"
  ```

  It sticks: the roll is what removed `COMPUTE_PASSWORD` from the pod, so
  nothing re-asserts the old verifier on a later restart. To find the endpoints
  that still need it, `SELECT left(rolpassword, 4) FROM pg_authid WHERE rolname
  = 'cloud_admin'` reads `md5…` rather than `SCRA`. Recreating the endpoint
  works too, and is simpler where the data is disposable.
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
- **An external client address is a point-in-time value.** An LB ingress or a
  NodePort address is recorded at create time and not refreshed; the `ClusterIP`
  default records the Service DNS, which does not go stale. The LoadBalancer poll
  (~2 min) can time out for a slow internal LB and fall back.

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
