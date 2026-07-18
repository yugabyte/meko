# controller — Kubernetes endpoint deploy

**Status:** Current (demo-grade — see Assumptions)
**Last updated:** 2026-07-01

How the **amp controller** runs on Kubernetes and deploys compute **endpoints**
there ([#108](https://github.com/yugabyte/yb-light/issues/108)). The k8s deployer
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
(deploy/controller, [#94](https://github.com/yugabyte/yb-light/issues/94)) — with the
`helm` CLI and the `yb-light-endpoint` chart baked into the image.

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
POST …/storage-pools/{pool}/projects/{proj}/branches/{br}/endpoints
   │  (handlers.go: CreateEndpoint)
   ├─ core.Backend.CreateEndpoint              → inserts the endpoint row (address empty)
   ├─ deploy.KubernetesDeployer.CreateEndpoint → helm upgrade --install --wait
   ├─ core.Backend.SetEndpointAddress(<LB IP / pod IP / Service DNS>:<port>)
   └─ registry.DeletePendingInit(...)  (consume the recipe)   → 201 {endpoint}
```

The deploy is **synchronous**: `helm upgrade --install --wait` blocks until the
release is ready, then the resolved client address is written back and the call
returns `201`. The row is inserted first so the single-writer rule is enforced
before a Helm install is spent; if the deploy then fails, the just-inserted row
is removed so it does not linger undeployed.

Retire is the mirror (`DELETE …/endpoints/{id}`): `helm uninstall
--ignore-not-found --wait` then drop the row → `204`. This is the design's
kill-before-create retire (ac-highlvl.md §4); `--wait` blocks until the
resources are gone.

## Materializing a branch: the recipe + bootstrap

Project/branch creation and endpoint creation are **separate** steps, joined by a
**recipe** (full detail in [controller-project-create.md](controller-project-create.md)):

- **create-project** (and **create-branch**) records the branch in the
  control-plane DB (`branch`) **and** a `pending_init` *recipe* in the storage
  controller, keyed by the flat `branch_id`. It does **not** deploy anything or
  eagerly materialize the branch's storage.
- **create-endpoint** deploys the endpoint with `BOOTSTRAP=true`. On first
  materialization the pod's supervisor runs `initdb` for the recipe's
  `branch_id`, registers +
  attaches to the controller (below), and registers the branch; the
  controller then **consumes** the recipe (`DeletePendingTimelineInit`). There is
  no separate create-database call — the first endpoint is what turns the declared
  branch into real storage.

The storage layer identifies a branch by 32 lowercase hex chars; the
control plane stores the same 16 bytes (the `branch_id`) as a SQL `uuid` and
converts with `hex.EncodeToString(id[:])` when rendering chart values. (The endpoint
image receives it as the flat `BRANCH_ID` env var, #168.)

## Controller coordination + addressing

With `controller.url` set (the controller path), the endpoint pod registers its
co-located pageserver with the controller, takes its pageserver
**generation from the controller's attach-hook** (not a pinned constant), and
registers the branch. (The safekeeper is endpoint-owned and not registered with
the controller.)

> **Planned direction:** these calls move *out of the pod and into amp's deploy
> code* — amp does node-register + attach-hook and injects the generation, so the
> pod stops self-registering. This evolves toward a **create-then-configure /
> pre-warm** model: pods are created unassigned and bound to a branch on demand via
> a push `configure` (amp mints `branch_id` + `generation`; the pod returns
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
  ([#121](https://github.com/yugabyte/yb-light/issues/121)).
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
    so with SC coordination on (the default) the pageserver/safekeeper/endpoint_storage
    HTTP ports are exposed on node ports too — demo-grade, like the rest of this path;
  - `ClusterIP` → the **pod IP** `<podIP>:<computePort>` (connect in-cluster).

  All fall back to the stable **Service DNS** when the lookup isn't possible. The
  LB/node/pod IP is **not** stable across pod (or node) restarts; the Service DNS is.

The deployed endpoints reach their **backing YugabyteDB** by its in-cluster
Service DNS (`--deploy-yb-host-ports`), which may differ from the controller's own
`--database-url` path to its state. The flat `branch_id` is the shared key.

## Endpoint data volume (`--deploy-storage-class`)

Each endpoint's `/data` (the compute's `pgdata`, the LFC cache, logs and
pidfiles) is provisioned from a storage class when `--deploy-storage-class` is
set: the chart renders a `ReadWriteOnce` **PersistentVolumeClaim** of that class
(a network block volume — **EBS** on AWS, **Persistent Disk** on GCP) of size
`--deploy-disk-size` (default `10Gi`) and mounts it at `/data`. Because the
ReadWriteOnce volume attaches to one pod at a time, the Deployment switches to the
**`Recreate`** strategy (stop the old pod before starting the new one). The pod's
**`podSecurityContext.fsGroup`** (default `1000`, the endpoint image's `yugabyte`
gid) makes the otherwise root-owned provisioned volume writable by the non-root
process — a single source for the fsGroup, applied whether or not a PVC is used (an
`emptyDir` is world-writable and unaffected). The PVC is part of the Helm release,
so `helm uninstall` on retire frees the volume. With the flag **empty**, `/data`
falls back to the current ephemeral **`emptyDir`** (no persistent volume). All durable state still lives in YugabyteDB; the volume is
local scratch + cache, so a pod (or PVC) reset is safe.

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

Wired in `cmd/controller/main.go` (`buildProvisioning`): with `--deploy`, a
`KubernetesDeployer` or `DockerDeployer` is built per `--deploy-driver` and passed
to the API as `api.Provisioning.Deployer`. With `--deploy` off, the endpoint
create/retire routes refuse with `501` (read routes stay).

## Idempotency

Deploy/retire converge on retry rather than duplicating or wedging:

- `helm upgrade --install` converges; a prior attempt killed mid-flight can leave
  the release `pending-*`, which would make the next install refuse — so the
  `internal/helm` wrapper first clears a stuck pending release (`helm status` →
  `uninstall`) before installing.
- `SetEndpointAddress` is an absolute `UPDATE … WHERE id = $1`.
- `helm uninstall --ignore-not-found` tolerates an already-gone release (helm ≥ 3.13).
- `DeleteEndpoint` is `DELETE … WHERE id = $1` (absent row = no-op).
- `RecordPendingTimelineInit` / `DeletePendingTimelineInit` are upsert/idempotent
  (a missing recipe on delete is tolerated).

## Configuration (§9)

The `--deploy*` block (env `AMP_DEPLOY*`), validated only when `--deploy` is set.

Common to both drivers: `--deploy`, `--deploy-driver`, `--deploy-yb-host-ports` /
`-yb-user` / `-yb-dbname` / `-yb-password` (or `--deploy-yb-existing-secret`),
`--deploy-bootstrap`, `--deploy-controller-url`, `--deploy-timeout`.

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
| `--deploy-disk-size` | requested size of the `/data` PVC when `--deploy-storage-class` is set (default `10Gi`) |

Docker driver: `--deploy-endpoint-image`, `--deploy-network`,
`--deploy-external-host`, `--deploy-supervisor-path`.

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
- **No authentication.** The controller runs `--dev` (no auth) and the
  control-plane API + UI are unauthenticated. Mitigated only by **internal
  LoadBalancers** (private VPC IPs, GKE `…/load-balancer-type: Internal`) — not by
  any authn/authz. Do not expose publicly.
- **Shared, well-known endpoint credentials.** Every endpoint's compute uses the
  same `cloud_admin` / `cloud_admin` superuser (the `COMPUTE_PASSWORD` default);
  there is no per-endpoint credential generation or project isolation at the SQL
  layer.
- **Postgres v18 only.** create-project rejects any other `pg_version`.
- **One pageserver+safekeeper per endpoint pod**, registered with a **unique SC
  node id derived from the branch id** (a 31-bit hash). Collision-resistant, not
  collision-proof. One `read_write` endpoint per branch (single-writer).
- **Bootstrap-on-first-endpoint.** Branches are materialized by the first
  endpoint's `initdb` (`BOOTSTRAP=true`), driven by the recipe — there is no
  standalone create-project/branch flow yet.
- **Synchronous, non-atomic provisioning.** create-project / create-endpoint run
  inline on the request (no executing write-ahead task framework —
  [#84](https://github.com/yugabyte/yb-light/issues/84) /
  [#113](https://github.com/yugabyte/yb-light/issues/113)); a crash mid-flow can
  orphan committed state ([#106](https://github.com/yugabyte/yb-light/issues/106)).
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
  network partition (ac-highlvl.md §4 partition gap). Generation fencing is the
  controller's (the attach-hook grants/bumps it), but live migration of a branch
  between endpoints is not built.
- **Pre-fix endpoints don't self-heal.** An endpoint registered before the stable
  Service-DNS change (#121) has a stale node address in the SC and will still
  `409` on reschedule; redeploy it (new endpoints are unaffected).
