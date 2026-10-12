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
          ├─ lfc_state             (LFC prewarm state; see below)
          └─ compute_ctl ─ postgres + yb_amp extension   (the compute)
```

A crashed service is restarted with backoff (default 2 s); more than
`SUPERVISOR_MAX_RESTARTS` (5) restarts within
`SUPERVISOR_RESTART_WINDOW_SECONDS` (180) fails the whole pod so
Kubernetes replaces it.

### Storage and compute run as different users

With `STORAGE_UID`/`STORAGE_GID` and `COMPUTE_UID`/`COMPUTE_GID` set, the
supervisor (which must then run as root) starts the safekeeper and pageserver as
the storage user and `compute_ctl` as the compute user. The point is that a
compromised Postgres cannot read the YugabyteDB client key or password, which
only the storage user holds. All four or none: a partial set, a child id of 0,
a shared uid or gid, or a non-root supervisor refuses to boot rather than run
with less isolation than asked for.

- The compute child gets no supplementary groups (checked before `exec`, since
  std ignores an `EPERM` from `setgroups`) and no-new-privileges.
- The compute does not inherit `YB_*`, the variable `YB_PASSWORD_ENV` names, or
  `COMPUTE_PASSWORD`.
- The run directory is `root:root` `0755`. `pageserver/` and `safekeeper/` are
  `root:<storage gid>` and `compute/` is `root:<compute gid>`, all `0770`: the
  children enter through their primary group, and the supervisor, which has no
  `DAC_OVERRIDE`, as owner. pgdata moves
  to `compute/pgdata`, because `compute_ctl` deletes and recreates pgdata and so
  needs write access to its parent.
- The settings probe (`postgres -C`) runs as the compute user, because Postgres
  refuses to run as root. Its config is in `settings-probe/` under the run
  directory, owned by root and only readable by the compute, because the
  supervisor writes it as root. The supervisor refuses to probe if any
  directory on that path is a symlink or writable by another user.
- The compute's server certificate and key are mounted readable only by the
  storage group, so the supervisor copies them to `compute/server.crt` and
  `compute/server.key` (`0600`, compute user) at boot and again on each
  rotation, before it reloads Postgres, and the spec points at the copies. If
  the copy at boot fails, the watcher tries it again on each poll. Each
  copy goes to a fresh `O_EXCL` file renamed into place, so a symlink the compute
  plants is replaced rather than written through.

Unset, every child inherits the supervisor's identity, as before.

LFC prewarm state is the one thing the compute would read and write in
YugabyteDB itself. Whenever the supervisor renders the compute spec, the spec
carries `lfc_state_url` instead of `yb_storage_conn`, and a supervisor child, `lfc_state`
(`endpoint-supervisor serve-lfc-state`), serves `GET`/`PUT
/v1/lfc_state/{endpoint_id}` on `127.0.0.1:$LFC_STATE_PORT` (default 9896),
storing the blob with the storage credentials. `yb_amp_local` allocates each
endpoint its own port; an endpoint it created before that has none, and its
compute keeps `yb_storage_conn`. With the users split it runs as the storage user
rather than inside the supervisor, so the compute's requests are parsed by a
process with no more privilege than the pageserver, not by root. It is not a
pageserver route because the pageserver's HTTP port is reachable from off the
pod and its writes need the control secret. Like the other children it is
restarted on exit. The branch is the supervisor's,
never the request's. `GET` may name another endpoint on the branch, which
`POST /lfc/prewarm?from_endpoint=` needs; `PUT` may name only this endpoint
(`ENDPOINT_ID`), otherwise 403. Uploads are capped at 256 MiB, as in
`compute_ctl`.

The compute shares the daemons' loopback, so it could otherwise push a write
token, create a branch or set a failpoint. In every mode the daemons' tomls carry
`http_control_secret`, the supervisor sends it on its own requests, and the
daemons refuse any request other than `GET`/`HEAD`/`OPTIONS` without it. The
supervisor generates a random one at each boot when it renders the tomls; with
`PRERENDERED_CONFIG` the caller writes one into both (`yb_amp_local` does) and
the supervisor reads it back. A caller's config without one runs unchecked with
a warning, which the split refuses. The controller only reads from the daemons,
so it needs no secret. The key is protected from registry overrides. Tools that
change daemon state from inside the pod read it from `pageserver.toml`.

The chart sets all of this with `userSplit.enabled` (off by default), which also
runs the container as root with only `SETUID`, `SETGID`, `CHOWN` and `KILL`.

### The same supervisor runs local endpoints

`yb_amp_local` starts one `endpoint-supervisor` per endpoint, so the
local stack has the process tree, the startup order and the restart
behaviour of the deployed one.

`PRERENDERED_CONFIG` says which config files the caller brought:

| value | the caller wrote | the supervisor renders |
|---|---|---|
| unset | nothing | all three. The pod. |
| `daemons` | `pageserver.toml`, `safekeeper.toml` | the compute spec. `yb_amp_local`. |
| `true` | all three | nothing. |

`yb_amp_local` sets `daemons`. The two tomls describe per-endpoint data
directories and ports it found by searching for free ones, which this
process cannot see; the compute spec is the supervisor's, so a local
compute gets the same invariants, the same resource-derived GUCs and the
same `amp_registry` settings a deployed one does. The launcher rendering
its own copy is how k8s endpoints came to run `full_page_writes = on`
([#1050](https://github.com/yugabyte/meko/issues/1050)). What is
genuinely local — loopback binding, the safekeeper's compute port, sizes
a machine without a cgroup cannot derive — travels in
`COMPUTE_EXTRA_SETTINGS_JSON`, the same layer the chart uses for the
extension paths only it can compute.

Any value other than `daemons` also gives each child its own log file
(`endpoints/<id>/pageserver/pageserver.log` and siblings) rather than the
single interleaved stream `kubectl logs` wants.

The launcher exits after each command, so it cannot signal a daemon
directly: the supervisor would restart it. `yb_amp_local pageserver
stop` and its siblings go over a Unix control socket in the endpoint's
run directory (`supervisor.sock`), which is what makes a stop stick —
the supervisor records the service as *not wanted* and leaves it alone
until asked to start it again. An endpoint is up or down as a unit:
`endpoint create` writes config and starts nothing, `endpoint start`
runs it.

## The settings API

Every setting an operator can change — the branch's `pg_settings` and
`write_token_settings`, the endpoint's `pg_settings`, `pageserver_settings` and
`safekeeper_settings` — is read once at startup and rendered into the files the
children start from. Editing a row therefore does nothing to a running endpoint
until it restarts.

`POST /v1/settings/reload` is how the controller closes that gap. The supervisor
re-reads the registry, re-renders the configs, and replies with what it could
**not** apply in place:

```
$ curl -sX POST http://$ENDPOINT:9899/v1/settings/reload
{"restarted":["pageserver"]}
```

The lease timings are applied immediately — they are this process's own. The
daemons read their config at start, so the supervisor restarts the ones whose
config actually changed and reports them.

**Only the ones that changed.** Each rendered file is compared with what is on
disk before it is written, so a reload that produces identical bytes restarts
nothing — which is what makes this safe to call after any edit rather than only
after one known to matter. Most of a config comes from the environment and the
resources, not from the registry row someone just edited, so most reloads are
no-ops.

**Postgres settings usually reload rather than restart.** Postgres itself says
which is which: `pg_settings.context` is `postmaster` for the settings fixed at
server start and `sighup`/`superuser`/`user` for the rest. The supervisor asks
the running server about the settings that actually changed, so the rule is not
a hand-kept list that drifts from the binary.

When none of them are `postmaster`, the new spec goes to `compute_ctl`'s in-pod
`/configure`, which rewrites `postgresql.conf` and calls `pg_reload_conf()` —
connections stay up. When one is, the compute restarts and the reply names the
setting that forced it, so the cost is explained rather than arbitrary.

Some settings never reach the config file at all. Postgres refuses a name it
does not recognise or a value it cannot parse, and it refuses them by refusing
the whole file: a reload of it applies nothing, and the next start fails
outright, so a typo stored today is an endpoint that will not come back from its
next restart. It has no option to ignore one. So the settings are put to the
binary before they are written — `postgres -C` parses a throwaway config without
needing a server, which is the only way to ask at create, when Postgres has never
run — and one it will not take is left out and named in the reply instead.

`internal` settings like `block_size` are left out on the same reasoning,
though they are inert rather than fatal: compiled into the binary, so no reload
or restart puts them in force. A name with a dot is always written, because
Postgres accepts it as a placeholder whether or not anything defines it — which
is what a setting belonging to an unloaded extension needs.

Either way the endpoint is not interrupted for them, and the reply naming them
is the only way the operator finds out the edit did nothing.

The pageserver and safekeeper restart on any change to their toml. They read it
once at start and have no reload path.

Two things follow from re-rendering at runtime rather than only at boot. Configs
are written beside and renamed over, because a re-render races the monitor loop
restarting a daemon and a half-written config fails with a parse error that
explains nothing. And reloads are serialised, because two interleaving would each
write the other's files and both report a "changed" that reflects the race rather
than the registry.

`GET /metrics` on the same listener serves what the supervisor's certificate
watcher sees — the two expiries `compute_cert_not_after_seconds` and
`compute_ca_not_after_seconds`, and `compute_cert_reload_pending`, which is the
one to alert on: the expiries read the mounted file and report health whether or
not the compute ever loaded it
([`ac-metrics.md`](../../controller/architecture/ac-metrics.md#certificate-expiry--from-the-pod-not-from-cert-manager)).
It shares this server because it is the only one the supervisor has; the
collector scrapes endpoint pods by explicit port, so nothing had to move.

`POST /v1/settings/check` is the other half, and it runs *before* a write. The
controller asks the branch's writer whether Postgres would take the names and
values, and refuses the request outright if not, rather than storing a setting
that can only ever be reported as not in force. Only the pod can answer: the
version judging them is the one in its image, and an extension's setting does not
exist until that extension is loaded. It applies nothing, so it is safe on an
endpoint running prerendered config and concurrently with a reload.

Served only where something calls it: the chart emits `SUPERVISOR_HTTP_PORT`
only when `controller.url` is set, and with no port the supervisor serves
nothing. It is unauthenticated, like the daemons' read routes the same Service
already publishes — inherited posture rather than a new decision. (Their
state-changing routes need the control secret; see above.)
`reload` takes no body, so the most a caller can do there is make the supervisor
re-read state it already trusts. `check` does take one, and does work
proportional to it — a `postgres` per setting when a set is refused — so it is
bounded: 64 KiB of body and 512 settings, both far above any real map, because
an unauthenticated route that spawns processes should not be the cheapest way to
load the pod.

An endpoint that brought **every** config file (`PRERENDERED_CONFIG=true`)
refuses the call: all of it belongs to the caller that wrote it, built from an
environment this process cannot see, so re-rendering would replace a working
config with one assembled from defaults.

On `daemons` the call is served, because the compute spec is the supervisor's to
re-render — a `pg_settings` edit applies to a local endpoint the same way it does
to a deployed one. The two daemon tomls are still the caller's, so a
`pageserver_settings` or `safekeeper_settings` edit is named back in
`not_applied` rather than silently accepted.

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
- Losing the lease — a denial, renewals failing until the token runs
  out, or a renewal granted only after the token had already lapsed —
  SIGTERMs **every** child, then goes back to waiting for the
  token and re-runs the whole bring-up when it wins one. A clean SIGTERM
  releases the lease, so a replacement starts immediately instead of
  waiting out the TTL.
- The safekeeper and pageserver are armed via `require_write_token = true`
  in the configs the supervisor renders, and kill themselves rather than
  write past their token's deadline — including when they have never been
  given one. Their page and WAL writes also carry the deadline to
  YugabyteDB, whose leader refuses them past it, so a service that stalls
  after checking cannot land them late either.

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
  `require_write_token`. `BRANCH_ID` is required — the supervisor refuses to
  start without it, so there is no generate-my-own mode; the branch always comes
  from the controller. How its timeline is created is not a supervisor concern:
  the pageserver reads it off the controller-seeded `branch_state` row. A NULL
  `ancestor_branch_id` means `initdb` + import; a set one means fork that
  ancestor at `ancestor_lsn`, where pre-fork pages stay under the ancestor's
  branch id and are reached via lineage-aware reads from shared storage
  (nothing is copied).
- **Safekeeper timeline.** After the pageserver's branch reaches
  `Active`, the supervisor creates the timeline on the safekeeper's
  HTTP API (`POST /v1/branch`) at the pageserver's `last_record_lsn`,
  with a single-member membership configuration (generation 1). A 409
  on restart is fine. The safekeeper blocks this call while it loads, so the
  supervisor gives it `SLOW_CONTROL_TIMEOUT` rather than the shared client's probe
  budget (#831) — and re-asks while the answer is 503 (`StillLoading`, or a
  concurrent creator), inside that same budget, stopping early on shutdown or a
  lost lease. Treating that 503 as fatal is what cost a branch its endpoint in
  #874: the teardown restarted the load from nothing, so every attempt hit the
  same wall.
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

  The password is `COMPUTE_PASSWORD`, and from the control plane it
  arrives already hashed — a `SCRAM-SHA-256$...` verifier, the form
  `pg_authid` stores under the spec's
  `password_encryption = scram-sha-256`. Postgres keeps a value of that
  shape verbatim rather than hashing it again, which is how a
  caller-supplied password reaches the role without the control plane
  ever holding something that can log in.

  The supervisor treats it as **bootstrap state, not desired state**,
  because this runs on every boot while the catalog it writes survives
  the pod. It applies the verifier only when `COMPUTE_PASSWORD` is set
  *and* `cloud_admin` has no password yet. An unset variable — every
  redeploy, every fleet roll, every `read_only` endpoint — leaves the
  role alone rather than falling back to a default, and a role that
  already has a password keeps it, so a password its owner set with
  `ALTER ROLE` survives a restart. The gap in the second rule is a
  forked branch, whose first endpoint inherits the parent's password and
  declines its own (yugabyte/meko#1461).

  All three outcomes log `remote_auth::READY_MARKER`, including the two
  that change nothing: the Docker deployer waits on that line, and
  "settled" is what it needs to know, not "wrote to `pg_authid`".

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
`/usr/local/v18/lib` (`libpq.so.5`), so `LD_LIBRARY_PATH` is set
image-wide (the supervisor also prepends it for everything it spawns,
keyed off `ENDPOINT_PG_VERSION`); the `yb_amp` extension additionally
needs `libcurl4` at runtime.

See [`deploy/README.md`](../../deploy/README.md) for build/run/config
details, `up.sh`, and the Helm charts.
