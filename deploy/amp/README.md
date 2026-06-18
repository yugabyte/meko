# yb-light AMP layer: amp + storage controller, bundled

This directory packages the **AMP layer** — the `amp` controller
(`yblight-controller`) together with the **storage controller** it manages — as
a single container. It implements
[issue #94](https://github.com/yugabyte/yb-light/issues/94).

```
deploy/
  amp/
    Dockerfile      # builds the bundled amp + storage_controller image
```

## What's in the container

| Process              | Role                                                      | Backed by  |
|----------------------|----------------------------------------------------------|------------|
| `amp`                | the control plane (API + task worker), and the supervisor| YugabyteDB |
| `storage_controller` | storage-layer registry + generation fencing (§11.1)      | YugabyteDB |
| `docker` (CLI)       | lets amp's Docker `deploy.Deployer` launch endpoint containers (yblight-controller.md §5.6) | — |

Unlike the [endpoint image](../endpoint), there is **no separate supervisor**:
`amp` *is* the supervisor. It spawns the storage controller as a child process,
streams its logs into amp's structured log, restarts it with backoff if it
exits, and stops it cleanly (SIGTERM, then SIGKILL after a grace period) when
amp shuts down. `tini` runs as PID 1 to forward signals and reap orphans.

### The bundle layout

The two binaries are installed **side by side** under `/opt/yb-light-amp`:

```
/opt/yb-light-amp/
  amp
  storage_controller
```

amp resolves a bare `--storage-controller-bin` name (the default,
`storage_controller`) against its **own directory** first, so it finds the
bundled storage controller with no path configuration — even though the bundle
is not the only thing on `PATH`. An absolute or explicitly-relative path is
honored verbatim; a bare name with no bundled sibling falls back to a normal
`PATH` lookup. This is the same layout `make amp-bundle` produces locally
(`target/amp-bundle/`), so behavior matches between a dev bundle and the image.

## Building the image

The image **packages prebuilt binaries** — it compiles nothing and needs no
build toolchain itself. Build the bundle first, then the image, from the **repo
root**:

```sh
git submodule update --init vendor/postgres-v17   # one-time: source for the PG v17 headers
make amp-bundle BUILD_TYPE=release                # -> target/amp-bundle/{amp,storage_controller}
docker build -t yb-light/amp:dev -f deploy/amp/Dockerfile .
```

`make amp-bundle` builds the `amp` Go binary (static, `CGO_ENABLED=0`) and
compiles `storage_controller`. It installs the Postgres v17 **headers** first —
headers only, no Postgres server, no `build-tools` image — its transitive
`postgres_ffi` dependency binds against them. So building locally needs the
repo's normal toolchain, not the build-tools container:

- the Rust toolchain pinned in [`rust-toolchain.toml`](../../rust-toolchain.toml);
- a C toolchain (`gcc`/`make`/`bison`/`flex`) **plus `clang`/`libclang`** (bindgen
  generates the `postgres_ffi` bindings), and the PG `configure` dev libraries;
- the `vendor/postgres-v17` submodule initialized (the command above).

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
build-tools image, or any bookworm host) before `docker build`. The `amp` binary
is static, so it is portable regardless. For local testing you can run the
bundle straight from `target/amp-bundle/` on your dev box — no container needed.

## Running it

`amp` refuses to start without a universe connection (`AMP_DATABASE_URL`). The
image enables the managed storage controller by default
(`AMP_STORAGE_CONTROLLER=true`); point both at a YugabyteDB universe:

```sh
docker run --rm --network host \
  -e AMP_DATABASE_URL=postgres://yugabyte@127.0.0.1:5433/yblight_controller \
  -e AMP_STORAGE_CONTROLLER_DEV=true \
  yb-light/amp:dev

# amp's API:
curl -s http://127.0.0.1:8080/healthz
```

(`--network host` lets the container reach a YugabyteDB on `127.0.0.1:5433` and
exposes amp's API on the host's `8080`. `AMP_STORAGE_CONTROLLER_DEV=true` runs
the storage controller in `--dev` mode — no auth — which suits local testing
only.)

### On Kubernetes

[`redeploy.sh`](redeploy.sh) (re-)deploys the controller to a cluster: it applies
the ServiceAccount + RBAC, the Deployment (wired for the Kubernetes/Helm endpoint
driver, `AMP_DEPLOY_DRIVER=kubernetes`), an in-cluster Service (the API + the SC
port endpoints register against), and an internal LoadBalancer for the UI/API. It
reuses an existing namespace, backing YugabyteDB, and image pull secret; override
defaults via env:

```sh
NS=shubin-amp CONTROLLER_TAG=latest deploy/amp/redeploy.sh
```

## Provisioning endpoints

With `AMP_DEPLOY=true` and `AMP_DEPLOY_DRIVER=docker` (the default), amp deploys a
compute **endpoint** through its Docker `deploy.Deployer` (yblight-controller.md
§5.6) when you `POST …/timelines/{id}/endpoints`: it `docker run`s the
[endpoint image](../endpoint), waits for it to come up, and returns the connection
address. For that, the container needs the **Docker daemon socket** mounted, and
amp + the endpoints it launches share a **user-defined network**. The bundled
`docker` CLI talks to the socket. `AMP_DEPLOY_BOOTSTRAP=true` initdb's the recorded
tenant/timeline on first start (there is no create-database flow yet).

```sh
docker run -d --name amp --hostname amp --network ybamp \
  --user root -v /var/run/docker.sock:/var/run/docker.sock \
  --add-host host.docker.internal:host-gateway -p 8080:8080 \
  -e AMP_DATABASE_URL=postgres://yugabyte@host.docker.internal:5433/yblight_controller \
  -e AMP_DEPLOY=true -e AMP_DEPLOY_DRIVER=docker -e AMP_DEPLOY_NETWORK=ybamp \
  -e AMP_DEPLOY_BOOTSTRAP=true \
  -e AMP_DEPLOY_YB_HOST_PORTS=host.docker.internal:5433 -e AMP_DEPLOY_YB_DBNAME=yb_storage \
  yb-light/amp:dev
```

`--user root` is the dev-simple way to give amp access to the mounted socket; the
end-to-end driver `test/e2e/run-amp.sh` wires all of this up. The Kubernetes
driver (`AMP_DEPLOY_DRIVER=kubernetes`) uses Helm + the cluster API instead and
needs neither the socket nor this CLI.

## Configuration

amp reads the usual `AMP_*` variables (see [`amp/README.md`](../../amp/README.md));
the storage-controller knobs the image cares about:

| Variable | Default (in image) | Meaning |
|---|---|---|
| `AMP_DATABASE_URL` | _(required)_ | universe connection (YSQL) |
| `AMP_STORAGE_CONTROLLER` | `true` | run the storage controller as a managed subprocess |
| `AMP_STORAGE_CONTROLLER_LISTEN` | `127.0.0.1:50051` | host:port the storage controller serves on |
| `AMP_STORAGE_CONTROLLER_DATABASE_URL` | `AMP_DATABASE_URL` | universe connection for the storage controller |
| `AMP_STORAGE_CONTROLLER_DEV` | _(unset → false)_ | run the storage controller in `--dev` mode (no auth) |
| `AMP_STORAGE_CONTROLLER_BIN` | `storage_controller` | resolved next to the amp binary (the bundle), then `PATH` |
| `AMP_DEPLOY` | _(unset → false)_ | enable the endpoint deployer |
| `AMP_DEPLOY_DRIVER` | `docker` | deployer driver: `docker` (default) or `kubernetes` (Helm) |
| `AMP_DEPLOY_NETWORK` | _(empty)_ | docker: shared network for amp + the endpoints it launches |
| `AMP_DEPLOY_ENDPOINT_IMAGE` | `yb-light/neon-endpoint:dev` | docker: endpoint image to launch |
| `AMP_DEPLOY_EXTERNAL_HOST` | `127.0.0.1` | docker: host a client uses to reach a published compute port |
| `AMP_DEPLOY_BOOTSTRAP` | _(unset → false)_ | docker: dev/e2e — initdb the recorded tenant/timeline on deploy (no create-database flow yet) |
| `AMP_DEPLOY_YB_HOST_PORTS` | _(required if `AMP_DEPLOY`)_ | how an endpoint reaches the backing YB storage universe |
| `AMP_DEPLOY_YB_DBNAME` | `yb_storage` | the storage database the endpoints use |

A Helm chart for the AMP layer (mirroring
[`helm/yb-light-endpoint`](../helm/yb-light-endpoint)) is not part of this change.
