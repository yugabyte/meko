# Safekeeper HTTP API

Source: `compute/safekeeper/src/http/routes.rs` (router + handlers),
`compute/safekeeper/src/http/mod.rs` (HTTP/HTTPS servers),
`compute/safekeeper/src/bin/yb-safekeeper.rs` (listener wiring),
`compute/libs/safekeeper_api/src/lib.rs` (port defaults).

The safekeeper exposes a small JSON/HTTP control-and-observability surface.
This is **separate** from the libpq replication protocol the safekeeper also
speaks (WAL push/stream); see
["Replication protocol (non-HTTP)"](#replication-protocol-non-http) below.

## Binary, listen address, auth

- **Binary**: `yb-safekeeper`. The HTTP router is built by `make_router()`
  and served on the listener bound to `listen_http` from `safekeeper.toml`
  (default `127.0.0.1:7676`; `DEFAULT_HTTP_LISTEN_PORT = 7676` in
  `compute/libs/safekeeper_api/src/lib.rs`). An optional HTTPS listener is
  bound when `listen_https` is set, using the `ssl_key_file` /
  `ssl_cert_file` pair (hot-reloaded every `ssl_cert_reload_period`).
- **Auth**: JWT (`SwappableJwtAuth`) is applied via `auth_middleware` only
  when `http_auth_public_key_path` is configured. When enabled, every route
  requires a valid token **except** the allowlist `/v1/status`, `/metrics`,
  `/profile/cpu`, `/profile/heap`. Per-route scope is enforced by
  `check_permission()` (`compute/safekeeper/src/auth.rs`): `SafekeeperData`
  tokens pass everywhere; `Tenant` tokens pass only branch-scoped routes for
  their own `branch_id`; every other scope is rejected.

`make_router()` is the source of truth for the route set — it registers
exactly the routes tabled below. The `openapi_spec.yaml` file next to the
router is not kept authoritative.

## Routes

### Status / health / observability

| Method | Path | Purpose |
| --- | --- | --- |
| GET | `/v1/status` | Healthcheck. Returns an empty `SafekeeperStatus {}` (liveness only). Auth-exempt. |
| GET | `/metrics` | Prometheus exposition (see [`safekeeper-metrics.md`](safekeeper-metrics.md)). Auth-exempt. Honors `force_metric_collection_on_scrape`. |
| GET | `/profile/cpu` | CPU profile (pprof). Auth-exempt. |
| GET | `/profile/heap` | Heap profile (jemalloc). Auth-exempt. |
| GET | `/v1/utilization` | `SafekeeperUtilization { timeline_count }` for scheduling (`get_timeline_counts`; 0 or 1 in the one-branch-per-process model). Global scope. |
| GET | `/v1/debug/filesystem_usage` | Filesystem capacity + usage of the safekeeper workdir, via statvfs. Returns `{ data_dir, capacity_bytes, usage_bytes }`. Reflects the local dir only (logs/pidfile — WAL lives in YugabyteDB). Global scope. |

### Timeline lifecycle

| Method | Path | Purpose |
| --- | --- | --- |
| POST | `/v1/branch` | Create a branch. Body = `TimelineCreateRequest` (`branch_id`, `pg_version`, optional `system_id`, `wal_seg_size`, `start_lsn`). Defaults: `system_id=0`, `wal_seg_size=WAL_SEGMENT_SIZE`. Writes the branch's `safekeeper.timeline_state` row; returns the existing timeline if already present; 500 with `TimelineError::Deleted` if a tombstone blocks re-creation. Branch scope. |
| GET | `/v1/branch` | List all loaded (non-deleted) branches as `[BranchId]` — at most one entry per process. Global scope. |
| GET | `/v1/branch/:branch_id` | Branch status: `TimelineStatus` with `pg_info`, `flush_lsn`, `commit_lsn` (reported equal to `flush_lsn` — every flushed LSN is committed), `timeline_start_lsn`, `local_start_lsn`, `remote_consistent_lsn`, peers, walsenders, walreceivers. In-memory values. Branch scope. |
| DELETE | `/v1/branch/:branch_id` | Deactivate the branch, remove its (empty) local dir, and delete its `safekeeper.timeline_state` row. Query `only_local=<bool>` (default false) skips the YB row deletion (`DeleteOrExclude::DeleteLocal` vs `Delete`). Returns `TimelineDeleteResult { dir_existed }`. Leaves an in-memory tombstone (24 h TTL) blocking on-demand re-creation. Branch scope. |

### Timeline maintenance / debug

| Method | Path | Purpose |
| --- | --- | --- |
| PATCH | `/v1/branch/:branch_id/control_file` | Patch control-file fields via JSON diff. Body = `patch_control_file::Request { updates, apply_fields }`; applies only the named fields and persists the row. Returns old + new state. Global scope. |
| POST | `/v1/branch/:branch_id/checkpoint` | Force-persist the control file (flush in-memory state to the `timeline_state` row). Global scope. |
| GET | `/v1/branch/:branch_id/digest` | Compute a SHA-256 WAL digest over `[from_lsn, until_lsn]` (reads WAL from YB via a residence guard). Both query params **required** (400 otherwise). Branch scope. |
| POST | `/v1/branch/:branch_id/backup_partial_reset` | Returns `[]` unconditionally — the manager answers `BackupPartialReset` with an empty segment list; there is no partial-segment upload state to reset (WAL durability is the `safekeeper.wals` mirror, not an object-storage tier). The route exists so callers get a well-formed 200. Branch scope. |

### Failpoints / global debug dump

| Method | Path | Purpose |
| --- | --- | --- |
| PUT | `/v1/failpoints` | Configure failpoints (testing builds). Global scope. |
| GET | `/v1/debug_dump` | Stream a full JSON dump of safekeeper state as `application/octet-stream`. Query toggles: `dump_all`, `dump_control_file`, `dump_memory`, `dump_disk_content`, `dump_wal_last_modified`, plus an optional `branch_id` filter; unknown params are a 400. Each toggle defaults to the value of `dump_all` (default false). `dump_memory` includes the `WalBookkeeping` watermarks and manager status; `dump_disk_content` / `dump_wal_last_modified` scan the local timeline dir, which holds no WAL files. Global scope. |

## Replication protocol (non-HTTP)

The WAL data plane is **not** HTTP. Computes (walproposer) and pageservers
connect over a libpq-style replication protocol on the `listen_pg` port
(default `127.0.0.1:5454`), handled in `compute/safekeeper/src/handler.rs`
(`parse_cmd`). The connection's startup options must carry
`branch_id=<id>`; `process_query` rejects any connection whose branch does
not match the process's baked identity before it can touch the registry.
Recognized commands:

- `START_WAL_PUSH [(proto_version 'N', allow_timeline_creation 'bool')]` —
  the walproposer pushes WAL in. Only `proto_version '3'` is accepted (also
  the default when the option is absent); `allow_timeline_creation`
  (default true) lets the handshake create the timeline on demand.
- `START_REPLICATION [SLOT ..] [PHYSICAL] <lsn>` — a consumer streams WAL
  forward from `<lsn>`. Output is always the version-portable
  interpreted-WAL protocol (`send_interpreted_wal`); the protocol choice
  arrives via the `protocol` startup option and defaults to
  `Interpreted { format: Protobuf }`.
- `IDENTIFY_SYSTEM` — system identification handshake (`systemid`,
  `timeline`, `xlogpos` = `flush_lsn`).
- `TIMELINE_STATUS` — one row of `flush_lsn` / `commit_lsn` over the
  replication connection (both columns carry `flush_lsn`).

A second, tenant-scoped replication listener is bound when
`listen_pg_tenant_only` is configured; it accepts only `Tenant`-scoped JWTs.
The WAL service speaks TLS when `enable_tls_wal_service_api` is set.

The HTTP API documented here is for control and observability only.
