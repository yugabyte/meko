use std::cell::Cell;
use std::collections::HashMap;
use std::num::NonZeroUsize;
use std::os::fd::RawFd;
use std::sync::atomic::AtomicU64;
use std::sync::{Arc, Mutex};
use std::time::{Duration, Instant};

use enum_map::{Enum as _, EnumMap};
use futures::Future;
use metrics::{
    Counter, CounterVec, GaugeVec, Histogram, HistogramVec, IntCounter, IntCounterPair,
    IntCounterPairVec, IntCounterVec, IntGauge, IntGaugeVec, UIntGauge, UIntGaugeVec,
    register_counter_vec, register_gauge_vec, register_histogram, register_histogram_vec,
    register_int_counter, register_int_counter_pair_vec, register_int_counter_vec,
    register_int_gauge, register_int_gauge_vec, register_uint_gauge, register_uint_gauge_vec,
};
use once_cell::sync::Lazy;
use pageserver_api::config::defaults::DEFAULT_MAX_GET_VECTORED_KEYS;
use pageserver_api::config::{
    PageServicePipeliningConfig, PageServicePipeliningConfigPipelined,
    PageServiceProtocolPipelinedBatchingStrategy, PageServiceProtocolPipelinedExecutionStrategy,
};
use postgres_backend::{QueryError, is_expected_io_error};
use pq_proto::framed::ConnectionError;
use strum::{EnumCount, IntoEnumIterator as _, VariantNames};
use strum_macros::{IntoStaticStr, VariantNames};
use utils::id::BranchId;

use crate::config;
use crate::config::PageServerConf;
use crate::page_server::tasks::BackgroundLoopKind;
use crate::page_server::throttle::ThrottleResult;
use crate::pgdatadir_mapping::DatadirModificationStats;
use crate::task_mgr::TaskKind;

/// Prometheus histogram buckets (in seconds) for operations in the critical
/// path. In other words, operations that directly affect that latency of user
/// queries.
///
/// The buckets capture the majority of latencies in the microsecond and
/// millisecond range but also extend far enough up to distinguish "bad" from
/// "really bad".
const CRITICAL_OP_BUCKETS: &[f64] = &[
    0.000_001, 0.000_010, 0.000_100, // 1 us, 10 us, 100 us
    0.001_000, 0.010_000, 0.100_000, // 1 ms, 10 ms, 100 ms
    1.0, 10.0, 100.0, // 1 s, 10 s, 100 s
];

// Metrics collected on operations on the storage repository.
#[derive(Debug, VariantNames, IntoStaticStr)]
#[strum(serialize_all = "kebab_case")]
pub(crate) enum StorageTimeOperation {
    #[strum(serialize = "logical size")]
    LogicalSize,

    #[strum(serialize = "find gc cutoffs")]
    FindGcCutoffs,
}

pub(crate) static STORAGE_TIME_SUM_PER_TIMELINE: Lazy<CounterVec> = Lazy::new(|| {
    register_counter_vec!(
        "pageserver_storage_operations_seconds_sum",
        "Total time spent on storage operations with operation, tenant and timeline dimensions",
        &["operation", "branch_id"],
    )
    .expect("failed to define a metric")
});

pub(crate) static STORAGE_TIME_COUNT_PER_TIMELINE: Lazy<IntCounterVec> = Lazy::new(|| {
    register_int_counter_vec!(
        "pageserver_storage_operations_seconds_count",
        "Count of storage operations with operation, tenant and timeline dimensions",
        &["operation", "branch_id"],
    )
    .expect("failed to define a metric")
});

pub(crate) static STORAGE_ACTIVE_COUNT_PER_TIMELINE: Lazy<IntGaugeVec> = Lazy::new(|| {
    register_int_gauge_vec!(
        "pageserver_active_storage_operations_count",
        "Count of active storage operations with operation, tenant and timeline dimensions",
        &["operation", "branch_id"],
    )
    .expect("failed to define a metric")
});

// Buckets for background operations like compaction, GC, size calculation
const STORAGE_OP_BUCKETS: &[f64] = &[0.010, 0.100, 1.0, 10.0, 100.0, 1000.0];

pub(crate) static STORAGE_TIME_GLOBAL: Lazy<HistogramVec> = Lazy::new(|| {
    register_histogram_vec!(
        "pageserver_storage_operations_seconds_global",
        "Time spent on storage operations",
        &["operation"],
        STORAGE_OP_BUCKETS.into(),
    )
    .expect("failed to define a metric")
});

/// Measures layers visited per read (i.e. read amplification).
///
/// NB: for a batch, we count all visited layers towards each read. While the cost of layer visits
/// are amortized across the batch, and some layers may not intersect with a given key, each visited
/// layer contributes directly to the observed latency for every read in the batch, which is what we
/// care about.
pub(crate) static LAYERS_PER_READ: Lazy<HistogramVec> = Lazy::new(|| {
    register_histogram_vec!(
        "pageserver_layers_per_read",
        "Layers visited to serve a single read (read amplification). In a batch, all visited layers count towards every read.",
        &["branch_id"],
        // Low resolution to reduce cardinality.
        vec![4.0, 8.0, 16.0, 32.0, 64.0, 128.0, 256.0],
    )
    .expect("failed to define a metric")
});

pub(crate) static LAYERS_PER_READ_GLOBAL: Lazy<Histogram> = Lazy::new(|| {
    register_histogram!(
        "pageserver_layers_per_read_global",
        "Layers visited to serve a single read (read amplification). In a batch, all visited layers count towards every read.",
        vec![1.0, 2.0, 4.0, 8.0, 16.0, 32.0, 64.0, 128.0, 256.0, 512.0, 1024.0],
    )
    .expect("failed to define a metric")
});

pub(crate) static LAYERS_PER_READ_BATCH_GLOBAL: Lazy<Histogram> = Lazy::new(|| {
    register_histogram!(
        "pageserver_layers_per_read_batch_global",
        "Layers visited to serve a single read batch (read amplification), regardless of number of reads.",
        vec![
            1.0, 2.0, 4.0, 8.0, 16.0, 32.0, 64.0, 128.0, 256.0, 512.0, 1024.0
        ],
    )
    .expect("failed to define a metric")
});

pub(crate) static LAYERS_PER_READ_AMORTIZED_GLOBAL: Lazy<Histogram> = Lazy::new(|| {
    register_histogram!(
        "pageserver_layers_per_read_amortized_global",
        "Layers visited to serve a single read (read amplification). Amortized across a batch: \
            all visited layers are divided by number of reads.",
        vec![
            1.0, 2.0, 4.0, 8.0, 16.0, 32.0, 64.0, 128.0, 256.0, 512.0, 1024.0
        ],
    )
    .expect("failed to define a metric")
});

/// Number of scattered single-block keys folded into one multi-key
/// `scan_scattered_set` YB query (the batched-reads fold size). Only recorded
/// for genuine folds (≥2 keys); a lone scattered block takes the single-key
/// path. Higher = more per-query batching = fewer YB round trips. Lets us
/// confirm in prod that scattered folding engages and how large the folds are.
pub(crate) static SCATTERED_SET_KEYS_GLOBAL: Lazy<Histogram> = Lazy::new(|| {
    register_histogram!(
        "pageserver_scattered_set_keys_global",
        "Scattered single-block keys folded into one multi-key scan_scattered_set YB query (batched-reads fold size; only folds of >=2 keys are recorded).",
        vec![2.0, 4.0, 8.0, 16.0, 32.0, 64.0],
    )
    .expect("failed to define a metric")
});

pub(crate) static DELTAS_PER_READ_GLOBAL: Lazy<Histogram> = Lazy::new(|| {
    // We expect this to be low because of Postgres checkpoints. Let's see if that holds.
    register_histogram!(
        "pageserver_deltas_per_read_global",
        "Number of delta pages applied to image page per read",
        vec![0.0, 1.0, 2.0, 4.0, 8.0, 16.0, 32.0, 64.0, 128.0, 256.0],
    )
    .expect("failed to define a metric")
});

pub(crate) static CONCURRENT_INITDBS: Lazy<UIntGauge> = Lazy::new(|| {
    register_uint_gauge!(
        "pageserver_concurrent_initdb",
        "Number of initdb processes running"
    )
    .expect("failed to define a metric")
});

/// Counter of `PageWriter::put` calls that blocked waiting for downstream
/// to drain. Labelled by the source of the stall:
///
/// - `flush` — the in-RAM buffer reached `FlushPolicy::backpressure_bytes`
///   and `put` awaited the next successful flush. Bounds heap growth
///   when YB INSERTs (own `pageserver.pages`) can't keep up with the
///   walreceiver's ingest rate.
/// - `materializer` — the image-materialiser channel was full
///   (`materialize_queue_capacity` jobs in flight) and `put` had to
///   await a slot. Bounds outstanding walredo work when the
///   materialiser can't keep up.
///
/// Either signal is a fast canary for "PageWriter is the slow side";
/// non-zero rate sustained over minutes means you need to either raise
/// the relevant cap, scale YB, or shed load upstream.
pub(crate) static PAGEWRITER_BACKPRESSURE_WAITS: Lazy<IntCounterVec> = Lazy::new(|| {
    register_int_counter_vec!(
        "pageserver_yb_pagewriter_backpressure_waits_total",
        "PageWriter::put calls that had to await downstream drain, by source",
        &["kind"],
    )
    .expect("failed to define a metric")
});

/// Current bytes pending durable flush (rel-page UnifiedPages.unflushed +
/// SLRU/meta aux buffers). Sampled by `PageWriter::stats()` when callers
/// emit the gauge. Hits `FlushPolicy::backpressure_bytes` (50 MiB default)
/// before triggering wait. Spikes well above the size-based flush trigger
/// (`max_bytes`, 25 MiB default) indicate YB is the bottleneck.
pub(crate) static PAGEWRITER_UNFLUSHED_BYTES: Lazy<UIntGauge> = Lazy::new(|| {
    register_uint_gauge!(
        "pageserver_yb_pagewriter_unflushed_bytes",
        "Bytes in PageWriter awaiting INSERT into pageserver.pages / slru_pages / meta",
    )
    .expect("failed to define a metric")
});

/// Current materialiser-queue slot usage out of `materialize_queue_capacity`.
/// Approaching capacity = walreceiver is about to backpressure on
/// `materializer` (see [`PAGEWRITER_BACKPRESSURE_WAITS`] above).
///
/// Sampled in two places so the value stays fresh whether the system
/// is active or idle: (a) the materialiser task updates it on every
/// `rx.recv` / `try_recv`, capturing the drain edge, and (b)
/// `PageWriter` updates it after each dispatch burst. Previously only
/// the dispatch path sampled, so the gauge could appear frozen at the
/// last burst's value for the entire quiescent window between bursts.
pub(crate) static MATERIALIZER_QUEUE_DEPTH: Lazy<UIntGauge> = Lazy::new(|| {
    register_uint_gauge!(
        "pageserver_yb_materializer_queue_depth",
        "Image-materialiser mpsc jobs currently queued (not yet processed)",
    )
    .expect("failed to define a metric")
});

/// Residual due span a budget-capped drain tick couldn't reach;
/// 0 while keeping up. Growth = insert rate > drain rate. (Docs:
/// pageserver-metrics.md §GC.)
pub(crate) static GC_DRAIN_LAG_BYTES: Lazy<UIntGauge> = Lazy::new(|| {
    register_uint_gauge!(
        "pageserver_yb_gc_drain_lag_bytes",
        "GC cutoff minus the oldest due pending_gc candidate's due_lsn (bytes; 0 = caught up)",
    )
    .expect("failed to define a metric")
});

/// One increment per drain iteration, by outcome: drained / capped /
/// empty / lease_grace / disabled / error (exactly one per tick).
pub(crate) static GC_DRAIN_TICKS: Lazy<IntCounterVec> = Lazy::new(|| {
    register_int_counter_vec!(
        "pageserver_yb_gc_drain_ticks_total",
        "GC drain-loop iterations, by outcome",
        &["outcome"]
    )
    .expect("failed to define a metric")
});

/// Current GC cutoff, sampled per tick. `last_record_lsn − this` =
/// retained-history window.
pub(crate) static GC_APPLIED_CUTOFF_LSN: Lazy<UIntGauge> = Lazy::new(|| {
    register_uint_gauge!(
        "pageserver_yb_gc_applied_cutoff_lsn",
        "Current GC cutoff (applied_gc_cutoff_lsn); reads below it are rejected",
    )
    .expect("failed to define a metric")
});

/// Candidates emitted into `pending_gc`, by producer path: trim from
/// ingest | materialiser (post every-n sampling), plus the unsampled
/// drop | shrink events.
pub(crate) static GC_CANDIDATES_EMITTED: Lazy<IntCounterVec> = Lazy::new(|| {
    register_int_counter_vec!(
        "pageserver_yb_gc_candidates_emitted_total",
        "GC candidates emitted into pending_gc, by producer path",
        &["path"]
    )
    .expect("failed to define a metric")
});

/// Drop events a drain tick held on the in-memory deferred list because
/// their fork had a materialise job pending (acting would race the
/// job's later image INSERT, stamped at a folded LSN below the drop);
/// re-counted every tick they stay held. Occasional increments are
/// normal; sustained growth for the same fork means a wedged job is
/// pinning its reclamation.
pub(crate) static GC_DROP_DEFERRED: Lazy<IntCounter> = Lazy::new(|| {
    register_int_counter!(
        "pageserver_yb_gc_drop_deferred_total",
        "GC drop events deferred: fork had a pending materialise job"
    )
    .expect("failed to define a metric")
});

/// Rows the drain's select returned per tick (pre-routing).
pub(crate) static GC_CANDIDATES_READ: Lazy<IntCounter> = Lazy::new(|| {
    register_int_counter!(
        "pageserver_yb_gc_candidates_read_total",
        "Rows returned by the GC drain's pending_gc select (per tick)",
    )
    .expect("failed to define a metric")
});

/// Rows the dequeue deleted (= the tick's acted rows; ≈ read at
/// steady state). rate(emitted) − rate(removed) = queue growth.
pub(crate) static GC_CANDIDATES_REMOVED: Lazy<IntCounter> = Lazy::new(|| {
    register_int_counter!(
        "pageserver_yb_gc_candidates_removed_total",
        "Rows deleted from pending_gc by the drain dequeue (true queue-shrink count)",
    )
    .expect("failed to define a metric")
});

/// `pages` rows deleted by the GC drain (trim / drop / shrink).
pub(crate) static GC_PAGES_ROWS_DELETED: Lazy<IntCounter> = Lazy::new(|| {
    register_int_counter!(
        "pageserver_yb_gc_pages_rows_deleted_total",
        "pageserver.pages rows deleted by the GC drain",
    )
    .expect("failed to define a metric")
});

/// Counter of read-path materialise-job dispatches. Fires when
/// `PageWriter::read_chain` / `scan_range` returns a chain longer
/// than `count_limit` — the write-side trigger should have fired but
/// didn't (PS crash dropped the queued job, or the post-image
/// catch-up window grew). A non-zero rate here is the self-healing
/// path closing the gap; sustained high rate suggests the write
/// trigger needs tuning.
pub(crate) static MATERIALIZER_READ_SIDE_DISPATCHES: Lazy<metrics::IntCounter> = Lazy::new(|| {
    metrics::register_int_counter!(
        "pageserver_yb_materializer_read_side_dispatches_total",
        "Materialise jobs enqueued by the read path because the returned chain exceeded count_limit",
    )
    .expect("failed to define a metric")
});

/// Page-chain reads, split by which path served them. Labels:
///
/// - `source` — `getpage` (`PageWriter::read_chain`, the GetPage@LSN /
///   vectored read path) vs `materializer`
///   (`PageWriter::read_chain_durable`, the image-fold path).
/// - `path` — `fast_cache` (the cache held a complete chain; no YB
///   round trip) vs `yb_blend` (fell through to a YB read).
///
/// `{source=*, path=yb_blend}` is the count of reads that actually hit
/// YB; the `fast_cache` / `yb_blend` ratio is the cache hit rate for
/// each source. A rising getpage `yb_blend` rate means the cache fast
/// path is being bypassed more (e.g. eviction pressure, or the install
/// guard skipping more below-min-cached images).
pub(crate) static PAGE_READS: Lazy<IntCounterVec> = Lazy::new(|| {
    register_int_counter_vec!(
        "pageserver_yb_page_reads_total",
        "Page-chain reads by source (getpage|materializer) and path (fast_cache|yb_blend)",
        &["source", "path"],
    )
    .expect("failed to define a metric")
});

/// Latency of every PageStore round trip to YB, by operation and
/// transport. `path` = `ysql` (Postgres wire) or `tsclient` (pure-Rust
/// pggate RPC, `hot_path = "tsclient"`); see `architecture/yugabyte.md`.
/// NOT the same `path` label as `pageserver_yb_page_reads_total` (which
/// means cache blend).
pub(crate) static YB_STORE_OP_SECONDS: Lazy<HistogramVec> = Lazy::new(|| {
    register_histogram_vec!(
        "pageserver_yb_store_op_seconds",
        "Latency of PageStore operations against YB, by op and transport path",
        &["op", "path"],
        metrics::exponential_buckets(0.0005, 2.0, 16)
            .expect("pageserver_yb_store_op_seconds bucket spec"),
    )
    .expect("failed to define a metric")
});

/// Thin-RPC (`hot_path = "tsclient"`) lane: successful `yb_tsclient`
/// session recoveries (reconnect + fresh server session + table
/// re-validation). Occasional after tserver restarts; a sustained rate
/// means the lane's endpoint is flapping.
pub(crate) static YB_STORE_SESSION_RECOVERIES: Lazy<IntCounter> = Lazy::new(|| {
    metrics::register_int_counter!(
        "pageserver_yb_store_session_recoveries_total",
        "yb_tsclient session recoveries on the pageserver's thin-RPC lane",
    )
    .expect("failed to define a metric")
});

/// Thin-RPC lane: per-attempt `yb_tsclient` calls that timed out waiting
/// for a response (transport healthy, server never answered within
/// `rpc_timeout`). The client retries these internally, so callers only
/// see the terminal failure — this counter is the early signal of a
/// server-side stall; a prior incident where reads stalled silently for
/// hours motivated it.
pub(crate) static YB_STORE_CALL_TIMEOUTS: Lazy<IntCounter> = Lazy::new(|| {
    metrics::register_int_counter!(
        "pageserver_yb_store_call_timeouts_total",
        "yb_tsclient per-attempt call timeouts on the pageserver's thin-RPC lane",
    )
    .expect("failed to define a metric")
});

/// Count of correctness-fatal errors observed across the pageserver
/// hot path, broken down by `kind`. **Any non-zero value is a
/// corruption signal**; this metric exists specifically so paging
/// alerts catch what the prior code's `warn! + drop the job` pattern
/// hid. Labels:
///
/// - `materializer_redo` — `redo_one` returned Err (chain
///   deserialise, walredo PANIC, INSERT failure).
/// - `materializer_setup` — `redo_phase` setup failed before any
///   per-job work ran (typically a YB read of a chain).
/// - `materializer_commit` — `commit_phase` `put_pages` failed.
/// - `set_yb_upsert_mode` — `SET yb_enable_upsert_mode = true` was
///   rejected. Every subsequent INSERT on the affected connection
///   runs in non-upsert mode and may silently fail retries with
///   duplicate-key.
/// - `write_path_roundtrip` — `NeonWalRecord` failed to deserialise
///   its own freshly-serialised bytes (see
///   `build_page_row` round-trip check). If this is non-zero our
///   serializer is at fault; if zero while reads still see
///   deserialise errors, corruption is between us and YB.
/// - `pagewriter_flush` — `PageWriter::flush` returned Err on a
///   periodic or shutdown tick; rows may not have made it to YB.
/// - `tenant_task_panic` — a tenant background task ended via
///   panic (`JoinError::is_panic()`).
/// - `gc_shrink_relsize_malformed` — a rel-size meta payload shorter
///   than 4 bytes (every writer stamps exactly 4). The shrink skips
///   the fork rather than wedging the drain on persistent state.
/// - `gc_unknown_candidate_kind` — a `pending_gc` row with an unknown
///   `blknum` kind. The drain skips it; it stays queued.
pub(crate) static CORRECTNESS_ERRORS: Lazy<metrics::IntCounterVec> = Lazy::new(|| {
    metrics::register_int_counter_vec!(
        "pageserver_yb_correctness_errors_total",
        "Correctness-fatal errors that the old code silently warn-and-dropped",
        &["kind"],
    )
    .expect("failed to define a metric")
});

/// Sum of chain-payload bytes + freshly-redone image bytes held by an
/// in-progress `materialize_batch_inner`. Driven by `redo_one` futures
/// inside the materialiser; cleared when the batch's bulk INSERT
/// completes. Captures the **transient** allocation the
/// `read_chain_durable → walredo → put_pages` pipeline holds for
/// `REDO_CONCURRENCY` × batch-size keys at a time. Unlike the
/// post-flush `UnifiedPages.durable_fifo`, this pool is **not** capped
/// by `read_cache_bytes` — it's bounded only by chain depth × batch
/// width. Watch this when investigating RSS spikes that don't line up
/// with [`PAGEWRITER_UNFLUSHED_BYTES`].
pub(crate) static MATERIALIZER_INFLIGHT_BYTES: Lazy<UIntGauge> = Lazy::new(|| {
    register_uint_gauge!(
        "pageserver_yb_materializer_inflight_bytes",
        "Bytes held by an in-progress image-materialiser batch (chain payloads + redone images)",
    )
    .expect("failed to define a metric")
});

/// Entries in `WriterState::delta_counts` — per-key delta tally that
/// accumulates from each `put_page` and only drains when a key's
/// chain crosses `image_chain_limit` (or its accumulated payload
/// crosses `image_chain_payload_bytes_limit`). At a steady working
/// set this should track the number of distinct keys recently
/// touched; sustained growth above that ceiling means the
/// materialiser isn't keeping up with chain accumulation.
pub(crate) static PAGEWRITER_DELTA_COUNTS_ENTRIES: Lazy<UIntGauge> = Lazy::new(|| {
    register_uint_gauge!(
        "pageserver_yb_pagewriter_delta_counts_entries",
        "Entries in PageWriter's per-key delta_counts HashMap",
    )
    .expect("failed to define a metric")
});

/// Approximate bytes held by `delta_counts`. Rough rule:
/// `entries × DELTA_COUNTS_BYTES_PER_ENTRY` (80 B per entry covers the
/// 8 B `DeltaTally` payload + HashMap overhead + the 32 B `Bytes`
/// header + a typical ~16 B serialized key). Use against
/// `process_resident_memory_bytes` to attribute RSS that doesn't
/// match any other capped consumer.
pub(crate) static PAGEWRITER_DELTA_COUNTS_APPROX_BYTES: Lazy<UIntGauge> = Lazy::new(|| {
    register_uint_gauge!(
        "pageserver_yb_pagewriter_delta_counts_approx_bytes",
        "Approximate bytes held by PageWriter's delta_counts HashMap (entries × 80)",
    )
    .expect("failed to define a metric")
});

/// Per-entry byte estimate for `delta_counts`. Update both this and
/// the metric doc together if the `DeltaTally` shape or key
/// representation changes.
pub(crate) const DELTA_COUNTS_BYTES_PER_ENTRY: u64 = 80;

/// Entries in `UnifiedPages::by_key` — the master per-key map that
/// indexes both the pre-flush write buffer and the post-flush
/// durable cache. Bounded *by bytes* via `read_cache_bytes`, but the
/// entry count itself is not capped; comparing this against
/// `delta_counts_entries` tells you whether unflushed traffic or the
/// durable cache is driving the working-set cardinality.
pub(crate) static UNIFIED_PAGES_BY_KEY_ENTRIES: Lazy<UIntGauge> = Lazy::new(|| {
    register_uint_gauge!(
        "pageserver_yb_unified_pages_by_key_entries",
        "Entries in UnifiedPages.by_key (per-key index over write buffer + durable cache)",
    )
    .expect("failed to define a metric")
});

// Per-process memory gauges (VmRSS, VmData, RssAnon, RssFile, RssShmem)
// are emitted as `libmetrics_process_status_*_bytes` by the
// `more_process_metrics::Collector` registered in `bin/yb-pageserver.rs`;
// see `libs/metrics/src/more_process_metrics.rs`.
//
// jemalloc-specific gauges (allocated/active/resident/mapped/dirty/muzzy)
// come from `jemalloc_stats::Collector` further below.

pub(crate) static INITDB_SEMAPHORE_ACQUISITION_TIME: Lazy<Histogram> = Lazy::new(|| {
    register_histogram!(
        "pageserver_initdb_semaphore_seconds_global",
        "Time spent getting a permit from the global initdb semaphore",
        STORAGE_OP_BUCKETS.into()
    )
    .expect("failed to define metric")
});

pub(crate) static INITDB_RUN_TIME: Lazy<Histogram> = Lazy::new(|| {
    register_histogram!(
        "pageserver_initdb_seconds_global",
        "Time spent performing initdb",
        STORAGE_OP_BUCKETS.into()
    )
    .expect("failed to define metric")
});

pub(crate) struct GetVectoredLatency {
    map: EnumMap<TaskKind, Option<Histogram>>,
}

#[allow(dead_code)]
pub(crate) struct ScanLatency {
    map: EnumMap<TaskKind, Option<Histogram>>,
}

impl GetVectoredLatency {
    // Only these task types perform vectored gets. Filter all other tasks out to reduce total
    // cardinality of the metric.
    const TRACKED_TASK_KINDS: [TaskKind; 1] = [TaskKind::PageRequestHandler];

    pub(crate) fn for_task_kind(&self, task_kind: TaskKind) -> Option<&Histogram> {
        self.map[task_kind].as_ref()
    }
}

impl ScanLatency {
    // Only these task types perform vectored gets. Filter all other tasks out to reduce total
    // cardinality of the metric.
    const TRACKED_TASK_KINDS: [TaskKind; 1] = [TaskKind::PageRequestHandler];

    pub(crate) fn for_task_kind(&self, task_kind: TaskKind) -> Option<&Histogram> {
        self.map[task_kind].as_ref()
    }
}

pub(crate) struct ScanLatencyOngoingRecording<'a> {
    parent: &'a Histogram,
    start: std::time::Instant,
}

impl<'a> ScanLatencyOngoingRecording<'a> {
    pub(crate) fn start_recording(parent: &'a Histogram) -> ScanLatencyOngoingRecording<'a> {
        let start = Instant::now();
        ScanLatencyOngoingRecording { parent, start }
    }

    pub(crate) fn observe(self) {
        let elapsed = self.start.elapsed();
        self.parent.observe(elapsed.as_secs_f64());
    }
}

pub(crate) static GET_VECTORED_LATENCY: Lazy<GetVectoredLatency> = Lazy::new(|| {
    let inner = register_histogram_vec!(
        "pageserver_get_vectored_seconds",
        "Time spent in get_vectored.",
        &["task_kind"],
        CRITICAL_OP_BUCKETS.into(),
    )
    .expect("failed to define a metric");

    GetVectoredLatency {
        map: EnumMap::from_array(std::array::from_fn(|task_kind_idx| {
            let task_kind = TaskKind::from_usize(task_kind_idx);

            if GetVectoredLatency::TRACKED_TASK_KINDS.contains(&task_kind) {
                let task_kind = task_kind.into();
                Some(inner.with_label_values(&[task_kind]))
            } else {
                None
            }
        })),
    }
});

pub(crate) static SCAN_LATENCY: Lazy<ScanLatency> = Lazy::new(|| {
    let inner = register_histogram_vec!(
        "pageserver_scan_seconds",
        "Time spent in scan.",
        &["task_kind"],
        CRITICAL_OP_BUCKETS.into(),
    )
    .expect("failed to define a metric");

    ScanLatency {
        map: EnumMap::from_array(std::array::from_fn(|task_kind_idx| {
            let task_kind = TaskKind::from_usize(task_kind_idx);

            if ScanLatency::TRACKED_TASK_KINDS.contains(&task_kind) {
                let task_kind = task_kind.into();
                Some(inner.with_label_values(&[task_kind]))
            } else {
                None
            }
        })),
    }
});

pub(crate) static WAIT_LSN_TIME: Lazy<Histogram> = Lazy::new(|| {
    register_histogram!(
        "pageserver_wait_lsn_seconds",
        "Time spent waiting for WAL to arrive. Updated on completion of the wait_lsn operation.",
        CRITICAL_OP_BUCKETS.into(),
    )
    .expect("failed to define a metric")
});

pub(crate) static WAIT_LSN_START_FINISH_COUNTERPAIR: Lazy<IntCounterPairVec> = Lazy::new(|| {
    register_int_counter_pair_vec!(
        "pageserver_wait_lsn_started_count",
        "Number of wait_lsn operations started.",
        "pageserver_wait_lsn_finished_count",
        "Number of wait_lsn operations finished.",
        &["branch_id"],
    )
    .expect("failed to define a metric")
});

pub(crate) static WAIT_LSN_IN_PROGRESS_MICROS: Lazy<IntCounterVec> = Lazy::new(|| {
    register_int_counter_vec!(
        "pageserver_wait_lsn_in_progress_micros",
        "Time spent waiting for WAL to arrive, by branch_id. Updated periodically while waiting.",
        &["branch_id"],
    )
    .expect("failed to define a metric")
});

pub(crate) static WAIT_LSN_IN_PROGRESS_GLOBAL_MICROS: Lazy<IntCounter> = Lazy::new(|| {
    register_int_counter!(
        "pageserver_wait_lsn_in_progress_micros_global",
        "Time spent waiting for WAL to arrive, globally. Updated periodically while waiting."
    )
    .expect("failed to define a metric")
});

static LAST_RECORD_LSN: Lazy<IntGaugeVec> = Lazy::new(|| {
    register_int_gauge_vec!(
        "pageserver_last_record_lsn",
        "Last record LSN grouped by timeline",
        &["branch_id"]
    )
    .expect("failed to define a metric")
});

#[allow(dead_code)]
pub(crate) static PROJECTED_REMOTE_CONSISTENT_LSN: Lazy<UIntGaugeVec> = Lazy::new(|| {
    register_uint_gauge_vec!(
        "pageserver_projected_remote_consistent_lsn",
        "Projected remote consistent LSN grouped by timeline",
        &["branch_id"]
    )
    .expect("failed to define a metric")
});

static PITR_HISTORY_SIZE: Lazy<UIntGaugeVec> = Lazy::new(|| {
    register_uint_gauge_vec!(
        "pageserver_pitr_history_size",
        "Data written since PITR cutoff on this timeline",
        &["branch_id"]
    )
    .expect("failed to define a metric")
});

#[derive(
    strum_macros::EnumIter,
    strum_macros::EnumString,
    strum_macros::Display,
    strum_macros::IntoStaticStr,
)]
#[strum(serialize_all = "kebab_case")]
pub(crate) enum LayerKind {
    Delta,
    Image,
}

#[derive(
    strum_macros::EnumIter,
    strum_macros::EnumString,
    strum_macros::Display,
    strum_macros::IntoStaticStr,
)]
#[strum(serialize_all = "kebab_case")]
pub(crate) enum LayerLevel {
    // We don't track the currently open ephemeral layer, since there's always exactly 1 and its
    // size changes. See `TIMELINE_EPHEMERAL_BYTES`.
    Frozen,
    L0,
    L1,
}

static TIMELINE_ARCHIVE_SIZE: Lazy<UIntGaugeVec> = Lazy::new(|| {
    register_uint_gauge_vec!(
        "pageserver_archive_size",
        "Timeline's logical size if it is considered eligible for archival (outside PITR window), else zero",
        &["branch_id"]
    )
    .expect("failed to define a metric")
});

static STANDBY_HORIZON: Lazy<IntGaugeVec> = Lazy::new(|| {
    register_int_gauge_vec!(
        "pageserver_standby_horizon",
        "Standby apply LSN for which GC is hold off, by timeline.",
        &["branch_id"]
    )
    .expect("failed to define a metric")
});

static RESIDENT_PHYSICAL_SIZE: Lazy<UIntGaugeVec> = Lazy::new(|| {
    register_uint_gauge_vec!(
        "pageserver_resident_physical_size",
        "The size of the layer files present in the pageserver's filesystem, for attached locations.",
        &["branch_id"]
    )
    .expect("failed to define a metric")
});

static VISIBLE_PHYSICAL_SIZE: Lazy<UIntGaugeVec> = Lazy::new(|| {
    register_uint_gauge_vec!(
        "pageserver_visible_physical_size",
        "The size of the layer files present in the pageserver's filesystem.",
        &["branch_id"]
    )
    .expect("failed to define a metric")
});

pub(crate) static RESIDENT_PHYSICAL_SIZE_GLOBAL: Lazy<UIntGauge> = Lazy::new(|| {
    register_uint_gauge!(
        "pageserver_resident_physical_size_global",
        "Like `pageserver_resident_physical_size`, but without tenant/timeline dimensions."
    )
    .expect("failed to define a metric")
});

#[allow(dead_code)]
static REMOTE_PHYSICAL_SIZE: Lazy<UIntGaugeVec> = Lazy::new(|| {
    register_uint_gauge_vec!(
        "pageserver_remote_physical_size",
        "The size of the layer files present in the remote storage that are listed in the remote index_part.json.",
        // Corollary: If any files are missing from the index part, they won't be included here.
        &["branch_id"]
    )
    .expect("failed to define a metric")
});

#[allow(dead_code)]
static REMOTE_PHYSICAL_SIZE_GLOBAL: Lazy<UIntGauge> = Lazy::new(|| {
    register_uint_gauge!(
        "pageserver_remote_physical_size_global",
        "Like `pageserver_remote_physical_size`, but without tenant/timeline dimensions."
    )
    .expect("failed to define a metric")
});

static CURRENT_LOGICAL_SIZE: Lazy<UIntGaugeVec> = Lazy::new(|| {
    register_uint_gauge_vec!(
        "pageserver_current_logical_size",
        "Current logical size grouped by timeline",
        &["branch_id"]
    )
    .expect("failed to define current logical size metric")
});

static AUX_FILE_SIZE: Lazy<IntGaugeVec> = Lazy::new(|| {
    register_int_gauge_vec!(
        "pageserver_aux_file_estimated_size",
        "The size of all aux files for a timeline in aux file v2 store.",
        &["branch_id"]
    )
    .expect("failed to define a metric")
});

static VALID_LSN_LEASE_COUNT: Lazy<UIntGaugeVec> = Lazy::new(|| {
    register_uint_gauge_vec!(
        "pageserver_valid_lsn_lease_count",
        "The number of valid leases after refreshing gc info.",
        &["branch_id"],
    )
    .expect("failed to define a metric")
});

pub(crate) static CIRCUIT_BREAKERS_BROKEN: Lazy<IntCounter> = Lazy::new(|| {
    register_int_counter!(
        "pageserver_circuit_breaker_broken",
        "How many times a circuit breaker has broken"
    )
    .expect("failed to define a metric")
});

pub(crate) static CIRCUIT_BREAKERS_UNBROKEN: Lazy<IntCounter> = Lazy::new(|| {
    register_int_counter!(
        "pageserver_circuit_breaker_unbroken",
        "How many times a circuit breaker has been un-broken (recovered)"
    )
    .expect("failed to define a metric")
});

pub(crate) static RELSIZE_LATEST_CACHE_ENTRIES: Lazy<UIntGauge> = Lazy::new(|| {
    register_uint_gauge!(
        "pageserver_relsize_latest_cache_entries",
        "Number of entries in the latest relation size cache",
    )
    .expect("failed to define a metric")
});

pub(crate) static RELSIZE_LATEST_CACHE_HITS: Lazy<IntCounter> = Lazy::new(|| {
    register_int_counter!(
        "pageserver_relsize_latest_cache_hits",
        "Latest relation size cache hits",
    )
    .expect("failed to define a metric")
});

pub(crate) static RELSIZE_LATEST_CACHE_MISSES: Lazy<IntCounter> = Lazy::new(|| {
    register_int_counter!(
        "pageserver_relsize_latest_cache_misses",
        "Relation size latest cache misses",
    )
    .expect("failed to define a metric")
});

pub(crate) static RELSIZE_SNAPSHOT_CACHE_ENTRIES: Lazy<UIntGauge> = Lazy::new(|| {
    register_uint_gauge!(
        "pageserver_relsize_snapshot_cache_entries",
        "Number of entries in the pitr relation size cache",
    )
    .expect("failed to define a metric")
});

pub(crate) static RELSIZE_SNAPSHOT_CACHE_HITS: Lazy<IntCounter> = Lazy::new(|| {
    register_int_counter!(
        "pageserver_relsize_snapshot_cache_hits",
        "Pitr relation size cache hits",
    )
    .expect("failed to define a metric")
});

pub(crate) static RELSIZE_SNAPSHOT_CACHE_MISSES: Lazy<IntCounter> = Lazy::new(|| {
    register_int_counter!(
        "pageserver_relsize_snapshot_cache_misses",
        "Relation size snapshot cache misses",
    )
    .expect("failed to define a metric")
});

pub(crate) static RELSIZE_CACHE_MISSES_OLD: Lazy<IntCounter> = Lazy::new(|| {
    register_int_counter!(
        "pageserver_relsize_cache_misses_old",
        "Relation size cache misses where the lookup LSN is older than the last relation update"
    )
    .expect("failed to define a metric")
});

pub(crate) mod initial_logical_size {
    use metrics::{IntCounter, IntCounterVec, register_int_counter, register_int_counter_vec};
    use once_cell::sync::Lazy;

    pub(crate) struct StartCalculation(IntCounterVec);
    pub(crate) static START_CALCULATION: Lazy<StartCalculation> = Lazy::new(|| {
        StartCalculation(
            register_int_counter_vec!(
                "pageserver_initial_logical_size_start_calculation",
                "Incremented each time we start an initial logical size calculation attempt. \
                 The `circumstances` label provides some additional details.",
                &["attempt", "circumstances"]
            )
            .unwrap(),
        )
    });

    struct DropCalculation {
        first: IntCounter,
        retry: IntCounter,
    }

    static DROP_CALCULATION: Lazy<DropCalculation> = Lazy::new(|| {
        let vec = register_int_counter_vec!(
            "pageserver_initial_logical_size_drop_calculation",
            "Incremented each time we abort a started size calculation attmpt.",
            &["attempt"]
        )
        .unwrap();
        DropCalculation {
            first: vec.with_label_values(&["first"]),
            retry: vec.with_label_values(&["retry"]),
        }
    });

    pub(crate) struct Calculated {
        pub(crate) births: IntCounter,
        pub(crate) deaths: IntCounter,
    }

    pub(crate) static CALCULATED: Lazy<Calculated> = Lazy::new(|| Calculated {
        births: register_int_counter!(
            "pageserver_initial_logical_size_finish_calculation",
            "Incremented every time we finish calculation of initial logical size.\
             If everything is working well, this should happen at most once per Timeline object."
        )
        .unwrap(),
        deaths: register_int_counter!(
            "pageserver_initial_logical_size_drop_finished_calculation",
            "Incremented when we drop a finished initial logical size calculation result.\
             Mainly useful to turn pageserver_initial_logical_size_finish_calculation into a gauge."
        )
        .unwrap(),
    });

    pub(crate) struct OngoingCalculationGuard {
        inc_drop_calculation: Option<IntCounter>,
    }

    #[derive(strum_macros::IntoStaticStr)]
    pub(crate) enum StartCircumstances {
        EmptyInitial,
        SkippedConcurrencyLimiter,
        AfterBackgroundTasksRateLimit,
    }

    impl StartCalculation {
        pub(crate) fn first(&self, circumstances: StartCircumstances) -> OngoingCalculationGuard {
            let circumstances_label: &'static str = circumstances.into();
            self.0
                .with_label_values(&["first", circumstances_label])
                .inc();
            OngoingCalculationGuard {
                inc_drop_calculation: Some(DROP_CALCULATION.first.clone()),
            }
        }
        pub(crate) fn retry(&self, circumstances: StartCircumstances) -> OngoingCalculationGuard {
            let circumstances_label: &'static str = circumstances.into();
            self.0
                .with_label_values(&["retry", circumstances_label])
                .inc();
            OngoingCalculationGuard {
                inc_drop_calculation: Some(DROP_CALCULATION.retry.clone()),
            }
        }
    }

    impl Drop for OngoingCalculationGuard {
        fn drop(&mut self) {
            if let Some(counter) = self.inc_drop_calculation.take() {
                counter.inc();
            }
        }
    }

    impl OngoingCalculationGuard {
        pub(crate) fn calculation_result_saved(mut self) -> FinishedCalculationGuard {
            drop(self.inc_drop_calculation.take());
            CALCULATED.births.inc();
            FinishedCalculationGuard {
                inc_on_drop: CALCULATED.deaths.clone(),
            }
        }
    }

    pub(crate) struct FinishedCalculationGuard {
        inc_on_drop: IntCounter,
    }

    impl Drop for FinishedCalculationGuard {
        fn drop(&mut self) {
            self.inc_on_drop.inc();
        }
    }

    // context: https://github.com/neondatabase/neon/issues/5963
    pub(crate) static TIMELINES_WHERE_WALRECEIVER_GOT_APPROXIMATE_SIZE: Lazy<IntCounter> =
        Lazy::new(|| {
            register_int_counter!(
                "pageserver_initial_logical_size_timelines_where_walreceiver_got_approximate_size",
                "Counter for the following event: walreceiver calls\
                 Timeline::get_current_logical_size() and it returns `Approximate` for the first time."
            )
            .unwrap()
        });
}

static DIRECTORY_ENTRIES_COUNT: Lazy<UIntGaugeVec> = Lazy::new(|| {
    register_uint_gauge_vec!(
        "pageserver_directory_entries_count",
        "Sum of the entries in pageserver-stored directory listings",
        &["branch_id"]
    )
    .expect("failed to define a metric")
});

pub(crate) static TIMELINE_STATE_METRIC: Lazy<UIntGaugeVec> = Lazy::new(|| {
    register_uint_gauge_vec!(
        "pageserver_timeline_states_count",
        "Count of timelines per state",
        &["state"]
    )
    .expect("Failed to register pageserver_timeline_states_count metric")
});

static EVICTIONS_WITH_LOW_RESIDENCE_DURATION: Lazy<IntCounterVec> = Lazy::new(|| {
    register_int_counter_vec!(
        "pageserver_evictions_with_low_residence_duration",
        "If a layer is evicted that was resident for less than `low_threshold`, it is counted to this counter. \
         Residence duration is determined using the `residence_duration_data_source`.",
        &["branch_id", "residence_duration_data_source", "low_threshold_secs"]
    )
    .expect("failed to define a metric")
});

pub(crate) static UNEXPECTED_ONDEMAND_DOWNLOADS: Lazy<IntCounter> = Lazy::new(|| {
    register_int_counter!(
        "pageserver_unexpected_ondemand_downloads_count",
        "Number of unexpected on-demand downloads. \
         We log more context for each increment, so, forgo any labels in this metric.",
    )
    .expect("failed to define a metric")
});

/// How long did we take to start up?  Broken down by labels to describe
/// different phases of startup.
pub static STARTUP_DURATION: Lazy<GaugeVec> = Lazy::new(|| {
    register_gauge_vec!(
        "pageserver_startup_duration_seconds",
        "Time taken by phases of pageserver startup, in seconds",
        &["phase"]
    )
    .expect("Failed to register pageserver_startup_duration_seconds metric")
});

pub static STARTUP_IS_LOADING: Lazy<UIntGauge> = Lazy::new(|| {
    register_uint_gauge!(
        "pageserver_startup_is_loading",
        "1 while in initial startup load of tenants, 0 at other times"
    )
    .expect("Failed to register pageserver_startup_is_loading")
});

/// Metrics related to the lifecycle of a [`crate::page_server::PageServer`] object: things
/// like how long it took to load.
///
/// Note that these are process-global metrics, _not_ per-tenant metrics.  Per-tenant
/// metrics are rather expensive, and usually fine grained stuff makes more sense
/// at a timeline level than tenant level.
pub(crate) struct TenantMetrics {
    pub(crate) preload: Histogram,
    pub(crate) attach: Histogram,

    /// How many tenants are included in the initial startup of the pagesrever?
    pub(crate) startup_scheduled: IntCounter,
    pub(crate) startup_complete: IntCounter,
}

pub(crate) static TENANT: Lazy<TenantMetrics> = Lazy::new(|| {
    TenantMetrics {
    preload: register_histogram!(
        "pageserver_tenant_preload_seconds",
        "Time taken by tenants to load remote metadata on startup/attach, in seconds",
        CRITICAL_OP_BUCKETS.into()
    )
    .expect("Failed to register metric"),
    attach: register_histogram!(
        "pageserver_tenant_attach_seconds",
        "Time taken by tenants to intialize, after remote metadata is already loaded",
        CRITICAL_OP_BUCKETS.into()
    )
    .expect("Failed to register metric"),
    startup_scheduled: register_int_counter!(
        "pageserver_tenant_startup_scheduled",
        "Number of tenants included in pageserver startup (doesn't count tenants attached later)"
    ).expect("Failed to register metric"),
    startup_complete: register_int_counter!(
        "pageserver_tenant_startup_complete",
        "Number of tenants that have completed warm-up, or activated on-demand during initial startup: \
         should eventually reach `pageserver_tenant_startup_scheduled_total`.  Does not include broken \
         tenants: such cases will lead to this metric never reaching the scheduled count."
    ).expect("Failed to register metric"),
}
});

/// Each `Timeline`'s  [`EVICTIONS_WITH_LOW_RESIDENCE_DURATION`] metric.
#[derive(Debug)]
pub(crate) struct EvictionsWithLowResidenceDuration {
    data_source: &'static str,
    threshold: Duration,
    counter: Option<IntCounter>,
}

pub(crate) struct EvictionsWithLowResidenceDurationBuilder {
    data_source: &'static str,
    threshold: Duration,
}

impl EvictionsWithLowResidenceDurationBuilder {
    pub fn new(data_source: &'static str, threshold: Duration) -> Self {
        Self {
            data_source,
            threshold,
        }
    }

    fn build(&self, branch_id: &str) -> EvictionsWithLowResidenceDuration {
        let counter = EVICTIONS_WITH_LOW_RESIDENCE_DURATION
            .get_metric_with_label_values(&[
                branch_id,
                self.data_source,
                &EvictionsWithLowResidenceDuration::threshold_label_value(self.threshold),
            ])
            .unwrap();
        EvictionsWithLowResidenceDuration {
            data_source: self.data_source,
            threshold: self.threshold,
            counter: Some(counter),
        }
    }
}

impl EvictionsWithLowResidenceDuration {
    fn threshold_label_value(threshold: Duration) -> String {
        format!("{}", threshold.as_secs())
    }

    #[allow(dead_code)]
    pub fn change_threshold(&mut self, branch_id: &str, new_threshold: Duration) {
        if new_threshold == self.threshold {
            return;
        }
        let mut with_new =
            EvictionsWithLowResidenceDurationBuilder::new(self.data_source, new_threshold)
                .build(branch_id);
        std::mem::swap(self, &mut with_new);
        with_new.remove(branch_id);
    }

    // This could be a `Drop` impl, but, we need the `branch_id`.
    fn remove(&mut self, branch_id: &str) {
        let Some(_counter) = self.counter.take() else {
            return;
        };

        let threshold = Self::threshold_label_value(self.threshold);

        let removed = EVICTIONS_WITH_LOW_RESIDENCE_DURATION.remove_label_values(&[
            branch_id,
            self.data_source,
            &threshold,
        ]);

        match removed {
            Err(e) => {
                // this has been hit in staging as
                // <https://neondatabase.sentry.io/issues/4142396994/>, but we don't know how.
                // because we can be in the drop path already, don't risk:
                // - "double-panic => illegal instruction" or
                // - future "drop panick => abort"
                //
                // so just nag: (the error has the labels)
                tracing::warn!(
                    "failed to remove EvictionsWithLowResidenceDuration, it was already removed? {e:#?}"
                );
            }
            Ok(()) => {
                // to help identify cases where we double-remove the same values, let's log all
                // deletions?
                tracing::info!(
                    "removed EvictionsWithLowResidenceDuration with {branch_id}, {branch_id}, {}, {threshold}",
                    self.data_source
                );
            }
        }
    }
}

#[derive(Clone, Copy)]
#[repr(usize)]
pub(crate) enum StorageIoSizeOperation {
    Read,
    Write,
}

impl StorageIoSizeOperation {
    pub(crate) const VARIANTS: &'static [&'static str] = &["read", "write"];

    fn as_str(&self) -> &'static str {
        Self::VARIANTS[*self as usize]
    }
}

// Needed for the https://neonprod.grafana.net/d/5uK9tHL4k/picking-tenant-for-relocation?orgId=1
pub(crate) static STORAGE_IO_SIZE: Lazy<UIntGaugeVec> = Lazy::new(|| {
    register_uint_gauge_vec!(
        "pageserver_io_operations_bytes_total",
        "Total amount of bytes read/written in IO operations",
        &["operation", "branch_id"]
    )
    .expect("failed to define a metric")
});

#[derive(Clone, Debug)]
pub(crate) struct StorageIoSizeMetrics {
    #[allow(dead_code)]
    pub read: UIntGauge,
    #[allow(dead_code)]
    pub write: UIntGauge,
}

impl StorageIoSizeMetrics {
    pub(crate) fn new(branch_id: &str) -> Self {
        let read = STORAGE_IO_SIZE
            .get_metric_with_label_values(&[StorageIoSizeOperation::Read.as_str(), branch_id])
            .unwrap();
        let write = STORAGE_IO_SIZE
            .get_metric_with_label_values(&[StorageIoSizeOperation::Write.as_str(), branch_id])
            .unwrap();
        Self { read, write }
    }
}

pub(crate) struct SmgrOpTimer(Option<SmgrOpTimerInner>);
pub(crate) struct SmgrOpTimerInner {
    global_execution_latency_histo: Histogram,
    per_timeline_execution_latency_histo: Option<Histogram>,

    global_batch_wait_time: Histogram,
    per_timeline_batch_wait_time: Histogram,

    global_flush_in_progress_micros: IntCounter,
    per_timeline_flush_in_progress_micros: IntCounter,

    throttling: Arc<tenant_throttling::Pagestream>,

    timings: SmgrOpTimerState,
}

/// The stages of request processing are represented by the enum variants.
/// Used as part of [`SmgrOpTimerInner::timings`].
///
/// Request processing calls into the `SmgrOpTimer::observe_*` methods at the
/// transition points.
/// These methods bump relevant counters and then update [`SmgrOpTimerInner::timings`]
/// to the next state.
///
/// Each request goes through every stage, in all configurations.
///
#[derive(Debug)]
enum SmgrOpTimerState {
    Received {
        // In the future, we may want to track the full time the request spent
        // inside pageserver process (time spent in kernel buffers can't be tracked).
        // `received_at` would be used for that.
        #[allow(dead_code)]
        received_at: Instant,
    },
    Throttling {
        throttle_started_at: Instant,
    },
    Batching {
        throttle_done_at: Instant,
    },
    Executing {
        execution_started_at: Instant,
    },
    Flushing,
    // NB: when adding observation points, remember to update the Drop impl.
}

// NB: when adding observation points, remember to update the Drop impl.
impl SmgrOpTimer {
    /// See [`SmgrOpTimerState`] for more context.
    pub(crate) fn observe_throttle_start(&mut self, at: Instant) {
        let Some(inner) = self.0.as_mut() else {
            return;
        };
        let SmgrOpTimerState::Received { received_at: _ } = &mut inner.timings else {
            return;
        };
        inner.throttling.count_accounted_start.inc();
        inner.timings = SmgrOpTimerState::Throttling {
            throttle_started_at: at,
        };
    }

    /// See [`SmgrOpTimerState`] for more context.
    pub(crate) fn observe_throttle_done(&mut self, throttle: ThrottleResult) {
        let Some(inner) = self.0.as_mut() else {
            return;
        };
        let SmgrOpTimerState::Throttling {
            throttle_started_at,
        } = &inner.timings
        else {
            return;
        };
        inner.throttling.count_accounted_finish.inc();
        match throttle {
            ThrottleResult::NotThrottled { end } => {
                inner.timings = SmgrOpTimerState::Batching {
                    throttle_done_at: end,
                };
            }
            ThrottleResult::Throttled { end } => {
                // update metrics
                inner.throttling.count_throttled.inc();
                inner
                    .throttling
                    .wait_time
                    .inc_by((end - *throttle_started_at).as_micros().try_into().unwrap());
                // state transition
                inner.timings = SmgrOpTimerState::Batching {
                    throttle_done_at: end,
                };
            }
        }
    }

    /// See [`SmgrOpTimerState`] for more context.
    pub(crate) fn observe_execution_start(&mut self, at: Instant) {
        let Some(inner) = self.0.as_mut() else {
            return;
        };
        let SmgrOpTimerState::Batching { throttle_done_at } = &inner.timings else {
            return;
        };
        // update metrics
        let batch = at - *throttle_done_at;
        inner.global_batch_wait_time.observe(batch.as_secs_f64());
        inner
            .per_timeline_batch_wait_time
            .observe(batch.as_secs_f64());
        // state transition
        inner.timings = SmgrOpTimerState::Executing {
            execution_started_at: at,
        }
    }

    /// For all but the first caller, this is a no-op.
    /// The first callers receives Some, subsequent ones None.
    ///
    /// See [`SmgrOpTimerState`] for more context.
    pub(crate) fn observe_execution_end(&mut self, at: Instant) -> Option<SmgrOpFlushInProgress> {
        // NB: unlike the other observe_* methods, this one take()s.
        #[allow(clippy::question_mark)] // maintain similar code pattern.
        let Some(mut inner) = self.0.take() else {
            return None;
        };
        let SmgrOpTimerState::Executing {
            execution_started_at,
        } = &inner.timings
        else {
            return None;
        };
        // update metrics
        let execution = at - *execution_started_at;
        inner
            .global_execution_latency_histo
            .observe(execution.as_secs_f64());
        if let Some(per_timeline_execution_latency_histo) =
            &inner.per_timeline_execution_latency_histo
        {
            per_timeline_execution_latency_histo.observe(execution.as_secs_f64());
        }

        // state transition
        inner.timings = SmgrOpTimerState::Flushing;

        // return the flush in progress object which
        // will do the remaining metrics updates
        let SmgrOpTimerInner {
            global_flush_in_progress_micros,
            per_timeline_flush_in_progress_micros,
            ..
        } = inner;
        Some(SmgrOpFlushInProgress {
            global_micros: global_flush_in_progress_micros,
            per_timeline_micros: per_timeline_flush_in_progress_micros,
        })
    }
}

/// The last stage of request processing is serializing and flushing the request
/// into the TCP connection. We want to make slow flushes observable
/// _while they are occuring_, so this struct provides a wrapper method [`Self::measure`]
/// to periodically bump the metric.
///
/// If in the future we decide that we're not interested in live updates, we can
/// add another `observe_*` method to [`SmgrOpTimer`], follow the existing pattern there,
/// and remove this struct from the code base.
pub(crate) struct SmgrOpFlushInProgress {
    global_micros: IntCounter,
    per_timeline_micros: IntCounter,
}

impl Drop for SmgrOpTimer {
    fn drop(&mut self) {
        // In case of early drop, update any of the remaining metrics with
        // observations so that (started,finished) counter pairs balance out
        // and all counters on the latency path have the the same number of
        // observations.
        // It's technically lying and it would be better if each metric had
        // a separate label or similar for cancelled requests.
        // But we don't have that right now and counter pairs balancing
        // out is useful when using the metrics in panels and whatnot.
        let now = Instant::now();
        self.observe_throttle_start(now);
        self.observe_throttle_done(ThrottleResult::NotThrottled { end: now });
        self.observe_execution_start(now);
        let maybe_flush_timer = self.observe_execution_end(now);
        drop(maybe_flush_timer);
    }
}

impl SmgrOpFlushInProgress {
    /// The caller must guarantee that `socket_fd`` outlives this function.
    pub(crate) async fn measure<Fut, O>(self, started_at: Instant, fut: Fut, socket_fd: RawFd) -> O
    where
        Fut: std::future::Future<Output = O>,
    {
        let mut fut = std::pin::pin!(fut);

        let mut logged = false;
        let mut last_counter_increment_at = started_at;
        let mut observe_guard = scopeguard::guard(
            |is_timeout| {
                let now = Instant::now();

                // Increment counter
                {
                    let elapsed_since_last_observe = now - last_counter_increment_at;
                    self.global_micros
                        .inc_by(u64::try_from(elapsed_since_last_observe.as_micros()).unwrap());
                    self.per_timeline_micros
                        .inc_by(u64::try_from(elapsed_since_last_observe.as_micros()).unwrap());
                    last_counter_increment_at = now;
                }

                // Log something on every timeout, and on completion but only if we hit a timeout.
                if is_timeout || logged {
                    logged = true;
                    let elapsed_total = now - started_at;
                    let msg = if is_timeout {
                        "slow flush ongoing"
                    } else {
                        "slow flush completed or cancelled"
                    };

                    let (inq, outq) = {
                        // SAFETY: caller guarantees that `socket_fd` outlives this function.
                        #[cfg(target_os = "linux")]
                        unsafe {
                            (
                                utils::linux_socket_ioctl::inq(socket_fd).unwrap_or(-2),
                                utils::linux_socket_ioctl::outq(socket_fd).unwrap_or(-2),
                            )
                        }
                        #[cfg(not(target_os = "linux"))]
                        {
                            _ = socket_fd; // appease unused lint on macOS
                            (-1, -1)
                        }
                    };

                    let elapsed_total_secs = format!("{:.6}", elapsed_total.as_secs_f64());
                    tracing::info!(elapsed_total_secs, inq, outq, msg);
                }
            },
            |mut observe| {
                observe(false);
            },
        );

        loop {
            match tokio::time::timeout(Duration::from_secs(10), &mut fut).await {
                Ok(v) => return v,
                Err(_timeout) => {
                    (*observe_guard)(true);
                }
            }
        }
    }
}

#[derive(
    Debug,
    Clone,
    Copy,
    IntoStaticStr,
    strum_macros::EnumCount,
    strum_macros::EnumIter,
    strum_macros::FromRepr,
    enum_map::Enum,
)]
#[strum(serialize_all = "snake_case")]
pub enum SmgrQueryType {
    GetRelExists,
    GetRelSize,
    GetPageAtLsn,
    GetDbSize,
    GetSlruSegment,
    #[cfg(feature = "testing")]
    Test,
}

#[derive(
    Debug,
    Clone,
    Copy,
    IntoStaticStr,
    strum_macros::EnumCount,
    strum_macros::EnumIter,
    strum_macros::FromRepr,
    enum_map::Enum,
)]
#[strum(serialize_all = "snake_case")]
pub enum GetPageBatchBreakReason {
    BatchFull,
    NonBatchableRequest,
    NonUniformLsn,
    SamePageAtDifferentLsn,
    NonUniformTimeline,
    ExecutorSteal,
    #[cfg(feature = "testing")]
    NonUniformKey,
}

pub(crate) struct SmgrQueryTimePerTimeline {
    global_started: [IntCounter; SmgrQueryType::COUNT],
    global_latency: [Histogram; SmgrQueryType::COUNT],
    per_timeline_getpage_started: IntCounter,
    per_timeline_getpage_latency: Histogram,
    global_batch_size: Histogram,
    per_timeline_batch_size: Histogram,
    global_flush_in_progress_micros: IntCounter,
    per_timeline_flush_in_progress_micros: IntCounter,
    global_batch_wait_time: Histogram,
    per_timeline_batch_wait_time: Histogram,
    global_batch_break_reason: [IntCounter; GetPageBatchBreakReason::COUNT],
    per_timeline_batch_break_reason: GetPageBatchBreakReasonTimelineMetrics,
    throttling: Arc<tenant_throttling::Pagestream>,
}

static SMGR_QUERY_STARTED_GLOBAL: Lazy<IntCounterVec> = Lazy::new(|| {
    register_int_counter_vec!(
        // it's a counter, but, name is prepared to extend it to a histogram of queue depth
        "pageserver_smgr_query_started_global_count",
        "Number of smgr queries started, aggregated by query type.",
        &["smgr_query_type"],
    )
    .expect("failed to define a metric")
});

static SMGR_QUERY_STARTED_PER_TENANT_TIMELINE: Lazy<IntCounterVec> = Lazy::new(|| {
    register_int_counter_vec!(
        // it's a counter, but, name is prepared to extend it to a histogram of queue depth
        "pageserver_smgr_query_started_count",
        "Number of smgr queries started, aggregated by query type and tenant/timeline.",
        &["smgr_query_type", "branch_id"],
    )
    .expect("failed to define a metric")
});

/// Per-timeline smgr histogram buckets should be the same as the compute buckets, such that the
/// metrics are comparable across compute and Pageserver. See also:
/// <https://github.com/neondatabase/neon/blob/1a87975d956a8ad17ec8b85da32a137ec4893fcc/pgxn/neon/neon_perf_counters.h#L18-L27>
/// <https://github.com/neondatabase/flux-fleet/blob/556182a939edda87ff1d85a6b02e5cec901e0e9e/apps/base/compute-metrics/scrape-compute-sql-exporter.yaml#L29-L35>
static SMGR_QUERY_TIME_PER_TENANT_TIMELINE_BUCKETS: &[f64] =
    &[0.0006, 0.001, 0.003, 0.006, 0.01, 0.03, 0.1, 1.0, 3.0];

static SMGR_QUERY_TIME_PER_TENANT_TIMELINE: Lazy<HistogramVec> = Lazy::new(|| {
    register_histogram_vec!(
        "pageserver_smgr_query_seconds",
        "Time spent _executing_ smgr query handling, excluding batch and throttle delays.",
        &["smgr_query_type", "branch_id"],
        SMGR_QUERY_TIME_PER_TENANT_TIMELINE_BUCKETS.into(),
    )
    .expect("failed to define a metric")
});

static SMGR_QUERY_TIME_GLOBAL_BUCKETS: Lazy<Vec<f64>> = Lazy::new(|| {
    [
        1,
        10,
        20,
        40,
        60,
        80,
        100,
        200,
        300,
        400,
        500,
        600,
        700,
        800,
        900,
        1_000, // 1ms
        2_000,
        4_000,
        6_000,
        8_000,
        10_000, // 10ms
        20_000,
        40_000,
        60_000,
        80_000,
        100_000,
        200_000,
        400_000,
        600_000,
        800_000,
        1_000_000, // 1s
        2_000_000,
        4_000_000,
        6_000_000,
        8_000_000,
        10_000_000, // 10s
        20_000_000,
        50_000_000,
        100_000_000,
        200_000_000,
        1_000_000_000, // 1000s
    ]
    .into_iter()
    .map(Duration::from_micros)
    .map(|d| d.as_secs_f64())
    .collect()
});

static SMGR_QUERY_TIME_GLOBAL: Lazy<HistogramVec> = Lazy::new(|| {
    register_histogram_vec!(
        "pageserver_smgr_query_seconds_global",
        "Like pageserver_smgr_query_seconds, but aggregated to instance level.",
        &["smgr_query_type"],
        SMGR_QUERY_TIME_GLOBAL_BUCKETS.clone(),
    )
    .expect("failed to define a metric")
});

static PAGE_SERVICE_BATCH_SIZE_BUCKETS_GLOBAL: Lazy<Vec<f64>> = Lazy::new(|| {
    (1..=u32::try_from(DEFAULT_MAX_GET_VECTORED_KEYS).unwrap())
        .map(|v| v.into())
        .collect()
});

static PAGE_SERVICE_BATCH_SIZE_GLOBAL: Lazy<Histogram> = Lazy::new(|| {
    register_histogram!(
        "pageserver_page_service_batch_size_global",
        "Batch size of pageserver page service requests",
        PAGE_SERVICE_BATCH_SIZE_BUCKETS_GLOBAL.clone(),
    )
    .expect("failed to define a metric")
});

static PAGE_SERVICE_BATCH_SIZE_BUCKETS_PER_TIMELINE: Lazy<Vec<f64>> = Lazy::new(|| {
    let mut buckets = Vec::new();
    for i in 0.. {
        let bucket = 1 << i;
        if bucket > u32::try_from(DEFAULT_MAX_GET_VECTORED_KEYS).unwrap() {
            break;
        }
        buckets.push(bucket.into());
    }
    buckets
});

static PAGE_SERVICE_BATCH_SIZE_PER_TENANT_TIMELINE: Lazy<HistogramVec> = Lazy::new(|| {
    register_histogram_vec!(
        "pageserver_page_service_batch_size",
        "Batch size of pageserver page service requests",
        &["branch_id"],
        PAGE_SERVICE_BATCH_SIZE_BUCKETS_PER_TIMELINE.clone()
    )
    .expect("failed to define a metric")
});

static PAGE_SERVICE_BATCH_BREAK_REASON_GLOBAL: Lazy<IntCounterVec> = Lazy::new(|| {
    register_int_counter_vec!(
        // it's a counter, but, name is prepared to extend it to a histogram of queue depth
        "pageserver_page_service_batch_break_reason_global",
        "Reason for breaking batches of get page requests",
        &["reason"],
    )
    .expect("failed to define a metric")
});

struct GetPageBatchBreakReasonTimelineMetrics {
    map: EnumMap<GetPageBatchBreakReason, IntCounter>,
}

impl GetPageBatchBreakReasonTimelineMetrics {
    fn new(branch_id: &str) -> Self {
        GetPageBatchBreakReasonTimelineMetrics {
            map: EnumMap::from_array(std::array::from_fn(|reason_idx| {
                let reason = GetPageBatchBreakReason::from_usize(reason_idx);
                PAGE_SERVICE_BATCH_BREAK_REASON_PER_TENANT_TIMELINE
                    .with_label_values(&[branch_id, reason.into()])
            })),
        }
    }

    fn inc(&self, reason: GetPageBatchBreakReason) {
        self.map[reason].inc()
    }
}

static PAGE_SERVICE_BATCH_BREAK_REASON_PER_TENANT_TIMELINE: Lazy<IntCounterVec> = Lazy::new(|| {
    register_int_counter_vec!(
        "pageserver_page_service_batch_break_reason",
        "Reason for breaking batches of get page requests",
        &["branch_id", "reason"],
    )
    .expect("failed to define a metric")
});

pub(crate) static PAGE_SERVICE_CONFIG_MAX_BATCH_SIZE: Lazy<IntGaugeVec> = Lazy::new(|| {
    register_int_gauge_vec!(
        "pageserver_page_service_config_max_batch_size",
        "Configured maximum batch size for the server-side batching functionality of page_service. \
         Labels expose more of the configuration parameters.",
        &["mode", "execution", "batching"]
    )
    .expect("failed to define a metric")
});

pub(crate) static PAGE_SERVICE_PIPELINE_INFLIGHT_BATCHES: Lazy<Histogram> = Lazy::new(|| {
    register_histogram!(
        "pageserver_page_service_pipeline_inflight_batches",
        "Number of batches (each a folded getpage set = one get_vectored / YB read request) \
         the executor has in flight, sampled when a new batch is admitted (#173).",
        vec![1.0, 2.0, 3.0, 4.0, 6.0, 8.0, 12.0, 16.0, 24.0, 32.0]
    )
    .expect("failed to define a metric")
});

fn set_page_service_config_max_batch_size(conf: &PageServicePipeliningConfig) {
    PAGE_SERVICE_CONFIG_MAX_BATCH_SIZE.reset();
    let (label_values, value) = match conf {
        PageServicePipeliningConfig::Serial => (["serial", "-", "-"], 1),
        PageServicePipeliningConfig::Pipelined(PageServicePipeliningConfigPipelined {
            max_batch_size,
            execution,
            batching,
            ..
        }) => {
            let mode = "pipelined";
            let execution = match execution {
                PageServiceProtocolPipelinedExecutionStrategy::ConcurrentFutures => {
                    "concurrent-futures"
                }
                PageServiceProtocolPipelinedExecutionStrategy::Tasks => "tasks",
            };
            let batching = match batching {
                PageServiceProtocolPipelinedBatchingStrategy::UniformLsn => "uniform-lsn",
                PageServiceProtocolPipelinedBatchingStrategy::ScatteredLsn => "scattered-lsn",
            };

            ([mode, execution, batching], max_batch_size.get())
        }
    };
    PAGE_SERVICE_CONFIG_MAX_BATCH_SIZE
        .with_label_values(&label_values)
        .set(value.try_into().unwrap());
}

static PAGE_SERVICE_SMGR_FLUSH_INPROGRESS_MICROS: Lazy<IntCounterVec> = Lazy::new(|| {
    register_int_counter_vec!(
        "pageserver_page_service_pagestream_flush_in_progress_micros",
        "Counter that sums up the microseconds that a pagestream response was being flushed into the TCP connection. \
         If the flush is particularly slow, this counter will be updated periodically to make slow flushes \
         easily discoverable in monitoring. \
         Hence, this is NOT a completion latency historgram.",
        &["branch_id"],
    )
    .expect("failed to define a metric")
});

static PAGE_SERVICE_SMGR_FLUSH_INPROGRESS_MICROS_GLOBAL: Lazy<IntCounter> = Lazy::new(|| {
    register_int_counter!(
        "pageserver_page_service_pagestream_flush_in_progress_micros_global",
        "Like pageserver_page_service_pagestream_flush_in_progress_seconds, but instance-wide.",
    )
    .expect("failed to define a metric")
});

static PAGE_SERVICE_SMGR_BATCH_WAIT_TIME: Lazy<HistogramVec> = Lazy::new(|| {
    register_histogram_vec!(
        "pageserver_page_service_pagestream_batch_wait_time_seconds",
        "Time a request spent waiting in its batch until the batch moved to throttle&execution.",
        &["branch_id"],
        SMGR_QUERY_TIME_PER_TENANT_TIMELINE_BUCKETS.into(),
    )
    .expect("failed to define a metric")
});

static PAGE_SERVICE_SMGR_BATCH_WAIT_TIME_GLOBAL: Lazy<Histogram> = Lazy::new(|| {
    register_histogram!(
        "pageserver_page_service_pagestream_batch_wait_time_seconds_global",
        "Like pageserver_page_service_pagestream_batch_wait_time_seconds, but aggregated to instance level.",
        SMGR_QUERY_TIME_GLOBAL_BUCKETS.to_vec(),
    )
    .expect("failed to define a metric")
});

impl SmgrQueryTimePerTimeline {
    pub(crate) fn new(
        branch_id: &BranchId,
        pagestream_throttle_metrics: Arc<tenant_throttling::Pagestream>,
    ) -> Self {
        let branch_id = branch_id.to_string();
        let global_started = std::array::from_fn(|i| {
            let op = SmgrQueryType::from_repr(i).unwrap();
            SMGR_QUERY_STARTED_GLOBAL
                .get_metric_with_label_values(&[op.into()])
                .unwrap()
        });
        let global_latency = std::array::from_fn(|i| {
            let op = SmgrQueryType::from_repr(i).unwrap();
            SMGR_QUERY_TIME_GLOBAL
                .get_metric_with_label_values(&[op.into()])
                .unwrap()
        });

        let per_timeline_getpage_started = SMGR_QUERY_STARTED_PER_TENANT_TIMELINE
            .get_metric_with_label_values(&[SmgrQueryType::GetPageAtLsn.into(), &branch_id])
            .unwrap();
        let per_timeline_getpage_latency = SMGR_QUERY_TIME_PER_TENANT_TIMELINE
            .get_metric_with_label_values(&[SmgrQueryType::GetPageAtLsn.into(), &branch_id])
            .unwrap();

        let global_batch_size = PAGE_SERVICE_BATCH_SIZE_GLOBAL.clone();
        let per_timeline_batch_size = PAGE_SERVICE_BATCH_SIZE_PER_TENANT_TIMELINE
            .get_metric_with_label_values(&[&branch_id])
            .unwrap();

        let global_batch_wait_time = PAGE_SERVICE_SMGR_BATCH_WAIT_TIME_GLOBAL.clone();
        let per_timeline_batch_wait_time = PAGE_SERVICE_SMGR_BATCH_WAIT_TIME
            .get_metric_with_label_values(&[&branch_id])
            .unwrap();

        let global_batch_break_reason = std::array::from_fn(|i| {
            let reason = GetPageBatchBreakReason::from_usize(i);
            PAGE_SERVICE_BATCH_BREAK_REASON_GLOBAL
                .get_metric_with_label_values(&[reason.into()])
                .unwrap()
        });
        let per_timeline_batch_break_reason =
            GetPageBatchBreakReasonTimelineMetrics::new(&branch_id);

        let global_flush_in_progress_micros =
            PAGE_SERVICE_SMGR_FLUSH_INPROGRESS_MICROS_GLOBAL.clone();
        let per_timeline_flush_in_progress_micros = PAGE_SERVICE_SMGR_FLUSH_INPROGRESS_MICROS
            .get_metric_with_label_values(&[&branch_id])
            .unwrap();

        Self {
            global_started,
            global_latency,
            per_timeline_getpage_latency,
            per_timeline_getpage_started,
            global_batch_size,
            per_timeline_batch_size,
            global_flush_in_progress_micros,
            per_timeline_flush_in_progress_micros,
            global_batch_wait_time,
            per_timeline_batch_wait_time,
            global_batch_break_reason,
            per_timeline_batch_break_reason,
            throttling: pagestream_throttle_metrics,
        }
    }
    pub(crate) fn start_smgr_op(&self, op: SmgrQueryType, received_at: Instant) -> SmgrOpTimer {
        self.global_started[op as usize].inc();

        let per_timeline_latency_histo = if matches!(op, SmgrQueryType::GetPageAtLsn) {
            self.per_timeline_getpage_started.inc();
            Some(self.per_timeline_getpage_latency.clone())
        } else {
            None
        };

        SmgrOpTimer(Some(SmgrOpTimerInner {
            global_execution_latency_histo: self.global_latency[op as usize].clone(),
            per_timeline_execution_latency_histo: per_timeline_latency_histo,
            global_flush_in_progress_micros: self.global_flush_in_progress_micros.clone(),
            per_timeline_flush_in_progress_micros: self
                .per_timeline_flush_in_progress_micros
                .clone(),
            global_batch_wait_time: self.global_batch_wait_time.clone(),
            per_timeline_batch_wait_time: self.per_timeline_batch_wait_time.clone(),
            throttling: self.throttling.clone(),
            timings: SmgrOpTimerState::Received { received_at },
        }))
    }

    /// TODO: do something about this? seems odd, we have a similar call on SmgrOpTimer
    pub(crate) fn observe_getpage_batch_start(
        &self,
        batch_size: usize,
        break_reason: GetPageBatchBreakReason,
    ) {
        self.global_batch_size.observe(batch_size as f64);
        self.per_timeline_batch_size.observe(batch_size as f64);

        self.global_batch_break_reason[break_reason.into_usize()].inc();
        self.per_timeline_batch_break_reason.inc(break_reason);
    }
}

// keep in sync with control plane Go code so that we can validate
// compute's basebackup_ms metric with our perspective in the context of SLI/SLO.
static COMPUTE_STARTUP_BUCKETS: Lazy<[f64; 28]> = Lazy::new(|| {
    // Go code uses milliseconds. Variable is called `computeStartupBuckets`
    [
        5, 10, 20, 30, 50, 70, 100, 120, 150, 200, 250, 300, 350, 400, 450, 500, 600, 800, 1000,
        1500, 2000, 2500, 3000, 5000, 10000, 20000, 40000, 60000,
    ]
    .map(|ms| (ms as f64) / 1000.0)
});

pub(crate) struct BasebackupQueryTime {
    ok: Histogram,
    error: Histogram,
    client_error: Histogram,
}

pub(crate) static BASEBACKUP_QUERY_TIME: Lazy<BasebackupQueryTime> = Lazy::new(|| {
    let vec = register_histogram_vec!(
        "pageserver_basebackup_query_seconds",
        "Histogram of basebackup queries durations, by result type",
        &["result"],
        COMPUTE_STARTUP_BUCKETS.to_vec(),
    )
    .expect("failed to define a metric");
    BasebackupQueryTime {
        ok: vec.get_metric_with_label_values(&["ok"]).unwrap(),
        error: vec.get_metric_with_label_values(&["error"]).unwrap(),
        client_error: vec.get_metric_with_label_values(&["client_error"]).unwrap(),
    }
});

pub(crate) struct BasebackupQueryTimeOngoingRecording<'a> {
    parent: &'a BasebackupQueryTime,
    start: std::time::Instant,
}

impl BasebackupQueryTime {
    pub(crate) fn start_recording(&self) -> BasebackupQueryTimeOngoingRecording<'_> {
        let start = Instant::now();
        BasebackupQueryTimeOngoingRecording {
            parent: self,
            start,
        }
    }
}

impl BasebackupQueryTimeOngoingRecording<'_> {
    pub(crate) fn observe<T>(self, res: &Result<T, QueryError>) {
        let elapsed = self.start.elapsed().as_secs_f64();
        // If you want to change categorize of a specific error, also change it in `log_query_error`.
        let metric = match res {
            Ok(_) => &self.parent.ok,
            Err(QueryError::Shutdown) | Err(QueryError::Reconnect) => {
                // Do not observe ok/err for shutdown/reconnect.
                // Reconnect error might be raised when the operation is waiting for LSN and the tenant shutdown interrupts
                // the operation. A reconnect error will be issued and the client will retry.
                return;
            }
            Err(QueryError::Disconnected(ConnectionError::Io(io_error)))
                if is_expected_io_error(io_error) =>
            {
                &self.parent.client_error
            }
            Err(_) => &self.parent.error,
        };
        metric.observe(elapsed);
    }
}

pub(crate) static LIVE_CONNECTIONS: Lazy<IntCounterPairVec> = Lazy::new(|| {
    register_int_counter_pair_vec!(
        "pageserver_live_connections_started",
        "Number of network connections that we started handling",
        "pageserver_live_connections_finished",
        "Number of network connections that we finished handling",
        &["pageserver_connection_kind"]
    )
    .expect("failed to define a metric")
});

#[derive(Clone, Copy, enum_map::Enum, IntoStaticStr)]
pub(crate) enum ComputeCommandKind {
    PageStreamV3,
    PageStreamV2,
    Basebackup,
    Fullbackup,
    LeaseLsn,
}

pub(crate) struct ComputeCommandCounters {
    map: EnumMap<ComputeCommandKind, IntCounter>,
}

pub(crate) static COMPUTE_COMMANDS_COUNTERS: Lazy<ComputeCommandCounters> = Lazy::new(|| {
    let inner = register_int_counter_vec!(
        "pageserver_compute_commands",
        "Number of compute -> pageserver commands processed",
        &["command"]
    )
    .expect("failed to define a metric");

    ComputeCommandCounters {
        map: EnumMap::from_array(std::array::from_fn(|i| {
            let command = ComputeCommandKind::from_usize(i);
            let command_str: &'static str = command.into();
            inner.with_label_values(&[command_str])
        })),
    }
});

impl ComputeCommandCounters {
    pub(crate) fn for_command(&self, command: ComputeCommandKind) -> &IntCounter {
        &self.map[command]
    }
}

// remote storage metrics

#[allow(dead_code)]
static REMOTE_TIMELINE_CLIENT_CALLS: Lazy<IntCounterPairVec> = Lazy::new(|| {
    register_int_counter_pair_vec!(
        "pageserver_remote_timeline_client_calls_started",
        "Number of started calls to remote timeline client.",
        "pageserver_remote_timeline_client_calls_finished",
        "Number of finshed calls to remote timeline client.",
        &["branch_id", "file_kind", "op_kind"],
    )
    .unwrap()
});

pub(crate) static NODE_UTILIZATION_SCORE: Lazy<UIntGauge> = Lazy::new(|| {
    register_uint_gauge!(
        "pageserver_utilization_score",
        "The utilization score we report to the controller for scheduling, where 0 is empty, 1000000 is full, and anything above is considered overloaded",
    )
    .expect("failed to define a metric")
});

#[derive(Debug, Clone, Copy, PartialEq, Eq, Hash)]
pub enum RemoteOpKind {
    Upload,
    Download,
    Delete,
}
impl RemoteOpKind {
    pub fn as_str(&self) -> &'static str {
        match self {
            Self::Upload => "upload",
            Self::Download => "download",
            Self::Delete => "delete",
        }
    }
}

#[derive(Debug, Clone, Copy, Hash, PartialEq, Eq)]
pub enum RemoteOpFileKind {
    Layer,
    Index,
}
impl RemoteOpFileKind {
    pub fn as_str(&self) -> &'static str {
        match self {
            Self::Layer => "layer",
            Self::Index => "index",
        }
    }
}

#[allow(dead_code)]
pub(crate) static REMOTE_TIMELINE_CLIENT_COMPLETION_LATENCY: Lazy<HistogramVec> = Lazy::new(|| {
    register_histogram_vec!(
        "pageserver_remote_timeline_client_seconds_global",
        "Time spent on remote timeline client operations. \
        Grouped by task_kind, file_kind, operation_kind and status. \
        The task_kind is \
          - for layer downloads, populated from RequestContext (primary objective of having the label) \
          - for index downloads, set to 'unknown' \
          - for any upload operation, set to 'RemoteUploadTask' \
        This keeps dimensionality at bay. \
        Does not account for time spent waiting in remote timeline client's queues.",
        &["task_kind", "file_kind", "op_kind", "status"]
    )
    .expect("failed to define a metric")
});

pub(crate) static TENANT_TASK_EVENTS: Lazy<IntCounterVec> = Lazy::new(|| {
    register_int_counter_vec!(
        "pageserver_tenant_task_events",
        "Number of task start/stop/fail events.",
        &["event"],
    )
    .expect("Failed to register tenant_task_events metric")
});

pub struct BackgroundLoopSemaphoreMetrics {
    counters: EnumMap<BackgroundLoopKind, IntCounterPair>,
    durations: EnumMap<BackgroundLoopKind, Histogram>,
    waiting_tasks: EnumMap<BackgroundLoopKind, IntGauge>,
    running_tasks: EnumMap<BackgroundLoopKind, IntGauge>,
}

pub(crate) static BACKGROUND_LOOP_SEMAPHORE: Lazy<BackgroundLoopSemaphoreMetrics> =
    Lazy::new(|| {
        let counters = register_int_counter_pair_vec!(
            "pageserver_background_loop_semaphore_wait_start_count",
            "Counter for background loop concurrency-limiting semaphore acquire calls started",
            "pageserver_background_loop_semaphore_wait_finish_count",
            "Counter for background loop concurrency-limiting semaphore acquire calls finished",
            &["task"],
        )
        .unwrap();

        let durations = register_histogram_vec!(
            "pageserver_background_loop_semaphore_wait_seconds",
            "Seconds spent waiting on background loop semaphore acquisition",
            &["task"],
            vec![0.01, 1.0, 5.0, 10.0, 30.0, 60.0, 180.0, 300.0, 600.0],
        )
        .unwrap();

        let waiting_tasks = register_int_gauge_vec!(
            "pageserver_background_loop_semaphore_waiting_tasks",
            "Number of background loop tasks waiting for semaphore",
            &["task"],
        )
        .unwrap();

        let running_tasks = register_int_gauge_vec!(
            "pageserver_background_loop_semaphore_running_tasks",
            "Number of background loop tasks running concurrently",
            &["task"],
        )
        .unwrap();

        BackgroundLoopSemaphoreMetrics {
            counters: EnumMap::from_array(std::array::from_fn(|i| {
                let kind = BackgroundLoopKind::from_usize(i);
                counters.with_label_values(&[kind.into()])
            })),
            durations: EnumMap::from_array(std::array::from_fn(|i| {
                let kind = BackgroundLoopKind::from_usize(i);
                durations.with_label_values(&[kind.into()])
            })),
            waiting_tasks: EnumMap::from_array(std::array::from_fn(|i| {
                let kind = BackgroundLoopKind::from_usize(i);
                waiting_tasks.with_label_values(&[kind.into()])
            })),
            running_tasks: EnumMap::from_array(std::array::from_fn(|i| {
                let kind = BackgroundLoopKind::from_usize(i);
                running_tasks.with_label_values(&[kind.into()])
            })),
        }
    });

impl BackgroundLoopSemaphoreMetrics {
    /// Starts recording semaphore metrics. Call `acquired()` on the returned recorder when the
    /// semaphore is acquired, and drop it when the task completes or is cancelled.
    pub(crate) fn record(
        &self,
        task: BackgroundLoopKind,
    ) -> BackgroundLoopSemaphoreMetricsRecorder {
        BackgroundLoopSemaphoreMetricsRecorder::start(self, task)
    }
}

/// Records metrics for a background task.
pub struct BackgroundLoopSemaphoreMetricsRecorder<'a> {
    metrics: &'a BackgroundLoopSemaphoreMetrics,
    task: BackgroundLoopKind,
    start: Instant,
    wait_counter_guard: Option<metrics::IntCounterPairGuard>,
}

impl<'a> BackgroundLoopSemaphoreMetricsRecorder<'a> {
    /// Starts recording semaphore metrics, by recording wait time and incrementing
    /// `wait_start_count` and `waiting_tasks`.
    fn start(metrics: &'a BackgroundLoopSemaphoreMetrics, task: BackgroundLoopKind) -> Self {
        metrics.waiting_tasks[task].inc();
        Self {
            metrics,
            task,
            start: Instant::now(),
            wait_counter_guard: Some(metrics.counters[task].guard()),
        }
    }

    /// Signals that the semaphore has been acquired, and updates relevant metrics.
    pub fn acquired(&mut self) -> Duration {
        let waited = self.start.elapsed();
        self.wait_counter_guard.take().expect("already acquired");
        self.metrics.durations[self.task].observe(waited.as_secs_f64());
        self.metrics.waiting_tasks[self.task].dec();
        self.metrics.running_tasks[self.task].inc();
        waited
    }
}

impl Drop for BackgroundLoopSemaphoreMetricsRecorder<'_> {
    /// The task either completed or was cancelled.
    fn drop(&mut self) {
        if self.wait_counter_guard.take().is_some() {
            // Waiting.
            self.metrics.durations[self.task].observe(self.start.elapsed().as_secs_f64());
            self.metrics.waiting_tasks[self.task].dec();
        } else {
            // Running.
            self.metrics.running_tasks[self.task].dec();
        }
    }
}

pub(crate) static BACKGROUND_LOOP_PERIOD_OVERRUN_COUNT: Lazy<IntCounterVec> = Lazy::new(|| {
    register_int_counter_vec!(
        "pageserver_background_loop_period_overrun_count",
        "Incremented whenever warn_when_period_overrun() logs a warning.",
        &["task", "period"],
    )
    .expect("failed to define a metric")
});

// walreceiver metrics

pub(crate) static WALRECEIVER_STARTED_CONNECTIONS: Lazy<IntCounter> = Lazy::new(|| {
    register_int_counter!(
        "pageserver_walreceiver_started_connections_total",
        "Number of started walreceiver connections"
    )
    .expect("failed to define a metric")
});

pub(crate) static WALRECEIVER_ACTIVE_MANAGERS: Lazy<IntGauge> = Lazy::new(|| {
    register_int_gauge!(
        "pageserver_walreceiver_active_managers",
        "Number of active walreceiver managers"
    )
    .expect("failed to define a metric")
});

pub(crate) static WALRECEIVER_SWITCHES: Lazy<IntCounterVec> = Lazy::new(|| {
    register_int_counter_vec!(
        "pageserver_walreceiver_switches_total",
        "Number of walreceiver manager change_connection calls",
        &["reason"]
    )
    .expect("failed to define a metric")
});

pub(crate) static WALRECEIVER_BROKER_UPDATES: Lazy<IntCounter> = Lazy::new(|| {
    register_int_counter!(
        "pageserver_walreceiver_broker_updates_total",
        "Number of received broker updates in walreceiver"
    )
    .expect("failed to define a metric")
});

pub(crate) static WALRECEIVER_CANDIDATES_EVENTS: Lazy<IntCounterVec> = Lazy::new(|| {
    register_int_counter_vec!(
        "pageserver_walreceiver_candidates_events_total",
        "Number of walreceiver candidate events",
        &["event"]
    )
    .expect("failed to define a metric")
});

pub(crate) static WALRECEIVER_CANDIDATES_ADDED: Lazy<IntCounter> =
    Lazy::new(|| WALRECEIVER_CANDIDATES_EVENTS.with_label_values(&["add"]));

pub(crate) static WALRECEIVER_CANDIDATES_REMOVED: Lazy<IntCounter> =
    Lazy::new(|| WALRECEIVER_CANDIDATES_EVENTS.with_label_values(&["remove"]));

// Counter keeping track of misrouted PageStream requests. Spelling out PageStream requests here to distinguish
// it from other types of reqeusts (SK wal replication, http requests, etc.). PageStream requests are used by
// Postgres compute to fetch data from pageservers.
// A misrouted PageStream request is registered if the pageserver cannot find the tenant identified in the
// request, or if the pageserver is not the "primary" serving the tenant shard. These error almost always identify
// issues with compute configuration, caused by either the compute node itself being stuck in the wrong
// configuration or Controller reconciliation bugs. Misrouted requests are expected during tenant migration
// and/or during recovery following a pageserver failure, but persistently high rates of misrouted requests
// are indicative of bugs (and unavailability).
pub(crate) static MISROUTED_PAGESTREAM_REQUESTS: Lazy<IntCounter> = Lazy::new(|| {
    register_int_counter!(
        "pageserver_misrouted_pagestream_requests_total",
        "Number of pageserver pagestream requests that were routed to the wrong pageserver"
    )
    .expect("failed to define a metric")
});

// Global counter for PageStream request results by outcome. Outcomes are divided into 3 categories:
// - success
// - internal_error: errors that indicate bugs in the storage cluster (e.g. page reconstruction errors, misrouted requests, LSN timeout errors)
// - other_error: transient error conditions that are expected in normal operation or indicate bugs with other parts of the system (e.g. error due to pageserver shutdown, malformed requests etc.)
pub(crate) static PAGESTREAM_HANDLER_RESULTS_TOTAL: Lazy<IntCounterVec> = Lazy::new(|| {
    register_int_counter_vec!(
        "pageserver_pagestream_handler_results_total",
        "Number of pageserver pagestream handler results by outcome (success, internal_error, other_error)",
        &["outcome"]
    )
    .expect("failed to define a metric")
});

// Constants for pageserver_pagestream_handler_results_total's outcome labels
pub(crate) const PAGESTREAM_HANDLER_OUTCOME_SUCCESS: &str = "success";
pub(crate) const PAGESTREAM_HANDLER_OUTCOME_INTERNAL_ERROR: &str = "internal_error";
pub(crate) const PAGESTREAM_HANDLER_OUTCOME_OTHER_ERROR: &str = "other_error";

// Metrics collected on WAL redo operations
//
// We collect the time spent in actual WAL redo ('redo'), and time waiting
// for access to the postgres process ('wait') since there is only one for
// each tenant.

/// Time buckets are small because we want to be able to measure the
/// smallest redo processing times. These buckets allow us to measure down
/// to 5us, which equates to 200'000 pages/sec, which equates to 1.6GB/sec.
/// This is much better than the previous 5ms aka 200 pages/sec aka 1.6MB/sec.
///
/// Values up to 1s are recorded because metrics show that we have redo
/// durations and lock times larger than 0.250s.
macro_rules! redo_histogram_time_buckets {
    () => {
        vec![
            0.000_005, 0.000_010, 0.000_025, 0.000_050, 0.000_100, 0.000_250, 0.000_500, 0.001_000,
            0.002_500, 0.005_000, 0.010_000, 0.025_000, 0.050_000, 0.100_000, 0.250_000, 0.500_000,
            1.000_000,
        ]
    };
}

/// While we're at it, also measure the amount of records replayed in each
/// operation. We have a global 'total replayed' counter, but that's not
/// as useful as 'what is the skew for how many records we replay in one
/// operation'.
macro_rules! redo_histogram_count_buckets {
    () => {
        vec![0.0, 1.0, 2.0, 5.0, 10.0, 25.0, 50.0, 100.0, 250.0, 500.0]
    };
}

macro_rules! redo_bytes_histogram_count_buckets {
    () => {
        // powers of (2^.5), from 2^4.5 to 2^15 (22 buckets)
        // rounded up to the next multiple of 8 to capture any MAXALIGNed record of that size, too.
        vec![
            24.0, 32.0, 48.0, 64.0, 96.0, 128.0, 184.0, 256.0, 368.0, 512.0, 728.0, 1024.0, 1456.0,
            2048.0, 2904.0, 4096.0, 5800.0, 8192.0, 11592.0, 16384.0, 23176.0, 32768.0,
        ]
    };
}

pub(crate) struct WalIngestMetrics {
    pub(crate) bytes_received: IntCounter,
    pub(crate) records_received: IntCounter,
    pub(crate) records_observed: IntCounter,
    pub(crate) records_committed: IntCounter,
    pub(crate) values_committed_metadata_images: IntCounter,
    pub(crate) values_committed_metadata_deltas: IntCounter,
    pub(crate) values_committed_data_images: IntCounter,
    pub(crate) values_committed_data_deltas: IntCounter,
    pub(crate) gap_blocks_zeroed_on_rel_extend: IntCounter,
}

impl WalIngestMetrics {
    pub(crate) fn inc_values_committed(&self, stats: &DatadirModificationStats) {
        if stats.metadata_images > 0 {
            self.values_committed_metadata_images
                .inc_by(stats.metadata_images);
        }
        if stats.metadata_deltas > 0 {
            self.values_committed_metadata_deltas
                .inc_by(stats.metadata_deltas);
        }
        if stats.data_images > 0 {
            self.values_committed_data_images.inc_by(stats.data_images);
        }
        if stats.data_deltas > 0 {
            self.values_committed_data_deltas.inc_by(stats.data_deltas);
        }
    }
}

pub(crate) static WAL_INGEST: Lazy<WalIngestMetrics> = Lazy::new(|| {
    let values_committed = register_int_counter_vec!(
        "pageserver_wal_ingest_values_committed",
        "Number of values committed to pageserver storage from WAL records",
        &["class", "kind"],
    )
    .expect("failed to define a metric");

    WalIngestMetrics {
    bytes_received: register_int_counter!(
        "pageserver_wal_ingest_bytes_received",
        "Bytes of WAL ingested from safekeepers",
    )
    .unwrap(),
    records_received: register_int_counter!(
        "pageserver_wal_ingest_records_received",
        "Number of WAL records received from safekeepers"
    )
    .expect("failed to define a metric"),
    records_observed: register_int_counter!(
        "pageserver_wal_ingest_records_observed",
        "Number of WAL records observed from safekeepers. These are metadata only records for shard 0."
    )
    .expect("failed to define a metric"),
    records_committed: register_int_counter!(
        "pageserver_wal_ingest_records_committed",
        "Number of WAL records which resulted in writes to pageserver storage"
    )
    .expect("failed to define a metric"),
    values_committed_metadata_images: values_committed.with_label_values(&["metadata", "image"]),
    values_committed_metadata_deltas: values_committed.with_label_values(&["metadata", "delta"]),
    values_committed_data_images: values_committed.with_label_values(&["data", "image"]),
    values_committed_data_deltas: values_committed.with_label_values(&["data", "delta"]),
    gap_blocks_zeroed_on_rel_extend: register_int_counter!(
        "pageserver_gap_blocks_zeroed_on_rel_extend",
        "Total number of zero gap blocks written on relation extends"
    )
    .expect("failed to define a metric"),
}
});

pub(crate) static PAGESERVER_TIMELINE_WAL_RECORDS_RECEIVED: Lazy<IntCounterVec> = Lazy::new(|| {
    register_int_counter_vec!(
        "pageserver_timeline_wal_records_received",
        "Number of WAL records received per shard",
        &["branch_id"]
    )
    .expect("failed to define a metric")
});

pub(crate) static WAL_REDO_TIME: Lazy<Histogram> = Lazy::new(|| {
    register_histogram!(
        "pageserver_wal_redo_seconds",
        "Time spent on WAL redo",
        redo_histogram_time_buckets!()
    )
    .expect("failed to define a metric")
});

pub(crate) static WAL_REDO_RECORDS_HISTOGRAM: Lazy<Histogram> = Lazy::new(|| {
    register_histogram!(
        "pageserver_wal_redo_records_histogram",
        "Histogram of number of records replayed per redo in the Postgres WAL redo process",
        redo_histogram_count_buckets!(),
    )
    .expect("failed to define a metric")
});

pub(crate) static WAL_REDO_BYTES_HISTOGRAM: Lazy<Histogram> = Lazy::new(|| {
    register_histogram!(
        "pageserver_wal_redo_bytes_histogram",
        "Histogram of number of records replayed per redo sent to Postgres",
        redo_bytes_histogram_count_buckets!(),
    )
    .expect("failed to define a metric")
});

// FIXME: isn't this already included by WAL_REDO_RECORDS_HISTOGRAM which has _count?
pub(crate) static WAL_REDO_RECORD_COUNTER: Lazy<IntCounter> = Lazy::new(|| {
    register_int_counter!(
        "pageserver_replayed_wal_records_total",
        "Number of WAL records replayed in WAL redo process"
    )
    .unwrap()
});

#[rustfmt::skip]
pub(crate) static WAL_REDO_PROCESS_LAUNCH_DURATION_HISTOGRAM: Lazy<Histogram> = Lazy::new(|| {
    register_histogram!(
        "pageserver_wal_redo_process_launch_duration",
        "Histogram of the duration of successful WalRedoProcess::launch calls",
        vec![
            0.0002, 0.0004, 0.0006, 0.0008, 0.0010,
            0.0020, 0.0040, 0.0060, 0.0080, 0.0100,
            0.0200, 0.0400, 0.0600, 0.0800, 0.1000,
            0.2000, 0.4000, 0.6000, 0.8000, 1.0000,
            1.5000, 2.0000, 2.5000, 3.0000, 4.0000, 10.0000
        ],
    )
    .expect("failed to define a metric")
});

pub(crate) struct WalRedoProcessCounters {
    pub(crate) started: IntCounter,
    pub(crate) killed_by_cause: EnumMap<WalRedoKillCause, IntCounter>,
    pub(crate) active_stderr_logger_tasks_started: IntCounter,
    pub(crate) active_stderr_logger_tasks_finished: IntCounter,
}

#[derive(Debug, enum_map::Enum, strum_macros::IntoStaticStr)]
pub(crate) enum WalRedoKillCause {
    WalRedoProcessDrop,
    NoLeakChildDrop,
    Startup,
}

impl Default for WalRedoProcessCounters {
    fn default() -> Self {
        let started = register_int_counter!(
            "pageserver_wal_redo_process_started_total",
            "Number of WAL redo processes started",
        )
        .unwrap();

        let killed = register_int_counter_vec!(
            "pageserver_wal_redo_process_stopped_total",
            "Number of WAL redo processes stopped",
            &["cause"],
        )
        .unwrap();

        let active_stderr_logger_tasks_started = register_int_counter!(
            "pageserver_walredo_stderr_logger_tasks_started_total",
            "Number of active walredo stderr logger tasks that have started",
        )
        .unwrap();

        let active_stderr_logger_tasks_finished = register_int_counter!(
            "pageserver_walredo_stderr_logger_tasks_finished_total",
            "Number of active walredo stderr logger tasks that have finished",
        )
        .unwrap();

        Self {
            started,
            killed_by_cause: EnumMap::from_array(std::array::from_fn(|i| {
                let cause = WalRedoKillCause::from_usize(i);
                let cause_str: &'static str = cause.into();
                killed.with_label_values(&[cause_str])
            })),
            active_stderr_logger_tasks_started,
            active_stderr_logger_tasks_finished,
        }
    }
}

pub(crate) static WAL_REDO_PROCESS_COUNTERS: Lazy<WalRedoProcessCounters> =
    Lazy::new(WalRedoProcessCounters::default);

/// Similar to `prometheus::HistogramTimer` but does not record on drop.
pub(crate) struct StorageTimeMetricsTimer {
    metrics: StorageTimeMetrics,
    start: Instant,
    stopped: Cell<bool>,
}

impl StorageTimeMetricsTimer {
    fn new(metrics: StorageTimeMetrics) -> Self {
        // record the active operation as the timer starts
        metrics.timeline_active_count.inc();
        Self {
            metrics,
            start: Instant::now(),
            stopped: Cell::new(false),
        }
    }

    /// Returns the elapsed duration of the timer.
    pub fn elapsed(&self) -> Duration {
        self.start.elapsed()
    }

    /// Record the time from creation to now and return it.
    pub fn stop_and_record(self) -> Duration {
        let duration = self.elapsed();
        let seconds = duration.as_secs_f64();
        self.metrics.timeline_sum.inc_by(seconds);
        self.metrics.timeline_count.inc();
        self.metrics.global_histogram.observe(seconds);
        self.stopped.set(true);
        self.metrics.timeline_active_count.dec();
        duration
    }

    /// Turns this timer into a timer, which will always record -- usually this means recording
    /// regardless an early `?` path was taken in a function.
    pub(crate) fn record_on_drop(self) -> AlwaysRecordingStorageTimeMetricsTimer {
        AlwaysRecordingStorageTimeMetricsTimer(Some(self))
    }
}

impl Drop for StorageTimeMetricsTimer {
    fn drop(&mut self) {
        if !self.stopped.get() {
            self.metrics.timeline_active_count.dec();
        }
    }
}

pub(crate) struct AlwaysRecordingStorageTimeMetricsTimer(Option<StorageTimeMetricsTimer>);

impl Drop for AlwaysRecordingStorageTimeMetricsTimer {
    fn drop(&mut self) {
        if let Some(inner) = self.0.take() {
            inner.stop_and_record();
        }
    }
}

/// Timing facilities for an globally histogrammed metric, which is supported by per tenant and
/// timeline total sum and count.
#[derive(Clone, Debug)]
pub(crate) struct StorageTimeMetrics {
    /// Sum of f64 seconds, per operation, branch_id and branch_id
    timeline_sum: Counter,
    /// Number of oeprations, per operation, branch_id and branch_id
    timeline_count: IntCounter,
    /// Number of active operations per operation, branch_id, and branch_id
    timeline_active_count: IntGauge,
    /// Global histogram having only the "operation" label.
    global_histogram: Histogram,
}

impl StorageTimeMetrics {
    pub fn new(operation: StorageTimeOperation, branch_id: &str) -> Self {
        let operation: &'static str = operation.into();

        let timeline_sum = STORAGE_TIME_SUM_PER_TIMELINE
            .get_metric_with_label_values(&[operation, branch_id])
            .unwrap();
        let timeline_count = STORAGE_TIME_COUNT_PER_TIMELINE
            .get_metric_with_label_values(&[operation, branch_id])
            .unwrap();
        let timeline_active_count = STORAGE_ACTIVE_COUNT_PER_TIMELINE
            .get_metric_with_label_values(&[operation, branch_id])
            .unwrap();
        let global_histogram = STORAGE_TIME_GLOBAL
            .get_metric_with_label_values(&[operation])
            .unwrap();

        StorageTimeMetrics {
            timeline_sum,
            timeline_count,
            timeline_active_count,
            global_histogram,
        }
    }

    /// Starts timing a new operation.
    ///
    /// Note: unlike `prometheus::HistogramTimer` the returned timer does not record on drop.
    pub fn start_timer(&self) -> StorageTimeMetricsTimer {
        StorageTimeMetricsTimer::new(self.clone())
    }
}

pub(crate) struct TimelineMetrics {
    branch_id: String,
    pub logical_size_histo: StorageTimeMetrics,
    pub find_gc_cutoffs_histo: StorageTimeMetrics,
    pub last_record_lsn_gauge: IntGauge,
    pub pitr_history_size: UIntGauge,
    pub archival_size: UIntGauge,
    #[allow(dead_code)]
    pub standby_horizon_gauge: IntGauge,
    pub resident_physical_size_gauge: UIntGauge,
    pub visible_physical_size_gauge: UIntGauge,
    /// copy of LayeredTimeline.current_logical_size
    pub current_logical_size_gauge: UIntGauge,
    pub aux_file_size_gauge: IntGauge,
    pub directory_entries_count_gauge: Lazy<UIntGauge, Box<dyn Send + Fn() -> UIntGauge>>,
    pub evictions_with_low_residence_duration: std::sync::RwLock<EvictionsWithLowResidenceDuration>,
    /// Number of valid LSN leases.
    pub valid_lsn_lease_count_gauge: UIntGauge,
    pub wal_records_received: IntCounter,
    #[allow(dead_code)]
    pub storage_io_size: StorageIoSizeMetrics,
    pub wait_lsn_in_progress_micros: GlobalAndPerTenantIntCounter,
    pub wait_lsn_start_finish_counterpair: IntCounterPair,
    shutdown: std::sync::atomic::AtomicBool,
}

impl TimelineMetrics {
    pub fn new(
        branch_id_raw: &BranchId,
        evictions_with_low_residence_duration_builder: EvictionsWithLowResidenceDurationBuilder,
    ) -> Self {
        let branch_id = branch_id_raw.to_string();
        let logical_size_histo =
            StorageTimeMetrics::new(StorageTimeOperation::LogicalSize, &branch_id);
        let find_gc_cutoffs_histo =
            StorageTimeMetrics::new(StorageTimeOperation::FindGcCutoffs, &branch_id);
        let last_record_lsn_gauge = LAST_RECORD_LSN
            .get_metric_with_label_values(&[&branch_id])
            .unwrap();

        let pitr_history_size = PITR_HISTORY_SIZE
            .get_metric_with_label_values(&[&branch_id])
            .unwrap();

        let archival_size = TIMELINE_ARCHIVE_SIZE
            .get_metric_with_label_values(&[&branch_id])
            .unwrap();

        let standby_horizon_gauge = STANDBY_HORIZON
            .get_metric_with_label_values(&[&branch_id])
            .unwrap();
        let resident_physical_size_gauge = RESIDENT_PHYSICAL_SIZE
            .get_metric_with_label_values(&[&branch_id])
            .unwrap();
        let visible_physical_size_gauge = VISIBLE_PHYSICAL_SIZE
            .get_metric_with_label_values(&[&branch_id])
            .unwrap();
        // TODO: we shouldn't expose this metric
        let current_logical_size_gauge = CURRENT_LOGICAL_SIZE
            .get_metric_with_label_values(&[&branch_id])
            .unwrap();
        let aux_file_size_gauge = AUX_FILE_SIZE
            .get_metric_with_label_values(&[&branch_id])
            .unwrap();
        // TODO use impl Trait syntax here once we have ability to use it: https://github.com/rust-lang/rust/issues/63065
        let directory_entries_count_gauge_closure = {
            let branch_id_raw = *branch_id_raw;
            move || {
                let branch_id = branch_id_raw.to_string();
                let gauge: UIntGauge = DIRECTORY_ENTRIES_COUNT
                    .get_metric_with_label_values(&[&branch_id])
                    .unwrap();
                gauge
            }
        };
        let directory_entries_count_gauge: Lazy<UIntGauge, Box<dyn Send + Fn() -> UIntGauge>> =
            Lazy::new(Box::new(directory_entries_count_gauge_closure));
        let evictions_with_low_residence_duration =
            evictions_with_low_residence_duration_builder.build(&branch_id);

        let valid_lsn_lease_count_gauge = VALID_LSN_LEASE_COUNT
            .get_metric_with_label_values(&[&branch_id])
            .unwrap();

        let wal_records_received = PAGESERVER_TIMELINE_WAL_RECORDS_RECEIVED
            .get_metric_with_label_values(&[&branch_id])
            .unwrap();

        let storage_io_size = StorageIoSizeMetrics::new(&branch_id);

        let wait_lsn_in_progress_micros = GlobalAndPerTenantIntCounter {
            global: WAIT_LSN_IN_PROGRESS_GLOBAL_MICROS.clone(),
            per_tenant: WAIT_LSN_IN_PROGRESS_MICROS
                .get_metric_with_label_values(&[&branch_id])
                .unwrap(),
        };

        let wait_lsn_start_finish_counterpair = WAIT_LSN_START_FINISH_COUNTERPAIR
            .get_metric_with_label_values(&[&branch_id])
            .unwrap();

        TIMELINE_STATE_METRIC.with_label_values(&["active"]).inc();

        TimelineMetrics {
            branch_id,
            logical_size_histo,
            find_gc_cutoffs_histo,
            last_record_lsn_gauge,
            pitr_history_size,
            archival_size,
            standby_horizon_gauge,
            resident_physical_size_gauge,
            visible_physical_size_gauge,
            current_logical_size_gauge,
            aux_file_size_gauge,
            directory_entries_count_gauge,
            evictions_with_low_residence_duration: std::sync::RwLock::new(
                evictions_with_low_residence_duration,
            ),
            storage_io_size,
            valid_lsn_lease_count_gauge,
            wal_records_received,
            wait_lsn_in_progress_micros,
            wait_lsn_start_finish_counterpair,
            shutdown: std::sync::atomic::AtomicBool::default(),
        }
    }

    pub(crate) fn resident_physical_size_get(&self) -> u64 {
        self.resident_physical_size_gauge.get()
    }

    pub(crate) fn shutdown(&self) {
        let was_shutdown = self
            .shutdown
            .swap(true, std::sync::atomic::Ordering::Relaxed);

        if was_shutdown {
            // this happens on tenant deletion because tenant first shuts down timelines, then
            // invokes timeline deletion which first shuts down the timeline again.
            // TODO: this can be removed once https://github.com/neondatabase/neon/issues/5080
            return;
        }

        TIMELINE_STATE_METRIC.with_label_values(&["active"]).dec();

        let branch_id = &self.branch_id;
        let _ = LAST_RECORD_LSN.remove_label_values(&[branch_id]);
        let _ = STANDBY_HORIZON.remove_label_values(&[branch_id]);
        {
            RESIDENT_PHYSICAL_SIZE_GLOBAL.sub(self.resident_physical_size_get());
            let _ = RESIDENT_PHYSICAL_SIZE.remove_label_values(&[branch_id]);
        }
        let _ = VISIBLE_PHYSICAL_SIZE.remove_label_values(&[branch_id]);
        let _ = CURRENT_LOGICAL_SIZE.remove_label_values(&[branch_id]);
        if let Some(metric) = Lazy::get(&DIRECTORY_ENTRIES_COUNT) {
            let _ = metric.remove_label_values(&[branch_id]);
        }

        let _ = TIMELINE_ARCHIVE_SIZE.remove_label_values(&[branch_id]);
        let _ = PITR_HISTORY_SIZE.remove_label_values(&[branch_id]);

        let _ = LAYERS_PER_READ.remove_label_values(&[branch_id]);

        let _ = AUX_FILE_SIZE.remove_label_values(&[branch_id]);
        let _ = VALID_LSN_LEASE_COUNT.remove_label_values(&[branch_id]);

        self.evictions_with_low_residence_duration
            .write()
            .unwrap()
            .remove(branch_id);

        // The following metrics are born outside of the TimelineMetrics lifecycle but still
        // removed at the end of it. The idea is to have the metrics outlive the
        // entity during which they're observed, e.g., the smgr metrics shall
        // outlive an individual smgr connection, but not the timeline.

        for op in StorageTimeOperation::VARIANTS {
            let _ = STORAGE_TIME_SUM_PER_TIMELINE.remove_label_values(&[op, branch_id]);
            let _ = STORAGE_TIME_COUNT_PER_TIMELINE.remove_label_values(&[op, branch_id]);
            let _ = STORAGE_ACTIVE_COUNT_PER_TIMELINE.remove_label_values(&[op, branch_id]);
        }

        for op in StorageIoSizeOperation::VARIANTS {
            let _ = STORAGE_IO_SIZE.remove_label_values(&[op, branch_id]);
        }

        let _ = WAIT_LSN_IN_PROGRESS_MICROS.remove_label_values(&[branch_id]);

        {
            let mut res = [Ok(()), Ok(())];
            WAIT_LSN_START_FINISH_COUNTERPAIR.remove_label_values(&mut res, &[branch_id]);
        }

        let _ = SMGR_QUERY_STARTED_PER_TENANT_TIMELINE
            .remove_label_values(&[SmgrQueryType::GetPageAtLsn.into(), branch_id]);
        let _ = SMGR_QUERY_TIME_PER_TENANT_TIMELINE
            .remove_label_values(&[SmgrQueryType::GetPageAtLsn.into(), branch_id]);
        let _ = PAGE_SERVICE_BATCH_SIZE_PER_TENANT_TIMELINE.remove_label_values(&[branch_id]);
        let _ = PAGESERVER_TIMELINE_WAL_RECORDS_RECEIVED.remove_label_values(&[branch_id]);
        let _ = PAGE_SERVICE_SMGR_FLUSH_INPROGRESS_MICROS.remove_label_values(&[branch_id]);
        let _ = PAGE_SERVICE_SMGR_BATCH_WAIT_TIME.remove_label_values(&[branch_id]);

        for reason in GetPageBatchBreakReason::iter() {
            let _ = PAGE_SERVICE_BATCH_BREAK_REASON_PER_TENANT_TIMELINE
                .remove_label_values(&[branch_id, reason.into()]);
        }
    }
}

pub(crate) fn remove_tenant_metrics(branch_id: &BranchId) {
    tenant_throttling::remove_tenant_metrics(branch_id);
}

/// Maintain a per timeline gauge in addition to the global gauge.
///
/// yb-amp: the metric is always 0 (no layer files = no "remote
/// physical size"). The struct survives because `RemoteTimelineClientMetrics`
/// still keeps the field, but the setter / getter are gone and
/// nothing ever advances `last_set` above zero. Drop is a no-op.
#[allow(dead_code)]
pub(crate) struct PerTimelineRemotePhysicalSizeGauge {
    last_set: AtomicU64,
}

impl PerTimelineRemotePhysicalSizeGauge {
    #[allow(dead_code)]
    fn new() -> Self {
        Self {
            last_set: AtomicU64::new(0),
        }
    }
}

impl Drop for PerTimelineRemotePhysicalSizeGauge {
    fn drop(&mut self) {
        REMOTE_PHYSICAL_SIZE_GLOBAL.sub(self.last_set.load(std::sync::atomic::Ordering::Relaxed));
    }
}

#[allow(dead_code)]
pub(crate) struct RemoteTimelineClientMetrics {
    branch_id: String,
    pub(crate) remote_physical_size_gauge: PerTimelineRemotePhysicalSizeGauge,
    calls: Mutex<HashMap<(&'static str, &'static str), IntCounterPair>>,
    pub(crate) projected_remote_consistent_lsn_gauge: UIntGauge,
}

impl RemoteTimelineClientMetrics {
    #[allow(dead_code)]
    pub fn new(branch_id: &BranchId) -> Self {
        let branch_id_str = branch_id.to_string();

        let remote_physical_size_gauge = PerTimelineRemotePhysicalSizeGauge::new();

        let projected_remote_consistent_lsn_gauge = PROJECTED_REMOTE_CONSISTENT_LSN
            .get_metric_with_label_values(&[&branch_id_str])
            .unwrap();

        RemoteTimelineClientMetrics {
            branch_id: branch_id_str,
            calls: Mutex::new(HashMap::default()),
            remote_physical_size_gauge,
            projected_remote_consistent_lsn_gauge,
        }
    }

    #[allow(dead_code)]
    pub fn remote_operation_time(
        &self,
        task_kind: Option<TaskKind>,
        file_kind: &RemoteOpFileKind,
        op_kind: &RemoteOpKind,
        status: &'static str,
    ) -> Histogram {
        REMOTE_TIMELINE_CLIENT_COMPLETION_LATENCY
            .get_metric_with_label_values(&[
                task_kind.as_ref().map(|tk| tk.into()).unwrap_or("unknown"),
                file_kind.as_str(),
                op_kind.as_str(),
                status,
            ])
            .unwrap()
    }

    #[allow(dead_code)]
    fn calls_counter_pair(
        &self,
        file_kind: &RemoteOpFileKind,
        op_kind: &RemoteOpKind,
    ) -> IntCounterPair {
        let mut guard = self.calls.lock().unwrap();
        let key = (file_kind.as_str(), op_kind.as_str());
        let metric = guard.entry(key).or_insert_with(move || {
            REMOTE_TIMELINE_CLIENT_CALLS
                .get_metric_with_label_values(&[&self.branch_id, key.0, key.1])
                .unwrap()
        });
        metric.clone()
    }
}

/// See [`RemoteTimelineClientMetrics::call_begin`].
#[must_use]
#[allow(dead_code)]
pub(crate) struct RemoteTimelineClientCallMetricGuard {
    /// Decremented on drop.
    calls_counter_pair: Option<IntCounterPair>,
}

impl Drop for RemoteTimelineClientCallMetricGuard {
    fn drop(&mut self) {
        if let Some(guard) = self.calls_counter_pair.take() {
            guard.dec();
        }
    }
}

impl RemoteTimelineClientMetrics {
    /// Update the metrics that change when a call to the remote timeline client instance starts.
    /// Drop the returned guard object once the operation is finished to update the corresponding
    /// metrics that track completions.
    #[allow(dead_code)]
    pub(crate) fn call_begin(
        &self,
        file_kind: &RemoteOpFileKind,
        op_kind: &RemoteOpKind,
    ) -> RemoteTimelineClientCallMetricGuard {
        let calls_counter_pair = self.calls_counter_pair(file_kind, op_kind);
        calls_counter_pair.inc();
        RemoteTimelineClientCallMetricGuard {
            calls_counter_pair: Some(calls_counter_pair),
        }
    }
}

impl Drop for RemoteTimelineClientMetrics {
    fn drop(&mut self) {
        let RemoteTimelineClientMetrics {
            branch_id,
            remote_physical_size_gauge,
            calls,
            projected_remote_consistent_lsn_gauge,
        } = self;
        for ((a, b), _) in calls.get_mut().unwrap().drain() {
            let mut res = [Ok(()), Ok(())];
            REMOTE_TIMELINE_CLIENT_CALLS.remove_label_values(&mut res, &[branch_id, a, b]);
            // don't care about results
        }
        {
            let _ = remote_physical_size_gauge; // use to avoid 'unused' warning in desctructuring above
            let _ = REMOTE_PHYSICAL_SIZE.remove_label_values(&[branch_id]);
        }
        {
            let _ = projected_remote_consistent_lsn_gauge;
            let _ = PROJECTED_REMOTE_CONSISTENT_LSN.remove_label_values(&[branch_id]);
        }
    }
}

/// Wrapper future that measures the time spent by a remote storage operation,
/// and records the time and success/failure as a prometheus metric.
#[allow(dead_code)]
pub(crate) trait MeasureRemoteOp<O, E>: Sized + Future<Output = Result<O, E>> {
    async fn measure_remote_op(
        self,
        task_kind: Option<TaskKind>, // not all caller contexts have a RequestContext / TaskKind handy
        file_kind: RemoteOpFileKind,
        op: RemoteOpKind,
        metrics: Arc<RemoteTimelineClientMetrics>,
    ) -> Result<O, E> {
        let start = Instant::now();
        let res = self.await;
        let duration = start.elapsed();
        let status = if res.is_ok() { &"success" } else { &"failure" };
        metrics
            .remote_operation_time(task_kind, &file_kind, &op, status)
            .observe(duration.as_secs_f64());
        res
    }
}

impl<Fut, O, E> MeasureRemoteOp<O, E> for Fut where Fut: Sized + Future<Output = Result<O, E>> {}

/// jemalloc stats exposed as Prometheus gauges. Registered alongside
/// the libmetrics process collector in `bin/yb-pageserver.rs` so each
/// scrape pulls fresh values from the allocator.
///
/// The values come from the `tikv-jemalloc-ctl::stats::*` mibs. They
/// need a refresh epoch advance to update; `collect()` does that
/// before reading.
pub mod jemalloc_stats {
    use metrics::UIntGauge;
    use metrics::prometheus::core::{Collector, Desc};
    use metrics::prometheus::proto::MetricFamily;

    pub struct JemallocCollector {
        descs: Vec<Desc>,
        allocated: UIntGauge,
        active: UIntGauge,
        resident: UIntGauge,
        mapped: UIntGauge,
        retained: UIntGauge,
        metadata: UIntGauge,
    }

    impl JemallocCollector {
        pub fn new() -> Self {
            fn g(name: &str, help: &str, descs: &mut Vec<Desc>) -> UIntGauge {
                let g = UIntGauge::new(name, help).unwrap();
                descs.extend(
                    metrics::prometheus::core::Collector::desc(&g)
                        .into_iter()
                        .cloned(),
                );
                g
            }
            let mut descs = Vec::new();
            let allocated = g(
                "pageserver_jemalloc_allocated_bytes",
                "Bytes allocated and not yet freed by the application (live heap)",
                &mut descs,
            );
            let active = g(
                "pageserver_jemalloc_active_bytes",
                "Bytes in active pages (allocated + immediate slack)",
                &mut descs,
            );
            let resident = g(
                "pageserver_jemalloc_resident_bytes",
                "Bytes in physically-resident pages held by jemalloc (active + dirty)",
                &mut descs,
            );
            let mapped = g(
                "pageserver_jemalloc_mapped_bytes",
                "Bytes mapped by jemalloc (active + dirty + muzzy)",
                &mut descs,
            );
            let retained = g(
                "pageserver_jemalloc_retained_bytes",
                "Bytes retained for future allocation (madvise'd away, may not be resident)",
                &mut descs,
            );
            let metadata = g(
                "pageserver_jemalloc_metadata_bytes",
                "Bytes used by jemalloc internal metadata",
                &mut descs,
            );
            Self {
                descs,
                allocated,
                active,
                resident,
                mapped,
                retained,
                metadata,
            }
        }
    }

    impl Default for JemallocCollector {
        fn default() -> Self {
            Self::new()
        }
    }

    impl Collector for JemallocCollector {
        fn desc(&self) -> Vec<&Desc> {
            self.descs.iter().collect()
        }

        fn collect(&self) -> Vec<MetricFamily> {
            use tikv_jemalloc_ctl::{epoch, stats};

            // jemalloc stats are cached; epoch.advance() forces a
            // refresh of the per-thread counters before we read them.
            if epoch::advance().is_err() {
                return vec![];
            }

            let mut mfs = Vec::with_capacity(6);
            if let Ok(v) = stats::allocated::read() {
                self.allocated.set(v as u64);
                mfs.extend(self.allocated.collect());
            }
            if let Ok(v) = stats::active::read() {
                self.active.set(v as u64);
                mfs.extend(self.active.collect());
            }
            if let Ok(v) = stats::resident::read() {
                self.resident.set(v as u64);
                mfs.extend(self.resident.collect());
            }
            if let Ok(v) = stats::mapped::read() {
                self.mapped.set(v as u64);
                mfs.extend(self.mapped.collect());
            }
            if let Ok(v) = stats::retained::read() {
                self.retained.set(v as u64);
                mfs.extend(self.retained.collect());
            }
            if let Ok(v) = stats::metadata::read() {
                self.metadata.set(v as u64);
                mfs.extend(self.metadata.collect());
            }
            mfs
        }
    }
}

pub mod tokio_epoll_uring {
    use std::collections::HashMap;
    use std::sync::{Arc, Mutex};

    use metrics::{Histogram, LocalHistogram, UIntGauge, register_histogram, register_int_counter};
    use once_cell::sync::Lazy;

    /// Shared storage for tokio-epoll-uring thread local metrics.
    pub(crate) static THREAD_LOCAL_METRICS_STORAGE: Lazy<ThreadLocalMetricsStorage> =
        Lazy::new(|| {
            let slots_submission_queue_depth = register_histogram!(
                "pageserver_tokio_epoll_uring_slots_submission_queue_depth",
                "The slots waiters queue depth of each tokio_epoll_uring system",
                vec![
                    1.0, 2.0, 4.0, 8.0, 16.0, 32.0, 64.0, 128.0, 256.0, 512.0, 1024.0
                ],
            )
            .expect("failed to define a metric");
            ThreadLocalMetricsStorage {
                observers: Mutex::new(HashMap::new()),
                slots_submission_queue_depth,
            }
        });

    pub struct ThreadLocalMetricsStorage {
        /// List of thread local metrics observers.
        observers: Mutex<HashMap<u64, Arc<ThreadLocalMetrics>>>,
        /// A histogram shared between all thread local systems
        /// for collecting slots submission queue depth.
        slots_submission_queue_depth: Histogram,
    }

    /// Each thread-local [`tokio_epoll_uring::System`] gets one of these as its
    /// [`tokio_epoll_uring::metrics::PerSystemMetrics`] generic.
    ///
    /// The System makes observations into [`Self`] and periodically, the collector
    /// comes along and flushes [`Self`] into the shared storage [`THREAD_LOCAL_METRICS_STORAGE`].
    ///
    /// [`LocalHistogram`] is `!Send`, so, we need to put it behind a [`Mutex`].
    /// But except for the periodic flush, the lock is uncontended so there's no waiting
    /// for cache coherence protocol to get an exclusive cache line.
    pub struct ThreadLocalMetrics {
        /// Local observer of thread local tokio-epoll-uring system's slots waiters queue depth.
        slots_submission_queue_depth: Mutex<LocalHistogram>,
    }

    impl ThreadLocalMetricsStorage {
        /// Registers a new thread local system. Returns a thread local metrics observer.
        pub fn register_system(&self, id: u64) -> Arc<ThreadLocalMetrics> {
            let per_system_metrics = Arc::new(ThreadLocalMetrics::new(
                self.slots_submission_queue_depth.local(),
            ));
            let mut g = self.observers.lock().unwrap();
            g.insert(id, Arc::clone(&per_system_metrics));
            per_system_metrics
        }

        /// Removes metrics observer for a thread local system.
        /// This should be called before dropping a thread local system.
        pub fn remove_system(&self, id: u64) {
            let mut g = self.observers.lock().unwrap();
            g.remove(&id);
        }

        /// Flush all thread local metrics to the shared storage.
        pub fn flush_thread_local_metrics(&self) {
            let g = self.observers.lock().unwrap();
            g.values().for_each(|local| {
                local.flush();
            });
        }
    }

    impl ThreadLocalMetrics {
        pub fn new(slots_submission_queue_depth: LocalHistogram) -> Self {
            ThreadLocalMetrics {
                slots_submission_queue_depth: Mutex::new(slots_submission_queue_depth),
            }
        }

        /// Flushes the thread local metrics to shared aggregator.
        pub fn flush(&self) {
            let Self {
                slots_submission_queue_depth,
            } = self;
            slots_submission_queue_depth.lock().unwrap().flush();
        }
    }

    impl tokio_epoll_uring::metrics::PerSystemMetrics for ThreadLocalMetrics {
        fn observe_slots_submission_queue_depth(&self, queue_depth: u64) {
            let Self {
                slots_submission_queue_depth,
            } = self;
            slots_submission_queue_depth
                .lock()
                .unwrap()
                .observe(queue_depth as f64);
        }
    }

    pub struct Collector {
        descs: Vec<metrics::core::Desc>,
        systems_created: UIntGauge,
        systems_destroyed: UIntGauge,
        thread_local_metrics_storage: &'static ThreadLocalMetricsStorage,
    }

    impl metrics::core::Collector for Collector {
        fn desc(&self) -> Vec<&metrics::core::Desc> {
            self.descs.iter().collect()
        }

        fn collect(&self) -> Vec<metrics::proto::MetricFamily> {
            let mut mfs = Vec::with_capacity(Self::NMETRICS);
            let tokio_epoll_uring::metrics::GlobalMetrics {
                systems_created,
                systems_destroyed,
            } = tokio_epoll_uring::metrics::global();
            self.systems_created.set(systems_created);
            mfs.extend(self.systems_created.collect());
            self.systems_destroyed.set(systems_destroyed);
            mfs.extend(self.systems_destroyed.collect());

            self.thread_local_metrics_storage
                .flush_thread_local_metrics();

            mfs.extend(
                self.thread_local_metrics_storage
                    .slots_submission_queue_depth
                    .collect(),
            );
            mfs
        }
    }

    impl Collector {
        const NMETRICS: usize = 3;

        #[allow(clippy::new_without_default)]
        pub fn new() -> Self {
            let mut descs = Vec::new();

            let systems_created = UIntGauge::new(
                "pageserver_tokio_epoll_uring_systems_created",
                "counter of tokio-epoll-uring systems that were created",
            )
            .unwrap();
            descs.extend(
                metrics::core::Collector::desc(&systems_created)
                    .into_iter()
                    .cloned(),
            );

            let systems_destroyed = UIntGauge::new(
                "pageserver_tokio_epoll_uring_systems_destroyed",
                "counter of tokio-epoll-uring systems that were destroyed",
            )
            .unwrap();
            descs.extend(
                metrics::core::Collector::desc(&systems_destroyed)
                    .into_iter()
                    .cloned(),
            );

            Self {
                descs,
                systems_created,
                systems_destroyed,
                thread_local_metrics_storage: &THREAD_LOCAL_METRICS_STORAGE,
            }
        }
    }

    pub(crate) static THREAD_LOCAL_LAUNCH_SUCCESSES: Lazy<metrics::IntCounter> = Lazy::new(|| {
        register_int_counter!(
            "pageserver_tokio_epoll_uring_pageserver_thread_local_launch_success_count",
            "Number of times where thread_local_system creation spanned multiple executor threads",
        )
        .unwrap()
    });

    pub(crate) static THREAD_LOCAL_LAUNCH_FAILURES: Lazy<metrics::IntCounter> = Lazy::new(|| {
        register_int_counter!(
            "pageserver_tokio_epoll_uring_pageserver_thread_local_launch_failures_count",
            "Number of times thread_local_system creation failed and was retried after back-off.",
        )
        .unwrap()
    });
}

pub(crate) struct GlobalAndPerTenantIntCounter {
    global: IntCounter,
    per_tenant: IntCounter,
}

impl GlobalAndPerTenantIntCounter {
    #[inline(always)]
    pub(crate) fn inc(&self) {
        self.inc_by(1)
    }
    #[inline(always)]
    pub(crate) fn inc_by(&self, n: u64) {
        self.global.inc_by(n);
        self.per_tenant.inc_by(n);
    }
}

pub(crate) mod tenant_throttling {
    use metrics::register_int_counter_vec;
    use once_cell::sync::Lazy;
    use utils::id::BranchId;

    use super::GlobalAndPerTenantIntCounter;

    pub(crate) struct Metrics<const KIND: usize> {
        pub(super) count_accounted_start: GlobalAndPerTenantIntCounter,
        pub(super) count_accounted_finish: GlobalAndPerTenantIntCounter,
        pub(super) wait_time: GlobalAndPerTenantIntCounter,
        pub(super) count_throttled: GlobalAndPerTenantIntCounter,
    }

    static COUNT_ACCOUNTED_START: Lazy<metrics::IntCounterVec> = Lazy::new(|| {
        register_int_counter_vec!(
            "pageserver_tenant_throttling_count_accounted_start_global",
            "Count of tenant throttling starts, by kind of throttle.",
            &["kind"]
        )
        .unwrap()
    });
    static COUNT_ACCOUNTED_START_PER_TENANT: Lazy<metrics::IntCounterVec> = Lazy::new(|| {
        register_int_counter_vec!(
            "pageserver_tenant_throttling_count_accounted_start",
            "Count of tenant throttling starts, by kind of throttle.",
            &["kind", "branch_id"]
        )
        .unwrap()
    });
    static COUNT_ACCOUNTED_FINISH: Lazy<metrics::IntCounterVec> = Lazy::new(|| {
        register_int_counter_vec!(
            "pageserver_tenant_throttling_count_accounted_finish_global",
            "Count of tenant throttling finishes, by kind of throttle.",
            &["kind"]
        )
        .unwrap()
    });
    static COUNT_ACCOUNTED_FINISH_PER_TENANT: Lazy<metrics::IntCounterVec> = Lazy::new(|| {
        register_int_counter_vec!(
            "pageserver_tenant_throttling_count_accounted_finish",
            "Count of tenant throttling finishes, by kind of throttle.",
            &["kind", "branch_id"]
        )
        .unwrap()
    });
    static WAIT_USECS: Lazy<metrics::IntCounterVec> = Lazy::new(|| {
        register_int_counter_vec!(
            "pageserver_tenant_throttling_wait_usecs_sum_global",
            "Sum of microseconds that spent waiting throttle by kind of throttle.",
            &["kind"]
        )
        .unwrap()
    });
    static WAIT_USECS_PER_TENANT: Lazy<metrics::IntCounterVec> = Lazy::new(|| {
        register_int_counter_vec!(
            "pageserver_tenant_throttling_wait_usecs_sum",
            "Sum of microseconds that spent waiting throttle by kind of throttle.",
            &["kind", "branch_id"]
        )
        .unwrap()
    });

    static WAIT_COUNT: Lazy<metrics::IntCounterVec> = Lazy::new(|| {
        register_int_counter_vec!(
            "pageserver_tenant_throttling_count_global",
            "Count of tenant throttlings, by kind of throttle.",
            &["kind"]
        )
        .unwrap()
    });
    static WAIT_COUNT_PER_TENANT: Lazy<metrics::IntCounterVec> = Lazy::new(|| {
        register_int_counter_vec!(
            "pageserver_tenant_throttling_count",
            "Count of tenant throttlings, by kind of throttle.",
            &["kind", "branch_id"]
        )
        .unwrap()
    });

    const KINDS: &[&str] = &["pagestream"];
    pub type Pagestream = Metrics<0>;

    impl<const KIND: usize> Metrics<KIND> {
        pub(crate) fn new(branch_id: &BranchId) -> Self {
            let per_tenant_label_values = &[KINDS[KIND], &branch_id.to_string()];
            Metrics {
                count_accounted_start: {
                    GlobalAndPerTenantIntCounter {
                        global: COUNT_ACCOUNTED_START.with_label_values(&[KINDS[KIND]]),
                        per_tenant: COUNT_ACCOUNTED_START_PER_TENANT
                            .with_label_values(per_tenant_label_values),
                    }
                },
                count_accounted_finish: {
                    GlobalAndPerTenantIntCounter {
                        global: COUNT_ACCOUNTED_FINISH.with_label_values(&[KINDS[KIND]]),
                        per_tenant: COUNT_ACCOUNTED_FINISH_PER_TENANT
                            .with_label_values(per_tenant_label_values),
                    }
                },
                wait_time: {
                    GlobalAndPerTenantIntCounter {
                        global: WAIT_USECS.with_label_values(&[KINDS[KIND]]),
                        per_tenant: WAIT_USECS_PER_TENANT
                            .with_label_values(per_tenant_label_values),
                    }
                },
                count_throttled: {
                    GlobalAndPerTenantIntCounter {
                        global: WAIT_COUNT.with_label_values(&[KINDS[KIND]]),
                        per_tenant: WAIT_COUNT_PER_TENANT
                            .with_label_values(per_tenant_label_values),
                    }
                },
            }
        }
    }

    pub(crate) fn preinitialize_global_metrics() {
        Lazy::force(&COUNT_ACCOUNTED_START);
        Lazy::force(&COUNT_ACCOUNTED_FINISH);
        Lazy::force(&WAIT_USECS);
        Lazy::force(&WAIT_COUNT);
    }

    pub(crate) fn remove_tenant_metrics(branch_id: &BranchId) {
        for m in &[
            &COUNT_ACCOUNTED_START_PER_TENANT,
            &COUNT_ACCOUNTED_FINISH_PER_TENANT,
            &WAIT_USECS_PER_TENANT,
            &WAIT_COUNT_PER_TENANT,
        ] {
            for kind in KINDS {
                let _ = m.remove_label_values(&[kind, &branch_id.to_string()]);
            }
        }
    }
}

static TOKIO_EXECUTOR_THREAD_COUNT: Lazy<UIntGaugeVec> = Lazy::new(|| {
    register_uint_gauge_vec!(
        "pageserver_tokio_executor_thread_configured_count",
        "Total number of configued tokio executor threads in the process.
         The `setup` label denotes whether we're running with multiple runtimes or a single runtime.",
        &["setup"],
    )
    .unwrap()
});

pub(crate) fn set_tokio_runtime_setup(setup: &str, num_threads: NonZeroUsize) {
    static SERIALIZE: std::sync::Mutex<()> = std::sync::Mutex::new(());
    let _guard = SERIALIZE.lock().unwrap();
    TOKIO_EXECUTOR_THREAD_COUNT.reset();
    TOKIO_EXECUTOR_THREAD_COUNT
        .get_metric_with_label_values(&[setup])
        .unwrap()
        .set(u64::try_from(num_threads.get()).unwrap());
}

pub(crate) static BASEBACKUP_CACHE_READ: Lazy<IntCounterVec> = Lazy::new(|| {
    register_int_counter_vec!(
        "pageserver_basebackup_cache_read_total",
        "Number of read accesses to the basebackup cache grouped by hit/miss/error",
        &["result"]
    )
    .expect("failed to define a metric")
});

pub(crate) static BASEBACKUP_CACHE_PREPARE: Lazy<IntCounterVec> = Lazy::new(|| {
    register_int_counter_vec!(
        "pageserver_basebackup_cache_prepare_total",
        "Number of prepare requests processed by the basebackup cache grouped by ok/skip/error",
        &["result"]
    )
    .expect("failed to define a metric")
});

pub(crate) static BASEBACKUP_CACHE_ENTRIES: Lazy<UIntGauge> = Lazy::new(|| {
    register_uint_gauge!(
        "pageserver_basebackup_cache_entries_total",
        "Number of entries in the basebackup cache"
    )
    .expect("failed to define a metric")
});

pub(crate) static BASEBACKUP_CACHE_SIZE: Lazy<UIntGauge> = Lazy::new(|| {
    register_uint_gauge!(
        "pageserver_basebackup_cache_size_bytes",
        "Total size of all basebackup cache entries on disk in bytes"
    )
    .expect("failed to define a metric")
});

pub(crate) static BASEBACKUP_CACHE_PREPARE_QUEUE_SIZE: Lazy<UIntGauge> = Lazy::new(|| {
    register_uint_gauge!(
        "pageserver_basebackup_cache_prepare_queue_size",
        "Number of requests in the basebackup prepare channel"
    )
    .expect("failed to define a metric")
});

static PAGESERVER_CONFIG_IGNORED_ITEMS: Lazy<UIntGaugeVec> = Lazy::new(|| {
    register_uint_gauge_vec!(
        "pageserver_config_ignored_items",
        "TOML items present in the on-disk configuration file but ignored by the pageserver config parser.\
         The `item` label is the dot-separated path of the ignored item in the on-disk configuration file.\
         The value for an unknown config item is always 1.\
         There is a special label value \"\", which is 0, so that there is always a metric exposed (simplifies dashboards).",
        &["item"]
    )
    .unwrap()
});

pub fn preinitialize_metrics(
    conf: &'static PageServerConf,
    ignored: config::ignored_fields::Paths,
) {
    set_page_service_config_max_batch_size(&conf.page_service_pipelining);

    PAGESERVER_CONFIG_IGNORED_ITEMS
        .with_label_values(&[""])
        .set(0);
    for path in &ignored.paths {
        PAGESERVER_CONFIG_IGNORED_ITEMS
            .with_label_values(&[path])
            .set(1);
    }

    // Python tests need these and on some we do alerting.
    //
    // FIXME(4813): make it so that we have no top level metrics as this fn will easily fall out of
    // order:
    // - global metrics reside in a Lazy<PageserverMetrics>
    //   - access via crate::metrics::PS_METRICS.some_metric.inc()
    // - could move the statics into TimelineMetrics::new()?

    // counters
    [
        &UNEXPECTED_ONDEMAND_DOWNLOADS,
        &WALRECEIVER_STARTED_CONNECTIONS,
        &WALRECEIVER_BROKER_UPDATES,
        &WALRECEIVER_CANDIDATES_ADDED,
        &WALRECEIVER_CANDIDATES_REMOVED,
        &tokio_epoll_uring::THREAD_LOCAL_LAUNCH_FAILURES,
        &tokio_epoll_uring::THREAD_LOCAL_LAUNCH_SUCCESSES,
        &CIRCUIT_BREAKERS_BROKEN,
        &CIRCUIT_BREAKERS_UNBROKEN,
        &PAGE_SERVICE_SMGR_FLUSH_INPROGRESS_MICROS_GLOBAL,
        &WAIT_LSN_IN_PROGRESS_GLOBAL_MICROS,
        &MISROUTED_PAGESTREAM_REQUESTS,
    ]
    .into_iter()
    .for_each(|c| {
        Lazy::force(c);
    });

    // Tenant stats
    Lazy::force(&TENANT);

    // countervecs
    [
        &BACKGROUND_LOOP_PERIOD_OVERRUN_COUNT,
        &SMGR_QUERY_STARTED_GLOBAL,
        &PAGE_SERVICE_BATCH_BREAK_REASON_GLOBAL,
    ]
    .into_iter()
    .for_each(|c| {
        Lazy::force(c);
    });

    // gauges
    WALRECEIVER_ACTIVE_MANAGERS.get();

    // histograms
    [
        &LAYERS_PER_READ_GLOBAL,
        &LAYERS_PER_READ_BATCH_GLOBAL,
        &LAYERS_PER_READ_AMORTIZED_GLOBAL,
        &DELTAS_PER_READ_GLOBAL,
        &WAIT_LSN_TIME,
        &WAL_REDO_TIME,
        &WAL_REDO_RECORDS_HISTOGRAM,
        &WAL_REDO_BYTES_HISTOGRAM,
        &WAL_REDO_PROCESS_LAUNCH_DURATION_HISTOGRAM,
        &PAGE_SERVICE_BATCH_SIZE_GLOBAL,
        &PAGE_SERVICE_SMGR_BATCH_WAIT_TIME_GLOBAL,
    ]
    .into_iter()
    .for_each(|h| {
        Lazy::force(h);
    });

    // Custom
    Lazy::force(&BASEBACKUP_QUERY_TIME);
    Lazy::force(&COMPUTE_COMMANDS_COUNTERS);
    Lazy::force(&tokio_epoll_uring::THREAD_LOCAL_METRICS_STORAGE);

    tenant_throttling::preinitialize_global_metrics();
}
