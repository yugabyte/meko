# yb-pageserver HTTP management API

Source: `compute/pageserver/src/http/routes.rs` (router in `make_router`), wired up in
`compute/pageserver/src/bin/yb-pageserver.rs` (`start_pageserver` → `http::make_router`).

The `yb-pageserver` binary serves this management API on a hyper listener bound
to `listen_http_addr` (default `127.0.0.1:9898`, see
`compute/libs/pageserver_api/src/config.rs` `DEFAULT_HTTP_LISTEN_ADDR`). If
`listen_https_addr` is set, the same router is also served over TLS on that
address. This is distinct from the compute-facing libpq port (`listen_pg_addr`,
default `127.0.0.1:64000`) and the optional gRPC port (`listen_grpc_addr`).

Responses are JSON (`json_response`) unless noted (`/metrics` is Prometheus text,
`/v1/doc` is HTML, `/profile/*` are pprof). Every API handler runs through
`api_handler`, which adds a tracing span and shields the handler from async
cancellation by running it on a spawned task with a `CancellationToken`.

## Auth model

Auth is controlled by `http_auth_type` in config. With `AuthType::Trust` no token
is required; with `AuthType::YbLightJWT` an EdDSA-signed JWT is required via the
`auth_middleware`. The accepted scopes (`compute/pageserver/src/auth.rs`
`check_permission`) are:

- `PageServerApi` — blanket access to all routes (branch-scoped and instance-wide).
- `Tenant` — access only to routes whose `:branch_id` matches the token's branch;
  rejected on instance-wide routes (those calling `check_permission(.., None)`).

All other scopes (`Admin`, `SafekeeperData`, `GenerationsApi`, etc.) are rejected.

The following routes are exempt from auth even when JWT is enabled
(`State::allowlist_routes`): `/v1/status`, `/v1/doc`, `/swagger.yml`, `/metrics`,
`/profile/cpu`, `/profile/heap`.

Every response carries a `PAGESERVER_LAUNCH_TIMESTAMP` header (set by middleware).

## Routes

### Status / health / introspection

| Method | Path | Purpose |
| --- | --- | --- |
| GET | `/v1/status` | Health check. Returns `{ "id": <node_id> }`. No branch scope required. |
| GET | `/metrics` | Prometheus metrics (text exposition). See `yb-pageserver-metrics.md`. Honors `force_metric_collection_on_scrape`. |
| GET | `/v1/doc` | Swagger UI HTML for the embedded OpenAPI spec. |
| GET | `/swagger.yml` | Raw OpenAPI spec (`openapi_spec.yml`, embedded at compile time). |
| GET | `/v1/utilization` | Node utilization document polled by the control plane (`crate::utilization::regenerate`). Cached/regenerated at most ~1 Hz. Returns `application/json`. |

### Profiling / debug

| Method | Path | Purpose |
| --- | --- | --- |
| GET | `/profile/cpu` | CPU profile (pprof) via `profile_cpu_handler`. |
| GET | `/profile/heap` | Heap profile (jemalloc/pprof) via `profile_heap_handler`. |
| GET | `/v1/panic` | Deliberately panics in a `catch_unwind` to exercise the panic hook. Returns `204`. |
| POST | `/v1/reload_auth_validation_keys` | Reload JWT public key(s) from `auth_validation_public_key_path` and swap into the live `SwappableJwtAuth`. Returns `400` if auth is not configured. |

### Testing-only (compiled out unless built with the `testing` feature)

These return `400 Bad Request` ("compiled without testing APIs") when the
`testing` feature is off (`testing_api_handler`).

| Method | Path | Purpose |
| --- | --- | --- |
| PUT | `/v1/failpoints` | Configure failpoints (`failpoints_handler`). Bypasses the `api-503`/`api-500` failpoints. |
| POST | `/v1/tracing/event` | Emit a tracing event at a chosen level. Body: `{ "level": "error\|warn\|info\|debug\|trace", "message": "..." }`. |

### Branch (branch-scoped)

All paths are under `/v1/branch/:branch_id/...`; `:branch_id`
is parsed as an ID and authorized against the token's branch scope. In yb-light
the process serves exactly one `branch_id` for its lifetime (see
`architecture/yb-pageserver.md`), so in practice only that single id resolves;
other IDs return `404`/`NotFound`.

| Method | Path | Purpose |
| --- | --- | --- |
| GET | `/v1/branch/:branch_id/timeline` | List timelines for the branch as `TimelineInfo[]`. |
| GET | `/v1/branch/:branch_id` | Detail for the branch (`TimelineInfo`). |
| GET | `/v1/branch/:branch_id/get_lsn_by_timestamp` | Find the LSN for a timestamp. |
| GET | `/v1/branch/:branch_id/get_timestamp_of_lsn` | Find the timestamp for an LSN. |
| POST | `/v1/branch/:branch_id/lsn_lease` | Acquire an LSN lease (hold off GC at an LSN). |
| PUT | `/v1/branch/:branch_id/do_gc` | Run GC immediately on the branch. |
| PUT | `/v1/branch/:branch_id/flush` | Flush pages to YB, advancing the durable `consistent_lsn`. |

#### Key params / bodies / responses

- **List / detail** (`timeline_list_handler`, `timeline_detail_handler`) accept
  optional bool query params: `include-non-incremental-logical-size`,
  `force-await-initial-logical-size`, `include-image-consistent-lsn`. Note in the
  returned `TimelineInfo`, the `disk_consistent_lsn` field is populated with the
  yb durable `consistent_lsn` (gap-free, post-flush), and `last_record_lsn` holds
  the applied LSN.
- **get_lsn_by_timestamp** (`get_lsn_by_timestamp_handler`): required query
  `timestamp` (RFC3339); optional `with_lease=true` to also grant a lease at the
  found LSN. Returns `{ lsn, kind }` where `kind` ∈ `present|future|past|nodata`,
  plus lease fields when requested.
- **get_timestamp_of_lsn** (`get_timestamp_of_lsn_handler`): required query
  `lsn`. Returns the RFC3339 timestamp string, or `412 Precondition Failed` if
  not found.
- **lsn_lease** (`lsn_lease_handler`): JSON body `LsnLeaseRequest { lsn }`.
  Returns the granted `LsnLease`.
- **do_gc** (`timeline_gc_handler`): JSON body `TimelineGcRequest` (e.g. PITR
  cutoff overrides). Returns the GC result.
- **flush** (`timeline_flush_handler`): optional query `lsn=<lsn>` — first waits
  for WAL ingest to reach that LSN, then calls `force_flush()` so the post-flush
  durable `consistent_lsn` is `>= lsn`. This is **not** testing-gated: the
  launcher calls it on the ancestor at branch time so the branch's
  `create_branch_row` durability barrier clears without waiting for the periodic
  flush. In yb-light "freeze" is a no-op (no in-memory layers); this is purely a
  flush-to-YB.

### Fallback

| Method | Path | Purpose |
| --- | --- | --- |
| ANY | (any unmatched) | `handler_404` → `404` with `{ "msg": "page not found" }`. |

## yb-light specifics vs upstream Neon

The router is heavily trimmed from upstream `pageserver`. Notable removals /
differences:

- **No branch lifecycle / attach / location-config routes.** There is no
  `POST /v1/branch`, no `branch/:id/location_config`, no attach/detach/ignore/load,
  no `do_gc`/`compact`/`checkpoint` at a branch-collection level. The
  `branch_id` identity is baked into `pageserver.toml` and opened
  synchronously at startup (single-branch process model).
- **No timeline create / delete / branch / archival / detach-ancestor routes.**
  Branching is driven by the launcher + the `flush?lsn=` barrier, not an HTTP
  create-branch call.
- **No on-demand download / layer / eviction / heatmap / secondary routes.** All
  storage lives in YB; there are no local layer files to manage or evict
  (disk-usage eviction was removed entirely).
- **`flush` replaces upstream's `checkpoint`/`compact` semantics** — it forces the
  durable `consistent_lsn` UPDATE through immediately, bypassing the periodic
  flush throttle.
- Auth uses the `YbLightJWT` auth type with only `PageServerApi` / `Tenant`
  scopes accepted.
