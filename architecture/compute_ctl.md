# compute_ctl

`compute_ctl` (the `compute_tools/` crate) is the **per-endpoint supervisor**. One
`compute_ctl` process runs alongside every compute: it acquires the endpoint's spec,
generates `postgresql.conf`, syncs the safekeeper, pulls a basebackup from the
pageserver, starts Postgres, applies the spec (roles / databases / extensions /
grants), and then supervises the running compute while exposing an HTTP control API.

It is the **single point of contact inside an endpoint** that the control plane talks
to — the control plane (today `yb_light_local`; tomorrow `amp-controller`) never
manages the Postgres / safekeeper / pageserver processes directly. See
[`amp-controller.md`](../controller/architecture/amp-controller.md) §5.2
("Per-endpoint services"), where `compute_ctl` is exactly this contact point.

> **Scope note.** [`compute.md`](compute.md) covers the *compute itself* — Postgres
> 17.8, the `neon` smgr hooks, LFC, walproposer. This doc covers the *supervisor* that
> brings that compute up and keeps it configured. `compute_ctl` is upstream Neon code;
> the YB-specific story is short and lives under "Changes in yb-light" below.

## Inherited from Neon

`compute_tools/` is upstream Neon, essentially unmodified (see "Changes in yb-light").
Everything in this section is Neon behavior that yb-light relies on as-is.

### What it supervises

A compute is Postgres plus a few optional sidecars, all under one `compute_ctl`:

- **Postgres** — the user-facing backend, started as a child process.
- **pgbouncer** — connection pooler, tuned from spec when `pgbouncer_settings` is set
  (`pgbouncer.rs`, `compute.rs`).
- **local_proxy** — JWT-validating proxy for Neon's managed-auth feature, configured
  via `/etc/local_proxy/config.json` + SIGHUP when `local_proxy_config` is set
  (`local_proxy.rs`).
- **rsyslog** — audit-log (pgaudit) and Postgres-log export relays (`rsyslog.rs`).
- **vm-monitor** (Linux, autoscaling only) — cgroup / LFC resource monitor.

`compute_ctl` does not fork the safekeeper or pageserver; those are separate processes
it merely *connects to* (their connstrings arrive in the spec — see below).

### Lifecycle & status state machine

The live status is a `ComputeStatus`
(`libs/compute_api/src/responses.rs`), surfaced over `GET /status` and used by the
control plane to drive an endpoint:

| Status | Meaning |
|---|---|
| `empty` | Process up, no spec yet — waiting on `/configure` (pooled-VM / control-plane mode) |
| `configuration_pending` | A spec has arrived; the configurator will pick it up |
| `init` | First-time startup in progress (prepare pgdata → start PG → apply spec) |
| `running` | Fully operational, Postgres accepting connections |
| `configuration` | A new spec is being applied to a running compute |
| `failed` | Startup or reconfiguration failed; `error` is populated |
| `termination_pending_fast` / `termination_pending_immediate` | `/terminate` requested; the two variants differ only in how long `/terminate` blocks before returning |
| `terminated` | Postgres has shut down |

`ComputeNode` (the long-lived handle) holds this behind a `Mutex<ComputeState>` plus a
`Condvar`; threads (the configurator, the monitor, HTTP handlers) wait on status
transitions rather than polling (`compute.rs`).

### Startup sequence

`main()` (`bin/compute_ctl.rs`) parses CLI args, builds a multi-thread tokio runtime,
initializes tracing, loads the config, constructs `ComputeNode`, and calls `run()`.
`run()` → `start_compute()` is the meat:

```
main()
 └─ ComputeNode::run()
     ├─ launch external HTTP server   (--external-http-port, default 3080)
     ├─ launch internal HTTP server   (--internal-http-port, default 3081)
     ├─ wait_spec()                   — block here if no spec yet (status: empty)
     ├─ launch LSN-lease task         — Static mode only
     └─ start_compute()               — status: init
         ├─ pre-startup (run in parallel):
         │    ├─ watch TLS certs
         │    ├─ download preload extensions (if shared_preload_libraries needs them)
         │    ├─ prepare_pgdata()      ← critical path
         │    │    ├─ create fresh pgdata/
         │    │    ├─ check_safekeepers_synced()  — TIMELINE_STATUS flush LSN (Primary)
         │    │    └─ get_basebackup()    — from pageserver at that LSN (gRPC or libpq)
         │    ├─ resize swap / set disk quota
         │    ├─ tune pgbouncer / configure local_proxy
         │    └─ configure audit + log-export rsyslog
         ├─ launch_monitor() + launch_configurator()
         ├─ start_postgres()          — exec the postgres child
         ├─ configure_as_primary()    — apply the spec (Primary only)
         ├─ status: running
         └─ background: extension-stats, LFC auto-prewarm, LFC offload loop
     ├─ wait_postgres()               — block until the child exits
     └─ cleanup_after_postgres_exit() — core-dump check, flip status to Terminated
```

The basebackup path (gRPC vs. libpq) is chosen from the pageserver connection scheme in
the spec (`compute.rs`); this is the same fork in the road described in
[`compute.md`](compute.md).

yb-light runs a single safekeeper, which reports `commit_lsn == flush_lsn` (a quorum of
one — every flush is durably committed). The startup LSN therefore comes straight from the
`TIMELINE_STATUS` query in `check_safekeepers_synced()`; there is no multi-safekeeper
consensus to drive, so compute_ctl never spawns `postgres --sync-safekeepers` at startup or
shutdown. (Upstream Neon ran a full `sync_safekeepers()` walproposer pass here; that spawned
a second postgres binding the client port and racing the next compute on shutdown — see
[#58].) `cleanup_after_postgres_exit()` no longer reports a `terminate_flush_lsn`.

[#58]: https://github.com/yugabyte/yb-light/issues/58

### The compute spec

The spec is `ComputeSpec` (`libs/compute_api/src/spec.rs`) — the complete description of
what the compute should be: mode, pageserver/safekeeper connection info, the catalog
(`cluster.roles`, `cluster.databases`), GUCs (`cluster.settings`), extensions, feature
flags, audit level, LFC/prewarm knobs, pgbouncer/local_proxy config, and the
`compute_ctl_config` (TLS + JWKS for API auth). It is acquired two ways:

- **From a file** — `--config <path>` (a `config.json`). This is how `yb_light_local`
  drives it: `control_plane/src/endpoint.rs` writes `config.json` into the endpoint dir
  and passes `--config`.
- **From the control plane** — `--control-plane-uri <base> --compute-id <id>`, which
  GETs `{base}/compute/api/v2/computes/{id}/spec` with retries (`spec.rs`). The two
  flags are mutually exclusive.

**One schema, three transports.** Both flags resolve to the same `ComputeConfig`
(`libs/compute_api/src/responses.rs`) — `{ spec: Option<ComputeSpec>, compute_ctl_config }`.
The `--config` file is deserialized into it directly; the control-plane GET returns a
`ControlPlaneConfigResponse` (the *same* fields plus an `Empty | Attached` status
discriminator) and `.into()`s to it, dropping `status`. That same struct is also the
body of `POST /configure`. So the spec data is identical whether it arrives as a file, a
pull from the spec endpoint, or a push to `/configure` — the differences are delivery and
trust (a file read with no auth vs. an authenticated, retried HTTP fetch carrying the
`Empty`/`Attached` handshake), not content. See
[amp-controller §5.5](../controller/architecture/amp-controller.md) for how the control
plane picks among the three.

At load, the raw spec is wrapped in a `ParsedSpec` (`compute.rs`) that extracts and
validates the load-bearing bits: the `branch_id`, the
`PageserverConnectionInfo`, the `safekeeper_connstrings` (required & non-empty for
Primary), the `storage_auth_token` (bearer auth to PS/SK when enabled), and the
`endpoint_storage` address + token (for LFC prewarm/offload).

**Applying the spec** (`spec_apply.rs`, `configure_as_primary()` in `compute.rs`) is an
ordered, partly per-database-parallel set of phases, each idempotent:

1. (optional) drop logical subscriptions before start;
2. drop / rename databases marked for deletion via `delta_operations`;
3. create/alter the privileged role and all cluster roles;
4. create/alter all cluster databases (owner, options, connlimit);
5. per-database work (rename/drop roles, create extensions, `neon` schema + grants,
   pgaudit / pg_session_jwt, JWKS roles for local_proxy) — parallelized up to a
   connection cap;
6. create the `public.health_check` table used by `/check_writability`.

### Compute modes

`ComputeMode` (`libs/compute_api/src/spec.rs`) selects behavior:

| Mode | Role | `compute_ctl` behavior |
|---|---|---|
| `Primary` | read-write | applies the full spec; uses `safekeeper_connstrings`; reads the startup LSN from the safekeeper's `TIMELINE_STATUS` |
| `Static(Lsn)` | read-only, pinned | skips spec application; runs the LSN-lease renewal task so the pageserver won't GC the pinned LSN |
| `Replica` | read-only, hot standby | follows the primary; can be promoted to Primary via `/promote` |

### HTTP control API

Two Axum servers (`http/server.rs`):

**External** (`--external-http-port`, default **3080**) — the control-plane surface,
behind JWT auth except for metrics:

| Route | Purpose |
|---|---|
| `GET /metrics`, `GET /autoscaling_metrics` | Prometheus scrape (unauthenticated) |
| `POST /configure` | hand `compute_ctl` a new spec → `configuration_pending` |
| `GET /status` | current `ComputeStatus` + tenant/timeline/error/timestamps |
| `POST /terminate` | graceful shutdown; returns flush LSN for a Primary |
| `POST /promote` | promote a Replica to Primary (target spec + WAL LSN) |
| `POST /check_writability` | write to `public.health_check`; tolerates DISK_FULL |
| `GET /dbs_and_roles`, `GET /database_schema?dbname=` | catalog inventory / `pg_dump` schema |
| `GET /insights`, `GET /metrics.json` | diagnostics / JSON metrics |
| `GET,POST,DELETE /lfc/prewarm`, `GET,POST /lfc/offload` | LFC prewarm/offload control |
| `GET /liveness_probe` | liveness |

**Internal** (`--internal-http-port`, default **3081**) — loopback-only, called by the
`yb_amp` extension and `local_proxy`:

| Route | Purpose |
|---|---|
| `POST /extension_server/{*filename}` | download a remote extension file |
| `POST /extensions` | install an extension (called by the `neon` ext) |
| `POST /grants` | add a grant |
| `POST /failpoints` | chaos hooks (`testing` builds only) |

Auth (`http/middleware/authorize.rs`): the external router validates a `Bearer` JWT
(EdDSA by default, RS256 supported) against the JWKS carried in
`compute_ctl_config`, checking expiry and the `compute` audience. A configured
`instance_id` (a deploy-time trusted-instance flag) bypasses JWT entirely.

### Background loops

- **monitor** (`monitor.rs`) — polls Postgres liveness/activity (~every 500 ms),
  maintains `last_active` and the current/total downtime metrics, and exits once the
  compute reaches a terminal/terminating status.
- **configurator** (`configurator.rs`) — waits on the `Condvar` for
  `configuration_pending` (from `POST /configure`), flips status to `configuration`,
  calls `reconfigure()`, then back to `running` or `failed`.

### Reconfiguration

`reconfigure()` (`compute.rs`) rewrites `postgresql.conf` and reloads it, re-applies the
spec SQL (unless `skip_pg_catalog_updates`), and refreshes pgbouncer / local_proxy /
rsyslog config.

### Feature subsystems

| Module | What it does |
|---|---|
| `compute_prewarm.rs` | LFC **prewarm** (load cache state before/at startup) and **offload** (periodically save it). State is read/written to **endpoint_storage** at `{addr}/{branch_id}/{endpoint}/lfc_state` (zstd). Auto-prewarm if `spec.autoprewarm`; offload loop if `offload_lfc_interval_seconds` set. |
| `compute_promote.rs` | Replica→Primary: wait for the standby to catch up to the primary's flush LSN, set `yb_amp.safekeepers` + `synchronous_standby_names`, `pg_promote()`, then reconfigure as Primary. |
| `sync_sk.rs` | Query each safekeeper's `TIMELINE_STATUS` for flush/commit LSN and confirm a quorum agrees, before basebackup and on shutdown. |
| `lsn_lease.rs` | Static mode: renew the pageserver LSN lease (`GET /lsn_lease`) so the pinned LSN survives GC. |
| `extension_server.rs` | Download remote extensions from the proxy gateway (`--remote-ext-base-url`), unpack into PG's `lib`/`share`. |
| `installed_extensions.rs` | Periodically inventory `pg_extension` across databases → `compute_installed_extensions` metric. |
| `migration.rs` | Ordered, tracked catalog migrations (`neon_migration.migration_id`). |
| `catalog.rs`, `checker.rs` | `dbs_and_roles` / schema dump; the `health_check` writability probe. |
| `disk_quota.rs` | Enforce a filesystem quota when `disk_quota_bytes` is set. |

### Metrics & tracing

`compute_ctl` exposes Prometheus text at `GET /metrics` (`metrics.rs`,
`compute_metrics.rs`): `compute_ctl_up`, Postgres up/down-time, control-plane request
counters, DB-migration failures, remote-extension request counts, pagestream-error
counters, `pg_cctl_attached{branch_id,…}`, LFC prewarm counters, and the
installed-extensions gauge. Tracing is OpenTelemetry: `init_tracing_and_logging()`
sets up an OTLP provider and propagates trace context across HTTP requests and via
`OTEL_*` env vars (`logger.rs`, `compute.rs`).

## Changes in yb-light

**There is no functional yb-light fork of `compute_tools`.** The only commits that have
touched the crate are fork-wide mechanical refactors, not behavior changes:

- the `neon_local` → `yb_light_local` / `cargo neon` → `cargo yb_light` rename;
- removal of the "Hadron" code;
- the removal of sharding entirely (the `ShardIdentity` / `ShardIndex` /
  `ShardCount` / … types and `TenantShardId` are gone — yb-light is single-shard);
- the collapse of `(tenant_id, timeline_id)` into a single `branch_id` (the
  `TenantId` / `TenantTimelineId` types are gone; `TimelineId` became `BranchId`);
- the strip of S3/Azure/GCS backends from `libs/remote_storage`.

`compute_ctl` does not know YugabyteDB exists, by design (mirroring
[`compute.md`](compute.md)). It speaks the same getpage-RPC / libpq / replication wire
protocols to a pageserver and safekeeper that *happen* to be YB-backed underneath. The
only operationally visible effects are the ones noted in `compute.md`: slightly higher
commit and cold-read latency from the YB-backed storage tier. Remote-extension download
is unaffected by the `remote_storage` strip — it goes through the `--remote-ext-base-url`
HTTP gateway, not a cloud-storage backend.

## How it fits amp-controller

The [amp-controller design](../controller/architecture/amp-controller.md) leans on
`compute_ctl` in a few concrete places:

- **Single point of contact (§5.2).** `amp-controller` will drive endpoints purely
  through `compute_ctl`'s HTTP API — `POST /configure` to push a spec, `GET /status` to
  observe, `POST /terminate` to retire, `POST /promote` for write-endpoint cut-over.
- **The spec is the durable artifact (§5.3).** Statelessness works because everything
  needed to recreate a compute lives in the spec (`config.json`): YB connection,
  `branch_id` binding, role, version, TLS/cert refs, otel config,
  endpoint_storage address + token. `compute_ctl` holds none of this durably itself.
- **endpoint_storage (§11.5).** The LFC prewarm/offload path is the live consumer of the
  endpoint_storage address + scoped token that `amp-controller` mints into every
  spec; losing that store costs only cache warmth.
- **Control-plane signing key (§9.4).** `compute_ctl`'s external API verifies JWTs
  against the JWKS embedded in its spec — the public half of the key `amp-controller`
  signs with.
- **Single-writer invariant (§4.4 / §7).** At most one Primary `compute_ctl` per
  branch; upgrades and promotions are replace-and-cut-over via `/promote` +
  `/terminate`, never two writers at once.

## Run / configure

In the dev cluster, `yb_light_local` launches it (`control_plane/src/endpoint.rs`):

```
compute_ctl \
    --external-http-port <port> \      # control-plane API (default 3080)
    --internal-http-port <port> \      # loopback API for neon ext / local_proxy (default 3081)
    --pgdata <endpoint>/pgdata \
    --connstr 'postgresql://cloud_admin@.../postgres' \
    --config  <endpoint>/config.json \ # the spec (file mode)
    --pgbin   <pg_install>/bin/postgres \
    --compute-id <endpoint_id> \
    [--remote-ext-base-url <url>] [--dev] [--privileged-role-name <role>]
```

`yb_light_local` then polls `GET /status` until the compute reports `running` (or times
out in `init`). Other notable flags: `-p/--control-plane-uri` (real-control-plane spec
mode, conflicts with `--config`), `--installed-extensions-collection-interval`,
`--lakebase-mode`, `--pg-init-timeout`.

## Where the code lives

- `compute_tools/src/bin/compute_ctl.rs` — binary entry, CLI, `main()`.
- `compute_tools/src/compute.rs` — `ComputeNode` / `ComputeState`, startup, basebackup,
  reconfigure, shutdown.
- `compute_tools/src/spec.rs`, `spec_apply.rs` — spec acquisition and the ordered apply
  phases.
- `compute_tools/src/http/` — the two Axum servers, routes, JWT middleware.
- `compute_tools/src/{monitor,configurator}.rs` — the background loops.
- `compute_tools/src/{compute_prewarm,compute_promote,sync_sk,lsn_lease}.rs` — feature
  subsystems.
- `libs/compute_api/src/{spec,responses}.rs` — `ComputeSpec` / `ComputeMode` /
  `ComputeStatus`, shared with the control plane.
- `control_plane/src/endpoint.rs` — the `yb_light_local` wrapper that writes
  `config.json` and launches the binary.
