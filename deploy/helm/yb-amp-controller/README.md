# yb-amp-controller Helm chart

Deploys the [amp controller](../../../controller/README.md) (the amp control
plane), its MCP server, **and** the
[`amp_ui`](../../../amp_ui/README.md) web UI, together, backed by an external
YugabyteDB. Replaces the raw `kubectl apply` manifests that used to live inline
in [`deploy/controller/redeploy.sh`](../../controller/redeploy.sh).

## Install

```sh
helm install yb-amp-controller deploy/helm/yb-amp-controller -n amp \
  --set controller.image.repository=454529406029.dkr.ecr.us-west-2.amazonaws.com/amp/controller \
  --set controller.image.tag=20260618-221855 \
  --set controller.endpoint.image.repository=454529406029.dkr.ecr.us-west-2.amazonaws.com/amp/endpoint \
  --set controller.endpoint.image.tag=20260618-184649 \
  --set 'imagePullSecrets[0].name=pull-secret' \
  --set controller.databaseUrl='postgres://yugabyte@yb-tserver.amp.svc.cluster.local:5433/yb_storage?sslmode=disable'
```

The chart does **not** create the namespace, the backing YugabyteDB, or the
image pull secret — those are prerequisites.

## What it renders

- **RBAC** — `ServiceAccount`, `Role`+`RoleBinding` `amp-controller` (full access
  to pods/services/configmaps/secrets/pvc/endpoints/events/serviceaccounts and
  apps deployments/replicasets/statefulsets in the release namespace), and
  `ClusterRole`+`ClusterRoleBinding` `amp-controller-namespaces` (cluster-scoped
  namespaces get/list/create + nodes get/list). The controller drives endpoint
  workloads via the Kubernetes/Helm driver, so it needs this.
- **Controller Deployment** `amp-controller` — one pod, container port `api`
  (8080: REST API + UI + `/healthz`/`/readyz` + the storage-layer control surface
  `/control/v1`+`/debug/v1`). Carries the full `AMP_*` env block; readiness
  `/readyz` and liveness `/healthz` on the api port.
- **Controller Service** `amp-controller` — always-on ClusterIP exposing `api`.
  This is the DNS endpoints register against (the control-surface URL handed to them).
- **Controller external Service** — per `controller.service.type`: a NodePort
  (`amp-controller-np`), an internal LoadBalancer (`amp-controller-lb`, carrying
  `controller.service.annotations`), a public LoadBalancer (`amp-controller-lb`),
  or none. Helm prunes the others on a mode switch.
- **MCP Deployment** `amp-controller-mcp` (when `mcp.enabled`) — one pod, the
  thin stateless wrapper of the controller REST API. Env `AMP_CONTROLLER_URL`
  points at the in-cluster controller Service on the api port,
  `AMP_MCP_TRANSPORT=http`, `AMP_MCP_HOST=0.0.0.0`, `AMP_MCP_PORT=8000`.
- **MCP Service** `amp-controller-mcp` (when `mcp.enabled`) — an always-on
  ClusterIP on 8000 (reach via `kubectl port-forward`), plus an external Service
  (`amp-controller-mcp-np` / `amp-controller-mcp-lb`) **per the same
  `controller.service.type`** as the controller — so the MCP endpoint is internal
  for dev and external for a prod release by that one knob.
- **UI Deployment** `amp-controller-ui` (when `ui.enabled`) — one pod running the
  [`amp_ui`](../../../amp_ui/README.md) React SPA served by nginx. nginx serves
  the static build and reverse-proxies `/api/` to the controller (env
  `CONTROLLER_URL`, defaulting to the in-cluster controller Service on the api
  port), so the SPA's relative `/api/v1` calls stay same-origin — no CORS, and no
  per-environment rebuild. Container port `http` (`ui.port`, default 80).
- **UI Service** `amp-controller-ui` (when `ui.enabled`) — an always-on ClusterIP
  on `ui.port`, plus an external Service (`amp-controller-ui-np` /
  `amp-controller-ui-lb`) selected by its **own `ui.service.type`**, independent of
  the controller's exposure. It defaults to `internal-lb`, so the UI gets its own
  internal LoadBalancer out of the box; this SPA pod is the intended front door,
  distinct from the controller's own built-in console at `/`.

### UI proxy & probe

The UI pod does not talk to the controller cross-origin: nginx proxies `/api/`
to `CONTROLLER_URL` (an origin with no trailing path, so the original `/api/...`
URI is forwarded verbatim) and falls back to `index.html` for client-side
routes. Its `GET /healthz` liveness/readiness probes are served by nginx itself
(a static 200), so a 200 confirms the UI is serving and does **not** depend on
the controller being reachable.

### MCP probe

The MCP Deployment uses HTTP `GET /healthz` liveness/readiness probes on port
8000. The server serves that route (FastMCP `@mcp.custom_route`) only under the
`http` transport — which this pod always runs — returning `{"status": "ok"}`, so
a 200 confirms the MCP app is actually serving, not just that the port is bound.

## Key values

| Value | Default | Meaning |
|---|---|---|
| `imagePullSecrets` | `[]` | pull secrets for both images; `[0].name` also becomes `AMP_DEPLOY_PULL_SECRET` |
| `controller.image.repository` / `.tag` | `amp/controller` / chart appVersion | controller image |
| `controller.databaseUrl` | `postgres://yugabyte@yb-tserver:5433/yb_storage?sslmode=disable` | `AMP_DATABASE_URL` |
| `controller.dbSchema` | `controller` | `AMP_DB_SCHEMA` |
| `controller.logFormat` | `text` | `AMP_LOG_FORMAT` |
| `controller.ports.api` | `8080` | controller API + storage-layer control surface port |
| `controller.service.type` | `internal-lb` | `nodeport` \| `internal-lb` \| `external-lb` \| `none`; drives the external Service for **both** the controller and the MCP server |
| `controller.service.nodePortValue` | _(empty)_ | fixed nodePort when `service.type=nodeport` (empty ⇒ auto-assign) |
| `controller.service.annotations` | GKE internal-LB annotations | list of `"k=v"` strings for the LB Service when `service.type=internal-lb` |
| `controller.endpoint.image.repository` / `.tag` | `amp/endpoint` / chart appVersion | `AMP_ENDPOINT_IMAGE_REPO` / `AMP_ENDPOINT_IMAGE_TAG` |
| `controller.endpoint.ybHostPorts` | `yb-tserver:5433` | `AMP_DEPLOY_YB_HOST_PORTS` |
| `controller.endpoint.ybUser` / `.ybDbname` | `yugabyte` / `yb_storage` | `AMP_DEPLOY_YB_USER` / `AMP_DEPLOY_YB_DBNAME` |
| `controller.endpoint.bootstrap` | `true` | `AMP_DEPLOY_BOOTSTRAP` |
| `controller.endpoint.storageClass` | _(empty)_ | `AMP_DEPLOY_STORAGE_CLASS` (empty ⇒ emptyDir) |
| `controller.endpoint.diskSize` | `10Gi` | `AMP_DEPLOY_DISK_SIZE` |
| `controller.endpoint.service.type` | `ClusterIP` | `AMP_DEPLOY_SERVICE_TYPE` (ClusterIP \| NodePort \| LoadBalancer) |
| `controller.endpoint.service.nodePortValue` | _(empty)_ | reserved — the deployer currently auto-assigns the endpoint NodePort |
| `controller.endpoint.service.annotations` | `[]` | list of `"k=v"` strings → `AMP_DEPLOY_SERVICE_ANNOTATIONS` (CSV) |
| `mcp.enabled` | `true` | deploy the MCP server (Deployment + Service) |
| `mcp.image.repository` / `.tag` | `amp/mcp` / chart appVersion | MCP image |
| `mcp.controllerUrl` | _(empty)_ | `AMP_CONTROLLER_URL`; empty ⇒ in-cluster controller Service DNS on the api port |
| `mcp.transport` | `http` | `AMP_MCP_TRANSPORT` |
| `mcp.port` | `8000` | `AMP_MCP_PORT` + MCP Service port |
| `ui.enabled` | `true` | deploy the UI (Deployment + Service) |
| `ui.image.repository` / `.tag` | `amp/controller-ui` / chart appVersion | UI image (built from `amp_ui/Dockerfile`) |
| `ui.controllerUrl` | _(empty)_ | `CONTROLLER_URL` nginx proxies `/api/` to; empty ⇒ in-cluster controller Service DNS on the api port |
| `ui.port` | `80` | `UI_PORT` nginx listens on + UI Service port |
| `ui.service.type` | `internal-lb` | how to expose the UI on its **own** Service, independent of the controller: `nodeport` \| `internal-lb` \| `external-lb` \| `none`; default gives the UI its own internal LoadBalancer (`amp-controller-ui-lb`) |
| `ui.service.annotations` | GKE internal-LB annotations | list of `"k=v"` annotations for the UI LB when `ui.service.type=internal-lb` |

See [`values.yaml`](values.yaml) for the full set.

## Unit tests

Tests use [helm-unittest](https://github.com/helm-unittest/helm-unittest)
(same approach as `yb-amp-endpoint`):

```sh
helm plugin install https://github.com/helm-unittest/helm-unittest.git   # once
helm unittest deploy/helm/yb-amp-controller
```

Suites live in [`tests/`](tests) (`*_test.yaml`), one per template group.
