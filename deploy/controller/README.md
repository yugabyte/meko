# yb-light controller layer: controller + storage controller, bundled

This directory packages the **controller layer** — the `controller`
(`amp-controller`) together with the **storage controller** it manages — as
a single container. It implements
[issue #94](https://github.com/yugabyte/yb-light/issues/94).

```
deploy/
  controller/
    Dockerfile      # builds the bundled controller + storage_controller image
```

## What's in the container

| Process              | Role                                                      | Backed by  |
|----------------------|----------------------------------------------------------|------------|
| `controller`                | the control plane (API + task worker), and the supervisor| YugabyteDB |
| `storage_controller` | storage-layer registry + generation fencing (§11.1)      | YugabyteDB |
| `docker` (CLI)       | lets controller's Docker `deploy.Deployer` launch endpoint containers (amp-controller.md §5.6) | — |

Unlike the [endpoint image](../endpoint), there is **no separate supervisor**:
`controller` *is* the supervisor. It spawns the storage controller as a child process,
streams its logs into controller's structured log, restarts it with backoff if it
exits, and stops it cleanly (SIGTERM, then SIGKILL after a grace period) when
controller shuts down. `tini` runs as PID 1 to forward signals and reap orphans.

### The bundle layout

The two binaries are installed **side by side** under `/opt/yb-light-amp`:

```
/opt/yb-light-amp/
  controller
  storage_controller
```

controller resolves a bare `--storage-controller-bin` name (the default,
`storage_controller`) against its **own directory** first, so it finds the
bundled storage controller with no path configuration — even though the bundle
is not the only thing on `PATH`. An absolute or explicitly-relative path is
honored verbatim; a bare name with no bundled sibling falls back to a normal
`PATH` lookup. This is the same layout `make controller-bundle` produces locally
(`target/controller-bundle/`), so behavior matches between a dev bundle and the image.

## Building the image

The image **packages prebuilt binaries** — it compiles nothing and needs no
build toolchain itself. Build the bundle first, then the image, from the **repo
root**:

```sh
git submodule update --init vendor/postgres-v18   # one-time: source for the PG v18 headers
make controller-bundle BUILD_TYPE=release                # -> target/controller-bundle/{controller,storage_controller}
docker build -t yb-light/controller:dev -f deploy/controller/Dockerfile .
```

`make controller-bundle` builds the `controller` Go binary (static, `CGO_ENABLED=0`) and
compiles `storage_controller`. It installs the Postgres v18 **headers** first —
headers only, no Postgres server, no `build-tools` image — its transitive
`postgres_ffi` dependency binds against them. So building locally needs the
repo's normal toolchain, not the build-tools container:

- the Rust toolchain pinned in [`rust-toolchain.toml`](../../rust-toolchain.toml);
- a C toolchain (`gcc`/`make`/`bison`/`flex`) **plus `clang`/`libclang`** (bindgen
  generates the `postgres_ffi` bindings), and the PG `configure` dev libraries;
- the `vendor/postgres-v18` submodule initialized (the command above).

Install the toolchain bits your distro is missing, e.g.:

```sh
# Debian/Ubuntu (and the build-tools base image):
sudo apt-get install -y clang libclang-dev gcc make bison flex \
  libssl-dev libreadline-dev zlib1g-dev libseccomp-dev libicu-dev
# Fedora/RHEL/AlmaLinux:
sudo dnf install -y clang clang-devel gcc make bison flex \
  openssl-devel readline-devel zlib-devel libseccomp-devel libicu-devel
```

The `storage_controller` dependency tree is large (~400 crates), so a cold
compile is slow, but it is the same workspace build that produces every other
yb-light binary. The `ghcr.io/neondatabase/build-tools` image simply bundles all
of the above pinned — convenient for CI, but not required to build locally.

One caveat for the **image**: `storage_controller` is dynamically linked against
glibc, so build the bundle on a base compatible with `debian:bookworm-slim` (the
build-tools image, or any bookworm host) before `docker build`. The `controller` binary
is static, so it is portable regardless. For local testing you can run the
bundle straight from `target/controller-bundle/` on your dev box — no container needed.

## Running it

`controller` refuses to start without a universe connection (`AMP_DATABASE_URL`). The
image enables the managed storage controller by default
(`AMP_STORAGE_CONTROLLER=true`); point both at a YugabyteDB universe:

```sh
docker run --rm --network host \
  -e AMP_DATABASE_URL=postgres://yugabyte@127.0.0.1:5433/amp_controller \
  -e AMP_STORAGE_CONTROLLER_DEV=true \
  yb-light/controller:dev

# controller's API:
curl -s http://127.0.0.1:8080/healthz
```

(`--network host` lets the container reach a YugabyteDB on `127.0.0.1:5433` and
exposes controller's API on the host's `8080`. `AMP_STORAGE_CONTROLLER_DEV=true` runs
the storage controller in `--dev` mode — no auth — which suits local testing
only.)

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
  yb-light/controller:dev
```

`--user root` is the dev-simple way to give controller access to the mounted socket; the
integration driver `test/integration/control_plane/run-controller.sh` wires all of this up. The Kubernetes
driver (`AMP_DEPLOY_DRIVER=kubernetes`) uses Helm + the cluster API instead and
needs neither the socket nor this CLI.

## Configuration

controller reads the usual `AMP_*` variables (see [`controller/README.md`](../../controller/README.md));
the storage-controller knobs the image cares about:

| Variable | Default (in image) | Meaning |
|---|---|---|
| `AMP_DATABASE_URL` | _(required)_ | universe connection (YSQL) |
| `AMP_STORAGE_CONTROLLER` | `true` | run the storage controller as a managed subprocess |
| `AMP_STORAGE_CONTROLLER_LISTEN` | `127.0.0.1:50051` | host:port the storage controller serves on |
| `AMP_STORAGE_CONTROLLER_DATABASE_URL` | `AMP_DATABASE_URL` | universe connection for the storage controller |
| `AMP_STORAGE_CONTROLLER_DEV` | _(unset → false)_ | run the storage controller in `--dev` mode (no auth) |
| `AMP_STORAGE_CONTROLLER_BIN` | `storage_controller` | resolved next to the controller binary (the bundle), then `PATH` |
| `AMP_DEPLOY` | _(unset → false)_ | enable the endpoint deployer |
| `AMP_DEPLOY_DRIVER` | `docker` | deployer driver: `docker` (default) or `kubernetes` (Helm) |
| `AMP_DEPLOY_NETWORK` | _(empty)_ | docker: shared network for controller + the endpoints it launches |
| `AMP_DEPLOY_ENDPOINT_IMAGE` | `yb-light/neon-endpoint:dev` | docker: endpoint image to launch |
| `AMP_DEPLOY_EXTERNAL_HOST` | `127.0.0.1` | docker: host a client uses to reach a published compute port |
| `AMP_DEPLOY_BOOTSTRAP` | _(unset → false)_ | docker: dev/integration — initdb the recorded tenant/timeline on deploy (no create-database flow yet) |
| `AMP_DEPLOY_YB_HOST_PORTS` | _(required if `AMP_DEPLOY`)_ | how an endpoint reaches the backing YB storage universe |
| `AMP_DEPLOY_YB_DBNAME` | `yb_storage` | the storage database the endpoints use |
| `AMP_DEPLOY_STORAGE_CLASS` | _(empty)_ | k8s: storage class for the endpoint `/data` PVC (EBS/PD); empty ⇒ ephemeral `emptyDir` |
| `AMP_DEPLOY_DISK_SIZE` | `10Gi` | k8s: size of the `/data` PVC when `AMP_DEPLOY_STORAGE_CLASS` is set |

## Helm chart

The controller is deployed via the
[`yb-amp-controller`](../helm/yb-amp-controller) Helm chart (mirroring
[`helm/yb-light-endpoint`](../helm/yb-light-endpoint)), which packages the
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
