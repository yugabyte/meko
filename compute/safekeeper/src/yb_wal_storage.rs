//! YugabyteDB-mirrored WAL storage.
//!
//! The safekeeper keeps no WAL on the local filesystem. Every WAL write is
//! persisted to a YB table (`safekeeper.wals`) — the durable copy — and reads
//! are served from YB (`WalReader` opens a [`YbSegmentReader`]) fronted by an
//! in-memory hot cache for recent WAL. Durability is the WAL landing in YB: a
//! commit is acked only once its bytes are INSERT'd (see the ack model under
//! "Batching" below).
//!
//! [`YbWalStorage`] wraps a [`WalBookkeeping`] — an in-memory tracker of the
//! WAL LSN watermarks and record boundaries, with no filesystem interaction.
//!
//! The table (`safekeeper.wals`, defined in the controller
//! migration) holds one row per coalesced run of a consumer batch:
//! contiguous chunks of a segment are concatenated into a single row
//! before the INSERT, so the typical batch is ONE row; runs break at
//! segment boundaries and at byte discontinuities.
//!
//! Bucketing: the hash key is `(branch_id, bucket_id)`, so a branch's WAL
//! spreads across [`WALS_BUCKETS`] tablets. `bucket_id` is assigned per
//! consumer BATCH by [`consumer_main`], rotating through the buckets one
//! batch at a time. One bucket per batch means every batch targets ONE
//! tablet: the tsclient lane applies it as one atomic write batch, and the
//! YSQL lane's multi-VALUES INSERT hits the single-shard transaction fast
//! path (no distributed txn), while write load rotates across all tablets.
//! Readers can't derive a chunk's bucket, so index-tight reads add a
//! `bucket_id IN (0..WALS_BUCKETS-1)` hash-key list (batched per-bucket
//! index probes).
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
//! ([`coalesce_runs`]), writes it (tsclient lane or one multi-row
//! `INSERT ... VALUES (...), (...)` through [`YsqlClient`]), and
//! publishes the highest end_lsn of each successful batch onto a
//! `tokio::sync::watch` watermark. This lets the producer keep pushing
//! the *next* batch while the previous one is in flight, which the
//! single-threaded design couldn't do (every `flush_pending` held
//! `&mut self` across the YB round-trip).
//!
//! Durability is acked off the ingest path. `flush_lsn()` is capped at
//! `min(inner.flush_lsn, durable_lsn)`, so nothing is ever advertised past
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
//! On a batch INSERT failure (after `YsqlClient`'s own auto-retry has
//! given up — e.g. persistent auth / schema / connectivity failure),
//! the consumer publishes [`DurableState::Failed`] onto the watermark
//! and exits. The producer's next `tx.send()` then fails (rx dropped),
//! `write_wal` returns the captured error, the walproposer reconnects,
//! and the safekeeper handles the rest on that reconnect handshake.
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
use postgres_ffi::XLogSegNo;
use tokio::io::{AsyncRead, ReadBuf};
use tokio::sync::{mpsc, watch};
use utils::id::BranchId;
use utils::lsn::Lsn;
use yb_tokio_postgres::types::ToSql;

use crate::metrics::WalStorageMetrics;
use crate::state::TimelinePersistentState;
use crate::wal_cache::WalCache;
use crate::wal_storage::{Storage, WalBookkeeping};

// Re-export the auto-reconnecting Postgres client wrapper shared with
// the pageserver's `YbStore`. See `ysql_client::YsqlClient` for the
// reconnect / retry-once semantics.
pub use ysql_client::YsqlClient;

/// The single YB table the safekeeper mirrors WAL into. Hardcoded —
/// the connection config carries only host/port/credentials, never a
/// table name. Schema-qualified (`safekeeper.wals`); every query
/// references it fully so it doesn't depend on the connection's
/// `search_path`.
const WALS_TABLE: &str = "safekeeper.wals";

/// Number of placement buckets in `safekeeper.wals`. `bucket_id` is the
/// second hash-key column; the consumer stamps each BATCH with one
/// bucket, rotating `(counter + 1) % WALS_BUCKETS` per batch, so a
/// branch's write load spreads across tablets instead of funneling into
/// one, while each batch stays single-tablet-atomic.
const WALS_BUCKETS: i16 = 10;

/// `bucket_id IN (0,1,...,WALS_BUCKETS-1)` for interpolation into `wals`
/// queries. Readers can't derive a chunk's bucket (assignment is
/// per consumer batch at write time, placement-only), so every
/// index-tight read names every bucket; YSQL executes a hash-key IN-list
/// as batched per-bucket index probes, not a scan.
fn bucket_in_list() -> String {
    let ids: Vec<String> = (0..WALS_BUCKETS).map(|b| b.to_string()).collect();
    format!("bucket_id IN ({})", ids.join(","))
}

/// Handles to the YB connections that mirror live WAL into
/// `safekeeper.wals`. Constructed at safekeeper boot via
/// [`YbWalConn::from_conf`] and shared across timelines via
/// `Arc<YbWalConn>`. Used by the WAL writer ([`YbWalStorage::new`]),
/// the WAL reader ([`YbSegmentReader::new`]), and the control-file
/// storage ([`crate::control_file::YbControlFileStorage`]).
///
/// Three independent connections, each with its own `application_name`
/// for attribution in YB's `pg_stat_activity`:
/// - [`Self::fpu_client`] — the only one with `yb_enable_upsert_mode =
///   true`. Carries the hot-path WAL chunk INSERTs and the
///   `control_file::create_new` INSERT (the latter needs the GUC so
///   the YsqlClient retry loop replays cleanly under
///   transient/connection-drop errors; without it, the integration
///   `restart_wal_replay` scenario regresses with a PS-side "relation
///   already exists" panic at WAL ingest).
/// - [`Self::read_client`] — hot-path chunk SELECTs for the WAL sender
///   ([`YbSegmentReader`]) and nothing else. Keeping streaming reads
///   off the system lane means a long control-file write or GC DELETE
///   can't stall the read-back path that walproposer commits depend
///   on.
/// - [`Self::system_client`] — everything else: startup rehydration
///   SELECTs, WAL GC DELETEs, idempotent control-file ops (restore /
///   persist / delete / list). No upsert GUC.
pub struct YbWalConn {
    /// Fast Path Upsert lane (`yb_enable_upsert_mode = true`). Used by
    /// the per-timeline consumer task in [`consumer_main`] for
    /// batched chunk INSERTs into `safekeeper.wals` and by
    /// `control_file::create_new` for the one-shot
    /// `safekeeper.timeline_state` INSERT.
    pub fpu_client: Arc<YsqlClient>,
    /// Hot-read lane. Used exclusively by [`YbSegmentReader`] for
    /// chunk fetches that feed the WAL sender. No upsert GUC.
    pub read_client: Arc<YsqlClient>,
    /// System / background lane (no upsert GUC). Used for startup
    /// rehydration ([`YbWalStorage::load_write_lsn`] /
    /// [`load_min_segno`]), the GC DELETE
    /// ([`YbWalStorage::remove_up_to`]), and the idempotent
    /// control-file ops (`restore_new` SELECT, `persist` UPDATE,
    /// `delete` DELETE, `list_all_branch_ids` SELECT). The non-idempotent
    /// `create_new` INSERT runs on the FPU lane.
    pub system_client: Arc<YsqlClient>,
    /// Name of the YB table that backs the WAL mirror. Always
    /// [`WALS_TABLE`] (`safekeeper.wals`); the field stays so the SQL
    /// builders below can interpolate it.
    pub table: String,
    /// In-memory cache of recent WAL chunks, shared between the
    /// writer ([`YbWalStorage::upsert_writes`]) and the reader
    /// ([`YbSegmentReader`]). `None` when the operator has set
    /// `--wal-cache-size-mib 0` to disable the cache entirely (every
    /// WAL read then hits YB).
    pub cache: Option<Arc<std::sync::Mutex<WalCache>>>,
    /// Thin-RPC lane (`hot_path = "tsclient"`): pure-Rust
    /// `yb_tsclient` handle for `safekeeper.wals`, speaking the pggate
    /// RPC protocol to a tserver's RPC port. `None` on the default YSQL
    /// path. Opened and health-checked at startup; carries the hot-path
    /// WAL write/read dispatch. Control file,
    /// GC, and rehydration stay on the YSQL lanes regardless.
    pub yb_tsclient: Option<Arc<YbTsClient>>,
}

/// See [`YbWalConn::yb_tsclient`] and `compute/architecture/safekeeper.md`.
pub struct YbTsClient {
    pub client: Arc<yb_tsclient::TsClient>,
    pub wals: Arc<yb_tsclient::TableHandle>,
}

/// Exports `yb_tsclient` health events (which its internal retries
/// would otherwise absorb invisibly) into this daemon's metrics.
struct TsClientLaneObserver;

impl yb_tsclient::ClientObserver for TsClientLaneObserver {
    fn on_session_recovery(&self) {
        crate::metrics::YB_WAL_SESSION_RECOVERIES.inc();
    }
    fn on_call_timeout(&self) {
        crate::metrics::YB_WAL_CALL_TIMEOUTS.inc();
    }
}

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
        // Three independent connections, each with a distinct
        // application_name so they're attributable in YB's
        // pg_stat_activity. The FPU lane is the only one with
        // `yb_enable_upsert_mode = true` — replayed same-offset chunk
        // INSERTs are idempotent by construction, so skipping YB's
        // read-before-write check is safe there. Reads and background
        // writes (GC DELETE, control-file UPDATE/INSERT/DELETE) run
        // without the GUC.
        // Three independent connections with lane-specific GUCs:
        // - FPU lane (`yb_enable_upsert_mode = true`): hot-path WAL
        //   chunk INSERTs and `control_file::create_new` INSERT.
        //   `create_new` runs here because it's the only INSERT
        //   reaching `safekeeper.timeline_state`; without the GUC the
        //   YsqlClient retry-on-transient/connection-drop loop can't
        //   prove the in-flight write didn't already commit, and a
        //   blind replay fails with duplicate-key — empirically this
        //   breaks the `restart_wal_replay` integration scenario with a
        //   downstream pageserver "relation already exists" panic.
        // - Read lane (no GUC): exclusively the segment reader.
        // - System lane (no GUC): startup SELECTs (`load_write_lsn` /
        //   `load_min_segno` / `list_all_branch_ids`), the GC DELETE
        //   (`remove_up_to`), and idempotent control-file ops
        //   (`restore_new` SELECT, `persist` UPDATE, `delete` DELETE).
        let fpu_client = Arc::new(
            ysql_client::YsqlClient::from_yb_config(yb, "safekeeper:yb_wal_fpu", true)
                .await
                .with_context(|| format!("connect WAL FPU lane at {endpoint}"))?,
        );
        let read_client = Arc::new(
            ysql_client::YsqlClient::from_yb_config(yb, "safekeeper:yb_wal_reads", false)
                .await
                .with_context(|| format!("connect WAL read lane at {endpoint}"))?,
        );
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
        let yb_tsclient = match yb.hot_path {
            ysql_client::HotPath::Ysql => None,
            ysql_client::HotPath::TsClient => Some(
                Self::connect_tsclient(yb, &system_client)
                    .await
                    .context("thin-RPC hot path init (hot_path = \"tsclient\")")?,
            ),
        };
        Ok(Some(Arc::new(YbWalConn {
            fpu_client,
            read_client,
            system_client,
            table: WALS_TABLE.to_string(),
            cache,
            yb_tsclient,
        })))
    }

    /// One-time catalog lookup for the tsclient hot-path lane: the
    /// database OID and `safekeeper.wals`'s pg_class OID.
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

    /// Connects the thin-RPC (`yb_tsclient`) lane and opens
    /// `safekeeper.wals`, validating tserver-RPC connectivity and table
    /// layout in one shot. Fail-fast at startup.
    async fn connect_tsclient(
        yb: &ysql_client::YbConnConfig,
        system_client: &Arc<ysql_client::YsqlClient>,
    ) -> anyhow::Result<Arc<YbTsClient>> {
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
        let client = yb_tsclient::TsClient::connect(yb_tsclient::TsClientConfig {
            tserver_addrs,
            read_sessions,
            write_sessions,
            sessions_per_conn,
            ..Default::default()
        })
        .await
        .context("connecting the thin-RPC (tsclient) client")?;
        client.set_observer(Arc::new(TsClientLaneObserver));
        let wals = client
            .open_table(db_oid, wals_oid)
            .await
            .with_context(|| format!("opening {WALS_TABLE} via the tsclient lane"))?;
        tracing::info!(
            "thin-RPC (tsclient) hot path ready: {WALS_TABLE} opened (table_id {}) on {} \
             ({read_sessions} read + {write_sessions} write sessions, {sessions_per_conn} per \
             connection, across {tserver_count} configured tserver(s))",
            wals.table_id,
            client.connected_addr(),
        );
        Ok(Arc::new(YbTsClient { client, wals }))
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
    /// one tablet. Placement only, not identity: tsclient-lane retries
    /// replay the same stamped chunks, so a chunk keeps its bucket across
    /// retried flushes.
    bucket_id: i16,
    chunk_offset: i64,
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

/// Durability state published by the consumer onto the watch channel.
#[derive(Clone, Debug)]
pub enum DurableState {
    /// Every `PendingChunk` whose `end_lsn <= self.0` has been durably
    /// INSERT'd into YugabyteDB.
    Healthy(Lsn),
    /// The consumer task hit a non-recoverable error and stopped
    /// processing. The string is the rendered error chain for
    /// propagation up through `flush_wal` / `write_wal`.
    Failed(String),
}

/// How far a YB flush should go — the arg to [`crate::wal_storage::Storage::yb_flush`].
#[derive(Clone, Copy, Debug)]
pub enum YbFlush {
    /// Wake the consumer, wait for the batch to be durable in `safekeeper.wals`,
    /// then advance the local watermark. The synchronous durable-commit
    /// boundary used by `handle_elected` / sync-safekeepers.
    NotifyAndWait,
    /// Wake the consumer, then advance the local watermark without waiting.
    /// Steady-state commit; durability is acked later off the ingest path.
    Notify,
    /// Advance the local watermark only, so `flush_lsn()` reaches the current
    /// `durable_lsn`. The durability-ack path, reacting to a flush already done.
    LocalAdvanceOnly,
}

pub struct YbWalStorage {
    inner: WalBookkeeping,
    /// Background-write lane (no upsert GUC). Used by
    /// [`Self::remove_up_to`] for the GC DELETE. The hot-path INSERTs
    /// run on the FPU lane owned by the consumer task; SELECTs run on
    /// the read lane (held by [`YbSegmentReader`]).
    ysql_system_client: Arc<YsqlClient>,
    branch_id: String,
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
    /// deleted, or never inserted on this timeline). Seeded at
    /// construction from `min(segno)` in the table so the first DELETE
    /// after a safekeeper restart doesn't run unbounded-low; advanced
    /// after each successful `remove_up_to(N)` to `N`. Pinned in an
    /// `Arc<AtomicU64>` because `remove_up_to` returns a `'static`
    /// future.
    delete_lower_bound: Arc<AtomicU64>,
    /// Highest LSN whose chunks are confirmed in `wals`. Advanced by
    /// the consumer after each successful batch INSERT. Read by
    /// `flush_lsn()` to cap the externally-reported flush watermark at
    /// true YB durability — the in-memory `inner.flush_record_lsn` can
    /// otherwise be moved ahead of YB by `truncate_wal(end_pos)` when
    /// `end_pos > submitted_max_lsn`, opening a permanent gap. Initialised
    /// from `load_write_lsn` at construction (YB's current max chunk end).
    durable_lsn: Arc<AtomicU64>,
    /// Kept so the consumer task isn't dropped immediately on
    /// `YbWalStorage` drop; closing `tx` is what signals shutdown.
    _consumer_handle: tokio::task::JoinHandle<()>,
}

impl YbWalStorage {
    pub async fn new(
        branch_id: &BranchId,
        state: &TimelinePersistentState,
        yb_wal: Arc<YbWalConn>,
    ) -> Result<Self> {
        // `read_client` is reserved for `YbSegmentReader` (owned via
        // the shared `Arc<YbWalConn>`), so we don't clone it here —
        // all non-streaming reads on this construction path use the
        // system lane.
        let fpu_client = yb_wal.fpu_client.clone();
        let system_client = yb_wal.system_client.clone();
        let table_name = yb_wal.table.clone();
        let cache = yb_wal.cache.clone();
        let wal_seg_size = state.server.wal_seg_size as usize;
        let branch_id_str = branch_id.to_string();

        // Rehydrate `write_lsn` from YB rather than scanning local FS. Take
        // the highest `chunk_offset + chunk_size` across all segnos for this
        // branch as the end-of-WAL position. Startup work →
        // system lane so it doesn't share the streaming-read connection.
        let max_offset = Self::load_write_lsn(
            &system_client,
            &table_name,
            &branch_id_str,
            wal_seg_size,
            state.timeline_start_lsn,
        )
        .await?;

        // `load_write_lsn` returns the max BYTE offset persisted, which
        // lands mid-record when an ungraceful compute shutdown left a torn
        // trailing page (#61). Treating that as the durable end-of-WAL
        // makes the interpreted reader decode into the torn page and panic.
        // Replay the decoder to the last complete record boundary <=
        // max_offset — where Postgres crash recovery also stops — and use
        // THAT as write_lsn, discarding the torn tail (the compute
        // re-streams from here and overwrites it). On scan failure, fall
        // back to the raw offset so startup is never worse than before.
        let write_lsn = {
            let pg_version = postgres_ffi::PgMajorVersion::try_from(state.server.pg_version)
                .unwrap_or(postgres_ffi::PgMajorVersion::PG18);
            let src = YbWalSource {
                client: &system_client,
                table: &table_name,
                branch_id: &branch_id_str,
                seg: wal_seg_size as u64,
            };
            // YB keeps no persisted record-boundary flush_lsn to start from
            // (the upstream safekeeper read it from the control file), so
            // anchor on a recent fresh-record page at or below the raw end,
            // then scan forward to the true end of WAL via `find_end_of_wal`
            // (the same scan Neon ran over local segments). On any scan
            // failure, fall back to the raw offset so startup is never worse.
            use postgres_ffi::wal_record_boundary::{find_anchor, find_end_of_wal};
            let scanned = if max_offset <= state.local_start_lsn {
                Ok(max_offset)
            } else {
                // `local_start_lsn` is where this safekeeper's persisted WAL
                // begins (see `TimelinePersistentState`) and a guaranteed record
                // boundary; pass it as the anchor-scan floor so a record spanning
                // back to a mid-page start (wal_craft's XLOG_SWITCH padding)
                // doesn't overshoot below the store (#181). It's the tighter,
                // correct bound: `timeline_start_lsn` can sit below where this SK
                // actually has WAL.
                match find_anchor(&src, max_offset, state.local_start_lsn).await {
                    Ok(anchor) => find_end_of_wal(&src, anchor, pg_version).await,
                    Err(e) => Err(e),
                }
            };
            match scanned {
                Ok(end) if end < max_offset => {
                    tracing::warn!(
                        branch_id = %branch_id_str,
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
                    // position is the next segment. find_end_of_wal decodes the
                    // switch and reports that next-segment LSN; honor it rather
                    // than clamping back to max_offset — clamping leaves
                    // write_lsn inside the padding, where the compute resumes
                    // streaming, the reader hits `invalid xl_tot_len 0`, and the
                    // safekeeper wedges (#181).
                    tracing::info!(
                        branch_id = %branch_id_str,
                        max_offset = %max_offset,
                        switch_end = %end,
                        "YbWalStorage::new — trailing XLOG_SWITCH advances end-of-WAL past segment padding",
                    );
                    end
                }
                Ok(_) => max_offset,
                Err(e) => {
                    tracing::warn!(
                        branch_id = %branch_id_str,
                        %max_offset,
                        "YbWalStorage::new — end-of-WAL record-boundary scan failed, using raw max offset: {e:#}",
                    );
                    max_offset
                }
            }
        };
        // Physically discard the torn tail so the table ends exactly at the
        // valid boundary — otherwise the dropped rows linger past
        // `durable_lsn` (violating `truncate_wal`'s "no rows past end_pos"
        // invariant) and would be re-counted by `load_write_lsn` on the
        // next restart. Best-effort: the reader is already bounded by
        // `write_lsn`, so a failed cleanup is harmless and retried next
        // restart.
        if write_lsn < max_offset {
            if let Err(e) = truncate_wals_above(
                &system_client,
                &table_name,
                &branch_id_str,
                wal_seg_size,
                write_lsn,
            )
            .await
            {
                tracing::warn!(
                    branch_id = %branch_id_str,
                    %write_lsn,
                    %max_offset,
                    "YbWalStorage::new — failed to delete torn WAL tail (harmless; retried next restart): {e:#}",
                );
            }
        }
        tracing::info!(
            branch_id = %branch_id_str,
            %write_lsn,
            cfile_local_start_lsn = %state.local_start_lsn,
            cfile_timeline_start_lsn = %state.timeline_start_lsn,
            "YbWalStorage::new — rehydrated write_lsn from safekeeper.wals \
             (last valid record boundary; falls back to timeline_start_lsn on empty)",
        );

        // WalBookkeeping holds the LSN tracker + WAL decoder for this
        // stream — durability is our job.
        let inner = WalBookkeeping::new(branch_id, state, write_lsn)?;

        // Producer/consumer plumbing. The watermark starts at
        // `write_lsn` so an immediately-following `flush_wal()` with
        // `submitted_max_lsn == write_lsn` returns at once (nothing to
        // wait for).
        let (tx, rx) = mpsc::channel::<PendingChunk>(CHUNK_CHANNEL_CAPACITY);
        let (watermark_tx, watermark_rx) = watch::channel(DurableState::Healthy(write_lsn));
        let flush_notify = Arc::new(tokio::sync::Notify::new());
        let durable_lsn = Arc::new(AtomicU64::new(write_lsn.0));
        let consumer_handle = tokio::spawn(consumer_main(
            rx,
            fpu_client,
            yb_wal.yb_tsclient.clone(),
            table_name.clone(),
            branch_id_str.clone(),
            watermark_tx,
            flush_notify.clone(),
            durable_lsn.clone(),
            cache.clone(),
        ));

        // Seed the DELETE lower bound from YB so post-restart removals
        // don't scan/tombstone the empty range below the first surviving
        // segno. Cheap: one tablet lookup keyed on branch.
        let min_segno = Self::load_min_segno(&system_client, &table_name, &branch_id_str).await?;

        let me = YbWalStorage {
            inner,
            ysql_system_client: system_client,
            branch_id: branch_id_str,
            table_name,
            wal_seg_size,
            tx,
            watermark_rx,
            flush_notify,
            submitted_max_lsn: write_lsn,
            durable_wait_log_every: YB_DURABLE_WAIT_LOG_EVERY,
            durable_wait_max: YB_DURABLE_WAIT_MAX,
            cache,
            delete_lower_bound: Arc::new(AtomicU64::new(min_segno)),
            durable_lsn,
            _consumer_handle: consumer_handle,
        };

        // No post-crash row scrub: `load_write_lsn` already takes the
        // max chunk-end across the table, so any committed-but-unacked
        // rows from a crashed run are absorbed as durable rather than
        // discarded. yb-amp has no truncation-past-durable case that
        // would justify a DELETE here.
        Ok(me)
    }

    /// Find the end of the persisted WAL with one max-end probe per
    /// bucket: each bucket's top row (highest `segno`, then highest
    /// `chunk_offset`) yields its end offset, and the global max across
    /// buckets is the end of WAL. Falls back to `commit_lsn` when the
    /// table is empty (fresh timeline). Startup-only, so a loop of
    /// WALS_BUCKETS single-row index probes is fine.
    ///
    /// Why `ORDER BY segno DESC, chunk_offset DESC LIMIT 1` gives the
    /// bucket's max end: in general max(chunk_offset + chunk_size) for
    /// the top segno is NOT the last row's end — an earlier row could be
    /// longer and reach further. But rows never overlap (the writer
    /// never produces overlapping chunks and `truncate_wal` /
    /// `truncate_wals_above` keep it that way), so within a segno the
    /// row with the highest `chunk_offset` necessarily also has the
    /// highest `chunk_offset + chunk_size`.
    ///
    /// Why a simple max is safe (no hole detection): every consumer
    /// batch is stamped with ONE bucket, so a batch targets one tablet
    /// and lands atomically on both write lanes — one atomic write batch
    /// on the tsclient lane, a single-statement
    /// (single-shard) INSERT on YSQL — and the consumer writes batches
    /// sequentially, one at a time. Batch-atomic + sequential batches ⇒
    /// the durable row set is always a contiguous prefix of the WAL
    /// stream; no bucket's max end can sit above a hole. If either
    /// property changes (a batch spanning tablets again, or concurrent
    /// in-flight batches), hole detection must return — git history has
    /// the hole-aware `contiguous_end` tail scan this replaced.
    async fn load_write_lsn(
        client: &YsqlClient,
        table: &str,
        branch_id: &str,
        wal_seg_size: usize,
        commit_lsn: Lsn,
    ) -> Result<Lsn> {
        let mut max_end: Option<u64> = None;
        for bucket in 0..WALS_BUCKETS {
            let sql = bake_branch_id(
                &format!(
                    "SELECT segno, chunk_offset + chunk_size AS end_off FROM {} \
                     WHERE branch_id = $1 AND bucket_id = {bucket} \
                     ORDER BY segno DESC, chunk_offset DESC \
                     LIMIT 1",
                    quote_relation(table)
                ),
                branch_id,
            );
            let row = client
                .query_opt(&sql, &[])
                .await
                .context("YbWalStorage: rehydrate write_lsn from YB (bucket max-end probe)")?;
            if let Some(row) = row {
                let segno: i64 = row.get("segno");
                let end_off: i64 = row.get("end_off");
                let end = (segno as u64) * (wal_seg_size as u64) + end_off as u64;
                max_end = Some(max_end.map_or(end, |m| m.max(end)));
            }
        }
        Ok(max_end.map(Lsn).unwrap_or(commit_lsn))
    }

    /// Lowest segno still present in `wals` for this timeline. Used to
    /// seed [`Self::delete_lower_bound`] at construction so the first
    /// `remove_up_to(N)` DELETE after a safekeeper restart is bounded
    /// from below rather than scanning from 0.
    async fn load_min_segno(client: &YsqlClient, table: &str, branch_id: &str) -> Result<u64> {
        // The bucket list completes the hash key for every bucket, so the
        // aggregate hash-routes to per-bucket index probes rather than
        // scanning the whole branch. min(segno) is index-tight: segno is the
        // leading range key, so each bucket's minimum is its first index row.
        let sql = bake_branch_id(
            &format!(
                "SELECT min(segno) AS m FROM {} \
                 WHERE branch_id = $1 AND {}",
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
            .context("YbWalStorage: rehydrate min(segno) from YB")?;
        let m: Option<i64> = row.and_then(|r| r.get("m"));
        Ok(m.map(|v| v as u64).unwrap_or(0))
    }

    fn inner_pg_version_id(&self) -> postgres_versioninfo::PgVersionId {
        self.inner.pg_version_id()
    }

    fn inner_system_id(&self) -> u64 {
        self.inner.system_id()
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
            // INSERT'd into `safekeeper.wals`. Populating it here
            // pre-INSERT (the previous behaviour) let the WAL sender
            // stream chunks to the pageserver that the safekeeper had
            // not yet flushed to YB — and on safekeeper crash before
            // the consumer drained, the pageserver was left holding a
            // persisted disk_consistent_lsn for WAL the restarted SK
            // could no longer serve.

            let pc = PendingChunk {
                segno,
                bucket_id: 0, // placeholder; consumer stamps the batch bucket
                chunk_offset: offset as i64,
                chunk_size: take as i32,
                data,
                end_lsn,
            };

            // tx.send().await may block on backpressure (bounded channel).
            // The walproposer side stalls here until the consumer drains —
            // intended behaviour when YB can't keep up.
            if self.tx.send(pc).await.is_err() {
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
        loop {
            let res = tokio::time::timeout(
                self.durable_wait_log_every,
                rx.wait_for(|state| match state {
                    DurableState::Healthy(d) => *d >= target,
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
                    let durable = match self.watermark_rx.borrow().clone() {
                        DurableState::Healthy(d) => d,
                        DurableState::Failed(msg) => anyhow::bail!(msg),
                    };
                    if started.elapsed() >= self.durable_wait_max {
                        anyhow::bail!(
                            "YB WAL durability wait timed out after {:?}: target {target},                              durable {durable} (YB slow or wals tablet splitting?);                              dropping the connection so the timeline lock is released",
                            started.elapsed(),
                        );
                    }
                    tracing::warn!(
                        "still waiting for YB WAL durability: target {target}, durable                          {durable}, elapsed {:?}",
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
    fn write_lsn(&self) -> Lsn {
        self.inner.write_lsn()
    }

    fn flush_lsn(&self) -> Lsn {
        // Cap at YB-confirmed durability. `inner.flush_lsn()` can be ahead
        // when `truncate_wal(end_pos)` ran with `end_pos > durable_lsn`
        // (ProposerElected with `start_streaming_at` past the persisted tail);
        // returning the uncapped value to the walproposer
        // is what creates the WAL gap forensic / 2026-05-29.
        let inner_lsn = self.inner.flush_lsn();
        let durable = Lsn(self.durable_lsn.load(Ordering::Acquire));
        std::cmp::min(inner_lsn, durable)
    }

    async fn initialize_first_segment(&mut self, init_lsn: Lsn) -> Result<()> {
        // Generate the standard PG segment header + zero padding, then push
        // it through the consumer just like a regular write. Wait for the
        // consumer to flush before returning so the 16 MiB header is
        // durable by the time the caller sees success. One-shot
        // (per-timeline init), not on the per-commit hot path.
        let segno = init_lsn.segment_number(self.wal_seg_size);
        let major_pg_version = postgres_ffi::PgMajorVersion::try_from(self.inner_pg_version_id())
            .map_err(|e| anyhow::anyhow!("invalid pg version: {e:?}"))?;
        let wal_seg = postgres_ffi::generate_wal_segment(
            segno,
            self.inner_system_id(),
            major_pg_version,
            init_lsn,
        )?;
        let end_lsn = Lsn(init_lsn.0 + wal_seg.len() as u64);
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
        self.inner.initialize_first_segment(init_lsn).await
    }

    async fn write_wal(&mut self, startpos: Lsn, buf: &[u8]) -> Result<()> {
        // Push chunks onto the consumer's channel. Durability is not
        // observed yet — the caller must call `flush_wal` to wait for it.
        self.upsert_writes(startpos, buf).await?;
        self.inner.write_wal(startpos, buf).await
    }

    async fn truncate_wal(&mut self, end_pos: Lsn) -> Result<()> {
        // Drain everything the consumer has in flight first. Otherwise a
        // buffered chunk for an LSN past `end_pos` could be flushed after
        // the DELETE and silently resurrect data the walproposer intended
        // to discard. Wake the consumer in case it's accumulating.
        self.flush_notify.notify_one();
        self.wait_for_durable(self.submitted_max_lsn).await?;

        // Refuse to advance the truncation point past confirmed YB
        // durability. `inner.truncate_wal(end_pos)` sets `flush_record_lsn
        // = end_pos` unconditionally, so accepting an `end_pos` past
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
        // Physically drop any WAL past `end_pos` before resetting the
        // in-memory watermarks. In the steady single-SK case `end_pos ==
        // durable` and there's nothing past it (no-op), but after a torn
        // tail (find_end_of_wal capped flush_lsn below the raw max on
        // restart) or a multi-SK divergence, rows can sit past `end_pos`;
        // leaving them would let `load_write_lsn` re-count them on the
        // next restart and could resurface a partial chunk to a reader.
        truncate_wals_above(
            &self.ysql_system_client,
            &self.table_name,
            &self.branch_id,
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
        self.inner.truncate_wal(end_pos).await
    }

    /// Flush the consumer's queue, then advance the local inner watermark; the
    /// mode selects how far to go (see [`YbFlush`]). `flush_lsn()` stays capped
    /// at `durable_lsn` regardless, so `append_response` only ever advertises
    /// durable WAL.
    async fn yb_flush(&mut self, mode: YbFlush) -> Result<()> {
        match mode {
            YbFlush::NotifyAndWait => {
                self.flush_notify.notify_one();
                self.wait_for_durable(self.submitted_max_lsn).await?;
            }
            YbFlush::Notify => self.flush_notify.notify_one(),
            YbFlush::LocalAdvanceOnly => {}
        }
        self.inner.flush_wal().await
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
        let inner_fut = self.inner.remove_up_to(segno_up_to);
        let system_client = self.ysql_system_client.clone();
        let table = self.table_name.clone();
        let branch_id = self.branch_id.clone();
        let cache = self.cache.clone();
        let delete_lower_bound = self.delete_lower_bound.clone();
        async move {
            // Drop cached chunks for segnos that are about to be DELETE'd
            // out of `safekeeper.wals`. Done before the DB delete so a
            // concurrent reader can't pick up a cached chunk that is no
            // longer durable. Reads that beat us here race against the
            // existing chunks just as they always would.
            if let Some(c) = &cache {
                c.lock().unwrap().remove_segnos_below(segno_up_to);
            }
            inner_fut.await?;
            // Bound the DELETE on both sides. The lower bound is the
            // segno boundary advanced by the previous successful call
            // (or `min(segno)` at startup) — rows below it are known to
            // be gone, so naming them in the WHERE clause turns what was
            // an unbounded-low range scan into a tight one. Idempotent if
            // `segno_up_to <= lower`: the predicate is unsatisfiable and
            // the DELETE is a no-op.
            // The bucket list completes the hash key for every bucket, so the
            // DELETE hash-routes to a per-bucket index range on `segno`
            // instead of scanning the branch across all tablets.
            let lower = delete_lower_bound.load(Ordering::Relaxed);
            let sql = bake_branch_id(
                &format!(
                    "DELETE FROM {} WHERE branch_id = $1 AND {} \
                     AND segno >= $2 AND segno < $3",
                    quote_relation(&table),
                    bucket_in_list()
                ),
                &branch_id,
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

    fn close(&mut self) {
        self.inner.close();
    }

    fn get_metrics(&self) -> WalStorageMetrics {
        self.inner.get_metrics()
    }
}

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

/// `WalSource` over the `wals` table, for the end-of-WAL record-boundary
/// scan run once at [`YbWalStorage::new`]. Plain binds on the system lane
/// — not a hot path.
struct YbWalSource<'a> {
    client: &'a YsqlClient,
    table: &'a str,
    branch_id: &'a str,
    seg: u64,
}

impl postgres_ffi::wal_record_boundary::WalSource for YbWalSource<'_> {
    fn seg(&self) -> u64 {
        self.seg
    }

    async fn chunk_at(&self, lsn: Lsn) -> anyhow::Result<(Lsn, Vec<u8>)> {
        let segno = (lsn.0 / self.seg) as i64;
        let off = (lsn.0 % self.seg) as i64;
        // The bucket of a chunk can't be derived, so probe all buckets
        // (hash-key IN-list = batched index probes) and take the highest
        // covering offset across them.
        let sql = format!(
            "SELECT chunk_offset, data FROM {} \
             WHERE branch_id = $1 AND {} AND segno = $2 AND chunk_offset <= $3 \
             ORDER BY chunk_offset DESC LIMIT 1",
            quote_relation(self.table),
            bucket_in_list()
        );
        let params: [&(dyn ToSql + Sync); 3] = [&self.branch_id, &segno, &off];
        let row = self
            .client
            .query_opt(&sql, &params)
            .await
            .context("end-of-WAL scan: reading wals")?
            .with_context(|| format!("WAL hole at {lsn}: no covering chunk in wals"))?;
        let chunk_offset = row.get::<_, i64>(0) as u64;
        let data: Vec<u8> = row.get::<_, Vec<u8>>(1);
        let chunk_start = segno as u64 * self.seg + chunk_offset;
        if lsn.0 >= chunk_start + data.len() as u64 {
            anyhow::bail!(
                "WAL hole at {lsn}: nearest chunk covers [{}, {}) only",
                Lsn(chunk_start),
                Lsn(chunk_start + data.len() as u64)
            );
        }
        Ok((Lsn(chunk_start), data))
    }
}

/// Bake the per-instance `branch_id` into a `wals` query whose `$1` is
/// `branch_id` (the convention here): replace that bind with a quoted
/// literal and shift every remaining `$N` (N ≥ 2) down by one. Each
/// `YbWalStorage` is bound to one branch for its lifetime and the
/// safekeeper does no lineage/ancestor reads, so the id is fixed for
/// every query on this instance — baking drops the bind from the wire.
/// The id renders as fixed-width hex (`BranchId` `Display`), so the
/// literal is injection-safe.
fn bake_branch_id(sql: &str, branch_id: &str) -> String {
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
/// next.chunk_offset`). Each run becomes ONE `safekeeper.wals` row —
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
        if cur.segno != prev.segno || prev.chunk_offset + prev.chunk_size as i64 != cur.chunk_offset
        {
            runs.push(start..i);
            start = i;
        }
    }
    if !chunks.is_empty() {
        runs.push(start..chunks.len());
    }
    runs
}

/// Discard all WAL strictly beyond `end_pos`: drop chunks that start at
/// or after it, and truncate the chunk straddling it down to `end_pos`.
/// After this, `max(chunk_offset + chunk_size)` for the timeline is
/// exactly `end_pos` — the YB equivalent of truncating a local WAL
/// segment file. Used to discard a torn tail on safekeeper restart and to
/// honor a walproposer truncation on ProposerElected. Both statements are
/// idempotent (keyed DELETE; absolute-`SET` UPDATE whose predicate stops
/// matching once the chunk has been truncated).
///
/// The bucket list completes the hash key for all buckets, so both
/// statements hash-route to the buckets' tablets rather than scatter-scanning
/// the branch; the cut still spans every bucket.
async fn truncate_wals_above(
    client: &YsqlClient,
    table: &str,
    branch_id: &str,
    wal_seg_size: usize,
    end_pos: Lsn,
) -> Result<()> {
    let table = quote_relation(table);
    let seg = wal_seg_size as i64;
    let end = end_pos.0 as i64;
    let params: [&(dyn ToSql + Sync); 2] = [&branch_id, &end];

    let buckets = bucket_in_list();
    // Chunks that begin at or after end_pos are entirely torn — drop them.
    let del = format!(
        "DELETE FROM {table} WHERE branch_id = $1 AND {buckets} \
         AND segno * {seg} + chunk_offset >= $2"
    );
    client
        .execute(&del, &params)
        .await
        .context("truncate_wals_above: delete chunks past end_pos")?;

    // The chunk straddling end_pos keeps its bytes up to end_pos; trim the
    // torn remainder so its end aligns exactly with end_pos.
    let upd = format!(
        "UPDATE {table} SET \
           data = substring(data FROM 1 FOR ($2 - (segno * {seg} + chunk_offset))::int), \
           chunk_size = ($2 - (segno * {seg} + chunk_offset))::int \
         WHERE branch_id = $1 AND {buckets} \
           AND segno * {seg} + chunk_offset < $2 \
           AND segno * {seg} + chunk_offset + chunk_size > $2"
    );
    client
        .execute(&upd, &params)
        .await
        .context("truncate_wals_above: trim straddling chunk")?;
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
///      coalesced into single rows; tsclient or YSQL lane per hot_path).
///   6. Publishing the highest `end_lsn` of the batch onto `watermark_tx`
///      as `DurableState::Healthy`.
///
/// `flush_wal` / `truncate_wal` ping `flush_notify` before they await
/// the watermark; that cuts the step-3 wait short so commit latency
/// doesn't pay for accumulation while still letting throughput-oriented
/// background streaming accumulate naturally.
///
/// On a batch failure, publishes `DurableState::Failed` and exits. The
/// producer (`upsert_writes`) detects the closed `rx` on its next
/// `tx.send().await` and propagates the captured error string upward.
///
/// Exits cleanly when `tx` is dropped (channel closes from the
/// producer side, `rx.recv()` returns `None`).
// Wide signature: consumer task needs the full set of YB connection params.
#[allow(clippy::too_many_arguments)]
async fn consumer_main(
    mut rx: mpsc::Receiver<PendingChunk>,
    ysql_fpu_client: Arc<YsqlClient>,
    yb_tsclient: Option<Arc<YbTsClient>>,
    table_name: String,
    branch_id: String,
    watermark_tx: watch::Sender<DurableState>,
    flush_notify: Arc<tokio::sync::Notify>,
    durable_lsn: Arc<AtomicU64>,
    cache: Option<Arc<std::sync::Mutex<crate::wal_cache::WalCache>>>,
) {
    // Placement-bucket rotation, one bucket per BATCH. Restart resets the
    // phase to 0 — the bucket is placement only, not identity, so the
    // phase doesn't matter. Rotating one bucket per batch spreads write load
    // across all WALS_BUCKETS tablets, while keeping each batch on ONE
    // tablet (atomic on both lanes).
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
        // stamping site, after assembly and before any write attempt, so
        // tsclient-lane retries (which replay this same Vec) inherit the
        // same bucket and produce identical rows.
        // One bucket per batch means the batch targets one tablet: a
        // single atomic write batch on the tsclient lane (a crash
        // mid-flush can no longer leave a hole) and the single-shard
        // transaction fast path on YSQL (no distributed txn).
        let bucket = next_batch_bucket;
        next_batch_bucket = (next_batch_bucket + 1) % WALS_BUCKETS;
        for pc in &mut batch {
            pc.bucket_id = bucket;
        }

        let highest_lsn = batch.last().map(|c| c.end_lsn).expect("batch is non-empty");

        match flush_batch(
            &ysql_fpu_client,
            yb_tsclient.as_deref(),
            &table_name,
            &branch_id,
            &batch,
        )
        .await
        {
            Ok(()) => {
                // Populate the read-side cache from the batch BEFORE
                // publishing the durability advance. The cache is the
                // WAL sender's fast path; we only insert chunks here
                // (after a successful INSERT) so the cache never
                // serves bytes that aren't durable in
                // `safekeeper.wals` — the invariant that pre-INSERT
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
            Err(e) => {
                let msg = format!("{e:#}");
                tracing::error!(
                    rows = batch.len(),
                    bytes = batch_bytes,
                    "YB WAL consumer batch failed: {msg}"
                );
                let _ = watermark_tx.send(DurableState::Failed(msg));
                return;
            }
        }
    }
}

/// Persist one batch of chunks into `safekeeper.wals` over the configured
/// transport: the thin-RPC (`yb_tsclient`) lane when `hot_path = "tsclient"`,
/// otherwise the YSQL lane. The whole batch carries ONE bucket (stamped by
/// `consumer_main` before the write), so it writes as one atomic write to a
/// single tablet.
///
/// Contiguous chunks are coalesced into single rows ([`coalesce_runs`]):
/// each run becomes one row keyed by its first chunk, sized/valued as the
/// concatenation of the run's chunks. The typical batch (sequential WAL
/// within one segment) is a single run — one row.
async fn flush_batch(
    ysql_fpu_client: &YsqlClient,
    yb_tsclient: Option<&YbTsClient>,
    table_name: &str,
    branch_id: &str,
    batch: &[PendingChunk],
) -> Result<()> {
    if batch.is_empty() {
        return Ok(());
    }
    let runs = coalesce_runs(batch);
    match yb_tsclient {
        Some(yb_tsclient) => flush_batch_tsclient(yb_tsclient, branch_id, batch, &runs).await,
        None => flush_batch_ysql(ysql_fpu_client, table_name, branch_id, batch, &runs).await,
    }
}

/// `data` is `Arc<Vec<u8>>` (shared with WalCache); this adapter lets
/// `Bytes::from_owner` borrow it without copying the chunk payload.
struct SharedChunk(Arc<Vec<u8>>);

impl AsRef<[u8]> for SharedChunk {
    fn as_ref(&self) -> &[u8] {
        self.0.as_slice()
    }
}

/// Concatenate a run's chunk payloads into the single contiguous buffer
/// the tsclient RPC lane binds for the run's `data` column.
///
/// A single-chunk run (no coalescing) stays zero-copy via the shared
/// Arc. A multi-chunk run is concatenated into one scratch Vec: the
/// tsclient lane takes a single contiguous buffer per bind value, so this
/// one memcpy per run (typically one per batch) is the price of the
/// single-row write.
///
/// The YSQL lane streams the same run through [`RunData`]'s `to_sql`
/// (bare byte-append into the wire buffer); the two serializations stay
/// byte-identical so a timeline reads back the same rows regardless of
/// which lane wrote them — `tsclient_and_ysql_lanes_serialize_identical_rows`
/// (test) pins them together.
fn run_data_bytes(chunks: &[PendingChunk]) -> bytes::Bytes {
    if chunks.len() == 1 {
        bytes::Bytes::from_owner(SharedChunk(chunks[0].data.clone()))
    } else {
        let total: usize = chunks.iter().map(|p| p.chunk_size as usize).sum();
        let mut buf = Vec::with_capacity(total);
        for pc in chunks {
            buf.extend_from_slice(&pc.data);
        }
        bytes::Bytes::from(buf)
    }
}

/// Thin-RPC (`yb_tsclient`) variant of the batched write: one Perform
/// with one `PGSQL_UPSERT` op per coalesced run, each run keyed by its
/// first chunk and valued as the concatenation of the run's chunks
/// ([`run_data_bytes`]). The whole batch carries ONE bucket, so all ops
/// land on one tablet and the tserver session applies them as one write
/// batch (atomic; verified live in `yb_tsclient`'s
/// `single_tablet_batch_atomicity`) — a crash mid-flush cannot leave a
/// hole (the invariant `load_write_lsn`'s simple max-end rehydration
/// relies on).
async fn flush_batch_tsclient(
    yb_tsclient: &YbTsClient,
    branch_id: &str,
    batch: &[PendingChunk],
    runs: &[std::ops::Range<usize>],
) -> Result<()> {
    // Test-only failpoint (build with `--features testing`, configure via
    // `fail::cfg("sk-wal-direct-flush-fail", "return(())")`): inject a
    // tsclient-lane WAL flush failure before any write reaches YB.
    fail::fail_point!("sk-wal-direct-flush-fail", |_| {
        Err(anyhow::anyhow!(
            "failpoint sk-wal-direct-flush-fail: injected tsclient-lane WAL flush failure"
        ))
    });
    use yb_tsclient::{BindValue, UpsertRow};
    let n = runs.len();
    let now_us: i64 = SystemTime::now()
        .duration_since(UNIX_EPOCH)
        .map(|d| d.as_micros() as i64)
        .unwrap_or(0);
    let rows = runs
        .iter()
        .map(|r| {
            let chunks = &batch[r.clone()];
            let first = &chunks[0];
            let total: usize = chunks.iter().map(|p| p.chunk_size as usize).sum();
            UpsertRow::new(
                &yb_tsclient.wals,
                vec![
                    BindValue::Text(branch_id.to_string()),
                    BindValue::I16(first.bucket_id),
                    BindValue::I64(first.segno as i64),
                    BindValue::I64(first.chunk_offset),
                ],
                &[
                    ("chunk_size", BindValue::I32(total as i32)),
                    ("data", BindValue::Bytes(run_data_bytes(chunks))),
                    ("last_modified_us", BindValue::I64(now_us)),
                ],
            )
        })
        .collect::<std::result::Result<Vec<_>, _>>()?;

    let batch_bytes: usize = batch.iter().map(|p| p.chunk_size as usize).sum();
    let started = std::time::Instant::now();
    // Full retry budget: this lane has no YSQL fallback, so it must ride
    // out transient failures itself. Each attempt round-robins to the next
    // tserver session (one per endpoint), so a single stalled/failed node
    // costs ~one attempt before the next lands on a healthy one — the
    // rotation is what keeps the full budget from pinning the timeline lock
    // on one bad node.
    let res = yb_tsclient
        .client
        .upsert_batch(&yb_tsclient.wals, &rows)
        .await;
    crate::metrics::YB_WAL_INSERT_SECONDS
        .with_label_values(&["tsclient"])
        .observe(started.elapsed().as_secs_f64());
    crate::metrics::YB_WAL_BATCH_ROWS.observe(n as f64);
    crate::metrics::YB_WAL_BATCH_BYTES.observe(batch_bytes as f64);
    res.with_context(|| format!("YB WAL tsclient upsert batch ({n} rows)"))?;
    Ok(())
}

/// Zero-copy BYTEA bind for one coalesced run. Holds the run's original
/// `&[PendingChunk]` slice (each chunk's `data` an `Arc<Vec<u8>>` shared
/// with the WalCache) instead of pre-concatenating into a scratch Vec:
/// `to_sql` appends each chunk's bytes into the wire buffer
/// sequentially, which is byte-identical to binding one concatenated
/// bytea — the binary BYTEA encoding is just the raw bytes (the
/// upstream `impl ToSql for &[u8]` delegates to
/// `postgres_protocol::types::bytea_to_sql`, a bare `put_slice`; the
/// length header is derived by the caller from the buffer growth). This
/// keeps the single wire-buffer copy a plain `&[u8]` bind already pays,
/// with no intermediate allocation.
struct RunData<'a>(&'a [PendingChunk]);

impl std::fmt::Debug for RunData<'_> {
    fn fmt(&self, f: &mut std::fmt::Formatter<'_>) -> std::fmt::Result {
        write!(
            f,
            "RunData({} chunks, {} bytes)",
            self.0.len(),
            self.0.iter().map(|p| p.data.len()).sum::<usize>()
        )
    }
}

impl ToSql for RunData<'_> {
    fn to_sql(
        &self,
        _ty: &yb_tokio_postgres::types::Type,
        out: &mut bytes::BytesMut,
    ) -> std::result::Result<
        yb_tokio_postgres::types::IsNull,
        Box<dyn std::error::Error + Sync + Send>,
    > {
        for pc in self.0 {
            out.extend_from_slice(&pc.data);
        }
        Ok(yb_tokio_postgres::types::IsNull::No)
    }

    fn accepts(ty: &yb_tokio_postgres::types::Type) -> bool {
        *ty == yb_tokio_postgres::types::Type::BYTEA
    }

    yb_tokio_postgres::types::to_sql_checked!();
}

/// Build and execute one multi-row `INSERT ... VALUES (...), (...)`
/// with one row per coalesced run. Returns the error chain on failure
/// (the statement is atomic — all rows commit or none; and since the
/// batch carries one bucket, all rows hash to one tablet, so the
/// statement takes the single-shard transaction fast path).
async fn flush_batch_ysql(
    ysql_fpu_client: &YsqlClient,
    table_name: &str,
    branch_id: &str,
    batch: &[PendingChunk],
    runs: &[std::ops::Range<usize>],
) -> Result<()> {
    if batch.is_empty() {
        return Ok(());
    }
    let n = runs.len();
    let now_us: i64 = SystemTime::now()
        .duration_since(UNIX_EPOCH)
        .map(|d| d.as_micros() as i64)
        .unwrap_or(0);

    let mut sql = String::with_capacity(128 + n * 96);
    sql.push_str("INSERT INTO ");
    sql.push_str(&quote_relation(table_name));
    sql.push_str(
        " (branch_id, bucket_id, segno, chunk_offset, \
          chunk_size, data, last_modified_us) VALUES ",
    );
    for i in 0..n {
        if i > 0 {
            sql.push_str(", ");
        }
        let b = i * 6;
        use std::fmt::Write as _;
        // branch_id baked as a literal (fixed for this instance's
        // lifetime); only the 6 per-row columns bind.
        let _ = write!(
            sql,
            "('{branch_id}',${},${},${},${},${},${})",
            b + 1,
            b + 2,
            b + 3,
            b + 4,
            b + 5,
            b + 6,
        );
    }

    // One row per run, keyed by the run's first chunk. `data` binds the
    // run's chunk slice through `RunData` — the bytes are concatenated
    // straight into the wire buffer, never into an intermediate Vec.
    let bucket_is: Vec<i16> = runs.iter().map(|r| batch[r.start].bucket_id).collect();
    let segno_is: Vec<i64> = runs.iter().map(|r| batch[r.start].segno as i64).collect();
    let offset_is: Vec<i64> = runs.iter().map(|r| batch[r.start].chunk_offset).collect();
    let size_is: Vec<i32> = runs
        .iter()
        .map(|r| batch[r.clone()].iter().map(|p| p.chunk_size).sum())
        .collect();
    let datas: Vec<RunData<'_>> = runs.iter().map(|r| RunData(&batch[r.clone()])).collect();

    let mut params: Vec<&(dyn ToSql + Sync)> = Vec::with_capacity(n * 6);
    for i in 0..n {
        params.push(&bucket_is[i]);
        params.push(&segno_is[i]);
        params.push(&offset_is[i]);
        params.push(&size_is[i]);
        params.push(&datas[i]);
        params.push(&now_us);
    }

    // Observe the YB round trip + batch shape (issue #172). batch_rows counts
    // the coalesced rows actually written (typically 1); batch_bytes still
    // measures the payload, i.e. the pre-coalescing chunk bytes.
    let batch_bytes: usize = batch.iter().map(|p| p.chunk_size as usize).sum();
    let started = std::time::Instant::now();
    let res = ysql_fpu_client.execute(&sql, &params).await;
    crate::metrics::YB_WAL_INSERT_SECONDS
        .with_label_values(&["ysql"])
        .observe(started.elapsed().as_secs_f64());
    crate::metrics::YB_WAL_BATCH_ROWS.observe(n as f64);
    crate::metrics::YB_WAL_BATCH_BYTES.observe(batch_bytes as f64);
    res.with_context(|| format!("YB WAL batched INSERT ({n} rows)"))?;
    Ok(())
}

/// Lazy chunk-streaming reader over `safekeeper.wals`, replacing the old
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
    /// Dedicated read lane (no upsert GUC). See [`YbWalConn::read_client`].
    read_client: Arc<YsqlClient>,
    /// Thin-RPC lane (`hot_path = "tsclient"`); when set, fetches use it.
    /// `None` on the YSQL read path.
    yb_tsclient: Option<Arc<YbTsClient>>,
    table: String,
    branch_id: String,

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
    /// The reader holds clones of the dedicated read lane + cache
    /// handle, so it's `Send + Sync` and can be passed up the WAL
    /// sender stack like the `tokio::fs::File` it replaces.
    pub fn new(
        yb_wal: Arc<YbWalConn>,
        branch_id: &BranchId,
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
            read_client: yb_wal.read_client.clone(),
            yb_tsclient: yb_wal.yb_tsclient.clone(),
            table: yb_wal.table.clone(),
            branch_id: branch_id.to_string(),
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
        let client = self.read_client.clone();
        let table = self.table.clone();
        let branch_id = self.branch_id.clone();
        let segno = self.segno;
        let pos = self.pos;
        // Base LSN of this segment, to bring segment-relative chunk offsets into
        // the same (absolute) frame as `issued_flush` for the cache-warm gate.
        let seg_base = self.segno * self.wal_seg_size;
        let cache = self.cache.clone();
        let yb_tsclient = self.yb_tsclient.clone();
        let (tx, rx) = tokio::sync::oneshot::channel();
        tokio::spawn(async move {
            let res = fetch_chunks(
                &client,
                yb_tsclient.as_deref(),
                &table,
                &branch_id,
                segno,
                pos,
            )
            .await;
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
                    // `fetch_chunks_from_yb` returns the covering chunk
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
                        "YB WAL fetch failed for segno {} offset {}: {e}",
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
/// prefetch window from `safekeeper.wals`.
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
/// Fetch the covering + forward chunks for `byte_offset` over the
/// configured transport: the thin-RPC (`yb_tsclient`) lane when
/// `hot_path = "tsclient"`, otherwise the YSQL read lane. Both return one
/// consistent snapshot (see each for how).
async fn fetch_chunks(
    client: &YsqlClient,
    yb_tsclient: Option<&YbTsClient>,
    table: &str,
    branch_id: &str,
    segno: u64,
    byte_offset: u64,
) -> anyhow::Result<Vec<CurrentChunk>> {
    match yb_tsclient {
        Some(yb_tsclient) => {
            fetch_chunks_tsclient(yb_tsclient, branch_id, segno, byte_offset).await
        }
        None => fetch_chunks_from_yb(client, table, branch_id, segno, byte_offset).await,
    }
}

/// Client-side merge for the tsclient fetch fan-out (the row decoder now
/// lives in `yb_tsclient`, so the result rows are `yb_tsclient::Row`).
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
    rows: impl Iterator<Item = &'a yb_tsclient::Row>,
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
            data: Arc::new(row[2].as_bytes()?.to_vec()),
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

/// Thin-RPC (`yb_tsclient`) two-arm fetch. The SQL version keeps both
/// arms in ONE statement for a single read snapshot; here a chunk's
/// bucket can't be derived, so every (arm, bucket) pair is its own
/// single-tablet read op — covering arm per bucket (`chunk_offset <
/// pos`, descending, limit 1) and forward arm per bucket (`[pos, pos +
/// FETCH_FORWARD_BYTES)`, ascending, limit [`FETCH_FORWARD_CHUNKS`]) —
/// and ALL `2×WALS_BUCKETS` of them are issued as ops of ONE Perform
/// ([`yb_tsclient::TsClient::read_pages`]): the proxy picks one read
/// point at setup and every op shares it, so the fetch is a single
/// snapshot BY CONSTRUCTION (#139; see `libs/yb_tsclient/DESIGN.md`).
/// Results merge client-side through [`merge_fetched_rows`], preserving
/// the SQL path's semantics: covering = the max-offset row with
/// `chunk_offset < pos` across all buckets that spans `pos`; forward =
/// ascending merge capped at `FETCH_FORWARD_CHUNKS`. The reader's
/// flush-bound gating stays as defense in depth regardless.
async fn fetch_chunks_tsclient(
    yb_tsclient: &YbTsClient,
    branch_id: &str,
    segno: u64,
    byte_offset: u64,
) -> anyhow::Result<Vec<CurrentChunk>> {
    use yb_tsclient::{BindValue, CondOp, RangeRead};
    let started_at = std::time::Instant::now();
    let wals = &yb_tsclient.wals;
    let targets = ["chunk_offset", "chunk_size", "data"];
    let segno_i = segno as i64;

    // Covering arm for one bucket: its highest chunk starting below
    // byte_offset; the cross-bucket max resolves in the merge.
    let covering_read = |bucket: i16| -> anyhow::Result<RangeRead> {
        Ok(RangeRead::new(
            wals,
            vec![
                BindValue::Text(branch_id.to_string()),
                BindValue::I16(bucket),
            ],
        )?
        .cond(wals, "segno", CondOp::Eq, BindValue::I64(segno_i))?
        .cond(
            wals,
            "chunk_offset",
            CondOp::Lt,
            BindValue::I64(byte_offset as i64),
        )?
        .targets(wals, &targets)?
        .descending()
        .limit(1))
    };
    // Forward arm for one bucket: ascending chunks in
    // [byte_offset, byte_offset + FETCH_FORWARD_BYTES).
    let forward_read = |bucket: i16| -> anyhow::Result<RangeRead> {
        Ok(RangeRead::new(
            wals,
            vec![
                BindValue::Text(branch_id.to_string()),
                BindValue::I16(bucket),
            ],
        )?
        .cond(wals, "segno", CondOp::Eq, BindValue::I64(segno_i))?
        .cond(
            wals,
            "chunk_offset",
            CondOp::Ge,
            BindValue::I64(byte_offset as i64),
        )?
        .cond(
            wals,
            "chunk_offset",
            CondOp::Lt,
            BindValue::I64(byte_offset.saturating_add(FETCH_FORWARD_BYTES) as i64),
        )?
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
    let pages = yb_tsclient.client.read_pages(wals, &reads).await?;

    let out = merge_fetched_rows(pages.iter().flat_map(|p| p.rows.iter()), byte_offset)?;
    let total_bytes: u64 = out.iter().map(|c| c.end_offset - c.chunk_offset).sum();

    crate::metrics::WAL_CACHE_YB_FETCH_SECONDS
        .with_label_values(&["tsclient"])
        .observe(started_at.elapsed().as_secs_f64());
    crate::metrics::WAL_CACHE_YB_FETCH_CHUNKS.observe(out.len() as f64);
    crate::metrics::WAL_CACHE_YB_FETCH_BYTES.observe(total_bytes as f64);
    Ok(out)
}

async fn fetch_chunks_from_yb(
    client: &YsqlClient,
    table: &str,
    branch_id: &str,
    segno: u64,
    byte_offset: u64,
) -> anyhow::Result<Vec<CurrentChunk>> {
    // Fetch the covering chunk (the one starting before `byte_offset` that
    // may span it) AND the forward chunks in ONE statement, so the whole
    // fetch sees a single read snapshot. Splitting it into two autocommit
    // SELECTs let a truncate+rewrite commit BETWEEN them and splice an
    // old-term covering chunk onto new-term forward chunks — a torn read the
    // interpreted-WAL decoder trips on ("invalid xlog page header", #139).
    // Both arms stay index-tight on the (segno, chunk_offset) PK; the hot
    // boundary-aligned path simply returns no covering row. The bucket of
    // a chunk can't be derived, so each arm carries the hash-key
    // `bucket_id IN (…)` list — YSQL turns it into batched per-bucket
    // index probes, still within this ONE statement / ONE read snapshot.
    let sql = bake_branch_id(
        &format!(
            "(SELECT chunk_offset, chunk_size, data FROM {tbl} \
                WHERE branch_id = $1 AND {buckets} AND segno = $2 \
                  AND chunk_offset < $3 \
                ORDER BY chunk_offset DESC LIMIT 1) \
             UNION ALL \
             (SELECT chunk_offset, chunk_size, data FROM {tbl} \
                WHERE branch_id = $1 AND {buckets} AND segno = $2 \
                  AND chunk_offset >= $3 AND chunk_offset < $4 \
                ORDER BY chunk_offset ASC LIMIT $5)",
            tbl = quote_relation(table),
            buckets = bucket_in_list()
        ),
        branch_id,
    );
    let segno_i: i64 = segno as i64;
    let lo_i: i64 = byte_offset as i64;
    let hi_i: i64 = byte_offset.saturating_add(FETCH_FORWARD_BYTES) as i64;
    let limit_i: i64 = FETCH_FORWARD_CHUNKS as i64;
    let params: [&(dyn ToSql + Sync); 4] = [&segno_i, &lo_i, &hi_i, &limit_i];
    let started_at = std::time::Instant::now();
    let rows = client
        .query(&sql, &params)
        .await
        .context("YB WAL fetch_chunks_from_yb")?;
    let elapsed = started_at.elapsed();

    let mut out: Vec<CurrentChunk> = Vec::with_capacity(rows.len());
    let mut total_bytes: u64 = 0;
    for row in rows {
        let chunk_offset: i64 = row.get("chunk_offset");
        let chunk_size: i32 = row.get("chunk_size");
        let data: Vec<u8> = row.get("data");
        let start = chunk_offset as u64;
        let end = start + chunk_size as u64;
        // The covering arm (`chunk_offset < byte_offset`) is only useful when
        // it actually spans the position; a chunk ending at/before it leaves
        // a real hole, not a cover. Forward chunks (`>= byte_offset`) are
        // always kept.
        if start < byte_offset && end <= byte_offset {
            continue;
        }
        total_bytes += chunk_size as u64;
        out.push(CurrentChunk {
            chunk_offset: start,
            end_offset: end,
            data: Arc::new(data),
        });
    }
    // UNION ALL does not guarantee overall ordering across the two arms;
    // the covering chunk (lowest offset) must come first.
    out.sort_by_key(|c| c.chunk_offset);

    crate::metrics::WAL_CACHE_YB_FETCH_SECONDS
        .with_label_values(&["ysql"])
        .observe(elapsed.as_secs_f64());
    crate::metrics::WAL_CACHE_YB_FETCH_CHUNKS.observe(out.len() as f64);
    crate::metrics::WAL_CACHE_YB_FETCH_BYTES.observe(total_bytes as f64);

    Ok(out)
}

/// WAL storage used by the safekeeper. Kept as a type alias so existing
/// `<WAL: wal_storage::Storage>` generics on `SafeKeeper` keep working
/// after the local-FS variant was removed.
pub type WalStore = YbWalStorage;

impl YbWalStorage {
    /// Forwarded passthrough for `WalBookkeeping::internal_state`. Used by
    /// debug-dump / metrics endpoints.
    pub fn internal_state(&self) -> (Lsn, Lsn, Lsn, bool) {
        self.inner.internal_state()
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
        // connection's search_path (a bare `"safekeeper.wals"` would be
        // a single — nonexistent — identifier).
        assert_eq!(quote_relation("safekeeper.wals"), "\"safekeeper\".\"wals\"");
        // Per-test scratch tables are unqualified → one quoted ident.
        assert_eq!(quote_relation("wals_test_abc"), "\"wals_test_abc\"");
        // Embedded quotes are doubled in every segment.
        assert_eq!(quote_relation("a\"b.c\"d"), "\"a\"\"b\".\"c\"\"d\"");
    }

    #[test]
    fn bake_branch_id_bakes_and_shifts() {
        let b = "00112233445566778899aabbccddeeff";
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
    /// returned range becomes ONE `safekeeper.wals` row, so runs must
    /// break at every segment boundary and byte discontinuity — and
    /// nowhere else.
    /// A `PendingChunk` with only the fields `coalesce_runs` reads set;
    /// data/end_lsn/bucket are placeholders.
    fn pc(segno: u64, chunk_offset: i64, chunk_size: i32) -> PendingChunk {
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
    fn scratch_table(prefix: &str) -> String {
        use std::hash::{Hash, Hasher};
        let mut h = std::collections::hash_map::DefaultHasher::new();
        std::thread::current().name().unwrap_or("t").hash(&mut h);
        format!("{prefix}_{:08x}", h.finish() as u32)
    }

    /// Per-test `wals`-shaped scratch table. Names are FIXED per test
    /// (not randomized): the DROP-IF-EXISTS here self-heals any table a
    /// panicked/killed earlier run left behind, where random suffixes
    /// accumulated leaked tables until the dev YB's tablet count
    /// degraded every daemon start on the box. Trade-off: two
    /// simultaneous `cargo test -p safekeeper` invocations on one
    /// cluster would collide — don't do that.
    async fn make_wals_table(client: &YsqlClient, table: &str) {
        let q = quote_relation(table);
        client
            .execute(&format!("DROP TABLE IF EXISTS {q}"), &[])
            .await
            .expect("drop scratch wals table");
        // Same shape as the production schema (bucketed hash key); the
        // migration's `SPLIT INTO 10 TABLETS` is omitted — placement is
        // not under test and default tablet counts keep setup fast.
        client
            .execute(
                &format!(
                    "CREATE TABLE {q} (\
                       branch_id        TEXT     NOT NULL,\
                       bucket_id        SMALLINT NOT NULL,\
                       segno            BIGINT   NOT NULL,\
                       chunk_offset     BIGINT   NOT NULL,\
                       chunk_size       INT      NOT NULL,\
                       data             BYTEA    NOT NULL,\
                       last_modified_us BIGINT   NOT NULL,\
                       PRIMARY KEY ((branch_id, bucket_id) HASH, segno, chunk_offset)\
                     )"
                ),
                &[],
            )
            .await
            .expect("create scratch wals table");
    }

    async fn drop_wals_table(client: &YsqlClient, table: &str) {
        let q = quote_relation(table);
        let _ = client
            .execute(&format!("DROP TABLE IF EXISTS {q}"), &[])
            .await;
    }

    /// Build a fresh `YbWalStorage` with its own per-test scratch table
    /// and tempdir. `start_lsn` becomes the rehydrated `write_lsn` (the
    /// table is empty, so `load_write_lsn` falls back to `commit_lsn`).
    ///
    /// All three lanes (`fpu`, `read`, `bg`) share a single underlying
    /// `YsqlClient` Arc here — production opens three independent
    /// connections, but the tests don't exercise per-lane isolation and
    /// keeping it to one connection cuts setup time.
    /// Regression for the 2026-07 TPC-C safekeeper wedge. Every
    /// `wait_for_durable` caller sits inside `process_msg`, holding the
    /// timeline shared_state WRITE lock. When the durability target can
    /// outrun what the consumer will ever publish (YB stalled by a
    /// splitting `safekeeper.wals` tablet, walproposer churn re-entering
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
        state.local_start_lsn = start_lsn;

        let tmp = camino_tempfile::tempdir().expect("tempdir");
        let yb_wal = Arc::new(YbWalConn {
            fpu_client: client.clone(),
            read_client: client.clone(),
            system_client: client,
            table: table.to_string(),
            cache: None,
            yb_tsclient: None,
        });
        let storage = YbWalStorage::new(&branch_id, &state, yb_wal)
            .await
            .expect("YbWalStorage::new");
        (storage, tmp)
    }

    const SEG_SIZE: u64 = 16 * 1024 * 1024;

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
        state.local_start_lsn = start_lsn;
        // truncate_wal decomposes end_pos with this, so it must be real.
        state.server.wal_seg_size = SEG_SIZE as u32;

        let tmp = camino_tempfile::tempdir().expect("tempdir");
        let yb_wal = Arc::new(YbWalConn {
            fpu_client: client.clone(),
            read_client: client.clone(),
            system_client: client,
            table: table.to_string(),
            cache: Some(Arc::new(std::sync::Mutex::new(WalCache::new(
                64 * 1024 * 1024,
            )))),
            yb_tsclient: None,
        });
        let storage = YbWalStorage::new(&branch_id, &state, yb_wal)
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
    /// rather than silently advance `inner.flush_record_lsn` past YB
    /// (which is what created the 2026-05-29 forensic gap — see the
    /// `truncate_wal` comment).
    ///
    /// Construction gives us a valid bug-exhibiting state without
    /// needing to push real WAL bytes: an empty `wals` falls back to
    /// `state.commit_lsn` for `write_lsn`, the watermark starts at
    /// `Healthy(write_lsn)`, and `durable_lsn` is initialised from the
    /// same value. So `submitted_max_lsn == durable_lsn == write_lsn ==
    /// start_lsn` at t=0. We then ask to truncate to `start_lsn + 4 KiB`
    /// — exactly the "ProposerElected with start_streaming_at past the
    /// persisted tail" pattern. With Fix 2 we bail; without it,
    /// `inner.truncate_wal(end_pos)` advances `flush_record_lsn` past
    /// `durable_lsn` and opens the gap.
    #[tokio::test]
    async fn truncate_wal_refuses_end_pos_past_durable() {
        let table = scratch_table("wals_test_truncate");
        // Start past the first-segment quick-exit edge case in
        // `inner.truncate_wal` (it special-cases `write_record_lsn == 0`).
        let start = Lsn(SEG_SIZE);
        let (mut storage, _tmp) = make_storage(&table, start).await;

        assert_eq!(
            Lsn(storage.durable_lsn.load(Ordering::Acquire)),
            start,
            "durable_lsn seeded from load_write_lsn at construction"
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

    /// `flush_lsn()` returns `min(inner, durable)` — defence-in-depth
    /// in case some future path moves `inner.flush_record_lsn` ahead of
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
            "flush_lsn must cap at durable_lsn when inner is ahead"
        );

        // Restore so the consumer's shutdown path doesn't observe a
        // permanently-regressed watermark.
        storage.durable_lsn.store(start.0, Ordering::Release);

        drop_wals_table(&storage.ysql_system_client, &table).await;
    }

    /// Open the 3 lanes the same way `YbWalConn::from_conf` does — one
    /// `YsqlClient` per role. Tests below that exercise per-lane
    /// behaviour use this rather than the cheaper single-connection
    /// scaffolding in `make_storage`.
    async fn open_three_lanes() -> (Arc<YsqlClient>, Arc<YsqlClient>, Arc<YsqlClient>) {
        let fpu = Arc::new(
            YsqlClient::connect_no_tls(pgcfg(), /* set_fast_path_upsert= */ true)
                .await
                .expect("connect fpu lane"),
        );
        let read = Arc::new(
            YsqlClient::connect_no_tls(pgcfg(), false)
                .await
                .expect("connect read lane"),
        );
        let system = Arc::new(
            YsqlClient::connect_no_tls(pgcfg(), false)
                .await
                .expect("connect system lane"),
        );
        (fpu, read, system)
    }

    /// `yb_enable_upsert_mode` is the GUC that lets the FPU lane's
    /// chunk INSERTs skip YB's read-before-write check. The lane split
    /// keeps the GUC on the FPU lane only — verify the other two lanes
    /// open clean.
    #[tokio::test]
    async fn fpu_lane_alone_has_upsert_mode_guc() {
        let (fpu, read, system) = open_three_lanes().await;

        async fn show_upsert(c: &YsqlClient) -> String {
            let row = c
                .query_opt("SHOW yb_enable_upsert_mode", &[])
                .await
                .expect("SHOW yb_enable_upsert_mode")
                .expect("non-empty row");
            row.get::<_, String>(0)
        }

        assert_eq!(show_upsert(&fpu).await, "on", "fpu lane must have GUC on");
        assert_eq!(show_upsert(&read).await, "off", "read lane must not");
        assert_eq!(
            show_upsert(&system).await,
            "off",
            "system lane must not have the GUC"
        );
    }

    /// `load_write_lsn` runs on `system_client` at SK startup, but the
    /// rows it scans were written by the previous SK instance's
    /// `fpu_client`. Regression guard: a SELECT on a freshly-opened
    /// system_client must see the committed FPU writes — anything else
    /// would let the safekeeper rehydrate a `write_lsn` below YB's
    /// actual durable tail and silently regress flush_lsn on restart.
    #[tokio::test]
    async fn load_write_lsn_via_system_client_sees_fpu_writes() {
        let table = scratch_table("wals_test_lane_view");
        let (fpu, _read, system) = open_three_lanes().await;
        make_wals_table(&fpu, &table).await;

        let branch_s = BranchId::generate().to_string();
        // Two contiguous chunks in segno 0, spread across two buckets
        // (this test is about cross-lane visibility; the per-bucket
        // max-end probe must take the max across both). End-offset is
        // 1024 + 256 = 1280.
        let rows: &[(i64, i64, i32)] = &[(0, 0, 1024), (0, 1024, 256)];
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
                    "INSERT INTO {} (branch_id, bucket_id, segno, chunk_offset, \
                                     chunk_size, data, last_modified_us) \
                     VALUES ($1, $2, $3, $4, $5, $6, $7)",
                    quote_relation(&table)
                ),
                &[&branch_s, &bucket, segno, offset, size, &blob, &now_us],
            )
            .await
            .expect("INSERT via fpu");
        }

        // Read via system_client — the lane that does the real
        // load_write_lsn at SK startup.
        let lsn =
            YbWalStorage::load_write_lsn(&system, &table, &branch_s, SEG_SIZE as usize, Lsn(0))
                .await
                .expect("load_write_lsn via system_client");
        let expected = 1024u64 + 256u64;
        assert_eq!(
            lsn,
            Lsn(expected),
            "system_client SELECT must reflect fpu_client INSERTs immediately"
        );

        // And load_min_segno — same cross-connection contract.
        let min_segno = YbWalStorage::load_min_segno(&system, &table, &branch_s)
            .await
            .expect("load_min_segno via system_client");
        assert_eq!(min_segno, 0, "min(segno) across lanes");

        drop_wals_table(&fpu, &table).await;
    }

    /// `truncate_wals_above` drops chunks past the cut, trims the chunk
    /// straddling it, and leaves `max(chunk_offset + chunk_size)` exactly
    /// at the cut — the durability-side half of the torn-tail fix (#61).
    /// Idempotent on re-run.
    #[tokio::test]
    async fn truncate_wals_above_deletes_and_trims() {
        let table = scratch_table("wals_test_trunc_above");
        let (fpu, _read, system) = open_three_lanes().await;
        make_wals_table(&fpu, &table).await;
        let branch_s = BranchId::generate().to_string();

        // Three chunks in segno 0 spanning [0, 20480): two full pages + a
        // partial. Cut at 10000 — mid the second chunk [8192, 16384).
        let chunks: &[(i64, i64, i32)] = &[(0, 0, 8192), (0, 8192, 8192), (0, 16384, 4096)];
        for (i, (segno, offset, size)) in chunks.iter().enumerate() {
            let bucket = (i % 10) as i16;
            let blob = vec![0x5au8; *size as usize];
            fpu.execute(
                &format!(
                    "INSERT INTO {} (branch_id, bucket_id, segno, chunk_offset, \
                                     chunk_size, data, last_modified_us) \
                     VALUES ($1, $2, $3, $4, $5, $6, $7)",
                    quote_relation(&table)
                ),
                &[&branch_s, &bucket, segno, offset, size, &blob, &0i64],
            )
            .await
            .expect("insert chunk");
        }

        truncate_wals_above(&system, &table, &branch_s, SEG_SIZE as usize, Lsn(10000))
            .await
            .expect("truncate_wals_above");

        // End of WAL is now exactly the cut.
        let lsn =
            YbWalStorage::load_write_lsn(&system, &table, &branch_s, SEG_SIZE as usize, Lsn(0))
                .await
                .expect("load_write_lsn");
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
                &[&branch_s],
            )
            .await
            .expect("count")
            .map(|r| r.get(0))
            .unwrap();
        assert_eq!(count, 2, "the chunk entirely past the cut must be deleted");

        // The straddling chunk is trimmed to 10000 - 8192 = 1808 bytes.
        let row = system
            .query_opt(
                &format!(
                    "SELECT chunk_size, octet_length(data) FROM {} \
                     WHERE branch_id=$1 AND chunk_offset=8192",
                    quote_relation(&table)
                ),
                &[&branch_s],
            )
            .await
            .expect("straddling chunk")
            .expect("straddling chunk exists");
        assert_eq!(row.get::<_, i32>(0), 1808, "chunk_size trimmed to the cut");
        assert_eq!(row.get::<_, i32>(1), 1808, "data trimmed to the cut");

        // Idempotent: a second call is a no-op.
        truncate_wals_above(&system, &table, &branch_s, SEG_SIZE as usize, Lsn(10000))
            .await
            .expect("truncate_wals_above (idempotent)");
        let lsn2 =
            YbWalStorage::load_write_lsn(&system, &table, &branch_s, SEG_SIZE as usize, Lsn(0))
                .await
                .expect("load_write_lsn after re-run");
        assert_eq!(lsn2, Lsn(10000), "re-run must not change the end of WAL");

        drop_wals_table(&fpu, &table).await;
    }

    /// `YbSegmentReader` is the read path the WAL sender plugs into.
    /// Stage a couple of non-overlapping chunks directly in YB, then
    /// drive a reader across them and verify the bytes come back in
    /// order. Bypasses `WalBookkeeping::write_wal` (which validates a
    /// real PG WAL page header that we don't want to synthesise here),
    /// so this stays focused on the YB read path: chunk-row selection,
    /// `pos` advance across chunk boundaries, and the read-lane SELECT.
    #[tokio::test]
    async fn segment_reader_round_trip_across_chunks() {
        use tokio::io::AsyncReadExt;

        let table = scratch_table("wals_test_reader");
        let (fpu, _read, _system) = open_three_lanes().await;
        make_wals_table(&fpu, &table).await;

        let branch_id = BranchId::generate();
        let branch_s = branch_id.to_string();

        let chunk_a: Vec<u8> = (0u8..=255).cycle().take(1024).collect();
        let chunk_b: Vec<u8> = (0u8..=255).cycle().take(2048).map(|b| b ^ 0xa5).collect();
        let now_us: i64 = 0;
        let segno_i: i64 = 0;
        for (i, (offset_i, size_i, data)) in [
            (0i64, 1024i32, chunk_a.as_slice()),
            (1024i64, 2048i32, chunk_b.as_slice()),
        ]
        .into_iter()
        .enumerate()
        {
            let bucket = (i % 10) as i16;
            fpu.execute(
                &format!(
                    "INSERT INTO {} (branch_id, bucket_id, segno, chunk_offset, \
                                     chunk_size, data, last_modified_us) \
                     VALUES ($1, $2, $3, $4, $5, $6, $7)",
                    quote_relation(&table)
                ),
                &[
                    &branch_s, &bucket, &segno_i, &offset_i, &size_i, &data, &now_us,
                ],
            )
            .await
            .expect("INSERT chunk");
        }

        // Reader uses the dedicated read lane in production. Cloning
        // the fpu Arc into all 3 slots is fine for the test — what
        // matters is the chunk-selection logic.
        let yb_wal = Arc::new(YbWalConn {
            fpu_client: fpu.clone(),
            read_client: fpu.clone(),
            system_client: fpu.clone(),
            table: table.clone(),
            cache: None,
            yb_tsclient: None,
        });
        let mut reader = YbSegmentReader::new(
            yb_wal,
            &branch_id,
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
    /// the bytes from that offset onward — i.e. `fetch_chunks_from_yb` must
    /// surface the *covering* chunk (the one starting before the position),
    /// not just chunks at/after it. Exercises the covering arm of the single
    /// atomic UNION query (#139).
    #[tokio::test]
    async fn segment_reader_mid_chunk_resume() {
        use tokio::io::AsyncReadExt;

        let table = scratch_table("wals_test_midchunk");
        let (fpu, _read, _system) = open_three_lanes().await;
        make_wals_table(&fpu, &table).await;

        let branch_id = BranchId::generate();
        let branch_s = branch_id.to_string();

        // One chunk spanning [0, 2048).
        let chunk: Vec<u8> = (0u8..=255).cycle().take(2048).collect();
        fpu.execute(
            &format!(
                "INSERT INTO {} (branch_id, bucket_id, segno, chunk_offset, \
                                 chunk_size, data, last_modified_us) \
                 VALUES ($1, 0, 0, 0, 2048, $2, 0)",
                quote_relation(&table)
            ),
            &[&branch_s, &chunk.as_slice()],
        )
        .await
        .expect("INSERT chunk");

        let yb_wal = Arc::new(YbWalConn {
            fpu_client: fpu.clone(),
            read_client: fpu.clone(),
            system_client: fpu.clone(),
            table: table.clone(),
            cache: None,
            yb_tsclient: None,
        });
        // Start the reader at byte offset 1024 — partway into the chunk.
        let mut reader = YbSegmentReader::new(
            yb_wal,
            &branch_id,
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
        let (fpu, _read, _system) = open_three_lanes().await;
        make_wals_table(&fpu, &table).await;

        let branch_id = BranchId::generate();
        let branch_s = branch_id.to_string();

        let seg: i64 = 0;
        let committed: Vec<u8> = vec![0xAA; 1024]; // [0,1024), below flush
        let tail_old: Vec<u8> = vec![0xBB; 3072]; // [1024,4096), above flush
        let tail_new: Vec<u8> = vec![0xCC; 3072]; // the rewrite

        let insert_sql = format!(
            "INSERT INTO {} (branch_id, bucket_id, segno, chunk_offset, \
                             chunk_size, data, last_modified_us) \
             VALUES ($1, $2, $3, $4, $5, $6, 0)",
            quote_relation(&table)
        );
        let delete_sql = format!(
            "DELETE FROM {} WHERE branch_id = $1 \
             AND segno = $2 AND chunk_offset = $3",
            quote_relation(&table)
        );

        // 1. Stage the committed chunk and the (still-uncommitted) tail.
        for (i, (off, bytes)) in [(0i64, &committed), (1024i64, &tail_old)]
            .into_iter()
            .enumerate()
        {
            let bucket = (i % 10) as i16;
            let size = bytes.len() as i32;
            let slice = bytes.as_slice();
            fpu.execute(
                &insert_sql,
                &[&branch_s, &bucket, &seg, &off, &size, &slice],
            )
            .await
            .expect("INSERT chunk");
        }

        // Shared cache, as in production (one per YbWalConn).
        let cache = Arc::new(std::sync::Mutex::new(WalCache::new(1 << 20)));
        let yb_wal = Arc::new(YbWalConn {
            fpu_client: fpu.clone(),
            read_client: fpu.clone(),
            system_client: fpu.clone(),
            table: table.clone(),
            cache: Some(cache.clone()),
            yb_tsclient: None,
        });

        // flush = 1024: [0,1024) committed, [1024,4096) not yet.
        let flush = Arc::new(AtomicU64::new(1024));
        let mut reader = YbSegmentReader::new(
            yb_wal.clone(),
            &branch_id,
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
        fpu.execute(&delete_sql, &[&branch_s, &seg, &1024i64])
            .await
            .expect("DELETE old tail");
        let tail_new_slice = tail_new.as_slice();
        let tail_size = tail_new.len() as i32;
        fpu.execute(
            &insert_sql,
            &[
                &branch_s,
                &1i16,
                &seg,
                &1024i64,
                &tail_size,
                &tail_new_slice,
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
        let (fpu, _read, _system) = open_three_lanes().await;
        make_wals_table(&fpu, &table).await;

        let branch_id = BranchId::generate();
        let branch_s = branch_id.to_string();

        // Segment 0 holds real WAL in [0, 1024) (think: up to the switch
        // record). Nothing is stored in [1024, SEG_SIZE) — the padding gap.
        let seg: i64 = 0;
        let head_bytes: Vec<u8> = vec![0xAA; 1024];
        let insert_sql = format!(
            "INSERT INTO {} (branch_id, bucket_id, segno, chunk_offset, \
                             chunk_size, data, last_modified_us) \
             VALUES ($1, $2, $3, $4, $5, $6, 0)",
            quote_relation(&table)
        );
        let head_slice = head_bytes.as_slice();
        let head_size = head_bytes.len() as i32;
        fpu.execute(
            &insert_sql,
            &[&branch_s, &0i16, &seg, &0i64, &head_size, &head_slice],
        )
        .await
        .expect("INSERT head chunk");

        let yb_wal = Arc::new(YbWalConn {
            fpu_client: fpu.clone(),
            read_client: fpu.clone(),
            system_client: fpu.clone(),
            table: table.clone(),
            cache: None,
            yb_tsclient: None,
        });

        // Committed frontier is in the NEXT segment: the gap is switch padding.
        let flush_past = Arc::new(AtomicU64::new(SEG_SIZE + 4096));
        let mut reader = YbSegmentReader::new(
            yb_wal.clone(),
            &branch_id,
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
            &branch_id,
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
        let (fpu, _read, _system) = open_three_lanes().await;
        make_wals_table(&fpu, &table).await;

        let branch_id = BranchId::generate();
        let branch_s = branch_id.to_string();

        let seg: i64 = 0;
        let off: i64 = 0;
        let size: i32 = 4096;
        // Committed prefix [0,1024) is identical across terms (committed WAL is
        // immutable); only the once-uncommitted tail [1024,4096) is rewritten.
        let old: Vec<u8> = vec![0xAB; 4096];
        let mut new: Vec<u8> = vec![0u8; 4096];
        new[..1024].fill(0xAB); // committed prefix, unchanged
        new[1024..].fill(0xCD); // rewritten tail

        let insert_sql = format!(
            "INSERT INTO {} (branch_id, bucket_id, segno, chunk_offset, \
                             chunk_size, data, last_modified_us) \
             VALUES ($1, $2, $3, $4, $5, $6, 0)",
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
            &[&branch_s, &0i16, &seg, &off, &size, &old_slice],
        )
        .await
        .expect("INSERT old chunk");

        let yb_wal = Arc::new(YbWalConn {
            fpu_client: fpu.clone(),
            read_client: fpu.clone(),
            system_client: fpu.clone(),
            table: table.clone(),
            cache: None,
            yb_tsclient: None,
        });

        // Live flush frontier, as the streaming reader would publish it.
        let flush = Arc::new(AtomicU64::new(1024));
        let mut reader = YbSegmentReader::new(
            yb_wal.clone(),
            &branch_id,
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
        fpu.execute(&delete_sql, &[&branch_s, &seg, &off])
            .await
            .expect("DELETE old chunk");
        let new_slice = new.as_slice();
        fpu.execute(
            &insert_sql,
            &[&branch_s, &0i16, &seg, &off, &size, &new_slice],
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
        let (fpu, _read, _system) = open_three_lanes().await;
        make_wals_table(&fpu, &table).await;

        let branch_id = BranchId::generate();
        let branch_s = branch_id.to_string();

        // Segment 1: absolute LSN = SEG_SIZE + offset.
        let seg: i64 = 1;
        let seg_base: u64 = SEG_SIZE; // 1 * SEG_SIZE
        let off: i64 = 0;
        let size: i32 = 4096;
        let old: Vec<u8> = vec![0xAB; 4096];
        let mut new: Vec<u8> = vec![0u8; 4096];
        new[..1024].fill(0xAB); // committed prefix, unchanged
        new[1024..].fill(0xCD); // rewritten tail

        let insert_sql = format!(
            "INSERT INTO {} (branch_id, bucket_id, segno, chunk_offset, \
                             chunk_size, data, last_modified_us) \
             VALUES ($1, $2, $3, $4, $5, $6, 0)",
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
            &[&branch_s, &0i16, &seg, &off, &size, &old_slice],
        )
        .await
        .expect("INSERT old chunk");

        let yb_wal = Arc::new(YbWalConn {
            fpu_client: fpu.clone(),
            read_client: fpu.clone(),
            system_client: fpu.clone(),
            table: table.clone(),
            cache: None,
            yb_tsclient: None,
        });

        // Flush frontier is ABSOLUTE: committed up to offset 1024 in segment 1.
        let flush = Arc::new(AtomicU64::new(seg_base + 1024));
        let mut reader = YbSegmentReader::new(
            yb_wal.clone(),
            &branch_id,
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

        fpu.execute(&delete_sql, &[&branch_s, &seg, &off])
            .await
            .expect("DELETE old chunk");
        let new_slice = new.as_slice();
        fpu.execute(
            &insert_sql,
            &[&branch_s, &0i16, &seg, &off, &size, &new_slice],
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
    /// as FEWER `safekeeper.wals` rows (contiguous runs concatenated by
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
            let off: i64 = row.get("chunk_offset");
            let size: i32 = row.get("chunk_size");
            let data: Vec<u8> = row.get("data");
            assert_eq!(
                data.len(),
                size as usize,
                "chunk_size must equal length(data)"
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
    /// one tablet — the atomicity `load_write_lsn`'s simple max-end
    /// rehydration relies on), and the bucket must rotate across batches
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
                        r.get::<_, i64>("chunk_offset"),
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
    /// Calls `load_write_lsn` directly (the same restore seam
    /// `YbWalStorage::new` uses) rather than constructing the full
    /// storage: construction additionally runs the record-boundary
    /// end-of-WAL scan over the row bytes, which on this synthetic
    /// payload could nondeterministically trim the tail and obscure what
    /// is under test — the cross-bucket max itself.
    #[tokio::test]
    async fn load_write_lsn_takes_max_across_adversarial_buckets() {
        let table = scratch_table("wals_test_bucket_max");
        let (fpu, _read, system) = open_three_lanes().await;
        make_wals_table(&fpu, &table).await;
        let branch_s = BranchId::generate().to_string();

        // (bucket_id, segno, chunk_offset, chunk_size); rows never
        // overlap within a segno, per the writer invariant.
        let rows: &[(i16, i64, i64, i32)] = &[
            (7, 5, 8192, 128),  // TRUE global max end: 5*SEG + 8320
            (7, 3, 0, 64),      // low decoy in the SAME bucket as the max
            (2, 5, 0, 8192),    // second-highest end, same top segno, other bucket
            (0, 4, 16000, 384), // bucket-local max in an older segno
            (9, 1, 0, 512),     // ancient decoy
        ];
        let insert_sql = format!(
            "INSERT INTO {} (branch_id, bucket_id, segno, chunk_offset, \
                             chunk_size, data, last_modified_us) \
             VALUES ($1, $2, $3, $4, $5, $6, 0)",
            quote_relation(&table)
        );
        for (bucket, segno, off, size) in rows {
            let blob = vec![0x77u8; *size as usize];
            let slice = blob.as_slice();
            fpu.execute(&insert_sql, &[&branch_s, bucket, segno, off, size, &slice])
                .await
                .expect("insert adversarial row");
        }

        let got =
            YbWalStorage::load_write_lsn(&system, &table, &branch_s, SEG_SIZE as usize, Lsn(0))
                .await
                .expect("load_write_lsn");
        assert_eq!(
            got,
            Lsn(5 * SEG_SIZE + 8192 + 128),
            "load_write_lsn must return the global max end across ALL buckets, \
             not a single bucket's top row"
        );

        // Fresh-timeline fallback: an unknown branch yields commit_lsn.
        let other = BranchId::generate().to_string();
        let fallback =
            YbWalStorage::load_write_lsn(&system, &table, &other, SEG_SIZE as usize, Lsn(4242))
                .await
                .expect("load_write_lsn (empty branch)");
        assert_eq!(
            fallback,
            Lsn(4242),
            "empty branch must fall back to commit_lsn"
        );

        drop_wals_table(&fpu, &table).await;
    }

    /// Cross-lane row identity — the shared-runs contract. `hot_path` can
    /// switch a timeline between the tsclient and YSQL lanes, so both must
    /// write byte-identical rows or a later read sees inconsistent bytes.
    /// Standing up a real `YbTsClient` needs a live tserver RPC
    /// endpoint, so this test pins the contract at the next best seam: from
    /// ONE `coalesce_runs` grouping, both lanes must serialize IDENTICAL
    /// (bucket, segno, chunk_offset, chunk_size, bytes) row tuples — the
    /// tsclient lane's payload via [`run_data_bytes`] (the exact buffer it
    /// binds), the YSQL lane's via [`RunData`]'s `to_sql` wire encoding.
    #[test]
    fn tsclient_and_ysql_lanes_serialize_identical_rows() {
        fn pc(segno: u64, off: i64, fill: u8, len: usize) -> PendingChunk {
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

        type RowTuple = (i16, i64, i64, i32, Vec<u8>);
        let mut tsclient_rows: Vec<RowTuple> = Vec::new();
        let mut ysql_rows: Vec<RowTuple> = Vec::new();
        let mut expected_rows: Vec<RowTuple> = Vec::new();
        for r in &runs {
            let chunks = &batch[r.clone()];
            // tsclient lane (flush_batch_tsclient): keyed by the run's first
            // chunk, sized as the run total, payload = run_data_bytes.
            let first = &chunks[0];
            let total: i32 = chunks.iter().map(|p| p.chunk_size).sum();
            tsclient_rows.push((
                first.bucket_id,
                first.segno as i64,
                first.chunk_offset,
                total,
                run_data_bytes(chunks).to_vec(),
            ));
            // YSQL lane (flush_batch_ysql): same key/size expressions;
            // payload is what RunData::to_sql streams onto the wire.
            let mut wire = bytes::BytesMut::new();
            RunData(chunks)
                .to_sql(&yb_tokio_postgres::types::Type::BYTEA, &mut wire)
                .expect("RunData::to_sql");
            ysql_rows.push((
                batch[r.start].bucket_id,
                batch[r.start].segno as i64,
                batch[r.start].chunk_offset,
                chunks.iter().map(|p| p.chunk_size).sum(),
                wire.to_vec(),
            ));
            // Ground truth: concatenation of the submitted chunk bytes.
            let mut bytes = Vec::new();
            for c in chunks {
                bytes.extend_from_slice(&c.data);
            }
            expected_rows.push((
                4,
                first.segno as i64,
                first.chunk_offset,
                bytes.len() as i32,
                bytes,
            ));
        }
        assert_eq!(
            tsclient_rows, ysql_rows,
            "both lanes must serialize a batch to IDENTICAL row tuples so a \
             timeline reads back the same rows regardless of which lane wrote it"
        );
        assert_eq!(
            tsclient_rows, expected_rows,
            "row tuples must be the exact per-run concatenation of the submitted chunks"
        );
    }

    /// Connect the thin-RPC (`yb_tsclient`) lane against dev YB for a
    /// scratch table (unqualified → `public` schema). Mirrors
    /// `YbWalConn::connect_tsclient`, which is bound to the production
    /// `safekeeper.wals` name.
    async fn tsclient_lane_for(client: &YsqlClient, table: &str) -> Arc<YbTsClient> {
        let (db_oid, oids) = ysql_client::lookup_table_oids(client, "public", &[table])
            .await
            .expect("lookup scratch table OIDs");
        let tserver =
            std::env::var("YB_TEST_TSERVER_RPC").unwrap_or_else(|_| "127.0.0.1:9100".into());
        let pg = yb_tsclient::TsClient::connect(yb_tsclient::TsClientConfig {
            tserver_addrs: vec![tserver],
            ..Default::default()
        })
        .await
        .expect("connect tsclient lane to dev YB; set YB_TEST_TSERVER_RPC to override");
        let wals = pg
            .open_table(db_oid, oids[table])
            .await
            .expect("open scratch table via tsclient");
        Arc::new(YbTsClient { client: pg, wals })
    }

    /// Live tsclient-lane round trip (`hot_path = "tsclient"`): flush
    /// through the real producer/consumer with the thin-RPC lane
    /// attached, then fetch through `YbSegmentReader` with no cache, so
    /// both hot paths run on the tsclient lane. With no YSQL fallback, a
    /// broken lane surfaces as an error here rather than silently
    /// round-tripping the bytes.
    #[tokio::test]
    async fn tsclient_lane_flush_and_fetch_round_trip() {
        use tokio::io::AsyncReadExt;

        let table = scratch_table("wals_test_tsclient");
        let client = Arc::new(
            YsqlClient::connect_no_tls(pgcfg(), true)
                .await
                .expect("connect to local YB; set YB_TEST_HOST/PORT/USER/DB to override"),
        );
        make_wals_table(&client, &table).await;
        let yb_tsclient = tsclient_lane_for(&client, &table).await;

        let branch_id = BranchId::generate();
        let start = Lsn(SEG_SIZE); // segno 1, offset 0
        let mut state = TimelinePersistentState::empty();
        state.branch_id = branch_id;
        state.timeline_start_lsn = start;
        state.local_start_lsn = start;
        state.server.wal_seg_size = SEG_SIZE as u32;

        let _tmp = camino_tempfile::tempdir().expect("tempdir");
        let yb_wal = Arc::new(YbWalConn {
            fpu_client: client.clone(),
            read_client: client.clone(),
            system_client: client.clone(),
            table: table.clone(),
            cache: None,
            yb_tsclient: Some(yb_tsclient),
        });
        let mut storage = YbWalStorage::new(&branch_id, &state, yb_wal.clone())
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
        // Baselines to prove each hot path actually ran on the tsclient
        // lane: its path-labeled metric advances only on the lane's arm,
        // so a dispatch bug routing to YSQL would leave these flat while
        // the cross-checks below still passed.
        let insert_before = crate::metrics::YB_WAL_INSERT_SECONDS
            .with_label_values(&["tsclient"])
            .get_sample_count();
        let fetch_before = crate::metrics::WAL_CACHE_YB_FETCH_SECONDS
            .with_label_values(&["tsclient"])
            .get_sample_count();

        // Durability barrier: the consumer flushes through the tsclient
        // lane (one Perform per coalesced batch).
        storage
            .flush_wal()
            .await
            .expect("flush_wal (tsclient lane)");
        assert!(
            crate::metrics::YB_WAL_INSERT_SECONDS
                .with_label_values(&["tsclient"])
                .get_sample_count()
                > insert_before,
            "flush did not run on the tsclient lane"
        );

        // Cross-transport visibility: plain-YSQL rehydration sees the
        // tsclient-written rows and lands exactly at the stream end.
        let lsn = YbWalStorage::load_write_lsn(
            &client,
            &table,
            &storage.branch_id,
            SEG_SIZE as usize,
            Lsn(0),
        )
        .await
        .expect("load_write_lsn");
        assert_eq!(
            lsn, pos,
            "rehydrated end must equal the submitted stream end"
        );

        // Fetch with no cache: every read goes through the tsclient
        // two-arm fan-out (2×WALS_BUCKETS read ops, ONE Perform, one
        // snapshot by construction).
        let mut reader = YbSegmentReader::new(
            yb_wal,
            &branch_id,
            1,
            SEG_SIZE as usize,
            0,
            Arc::new(std::sync::atomic::AtomicU64::new(u64::MAX)),
        );
        let mut got = vec![0u8; stream.len()];
        reader
            .read_exact(&mut got)
            .await
            .expect("read_exact via tsclient lane");
        assert_eq!(
            got, stream,
            "tsclient fetch must round-trip the flushed bytes"
        );
        assert!(
            crate::metrics::WAL_CACHE_YB_FETCH_SECONDS
                .with_label_values(&["tsclient"])
                .get_sample_count()
                > fetch_before,
            "fetch did not run on the tsclient lane"
        );

        drop_wals_table(&client, &table).await;
    }
}
