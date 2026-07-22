# yb-pageserver Prometheus metrics

Reference for the Prometheus metrics that `yb-pageserver` exposes. Most metric
definitions live in [`compute/pageserver/src/metrics.rs`](../pageserver/src/metrics.rs);
the process/allocator/runtime collectors are registered in
[`bin/yb-pageserver.rs`](../pageserver/src/bin/yb-pageserver.rs) via
`metrics::register_internal`. See also the component overview in
[`pageserver.md`](./pageserver.md), the HTTP surface in
[`pageserver-apis.md`](./pageserver-apis.md), and the storage back end in
[`yugabyte.md`](./yugabyte.md).

## How metrics are served

Metrics are exposed as Prometheus text exposition on the HTTP management port
under `GET /metrics` (route wired in
[`compute/pageserver/src/http/routes.rs`](../pageserver/src/http/routes.rs)).
The handler gathers the default Prometheus registry through `METRICS_COLLECTOR`.

- A background task collects metrics on an interval
  (`METRICS_COLLECTION_INTERVAL`) so each scrape returns the last collected
  snapshot. When the `force_metric_collection_on_scrape` config flag is set,
  collection runs inline on every scrape instead.
- `preinitialize_metrics` force-instantiates the instance-wide (non-per-branch)
  metrics at startup so they are present in the exposition before any traffic.
- Collectors registered with `register_internal` at startup:
  `more_process_metrics::Collector` (Linux only), the tokio-epoll-uring
  `Collector`, and the jemalloc `JemallocCollector`. `set_build_info_metric`
  and `set_launch_timestamp_metric` register the two `libmetrics_*` info gauges.

## Label cardinality

Metrics labelled `branch_id` are per-branch. A `yb-pageserver` process serves
exactly one `branch_id`, so these series are effectively single-series per
process (the label is still present so series aggregate cleanly across
processes). Per-branch series are dropped when a branch shuts down
(`TimelineMetrics::shutdown`). Metric names ending in `_global` carry no
`branch_id` dimension and aggregate at instance level.

## getpage / page_service (compute read hot path)

| Metric | Type | Labels | Meaning |
| --- | --- | --- | --- |
| `pageserver_smgr_query_started_global_count` | IntCounterVec | `smgr_query_type` | smgr queries started, by query type (`get_rel_exists`, `get_rel_size`, `get_page_at_lsn`, `get_db_size`, `get_slru_segment`). |
| `pageserver_smgr_query_started_count` | IntCounterVec | `smgr_query_type`, `branch_id` | smgr queries started, per branch. Only the `get_page_at_lsn` series is populated. |
| `pageserver_smgr_query_seconds` | HistogramVec | `smgr_query_type`, `branch_id` | Execution time of smgr query handling, excluding batch and throttle delays. Per branch, only `get_page_at_lsn` is tracked. |
| `pageserver_smgr_query_seconds_global` | HistogramVec | `smgr_query_type` | Instance-wide execution time, all query types. |
| `pageserver_page_service_batch_size_global` | Histogram | — | getpage request batch sizes. |
| `pageserver_page_service_batch_size` | HistogramVec | `branch_id` | getpage request batch sizes, per branch. |
| `pageserver_page_service_batch_break_reason_global` | IntCounterVec | `reason` | Why a getpage batch was broken (`batch_full`, `non_batchable_request`, `non_uniform_lsn`, `same_page_at_different_lsn`, `non_uniform_timeline`, `executor_steal`). |
| `pageserver_page_service_batch_break_reason` | IntCounterVec | `branch_id`, `reason` | Batch-break reason, per branch. |
| `pageserver_page_service_config_max_batch_size` | IntGaugeVec | `mode`, `execution`, `batching` | Configured max batch size and pipelining configuration, exposed as the gauge value plus config labels. |
| `pageserver_page_service_pipeline_inflight_batches` | Histogram | — | Batches in flight per connection in the pipelined executor, sampled when a new batch is admitted (each batch is one folded getpage set = one `get_vectored` / YB read). |
| `pageserver_scattered_set_keys_global` | Histogram | — | Scattered single-block keys folded into one multi-key `scan_scattered_set` YB query (batched-reads fold size). Only folds of >=2 keys are recorded; higher values mean more per-query batching and fewer YB round trips. |
| `pageserver_page_service_pagestream_batch_wait_time_seconds` | HistogramVec | `branch_id` | Time a request waited in its batch before the batch moved to throttle and execution. |
| `pageserver_page_service_pagestream_batch_wait_time_seconds_global` | Histogram | — | Instance-wide batch wait time. |
| `pageserver_page_service_pagestream_flush_in_progress_micros` | IntCounterVec | `branch_id` | Microseconds a pagestream response spent flushing into the TCP connection (in-progress sum, updated periodically during slow flushes — not a completion-latency histogram). |
| `pageserver_page_service_pagestream_flush_in_progress_micros_global` | IntCounter | — | Instance-wide flush-in-progress micros. |
| `pageserver_get_vectored_seconds` | HistogramVec | `task_kind` | Time spent in `get_vectored`. Only `PageRequestHandler` is tracked. |
| `pageserver_scan_seconds` | HistogramVec | `task_kind` | Time spent in `scan`. Only `PageRequestHandler` is tracked. |
| `pageserver_wait_lsn_seconds` | Histogram | — | Time spent waiting for WAL to arrive, recorded on completion of the wait. |
| `pageserver_wait_lsn_started_count` / `pageserver_wait_lsn_finished_count` | IntCounterPairVec | `branch_id` | wait_lsn operations started / finished. |
| `pageserver_wait_lsn_in_progress_micros` | IntCounterVec | `branch_id` | wait_lsn microseconds, per branch, updated periodically while waiting. |
| `pageserver_wait_lsn_in_progress_micros_global` | IntCounter | — | wait_lsn in-progress micros, instance-wide. |
| `pageserver_layers_per_read` | HistogramVec | `branch_id` | Layers visited to serve a single read (read amplification). In a batch, all visited layers count towards every read. |
| `pageserver_layers_per_read_global` | Histogram | — | Layers per read, instance-wide. |
| `pageserver_layers_per_read_batch_global` | Histogram | — | Layers visited per read batch, regardless of the number of reads. |
| `pageserver_layers_per_read_amortized_global` | Histogram | — | Layers per read amortized across the batch (visited layers divided by number of reads). |
| `pageserver_deltas_per_read_global` | Histogram | — | Delta pages applied onto the image page per read. |
| `pageserver_pagestream_handler_results_total` | IntCounterVec | `outcome` | Pagestream handler results by `outcome`: `success`, `internal_error` (bugs in the storage cluster), `other_error` (transient/expected conditions). |
| `pageserver_misrouted_pagestream_requests_total` | IntCounter | — | Pagestream requests routed to the wrong pageserver (tenant not found, or not the primary). Persistently high rates indicate compute misconfiguration or reconciliation bugs. |
| `pageserver_relsize_latest_cache_entries` | UIntGauge | — | Entries in the latest relation-size cache. |
| `pageserver_relsize_latest_cache_hits` / `pageserver_relsize_latest_cache_misses` | IntCounter | — | Latest relation-size cache hits / misses. |
| `pageserver_relsize_snapshot_cache_entries` | UIntGauge | — | Entries in the PITR (snapshot) relation-size cache. |
| `pageserver_relsize_snapshot_cache_hits` / `pageserver_relsize_snapshot_cache_misses` | IntCounter | — | Snapshot relation-size cache hits / misses. |
| `pageserver_relsize_cache_misses_old` | IntCounter | — | Relation-size cache misses where the lookup LSN predates the last relation update. |

## Tenant throttling (pagestream)

The `kind` label is always `pagestream`. `_global` variants carry only `kind`;
per-branch variants add `branch_id`.

| Metric | Type | Labels | Meaning |
| --- | --- | --- | --- |
| `pageserver_tenant_throttling_count_accounted_start_global` / `pageserver_tenant_throttling_count_accounted_start` | IntCounterVec | `kind` (+ `branch_id`) | Throttle accounting starts. |
| `pageserver_tenant_throttling_count_accounted_finish_global` / `pageserver_tenant_throttling_count_accounted_finish` | IntCounterVec | `kind` (+ `branch_id`) | Throttle accounting finishes. |
| `pageserver_tenant_throttling_wait_usecs_sum_global` / `pageserver_tenant_throttling_wait_usecs_sum` | IntCounterVec | `kind` (+ `branch_id`) | Microseconds spent waiting on the throttle. |
| `pageserver_tenant_throttling_count_global` / `pageserver_tenant_throttling_count` | IntCounterVec | `kind` (+ `branch_id`) | Number of throttlings. |

## WAL ingest / walreceiver

| Metric | Type | Labels | Meaning |
| --- | --- | --- | --- |
| `pageserver_wal_ingest_bytes_received` | IntCounter | — | Bytes of WAL ingested from safekeepers. |
| `pageserver_wal_ingest_records_received` | IntCounter | — | WAL records received from safekeepers. |
| `pageserver_wal_ingest_records_observed` | IntCounter | — | Metadata-only records observed (shard 0). |
| `pageserver_wal_ingest_records_committed` | IntCounter | — | WAL records that resulted in writes to storage. |
| `pageserver_wal_ingest_values_committed` | IntCounterVec | `class`, `kind` | Values committed to storage. `class` ∈ `metadata`, `data`; `kind` ∈ `image`, `delta`. |
| `pageserver_gap_blocks_zeroed_on_rel_extend` | IntCounter | — | Zero gap blocks written on relation extends. |
| `pageserver_timeline_wal_records_received` | IntCounterVec | `branch_id` | WAL records received, per branch. |
| `pageserver_walreceiver_started_connections_total` | IntCounter | — | walreceiver connections started. |
| `pageserver_walreceiver_active_managers` | IntGauge | — | Active walreceiver managers. |
| `pageserver_walreceiver_switches_total` | IntCounterVec | `reason` | walreceiver `change_connection` calls, by reason. |
| `pageserver_walreceiver_broker_updates_total` | IntCounter | — | Broker updates received in walreceiver. |
| `pageserver_walreceiver_candidates_events_total` | IntCounterVec | `event` | walreceiver candidate events (`add` / `remove`). |

## Page store / YB read-write path

| Metric | Type | Labels | Meaning |
| --- | --- | --- | --- |
| `pageserver_yb_pagewriter_backpressure_waits_total` | IntCounterVec | `kind` | `PageWriter::put` calls that awaited downstream drain. `flush` = the in-RAM buffer hit `FlushPolicy::backpressure_bytes`; `materializer` = the image-materialiser channel was full. Sustained non-zero rate means PageWriter is the slow side. |
| `pageserver_yb_pagewriter_unflushed_bytes` | UIntGauge | — | Bytes in PageWriter awaiting INSERT into `pageserver.pages` / `slru_pages` / `meta`. Spikes well above the size-based flush trigger indicate YB is the bottleneck. |
| `pageserver_yb_pagewriter_delta_counts_entries` | UIntGauge | — | Entries in PageWriter's per-key `delta_counts` map. Sustained growth above the working-set ceiling means the materialiser is not keeping up with chain accumulation. |
| `pageserver_yb_pagewriter_delta_counts_approx_bytes` | UIntGauge | — | Approximate bytes held by `delta_counts` (entries × 80). Use against process RSS to attribute memory. |
| `pageserver_yb_materializer_queue_depth` | UIntGauge | — | Image-materialiser jobs currently queued. Approaching `materialize_queue_capacity` means walreceiver is about to backpressure on `materializer`. |
| `pageserver_yb_materializer_inflight_bytes` | UIntGauge | — | Bytes held by an in-progress materialiser batch (chain payloads + freshly redone images). Watch this for RSS spikes not explained by unflushed bytes. |
| `pageserver_yb_materializer_read_side_dispatches_total` | IntCounter | — | Materialise jobs enqueued by the read path because the returned chain exceeded `count_limit`. A non-zero rate is the read side closing a gap the write-side trigger missed; sustained high rate suggests the write trigger needs tuning. |
| `pageserver_yb_page_reads_total` | IntCounterVec | `source`, `path` | Page-chain reads. `source` = `getpage` (`read_chain`) or `materializer` (`read_chain_durable`); `path` = `fast_cache` (cache held a complete chain, no YB round trip) or `yb_blend` (fell through to a YB read). `{source=*, path=yb_blend}` is the count of reads that hit YB; the `fast_cache`/`yb_blend` ratio is the per-source cache hit rate. |
| `pageserver_yb_store_op_seconds` | HistogramVec | `op`, `path` | Latency of every PageStore round trip to YB. `op` ∈ `read_chain`, `read_slru_chain`, `read_meta`, `scan_range`, `scan_slru_range`, `put_pages`, `put_slru_pages`; `path` ∈ `ysql` (Postgres wire) or `tsclient` (pure-Rust pggate RPC). This `path` label is unrelated to the cache-blend `path` of `pageserver_yb_page_reads_total`. |
| `pageserver_yb_store_session_recoveries_total` | IntCounter | — | `yb_tsclient` session recoveries (reconnect + fresh tserver session + table re-validation) on the thin-RPC lane. Occasional after tserver restarts; a sustained rate means the lane's endpoint is flapping. |
| `pageserver_yb_store_call_timeouts_total` | IntCounter | — | `yb_tsclient` per-attempt call timeouts (transport healthy, no response within `rpc_timeout`) on the thin-RPC lane. Retried internally, so callers see only terminal failures — the early signal of a server-side stall. |
| `pageserver_yb_unified_pages_by_key_entries` | UIntGauge | — | Entries in `UnifiedPages.by_key`, the per-key index over the write buffer and durable cache. Comparing against `delta_counts_entries` shows whether unflushed traffic or the durable cache drives working-set cardinality. |
| `pageserver_yb_correctness_errors_total` | IntCounterVec | `kind` | Correctness-fatal events observed on the hot path. **Any non-zero value is a corruption signal** and is meant to fire paging alerts. Kinds are listed below. |

`pageserver_yb_correctness_errors_total` `kind` values:

| `kind` | Meaning |
| --- | --- |
| `materializer_redo` | `redo_one` returned an error (chain deserialise, walredo panic, or INSERT failure). |
| `materializer_setup` | Materialiser setup (`redo_phase`, typically a YB read of a chain) failed before any per-job work ran. |
| `materializer_commit` | Materialiser `commit_phase` `put_pages` failed. |
| `materialize_unanchored_chain` | The merged cache+YB chain up to the target LSN has no `will_init` anchor image; the materialiser refuses rather than fold deltas onto a zero page and make the loss durable. |
| `read_unanchored_chain` | A read found a chain with no anchoring image (init record missing), so the read is unservable. |
| `chain_dedup_dropped_delta` | Chain dedup found two rows at the same LSN whose content diverges, dropping a genuine delta — a record-loss signature (byte-identical duplicates are benign and not flagged). |
| `durable_walk_unflushed_row` | The durable-chain walk saw a not-yet-flushed row at or below the target LSN. Benign during attach replay; a sustained rate outside attach means a flush-horizon violation. |
| `pagewriter_flush` | `PageWriter::flush` returned an error on a periodic or shutdown tick; rows may not have reached YB. |
| `write_path_roundtrip` | A WAL record failed to deserialise its own freshly-serialised bytes (round-trip self-check on the write path). |
| `snapshot_fifo_orphan` | Debug-build diagnostic only: the snapshot FIFO referenced a slot that is gone or already durable. |
| `walreceiver_worker_panic` | A walreceiver worker task ended via a panic. |

## Garbage collection

GC metrics live with the GC docs — see
[`pageserver-gc.md` §Metrics](pageserver-gc.md#metrics).

## walredo

| Metric | Type | Labels | Meaning |
| --- | --- | --- | --- |
| `pageserver_wal_redo_seconds` | Histogram | — | Time spent in WAL redo. |
| `pageserver_wal_redo_records_histogram` | Histogram | — | Records replayed per redo in the Postgres WAL redo process. |
| `pageserver_wal_redo_bytes_histogram` | Histogram | — | Bytes replayed per redo sent to Postgres. |
| `pageserver_replayed_wal_records_total` | IntCounter | — | Total WAL records replayed in the redo process. |
| `pageserver_wal_redo_process_launch_duration` | Histogram | — | Duration of successful `WalRedoProcess::launch` calls. |
| `pageserver_wal_redo_process_started_total` | IntCounter | — | WAL redo processes started. |
| `pageserver_wal_redo_process_stopped_total` | IntCounterVec | `cause` | WAL redo processes stopped, by kill cause. |
| `pageserver_walredo_stderr_logger_tasks_started_total` / `pageserver_walredo_stderr_logger_tasks_finished_total` | IntCounter | — | walredo stderr-logger tasks started / finished. |

## basebackup / compute commands

| Metric | Type | Labels | Meaning |
| --- | --- | --- | --- |
| `pageserver_basebackup_query_seconds` | HistogramVec | `result` | basebackup query durations, by `result` (`ok`, `error`, `client_error`). |
| `pageserver_compute_commands` | IntCounterVec | `command` | compute→pageserver commands processed. `command` ∈ `PageStreamV2`, `PageStreamV3`, `Basebackup`, `Fullbackup`, `LeaseLsn`. |
| `pageserver_basebackup_cache_read_total` | IntCounterVec | `result` | basebackup cache reads, by `result` (`hit` / `miss` / `error`). |
| `pageserver_basebackup_cache_prepare_total` | IntCounterVec | `result` | basebackup cache prepare requests, by `result` (`ok` / `skip` / `error`). |
| `pageserver_basebackup_cache_entries_total` | UIntGauge | — | Entries in the basebackup cache. |
| `pageserver_basebackup_cache_size_bytes` | UIntGauge | — | On-disk size of all basebackup cache entries. |
| `pageserver_basebackup_cache_prepare_queue_size` | UIntGauge | — | Pending requests in the basebackup prepare channel. |

## Per-branch storage / size

| Metric | Type | Labels | Meaning |
| --- | --- | --- | --- |
| `pageserver_last_record_lsn` | IntGaugeVec | `branch_id` | Last record (applied) LSN. |
| `pageserver_current_logical_size` | UIntGaugeVec | `branch_id` | Current logical size. |
| `pageserver_resident_physical_size` | UIntGaugeVec | `branch_id` | Bytes of layer files present on the local filesystem for attached locations. Typically 0 (no local layer files). |
| `pageserver_resident_physical_size_global` | UIntGauge | — | Resident physical size, instance-wide. |
| `pageserver_visible_physical_size` | UIntGaugeVec | `branch_id` | Bytes of layer files present on the local filesystem. |
| `pageserver_pitr_history_size` | UIntGaugeVec | `branch_id` | Data written since the PITR cutoff on this branch. |
| `pageserver_archive_size` | UIntGaugeVec | `branch_id` | Logical size if the branch is eligible for archival (outside the PITR window), else 0. |
| `pageserver_standby_horizon` | IntGaugeVec | `branch_id` | Standby apply LSN for which GC is held off. |
| `pageserver_aux_file_estimated_size` | IntGaugeVec | `branch_id` | Size of all aux files in the aux-file v2 store. |
| `pageserver_directory_entries_count` | UIntGaugeVec | `branch_id` | Sum of entries in pageserver-stored directory listings. |
| `pageserver_valid_lsn_lease_count` | UIntGaugeVec | `branch_id` | Valid LSN leases after refreshing GC info. |
| `pageserver_io_operations_bytes_total` | UIntGaugeVec | `operation`, `branch_id` | Bytes read/written in IO operations. `operation` ∈ `read`, `write`. |
| `pageserver_storage_operations_seconds_sum` | CounterVec | `operation`, `branch_id` | Total time spent on storage operations (sum). `operation` ∈ `logical size`, `find gc cutoffs`. |
| `pageserver_storage_operations_seconds_count` | IntCounterVec | `operation`, `branch_id` | Count of storage operations. |
| `pageserver_active_storage_operations_count` | IntGaugeVec | `operation`, `branch_id` | Currently active storage operations. |
| `pageserver_storage_operations_seconds_global` | HistogramVec | `operation` | Storage operation time, instance-wide. |
| `pageserver_timeline_states_count` | UIntGaugeVec | `state` | Count of timelines per state. |
| `pageserver_evictions_with_low_residence_duration` | IntCounterVec | `branch_id`, `residence_duration_data_source`, `low_threshold_secs` | Layers evicted while resident for less than the low threshold. |

## Initial logical size calculation

| Metric | Type | Labels | Meaning |
| --- | --- | --- | --- |
| `pageserver_initial_logical_size_start_calculation` | IntCounterVec | `attempt`, `circumstances` | Initial logical-size calculation attempts started. `attempt` ∈ `first`, `retry`; `circumstances` ∈ `EmptyInitial`, `SkippedConcurrencyLimiter`, `AfterBackgroundTasksRateLimit`. |
| `pageserver_initial_logical_size_drop_calculation` | IntCounterVec | `attempt` | Aborted size-calculation attempts. |
| `pageserver_initial_logical_size_finish_calculation` | IntCounter | — | Finished calculations. |
| `pageserver_initial_logical_size_drop_finished_calculation` | IntCounter | — | Dropped finished results (lets the finish counter be read as a gauge). |
| `pageserver_initial_logical_size_timelines_where_walreceiver_got_approximate_size` | IntCounter | — | Timelines where walreceiver first observed an `Approximate` size. |

## Tenant lifecycle / startup / background

| Metric | Type | Labels | Meaning |
| --- | --- | --- | --- |
| `pageserver_startup_duration_seconds` | GaugeVec | `phase` | Time taken by each startup phase. |
| `pageserver_startup_is_loading` | UIntGauge | — | 1 during initial tenant load, 0 otherwise. |
| `pageserver_tenant_preload_seconds` | Histogram | — | Time loading remote metadata on startup/attach. |
| `pageserver_tenant_attach_seconds` | Histogram | — | Time to initialize a tenant after metadata is loaded. |
| `pageserver_tenant_startup_scheduled` | IntCounter | — | Tenants included in the initial startup. |
| `pageserver_tenant_startup_complete` | IntCounter | — | Tenants that completed warm-up (or activated on demand during startup). |
| `pageserver_tenant_task_events` | IntCounterVec | `event` | Task lifecycle events (`start` / `stop` / `fail`). |
| `pageserver_background_loop_semaphore_wait_start_count` / `pageserver_background_loop_semaphore_wait_finish_count` | IntCounterPairVec | `task` | Background-loop concurrency-semaphore acquire calls started / finished. |
| `pageserver_background_loop_semaphore_wait_seconds` | HistogramVec | `task` | Time spent waiting on the background-loop semaphore. |
| `pageserver_background_loop_semaphore_waiting_tasks` | IntGaugeVec | `task` | Background-loop tasks waiting for the semaphore. |
| `pageserver_background_loop_semaphore_running_tasks` | IntGaugeVec | `task` | Background-loop tasks running concurrently. |
| `pageserver_background_loop_period_overrun_count` | IntCounterVec | `task`, `period` | Background-loop period-overrun warnings. |
| `pageserver_circuit_breaker_broken` / `pageserver_circuit_breaker_unbroken` | IntCounter | — | Circuit-breaker break / recover counts. |
| `pageserver_unexpected_ondemand_downloads_count` | IntCounter | — | Unexpected on-demand downloads. |
| `pageserver_concurrent_initdb` | UIntGauge | — | initdb processes running. |
| `pageserver_initdb_semaphore_seconds_global` | Histogram | — | Time spent getting a permit from the global initdb semaphore. |
| `pageserver_initdb_seconds_global` | Histogram | — | Time spent performing initdb. |

## Connections / utilization / config

| Metric | Type | Labels | Meaning |
| --- | --- | --- | --- |
| `pageserver_live_connections_started` / `pageserver_live_connections_finished` | IntCounterPairVec | `pageserver_connection_kind` | Network connections started / finished. |
| `pageserver_utilization_score` | UIntGauge | — | Utilization score reported to the controller for scheduling (0 empty, 1000000 full, above that overloaded). |
| `pageserver_tokio_executor_thread_configured_count` | UIntGaugeVec | `setup` | Configured tokio executor threads. `setup` denotes single vs multiple runtimes. |
| `pageserver_config_ignored_items` | UIntGaugeVec | `item` | On-disk config items ignored by the parser. Value is 1 per unknown item; the always-present `item=""` series is 0 so dashboards always have a series. |

## Process, allocator, and runtime

The `libmetrics_process_status_*` and `libmetrics_process_cpu_seconds_highres`
gauges come from `more_process_metrics::Collector` and are Linux-only. The
`pageserver_jemalloc_*` and `pageserver_tokio_epoll_uring_*` values are read
fresh on each scrape by their collectors (jemalloc advances its stats epoch
before reading).

| Metric | Type | Labels | Meaning |
| --- | --- | --- | --- |
| `libmetrics_build_info` | IntGaugeVec | `revision`, `build_tag` | Build/version info; value is always 1. |
| `libmetrics_launch_timestamp` | UIntGauge | — | Process launch time (milliseconds since epoch). |
| `libmetrics_process_status_vm_rss_bytes` | UIntGauge | — | `/proc/self/status` VmRSS — total resident set size (bytes). |
| `libmetrics_process_status_rss_anon_bytes` | UIntGauge | — | RssAnon — resident anonymous memory: heap, stack, private mappings (bytes). |
| `libmetrics_process_status_rss_file_bytes` | UIntGauge | — | RssFile — resident file-backed memory: shared libs, file mmaps (bytes). |
| `libmetrics_process_status_rss_shmem_bytes` | UIntGauge | — | RssShmem — resident shared memory: System V / POSIX shm (bytes). |
| `libmetrics_process_status_vm_data_bytes` | UIntGauge | — | VmData — data + heap + bss segment (bytes). |
| `libmetrics_process_status_vmlck` | UIntGauge | — | VmLck — locked memory (KiB). |
| `libmetrics_process_cpu_seconds_highres` | Gauge | — | Total user + system CPU time (seconds, sub-second resolution). |
| `pageserver_jemalloc_allocated_bytes` | UIntGauge | — | Bytes allocated and not yet freed (live heap). |
| `pageserver_jemalloc_active_bytes` | UIntGauge | — | Bytes in active pages (allocated + immediate slack). |
| `pageserver_jemalloc_resident_bytes` | UIntGauge | — | Bytes in physically-resident pages held by jemalloc (active + dirty). |
| `pageserver_jemalloc_mapped_bytes` | UIntGauge | — | Bytes mapped by jemalloc (active + dirty + muzzy). |
| `pageserver_jemalloc_retained_bytes` | UIntGauge | — | Bytes retained for future allocation (madvise'd away, may not be resident). |
| `pageserver_jemalloc_metadata_bytes` | UIntGauge | — | Bytes used by jemalloc internal metadata. |
| `pageserver_tokio_epoll_uring_slots_submission_queue_depth` | Histogram | — | Slots-waiters queue depth of each tokio-epoll-uring system. |
| `pageserver_tokio_epoll_uring_systems_created` / `pageserver_tokio_epoll_uring_systems_destroyed` | UIntGauge | — | tokio-epoll-uring systems created / destroyed. |
| `pageserver_tokio_epoll_uring_pageserver_thread_local_launch_success_count` | IntCounter | — | Thread-local system launches that spanned multiple executor threads. |
| `pageserver_tokio_epoll_uring_pageserver_thread_local_launch_failures_count` | IntCounter | — | Thread-local system launches that failed and were retried after back-off. |

## Declared but not emitted

The following statics are declared in `metrics.rs` as lazily-registered metrics
whose only writers are the `RemoteTimelineClientMetrics` code paths. Those paths
are inactive, so the statics are never forced, never register, and do not appear
in `/metrics` output: `pageserver_remote_physical_size`,
`pageserver_remote_physical_size_global`,
`pageserver_projected_remote_consistent_lsn`,
`pageserver_remote_timeline_client_calls_started`,
`pageserver_remote_timeline_client_calls_finished`, and
`pageserver_remote_timeline_client_seconds_global`.
