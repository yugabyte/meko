# yb-pageserver Prometheus metrics

Source: `compute/pageserver/src/metrics.rs` (metric definitions), exposed via the
`GET /metrics` route in `compute/pageserver/src/http/routes.rs`.

Metrics are served as Prometheus text exposition on the HTTP management port
(default `127.0.0.1:9898`). By default a background task collects them on an
interval (`METRICS_COLLECTION_INTERVAL`); if `force_metric_collection_on_scrape`
is set, collection happens on each scrape instead. `preinitialize_metrics`
force-instantiates the instance-wide (non-per-branch) metrics at startup so
they appear before any traffic.

Process-level metrics (`libmetrics_process_*`, `libmetrics_build_info`,
`libmetrics_launch_timestamp`) and the `pageserver_jemalloc_*` /
`pageserver_tokio_epoll_uring_*` collectors are registered in
`bin/yb-pageserver.rs` (`register_internal`). Everything below is registered in
`metrics.rs`.

## Label cardinality

Metrics labelled `branch_id` are per-branch. In yb-light a
process serves exactly one `branch_id`, so these are effectively
single-series per process (label values still present for cross-process
aggregation). Per-branch series are removed on branch shutdown
(`TimelineMetrics::shutdown`). `*_global` variants drop the `branch_id`
dimension.

## getpage / page_service (compute read hot path)

| Metric | Type | Labels | Meaning |
| --- | --- | --- | --- |
| `pageserver_smgr_query_started_global_count` | IntCounter | `smgr_query_type` | smgr queries started, by query type. |
| `pageserver_smgr_query_started_count` | IntCounter | `smgr_query_type`, `branch_id` | smgr queries started, per branch (getpage tracked per branch). |
| `pageserver_smgr_query_seconds` | Histogram | `smgr_query_type`, `branch_id` | Execution time of smgr handling, excluding batch/throttle delays. |
| `pageserver_smgr_query_seconds_global` | Histogram | `smgr_query_type` | Instance-wide version of the above. |
| `pageserver_page_service_batch_size_global` | Histogram | — | getpage batch sizes. |
| `pageserver_page_service_batch_size` | Histogram | `branch_id` | getpage batch sizes, per branch. |
| `pageserver_page_service_batch_break_reason_global` | IntCounter | `reason` | Why a getpage batch was broken. |
| `pageserver_page_service_batch_break_reason` | IntCounter | `branch_id`, `reason` | Batch-break reason, per branch. |
| `pageserver_page_service_config_max_batch_size` | IntGauge | `mode`, `execution`, `batching` | Configured max batch size + pipelining config. |
| `pageserver_page_service_pipeline_inflight_batches` | Histogram | — | #173: getpage batches in flight per connection in the pipelined executor (batch-concurrency), sampled when a batch is admitted. |
| `pageserver_scattered_set_keys_global` | Histogram | — | #220: scattered single-block keys folded into one multi-key `scan_scattered_set` YB query (batched-reads fold size; only folds of >=2 keys recorded). |
| `pageserver_page_service_pagestream_batch_wait_time_seconds` | Histogram | `branch_id` | Time a request waited in its batch before throttle+execution. |
| `pageserver_page_service_pagestream_batch_wait_time_seconds_global` | Histogram | — | Instance-wide batch wait time. |
| `pageserver_page_service_pagestream_flush_in_progress_micros` | IntCounter | `branch_id` | Microseconds a pagestream response spent flushing to TCP (in-progress sum, not completion latency). |
| `pageserver_page_service_pagestream_flush_in_progress_micros_global` | IntCounter | — | Instance-wide flush-in-progress micros. |
| `pageserver_get_vectored_seconds` | Histogram | `task_kind` | Time in `get_vectored` (only `PageRequestHandler` tracked). |
| `pageserver_scan_seconds` | Histogram | `task_kind` | Time in `scan` (only `PageRequestHandler` tracked). |
| `pageserver_wait_lsn_seconds` | Histogram | — | Time waiting for WAL to arrive (on completion). |
| `pageserver_wait_lsn_started_count` / `pageserver_wait_lsn_finished_count` | IntCounterPair | `branch_id` | wait_lsn operations started/finished. |
| `pageserver_wait_lsn_in_progress_micros` | IntCounter | `branch_id` | wait_lsn in-progress micros, per branch (periodic). |
| `pageserver_wait_lsn_in_progress_micros_global` | IntCounter | — | wait_lsn in-progress micros, global. |
| `pageserver_layers_per_read` | Histogram | `branch_id` | Layers visited per read (read amplification). |
| `pageserver_layers_per_read_global` | Histogram | — | Layers per read, global. |
| `pageserver_layers_per_read_batch_global` | Histogram | — | Layers visited per read batch. |
| `pageserver_layers_per_read_amortized_global` | Histogram | — | Layers per read amortized across batch. |
| `pageserver_deltas_per_read_global` | Histogram | — | Delta pages applied to image per read. |
| `pageserver_pagestream_handler_results_total` | IntCounter | `outcome` | Pagestream handler results: `success` / `internal_error` / `other_error`. |
| `pageserver_misrouted_pagestream_requests_total` | IntCounter | — | Pagestream requests routed to the wrong pageserver. |
| `pageserver_relsize_latest_cache_entries` | UIntGauge | — | Entries in latest rel-size cache. |
| `pageserver_relsize_latest_cache_hits` / `_misses` | IntCounter | — | Latest rel-size cache hits / misses. |
| `pageserver_relsize_snapshot_cache_entries` | UIntGauge | — | Entries in PITR (snapshot) rel-size cache. |
| `pageserver_relsize_snapshot_cache_hits` / `_misses` | IntCounter | — | Snapshot rel-size cache hits / misses. |
| `pageserver_relsize_cache_misses_old` | IntCounter | — | Rel-size cache misses where lookup LSN predates last rel update. |

## Tenant throttling (pagestream)

`kind` is always `pagestream`. Global variants have only `kind`; per-branch
variants add `branch_id`.

| Metric | Type | Labels | Meaning |
| --- | --- | --- | --- |
| `pageserver_tenant_throttling_count_accounted_start_global` / `..._start` | IntCounter | `kind` (+`branch_id`) | Throttle accounting starts. |
| `pageserver_tenant_throttling_count_accounted_finish_global` / `..._finish` | IntCounter | `kind` (+`branch_id`) | Throttle accounting finishes. |
| `pageserver_tenant_throttling_wait_usecs_sum_global` / `..._sum` | IntCounter | `kind` (+`branch_id`) | Microseconds spent waiting on throttle. |
| `pageserver_tenant_throttling_count_global` / `pageserver_tenant_throttling_count` | IntCounter | `kind` (+`branch_id`) | Number of throttlings. |

## WAL ingest / walreceiver

| Metric | Type | Labels | Meaning |
| --- | --- | --- | --- |
| `pageserver_wal_ingest_bytes_received` | IntCounter | — | Bytes of WAL ingested from safekeepers. |
| `pageserver_wal_ingest_records_received` | IntCounter | — | WAL records received. |
| `pageserver_wal_ingest_records_observed` | IntCounter | — | Metadata-only records observed (shard 0). |
| `pageserver_wal_ingest_records_committed` | IntCounter | — | WAL records that resulted in storage writes. |
| `pageserver_wal_ingest_values_committed` | IntCounter | `class`, `kind` | Values committed (`class`=metadata/data, `kind`=image/delta). |
| `pageserver_gap_blocks_zeroed_on_rel_extend` | IntCounter | — | Zero gap blocks written on relation extends. |
| `pageserver_timeline_wal_records_received` | IntCounter | `branch_id` | WAL records received, per branch. |
| `pageserver_walreceiver_started_connections_total` | IntCounter | — | walreceiver connections started. |
| `pageserver_walreceiver_active_managers` | IntGauge | — | Active walreceiver managers. |
| `pageserver_walreceiver_switches_total` | IntCounter | `reason` | walreceiver `change_connection` calls. |
| `pageserver_walreceiver_broker_updates_total` | IntCounter | — | Broker updates received. |
| `pageserver_walreceiver_candidates_events_total` | IntCounter | `event` (`add`/`remove`) | walreceiver candidate add/remove events. |

## Page store / YB write path (yb-light specific)

| Metric | Type | Labels | Meaning |
| --- | --- | --- | --- |
| `pageserver_yb_pagewriter_backpressure_waits_total` | IntCounter | `kind` (`flush`/`materializer`) | `PageWriter::put` calls that awaited downstream drain. |
| `pageserver_yb_pagewriter_unflushed_bytes` | UIntGauge | — | Bytes in PageWriter awaiting INSERT into YB (`pages`/`slru_pages`/`meta`). |
| `pageserver_yb_pagewriter_delta_counts_entries` | UIntGauge | — | Entries in PageWriter's per-key `delta_counts` map. |
| `pageserver_yb_pagewriter_delta_counts_approx_bytes` | UIntGauge | — | Approx bytes held by `delta_counts` (entries × 80). |
| `pageserver_yb_materializer_queue_depth` | UIntGauge | — | Image-materialiser jobs currently queued. |
| `pageserver_yb_materializer_inflight_bytes` | UIntGauge | — | Bytes held by an in-progress materialiser batch (chain payloads + redone images). |
| `pageserver_yb_materializer_read_side_dispatches_total` | IntCounter | — | Materialise jobs enqueued by the read path (chain exceeded `count_limit`). |
| `pageserver_yb_page_reads_total` | IntCounter | `source`, `path` | Page-chain reads. `source` = `getpage` (`read_chain`) or `materializer` (`read_chain_durable`); `path` = `fast_cache` (cache held a complete chain) or `yb_blend` (fell through to YB). `{source=*, path=yb_blend}` is the count of reads that hit YB. |
| `pageserver_yb_store_op_seconds` | HistogramVec | `op`, `path` | Latency of every PageStore round trip to YB. `op` = `read_chain` \| `read_slru_chain` \| `read_meta` \| `scan_range` \| `scan_slru_range` \| `put_pages` \| `put_slru_pages`; `path` = `ysql` or `tsclient` (pure-Rust pggate RPC, `hot_path = "tsclient"`). NOT the cache-blend `path` of `pageserver_yb_page_reads_total`. |
| `pageserver_yb_store_session_recoveries_total` | IntCounter | — | `yb_tsclient` session recoveries (reconnect + fresh tserver session + table re-validation) on the thin-RPC lane. Occasional after tserver restarts; sustained rate = flapping endpoint. |
| `pageserver_yb_store_call_timeouts_total` | IntCounter | — | `yb_tsclient` per-attempt call timeouts (transport healthy, no response within `rpc_timeout`) on the thin-RPC lane. Retried internally, so callers only see terminal failures — this is the early signal of a server-side stall. |
| `pageserver_yb_unified_pages_by_key_entries` | UIntGauge | — | Entries in `UnifiedPages.by_key` (write buffer + durable cache index). |
| `pageserver_yb_correctness_errors_total` | IntCounter | `kind` | Correctness-fatal errors. **Any non-zero value is a corruption signal.** Kinds include `materializer_redo`, `materializer_setup`, `materializer_commit`, `set_yb_upsert_mode`, `write_path_roundtrip`, `pagewriter_flush`, `tenant_task_panic`. |

## walredo

| Metric | Type | Labels | Meaning |
| --- | --- | --- | --- |
| `pageserver_wal_redo_seconds` | Histogram | — | Time spent in WAL redo. |
| `pageserver_wal_redo_records_histogram` | Histogram | — | Records replayed per redo in the redo process. |
| `pageserver_wal_redo_bytes_histogram` | Histogram | — | Bytes replayed per redo sent to Postgres. |
| `pageserver_replayed_wal_records_total` | IntCounter | — | Total WAL records replayed in redo. |
| `pageserver_wal_redo_process_launch_duration` | Histogram | — | Duration of successful `WalRedoProcess::launch` calls. |
| `pageserver_wal_redo_process_started_total` | IntCounter | — | WAL redo processes started. |
| `pageserver_wal_redo_process_stopped_total` | IntCounter | `cause` | WAL redo processes stopped, by kill cause. |
| `pageserver_walredo_stderr_logger_tasks_started_total` / `_finished_total` | IntCounter | — | walredo stderr-logger tasks started / finished. |

## basebackup / compute commands

| Metric | Type | Labels | Meaning |
| --- | --- | --- | --- |
| `pageserver_basebackup_query_seconds` | Histogram | `result` (`ok`/`error`/`client_error`) | basebackup query durations. |
| `pageserver_compute_commands` | IntCounter | `command` | compute→pageserver commands processed (pagestream v2/v3, basebackup, fullbackup, lease_lsn). |
| `pageserver_basebackup_cache_read_total` | IntCounter | `result` (hit/miss/error) | basebackup cache reads. |
| `pageserver_basebackup_cache_prepare_total` | IntCounter | `result` (ok/skip/error) | basebackup cache prepare requests. |
| `pageserver_basebackup_cache_entries_total` | UIntGauge | — | Entries in basebackup cache. |
| `pageserver_basebackup_cache_size_bytes` | UIntGauge | — | On-disk size of basebackup cache. |
| `pageserver_basebackup_cache_prepare_queue_size` | UIntGauge | — | Pending requests in basebackup prepare channel. |

## Per-branch storage / size

| Metric | Type | Labels | Meaning |
| --- | --- | --- | --- |
| `pageserver_last_record_lsn` | IntGauge | `branch_id` | Last record (applied) LSN. |
| `pageserver_current_logical_size` | UIntGauge | `branch_id` | Current logical size. |
| `pageserver_resident_physical_size` | UIntGauge | `branch_id` | Resident layer-file bytes (attached). yb-light: ~0. |
| `pageserver_resident_physical_size_global` | UIntGauge | — | Global resident physical size. |
| `pageserver_visible_physical_size` | UIntGauge | `branch_id` | Visible layer-file bytes. |
| `pageserver_pitr_history_size` | UIntGauge | `branch_id` | Data written since PITR cutoff. |
| `pageserver_archive_size` | UIntGauge | `branch_id` | Logical size if eligible for archival, else 0. |
| `pageserver_standby_horizon` | IntGauge | `branch_id` | Standby apply LSN holding off GC. |
| `pageserver_aux_file_estimated_size` | IntGauge | `branch_id` | Aux-file size (v2 store). |
| `pageserver_directory_entries_count` | UIntGauge | `branch_id` | Sum of entries in stored directory listings. |
| `pageserver_valid_lsn_lease_count` | UIntGauge | `branch_id` | Valid LSN leases after GC-info refresh. |
| `pageserver_io_operations_bytes_total` | UIntGauge | `operation` (read/write), `branch_id` | Bytes read/written in IO operations. |
| `pageserver_storage_operations_seconds_sum` | Counter | `operation`, `branch_id` | Time on storage operations (sum). `operation` ∈ logical-size, find-gc-cutoffs. |
| `pageserver_storage_operations_seconds_count` | IntCounter | `operation`, `branch_id` | Storage operation count. |
| `pageserver_active_storage_operations_count` | IntGauge | `operation`, `branch_id` | Active storage operations. |
| `pageserver_storage_operations_seconds_global` | Histogram | `operation` | Storage operation time, global. |
| `pageserver_timeline_states_count` | UIntGauge | `state` | Count of timelines per state. |
| `pageserver_evictions_with_low_residence_duration` | IntCounter | `branch_id`, `residence_duration_data_source`, `low_threshold_secs` | Layers evicted below residence threshold. |

## Initial logical size calculation

| Metric | Type | Labels | Meaning |
| --- | --- | --- | --- |
| `pageserver_initial_logical_size_start_calculation` | IntCounter | `attempt`, `circumstances` | Initial logical-size calculation attempts started. |
| `pageserver_initial_logical_size_drop_calculation` | IntCounter | `attempt` | Aborted size-calculation attempts. |
| `pageserver_initial_logical_size_finish_calculation` | IntCounter | — | Finished calculations. |
| `pageserver_initial_logical_size_drop_finished_calculation` | IntCounter | — | Dropped finished results (turns finish into a gauge). |
| `pageserver_initial_logical_size_timelines_where_walreceiver_got_approximate_size` | IntCounter | — | Timelines where walreceiver first saw `Approximate` size. |

## Tenant lifecycle / startup / background

| Metric | Type | Labels | Meaning |
| --- | --- | --- | --- |
| `pageserver_startup_duration_seconds` | Gauge | `phase` | Time per startup phase. |
| `pageserver_startup_is_loading` | UIntGauge | — | 1 during initial tenant load, else 0. |
| `pageserver_tenant_preload_seconds` | Histogram | — | Time loading remote metadata on startup/attach. |
| `pageserver_tenant_attach_seconds` | Histogram | — | Time to initialize tenant after metadata load. |
| `pageserver_tenant_startup_scheduled` | IntCounter | — | Tenants included in startup. |
| `pageserver_tenant_startup_complete` | IntCounter | — | Tenants that completed warm-up. |
| `pageserver_tenant_task_events` | IntCounter | `event` | Task start/stop/fail events. |
| `pageserver_background_loop_semaphore_wait_start_count` / `_wait_finish_count` | IntCounterPair | `task` | Background-loop semaphore acquire calls started/finished. |
| `pageserver_background_loop_semaphore_wait_seconds` | Histogram | `task` | Time waiting on background-loop semaphore. |
| `pageserver_background_loop_semaphore_waiting_tasks` | IntGauge | `task` | Background-loop tasks waiting for semaphore. |
| `pageserver_background_loop_semaphore_running_tasks` | IntGauge | `task` | Background-loop tasks running concurrently. |
| `pageserver_background_loop_period_overrun_count` | IntCounter | `task`, `period` | Background-loop period-overrun warnings. |
| `pageserver_circuit_breaker_broken` / `pageserver_circuit_breaker_unbroken` | IntCounter | — | Circuit-breaker break / recover counts. |
| `pageserver_unexpected_ondemand_downloads_count` | IntCounter | — | Unexpected on-demand downloads. |
| `pageserver_concurrent_initdb` | UIntGauge | — | initdb processes running. |
| `pageserver_initdb_semaphore_seconds_global` | Histogram | — | Time getting the global initdb semaphore permit. |
| `pageserver_initdb_seconds_global` | Histogram | — | Time performing initdb. |

## Connections / utilization / process / config

| Metric | Type | Labels | Meaning |
| --- | --- | --- | --- |
| `pageserver_live_connections_started` / `pageserver_live_connections_finished` | IntCounterPair | `pageserver_connection_kind` | Network connections started/finished. |
| `pageserver_utilization_score` | UIntGauge | — | Utilization score reported to storage controller (0 empty, 1000000 full). |
| `pageserver_tokio_executor_thread_configured_count` | UIntGauge | `setup` | Configured tokio executor threads. |
| `pageserver_config_ignored_items` | UIntGauge | `item` | Unknown on-disk config items (1 = ignored; `""` always 0). |
| `pageserver_jemalloc_allocated_bytes` / `_active_bytes` / `_resident_bytes` / `_mapped_bytes` / `_retained_bytes` / `_metadata_bytes` | UIntGauge | — | jemalloc stats (collector refreshes epoch per scrape). |
| `pageserver_tokio_epoll_uring_slots_submission_queue_depth` | Histogram | — | tokio-epoll-uring slots-waiters queue depth. |
| `pageserver_tokio_epoll_uring_systems_created` / `_systems_destroyed` | UIntGauge | — | tokio-epoll-uring systems created / destroyed. |
| `pageserver_tokio_epoll_uring_pageserver_thread_local_launch_success_count` / `_failures_count` | IntCounter | — | thread-local system launch successes / failures. |

## Defined but inactive in yb-light

These metrics exist in `metrics.rs` but are dead code in yb-light (the remote
timeline client / S3-style layer upload path is gone) and will not appear with
non-zero values: `pageserver_remote_physical_size`,
`pageserver_remote_physical_size_global`,
`pageserver_projected_remote_consistent_lsn`,
`pageserver_remote_timeline_client_calls_started` / `_finished`,
`pageserver_remote_timeline_client_seconds_global`.
