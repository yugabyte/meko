# Storage controller metrics

Source: `storage_controller/src/metrics.rs` (`StorageControllerMetricGroup` +
label-group types; `DatabaseOperation` from `storage_controller/src/persistence.rs`).
Exposed as Prometheus text (`text/plain; version=0.0.4`) on `GET /metrics`
(`measured_metrics_handler` in `storage_controller/src/http.rs`). The encoder
emits the `measured` group plus the shared `NeonMetrics` (build_info / process
metrics).

All metric names below are the exact field names in `StorageControllerMetricGroup`.
Histograms use exponential buckets (the `start`/`factor` noted per metric).

## Gauges

| Metric | Type | Labels | Meaning |
|---|---|---|---|
| `storage_controller_tenants` | Gauge | — | Size of the in-memory tenant map |
| `storage_controller_pageserver_nodes` | Gauge | — | Size of the in-memory pageserver-node map |
| `storage_controller_https_pageserver_nodes` | Gauge | — | Count of pageserver nodes with HTTPS configured |
| `storage_controller_safekeeper_nodes` | Gauge | — | Size of the in-memory safekeeper-node map |
| `storage_controller_https_safekeeper_nodes` | Gauge | — | Count of safekeeper nodes with HTTPS configured |
| `storage_controller_node_shards` | GaugeVec | `node_id` | Shard locations (attached + secondary) on each node |
| `storage_controller_node_attached_shards` | GaugeVec | `node_id` | Attached shard locations on each node |
| `storage_controller_ps_node_pool_rotation_pending` | Gauge | — | 1 if any PS pod has a pending node-pool-rotation annotation (manual reset) |
| `storage_controller_num_pageservers_desired` | Gauge | — | Desired pageserver count |
| `storage_controller_num_safekeeper_desired` | Gauge | — | Desired safekeeper count |

## HTTP request metrics (this controller's own API)

| Metric | Type | Labels | Meaning |
|---|---|---|---|
| `storage_controller_http_request_status` | CounterVec | `path`, `method`, `status` | Count of handled requests by route name, method, and HTTP status code |
| `storage_controller_http_request_latency` | HistogramVec (start 0.1, ×2.0) | `path`, `method` | Handler latency across all status codes |
| `storage_controller_http_request_rate_limited` | Histogram (start 0.1, ×10.0) | — | Time spent waiting on the per-tenant rate limiter |

`path` is the route's static `RequestName` (e.g. `control_v1_node`,
`v1_tenant`), not the raw URL — tenant/timeline ids are never in labels.
`method` is the fixed-cardinality enum `{Get,Put,Post,Delete,Other}`; `status`
is the numeric HTTP code.

## Pageserver / safekeeper request metrics (outbound)

Errors + latency for requests the controller makes *to* pageservers and
safekeepers. `path` is the request path with ids stripped
(`path_without_ids`); `method` is the `{Get,...}` enum.

| Metric | Type | Labels | Meaning |
|---|---|---|---|
| `storage_controller_pageserver_request_error` | CounterVec | `pageserver_id`, `path`, `method` | Errored outbound requests to a pageserver |
| `storage_controller_pageserver_request_latency` | HistogramVec (start 0.1, ×2.0) | `pageserver_id`, `path`, `method` | Outbound pageserver request latency (success + failure) |
| `storage_controller_safekeeper_request_error` | CounterVec | `safekeeper_id`, `path`, `method` | Errored outbound requests to a safekeeper |
| `storage_controller_safekeeper_request_latency` | HistogramVec (start 0.1, ×2.0) | `safekeeper_id`, `path`, `method` | Outbound safekeeper request latency (success + failure) |
| `storage_controller_passthrough_request_error` | CounterVec | `pageserver_id`, `path`, `method` | Errored `/v1/...` passthrough requests forwarded to a pageserver |
| `storage_controller_passthrough_request_latency` | HistogramVec (start 0.1, ×2.0) | `pageserver_id`, `path`, `method` | Latency of `/v1/...` passthrough requests to a pageserver |

## Database metrics

| Metric | Type | Labels | Meaning |
|---|---|---|---|
| `storage_controller_database_query_error` | CounterVec | `error_type`, `operation` | Failed DB queries by error class and operation |
| `storage_controller_database_query_latency` | HistogramVec (start 0.1, ×2.0) | `operation` | DB query latency by operation |

`error_type` is the fixed enum `{Query, Connection, ConnectionPool, Logical,
Migration}` (from `DatabaseError::error_label`). `operation` is the
`DatabaseOperation` enum — one variant per persistence call, including:
`InsertNode`, `UpdateNode`, `DeleteNode`, `ListNodes`, `ListTombstones`,
`Detach`, `ReAttach`, `IncrementGeneration`, `TenantGenerations`,
`ShardGenerations`, `ListTenantShards`, `LoadTenant`, `InsertTenantShards`,
`UpdateTenant`, `DeleteTenant`, `UpdateTenantConfig`, `UpdateMetadataHealth`,
`ListMetadataHealth`, `ListMetadataHealthUnhealthy`, `ListMetadataHealthOutdated`,
`ListSafekeepers`, `InsertTimeline`, `UpdateTimeline`, `GetTimeline`,
`InsertPendingTimelineInit`, `ListPendingTimelineInits`,
`DeletePendingTimelineInit`, `InsertTimelineImport`, `UpdateTimelineImport`,
`DeleteTimelineImport`, `ListTimelineImports`, `IsTenantImportingTimeline`,
`InsertEndpoint`, `ListEndpoints`, `DeleteEndpoint`.

## Storage-scrubber metrics

| Metric | Type | Labels | Meaning |
|---|---|---|---|
| `storage_controller_storage_scrub_status` | CounterVec | `tenant_id`, `shard_number`, `timeline_id`, `outcome` | Storage-scrubber results per timeline |

`outcome` is the fixed enum `{PSOk, PSWarning, PSError, PSOrphan, SKOk, SKError}`.

## yb-light notes

- **No reconcile / scheduler / leadership metrics.** The upstream
  `storage_controller_reconcile_*`, optimisation, and leadership/step-down gauges
  do not exist here — those subsystems were removed with the pageserver
  reconciler (see [`storage-controller.md`](storage-controller.md)). The
  `node_shards` / `*_desired` / `ps_node_pool_rotation_pending` gauges survive
  but reflect the launcher-managed single-tenant model.
- `NeonMetrics` (build_info + libmetrics process stats) is emitted alongside the
  group on every `/metrics` scrape.
