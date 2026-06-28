# Single-container endpoint

A **yb-light endpoint** is normally a set of co-located processes started by
`yb_light_local` (pageserver, safekeeper, endpoint_storage, the compute, and a
cluster-level storage_controller). The single-container endpoint packages the
per-endpoint subset of those into **one container image** so an endpoint can be
deployed as a single Kubernetes pod, backed by an external YugabyteDB.

This is the deployment artifact under [`deploy/`](../deploy); the image is built
from [`deploy/endpoint/Dockerfile`](../deploy/endpoint/Dockerfile) and driven by
the [`yb-light-endpoint`](../deploy/helm/yb-light-endpoint) Helm chart.

## What runs in the pod

A small Python supervisor (`deploy/endpoint/supervisor.py`, PID 1 under `tini`)
renders each service's config from environment variables, starts the services
in order, and monitors/restarts them:

```
 tini ─ supervisor.py
          ├─ endpoint_storage      (HTTP; LFC scratch blobs → public.blobs)
          ├─ safekeeper            (WAL → safekeeper.wals; YB-WAL mode)
          ├─ yb-pageserver         (page service → pageserver.* ; initdb on bootstrap)
          └─ compute_ctl ─ postgres 17 + neon extension   (the compute)
```

All durable state is in YugabyteDB; the pod's local disk holds only logs,
pidfiles and the compute's ephemeral `pgdata`, so the pod is stateless (a
`Deployment` with an `emptyDir`, not a `StatefulSet`).

## Why the storage_controller is not in the pod

The `storage_controller` is a **shared, cluster-level control plane**: it owns
the pageserver node registry, generation/attach fencing, and the endpoint
registry in YugabyteDB's `storage_controller` schema. Running one per endpoint
pod would mean N uncoordinated controllers racing on that shared state
(multi-controller leadership was removed from this fork). It therefore lives in
the **AMP layer**, not in the endpoint pod — the pod talks only to YugabyteDB.

Two consequences follow.

### Schema provisioning is external

Every yb-light schema (`storage_controller.*`, `pageserver.*`, `safekeeper.*`,
`public.blobs`, the `blob_namespace` ENUM) is created by the
storage_controller's diesel migrations
(`storage_controller/migrations/.../up.sql`). No service self-creates its
tables. So the `yb_storage` database + schema must already exist before the pod
starts; in production AMP provisions it. For standalone testing the supervisor
can do it itself (`DEV_PROVISION_SCHEMA=true` — creates the database and applies
the bundled migrations with `psql`).

### Bootstrap/attach happen without the controller

`yb_light_local`'s `branch create` / `endpoint create` route through the
storage_controller (it records a pending init, registers the node, grants a
generation via the attach-hook, and registers the branch). The pod reproduces
the **SC-free subset** of that flow:

- **Pageserver attach + initial branch.** The pageserver's single-branch
  identity (`branch_id` / `generation`) is baked into
  `pageserver.toml`; `register_and_attach` (the only step that calls the
  controller) is skipped because the supervisor runs the `yb-pageserver` binary
  directly rather than via the launcher. For a fresh branch the supervisor also
  bakes `[timeline_init] mode = "bootstrap"`, which makes the pageserver run
  `initdb` and import the initial page set on start. The generation is pinned
  (default `1`) — safe because each endpoint pod is the sole attacher of its
  branch, so there is no concurrent process to fence. `control_plane_api` is set
  to a placeholder (the pageserver requires it, but this fork no longer calls
  back to it — `controller_upcall_client` was removed).
- **Safekeeper timeline.** Created directly against the safekeeper's HTTP API
  (`POST /v1/branch`) with a single-member membership configuration
  (generation 1), matching what the launcher does minus the controller's
  `timeline_register` bookkeeping.
- **Compute.** `compute_ctl` is given a generated spec (`ComputeConfig`) whose
  `yb_amp.pageserver_connstring` / `yb_amp.safekeepers` (with the `g#1:` membership
  prefix) / `yb_amp.branch_id` (from the spec's `storage_branch_id`) point at the
  co-located services over `localhost`. It fetches a basebackup from the pageserver
  and starts Postgres as usual.

A pod with `BRANCH_ID` unset bootstraps a fresh branch on first start; with
`BRANCH_ID` set (the production case, supplied by AMP) it attaches an existing
one (no `initdb`).

## Image notes

yb-light is Postgres v17 only (`POSTGRES_VERSIONS = v17`,
`vendor/postgres-v17`). The endpoint image has its own multi-stage Dockerfile
rather than reusing the repo-root `Dockerfile`, which is unmaintained upstream
cruft for this fork (it references absent `postgres-v14..v16` submodules, a
`pageserver` binary that no longer exists — it is `yb-pageserver` — and the
removed `storage_broker`). The Postgres-family binaries link
`pg_install/v17/lib` (`libpq.so.5`), so `LD_LIBRARY_PATH` is set image-wide; the
`yb_amp` extension additionally needs `libcurl4` at runtime.

See [`deploy/README.md`](../deploy/README.md) for the build/run/config details
and the Helm chart.
