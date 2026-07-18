# yb-amp controller layer

This directory packages the **controller layer** — the `controller`
(`amp-controller`) — as a single container. It implements
[issue #94](https://github.com/yugabyte/amp/issues/94).

```
deploy/
  controller/
    Dockerfile      # builds the controller image
```

## What's in the container

| Process        | Role                                                      | Backed by  |
|----------------|----------------------------------------------------------|------------|
| `controller`   | the control plane: REST API + task worker + the storage-layer control surface (generation authority, registries, `yb_storage` provisioning) | YugabyteDB |
| `docker` (CLI) | lets controller's Docker `deploy.Deployer` launch endpoint containers (amp-controller.md §5.6) | — |

`tini` runs as PID 1 to forward signals and reap orphans.

### The bundle layout

The `controller` binary is installed under `/opt/yb-amp-amp`:

```
/opt/yb-amp-amp/
  controller
```

This is the layout `make controller-bundle` produces locally
(`target/controller-bundle/`), so behavior matches between a dev bundle and the image.

## Building the image

The image **packages a prebuilt binary** — it compiles nothing. Build the bundle
first, then the image, from the **repo root**:

```sh
make controller-bundle BUILD_TYPE=release                # -> target/controller-bundle/controller
docker build -t yb-amp/controller:dev -f deploy/controller/Dockerfile .
```

`make controller-bundle` builds the `controller` Go binary (static,
`CGO_ENABLED=0`) — it needs only the Go toolchain, no Rust, no Postgres headers,
no submodule. The static binary runs on any `debian:bookworm-slim`-compatible
base. For local testing you can run the bundle straight from
`target/controller-bundle/` on your dev box — no container needed.

## Running it

`controller` refuses to start without a universe connection (`AMP_DATABASE_URL`);
point it at a YugabyteDB universe:

```sh
docker run --rm --network host \
  -e AMP_DATABASE_URL=postgres://yugabyte@127.0.0.1:5433/yb_storage \
  yb-amp/controller:dev

# controller's API:
curl -s http://127.0.0.1:8080/healthz
```

(`--network host` lets the container reach a YugabyteDB on `127.0.0.1:5433` and
exposes controller's API on the host's `8080`. The controller provisions the
`yb_storage` schemas and serves the storage-layer control surface on the same
port as the API.)

### On Kubernetes

[`redeploy.sh`](redeploy.sh) (re-)deploys the controller to a cluster: it applies
the ServiceAccount + RBAC, the Deployment (wired for the Kubernetes/Helm endpoint
driver, `AMP_DEPLOY_DRIVER=kubernetes`), the in-cluster Service (the API + the SC
port endpoints register against), and an external-facing Service for the UI/API
per `CONTROLLER_EXPOSE`. It reuses an existing namespace + backing YugabyteDB and
(re)mints the ECR image pull secret each run; override defaults via env:

```sh
NS=amp CONTROLLER_TAG=<tag> ENDPOINT_TAG=<tag> deploy/controller/redeploy.sh
#   (namespace also reads AMP_NAMESPACE, default amp)

# Images default to amp/controller + amp/endpoint at
# 454529406029.dkr.ecr.us-west-2.amazonaws.com (override via CONTROLLER_IMAGE_REPO /
# ENDPOINT_IMAGE_REPO). redeploy.sh (re)mints the `dshubin_bot`-backed `pull-secret`
# each run (its ECR token expires ~12h), resolving AWS creds via ../ecr-creds.sh
# (env keys > AWS_BOT_JSON > SSO) — see ../README.md "ECR access & the pull secret".
```

**Exposure** (defaults avoid slow per-Service cloud LBs):

- `CONTROLLER_EXPOSE` — `internal-lb` (default), `external-lb`, `nodeport`
  (`amp-controller-np`, node-ip:3xxxx, VPC-internal, no cloud LB), or `none`
  (ClusterIP only → `kubectl port-forward`). Switching modes removes the
  other mode's leftover Service.
- `ENDPOINT_SERVICE_TYPE` — `ClusterIP` (default; no per-endpoint cloud LB,
  reach via Service DNS / port-forward), `NodePort` (the controller records
  `<nodeIP>:<nodePort>` as the endpoint's connection address), or `LoadBalancer`
  (+ `ENDPOINT_SERVICE_ANNOTATIONS`). The deployer accepts no other value.
- `ENDPOINT_STORAGE_CLASS` — storage class for each endpoint's `/data` volume; a
  PVC of that class (EBS/PD), sized by `ENDPOINT_DISK_SIZE` (default `10Gi`).
  Empty (default) ⇒ an ephemeral `emptyDir`.

(`deploy/up.sh` overrides these to `nodeport` + `ClusterIP` by default.)

To stand the prerequisites up too (namespace, pull secret, and a single-node
YugabyteDB) and then deploy the controller in one shot, use
[`deploy/up.sh`](../README.md#one-shot-bring-up--upsh).

## Provisioning endpoints

With `AMP_DEPLOY=true` and `AMP_DEPLOY_DRIVER=docker` (the default), controller deploys a
compute **endpoint** through its Docker `deploy.Deployer` (amp-controller.md
§5.6) when you `POST …/timelines/{id}/endpoints`: it `docker run`s the
[endpoint image](../endpoint), waits for it to come up, and returns the connection
address. For that, the container needs the **Docker daemon socket** mounted, and
controller + the endpoints it launches share a **user-defined network**. The bundled
`docker` CLI talks to the socket. `AMP_DEPLOY_BOOTSTRAP=true` initdb's the recorded
tenant/timeline on first start (there is no create-database flow yet).

```sh
docker run -d --name controller --hostname controller --network ybamp \
  --user root -v /var/run/docker.sock:/var/run/docker.sock \
  --add-host host.docker.internal:host-gateway -p 8080:8080 \
  -e AMP_DATABASE_URL=postgres://yugabyte@host.docker.internal:5433/amp_controller \
  -e AMP_DEPLOY=true -e AMP_DEPLOY_DRIVER=docker -e AMP_DEPLOY_NETWORK=ybamp \
  -e AMP_DEPLOY_BOOTSTRAP=true \
  -e AMP_DEPLOY_YB_HOST_PORTS=host.docker.internal:5433 -e AMP_DEPLOY_YB_DBNAME=yb_storage \
  yb-amp/controller:dev
```

`--user root` is the dev-simple way to give controller access to the mounted socket; the
integration driver `test/integration/control_plane/run-controller.sh` wires all of this up. The Kubernetes
driver (`AMP_DEPLOY_DRIVER=kubernetes`) uses Helm + the cluster API instead and
needs neither the socket nor this CLI.

## Configuration

controller reads the usual `AMP_*` variables (see [`controller/README.md`](../../controller/README.md));
the knobs the image cares about:

| Variable | Default (in image) | Meaning |
|---|---|---|
| `AMP_DATABASE_URL` | _(required)_ | universe connection (YSQL) |
| `AMP_DEPLOY` | _(unset → false)_ | enable the endpoint deployer |
| `AMP_DEPLOY_DRIVER` | `docker` | deployer driver: `docker` (default) or `kubernetes` (Helm) |
| `AMP_DEPLOY_NETWORK` | _(empty)_ | docker: shared network for controller + the endpoints it launches |
| `AMP_DEPLOY_ENDPOINT_IMAGE` | `yb-amp/neon-endpoint:dev` | docker: endpoint image to launch |
| `AMP_DEPLOY_EXTERNAL_HOST` | `127.0.0.1` | docker: host a client uses to reach a published compute port |
| `AMP_DEPLOY_BOOTSTRAP` | _(unset → false)_ | docker: dev/integration — initdb the recorded tenant/timeline on deploy (no create-database flow yet) |
| `AMP_DEPLOY_YB_HOST_PORTS` | _(required if `AMP_DEPLOY`)_ | how an endpoint reaches the backing YB storage universe |
| `AMP_DEPLOY_YB_DBNAME` | `yb_storage` | the storage database the endpoints use |
| `AMP_DEPLOY_STORAGE_CLASS` | _(empty)_ | k8s: storage class for the endpoint `/data` PVC (EBS/PD); empty ⇒ ephemeral `emptyDir` |
| `AMP_DEPLOY_DISK_SIZE` | `10Gi` | k8s: size of the `/data` PVC when `AMP_DEPLOY_STORAGE_CLASS` is set |

## Helm chart

The controller is deployed via the
[`yb-amp-controller`](../helm/yb-amp-controller) Helm chart (mirroring
[`helm/yb-amp-endpoint`](../helm/yb-amp-endpoint)), which packages the
controller, the MCP server, and the web UI. [`redeploy.sh`](redeploy.sh) is now a
`helm upgrade --install yb-amp-controller …` that maps its env interface onto
chart values; Helm owns the RBAC, the Deployment, and the Service(s) (and prunes
leftover Services on an exposure-mode switch).

The **MCP server** (a FastMCP wrapper that exposes the controller's REST API as
MCP tools for AI agents — see
[`controller/architecture/ac-mcp.md`](../../controller/architecture/ac-mcp.md))
ships as its **own pod** in the same chart: a separate `amp-controller-mcp`
Deployment + Service, running the `http` transport and pointed at the controller's
in-cluster Service. It is gated by **`mcp.enabled`** (default `true`); set
`mcp.enabled=false` for a controller-only release.

The **web UI** (the [`amp_ui`](../../amp_ui/README.md) React SPA, built into an
nginx image — see [`amp_ui/Dockerfile`](../../amp_ui/Dockerfile)) likewise ships
as its **own pod**: a separate `amp-controller-ui` Deployment + Service. nginx
serves the static build and reverse-proxies `/api/` to the controller's
in-cluster Service (`CONTROLLER_URL`), so the SPA stays same-origin — no CORS.
It is exposed the same way as the controller (`CONTROLLER_EXPOSE`) and gated by
**`ui.enabled`** (default `true`); set `ui.enabled=false` to drop it (the
controller still serves its own built-in console at `/`). Its image
(`amp/controller-ui`) is built and pushed separately; `redeploy.sh` exposes
`UI_ENABLED` / `UI_IMAGE_REPO` / `UI_TAG` (the tag defaults to the controller's).
