# yb-light endpoint: single-container image + Helm chart

This directory packages a **yb-light endpoint** as a single container and a
Helm chart to run it on Kubernetes. It implements
[issue #83](https://github.com/yugabyte/yb-light/issues/83).

```
deploy/
  endpoint/
    Dockerfile      # builds the single-container endpoint image
    supervisor.py   # PID-1 (under tini) process manager for the 5 services
  amp/
    Dockerfile      # the AMP layer: amp + storage_controller bundled (see deploy/amp/README.md, #94)
  helm/
    yb-light-endpoint/   # Helm chart (Deployment + ConfigMap + Secret + Service)
```

The **AMP layer** — the `amp` controller plus the shared `storage_controller`
this endpoint deliberately omits — has its own bundled image under
[`amp/`](amp/README.md).

## What's in the container

One image runs every process that makes up an endpoint, supervised by a small
Python script (`supervisor.py`) started by `tini` as PID 1:

| Service          | Role                                   | Backed by   |
|------------------|----------------------------------------|-------------|
| `endpoint_storage` | LFC pre-warm scratch blobs           | YugabyteDB  |
| `safekeeper`       | WAL                                  | YugabyteDB  |
| `yb-pageserver`    | page service                         | YugabyteDB  |
| `compute_ctl`      | supervises Postgres v17 + `neon` ext | (compute)   |

The supervisor renders each service's config from environment variables,
brings the stack up in order (endpoint_storage → safekeeper → pageserver →
compute), creates the safekeeper timeline, then monitors the children and
restarts them with backoff; if a service exceeds its restart budget the
supervisor exits non-zero so the pod is recreated.

### Why no storage_controller

The `storage_controller` is a **shared, cluster-level** control plane (it owns
the pageserver node registry, generation/attach fencing, and the endpoint
registry in YugabyteDB's `storage_controller` schema). It is **not** part of
the endpoint pod — it lives in the AMP layer. The pod talks only to YugabyteDB.

A consequence: the `yb_storage` schema (created by the storage_controller's
diesel migrations) must already exist. In production, AMP provisions it. For
standalone testing, set `DEV_PROVISION_SCHEMA=true` and the supervisor will
create the database and apply the bundled migrations itself.

## Building the image

yb-light is Postgres v17 only. From the **repo root**:

```sh
docker build -t yb-light/neon-endpoint:dev -f deploy/endpoint/Dockerfile .
```

This compiles patched Postgres v17, the Rust services, and the `neon`
extension, so the first build is slow. The `ghcr.io/neondatabase/build-tools`
base image is required (same as the repo-root Dockerfile).

## Configuration contract

The supervisor reads these environment variables (the Helm chart sets them via
a ConfigMap + Secret):

| Variable | Default | Meaning |
|---|---|---|
| `YB_HOST_PORTS` | `127.0.0.1:5433` | CSV of YB tservers `host:port[,...]` |
| `YB_USER` | `yugabyte` | YB role |
| `YB_DBNAME` | `yb_storage` | YB database |
| `YB_SSLMODE` | _(unset)_ | `disable`/`require`/`verify-ca`/`verify-full` |
| `YB_SSLROOTCERT` | _(unset)_ | CA bundle path (for verify modes) |
| `YB_PASSWORD` | _(unset)_ | YB password (from a Secret) |
| `ENDPOINT_PG_VERSION` | `17` | compute Postgres major version |
| `COMPUTE_PORT` | `55433` | compute Postgres port |
| `COMPUTE_PASSWORD` | `cloud_admin` | password for the `cloud_admin` role |
| `SHARED_PRELOAD_LIBRARIES` | `neon` | compute `shared_preload_libraries` |
| `TENANT_ID` / `TIMELINE_ID` | _(unset)_ | serve an existing tenant; unset ⇒ bootstrap a fresh one |
| `DEV_PROVISION_SCHEMA` | `false` | create DB + apply migrations (test only) |

Internal ports (`PAGESERVER_PG_PORT`, `PAGESERVER_HTTP_PORT`,
`PAGESERVER_GRPC_PORT`, `SAFEKEEPER_PG_PORT`, `SAFEKEEPER_HTTP_PORT`,
`ENDPOINT_STORAGE_HTTP_PORT`, `COMPUTE_CTL_HTTP_PORT`) and supervisor knobs
(`SUPERVISOR_RESTART_BACKOFF_SECONDS`, `SUPERVISOR_MAX_RESTARTS`,
`SUPERVISOR_RESTART_WINDOW_SECONDS`) have sensible defaults.

## Running it locally (against a YugabyteDB on the host)

```sh
docker run --rm --name yb-endpoint \
  --network host \
  -e YB_HOST_PORTS=127.0.0.1:5433 \
  -e YB_USER=yugabyte -e YB_PASSWORD=yugabyte \
  -e DEV_PROVISION_SCHEMA=true \
  yb-light/neon-endpoint:dev

# then, from the host:
psql -h 127.0.0.1 -p 55433 -U cloud_admin -d postgres   # password: cloud_admin
```

(`--network host` lets the container reach a host YugabyteDB on `127.0.0.1:5433`
and exposes the compute on the host's `55433`.)

## Helm chart

See [`helm/yb-light-endpoint`](helm/yb-light-endpoint). It renders a
Deployment (stateless — PGDATA is ephemeral, all durable state is in YB), a
ConfigMap carrying the YB connection block + endpoint settings, an optional
Secret for the YB password, and a Service exposing the compute port.

```sh
helm install ep deploy/helm/yb-light-endpoint \
  --set yugabyte.hostPorts=yb-tserver.default.svc:5433 \
  --set yugabyte.password=$YB_PASSWORD \
  --set image.repository=yb-light/neon-endpoint --set image.tag=dev

# unit tests (requires the helm-unittest plugin):
helm unittest deploy/helm/yb-light-endpoint
```
