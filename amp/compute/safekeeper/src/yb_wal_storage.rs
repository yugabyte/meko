//! YugabyteDB-mirrored WAL storage.
//!
//! The safekeeper keeps no WAL on the local filesystem. Every WAL write is
//! persisted to a YB table (`amp_storage.wals`) — the durable copy — and reads
//! are served from YB (`WalReader` opens a [`YbSegmentReader`]) fronted by an
//! in-memory hot cache for recent WAL. Durability is the WAL landing in YB: a
//! commit is acked only once its bytes are INSERT'd (see the ack model under
//! "Batching" below).
//!
//! [`YbWalStorage`] tracks two in-memory watermarks over that stream — the byte
//! end it has accepted and the last complete record within it (a
//! [`WalStreamTracker`]) — and no filesystem state of any kind.
//!
//! The table (`amp_storage.wals`, defined in the controller
//! migration) holds one row per coalesced run of a consumer batch:
//! contiguous chunks of a segment are concatenated into a single row
//! before the INSERT, so the typical batch is ONE row; runs break at
//! segment boundaries and at byte discontinuities.
//!
//! Bucketing: the key is ALL-RANGE, led by `bucket_id`, and the table is
//! pre-split into exactly [`WALS_BUCKETS`] tablets, so a branch's WAL spreads
//! across all of them from the first row. `bucket_id` is assigned per
//! consumer BATCH by [`consumer_main`], rotating through the buckets one
//! batch at a time. One bucket per batch means every batch targets ONE
//! tablet, applied as a single atomic write batch, while write load rotates
//! across all tablets. That holds until the bucket's tablet splits: then the
//! rows of one batch can land on different tablets, and a flush cut short
//! leaves a hole that the next load has to find
//! ([`YbWalStorage::contiguous_wal_end`]).
//! Readers can't derive a chunk's bucket, so index-tight reads add a
//! `bucket_id IN (0..WALS_BUCKETS-1)` key list (batched per-bucket index
//! probes).
//!
//! `bucket_id` leads rather than `branch_id` so the pre-split boundaries are
//! literal integers: branch IDs are minted at runtime, and a leading
//! `branch_id` would also put one branch's whole WAL stream in a single
//! contiguous key span — one tablet until it auto-splits, which is the write
//! hotspot the bucket exists to prevent.
//!
//! Notes:
//! - `chunk_offset` uniquely identifies a chunk within a segment because
//!   the writer never produces overlapping chunks: `truncate_wal` aligns
//!   each segment so the next `write_wal` starts at exactly the truncation
//!   point. A retried same-offset INSERT is, by construction, an idempotent
//!   replay of the same bytes — safe to silently UPSERT under
//!   `yb_enable_upsert_mode = true`.
//! - `truncate_wal` deletes rows whose end offset (`chunk_offset + chunk_size`)
//!   lies entirely past the new end; rows that straddle the cut are left
//!   intact. Reads are bounded by the committed flush frontier — every
//!   [`YbSegmentReader`] is capped at `flush_lsn`, which sits at the
//!   truncation point — so the over-kept tail is never served.
//! - `remove_up_to(segno)` deletes all rows with `segno < segno_up_to`.
//!
//! ## Batching (producer / consumer)
//!
//! `write_wal()` is the producer: it parses the incoming WAL byte range
//! into one `PendingChunk` per segment, stamps each with an `end_lsn`,
//! then pushes them onto a bounded `mpsc` channel. It does **not** wait
//! for YB.
//!
//! A background consumer task (spawned in `YbWalStorage::new`) drains the
//! channel greedily into a batch — up to [`FLUSH_ROWS_THRESHOLD`] chunks
//! or [`FLUSH_BYTES_THRESHOLD`] bytes — stamps the whole batch with one
//! placement bucket, coalesces contiguous chunks into single rows
//! ([`coalesce_runs`]), writes it, and
//! publishes the highest end_lsn of each successful batch onto a
//! `tokio::sync::watch` watermark. This lets the producer keep pushing
//! the *next* batch while the previous one is in flight, which the
//! single-threaded design couldn't do (every `flush_pending` held
//! `&mut self` across the YB round-trip).
//!
//! Durability is acked off the ingest path. `flush_lsn()` is capped at
//! `min(record boundary, durable_lsn)`, so nothing is ever advertised past
//! what has landed in YB. Steady-state commits are acked as that watermark
//! advances: `handle_flush` kicks the consumer without waiting
//! (`yb_flush(YbFlush::Notify)`), and the WAL acceptor's durable-watch arm
//! emits an `AppendResponse` per advance — so the ingest loop never blocks on
//! the YB round trip. The blocking barrier — `flush_wal`
//! (`yb_flush(YbFlush::NotifyAndWait)`), which records `submitted_max_lsn` and
//! awaits the watermark — is kept only where a synchronous durable point is
//! needed (`handle_elected` / sync-safekeepers). A crash between `write_wal()` returning
//! and the batch landing in YB is a no-op for the compute (no commit ack was
//! sent).
//!
//! `truncate_wal` blocks the same way before issuing the DELETE, so a
//! pending chunk past `end_pos` cannot land after the truncate.
//!
//! ## Failure handling
//!
//! On a batch INSERT failure the consumer retries the batch with capped
//! backoff until it lands — the chunks are not durable, so the batch can
//! be neither dropped nor allowed to end the task (#770). Once the batch
//! has been failing for `CONSUMER_RETRY_SIGNAL_AFTER` the failure is
//! published so blocked waiters fail fast and the walproposer reconnects;
//! a later success publishes `Healthy` again, so `Failed` means
//! "currently failing", not "dead".
//! Both durability paths surface the failure: blocking `flush_wal` waiters
//! propagate it, and the WAL acceptor's durable-watch arm bails on `Failed`
//! (or on the watch channel closing) so the connection terminates rather
//! than hanging on a frozen watermark.
//!
//! `remove_up_to` does **not** wait — by construction it only deletes
//! segments well below the writer's current position, which the
//! channel never holds.

use std::pin::Pin;
use std::sync::Arc;
use std::sync::atomic::{AtomicU64, Ordering};
use std::task::{Context as TaskContext, Poll};
use std::time::{Duration, SystemTime, UNIX_EPOCH};

use anyhow::{Context, Result};
use futures::future::{BoxFuture, FutureExt};
use postgres_ffi::wal_record_boundary::WalStreamTracker;
use postgres_ffi::{PgMajorVersion, XLogSegNo};
use tokio::io::{AsyncRead, ReadBuf};
use tokio::sync::{mpsc, watch};
use utils::id::BranchId;
use utils::lsn::Lsn;
use utils::payload_codec;
use yb_tokio_postgres::types::ToSql;

use crate::metrics::{WAL_STORAGE_OPERATION_SECONDS, WalStorageMetrics};
use crate::state::TimelinePersistentState;
use crate::wal_cache::WalCache;
use crate::wal_storage::Storage;

// Re-export the auto-reconnecting Postgres client wrapper shared with
// the pageserver's `YbStore`. See `ysql_client::YsqlClient` for the
// reconnect / retry-once semantics.
pub use ysql_client::YsqlClient;

/// The single YB table the safekeeper mirrors WAL into. Hardcoded —
/// the connection config carries only host/port/credentials, never a
/// table name. Schema-qualified (`amp_storage.wals`); every query
/// references it fully so it doesn't depend on the connection's
/// `search_path`.
const WALS_TABLE: &str = "amp_storage.wals";

/// Number of placement buckets in `amp_storage.wals`, and the number of
/// tablets the migration pre-splits it into. `bucket_id` is the LEADING
/// key column; the consumer stamps each BATCH with one bucket, rotating
/// `(counter + 1) % WALS_BUCKETS` per batch, so a branch's write load spreads
/// across tablets instead of funneling into one, while each batch stays
/// single-tablet-atomic.
///
/// Must match the `SPLIT AT VALUES` boundary count in migration 0002, and the
/// `wals_bucket_id_in_range` CHECK there. 6 matches `pages` and keeps the
/// leader count even on a 3-node RF=3 cluster (2 per node).
const WALS_BUCKETS: i16 = 6;

/// The `version` key column of `wals`: always 0 for now (see migration
/// `0003_compute_storage`).
///
/// The controller's branch reaper (`controller/internal/branchgc`) reclaims
/// only `version = 0` from the hash-keyed `slru_pages`, `meta` and
/// `pending_gc`. Before any other version is written, fix it to delete every
/// version.
const DEFAULT_VERSION: i16 = 0;

/// `bucket_id IN (0,1,...,WALS_BUCKETS-1)` for interpolation into `wals`
/// queries. Readers can't derive a chunk's bucket (assignment is
/// per consumer batch at write time, placement-only), so every
/// index-tight read names every bucket. `bucket_id` leads the all-range key,
/// so YSQL turns this IN-list into batched per-bucket index probes, not a scan.
fn bucket_in_list() -> String {
    let ids: Vec<String> = (0..WALS_BUCKETS).map(|b| b.to_string()).collect();
    format!("bucket_id IN ({})", ids.join(","))
}

/// How many of the highest rows [`YbWalStorage::load_tail`] reads, which is
/// what bounds where [`YbWalStorage::contiguous_wal_end`] can see a hole.
///
/// The consumer writes one batch at a time and retries it until it lands, so
/// only the batch in flight when the process died can be partly applied, and
/// its rows are the highest in `wals`. A batch is at most
/// [`FLUSH_ROWS_THRESHOLD`] chunks, so at most that many rows. Every row above
/// a hole therefore belongs to that batch, and the top `FLUSH_ROWS_THRESHOLD +
/// 1` rows always hold the hole and the row below it, whatever the chunk sizes.
/// Overlapping batches would break this: the bound has to grow with the number
/// in flight.
const TAIL_SCAN_ROWS: usize = FLUSH_ROWS_THRESHOLD + 1;

/// How many padding-shaped gaps [`YbWalStorage::contiguous_wal_end`] will
/// spend a decode on. One `XLOG_SWITCH` in the tail is the realistic case; the
/// budget only stops a pathological table from turning startup into a decode
/// loop. Gaps past it are treated as padding — see that function on why
/// ambiguity resolves toward continuing.
const MAX_PADDING_PROBES: usize = 8;

/// The `k` highest of `rows` — `wals` rows led by their start LSN — sorted
/// ascending.
///
/// The per-bucket probes each return their own bucket's top rows, and their
/// union holds the global top `k`: a row in the global top `k` has fewer than
/// `k` rows above it in its own bucket. It can also hold rows from much further
/// down a bucket that has been quiet at the top, and between those sit rows of
/// other buckets that were never fetched, which would read as gaps. Cutting to
/// the global top `k` leaves a run with no unfetched row inside it.
fn global_tail<T: Ord>(mut rows: Vec<T>, k: usize) -> Vec<T> {
    rows.sort_unstable();
    let below = rows.len().saturating_sub(k);
    rows.drain(..below);
    rows
}

/// Why the rows above a hole at `cut` can't be what a cut-short flush leaves,
/// or `None` when they can be. `tail` is [`WalTail::rows`].
///
/// A cut-short flush leaves the upper runs of the one batch in flight: one
/// bucket, and fewer than [`FLUSH_ROWS_THRESHOLD`] rows since at least one is
/// missing. More than that is later batches landing above the hole, and those
/// may have been acked: a build without the hole scan rehydrated past a hole and
/// kept acking commits stored above it. Deleting them would lose committed WAL.
fn not_one_partial_batch(tail: &[(u64, u64, i16)], cut: u64) -> Option<String> {
    let above: Vec<&(u64, u64, i16)> = tail.iter().filter(|r| r.0 > cut).collect();
    let mut buckets: Vec<i16> = above.iter().map(|r| r.2).collect();
    buckets.sort_unstable();
    buckets.dedup();
    if buckets.len() > 1 {
        return Some(format!("{} rows across buckets {buckets:?}", above.len()));
    }
    if above.len() >= FLUSH_ROWS_THRESHOLD {
        return Some(format!(
            "{} rows, more than one batch can leave",
            above.len()
        ));
    }
    None
}

/// Every gap in `spans` (sorted ascending) as `(gap_start, gap_end)` pairs.
///
/// Walks the whole list rather than stopping at the first gap, because the
/// caller has to classify each one (padding vs hole) before it knows which
/// gap actually ends the WAL.
///
/// A row that starts at or below the running end closes no gap and is folded
/// in with `max`: rows never overlap, but a row left behind by a failed
/// torn-tail cleanup can, and it must not read as a gap.
fn tail_gaps(spans: &[(u64, u64)]) -> Vec<(u64, u64)> {
    let Some(&(first_start, _)) = spans.first() else {
        return Vec::new();
    };
    let mut gaps = Vec::new();
    let mut end = first_start;
    for &(start, span_end) in spans {
        if start > end {
            gaps.push((end, start));
        }
        end = end.max(span_end);
    }
    gaps
}

/// Could `[gap_start, gap_end)` be `XLOG_SWITCH` padding? Padding runs from
/// the switch record's end to the end of that segment and no further, so the
/// gap has to close exactly on the next segment boundary above `gap_start`. A
/// gap of any other shape is a hole, no decode needed.
fn gap_is_padding_shaped(gap_start: u64, gap_end: u64, wal_seg_size: u64) -> bool {
    gap_end == (gap_start / wal_seg_size + 1) * wal_seg_size
}

/// Walk `spans` up to the first gap that isn't in `padding`, and return where
/// the reachable run ends.
///
/// The walk anchors at the lowest span: everything below the tail is contiguous
/// by construction (see [`TAIL_SCAN_ROWS`]). `raw_max` is the answer for an
/// empty tail, which only an empty branch has.
fn contiguous_end(spans: &[(u64, u64)], raw_max: u64, padding: &[(u64, u64)]) -> u64 {
    let Some(&(first_start, _)) = spans.first() else {
        return raw_max;
    };
    let mut end = first_start;
    for &(start, span_end) in spans {
        if start > end && !padding.contains(&(end, start)) {
            break;
        }
        end = end.max(span_end);
    }
    end
}

/// Rows one [`YbWalSource::contiguous_chunks_from`] fetch may return. The segno
/// scoping bounds the bytes; this bounds row count, which it does not — rows are one
/// coalesced consumer batch each, so tiny commits can put ~110k in a 16 MiB segment.
/// Set above a full segment's normal count (~11k at OLTP's ~1.5 KiB rows) so the
/// ordinary case is one query per segment.
const CHUNK_FETCH_ROW_CAP: usize = 16384;

/// Handles to the YB connections that mirror live WAL into
/// `amp_storage.wals`. Constructed at safekeeper boot via
/// [`YbWalConn::from_conf`] and shared across timelines via
/// `Arc<YbWalConn>`. Used by the WAL writer ([`YbWalStorage::new`]),
/// the WAL reader ([`YbSegmentReader::new`]), and the control-file
/// storage ([`crate::control_file::YbControlFileStorage`]).
///
/// The hot path — WAL chunk writes ([`consumer_main`]) and the WAL
/// sender's chunk reads ([`YbSegmentReader`]) — runs on
/// [`Self::yb_thinclient`]. Everything else runs over YSQL on the single
/// [`Self::system_client`] connection, which carries its own
/// `application_name` for attribution in YB's `pg_stat_activity`.
pub struct YbWalConn {
    /// System / background lane (no upsert GUC). Used for startup
    /// rehydration ([`YbWalStorage::load_tail`]), the GC DELETE
    /// ([`YbWalStorage::remove_up_to`]), and the idempotent
    /// control-file ops (`restore_opt` SELECT, `write_initial_state` and `persist`
    /// UPDATEs — the control file's row is provisioned by the controller, so
    /// the safekeeper only ever UPDATEs its own columns and every write here is
    /// naturally idempotent).
    pub system_client: Arc<YsqlClient>,
    /// Name of the YB table that backs the WAL mirror. Always
    /// [`WALS_TABLE`] (`amp_storage.wals`); the field stays so the SQL
    /// builders below can interpolate it.
    pub table: String,
    /// In-memory cache of recent WAL chunks, shared between the
    /// writer ([`YbWalStorage::upsert_writes`]) and the reader
    /// ([`YbSegmentReader`]). `None` when the operator has set
    /// `--wal-cache-size-mib 0` to disable the cache entirely (every
    /// WAL read then hits YB).
    pub cache: Option<Arc<std::sync::Mutex<WalCache>>>,
    /// The hot-path lane: a `yb_thin_client` handle for `amp_storage.wals`,
    /// speaking `Perform` to a tserver through the `libyb_thin_client` C++
    /// shim. Opened and health-checked at startup; carries every WAL chunk
    /// write and read. Control file, GC, and rehydration stay on the YSQL
    /// system lane.
    ///
    /// Held through a [`yb_thin_client::Recycling`], which re-connects the shim
    /// client and re-opens `wals` once the current generation passes
    /// `YbConnConfig::connection_lifetime_ms` — re-resolving the configured
    /// tserver addresses, so a connection cannot outlive the tservers it was
    /// aimed at (#811). Each write/read takes the current generation for the
    /// duration of its call.
    pub yb_thinclient: Arc<YbThinPool>,
    /// Codec applied to the `data` column on the way out to YB, from
    /// `yb_wal_compression`. Write-side only — the read path decodes
    /// whatever tag each stored row carries, so rows written under
    /// different settings coexist and a codec change needs no migration.
    pub codec: payload_codec::Codec,
}

/// See [`YbWalConn::yb_thinclient`]. The `yb_thin_client` (shim) lane. One
/// generation of [`YbThinPool`]: `wals` belongs to `client`, so the two are
/// replaced together.
pub struct YbThinClient {
    pub client: Arc<yb_thin_client::TsClient>,
    pub wals: Arc<yb_thin_client::TableHandle>,
}

/// The recycled holder of [`YbThinClient`] — see [`YbWalConn::yb_thinclient`].
pub type YbThinPool = yb_thin_client::Recycling<YbThinClient>;

impl YbWalConn {
    /// Build from the SK config. Returns `None` when
    /// `yb_storage_conn` isn't set (which yb-amp shouldn't be
    /// running without — every persistence path needs this).
    pub async fn from_conf(conf: &crate::SafeKeeperConf) -> anyhow::Result<Option<Arc<YbWalConn>>> {
        use anyhow::Context;
        let Some(yb) = conf.yb_storage_conn.as_ref() else {
            return Ok(None);
        };
        let endpoint = format!("{} (sslmode={:?})", yb.host_ports, yb.sslmode);
        // One YSQL connection, with its own application_name so it is
        // attributable in YB's pg_stat_activity; the hot path runs on the
        // thin-client lane opened below. The system lane runs without
        // `yb_enable_upsert_mode`: it carries the startup SELECT
        // (`load_tail`), the GC DELETE (`remove_up_to`), and the
        // control-file ops (`restore_opt` SELECT, `write_initial_state` and
        // `persist` UPDATEs) — all UPDATEs against a row the controller
        // provisioned at branch create, so a blind retry can't hit
        // duplicate-key.
        let system_client = Arc::new(
            ysql_client::YsqlClient::from_yb_config(yb, "safekeeper:yb_wal_system", false)
                .await
                .with_context(|| format!("connect WAL system lane at {endpoint}"))?,
        );
        let cache = if conf.wal_cache_size_mib > 0 {
            Some(Arc::new(std::sync::Mutex::new(WalCache::new(
                conf.wal_cache_size_mib * 1024 * 1024,
            ))))
        } else {
            None
        };
        let yb_thinclient = Self::connect_thin(yb, &system_client)
            .await
            .context("thin-client hot path init")?;
        Ok(Some(Arc::new(YbWalConn {
            system_client,
            table: WALS_TABLE.to_string(),
            cache,
            yb_thinclient,
            codec: conf.yb_wal_compression.into(),
        })))
    }

    /// One-time catalog lookup for the direct-RPC hot-path lane: the
    /// database OID and `amp_storage.wals`'s pg_class OID.
    async fn lookup_wals_oids(
        system_client: &ysql_client::YsqlClient,
    ) -> anyhow::Result<(u32, u32)> {
        use anyhow::Context;
        let (schema, relname) = WALS_TABLE
            .split_once('.')
            .expect("WALS_TABLE is schema-qualified");
        let (db_oid, oids) = ysql_client::lookup_table_oids(system_client, schema, &[relname])
            .await
            .context("looking up wals table OIDs")?;
        let wals_oid = *oids
            .get(relname)
            .ok_or_else(|| anyhow::anyhow!("wals table {relname} missing from OID lookup"))?;
        Ok((db_oid, wals_oid))
    }

    /// Connects the thin-client (`yb_thin_client`) lane and opens
    /// `amp_storage.wals`, validating tserver connectivity and table
    /// layout in one shot. Fail-fast at startup.
    async fn connect_thin(
        yb: &ysql_client::YbConnConfig,
        system_client: &Arc<ysql_client::YsqlClient>,
    ) -> anyhow::Result<Arc<YbThinPool>> {
        use anyhow::Context;
        let (db_oid, wals_oid) = Self::lookup_wals_oids(system_client).await?;
        // WAL lane: one read + one write session by default (low-concurrency —
        // one-batch-at-a-time flush + segment reads), round-robin across the
        // configured tservers. `yb_storage_conn` can pin both counts.
        let read_sessions = yb.tsclient_read_sessions.unwrap_or(1);
        let write_sessions = yb.tsclient_write_sessions.unwrap_or(1);
        let sessions_per_conn = yb.tsclient_sessions_per_conn.unwrap_or(4);
        let tserver_addrs = yb.effective_tserver_rpc_addrs();
        let tserver_count = tserver_addrs.len();
        let tls = yb
            .thin_client_tls()
            .context("TLS for the thin-client lane")?;
        let tls_desc = match &tls {
            None => "plaintext".to_string(),
            Some(t) if t.client.is_some() => "mTLS".to_string(),
            Some(_) => "TLS".to_string(),
        };
        let config = yb_thin_client::TsClientConfig {
            tserver_addrs,
            tls: tls.map(|t| {
                yb_thin_client::TlsOpts::from_paths(
                    t.ca_cert_path,
                    t.client.map(|c| (c.cert_path, c.key_path)),
                )
            }),
            read_sessions: read_sessions as u32,
            write_sessions: write_sessions as u32,
            sessions_per_conn: sessions_per_conn as u32,
            ..Default::default()
        };
        // The OID lookup above runs once (OIDs don't move); connect and
        // `open_table` go into the closure the pool re-runs on each recycle.
        let pool = yb_thin_client::Recycling::new(
            "safekeeper WAL",
            config,
            yb.max_connection_lifetime(),
            move |client: Arc<yb_thin_client::TsClient>| {
                let wals = client.open_table(db_oid, wals_oid).map_err(|e| {
                    yb_thin_client::YbError::Other(format!(
                        "opening {WALS_TABLE} via the yb_thin_client lane: {e}"
                    ))
                })?;
                Ok(Arc::new(YbThinClient { client, wals }))
            },
        )
        .await
        .context("connecting the thin-client (yb_thin_client) client")?;
        tracing::info!(
            "thin-client (yb_thin_client) hot path ready: {WALS_TABLE} opened \
             ({read_sessions} read + {write_sessions} write sessions, {sessions_per_conn} per \
             connection, across {tserver_count} configured tserver(s), {tls_desc})",
        );
        Ok(pool)
    }
}

/// Flush the buffer when it accumulates this many chunks. Picked to amortize
/// the per-statement DocDB cost (single-row ≈ 0.6 ms; 50-row batch ≈ 2.5 ms
/// → ~12× fewer per-row ms).
const FLUSH_ROWS_THRESHOLD: usize = 50;

/// Flush the buffer when the total payload bytes cross this threshold,
/// even if `FLUSH_ROWS_THRESHOLD` hasn't been reached. Keeps memory bounded
/// and avoids ever building an INSERT that would push YB's
/// `rpc_max_message_size` (256 MiB default). 4 MiB leaves comfortable
/// headroom for the row metadata + protocol overhead.
const FLUSH_BYTES_THRESHOLD: usize = 4 * 1024 * 1024;

/// One row queued for the next batched INSERT. Carries `end_lsn` so the
/// consumer can publish a durability watermark after each batch.
///
/// `data` is held as `Arc<Vec<u8>>` so the same allocation is shared with
/// the in-memory [`WalCache`] entry — the read path doesn't need a copy.
struct PendingChunk {
    segno: u64,
    /// Placement bucket. The producer leaves this at 0 (placeholder);
    /// [`consumer_main`] stamps every chunk of an assembled batch with
    /// ONE bucket before any write attempt, so the whole batch lands on
    /// one tablet. Placement only, not identity: a retried flush replays the
    /// same stamped chunks, so a chunk keeps its bucket across retries.
    bucket_id: i16,
    /// `wals.chunk_offset`'s own INTEGER width.
    chunk_offset: i32,
    chunk_size: i32,
    data: Arc<Vec<u8>>,
    /// LSN immediately after this chunk's last byte. The consumer uses
    /// the last chunk's `end_lsn` in a batch as the post-flush watermark.
    end_lsn: Lsn,
}

/// Capacity of the producer→consumer `mpsc` channel. Bounded so that a
/// stalled consumer back-pressures the producer (`write_wal` blocks on
/// `tx.send().await`) rather than growing memory unboundedly.
const CHUNK_CHANNEL_CAPACITY: usize = 1024;

/// Maximum time the consumer waits for more chunks to accumulate into the
/// current batch once it already has at least one. Acts like a TCP
/// `Nagle` window: when chunks arrive faster than 1/`ACCUMULATION_MAX`,
/// batches grow to amortize the per-statement DocDB cost; when chunks
/// arrive slowly, the consumer flushes after a short wait rather than
/// sitting on the held-up batch.
///
/// `flush_wal()` cuts the wait short by signalling the
/// `flush_notify` — see `consumer_main` — so commit latency does not pay
/// for this accumulation.
const ACCUMULATION_MAX: Duration = Duration::from_millis(2);

/// Log-progress interval for [`YbWalStorage::wait_for_durable`].
const YB_DURABLE_WAIT_LOG_EVERY: Duration = Duration::from_secs(10);

/// Hard bound for [`YbWalStorage::wait_for_durable`]. Callers hold the
/// timeline shared_state write lock; see the method docs for why an
/// unbounded wait wedges the whole safekeeper.
const YB_DURABLE_WAIT_MAX: Duration = Duration::from_secs(60);

/// Bound for the best-effort torn-tail cleanup in [`YbWalStorage::new`], which
/// runs inside `load_timeline` where an unbounded wait hangs the load silently
/// (#812). Giving up is safe at any point, including between the delete and the
/// trim: readers are bounded by `accepted_byte_end`, so leftover rows are inert and the
/// next restart retries. Bounds the safekeeper, not YB — `statement_timeout` is
/// off on these connections, so the statement runs on regardless.
const YB_TORN_TAIL_CLEANUP_MAX: Duration = Duration::from_secs(60);

/// Durability state published by the consumer onto the watch channel.
#[derive(Clone, Debug)]
pub enum DurableState {
    /// Every `PendingChunk` whose `end_lsn <= self.0` has been durably
    /// INSERT'd into YugabyteDB.
    Healthy(Lsn),
    /// The consumer's current batch has been failing past its fail-fast
    /// bound. The consumer keeps retrying underneath and publishes
    /// [`Self::Healthy`] again on success; waiters treat this as an error
    /// now rather than blocking on a watermark that isn't moving. The
    /// string is the rendered error chain for propagation up through
    /// `flush_wal` / `write_wal`.
    Failed(String),
}

/// How far a YB flush should go — the arg to [`crate::wal_storage::Storage::yb_flush`].
#[derive(Clone, Copy, Debug)]
pub enum YbFlush {
    /// Wake the consumer and wait for the batch to be durable in
    /// `amp_storage.wals`. The synchronous durable-commit boundary used by
    /// `handle_elected` / sync-safekeepers.
    NotifyAndWait,
    /// Wake the consumer without waiting. Steady-state commit; durability is
    /// acked later off the ingest path.
    Notify,
}

pub struct YbWalStorage {
    /// End of the last complete record accepted; reported by
    /// [`Storage::flush_lsn`], capped there at `durable_lsn`.
    accepted_stream: WalStreamTracker,
    /// Segment-header material for [`Storage::initialize_first_segment`].
    pg_version: PgMajorVersion,
    system_id: u64,
    metrics: WalStorageMetrics,
    /// Background-write lane (no upsert GUC). Used by
    /// [`Self::remove_up_to`] for the GC DELETE. The hot-path chunk writes
    /// and reads run on the thin-client lane instead.
    ysql_system_client: Arc<YsqlClient>,
    branch_id: BranchId,
    table_name: String,
    wal_seg_size: usize,
    /// Producer side of the chunk channel.
    tx: mpsc::Sender<PendingChunk>,
    /// Receiver side of the durability watermark.
    watermark_rx: watch::Receiver<DurableState>,
    /// Pinged by `flush_wal` / `truncate_wal` to short-circuit the
    /// consumer's accumulation wait. See `ACCUMULATION_MAX`.
    flush_notify: Arc<tokio::sync::Notify>,
    /// Highest `end_lsn` the producer has pushed onto `tx`. `flush_wal`
    /// awaits the watermark catching up to this.
    submitted_max_lsn: Lsn,
    /// Bounds for [`Self::wait_for_durable`]; the consts by default,
    /// shrinkable in tests.
    durable_wait_log_every: Duration,
    durable_wait_max: Duration,
    /// In-memory cache of recent WAL chunks, shared with
    /// [`YbSegmentReader`] (the reader side). Populated by
    /// [`Self::upsert_writes`] immediately, in addition to the
    /// producer/consumer path that INSERTs into YB. Read access goes
    /// shared with the WAL sender through the same `Arc<YbWalConn>`
    /// so the reader doesn't need a separate plumbing path.
    ///
    /// `None` when the operator has set `--wal-cache-size-mib 0`.
    cache: Option<Arc<std::sync::Mutex<WalCache>>>,
    /// Lower bound for the next `remove_up_to` DELETE. Rows with
    /// `segno < delete_lower_bound` are known to no longer exist (already
    /// deleted, or never inserted on this timeline), so naming it keeps the
    /// DELETE's range tight. Advanced after each successful `remove_up_to(N)`
    /// to `N`. Pinned in an `Arc<AtomicU64>` because `remove_up_to` returns a
    /// `'static` future.
    ///
    /// Starts at [`DELETE_BOUND_UNRESOLVED`] and is resolved by the first
    /// removal, from [`YbWalStorage::load_min_segno`] — **not** at
    /// construction. That query is a forward seek from the bottom of the
    /// branch's key range, the span GC has emptied, so it pays for every deleted
    /// key it crosses: 51 s on a branch that had churned ~260 GB of WAL. On the
    /// startup path that was a load that never finished, which took the branch's
    /// endpoint with it (#874). In the spawned removal task it holds no timeline
    /// lock and nothing waits on it.
    ///
    /// Resolved exactly once per load, successfully or not — see the sentinel
    /// for why a plain 0 could not mark "unresolved".
    delete_lower_bound: Arc<AtomicU64>,
    /// Highest LSN whose chunks are confirmed in `wals`. Advanced by
    /// the consumer after each successful batch INSERT. Read by
    /// `flush_lsn()` to cap the externally-reported flush watermark at
    /// true YB durability — the in-memory record boundary can
    /// otherwise be moved ahead of YB by `truncate_wal(end_pos)` when
    /// `end_pos > submitted_max_lsn`, opening a permanent gap. Initialised
    /// to the rehydrated end of WAL at construction. `truncate_wal` lowers it
    /// to the cut.
    durable_lsn: Arc<AtomicU64>,
    /// Kept so the consumer task isn't dropped immediately on
    /// `YbWalStorage` drop; closing `tx` is what signals shutdown.
    _consumer_handle: tokio::task::JoinHandle<()>,
}

impl YbWalStorage {
    /// `retention_floor` — the pageserver's `consistent_lsn`, see
    /// `ControlFileStorage::restore_opt` — floors the end-of-WAL scan (#788).
    pub async fn new(
        branch_id: BranchId,
        state: &TimelinePersistentState,
        retention_floor: Lsn,
        yb_wal: Arc<YbWalConn>,
    ) -> Result<Self> {
        let system_client = yb_wal.system_client.clone();
        let table_name = yb_wal.table.clone();
        let cache = yb_wal.cache.clone();
        let wal_seg_size = state.server.wal_seg_size as usize;

        // Rehydrate the end of WAL from YB rather than scanning local FS: the
        // highest stored chunk end, plus the rows just below it for the hole
        // scan. Startup work → system lane so it doesn't share the
        // streaming-read connection.
        let tail = Self::load_tail(
            &system_client,
            &table_name,
            branch_id,
            wal_seg_size,
            state.timeline_start_lsn,
            retention_floor,
        )
        .await?;
        let raw_max = tail.end;
        let pg_version =
            PgMajorVersion::try_from(state.server.pg_version).unwrap_or(PgMajorVersion::PG18);
        // A flush cut short after its batch's rows split across tablets can
        // leave rows above a hole. None of them were acked, and nothing can
        // replay across the gap, so the end of WAL is the hole's lower edge
        // and the `truncate_wals_above` below deletes the rows past it — but
        // only when they are that one batch (see `not_one_partial_batch`).
        let max_offset = if raw_max > state.timeline_start_lsn {
            // Its own source, so the end-of-WAL scan's query counts below stay
            // that scan's cost.
            let hole_src = YbWalSource {
                client: &system_client,
                table: &table_name,
                branch_id,
                seg: wal_seg_size as u64,
                find_anchor_queries: AtomicU64::new(0),
                find_end_of_wal_queries: AtomicU64::new(0),
                read_failures: AtomicU64::new(0),
            };
            let t = std::time::Instant::now();
            let contiguous = Self::contiguous_wal_end(
                &hole_src,
                &tail.spans(),
                raw_max,
                pg_version,
                retention_floor,
            )
            .await?;
            let probe_queries = hole_src.find_anchor_queries.load(Ordering::Relaxed)
                + hole_src.find_end_of_wal_queries.load(Ordering::Relaxed);
            if probe_queries > 0 {
                tracing::info!(
                    branch_id = %branch_id,
                    %probe_queries,
                    hole_scan_ms = %t.elapsed().as_millis(),
                    "YbWalStorage::new — tail hole scan decoded padding-shaped gaps",
                );
            }
            if contiguous < raw_max {
                if let Some(why) = not_one_partial_batch(&tail.rows, contiguous.0) {
                    return Err(anyhow::anyhow!(
                        "YbWalStorage::new — hole in the persisted WAL tail at {contiguous} below \
                         {raw_max} for branch {branch_id}, but the rows above it ({why}) are not \
                         the one batch a cut-short flush leaves; they may hold acked WAL, so the \
                         load refuses to delete them"
                    ));
                }
                tracing::warn!(
                    branch_id = %branch_id,
                    contiguous_end = %contiguous,
                    raw_max_end = %raw_max,
                    "YbWalStorage::new — hole in the persisted WAL tail; \
                     end-of-WAL is the contiguous end and the rows past it are dropped",
                );
            }
            contiguous
        } else {
            raw_max
        };

        // `max_offset` is a BYTE offset, which lands mid-record when an
        // ungraceful compute shutdown left a torn trailing page (#61), or when
        // a hole cut through a record. Treating that as the durable end-of-WAL
        // makes the interpreted reader decode into the torn page and panic.
        // Replay the decoder to the last complete record boundary <=
        // max_offset — where Postgres crash recovery also stops — and use
        // THAT as the end of WAL, discarding the torn tail (the compute
        // re-streams from here and overwrites it).
        let end_of_wal = {
            let src = YbWalSource {
                client: &system_client,
                table: &table_name,
                branch_id,
                seg: wal_seg_size as u64,
                find_anchor_queries: AtomicU64::new(0),
                find_end_of_wal_queries: AtomicU64::new(0),
                read_failures: AtomicU64::new(0),
            };
            // Timed apart: the backward walk and the forward replay get expensive
            // under opposite conditions, so a combined figure names neither.
            let mut find_anchor_ms = 0u128;
            let mut find_end_of_wal_ms = 0u128;
            // YB keeps no persisted record-boundary flush_lsn to start from
            // (the upstream safekeeper read it from the control file), so
            // anchor on a recent fresh-record page at or below the raw end,
            // then scan forward to the true end of WAL via `find_end_of_wal`.
            use postgres_ffi::wal_record_boundary::{find_anchor, find_end_of_wal};
            let scanned = if max_offset <= state.timeline_start_lsn {
                Ok(max_offset)
            } else {
                // The floor must be a boundary that still exists: `remove_up_to`
                // drops whole segnos above `timeline_start_lsn`, so anchoring
                // there walks into a hole and fails the load (#788). GC never
                // trims below `consistent_lsn`, which `retention_floor` carries.
                //
                // Passed through unclamped: a trailing XLOG_SWITCH legitimately
                // puts it *above* `max_offset`, and clamping would land
                // `end_of_wal` in the segment padding (#181). `find_anchor`
                // returns a floor at or above its target, and `find_end_of_wal`
                // hands it back, so the `end > max_offset` arm below honors it.
                let target = anchor_target(max_offset, wal_seg_size as u64);
                let t = std::time::Instant::now();
                let anchor = find_anchor(&src, target, retention_floor).await;
                find_anchor_ms = t.elapsed().as_millis();
                let t = std::time::Instant::now();
                let scanned = match anchor {
                    Ok(anchor) => find_end_of_wal(&src, anchor, pg_version).await,
                    Err(e) => Err(e),
                };
                find_end_of_wal_ms = t.elapsed().as_millis();
                scanned
            };
            // Every probe is a YB round trip, so the counts are the scan's cost.
            tracing::info!(
                branch_id = %branch_id,
                find_anchor_queries = %src.find_anchor_queries.load(Ordering::Relaxed),
                %find_anchor_ms,
                find_end_of_wal_queries = %src.find_end_of_wal_queries.load(Ordering::Relaxed),
                %find_end_of_wal_ms,
                "YbWalStorage::new — end-of-WAL scan finished",
            );
            match scanned {
                Ok(end) if end < max_offset => {
                    tracing::warn!(
                        branch_id = %branch_id,
                        torn_end = %max_offset,
                        valid_end = %end,
                        "YbWalStorage::new — truncated torn WAL tail to the last valid record boundary",
                    );
                    end
                }
                Ok(end) if end > max_offset => {
                    // The last record is an XLOG_SWITCH (pg_switch_wal): its
                    // end-of-record LSN rounds *up* to the next segment
                    // boundary, past the segment's trailing zero padding. The
                    // compute doesn't stream that padding, so the persisted
                    // byte range stops at (or inside) it while the true flush
                    // position is the next segment. Honor it rather than clamping
                    // back to max_offset — clamping leaves end_of_wal inside the
                    // padding, where the compute resumes streaming, the reader hits
                    // `invalid xl_tot_len 0`, and the safekeeper wedges (#181).
                    //
                    // The boundary reaches here two ways, and neither is verified as
                    // a switch: find_end_of_wal decoded one, or `retention_floor`
                    // already sat above max_offset and was handed straight back. So
                    // the log names the observation, not the cause.
                    tracing::info!(
                        branch_id = %branch_id,
                        max_offset = %max_offset,
                        end_of_wal = %end,
                        "YbWalStorage::new — end-of-WAL is past the stored WAL end (expected after pg_switch_wal)",
                    );
                    end
                }
                Ok(_) => max_offset,
                Err(e) => {
                    // Neither guess is safe: truncating to the boundary reached so
                    // far DELETEs durable rows whose commits were acked (#756), and
                    // the raw max offset may sit mid-record, where every `wait_lsn`
                    // hangs (#678). Availability cost: runbooks/startup-failures.md
                    // §P. The integration harness's FATAL_MARKERS greps this text.
                    return Err(e).with_context(|| {
                        format!(
                            "YbWalStorage::new — end-of-WAL record-boundary scan failed for \
                             branch {branch_id} below max_offset {max_offset}"
                        )
                    });
                }
            }
        };
        // Physically discard everything above the valid boundary — a torn
        // tail, and rows orphaned above a hole — so the table ends exactly
        // there. Otherwise the dropped rows linger past `durable_lsn`
        // (violating `truncate_wal`'s "no rows past end_pos" invariant) and
        // would be re-counted by `load_tail` on the next restart.
        //
        // Against the RAW max: after a hole `end_of_wal` usually lands exactly
        // on the contiguous end, so comparing with that would leave the
        // orphans in place on every restart.
        //
        // Best-effort: the reader is already bounded by `end_of_wal`, so a
        // failed cleanup is harmless and retried next restart.
        if end_of_wal < raw_max {
            let cleanup = truncate_wals_above(
                &system_client,
                &table_name,
                branch_id,
                wal_seg_size,
                end_of_wal,
            );
            // Bounded: see `YB_TORN_TAIL_CLEANUP_MAX`. A timeout is reported
            // like any other cleanup failure — the load carries on.
            let outcome = match tokio::time::timeout(YB_TORN_TAIL_CLEANUP_MAX, cleanup).await {
                Ok(res) => res.err().map(|e| format!("{e:#}")),
                Err(_) => Some(format!("timed out after {YB_TORN_TAIL_CLEANUP_MAX:?}")),
            };
            if let Some(why) = outcome {
                tracing::warn!(
                    branch_id = %branch_id,
                    %end_of_wal,
                    %raw_max,
                    "YbWalStorage::new — failed to delete torn WAL tail (harmless; retried next restart): {why}",
                );
            }
        }
        tracing::info!(
            branch_id = %branch_id,
            %end_of_wal,
            cfile_timeline_start_lsn = %state.timeline_start_lsn,
            %retention_floor,
            "YbWalStorage::new — rehydrated the end of WAL from amp_storage.wals \
             (last valid record boundary; falls back to timeline_start_lsn on empty)",
        );

        // Producer/consumer plumbing. The watermark starts at
        // `end_of_wal` so an immediately-following `flush_wal()` with
        // `submitted_max_lsn == end_of_wal` returns at once (nothing to
        // wait for).
        let (tx, rx) = mpsc::channel::<PendingChunk>(CHUNK_CHANNEL_CAPACITY);
        let (watermark_tx, watermark_rx) = watch::channel(DurableState::Healthy(end_of_wal));
        let flush_notify = Arc::new(tokio::sync::Notify::new());
        let durable_lsn = Arc::new(AtomicU64::new(end_of_wal.0));
        let consumer_handle = tokio::spawn(consumer_main(
            rx,
            yb_wal.yb_thinclient.clone(),
            branch_id,
            watermark_tx,
            flush_notify.clone(),
            durable_lsn.clone(),
            cache.clone(),
            yb_wal.codec,
        ));

        // The DELETE lower bound is left unestablished; the first removal
        // resolves it. See the field's doc for why not here.

        let me = YbWalStorage {
            accepted_stream: WalStreamTracker::new(end_of_wal, pg_version),
            pg_version,
            system_id: state.server.system_id,
            metrics: WalStorageMetrics::default(),
            ysql_system_client: system_client,
            branch_id,
            table_name,
            wal_seg_size,
            tx,
            watermark_rx,
            flush_notify,
            submitted_max_lsn: end_of_wal,
            durable_wait_log_every: YB_DURABLE_WAIT_LOG_EVERY,
            durable_wait_max: YB_DURABLE_WAIT_MAX,
            cache,
            delete_lower_bound: Arc::new(AtomicU64::new(DELETE_BOUND_UNRESOLVED)),
            durable_lsn,
            _consumer_handle: consumer_handle,
        };

        // The post-crash scrub is the hole scan plus `truncate_wals_above`
        // above. Committed-but-unacked rows from a crashed run are absorbed as
        // durable wherever they extend the contiguous run; the ones stranded
        // above a hole are discarded there, because nothing can replay across
        // the gap below them.
        Ok(me)
    }

    /// The end of the persisted WAL, and the [`TAIL_SCAN_ROWS`] highest rows
    /// below it for [`Self::contiguous_wal_end`]. Falls back to `commit_lsn`
    /// when the table is empty (fresh timeline). Startup-only: one index probe
    /// of at most `TAIL_SCAN_ROWS` metadata rows per bucket.
    ///
    /// Each probe stops at the segment before `floor`'s, the oldest one GC keeps
    /// (`remove_up_to` deletes below `consistent_lsn`'s segment minus one). A
    /// bucket with fewer than `TAIL_SCAN_ROWS` rows above that would otherwise
    /// scan on through every deleted key below it, the cost that took
    /// [`Self::load_min_segno`] off this path (#874). Nothing the hole scan needs
    /// is lower: the pageserver can't read past a hole, so a hole and the row
    /// ending at it are at or above `floor`. `Lsn(0)` means no bound.
    ///
    /// `ORDER BY segno DESC, chunk_offset DESC` yields each bucket's highest
    /// rows because rows never overlap (the writer never produces overlapping
    /// chunks and `truncate_wal` / `truncate_wals_above` keep it that way): within
    /// a segno the row with the highest `chunk_offset` also has the highest
    /// `chunk_offset + chunk_size`.
    ///
    /// The highest end is the end of WAL only when the stored rows are a
    /// contiguous prefix of the stream. A partly applied batch breaks that, so
    /// the caller pairs this with [`Self::contiguous_wal_end`].
    async fn load_tail(
        client: &YsqlClient,
        table: &str,
        branch_id: BranchId,
        wal_seg_size: usize,
        commit_lsn: Lsn,
        floor: Lsn,
    ) -> Result<WalTail> {
        let seg = wal_seg_size as u64;
        let floor_segno = (floor.0 / seg).saturating_sub(1);
        let mut rows: Vec<(u64, u64, i16)> = Vec::new();
        for bucket in 0..WALS_BUCKETS {
            let sql = bake_branch_id(
                &format!(
                    "SELECT segno, chunk_offset, chunk_size FROM {} \
                     WHERE branch_id = $1 AND version = {DEFAULT_VERSION} AND bucket_id = {bucket} \
                       AND segno >= {floor_segno} \
                     ORDER BY segno DESC, chunk_offset DESC \
                     LIMIT {TAIL_SCAN_ROWS}",
                    quote_relation(table)
                ),
                branch_id,
            );
            let probed = client
                .query(&sql, &[])
                .await
                .context("YbWalStorage: probe the WAL tail in YB (per-bucket)")?;
            for row in probed {
                let segno: i64 = row.get("segno");
                // `chunk_offset` is INTEGER: tokio-postgres reads it only as `i32`.
                let chunk_offset: i32 = row.get("chunk_offset");
                let chunk_size: i32 = row.get("chunk_size");
                let start = segno as u64 * seg + chunk_offset as u64;
                rows.push((start, start + chunk_size as u64, bucket));
            }
        }
        let rows = global_tail(rows, TAIL_SCAN_ROWS);
        let end = rows
            .iter()
            .map(|&(_, end, _)| end)
            .max()
            .map_or(commit_lsn, Lsn);
        Ok(WalTail { end, rows })
    }

    /// [`Self::load_tail`]'s end alone.
    #[cfg(test)]
    async fn load_max_offset(
        client: &YsqlClient,
        table: &str,
        branch_id: BranchId,
        wal_seg_size: usize,
        commit_lsn: Lsn,
    ) -> Result<Lsn> {
        Ok(
            Self::load_tail(client, table, branch_id, wal_seg_size, commit_lsn, Lsn(0))
                .await?
                .end,
        )
    }

    /// Highest LSN reachable from below `raw_max` without crossing a hole,
    /// walking `spans` ([`Self::load_tail`]'s rows) in LSN order.
    ///
    /// **Not every gap is a hole.** `pg_switch_wal()` can leave a permanent
    /// legitimate gap: the switch record's end LSN rounds up to the next
    /// segment boundary, and the compute need not stream the trailing zero
    /// padding, leaving `wals` with no rows for `[switch_end, segment_end)`
    /// (the read path synthesizes those zeros — see [`YbSegmentReader`]).
    /// Reading that as a hole would rehydrate the end of WAL *below* acked WAL
    /// and hand `truncate_wals_above` everything above the switch to delete. So
    /// a gap shaped like padding is confirmed by decoding: the record ending
    /// there has to be an `XLOG_SWITCH`, which `find_end_of_wal` reports by
    /// returning the rounded-up boundary. A batch that crossed a segment and
    /// lost its lower run leaves the same shape, and the decode is what tells
    /// it apart.
    ///
    /// Ambiguity resolves toward *continuing* the walk, because the two
    /// misreadings are not symmetric. Continuing past a real hole leaves
    /// unreplayable WAL in place, which fails loudly on the next read; stopping
    /// at padding deletes WAL that was already acked to a committed
    /// transaction. So a decode that can't reach an answer — no anchor within
    /// `MAX_ANCHOR_SCAN_BYTES`, or the walk running into another gap — or one past
    /// [`MAX_PADDING_PROBES`], is treated as padding.
    ///
    /// A failed *read* is not that ambiguity. It says nothing about where the WAL
    /// ends (#756), and walking through on one would report flush across a real
    /// hole, so it fails the load and the walproposer handshake retries it, as the
    /// end-of-WAL scan does.
    async fn contiguous_wal_end(
        src: &YbWalSource<'_>,
        spans: &[(u64, u64)],
        raw_max: Lsn,
        pg_version: PgMajorVersion,
        anchor_floor: Lsn,
    ) -> Result<Lsn> {
        let (branch_id, seg) = (src.branch_id, src.seg);
        let mut padding: Vec<(u64, u64)> = Vec::new();
        let mut probes = 0usize;
        for (gap_start, gap_end) in tail_gaps(spans) {
            if !gap_is_padding_shaped(gap_start, gap_end, seg) {
                continue;
            }
            if probes >= MAX_PADDING_PROBES {
                tracing::warn!(
                    %branch_id,
                    gap_start = %Lsn(gap_start),
                    "YbWalStorage: past the {MAX_PADDING_PROBES}-probe budget; \
                     treating this padding-shaped gap as XLOG_SWITCH padding unverified",
                );
                padding.push((gap_start, gap_end));
                continue;
            }
            // `find_anchor` hands its floor straight back once the walk reaches
            // it, so a floor past the gap's lower edge anchors inside or above the
            // gap: the replay can't see the switch and returns something other
            // than `gap_end`, which would read as "not a switch" and cut at real
            // padding. At or past `gap_end` the pageserver has read through the
            // gap, which only padding allows; strictly inside, the floor is no
            // record boundary at all. A floor exactly at the lower edge is
            // answerable: the replay returns it, and real padding can't put a
            // boundary there (a switch's is the rounded-up end), so it is a hole.
            if anchor_floor > Lsn(gap_start) {
                tracing::warn!(
                    %branch_id,
                    gap_start = %Lsn(gap_start),
                    %anchor_floor,
                    "YbWalStorage: padding-shaped gap starts below the anchor floor, \
                     so it cannot be confirmed; treating it as padding",
                );
                padding.push((gap_start, gap_end));
                continue;
            }
            probes += 1;
            let failures_before = src.read_failures.load(Ordering::Relaxed);
            match switch_ends_at(src, pg_version, Lsn(gap_start), anchor_floor).await {
                Ok(end) if end.0 == gap_end => {
                    tracing::info!(
                        %branch_id,
                        switch_end = %Lsn(gap_start),
                        segment_end = %Lsn(gap_end),
                        "YbWalStorage: gap in the WAL tail is XLOG_SWITCH padding, not a hole",
                    );
                    padding.push((gap_start, gap_end));
                }
                Ok(end) => {
                    tracing::warn!(
                        %branch_id,
                        gap_start = %Lsn(gap_start),
                        gap_end = %Lsn(gap_end),
                        decoded_end = %end,
                        "YbWalStorage: padding-shaped gap is NOT an XLOG_SWITCH — treating as a hole",
                    );
                }
                Err(e) if src.read_failures.load(Ordering::Relaxed) > failures_before => {
                    return Err(e.context(format!(
                        "hole scan: a read failed while decoding across the padding-shaped gap \
                         at {}",
                        Lsn(gap_start)
                    )));
                }
                Err(e) => {
                    tracing::warn!(
                        %branch_id,
                        gap_start = %Lsn(gap_start),
                        "YbWalStorage: could not decode across a padding-shaped gap, \
                         treating it as padding (never delete acked WAL on a guess): {e:#}",
                    );
                    padding.push((gap_start, gap_end));
                }
            }
        }
        Ok(Lsn(contiguous_end(spans, raw_max.0, &padding)))
    }

    /// The lowest `segno` the branch still has a row for, or 0 when it has
    /// none — the floor [`Self::delete_lower_bound`] wants.
    ///
    /// **Slow by nature, and deliberately not on any hot path.** Naming every
    /// bucket completes the key prefix for each, so the aggregate is pushed
    /// down per bucket rather than scanning the branch through YSQL — but the
    /// seek still starts at the bottom of the range, which is the span GC has
    /// emptied, and DocDB walks every deleted key on the way to the first live
    /// one. YB's own counters do not show it: on a branch that had churned
    /// ~260 GB of WAL this took 51 s and reported `Storage Rows Scanned: 63`.
    /// An all-deleted branch is the worst case, not the cheapest: the answer is
    /// 0 and nothing terminated the seek early.
    ///
    /// Called once per load, from the spawned WAL-removal task; see
    /// [`Self::delete_lower_bound`] for what happened when it ran at startup.
    async fn load_min_segno(client: &YsqlClient, table: &str, branch_id: BranchId) -> Result<u64> {
        let sql = bake_branch_id(
            &format!(
                "SELECT min(segno) AS m FROM {} \
                 WHERE branch_id = $1 AND version = {DEFAULT_VERSION} AND {}",
                quote_relation(table),
                bucket_in_list()
            ),
            branch_id,
        );
        // Aggregate over an empty set still returns one row (NULL); a
        // missing row would mean the query itself didn't run.
        let row = client
            .query_opt(&sql, &[])
            .await
            .context("YbWalStorage: resolve min(segno) from YB")?;
        let m: Option<i64> = row.and_then(|r| r.get("m"));
        Ok(m.map(|v| v as u64).unwrap_or(0))
    }

    /// Producer side: split the incoming WAL range at segment boundaries,
    /// stamp each piece with `end_lsn`, and push onto the
    /// `mpsc` channel. The consumer task INSERTs them in batches; the
    /// caller does **not** wait for durability (use `flush_wal`).
    ///
    /// Each chunk is also inserted into [`WalCache`] *immediately* (before
    /// it lands in YB) so a read-after-write from the WAL sender hits
    /// memory, not YB.
    async fn upsert_writes(&mut self, mut startpos: Lsn, buf: &[u8]) -> Result<()> {
        if buf.is_empty() {
            return Ok(());
        }
        let mut remaining = buf;
        while !remaining.is_empty() {
            let segno = startpos.segment_number(self.wal_seg_size);
            let offset = startpos.segment_offset(self.wal_seg_size);
            let room_in_segment = self.wal_seg_size - offset;
            let take = remaining.len().min(room_in_segment);
            let end_lsn = Lsn(startpos.0 + take as u64);
            let data: Arc<Vec<u8>> = Arc::new(remaining[..take].to_vec());

            // NB: do NOT populate the read-side cache here. The cache
            // is durability-aware now: it's only populated by
            // `consumer_main` *after* the chunk has been successfully
            // INSERT'd into `amp_storage.wals`. Populating it here
            // pre-INSERT (the previous behaviour) let the WAL sender
            // stream chunks to the pageserver that the safekeeper had
            // not yet flushed to YB — and on safekeeper crash before
            // the consumer drained, the pageserver was left holding a
            // persisted consistent_lsn for WAL the restarted SK
            // could no longer serve.

            let pc = PendingChunk {
                segno,
                bucket_id: 0, // placeholder; consumer stamps the batch bucket
                // PostgreSQL caps a WAL segment at 1 GiB, below 2^31.
                chunk_offset: i32::try_from(offset)
                    .with_context(|| format!("WAL segment offset {offset} exceeds INTEGER"))?,
                chunk_size: take as i32,
                data,
                end_lsn,
            };

            // send() blocks on backpressure (bounded channel) — intended
            // while YB is merely slow, but NOT open-ended: this runs inside
            // `process_msg` under the timeline shared_state write lock, so an
            // unbounded block here starves the manager exactly like an
            // unbounded `wait_for_durable` (#765) — and, unlike that path,
            // no published `DurableState::Failed` can unblock a send. The
            // timeout converts a consumer stuck retrying a failing batch
            // (#770) into a dropped connection: lock released, walproposer
            // retries, and ingest resumes when the consumer drains.
            match self.tx.send_timeout(pc, YB_SEND_BACKPRESSURE_MAX).await {
                Ok(()) => {}
                Err(mpsc::error::SendTimeoutError::Timeout(_)) => {
                    let state = self.watermark_rx.borrow().clone();
                    let detail = match state {
                        DurableState::Failed(msg) => format!("consumer failing: {msg}"),
                        DurableState::Healthy(_) => format!(
                            "durable {}, channel full",
                            Lsn(self.durable_lsn.load(Ordering::Acquire))
                        ),
                    };
                    anyhow::bail!(
                        "YB WAL chunk queue made no room in {YB_SEND_BACKPRESSURE_MAX:?} \
                         ({detail}); dropping the connection so the timeline lock is released"
                    );
                }
                Err(mpsc::error::SendTimeoutError::Closed(_)) => {
                    // Consumer task exited. The watermark should carry the
                    // captured error string.
                    let state = self.watermark_rx.borrow().clone();
                    match state {
                        DurableState::Failed(msg) => {
                            anyhow::bail!("YB WAL consumer task failed: {msg}");
                        }
                        DurableState::Healthy(_) => {
                            anyhow::bail!("YB WAL consumer task gone (channel closed)");
                        }
                    }
                }
            }
            // Advance the submitted watermark only after the chunk is
            // actually queued — otherwise a `flush_wal()` racing with a
            // failed `tx.send()` could wait on an LSN that no longer
            // corresponds to anything in flight.
            self.submitted_max_lsn = end_lsn;

            remaining = &remaining[take..];
            startpos = end_lsn;
        }
        Ok(())
    }

    /// Wait for the consumer to publish durability up to `target` LSN,
    /// or propagate a captured error if the consumer has failed.
    ///
    /// Bounded and chatty: every caller sits inside `process_msg`, i.e.
    /// holds the timeline shared_state WRITE lock. An unbounded wait
    /// there starves the manager (fair RwLock ⇒ its read queues behind
    /// any writer), which then can't serve WalResidentTimeline guard
    /// requests — every receiver/sender times out and the walproposer
    /// churns until the wait resolves. Under a YB tablet-split storm the
    /// consumer's batch can take minutes, and that presented as a full
    /// safekeeper wedge. Timing out
    /// converts the wedge into a dropped connection: the guard releases,
    /// the manager breathes, and the walproposer retries against a
    /// recovering consumer.
    async fn wait_for_durable(&mut self, target: Lsn) -> Result<()> {
        let started = std::time::Instant::now();
        // Cloned receiver: the timeout arm needs to inspect the current
        // value while the wait future's borrow is still in scope.
        let mut rx = self.watermark_rx.clone();
        // The atomic, not the published value: `truncate_wal` lowers the atomic
        // but cannot rewrite the watch, which stays above a truncation until the
        // next batch lands. The watch is only the wakeup.
        let durable_lsn = self.durable_lsn.clone();
        let durable = || Lsn(durable_lsn.load(Ordering::Acquire));
        loop {
            let res = tokio::time::timeout(
                self.durable_wait_log_every,
                rx.wait_for(|state| match state {
                    DurableState::Healthy(_) => durable() >= target,
                    DurableState::Failed(_) => true,
                }),
            )
            .await;
            let state = match res {
                Ok(Ok(guard)) => guard.clone(),
                Ok(Err(_)) => {
                    anyhow::bail!("YB WAL consumer task gone (watermark channel closed)");
                }
                Err(_elapsed) => {
                    if let DurableState::Failed(msg) = self.watermark_rx.borrow().clone() {
                        anyhow::bail!(msg);
                    }
                    let durable = durable();
                    if started.elapsed() >= self.durable_wait_max {
                        anyhow::bail!(
                            "YB WAL durability wait timed out after {:?}: target {target}, \
                             durable {durable} (YB slow or wals tablet splitting?); \
                             dropping the connection so the timeline lock is released",
                            started.elapsed(),
                        );
                    }
                    tracing::warn!(
                        "still waiting for YB WAL durability: target {target}, \
                         durable {durable}, elapsed {:?}",
                        started.elapsed(),
                    );
                    continue;
                }
            };
            return match state {
                DurableState::Healthy(_) => Ok(()),
                DurableState::Failed(msg) => Err(anyhow::anyhow!(msg)),
            };
        }
    }
}

impl Storage for YbWalStorage {
    fn accepted_byte_end(&self) -> Lsn {
        self.accepted_stream.byte_end()
    }

    fn flush_lsn(&self) -> Lsn {
        // The LSN this safekeeper answers for. It goes out in the greeting and in
        // every AppendResponse, the walproposer turns it into commit_lsn, and the
        // compute releases synchronous commits on it — so an LSN reported here has
        // been promised to a client.
        //
        // Both bounds are needed, for different reasons:
        //
        // Past `durable_lsn`, the bytes may not be in `amp_storage.wals` yet. A crash
        // after the ack but before the INSERT loses a commit the client was told was
        // durable. That is the 2026-05-29 gap forensic.
        //
        // Past `record_end`, the LSN can fall inside a record. A reader that stops
        // there has half a record and fails to decode; and this safekeeper discards
        // that partial tail on its next restart, so it would be advertising a
        // position it will not have after a crash.
        //
        // `record_end` advances as soon as bytes are decoded, well before they are
        // INSERTed, so while a batch is in flight `durable_lsn` is the only honest
        // statement about what YB can serve. That holds only because `truncate_wal`
        // lowers `durable_lsn` with `record_end`: left at the old end, it would let
        // re-streamed bytes below it be acked from memory (#1626).
        let durable = Lsn(self.durable_lsn.load(Ordering::Acquire));
        std::cmp::min(self.accepted_stream.record_end(), durable)
    }

    async fn initialize_first_segment(&mut self, init_lsn: Lsn) -> Result<()> {
        let _timer = WAL_STORAGE_OPERATION_SECONDS
            .with_label_values(&["initialize_first_segment"])
            .start_timer();
        // Generate the standard PG segment header + zero padding, then push
        // it through the consumer just like a regular write. Wait for the
        // consumer to flush before returning so the 16 MiB header is
        // durable by the time the caller sees success. One-shot
        // (per-timeline init), not on the per-commit hot path.
        let segno = init_lsn.segment_number(self.wal_seg_size);
        let wal_seg =
            postgres_ffi::generate_wal_segment(segno, self.system_id, self.pg_version, init_lsn)?;
        // The blob is the whole segment from offset 0, so it ends at the segment's
        // end, not `init_lsn + len`.
        let end_lsn = Lsn(segno * self.wal_seg_size as u64 + wal_seg.len() as u64);
        // Init blob is a 16 MiB zero-padded segment. Intentionally not
        // cached — see WalCache module docs. It still goes through the
        // consumer (so YB has a row), but the read path will fetch it
        // lazily from YB if needed.
        let pc = PendingChunk {
            segno,
            bucket_id: 0, // placeholder; consumer stamps the batch bucket
            chunk_offset: 0,
            chunk_size: wal_seg.len() as i32,
            data: Arc::new(wal_seg.to_vec()),
            end_lsn,
        };
        self.tx
            .send(pc)
            .await
            .map_err(|_| anyhow::anyhow!("YB WAL consumer task gone"))?;
        self.submitted_max_lsn = end_lsn;
        self.flush_notify.notify_one();
        self.wait_for_durable(end_lsn).await?;
        tracing::info!("initialize_first_segment: segno {segno} at lsn {init_lsn}");
        Ok(())
    }

    /// Sequential append only: moving the write pointer takes a `ProposerElected`
    /// truncation first. The rewrite case is expected in normal operation — a
    /// reconnecting walproposer whose first connection is still delivering packets.
    async fn write_wal(&mut self, startpos: Lsn, buf: &[u8]) -> Result<()> {
        let accepted_byte_end = self.accepted_stream.byte_end();
        if accepted_byte_end > startpos {
            anyhow::bail!(
                "write_wal rewrites WAL already accepted, accepted_byte_end={accepted_byte_end}, \
                 startpos={startpos}"
            );
        }
        if accepted_byte_end < startpos && accepted_byte_end != Lsn(0) {
            anyhow::bail!(
                "write_wal creates gap in accepted WAL, accepted_byte_end={accepted_byte_end}, \
                 startpos={startpos}"
            );
        }
        // Validate and decode before queueing, so a rejection leaves nothing on the
        // channel. A failed `upsert_writes` recovers only via the dropped connection;
        // the stream end is already advanced, so the next append would pass.
        self.accepted_stream.feed(startpos, buf)?;
        self.metrics.observe_write_bytes(buf.len());
        // Durability is not observed yet — the caller must `flush_wal` to wait.
        self.upsert_writes(startpos, buf).await
    }

    async fn truncate_wal(&mut self, end_pos: Lsn) -> Result<()> {
        let _timer = WAL_STORAGE_OPERATION_SECONDS
            .with_label_values(&["truncate_wal"])
            .start_timer();
        // Drain everything the consumer has in flight first. Otherwise a
        // buffered chunk for an LSN past `end_pos` could be flushed after
        // the DELETE and silently resurrect data the walproposer intended
        // to discard. Wake the consumer in case it's accumulating.
        self.flush_notify.notify_one();
        self.wait_for_durable(self.submitted_max_lsn).await?;

        // Refuse to advance the truncation point past confirmed YB
        // durability. The reset below moves `record_end` to `end_pos`
        // unconditionally, so accepting an `end_pos` past
        // `durable_lsn` would advertise WAL the row store can't return —
        // exactly the forensic gap (2026-05-29). The walproposer should
        // reissue ProposerElected with a corrected `start_streaming_at`
        // after seeing our capped `flush_lsn()` in the greeting on the
        // next reconnect.
        let durable = Lsn(self.durable_lsn.load(Ordering::Acquire));
        if end_pos > durable {
            anyhow::bail!(
                "YbWalStorage::truncate_wal: refusing end_pos {end_pos} \
                 past confirmed durable LSN {durable} \
                 (submitted_max_lsn={})",
                self.submitted_max_lsn,
            );
        }
        // Also before the DELETE: bailing after it leaves the rows gone with the
        // watermarks still above `end_pos`, advertising a range the table lost.
        if self.accepted_stream.record_end() != Lsn(0)
            && end_pos > self.accepted_stream.record_end()
        {
            anyhow::bail!(
                "truncate_wal called on non-written WAL, record_end={}, end_pos={end_pos}",
                self.accepted_stream.record_end(),
            );
        }

        // Nothing else lowers `durable_lsn`: left at the old end, WAL re-streamed
        // below it is acked as soon as it is decoded. Done before the DELETE, so
        // one that fails part way advertises nothing it may have removed. The
        // consumer is drained and only `&mut self` feeds it, so nothing races
        // this store.
        self.durable_lsn.store(end_pos.0, Ordering::Release);
        self.submitted_max_lsn = end_pos;

        // Physically drop any WAL past `end_pos` before resetting the
        // stream ends. In the steady single-SK case `end_pos ==
        // durable` and there's nothing past it (no-op), but after a torn
        // tail (find_end_of_wal capped flush_lsn below the raw max on
        // restart) or a multi-SK divergence, rows can sit past `end_pos`;
        // leaving them would let `load_tail` re-count them on the
        // next restart and could resurface a partial chunk to a reader.
        truncate_wals_above(
            &self.ysql_system_client,
            &self.table_name,
            self.branch_id,
            self.wal_seg_size,
            end_pos,
        )
        .await?;
        // Drop any cached chunks past `end_pos`. Without this the in-memory
        // `WalCache` keeps the pre-truncation (now-torn) bytes for that
        // region, and `YbSegmentReader` would serve them to a reader even
        // after the YB rows were deleted above — so when the walproposer
        // next rewrites WAL forward from `end_pos`, the interpreted-WAL reader
        // decodes a stale page and trips `critical_timeline!` (the
        // `invalid xlog page header` going_away flake). Done after the YB
        // delete so a concurrent reader re-populating on a cache miss reads
        // the already-trimmed table, not the stale rows. Symmetric to the
        // cache drop `remove_up_to` does for its DELETE.
        if let Some(cache) = &self.cache {
            let segno_end = end_pos.segment_number(self.wal_seg_size);
            let end_offset = end_pos.segment_offset(self.wal_seg_size) as u64;
            cache.lock().unwrap().truncate_after(segno_end, end_offset);
        }

        if end_pos == self.accepted_stream.byte_end()
            && end_pos == self.accepted_stream.record_end()
        {
            return Ok(());
        }
        self.accepted_stream.reset(end_pos);
        tracing::info!("truncated WAL to {end_pos}");
        Ok(())
    }

    /// Push the consumer's queue toward durability; the mode selects how far to
    /// wait (see [`YbFlush`]). `flush_lsn()` reads `durable_lsn` directly, so it
    /// needs no local advance here and `append_response` only ever advertises
    /// durable WAL.
    async fn yb_flush(&mut self, mode: YbFlush) -> Result<()> {
        match mode {
            YbFlush::NotifyAndWait => {
                self.flush_notify.notify_one();
                self.wait_for_durable(self.submitted_max_lsn).await?;
            }
            YbFlush::Notify => self.flush_notify.notify_one(),
        }
        Ok(())
    }

    /// Durability sync point (`handle_elected` / sync-safekeepers): notify, wait, advance.
    /// The walproposer treats this as the durable-commit boundary, so it
    /// completes or errors before returning.
    async fn flush_wal(&mut self) -> Result<()> {
        self.yb_flush(YbFlush::NotifyAndWait).await
    }

    fn yb_durable_watch(&self) -> Option<watch::Receiver<DurableState>> {
        Some(self.watermark_rx.clone())
    }

    fn remove_up_to(&self, segno_up_to: XLogSegNo) -> BoxFuture<'static, anyhow::Result<()>> {
        let system_client = self.ysql_system_client.clone();
        let table = self.table_name.clone();
        let branch_id = self.branch_id;
        let cache = self.cache.clone();
        let delete_lower_bound = self.delete_lower_bound.clone();
        async move {
            // Drop cached chunks for segnos that are about to be DELETE'd
            // out of `amp_storage.wals`. Done before the DB delete so a
            // concurrent reader can't pick up a cached chunk that is no
            // longer durable. Reads that beat us here race against the
            // existing chunks just as they always would.
            if let Some(c) = &cache {
                c.lock().unwrap().remove_segnos_below(segno_up_to);
            }
            // Bound the DELETE on both sides. The lower bound is the segno
            // boundary advanced by the previous successful call — rows below it
            // are known to be gone, so naming them in the WHERE clause turns
            // what was an unbounded-low range scan into a tight one. Idempotent
            // if `segno_up_to <= lower`: the predicate is unsatisfiable and the
            // DELETE is a no-op.
            // Naming every bucket completes the key prefix for each, so the
            // DELETE resolves to a per-bucket index range on `segno`
            // instead of scanning the branch across all tablets.
            //
            // Nothing has established the bound yet on the first removal after a
            // load, so ask the table. That query crosses the deleted span and can
            // take tens of seconds (see `load_min_segno`) — affordable here and
            // nowhere else: this runs in the manager's spawned removal task,
            // holding no timeline lock, while the compute is already serving. A
            // failure is not fatal; 0 is a valid floor, so the DELETE just starts
            // wider and the bound is fixed by the `fetch_max` below.
            let mut lower = delete_lower_bound.load(Ordering::Relaxed);
            if lower == DELETE_BOUND_UNRESOLVED {
                lower = match Self::load_min_segno(&system_client, &table, branch_id).await {
                    Ok(min_segno) => min_segno,
                    Err(e) => {
                        tracing::warn!(
                            %branch_id,
                            "could not resolve the WAL DELETE lower bound; deleting from 0: {e:#}"
                        );
                        0
                    }
                };
                // Resolved either way, including the failure: 0 is a valid floor,
                // and re-running a seek that just cost tens of seconds on every
                // removal would be worse than one wide DELETE. The advance below
                // tightens it from here on.
                delete_lower_bound.store(lower, Ordering::Relaxed);
            }
            let sql = bake_branch_id(
                &format!(
                    "DELETE FROM {} WHERE branch_id = $1 AND version = {DEFAULT_VERSION} AND {} \
                     AND segno >= $2 AND segno < $3",
                    quote_relation(&table),
                    bucket_in_list()
                ),
                branch_id,
            );
            let lower_i: i64 = lower as i64;
            let upper_i: i64 = segno_up_to as i64;
            let params: [&(dyn ToSql + Sync); 2] = [&lower_i, &upper_i];
            system_client
                .execute(&sql, &params)
                .await
                .context("YbWalStorage: remove_up_to delete")?;
            delete_lower_bound.fetch_max(segno_up_to, Ordering::Relaxed);
            Ok(())
        }
        .boxed()
    }

    fn get_metrics(&self) -> WalStorageMetrics {
        self.metrics.clone()
    }
}

/// [`YbWalStorage::delete_lower_bound`] before the first removal has resolved it.
/// `u64::MAX` rather than 0 because 0 is a legitimate floor, and a `min(segno)`
/// answering 0 — which it also does for an all-deleted branch, after crossing the
/// whole span — must not leave the bound looking unresolved and re-run that seek on
/// every removal.
const DELETE_BOUND_UNRESOLVED: u64 = u64::MAX;

/// Quote a (possibly schema-qualified) relation name for safe SQL
/// interpolation. A `schema.table` input is split on the first `.` and
/// each part quoted independently (`"schema"."table"`); an unqualified
/// name (the per-test scratch tables) is quoted as a single identifier.
fn quote_relation(relation: &str) -> String {
    match relation.split_once('.') {
        Some((schema, table)) => format!("{}.{}", quote_one(schema), quote_one(table)),
        None => quote_one(relation),
    }
}

fn quote_one(ident: &str) -> String {
    let escaped = ident.replace('"', "\"\"");
    format!("\"{escaped}\"")
}

/// What [`YbWalStorage::load_tail`] read: the highest stored chunk end, and the
/// rows just below it as `(start, end, bucket_id)`, ascending.
struct WalTail {
    end: Lsn,
    rows: Vec<(u64, u64, i16)>,
}

impl WalTail {
    fn spans(&self) -> Vec<(u64, u64)> {
        self.rows
            .iter()
            .map(|&(start, end, _)| (start, end))
            .collect()
    }
}

/// `WalSource` over the `wals` table, for the end-of-WAL record-boundary
/// scan run once at [`YbWalStorage::new`]. Plain binds on the system lane
/// — not a hot path.
struct YbWalSource<'a> {
    client: &'a YsqlClient,
    table: &'a str,
    branch_id: BranchId,
    seg: u64,
    /// YB round trips, one per statement, named for the phase that spends them.
    /// `find_anchor` reaches `chunk_at` through `read_exact`, one page header per
    /// call, and is `chunk_at`'s only caller here — `contiguous_chunks_from`'s default is
    /// overridden below and `decode_forward` has no caller on this path. Logged by
    /// `YbWalStorage::new` beside each phase's elapsed time; the quotient is what
    /// separates a long scan from a slow cluster.
    find_anchor_queries: AtomicU64,
    find_end_of_wal_queries: AtomicU64,
    /// Reads that failed (a query error, or a row that would not decode), as
    /// opposed to finding nothing. Lets [`YbWalStorage::contiguous_wal_end`] tell
    /// a failed read from a decode that genuinely couldn't answer.
    read_failures: AtomicU64,
}

impl YbWalSource<'_> {
    fn note_failure<T>(&self, res: Result<T>) -> Result<T> {
        if res.is_err() {
            self.read_failures.fetch_add(1, Ordering::Relaxed);
        }
        res
    }

    /// One scan row, decoded, its offset resolved against `segno`. Borrows the
    /// row buffer: a batch can be thousands of rows.
    fn decode_row(&self, row: &yb_tokio_postgres::Row, segno: i64) -> Result<(Lsn, Vec<u8>)> {
        // `chunk_offset` is INTEGER: tokio-postgres reads it only as `i32`.
        let chunk_offset = row.get::<_, i32>(0) as u64;
        let stored: &[u8] = row.get(1);
        let data = decode_chunk_data(stored, "end-of-WAL scan")
            .with_context(|| format!("chunk segno={segno} offset={chunk_offset}"))?;
        Ok((Lsn(segno as u64 * self.seg + chunk_offset), data))
    }
}

/// The inclusive `find_anchor` target for WAL that ends at `end` (exclusive).
///
/// Normally `end - 1`: passing `end` itself probes a page that holds no bytes
/// (#678). But `find_anchor` reads the header of the target's page first, and
/// an `end` inside a page header leaves that header incomplete, so the walk has
/// to start on the page below. Stored WAL can end there: a row ends wherever
/// the walproposer cut its send, not on a record boundary, so a torn tail or the
/// lower edge of a hole can stop a few bytes into a page.
fn anchor_target(end: Lsn, wal_seg_size: u64) -> Lsn {
    use postgres_ffi::XLOG_BLCKSZ;
    use postgres_ffi::wal_record_boundary::page_header_size;
    let page = end.0 - end.0 % XLOG_BLCKSZ as u64;
    if page > 0 && end.0 - page < page_header_size(Lsn(page), wal_seg_size) as u64 {
        Lsn(page - 1)
    } else {
        Lsn(end.0 - 1)
    }
}

/// Where the WAL ends according to a decoder replay that stops at `at`, used by
/// [`YbWalStorage::contiguous_wal_end`] to tell `XLOG_SWITCH` padding from a
/// hole.
///
/// The two are indistinguishable from the row extents alone — both are a stretch
/// of `wals` with no rows — so this asks the bytes. Anchor on a fresh-record page
/// at or below `at` and replay forward: an `XLOG_SWITCH` ending at `at` makes
/// `find_end_of_wal` return the *rounded-up segment boundary*, because that is
/// where the switch record's end LSN sits. Any other record ends at or below
/// `at`.
///
/// `at` is exclusive (one past the run's last byte); see [`anchor_target`].
async fn switch_ends_at(
    src: &YbWalSource<'_>,
    pg_version: PgMajorVersion,
    at: Lsn,
    anchor_floor: Lsn,
) -> Result<Lsn> {
    use postgres_ffi::wal_record_boundary::{find_anchor, find_end_of_wal};
    let anchor = find_anchor(src, anchor_target(at, src.seg), anchor_floor)
        .await
        .context("anchor scan below a padding-shaped gap")?;
    find_end_of_wal(src, anchor, pg_version)
        .await
        .context("record replay across a padding-shaped gap")
}

impl postgres_ffi::wal_record_boundary::WalSource for YbWalSource<'_> {
    fn seg(&self) -> u64 {
        self.seg
    }

    async fn chunk_at(&self, lsn: Lsn) -> anyhow::Result<Option<(Lsn, Vec<u8>)>> {
        self.find_anchor_queries.fetch_add(1, Ordering::Relaxed);
        let segno = (lsn.0 / self.seg) as i64;
        let off = (lsn.0 % self.seg) as i32;
        // The bucket of a chunk can't be derived, so probe all buckets (an
        // IN-list on the leading key column = batched index probes) and take
        // the highest covering offset across them.
        let sql = format!(
            "SELECT chunk_offset, data FROM {} \
             WHERE branch_id = $1 AND version = {DEFAULT_VERSION} AND {} AND segno = $2 AND chunk_offset <= $3 \
             ORDER BY chunk_offset DESC LIMIT 1",
            quote_relation(self.table),
            bucket_in_list()
        );
        let params: [&(dyn ToSql + Sync); 3] = [&self.branch_id, &segno, &off];
        // Only a missing row ends the WAL. A failed query is a failed read and
        // must stay an `Err` — see `WalSource::chunk_at` (#756).
        let Some(row) = self.note_failure(
            self.client
                .query_opt(&sql, &params)
                .await
                .context("end-of-WAL scan: reading wals"),
        )?
        else {
            return Ok(None);
        };
        // Decode before any length arithmetic — the coverage check and the
        // returned buffer are both in logical WAL bytes.
        let (chunk_start, data) = self.note_failure(self.decode_row(&row, segno))?;
        if lsn.0 >= chunk_start.0 + data.len() as u64 {
            // The nearest chunk stops short of `lsn`: the WAL ends here.
            return Ok(None);
        }
        Ok(Some((chunk_start, data)))
    }

    /// One range query per byte window instead of one point query per chunk.
    ///
    /// Scoped to `lsn`'s segno, which bounds the bytes: chunks never overlap, so a
    /// segment's rows carry at most `wal_seg_size` however many rows that is.
    ///
    /// The subquery finds the chunk covering `lsn` — a record boundary is not a chunk
    /// boundary, so `lsn` lands mid-chunk — and the batch starts from that chunk. Its
    /// plan:
    ///
    /// ```text
    /// Limit
    ///   -> Sort                    <- carries `data`, i.e. the WAL bytes
    ///        Sort Key: chunk_offset DESC
    ///        -> Index Scan using wals_pkey
    ///             Index Cond: bucket_id = ANY ('{0,1,2,3,4,5}') AND ... AND chunk_offset <= $3
    /// ```
    ///
    /// The Sort is there because `bucket_id` leads the key and a chunk's bucket cannot be
    /// derived from its offset, so all six are named and the scan returns rows grouped by
    /// bucket. One segment's rows, once per timeline load.
    ///
    /// `Set(yb_enable_cbo on) Set(yb_max_merge_scan_streams 6) Set(enable_sort off)` gets
    /// YB to merge the six ranges inside the scan and drop the Sort instead:
    ///
    /// ```text
    /// Limit
    ///   -> Index Scan Backward using wals_pkey
    ///        Index Cond: ...
    ///        Merge Sort Key: chunk_offset DESC
    ///        Merge Stream Key: bucket_id
    ///        Merge Streams: 6
    /// ```
    ///
    /// Don't. Hints are statement-wide, so they re-plan the subquery too, and there it
    /// runs into yugabyte-db#32733: `DESC LIMIT 1` comes back with one bucket's max
    /// instead of the global max. The chunk it names then stops short of `lsn`, this
    /// returns empty, and an empty batch reads as an end of WAL — so the safekeeper
    /// deletes the durable rows above it (#938).
    async fn contiguous_chunks_from(&self, lsn: Lsn) -> anyhow::Result<Vec<(Lsn, Vec<u8>)>> {
        self.find_end_of_wal_queries.fetch_add(1, Ordering::Relaxed);
        let segno = (lsn.0 / self.seg) as i64;
        let off = (lsn.0 % self.seg) as i32;
        let table = quote_relation(self.table);
        let buckets = bucket_in_list();
        let sql = format!(
            "SELECT chunk_offset, data FROM {table} \
             WHERE branch_id = $1 AND version = {DEFAULT_VERSION} AND {buckets} AND segno = $2 \
               AND chunk_offset >= (SELECT chunk_offset FROM {table} \
                                    WHERE branch_id = $1 AND version = {DEFAULT_VERSION} AND {buckets} AND segno = $2 \
                                      AND chunk_offset <= $3 \
                                    ORDER BY chunk_offset DESC LIMIT 1) \
             ORDER BY chunk_offset ASC LIMIT $4"
        );
        let params: [&(dyn ToSql + Sync); 4] =
            [&self.branch_id, &segno, &off, &(CHUNK_FETCH_ROW_CAP as i64)];
        // A failed query stays an `Err`; only an empty result ends the WAL (#756).
        let rows = self.note_failure(
            self.client
                .query(&sql, &params)
                .await
                .context("end-of-WAL scan: reading wals"),
        )?;
        // Cut the run at the first byte it does not hold, so the batch is
        // contiguous in fact and not just in the contract. `lsn` seeds it because
        // the leading chunk starts at or below `lsn` and may stop short of it.
        let mut out: Vec<(Lsn, Vec<u8>)> = Vec::with_capacity(rows.len());
        let mut want = lsn;
        for (i, row) in rows.iter().enumerate() {
            let (start, data) = match self.decode_row(row, segno) {
                Ok(chunk) => chunk,
                // A row the scan may never reach must not fail the load: the
                // one-row-per-query scan stopped at the last complete record and
                // never fetched what lay above it. Ending the run here keeps that
                // — and if the scan does reach this row, the next fetch leads with
                // it and the error surfaces then (#756).
                Err(_) if i > 0 => break,
                Err(e) => return self.note_failure(Err(e)),
            };
            if start > want || want.0 - start.0 >= data.len() as u64 {
                break;
            }
            want = Lsn(start.0 + data.len() as u64);
            out.push((start, data));
        }
        // Empty carries `chunk_at`'s `None`: nothing covers `lsn`. The batch may
        // hold rows above a gap, which `chunk_at` could not see and this does not
        // act on — reporting that as an end of WAL is #799, unchanged here.
        Ok(out)
    }
}

/// Bake the per-instance `branch_id` into a `wals` query whose `$1` is
/// `branch_id` (the convention here): replace that bind with a quoted
/// literal and shift every remaining `$N` (N ≥ 2) down by one. Each
/// `YbWalStorage` is bound to one branch for its lifetime and the
/// safekeeper does no lineage/ancestor reads, so the id is fixed for
/// every query on this instance — baking drops the bind from the wire.
/// `BranchId`'s `Display` emits only hex digits and dashes, so the literal is
/// injection-safe, and Postgres parses it as the `uuid` the column is.
fn bake_branch_id(sql: &str, branch_id: BranchId) -> String {
    let with_id = sql.replace("branch_id = $1", &format!("branch_id = '{branch_id}'"));
    // No bare `$1` may remain — it's branch_id only.
    regex::Regex::new(r"\$(\d+)")
        .expect("static regex")
        .replace_all(&with_id, |c: &regex::Captures<'_>| {
            let n: u32 = c[1].parse().expect("param index");
            let shifted = n
                .checked_sub(1)
                .expect("wals query must use $1=branch_id only");
            format!("${shifted}")
        })
        .into_owned()
}

/// Group a batch's chunks into coalescible runs. Input is the batch in
/// production (= arrival = byte) order; output is index ranges into that
/// slice, where each range is one maximal run of chunks that share a segno
/// AND are byte-contiguous (`prev.chunk_offset + prev.chunk_size ==
/// next.chunk_offset`). Each run becomes ONE `amp_storage.wals` row —
/// `write_wal` splits at segment boundaries and streams sequentially, so
/// the typical batch is a single run. Runs break at segment boundaries
/// (different segno) and at any byte discontinuity (e.g. after a
/// `truncate_wal` re-aligned the stream). Pure helper for
/// [`flush_batch`]; unit-tested below.
fn coalesce_runs(chunks: &[PendingChunk]) -> Vec<std::ops::Range<usize>> {
    let mut runs = Vec::with_capacity(1);
    let mut start = 0;
    for i in 1..chunks.len() {
        let prev = &chunks[i - 1];
        let cur = &chunks[i];
        if cur.segno != prev.segno || prev.chunk_offset + prev.chunk_size != cur.chunk_offset {
            runs.push(start..i);
            start = i;
        }
    }
    if !chunks.is_empty() {
        runs.push(start..chunks.len());
    }
    runs
}

/// Unwrap a stored `wals.data` envelope, naming the chunk in any error.
///
/// Every read path funnels through here, and it is deliberately independent
/// of the writer's configured codec: the envelope's tag says how the row was
/// written, so rows produced under different `yb_wal_compression` settings
/// all decode with one reader. That is what makes changing the codec a
/// restart rather than a migration.
///
/// The decoded bytes are the WAL stream itself — everything above this point
/// (the `WalCache`, the segment reader's byte-range slicing, record
/// decoding) works on plaintext and needed no change.
fn decode_chunk_data(stored: &[u8], what: &str) -> Result<Vec<u8>> {
    let stored = bytes::Bytes::copy_from_slice(stored);
    payload_codec::decode(&stored)
        .map(|b| b.to_vec())
        .with_context(|| {
            format!(
                "{what}: decoding stored WAL chunk ({} bytes, codec {})",
                stored.len(),
                payload_codec::stored_codec_name(&stored),
            )
        })
}

/// The `(delete, select)` pair [`truncate_wals_above`] issues, against an
/// already-quoted `table`. Binds: `$1` branch_id, `$2` the segno holding the
/// cut, `$3` its offset within that segment. Split out so
/// `truncate_stmts_are_index_tight` can EXPLAIN the exact text production runs.
///
/// The cut is addressed as `(segno, offset)` rather than as one LSN because
/// filtering on the computed `segno * seg + chunk_offset` cannot be pushed down
/// as an index bound — it scanned every WAL row of the branch to reach a
/// handful at the tail, which hung `load_timeline` (#812) and, on the
/// `truncate_wal` path, held the timeline lock. `remove_up_to` carries the same
/// rewrite.
///
/// The split is exact because `chunk_offset` is segment-relative and rows never
/// span segments (`upsert_writes` splits at the boundary): the leading
/// `segno >= $2` is the index bound and the disjunction only decides within the
/// cut's own segment, which is also the only segment a straddling chunk can lie
/// in. A segment-aligned cut has no straddler, and `chunk_offset < 0` matches
/// nothing — which is that answer.
fn truncate_stmts(table: &str) -> (String, String) {
    let buckets = bucket_in_list();
    let del = format!(
        "DELETE FROM {table} WHERE branch_id = $1 AND version = {DEFAULT_VERSION} AND {buckets} \
         AND segno >= $2 AND (segno > $2 OR chunk_offset >= $3)"
    );
    // The predicate reads only `chunk_size`, which is LOGICAL length, so it
    // needs no adjustment for compression — only the byte-slicing that
    // follows the read moves to Rust.
    let sel = format!(
        "SELECT bucket_id, segno, chunk_offset, data \
         FROM {table} \
         WHERE branch_id = $1 AND version = {DEFAULT_VERSION} AND {buckets} \
           AND segno = $2 AND chunk_offset < $3 \
           AND chunk_offset + chunk_size > $3"
    );
    (del, sel)
}

/// Discard all WAL strictly beyond `end_pos`: drop chunks that start at
/// or after it, and truncate the chunk straddling it down to `end_pos`.
/// After this, `max(chunk_offset + chunk_size)` for the timeline is
/// exactly `end_pos` — the YB equivalent of truncating a local WAL
/// segment file. Used to discard a torn tail on safekeeper restart and to
/// honor a walproposer truncation on ProposerElected. Both steps are
/// idempotent (keyed DELETE; the trim's predicate stops matching once the
/// chunk has been truncated).
///
/// Naming every bucket completes the key prefix for each, so the statements
/// resolve to the buckets' tablets rather than scatter-scanning the branch;
/// the cut still spans every bucket.
///
/// # Why the trim is read-modify-write
///
/// This used to be one `UPDATE ... SET data = substring(data FROM 1 FOR n)`.
/// With the [`payload_codec`] envelope that is wrong twice over: it would
/// slice a compressed LZ4 stream in half, and even for an uncompressed row
/// it would cut from the tag byte rather than the payload. YSQL cannot
/// decompress — Postgres exposes no SQL-callable LZ4/pglz decompressor and
/// YB ships no extension for it — so the row has to come back to Rust.
///
/// The rewritten chunk is stored **raw**, never re-compressed. Truncation is
/// rare (torn tail on restart, or a walproposer ProposerElected cut), and
/// leaving one chunk uncompressed until GC drops it is not worth a second
/// code path.
///
/// At most one chunk can straddle `end_pos` — chunks tile LSN space without
/// overlap — but the loop below handles whatever the predicate returns
/// rather than assuming exactly one.
async fn truncate_wals_above(
    client: &YsqlClient,
    table: &str,
    branch_id: BranchId,
    wal_seg_size: usize,
    end_pos: Lsn,
) -> Result<()> {
    let table = quote_relation(table);
    let seg = wal_seg_size as i64;
    let end = end_pos.0 as i64;
    // Both statements address `end_pos` as `(end_segno, end_off)` rather than
    // as a single LSN — see `truncate_stmts` for why.
    let end_segno = end / seg;
    let end_off = (end % seg) as i32;
    let params: [&(dyn ToSql + Sync); 3] = [&branch_id, &end_segno, &end_off];
    let (del, sel) = truncate_stmts(&table);

    client
        .execute(&del, &params)
        .await
        .context("truncate_wals_above: delete chunks past end_pos")?;

    let straddling = client
        .query(&sel, &params)
        .await
        .context("truncate_wals_above: select straddling chunk")?;

    for row in straddling {
        let bucket_id: i16 = row.get(0);
        let segno: i64 = row.get(1);
        let chunk_offset: i32 = row.get(2);
        let stored: Vec<u8> = row.get(3);
        let stored = bytes::Bytes::from(stored);

        let keep = end - (segno * seg + i64::from(chunk_offset));
        // The predicate guarantees 0 < keep < chunk_size; a violation means
        // the row disagrees with the arithmetic that selected it.
        anyhow::ensure!(
            keep > 0,
            "truncate_wals_above: straddling chunk segno={segno} \
             offset={chunk_offset} computed non-positive keep={keep}"
        );
        let keep = keep as usize;

        let plain = payload_codec::decode(&stored).with_context(|| {
            format!(
                "truncate_wals_above: decode straddling chunk segno={segno} \
                 offset={chunk_offset} ({} stored bytes, codec {})",
                stored.len(),
                payload_codec::stored_codec_name(&stored),
            )
        })?;
        anyhow::ensure!(
            keep <= plain.len(),
            "truncate_wals_above: straddling chunk segno={segno} \
             offset={chunk_offset} decoded to {} bytes, need {keep}",
            plain.len()
        );

        // Store the trimmed remainder raw — see the fn doc.
        let (trimmed, _) = payload_codec::encode(&plain[..keep], payload_codec::Codec::Raw);

        // Compare-and-swap on `data`: `truncate_wal` drains the consumer and
        // holds `&mut self` before calling us, so nothing should be writing
        // this row — the guard makes that assumption checkable instead of
        // load-bearing, and would catch a future refactor that drops the
        // drain.
        //
        // The predicate matches EITHER the pre-image ($7) or the exact
        // post-image we are about to write ($5/$6). The second arm is what
        // keeps the statement idempotent under `YsqlClient::run_with_retries`,
        // which blindly replays a statement whose ack was lost to a
        // connection drop (see its "Idempotency assumption" doc). On such a
        // replay the row is already trimmed, so the pre-image no longer
        // matches; without the second arm the affected-count check below
        // would turn an already-durable trim into a hard error. A row
        // matching neither arm genuinely changed underneath us — which is
        // the case the guard exists for.
        let upd = format!(
            "UPDATE {table} SET data = $5, chunk_size = $6 \
             WHERE branch_id = $1 AND version = {DEFAULT_VERSION} AND bucket_id = $2 AND segno = $3 \
               AND chunk_offset = $4 \
               AND (data = $7 OR (data = $5 AND chunk_size = $6))"
        );
        let keep_i32 = keep as i32;
        let new_data = trimmed.as_ref();
        let old_data = stored.as_ref();
        let affected = client
            .execute(
                &upd,
                &[
                    &branch_id,
                    &bucket_id,
                    &segno,
                    &chunk_offset,
                    &new_data,
                    &keep_i32,
                    &old_data,
                ],
            )
            .await
            .context("truncate_wals_above: trim straddling chunk")?;
        anyhow::ensure!(
            affected == 1,
            "truncate_wals_above: trim of segno={segno} offset={chunk_offset} \
             affected {affected} rows (expected 1) — the chunk is neither at \
             its pre-trim bytes nor at the trimmed state we computed, so it \
             changed between the read and the write"
        );
    }
    Ok(())
}

/// Consumer task body. Builds batches by:
///
///   1. Blocking on the channel for the first chunk.
///   2. Greedily draining more chunks (non-blocking `try_recv`) until
///      `FLUSH_ROWS_THRESHOLD` / `FLUSH_BYTES_THRESHOLD` is hit, or the
///      channel is momentarily empty.
///   3. If still under threshold, waiting up to `ACCUMULATION_MAX` for
///      additional chunks — or until `flush_notify` is pinged.
///   4. Stamping EVERY chunk of the batch with one placement bucket from
///      a local rotating counter (one bucket per batch → one tablet per
///      batch; see the stamping comment in the body).
///   5. Writing the batch via [`flush_batch`] (contiguous chunks
///      coalesced into single rows).
///   6. Publishing the highest `end_lsn` of the batch onto `watermark_tx`
///      as `DurableState::Healthy`.
///
/// `flush_wal` / `truncate_wal` ping `flush_notify` before they await
/// the watermark; that cuts the step-3 wait short so commit latency
/// doesn't pay for accumulation while still letting throughput-oriented
/// background streaming accumulate naturally.
///
/// On a batch failure, retries with capped backoff until the batch lands,
/// publishing `DurableState::Failed` once the failure has persisted past
/// `CONSUMER_RETRY_SIGNAL_AFTER` so waiters fail fast (#770). Exits on
/// failure only when the producer is already gone and nothing is queued.
///
/// Exits cleanly when `tx` is dropped (channel closes from the
/// producer side, `rx.recv()` returns `None`).
// Wide signature: consumer task needs the full set of YB connection params.
#[allow(clippy::too_many_arguments)]
async fn consumer_main(
    mut rx: mpsc::Receiver<PendingChunk>,
    yb_thinclient: Arc<YbThinPool>,
    branch_id: BranchId,
    watermark_tx: watch::Sender<DurableState>,
    flush_notify: Arc<tokio::sync::Notify>,
    durable_lsn: Arc<AtomicU64>,
    cache: Option<Arc<std::sync::Mutex<crate::wal_cache::WalCache>>>,
    codec: payload_codec::Codec,
) {
    // Placement-bucket rotation, one bucket per BATCH. Restart resets the
    // phase to 0 — the bucket is placement only, not identity, so the
    // phase doesn't matter. Rotating one bucket per batch spreads write load
    // across all WALS_BUCKETS tablets, while keeping each batch on ONE
    // tablet (atomic).
    let mut next_batch_bucket: i16 = 0;
    loop {
        // Block until at least one chunk arrives.
        let first = match rx.recv().await {
            Some(c) => c,
            None => return,
        };
        let mut batch: Vec<PendingChunk> = Vec::with_capacity(FLUSH_ROWS_THRESHOLD);
        let mut batch_bytes = first.chunk_size as usize;
        batch.push(first);

        // Greedy drain (no async yield).
        while batch.len() < FLUSH_ROWS_THRESHOLD && batch_bytes < FLUSH_BYTES_THRESHOLD {
            match rx.try_recv() {
                Ok(c) => {
                    batch_bytes += c.chunk_size as usize;
                    batch.push(c);
                }
                Err(_) => break,
            }
        }

        // Accumulation wait. Under-threshold batches block up to
        // `ACCUMULATION_MAX` for more chunks to arrive, unless
        // `flush_notify` fires first — that's the "force drain on flush"
        // path. Skip the wait entirely if the batch is already at
        // threshold or if a notify is pending (from a flush_wal that
        // fired during the previous INSERT).
        let deadline = tokio::time::Instant::now() + ACCUMULATION_MAX;
        while batch.len() < FLUSH_ROWS_THRESHOLD && batch_bytes < FLUSH_BYTES_THRESHOLD {
            tokio::select! {
                biased;
                _ = flush_notify.notified() => break,
                _ = tokio::time::sleep_until(deadline) => break,
                next = rx.recv() => match next {
                    Some(c) => {
                        batch_bytes += c.chunk_size as usize;
                        batch.push(c);
                    }
                    None => break, // producer dropped; flush what we have then exit
                }
            }
        }

        // Stamp the assembled batch with its placement bucket — the ONE
        // stamping site, after assembly and before any write attempt, so a
        // retry (which replays this same Vec) inherits the same bucket and
        // produces identical rows. One bucket per batch means the batch
        // targets one tablet and lands as a single atomic write batch, until
        // that tablet splits (see `YbWalStorage::contiguous_wal_end`).
        let bucket = next_batch_bucket;
        next_batch_bucket = (next_batch_bucket + 1) % WALS_BUCKETS;
        for pc in &mut batch {
            pc.bucket_id = bucket;
        }

        let highest_lsn = batch.last().map(|c| c.end_lsn).expect("batch is non-empty");

        // Simulates the batch INSERT stalling — what a YB tablet-split storm
        // does to this path (integration sk_yb_durable_wait_stall). The stall
        // has to sit here, before the watermark advance below, because it is
        // the watermark standing still that makes `wait_for_durable` block
        // while its caller holds the timeline write lock. Configure with
        // return(<millis>).
        utils::failpoint_support::sleep_millis_async!("sk-yb-wal-flush");

        // Retry the batch until it lands. Exiting instead would make a single
        // failed batch permanent: nothing respawns this task, the watch
        // channel retains `DurableState::Failed`, and every later WAL push
        // would replay that captured string without ever calling YB — one
        // transient write error costing the endpoint all write availability
        // until a safekeeper restart (#770).
        //
        // Retrying keeps the durability invariant intact: the watermark still
        // advances only after a batch actually lands. That is recoverable —
        // when YB comes back the batch lands and progress resumes on its own.
        //
        // Retrying is NOT the same as making callers wait for it. Once the
        // batch has been failing for `CONSUMER_RETRY_SIGNAL_AFTER` the failure
        // is published so waiters fail fast instead of sitting on a watermark
        // that isn't moving: `wait_for_durable` bails immediately rather than
        // burning its full 60s bound, which releases the timeline write lock
        // and lets the manager breathe (the starvation in #765). The task
        // keeps retrying behind that, and a later success publishes `Healthy`
        // again, so the state is a transient "currently failing", not the
        // absorbing state it used to be. Wall-clock, not attempts: a
        // timeout-class failure burns up to the full RPC timeout per attempt,
        // and an attempt count would push the signal past the very bound it
        // exists to beat.
        //
        // What is deliberately NOT bounded is the data: the batch is not
        // durable, so abandoning it after N attempts would leave a hole under
        // the batches written after it — further down than the load-time hole
        // scan looks (`TAIL_SCAN_ROWS`). The bound is on how long callers
        // block, never on whether the chunks eventually land.
        let mut attempt: u32 = 0;
        let mut signalled = false;
        let mut failing_since: Option<std::time::Instant> = None;
        let flushed = loop {
            // Resolved per attempt, not once per batch: a batch that retries
            // for a long time is exactly when the lifetime bound should be
            // allowed to hand this loop a freshly-connected client (#811).
            let thin = yb_thinclient.current().await;
            match flush_batch(&thin, branch_id, &batch, codec).await {
                Ok(()) => break true,
                Err(e) => {
                    attempt += 1;
                    crate::metrics::WAL_CONSUMER_BATCH_RETRIES.inc();
                    // The batch is NOT durable, so it cannot be dropped or
                    // skipped — see the hole note above. The only options are
                    // retry or stop, and stopping is what #770 was.
                    let msg = format!("{e:#}");
                    // Chatty for the first few, then every
                    // CONSUMER_RETRY_LOG_EVERY, so a long outage doesn't drown
                    // the log while a short blip is still visible.
                    if attempt <= 3 || attempt % CONSUMER_RETRY_LOG_EVERY == 0 {
                        tracing::warn!(
                            rows = batch.len(),
                            bytes = batch_bytes,
                            attempt,
                            "YB WAL consumer batch failed, retrying: {msg}"
                        );
                    }
                    if rx.is_closed() && rx.is_empty() {
                        // Producer is gone and nothing is queued behind us:
                        // this is shutdown, not an outage. Publish the error so
                        // a waiter unblocks rather than hanging on a watermark
                        // that will never move again, then exit.
                        tracing::error!(
                            attempt,
                            "YB WAL consumer batch failed and the producer is gone: {msg}"
                        );
                        let _ = watermark_tx.send(DurableState::Failed(msg));
                        break false;
                    }
                    // Unblock waiters once, then keep retrying underneath them.
                    let since = *failing_since.get_or_insert_with(std::time::Instant::now);
                    if !signalled && since.elapsed() >= CONSUMER_RETRY_SIGNAL_AFTER {
                        tracing::error!(
                            attempt,
                            "YB WAL consumer batch still failing after {attempt} attempts; \
                             failing waiters fast while retrying: {msg}"
                        );
                        let _ = watermark_tx.send(DurableState::Failed(msg));
                        signalled = true;
                    }
                    tokio::time::sleep(consumer_retry_backoff(attempt)).await;
                }
            }
        };
        if !flushed {
            return;
        }
        {
            // Populate the read-side cache from the batch BEFORE
            // publishing the durability advance. The cache is the
            // WAL sender's fast path; we only insert chunks here
            // (after a successful INSERT) so the cache never
            // serves bytes that aren't durable in
            // `amp_storage.wals` — the invariant that pre-INSERT
            // cache population violated (the 2026-06 PS-restart
            // WAL replay bug). Deliberately the ORIGINAL chunks,
            // not the coalesced rows `flush_batch` wrote:
            // coalescing exists only at the YB row layer, and the
            // cache keeps the producer's chunk boundaries (shared
            // `Arc<Vec<u8>>` allocations, no copy).
            if let Some(c) = &cache {
                let mut guard = c.lock().unwrap();
                for pc in &batch {
                    guard.insert(pc.segno, pc.chunk_offset as u64, pc.data.clone());
                }
            }
            // Publish durability advance. The atomic is the
            // authoritative source for `YbWalStorage::flush_lsn()`'s
            // cap and `truncate_wal`'s precondition — must be set
            // BEFORE the watch send so a `wait_for_durable` waiter
            // can't observe the watch advance and then read a stale
            // `durable_lsn`. Failure on the watch send means all
            // receivers have dropped — only during shutdown — ignore.
            durable_lsn.store(highest_lsn.0, Ordering::Release);
            let _ = watermark_tx.send(DurableState::Healthy(highest_lsn));
        }
    }
}

/// Longest a producer `send` may block on chunk-channel backpressure before
/// erroring out and dropping the connection. The send runs under the timeline
/// write lock, so this is a lock-hold bound like `YB_DURABLE_WAIT_MAX`, and
/// sits inside it for the same reason `CONSUMER_RETRY_SIGNAL_AFTER` does: the
/// caller should give up on our terms, with the lock released, before anything
/// upstream starts timing out. Ordinary backpressure (YB slow, consumer
/// draining) clears in milliseconds; only a consumer that cannot land its
/// batch at all holds the channel full this long.
const YB_SEND_BACKPRESSURE_MAX: Duration = Duration::from_secs(20);

/// Log every Nth consumer batch retry once past the first few, so a long YB
/// outage leaves a readable trail instead of a flood.
const CONSUMER_RETRY_LOG_EVERY: u32 = 20;

/// How long a batch may keep failing before the consumer publishes the
/// failure and lets waiters fail fast. Comfortably inside
/// `YB_DURABLE_WAIT_MAX` (60s), so a caller blocked in `wait_for_durable`
/// gives up on our signal rather than on its own timeout and releases the
/// timeline write lock roughly three times sooner. Wall-clock deliberately:
/// timeout-class failures reach only a handful of attempts per minute, so an
/// attempt-count bound would fire after the 60s it exists to undercut.
///
/// It does not bound the retrying itself: the batch is not durable, so it is
/// retried until it lands or the producer goes away.
const CONSUMER_RETRY_SIGNAL_AFTER: Duration = Duration::from_secs(20);

/// Backoff before re-flushing a failed consumer batch: quick at first so a
/// blip costs almost nothing, capped so a long outage doesn't spin.
fn consumer_retry_backoff(attempt: u32) -> Duration {
    const CAP: Duration = Duration::from_secs(5);
    let ms = 50u64.saturating_mul(1u64 << attempt.min(7));
    std::cmp::min(Duration::from_millis(ms), CAP)
}

/// Persist one batch of chunks into `amp_storage.wals`. The whole batch
/// carries ONE bucket (stamped by `consumer_main` before the write), so it
/// writes as one atomic write to a single tablet.
///
/// Contiguous chunks are coalesced into single rows ([`coalesce_runs`]):
/// each run becomes one row keyed by its first chunk, sized/valued as the
/// concatenation of the run's chunks. The typical batch (sequential WAL
/// within one segment) is a single run — one row.
async fn flush_batch(
    yb_thinclient: &YbThinClient,
    branch_id: BranchId,
    batch: &[PendingChunk],
    codec: payload_codec::Codec,
) -> Result<()> {
    if batch.is_empty() {
        return Ok(());
    }

    // The write fence, at the single point every WAL byte crosses on its way
    // into `amp_storage.wals`. If the endpoint's lease has lapsed, this
    // process exits rather than returning an
    // error: an error goes back to the walproposer, which reconnects and
    // retries against this same stranded safekeeper. The fence has to remove
    // the writer, not the write. Armed unless `require_write_token` is turned
    // off. See `compute/architecture/compute-fencing.md`.
    utils::write_fence::global().enforce("WAL flush to amp_storage.wals");

    let runs = coalesce_runs(batch);
    let encoded: Vec<bytes::Bytes> = runs
        .iter()
        .map(|r| encode_run(&batch[r.clone()], codec))
        .collect();
    // The same lease again, this time for the leader to enforce. The check above
    // only stops a writer that is on time to notice; one that stalls between it
    // and the write is refused by the tablet, and `write_fenced` then resubmits
    // under a renewed token or exits. The batch is one bucket, but a bucket's
    // key span is not pinned to one tablet once tablets split, so a refusal
    // can be partial; after an exit the next load trims the hole
    // (`YbWalStorage::contiguous_wal_end`).
    utils::write_fence::global()
        .write_fenced("WAL flush to amp_storage.wals", server_fenced, |deadline| {
            flush_batch_thin(
                yb_thinclient,
                branch_id,
                batch,
                &runs,
                &encoded,
                deadline.into(),
            )
        })
        .await
}

/// A write error the leader returned because the write's lease deadline had
/// passed, as opposed to a failure worth retrying.
fn server_fenced(e: &anyhow::Error) -> bool {
    e.downcast_ref::<yb_thin_client::YbError>()
        .is_some_and(yb_thin_client::YbError::is_fenced)
}

/// Concatenate a run's chunk payloads and wrap them in the
/// [`payload_codec`] envelope — the exact bytes bound to the run's `data`
/// column.
///
/// This is where the WAL path gives up a zero-copy write. Previously a
/// single-chunk run bound the shared `Arc` directly and the write
/// streamed multi-chunk runs straight into the wire buffer with no
/// intermediate allocation. LZ4 needs one contiguous input, and even an
/// uncompressed row now carries a leading tag byte, so a run has to be
/// materialised either way. The compressed buffer that goes on the wire is
/// smaller than what it replaced, which is the trade.
///
/// Coalescing before compressing is deliberate: with
/// `full_page_writes = off` the WAL is a stream of small records that
/// barely compress alone, but a run of hundreds of structurally similar
/// records has ample cross-record redundancy. The bigger the window, the
/// better the ratio.
fn encode_run(chunks: &[PendingChunk], codec: payload_codec::Codec) -> bytes::Bytes {
    let plain: std::borrow::Cow<'_, [u8]> = if chunks.len() == 1 {
        std::borrow::Cow::Borrowed(chunks[0].data.as_slice())
    } else {
        let total: usize = chunks.iter().map(|p| p.chunk_size as usize).sum();
        let mut buf = Vec::with_capacity(total);
        for pc in chunks {
            buf.extend_from_slice(&pc.data);
        }
        std::borrow::Cow::Owned(buf)
    };
    let (encoded, outcome) = payload_codec::encode(&plain, codec);
    crate::metrics::record_wal_payload(plain.len(), encoded.len(), outcome);
    encoded
}

/// Thin-client (`yb_thin_client`) variant of the batched write: one Perform
/// with one `PGSQL_UPSERT` op per coalesced run, each run keyed by its
/// first chunk and valued as the concatenation of the run's chunks
/// ([`encode_run`]). The whole batch carries ONE bucket, so while that
/// bucket's key span is one tablet the ops apply as one write batch. Once a
/// tablet splits inside the bucket, two runs of a batch can land on different
/// tablets, and a crash or a fence refusal mid-flush can leave a hole, which
/// the next load finds and trims ([`YbWalStorage::contiguous_wal_end`]).
///
/// `encoded` carries the [`payload_codec`]-wrapped run buffers produced by
/// [`flush_batch`], 1:1 with `runs`. `deadline` is the lease the rows are
/// fenced on; deciding what a refusal means is [`flush_batch`]'s job.
async fn flush_batch_thin(
    yb_thinclient: &YbThinClient,
    branch_id: BranchId,
    batch: &[PendingChunk],
    runs: &[std::ops::Range<usize>],
    encoded: &[bytes::Bytes],
    deadline: yb_thin_client::WriteDeadline,
) -> Result<()> {
    // Test-only failpoint (build with `--features testing`, configure via
    // `fail::cfg("sk-wal-direct-flush-fail", "return(())")`): inject a
    // thin-client-lane WAL flush failure before any write reaches YB.
    fail::fail_point!("sk-wal-direct-flush-fail", |_| {
        Err(anyhow::anyhow!(
            "failpoint sk-wal-direct-flush-fail: injected thin-client-lane WAL flush failure"
        ))
    });
    use yb_thin_client::{BindValue, UpsertRow};
    let wals = &yb_thinclient.wals;
    let n = runs.len();
    let now_us: i64 = SystemTime::now()
        .duration_since(UNIX_EPOCH)
        .map(|d| d.as_micros() as i64)
        .unwrap_or(0);
    let branch_b = branch_id.to_bytes();
    let rows = runs
        .iter()
        .zip(encoded)
        .map(|(r, data)| {
            let chunks = &batch[r.clone()];
            let first = &chunks[0];
            // `chunk_size` stays the run's UNCOMPRESSED length — the read
            // path and `truncate_wals_above` do LSN arithmetic with
            // `chunk_offset + chunk_size`.
            // A run never leaves its segment, so it fits INTEGER as its offset does.
            let total: i32 = chunks.iter().map(|p| p.chunk_size).sum();
            UpsertRow::new(
                wals,
                // PRIMARY KEY order: `bucket_id` leads the all-range key.
                vec![
                    BindValue::I16(first.bucket_id),
                    BindValue::Bytes(branch_b.clone()),
                    BindValue::I16(DEFAULT_VERSION),
                    BindValue::I64(first.segno as i64),
                    BindValue::I32(first.chunk_offset),
                ],
                &[
                    ("chunk_size", BindValue::I32(total)),
                    ("data", BindValue::Bytes(data.clone())),
                    ("last_modified_us", BindValue::I64(now_us)),
                ],
            )
        })
        .collect::<std::result::Result<Vec<_>, _>>()?;

    // Test-only failpoint (build with `--features testing`): land every run but
    // the lowest, then exit the way a lapsed fence does — the partial batch a
    // refusal leaves once the bucket's tablet has split. Single-run batches pass
    // through, so it trips only on a batch that crosses a segment boundary or
    // isn't contiguous.
    #[cfg(feature = "testing")]
    if n >= 2 && fail::eval("sk-wal-flush-drop-leading-run", |_| ()).is_some() {
        yb_thinclient
            .client
            .upsert(&rows[1..], deadline)
            .await
            .context("failpoint sk-wal-flush-drop-leading-run: upsert the upper runs")?;
        tracing::error!(
            runs = n,
            "failpoint sk-wal-flush-drop-leading-run: landed all but the lowest run; exiting"
        );
        std::process::exit(utils::write_fence::FENCE_LAPSED_EXIT_CODE);
    }

    let batch_bytes: usize = batch.iter().map(|p| p.chunk_size as usize).sum();
    let started = std::time::Instant::now();
    // Full retry budget (inside `upsert`): the lane must ride out transient
    // failures itself — a hard error here fails the
    // consumer's flush and stalls WAL durability. There is no per-attempt
    // session rotation: the shim owns the session pool and the tserver does
    // the routing.
    let res = yb_thinclient.client.upsert(&rows, deadline).await;
    crate::metrics::YB_WAL_INSERT_SECONDS
        .with_label_values(&["yb_thin_client"])
        .observe(started.elapsed().as_secs_f64());
    crate::metrics::YB_WAL_BATCH_ROWS.observe(n as f64);
    crate::metrics::YB_WAL_BATCH_BYTES.observe(batch_bytes as f64);
    res.with_context(|| format!("YB WAL yb_thin_client upsert batch ({n} rows)"))?;
    Ok(())
}

/// Lazy chunk-streaming reader over `amp_storage.wals`, replacing the old
/// snapshot-at-open `VecAsyncRead`.
///
/// The previous design reassembled the entire 16 MiB segment from a single
/// SELECT *at open time* and served the WAL sender from that frozen
/// buffer. Chunks INSERTed into `wals` *after* the SELECT (but while the
/// WAL sender was still streaming forward through the buffer) were
/// invisible, and the buffer's leading zeros at those positions decoded
/// as `xlp_magic=0` / `xl_tot_len=0` on the pageserver side. See
/// `wal_cache.rs` module docs for the full incident.
///
/// New design: each `poll_read` finds the chunk covering the current
/// byte offset (cache → YB), then serves bytes from that chunk's data.
/// When the reader advances past the chunk's `end_offset`, it
/// transparently picks up the next chunk.
///
/// `current` caches the chunk currently being emitted so we don't
/// re-query per byte. `fetch` holds an in-flight YB query future when
/// we have to round-trip; subsequent `poll_read` calls poll the same
/// future until it resolves.
pub struct YbSegmentReader {
    segno: u64,
    /// Segment size, used to convert the segment-relative chunk offsets into
    /// absolute LSNs when comparing against the (absolute) flush bound.
    wal_seg_size: u64,
    pos: u64,

    cache: Option<Arc<std::sync::Mutex<WalCache>>>,
    /// The hot-path lane every chunk fetch runs on.
    yb_thinclient: Arc<YbThinPool>,
    branch_id: BranchId,

    current: Option<CurrentChunk>,
    /// In-flight YB fetch. A `oneshot::Receiver` (not a raw `BoxFuture`)
    /// because `WalReader::open_segment` is typed to return
    /// `Pin<Box<dyn AsyncRead + Send + Sync>>` — and `tokio_postgres`
    /// query futures aren't `Sync`. The fetch runs on a spawned task and
    /// hands the result back through this channel.
    fetch: Option<tokio::sync::oneshot::Receiver<anyhow::Result<Option<CurrentChunk>>>>,

    /// Live flush (durable/committed) frontier of the timeline. A read must
    /// never SERVE bytes past the flush that was live when the serving chunk
    /// was obtained: bytes above flush are not yet committed and a later
    /// walproposer term can truncate+rewrite them, so a held `current` (or a
    /// cached chunk) that extends past that flush would serve stale,
    /// post-rewrite-shadowing bytes once flush advances (#139). Updated live
    /// by the streaming reader as the frontier moves.
    flush_lsn: Arc<std::sync::atomic::AtomicU64>,
    /// The flush frontier that was live when `current` was acquired — the
    /// trust boundary for it. `current` is served only up to here; past it we
    /// re-acquire (re-fetch) so we never serve bytes that were uncommitted at
    /// fetch time.
    current_flush_bound: u64,
    /// Flush frontier captured when the in-flight `fetch` was issued (its
    /// bytes are a snapshot as of then), carried to `current_flush_bound` when
    /// the fetch lands.
    fetch_issued_flush: u64,
}

#[derive(Clone)]
struct CurrentChunk {
    chunk_offset: u64,
    end_offset: u64,
    data: Arc<Vec<u8>>,
}

impl YbSegmentReader {
    /// Build a reader for `segno` starting at byte offset `start_xlogoff`.
    /// The reader holds clones of the hot-path lane + cache handle, so it's
    /// `Send + Sync` and can be passed up the WAL sender stack like the
    /// `tokio::fs::File` it replaces.
    pub fn new(
        yb_wal: Arc<YbWalConn>,
        branch_id: BranchId,
        segno: XLogSegNo,
        wal_seg_size: usize,
        start_xlogoff: usize,
        flush_lsn: Arc<std::sync::atomic::AtomicU64>,
    ) -> Self {
        Self {
            segno,
            wal_seg_size: wal_seg_size as u64,
            pos: start_xlogoff as u64,
            cache: yb_wal.cache.clone(),
            yb_thinclient: yb_wal.yb_thinclient.clone(),
            branch_id,
            current: None,
            fetch: None,
            flush_lsn,
            current_flush_bound: 0,
            fetch_issued_flush: 0,
        }
    }

    fn start_fetch(&mut self) {
        // Snapshot the flush frontier at issue time: the rows this fetch reads
        // are committed at least up to here, and a walproposer term change can
        // only rewrite bytes *above* the flush that is live now. So whatever
        // this fetch returns is trustworthy up to `fetch_issued_flush`, which
        // becomes the served chunk's trust boundary when it lands — and gates
        // which chunks are safe to cache (below).
        self.fetch_issued_flush = self.flush_lsn.load(std::sync::atomic::Ordering::Acquire);
        let issued_flush = self.fetch_issued_flush;
        let branch_id = self.branch_id;
        let segno = self.segno;
        let pos = self.pos;
        // Base LSN of this segment, to bring segment-relative chunk offsets into
        // the same (absolute) frame as `issued_flush` for the cache-warm gate.
        let seg_base = self.segno * self.wal_seg_size;
        let cache = self.cache.clone();
        let yb_thinclient = self.yb_thinclient.clone();
        let (tx, rx) = tokio::sync::oneshot::channel();
        tokio::spawn(async move {
            let thin = yb_thinclient.current().await;
            let res = fetch_chunks(&thin, branch_id, segno, pos).await;
            let covering = match res {
                Ok(fetched) => {
                    // Warm the cache with what we just read — but ONLY chunks
                    // lying fully below the flush frontier live when this fetch
                    // was issued (`end_offset <= issued_flush`). Those are
                    // committed, and committed WAL is immutable: a later term
                    // can only truncate+rewrite bytes ABOVE flush, and truncate
                    // never removes committed WAL, so a cached copy can never be
                    // shadowed by a rewrite. A chunk straddling or past flush is
                    // deliberately left out — its tail is still mutable, and a
                    // read that cached it could race a truncate+rewrite (its
                    // insert landing after `cache.truncate_after`) and pin a
                    // stale chunk the Vacant-only insert can't displace: the
                    // torn read of #139. The serve-side flush bound guards the
                    // *held* chunk, but a cache *re-lookup* returns the same
                    // bytes — so the cache must never take uncommitted bytes in
                    // the first place.
                    //
                    // `c.end_offset` is a SEGMENT-relative offset; `issued_flush`
                    // is an ABSOLUTE LSN. Compare in the same (absolute) frame —
                    // `seg_base + c.end_offset <= issued_flush` — or the gate is a
                    // no-op for any segment ≥ 1 (a within-segment offset is always
                    // below an absolute LSN), caching uncommitted-tail chunks that
                    // a later term can rewrite (the other half of the #139 splice).
                    if let Some(cache) = &cache {
                        let mut guard = cache.lock().unwrap();
                        for c in &fetched {
                            if seg_base + c.end_offset <= issued_flush {
                                guard.insert(segno, c.chunk_offset, c.data.clone());
                            }
                        }
                    }
                    // `fetch_chunks` returns the covering chunk
                    // first — its `chunk_offset` is `<= pos` (exactly
                    // `pos` on the boundary-aligned hot path, below it on
                    // a cold mid-chunk resume). Accept it only when it
                    // actually spans `pos`; a first row that starts past
                    // `pos`, or whose range ends at/before it, is a real
                    // WAL hole and propagates as `None`.
                    let covering = fetched
                        .into_iter()
                        .next()
                        .filter(|c| c.chunk_offset <= pos && pos < c.end_offset);
                    Ok(covering)
                }
                Err(e) => Err(e),
            };
            let _ = tx.send(covering);
        });
        self.fetch = Some(rx);
    }
}

impl AsyncRead for YbSegmentReader {
    fn poll_read(
        mut self: Pin<&mut Self>,
        cx: &mut TaskContext<'_>,
        dst: &mut ReadBuf<'_>,
    ) -> Poll<std::io::Result<()>> {
        loop {
            // 1. Serve from the current chunk if it covers self.pos.
            let me = self.as_mut().get_mut();
            if let Some(c) = &me.current {
                // Serve only up to the flush frontier that was live when this
                // chunk was acquired. Bytes above that flush were not yet
                // committed then, so a later walproposer term may have
                // truncated+rewritten them in YB; serving them from a held
                // (now-stale) chunk after flush advances is exactly the
                // spliced "invalid xlog page header" read of #139. Past the
                // boundary we drop the chunk and re-acquire, picking up the
                // post-rewrite bytes (and a higher boundary).
                // `pos`, `chunk_offset` and `end_offset` are SEGMENT-relative
                // byte offsets, but `current_flush_bound` is an ABSOLUTE LSN
                // (it comes from the `flush_lsn` atomic = `wal_reader.end`).
                // Bring the bound into this segment's frame before the min, or
                // the comparison is a no-op: a within-segment offset (< 16 MiB)
                // is always smaller than an absolute LSN for any segment ≥ 1, so
                // `end_offset.min(absolute_bound)` always returns `end_offset`
                // and the chunk is served in full — including bytes that were
                // uncommitted when fetched, which a later term can rewrite. That
                // was the #139 splice: an old-term held chunk served its stale
                // tail across a truncate. `saturating_sub` clamps a bound below
                // this segment to 0 (serve nothing, re-acquire); a bound in a
                // later segment exceeds `end_offset`, so the min keeps the whole
                // (fully-committed) chunk.
                let seg_base = me.segno * me.wal_seg_size;
                let bound_in_seg = me.current_flush_bound.saturating_sub(seg_base);
                let serve_end = c.end_offset.min(bound_in_seg);
                if me.pos >= c.chunk_offset && me.pos < serve_end {
                    let chunk_idx = (me.pos - c.chunk_offset) as usize;
                    let serve_remaining = (serve_end - me.pos) as usize;
                    let want = dst.remaining().min(serve_remaining);
                    if want == 0 {
                        return Poll::Ready(Ok(()));
                    }
                    dst.put_slice(&c.data[chunk_idx..chunk_idx + want]);
                    me.pos += want as u64;
                    return Poll::Ready(Ok(()));
                }
                // Walked past the current chunk's trusted range — drop it.
                me.current = None;
            }

            // 2. Try the in-memory cache. Fast O(log n), no I/O. The cached
            // chunk is trusted only up to the flush frontier live right now;
            // the consumer may have inserted it before flush caught up, so its
            // tail can still be above the committed line.
            if let Some(cache) = me.cache.clone() {
                let hit = cache.lock().unwrap().lookup(me.segno, me.pos);
                if let Some((chunk_offset, end_offset, data)) = hit {
                    me.current_flush_bound =
                        me.flush_lsn.load(std::sync::atomic::Ordering::Acquire);
                    me.current = Some(CurrentChunk {
                        chunk_offset,
                        end_offset,
                        data,
                    });
                    continue;
                }
            }

            // 3. Fall through to YB. Drive the fetch via a spawned task.
            if me.fetch.is_none() {
                me.start_fetch();
            }
            let rx = me.fetch.as_mut().unwrap();
            match Pin::new(rx).poll(cx) {
                Poll::Pending => return Poll::Pending,
                Poll::Ready(Ok(Ok(Some(c)))) => {
                    me.fetch = None;
                    me.current_flush_bound = me.fetch_issued_flush;
                    me.current = Some(c);
                    continue;
                }
                Poll::Ready(Ok(Ok(None))) => {
                    // Neither cache nor YB has a chunk covering self.pos, and
                    // the forward search found none later in this segment —
                    // so [self.pos, segment end) is a gap.
                    //
                    // The benign cause is XLOG_SWITCH padding: pg_switch_wal()
                    // ends a segment with a switch record and leaves the rest
                    // of the segment zero-padded. The compute streams WAL only
                    // up to the switch record, not the padding, so a cold
                    // reader catching up across a *historical* switch (e.g. a
                    // pageserver whose durable consistent_lsn lagged behind it)
                    // finds the segment tail missing (#181). When the committed
                    // frontier has already advanced to or past the next segment
                    // (flush_lsn >= segment end), this segment is complete and
                    // the gap is that padding: synthesize the zero bytes
                    // Postgres would have on disk. The WalStreamDecoder, having
                    // decoded the switch record (which precedes the gap and is
                    // always persisted), is in its skip-to-next-segment state
                    // and consumes them. If the gap is NOT padding (genuine
                    // missing WAL), the decoder is not skipping and chokes on
                    // the zeros (`xlp_magic=0`) — surfacing as a decode error,
                    // never silently masked.
                    me.fetch = None;
                    let seg_end = me.segno * me.wal_seg_size + me.wal_seg_size;
                    let committed_past_segment =
                        me.flush_lsn.load(std::sync::atomic::Ordering::Acquire) >= seg_end;
                    if committed_past_segment && me.pos < me.wal_seg_size {
                        let zeros = (me.wal_seg_size - me.pos).min(dst.remaining() as u64) as usize;
                        if zeros > 0 {
                            dst.initialize_unfilled_to(zeros)[..zeros].fill(0);
                            dst.advance(zeros);
                            me.pos += zeros as u64;
                            return Poll::Ready(Ok(()));
                        }
                    }
                    return Poll::Ready(Err(std::io::Error::new(
                        std::io::ErrorKind::UnexpectedEof,
                        format!(
                            "YB WAL hole at segno {} offset {} (neither cache nor `wals` has a covering chunk)",
                            me.segno, me.pos
                        ),
                    )));
                }
                Poll::Ready(Ok(Err(e))) => {
                    me.fetch = None;
                    return Poll::Ready(Err(std::io::Error::other(format!(
                        "YB WAL fetch failed for segno {} offset {}: {e:#}",
                        me.segno, me.pos
                    ))));
                }
                Poll::Ready(Err(_recv_err)) => {
                    // Spawned fetch task dropped its sender without
                    // sending — shouldn't happen, but treat as IO error.
                    me.fetch = None;
                    return Poll::Ready(Err(std::io::Error::other(
                        "YB WAL fetch task dropped without result",
                    )));
                }
            }
        }
    }
}

/// Soft caps on the forward prefetch window. Each fetch turns into
/// one YB round trip, so amortising it across more chunks is the
/// whole point — but pulling 16 MiB at once would also be silly. With
/// chunks averaging ~10 KiB observed on TPCC, 64 chunks ≈ 640 KiB per
/// round trip is enough to make the WAL sender's sequential stream
/// hit cache every time after the first miss in a run.
const FETCH_FORWARD_CHUNKS: usize = 64;
const FETCH_FORWARD_BYTES: u64 = 4 * 1024 * 1024;

/// Fetch the chunk covering `(segno, byte_offset)` plus a forward
/// prefetch window from `amp_storage.wals`.
///
/// The common case is `byte_offset` exactly equal to an existing
/// `chunk_offset`: the WAL sender consumes whole chunks and re-enters
/// this path at chunk boundaries, and the warm `WalCache` lookup that
/// runs before us already resolves any sub-chunk position via its
/// `range(..=)` walk. Then the forward SELECT's first row is the
/// covering chunk and a single round trip suffices.
///
/// A cold reader created right after a safekeeper restart is the
/// exception: its first read starts at the pageserver's resume LSN
/// (its `consistent_lsn`), an arbitrary record-end offset that can
/// land in the MIDDLE of a durable chunk, and the cache is empty. The
/// forward SELECT (`chunk_offset >= byte_offset`) then misses the
/// covering chunk, so when the first forward row doesn't start exactly
/// at `byte_offset` we issue one extra reverse-seek for the chunk that
/// starts at or before it and prepend it when it covers — otherwise a
/// legitimate mid-chunk resume would surface as a bogus "WAL hole".
///
/// The first returned row is the covering chunk; the rest populate the
/// `WalCache` so the WAL sender's forward-sequential reads hit RAM.
/// One YB round trip per ~`FETCH_FORWARD_CHUNKS` chunks instead of the
/// previous one-round-trip-per-chunk.
/// Fetch the covering + forward chunks for `byte_offset` on the hot-path
/// lane, from one consistent snapshot (see [`fetch_chunks_thin`] for how).
async fn fetch_chunks(
    yb_thinclient: &YbThinClient,
    branch_id: BranchId,
    segno: u64,
    byte_offset: u64,
) -> anyhow::Result<Vec<CurrentChunk>> {
    fetch_chunks_thin(yb_thinclient, branch_id, segno, byte_offset).await
}

/// Client-side merge for the thin-client fetch fan-out (the row decoder
/// lives in `yb_thin_client`, so the result rows are `yb_thin_client::Row`).
/// Preserves the SQL path's semantics:
/// covering = the max-offset row with `chunk_offset < pos` across all
/// buckets that spans `pos` (a "covering" row that ends at or before
/// the position is a real hole, not a cover — same filter as the SQL
/// path); forward = ascending merge capped at [`FETCH_FORWARD_CHUNKS`]
/// total (per-bucket limits alone would allow WALS_BUCKETS× that). At
/// most one covering chunk survives the filter (chunks are disjoint, so
/// only one can span `byte_offset`); it sorts first and doesn't count
/// toward the cap.
fn merge_fetched_rows<'a>(
    rows: impl Iterator<Item = &'a yb_thin_client::Row>,
    byte_offset: u64,
) -> anyhow::Result<Vec<CurrentChunk>> {
    let mut out: Vec<CurrentChunk> = Vec::new();
    for row in rows {
        let start: u64 = row[0]
            .as_i64()?
            .try_into()
            .map_err(|_| anyhow::anyhow!("negative chunk_offset"))?;
        let size: u64 = row[1]
            .as_i32()?
            .try_into()
            .map_err(|_| anyhow::anyhow!("negative chunk_size"))?;
        let end = start + size;
        if start < byte_offset && end <= byte_offset {
            continue;
        }
        out.push(CurrentChunk {
            chunk_offset: start,
            end_offset: end,
            // MUST decode: `chunk_offset`/`end_offset` are LSN-space, and the
            // segment reader slices `data` with those offsets. Handing it a
            // still-encoded buffer panics with an out-of-range slice as soon
            // as compression makes the stored bytes shorter than the logical
            // span.
            data: Arc::new(decode_chunk_data(
                row[2].as_bytes()?,
                &format!("WAL chunk fetch (thin): offset={start}"),
            )?),
        });
    }
    out.sort_by_key(|c| c.chunk_offset);
    let n_covering = out
        .iter()
        .take_while(|c| c.chunk_offset < byte_offset)
        .count();
    out.truncate(n_covering + FETCH_FORWARD_CHUNKS);
    Ok(out)
}

/// Thin-client (`yb_thin_client`) two-arm fetch. The SQL version keeps both
/// arms in ONE statement for a single read snapshot; here a chunk's
/// bucket can't be derived, so every (arm, bucket) pair is its own
/// read op — covering arm per bucket (`chunk_offset < pos`, descending,
/// limit 1) and forward arm per bucket (`[pos, pos +
/// FETCH_FORWARD_BYTES)`, ascending, limit [`FETCH_FORWARD_CHUNKS`]) —
/// and ALL `2×WALS_BUCKETS` of them are issued as ops of ONE Perform: the
/// proxy picks one read point at setup and every op shares it, so the
/// fetch is a single snapshot BY CONSTRUCTION (#139). Ops that come back
/// with a continuation are re-issued, pinned to that same read point,
/// until each has the rows it wants — see the paging loop for why one
/// page is not enough once `wals` is range-sharded.
/// Results merge client-side through [`merge_fetched_rows`], preserving
/// the SQL path's semantics: covering = the max-offset row with
/// `chunk_offset < pos` across all buckets that spans `pos`; forward =
/// ascending merge capped at `FETCH_FORWARD_CHUNKS`. The reader's
/// flush-bound gating stays as defense in depth regardless.
async fn fetch_chunks_thin(
    yb_thinclient: &YbThinClient,
    branch_id: BranchId,
    segno: u64,
    byte_offset: u64,
) -> anyhow::Result<Vec<CurrentChunk>> {
    use yb_thin_client::{BindValue, CondOp, RangeRead};
    let started_at = std::time::Instant::now();
    let wals = &yb_thinclient.wals;
    let targets = ["chunk_offset", "chunk_size", "data"];
    let segno_i = segno as i64;
    // `chunk_offset` is INTEGER. `byte_offset` is inside the segment,
    // which PostgreSQL caps at 1 GiB; the forward bound can run past the
    // segment's end, and clamping it loses nothing — no offset exceeds INTEGER.
    let offset_lo = i32::try_from(byte_offset)
        .with_context(|| format!("WAL segment offset {byte_offset} exceeds INTEGER"))?;
    let offset_hi =
        i32::try_from(byte_offset.saturating_add(FETCH_FORWARD_BYTES)).unwrap_or(i32::MAX);

    // Both arms scan one `(bucket_id, branch_id, segno)`'s chunk range and
    // differ only in their `chunk_offset` bound, so they share a base read.
    //
    // `wals` is all-range with no hash columns, so the prefix goes in as
    // `range_values` in PRIMARY KEY order — bucket first. This bounds the scan
    // to a contiguous key span but NOT to a single tablet, which is why the
    // caller must page: a tablet holding no matching chunk returns an empty
    // page plus a continuation, meaning "nothing here", not "scan done".
    //
    // The hash-sharded arm this used to carry is gone with the migration. It
    // could not be kept anyway: the key order changed to bucket-leading, so a
    // hash binding of `(branch_id, bucket_id)` would now be wrong rather than
    // merely unused.
    let branch_b = branch_id.to_bytes();
    let base_read = |bucket: i16| -> anyhow::Result<RangeRead> {
        Ok(RangeRead::new_range(
            wals,
            vec![
                BindValue::I16(bucket),
                BindValue::Bytes(branch_b.clone()),
                BindValue::I16(DEFAULT_VERSION),
                BindValue::I64(segno_i),
            ],
        )?)
    };

    // Covering arm for one bucket: its highest chunk starting below
    // byte_offset; the cross-bucket max resolves in the merge.
    let covering_read = |bucket: i16| -> anyhow::Result<RangeRead> {
        Ok(base_read(bucket)?
            .cond(wals, "chunk_offset", CondOp::Lt, BindValue::I32(offset_lo))?
            .targets(wals, &targets)?
            .descending()
            .limit(1))
    };
    // Forward arm for one bucket: ascending chunks in
    // [byte_offset, byte_offset + FETCH_FORWARD_BYTES).
    let forward_read = |bucket: i16| -> anyhow::Result<RangeRead> {
        Ok(base_read(bucket)?
            .cond(wals, "chunk_offset", CondOp::Ge, BindValue::I32(offset_lo))?
            .cond(wals, "chunk_offset", CondOp::Lt, BindValue::I32(offset_hi))?
            .targets(wals, &targets)?
            .limit(FETCH_FORWARD_CHUNKS as u64))
    };

    let mut reads: Vec<RangeRead> = Vec::with_capacity(2 * WALS_BUCKETS as usize);
    for bucket in 0..WALS_BUCKETS {
        reads.push(covering_read(bucket)?);
    }
    for bucket in 0..WALS_BUCKETS {
        reads.push(forward_read(bucket)?);
    }
    // How many rows each arm still wants. The covering arm is `limit(1)` and
    // is finished the moment it yields its row; the forward arm wants up to
    // FETCH_FORWARD_CHUNKS (the merge caps the cross-bucket total again).
    let wanted = |op_idx: usize| -> usize {
        if op_idx < WALS_BUCKETS as usize {
            1
        } else {
            FETCH_FORWARD_CHUNKS
        }
    };

    // Page each op until it has what it wants or its scan is exhausted.
    //
    // A single page is NOT enough once `wals` is range-sharded. An op's scan
    // then spans tablets, and a tablet that holds no matching row yields an
    // EMPTY page plus a continuation — "this tablet had no match", not "the
    // scan is done". The covering arm is where that bites: it scans backwards
    // from `byte_offset`, so it opens in the highest tablet in range, whose
    // rows may all start at or after `byte_offset` and be filtered out. Taking
    // only that first page reads as "no covering chunk" and surfaces as a
    // bogus `WAL hole at {lsn}` for a perfectly healthy segment.
    //
    // `wals` is range-sharded, so an op's scan is bounded to a contiguous key
    // span but NOT pinned to one tablet, and this loop genuinely runs several
    // rounds — see the barren-round reasoning in `yb_thin_client::barren`. It also
    // picks up forward rows that a size-capped page used to drop. Reads are
    // rare relative to writes, so the extra round trips are cheap insurance —
    // and nothing here touches the write path.
    // Rounds within one attempt; attempts of the whole fetch on a read-restart.
    //
    // The restart budget is not optional. `TsClient::read` heals a read-restart
    // internally only for an UNPINNED batch: it classifies a batch as pinned
    // when `read_time_ht != 0` or any op carries a `paging_state`, and gives up
    // immediately in that case because every retry would replay the same read
    // point. An op only reaches round 2 by having returned a continuation, so
    // every round after the first is pinned by construction, and a restart
    // there — a routine hybrid-time uncertainty window from a concurrent write,
    // not a failure — would propagate out as a hard "YB WAL fetch failed" IO
    // error and likely drop the WAL sender connection. `TsClient::read` says as
    // much: it surfaces the restart "so the scan driver restarts the whole
    // scan." This loop is that driver, so it owns the restart, discarding the
    // half-built rows and starting over from an unpinned round 1 the way
    // `read_scan` does.
    const MAX_FETCH_RESTARTS: usize = 5;
    let mut fetched: Option<Vec<Vec<yb_thin_client::Row>>> = None;
    for attempt in 1..=MAX_FETCH_RESTARTS {
        let mut rows: Vec<Vec<yb_thin_client::Row>> = vec![Vec::new(); reads.len()];
        let mut paging: Vec<Option<bytes::Bytes>> = vec![None; reads.len()];
        let mut active: Vec<usize> = (0..reads.len()).collect();
        // Per-op consecutive-empty-round counters; see `check_barren_round`.
        let mut barren: Vec<usize> = vec![0; reads.len()];
        let mut read_time_ht = 0u64;
        let mut round = 0usize;
        let mut restart = false;
        while !active.is_empty() {
            round += 1;
            let ops: Vec<yb_thin_client::ReadOp> = active
                .iter()
                .map(|&i| yb_thin_client::ReadOp {
                    table: wals.as_ref(),
                    read: &reads[i],
                    paging_state: paging[i].as_deref(),
                })
                .collect();
            let batch = match yb_thinclient.client.read(&ops, read_time_ht).await {
                Ok(batch) => batch,
                Err(e) if e.is_read_restart() && attempt < MAX_FETCH_RESTARTS => {
                    tracing::warn!(
                        "thin-lane WAL fetch read-restart on round {round} \
                         (attempt {attempt}/{MAX_FETCH_RESTARTS}), restarting the fetch: {e}"
                    );
                    restart = true;
                    break;
                }
                Err(e) => return Err(e.into()),
            };
            // Pin every continuation to the snapshot round 1 ran at, so the
            // whole fan-out still resolves at ONE read point (#139).
            read_time_ht = batch.used_read_time_ht;
            // `active[slot]` below is safe because `ops` is 1:1 with `active`
            // and `TsClient::read` guarantees one result per op sent.
            debug_assert_eq!(ops.len(), active.len());
            let mut next: Vec<usize> = Vec::new();
            for (slot, res) in batch.ops.into_iter().enumerate() {
                let i = active[slot];
                let got = res.rows.len();
                rows[i].extend(res.rows);
                if rows[i].len() < wanted(i) && res.paging_state.is_some() {
                    yb_thin_client::check_barren_round(&mut barren[i], got).with_context(|| {
                        let arm = if i < WALS_BUCKETS as usize {
                            "covering"
                        } else {
                            "forward"
                        };
                        format!(
                            "WAL chunk fetch for segno {segno} offset {byte_offset} \
                             on the {arm} arm of bucket {}",
                            i % WALS_BUCKETS as usize
                        )
                    })?;
                    paging[i] = res.paging_state;
                    next.push(i);
                }
            }
            active = next;
        }
        if !restart {
            fetched = Some(rows);
            break;
        }
    }
    // The final attempt either converges or propagates its error above.
    let rows = fetched.expect("the final fetch attempt returns or propagates");
    let out = merge_fetched_rows(rows.iter().flatten(), byte_offset)?;
    let total_bytes: u64 = out.iter().map(|c| c.end_offset - c.chunk_offset).sum();

    crate::metrics::WAL_CACHE_YB_FETCH_SECONDS
        .with_label_values(&["yb_thin_client"])
        .observe(started_at.elapsed().as_secs_f64());
    crate::metrics::WAL_CACHE_YB_FETCH_CHUNKS.observe(out.len() as f64);
    crate::metrics::WAL_CACHE_YB_FETCH_BYTES.observe(total_bytes as f64);
    Ok(out)
}

/// WAL storage used by the safekeeper, named as a type alias so the
/// `<WAL: wal_storage::Storage>` generics on `SafeKeeper` read uniformly.
pub type WalStore = YbWalStorage;

impl YbWalStorage {
    /// `(byte_end, record_end)` for the debug-dump endpoint.
    pub fn internal_state(&self) -> (Lsn, Lsn) {
        (
            self.accepted_stream.byte_end(),
            self.accepted_stream.record_end(),
        )
    }
}

#[cfg(test)]
mod tests {
    use super::*;
    use crate::state::TimelinePersistentState;
    use crate::wal_storage::Storage;
    use camino_tempfile::Utf8TempDir;
    use utils::id::BranchId;
    // Tests build a `yb_tokio_postgres::Config` via `tokio_postgres::*`
    // type paths; the production code in this module doesn't need the
    // alias.
    use yb_tokio_postgres as tokio_postgres;

    /// `bake_branch_id` inlines branch_id as a literal and shifts the
    /// remaining params down by one — no live YB needed. Locks the
    /// numbering the `wals` query call sites depend on.
    #[test]
    fn quote_relation_splits_schema_qualified_names() {
        // Production uses the schema-qualified `WALS_TABLE`; each part
        // must be quoted independently so it resolves regardless of the
        // connection's search_path (a bare `"amp_storage.wals"` would be
        // a single — nonexistent — identifier).
        assert_eq!(
            quote_relation("amp_storage.wals"),
            "\"amp_storage\".\"wals\""
        );
        // Per-test scratch tables are unqualified → one quoted ident.
        assert_eq!(quote_relation("wals_test_abc"), "\"wals_test_abc\"");
        // Embedded quotes are doubled in every segment.
        assert_eq!(quote_relation("a\"b.c\"d"), "\"a\"\"b\".\"c\"\"d\"");
    }

    /// Both transient classes must classify as races and a real error must
    /// not. The `already exists` and `deleted while still in use` literals are
    /// verbatim CI captures (#508, #1130); the `42710` case and the negative
    /// are synthesized.
    #[test]
    fn scratch_table_catalog_races_are_retryable() {
        assert!(is_scratch_table_catalog_race(
            "db error: ERROR: relation \"wals_test_trunc_cache_12e58617\" already exists"
        ));
        assert!(is_scratch_table_catalog_race(
            "db error: ERROR: relation 16920 deleted while still in use"
        ));
        assert!(is_scratch_table_catalog_race(
            "db error: ERROR: type \"wals_test_a1b2c3d4\" already exists"
        ));
        assert!(!is_scratch_table_catalog_race(
            "db error: ERROR: syntax error at or near \"CRAETE\""
        ));
    }

    #[test]
    fn bake_branch_id_bakes_and_shifts() {
        let b: BranchId = "00112233-4455-6677-8899-aabbccddeeff".parse().unwrap();
        let sql = bake_branch_id(
            "SELECT chunk_offset FROM \"wals\" \
             WHERE branch_id = $1 AND segno = $2 \
               AND chunk_offset >= $3 AND chunk_offset < $4 LIMIT $5",
            b,
        );
        assert!(sql.contains(&format!("branch_id = '{b}'")));
        // $2..$5 shifted to $1..$4; no $5 and no bare $1 (branch) left.
        assert!(sql.contains("segno = $1"));
        assert!(sql.contains("chunk_offset >= $2 AND chunk_offset < $3"));
        assert!(sql.contains("LIMIT $4"));
        assert!(!sql.contains("$5"));
    }

    /// `coalesce_runs` backs the consumer's chunk→row coalescing: each
    /// returned range becomes ONE `amp_storage.wals` row, so runs must
    /// break at every segment boundary and byte discontinuity — and
    /// nowhere else.
    /// A `PendingChunk` with only the fields `coalesce_runs` reads set;
    /// data/end_lsn/bucket are placeholders.
    fn pc(segno: u64, chunk_offset: i32, chunk_size: i32) -> PendingChunk {
        PendingChunk {
            segno,
            bucket_id: 0,
            chunk_offset,
            chunk_size,
            data: Arc::new(Vec::new()),
            end_lsn: Lsn(0),
        }
    }

    #[test]
    fn coalesce_runs_empty() {
        assert_eq!(coalesce_runs(&[]), Vec::<std::ops::Range<usize>>::new());
    }

    #[test]
    fn coalesce_runs_single_chunk() {
        assert_eq!(coalesce_runs(&[pc(3, 100, 50)]), vec![0..1]);
    }

    #[test]
    fn coalesce_runs_single_run() {
        // Three byte-contiguous chunks in one segment → one row.
        let chunks = [pc(7, 0, 100), pc(7, 100, 400), pc(7, 500, 24)];
        assert_eq!(coalesce_runs(&chunks), vec![0..3]);
    }

    #[test]
    fn coalesce_runs_splits_at_discontinuity() {
        // Gap between chunk 1 and 2 (600 != 500) → two rows. A
        // discontinuity mid-batch can only come from a truncate/rewrite
        // realigning the stream; the grouping must not paper over it.
        let chunks = [pc(7, 0, 100), pc(7, 100, 400), pc(7, 600, 24)];
        assert_eq!(coalesce_runs(&chunks), vec![0..2, 2..3]);
    }

    #[test]
    fn coalesce_runs_splits_at_segment_boundary() {
        // Byte-adjacent across the segment switch, but segno differs →
        // split; rows never span segments (chunk_offset is
        // segment-relative).
        let chunks = [pc(7, 100, 400), pc(8, 0, 100), pc(8, 100, 32)];
        assert_eq!(coalesce_runs(&chunks), vec![0..1, 1..3]);
    }

    #[test]
    fn anchor_target_skips_a_page_whose_header_is_cut() {
        use postgres_ffi::XLOG_BLCKSZ;
        let blk = XLOG_BLCKSZ as u64;
        let page = SEG_SIZE + 6 * blk;
        assert_eq!(anchor_target(Lsn(page + 100), SEG_SIZE), Lsn(page + 99));
        // Page-aligned: the last byte is on the page below (#678).
        assert_eq!(anchor_target(Lsn(page), SEG_SIZE), Lsn(page - 1));
        // Inside the short header.
        assert_eq!(anchor_target(Lsn(page + 17), SEG_SIZE), Lsn(page - 1));
        // Inside a segment's long header, which is longer than a short one.
        assert_eq!(
            anchor_target(Lsn(SEG_SIZE + 30), SEG_SIZE),
            Lsn(SEG_SIZE - 1)
        );
    }

    /// The tail scan's bound only holds if it reaches past a whole batch.
    #[test]
    fn the_tail_scan_reaches_below_a_whole_batch() {
        const { assert!(TAIL_SCAN_ROWS > FLUSH_ROWS_THRESHOLD) };
    }

    #[test]
    fn global_tail_keeps_only_the_highest_rows() {
        // Bucket A holds the top of the stream; bucket B went quiet long ago and
        // its top rows sit far below. Their union has a gap where neither
        // bucket's top rows reach — rows that exist but were never fetched.
        let a: Vec<(u64, u64)> = (10..13).map(|i| (i * 100, i * 100 + 100)).collect();
        let b: Vec<(u64, u64)> = (0..3).map(|i| (i * 100, i * 100 + 100)).collect();
        let union: Vec<(u64, u64)> = b.iter().chain(&a).copied().collect();
        assert_eq!(
            contiguous_end(&global_tail(union.clone(), usize::MAX), 1300, &[]),
            300,
            "untrimmed, the unfetched rows read as a hole"
        );
        let tail = global_tail(union, 3);
        assert_eq!(tail, a);
        assert_eq!(contiguous_end(&tail, 1300, &[]), 1300);

        assert!(global_tail(Vec::<(u64, u64)>::new(), 3).is_empty());
        assert_eq!(global_tail(vec![(5, 9), (0, 5)], 3), vec![(0, 5), (5, 9)]);
    }

    /// Only the upper runs of the one batch in flight may be trimmed; anything
    /// more landed later and may have been acked.
    #[test]
    fn only_one_partial_batch_above_a_hole_may_be_trimmed() {
        let below = [(0u64, 100u64, 0i16), (100, 200, 1)];
        let one_run = [below.as_slice(), &[(300, 400, 2)]].concat();
        assert_eq!(not_one_partial_batch(&one_run, 200), None);

        let two_buckets = [below.as_slice(), &[(300, 400, 2), (400, 500, 3)]].concat();
        let why = not_one_partial_batch(&two_buckets, 200).expect("two buckets");
        assert!(why.contains("buckets [2, 3]"), "{why}");

        let full_batch: Vec<(u64, u64, i16)> = (0..FLUSH_ROWS_THRESHOLD as u64)
            .map(|i| (300 + i * 10, 310 + i * 10, 4))
            .collect();
        let too_many = [below.as_slice(), &full_batch].concat();
        assert!(not_one_partial_batch(&too_many, 200).is_some());
        assert_eq!(
            not_one_partial_batch(&too_many[..too_many.len() - 1], 200),
            None
        );
    }

    #[test]
    fn contiguous_end_stops_at_the_first_hole() {
        // Empty tail: only an empty branch, where the raw max is the fallback.
        assert_eq!(contiguous_end(&[], 4242, &[]), 4242);

        let clean = [(0u64, 100u64), (100, 250), (250, 900)];
        assert_eq!(contiguous_end(&clean, 900, &[]), 900);

        let holed = [(0u64, 100u64), (100, 250), (400, 700), (700, 900)];
        assert_eq!(contiguous_end(&holed, 900, &[]), 250);

        let two_holes = [(0u64, 100u64), (200, 300), (300, 900)];
        assert_eq!(contiguous_end(&two_holes, 900, &[]), 100);

        // The walk anchors at the lowest row, not at zero.
        let anchored = [(SEG_SIZE, SEG_SIZE + 100), (SEG_SIZE + 100, SEG_SIZE + 300)];
        assert_eq!(
            contiguous_end(&anchored, SEG_SIZE + 300, &[]),
            SEG_SIZE + 300
        );

        let overlap = [(0u64, 100u64), (0, 100), (100, 250)];
        assert_eq!(contiguous_end(&overlap, 250, &[]), 250);

        // Confirmed XLOG_SWITCH padding is walked through: the rows above it
        // are acked WAL.
        let padded = [(0u64, 250u64), (SEG_SIZE, SEG_SIZE + 300)];
        assert_eq!(
            contiguous_end(&padded, SEG_SIZE + 300, &[(250, SEG_SIZE)]),
            SEG_SIZE + 300
        );
        assert_eq!(contiguous_end(&padded, SEG_SIZE + 300, &[]), 250);
        let padded_then_holed = [
            (0u64, 250u64),
            (SEG_SIZE, SEG_SIZE + 300),
            (SEG_SIZE + 900, SEG_SIZE + 1000),
        ];
        assert_eq!(
            contiguous_end(&padded_then_holed, SEG_SIZE + 1000, &[(250, SEG_SIZE)]),
            SEG_SIZE + 300
        );
    }

    #[test]
    fn tail_gaps_reports_every_gap_not_just_the_first() {
        assert!(tail_gaps(&[]).is_empty());
        assert!(tail_gaps(&[(0, 100), (100, 250)]).is_empty());
        assert_eq!(
            tail_gaps(&[(0, 100), (200, 300), (400, 500)]),
            vec![(100, 200), (300, 400)],
            "classification needs all of them — the first gap may be padding",
        );
    }

    #[test]
    fn only_segment_closing_gaps_look_like_padding() {
        assert!(gap_is_padding_shaped(
            SEG_SIZE + 250,
            2 * SEG_SIZE,
            SEG_SIZE
        ));
        // Closes on a boundary, but two segments up.
        assert!(!gap_is_padding_shaped(
            SEG_SIZE + 250,
            3 * SEG_SIZE,
            SEG_SIZE
        ));
        assert!(!gap_is_padding_shaped(
            SEG_SIZE + 250,
            2 * SEG_SIZE - 8,
            SEG_SIZE
        ));
        assert!(!gap_is_padding_shaped(SEG_SIZE, 3 * SEG_SIZE, SEG_SIZE));
    }

    fn pgcfg() -> tokio_postgres::Config {
        let mut cfg = tokio_postgres::Config::new();
        cfg.host(&std::env::var("YB_TEST_HOST").unwrap_or_else(|_| "127.0.0.1".into()));
        cfg.port(
            std::env::var("YB_TEST_PORT")
                .ok()
                .and_then(|s| s.parse().ok())
                .unwrap_or(5433),
        );
        cfg.user(&std::env::var("YB_TEST_USER").unwrap_or_else(|_| "yugabyte".into()));
        cfg.dbname(&std::env::var("YB_TEST_DB").unwrap_or_else(|_| "yb_storage".into()));
        cfg.application_name("yb_wal_storage_test");
        cfg
    }

    /// Scratch-table name, stable per test: derived from the test's
    /// thread name (the harness names threads after the test path), so
    /// a rerun reuses — and `make_wals_table`'s DROP-IF-EXISTS heals —
    /// the same table instead of leaking a fresh one per run, while
    /// parallel tests still get distinct names.
    /// Wrap logical WAL bytes the way the writer does, for fixtures that
    /// `INSERT` into `wals` directly instead of going through
    /// [`flush_batch`]. Without this the read path rejects the row: an
    /// un-enveloped blob's first byte is data, not a codec tag.
    ///
    /// `chunk_size` in those fixtures stays the LOGICAL length (the length
    /// of what is passed here), which is the invariant the LSN arithmetic
    /// depends on.
    fn enc(plain: &[u8]) -> Vec<u8> {
        payload_codec::encode(plain, payload_codec::Codec::Lz4)
            .0
            .to_vec()
    }

    fn scratch_table(prefix: &str) -> String {
        use std::hash::{Hash, Hasher};
        let mut h = std::collections::hash_map::DefaultHasher::new();
        std::thread::current().name().unwrap_or("t").hash(&mut h);
        format!("{prefix}_{:08x}", h.finish() as u32)
    }

    /// The two classes the setup loop tolerates. Different mechanisms, all
    /// cleared by re-running the DROP-then-CREATE pair:
    ///
    /// - `deleted while still in use` (#1130), the failure this loop was added
    ///   for. YSQL raises it when a relcache rebuild finds no pg_class row
    ///   for an OID whose entry is still pinned, which is consistent with the
    ///   DROP's own invalidation landing mid-statement. Relcache entries go at
    ///   transaction end, so the next attempt starts clean.
    /// - `already exists`, two shapes. The live one is `42710 type "..."
    ///   already exists`, where a CREATE's pg_class and pg_type checks
    ///   disagree across a mid-statement catalog version change — YSQL's own
    ///   `Catalog Version Mismatch ... Try again.` context says to retry.
    ///   Open as yugabyte-db#24403, whose repro is this helper's two
    ///   statements; `IF NOT EXISTS` does not cover it, since the guard gates
    ///   only the pg_class check the CREATE already passed. The other is
    ///   #508's `42P07` from a *bare* CREATE that had committed, where
    ///   re-running the DROP is what converges it; #556's `IF NOT EXISTS`
    ///   fixed that shape.
    ///
    /// Advisory, not exhaustive: anything unrecognised is a real failure.
    fn is_scratch_table_catalog_race(msg: &str) -> bool {
        msg.contains("already exists") || msg.contains("deleted while still in use")
    }

    /// DROP-then-CREATE `table`, retrying [`is_scratch_table_catalog_race`].
    /// `create_sql` is re-run per attempt and must create `table`; the DROP is
    /// derived from the name, not from the SQL. This tolerates the two races,
    /// it does not prevent them: three attempts with a heartbeat-length wait
    /// has been enough on CI, and an exhausted budget panics rather than
    /// hiding one.
    ///
    /// Both statements have to survive a re-send. On a dropped connection
    /// `execute` repeats a statement without learning whether it committed
    /// (`compute/architecture/yugabyte.md`, the client retry contract). The
    /// CREATE survives that through `IF NOT EXISTS`, so keep the guard even
    /// under the unconditional DROP. Without it a re-send of a committed
    /// CREATE fails `42P07` permanently rather than transiently (#508/#556),
    /// and removing it stalled the suite to 183s against a normal 6.5s.
    ///
    /// The DROP does not survive one. It resolves its name to an OID when it
    /// runs, so a re-send drops whatever holds the name by then. Nothing
    /// creates these names concurrently: the controller's migrations create the
    /// `amp_storage` tables idempotently and never DROP them, neither daemon
    /// issues DDL, and each test here owns one name. `make_wals_table` below
    /// rules out the one case that would break that.
    ///
    /// Do not re-add the `DROP TYPE IF EXISTS ... CASCADE` from #1041. It was
    /// added on the theory that a replayed CREATE strands the rowtype in
    /// pg_type with no pg_class row to match. No such row can exist: the
    /// pg_type rows and the pg_class row go in one DDL transaction, so an
    /// abort past the rowtype leaves neither. (The DocDB table is *not* in
    /// that transaction. A separate master RPC creates it, and the master's
    /// DDL verifier reconciles it afterwards.)
    ///
    /// The `42710` it was reacting to is real but transient — a stale cached
    /// pg_type answer, not a stranded row (yugabyte-db#24403, above). The
    /// retry clears that; the cleanup would not, since it resolves the name
    /// from one syscache and deletes through another, raising `cache lookup
    /// failed for type` on the state it targeted. It also fails `2BP01` when
    /// the name is a live table.
    async fn recreate_scratch_table(client: &YsqlClient, table: &str, create_sql: &str) {
        let q = quote_relation(table);
        let drop_table = format!("DROP TABLE IF EXISTS {q}");
        // Labelled so the panic names the failing statement: `deleted while
        // still in use` means different things on the DROP and the CREATE.
        let steps = [
            ("drop scratch wals table", drop_table.as_str()),
            ("create scratch wals table", create_sql),
        ];
        const ATTEMPTS: u32 = 3;
        for attempt in 1..=ATTEMPTS {
            let mut failure = None;
            for (label, sql) in steps {
                if let Err(e) = client.execute(sql, &[]).await {
                    failure = Some((label, e));
                    break;
                }
            }
            let Some((label, e)) = failure else { return };
            assert!(
                attempt < ATTEMPTS && is_scratch_table_catalog_race(&e.to_string()),
                "{label} {table} (attempt {attempt} of {ATTEMPTS}): {e:#}"
            );
            // Plain backoff: `execute` classifies both Fatal (42xxx, and an
            // XX000 with no transient pattern), so it never backs off itself.
            // Sized to the default 1s `heartbeat_interval_ms`, since a catalog
            // version reaches tservers on that heartbeat and a shorter wait
            // re-reads the state that just failed. #1130 does not need the
            // wait; the DROP on the next attempt is what fixes both classes.
            tokio::time::sleep(Duration::from_secs(u64::from(attempt))).await;
        }
    }

    /// Per-test `wals`-shaped scratch table. Names are FIXED per test
    /// (not randomized): `recreate_scratch_table`'s DROP-IF-EXISTS self-heals
    /// any table a panicked/killed earlier run left behind, where random
    /// suffixes accumulated leaked tables until the dev YB's tablet count
    /// degraded every daemon start on the box. Trade-off: two
    /// simultaneous `cargo test -p safekeeper` invocations on one
    /// cluster would collide — don't do that.
    async fn make_wals_table(client: &YsqlClient, table: &str) {
        let q = quote_relation(table);
        // Same shape as the production schema (all-range key led by
        // `bucket_id`); the migration's `SPLIT AT VALUES` is omitted —
        // placement is not under test and default tablet counts keep setup
        // fast. Note that without the pre-split this is a single tablet, so it
        // does NOT exercise the cross-tablet paging the production table
        // needs; `thin_lane_range_sharded_covering_read_pages_past_an_empty_tablet`
        // splits explicitly for that.
        let create_sql = format!(
            "CREATE TABLE IF NOT EXISTS {q} (\
               bucket_id        SMALLINT NOT NULL,\
               branch_id        uuid     NOT NULL,\
               version          SMALLINT NOT NULL,\
               segno            BIGINT   NOT NULL,\
               chunk_offset     INTEGER  NOT NULL,\
               chunk_size       INT      NOT NULL,\
               data             BYTEA    NOT NULL,\
               last_modified_us BIGINT   NOT NULL,\
               PRIMARY KEY (bucket_id ASC, branch_id ASC, version ASC, segno ASC, chunk_offset ASC)\
             )"
        );
        recreate_scratch_table(client, table, &create_sql).await;
    }

    /// One `wals` row, encoded the way the write path stores it.
    async fn insert_chunk(
        client: &YsqlClient,
        table: &str,
        branch_id: BranchId,
        segno: i64,
        chunk_offset: i64,
        data: &[u8],
    ) {
        insert_chunk_in(client, table, branch_id, 0, segno, chunk_offset, data).await;
    }

    /// [`insert_chunk`] into a chosen bucket.
    async fn insert_chunk_in(
        client: &YsqlClient,
        table: &str,
        branch_id: BranchId,
        bucket: i16,
        segno: i64,
        chunk_offset: i64,
        data: &[u8],
    ) {
        client
            .execute(
                &format!(
                    "INSERT INTO {} (bucket_id, branch_id, version, segno, chunk_offset, \
                                     chunk_size, data, last_modified_us) \
                     VALUES ($1, $2, {DEFAULT_VERSION}, $3, $4, $5, $6, 0)",
                    quote_relation(table)
                ),
                &[
                    &bucket,
                    &branch_id,
                    &segno,
                    &(chunk_offset as i32),
                    &(data.len() as i32),
                    &enc(data),
                ],
            )
            .await
            .expect("insert WAL chunk");
    }

    /// Store `[from, to)` of `wal` — which starts at `wal_start` — as one row
    /// in `bucket`.
    #[allow(clippy::too_many_arguments)]
    async fn insert_span(
        client: &YsqlClient,
        table: &str,
        branch_id: BranchId,
        bucket: i16,
        wal: &[u8],
        wal_start: Lsn,
        from: Lsn,
        to: Lsn,
    ) {
        assert_eq!(
            from.0 / SEG_SIZE,
            (to.0 - 1) / SEG_SIZE,
            "a row never spans segments"
        );
        let data = &wal[(from.0 - wal_start.0) as usize..(to.0 - wal_start.0) as usize];
        let (segno, offset) = ((from.0 / SEG_SIZE) as i64, (from.0 % SEG_SIZE) as i64);
        insert_chunk_in(client, table, branch_id, bucket, segno, offset, data).await;
    }

    async fn drop_wals_table(client: &YsqlClient, table: &str) {
        let q = quote_relation(table);
        let _ = client
            .execute(&format!("DROP TABLE IF EXISTS {q}"), &[])
            .await;
    }

    /// Build a fresh `YbWalStorage` with its own per-test scratch table
    /// and tempdir. `start_lsn` becomes the rehydrated `accepted_byte_end` (the
    /// table is empty, so `load_max_offset` falls back to `commit_lsn`).
    ///
    /// Regression for the 2026-07 TPC-C safekeeper wedge. Every
    /// `wait_for_durable` caller sits inside `process_msg`, holding the
    /// timeline shared_state WRITE lock. When the durability target can
    /// outrun what the consumer will ever publish (YB stalled by a
    /// splitting `amp_storage.wals` tablet, walproposer churn re-entering
    /// the reconnect/elected handshake), an unbounded wait froze the lock forever:
    /// the manager blocked in `state_snapshot`, every WAL receiver died
    /// on residence-guard timeouts, `flush_lsn` froze, and all commits
    /// hung. The wait must instead time out — dropping the connection
    /// releases the lock and lets the system retry — and the storage
    /// must keep working afterwards.
    #[tokio::test]
    async fn durable_wait_times_out_instead_of_wedging() {
        let table = scratch_table("wals_test_wedge");
        let start = Lsn(SEG_SIZE);
        let (mut storage, _tmp) = make_storage(&table, start).await;
        storage.durable_wait_log_every = Duration::from_millis(50);
        storage.durable_wait_max = Duration::from_millis(300);

        // Wedge precondition: an accounted flush target that no enqueued
        // chunk will ever satisfy (consumer idle, queue empty — exactly
        // the task-dump state from the finding).
        storage.submitted_max_lsn = Lsn(start.0 + 12345);

        let res = tokio::time::timeout(Duration::from_secs(30), storage.flush_wal()).await;
        let err = res
            .expect("flush_wal must return (timeout), not wedge the caller forever")
            .expect_err("flush_wal must fail when durability can never be reached");
        assert!(
            err.to_string().contains("durability wait timed out"),
            "unexpected error: {err:#}"
        );

        // The storage recovers: the next producer write overwrites
        // `submitted_max_lsn` (assignment, not max), so a later flush waits
        // on a reachable target again. Simulate that overwrite directly —
        // generating valid WAL bytes is out of scope here — and verify the
        // flush completes instantly.
        storage.submitted_max_lsn = Lsn(storage.durable_lsn.load(Ordering::Acquire));
        tokio::time::timeout(Duration::from_secs(60), storage.flush_wal())
            .await
            .expect("recovered flush must not hang")
            .expect("recovered flush must succeed");
    }

    async fn make_storage(table: &str, start_lsn: Lsn) -> (YbWalStorage, Utf8TempDir) {
        let client = Arc::new(
            YsqlClient::connect_no_tls(pgcfg(), true)
                .await
                .expect("connect to local YB; set YB_TEST_HOST/PORT/USER/DB to override"),
        );
        make_wals_table(&client, table).await;

        let branch_id = BranchId::generate();
        let mut state = TimelinePersistentState::empty();
        state.branch_id = branch_id;
        state.timeline_start_lsn = start_lsn;

        let tmp = camino_tempfile::tempdir().expect("tempdir");
        let storage = YbWalStorage::new(
            branch_id,
            &state,
            state.timeline_start_lsn,
            wal_conn(&client, table).await,
        )
        .await
        .expect("YbWalStorage::new");
        (storage, tmp)
    }

    /// Open the hot-path lane against a per-test scratch `wals` table.
    /// `make_wals_table` creates it unqualified, so it lands in `public` —
    /// the production path looks up `amp_storage.wals` instead
    /// (`lookup_wals_oids`).
    /// A `TsClient` per caller, NOT one shared across the suite. `cargo test`
    /// runs these concurrently, and sharing one was tried and produced
    /// intermittent `segment_reader_*` failures.
    fn thin_client() -> Arc<yb_thin_client::TsClient> {
        let tserver =
            std::env::var("YB_TEST_TSERVER_RPC").unwrap_or_else(|_| "127.0.0.1:9100".into());
        // Reads the same `YB_TEST_SSL*` variables as the pageserver's
        // `test_yb_config`, but here the CA is what turns TLS on — the shim
        // has no sslmode, and no accept-all verifier to fall back to.
        let tls = std::env::var("YB_TEST_SSLROOTCERT").ok().map(|ca| {
            let client = std::env::var("YB_TEST_SSLCERT")
                .ok()
                .zip(std::env::var("YB_TEST_SSLKEY").ok());
            yb_thin_client::TlsOpts::from_paths(ca, client)
        });
        yb_thin_client::TsClient::connect(&yb_thin_client::TsClientConfig {
            tserver_addrs: vec![tserver],
            tls,
            ..Default::default()
        })
        .expect("connect thin lane to dev YB; set YB_TEST_TSERVER_RPC to override")
    }

    async fn thin_lane_for(client: &Arc<YsqlClient>, table: &str) -> Arc<YbThinPool> {
        let (db_oid, oids) = ysql_client::lookup_table_oids(client, "public", &[table])
            .await
            .expect("look up scratch wals table OIDs");
        let thin_client = thin_client();
        let wals = thin_client
            .open_table(db_oid, oids[table])
            .expect("open scratch wals table via the thin lane");
        // No lifetime bound: the test wants the handle it opened for as long
        // as it runs. `Recycling::fixed` never rebuilds.
        yb_thin_client::Recycling::fixed(Arc::new(YbThinClient {
            client: thin_client,
            wals,
        }))
    }

    /// One shared YSQL connection for the system lane plus the hot-path lane
    /// on `table`.
    async fn wal_conn_cached(
        client: &Arc<YsqlClient>,
        table: &str,
        cache: Option<Arc<std::sync::Mutex<WalCache>>>,
    ) -> Arc<YbWalConn> {
        Arc::new(YbWalConn {
            system_client: client.clone(),
            table: table.to_string(),
            cache,
            yb_thinclient: thin_lane_for(client, table).await,
            codec: payload_codec::Codec::Lz4,
        })
    }

    /// [`wal_conn_cached`] with the chunk cache off.
    async fn wal_conn(client: &Arc<YsqlClient>, table: &str) -> Arc<YbWalConn> {
        wal_conn_cached(client, table, None).await
    }

    const SEG_SIZE: u64 = 16 * 1024 * 1024;

    /// `contiguous_chunks_from` replaces one point query per chunk with one range query per
    /// segment, so it has to return the covering chunk for a mid-chunk `lsn`, then
    /// its successors ascending, and stop at the segment edge.
    #[tokio::test]
    async fn chunks_from_returns_a_contiguous_run_within_the_segment() {
        use postgres_ffi::wal_record_boundary::WalSource;

        let table = scratch_table("wals_test_chunks_from");
        let client = Arc::new(
            YsqlClient::connect_no_tls(pgcfg(), true)
                .await
                .expect("connect to local YB; set YB_TEST_HOST/PORT/USER/DB to override"),
        );
        make_wals_table(&client, &table).await;
        let branch_id = BranchId::generate();

        // Ten 1 KiB chunks in segno 1, then one in segno 2 the fetch must not reach.
        const N: usize = 10;
        const SZ: usize = 1024;
        for i in 0..N {
            let body = vec![i as u8; SZ];
            insert_chunk(&client, &table, branch_id, 1, (i * SZ) as i64, &body).await;
        }
        insert_chunk(&client, &table, branch_id, 2, 0, &[0xAA; SZ]).await;

        let src = YbWalSource {
            client: client.as_ref(),
            table: &table,
            branch_id,
            seg: SEG_SIZE,
            find_anchor_queries: AtomicU64::new(0),
            find_end_of_wal_queries: AtomicU64::new(0),
            read_failures: AtomicU64::new(0),
        };

        // Mid-chunk start: the covering chunk is the one that holds it, not the
        // next one up — a bare `chunk_offset >= lsn` would skip it.
        let mid = Lsn(SEG_SIZE + (3 * SZ + 500) as u64);
        let got = src
            .contiguous_chunks_from(mid)
            .await
            .expect("contiguous_chunks_from");
        assert_eq!(
            got.len(),
            N - 3,
            "must return chunk 3 through the segment end"
        );
        assert_eq!(got[0].0, Lsn(SEG_SIZE + (3 * SZ) as u64));
        for (i, (start, data)) in got.iter().enumerate() {
            assert_eq!(
                *start,
                Lsn(SEG_SIZE + ((i + 3) * SZ) as u64),
                "chunk {i} start"
            );
            assert_eq!(data.len(), SZ);
        }

        // Past the last chunk of the segment: empty, which carries `chunk_at`'s
        // `None` meaning. It must NOT reach into segno 2.
        let past = Lsn(SEG_SIZE + (N * SZ) as u64);
        assert!(
            src.contiguous_chunks_from(past)
                .await
                .expect("past")
                .is_empty()
        );

        // A gap mid-segment ends the run there rather than splicing across it:
        // the bytes after a hole are not contiguous with the ones before.
        let gapped = scratch_table("wals_test_chunks_from_gap");
        make_wals_table(&client, &gapped).await;
        let gap_branch = BranchId::generate();
        for i in [0usize, 1, 2, 4, 5] {
            let body = vec![i as u8; SZ];
            insert_chunk(&client, &gapped, gap_branch, 1, (i * SZ) as i64, &body).await;
        }
        let gap_src = YbWalSource {
            client: client.as_ref(),
            table: &gapped,
            branch_id: gap_branch,
            seg: SEG_SIZE,
            find_anchor_queries: AtomicU64::new(0),
            find_end_of_wal_queries: AtomicU64::new(0),
            read_failures: AtomicU64::new(0),
        };
        let run = gap_src
            .contiguous_chunks_from(Lsn(SEG_SIZE))
            .await
            .expect("contiguous_chunks_from across a gap");
        assert_eq!(
            run.len(),
            3,
            "the run must stop at the hole at offset {}, not resume at {}",
            3 * SZ,
            4 * SZ
        );
        drop_wals_table(&client, &gapped).await;

        drop_wals_table(&client, &table).await;
    }

    /// Regression for #678: `max_offset` is an exclusive end, so a page-aligned
    /// one must not be handed to the inclusive-target `find_anchor`.
    #[tokio::test]
    async fn rehydrate_truncates_page_aligned_torn_tail() {
        use postgres_ffi::XLOG_BLCKSZ;
        use postgres_ffi::v18::wal_generator::{LogicalMessageGenerator, WalGenerator};

        let table = scratch_table("wals_test_page_aligned_tail");
        let client = Arc::new(
            YsqlClient::connect_no_tls(pgcfg(), true)
                .await
                .expect("connect to local YB; set YB_TEST_HOST/PORT/USER/DB to override"),
        );
        make_wals_table(&client, &table).await;

        // Real WAL from the start of segment 1.
        let start = Lsn(SEG_SIZE);
        let mut g = WalGenerator::new(LogicalMessageGenerator::new(c"t", &[]), start);
        let mut buf: Vec<u8> = Vec::new();
        let mut starts: Vec<Lsn> = Vec::new();
        for _ in 0..400 {
            let (lsn, bytes) = g.append_logical_message(c"t", &[7u8; 16]);
            starts.push(lsn);
            buf.extend_from_slice(&bytes);
        }

        // Trim to a page boundary — page-aligned by construction, since `start`
        // is — landing mid-record, which is the torn tail #678 must truncate.
        let len = buf.len() / XLOG_BLCKSZ * XLOG_BLCKSZ;
        let cut = Lsn(start.0 + len as u64);
        let full_end = Lsn(start.0 + buf.len() as u64);
        assert!(
            !starts.contains(&cut) && cut != full_end,
            "test needs a mid-record cut"
        );

        let branch_id = BranchId::generate();
        insert_chunk(&client, &table, branch_id, 1, 0, &buf[..len]).await;

        let mut state = TimelinePersistentState::empty();
        state.branch_id = branch_id;
        state.timeline_start_lsn = start;

        let storage = YbWalStorage::new(
            branch_id,
            &state,
            state.timeline_start_lsn,
            wal_conn(&client, &table).await,
        )
        .await
        .expect("YbWalStorage::new");

        let got = storage.accepted_byte_end();
        assert!(
            got < cut,
            "rehydrated accepted_byte_end {got} must fall below the page-aligned torn end {cut}"
        );
        assert!(
            starts.contains(&got),
            "rehydrated accepted_byte_end {got} must be a real record boundary"
        );

        drop_wals_table(&client, &table).await;
    }

    /// Real WAL of `msg_len`-byte logical messages from `start` until the
    /// generator passes `until`: the bytes, every record start, and the end of
    /// the last record.
    fn gen_wal_until(start: Lsn, until: Lsn, msg_len: usize) -> (Vec<u8>, Vec<Lsn>, Lsn) {
        use postgres_ffi::v18::wal_generator::{LogicalMessageGenerator, WalGenerator};
        let mut g = WalGenerator::new(LogicalMessageGenerator::new(c"t", &[]), start);
        let (mut buf, mut starts) = (Vec::new(), Vec::new());
        while g.lsn < until {
            let (lsn, bytes) = g.append_logical_message(c"t", &vec![7u8; msg_len]);
            starts.push(lsn);
            buf.extend_from_slice(&bytes);
        }
        starts.push(g.lsn);
        (buf, starts, g.lsn)
    }

    fn state_starting_at(branch_id: BranchId, start: Lsn) -> TimelinePersistentState {
        let mut state = TimelinePersistentState::empty();
        state.branch_id = branch_id;
        state.timeline_start_lsn = start;
        state
    }

    async fn connect_test_yb() -> Arc<YsqlClient> {
        Arc::new(
            YsqlClient::connect_no_tls(pgcfg(), true)
                .await
                .expect("connect to local YB; set YB_TEST_HOST/PORT/USER/DB to override"),
        )
    }

    /// Loads `branch_id` from `table` with `floor` as the retention floor and
    /// checks it rehydrated to the last record boundary at or below `hole`, with
    /// every row past it deleted.
    #[allow(clippy::too_many_arguments)]
    async fn assert_rehydrates_below_hole(
        client: &Arc<YsqlClient>,
        table: &str,
        branch_id: BranchId,
        start: Lsn,
        floor: Lsn,
        starts: &[Lsn],
        hole: Lsn,
    ) {
        let storage = YbWalStorage::new(
            branch_id,
            &state_starting_at(branch_id, start),
            floor,
            wal_conn(client, table).await,
        )
        .await
        .expect("YbWalStorage::new");
        let got = storage.accepted_byte_end();
        assert!(
            got <= hole,
            "rehydrated end of WAL {got} must not pass the hole at {hole}: the pageserver \
             would stall on it"
        );
        assert!(
            starts.contains(&got),
            "rehydrated end of WAL {got} must be a real record boundary"
        );
        assert_eq!(Lsn(storage.durable_lsn.load(Ordering::Acquire)), got);
        drop(storage);

        let left =
            YbWalStorage::load_max_offset(client, table, branch_id, SEG_SIZE as usize, Lsn(0))
                .await
                .expect("load_max_offset");
        assert_eq!(
            left, got,
            "the rows past the hole must be deleted, or the next load counts them again"
        );
    }

    /// The shape a partly refused flush leaves: the batch in flight crossed a
    /// segment boundary, so it was two rows, and only the upper one landed. The
    /// gap closes exactly on the boundary, so it looks like `XLOG_SWITCH`
    /// padding, and only the decode tells it apart.
    #[tokio::test]
    async fn rehydration_cuts_a_batch_that_lost_its_lower_run_across_a_segment() {
        use postgres_ffi::XLOG_BLCKSZ;
        let blk = XLOG_BLCKSZ as u64;
        let table = scratch_table("wals_test_partial_batch_seg");
        let client = connect_test_yb().await;
        make_wals_table(&client, &table).await;

        let start = Lsn(2 * SEG_SIZE - 16 * blk);
        let boundary = Lsn(2 * SEG_SIZE);
        let (wal, starts, end) = gen_wal_until(start, Lsn(boundary.0 + 8 * blk), 300);
        let earlier = Lsn(start.0 + 5 * blk + 13);
        let hole = Lsn(boundary.0 - 3 * blk + 40);

        let branch_id = BranchId::generate();
        // Two complete earlier batches, then the top batch's upper run alone.
        insert_span(&client, &table, branch_id, 0, &wal, start, start, earlier).await;
        insert_span(&client, &table, branch_id, 1, &wal, start, earlier, hole).await;
        insert_span(&client, &table, branch_id, 2, &wal, start, boundary, end).await;

        assert_rehydrates_below_hole(&client, &table, branch_id, start, start, &starts, hole).await;
        drop_wals_table(&client, &table).await;
    }

    /// The same shape on an idle branch: the pageserver has caught up to the
    /// last batch that landed whole, so its `consistent_lsn` — the anchor floor
    /// — is the hole's lower edge. The decode can still answer there.
    #[tokio::test]
    async fn rehydration_cuts_a_hole_whose_lower_edge_is_the_floor() {
        use postgres_ffi::XLOG_BLCKSZ;
        let blk = XLOG_BLCKSZ as u64;
        let table = scratch_table("wals_test_hole_at_floor");
        let client = connect_test_yb().await;
        make_wals_table(&client, &table).await;

        let start = Lsn(2 * SEG_SIZE - 16 * blk);
        let boundary = Lsn(2 * SEG_SIZE);
        let (wal, starts, end) = gen_wal_until(start, Lsn(boundary.0 + 8 * blk), 300);
        // The earlier batches end on a record boundary, as an acked one does.
        let hole = *starts
            .iter()
            .rev()
            .find(|&&lsn| lsn < Lsn(boundary.0 - 2 * blk))
            .expect("a record boundary below the segment end");

        let branch_id = BranchId::generate();
        insert_span(&client, &table, branch_id, 0, &wal, start, start, hole).await;
        insert_span(&client, &table, branch_id, 1, &wal, start, boundary, end).await;

        assert_rehydrates_below_hole(&client, &table, branch_id, start, hole, &starts, hole).await;
        drop_wals_table(&client, &table).await;
    }

    /// A hole that doesn't close on a segment boundary is cut without a decode.
    #[tokio::test]
    async fn rehydration_cuts_a_hole_inside_a_segment() {
        use postgres_ffi::XLOG_BLCKSZ;
        let blk = XLOG_BLCKSZ as u64;
        let table = scratch_table("wals_test_hole_in_segment");
        let client = connect_test_yb().await;
        make_wals_table(&client, &table).await;

        let start = Lsn(SEG_SIZE);
        let (wal, starts, end) = gen_wal_until(start, Lsn(start.0 + 12 * blk), 100);
        let earlier = Lsn(start.0 + 3 * blk + 5);
        let hole = Lsn(start.0 + 6 * blk + 17);
        let resume = Lsn(start.0 + 8 * blk + 3);

        let branch_id = BranchId::generate();
        insert_span(&client, &table, branch_id, 0, &wal, start, start, earlier).await;
        insert_span(&client, &table, branch_id, 1, &wal, start, earlier, hole).await;
        insert_span(&client, &table, branch_id, 2, &wal, start, resume, end).await;

        assert_rehydrates_below_hole(&client, &table, branch_id, start, start, &starts, hole).await;
        drop_wals_table(&client, &table).await;
    }

    /// A hole with more than one batch above it is not what a cut-short flush
    /// leaves: a build without the hole scan loaded past the hole and acked the
    /// commits stored above it. The load must refuse rather than delete them.
    #[tokio::test]
    async fn rehydration_refuses_to_trim_more_than_one_batch_above_a_hole() {
        use postgres_ffi::XLOG_BLCKSZ;
        let blk = XLOG_BLCKSZ as u64;
        let table = scratch_table("wals_test_hole_acked_above");
        let client = connect_test_yb().await;
        make_wals_table(&client, &table).await;

        let start = Lsn(SEG_SIZE);
        let (wal, _, end) = gen_wal_until(start, Lsn(start.0 + 12 * blk), 100);
        let hole = Lsn(start.0 + 4 * blk + 17);
        let resume = Lsn(start.0 + 6 * blk + 3);
        let later = Lsn(start.0 + 9 * blk + 11);

        let branch_id = BranchId::generate();
        insert_span(&client, &table, branch_id, 0, &wal, start, start, hole).await;
        insert_span(&client, &table, branch_id, 1, &wal, start, resume, later).await;
        insert_span(&client, &table, branch_id, 2, &wal, start, later, end).await;

        let err = YbWalStorage::new(
            branch_id,
            &state_starting_at(branch_id, start),
            start,
            wal_conn(&client, &table).await,
        )
        .await
        .err()
        .expect("the load must refuse");
        assert!(
            format!("{err:#}").contains("they may hold acked WAL"),
            "unexpected error: {err:#}"
        );
        assert_eq!(
            YbWalStorage::load_max_offset(&client, &table, branch_id, SEG_SIZE as usize, Lsn(0))
                .await
                .expect("load_max_offset"),
            end,
            "nothing may be deleted"
        );
        drop_wals_table(&client, &table).await;
    }

    /// `pg_switch_wal()` with the padding never stored leaves a gap of the same
    /// shape as a lost lower run. The rows above it are acked WAL: the decode
    /// has to confirm the switch and the load must keep everything.
    #[tokio::test]
    async fn rehydration_walks_through_unstored_switch_padding() {
        use bytes::Bytes;
        use postgres_ffi::XLOG_BLCKSZ;
        use postgres_ffi::pg_constants::{RM_XLOG_ID, XLOG_SWITCH};
        use postgres_ffi::v18::wal_generator::{LogicalMessageGenerator, Record, WalGenerator};
        let blk = XLOG_BLCKSZ as u64;
        let table = scratch_table("wals_test_switch_padding");
        let client = connect_test_yb().await;
        make_wals_table(&client, &table).await;

        let start = Lsn(2 * SEG_SIZE - 8 * blk);
        let boundary = Lsn(2 * SEG_SIZE);
        let mut g = WalGenerator::new(LogicalMessageGenerator::new(c"t", &[]), start);
        let mut seg1 = Vec::new();
        for _ in 0..200 {
            let (_, bytes) = g.append_logical_message(c"t", &[7u8; 100]);
            seg1.extend_from_slice(&bytes);
        }
        let mut switch = WalGenerator::new(
            std::iter::once(Record {
                rmid: RM_XLOG_ID,
                info: XLOG_SWITCH,
                data: Bytes::new(),
            }),
            g.lsn,
        );
        switch.prev_lsn = g.prev_lsn;
        let (_, bytes) = switch.next().expect("the switch record");
        seg1.extend_from_slice(&bytes);
        let switch_end = Lsn(start.0 + seg1.len() as u64);
        let mid1 = Lsn(start.0 + 2 * blk + 9);
        assert!(mid1 < switch_end && switch_end < boundary);

        let (seg2, _, end) = gen_wal_until(boundary, Lsn(boundary.0 + 3 * blk), 100);
        let mid2 = Lsn(boundary.0 + blk + 21);

        let branch_id = BranchId::generate();
        insert_span(&client, &table, branch_id, 0, &seg1, start, start, mid1).await;
        insert_span(
            &client, &table, branch_id, 1, &seg1, start, mid1, switch_end,
        )
        .await;
        insert_span(
            &client, &table, branch_id, 2, &seg2, boundary, boundary, mid2,
        )
        .await;
        insert_span(&client, &table, branch_id, 3, &seg2, boundary, mid2, end).await;

        let storage = YbWalStorage::new(
            branch_id,
            &state_starting_at(branch_id, start),
            start,
            wal_conn(&client, &table).await,
        )
        .await
        .expect("YbWalStorage::new");
        assert_eq!(
            storage.accepted_byte_end(),
            end,
            "switch padding is not a hole; cutting there deletes the acked WAL above it"
        );
        drop(storage);
        assert_eq!(
            YbWalStorage::load_max_offset(&client, &table, branch_id, SEG_SIZE as usize, Lsn(0))
                .await
                .expect("load_max_offset"),
            end,
            "nothing may be deleted"
        );
        drop_wals_table(&client, &table).await;
    }

    /// [`YbWalStorage::contiguous_wal_end`] over `table`'s tail, with no anchor
    /// floor: `(raw max, contiguous end, tail rows read)`.
    async fn contiguous_end_via_yb(
        client: &YsqlClient,
        table: &str,
        branch_id: BranchId,
    ) -> (Lsn, Lsn, usize) {
        let tail =
            YbWalStorage::load_tail(client, table, branch_id, SEG_SIZE as usize, Lsn(0), Lsn(0))
                .await
                .expect("load_tail");
        let src = YbWalSource {
            client,
            table,
            branch_id,
            seg: SEG_SIZE,
            find_anchor_queries: AtomicU64::new(0),
            find_end_of_wal_queries: AtomicU64::new(0),
            read_failures: AtomicU64::new(0),
        };
        let end = YbWalStorage::contiguous_wal_end(
            &src,
            &tail.spans(),
            tail.end,
            PgMajorVersion::PG18,
            Lsn(0),
        )
        .await
        .expect("contiguous_wal_end");
        (tail.end, end, tail.rows.len())
    }

    /// A padding-shaped gap the decode can't confirm either way — synthetic
    /// bytes here — is walked through: continuing past a real hole fails loudly
    /// on the next read, while stopping at real padding destroys acked WAL.
    #[tokio::test]
    async fn an_undecodable_padding_shaped_gap_is_never_read_as_a_hole() {
        let table = scratch_table("wals_test_padding_gap");
        let client = connect_test_yb().await;
        make_wals_table(&client, &table).await;
        let branch_id = BranchId::generate();

        let blob = [0x33u8; 8192];
        insert_chunk_in(&client, &table, branch_id, 0, 1, 0, &blob).await;
        insert_chunk_in(&client, &table, branch_id, 1, 2, 0, &blob).await;

        let (raw_max, end, _) = contiguous_end_via_yb(&client, &table, branch_id).await;
        assert_eq!(raw_max, Lsn(2 * SEG_SIZE + 8192));
        assert_eq!(end, raw_max);
        drop_wals_table(&client, &table).await;
    }

    /// A failed read while confirming a padding-shaped gap is not ambiguity: it
    /// says nothing about the gap (#756). Walking through on it would report
    /// flush across a real hole, so the scan fails and the load is retried.
    #[tokio::test]
    async fn a_failed_read_while_confirming_padding_fails_the_scan() {
        use postgres_ffi::XLOG_BLCKSZ;
        let blk = XLOG_BLCKSZ as u64;
        let table = scratch_table("wals_test_probe_read_fail");
        let client = connect_test_yb().await;
        make_wals_table(&client, &table).await;

        let start = Lsn(2 * SEG_SIZE - 16 * blk);
        let boundary = Lsn(2 * SEG_SIZE);
        let (wal, _, end) = gen_wal_until(start, Lsn(boundary.0 + 8 * blk), 300);
        let earlier = Lsn(start.0 + 5 * blk + 13);
        let hole = Lsn(boundary.0 - 3 * blk + 40);

        let branch_id = BranchId::generate();
        insert_span(&client, &table, branch_id, 0, &wal, start, start, earlier).await;
        // The row the confirming decode reads first, under a codec tag this
        // build doesn't know: durable, but every read of it fails.
        let mut undecodable = vec![0x7fu8];
        undecodable
            .extend_from_slice(&wal[(earlier.0 - start.0) as usize..(hole.0 - start.0) as usize]);
        client
            .execute(
                &format!(
                    "INSERT INTO {} (bucket_id, branch_id, version, segno, chunk_offset, \
                                     chunk_size, data, last_modified_us) \
                     VALUES (1, $1, {DEFAULT_VERSION}, $2, $3, $4, $5, 0)",
                    quote_relation(&table)
                ),
                &[
                    &branch_id,
                    &((earlier.0 / SEG_SIZE) as i64),
                    &((earlier.0 % SEG_SIZE) as i32),
                    &((hole.0 - earlier.0) as i32),
                    &undecodable,
                ],
            )
            .await
            .expect("insert undecodable WAL chunk");
        insert_span(&client, &table, branch_id, 2, &wal, start, boundary, end).await;

        let tail = YbWalStorage::load_tail(
            &client,
            &table,
            branch_id,
            SEG_SIZE as usize,
            Lsn(0),
            Lsn(0),
        )
        .await
        .expect("load_tail");
        let src = YbWalSource {
            client: &client,
            table: &table,
            branch_id,
            seg: SEG_SIZE,
            find_anchor_queries: AtomicU64::new(0),
            find_end_of_wal_queries: AtomicU64::new(0),
            read_failures: AtomicU64::new(0),
        };
        let err = YbWalStorage::contiguous_wal_end(
            &src,
            &tail.spans(),
            tail.end,
            PgMajorVersion::PG18,
            start,
        )
        .await
        .expect_err("a failed read must not be walked through as padding");
        assert!(
            format!("{err:#}").contains("hole scan: a read failed"),
            "unexpected error: {err:#}"
        );

        YbWalStorage::new(
            branch_id,
            &state_starting_at(branch_id, start),
            start,
            wal_conn(&client, &table).await,
        )
        .await
        .err()
        .expect("the load must fail rather than guess");
        assert_eq!(
            YbWalStorage::load_max_offset(&client, &table, branch_id, SEG_SIZE as usize, Lsn(0))
                .await
                .expect("load_max_offset"),
            end,
            "nothing may be deleted"
        );
        drop_wals_table(&client, &table).await;
    }

    /// The tail probe stops at the segment before the floor's, the oldest GC
    /// keeps, so a bucket with few rows above it doesn't scan on through the
    /// deleted span below.
    #[tokio::test]
    async fn the_tail_probe_stops_at_the_oldest_segment_gc_keeps() {
        let table = scratch_table("wals_test_tail_floor");
        let client = connect_test_yb().await;
        make_wals_table(&client, &table).await;
        let branch_id = BranchId::generate();

        let blob = [0x55u8; 64];
        insert_chunk_in(&client, &table, branch_id, 0, 1, 0, &blob).await;
        insert_chunk_in(&client, &table, branch_id, 0, 5, 0, &blob).await;
        insert_chunk_in(&client, &table, branch_id, 1, 6, 0, &blob).await;

        let floor = Lsn(6 * SEG_SIZE + 32);
        let tail =
            YbWalStorage::load_tail(&client, &table, branch_id, SEG_SIZE as usize, Lsn(0), floor)
                .await
                .expect("load_tail");
        assert_eq!(
            tail.spans(),
            vec![
                (5 * SEG_SIZE, 5 * SEG_SIZE + 64),
                (6 * SEG_SIZE, 6 * SEG_SIZE + 64)
            ],
            "segno 1 is below the oldest kept segment (5) and must not be read"
        );
        assert_eq!(tail.end, Lsn(6 * SEG_SIZE + 64));
        drop_wals_table(&client, &table).await;
    }

    /// The per-bucket probes against a real table, with one bucket gone quiet
    /// far below the top: the tail must be cut to the global top rows, or the
    /// rows between that bucket's and the others' read as a hole.
    #[tokio::test]
    async fn the_tail_probe_is_cut_to_the_global_top_rows() {
        let table = scratch_table("wals_test_tail_probe");
        let client = connect_test_yb().await;
        make_wals_table(&client, &table).await;
        let branch_id = BranchId::generate();

        const ROWS: i64 = 600;
        const QUIET_ROWS: i64 = 20;
        const SZ: i64 = 64;
        client
            .execute(
                &format!(
                    "INSERT INTO {} (bucket_id, branch_id, version, segno, chunk_offset, \
                                     chunk_size, data, last_modified_us) \
                     SELECT CASE WHEN g < {QUIET_ROWS} THEN 5 ELSE g % 5 END, $1, \
                            {DEFAULT_VERSION}, 1, g * {SZ}, {SZ}, $2, 0 \
                     FROM generate_series(0, {}) g",
                    quote_relation(&table),
                    ROWS - 1
                ),
                &[&branch_id, &enc(&[0x44u8; SZ as usize])],
            )
            .await
            .expect("insert rows");

        let (raw_max, end, rows) = contiguous_end_via_yb(&client, &table, branch_id).await;
        assert_eq!(raw_max, Lsn(SEG_SIZE + (ROWS * SZ) as u64));
        assert_eq!(rows, TAIL_SCAN_ROWS);
        assert_eq!(end, raw_max, "a clean tail has no hole");
        drop_wals_table(&client, &table).await;
    }

    /// #788: once GC has run, `wals` begins far above `timeline_start_lsn`, so
    /// anchoring the walk there descends into dropped segments; the hole errors,
    /// and since #756 that fails the load outright — the branch never comes up
    /// while its WAL sits durable (`runbooks/startup-failures.md` §P).
    ///
    /// Two large records back to back make every page in the retained window a
    /// continuation, so the walk finds no anchor above the hole and can only
    /// stop on the floor.
    #[tokio::test]
    async fn rehydrate_survives_wal_trimmed_below_timeline_start() {
        use postgres_ffi::XLOG_BLCKSZ;
        use postgres_ffi::v18::wal_generator::{LogicalMessageGenerator, WalGenerator};
        use postgres_ffi::v18::xlog_utils::XLP_FIRST_IS_CONTRECORD;

        let table = scratch_table("wals_test_trimmed_below_start");
        let client = Arc::new(
            YsqlClient::connect_no_tls(pgcfg(), true)
                .await
                .expect("connect to local YB; set YB_TEST_HOST/PORT/USER/DB to override"),
        );
        make_wals_table(&client, &table).await;

        // GC kept segno 2 and dropped everything below it.
        let retained_start = Lsn(2 * SEG_SIZE);

        // Start mid-page in segno 1 so the first record spans the segment
        // boundary — that is what makes segno 2's opening page a continuation
        // rather than an anchor.
        let gen_start = Lsn(retained_start.0 - XLOG_BLCKSZ as u64 / 2);
        let mut g = WalGenerator::new(LogicalMessageGenerator::new(c"t", &[]), gen_start);
        let mut buf: Vec<u8> = Vec::new();

        let (_, bytes) = g.append_logical_message(c"t", &vec![7u8; 100 * 1024]);
        buf.extend_from_slice(&bytes);
        let (record_b, bytes) = g.append_logical_message(c"t", &vec![7u8; 100 * 1024]);
        buf.extend_from_slice(&bytes);
        let end = Lsn(gen_start.0 + buf.len() as u64);

        // Everything below `retained_start` is gone; keep only what survives.
        let kept = buf.split_off((retained_start.0 - gen_start.0) as usize);
        assert_eq!(
            Lsn(retained_start.0 + kept.len() as u64),
            end,
            "the retained slice must end where the last record does"
        );
        assert!(
            record_b > retained_start && record_b < end,
            "record_b {record_b} must be a boundary strictly inside the retained window"
        );

        // The premise: no page in the retained window starts a fresh record, so
        // the walk cannot stop above the hole on its own.
        for page_off in (0..kept.len()).step_by(XLOG_BLCKSZ) {
            let xlp_info = u16::from_le_bytes([kept[page_off + 2], kept[page_off + 3]]);
            assert!(
                xlp_info & XLP_FIRST_IS_CONTRECORD != 0,
                "page at {} starts a fresh record — the walk would anchor there and never \
                 reach the trimmed segment this test is about",
                Lsn(retained_start.0 + page_off as u64)
            );
        }

        let branch_id = BranchId::generate();
        insert_chunk(&client, &table, branch_id, 2, 0, &kept).await;

        let mut state = TimelinePersistentState::empty();
        state.branch_id = branch_id;
        // Where the *branch* starts — two segments below where its WAL now does.
        state.timeline_start_lsn = Lsn(SEG_SIZE);

        // What the pageserver's `consistent_lsn` would hold: a boundary inside
        // the surviving window.
        let storage =
            YbWalStorage::new(branch_id, &state, record_b, wal_conn(&client, &table).await)
                .await
                .expect("a branch whose WAL was trimmed by GC must still load");

        assert_eq!(
            storage.accepted_byte_end(),
            end,
            "rehydrated accepted_byte_end must reach the true end of the retained WAL; anything \
             short of it gets DELETEd by the torn-tail cleanup"
        );
        drop(storage);

        // A pageserver caught all the way up leaves `consistent_lsn == max_offset`
        // — the common restart. The floor is then the end itself, which the scan
        // must accept rather than treat as out of range and fall back from.
        let caught_up = YbWalStorage::new(branch_id, &state, end, wal_conn(&client, &table).await)
            .await
            .expect("a floor at the end of the WAL is the caught-up case, not a disagreement");
        assert_eq!(caught_up.accepted_byte_end(), end);

        drop_wals_table(&client, &table).await;
    }

    /// #756: a failed *read* during the end-of-WAL scan is indistinguishable
    /// from the WAL having ended, so the scan reports a short boundary and the
    /// rehydration DELETEs every `wals` row above it. Those rows are the only
    /// copy of WAL whose commits were already acked to the client — `wals` is
    /// keyed without a safekeeper id, so no peer can restore them.
    ///
    /// The injected fault is a durable chunk this build cannot decode (one of
    /// `chunk_at`'s error paths). It has to begin inside the last WAL page, past
    /// that page's header, which `find_anchor` is the only reader of: an anchor
    /// probe that failed would abort the scan before the forward walk, leaving
    /// the arm under test unexercised. The `anchored` assertion below enforces it.
    #[tokio::test]
    async fn failed_scan_read_must_not_delete_durable_wal() {
        use postgres_ffi::XLOG_BLCKSZ;
        use postgres_ffi::v18::wal_generator::{LogicalMessageGenerator, WalGenerator};
        use postgres_ffi::wal_record_boundary::{find_anchor, page_header_size};

        let table = scratch_table("wals_test_scan_read_fail");
        let client = Arc::new(
            YsqlClient::connect_no_tls(pgcfg(), true)
                .await
                .expect("connect to local YB; set YB_TEST_HOST/PORT/USER/DB to override"),
        );
        make_wals_table(&client, &table).await;

        // A clean WAL ending exactly on a record boundary: the state after the
        // last commit was acked, with every byte durable in `wals`.
        let start = Lsn(SEG_SIZE);
        let mut g = WalGenerator::new(LogicalMessageGenerator::new(c"t", &[]), start);
        let mut buf: Vec<u8> = Vec::new();
        let mut starts: Vec<Lsn> = Vec::new();
        for _ in 0..400 {
            let (lsn, bytes) = g.append_logical_message(c"t", &[7u8; 16]);
            starts.push(lsn);
            buf.extend_from_slice(&bytes);
        }
        let durable_end = *starts.last().expect("generated records");
        buf.truncate((durable_end.0 - start.0) as usize);

        let page = Lsn((durable_end.0 - 1) - (durable_end.0 - 1) % XLOG_BLCKSZ as u64);
        let hdr = page_header_size(page, SEG_SIZE) as u64;
        assert!(
            page > start && durable_end.0 > page.0 + hdr + 8,
            "geometry: the last page {page} leaves no room for a split below {durable_end}"
        );
        let split = Lsn(page.0 + hdr + (durable_end.0 - page.0 - hdr) / 2);

        let branch_id = BranchId::generate();
        let readable = (split.0 - start.0) as usize;
        let ins = format!(
            "INSERT INTO {} (bucket_id, branch_id, version, segno, chunk_offset, \
                             chunk_size, data, last_modified_us) \
             VALUES (0, $1, {DEFAULT_VERSION}, 1, $2, $3, $4, 0)",
            quote_relation(&table)
        );
        client
            .execute(
                &ins,
                &[
                    &branch_id,
                    &0i32,
                    &(readable as i32),
                    &enc(&buf[..readable]),
                ],
            )
            .await
            .expect("insert readable WAL chunk");
        // The same WAL bytes under a codec tag this build doesn't know: the row
        // is durable and its declared range still counts toward the end-of-WAL,
        // only reading it back fails.
        let mut undecodable = vec![0x7fu8];
        undecodable.extend_from_slice(&buf[readable..]);
        client
            .execute(
                &ins,
                &[
                    &branch_id,
                    &(readable as i32),
                    &((buf.len() - readable) as i32),
                    &undecodable,
                ],
            )
            .await
            .expect("insert undecodable WAL chunk");

        let mut state = TimelinePersistentState::empty();
        state.branch_id = branch_id;
        state.timeline_start_lsn = start;
        state.server.wal_seg_size = SEG_SIZE as u32;

        let anchored = find_anchor(
            &YbWalSource {
                client: client.as_ref(),
                table: &table,
                branch_id,
                seg: SEG_SIZE,
                find_anchor_queries: AtomicU64::new(0),
                find_end_of_wal_queries: AtomicU64::new(0),
                read_failures: AtomicU64::new(0),
            },
            Lsn(durable_end.0 - 1),
            start,
        )
        .await;

        let res = YbWalStorage::new(
            branch_id,
            &state,
            state.timeline_start_lsn,
            wal_conn(&client, &table).await,
        )
        .await;
        let accepted_byte_end = res.as_ref().ok().map(|s| s.accepted_byte_end());
        let failure = res.as_ref().err().map(|e| format!("{e:#}"));

        let persisted_end: Option<i64> = client
            .query_opt(
                &format!(
                    "SELECT max(segno * {} + chunk_offset + chunk_size)::BIGINT FROM {} \
                     WHERE branch_id = $1",
                    SEG_SIZE,
                    quote_relation(&table)
                ),
                &[&branch_id],
            )
            .await
            .expect("read back the persisted end-of-WAL")
            .and_then(|row| row.get::<_, Option<i64>>(0));

        drop_wals_table(&client, &table).await;

        anchored.expect("anchor probe must be served by the readable chunk below the split");
        // Whichever way the scan resolves — refusing to trust the failed read
        // (startup retries) or trusting a boundary at the durable end — the
        // acked WAL must still be there.
        assert_eq!(
            persisted_end.map(|e| Lsn(e as u64)),
            Some(durable_end),
            "durable WAL was DELETEd: `wals` now ends at {:?}, below the acked end \
             {durable_end}; the scan stopped at {accepted_byte_end:?}",
            persisted_end.map(|e| Lsn(e as u64)),
        );
        if let Some(got) = accepted_byte_end {
            assert_eq!(
                got, durable_end,
                "the advertised end-of-WAL regressed below the acked end {durable_end}"
            );
        }
        // The fault is permanent — no implementation can decode a tag it doesn't
        // know — so reaching `durable_end` is impossible and refusing the load is
        // the only correct outcome. Requiring the failure, and pinning it to this
        // chunk, is what stops the assertions above from passing on a run where
        // the scan was never exercised at all.
        let chain = failure.expect("the injected undecodable chunk must fail the scan");
        assert!(
            chain.contains("unknown payload codec tag"),
            "`new` failed, but not on the injected undecodable chunk: {chain}"
        );
    }

    /// Like [`make_storage`] but with a populated `WalCache` and a real
    /// `wal_seg_size`, so tests can assert how `truncate_wal` interacts
    /// with the in-memory cache.
    async fn make_storage_with_cache(table: &str, start_lsn: Lsn) -> (YbWalStorage, Utf8TempDir) {
        let client = Arc::new(
            YsqlClient::connect_no_tls(pgcfg(), true)
                .await
                .expect("connect to local YB; set YB_TEST_HOST/PORT/USER/DB to override"),
        );
        make_wals_table(&client, table).await;

        let branch_id = BranchId::generate();
        let mut state = TimelinePersistentState::empty();
        state.branch_id = branch_id;
        state.timeline_start_lsn = start_lsn;
        // truncate_wal decomposes end_pos with this, so it must be real.
        state.server.wal_seg_size = SEG_SIZE as u32;

        let tmp = camino_tempfile::tempdir().expect("tempdir");
        let yb_wal = wal_conn_cached(
            &client,
            table,
            Some(Arc::new(std::sync::Mutex::new(WalCache::new(
                64 * 1024 * 1024,
            )))),
        )
        .await;
        let storage = YbWalStorage::new(branch_id, &state, state.timeline_start_lsn, yb_wal)
            .await
            .expect("YbWalStorage::new");
        (storage, tmp)
    }

    /// Regression for the `going_away` torn-tail decode panic: `truncate_wal`
    /// must invalidate cached chunks at/above `end_pos`. Otherwise the
    /// in-memory `WalCache` keeps the pre-truncation (now-torn) bytes for
    /// that region, and `YbSegmentReader` serves them to the interpreted-WAL
    /// reader after the next walproposer term rewrites WAL forward — which
    /// decodes a stale page (`invalid xlog page header`) and trips
    /// `critical_timeline!`. Mirrors the cache drop `remove_up_to` does for
    /// its DELETE.
    #[tokio::test]
    async fn truncate_wal_invalidates_cache_past_end_pos() {
        let table = scratch_table("wals_test_trunc_cache");
        // start == durable tail; segno 1, offset 0. truncate back to it is
        // the "ProposerElected truncates to the durable tail" case.
        let start = Lsn(SEG_SIZE);
        let (mut storage, _tmp) = make_storage_with_cache(&table, start).await;

        // Seed the cache: one chunk below the cut (must survive) and two
        // past it — a later offset in the cut's segment and a higher
        // segment — standing in for the stale pre-truncation bytes a reader
        // must never serve.
        {
            let cache = storage.cache.as_ref().expect("cache enabled").clone();
            let mut c = cache.lock().unwrap();
            c.insert(0, 0, Arc::new(vec![1u8; 8192])); // below cut  → keep
            c.insert(1, 8192, Arc::new(vec![2u8; 8192])); // past cut   → drop
            c.insert(2, 0, Arc::new(vec![3u8; 8192])); // higher seg → drop
        }

        storage.truncate_wal(start).await.expect("truncate_wal");

        {
            let cache = storage.cache.as_ref().unwrap().clone();
            let mut c = cache.lock().unwrap();
            assert!(c.lookup(0, 0).is_some(), "chunk below the cut must survive");
            assert!(
                c.lookup(1, 8192).is_none(),
                "stale chunk past the cut must be invalidated"
            );
            assert!(
                c.lookup(2, 0).is_none(),
                "stale chunk in a higher segment must be invalidated"
            );
        }

        drop_wals_table(&storage.ysql_system_client, &table).await;
    }

    /// Regression: when the consumer's confirmed `durable_lsn` is at X
    /// and a caller asks to truncate to Y > X, `truncate_wal` must bail
    /// rather than silently advance the record boundary past YB
    /// (which is what created the 2026-05-29 forensic gap — see the
    /// `truncate_wal` comment).
    ///
    /// Construction gives us a valid bug-exhibiting state without
    /// needing to push real WAL bytes: an empty `wals` falls back to
    /// `state.commit_lsn` for `accepted_byte_end`, the watermark starts at
    /// `Healthy(accepted_byte_end)`, and `durable_lsn` is initialised from the
    /// same value. So `submitted_max_lsn == durable_lsn == accepted_byte_end ==
    /// start_lsn` at t=0. We then ask to truncate to `start_lsn + 4 KiB`
    /// — exactly the "ProposerElected with start_streaming_at past the
    /// persisted tail" pattern. With Fix 2 we bail; without it,
    /// the reset in `truncate_wal` advances `record_end` past `durable_lsn`
    /// and opens the gap.
    #[tokio::test]
    async fn truncate_wal_refuses_end_pos_past_durable() {
        let table = scratch_table("wals_test_truncate");
        // Start past the first-segment quick-exit edge case in `truncate_wal`
        // (it special-cases a zero boundary).
        let start = Lsn(SEG_SIZE);
        let (mut storage, _tmp) = make_storage(&table, start).await;

        assert_eq!(
            Lsn(storage.durable_lsn.load(Ordering::Acquire)),
            start,
            "durable_lsn seeded from load_max_offset at construction"
        );
        assert_eq!(storage.flush_lsn(), start);

        let bogus_end = Lsn(start.0 + 4096);
        let err = storage
            .truncate_wal(bogus_end)
            .await
            .expect_err("truncate_wal past durable should bail");
        let msg = format!("{err:#}");
        assert!(
            msg.contains("past confirmed durable LSN"),
            "expected bail message, got: {msg}"
        );

        assert_eq!(
            storage.flush_lsn(),
            start,
            "flush_lsn must not have advanced after the refused truncate"
        );

        // `truncate_wal` at exactly `durable_lsn` is a permitted no-op
        // (matches the boundary case where the proposer truncates back
        // to the durable tail).
        storage
            .truncate_wal(start)
            .await
            .expect("truncate_wal at durable should succeed");

        drop_wals_table(&storage.ysql_system_client, &table).await;
    }

    /// `flush_lsn()` returns `min(boundary, durable)` — defence-in-depth
    /// in case some future path moves the record boundary ahead of
    /// YB independent of `truncate_wal`. Poke `durable_lsn` directly to
    /// simulate "consumer is lagging" without needing to keep real
    /// chunks in flight.
    #[tokio::test]
    async fn flush_lsn_caps_at_durable() {
        let table = scratch_table("wals_test_cap");
        let start = Lsn(SEG_SIZE);
        let (storage, _tmp) = make_storage(&table, start).await;

        assert_eq!(storage.flush_lsn(), start);

        let phony_durable = Lsn(start.0 - 1024);
        storage
            .durable_lsn
            .store(phony_durable.0, Ordering::Release);
        assert_eq!(
            storage.flush_lsn(),
            phony_durable,
            "flush_lsn must cap at durable_lsn when record_end is ahead"
        );

        // Restore so the consumer's shutdown path doesn't observe a
        // permanently-regressed watermark.
        storage.durable_lsn.store(start.0, Ordering::Release);

        drop_wals_table(&storage.ysql_system_client, &table).await;
    }

    /// #1626: `truncate_wal` must lower `durable_lsn` to the cut. Left at the old
    /// end, WAL re-streamed below it is acked as soon as it is decoded, before
    /// the consumer INSERTs it. A fresh timeline is the widest window: the init
    /// segment makes `durable_lsn` the segment end, and `handle_elected` then
    /// truncates to `start_streaming_at`. Seeded at 0, as a fresh timeline is:
    /// `handle_elected` initializes the segment only when `flush_lsn()` is 0.
    ///
    /// The consumer cannot run between `write_wal` and the first assert: a
    /// current-thread runtime and a send with free capacity never yield (see
    /// `consumer_coalesces_contiguous_chunks_byte_equivalent`).
    #[tokio::test]
    async fn truncate_wal_lowers_durable_lsn_to_the_cut() {
        use postgres_ffi::v18::wal_generator::{LogicalMessageGenerator, WalGenerator};

        let table = scratch_table("wals_test_trunc_durable");
        let start = Lsn(SEG_SIZE);
        let (mut storage, _tmp) = make_storage(&table, Lsn(0)).await;
        assert_eq!(storage.flush_lsn(), Lsn(0));

        storage
            .initialize_first_segment(start)
            .await
            .expect("initialize_first_segment");
        assert_eq!(
            Lsn(storage.durable_lsn.load(Ordering::Acquire)),
            Lsn(2 * SEG_SIZE),
            "the init segment is durable to its end"
        );

        storage.truncate_wal(start).await.expect("truncate_wal");
        assert_eq!(
            Lsn(storage.durable_lsn.load(Ordering::Acquire)),
            start,
            "durable_lsn must follow the cut"
        );

        let mut g = WalGenerator::new(LogicalMessageGenerator::new(c"t", &[]), start);
        let mut buf: Vec<u8> = Vec::new();
        for _ in 0..16 {
            let (_, bytes) = g.append_logical_message(c"t", &[7u8; 16]);
            buf.extend_from_slice(&bytes);
        }
        let end = Lsn(start.0 + buf.len() as u64);
        storage.write_wal(start, &buf).await.expect("write_wal");
        assert_eq!(
            storage.flush_lsn(),
            start,
            "flush_lsn must not ack WAL the consumer has not INSERTed"
        );

        storage.flush_wal().await.expect("flush_wal");
        let record_end = storage.accepted_stream.record_end();
        assert!(record_end > start && record_end <= end);
        assert_eq!(
            storage.flush_lsn(),
            record_end,
            "acked once the batch lands"
        );

        drop_wals_table(&storage.ysql_system_client, &table).await;
    }

    /// A dedicated write connection for seeding scratch rows, plus the
    /// system lane `YbWalConn::from_conf` opens. The seeding connection keeps
    /// `yb_enable_upsert_mode` on so a replayed same-offset seed is
    /// idempotent, matching how the WAL hot path writes its own rows.
    async fn open_lanes() -> (Arc<YsqlClient>, Arc<YsqlClient>) {
        let writer = Arc::new(
            YsqlClient::connect_no_tls(pgcfg(), /* set_fast_path_upsert= */ true)
                .await
                .expect("connect seeding lane"),
        );
        let system = Arc::new(
            YsqlClient::connect_no_tls(pgcfg(), false)
                .await
                .expect("connect system lane"),
        );
        (writer, system)
    }

    /// `load_max_offset` runs on `system_client` at SK startup, but the rows
    /// it scans were written by the previous SK instance, on a different
    /// connection. Regression guard: a SELECT on a freshly-opened
    /// system_client must see those committed writes — anything else would
    /// let the safekeeper rehydrate an `accepted_byte_end` below YB's actual
    /// durable tail and silently regress flush_lsn on restart.
    #[tokio::test]
    async fn load_max_offset_via_system_client_sees_committed_writes() {
        let table = scratch_table("wals_test_lane_view");
        let (fpu, system) = open_lanes().await;
        make_wals_table(&fpu, &table).await;

        let branch_id = BranchId::generate();
        // Two contiguous chunks in segno 0, spread across two buckets
        // (this test is about cross-lane visibility; the per-bucket
        // max-end probe must take the max across both). End-offset is
        // 1024 + 256 = 1280.
        let rows: &[(i64, i32, i32)] = &[(0, 0, 1024), (0, 1024, 256)];
        let now_us: i64 = 0;
        let blob_small = vec![0xabu8; 1024];
        let blob_tail = vec![0xcdu8; 256];
        for (i, (segno, offset, size)) in rows.iter().enumerate() {
            let bucket = (i % 10) as i16;
            let blob: &[u8] = if *size as usize == blob_small.len() {
                &blob_small
            } else {
                &blob_tail
            };
            fpu.execute(
                &format!(
                    "INSERT INTO {} (bucket_id, branch_id, version, segno, chunk_offset, \
                                     chunk_size, data, last_modified_us) \
                     VALUES ($2, $1, {DEFAULT_VERSION}, $3, $4, $5, $6, $7)",
                    quote_relation(&table)
                ),
                &[
                    &branch_id,
                    &bucket,
                    segno,
                    offset,
                    size,
                    &enc(blob),
                    &now_us,
                ],
            )
            .await
            .expect("INSERT via fpu");
        }

        // Read via system_client — the lane that does the real
        // load_max_offset at SK startup.
        let lsn =
            YbWalStorage::load_max_offset(&system, &table, branch_id, SEG_SIZE as usize, Lsn(0))
                .await
                .expect("load_max_offset via system_client");
        let expected = 1024u64 + 256u64;
        assert_eq!(
            lsn,
            Lsn(expected),
            "system_client SELECT must reflect another connection's committed \
             INSERTs immediately"
        );

        drop_wals_table(&fpu, &table).await;
    }

    /// The DELETE bound is unset at construction and learned by the first
    /// removal, which is the whole point: resolving it means a `min(segno)` seek
    /// across the span GC has emptied, and on the startup path that cost took
    /// branches down (#874). Live, because what is under test is what YB
    /// answers for a branch whose rows start well above zero.
    #[tokio::test]
    async fn the_first_removal_resolves_the_delete_bound_from_the_table() {
        let table = scratch_table("wals_test_delete_bound");
        let (storage, _tmp) = make_storage(&table, Lsn(SEG_SIZE)).await;
        let branch_id = storage.branch_id;
        let client = storage.ysql_system_client.clone();
        let q = quote_relation(&table);

        // Rows at segnos 100..=103 and nothing below, so a bound of 0 and a
        // bound of 100 produce the same deletions but not the same range.
        for segno in 100i64..=103 {
            client
                .execute(
                    &format!(
                        "INSERT INTO {q} (bucket_id, branch_id, version, segno, chunk_offset, \
                                          chunk_size, data, last_modified_us) \
                         VALUES (0, $1, {DEFAULT_VERSION}, $2, 0, 8, $3, 0)"
                    ),
                    &[&branch_id, &segno, &b"12345678".to_vec()],
                )
                .await
                .expect("insert chunk");
        }

        assert_eq!(
            storage.delete_lower_bound.load(Ordering::Relaxed),
            DELETE_BOUND_UNRESOLVED,
            "construction must not have queried the table for the bound"
        );
        assert_eq!(
            YbWalStorage::load_min_segno(&client, &table, branch_id)
                .await
                .expect("resolve min(segno)"),
            100,
            "the resolver reports the branch's real floor"
        );

        crate::wal_storage::Storage::remove_up_to(&storage, 102)
            .await
            .expect("remove_up_to");

        assert_eq!(
            storage.delete_lower_bound.load(Ordering::Relaxed),
            102,
            "a successful removal leaves the bound at its upper edge"
        );
        let left: Vec<i64> = client
            .query(
                &format!("SELECT segno FROM {q} WHERE branch_id = $1 ORDER BY segno"),
                &[&branch_id],
            )
            .await
            .expect("read back")
            .iter()
            .map(|r| r.get::<_, i64>("segno"))
            .collect();
        assert_eq!(
            left,
            vec![102, 103],
            "only segnos below the cut are removed"
        );

        drop_wals_table(&client, &table).await;
    }

    /// `truncate_wals_above` drops chunks past the cut, trims the chunk
    /// straddling it, and leaves `max(chunk_offset + chunk_size)` exactly
    /// at the cut — the durability-side half of the torn-tail fix (#61).
    /// Idempotent on re-run.
    #[tokio::test]
    async fn truncate_wals_above_deletes_and_trims() {
        let table = scratch_table("wals_test_trunc_above");
        let (fpu, system) = open_lanes().await;
        make_wals_table(&fpu, &table).await;
        let branch_id = BranchId::generate();

        // Three chunks in segno 0 spanning [0, 20480): two full pages + a
        // partial. Cut at 10000 — mid the second chunk [8192, 16384).
        let chunks: &[(i64, i32, i32)] = &[(0, 0, 8192), (0, 8192, 8192), (0, 16384, 4096)];
        for (i, (segno, offset, size)) in chunks.iter().enumerate() {
            let bucket = (i % 10) as i16;
            let blob = vec![0x5au8; *size as usize];
            fpu.execute(
                &format!(
                    "INSERT INTO {} (bucket_id, branch_id, version, segno, chunk_offset, \
                                     chunk_size, data, last_modified_us) \
                     VALUES ($2, $1, {DEFAULT_VERSION}, $3, $4, $5, $6, $7)",
                    quote_relation(&table)
                ),
                &[&branch_id, &bucket, segno, offset, size, &enc(&blob), &0i64],
            )
            .await
            .expect("insert chunk");
        }

        truncate_wals_above(&system, &table, branch_id, SEG_SIZE as usize, Lsn(10000))
            .await
            .expect("truncate_wals_above");

        // End of WAL is now exactly the cut.
        let lsn =
            YbWalStorage::load_max_offset(&system, &table, branch_id, SEG_SIZE as usize, Lsn(0))
                .await
                .expect("load_max_offset");
        assert_eq!(
            lsn,
            Lsn(10000),
            "end of WAL must equal the truncation point"
        );

        // The chunk past the cut is gone; only two rows remain.
        let count: i64 = system
            .query_opt(
                &format!(
                    "SELECT count(*) FROM {} WHERE branch_id=$1",
                    quote_relation(&table)
                ),
                &[&branch_id],
            )
            .await
            .expect("count")
            .map(|r| r.get(0))
            .unwrap();
        assert_eq!(count, 2, "the chunk entirely past the cut must be deleted");

        // The straddling chunk is trimmed to 10000 - 8192 = 1808 bytes.
        // `octet_length(data)` would measure the STORED bytes (envelope
        // included), so fetch the column and unwrap it here instead.
        let row = system
            .query_opt(
                &format!(
                    "SELECT chunk_size, data FROM {} \
                     WHERE branch_id=$1 AND chunk_offset=8192",
                    quote_relation(&table)
                ),
                &[&branch_id],
            )
            .await
            .expect("straddling chunk")
            .expect("straddling chunk exists");
        assert_eq!(row.get::<_, i32>(0), 1808, "chunk_size trimmed to the cut");
        let stored: Vec<u8> = row.get(1);
        let trimmed = decode_chunk_data(&stored, "trimmed straddling chunk").expect("decode");
        assert_eq!(trimmed.len(), 1808, "data trimmed to the cut");
        // `truncate_wals_above` rewrites the trimmed remainder raw rather
        // than re-compressing it (see its doc comment).
        assert_eq!(
            payload_codec::stored_codec_name(&stored),
            "raw",
            "the trimmed chunk is stored raw"
        );

        // Idempotent: a second call is a no-op.
        truncate_wals_above(&system, &table, branch_id, SEG_SIZE as usize, Lsn(10000))
            .await
            .expect("truncate_wals_above (idempotent)");
        let lsn2 =
            YbWalStorage::load_max_offset(&system, &table, branch_id, SEG_SIZE as usize, Lsn(0))
                .await
                .expect("load_max_offset after re-run");
        assert_eq!(lsn2, Lsn(10000), "re-run must not change the end of WAL");

        // Idempotent under BLIND REPLAY of the trim statement itself, which
        // is a different case from the re-run above: `run_with_retries`
        // re-issues a statement verbatim when its ack is lost to a connection
        // drop, so the replayed UPDATE still carries the stale pre-image in
        // its predicate. The re-run above never reaches the UPDATE (nothing
        // straddles any more), so only this exercises the CAS.
        //
        // Replay the exact statement with the ORIGINAL pre-trim bytes as the
        // CAS operand; it must still report one affected row via the
        // post-image arm, or `truncate_wals_above` would report an
        // already-durable trim as a hard error.
        let (replay_data, replay_size): (Vec<u8>, i32) = (stored.clone(), 1808);
        let pre_image = enc(&vec![0x5au8; 8192]);
        let bucket_of_straddler: i16 = system
            .query_opt(
                &format!(
                    "SELECT bucket_id FROM {} WHERE branch_id=$1 AND chunk_offset=8192",
                    quote_relation(&table)
                ),
                &[&branch_id],
            )
            .await
            .expect("bucket lookup")
            .expect("straddling chunk exists")
            .get(0);
        // First pin why the post-image arm is needed at all: the pre-image
        // arm ALONE — the predicate this code shipped with before — matches
        // nothing once the row is trimmed. That zero is what used to trip the
        // `affected == 1` check on a replay.
        let pre_image_only = system
            .execute(
                &format!(
                    "UPDATE {} SET data = $5, chunk_size = $6 \
                     WHERE branch_id = $1 AND bucket_id = $2 AND segno = $3 \
                       AND chunk_offset = $4 AND data = $7",
                    quote_relation(&table)
                ),
                &[
                    &branch_id,
                    &bucket_of_straddler,
                    &0i64,
                    &8192i32,
                    &replay_data.as_slice(),
                    &replay_size,
                    &pre_image.as_slice(),
                ],
            )
            .await
            .expect("pre-image-only trim UPDATE");
        assert_eq!(
            pre_image_only, 0,
            "sanity: the pre-image no longer matches a trimmed row — this is \
             the zero that made the old CAS fail on a blind replay"
        );

        let replayed = system
            .execute(
                &format!(
                    "UPDATE {} SET data = $5, chunk_size = $6 \
                     WHERE branch_id = $1 AND bucket_id = $2 AND segno = $3 \
                       AND chunk_offset = $4 \
                       AND (data = $7 OR (data = $5 AND chunk_size = $6))",
                    quote_relation(&table)
                ),
                &[
                    &branch_id,
                    &bucket_of_straddler,
                    &0i64,
                    &8192i32,
                    &replay_data.as_slice(),
                    &replay_size,
                    &pre_image.as_slice(),
                ],
            )
            .await
            .expect("replayed trim UPDATE");
        assert_eq!(
            replayed, 1,
            "a blindly replayed trim must still match one row (post-image arm); \
             otherwise a lost ack turns a durable trim into a hard error"
        );

        drop_wals_table(&fpu, &table).await;
    }

    /// Stage chunks in three consecutive segments and cut mid-chunk in the
    /// middle one. The cut compares `(segno, chunk_offset)` against
    /// `(end_segno, end_off)` pairwise, so the fixture puts a row on each
    /// side of both halves of that comparison: a survivor *below* the cut
    /// whose offset is at or past `end_off`, and a casualty *above* it whose
    /// offset is below `end_off`. Comparing either column alone gets one of
    /// them wrong — and getting it wrong here either resurrects torn WAL or
    /// deletes acked WAL, neither of which the single-segment fixture above
    /// can see.
    #[tokio::test]
    async fn truncate_wals_above_cuts_across_segments() {
        let table = scratch_table("wals_test_trunc_segments");
        let (fpu, system) = open_lanes().await;
        make_wals_table(&fpu, &table).await;
        let branch_id = BranchId::generate();

        let seg = SEG_SIZE as i64;
        // 8 KiB chunks across segnos 4, 5 and 6, cut mid-chunk at offset
        // 12288 of segno 5. The interesting rows:
        //   segno 4 offset 16384  survives  (below the cut, offset > end_off)
        //   segno 5 offset 0      survives  (same segment, offset < end_off)
        //   segno 5 offset 8192   trimmed   (straddles the cut)
        //   segno 6 offset 0      deleted   (above the cut, offset < end_off)
        let cut = Lsn((5 * seg + 12288) as u64);
        let chunks: &[(i64, i32)] = &[
            (4, 0),
            (4, 8192),
            (4, 16384),
            (5, 0),
            (5, 8192),
            (6, 0),
            (6, 8192),
        ];
        for (i, (segno, offset)) in chunks.iter().enumerate() {
            let bucket = (i % 6) as i16;
            let blob = vec![(i as u8) + 1; 8192];
            fpu.execute(
                &format!(
                    "INSERT INTO {} (bucket_id, branch_id, version, segno, chunk_offset, \
                                     chunk_size, data, last_modified_us) \
                     VALUES ($2, $1, {DEFAULT_VERSION}, $3, $4, $5, $6, $7)",
                    quote_relation(&table)
                ),
                &[
                    &branch_id,
                    &bucket,
                    segno,
                    offset,
                    &8192i32,
                    &enc(&blob),
                    &0i64,
                ],
            )
            .await
            .expect("insert chunk");
        }

        truncate_wals_above(&system, &table, branch_id, SEG_SIZE as usize, cut)
            .await
            .expect("truncate_wals_above");

        let surviving: Vec<(i64, i64)> = system
            .query(
                &format!(
                    "SELECT segno, chunk_offset FROM {} WHERE branch_id=$1 \
                     ORDER BY segno, chunk_offset",
                    quote_relation(&table)
                ),
                &[&branch_id],
            )
            .await
            .expect("surviving rows")
            .iter()
            .map(|r| (r.get(0), i64::from(r.get::<_, i32>(1))))
            .collect();
        assert_eq!(
            surviving,
            vec![(4, 0), (4, 8192), (4, 16384), (5, 0), (5, 8192)],
            "everything at or past the cut goes, everything below it stays"
        );

        // The straddler is trimmed to 12288 - 8192 = 4096, and the end of WAL
        // lands exactly on the cut. Trimming the wrong row — the failure mode
        // if the straddling SELECT reached outside `end_segno` — shows up
        // here, since `keep` is derived from the row's own segno.
        let straddler_size: i32 = system
            .query_opt(
                &format!(
                    "SELECT chunk_size FROM {} \
                     WHERE branch_id=$1 AND segno=5 AND chunk_offset=8192",
                    quote_relation(&table)
                ),
                &[&branch_id],
            )
            .await
            .expect("straddling chunk")
            .expect("straddling chunk exists")
            .get(0);
        assert_eq!(straddler_size, 4096, "the straddler is trimmed to the cut");
        let lsn =
            YbWalStorage::load_max_offset(&system, &table, branch_id, SEG_SIZE as usize, Lsn(0))
                .await
                .expect("load_max_offset");
        assert_eq!(lsn, cut, "end of WAL must equal the cut");

        // Re-cut at a segment boundary. Nothing can straddle a segment-aligned
        // cut (rows never span segments), so this must drop segno 5 whole and
        // leave segno 4 — including its rows above `end_off` — untouched.
        truncate_wals_above(
            &system,
            &table,
            branch_id,
            SEG_SIZE as usize,
            Lsn((5 * seg) as u64),
        )
        .await
        .expect("truncate_wals_above at a segment boundary");
        let surviving: Vec<(i64, i64)> = system
            .query(
                &format!(
                    "SELECT segno, chunk_offset FROM {} WHERE branch_id=$1 \
                     ORDER BY segno, chunk_offset",
                    quote_relation(&table)
                ),
                &[&branch_id],
            )
            .await
            .expect("surviving rows after boundary cut")
            .iter()
            .map(|r| (r.get(0), i64::from(r.get::<_, i32>(1))))
            .collect();
        assert_eq!(
            surviving,
            vec![(4, 0), (4, 8192), (4, 16384)],
            "a segment-aligned cut drops the segment above it and trims nothing"
        );
        let sizes: Vec<i32> = system
            .query(
                &format!(
                    "SELECT chunk_size FROM {} WHERE branch_id=$1",
                    quote_relation(&table)
                ),
                &[&branch_id],
            )
            .await
            .expect("chunk sizes")
            .iter()
            .map(|r| r.get(0))
            .collect();
        assert_eq!(
            sizes,
            vec![8192, 8192, 8192],
            "no surviving chunk was trimmed"
        );

        drop_wals_table(&fpu, &table).await;
    }

    /// The statements must resolve to an index range on `segno`, not a scan
    /// of the branch's whole WAL. This is the actual content of the #812 fix
    /// — the behavioural tests above pass either way, because a full scan is
    /// only wrong in how long it takes, and on a loaded timeline that is long
    /// enough to hang `load_timeline` outright.
    ///
    /// Asserted two ways, because either alone is weak: the plan must carry
    /// `segno` in its **Index Cond** (a `Storage Filter` there is the
    /// regression), and the scan must read far fewer rows than the branch
    /// holds.
    #[tokio::test]
    async fn truncate_stmts_are_index_tight() {
        let table = scratch_table("wals_test_trunc_plan");
        let (fpu, system) = open_lanes().await;
        make_wals_table(&fpu, &table).await;
        let branch_id = BranchId::generate();

        // 3000 chunks spread over 30 segments and all six buckets: enough
        // that a branch-wide scan is unmistakable next to a one-segment one.
        const ROWS: i64 = 3000;
        const PER_SEG: i64 = 100;
        fpu.execute(
            &format!(
                "INSERT INTO {} (bucket_id, branch_id, version, segno, chunk_offset, \
                                 chunk_size, data, last_modified_us) \
                 SELECT (i % 6)::smallint, $1, {DEFAULT_VERSION}, i / {PER_SEG}, \
                         (i % {PER_SEG}) * 8192, 8192, $2, 0 \
                  FROM generate_series(0, {}) i",
                quote_relation(&table),
                ROWS - 1
            ),
            &[&branch_id, &enc(&[0x5au8; 64])],
        )
        .await
        .expect("bulk insert chunks");

        // Cut inside the last segment, the worst case for the old shape: the
        // rows it must touch are at the very end of the branch's key range.
        let end_segno = ROWS / PER_SEG - 1;
        let end_off = 50 * 8192i32;
        let (del, sel) = truncate_stmts(&quote_relation(&table));
        for (what, stmt) in [("delete", del), ("select", sel)] {
            let plan: String = system
                .query(
                    &format!("EXPLAIN (ANALYZE, DIST, COSTS OFF, TIMING OFF) {stmt}"),
                    &[&branch_id, &end_segno, &end_off],
                )
                .await
                .unwrap_or_else(|e| panic!("EXPLAIN the {what}: {e:#}"))
                .iter()
                .map(|r| r.get::<_, String>(0))
                .collect::<Vec<_>>()
                .join("\n");

            let index_cond = plan
                .lines()
                .find(|l| l.trim_start().starts_with("Index Cond:"))
                .unwrap_or_else(|| panic!("the {what} plan has no Index Cond:\n{plan}"));
            assert!(
                index_cond.contains("segno"),
                "the {what} must bound `segno` in its Index Cond, or it scans \
                 the branch's whole WAL (#812); plan was:\n{plan}"
            );

            // The plan's own accounting, so this holds whatever the row width
            // and however YB pages the scan.
            let scanned: i64 = plan
                .lines()
                .find_map(|l| {
                    l.trim()
                        .strip_prefix("Storage Table Rows Scanned: ")?
                        .parse()
                        .ok()
                })
                .unwrap_or_else(|| panic!("the {what} plan reports no rows scanned:\n{plan}"));
            assert!(
                scanned < ROWS / 4,
                "the {what} scanned {scanned} of {ROWS} rows — an index range \
                 over one segment reads ~{PER_SEG}; plan was:\n{plan}"
            );
        }

        drop_wals_table(&fpu, &table).await;
    }

    /// `YbSegmentReader` is the read path the WAL sender plugs into.
    /// Stage a couple of non-overlapping chunks directly in YB, then
    /// drive a reader across them and verify the bytes come back in
    /// order. Bypasses `write_wal` (which validates a
    /// real PG WAL page header that we don't want to synthesise here),
    /// so this stays focused on the YB read path: chunk-row selection,
    /// `pos` advance across chunk boundaries, and the read-lane SELECT.
    #[tokio::test]
    async fn segment_reader_round_trip_across_chunks() {
        use tokio::io::AsyncReadExt;

        let table = scratch_table("wals_test_reader");
        let (fpu, _system) = open_lanes().await;
        make_wals_table(&fpu, &table).await;

        let branch_id = BranchId::generate();

        let chunk_a: Vec<u8> = (0u8..=255).cycle().take(1024).collect();
        let chunk_b: Vec<u8> = (0u8..=255).cycle().take(2048).map(|b| b ^ 0xa5).collect();
        let now_us: i64 = 0;
        let segno_i: i64 = 0;
        for (i, (offset_i, size_i, data)) in [
            (0i32, 1024i32, chunk_a.as_slice()),
            (1024i32, 2048i32, chunk_b.as_slice()),
        ]
        .into_iter()
        .enumerate()
        {
            let bucket = (i % 10) as i16;
            fpu.execute(
                &format!(
                    "INSERT INTO {} (bucket_id, branch_id, version, segno, chunk_offset, \
                                     chunk_size, data, last_modified_us) \
                     VALUES ($2, $1, {DEFAULT_VERSION}, $3, $4, $5, $6, $7)",
                    quote_relation(&table)
                ),
                &[
                    &branch_id,
                    &bucket,
                    &segno_i,
                    &offset_i,
                    &size_i,
                    &enc(data),
                    &now_us,
                ],
            )
            .await
            .expect("INSERT chunk");
        }

        let yb_wal = wal_conn(&fpu, &table).await;
        let mut reader = YbSegmentReader::new(
            yb_wal,
            branch_id,
            0,
            SEG_SIZE as usize,
            0,
            Arc::new(std::sync::atomic::AtomicU64::new(u64::MAX)),
        );

        let mut got = vec![0u8; chunk_a.len() + chunk_b.len()];
        reader
            .read_exact(&mut got)
            .await
            .expect("read_exact across two chunks");
        let mut expected = chunk_a.clone();
        expected.extend_from_slice(&chunk_b);
        assert_eq!(got, expected, "round-trip bytes across chunks must match");

        drop_wals_table(&fpu, &table).await;
    }

    /// Mid-chunk resume: starting a read partway into a chunk must return
    /// the bytes from that offset onward — i.e. `fetch_chunks` must surface
    /// the *covering* chunk (the one starting before the position), not just
    /// chunks at/after it. Exercises the covering arm of the single-snapshot
    /// fan-out (#139).
    #[tokio::test]
    async fn segment_reader_mid_chunk_resume() {
        use tokio::io::AsyncReadExt;

        let table = scratch_table("wals_test_midchunk");
        let (fpu, _system) = open_lanes().await;
        make_wals_table(&fpu, &table).await;

        let branch_id = BranchId::generate();

        // One chunk spanning [0, 2048).
        let chunk: Vec<u8> = (0u8..=255).cycle().take(2048).collect();
        insert_chunk(&fpu, &table, branch_id, 0, 0, &chunk).await;

        let yb_wal = wal_conn(&fpu, &table).await;
        // Start the reader at byte offset 1024 — partway into the chunk.
        let mut reader = YbSegmentReader::new(
            yb_wal,
            branch_id,
            0,
            SEG_SIZE as usize,
            1024,
            Arc::new(std::sync::atomic::AtomicU64::new(u64::MAX)),
        );

        let mut got = vec![0u8; 1024];
        reader
            .read_exact(&mut got)
            .await
            .expect("read from mid-chunk offset");
        assert_eq!(
            got,
            chunk[1024..2048],
            "mid-chunk resume must return bytes from the requested offset"
        );

        drop_wals_table(&fpu, &table).await;
    }

    /// Coherence (#139): reader-side cache population is allowed, but a read
    /// may cache only chunks lying fully below the flush frontier live when it
    /// fetched them. Committed WAL is immutable (a later term can only
    /// truncate+rewrite bytes ABOVE flush, and truncate never removes committed
    /// WAL), so a cached committed chunk can never be shadowed by a rewrite.
    /// The once-dangerous chunk — one straddling/past flush whose tail a new
    /// term rewrites — is never taken into the cache, so the racing-insert
    /// hazard (insert landing after `cache.truncate_after`, pinned by the
    /// Vacant-only insert) cannot arise. (The serve-side flush bound only
    /// guards the *held* chunk; a cache re-lookup returns the same bytes, so
    /// the cache must refuse uncommitted bytes at insert time.)
    ///
    /// Deterministic: a committed chunk `[0,1024)` and an uncommitted tail
    /// `[1024,4096)` are staged with flush at 1024. A read of `[0,1024)` must
    /// cache the committed chunk (warming) but NOT the tail. A new term then
    /// rewrites the tail and flush advances; re-reading `[1024,4096)` must
    /// observe the rewrite, never a stale cached copy.
    #[tokio::test]
    async fn segment_reader_caches_only_committed_chunks() {
        use std::sync::atomic::{AtomicU64, Ordering};
        use tokio::io::AsyncReadExt;

        let table = scratch_table("wals_test_coherence");
        let (fpu, _system) = open_lanes().await;
        make_wals_table(&fpu, &table).await;

        let branch_id = BranchId::generate();

        let seg: i64 = 0;
        let committed: Vec<u8> = vec![0xAA; 1024]; // [0,1024), below flush
        let tail_old: Vec<u8> = vec![0xBB; 3072]; // [1024,4096), above flush
        let tail_new: Vec<u8> = vec![0xCC; 3072]; // the rewrite

        let insert_sql = format!(
            "INSERT INTO {} (bucket_id, branch_id, version, segno, chunk_offset, \
                             chunk_size, data, last_modified_us) \
             VALUES ($2, $1, {DEFAULT_VERSION}, $3, $4, $5, $6, 0)",
            quote_relation(&table)
        );
        let delete_sql = format!(
            "DELETE FROM {} WHERE branch_id = $1 \
             AND segno = $2 AND chunk_offset = $3",
            quote_relation(&table)
        );

        // 1. Stage the committed chunk and the (still-uncommitted) tail.
        for (i, (off, bytes)) in [(0i32, &committed), (1024i32, &tail_old)]
            .into_iter()
            .enumerate()
        {
            let bucket = (i % 10) as i16;
            let size = bytes.len() as i32;
            let slice = bytes.as_slice();
            fpu.execute(
                &insert_sql,
                &[&branch_id, &bucket, &seg, &off, &size, &enc(slice)],
            )
            .await
            .expect("INSERT chunk");
        }

        // Shared cache, as in production (one per YbWalConn).
        let cache = Arc::new(std::sync::Mutex::new(WalCache::new(1 << 20)));
        let yb_wal = wal_conn_cached(&fpu, &table, Some(cache.clone())).await;

        // flush = 1024: [0,1024) committed, [1024,4096) not yet.
        let flush = Arc::new(AtomicU64::new(1024));
        let mut reader = YbSegmentReader::new(
            yb_wal.clone(),
            branch_id,
            0,
            SEG_SIZE as usize,
            0,
            flush.clone(),
        );

        // 2. Read the committed range. The fetch sees both chunks, but only the
        //    committed one is cacheable.
        let mut head = vec![0u8; 1024];
        reader
            .read_exact(&mut head)
            .await
            .expect("read committed head");
        assert_eq!(head, committed, "committed range must read back");

        {
            let mut guard = cache.lock().unwrap();
            assert!(
                guard.lookup(0, 0).is_some(),
                "committed chunk must be cached (reader-side warming)"
            );
            assert!(
                guard.lookup(0, 1024).is_none(),
                "uncommitted tail must NOT be cached — it could shadow a rewrite (#139)"
            );
        }

        // 3. A new term rewrites the once-uncommitted tail; flush then advances
        //    to cover it. (No cache invalidation is modeled — the point is that
        //    the tail was never cached, so there is nothing stale to shadow.)
        fpu.execute(&delete_sql, &[&branch_id, &seg, &1024i32])
            .await
            .expect("DELETE old tail");
        let tail_new_slice = tail_new.as_slice();
        let tail_size = tail_new.len() as i32;
        fpu.execute(
            &insert_sql,
            &[
                &branch_id,
                &1i16,
                &seg,
                &1024i32,
                &tail_size,
                &enc(tail_new_slice),
            ],
        )
        .await
        .expect("INSERT rewritten tail");
        flush.store(4096, Ordering::Release);

        // 4. The now-committed tail MUST come back as the rewrite.
        let mut tail = vec![0u8; 3072];
        reader
            .read_exact(&mut tail)
            .await
            .expect("read rewritten tail");
        assert_eq!(
            tail, tail_new,
            "reader served a stale cached tail after a term rewrite (#139)"
        );

        drop_wals_table(&fpu, &table).await;
    }

    /// XLOG_SWITCH padding hole (#181). `pg_switch_wal()` ends a segment with a
    /// switch record and zero-pads the rest; the compute streams only up to the
    /// switch record, so the segment tail is absent from `wals`. A cold reader
    /// catching up across that *historical* switch (a pageserver whose durable
    /// consistent_lsn lagged behind it) reads into the missing tail. Once the
    /// committed frontier has advanced to/past the next segment, that gap is the
    /// padding and the reader must synthesize zeros (which the decoder skips),
    /// not fail with a "WAL hole" and wedge catch-up. But a gap in a segment
    /// that is NOT yet fully committed is the live frontier (or genuine loss)
    /// and must still error.
    #[tokio::test]
    async fn segment_reader_zero_fills_xlog_switch_padding() {
        use std::sync::atomic::AtomicU64;
        use tokio::io::AsyncReadExt;

        let table = scratch_table("wals_test_switchpad");
        let (fpu, _system) = open_lanes().await;
        make_wals_table(&fpu, &table).await;

        let branch_id = BranchId::generate();

        // Segment 0 holds real WAL in [0, 1024) (think: up to the switch
        // record). Nothing is stored in [1024, SEG_SIZE) — the padding gap.
        let seg: i64 = 0;
        let head_bytes: Vec<u8> = vec![0xAA; 1024];
        let insert_sql = format!(
            "INSERT INTO {} (bucket_id, branch_id, version, segno, chunk_offset, \
                             chunk_size, data, last_modified_us) \
             VALUES ($2, $1, {DEFAULT_VERSION}, $3, $4, $5, $6, 0)",
            quote_relation(&table)
        );
        let head_slice = head_bytes.as_slice();
        let head_size = head_bytes.len() as i32;
        fpu.execute(
            &insert_sql,
            &[&branch_id, &0i16, &seg, &0i32, &head_size, &enc(head_slice)],
        )
        .await
        .expect("INSERT head chunk");

        let yb_wal = wal_conn(&fpu, &table).await;

        // Committed frontier is in the NEXT segment: the gap is switch padding.
        let flush_past = Arc::new(AtomicU64::new(SEG_SIZE + 4096));
        let mut reader = YbSegmentReader::new(
            yb_wal.clone(),
            branch_id,
            0,
            SEG_SIZE as usize,
            1024,
            flush_past,
        );
        let mut buf = vec![0xFFu8; 4096];
        reader
            .read_exact(&mut buf)
            .await
            .expect("padding gap below a completed segment must read back as zeros");
        assert!(
            buf.iter().all(|&b| b == 0),
            "switch padding must be synthesized as zeros"
        );

        // Same gap, but the frontier is still INSIDE this segment: not padding
        // (live frontier / genuine loss) — must surface as a hole, not zeros.
        let flush_inside = Arc::new(AtomicU64::new(2048));
        let mut reader = YbSegmentReader::new(
            yb_wal.clone(),
            branch_id,
            0,
            SEG_SIZE as usize,
            1024,
            flush_inside,
        );
        let mut buf = vec![0u8; 4096];
        let err = reader
            .read_exact(&mut buf)
            .await
            .expect_err("a gap below the committed frontier must not be masked as zeros");
        assert!(
            err.to_string().contains("WAL hole") || err.kind() == std::io::ErrorKind::UnexpectedEof,
            "expected a WAL-hole error, got: {err}"
        );

        drop_wals_table(&fpu, &table).await;
    }

    /// Read-past-flush splice (the true root cause of #139). The YB fetch is
    /// blind to the commit/flush frontier: it pulls a whole covering chunk,
    /// including bytes that lie *above* flush and are therefore still
    /// uncommitted. yb-amp WAL is immutable only once committed — a later
    /// walproposer term truncates+rewrites the uncommitted tail. If the reader
    /// keeps serving the chunk it already holds as flush advances, it streams
    /// the pre-rewrite (now-stale) bytes for the `[old_flush, new_flush)`
    /// region and the interpreted-WAL decoder trips ("invalid xlog page
    /// header"). The reader must serve a held chunk only up to the flush that
    /// was live when it was fetched, and re-fetch past that.
    ///
    /// Deterministic model: one chunk spans `[0, 4096)`. With flush at 1024 we
    /// read `[0, 1024)` (all that's committed). A new term then rewrites the
    /// once-uncommitted tail `[1024, 4096)` in YB and flush advances to 4096.
    /// Reading `[1024, 4096)` must now observe the rewrite — not the bytes the
    /// reader fetched (and may still be holding) from before the rewrite.
    #[tokio::test]
    async fn segment_reader_must_not_serve_past_flush_after_rewrite() {
        use std::sync::atomic::{AtomicU64, Ordering};
        use tokio::io::AsyncReadExt;

        let table = scratch_table("wals_test_pastflush");
        let (fpu, _system) = open_lanes().await;
        make_wals_table(&fpu, &table).await;

        let branch_id = BranchId::generate();

        let seg: i64 = 0;
        let off: i32 = 0;
        let size: i32 = 4096;
        // Committed prefix [0,1024) is identical across terms (committed WAL is
        // immutable); only the once-uncommitted tail [1024,4096) is rewritten.
        let old: Vec<u8> = vec![0xAB; 4096];
        let mut new: Vec<u8> = vec![0u8; 4096];
        new[..1024].fill(0xAB); // committed prefix, unchanged
        new[1024..].fill(0xCD); // rewritten tail

        let insert_sql = format!(
            "INSERT INTO {} (bucket_id, branch_id, version, segno, chunk_offset, \
                             chunk_size, data, last_modified_us) \
             VALUES ($2, $1, {DEFAULT_VERSION}, $3, $4, $5, $6, 0)",
            quote_relation(&table)
        );
        let delete_sql = format!(
            "DELETE FROM {} WHERE branch_id = $1 \
             AND segno = $2 AND chunk_offset = $3",
            quote_relation(&table)
        );

        // 1. Stage the chunk as written by the old term.
        let old_slice = old.as_slice();
        fpu.execute(
            &insert_sql,
            &[&branch_id, &0i16, &seg, &off, &size, &enc(old_slice)],
        )
        .await
        .expect("INSERT old chunk");

        let yb_wal = wal_conn(&fpu, &table).await;

        // Live flush frontier, as the streaming reader would publish it.
        let flush = Arc::new(AtomicU64::new(1024));
        let mut reader = YbSegmentReader::new(
            yb_wal.clone(),
            branch_id,
            0,
            SEG_SIZE as usize,
            0,
            flush.clone(),
        );

        // 2. Read everything committed so far ([0,1024)). The reader fetches
        //    the whole covering chunk [0,4096) but must trust it only to 1024.
        let mut head = vec![0u8; 1024];
        reader
            .read_exact(&mut head)
            .await
            .expect("read committed head");
        assert_eq!(head, old[..1024], "committed prefix must read back");

        // 3. A new term rewrites the once-uncommitted tail; flush then advances
        //    to cover it.
        fpu.execute(&delete_sql, &[&branch_id, &seg, &off])
            .await
            .expect("DELETE old chunk");
        let new_slice = new.as_slice();
        fpu.execute(
            &insert_sql,
            &[&branch_id, &0i16, &seg, &off, &size, &enc(new_slice)],
        )
        .await
        .expect("INSERT rewritten chunk");
        flush.store(4096, Ordering::Release);

        // 4. The now-committed [1024,4096) MUST come back as the rewrite, not
        //    the pre-rewrite bytes the reader fetched (and may still hold).
        let mut tail = vec![0u8; 3072];
        reader
            .read_exact(&mut tail)
            .await
            .expect("read rewritten tail");
        assert_eq!(
            tail,
            new[1024..],
            "reader served stale past-flush WAL after a term rewrite — the read-past-flush splice (#139)"
        );

        drop_wals_table(&fpu, &table).await;
    }

    /// Same read-past-flush rewrite as above, but on a NON-ZERO segment. This
    /// is the regression guard for the #139 root cause: the flush bound is an
    /// ABSOLUTE LSN while chunk offsets are SEGMENT-relative, so the serve guard
    /// (`end_offset.min(bound)`) and the cache-warm gate (`end_offset <= flush`)
    /// were no-ops for any segment ≥ 1 — a within-segment offset (< 16 MiB) is
    /// always below an absolute LSN. The seg-0 test can't catch this because
    /// there segment-relative == absolute. With the bug, the reader serves the
    /// stale pre-rewrite tail (the splice); with the fix it re-fetches and
    /// serves the rewrite.
    #[tokio::test]
    async fn segment_reader_must_not_serve_past_flush_after_rewrite_seg1() {
        use std::sync::atomic::{AtomicU64, Ordering};
        use tokio::io::AsyncReadExt;

        let table = scratch_table("wals_test_pastflush_seg1");
        let (fpu, _system) = open_lanes().await;
        make_wals_table(&fpu, &table).await;

        let branch_id = BranchId::generate();

        // Segment 1: absolute LSN = SEG_SIZE + offset.
        let seg: i64 = 1;
        let seg_base: u64 = SEG_SIZE; // 1 * SEG_SIZE
        let off: i32 = 0;
        let size: i32 = 4096;
        let old: Vec<u8> = vec![0xAB; 4096];
        let mut new: Vec<u8> = vec![0u8; 4096];
        new[..1024].fill(0xAB); // committed prefix, unchanged
        new[1024..].fill(0xCD); // rewritten tail

        let insert_sql = format!(
            "INSERT INTO {} (bucket_id, branch_id, version, segno, chunk_offset, \
                             chunk_size, data, last_modified_us) \
             VALUES ($2, $1, {DEFAULT_VERSION}, $3, $4, $5, $6, 0)",
            quote_relation(&table)
        );
        let delete_sql = format!(
            "DELETE FROM {} WHERE branch_id = $1 \
             AND segno = $2 AND chunk_offset = $3",
            quote_relation(&table)
        );

        let old_slice = old.as_slice();
        fpu.execute(
            &insert_sql,
            &[&branch_id, &0i16, &seg, &off, &size, &enc(old_slice)],
        )
        .await
        .expect("INSERT old chunk");

        let yb_wal = wal_conn(&fpu, &table).await;

        // Flush frontier is ABSOLUTE: committed up to offset 1024 in segment 1.
        let flush = Arc::new(AtomicU64::new(seg_base + 1024));
        let mut reader = YbSegmentReader::new(
            yb_wal.clone(),
            branch_id,
            1,
            SEG_SIZE as usize,
            0,
            flush.clone(),
        );

        let mut head = vec![0u8; 1024];
        reader
            .read_exact(&mut head)
            .await
            .expect("read committed head");
        assert_eq!(head, old[..1024], "committed prefix must read back");

        fpu.execute(&delete_sql, &[&branch_id, &seg, &off])
            .await
            .expect("DELETE old chunk");
        let new_slice = new.as_slice();
        fpu.execute(
            &insert_sql,
            &[&branch_id, &0i16, &seg, &off, &size, &enc(new_slice)],
        )
        .await
        .expect("INSERT rewritten chunk");
        flush.store(seg_base + 4096, Ordering::Release);

        let mut tail = vec![0u8; 3072];
        reader
            .read_exact(&mut tail)
            .await
            .expect("read rewritten tail");
        assert_eq!(
            tail,
            new[1024..],
            "seg-1 reader served stale past-flush WAL after a term rewrite — \
             the absolute-vs-segment-relative flush-bound no-op (#139)"
        );

        drop_wals_table(&fpu, &table).await;
    }

    /// Multi-chunk batch coalescing, end to end through the real
    /// producer/consumer: many small contiguous `upsert_writes` must land
    /// as FEWER `amp_storage.wals` rows (contiguous runs concatenated by
    /// [`coalesce_runs`]) whose concatenation is byte-identical to the
    /// submitted stream.
    ///
    /// Determinism: `#[tokio::test]` runs a current-thread runtime, and
    /// `mpsc::Sender::send` on a channel with free capacity resolves
    /// without yielding to the scheduler — so the consumer task (spawned
    /// in `YbWalStorage::new`, parked in `rx.recv()`) cannot run between
    /// our submissions. All 20 chunks are queued when `flush_wal` first
    /// yields; the consumer's greedy `try_recv` drain then assembles them
    /// into ONE multi-chunk batch (20 < FLUSH_ROWS_THRESHOLD). If that
    /// ever stops holding, the explicit "no coalescing happened" assert
    /// below fails loudly rather than letting the test pass while
    /// exercising only single-chunk batches.
    #[tokio::test]
    async fn consumer_coalesces_contiguous_chunks_byte_equivalent() {
        let table = scratch_table("wals_test_coalesce_rt");
        let start = Lsn(SEG_SIZE); // segno 1, offset 0
        let (mut storage, _tmp) = make_storage(&table, start).await;

        // 20 small contiguous chunks with distinct, position-dependent
        // bytes (so any reordering/duplication corrupts the comparison).
        let mut stream: Vec<u8> = Vec::new();
        let mut pos = start;
        let mut max_submitted = 0usize;
        for i in 0..20u8 {
            let len = 64 + (i as usize) * 7;
            max_submitted = max_submitted.max(len);
            let buf: Vec<u8> = (0..len)
                .map(|j| i.wrapping_mul(31).wrapping_add(j as u8))
                .collect();
            storage
                .upsert_writes(pos, &buf)
                .await
                .expect("upsert_writes");
            pos = Lsn(pos.0 + len as u64);
            stream.extend_from_slice(&buf);
        }

        // Durability barrier: wakes the consumer and waits for the batch.
        storage.flush_wal().await.expect("flush_wal");

        let rows = storage
            .ysql_system_client
            .query(
                &format!(
                    "SELECT segno, chunk_offset, chunk_size, data FROM {} \
                     WHERE branch_id = $1 ORDER BY segno, chunk_offset",
                    quote_relation(&table)
                ),
                &[&storage.branch_id],
            )
            .await
            .expect("select wals rows");
        assert!(!rows.is_empty(), "flushed WAL must be visible in the table");

        // (a) Coalescing actually happened: at least one row is larger
        // than ANY single submitted chunk.
        let max_row_size = rows
            .iter()
            .map(|r| r.get::<_, i32>("chunk_size"))
            .max()
            .unwrap();
        assert!(
            max_row_size as usize > max_submitted,
            "no coalescing happened: largest row is {max_row_size} B but the largest \
             submitted chunk was {max_submitted} B — every chunk landed as its own \
             row, so the multi-chunk coalescing path was NOT exercised"
        );

        // (b) The rows tile the submitted range exactly and concatenate
        // back to the original byte stream.
        let mut got: Vec<u8> = Vec::with_capacity(stream.len());
        for row in &rows {
            let segno: i64 = row.get("segno");
            let off: i32 = row.get("chunk_offset");
            let size: i32 = row.get("chunk_size");
            let stored: Vec<u8> = row.get("data");
            // `chunk_size` is LOGICAL length: it must match the payload
            // AFTER the compression envelope is unwrapped, not the stored
            // byte count. The LSN arithmetic in the read path and in
            // `truncate_wals_above` depends on this.
            let data = decode_chunk_data(&stored, "round-trip test row").expect("decode");
            assert_eq!(
                data.len(),
                size as usize,
                "chunk_size must equal the DECODED length of data"
            );
            let abs = segno as u64 * SEG_SIZE + off as u64;
            assert_eq!(
                abs,
                start.0 + got.len() as u64,
                "rows must tile the submitted range with no gap or overlap"
            );
            got.extend_from_slice(&data);
        }
        assert_eq!(
            got, stream,
            "concatenated row bytes must equal the submitted byte stream exactly"
        );

        drop_wals_table(&storage.ysql_system_client, &table).await;
    }

    /// Per-batch bucket stamping + rotation: every row written by one
    /// consumer batch must carry ONE `bucket_id` (the whole batch targets
    /// one tablet while that tablet is unsplit), and the bucket must rotate across batches
    /// (the load spread across tablets).
    ///
    /// Three flush cycles, one batch each (same current-thread-runtime
    /// determinism as `consumer_coalesces_contiguous_chunks_byte_equivalent`).
    /// Each cycle submits two contiguous pairs separated by a byte gap,
    /// so the batch coalesces into two rows — making "all rows of the
    /// batch share one bucket" a real multi-row assertion. Batch
    /// membership is tracked by diffing the table's row set across
    /// flushes.
    #[tokio::test]
    async fn consumer_stamps_one_bucket_per_batch_and_rotates() {
        use std::collections::HashSet;

        let table = scratch_table("wals_test_bucket_rot");
        let start = Lsn(SEG_SIZE);
        let (mut storage, _tmp) = make_storage(&table, start).await;

        let select = format!(
            "SELECT bucket_id, segno, chunk_offset FROM {} WHERE branch_id = $1",
            quote_relation(&table)
        );
        let mut seen: HashSet<(i64, i64)> = HashSet::new();
        let mut batch_buckets: Vec<i16> = Vec::new();

        for cycle in 0..3u64 {
            // Two contiguous pairs with a gap between them: coalesces to
            // exactly two rows within ONE batch.
            let base = start.0 + cycle * 100_000;
            for (delta, len) in [(0u64, 64usize), (64, 64), (1000, 96), (1096, 32)] {
                let buf = vec![(0xA0 ^ cycle as u8).wrapping_add(delta as u8); len];
                storage
                    .upsert_writes(Lsn(base + delta), &buf)
                    .await
                    .expect("upsert_writes");
            }
            storage.flush_wal().await.expect("flush_wal");

            let rows = storage
                .ysql_system_client
                .query(&select, &[&storage.branch_id])
                .await
                .expect("select wals rows");
            let new_rows: Vec<(i16, i64, i64)> = rows
                .iter()
                .map(|r| {
                    (
                        r.get::<_, i16>("bucket_id"),
                        r.get::<_, i64>("segno"),
                        i64::from(r.get::<_, i32>("chunk_offset")),
                    )
                })
                .filter(|(_, s, o)| !seen.contains(&(*s, *o)))
                .collect();
            assert!(
                new_rows.len() >= 2,
                "flush {cycle} must have written its two coalesced runs, got {new_rows:?}"
            );
            let bucket = new_rows[0].0;
            assert!(
                (0..WALS_BUCKETS).contains(&bucket),
                "bucket_id {bucket} out of range"
            );
            assert!(
                new_rows.iter().all(|(b, _, _)| *b == bucket),
                "rows written by ONE flushed batch must all carry one bucket_id; \
                 flush {cycle} wrote {new_rows:?}"
            );
            batch_buckets.push(bucket);
            for (_, s, o) in &new_rows {
                seen.insert((*s, *o));
            }
        }

        // Rotation happened: the batches did not all share one bucket.
        let distinct: HashSet<i16> = batch_buckets.iter().copied().collect();
        assert!(
            distinct.len() >= 2,
            "bucket must rotate across batches; per-batch buckets: {batch_buckets:?}"
        );
        // And the SQL form of the same invariant, across the whole table.
        let distinct_sql: i64 = storage
            .ysql_system_client
            .query_opt(
                &format!(
                    "SELECT count(DISTINCT bucket_id) FROM {} WHERE branch_id = $1",
                    quote_relation(&table)
                ),
                &[&storage.branch_id],
            )
            .await
            .expect("count distinct buckets")
            .map(|r| r.get(0))
            .unwrap();
        assert!(
            distinct_sql >= 2,
            "COUNT(DISTINCT bucket_id) = {distinct_sql}, expected >= 2 (rotation)"
        );

        drop_wals_table(&storage.ysql_system_client, &table).await;
    }

    /// Rehydration must take the max end position ACROSS buckets, not
    /// from any single bucket's top row. Adversarial placement: the
    /// globally-highest end lives in bucket 7 (which also holds a low
    /// decoy the per-bucket `ORDER BY segno DESC, chunk_offset DESC`
    /// probe must skip), while other buckets hold their own bucket-local
    /// maxima that end lower — including one in the same top segno.
    ///
    /// Calls `load_max_offset` directly (the end of the `load_tail` restore
    /// seam `YbWalStorage::new` uses) rather than constructing the full
    /// storage: construction additionally runs the record-boundary
    /// end-of-WAL scan over the row bytes, which on this synthetic
    /// payload could nondeterministically trim the tail and obscure what
    /// is under test — the cross-bucket max itself.
    #[tokio::test]
    async fn load_max_offset_takes_max_across_adversarial_buckets() {
        let table = scratch_table("wals_test_bucket_max");
        let (fpu, system) = open_lanes().await;
        make_wals_table(&fpu, &table).await;
        let branch_id = BranchId::generate();

        // (bucket_id, segno, chunk_offset, chunk_size); rows never
        // overlap within a segno, per the writer invariant. Buckets must be
        // < WALS_BUCKETS or the reader's `bucket_id IN (…)` list cannot see
        // them and the max silently comes back low.
        let rows: &[(i16, i64, i32, i32)] = &[
            (4, 5, 8192, 128),  // TRUE global max end: 5*SEG + 8320
            (4, 3, 0, 64),      // low decoy in the SAME bucket as the max
            (2, 5, 0, 8192),    // second-highest end, same top segno, other bucket
            (0, 4, 16000, 384), // bucket-local max in an older segno
            (5, 1, 0, 512),     // ancient decoy
        ];
        let insert_sql = format!(
            "INSERT INTO {} (bucket_id, branch_id, version, segno, chunk_offset, \
                             chunk_size, data, last_modified_us) \
             VALUES ($2, $1, {DEFAULT_VERSION}, $3, $4, $5, $6, 0)",
            quote_relation(&table)
        );
        for (bucket, segno, off, size) in rows {
            let blob = vec![0x77u8; *size as usize];
            let slice = blob.as_slice();
            fpu.execute(
                &insert_sql,
                &[&branch_id, bucket, segno, off, size, &enc(slice)],
            )
            .await
            .expect("insert adversarial row");
        }

        let got =
            YbWalStorage::load_max_offset(&system, &table, branch_id, SEG_SIZE as usize, Lsn(0))
                .await
                .expect("load_max_offset");
        assert_eq!(
            got,
            Lsn(5 * SEG_SIZE + 8192 + 128),
            "load_max_offset must return the global max end across ALL buckets, \
             not a single bucket's top row"
        );

        // Fresh-timeline fallback: an unknown branch yields commit_lsn.
        let other = BranchId::generate();
        let fallback =
            YbWalStorage::load_max_offset(&system, &table, other, SEG_SIZE as usize, Lsn(4242))
                .await
                .expect("load_max_offset (empty branch)");
        assert_eq!(
            fallback,
            Lsn(4242),
            "empty branch must fall back to commit_lsn"
        );

        drop_wals_table(&fpu, &table).await;
    }

    /// Every WAL write must bind the [`encode_run`] output, never a run's
    /// plaintext bytes.
    ///
    /// A source-level guard, because the failure it catches is invisible to
    /// the type system and to every other test here. A new `flush_batch_*`
    /// variant is *new code* — it conflicts with nothing, compiles fine, and
    /// silently writes untagged plaintext that the read path then rejects.
    /// That is exactly how the `yb_thin_client` writes reached the
    /// pageserver's `put_pages_thin`: clean auto-merge, clippy clean, every
    /// unit test green, and the cluster could not start because it failed to
    /// decode its own pages.
    #[test]
    fn every_wal_write_binds_encoded_runs() {
        let src = include_str!("yb_wal_storage.rs");

        // Needles assembled from fragments so this test's own source does
        // not match them — otherwise the guard flags itself.
        let bind_prefix = format!("(\"da{}\", BindValue::Bytes(", "ta");
        let raw_concat = format!("run_data_by{}(", "tes");
        let raw_chunk = format!("pc.da{}", "ta");

        let raw_binds: Vec<_> = src
            .lines()
            .enumerate()
            .filter(|(_, l)| {
                l.contains(&bind_prefix) && (l.contains(&raw_concat) || l.contains(&raw_chunk))
            })
            .map(|(i, l)| format!("  line {}: {}", i + 1, l.trim()))
            .collect();
        assert!(
            raw_binds.is_empty(),
            "these WAL writes bind PLAINTEXT run bytes instead of the \
             encoded buffer from `encode_run` — chunks written this way have \
             no codec tag and `decode_chunk_data` will reject them on \
             read:\n{}",
            raw_binds.join("\n")
        );

        // Sanity: the guard only means something if it can see the binds it
        // polices. One literal match — [`flush_batch_thin`].
        let seen = src.matches(&bind_prefix).count();
        assert!(
            seen >= 1,
            "expected at least 1 `data` BindValue site to police, saw \
             {seen} — the bind syntax changed and this guard has gone blind"
        );

        // The READ side needs the same guard, and for a nastier reason:
        // `CurrentChunk`'s offsets are LSN-space and the segment reader
        // slices `data` with them, so an undecoded buffer does not merely
        // return wrong bytes — it panics the WAL sender with an
        // out-of-range slice the moment compression shortens the stored
        // span. Every construction site must decode.
        // Flag the bug's actual signature: a chunk buffer wrapped straight
        // from an indexed DB row cell, with no decode in the expression. A
        // correct site wraps the decoder's output, or copies a local that was
        // decoded first.
        //
        // An earlier version of this check looked for a decoder call
        // "somewhere near" the constructor, and passed even with the decode
        // deleted — proximity is not the invariant, the field expression is.
        // The needle is built from fragments so these lines cannot match it.
        let raw_row = format!("Arc::new(ro{}[", "w");
        let lines: Vec<&str> = src.lines().collect();
        let raw_chunks: Vec<_> = lines
            .iter()
            .enumerate()
            .filter(|(_, l)| l.contains(&raw_row))
            .map(|(i, l)| format!("  line {}: {}", i + 1, l.trim()))
            .collect();
        assert!(
            raw_chunks.is_empty(),
            "these sites build a WAL chunk buffer from RAW row bytes without \
             `decode_chunk_data` — `CurrentChunk`'s offsets are LSN-space and \
             the segment reader slices `data` with them, so an encoded buffer \
             panics the WAL sender with an out-of-range slice as soon as \
             compression shortens the stored span:\n{}",
            raw_chunks.join("\n")
        );

        // Sanity: the decode funnel must actually be in use on the read side.
        // Counted over PRODUCTION source only, and net of the function's own
        // definition: the tests below decode too, and letting their call sites
        // count would keep this green with every real read path stripped —
        // the exact blindness the guard above exists to avoid.
        let decoder = format!("decode_chunk_da{}(", "ta");
        let production = src.split("\n#[cfg(test)]").next().unwrap_or(src);
        let decodes = production.matches(&decoder).count()
            - production.matches(&format!("fn {decoder}")).count();
        assert!(
            decodes >= 2,
            "expected at least 2 production `decode_chunk_data` call sites \
             (the end-of-WAL scan and the segment reader's fetch), saw \
             {decodes} — a read path has lost its decode"
        );
    }

    #[test]
    fn encoded_runs_round_trip_and_keep_chunk_size_logical() {
        fn pc(segno: u64, off: i32, fill: u8, len: usize) -> PendingChunk {
            PendingChunk {
                segno,
                bucket_id: 0, // placeholder, stamped below like consumer_main does
                chunk_offset: off,
                chunk_size: len as i32,
                data: Arc::new(vec![fill; len]),
                end_lsn: Lsn(segno * SEG_SIZE + off as u64 + len as u64),
            }
        }
        // Every run shape in one batch: a 3-chunk contiguous run, a
        // single-chunk run after a byte gap, a 2-chunk run in the next
        // segment.
        let mut batch = vec![
            pc(1, 0, 0x11, 64),
            pc(1, 64, 0x22, 32),
            pc(1, 96, 0x33, 16),
            pc(1, 512, 0x44, 8),
            pc(2, 0, 0x55, 128),
            pc(2, 128, 0x66, 64),
        ];
        // consumer_main stamps the assembled batch with ONE bucket.
        for c in &mut batch {
            c.bucket_id = 4;
        }
        let runs = coalesce_runs(&batch);
        assert_eq!(
            runs,
            vec![0..3, 3..4, 4..6],
            "batch must produce a multi-chunk run, a singleton, and a cross-segment break"
        );

        // Exercise both codecs: rows written under either must decode with
        // one reader, which is what makes `yb_wal_compression` a restart
        // rather than a migration.
        for codec in [payload_codec::Codec::Lz4, payload_codec::Codec::Raw] {
            for r in &runs {
                let chunks = &batch[r.clone()];
                let first = &chunks[0];

                // Ground truth: concatenation of the submitted chunk bytes.
                let mut plain = Vec::new();
                for c in chunks {
                    plain.extend_from_slice(&c.data);
                }

                // The buffer the write binds for this run.
                let stored = encode_run(chunks, codec);
                let decoded = payload_codec::decode(&stored).expect("stored run must decode");
                assert_eq!(
                    &decoded[..],
                    &plain[..],
                    "run at segno={} off={} must survive the {codec:?} envelope intact",
                    first.segno,
                    first.chunk_offset,
                );

                // The invariant the LSN arithmetic depends on.
                let chunk_size: i32 = chunks.iter().map(|p| p.chunk_size).sum();
                assert_eq!(
                    chunk_size as usize,
                    decoded.len(),
                    "chunk_size must be the DECODED length, not the stored length"
                );
            }
        }

        // The all-same-byte fills above compress hard; make sure the
        // threshold path is covered too — a run under MIN_COMPRESS_BYTES
        // stays raw and still round-trips.
        let tiny = vec![pc(3, 0, 0x77, 8)];
        let stored = encode_run(&tiny, payload_codec::Codec::Lz4);
        assert_eq!(stored.len(), 9, "one tag byte + the 8 payload bytes");
        assert_eq!(
            &payload_codec::decode(&stored).expect("tiny run decodes")[..],
            &[0x77u8; 8][..]
        );
    }

    /// A RANGE-sharded `wals` breaks the one-page-per-op assumption, and the
    /// covering arm is where it bites.
    ///
    /// That arm scans BACKWARDS from `byte_offset` for the newest chunk
    /// starting below it. Range-sharded, its key span can cross a tablet
    /// boundary, so it opens in the HIGHEST tablet in range — whose rows may
    /// all start at or after `byte_offset` and be filtered out server-side.
    /// The result is an EMPTY first page plus a continuation: "this tablet
    /// had no match", not "the scan is done". Read as one page it looks like
    /// there is no covering chunk, and a healthy segment surfaces as
    /// `WAL hole at {lsn}: no covering chunk in wals`.
    ///
    /// The split here lands INSIDE one segno's chunk_offset range, at 2000,
    /// with the fetch position at 1500: chunks 2000/3000 sit in the upper
    /// tablet and are all filtered out, so the covering chunk at 1000 is only
    /// reachable on the second page.
    #[tokio::test]
    async fn thin_lane_range_sharded_covering_read_pages_past_an_empty_tablet() {
        let table = scratch_table("wals_test_thin_range");
        let client = Arc::new(
            YsqlClient::connect_no_tls(pgcfg(), true)
                .await
                .expect("connect to local YB; set YB_TEST_HOST/PORT/USER/DB to override"),
        );
        let branch_id = BranchId::generate();
        let q = quote_relation(&table);
        // The post-migration shape: all-range PK, no hash columns, led by
        // `bucket_id` (the split boundaries have to be literals, and branch IDs
        // are minted at runtime). The split point is given in key order and
        // deliberately cuts one segno's chunk range in half.
        let create_sql = format!(
            "CREATE TABLE IF NOT EXISTS {q} (\
               bucket_id        SMALLINT NOT NULL,\
               branch_id        uuid     NOT NULL,\
               version          SMALLINT NOT NULL,\
               segno            BIGINT   NOT NULL,\
               chunk_offset     INTEGER  NOT NULL,\
               chunk_size       INT      NOT NULL,\
               data             BYTEA    NOT NULL,\
               last_modified_us BIGINT   NOT NULL,\
               PRIMARY KEY (bucket_id ASC, branch_id ASC, version ASC, segno ASC, chunk_offset ASC)\
             ) SPLIT AT VALUES ((0, '{branch_id}', 0, 1, 2000))"
        );
        recreate_scratch_table(&client, &table, &create_sql).await;

        // Chunks at 0, 1000, 2000, 3000 — each 1000 bytes, so the one at 1000
        // spans the fetch position 1500 and is the true covering chunk.
        for offset in [0i32, 1000, 2000, 3000] {
            let payload = vec![(offset / 1000) as u8 + 1; 1000];
            let stored = encode_run(
                &[PendingChunk {
                    segno: 1,
                    bucket_id: 0,
                    chunk_offset: offset,
                    chunk_size: 1000,
                    data: Arc::new(payload),
                    end_lsn: Lsn(0),
                }],
                payload_codec::Codec::Lz4,
            );
            client
                .execute(
                    &format!(
                        "INSERT INTO {q} (bucket_id, branch_id, version, segno, chunk_offset, \
                                          chunk_size, data, last_modified_us) \
                         VALUES (0, $1, {DEFAULT_VERSION}, 1, $2, 1000, $3, 0)"
                    ),
                    &[&branch_id, &offset, &stored.as_ref()],
                )
                .await
                .expect("insert chunk");
        }

        let (db_oid, oids) = ysql_client::lookup_table_oids(&client, "public", &[table.as_str()])
            .await
            .expect("look up scratch wals table OIDs");
        let thin_client = thin_client();
        let wals = thin_client
            .open_table(db_oid, oids[table.as_str()])
            .expect("open scratch wals table via the thin lane");
        assert_eq!(
            wals.num_hash_columns(),
            0,
            "scratch table should be range-sharded"
        );
        let thin = YbThinClient {
            client: thin_client,
            wals,
        };

        let got = fetch_chunks_thin(&thin, branch_id, 1, 1500)
            .await
            .expect("fetch_chunks_thin against a range-sharded wals");
        let offsets: Vec<u64> = got.iter().map(|c| c.chunk_offset).collect();
        println!("thin range-sharded fetch at 1500: chunks {offsets:?} (want [1000, 2000, 3000])");
        assert_eq!(
            offsets,
            vec![1000, 2000, 3000],
            "covering chunk at 1000 was lost — the reverse scan's first page came back \
             empty from the upper tablet and was taken as the end of the scan, which is \
             exactly the bogus `WAL hole` failure"
        );
        assert_eq!(
            got[0].end_offset, 2000,
            "the covering chunk must actually span the fetch position"
        );

        drop_wals_table(&client, &table).await;
    }

    /// The WAL write carries the lease to the leader. A flush submitted under
    /// a deadline that has passed is refused and leaves nothing in `wals`, and
    /// the refusal reads as the fence — `flush_batch` keys its resubmit-or-exit
    /// decision on that — rather than as a failure the consumer would retry
    /// against the same lapsed lease. The same batch under a live deadline
    /// lands.
    ///
    /// This is the stalled-writer case the local check cannot catch: the
    /// deadline is passed in rather than read from the process's fence, so the
    /// test reaches the write with its lease already over.
    #[tokio::test]
    async fn a_wal_flush_past_its_deadline_is_refused_and_leaves_nothing() {
        let table = scratch_table("wals_test_fence");
        let client = Arc::new(
            YsqlClient::connect_no_tls(pgcfg(), true)
                .await
                .expect("connect to local YB; set YB_TEST_HOST/PORT/USER/DB to override"),
        );
        make_wals_table(&client, &table).await;
        let (db_oid, oids) = ysql_client::lookup_table_oids(&client, "public", &[table.as_str()])
            .await
            .expect("look up scratch wals table OIDs");
        let thin_client = thin_client();
        let wals = thin_client
            .open_table(db_oid, oids[table.as_str()])
            .expect("open scratch wals table via the thin lane");
        let thin = YbThinClient {
            client: thin_client,
            wals,
        };

        let branch_id = BranchId::generate();
        let batch = vec![PendingChunk {
            segno: 1,
            bucket_id: 0,
            chunk_offset: 0,
            chunk_size: 64,
            data: Arc::new(vec![0xab; 64]),
            end_lsn: Lsn(SEG_SIZE + 64),
        }];
        let runs = coalesce_runs(&batch);
        let encoded: Vec<bytes::Bytes> = runs
            .iter()
            .map(|r| encode_run(&batch[r.clone()], payload_codec::Codec::Lz4))
            .collect();
        let rows_for_branch = || async {
            client
                .query_opt(
                    &format!(
                        "SELECT count(*) FROM {} WHERE branch_id=$1",
                        quote_relation(&table)
                    ),
                    &[&branch_id],
                )
                .await
                .expect("count")
                .map(|r| r.get::<_, i64>(0))
                .unwrap()
        };
        let now_us = SystemTime::now()
            .duration_since(UNIX_EPOCH)
            .unwrap()
            .as_micros() as u64;
        let minute_us = 60_000_000;

        let err = flush_batch_thin(
            &thin,
            branch_id,
            &batch,
            &runs,
            &encoded,
            yb_thin_client::WriteDeadline::EpochMicros(now_us - minute_us),
        )
        .await
        .expect_err("a flush under a lapsed deadline must be refused");
        assert!(
            server_fenced(&err),
            "the refusal must read as the fence, not a retryable failure: {err:#}"
        );
        assert_eq!(rows_for_branch().await, 0, "a refused flush must not land");

        flush_batch_thin(
            &thin,
            branch_id,
            &batch,
            &runs,
            &encoded,
            yb_thin_client::WriteDeadline::EpochMicros(now_us + minute_us),
        )
        .await
        .expect("a flush under a live deadline lands");
        assert_eq!(rows_for_branch().await, 1);

        drop_wals_table(&client, &table).await;
    }

    /// Live hot-path round trip: flush through the real producer/consumer,
    /// then fetch back through `YbSegmentReader` with no cache, so both hot
    /// paths run on the lane end to end.
    #[tokio::test]
    async fn thin_lane_flush_and_fetch_round_trip() {
        use tokio::io::AsyncReadExt;

        let table = scratch_table("wals_test_thin");
        let client = Arc::new(
            YsqlClient::connect_no_tls(pgcfg(), true)
                .await
                .expect("connect to local YB; set YB_TEST_HOST/PORT/USER/DB to override"),
        );
        make_wals_table(&client, &table).await;

        // Attach the direct lane to the scratch table. `make_wals_table`
        // creates it unqualified, so it lands in `public` — the production
        // path looks up `amp_storage.wals` instead (`lookup_wals_oids`).
        let (db_oid, oids) = ysql_client::lookup_table_oids(&client, "public", &[table.as_str()])
            .await
            .expect("look up scratch wals table OIDs");
        let thin_client = thin_client();
        let wals = thin_client
            .open_table(db_oid, oids[table.as_str()])
            .expect("open scratch wals table via the thin lane");

        let branch_id = BranchId::generate();
        let start = Lsn(SEG_SIZE); // segno 1, offset 0
        let mut state = TimelinePersistentState::empty();
        state.branch_id = branch_id;
        state.timeline_start_lsn = start;
        state.server.wal_seg_size = SEG_SIZE as u32;

        let _tmp = camino_tempfile::tempdir().expect("tempdir");
        let yb_wal = Arc::new(YbWalConn {
            system_client: client.clone(),
            table: table.clone(),
            cache: None,
            // No lifetime bound: the test wants the connection it opened for
            // as long as it runs. `Recycling::fixed` never rebuilds.
            yb_thinclient: yb_thin_client::Recycling::fixed(Arc::new(YbThinClient {
                client: thin_client,
                wals,
            })),
            codec: payload_codec::Codec::Lz4,
        });
        let mut storage =
            YbWalStorage::new(branch_id, &state, state.timeline_start_lsn, yb_wal.clone())
                .await
                .expect("YbWalStorage::new");

        // Distinctive position-dependent byte stream through the producer.
        let mut stream: Vec<u8> = Vec::new();
        let mut pos = start;
        for i in 0..8u8 {
            let len = 512 + (i as usize) * 33;
            let buf: Vec<u8> = (0..len)
                .map(|j| i.wrapping_mul(37).wrapping_add(j as u8))
                .collect();
            storage
                .upsert_writes(pos, &buf)
                .await
                .expect("upsert_writes");
            pos = Lsn(pos.0 + len as u64);
            stream.extend_from_slice(&buf);
        }
        // Baselines to prove each hot path actually ran on the direct
        // lane: its path-labeled metric advances only on the lane's arm,
        // so a silently-erroring lane would leave these flat while the
        // cross-checks below still passed.
        let insert_before = crate::metrics::YB_WAL_INSERT_SECONDS
            .with_label_values(&["yb_thin_client"])
            .get_sample_count();
        let fetch_before = crate::metrics::WAL_CACHE_YB_FETCH_SECONDS
            .with_label_values(&["yb_thin_client"])
            .get_sample_count();

        // Durability barrier: the consumer flushes through the direct
        // lane (one Perform per coalesced batch).
        storage.flush_wal().await.expect("flush_wal (thin lane)");
        assert!(
            crate::metrics::YB_WAL_INSERT_SECONDS
                .with_label_values(&["yb_thin_client"])
                .get_sample_count()
                > insert_before,
            "flush did not run on the thin lane"
        );

        // Cross-transport visibility: plain-YSQL rehydration sees the
        // thin-lane-written rows and lands exactly at the stream end.
        let lsn = YbWalStorage::load_max_offset(
            &client,
            &table,
            storage.branch_id,
            SEG_SIZE as usize,
            Lsn(0),
        )
        .await
        .expect("load_max_offset");
        assert_eq!(
            lsn, pos,
            "rehydrated end must equal the submitted stream end"
        );

        // Fetch with no cache: every read goes through the thin lane's
        // two-arm fan-out (2×WALS_BUCKETS read ops, ONE Perform, one
        // snapshot by construction).
        let mut reader = YbSegmentReader::new(
            yb_wal,
            branch_id,
            1,
            SEG_SIZE as usize,
            0,
            Arc::new(std::sync::atomic::AtomicU64::new(u64::MAX)),
        );
        let mut got = vec![0u8; stream.len()];
        reader
            .read_exact(&mut got)
            .await
            .expect("read_exact via thin lane");
        assert_eq!(
            got, stream,
            "thin-lane fetch must round-trip the flushed bytes"
        );
        assert!(
            crate::metrics::WAL_CACHE_YB_FETCH_SECONDS
                .with_label_values(&["yb_thin_client"])
                .get_sample_count()
                > fetch_before,
            "fetch did not run on the thin lane"
        );

        drop_wals_table(&client, &table).await;
    }
    /// The safekeeper never creates its `branch_state` row — the controller
    /// provisions it at branch create, before any daemon starts. If the row is
    /// missing, that is a wiring bug (started against a branch the controller
    /// never recorded), and the create path must say so rather than silently
    /// dropping the control file.
    ///
    /// Uses a fresh random branch id, so the UPDATE matches nothing and the
    /// existence probe comes back empty. Nothing is written, so nothing needs
    /// cleaning up.
    #[tokio::test]
    async fn control_file_create_requires_a_provisioned_row() {
        use crate::control_file::ControlFileStorage;
        use postgres_versioninfo::PgVersionId;
        use safekeeper_api::ServerInfo;

        let client = Arc::new(
            YsqlClient::connect_no_tls(pgcfg(), false)
                .await
                .expect("connect"),
        );
        let branch_id = BranchId::generate();
        let server = ServerInfo {
            pg_version: PgVersionId::from_full_pg_version(180_002),
            system_id: 0,
            wal_seg_size: 16 * 1024 * 1024,
        };
        let state = TimelinePersistentState::new(&branch_id, server, Lsn(0x1000)).unwrap();

        let err = ControlFileStorage::write_initial_state(client, &branch_id, state)
            .await
            .expect_err("an unprovisioned branch must not create its own row");
        assert!(
            err.to_string().contains("no branch_state row"),
            "expected the unprovisioned-branch bail, got: {err}"
        );
    }

    /// `safekeeper_initialized` is the safekeeper's presence predicate, and
    /// `write_row` is its only writer. A controller-seeded row must read back
    /// as "no control file yet", the creation write must flip the flag, and a
    /// second create must not rewrite the group.
    ///
    /// Mirrors the pageserver's `pageserver_initialized`. The safekeeper's
    /// group has a single writer, so the old `timeline_start_lsn IS NOT NULL`
    /// test happened to be sound — the flag removes the dependency on that
    /// staying true, which is exactly the assumption that broke in #552.
    #[tokio::test]
    async fn control_file_presence_is_the_initialized_flag() {
        use crate::control_file::ControlFileStorage;
        use postgres_versioninfo::PgVersionId;
        use safekeeper_api::ServerInfo;

        let client = Arc::new(
            YsqlClient::connect_no_tls(pgcfg(), false)
                .await
                .expect("connect"),
        );
        let branch_id = BranchId::generate();

        // The controller's seed: the row exists, both groups unpopulated.
        client
            .execute(
                "INSERT INTO amp_storage.branch_state (branch_id, pg_version) \
                 VALUES ($1, $2) ON CONFLICT (branch_id) DO NOTHING",
                &[&branch_id, &180_002i32],
            )
            .await
            .expect("seed the row");

        assert!(
            ControlFileStorage::restore_opt(client.clone(), &branch_id)
                .await
                .expect("restore must not error on a seeded row")
                .is_none(),
            "a seeded row carries no control file yet"
        );

        let server = ServerInfo {
            pg_version: PgVersionId::from_full_pg_version(180_002),
            system_id: 0,
            wal_seg_size: 16 * 1024 * 1024,
        };
        let state = TimelinePersistentState::new(&branch_id, server, Lsn(0x1000)).unwrap();
        ControlFileStorage::write_initial_state(client.clone(), &branch_id, state.clone())
            .await
            .expect("creation write");

        let (restored, floor) = ControlFileStorage::restore_opt(client.clone(), &branch_id)
            .await
            .expect("restore")
            .expect("the flag is set, so the control file is visible");
        assert_eq!(restored.timeline_start_lsn, state.timeline_start_lsn);
        assert_eq!(
            floor, state.timeline_start_lsn,
            "the pageserver has not written consistent_lsn, so the retention \
             floor falls back to the timeline start and all WAL is kept"
        );

        // Second create is a no-op — the group is already populated.
        let mut second = state.clone();
        second.timeline_start_lsn = Lsn(0x9000);
        ControlFileStorage::write_initial_state(client.clone(), &branch_id, second)
            .await
            .expect("re-create is a no-op, not an error");
        let (again, _) = ControlFileStorage::restore_opt(client.clone(), &branch_id)
            .await
            .expect("restore")
            .expect("still visible");
        assert_eq!(
            again.timeline_start_lsn, state.timeline_start_lsn,
            "the populated group is the source of truth"
        );

        client
            .execute(
                "DELETE FROM amp_storage.branch_state WHERE branch_id = $1",
                &[&branch_id],
            )
            .await
            .ok();
    }
}
