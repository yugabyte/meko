# compute_ctl

`compute_ctl` (the `compute/compute_tools/` crate; the binary is built from
`src/bin/compute-ctl.rs` and ships as `compute-ctl`) is the **per-endpoint
supervisor**. One `compute_ctl` process runs alongside every compute: it acquires the
endpoint's spec, generates `postgresql.conf`, reads the durable LSN from the safekeeper,
pulls a basebackup from the pageserver, starts Postgres, applies the spec (roles /
databases / extensions / grants), and then supervises the running compute while exposing
an HTTP control API.

It is the **single point of contact inside an endpoint** that the control plane talks
to — the control plane (today `yb_amp_local`; tomorrow `amp-controller`) never manages
the Postgres process directly. See
[`amp-controller.md`](../../controller/architecture/amp-controller.md) §5.2
("Per-endpoint services"), where `compute_ctl` is exactly this contact point.

> **Scope note.** [`compute.md`](compute.md) covers the *compute itself* — Postgres,
> the `yb_amp` extension's smgr hooks, LFC, walproposer. This doc covers the
> *supervisor* that brings that compute up and keeps it configured. The HTTP API and
> metrics surfaces have their own reference docs:
> [`compute_ctl-apis.md`](compute_ctl-apis.md),
> [`compute_ctl-metrics.md`](compute_ctl-metrics.md).

## What it supervises

A compute is Postgres plus a few optional sidecars, all under one `compute_ctl`:

- **Postgres** — the user-facing backend, started as a child process in its own
  process group (so shutdown can `SIGKILL` the whole group and reap backends that
  outlive the postmaster while holding the client port — `wait_postgres()` in
  `compute.rs`), and with `PR_SET_PDEATHSIG` set to SIGQUIT, so the kernel takes
  the postmaster down even when `compute_ctl` dies without running any shutdown
  path at all (see [Shutdown and the orphaned postmaster](#shutdown-and-the-orphaned-postmaster)).
- **pgbouncer** — connection pooler, tuned from the spec when `pgbouncer_settings`
  is set (`pgbouncer.rs`).
- **local_proxy** — JWT-validating proxy for the managed-auth feature, configured
  via `/etc/local_proxy/config.json` + SIGHUP when `local_proxy_config` is set
  (`local_proxy.rs`).
- **rsyslog** — pgaudit audit-log export (when `audit_log_level` is
  `hipaa`/`extended`/`full`) and Postgres-log export (when `logs_export_host` is
  set) relays (`rsyslog.rs`).
- **vm-monitor** (Linux only, and only when the `AUTOSCALING` env var is set) —
  cgroup / LFC resource monitor for autoscaling VMs.

`compute_ctl` does not spawn the safekeeper or pageserver; those are separate services
it *connects to* (their connection info arrives in the spec — see below). Durability
lives entirely on that storage side: the pageserver and safekeeper persist into
YugabyteDB, so the compute (and `compute_ctl` itself) is disposable — every start
wipes `pgdata` and rebuilds it from a fresh basebackup.

## Lifecycle & status state machine

The live status is a `ComputeStatus` (`compute/libs/compute_api/src/responses.rs`),
surfaced over `GET /status` and used by the control plane to drive an endpoint:

| Status | Meaning |
|---|---|
| `empty` | Process up, no spec yet — waiting on `POST /configure` (pooled-VM / control-plane mode) |
| `configuration_pending` | A spec has arrived; the configurator will pick it up |
| `init` | First-time startup in progress (prepare pgdata → start PG → apply spec) |
| `running` | Fully operational, Postgres accepting connections |
| `configuration` | A new spec is being applied to a running compute |
| `failed` | Startup or reconfiguration failed; `error` is populated |
| `termination_pending_fast` / `termination_pending_immediate` | `/terminate` requested; the variants differ only in how long the process lingers after Postgres exits (30 s vs ~300 ms) |
| `terminated` | Postgres has shut down |

`ComputeNode` (the long-lived handle, `compute.rs`) holds this behind a
`Mutex<ComputeState>` plus a `Condvar`; threads (the configurator, the monitor, HTTP
handlers) wait on status transitions rather than polling. Every transition also
re-points the `compute_ctl_up{build_tag,status}` gauge.

## Startup sequence

`main()` (`bin/compute-ctl.rs`) parses CLI args, builds a multi-thread tokio
runtime, initializes tracing/logging, loads the config (file or control plane),
raises the core-dump rlimit, constructs `ComputeNode`, and calls `run()`:

```
main()
 └─ ComputeNode::run()
     ├─ prewarm_postgres_vm_memory()    — only when started without a spec (pooled VM)
     ├─ launch external HTTP server    (--external-http-port, default 3080)
     ├─ launch internal HTTP server    (--internal-http-port, default 3081)
     ├─ wait_spec()                    — block here if no spec yet (status: empty)
     ├─ launch LSN-lease task          — Static mode only (lsn_lease.rs)
     └─ start_compute()                — status: init
         ├─ pre-startup tasks (run in parallel):
         │    ├─ watch TLS certs (tls.rs)
         │    ├─ download remote shared_preload_libraries (extension_server.rs)
         │    ├─ prepare_pgdata()      ← critical path
         │    │    ├─ reclaim_pgdata() — shut down a postmaster still running in it
         │    │    ├─ wipe + recreate pgdata/ (mode 0700)
         │    │    ├─ config::write_postgres_conf()
         │    │    ├─ pick the startup LSN (safekeeper TIMELINE_STATUS for Primary)
         │    │    ├─ get_basebackup()  — from the pageserver at that LSN (gRPC or libpq)
         │    │    ├─ update pg_hba.conf / pg_ident.conf
         │    │    └─ symlink pg_dynshmem → /dev/shm; standby.signal for Replica/Static
         │    ├─ resize swap / set disk quota (per spec + CLI gates)
         │    ├─ tune pgbouncer / configure local_proxy (fire-and-forget)
         │    └─ configure audit + log-export rsyslog
         ├─ launch_monitor() + launch_configurator()
         ├─ start_postgres()           — exec the postgres child (own process group,
         │                                 PR_SET_PDEATHSIG=SIGQUIT)
         ├─ configure_as_primary()     — apply the spec SQL (Primary only)
         ├─ status: running
         └─ background: extension-stats task, LFC auto-prewarm, LFC offload loop
     ├─ wait_postgres()                — block until the child exits, then SIGKILL its
     │                                   process group to reap stragglers
     └─ cleanup_after_postgres_exit()  — core-dump backtraces, flip status to terminated
```

`run()` wraps `start_compute()` + `wait_postgres()` in a bounded **startup retry
loop** (5 attempts, 30 s grace window): if startup fails, or Postgres exits non-zero
within 30 s of starting, `compute_ctl` re-runs the whole startup — including a fresh
basebackup at the advanced LSN. This exists because the walproposer deliberately
PANICs when WAL advanced between the basebackup and its startup handshake
("safekeeper end-of-WAL … is behind basebackup LSN …"), expecting to be restarted from a fresh
basebackup; with `restart_after_crash=off` that surfaces as a quick non-zero exit.
Without the retry, a compute recycle while WAL is in flight crash-loops into a
walproposer election storm ([#71]). A clean exit (code 0), a long-lived run, or a
signal kill is never retried, so intentional shutdowns are not fought.

[#71]: https://github.com/yugabyte/amp/issues/71

### The startup LSN and the single safekeeper

yb-amp runs a single safekeeper per branch, which reports `commit_lsn == flush_lsn`
(a quorum of one — every flush is durably committed, because the safekeeper's WAL
itself lives in YugabyteDB). The startup LSN for a Primary therefore comes straight
from the `TIMELINE_STATUS` query (`sync_sk.rs`, `check_safekeepers_synced()`): ping
each safekeeper in `safekeeper_connstrings`, require a quorum of identical
`flush_lsn`/`commit_lsn` answers with `commit == flush`. There is no
multi-safekeeper consensus to drive, so `compute_ctl` never spawns
`postgres --sync-safekeepers` — the `sync_safekeepers_ms` startup metric is always 0
and `/terminate` reports `lsn: null` (see [#58]).

The check retries up to 10 times with backoff (`prepare_pgdata()`): during a compute
recycle after a safekeeper restart the first ping can time out or catch a
walproposer reconnection handshake, and a single miss must not abort startup — exiting
`compute_ctl` gets it respawned by the orchestrator, and that restart churn feeds
the same election storm. Replica mode uses `Lsn(0)` ("latest") and Static
mode uses its pinned LSN.

### Basebackup

`get_basebackup()` retries up to 10 times with exponential backoff (500 ms × 1.5).
The transport is chosen from `pageserver_connection_info.prefer_protocol`:

- **gRPC** (`grpc://`) — `page_api::Client::get_base_backup` with gzip payload
  compression, `replica: true` for non-Primary modes.
- **libpq** (`postgresql://`) — a `basebackup <branch_id> [<lsn>] --gzip
  [--replica]` COPY-out. The connection sets `-c yb_amp.compute_mode=<mode>` and
  uses `storage_auth_token` as the password when present.

Both paths untar the (gzip) stream straight into `pgdata` with `ignore_zeros` set,
so a server-side error emitted after the end-of-archive marker is still noticed.

## The compute spec

The spec is `ComputeSpec` (`compute/libs/compute_api/src/spec.rs`) — the complete
description of what the compute should be: mode, pageserver/safekeeper connection
info, the catalog (`cluster.roles`, `cluster.databases`, `delta_operations`), GUCs
(`cluster.settings`, `cluster.postgresql_conf`), feature flags, audit level,
LFC/prewarm knobs (`autoprewarm`, `offload_lfc_interval_seconds`),
pgbouncer/local_proxy config, remote-extension index, swap/disk-quota sizes, and
`suspend_timeout_seconds`. It travels inside a `ComputeConfig`
(`compute/libs/compute_api/src/responses.rs`):

```json
{ "spec": { ... },              // Option<ComputeSpec>
  "compute_ctl_config": { "jwks": {...}, "tls": null } }
```

`compute_ctl_config` carries the JWKS (and optional TLS cert paths) that the
external HTTP server's auth verifies against. The config is acquired two ways:

- **From a file** — `--config <path>`. This is how `yb_amp_local` drives it: the
  endpoint's `endpoint-supervisor` renders `compute_spec.json` into the endpoint
  dir and passes `--config`, the same as in a pod.
- **From the control plane** — `--control-plane-uri <base>` (with `--compute-id`),
  which GETs `{base}/compute/api/v2/computes/{id}/spec` with 3 attempts (`spec.rs`),
  sending `Bearer $YBAMP_CONTROL_PLANE_TOKEN` if that env var is set. The response
  (`ControlPlaneConfigResponse`) is the same payload plus an `Empty | Attached`
  status discriminator, and converts into `ComputeConfig` by dropping `status`. The
  two flags are mutually exclusive.

**One schema, three transports.** The same data can also be *pushed* later as the
body of `POST /configure`. So the spec is identical whether it arrives as a file, a
pull from the spec endpoint, or a push to `/configure` — the differences are
delivery and trust (a local file read vs. an authenticated, retried HTTP exchange
carrying the `Empty`/`Attached` handshake), not content. See
[amp-controller §5.5](../../controller/architecture/amp-controller.md).

At load, the raw spec is wrapped in a `ParsedSpec` (`compute.rs`) that extracts and
validates the load-bearing parts, each with a GUC fallback so older spec layouts
keep working:

| `ParsedSpec` field | Primary source | Fallback |
|---|---|---|
| `branch_id` | `storage_branch_id` | `yb_amp.branch_id` in `cluster.settings` |
| `pageserver_conninfo` | `pageserver_connection_info` | `pageserver_connstring` field, then `yb_amp.pageserver_connstring` GUC |
| `safekeeper_connstrings` | `safekeeper_connstrings` | `yb_amp.safekeepers` GUC (required, non-empty, unique entries — Primary only) |
| `storage_auth_token` | `storage_auth_token` | — (bearer auth to PS/SK when set; exported to Postgres as `YBAMP_AUTH_TOKEN`) |
| `yb_storage_conn` | `yb_storage_conn` | the YB blob store LFC prewarm/offload reads and writes |
| `lfc_state_url` | `lfc_state_url` | when set, LFC prewarm/offload goes through the supervisor's loopback `lfc_state` server instead of `yb_storage_conn`. Set whenever the supervisor renders the spec (see `single-container-endpoint.md`) |

The compute is **single-shard and branch-scoped**: storage is addressed by a single
`BranchId`, with no shard or tenant/timeline identifiers anywhere in the spec or the
supervisor.

### Compute modes

`ComputeMode` (`compute/libs/compute_api/src/spec.rs`) selects behavior:

| Mode | Role | `compute_ctl` behavior |
|---|---|---|
| `Primary` | read-write | applies the full spec SQL; requires `safekeeper_connstrings`; reads the startup LSN from the safekeeper's `TIMELINE_STATUS` |
| `Static(Lsn)` | read-only, pinned | skips spec application; writes `recovery_target_lsn`; runs the LSN-lease renewal task so the pageserver won't GC the pinned LSN |
| `Replica` | read-only, hot standby | follows the primary from the latest LSN; can be promoted to Primary via `POST /promote` |

## Generating postgresql.conf

`config::write_postgres_conf()` (`config.rs`) rewrites
`pgdata/postgresql.conf` from scratch on every start and reconfiguration, in this
order (later lines win in Postgres, which is the point):

1. `cluster.postgresql_conf` verbatim (the spec's base config).
2. Storage wiring: `yb_amp.pageserver_connstring` (the libpq URLs from
   `pageserver_connection_info`, comma-joined), `yb_amp.safekeepers`,
   `yb_amp.branch_id`, `yb_amp.cluster_id`, `yb_amp.endpoint_id`.
3. TLS (`ssl = on` + cert/key paths) when the `tls_experimental` feature flag and
   `compute_ctl_config.tls` are both present; the key is copied into `pgdata` with
   0600 perms because Postgres insists on a private key file.
4. Locales, `yb_amp.compute_mode`, and mode extras (`hot_standby=on`,
   `recovery_target_lsn` for Static).
5. `dynamic_shared_memory_type = mmap` when the host has
   `/proc/sys/vm/overcommit_memory == 2` — paired with the `pg_dynshmem →
   /dev/shm` symlink made in `prepare_pgdata()`. Rationale: with the `posix` DSM
   implementation, `posix_fallocate()` on tmpfs returns ENOMEM for segments larger
   than current RAM, which breaks autoscaling's "allocate big, scale up when
   touched" model; the `mmap` implementation fills the file with `write(2)` and can
   grow into swap.
6. `yb_amp.privileged_role_name` (from `--privileged-role-name`, default
   `yb_amp_superuser`).
7. The spec's `cluster.settings`, bracketed by `# Managed by compute_ctl` markers.
8. Audit settings, written *after* the spec settings so neither the user nor the
   control plane can override them: `base`/`log` levels get
   `pgaudit.log='ddl,role'`; `hipaa`/`extended`/`full` get `pgaudit.log='all[, -misc]'`,
   parameter logging on for `full`, 5-minute log rotation, and `pgaudit` /
   `pgauditlogtofile` are appended to `shared_preload_libraries` if missing.
9. `yb_amp.extension_server_port=<internal HTTP port>` — how the `yb_amp`
   extension finds the internal API for on-demand extension downloads.
10. `yb_amp.disable_logical_replication_subscribers` (true iff
    `drop_subscriptions_before_start`), `log_destination='stderr,syslog'` when logs
    export is on.
11. Last line, always: `include_if_exists = 'compute_ctl_temp_override.conf'`.

That include is the hook for `with_compute_ctl_tmp_override()`: during spec
application and reconfiguration `compute_ctl` temporarily writes
`yb_amp.max_cluster_size=-1` there (so catalog changes can't trip the size limit),
runs the work, then truncates the file.

## Applying the spec

`configure_as_primary()` → `apply_config()` → `apply_spec_sql()` (`spec_apply.rs`)
runs an ordered set of idempotent phases over a maintenance connection
(`cloud_admin`, with `yb_amp.forward_ddl = false` since the control plane already
knows about these objects):

1. **Pre-drop pass** — `DropLogicalSubscriptions` runs in every database about to be
   dropped via `delta_operations: delete_db`.
2. **Cluster-wide phases** (single connection, in order): `CreatePrivilegedRole`,
   `DropInvalidDatabases`, `RenameRoles`, `CreateAndAlterRoles`,
   `RenameAndDeleteDatabases`, `CreateAndAlterDatabases`, `CreateSchemaYbAmp`.
3. **Per-database phases**, parallelized across databases up to a connection cap
   (`reconfigure_concurrency`, default 1, during reconfiguration; a computed cap at
   first start): `DeleteDBRoleReferences`, `ChangeSchemaPerms`, and (first start on
   a fresh branch) `DropLogicalSubscriptions`. Whether the subscription drop already
   happened is recorded in the `yb_amp.drop_subscriptions_done` table keyed by
   `branch_id` — the spec flag alone can't distinguish "first start on this branch"
   from "same VM restarting with the same spec", and dropping twice would destroy
   subscriptions the user created in between.
4. **Finishing phases**: `HandleOtherExtensions` (the preloaded extensions, see
   `post_apply_config` below — amp's own specs set `skip_pg_catalog_updates`, so
   this pass is skipped and that is where they are actually created),
   `HandleYbAmpExtension` (`CREATE/UPDATE EXTENSION yb_amp` where the spec installs
   it), `CreateAvailabilityCheck` (the `public.health_check` table used by
   `POST /check_writability`), `DropRoles`, then per audit level the pgaudit
   extension phases.

Afterwards `post_apply_config()` spawns a background task that runs
`ALTER EXTENSION yb_amp UPDATE` — only if the extension is actually installed in the
maintenance database, to avoid a spurious error on every startup — and creates the
**preloaded extensions**: `CREATE EXTENSION IF NOT EXISTS <name> WITH SCHEMA public`
for every entry of `spec_apply.rs`'s `PRELOADED_EXTENSIONS` that the effective
`shared_preload_libraries` names (`pg_stat_statements`, a contrib module, and
`pgsentinel`, built from `compute/pgxn`). It runs here, not in the
`HandleOtherExtensions` phase above, because this task runs whether or not the
spec sets `skip_pg_catalog_updates` — and amp's specs do. The schema is pinned
because these extensions are relocatable: without it one lands in the first schema
of the connection's `search_path`, while `/insights` reads
`public.pg_stat_statements` by name.

### Catalog migrations

`handle_migrations()` (`spec.rs`, `migration.rs`) runs *after* the spec SQL, in a
background task so it never holds up a cold start. The migration list is the twelve
files under `src/migrations/*.sql` (BYPASSRLS for the privileged role, role
alterations, `pg_create_subscription` / `pg_monitor` / `pg_signal_backend` grants,
default-privilege grants on tables and sequences with and without grant option,
replication revocation, snapshot-synchronization and replication-origin function
grants) — most templated with `--privileged-role-name`. `MigrationRunner` tracks
progress in `yb_amp_migration.migration_id` (a single-row table), runs each
migration in its own transaction, updates the id in the same transaction, and stops
at the first failure (incrementing `compute_ctl_db_migration_failed_total`). A
migration whose text starts with `-- SKIP` only advances the id. Appending to the
end of the list is the only safe way to add one.

## Background loops

- **monitor** (`monitor.rs`) — a dedicated thread polling Postgres every 500 ms.
  It maintains the `compute_pg_current_downtime_ms` / `compute_pg_downtime_ms_total`
  metrics and computes `last_active` for suspend decisions from: per-database
  `active_time`/`sessions` deltas (when the `activity_monitor_experimental` feature
  flag is set), client-backend state changes, live walsenders (excluding the
  walproposer), active logical-replication subscriptions, and running autovacuum
  workers. It exits once the compute reaches a terminal status. When
  `--pg-init-timeout` is passed it also enforces that deadline: if Postgres doesn't
  reach `running` in time — usually a sign the compute was started against wrong
  storage — the process exits so it can restart with a fresh spec. Without the flag
  it waits indefinitely.
- **configurator** (`configurator.rs`) — a thread waiting on the `Condvar` for
  `configuration_pending` (set by `POST /configure`), flips status to
  `configuration`, calls `reconfigure()`, then back to `running` or `failed`.
- **extension-stats task** — periodically inventories `pg_extension` across
  databases into the `compute_installed_extensions` gauge
  (`installed_extensions.rs`); the interval derives from
  `suspend_timeout_seconds`.
- **LFC offload loop** — see below.

## Reconfiguration

`reconfigure()` (`compute.rs`) rewrites `postgresql.conf`, `pg_ctl reload`s it,
re-applies the spec SQL (Primary only, unless `skip_pg_catalog_updates`, under the
`max_cluster_size=-1` override), and refreshes pgbouncer / local_proxy / rsyslog
config.

## Termination

`POST /terminate[?mode=fast|immediate]` flips the status to the corresponding
`termination_pending_*`, sends Postgres a fast-shutdown SIGINT (fast shutdown also
writes a shutdown checkpoint, which lets read replicas get the running-xacts list
without scanning CLOG), and blocks until the main thread observes the exit and sets
`terminated`. The response is `{"lsn": null}` — nothing populates
`terminate_flush_lsn` ([#58]; the durable LSN is already on the safekeeper, whose
WAL is in YugabyteDB). `fast` keeps the process alive ~30 s afterwards so the
control plane can collect status; `immediate` exits after ~300 ms.

Process signals (SIGINT/SIGTERM/SIGQUIT) take the same path via
`forward_termination_signal()`, which also SIGKILLs pgbouncer and SIGTERMs
local_proxy (skipped with `--dev`, where those sidecars are not `compute_ctl`'s to
kill). `handle_exit_signal()` then waits (8 s) for Postgres to go before it exits:
`compute_ctl`'s own exit is a signal to Postgres now (below), so exiting straight
away would overtake the fast shutdown it just asked for and lose the shutdown
checkpoint. This is the supervisor's ordinary stop path — SIGTERM, then SIGKILL
after its 10 s grace.

[#58]: https://github.com/yugabyte/amp/issues/58

## Shutdown and the orphaned postmaster

Postgres outliving its `compute_ctl` is not a tidiness problem: the postmaster
keeps the client **port** bound, and nothing the next `compute_ctl` does about a
failed start — its five retries all take a fresh basebackup — can fix a port
conflict. Measured on a k8s endpoint, five SIGKILLs of `compute_ctl` cost 20-25 s
of downtime four times and 530 s the fifth — which never recovered on its own
inside the 300 s observation cap ([#898]). Three mechanisms cover it,
each for a case the previous one cannot reach:

1. **`wait_postgres()` killpg** — the graceful path. Once the postmaster is reaped,
   `SIGKILL` its whole process group, because a backend can outlive the postmaster
   (one wedged on a crashed pageserver) and keep the port after reparenting.
2. **`PR_SET_PDEATHSIG` (SIGQUIT)** on the postgres spawn — the abrupt path. The
   kernel signals the postmaster the moment `compute_ctl` dies, so a SIGKILL, an
   OOM kill or an abort cannot orphan it. SIGQUIT (immediate shutdown) rather than
   SIGINT (fast), because a fast shutdown waits for the backends and the wedged one
   is exactly the backend that will not go. The deathsig is tied to the *thread*
   that forked, which is why the spawn must stay on `compute_ctl`'s main thread.
   Since it fires on *every* exit, the signal path waits for Postgres first
   (above) rather than letting it escalate the fast shutdown.
3. **`reclaim_pgdata()`** at the top of `create_pgdata()` — defence in depth, and
   a fix for something wrong on its own terms: `create_pgdata()` wipes the data
   directory, and doing that while something is still running in it frees none of
   what that process holds. The port stays bound, and the pidfile that would have
   identified it is one of the files just deleted, so Postgres refuses to start
   (`lock file "postmaster.pid" already exists`) or cannot bind, and the retry loop
   spends its budget on basebackups. Behind that, mechanism 2 is a single point of
   failure for an outage measured in minutes, and it cannot cover an orphan this
   `compute_ctl` did not spawn itself, or one whose deathsig an interposed `cgexec`
   took by forking rather than exec'ing. So this reads
   `pgdata/postmaster.pid`, confirms the pid is alive *and* running with its cwd in
   that data directory (`ChangeToDataDir` — how postgres marks the directory it
   owns, and what distinguishes an owner from a recycled pid), and shuts it down:
   SIGQUIT, then SIGKILL — of the process group when the postmaster leads one, of
   the pid alone when it does not, since `kill(-pid)` on a mere group member would
   signal an unrelated group. Before the wipe is also the only moment the pidfile
   is there to read at all.

[#898]: https://github.com/yugabyte/meko/issues/898

## Promotion (`compute_promote.rs`)

`POST /promote` turns a running Replica into the Primary; it is the write-endpoint
cut-over primitive (the previous Primary must already be terminated). The body is
`PromoteConfig { spec, wal_flush_lsn }` — the new Primary spec plus the WAL flush
LSN the old primary reached. Steps, all inside `promote_impl()`:

1. Refuse unless the compute is a Replica, and refuse while an LFC prewarm is
   missing or in flight (a promotion mid-prewarm would serve a cold cache; a
   *failed* prewarm only warns).
2. Poll `pg_last_wal_replay_lsn()` (up to 20 × 1 s) until the standby catches up to
   `wal_flush_lsn`.
3. `ALTER SYSTEM SET yb_amp.safekeepers` (from the new spec),
   `ALTER SYSTEM SET synchronous_standby_names = walproposer`, `pg_reload_conf()`.
4. `SELECT pg_promote()`, then verify `transaction_read_only` is off.
5. Swap the new `ParsedSpec` into state and `reconfigure()` as Primary.

The promotion runs once per process behind a `watch` channel: concurrent or repeated
`/promote` calls (including a client that disconnected and retried) attach to the
same in-flight attempt and receive its final `PromoteState`
(`completed { lsn_wait_time_ms, pg_promote_time_ms, reconfigure_time_ms }` or
`failed { error }`).

## LFC prewarm / offload (`compute_prewarm.rs`)

The Local File Cache's contents can be saved to and restored from **YugabyteDB**,
stored through `remote_storage`'s `YbStore` backend. Losing that store costs only cache
warmth, never durability. The object key is
`{branch_id}/{endpoint_id}/lfc_state`. A spec the endpoint supervisor rendered
reaches it through the supervisor's `lfc_state` server (`lfc_state_url`), so
`compute_ctl` holds no YugabyteDB credential; otherwise the spec's
`yb_storage_conn` carries the connection.

Both ids are validated before they reach the key: `?from_endpoint=` arrives from
a query parameter, so an id containing `/` or `..` would otherwise address a key
outside the endpoint's own prefix.

- **Prewarm** (`POST /lfc/prewarm`, or automatically at startup when
  `spec.autoprewarm`): read the state (absent ⇒ `skipped` — normal on first
  start), zstd-decompress, feed it to `yb_amp.prewarm_local_cache()`, then report
  `yb_amp.get_prewarm_info()` counts. Cancellable via `DELETE /lfc/prewarm`; the
  store's own retries honour the cancellation token, so a cancel during a retry
  backoff is not held until the budget runs out. `?from_endpoint=<id>` prewarms
  from another endpoint's saved state (the readonly→readwrite flow).
- **Offload** (`POST /lfc/offload`, or every `offload_lfc_interval_seconds`): read
  `yb_amp.get_local_cache_state()`, zstd-compress, write it back. The periodic loop
  skips offloading while a prewarm is incomplete — uploading a half-warm state
  would overwrite the last complete snapshot, and a restart would then prewarm from
  the truncated state.

Both directions expose their state machines (`LfcPrewarmState`, `LfcOffloadState`)
over `GET /lfc/prewarm` / `GET /lfc/offload` and count attempts/errors in the
`compute_ctl_lfc_*` metrics.

## Other feature subsystems

| Module | What it does |
|---|---|
| `lsn_lease.rs` | Static mode: a background thread renews the pageserver LSN lease for the pinned LSN (gRPC or libpq per the spec's preferred protocol), sleeping until shortly before expiry and re-reading the pageserver conninfo on every cycle so a pageserver move is picked up. |
| `extension_server.rs` | Downloads remote extension archives (`{build_tag}/{arch}/{pg_version}/extensions/{name}.tar.zst`) from the `--remote-ext-base-url` HTTP gateway and unpacks them into PG's `lib`/`share`. Concurrent requests for the same archive coalesce via `ext_download_progress`. |
| `installed_extensions.rs` | The periodic `pg_extension` inventory → `compute_installed_extensions` metric. |
| `catalog.rs` | `GET /dbs_and_roles` inventory and the streamed `pg_dump --schema-only` behind `GET /database_schema`. |
| `checker.rs` | `POST /check_writability`: upsert a row in `public.health_check`; a `DISK_FULL` SQL error is tolerated (warned, not failed). |
| `swap.rs` / `disk_quota.rs` | `resize-swap` and `set-disk-quota` invocations, gated on both a CLI flag and a spec field so the control plane can roll them out without knowing which VM image it is talking to. |
| `pg_isready.rs` | Wraps the `pg_isready` binary for `GET /liveness_probe`. |
| `logger.rs` | Tracing + OTLP export; `startup_context_from_env()` attaches the startup span to a trace context passed in env vars, and request spans propagate via `X-Request-Id` and OpenTelemetry headers. |

## CLI reference

```
compute-ctl \
    --pgdata <endpoint>/pgdata \                # -D
    --connstr 'postgresql://cloud_admin@.../postgres' \   # -C
    --pgbin   <pg_install>/bin/postgres \       # -b, env POSTGRES_PATH
    --compute-id <endpoint_id> \                # -i
    --config  <endpoint>/compute_spec.json \    # -c; XOR with:
    --control-plane-uri <base_url> \            # -p
    [--external-http-port 3080] [--internal-http-port 3081] \
    [--remote-ext-base-url <url>] \             # -r
    [--privileged-role-name yb_amp_superuser] \ # ^[a-z_]+$ only
    [--resize-swap-on-bind] [--set-disk-quota-for-fs <mount>] \
    [--installed-extensions-collection-interval 3600] \
    [--pg-init-timeout <secs>] [--dev] \
    [--cgroup yb-amp-postgres] [--filecache-connstr <c>] [--vm-monitor-addr 0.0.0.0:10301]
```

`BUILD_TAG` (compile-time) stamps metrics and remote-extension paths.

In the dev cluster the endpoint's `endpoint-supervisor` renders the spec
(`compute_spec.json`) and runs the binary, the same way a pod does — see
[single-container-endpoint.md](single-container-endpoint.md). The launcher
decides when the compute is *up*, polling `GET /status` until `running`, since
only that distinguishes a compute that is serving from one that failed its
startup; it stops endpoints with `POST /terminate?mode=immediate` using a
self-minted `compute_ctl:admin` JWT.

## How it fits amp-controller

The [amp-controller design](../../controller/architecture/amp-controller.md) leans
on `compute_ctl` in a few concrete places:

- **Single point of contact (§5.2).** Endpoints are driven purely through the HTTP
  API — `POST /configure` to push a spec, `GET /status` to observe,
  `POST /terminate` to retire, `POST /promote` for write-endpoint cut-over.
- **The spec is the durable artifact (§5.3).** Statelessness works because
  everything needed to recreate a compute lives in the spec: storage connection
  info, the `branch_id` binding, mode, TLS/JWKS, the LFC state store.
  `compute_ctl` holds none of this durably itself; durable state lives in
  YugabyteDB behind the pageserver/safekeeper.
- **The LFC state blob (§11.5).** Prewarm and offload read and write it in YB
  directly, over the `yb_storage_conn` every spec carries.
- **Control-plane signing key (§9.4).** The external API verifies JWTs against the
  JWKS embedded in `compute_ctl_config` — the public half of the controller's key.
- **Single-writer invariant (§4.4 / §7).** At most one Primary per branch; upgrades
  and promotions are replace-and-cut-over via `/promote` + `/terminate`, never two
  writers at once.

## Where the code lives

- `compute/compute_tools/src/bin/compute-ctl.rs` — binary entry, CLI, `main()`.
- `compute/compute_tools/src/compute.rs` — `ComputeNode` / `ComputeState` /
  `ParsedSpec`, startup, basebackup, reconfigure, shutdown.
- `compute/compute_tools/src/config.rs` — `postgresql.conf` generation.
- `compute/compute_tools/src/spec.rs`, `spec_apply.rs` — spec acquisition and the
  ordered apply phases (SQL fragments under `src/sql/`).
- `compute/compute_tools/src/migration.rs`, `src/migrations/*.sql` — tracked
  catalog migrations.
- `compute/compute_tools/src/http/` — the two Axum servers, routes, JWT middleware
  ([`compute_ctl-apis.md`](compute_ctl-apis.md)).
- `compute/compute_tools/src/{monitor,configurator}.rs` — the background loops.
- `compute/compute_tools/src/{compute_prewarm,compute_promote,sync_sk,lsn_lease}.rs`
  — feature subsystems.
- `compute/libs/compute_api/src/{spec,requests,responses}.rs` — `ComputeSpec` /
  `ComputeMode` / `ComputeStatus` and the API request/response types, shared with
  the control plane.
- `controller/local/launch/endpoint.go`, `controller/local/launch/supervisor.go`,
  `controller/local/cfg` — the `yb_amp_local` endpoint lifecycle: it writes the
  two daemon tomls and runs the binary under `endpoint-supervisor`, which renders
  `compute_spec.json` itself.
