//! YugabyteDB-mirrored WAL storage.
//!
//! Wraps [`PhysicalStorage`] so that every WAL write is also persisted to a
//! YB table (`yb_storage.wals` by default). YB becomes the durable copy;
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
//! CREATE TABLE wals (
//!     tenant_id        TEXT    NOT NULL,
//!     timeline_id      TEXT    NOT NULL,
//!     segno            BIGINT  NOT NULL,
//!     chunk_offset     BIGINT  NOT NULL,   -- byte offset of `data` within the segment
//!     chunk_size       INT     NOT NULL,   -- redundant: length(data)
//!     data             BYTEA   NOT NULL,
//!     last_modified_us BIGINT  NOT NULL,
//!     PRIMARY KEY ((tenant_id, timeline_id) HASH, segno, chunk_offset)
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
use yb_tokio_postgres::types::ToSql;
use utils::id::TenantTimelineId;
use utils::lsn::Lsn;

use crate::metrics::WalStorageMetrics;
use crate::state::TimelinePersistentState;
use crate::wal_cache::WalCache;
use crate::wal_storage::{PhysicalStorage, Storage};

// Re-export the auto-reconnecting Postgres ysql_fpu_client wrapper
// shared with the pageserver's `YbStore`. See `ysql_client::YsqlClient`
// for the reconnect / retry-once semantics.
pub use ysql_client::YsqlClient;

/// Handles to the YB connection that mirrors live WAL into
/// `yb_storage.wals`. Constructed at safekeeper boot via
/// [`YbWalConn::from_conf`] and shared across timelines via
/// `Arc<YbWalConn>`. Used by the WAL writer ([`YbWalStorage::new`]),
/// the WAL reader ([`YbSegmentReader::new`]), and (since task 19)
/// the control-file storage ([`crate::control_file::YbControlFileStorage`]).
pub struct YbWalConn {
    /// Auto-reconnecting wrapper around `tokio_postgres::Client`. A
    /// TCP-level disconnect (e.g. the YB tserver restarting) triggers
    /// a transparent reconnect on the next operation — without this
    /// wrapper the process held a corpse `Arc<Client>` until restart.
    pub client: Arc<YsqlClient>,
    /// Name of the YB table that backs the WAL mirror
    /// (`public.wals` by default, configurable via `--wal-yb-config
    /// table_name='...'`).
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
    /// `--wal-yb-config` isn't set (which yb-light shouldn't be
    /// running without — every persistence path needs this).
    pub async fn from_conf(
        conf: &crate::SafeKeeperConf,
    ) -> anyhow::Result<Option<Arc<YbWalConn>>> {
        use anyhow::Context;
        let Some(yb) = conf.wal_yb_config.as_ref() else {
            return Ok(None);
        };
        // Same factory the pageserver's YbStore uses — honors
        // sslmode + sslrootcert + password_env consistently.
        // `set_fast_path_upsert = true`: this client serves
        // WAL chunk INSERTs, WAL GC, and the control-file UPSERT
        // — all idempotent / last-writer-wins, so YB's
        // read-before-write check adds nothing.
        let client = ysql_client::YsqlClient::from_yb_config(
            &yb.conn_config(),
            "safekeeper:yb_wal_mirror",
            true,
        )
        .await
        .with_context(|| {
            format!(
                "connect WAL mirror YB at {}:{} (sslmode={:?})",
                yb.host, yb.port, yb.sslmode
            )
        })?;
        let cache = if conf.wal_cache_size_mib > 0 {
            Some(Arc::new(std::sync::Mutex::new(WalCache::new(
                conf.wal_cache_size_mib * 1024 * 1024,
            ))))
        } else {
            None
        };
        Ok(Some(Arc::new(YbWalConn {
            client: Arc::new(client),
            table: yb.table_name.clone(),
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
    /// Auto-reconnecting wrapper around `tokio_postgres::Client`. Used
    /// directly for the synchronous DELETE paths (`truncate_wal`,
    /// `remove_up_to`). The consumer task gets a clone of this `Arc` for
    /// its batched INSERTs.
    ysql_fpu_client: Arc<YsqlClient>,
    tenant_id: String,
    timeline_id: String,
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
        ttid: &TenantTimelineId,
        state: &TimelinePersistentState,
        yb_wal: Arc<YbWalConn>,
    ) -> Result<Self> {
        let ysql_fpu_client = yb_wal.client.clone();
        let table_name = yb_wal.table.clone();
        let cache = yb_wal.cache.clone();
        let wal_seg_size = state.server.wal_seg_size as usize;
        let tenant_id_str = ttid.tenant_id.to_string();
        let timeline_id_str = ttid.timeline_id.to_string();

        // Rehydrate `write_lsn` from YB rather than scanning local FS. Take
        // the highest `chunk_offset + chunk_size` across all segnos for this
        // (tenant, timeline) as the end-of-WAL position.
        let write_lsn = Self::load_write_lsn(
            &ysql_fpu_client,
            &table_name,
            &tenant_id_str,
            &timeline_id_str,
            wal_seg_size,
            state.timeline_start_lsn,
        )
        .await?;
        tracing::info!(
            tenant_id = %tenant_id_str,
            timeline_id = %timeline_id_str,
            %write_lsn,
            cfile_local_start_lsn = %state.local_start_lsn,
            cfile_timeline_start_lsn = %state.timeline_start_lsn,
            "YbWalStorage::new — rehydrated write_lsn from yb_storage.wals \
             (falls back to timeline_start_lsn on empty)",
        );

        // PhysicalStorage holds the LSN tracker + WAL decoder for this
        // stream — durability is our job.
        let inner = PhysicalStorage::new(ttid, state, write_lsn)?;

        // Producer/consumer plumbing. The watermark starts at
        // `write_lsn` so an immediately-following `flush_wal()` with
        // `submitted_max_lsn == write_lsn` returns at once (nothing to
        // wait for).
        let (tx, rx) = mpsc::channel::<PendingChunk>(CHUNK_CHANNEL_CAPACITY);
        let (watermark_tx, watermark_rx) =
            watch::channel(DurableState::Healthy(write_lsn));
        let flush_notify = Arc::new(tokio::sync::Notify::new());
        let durable_lsn = Arc::new(AtomicU64::new(write_lsn.0));
        let consumer_handle = tokio::spawn(consumer_main(
            rx,
            ysql_fpu_client.clone(),
            table_name.clone(),
            tenant_id_str.clone(),
            timeline_id_str.clone(),
            watermark_tx,
            flush_notify.clone(),
            durable_lsn.clone(),
            cache.clone(),
        ));

        // Seed the DELETE lower bound from YB so post-restart removals
        // don't scan/tombstone the empty range below the first surviving
        // segno. Cheap: one tablet lookup keyed on (tenant, timeline).
        let min_segno = Self::load_min_segno(
            &ysql_fpu_client,
            &table_name,
            &tenant_id_str,
            &timeline_id_str,
        )
        .await?;

        let me = YbWalStorage {
            inner,
            ysql_fpu_client,
            tenant_id: tenant_id_str,
            timeline_id: timeline_id_str,
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
        // discarded. yb-mode has no truncation-past-durable case that
        // would justify a DELETE here.
        Ok(me)
    }

    /// Find the end of the persisted WAL by scanning the YB table for the
    /// highest end-byte across all segnos for this timeline. Falls back to
    /// `commit_lsn` when the table is empty (fresh timeline).
    async fn load_write_lsn(
        ysql_fpu_client: &YsqlClient,
        table: &str,
        tenant_id: &str,
        timeline_id: &str,
        wal_seg_size: usize,
        commit_lsn: Lsn,
    ) -> Result<Lsn> {
        let sql = format!(
            "SELECT segno, max(chunk_offset + chunk_size) AS end_off FROM {} \
             WHERE tenant_id = $1 AND timeline_id = $2 \
             GROUP BY segno ORDER BY segno DESC LIMIT 1",
            quote_ident(table)
        );
        let params: [&(dyn ToSql + Sync); 2] = [&tenant_id, &timeline_id];
        let row_opt = ysql_fpu_client
            .query_opt(&sql, &params)
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
    async fn load_min_segno(
        ysql_fpu_client: &YsqlClient,
        table: &str,
        tenant_id: &str,
        timeline_id: &str,
    ) -> Result<u64> {
        let sql = format!(
            "SELECT min(segno) AS m FROM {} \
             WHERE tenant_id = $1 AND timeline_id = $2",
            quote_ident(table)
        );
        let params: [&(dyn ToSql + Sync); 2] = [&tenant_id, &timeline_id];
        // Aggregate over an empty set still returns one row (NULL); a
        // missing row would mean the query itself didn't run.
        let row = ysql_fpu_client
            .query_opt(&sql, &params)
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
            // INSERT'd into `yb_storage.wals`. Populating it here
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
        // No DELETE in yb-mode: with single-SK quorum, `end_pos` is
        // always exactly the SK's confirmed durable LSN on election,
        // so there are no rows past `end_pos` to drop. Only the
        // in-memory watermarks need resetting.
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
        let ysql_fpu_client = self.ysql_fpu_client.clone();
        let table = self.table_name.clone();
        let tenant_id = self.tenant_id.clone();
        let timeline_id = self.timeline_id.clone();
        let cache = self.cache.clone();
        let delete_lower_bound = self.delete_lower_bound.clone();
        async move {
            // Drop cached chunks for segnos that are about to be DELETE'd
            // out of `yb_storage.wals`. Done before the DB delete so a
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
            let sql = format!(
                "DELETE FROM {} WHERE tenant_id = $1 AND timeline_id = $2 \
                 AND segno >= $3 AND segno < $4",
                quote_ident(&table)
            );
            let lower_i: i64 = lower as i64;
            let upper_i: i64 = segno_up_to as i64;
            let params: [&(dyn ToSql + Sync); 4] =
                [&tenant_id, &timeline_id, &lower_i, &upper_i];
            ysql_fpu_client
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

fn quote_ident(ident: &str) -> String {
    let escaped = ident.replace('"', "\"\"");
    format!("\"{escaped}\"")
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
async fn consumer_main(
    mut rx: mpsc::Receiver<PendingChunk>,
    ysql_fpu_client: Arc<YsqlClient>,
    table_name: String,
    tenant_id: String,
    timeline_id: String,
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

        let highest_lsn = batch
            .last()
            .map(|c| c.end_lsn)
            .expect("batch is non-empty");

        match flush_batch(&ysql_fpu_client, &table_name, &tenant_id, &timeline_id, &batch).await {
            Ok(()) => {
                // Populate the read-side cache from the batch BEFORE
                // publishing the durability advance. The cache is the
                // WAL sender's fast path; we only insert chunks here
                // (after a successful INSERT) so the cache never
                // serves bytes that aren't durable in
                // `yb_storage.wals`. See findings-ps-wal-replay-
                // restart-bug-2026-06-04 for the failure mode that
                // earlier pre-INSERT cache population caused.
                if let Some(c) = &cache {
                    let mut guard = c.lock().unwrap();
                    for pc in &batch {
                        guard.insert(
                            pc.segno,
                            pc.chunk_offset as u64,
                            pc.data.clone(),
                        );
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
    tenant_id: &str,
    timeline_id: &str,
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

    let mut sql = String::with_capacity(128 + n * 56);
    sql.push_str("INSERT INTO ");
    sql.push_str(&quote_ident(table_name));
    sql.push_str(
        " (tenant_id, timeline_id, segno, chunk_offset, \
          chunk_size, data, last_modified_us) VALUES ",
    );
    for i in 0..n {
        if i > 0 {
            sql.push_str(", ");
        }
        let b = i * 7;
        use std::fmt::Write as _;
        let _ = write!(
            sql,
            "(${},${},${},${},${},${},${})",
            b + 1,
            b + 2,
            b + 3,
            b + 4,
            b + 5,
            b + 6,
            b + 7,
        );
    }

    let segno_is: Vec<i64> = batch.iter().map(|p| p.segno as i64).collect();
    let offset_is: Vec<i64> = batch.iter().map(|p| p.chunk_offset).collect();
    let size_is: Vec<i32> = batch.iter().map(|p| p.chunk_size).collect();
    // `PendingChunk::data` is `Arc<Vec<u8>>` (shared with WalCache).
    // tokio_postgres' ToSql doesn't see through Arc, so deref each entry
    // to a `&[u8]` for the param vector.
    let datas: Vec<&[u8]> = batch.iter().map(|p| p.data.as_slice()).collect();

    let mut params: Vec<&(dyn ToSql + Sync)> = Vec::with_capacity(n * 7);
    for i in 0..n {
        params.push(&tenant_id);
        params.push(&timeline_id);
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

/// Lazy chunk-streaming reader over `yb_storage.wals`, replacing the old
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
    /// Kept for symmetry with the local-FS reader and for future
    /// segment-boundary assertions; not currently read.
    #[allow(dead_code)]
    wal_seg_size: u64,
    pos: u64,

    cache: Option<Arc<std::sync::Mutex<WalCache>>>,
    ysql_fpu_client: Arc<YsqlClient>,
    table: String,
    tenant_id: String,
    timeline_id: String,

    current: Option<CurrentChunk>,
    /// In-flight YB fetch. A `oneshot::Receiver` (not a raw `BoxFuture`)
    /// because `WalReader::open_segment` is typed to return
    /// `Pin<Box<dyn AsyncRead + Send + Sync>>` — and `tokio_postgres`
    /// query futures aren't `Sync`. The fetch runs on a spawned task and
    /// hands the result back through this channel.
    fetch: Option<tokio::sync::oneshot::Receiver<anyhow::Result<Option<CurrentChunk>>>>,
}

#[derive(Clone)]
struct CurrentChunk {
    chunk_offset: u64,
    end_offset: u64,
    data: Arc<Vec<u8>>,
}

impl YbSegmentReader {
    /// Build a reader for `segno` starting at byte offset `start_xlogoff`.
    /// The reader holds clones of the YB ysql_fpu_client + cache handle, so it's
    /// `Send + Sync` and can be passed up the WAL sender stack like the
    /// `tokio::fs::File` it replaces.
    pub fn new(
        yb_wal: Arc<YbWalConn>,
        ttid: &TenantTimelineId,
        segno: XLogSegNo,
        wal_seg_size: usize,
        start_xlogoff: usize,
    ) -> Self {
        Self {
            segno,
            wal_seg_size: wal_seg_size as u64,
            pos: start_xlogoff as u64,
            cache: yb_wal.cache.clone(),
            ysql_fpu_client: yb_wal.client.clone(),
            table: yb_wal.table.clone(),
            tenant_id: ttid.tenant_id.to_string(),
            timeline_id: ttid.timeline_id.to_string(),
            current: None,
            fetch: None,
        }
    }

    fn start_fetch(&mut self) {
        let ysql_fpu_client = self.ysql_fpu_client.clone();
        let table = self.table.clone();
        let tenant_id = self.tenant_id.clone();
        let timeline_id = self.timeline_id.clone();
        let segno = self.segno;
        let pos = self.pos;
        let (tx, rx) = tokio::sync::oneshot::channel();
        tokio::spawn(async move {
            let res =
                fetch_chunk_from_yb(&ysql_fpu_client, &table, &tenant_id, &timeline_id, segno, pos).await;
            let _ = tx.send(res);
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
                if me.pos >= c.chunk_offset && me.pos < c.end_offset {
                    let chunk_idx = (me.pos - c.chunk_offset) as usize;
                    let chunk_remaining = c.data.len().saturating_sub(chunk_idx);
                    let want = dst.remaining().min(chunk_remaining);
                    if want == 0 {
                        return Poll::Ready(Ok(()));
                    }
                    dst.put_slice(&c.data[chunk_idx..chunk_idx + want]);
                    me.pos += want as u64;
                    return Poll::Ready(Ok(()));
                }
                // Walked past the current chunk's range — drop it.
                me.current = None;
            }

            // 2. Try the in-memory cache. Fast O(log n), no I/O.
            if let Some(cache) = me.cache.clone() {
                let hit = cache.lock().unwrap().lookup(me.segno, me.pos);
                if let Some((chunk_offset, end_offset, data)) = hit {
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
                    return Poll::Ready(Err(std::io::Error::new(
                        std::io::ErrorKind::Other,
                        format!("YB WAL fetch failed for segno {} offset {}: {e}", me.segno, me.pos),
                    )));
                }
                Poll::Ready(Err(_recv_err)) => {
                    // Spawned fetch task dropped its sender without
                    // sending — shouldn't happen, but treat as IO error.
                    me.fetch = None;
                    return Poll::Ready(Err(std::io::Error::new(
                        std::io::ErrorKind::Other,
                        "YB WAL fetch task dropped without result",
                    )));
                }
            }
        }
    }
}

/// Fetch the chunk covering `(segno, byte_offset)` from
/// `yb_storage.wals`. By construction the writer produces at most one
/// chunk covering any given offset within a segment (writes never
/// overlap — see the module-level "Notes" on chunk_offset uniqueness),
/// so the range filter selects at most one row and we don't need an
/// ORDER BY / LIMIT tiebreaker.
async fn fetch_chunk_from_yb(
    ysql_fpu_client: &YsqlClient,
    table: &str,
    tenant_id: &str,
    timeline_id: &str,
    segno: u64,
    byte_offset: u64,
) -> anyhow::Result<Option<CurrentChunk>> {
    let sql = format!(
        "SELECT chunk_offset, chunk_size, data FROM {} \
         WHERE tenant_id = $1 AND timeline_id = $2 AND segno = $3 \
           AND chunk_offset <= $4 AND chunk_offset + chunk_size > $4 \
         LIMIT 1",
        quote_ident(table)
    );
    let segno_i: i64 = segno as i64;
    let pos_i: i64 = byte_offset as i64;
    let params: [&(dyn ToSql + Sync); 4] = [&tenant_id, &timeline_id, &segno_i, &pos_i];
    let row_opt = ysql_fpu_client
        .query_opt(&sql, &params)
        .await
        .context("YB WAL fetch_chunk_from_yb")?;
    Ok(row_opt.map(|row| {
        let chunk_offset: i64 = row.get("chunk_offset");
        let chunk_size: i32 = row.get("chunk_size");
        let data: Vec<u8> = row.get("data");
        CurrentChunk {
            chunk_offset: chunk_offset as u64,
            end_offset: (chunk_offset as u64) + (chunk_size as u64),
            data: Arc::new(data),
        }
    }))
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
    use utils::id::{TenantId, TenantTimelineId, TimelineId};
    // Tests build a `yb_tokio_postgres::Config` via `tokio_postgres::*`
    // type paths; the production code in this module doesn't need the
    // alias.
    use yb_tokio_postgres as tokio_postgres;

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
    async fn make_wals_table(ysql_fpu_client: &YsqlClient, table: &str) {
        let q = quote_ident(table);
        ysql_fpu_client
            .execute(&format!("DROP TABLE IF EXISTS {q}"), &[])
            .await
            .expect("drop scratch wals table");
        ysql_fpu_client
            .execute(
                &format!(
                    "CREATE TABLE {q} (\
                       tenant_id        TEXT    NOT NULL,\
                       timeline_id      TEXT    NOT NULL,\
                       segno            BIGINT  NOT NULL,\
                       chunk_offset     BIGINT  NOT NULL,\
                       chunk_size       INT     NOT NULL,\
                       data             BYTEA   NOT NULL,\
                       last_modified_us BIGINT  NOT NULL,\
                       PRIMARY KEY ((tenant_id, timeline_id) HASH, segno, chunk_offset)\
                     )"
                ),
                &[],
            )
            .await
            .expect("create scratch wals table");
    }

    async fn drop_wals_table(ysql_fpu_client: &YsqlClient, table: &str) {
        let q = quote_ident(table);
        let _ = ysql_fpu_client.execute(&format!("DROP TABLE IF EXISTS {q}"), &[]).await;
    }

    /// Build a fresh `YbWalStorage` with its own per-test scratch table
    /// and tempdir. `start_lsn` becomes the rehydrated `write_lsn` (the
    /// table is empty, so `load_write_lsn` falls back to `commit_lsn`).
    async fn make_storage(table: &str, start_lsn: Lsn) -> (YbWalStorage, Utf8TempDir) {
        let ysql_fpu_client = Arc::new(
            YsqlClient::connect_no_tls(pgcfg(), true)
                .await
                .expect("connect to local YB; set YB_TEST_HOST/PORT/USER/DB to override"),
        );
        make_wals_table(&ysql_fpu_client, table).await;

        let tenant = TenantId::generate();
        let timeline = TimelineId::generate();
        let ttid = TenantTimelineId::new(tenant, timeline);
        let mut state = TimelinePersistentState::empty();
        state.tenant_id = ttid.tenant_id;
        state.timeline_id = ttid.timeline_id;
        state.timeline_start_lsn = start_lsn;
        state.local_start_lsn = start_lsn;

        let tmp = camino_tempfile::tempdir().expect("tempdir");
        let yb_wal = Arc::new(YbWalConn {
            client: ysql_fpu_client,
            table: table.to_string(),
            cache: None,
        });
        let storage = YbWalStorage::new(&ttid, &state, yb_wal)
            .await
            .expect("YbWalStorage::new");
        (storage, tmp)
    }

    const SEG_SIZE: u64 = 16 * 1024 * 1024;

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

        drop_wals_table(&storage.ysql_fpu_client, &table).await;
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

        drop_wals_table(&storage.ysql_fpu_client, &table).await;
    }

    fn rand_suffix() -> String {
        let pid = std::process::id();
        let nanos = std::time::UNIX_EPOCH.elapsed().map(|d| d.as_nanos()).unwrap_or(0);
        format!("{pid}_{nanos}")
    }
}
