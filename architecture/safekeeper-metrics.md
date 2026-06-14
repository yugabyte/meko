# Safekeeper Prometheus Metrics

Source: `safekeeper/src/metrics.rs` (all definitions),
`safekeeper/src/bin/safekeeper.rs` (`TimelineCollector` registration).

Metrics are exposed at `GET /metrics` (see [`safekeeper-apis.md`](safekeeper-apis.md)),
which is auth-exempt. Two registration paths feed it:

1. **Global `Lazy` metrics** — registered into the default registry on first
   touch via `register_*!` macros.
2. **Per-timeline collector** — `TimelineCollector` is a custom Prometheus
   `Collector` registered once at startup (`register_internal`). On every
   scrape it locks the timeline map, resets its gauges, and re-emits one
   series per **active** timeline.

**Per-timeline label cardinality**: every metric in the *Per-timeline state*
section carries `tenant_id` + `timeline_id` labels, so series count scales
with the number of active timelines. The collector `reset()`s these each
scrape, so inactive timelines drop out. In yb-light this is typically a single
timeline (single-SK), keeping cardinality low.

## WAL storage / write path (global)

| Metric | Type | Labels | Meaning |
| --- | --- | --- | --- |
| `safekeeper_write_wal_bytes` | Histogram | — | Bytes written to WAL in a single request. |
| `safekeeper_write_wal_seconds` | Histogram | — | Seconds writing+syncing WAL to disk per request. |
| `safekeeper_flush_wal_seconds` | Histogram | — | Seconds syncing WAL to disk (excludes segment init). |
| `safekeeper_persist_control_file_seconds` | Histogram | — | Seconds to persist+sync the control file. |
| `safekeeper_wal_storage_operation_seconds` | HistogramVec | `operation` | Seconds per WAL-storage operation. |
| `safekeeper_misc_operation_seconds` | HistogramVec | `operation` | Seconds per miscellaneous operation. |
| `safekeeper_wal_disk_io_errors` | IntCounter | — | Disk I/O errors creating/flushing WAL and control files. |
| `safekeeper_wal_storage_limit_errors` | IntCounter | — | Errors from WAL storage exceeding the configured limit (backup/removal lag). |
| `safekeeper_proposer_acceptor_messages_total` | IntCounterVec | `outcome` | Total ProposerAcceptorMessage requests received. |

## WAL cache / YB mirror (global)

The WalCache fronts `yb_storage.wals`; misses fall through to YB. These are
yb-light-specific.

| Metric | Type | Labels | Meaning |
| --- | --- | --- | --- |
| `safekeeper_wal_cache_hits_total` | IntCounter | — | WalCache lookups returning a chunk covering the requested byte. |
| `safekeeper_wal_cache_misses_total` | IntCounter | — | WalCache lookups that fell through to YB. |
| `safekeeper_wal_cache_evictions_total` | IntCounter | — | Chunks evicted by the FIFO over-capacity policy. |
| `safekeeper_wal_cache_bytes` | IntGauge | — | Current WalCache occupancy in bytes (sum across instances). |
| `safekeeper_wal_cache_yb_fetch_seconds` | Histogram | — | Latency of `YbSegmentReader` fetches that fell through to `yb_storage.wals`. |
| `safekeeper_wal_cache_yb_fetch_chunks` | Histogram | — | Chunks returned per `yb_storage.wals` fetch (>1 = range-fetch amortizing round trips). |
| `safekeeper_wal_cache_yb_fetch_bytes` | Histogram | — | Bytes returned per `yb_storage.wals` fetch. |

## WAL senders / receivers / connections (global)

| Metric | Type | Labels | Meaning |
| --- | --- | --- | --- |
| `safekeeper_wal_receivers` | IntGauge | — | Currently connected WAL receivers (connected computes). |
| `safekeeper_wal_readers` | IntGaugeVec | `kind`, `target` | Active WAL readers (serving pageservers or other safekeepers). |
| `safekeeper_wal_receiver_queue_depth` | Histogram | — | Queued messages per WAL receiver (sampled every 5s). |
| `safekeeper_wal_receiver_queue_depth_total` | IntGauge | — | Total queued messages across all WAL receivers. |
| `safekeeper_wal_receiver_queue_size_total` | IntGauge | — | Total byte size of queued messages across all WAL receivers. |
| `safekeeper_pg_io_bytes_total` | IntCounterVec | `client_az`, `sk_az`, `app_name`, `dir`, `same_az` | Bytes read/written over any PostgreSQL connection. |
| `safekeeper_pg_queries_received_total` / `safekeeper_pg_queries_finished_total` | IntCounterPairVec | `query` | Queries received vs finished over the pg protocol. |
| `safekeeper_received_ps_feedbacks_total` | IntCounter | — | Pageserver feedbacks received. |

## Removal / eviction / GC (global)

| Metric | Type | Labels | Meaning |
| --- | --- | --- | --- |
| `safekeeper_removed_wal_segments_total` | IntCounter | — | WAL segments removed from disk. |
| `safekeeper_eviction_events_started_total` | IntCounterVec | `kind` | Eviction state changes counted at start (`evict`/`restore`). |
| `safekeeper_eviction_events_completed_total` | IntCounterVec | `kind` | Eviction state changes counted at completion. |

## Broker / manager / disk-util (global)

| Metric | Type | Labels | Meaning |
| --- | --- | --- | --- |
| `safekeeper_broker_pushed_updates_total` | IntCounter | — | Timeline updates pushed to the broker. |
| `safekeeper_broker_pulled_updates_total` | IntCounterVec | `result` | Timeline updates pulled+processed from the broker. |
| `safekeeper_broker_push_update_seconds` | Histogram | — | Seconds to push all timeline updates to the broker. |
| `safekeeper_broker_iteration_timelines` | Histogram | — | Timelines pushed to the broker per iteration. |
| `safekeeper_manager_iterations_total` | IntCounter | — | Iterations of the timeline-manager task. |
| `safekeeper_manager_active_changes_total` | IntCounter | — | Timeline active-status changes in the manager task. |
| `safekeeper_global_disk_utilization_check_seconds` | Histogram | — | Seconds per filesystem utilization check. |

## Per-timeline state (`TimelineCollector`)

All carry labels `tenant_id`, `timeline_id`; emitted once per active timeline
and reset each scrape.

| Metric | Type | Meaning |
| --- | --- | --- |
| `safekeeper_commit_lsn` | GaugeVec | Current commit_lsn. **yb-mode: ≡ `flush_lsn`** (single-SK quorum). |
| `safekeeper_flush_lsn` | GaugeVec | Current flush_lsn. |
| `safekeeper_epoch_start_lsn` | GaugeVec | LSN since which compute generates new WAL in the current term. |
| `safekeeper_remote_consistent_lsn` | GaugeVec | LSN persisted to remote storage in the pageserver. |
| `safekeeper_ps_last_received_lsn` | GaugeVec | Last LSN received by the pageserver (per feedback). |
| `safekeeper_feedback_last_time_seconds` | GaugeVec | Unix timestamp of last pageserver feedback. |
| `safekeeper_ps_feedback_count_total` | GaugeVec | Number of pageserver feedbacks received. |
| `safekeeper_ps_corruption_detected` | IntGaugeVec | 1 if pageserver feedback reports corruption, else 0. |
| `safekeeper_timeline_active` | GaugeVec | 1 for active timelines, 0 for inactive. |
| `safekeeper_connected_computes` | IntGaugeVec | Active compute connections. |
| `safekeeper_disk_usage_bytes` | GaugeVec | Estimated disk space used by WAL segments. |
| `safekeeper_acceptor_term` | GaugeVec | Current consensus term. |
| `safekeeper_written_wal_bytes_total` | GaugeVec | WAL bytes written to disk. |
| `safekeeper_written_wal_seconds_total` | GaugeVec | Total time in `write(2)` writing WAL. |
| `safekeeper_flushed_wal_seconds_total` | GaugeVec | Total time in `fsync(2)` flushing WAL. |
| `safekeeper_interpreted_wal_reader_tasks` | GaugeVec | Active interpreted-WAL reader tasks. |

## Collector-level (`TimelineCollector`, unlabeled)

| Metric | Type | Meaning |
| --- | --- | --- |
| `safekeeper_collect_timeline_metrics_seconds` | Gauge | Time to collect timeline metrics (incl. acquiring the timelines lock). |
| `safekeeper_timelines` | IntGauge | Total timelines loaded in-memory. |
| `safekeeper_active_timelines` | IntGauge | Total active timelines. |
