# Single-container endpoint

A **yb-amp endpoint** is a set of co-located per-endpoint services
(pageserver, safekeeper, endpoint_storage, the compute). The
single-container endpoint packages all of them into **one container
image** so an endpoint deploys as a single Kubernetes pod (or Docker
container), backed by an external YugabyteDB.

This is the deployment artifact under [`deploy/`](../../deploy): the
image is built from
[`deploy/endpoint/Dockerfile`](../../deploy/endpoint/Dockerfile) and
driven by the
[`yb-amp-endpoint`](../../deploy/helm/yb-amp-endpoint) Helm chart. The
controller's endpoint deployers
(`controller/internal/deploy/` — Docker and Kubernetes/Helm drivers)
launch these pods on `endpoint create`; `deploy/up.sh` stands up the
surrounding stack (namespace, YB, controller) from nothing.

## What runs in the pod

A small Python supervisor
([`deploy/endpoint/supervisor.py`](../../deploy/endpoint/supervisor.py),
PID 1 under `tini`) renders each service's config from environment
variables, starts the services in order, and monitors/restarts them:

```
 tini ─ supervisor.py
          ├─ endpoint_storage      (HTTP; LFC scratch blobs → public.blobs)
          ├─ safekeeper            (WAL → safekeeper.wals)
          ├─ yb-pageserver         (page service → pageserver.* ; initdb on bootstrap)
          └─ compute_ctl ─ postgres + yb_amp extension   (the compute)
```

A crashed service is restarted with backoff (default 2 s); more than
`SUPERVISOR_MAX_RESTARTS` (5) restarts within
`SUPERVISOR_RESTART_WINDOW_SECONDS` (180) fails the whole pod so
Kubernetes replaces it.

All durable state is in YugabyteDB; the pod's `/data` volume holds only
configs, logs, keys, and the compute's ephemeral `pgdata`, so the pod
is stateless — a `Deployment`, not a `StatefulSet`. The volume is an
`emptyDir` by default; setting `persistence.storageClass` in the chart
swaps it for a PVC (useful to keep `pgdata` warm across restarts, not
for durability).

## Why the controller is not in the pod

The controller is a **shared, cluster-level control plane**: it owns
the pageserver node registry, generation/attach fencing, and the
endpoint registry in YugabyteDB's `registry` schema. One controller per
endpoint pod would mean N uncoordinated controllers racing on that
shared state, so the controller runs once, in the AMP layer (the
`yb-amp-controller` chart). The pod itself talks only to YugabyteDB —
plus, optionally, to that external controller (below).

Two consequences follow.

### Schema provisioning is external

Every yb-amp schema (`controller.*`, `registry.*`, `pageserver.*`,
`safekeeper.*`, `public.blobs`, the `blob_namespace` ENUM) is created
by the controller's migrations on controller startup
([`controller/migrations/`](../../controller/migrations/), applied by
`db.Migrate`). No service self-creates its tables — the daemons bind
hardcoded schema/table names at startup. So the `yb_storage` database +
schemas must exist before the pod starts; in production the controller
provisions them. For standalone testing the supervisor can do it itself:
`DEV_PROVISION_SCHEMA=true` makes it `CREATE DATABASE`, check whether
`pageserver.pages` already exists, and if not apply the bundled
`*_provision_yb_storage.up.sql` migration with `psql`.

### Bootstrap/attach run in the pod

The supervisor materializes the branch itself:

- **Pageserver identity + initial branch.** The pageserver's
  single-branch identity (`branch_id` / `generation`) is baked into the
  `pageserver.toml` the supervisor renders. The init mode is chosen
  from the environment:
  - `BRANCH_ID` unset → generate a fresh 32-hex id and bake
    `[timeline_init] mode = "bootstrap"` (the pageserver runs `initdb`
    and imports the initial page set on start).
  - `BRANCH_ID` set (the production case, supplied by the controller) →
    attach the existing branch; `BOOTSTRAP=true` forces `initdb` of the
    supplied id (for a controller-allocated id that has not been
    materialized yet).
  - `ANCESTOR_BRANCH_ID` + `ANCESTOR_LSN` set → `mode = "branch"`: fork
    the ancestor at that LSN instead of `initdb`. Pre-fork pages stay
    under the ancestor's branch id and are reached via lineage-aware
    reads from shared storage; nothing is copied.
- **Safekeeper timeline.** After the pageserver's branch reaches
  `Active`, the supervisor creates the timeline on the safekeeper's
  HTTP API (`POST /v1/branch`) at the pageserver's `last_record_lsn`,
  with a single-member membership configuration (generation 1). A 409
  on restart is fine.
- **Compute.** `compute_ctl` gets a generated spec (`ComputeConfig`)
  whose `yb_amp.pageserver_connstring` / `yb_amp.safekeepers` (with the
  `g#1:` membership prefix, from `safekeepers_generation: 1`) /
  `yb_amp.branch_id` (from the spec's `storage_branch_id`) point at the
  co-located services over `localhost`. It fetches a basebackup from
  the pageserver and starts Postgres. A background thread then sets the
  `cloud_admin` password over the loopback trust connection so
  non-loopback clients can authenticate (the spec sets
  `skip_pg_catalog_updates`, so the password in the spec is never
  applied by `compute_ctl` itself); the controller's Docker deployer
  waits for that log line as the readiness signal.

## GENERATION and NODE_ID

Two coordination modes, selected by whether `CONTROLLER_URL` is set:

- **Controller-free (default).** `GENERATION` comes from the
  environment, default `1`, and stays pinned. Safe because each
  endpoint pod is the sole attacher of its branch — there is no
  concurrent pageserver to fence. `NODE_ID` defaults to `1` when no
  branch id is supplied.
- **Controller-coordinated (`CONTROLLER_URL` set).** Before writing the
  pageserver config, the supervisor registers the pod's pageserver as a
  node (`POST /control/v1/node`) and calls the attach-hook
  (`POST /debug/v1/attach-hook`) — the controller is the generation
  authority, and the returned generation is what gets baked into
  `pageserver.toml`. After the safekeeper timeline exists, the branch
  is also registered with the controller (best-effort bookkeeping:
  start LSN + name; a failure logs a warning rather than crashing the
  pod). The controller heartbeats the nodes it knows, so it must be
  able to reach the pod: `CONTROLLER_REACHABLE_HOST` plus the
  `PAGESERVER_*_PUB_PORT` values are the address it uses, and the
  pageserver/safekeeper bind `0.0.0.0` in this mode so published ports
  route in. The controller does not track safekeepers; it reaches the
  pod's SK for fork-point reads at the fixed HTTP port.

`NODE_ID` (when not set explicitly) is derived from the branch id: a
31-bit hash of its first 8 hex digits. Rationale: every endpoint pod
runs its own pageserver + safekeeper, and when many pods register with
one shared controller a fixed id collides — the controller rejects
re-registering node 1 at a new address with 409, so every endpoint
after the first would crash-loop. Deriving the id from the branch id
(one endpoint per branch) makes it stable across pod restarts and
distinct per branch with overwhelming probability. An explicit
`NODE_ID` env (e.g. from an allocator) overrides.

One more config knot: the pageserver requires `control_plane_api` to be
present in its config, but does not call back to it at runtime — so in
the controller-free pod it is set to a placeholder
(`http://localhost:6666`), and under controller coordination it points
at `CONTROLLER_URL`.

## Image notes

yb-amp is currently Postgres v18 only (`POSTGRES_VERSIONS = v18`,
[`compute/vendor/postgres-v18`](../vendor/)); the multi-stage
[`deploy/endpoint/Dockerfile`](../../deploy/endpoint/Dockerfile) builds
patched Postgres, the Rust daemons, and the `yb_amp` extension, and
ships the migrations for `DEV_PROVISION_SCHEMA`. The Postgres-family
binaries and the compute's walredo child link against
`pg_install/v18/lib` (`libpq.so.5`), so `LD_LIBRARY_PATH` is set
image-wide (the supervisor also prepends it for everything it spawns,
keyed off `ENDPOINT_PG_VERSION`); the `yb_amp` extension additionally
needs `libcurl4` at runtime.

See [`deploy/README.md`](../../deploy/README.md) for build/run/config
details, `up.sh`, and the Helm charts.
