# Safekeeper HTTP API

Source: `compute/safekeeper/src/http/routes.rs` (router + handlers),
`compute/safekeeper/src/bin/yb-safekeeper.rs` (listener wiring),
`compute/libs/safekeeper_api/src/lib.rs` (port default).

The safekeeper exposes a small JSON/HTTP control-and-observability surface.
This is **separate** from the libpq replication protocol the safekeeper also
speaks (WAL push/stream); see ["Replication protocol (non-HTTP)"](#replication-protocol-non-http)
below.

## Binary, listen address, auth

- **Binary**: `yb-safekeeper` (`compute/safekeeper/src/bin/yb-safekeeper.rs`). The HTTP
  router is built by `make_router()` and served on the listener bound to
  `conf.listen_http_addr`.
- **Listen address**: `--listen-http`, default `127.0.0.1:7676`
  (`DEFAULT_HTTP_LISTEN_ADDR` / `DEFAULT_HTTP_LISTEN_PORT = 7676` in
  `compute/libs/safekeeper_api/src/lib.rs`). An optional HTTPS listener is bound when
  `--listen-https` is set.
- **Auth**: JWT (`SwappableJwtAuth`) is applied via `auth_middleware` only
  when `conf.http_auth` is configured. When enabled, every route requires a
  valid token **except** the allowlist `/v1/status`, `/metrics`,
  `/profile/cpu`, `/profile/heap`. Per-route scope is enforced by
  `check_permission()` (branch-scoped where a `branch_id` is in the path,
  otherwise global).

## Routes

### Status / health / observability

| Method | Path | Purpose |
| --- | --- | --- |
| GET | `/v1/status` | Healthcheck. Returns `SafekeeperStatus { id }` (the SK node id). Auth-exempt. |
| GET | `/metrics` | Prometheus metrics exposition (see [`safekeeper-metrics.md`](safekeeper-metrics.md)). Auth-exempt. Honors `force_metric_collection_on_scrape`. |
| GET | `/profile/cpu` | CPU profile (pprof). Auth-exempt. |
| GET | `/profile/heap` | Heap profile. Auth-exempt. |
| GET | `/v1/utilization` | Timeline counts for scheduling/utilization (`get_timeline_counts`). Global scope. |
| GET | `/v1/debug/filesystem_usage` | Filesystem capacity + usage of the SK data dir. Returns `{ data_dir, capacity_bytes, usage_bytes }`. Global scope. |

### Timeline lifecycle

| Method | Path | Purpose |
| --- | --- | --- |
| POST | `/v1/branch` | Create a branch. Body = `TimelineCreateRequest` (`branch_id`, `pg_version`, optional `system_id`, `wal_seg_size`, `start_lsn`). Defaults: `system_id=0`, `wal_seg_size=WAL_SEGMENT_SIZE`. |
| GET | `/v1/branch` | List all non-deleted branches as `[BranchId]`. Global scope. |
| GET | `/v1/branch/:branch_id` | Branch status: `TimelineStatus` with `pg_info`, `flush_lsn`, `commit_lsn`, `timeline_start_lsn`, `local_start_lsn`, `remote_consistent_lsn`, peers, walsenders, walreceivers. In-memory values (may be lost on crash). |
| DELETE | `/v1/branch/:branch_id` | Deactivate a branch and remove its data dir. Query `only_local=<bool>` (default false): `DeleteLocal` vs full `Delete`. |

### Timeline maintenance / debug

| Method | Path | Purpose |
| --- | --- | --- |
| PATCH | `/v1/branch/:branch_id/control_file` | Patch control-file fields. Body = `patch_control_file::Request`. Global scope. |
| POST | `/v1/branch/:branch_id/checkpoint` | Force-persist (flush) the control file to durable state. Global scope. |
| GET | `/v1/branch/:branch_id/digest` | Compute a WAL digest over `[from_lsn, until_lsn]`. Both query params **required** (400 otherwise). |
| POST | `/v1/branch/:branch_id/backup_partial_reset` | Unevict branch and remove uploaded partial segment(s) from remote storage; returns segments that existed before deletion. One-off repair tool. |

### Failpoints / global debug dump

| Method | Path | Purpose |
| --- | --- | --- |
| PUT | `/v1/failpoints` | Configure failpoints (testing). Global scope. |
| GET | `/v1/debug_dump` | Stream a full debug dump of SK state as `application/octet-stream`. Query toggles: `dump_all`, `dump_control_file`, `dump_memory`, `dump_disk_content`, `dump_wal_last_modified`, plus an optional `branch_id` filter. Most toggles default to the value of `dump_all` (default false). |

## yb-amp specifics

- **Single safekeeper / single-SK quorum**: `commit_lsn` is reported as
  identical to `flush_lsn` in `timeline_status_handler` (no multi-SK quorum
  to compute a separate commit point).
- **Durable state in YugabyteDB**: control-file and WAL semantics behind
  these routes are backed by YB (see [`safekeeper.md`](safekeeper.md)); the
  local data dir holds little. `filesystem_usage` / `disk_usage`-style
  endpoints reflect the local dir only.
- **Removed upstream routes**: there is **no** `pull_timeline` /
  `copy_timeline` / `timeline/.../snapshot` membership-migration surface in
  this fork's router — `make_router()` registers only the routes tabled
  above. The router build is the source of truth; the
  `openapi_spec.yaml` referenced in code comments may list upstream extras
  that do not exist here.

## Replication protocol (non-HTTP)

The WAL data plane is **not** HTTP. Computes (walproposer) and pageservers
connect over a libpq-style replication protocol on the `--listen-pg` port,
handled in `compute/safekeeper/src/handler.rs` (`parse_cmd`). Recognized commands:

- `START_WAL_PUSH` — walproposer pushes WAL into the SK.
- `START_REPLICATION [SLOT ..] [PHYSICAL] <lsn>` — consumer
  streams WAL forward from `<lsn>` (pageservers use the version-portable
  interpreted-WAL stream, `send_interpreted_wal`).
- `IDENTIFY_SYSTEM` — system identification handshake.
- `TIMELINE_STATUS` — timeline status over the replication connection.

The HTTP API documented here is for control and observability only.
