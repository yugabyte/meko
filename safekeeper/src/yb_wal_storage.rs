//! YugabyteDB-mirrored WAL storage.
//!
//! Wraps [`PhysicalStorage`] so that every WAL write is also persisted to a
//! YB table (`safekeeper.wals`). YB becomes the durable copy;
//! the local FS remains the read path (used by `WalReader`) and a hot cache
//! for re-reads of recent WAL.
//!
//! Phase 1 (this module): write-through mirror. `flush_wal()` doesn't return
//! until the corresponding bytes have been INSERT'd into YB.
//!
//! Phase 2 (future): make `WalReader` capable of fetching from YB, so the
//! local FS becomes truly optional.
//!
//! Schema (one row per `write_wal()` call within a segment; multiple rows
//! across a segment boundary):
//!
//! ```sql
//! CREATE TABLE safekeeper.wals (
//!     branch_id        TEXT    NOT NULL,
//!     segno            BIGINT  NOT NULL,
//!     chunk_offset     BIGINT  NOT NULL,   -- byte offset of `data` within the segment
//!     chunk_size       INT     NOT NULL,   -- redundant: length(data)
//!     data             BYTEA   NOT NULL,
//!     last_modified_us BIGINT  NOT NULL,
//!     PRIMARY KEY ((branch_id) HASH, segno, chunk_offset)
//! );
//! ```
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
//!   intact (the local-FS truncation is the source of truth for that case —
//!   the reader uses local FS, so a tiny over-keep in YB is harmless).
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
//! channel greedily, builds one multi-row `INSERT ... VALUES (...), (...)`
//! per batch — up to [`FLUSH_ROWS_THRESHOLD`] rows or
//! [`FLUSH_BYTES_THRESHOLD`] bytes — runs it through [`YsqlClient`], and
//! publishes the highest end_lsn of each successful batch onto a
//! `tokio::sync::watch` watermark. This lets the producer keep pushing
//! the *next* batch while the previous one is in flight, which the
//! single-threaded design couldn't do (every `flush_pending` held
//! `&mut self` across the YB round-trip).
//!
//! `flush_wal()` is the durability sync point: it records the highest
//! end_lsn the producer has pushed (`submitted_max_lsn`) and awaits the
//! watermark catching up. The walproposer only treats data as durable
//! after a successful `flush_wal()`, so any crash between `write_wal()`
//! returning and the batch landing in YB is a no-op for the compute (no
//! commit ack was sent).
//!
//! `truncate_wal` waits the same way before issuing the DELETE, so a
//! pending chunk past `end_pos` cannot land after the truncate.
//!
//! ## Failure handling
//!
//! On a batch INSERT failure (after `YsqlClient`'s own auto-retry has
//! given up — e.g. persistent auth / schema / connectivity failure),
//! the consumer publishes [`DurableState::Failed`] onto the watermark
//! and exits. The producer's next `tx.send()` then fails (rx dropped),
//! `write_wal` returns the captured error, the walproposer reconnects,
//! and the safekeeper handles the rest via the consensus state machine.
//! `flush_wal` waiters see `Failed` via the watermark and propagate the
//! same error.
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
use crate::wal_storage::{PhysicalStorage, Storage};

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
///   transient/connection-drop errors; without it, the e2e
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
}

impl YbWalConn {
    /// Build from the SK config. Returns `None` when
    /// `yb_storage_conn` isn't set (which yb-light shouldn't be
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
        //   breaks the `restart_wal_replay` e2e scenario with a
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
        Ok(Some(Arc::new(YbWalConn {
            fpu_client,
            read_client,
            system_client,
            table: WALS_TABLE.to_string(),
            cache,
        })))
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

/// Durability state published by the consumer onto the watch channel.
#[derive(Clone, Debug)]
enum DurableState {
    /// Every `PendingChunk` whose `end_lsn <= self.0` has been durably
    /// INSERT'd into YugabyteDB.
    Healthy(Lsn),
    /// The consumer task hit a non-recoverable error and stopped
    /// processing. The string is the rendered error chain for
    /// propagation up through `flush_wal` / `write_wal`.
    Failed(String),
}

pub struct YbWalStorage {
    inner: PhysicalStorage,
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
                .unwrap_or(postgres_ffi::PgMajorVersion::PG17);
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
            let scanned = if max_offset <= state.timeline_start_lsn {
                Ok(max_offset)
            } else {
                match find_anchor(&src, max_offset).await {
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

        // PhysicalStorage holds the LSN tracker + WAL decoder for this
        // stream — durability is our job.
        let inner = PhysicalStorage::new(branch_id, state, write_lsn)?;

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
        let min_segno =
            Self::load_min_segno(&system_client, &table_name, &branch_id_str).await?;

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
            cache,
            delete_lower_bound: Arc::new(AtomicU64::new(min_segno)),
            durable_lsn,
            _consumer_handle: consumer_handle,
        };

        // No post-crash row scrub: `load_write_lsn` already takes the
        // max chunk-end across the table, so any committed-but-unacked
        // rows from a crashed run are absorbed as durable rather than
        // discarded. yb-light has no truncation-past-durable case that
        // would justify a DELETE here.
        Ok(me)
    }

    /// Find the end of the persisted WAL by scanning the YB table for the
    /// highest end-byte across all segnos for this timeline. Falls back to
    /// `commit_lsn` when the table is empty (fresh timeline).
    async fn load_write_lsn(
        client: &YsqlClient,
        table: &str,
        branch_id: &str,
        wal_seg_size: usize,
        commit_lsn: Lsn,
    ) -> Result<Lsn> {
        let sql = bake_branch_id(
            &format!(
                "SELECT segno, max(chunk_offset + chunk_size) AS end_off FROM {} \
                 WHERE branch_id = $1 \
                 GROUP BY segno ORDER BY segno DESC LIMIT 1",
                quote_relation(table)
            ),
            branch_id,
        );
        let row_opt = client
            .query_opt(&sql, &[])
            .await
            .context("YbWalStorage: rehydrate write_lsn from YB")?;
        let lsn = match row_opt {
            Some(row) => {
                let segno: i64 = row.get("segno");
                let end_off: i64 = row.get("end_off");
                Lsn((segno as u64) * (wal_seg_size as u64) + end_off as u64)
            }
            None => commit_lsn,
        };
        Ok(lsn)
    }

    /// Lowest segno still present in `wals` for this timeline. Used to
    /// seed [`Self::delete_lower_bound`] at construction so the first
    /// `remove_up_to(N)` DELETE after a safekeeper restart is bounded
    /// from below rather than scanning from 0.
    async fn load_min_segno(client: &YsqlClient, table: &str, branch_id: &str) -> Result<u64> {
        let sql = bake_branch_id(
            &format!(
                "SELECT min(segno) AS m FROM {} \
                 WHERE branch_id = $1",
                quote_relation(table)
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
            // could no longer serve. See findings-ps-wal-replay-
            // restart-bug-2026-06-04.

            let pc = PendingChunk {
                segno,
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
    async fn wait_for_durable(&mut self, target: Lsn) -> Result<()> {
        let res = self
            .watermark_rx
            .wait_for(|state| match state {
                DurableState::Healthy(d) => *d >= target,
                DurableState::Failed(_) => true,
            })
            .await;
        let state = match res {
            Ok(guard) => guard.clone(),
            Err(_) => {
                anyhow::bail!("YB WAL consumer task gone (watermark channel closed)");
            }
        };
        match state {
            DurableState::Healthy(_) => Ok(()),
            DurableState::Failed(msg) => Err(anyhow::anyhow!(msg)),
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
        // (election with `start_streaming_at` past the persisted tail);
        // returning the uncapped value to walproposer or to a Raft vote
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
        // after seeing our capped `flush_lsn()` on the next VoteResponse.
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
        // after the YB rows were deleted above — so when the next term
        // rewrites WAL forward from `end_pos`, the interpreted-WAL reader
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

    async fn flush_wal(&mut self) -> Result<()> {
        // Durability sync point — wake the consumer (cut its
        // accumulation wait short if any), wait for the watermark to
        // catch up to the highest LSN write_wal has pushed, then advance
        // the inner LSN watermark. The walproposer treats this as the
        // durable-commit boundary, so the flush MUST complete (or return
        // an error) before this returns.
        self.flush_notify.notify_one();
        self.wait_for_durable(self.submitted_max_lsn).await?;
        self.inner.flush_wal().await
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
            let lower = delete_lower_bound.load(Ordering::Relaxed);
            let sql = bake_branch_id(
                &format!(
                    "DELETE FROM {} WHERE branch_id = $1 \
                     AND segno >= $2 AND segno < $3",
                    quote_relation(&table)
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
        let sql = format!(
            "SELECT chunk_offset, data FROM {} \
             WHERE branch_id = $1 AND segno = $2 AND chunk_offset <= $3 \
             ORDER BY chunk_offset DESC LIMIT 1",
            quote_relation(self.table)
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

/// Discard all WAL strictly beyond `end_pos`: drop chunks that start at
/// or after it, and truncate the chunk straddling it down to `end_pos`.
/// After this, `max(chunk_offset + chunk_size)` for the timeline is
/// exactly `end_pos` — the YB equivalent of truncating a local WAL
/// segment file. Used to discard a torn tail on safekeeper restart and to
/// honor a walproposer truncation on election. Both statements are
/// idempotent (keyed DELETE; absolute-`SET` UPDATE whose predicate stops
/// matching once the chunk has been truncated).
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

    // Chunks that begin at or after end_pos are entirely torn — drop them.
    let del = format!(
        "DELETE FROM {table} WHERE branch_id = $1 \
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
         WHERE branch_id = $1 \
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
///   4. INSERTing the whole batch via `YsqlClient`.
///   5. Publishing the highest `end_lsn` of the batch onto `watermark_tx`
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
    table_name: String,
    branch_id: String,
    watermark_tx: watch::Sender<DurableState>,
    flush_notify: Arc<tokio::sync::Notify>,
    durable_lsn: Arc<AtomicU64>,
    cache: Option<Arc<std::sync::Mutex<crate::wal_cache::WalCache>>>,
) {
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

        let highest_lsn = batch.last().map(|c| c.end_lsn).expect("batch is non-empty");

        match flush_batch(&ysql_fpu_client, &table_name, &branch_id, &batch).await {
            Ok(()) => {
                // Populate the read-side cache from the batch BEFORE
                // publishing the durability advance. The cache is the
                // WAL sender's fast path; we only insert chunks here
                // (after a successful INSERT) so the cache never
                // serves bytes that aren't durable in
                // `safekeeper.wals`. See findings-ps-wal-replay-
                // restart-bug-2026-06-04 for the failure mode that
                // earlier pre-INSERT cache population caused.
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

/// Build and execute one multi-row `INSERT ... VALUES (...), (...)`
/// for the entire batch. Returns the error chain on failure (the
/// statement is atomic — all rows commit or none).
async fn flush_batch(
    ysql_fpu_client: &YsqlClient,
    table_name: &str,
    branch_id: &str,
    batch: &[PendingChunk],
) -> Result<()> {
    if batch.is_empty() {
        return Ok(());
    }
    let n = batch.len();
    let now_us: i64 = SystemTime::now()
        .duration_since(UNIX_EPOCH)
        .map(|d| d.as_micros() as i64)
        .unwrap_or(0);

    let mut sql = String::with_capacity(128 + n * 96);
    sql.push_str("INSERT INTO ");
    sql.push_str(&quote_relation(table_name));
    sql.push_str(
        " (branch_id, segno, chunk_offset, \
          chunk_size, data, last_modified_us) VALUES ",
    );
    for i in 0..n {
        if i > 0 {
            sql.push_str(", ");
        }
        let b = i * 5;
        use std::fmt::Write as _;
        // branch_id baked as a literal (fixed for this instance's
        // lifetime); only the 5 per-row columns bind.
        let _ = write!(
            sql,
            "('{branch_id}',${},${},${},${},${})",
            b + 1,
            b + 2,
            b + 3,
            b + 4,
            b + 5,
        );
    }

    let segno_is: Vec<i64> = batch.iter().map(|p| p.segno as i64).collect();
    let offset_is: Vec<i64> = batch.iter().map(|p| p.chunk_offset).collect();
    let size_is: Vec<i32> = batch.iter().map(|p| p.chunk_size).collect();
    // `PendingChunk::data` is `Arc<Vec<u8>>` (shared with WalCache).
    // tokio_postgres' ToSql doesn't see through Arc, so deref each entry
    // to a `&[u8]` for the param vector.
    let datas: Vec<&[u8]> = batch.iter().map(|p| p.data.as_slice()).collect();

    let mut params: Vec<&(dyn ToSql + Sync)> = Vec::with_capacity(n * 5);
    for i in 0..n {
        params.push(&segno_is[i]);
        params.push(&offset_is[i]);
        params.push(&size_is[i]);
        params.push(&datas[i]);
        params.push(&now_us);
    }

    ysql_fpu_client
        .execute(&sql, &params)
        .await
        .with_context(|| format!("YB WAL batched INSERT ({n} rows)"))?;
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
        let (tx, rx) = tokio::sync::oneshot::channel();
        tokio::spawn(async move {
            let res = fetch_chunks_from_yb(&client, &table, &branch_id, segno, pos)
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
                    // Neither cache nor YB has a chunk covering self.pos.
                    // This is a *real* hole — far better to fail loudly
                    // here than serve zeros and let the pageserver hit
                    // `xlp_magic=0` three layers down.
                    me.fetch = None;
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
                    return Poll::Ready(Err(std::io::Error::other(
                        format!(
                            "YB WAL fetch failed for segno {} offset {}: {e}",
                            me.segno, me.pos
                        ),
                    )));
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
    // boundary-aligned path simply returns no covering row.
    let sql = bake_branch_id(
        &format!(
            "(SELECT chunk_offset, chunk_size, data FROM {tbl} \
                WHERE branch_id = $1 AND segno = $2 \
                  AND chunk_offset < $3 \
                ORDER BY chunk_offset DESC LIMIT 1) \
             UNION ALL \
             (SELECT chunk_offset, chunk_size, data FROM {tbl} \
                WHERE branch_id = $1 AND segno = $2 \
                  AND chunk_offset >= $3 AND chunk_offset < $4 \
                ORDER BY chunk_offset ASC LIMIT $5)",
            tbl = quote_relation(table)
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

    crate::metrics::WAL_CACHE_YB_FETCH_SECONDS.observe(elapsed.as_secs_f64());
    crate::metrics::WAL_CACHE_YB_FETCH_CHUNKS.observe(out.len() as f64);
    crate::metrics::WAL_CACHE_YB_FETCH_BYTES.observe(total_bytes as f64);

    Ok(out)
}

/// WAL storage used by the safekeeper. Kept as a type alias so existing
/// `<WAL: wal_storage::Storage>` generics on `SafeKeeper` keep working
/// after the local-FS variant was removed.
pub type WalStore = YbWalStorage;

impl YbWalStorage {
    /// Forwarded passthrough for `PhysicalStorage::internal_state`. Used by
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

    /// One-off `wals`-shaped table per test so concurrent runs and dirty
    /// state from prior failures can't interfere.
    async fn make_wals_table(client: &YsqlClient, table: &str) {
        let q = quote_relation(table);
        client
            .execute(&format!("DROP TABLE IF EXISTS {q}"), &[])
            .await
            .expect("drop scratch wals table");
        client
            .execute(
                &format!(
                    "CREATE TABLE {q} (\
                       branch_id        TEXT    NOT NULL,\
                       segno            BIGINT  NOT NULL,\
                       chunk_offset     BIGINT  NOT NULL,\
                       chunk_size       INT     NOT NULL,\
                       data             BYTEA   NOT NULL,\
                       last_modified_us BIGINT  NOT NULL,\
                       PRIMARY KEY ((branch_id) HASH, segno, chunk_offset)\
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
            cache: Some(Arc::new(std::sync::Mutex::new(WalCache::new(64 * 1024 * 1024)))),
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
        let table = format!("wals_test_trunc_cache_{}", rand_suffix());
        // start == durable tail; segno 1, offset 0. truncate back to it is
        // the "election truncates to the durable tail" case.
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
    /// — exactly the "election with start_streaming_at past the
    /// persisted tail" pattern. With Fix 2 we bail; without it,
    /// `inner.truncate_wal(end_pos)` advances `flush_record_lsn` past
    /// `durable_lsn` and opens the gap.
    #[tokio::test]
    async fn truncate_wal_refuses_end_pos_past_durable() {
        let table = format!("wals_test_truncate_{}", rand_suffix());
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
        let table = format!("wals_test_cap_{}", rand_suffix());
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

    fn rand_suffix() -> String {
        let pid = std::process::id();
        let nanos = std::time::UNIX_EPOCH
            .elapsed()
            .map(|d| d.as_nanos())
            .unwrap_or(0);
        format!("{pid}_{nanos}")
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
        let table = format!("wals_test_lane_view_{}", rand_suffix());
        let (fpu, _read, system) = open_three_lanes().await;
        make_wals_table(&fpu, &table).await;

        let branch_s = BranchId::generate().to_string();
        // Two non-overlapping chunks in segno 0. Highest end-offset
        // should be 8192 + 256 = 8448.
        let rows: &[(i64, i64, i32)] = &[(0, 0, 1024), (0, 8192, 256)];
        let now_us: i64 = 0;
        let blob_small = vec![0xabu8; 1024];
        let blob_tail = vec![0xcdu8; 256];
        for (segno, offset, size) in rows {
            let blob: &[u8] = if *size as usize == blob_small.len() {
                &blob_small
            } else {
                &blob_tail
            };
            fpu.execute(
                &format!(
                    "INSERT INTO {} (branch_id, segno, chunk_offset, \
                                     chunk_size, data, last_modified_us) \
                     VALUES ($1, $2, $3, $4, $5, $6)",
                    quote_relation(&table)
                ),
                &[&branch_s, segno, offset, size, &blob, &now_us],
            )
            .await
            .expect("INSERT via fpu");
        }

        // Read via system_client — the lane that does the real
        // load_write_lsn at SK startup.
        let lsn = YbWalStorage::load_write_lsn(
            &system,
            &table,
            &branch_s,
            SEG_SIZE as usize,
            Lsn(0),
        )
        .await
        .expect("load_write_lsn via system_client");
        let expected = 8192u64 + 256u64;
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
        let table = format!("wals_test_trunc_above_{}", rand_suffix());
        let (fpu, _read, system) = open_three_lanes().await;
        make_wals_table(&fpu, &table).await;
        let branch_s = BranchId::generate().to_string();

        // Three chunks in segno 0 spanning [0, 20480): two full pages + a
        // partial. Cut at 10000 — mid the second chunk [8192, 16384).
        let chunks: &[(i64, i64, i32)] = &[(0, 0, 8192), (0, 8192, 8192), (0, 16384, 4096)];
        for (segno, offset, size) in chunks {
            let blob = vec![0x5au8; *size as usize];
            fpu.execute(
                &format!(
                    "INSERT INTO {} (branch_id, segno, chunk_offset, \
                                     chunk_size, data, last_modified_us) \
                     VALUES ($1, $2, $3, $4, $5, $6)",
                    quote_relation(&table)
                ),
                &[&branch_s, segno, offset, size, &blob, &0i64],
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
        assert_eq!(lsn, Lsn(10000), "end of WAL must equal the truncation point");

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
    /// order. Bypasses `PhysicalStorage::write_wal` (which validates a
    /// real PG WAL page header that we don't want to synthesise here),
    /// so this stays focused on the YB read path: chunk-row selection,
    /// `pos` advance across chunk boundaries, and the read-lane SELECT.
    #[tokio::test]
    async fn segment_reader_round_trip_across_chunks() {
        use tokio::io::AsyncReadExt;

        let table = format!("wals_test_reader_{}", rand_suffix());
        let (fpu, _read, _system) = open_three_lanes().await;
        make_wals_table(&fpu, &table).await;

        let branch_id = BranchId::generate();
        let branch_s = branch_id.to_string();

        let chunk_a: Vec<u8> = (0u8..=255).cycle().take(1024).collect();
        let chunk_b: Vec<u8> = (0u8..=255).cycle().take(2048).map(|b| b ^ 0xa5).collect();
        let now_us: i64 = 0;
        let segno_i: i64 = 0;
        for (offset_i, size_i, data) in [
            (0i64, 1024i32, chunk_a.as_slice()),
            (1024i64, 2048i32, chunk_b.as_slice()),
        ] {
            fpu.execute(
                &format!(
                    "INSERT INTO {} (branch_id, segno, chunk_offset, \
                                     chunk_size, data, last_modified_us) \
                     VALUES ($1, $2, $3, $4, $5, $6)",
                    quote_relation(&table)
                ),
                &[
                    &branch_s,
                    &segno_i,
                    &offset_i,
                    &size_i,
                    &data,
                    &now_us,
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

        let table = format!("wals_test_midchunk_{}", rand_suffix());
        let (fpu, _read, _system) = open_three_lanes().await;
        make_wals_table(&fpu, &table).await;

        let branch_id = BranchId::generate();
        let branch_s = branch_id.to_string();

        // One chunk spanning [0, 2048).
        let chunk: Vec<u8> = (0u8..=255).cycle().take(2048).collect();
        fpu.execute(
            &format!(
                "INSERT INTO {} (branch_id, segno, chunk_offset, \
                                 chunk_size, data, last_modified_us) \
                 VALUES ($1, 0, 0, 2048, $2, 0)",
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

        let table = format!("wals_test_coherence_{}", rand_suffix());
        let (fpu, _read, _system) = open_three_lanes().await;
        make_wals_table(&fpu, &table).await;

        let branch_id = BranchId::generate();
        let branch_s = branch_id.to_string();

        let seg: i64 = 0;
        let committed: Vec<u8> = vec![0xAA; 1024]; // [0,1024), below flush
        let tail_old: Vec<u8> = vec![0xBB; 3072]; // [1024,4096), above flush
        let tail_new: Vec<u8> = vec![0xCC; 3072]; // the rewrite

        let insert_sql = format!(
            "INSERT INTO {} (branch_id, segno, chunk_offset, \
                             chunk_size, data, last_modified_us) \
             VALUES ($1, $2, $3, $4, $5, 0)",
            quote_relation(&table)
        );
        let delete_sql = format!(
            "DELETE FROM {} WHERE branch_id = $1 \
             AND segno = $2 AND chunk_offset = $3",
            quote_relation(&table)
        );

        // 1. Stage the committed chunk and the (still-uncommitted) tail.
        for (off, bytes) in [(0i64, &committed), (1024i64, &tail_old)] {
            let size = bytes.len() as i32;
            let slice = bytes.as_slice();
            fpu.execute(
                &insert_sql,
                &[&branch_s, &seg, &off, &size, &slice],
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
        });

        // flush = 1024: [0,1024) committed, [1024,4096) not yet.
        let flush = Arc::new(AtomicU64::new(1024));
        let mut reader =
            YbSegmentReader::new(yb_wal.clone(), &branch_id, 0, SEG_SIZE as usize, 0, flush.clone());

        // 2. Read the committed range. The fetch sees both chunks, but only the
        //    committed one is cacheable.
        let mut head = vec![0u8; 1024];
        reader.read_exact(&mut head).await.expect("read committed head");
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
            &[&branch_s, &seg, &1024i64, &tail_size, &tail_new_slice],
        )
        .await
        .expect("INSERT rewritten tail");
        flush.store(4096, Ordering::Release);

        // 4. The now-committed tail MUST come back as the rewrite.
        let mut tail = vec![0u8; 3072];
        reader.read_exact(&mut tail).await.expect("read rewritten tail");
        assert_eq!(
            tail, tail_new,
            "reader served a stale cached tail after a term rewrite (#139)"
        );

        drop_wals_table(&fpu, &table).await;
    }

    /// Read-past-flush splice (the true root cause of #139). The YB fetch is
    /// blind to the commit/flush frontier: it pulls a whole covering chunk,
    /// including bytes that lie *above* flush and are therefore still
    /// uncommitted. yb-light WAL is immutable only once committed — a later
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

        let table = format!("wals_test_pastflush_{}", rand_suffix());
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
            "INSERT INTO {} (branch_id, segno, chunk_offset, \
                             chunk_size, data, last_modified_us) \
             VALUES ($1, $2, $3, $4, $5, 0)",
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
            &[&branch_s, &seg, &off, &size, &old_slice],
        )
        .await
        .expect("INSERT old chunk");

        let yb_wal = Arc::new(YbWalConn {
            fpu_client: fpu.clone(),
            read_client: fpu.clone(),
            system_client: fpu.clone(),
            table: table.clone(),
            cache: None,
        });

        // Live flush frontier, as the streaming reader would publish it.
        let flush = Arc::new(AtomicU64::new(1024));
        let mut reader =
            YbSegmentReader::new(yb_wal.clone(), &branch_id, 0, SEG_SIZE as usize, 0, flush.clone());

        // 2. Read everything committed so far ([0,1024)). The reader fetches
        //    the whole covering chunk [0,4096) but must trust it only to 1024.
        let mut head = vec![0u8; 1024];
        reader.read_exact(&mut head).await.expect("read committed head");
        assert_eq!(head, old[..1024], "committed prefix must read back");

        // 3. A new term rewrites the once-uncommitted tail; flush then advances
        //    to cover it.
        fpu.execute(&delete_sql, &[&branch_s, &seg, &off])
            .await
            .expect("DELETE old chunk");
        let new_slice = new.as_slice();
        fpu.execute(
            &insert_sql,
            &[&branch_s, &seg, &off, &size, &new_slice],
        )
        .await
        .expect("INSERT rewritten chunk");
        flush.store(4096, Ordering::Release);

        // 4. The now-committed [1024,4096) MUST come back as the rewrite, not
        //    the pre-rewrite bytes the reader fetched (and may still hold).
        let mut tail = vec![0u8; 3072];
        reader.read_exact(&mut tail).await.expect("read rewritten tail");
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

        let table = format!("wals_test_pastflush_seg1_{}", rand_suffix());
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
            "INSERT INTO {} (branch_id, segno, chunk_offset, \
                             chunk_size, data, last_modified_us) \
             VALUES ($1, $2, $3, $4, $5, 0)",
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
            &[&branch_s, &seg, &off, &size, &old_slice],
        )
        .await
        .expect("INSERT old chunk");

        let yb_wal = Arc::new(YbWalConn {
            fpu_client: fpu.clone(),
            read_client: fpu.clone(),
            system_client: fpu.clone(),
            table: table.clone(),
            cache: None,
        });

        // Flush frontier is ABSOLUTE: committed up to offset 1024 in segment 1.
        let flush = Arc::new(AtomicU64::new(seg_base + 1024));
        let mut reader =
            YbSegmentReader::new(yb_wal.clone(), &branch_id, 1, SEG_SIZE as usize, 0, flush.clone());

        let mut head = vec![0u8; 1024];
        reader.read_exact(&mut head).await.expect("read committed head");
        assert_eq!(head, old[..1024], "committed prefix must read back");

        fpu.execute(&delete_sql, &[&branch_s, &seg, &off])
            .await
            .expect("DELETE old chunk");
        let new_slice = new.as_slice();
        fpu.execute(
            &insert_sql,
            &[&branch_s, &seg, &off, &size, &new_slice],
        )
        .await
        .expect("INSERT rewritten chunk");
        flush.store(seg_base + 4096, Ordering::Release);

        let mut tail = vec![0u8; 3072];
        reader.read_exact(&mut tail).await.expect("read rewritten tail");
        assert_eq!(
            tail,
            new[1024..],
            "seg-1 reader served stale past-flush WAL after a term rewrite — \
             the absolute-vs-segment-relative flush-bound no-op (#139)"
        );

        drop_wals_table(&fpu, &table).await;
    }
}
