# yb-pageserver APIs

The pageserver exposes three surfaces:

1. the **management HTTP API** — router in `make_router`
   (`compute/pageserver/src/http/routes.rs`), wired up in
   `compute/pageserver/src/bin/yb-pageserver.rs`
   (`start_pageserver` → `http::make_router`);
2. the **compute-facing gRPC page service** —
   `GrpcPageServiceHandler` (`compute/pageserver/src/page_service.rs`)
   implementing `proto::PageService`
   (`compute/pageserver/page_api/proto/page_service.proto`);
3. the **compute-facing libpq page service** — `PageServerHandler` in
   the same file.

This document is the route-by-route inventory. Storage semantics behind
the handlers are in [`pageserver.md`](pageserver.md).

## Listeners

| Surface | Config | Default |
|---|---|---|
| HTTP mgmt | `listen_http_addr` | `127.0.0.1:9898` (`DEFAULT_HTTP_LISTEN_ADDR`, `compute/libs/pageserver_api/src/config.rs`) |
| HTTPS mgmt | `listen_https_addr` | off; when set, the same router is served over TLS on that address |
| libpq page service | `listen_pg_addr` | `127.0.0.1:64000` (`DEFAULT_PG_LISTEN_ADDR`) |
| gRPC page service | `listen_grpc_addr` | off; conventional port 51051 (`DEFAULT_GRPC_LISTEN_PORT`) |

## Management HTTP API

Responses are JSON (`json_response`) unless noted (`/metrics` is
Prometheus text, `/v1/doc` is HTML, `/profile/*` are pprof). Every API
handler runs through `api_handler`, which adds a tracing span, honors
the `api-503`/`api-500` failpoints (skipped for `/v1/failpoints`
itself), and shields the handler from async cancellation by running it
on a spawned task with a `CancellationToken` — most pageserver code is
not async-cancellation-safe, so a dropped hyper future becomes a
graceful cancellation instead. Every response carries a
`PAGESERVER_LAUNCH_TIMESTAMP` header (middleware). Anything unmatched
falls through to `handler_404` → `404` with
`{ "msg": "page not found" }`.

### Auth model

Auth is controlled by `http_auth_type`. With `AuthType::Trust` no token
is required; with `AuthType::YbAmpJWT` an EdDSA-signed JWT is required
via `auth_middleware`. Accepted scopes
(`compute/pageserver/src/auth.rs`, `check_permission`):

- `PageServerApi` — blanket access to all routes (branch-scoped and
  instance-wide).
- `Tenant` — access only to routes whose `:branch_id` matches the
  token's branch; rejected on instance-wide routes (those calling
  `check_permission(.., None)`).

All other scopes (`Admin`, `SafekeeperData`, `GenerationsApi`, `Infra`,
`Scrubber`, `ControllerPeer`, `TenantEndpoint`) are rejected.

Routes exempt from auth even when JWT is enabled
(`State::allowlist_routes`): `/v1/status`, `/v1/doc`, `/swagger.yml`,
`/metrics`, `/profile/cpu`, `/profile/heap`.

### Status / health / introspection

| Method | Path | Behavior |
| --- | --- | --- |
| GET | `/v1/status` | Health check. Returns `{ "id": <node_id> }` (`StatusResponse`). Instance-wide scope. |
| GET | `/metrics` | Prometheus text exposition (`prometheus_metrics_handler`). Honors `force_metric_collection_on_scrape`. See [`pageserver-metrics.md`](pageserver-metrics.md). |
| GET | `/v1/doc` | Swagger UI HTML for the embedded OpenAPI spec. |
| GET | `/swagger.yml` | Raw OpenAPI spec (`openapi_spec.yml`, embedded at compile time). |
| GET | `/v1/utilization` | Node utilization document polled by the control plane (`crate::utilization::regenerate`). Cached and regenerated at most ~1 Hz (`latest_utilization`). Returns `application/json`. Instance-wide scope; has a `get-utilization-http-handler` failpoint. |

### Profiling / debug

| Method | Path | Behavior |
| --- | --- | --- |
| GET | `/profile/cpu` | CPU profile (pprof) via `profile_cpu_handler`. |
| GET | `/profile/heap` | Heap profile (jemalloc/pprof) via `profile_heap_handler`. |
| GET | `/v1/panic` | Deliberately panics inside `catch_unwind` to exercise the panic hook. Returns `204 No Content`. |
| POST | `/v1/reload_auth_validation_keys` | Reloads JWT public key(s) from `auth_validation_public_key_path` and swaps them into the live `SwappableJwtAuth`. `400` if auth is not configured; `500` with an error body if the reload fails. |

### Testing-gated

These return `400 Bad Request` ("compiled without testing APIs") unless
the binary is built with the `testing` feature (`testing_api_handler`).

| Method | Path | Behavior |
| --- | --- | --- |
| PUT | `/v1/failpoints` | Configure failpoints (`http_utils::failpoints::failpoints_handler`). Exempt from the `api-503`/`api-500` failpoints so it can always clear them. |
| POST | `/v1/tracing/event` | Emit a tracing event at a chosen level. Body: `{ "level": "error\|warn\|info\|debug\|trace", "message": "..." }`. Invalid JSON → `400`. |

### Branch-scoped routes

All paths are under `/v1/branch/:branch_id/...`; `:branch_id` is parsed
as an ID and authorized against the token's branch scope. The process
serves exactly one `branch_id` for its lifetime (see
[`pageserver.md`](pageserver.md)), so only that single id
resolves; any other id returns `404` (`GetTenantError::NotFound`).

| Method | Path | Behavior |
| --- | --- | --- |
| GET | `/v1/branch/:branch_id/timeline` | List timelines for the branch as `TimelineInfo[]` (`timeline_list_handler`). Always a one-element list in practice. |
| GET | `/v1/branch/:branch_id` | Detail for the branch as a single `TimelineInfo` (`timeline_detail_handler`). |
| GET | `/v1/branch/:branch_id/get_lsn_by_timestamp` | Find the LSN for a timestamp (`get_lsn_by_timestamp_handler`). |
| GET | `/v1/branch/:branch_id/get_timestamp_of_lsn` | Find the timestamp for an LSN (`get_timestamp_of_lsn_handler`). |
| POST | `/v1/branch/:branch_id/lsn_lease` | Acquire an LSN lease, holding off GC at that LSN (`lsn_lease_handler`). |
| PUT | `/v1/branch/:branch_id/do_gc` | Run a GC iteration immediately (`timeline_gc_handler`). |
| PUT | `/v1/branch/:branch_id/flush` | Flush buffered pages to YB and force the durable `consistent_lsn` UPDATE through (`timeline_flush_handler`). |

Details per handler:

- **List / detail** accept optional bool query params
  `include-non-incremental-logical-size`,
  `force-await-initial-logical-size`, and
  `include-image-consistent-lsn`. In the returned `TimelineInfo`, the
  `disk_consistent_lsn` field (wire name kept for client compatibility)
  is populated with the durable `consistent_lsn` (gap-free,
  post-flush), and `last_record_lsn` holds the applied ingest position.
  `image_consistent_lsn`, when requested, is the applied LSN
  (`Timeline::compute_image_consistent_lsn` — there are no image
  layers, so applied is image-consistent by definition).
  `current_physical_size` is 0 (`layer_size_sum` — no layer files) and
  `min_readable_lsn` is `max(gc_cutoff, applied_gc_cutoff_lsn)`.
- **get_lsn_by_timestamp**: required query `timestamp` (RFC 3339;
  invalid → `400`); optional `with_lease=true` to also grant a lease at
  the found LSN. Returns `{ lsn, kind }` where `kind` ∈
  `present|future|past|nodata` (`LsnForTimestamp`), plus flattened
  lease fields when requested.
- **get_timestamp_of_lsn**: required query `lsn`. Returns the RFC 3339
  timestamp string, or `412 Precondition Failed` when no commit record
  at or below that LSN carries a timestamp.
- **lsn_lease**: JSON body `LsnLeaseRequest { lsn }`. Returns the
  granted `LsnLease` (with `valid_until`).
- **do_gc**: JSON body `TimelineGcRequest` (optional `gc_horizon`
  override; PITR comes from the branch config). Runs
  `TenantManager::immediate_gc` and returns the `GcResult`. Note
  `Timeline::gc` deletes no rows — this refreshes cutoffs and lease
  state only (see [`pageserver.md`](pageserver.md) "Background
  tasks").
- **flush**: optional query `lsn=<lsn>` — first `wait_lsn`s until WAL
  ingest reaches that LSN, then calls `Timeline::force_flush()`
  (`PageWriter::flush` + `force_upsert_consistent_lsn_if_advanced`), so
  the post-flush durable `consistent_lsn` is `>= lsn` and the UPDATE
  bypasses the per-`max_age` upsert throttle the periodic flushes use.
  **Not** testing-gated: the launcher calls it on the ancestor at
  branch time so the branch's `create_branch_row` durability barrier
  clears immediately instead of waiting out a flush interval. Returns
  `200` with an empty body; `ShuttingDown` maps to the standard
  shutdown error.

### Surface boundaries

The router contains **no branch lifecycle routes**: no branch/timeline
create or delete, no attach/detach, no location_config, no
archival/detach-ancestor, no download/layer/eviction/heatmap/secondary
routes, and no compact/checkpoint. The branch identity is baked into
`pageserver.toml` and opened synchronously at startup; branch creation
is driven by the launcher (config bake + the `flush?lsn=` barrier), and
there are no layer files to manage. `flush` is the only
write-side maintenance verb.

## gRPC page service

Served when `listen_grpc_addr` is set, on the compute-request runtime;
this port is exposed to computes and carries only compute-safe
services. `proto::PageService` RPCs:

| RPC | Shape | Behavior |
| --- | --- | --- |
| `GetBaseBackup` | unary → stream | Base backup tarball streamed in 256 KiB chunks. |
| `GetDbSize` | unary | Total size of a database's relations at an LSN. |
| `GetPages` | bidi stream | The getpage hot path: a stream of `GetPageRequest`s answered by a stream of `GetPageResponse`s. |
| `GetRelSize` | unary | Relation size in blocks at an LSN. |
| `GetSlruSegment` | unary | An SLRU segment's pages at an LSN. |
| `LeaseLsn` | unary | Acquire/renew an LSN lease. |

Plumbing: a `BranchMetadataInterceptor` extracts the branch identity
from request metadata and a `TenantAuthInterceptor` validates the JWT
(same `check_permission` scopes as HTTP: `PageServerApi` or a matching
`Tenant`) when `grpc_auth_type = "YbAmpJWT"`. Requests for a branch
other than the configured identity are rejected as misrouted. A
tonic-reflection service is registered so `grpcurl` works. TCP and
HTTP/2 keepalives are configured; responses are sent uncompressed
(pageserver and compute are co-located, so compressing getpage replies
burns CPU for no bandwidth win) while gzip/zstd request compression is
accepted.

## libpq page service

The libpq listener (`listen_pg_addr`) speaks the Postgres wire protocol
with `pg_auth_type` ∈ {`Trust`, `YbAmpJWT`}; with JWT, a `Tenant`-scope
token must carry a `branch_id` claim matching the query's branch.
Query-string commands (`PageServiceCmd::parse`):

| Command | Behavior |
| --- | --- |
| `pagestream_v2 <branch_id>` / `pagestream_v3 <branch_id>` | Enter the COPY-both pagestream subprotocol. Messages: `Exists` (rel exists?), `Nblocks` (rel size), `DbSize`, `GetSlruSegment`, `GetPage`, and the testing-only `Test`. V3 carries per-request ids. Requests are batched and pipelined per `page_service_pipelining` (default `pipeline_depth` 16); on client disconnect, in-flight reads are aborted rather than drained. |
| `basebackup <branch_id> [<lsn>\|latest] [--gzip] [--replica]` | Stream a base backup tarball at the given (or latest) LSN. Waits for the replay-quarantine latch and pre-flushes the PageWriter first; ships no SLRU pages when `lazy_slru_download` is true (the default). |
| `fullbackup <branch_id> [<lsn> [<prev_lsn>]]` | Like basebackup but includes relation data files — a self-contained pgdata. |
| `lease lsn <branch_id> <lsn>` | Acquire an LSN lease. |
| `set ...` | Accepted and ignored (compat no-op for clients that send `SET`). |

Every command validates `branch_id` against the configured identity;
mismatches count `MISROUTED_PAGESTREAM_REQUESTS` and fail. Sockets get
TCP keepalive (30 s idle + 3 × 10 s probes) so computes that vanish
without a FIN are reaped and their fds reclaimed.
