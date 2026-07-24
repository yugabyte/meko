# Safekeeper Prometheus Metrics

Source: `compute/safekeeper/src/metrics.rs` (definitions),
`compute/safekeeper/src/bin/yb-safekeeper.rs` (`TimelineCollector`
registration + the forced-collection ticker).

Metrics are exposed at `GET /metrics` (auth-exempt; see
[`safekeeper-apis.md`](safekeeper-apis.md)). Two registration paths feed it:

1. **Global `Lazy` metrics** — registered into the default registry on first
   touch via the `register_*!` macros. A `Lazy` static that no code path
   ever touches never registers and therefore never appears in the
   exposition (see [Defined but inert](#defined-but-inert)).
2. **Per-timeline collector** — `TimelineCollector` is a custom Prometheus
   `Collector` registered once at startup (`metrics::register_internal`).
   On every scrape it resets its gauges and re-emits one series per
   **active** timeline, so inactive branches drop out.

With `force_metric_collection_on_scrape` (default true), a background task
also runs the shared `METRICS_COLLECTOR` on a fixed interval so scrapes stay
cheap. The shared `metrics` crate additionally contributes the standard
build-info/process series (`set_build_info_metric(GIT_VERSION, BUILD_TAG)`).

**Per-timeline label cardinality**: every metric in the *Per-timeline*
sections carries a single `branch_id` label. One safekeeper process serves
one branch, so this is one series per metric per process.

## WAL write path (global)

| Metric | Type | Labels | Meaning |
| --- | --- | --- | --- |
| `safekeeper_write_wal_bytes` | Histogram | — | Bytes accepted per `write_wal` call (observed by `WalBookkeeping`). |
| `safekeeper_proposer_acceptor_messages_total` | IntCounterVec | `outcome` (`success` \| `error`) | ProposerAcceptorMessages processed by the acceptor. |
| `safekeeper_wal_storage_operation_seconds` | HistogramVec | `operation` | WAL-storage operation timings. Observed operations: `initialize_first_segment`, `truncate_wal`. |
| `safekeeper_misc_operation_seconds` | HistogramVec | `operation` | Miscellaneous timings. Observed operations: `handle_elected`, `state_snapshot`, `wal_residence_guard`, `try_wal_residence_guard`. |
| `safekeeper_persist_control_file_seconds` | Histogram | — | Latency of persisting the control file: full-row `UPDATE`s (`Storage::persist`) and the advisory `remote_consistent_lsn` UPDATE (`AdvisoryPersist`), both against `safekeeper.timeline_state`. |
| `safekeeper_wal_storage_limit_errors` | IntCounter | — | Appends rejected because the timeline's retained-WAL estimate exceeds `max_timeline_disk_usage_bytes`. |

## YB WAL mirror: writes into `safekeeper.wals` (global)

Emitted by the consumer task's `flush_batch` (`yb_wal_storage.rs`).

| Metric | Type | Labels | Meaning |
| --- | --- | --- | --- |
| `safekeeper_yb_wal_insert_seconds` | HistogramVec | `path` (`ysql` \| `tsclient`) | Latency of the batched write into `safekeeper.wals`: safekeeper→YB hop + DocDB write + Raft commit. `ysql` = multi-row INSERT over the Postgres wire; `tsclient` = pure-Rust pggate RPC upsert batch (`hot_path = "tsclient"`). |
| `safekeeper_yb_wal_batch_rows` | Histogram | — | Coalesced rows per write (~1 = sequential WAL coalescing into single rows or no batching; near 50 = `FLUSH_ROWS_THRESHOLD` saturating). |
| `safekeeper_yb_wal_batch_bytes` | Histogram | — | Payload bytes per write (pre-coalescing chunk bytes). |
| `safekeeper_yb_wal_session_recoveries_total` | IntCounter | — | `yb_tsclient` session recoveries (reconnect + fresh tserver session + table re-validation) on the thin-RPC lane. Occasional after tserver restarts; a sustained rate means the lane's endpoint is flapping. |
| `safekeeper_yb_wal_call_timeouts_total` | IntCounter | — | `yb_tsclient` per-attempt call timeouts (transport healthy, no response within `rpc_timeout`) on the thin-RPC lane. Retried internally, so callers only see terminal failures — this is the early signal of a server-side stall. |
| `safekeeper_yb_wal_write_backpressure_total` | IntCounter | — | `yb_tsclient` calls shed because a connection's write budget was exhausted (a stalled tserver blocked the writer; the per-connection frame backlog hit its byte cap). The safekeeper WAL-write lane is the most write-heavy tsclient lane, so a sustained rate here is the direct signal the YB write path can't drain. Mirrors `pageserver_yb_store_write_backpressure_total`. |

## WAL cache / YB reads (global)

The `WalCache` fronts `safekeeper.wals` on the read path; misses fall
through to YB via `YbSegmentReader`.

| Metric | Type | Labels | Meaning |
| --- | --- | --- | --- |
| `safekeeper_wal_cache_hits_total` | IntCounter | — | Lookups returning a chunk covering the requested byte. |
| `safekeeper_wal_cache_misses_total` | IntCounter | — | Lookups that fell through to YB. |
| `safekeeper_wal_cache_evictions_total` | IntCounter | — | Chunks evicted by the FIFO over-capacity policy. |
| `safekeeper_wal_cache_bytes` | IntGauge | — | Current cache occupancy in bytes (sum across cache instances). |
| `safekeeper_wal_cache_yb_fetch_seconds` | HistogramVec | `path` (`ysql` \| `tsclient`) | Latency per `safekeeper.wals` fetch round trip (covering chunk + forward prefetch window). |
| `safekeeper_wal_cache_yb_fetch_chunks` | Histogram | — | Chunks returned per fetch (pegged at 1 = no prefetch amortization; >1 = the range fetch is amortizing round trips). |
| `safekeeper_wal_cache_yb_fetch_bytes` | Histogram | — | Bytes returned per fetch. |

## WAL sender / decode health (global)

| Metric | Type | Labels | Meaning |
| --- | --- | --- | --- |
| `safekeeper_wal_readers` | IntGaugeVec | `kind` (`task` \| `future`), `target` (client `appname`, else `safekeeper`) | Active interpreted-WAL reader instances (`task` = spawned fan-out reader, `future` = reader driven inline by one walsender). |
| `safekeeper_wal_reader_decode_errors_total` | IntCounter | — | WAL decode failures in the interpreted reader. Treated as recoverable: the reader exits, the client reconnects and re-reads from YB at its last applied record boundary. A **sustained** rate indicates genuine corruption rather than a transient torn read across a rewrite window (#139). |
| `safekeeper_wal_reader_decode_tripwire_total` | IntCounterVec | `verdict` (`transient` \| `durable` \| `inconclusive`) | Verdict of the decode-error tripwire: on a decode failure the reader re-reads the failing range on a fresh YB snapshot and re-decodes. `transient` = the re-read decoded cleanly (read-time splice, #139); `durable` = it failed again (genuine WAL corruption — page!); `inconclusive` = could not re-read/decide. |
| `safekeeper_received_ps_feedbacks_total` | IntCounter | — | Pageserver feedback messages received on replication connections. |

## WAL receivers / connections (global)

| Metric | Type | Labels | Meaning |
| --- | --- | --- | --- |
| `safekeeper_wal_receivers` | IntGauge | — | Currently connected WAL receivers (compute connections). |
| `safekeeper_wal_receiver_queue_depth` | Histogram | — | Queued messages per WAL receiver, sampled every 5 s by the acceptor loop. |
| `safekeeper_wal_receiver_queue_depth_total` | IntGauge | — | Total queued messages across all WAL receivers. |
| `safekeeper_wal_receiver_queue_size_total` | IntGauge | — | Total byte size of queued messages across all WAL receivers. |
| `safekeeper_pg_io_bytes_total` | IntCounterVec | `app_name`, `dir` (`read` \| `write`) | Bytes read/written on PostgreSQL-protocol connections, attributed by application name. |
| `safekeeper_pg_queries_received_total` / `safekeeper_pg_queries_finished_total` | IntCounterPairVec | `query` (`START_WAL_PUSH`, `START_REPLICATION`, `IDENTIFY_SYSTEM`, `TIMELINE_STATUS`) | Replication-port commands received vs finished. |
| `safekeeper_received_ps_feedbacks_total` | IntCounter | — | Pageserver feedbacks received. |

## Manager / disk utilization (global)

| Metric | Type | Labels | Meaning |
| --- | --- | --- | --- |
| `safekeeper_manager_iterations_total` | IntCounter | — | Iterations of the per-timeline manager loop. |
| `safekeeper_manager_active_changes_total` | IntCounter | — | Timeline active-status flips in the manager. |
| `safekeeper_global_disk_utilization_check_seconds` | Histogram | — | Duration of each background filesystem utilization check of the workdir. |

## Per-timeline state (`TimelineCollector`)

All carry a single `branch_id` label; emitted once per active timeline and
reset each scrape.

| Metric | Type | Meaning |
| --- | --- | --- |
| `safekeeper_commit_lsn` | GaugeVec | Commit LSN. Set to the same value as `safekeeper_flush_lsn`: with a single safekeeper per branch, every YB-durable flush is committed. |
| `safekeeper_flush_lsn` | GaugeVec | Durable end-of-WAL (capped at what has landed in `safekeeper.wals`). |
| `safekeeper_remote_consistent_lsn` | GaugeVec | In-memory pageserver durable-ingest frontier (from feedback). |
| `safekeeper_ps_last_received_lsn` | GaugeVec | Last LSN the pageserver acknowledged in feedback. |
| `safekeeper_feedback_last_time_seconds` | GaugeVec | Unix timestamp of the last pageserver feedback. |
| `safekeeper_ps_feedback_count_total` | GaugeVec | Pageserver feedbacks received for this timeline. |
| `safekeeper_ps_corruption_detected` | IntGaugeVec | 1 if any pageserver feedback reported corruption, else 0. Latched for the process lifetime. |
| `safekeeper_timeline_active` | GaugeVec | 1 for active timelines (live computes, or pageserver behind `flush_lsn`), 0 otherwise. |
| `safekeeper_connected_computes` | IntGaugeVec | Active compute connections. |
| `safekeeper_disk_usage_bytes` | GaugeVec | Retained-WAL estimate: `(flush segno − last removed segno) × wal_seg_size`. Emitted only once GC has removed at least one segment. Measures the WAL span retained in `safekeeper.wals`, not local disk (the safekeeper stores no WAL locally). |
| `safekeeper_written_wal_bytes_total` | GaugeVec | Cumulative WAL bytes accepted by `write_wal` for this timeline. |
| `safekeeper_written_wal_seconds_total` | GaugeVec | Always 0 — the per-timeline accumulator exists but no code path adds to it (WAL writes are an in-memory enqueue; the YB write cost is on `safekeeper_yb_wal_insert_seconds`). |
| `safekeeper_flushed_wal_seconds_total` | GaugeVec | Always 0 — same as above; there is no local fsync to time. |
| `safekeeper_interpreted_wal_reader_tasks` | GaugeVec | Distinct interpreted-WAL reader instances serving this timeline's walsenders. |

## Collector-level (`TimelineCollector`, unlabeled)

| Metric | Type | Meaning |
| --- | --- | --- |
| `safekeeper_collect_timeline_metrics_seconds` | Gauge | Time to collect per-timeline metrics on the last scrape (includes taking the timeline locks). |
| `safekeeper_timelines` | IntGauge | Timelines loaded in memory (0 or 1 per process). |
| `safekeeper_active_timelines` | IntGauge | Active timelines (0 or 1 per process). |

## Defined but inert

These `Lazy` statics exist in `compute/safekeeper/src/metrics.rs` but no
code path observes them, so they never register and never appear on
`/metrics`: `safekeeper_write_wal_seconds`, `safekeeper_flush_wal_seconds`
(there is no local disk write/fsync to time), `safekeeper_wal_disk_io_errors`
(WAL I/O errors surface through the durability watermark and connection
errors instead), `safekeeper_removed_wal_segments_total` (GC is a ranged SQL
DELETE, not per-segment file removal), the four `safekeeper_broker_*`
metrics (the process runs no broker client), and the two
`safekeeper_eviction_events_*` counters (timelines are never evicted to a
backup tier — WAL is always resident in YB).
