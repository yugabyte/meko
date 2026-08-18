# Single-container endpoint

A **yb-amp endpoint** is a set of co-located per-endpoint services
(pageserver, safekeeper, the compute). The
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

A supervisor
([`compute/endpoint_supervisor`](../endpoint_supervisor), PID 1 under
`tini`) renders each service's config from environment variables,
starts the services in order, and monitors/restarts them:

```
 tini ─ endpoint-supervisor
          ├─ safekeeper            (WAL → amp_storage.wals)
          ├─ pageserver            (page service → amp_storage.* ; initdb on bootstrap)
          └─ compute_ctl ─ postgres + yb_amp extension   (the compute)
```

A crashed service is restarted with backoff (default 2 s); more than
`SUPERVISOR_MAX_RESTARTS` (5) restarts within
`SUPERVISOR_RESTART_WINDOW_SECONDS` (180) fails the whole pod so
Kubernetes replaces it.

### The same supervisor runs local endpoints

`yb_amp_local` starts one `endpoint-supervisor` per endpoint, so the
local stack has the process tree, the startup order and the restart
behaviour of the deployed one. It sets `PRERENDERED_CONFIG=true`: the
launcher writes all three config files itself — from ports it allocated,
a Postgres install outside any image, and per-endpoint settings held in
the control plane, none of which the supervisor can see — and the
supervisor supervises without rendering. That mode also gives each child
its own log file (`endpoints/<id>/pageserver/pageserver.log` and
siblings) rather than the single interleaved stream `kubectl logs`
wants.

The launcher exits after each command, so it cannot signal a daemon
directly: the supervisor would restart it. `yb_amp_local pageserver
stop` and its siblings go over a Unix control socket in the endpoint's
run directory (`supervisor.sock`), which is what makes a stop stick —
the supervisor records the service as *not wanted* and leaves it alone
until asked to start it again. An endpoint is up or down as a unit:
`endpoint create` writes config and starts nothing, `endpoint start`
runs it.

## The write fence

The supervisor also holds the endpoint's **lease on its branch** — a
YSQL-issued token that doubles as the write deadline it passes to the
services. This is what keeps a rescheduled-but-not-yet-dead pod from
writing the same branch as its replacement; the full design is in
[`compute-fencing.md`](compute-fencing.md).

In this pod, concretely:

- Nothing starts until `amp_registry.token_acquire` grants the lease on
  `branch:<BRANCH_ID>`. A pod that cannot get it logs who holds it and
  waits — it is a pod waiting its turn, not a failed pod, so it never
  exits on that account.
- The client id is generated **per process**
  (`<hostname>-<pid>-<nonce>-<branch>`, override with
  `WRITE_TOKEN_CLIENT_ID`), never per endpoint: the lease grants
  unconditionally to its current holder, so two pods sharing a client id
  would both be granted it.
- 15 s TTL (`WRITE_TOKEN_TTL_MS`), renewed every 5 s
  (`WRITE_TOKEN_RENEW_SECONDS`). Each renewal is pushed to the
  safekeeper and pageserver over `POST /v1/write_token`.
- Losing the lease — a denial, or renewals failing until the token runs
  out — SIGTERMs **every** child, then goes back to waiting for the
  token and re-runs the whole bring-up when it wins one. A clean SIGTERM
  releases the lease, so a replacement starts immediately instead of
  waiting out the TTL.
- The safekeeper and pageserver are armed via `require_write_token = true`
  in the configs the supervisor renders, and kill themselves rather than
  write past their token's deadline — including when they have never been
  given one.

`REQUIRE_WRITE_TOKEN=false` runs the endpoint unfenced (no lease, no
pushes, `require_write_token = false`). It is the escape hatch for a
fencing bug, not an operating mode: while it is set, nothing stops a
second endpoint writing the same branch.

All durable state is in YugabyteDB; the pod's `/data` volume holds only
configs, logs, and the compute's ephemeral `pgdata`, so the pod
is stateless — a `Deployment`, not a `StatefulSet`. The volume is an
`emptyDir` by default; setting `persistence.storageClass` in the chart
swaps it for a PVC (useful to keep `pgdata` warm across restarts, not
for durability).

## Why the controller is not in the pod

The controller is a **shared, cluster-level control plane**: it owns the
branch and endpoint records in YugabyteDB's `amp_registry` schema. One
controller per endpoint pod would mean N
uncoordinated controllers racing on that shared state, so the controller
runs once, in the AMP layer (the `yb-amp-controller` chart). The pod
itself talks only to YugabyteDB — plus, optionally, to that external
controller (below).

Two consequences follow.

### Schema provisioning is external

Every yb-amp schema (`amp_registry.*`, `controller.*`, `amp_storage.*`
including `amp_storage.blobs` and the `blob_namespace` ENUM) is created
by the controller's migrations on controller startup
([`controller/migrations/`](../../controller/migrations/), applied by
`db.Migrate`). No service self-creates its tables — the daemons bind
hardcoded schema/table names at startup. So the `yb_storage` database +
schemas must exist before the pod starts, and the **controller** provisions
them — it is the only thing that does. A pod never provisions its own: serving a
branch needs `amp_registry` (which the endpoint reads its overrides from), the
`amp_storage` schema, and a seeded `branch_state` row for the pageserver to read
its init mode off, all of which the controller writes at branch-create time.

### Bootstrap/attach run in the pod

The supervisor materializes the branch itself:

- **Pageserver identity + initial branch.** The pageserver's
  single-branch identity (`branch_id`) is baked into the
  `pageserver.toml` the supervisor renders, alongside
  `require_write_token`. The init mode is chosen from the environment:
  - `BRANCH_ID` unset → generate a fresh 32-hex id and bake
    the controller-seeded `branch_state` row with a NULL ancestor (the pageserver runs `initdb`
    and imports the initial page set on start).
  - `BRANCH_ID` set (the production case, supplied by the controller) →
    serve that branch. How its timeline is created is not a supervisor
    concern: the pageserver reads it off the controller-seeded
    `branch_state` row. A NULL `ancestor_branch_id` means `initdb` +
    import; a set one means fork that ancestor at `ancestor_lsn`, where
    pre-fork pages stay under the ancestor's branch id and are reached
    via lineage-aware reads from shared storage (nothing is copied).
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
  the pageserver and starts Postgres. The supervisor then sets the
  `cloud_admin` password over the loopback trust connection, on its own
  thread, so non-loopback clients can authenticate (the spec sets
  `skip_pg_catalog_updates`, so the password in the spec is never
  applied by `compute_ctl` itself); the controller's Docker deployer
  waits for that log line as the readiness signal
  (`remote_auth::READY_MARKER`).

## Fencing is the supervisor's, not the pageserver's

The supervisor holds the lease for the whole endpoint and every service
enforces it (see "The write fence" above and
[`compute-fencing.md`](compute-fencing.md)). A replacement pod waits out
the lease rather than outranking the instance it replaces — one fence
for every process in the endpoint, the WAL path included, not per-service
state.

Setting `CONTROLLER_URL` still selects controller coordination for
everything else — but the controller tracks neither safekeepers nor
  pageserver nodes; it reaches the pod's SK for fork-point reads at the
  fixed HTTP port.

One more config knot: the pageserver requires `control_plane_api` to be
present in its config, but does not call back to it at runtime — so in
the controller-free pod it is set to a placeholder
(`http://localhost:6666`), and under controller coordination it points
at `CONTROLLER_URL`.

## Image notes

yb-amp is currently Postgres v18 only (`POSTGRES_VERSIONS = v18`,
[`compute/vendor/postgres-v18`](../vendor/)); the multi-stage
[`deploy/endpoint/Dockerfile`](../../deploy/endpoint/Dockerfile) builds
patched Postgres, the Rust daemons, and the `yb_amp` extension. The Postgres-family
binaries and the compute's walredo child link against
`pg_install/v18/lib` (`libpq.so.5`), so `LD_LIBRARY_PATH` is set
image-wide (the supervisor also prepends it for everything it spawns,
keyed off `ENDPOINT_PG_VERSION`); the `yb_amp` extension additionally
needs `libcurl4` at runtime.

See [`deploy/README.md`](../../deploy/README.md) for build/run/config
details, `up.sh`, and the Helm charts.
