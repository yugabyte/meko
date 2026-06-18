# amp — Kubernetes endpoint deploy

**Status:** Current (demo-grade — see Assumptions)
**Last updated:** 2026-06-18

How the **amp controller** runs on Kubernetes and deploys compute **endpoints**
there ([#108](https://github.com/yugabyte/yb-light/issues/108)). The k8s deployer
is one concrete implementation behind the **provisioning interface** the design
mandates (yblight-controller.md principles 2–3): runtime-specific "create a pod
vs. a VM" logic lives behind `deploy.Deployer` (`CreateEndpoint`/`DeleteEndpoint`),
not in the core, so other runtimes slot in behind the same interface. The driver
is chosen by `--deploy-driver`: **`docker`** (default — one container per
endpoint, for single-host e2e) or **`kubernetes`** (this doc, the
`KubernetesDeployer`). On Kubernetes the deploy mechanism is **Helm**
(yblight-controller.md §11): the controller shells out to the `helm` CLI rather
than embedding a Helm SDK — the same "shell out to a managed binary" posture used
for the storage controller (§11.1).

## Deploying the controller

The controller is a **single bundled container** — `amp` (the control-plane API
+ task worker) plus the **storage controller** it runs as a managed subprocess
(deploy/amp, [#94](https://github.com/yugabyte/yb-light/issues/94)) — with the
`helm` CLI and the `yb-light-endpoint` chart baked into the image.

[`deploy/amp/redeploy.sh`](../../deploy/amp/redeploy.sh) is the canonical
(re-)deploy. It is idempotent (`kubectl apply`) and lays down:

- a **ServiceAccount** + namespaced **Role/RoleBinding** — the controller drives
  Helm to create/delete endpoint Deployments, Services, ConfigMaps, Secrets (and
  Helm's own release Secrets), and reads pods/services for address lookups — plus
  a small **ClusterRole** for `namespaces` (Helm's `--create-namespace` probes it);
- the **Deployment**, wired for the Kubernetes driver (`AMP_DEPLOY_DRIVER=kubernetes`)
  and internal-LoadBalancer endpoints;
- an in-cluster **Service** (`amp-controller`, ClusterIP) exposing the API (`8080`)
  and the storage-controller port (`50051`) that endpoints register against; and
- an **internal LoadBalancer** Service (`amp-controller-lb`) for the UI/API on a
  private VPC IP (the SC port stays on the ClusterIP only).

It does **not** create the namespace, the backing YugabyteDB, or the image pull
secret — those pre-exist (see Assumptions). Everything is env-overridable
(`NS`, `CONTROLLER_TAG`, `ENDPOINT_TAG`, `YB_HOST_PORTS`, `LB_ANNOTATIONS`, …).

## The shape — one endpoint = one Helm release = one pod

```
POST …/storage-pools/{pool}/timelines/{tl}/endpoints
   │  (handlers.go: CreateEndpoint)
   ├─ core.Backend.CreateEndpoint              → inserts the endpoint row (address empty)
   ├─ deploy.KubernetesDeployer.CreateEndpoint → helm upgrade --install --wait
   ├─ core.Backend.SetEndpointAddress(<LB IP / pod IP / Service DNS>:<port>)
   └─ storagecontroller.DeletePendingTimelineInit(...)  (consume the recipe)   → 201 {endpoint}
```

The deploy is **synchronous**: `helm upgrade --install --wait` blocks until the
release is ready, then the resolved client address is written back and the call
returns `201`. The row is inserted first so the single-writer rule is enforced
before a Helm install is spent; if the deploy then fails, the just-inserted row
is removed so it does not linger undeployed.

Retire is the mirror (`DELETE …/endpoints/{id}`): `helm uninstall
--ignore-not-found --wait` then drop the row → `204`. This is the design's
kill-before-create retire (yblc-highlvl.md §4); `--wait` blocks until the
resources are gone.

## Materializing a timeline: the recipe + bootstrap

Timeline creation and endpoint creation are **separate** steps, joined by a
**recipe** (full detail in [amp-timeline-create.md](amp-timeline-create.md)):

- **create-timeline** records the timeline in the control-plane DB (`pg_cluster`)
  **and** a `pending_init` *recipe* in the storage controller. It does **not**
  deploy anything or eagerly create the tenant.
- **create-endpoint** deploys the endpoint with `BOOTSTRAP=true`. On first
  materialization the pod's supervisor runs `initdb` for the recipe's
  tenant/timeline, registers + attaches to the storage controller (below), and
  registers the timeline; the controller then **consumes** the recipe
  (`DeletePendingTimelineInit`). There is no separate create-database call —
  the first endpoint is what turns the declared timeline into real storage.

The storage layer identifies a tenant/timeline by 32 lowercase hex chars; the
control plane stores the same 16 bytes as a SQL `uuid` and converts with
`hex.EncodeToString(id[:])` when rendering chart values.

## Storage-controller coordination + addressing

With `storageController.url` set (the amp path), the endpoint pod registers its
co-located pageserver + safekeeper with the controller, takes its pageserver
**generation from the controller's attach-hook** (not a pinned constant), and
registers the timeline. Two distinct network paths:

- **controller → endpoint** (heartbeats, tenant ops): the controller dials the
  endpoint's **own stable Service DNS** — `SC_REACHABLE_HOST =
  <release>.<ns>.svc.cluster.local` — and the SC-needed HTTP ports
  (pageserver/safekeeper) are published on that Service. Because the address is
  the Service, not the pod IP, a **rescheduled pod (new IP) re-registers the same
  address** and the controller's node-register stays `UpToDate` instead of
  returning `409` — the fix for the reschedule crash-loop
  ([#121](https://github.com/yugabyte/yb-light/issues/121)).
- **client → endpoint** (SQL): the address recorded on the endpoint row, resolved
  from the Kubernetes API once the release is ready, by `--deploy-service-type`:
  - `LoadBalancer` → the external **LB ingress IP** `<lbIP>:<computePort>` (the
    deployer polls until the cloud LB assigns it, ~2 min, then falls back);
  - `ClusterIP` → the **pod IP** `<podIP>:<computePort>` (connect in-cluster).

  Both fall back to the stable **Service DNS** when the lookup isn't possible. The
  LB/pod IP is **not** stable across pod restarts; the Service DNS is.

The deployed endpoints reach their **backing YugabyteDB** by its in-cluster
Service DNS (`--deploy-yb-host-ports`), which may differ from the controller's own
`--database-url` path to its state. The `(tenant_id, timeline_id)` pair is the
shared key.

## Packages

- **`internal/helm`** — a thin, one-shot wrapper over the `helm` CLI
  (`UpgradeInstall`, `Uninstall`). Per-endpoint values are marshaled to a temp
  file passed with `--values` (JSON is valid YAML, sidestepping `--set` escaping).
- **`internal/deploy`** — the `Deployer` interface and its `KubernetesDeployer` /
  `DockerDeployer` implementations + the `EndpointSpec` the handler hands them.
  The Kubernetes deployer maps an endpoint to chart values, looks up the client
  address via the in-cluster service account (`k8sAPIGet`), and sets the Service
  type/annotations.

Wired in `cmd/amp/main.go` (`buildProvisioning`): with `--deploy`, a
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
`--deploy-bootstrap`, `--deploy-storage-controller-url`, `--deploy-timeout`.

Kubernetes driver:

| Flag (env) | Purpose |
|---|---|
| `--deploy-namespace` | namespace endpoints are deployed into |
| `--helm-bin` / `--deploy-chart` / `--kubeconfig` | helm binary, chart path, KUBECONFIG (empty = in-cluster) |
| `--endpoint-image-repo` / `--endpoint-image-tag` | endpoint image (tag must be set) |
| `--deploy-pull-secret` | `imagePullSecrets` name in the namespace |
| `--deploy-compute-port` | compute Postgres port the Service publishes |
| `--deploy-service-type` | endpoint Service type: `ClusterIP` (default) or `LoadBalancer` |
| `--deploy-service-annotations` | `k=v,k=v` annotations on the endpoint Service (e.g. internal-LB) |

Docker driver: `--deploy-endpoint-image`, `--deploy-network`,
`--deploy-external-host`, `--deploy-supervisor-path`.

`--deploy` requires helm ≥ 3.13 on `PATH`.

## Assumptions (current)

The deploy is **demo-grade**. What it currently takes for granted:

- **External, pre-existing YugabyteDB.** amp does not run or create YB; the
  controller, the storage controller, and every endpoint connect to an existing
  universe (`yb_storage`, schemas `amp` / `storage_controller` / `pageserver` /
  `safekeeper`). State **persists across re-deploys** — `redeploy.sh` re-attaches
  to it (migrations are idempotent); deleting the controller does not lose data.
- **Pre-existing namespace and image pull secret.** Both must exist before
  `redeploy.sh`; it warns but does not create them. Everything lives in a
  **single namespace** (no cross-namespace deploys).
- **No authentication.** The storage controller runs `--dev` (no auth) and the
  control-plane API + UI are unauthenticated. Mitigated only by **internal
  LoadBalancers** (private VPC IPs, GKE `…/load-balancer-type: Internal`) — not by
  any authn/authz. Do not expose publicly.
- **Shared, well-known endpoint credentials.** Every endpoint's compute uses the
  same `cloud_admin` / `cloud_admin` superuser (the `COMPUTE_PASSWORD` default);
  there is no per-endpoint credential generation or tenant isolation at the SQL
  layer.
- **Postgres v17 only.** create-timeline rejects any other `pg_version`.
- **One pageserver+safekeeper per endpoint pod**, registered with a **unique SC
  node id derived from the timeline id** (a 31-bit hash). Collision-resistant, not
  collision-proof. One `read_write` endpoint per timeline (single-writer).
- **Bootstrap-on-first-endpoint.** Timelines are materialized by the first
  endpoint's `initdb` (`BOOTSTRAP=true`), driven by the recipe — there is no
  standalone create-database/branch flow yet.
- **Synchronous, non-atomic provisioning.** create-timeline / create-endpoint run
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
  yblight-controller.md principle 6); deploy/retire are one-shot reconciles.
- **No attach/detach handoff.** Retire is kill-before-create, correct absent a
  network partition (yblc-highlvl.md §4 partition gap). Generation fencing is the
  controller's (the attach-hook grants/bumps it), but live migration of a tenant
  between endpoints is not built.
- **Pre-fix endpoints don't self-heal.** An endpoint registered before the stable
  Service-DNS change (#121) has a stale node address in the SC and will still
  `409` on reschedule; redeploy it (new endpoints are unaffected).
