# yb-amp-controller Helm chart

Deploys the [amp controller](../../../controller/README.md) (the amp control
plane), its MCP server, **and** the
[`amp_ui`](../../../amp_ui/README.md) web UI, together, backed by an external
YugabyteDB. Replaces the raw `kubectl apply` manifests that used to live inline
in [`deploy/controller/redeploy.sh`](../../controller/redeploy.sh).

## Install

Fetch the subcharts first. The chart declares cert-manager, `charts/*.tgz` is
gitignored, and Helm refuses to render a chart whose declared dependency is
missing even when its condition is false:

```sh
```

```sh
helm install yb-amp-controller deploy/helm/yb-amp-controller -n amp \
  --set-string imageTag=custom-7f195e43e51f \
  --set 'imagePullSecrets[0].name=pull-secret' \
  --set-string controller.databaseUrl='postgres://yugabyte@yb-tserver.amp.svc.cluster.local:5433/yb_storage?sslmode=disable'
```

### From Quay

Every `main` commit touching this chart publishes it to
`oci://quay.io/yugabyte/charts/yb-amp-controller`
([`publish-controller-chart.sh`](../publish-controller-chart.sh)) as a prerelease of
the next patch, `<major>.<minor>.<patch+1>-<UTC timestamp>.g<sha>`, plus `<version>`
itself the first time a version appears. Helm skips prereleases unless asked, so:

```sh
helm registry login quay.io
helm install yb-amp-controller oci://quay.io/yugabyte/charts/yb-amp-controller          # newest bumped version
helm install yb-amp-controller oci://quay.io/yugabyte/charts/yb-amp-controller --devel  # newest commit
```

Bump `version` in `Chart.yaml` to cut a release a plain `helm install` resolves.

The TLS mode needs **cert-manager** to issue the controller's and the endpoints'
certificates, but this chart does not install it — `deploy/tls/trust-root.sh`
does, along with trust-manager and the `amp-ca` issuer, and `up.sh`, `up-gke.sh`
and `controller/redeploy.sh` all run it. Installing it here as a subchart would
make `helm dependency build` a precondition for everyone who renders this chart,
including callers that only paste the `helm upgrade` line above. With
`tls.enabled` set and no cert-manager on the cluster, the chart refuses to render
and says to run that script.

`imageTag` covers all four amp images and the reconcile-agent; `imageRegistry`
(default `quay.io/yugabyte`) supplies the host. Move the whole set with
`--set imageRegistry=<host>/<org>`, or point one image elsewhere by giving it a
repository that carries its own host — a dot or colon in the first segment means
"already qualified", and it is used verbatim. That is how the ECR and GCR deploy
scripts work:

```sh
  --set controller.image.repository=454529406029.dkr.ecr.us-west-2.amazonaws.com/amp/controller \
  --set-string controller.image.tag=20260618-221855
```

`--set-string` for the DSN and tags: `--set` mis-tokenizes a DSN's `?` and `=`, and
reads a bare-numeric tag as a number.

The chart does **not** create the namespace, the backing YugabyteDB, or the
image pull secret — those are prerequisites. It installs **once per namespace**, and a
cluster can hold several installs, one environment each with its own backing database;
the cluster-scoped RBAC it creates is namespace-prefixed so the copies do not collide.

## What it renders

- **RBAC** — `ServiceAccount`, `Role`+`RoleBinding` `amp-controller` (full access
  to pods/services/configmaps/secrets/pvc/endpoints/events/serviceaccounts and
  apps deployments/replicasets/statefulsets in the release namespace), and
  `ClusterRole`+`ClusterRoleBinding` `amp-controller-namespaces` (cluster-scoped
  namespaces get/list/create + nodes get/list). The controller drives endpoint
  workloads via the Kubernetes/Helm driver, so it needs this.

  With `agent.enabled`, a second `Role`+`RoleBinding` `amp-reconcile-agent` — and
  a `ClusterRole` pair when the release holds a cluster-scoped object, which a
  stock install does through `rbac.nodeRead`. **This is a real privilege
  increase and worth reading before you enable the agent**: the agent upgrades the
  release it is itself a member of (`ycp/architecture/byoc_upgrades.md`), which
  means write access to every object in it plus `escalate` on Roles and `bind` on
  the release's own Roles — what Kubernetes requires of anything rewriting RBAC it
  does not already hold — plus Jobs, because the agent runs each upgrade as a Job
  outside the release and that helm re-runs the telemetry preflight hook, and read
  on pods. So an installed agent ends up at least as privileged as the
  controller, and since `escalate` is not pinned to names it can rewrite any Role
  (and, with the cluster-scoped pair, any ClusterRole) — treat it as admin of the
  namespace, or of the cluster. The two do not share a ServiceAccount, so nothing
  else widens.
- **Controller Deployment** `amp-controller` — one pod, container port `api`
  (8080: REST API + UI + `/healthz`/`/readyz`). Carries the full `AMP_*` env
  block; readiness
  `/readyz` on the api port — deliberately **no liveness probe**: startup
  migrations run before anything answers `/healthz`, and a liveness kill
  mid-migration leaves the schema dirty (#398).
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

## Upgrading

Chart `0.260714.2` renames the otel `podMetrics` ClusterRole/ClusterRoleBinding
from `<release>-otel` to `<namespace>-<release>-otel`, so two installs in one
cluster no longer share them (#888). Helm creates the new pair and orphans the
old one; delete it after upgrading an install that predates this version, since
nothing else will:

```sh
kubectl delete clusterrole,clusterrolebinding <release>-otel --ignore-not-found
```

Chart `0.260714.3` moves the logs collector's checkpoint hostPath from
`/var/lib/otelcol/<release>-logs` to `/var/lib/otelcol/<namespace>-<release>-logs`
for the same reason (#889). The collector re-reads each container log from the
start once after the upgrade, so expect one round of duplicate lines, bounded by
kubelet log rotation.

The old dir is left on each node and holds read offsets only. It is still the
**live** checkpoint dir of every release on the cluster not yet on
`0.260714.3`, which is the collision this change fixes, so deleting it before
they are all upgraded costs them their read offsets and replays their logs.
Once every release on the cluster is upgraded:

```sh
rm -rf /var/lib/otelcol/<release>-logs   # on each node
```

## Key values

| Value | Default | Meaning |
|---|---|---|
| `imagePullSecrets` | `[]` | pull secrets for both images; `[0].name` also becomes `AMP_DEPLOY_PULL_SECRET` |
| `podSecurityContext` | `{fsGroup: 1000}` | controller pod security context; the gid is the image's, so a 0440 Secret mount (the TLS client key) is readable |
| `imageRegistry` | `quay.io/yugabyte` | prefixed onto any image repository below that names no registry host of its own |
| `imageTag` | _(empty)_ | tag for every image that sets none of its own; empty ⇒ chart appVersion, which is **not published** — a real deploy sets this |
| `rbac.nodeRead` | `true` | create a read-only cluster-scoped ClusterRole for `nodes get, list`. What lets the controller detect the cloud (for the `nlb_*` endpoint connectivity modes) and resolve NodePort node IPs. Set `false` only where the install may hold no cluster-scoped binding — then `controller.endpoint.cloud` is required for `nlb_internal` |
| `rbac.clusterScoped` | `false` | additionally create a ClusterRole for `namespaces get, list, create` (Helm `--create-namespace`). Separate from `nodeRead` because this one can change the cluster |
| `controller.image.repository` / `.tag` | `amp-controller` / `imageTag` ⇒ appVersion | controller image |
| `controller.databaseUrl` | `postgres://yugabyte@yb-tserver:5433/yb_storage?sslmode=disable` | `AMP_DATABASE_URL` |
| `controller.dbSchema` | `controller` | `AMP_DB_SCHEMA` |
| `controller.logFormat` | `text` | `AMP_LOG_FORMAT` |
| `controller.ports.api` | `8080` | controller API + storage-layer control surface port |
| `controller.service.type` | `internal-lb` | `nodeport` \| `internal-lb` \| `external-lb` \| `none`; drives the external Service for **both** the controller and the MCP server |
| `controller.service.nodePortValue` | _(empty)_ | fixed nodePort when `service.type=nodeport` (empty ⇒ auto-assign) |
| `controller.service.annotations` | GKE internal-LB annotations | list of `"k=v"` strings for the LB Service when `service.type=internal-lb` |
| `controller.endpoint.image.repository` / `.tag` | `amp-endpoint` / `imageTag` ⇒ appVersion | `AMP_ENDPOINT_IMAGE_REPO` / `AMP_ENDPOINT_IMAGE_TAG` |
| `controller.endpoint.ybHostPorts` | `yb-tserver:5433` | `AMP_DEPLOY_YB_HOST_PORTS` |
| `controller.endpoint.ybUser` / `.ybDbname` | `yugabyte` / `yb_storage` | `AMP_DEPLOY_YB_USER` / `AMP_DEPLOY_YB_DBNAME` |
| `controller.endpoint.ybTlsMode` | _(empty)_ | `AMP_DEPLOY_YB_TLS_MODE` — `require` \| `verify-ca` \| `verify-full`. Set ⇒ every endpoint gets a cert-manager `Certificate` (CN = `ybUser`, from `tls.issuerRef`) and the `tls.caBundleConfigMap` CA mounted, and connects with them. Empty ⇒ plaintext |
| `controller.endpoint.ybTlsCertDuration` / `.ybTlsCertRenewBefore` | `720h` / `240h` | `AMP_DEPLOY_YB_TLS_CERT_DURATION` / `AMP_DEPLOY_YB_TLS_CERT_RENEW_BEFORE` — endpoint certificate lifetime and renewal lead |
| `controller.endpoint.computeTlsMode` | _(empty)_ | `AMP_DEPLOY_COMPUTE_TLS_MODE` — the certificate each compute presents to its own **clients**, so `sslmode=verify-full` works (`controller/architecture/ac-compute-tls.md`). `issue` = one per endpoint from `computeTlsIssuerRef`; `existing-secret` = every compute mounts `computeTlsSecret`, one wildcard for the zone. Empty ⇒ off. Independent of `ybTlsMode`, which is the other direction |
| `controller.endpoint.computeTlsSecret` | _(empty)_ | `AMP_DEPLOY_COMPUTE_TLS_SECRET` — the wildcard Secret for `existing-secret`; `deploy/tls/acme-issuer.sh` issues it |
| `controller.endpoint.computeTlsIssuerRef.kind` / `.name` / `.group` | `ClusterIssuer` / `amp-ca` / `cert-manager.io` | issuer for `computeTlsMode=issue`. Separate from `tls.issuerRef`: the client-facing certificate is the one that may need a public CA |
| `controller.endpoint.computeTlsCertDuration` / `.computeTlsCertRenewBefore` | `2160h` / `720h` | `AMP_DEPLOY_COMPUTE_TLS_CERT_DURATION` / `..._RENEW_BEFORE` — leave the duration empty for an ACME issuer |
| `controller.endpoint.computeTlsIncludeServiceDns` | `true` | `AMP_DEPLOY_COMPUTE_TLS_INCLUDE_SERVICE_DNS` — cover the endpoint's in-cluster Service name. Turn **off** for an ACME issuer |
| `tls.enabled` | `false` | mount a cert-manager client certificate (`Certificate <controller>-db`, CN = `tls.controllerUser`) and the CA bundle into the controller at `/etc/amp/tls/{client,ca}`; `controller.databaseUrl` must then name them (`sslmode=verify-full&sslrootcert=/etc/amp/tls/ca/ca.crt&sslcert=/etc/amp/tls/client/tls.crt&sslkey=/etc/amp/tls/client/tls.key`) |
| `tls.issuerRef.kind` / `.name` / `.group` | `ClusterIssuer` / `amp-ca` / `cert-manager.io` | issuer for the controller's and (via `AMP_DEPLOY_YB_TLS_ISSUER_*`) every endpoint's certificate |
| `tls.caBundleConfigMap` | `amp-ca-bundle` | ConfigMap (key `ca.crt`) with the trusted roots, kept in the namespace by trust-manager |
| `tls.controllerUser` | `yugabyte` | CN of the controller's certificate, the YSQL role it logs in as |
| `tls.certificate.duration` / `.renewBefore` | `720h` / `240h` | controller certificate lifetime and renewal lead |
| `controller.endpoint.storageClass` | _(empty)_ | `AMP_DEPLOY_STORAGE_CLASS` (empty ⇒ emptyDir) |
| `controller.endpoint.diskSize` | `10Gi` | `AMP_DEPLOY_DISK_SIZE` (a **floor**; the size itself is 20 GiB × the endpoint's vCPU) |
| `controller.endpoint.memoryGiBPerVCPU` | `0` | `AMP_DEPLOY_MEMORY_GIB_PER_VCPU` — GiB of memory per endpoint vCPU. Fills in the memory when a create names only CPU, and is the **floor** every request is held to (a pair below it is a `400`). `0` ⇒ the built-in 8. Lower it only for a cluster whose nodes cannot back that shape |
| `controller.endpoint.diskSizeOverride` | _(empty)_ | `AMP_DEPLOY_DISK_SIZE_OVERRIDE` — flat `/data` size for every endpoint whatever its CPU, bypassing the ratio and the floor. Empty ⇒ CPU-proportional. Test clusters only |
| `controller.endpoint.cloud` | _(empty ⇒ detect)_ | `AMP_DEPLOY_CLOUD` — which cloud's load-balancer annotations the per-endpoint `nlb_*` connectivity modes render: `aws` \| `gcp` \| `azure`. Empty detects it from a node's `spec.providerID`, which `rbac.nodeRead` grants by default; set it only where that read is unavailable, or where the provider IDs are read wrongly. A value outside the enum fails the render |
| `controller.endpoint.service.type` | `ClusterIP` | `AMP_DEPLOY_SERVICE_TYPE` (ClusterIP \| NodePort \| LoadBalancer) |
| `controller.endpoint.service.nodePortValue` | _(empty)_ | reserved — the deployer currently auto-assigns the endpoint NodePort |
| `controller.endpoint.service.annotations` | `[]` | list of `"k=v"` strings → `AMP_DEPLOY_SERVICE_ANNOTATIONS` (CSV) |
| `mcp.enabled` | `true` | deploy the MCP server (Deployment + Service) |
| `mcp.image.repository` / `.tag` | `amp-mcp` / `imageTag` ⇒ appVersion | MCP image |
| `mcp.controllerUrl` | _(empty)_ | `AMP_CONTROLLER_URL`; empty ⇒ in-cluster controller Service DNS on the api port |
| `mcp.transport` | `http` | `AMP_MCP_TRANSPORT` |
| `mcp.port` | `8000` | `AMP_MCP_PORT` + MCP Service port |
| `ui.enabled` | `true` | deploy the UI (Deployment + Service) |
| `ui.image.repository` / `.tag` | `amp-controller-ui` / `imageTag` ⇒ appVersion | UI image (built from `amp_ui/Dockerfile`) |
| `ui.controllerUrl` | _(empty)_ | `CONTROLLER_URL` nginx proxies `/api/` to; empty ⇒ in-cluster controller Service DNS on the api port |
| `ui.port` | `80` | `UI_PORT` nginx listens on + UI Service port |
| `ui.service.type` | `internal-lb` | how to expose the UI on its **own** Service, independent of the controller: `nodeport` \| `internal-lb` \| `external-lb` \| `none`; default gives the UI its own internal LoadBalancer (`amp-controller-ui-lb`) |
| `ui.service.annotations` | GKE internal-LB annotations | list of `"k=v"` annotations for the UI LB when `ui.service.type=internal-lb` |
| `agent.enabled` | `false` | deploy the reconcile-agent (Deployment + ServiceAccount + RBAC) |
| `agent.image.repository` / `.tag` | `ycp-reconciler-agent` / `imageTag` ⇒ appVersion | reconcile-agent image |
| `agent.ycpUrl` | _(empty)_ | **required** when enabled — `YCP_URL`, the base the agent dials, API base path included. Take it from the install hint verbatim |
| `agent.envName` | _(empty)_ | **required** when enabled — `ENV_NAME`, the registered environment id. A label and a re-point guard; the agent resolves its environment from the API key |
| `agent.placement` | `cloud` | `ENV_PLACEMENT` (`cloud` \| `onprem`); must match what the environment was registered with |
| `agent.controllerUrl` | _(empty)_ | `AMP_CONTROLLER_URL`; empty ⇒ the in-cluster controller Service DNS **with** `/api/v1` |
| `agent.reconcileInterval` | _(empty)_ | `RECONCILE_INTERVAL`; empty ⇒ the binary's default (10s) |
| `agent.controllerChart` | _(empty)_ | `CONTROLLER_CHART`, the chart the agent helm-upgrades this release to when YCP declares a controller version. Empty ⇒ the agent's default, `oci://quay.io/yugabyte/charts/yb-amp-controller` |
| `agent.chartPullSecret` | _(empty)_ | dockerconfigjson Secret mounted as helm's registry config (`HELM_REGISTRY_CONFIG`) to pull an OCI chart. Empty ⇒ `imagePullSecrets[0]`; neither set ⇒ no registry credentials |
| `agent.apiKeySecret.name` / `.key` | `amp-agent-api-key` / `api-key` | Secret the agent reads the YBM API key from. Create it before install — the chart holds no credentials |
| `agent.extraEnv` | `[]` | extra `{name, value}` env for the agent container, rendered **before** the chart's own vars so a name the chart sets wins. For a YCP reachable only through a proxy: set `HTTPS_PROXY` here, and `NO_PROXY` covering `.svc.cluster.local`, or the agent cannot reach its own controller or the collector |
| `otel.environmentId` | _(empty)_ | stamped as an `environment_id` label on every series both metrics collectors scrape. The YCP environment id (same value as `agent.envName`); the install hint sets it |
| `otel.accountId` | _(empty)_ | stamped as an `account_id` label alongside it. The install hint omits it for a standalone YCP, which has no account |

See [`values.yaml`](values.yaml) for the full set.

## Unit tests

Tests use [helm-unittest](https://github.com/helm-unittest/helm-unittest)
(same approach as `yb-amp-endpoint`):

```sh
helm plugin install https://github.com/helm-unittest/helm-unittest.git   # once
helm unittest deploy/helm/yb-amp-controller
```

Suites live in [`tests/`](tests) (`*_test.yaml`), one per template group.
