//! `PageStore` — YugabyteDB row store for **relation pages only**.
//!
//! This is yb-pageserver's replacement for the upstream layer-file
//! machinery, but **only for relation-page keys** — i.e. keys where
//! `Key::is_rel_block_key()` holds (`field1 == 0x00`, `field4 != 0`,
//! `field6 != 0xffffffff`). Non-page keys (SLRUs, metadata, dir
//! listings, aux files, replication origins, rel-size sentinels)
//! continue to flow through the existing layer-file path in
//! `public.blobs`.
//!
//! Schema:
//! ```sql
//! CREATE TABLE pageserver.pages (
//!     tenant_id   TEXT     NOT NULL,
//!     timeline_id TEXT     NOT NULL,
//!     spcnode     BIGINT   NOT NULL,
//!     dbnode      BIGINT   NOT NULL,
//!     relnode     BIGINT   NOT NULL,
//!     forknum     SMALLINT NOT NULL,
//!     blknum      BIGINT   NOT NULL,
//!     is_image    BOOLEAN  NOT NULL,
//!     lsn         BIGINT   NOT NULL,
//!     will_init   BOOLEAN  NOT NULL,
//!     payload     BYTEA    NOT NULL,
//!     PRIMARY KEY ((tenant_id, timeline_id, spcnode, dbnode, relnode, forknum) HASH,
//!                  blknum, lsn DESC, is_image)
//! );
//! ```
//!
//! Hash partition `(tenant, timeline, spcnode, dbnode, relnode, forknum)`
//! means each fork's whole block-set lives on one tablet. Per-block
//! reads are one tablet hash lookup; per-fork range scans (basebackup,
//! image-layer materialisation) are one tablet's range scan.
//!
//! `is_image` sits in the clustering key between `blknum` and `lsn` so
//! the "most recent image at or before target_lsn" lookup is a single
//! seek (`AND is_image=TRUE ORDER BY lsn DESC LIMIT 1`). Reads anchor
//! the chain on that image and return the deltas above it. Chain length
//! is no longer a write-time invariant — a background image materialiser
//! creates fresh images for hot keys to keep chains short. Reads remain
//! correct without it, just potentially slower.

use std::collections::HashMap;
use std::ops::Range;
use std::sync::Arc;

use anyhow::{Context, Result, bail};
use bytes::Bytes;
use futures::stream::{StreamExt, TryStreamExt};
use pageserver_api::key::{KEY_SIZE, Key};
use pageserver_api::models::RelSizeMigration;
use pageserver_api::reltag::SlruKind;

use crate::tenant::metadata::Lineage;
use remote_storage::{YbStoreConfig, YsqlClient};
// We talk to YugabyteDB through the YB smart-driver fork of
// tokio-postgres; alias it so the type names below (`tokio_postgres::Row`,
// `tokio_postgres::types::ToSql`, etc.) keep working without
// per-callsite churn. The Neon fork of tokio-postgres survives
// elsewhere in the workspace for the walproposer/walreceiver
// replication path.
use utils::id::{TenantId, TimelineId};
use utils::lsn::Lsn;
use yb_tokio_postgres as tokio_postgres;
use yb_tokio_postgres::types::ToSql;

/// One row in `pageserver.pages`. `key` is the 18-byte BYTEA encoding
/// of a `pageserver_api::key::Key` for a relation page; it must satisfy
/// `Key::is_rel_block_key()` or `put_pages` returns an error.
#[derive(Debug, Clone, PartialEq, Eq)]
pub struct PageRow {
    pub key: Bytes,
    pub lsn: Lsn,
    pub is_image: bool,
    pub will_init: bool,
    pub payload: Bytes,
}

/// One row in `pageserver.meta`. The key is the 18-byte BYTEA encoding
/// of any `Key` that isn't a relation-page or SLRU-block (those have
/// their own tables). Full-overwrite semantics: read = latest row at
/// LSN ≤ target.
#[derive(Debug, Clone, PartialEq, Eq)]
pub struct MetaRow {
    pub key: Bytes,
    pub lsn: Lsn,
    pub payload: Bytes,
}

/// One row in `pageserver.slru_pages`.
#[derive(Debug, Clone, PartialEq, Eq)]
pub struct SlruPageRow {
    pub kind: SlruKind,
    pub segno: u32,
    pub blknum: u32,
    pub lsn: Lsn,
    pub is_image: bool,
    pub will_init: bool,
    pub payload: Bytes,
}

fn slru_kind_to_i16(kind: SlruKind) -> i16 {
    match kind {
        SlruKind::Clog => 0,
        SlruKind::MultiXactMembers => 1,
        SlruKind::MultiXactOffsets => 2,
    }
}

fn slru_kind_from_i16(v: i16) -> Result<SlruKind> {
    Ok(match v {
        0 => SlruKind::Clog,
        1 => SlruKind::MultiXactMembers,
        2 => SlruKind::MultiXactOffsets,
        _ => bail!("unrecognized SLRU kind {v}"),
    })
}

#[derive(Debug, Clone, PartialEq, Eq)]
pub struct TimelineRow {
    pub tenant_id: TenantId,
    pub timeline_id: TimelineId,
    /// Highest LSN durably present in `pageserver.pages`/`slru_pages`/`meta`.
    /// Advances on every successful `PageWriter::flush`. Drives both
    /// safekeeper WAL truncation (SK keeps WAL ≥ this) and ingest
    /// resume on restart (walreceiver replays from this LSN forward).
    pub disk_consistent_lsn: Lsn,
    pub applied_gc_cutoff_lsn: Lsn,
    pub ancestor_timeline_id: Option<TimelineId>,
    pub ancestor_lsn: Option<Lsn>,
    pub current_generation: i32,
    pub pg_version: u32,

    // Fields that used to live in `index_part.json` and now ride on
    // the row directly. All optional / default-able so a freshly
    // created row only has to fill the load-bearing columns above.
    /// Last LSN at which initdb-tar-style import seeded this timeline.
    /// Re-derived on attach by Phase 5c, persisted here once we have
    /// it so subsequent restarts skip the synthesis.
    pub initdb_lsn: Option<Lsn>,
    /// The walredo "previous record LSN" pointer (`xlog.c`'s
    /// `XLogCtl->Insert.PrevBytePos` viewpoint). Re-derived on
    /// walreceiver resume, persisted so cold restarts can verify it.
    pub prev_record_lsn: Option<Lsn>,
    /// `Some(ts)` when the timeline is archived (compute denied);
    /// `None` while live. Surfaced via the HTTP archive endpoint.
    pub archived_at: Option<chrono::NaiveDateTime>,
    /// `Some(ts)` when the timeline is mid-delete; the deletion
    /// pipeline keys off this so a crash-restart can resume the
    /// teardown instead of leaving the timeline half-deleted.
    pub deleted_at: Option<chrono::NaiveDateTime>,
    /// Rel-size schema migration state (Legacy / Migrating /
    /// Migrated). `None` is treated as `Legacy` per the upstream
    /// convention. Read path branches on this; walingest writes here
    /// when migration progresses.
    pub rel_size_migration: Option<RelSizeMigration>,
    /// LSN at which the rel-size migration started; pairs with
    /// `rel_size_migration` for read-path correctness around the
    /// migration boundary.
    pub rel_size_migrated_at: Option<Lsn>,
    /// Ancestor-detach history. Kept as JSON for forward-compat;
    /// always `Default` in yb-mode today (detach_ancestor is
    /// unsupported) but the column is wired up for when the feature
    /// lands.
    pub lineage: Lineage,
}

pub const DEFAULT_SCHEMA: &str = "pageserver";

/// Result of `PageStore::advance_disk_consistent_lsn`. Distinguishes a
/// successful UPDATE from generation-fence failure, the latter being
/// fatal: a higher-generation pageserver has taken over and this one
/// must shut down (its in-flight YB INSERTs will start being rejected
/// by the same generation check, so it can't make forward progress).
#[derive(Debug, Clone, Copy, PartialEq, Eq)]
pub enum AdvanceDcLsnOutcome {
    Updated,
    StaleGeneration { live_generation: u32 },
}

/// Result of [`PageStore::update_timeline_state`]. The UPDATE's WHERE
/// clause enforces `current_generation = $caller_gen`, so a stale
/// pageserver — one whose generation has been bumped past since
/// attach — gets `StaleGeneration`. The caller logs the skip; the
/// next flush's `advance_disk_consistent_lsn` is what actually trips
/// the shutdown-for-stale-generation path.
#[derive(Debug, Clone, Copy, PartialEq, Eq)]
pub enum UpdateTimelineStateOutcome {
    /// Row was found at our generation; UPDATE landed.
    Updated,
    /// Either the row is missing (wiring bug — `insert_timeline_at_creation`
    /// is supposed to have run) or the live generation is no longer
    /// ours. In practice the latter dominates; we can't disambiguate
    /// without a follow-up SELECT, and the caller doesn't need to —
    /// either way the right move is "log and proceed".
    StaleGeneration,
}

/// Result of `PageStore::cas_bump_generation_on_attach`. Called once
/// per attach to push the controller-issued generation through to the
/// YB column before any flushes run, so the flush path's
/// `current_generation <= $our` invariant holds even on re-attach.
#[derive(Debug, Clone, Copy, PartialEq, Eq)]
pub enum CasBumpOutcome {
    /// Stored gen was strictly less than ours; we bumped it.
    Bumped,
    /// Stored gen already equals ours (idempotent re-attach).
    AlreadyAtOurGen,
    /// Stored gen is strictly greater than ours — a newer pageserver
    /// has taken over. Caller should refuse the attach + shut down.
    StaleGeneration { live_generation: u32 },
}

#[derive(Debug, Clone)]
pub struct PageStoreConfig {
    pub yb: YbStoreConfig,
    pub schema: String,
    /// Multiplier applied to the per-statement INSERT fan-out
    /// (`put_pages` / `put_slru_pages` / `put_meta`). Plumbed through
    /// from `PageServerConf::scale_factor`; tests default to 1.0.
    pub scale_factor: f64,
}

impl PageStoreConfig {
    pub fn new(yb: YbStoreConfig) -> Self {
        Self {
            yb,
            schema: DEFAULT_SCHEMA.to_string(),
            scale_factor: pageserver_api::config::DEFAULT_SCALE_FACTOR,
        }
    }

    pub fn with_scale_factor(mut self, scale_factor: f64) -> Self {
        self.scale_factor = scale_factor;
        self
    }
}

#[derive(Clone)]
pub struct PageStore {
    ysql_client: Arc<YsqlClient>,
    schema: Arc<str>,
    scale_factor: f64,
}

/// Decompose an 18-byte relation-page Key into the typed columns.
/// Returns `Err` if the bytes don't parse as `field1 = 0` (the
/// relation-page namespace).
fn key_columns(key_bytes: &[u8]) -> Result<(i64, i64, i64, i16, i64)> {
    if key_bytes.len() != KEY_SIZE {
        bail!(
            "PageStore: key bytes wrong length ({}, expected {})",
            key_bytes.len(),
            KEY_SIZE
        );
    }
    let key = Key::from_slice(key_bytes);
    if !key.is_rel_block_key() {
        bail!(
            "PageStore: key {key} is not a relation block (field1=0x{:02x}, field4={}, field6=0x{:x}); \
             non-page keys belong in the layer-file path, not pageserver.pages",
            key.field1,
            key.field4,
            key.field6
        );
    }
    Ok((
        key.field2 as i64,
        key.field3 as i64,
        key.field4 as i64,
        key.field5 as i16,
        key.field6 as i64,
    ))
}

/// Inverse: rebuild the 18-byte BYTEA Key from typed columns.
fn columns_to_key_bytes(sp: i64, db: i64, rel: i64, fork: i16, blk: i64) -> Bytes {
    let mut buf = [0u8; KEY_SIZE];
    // field1 = 0 (relation-page namespace)
    buf[0] = 0;
    buf[1..5].copy_from_slice(&(sp as u32).to_be_bytes());
    buf[5..9].copy_from_slice(&(db as u32).to_be_bytes());
    buf[9..13].copy_from_slice(&(rel as u32).to_be_bytes());
    buf[13] = fork as u8;
    buf[14..18].copy_from_slice(&(blk as u32).to_be_bytes());
    Bytes::copy_from_slice(&buf)
}

impl PageStore {
    /// Open a `PageStore` with a single YSQL connection. Equivalent to
    /// `connect_pooled(config, app, 1, set_fast_path_upsert)`.
    pub async fn connect(
        config: PageStoreConfig,
        application_name: &str,
        set_fast_path_upsert: bool,
    ) -> Result<Self> {
        Self::connect_pooled(config, application_name, 1, set_fast_path_upsert).await
    }

    /// Open a `PageStore` with `pool_size` independent YSQL connections.
    /// Use for the compute-facing read path where parallel prefetches
    /// from one Postgres backend (or many concurrent computes) need
    /// to overlap SQL execution. `pool_size = 1` matches the single-
    /// connection behaviour of `connect()`.
    ///
    /// `set_fast_path_upsert` enables `yb_enable_upsert_mode = true`
    /// on each connection — true for write-heavy pools (the internal
    /// store's `put_pages` / `put_slru_pages` / `put_meta` fast paths,
    /// hence the "fpu" naming), false for read-only pools (compute-
    /// facing reads).
    pub async fn connect_pooled(
        config: PageStoreConfig,
        application_name: &str,
        pool_size: usize,
        set_fast_path_upsert: bool,
    ) -> Result<Self> {
        let ysql_client = ysql_client::YsqlClient::from_yb_config_pooled(
            &config.yb.conn_config(),
            application_name,
            pool_size,
            set_fast_path_upsert,
        )
        .await
        .context("opening PageStore connection")?;
        Ok(Self {
            ysql_client: Arc::new(ysql_client),
            schema: Arc::from(config.schema),
            scale_factor: config.scale_factor,
        })
    }

    // ------------------- pages ---------------------------------------------

    pub async fn put_pages(
        &self,
        tenant: TenantId,
        timeline: TimelineId,
        rows: &[PageRow],
    ) -> Result<()> {
        // PostgreSQL caps a query at 65535 parameters; chunk to stay
        // well below it. 11 columns/row × 4000 rows = 44k params.
        // Chunks dispatch in parallel — tokio-postgres pipelines them
        // on the shared connection so YB can absorb them concurrently.
        const COLS: usize = 11;
        const ROWS_PER_INSERT: usize = 500;
        let parallelism = pageserver_api::config::scale_count(4, self.scale_factor);
        let tenant_s = tenant.to_string();
        let timeline_s = timeline.to_string();

        let mut futs: Vec<futures::future::BoxFuture<'_, Result<()>>> = Vec::new();
        for chunk in rows.chunks(ROWS_PER_INSERT) {
            if chunk.is_empty() {
                continue;
            }
            let mut sql = format!(
                "INSERT INTO \"{}\".pages \
                 (tenant_id, timeline_id, spcnode, dbnode, relnode, forknum, blknum, \
                  lsn, is_image, will_init, payload) VALUES ",
                self.schema
            );
            let mut params: Vec<Box<dyn ToSql + Sync + Send + 'static>> =
                Vec::with_capacity(chunk.len() * COLS);
            for (i, row) in chunk.iter().enumerate() {
                if i > 0 {
                    sql.push(',');
                }
                let base = i * COLS + 1;
                sql.push_str(&format!(
                    "(${}, ${}, ${}, ${}, ${}, ${}, ${}, ${}, ${}, ${}, ${})",
                    base,
                    base + 1,
                    base + 2,
                    base + 3,
                    base + 4,
                    base + 5,
                    base + 6,
                    base + 7,
                    base + 8,
                    base + 9,
                    base + 10,
                ));
                let (sp, db, rel, fork, blk) =
                    key_columns(&row.key).with_context(|| format!("put_pages row {i}: bad key"))?;
                params.push(Box::new(tenant_s.clone()));
                params.push(Box::new(timeline_s.clone()));
                params.push(Box::new(sp));
                params.push(Box::new(db));
                params.push(Box::new(rel));
                params.push(Box::new(fork));
                params.push(Box::new(blk));
                params.push(Box::new(row.lsn.0 as i64));
                params.push(Box::new(row.is_image));
                params.push(Box::new(row.will_init));
                params.push(Box::new(row.payload.to_vec()));
            }
            let chunk_rows = chunk.len();
            let ysql_client = Arc::clone(&self.ysql_client);
            futs.push(Box::pin(async move {
                let params_ref: Vec<&(dyn ToSql + Sync)> = params
                    .iter()
                    .map(|p| p.as_ref() as &(dyn ToSql + Sync))
                    .collect();
                ysql_client
                    .execute(&sql, &params_ref)
                    .await
                    .with_context(|| {
                        format!(
                            "put_pages tenant={tenant} timeline={timeline} chunk_rows={chunk_rows}"
                        )
                    })?;
                Ok(())
            }));
        }

        futures::stream::iter(futs)
            .buffer_unordered(parallelism)
            .try_collect::<Vec<_>>()
            .await?;
        Ok(())
    }

    /// Per-key chain read. `key` must be a valid relation-page Key.
    ///
    /// Fast path: a CTE seeks the latest `is_image=TRUE` row at LSN ≤
    /// `target_lsn` and returns it plus all delta rows above it (up to
    /// `target_lsn`). With `is_image` in the clustering key, this is a
    /// single tablet seek for the anchor and a small range scan for the
    /// deltas.
    ///
    /// Slow path: if no image exists at or before `target_lsn`, fall
    /// back to scanning the per-key chain LSN-descending and walking
    /// rows until a `will_init=true` row is found. This is what gets
    /// hit for blocks the background image materialiser hasn't reached
    /// yet, or freshly-written blocks whose first record was a
    /// will_init delta (page creation). Correct in all cases; only
    /// slower because the will_init scan isn't index-seek-friendly.
    pub async fn read_chain(
        &self,
        tenant: TenantId,
        timeline: TimelineId,
        key: &[u8],
        target_lsn: Lsn,
    ) -> Result<Vec<PageRow>> {
        let (sp, db, rel, fork, blk) = key_columns(key)?;
        let tenant_s = tenant.to_string();
        let timeline_s = timeline.to_string();
        let target = target_lsn.0 as i64;

        // Scalar-subquery anchor + COALESCE(..., 0) lower bound so
        // both ends of the lsn range are pushed into the DocDB scan
        // (rows scanned = rows returned). `lsn >= 0` covers the
        // no-image case.
        //
        // The Rust-side trim below still truncates at the first
        // `will_init=true` row walking LSN-desc — same as before, so
        // (a) the no-image case keeps the original full-chain
        // semantics and (b) an init-delta between the image and
        // target_lsn correctly shortens the chain.
        let sql = format!(
            "SELECT p.spcnode, p.dbnode, p.relnode, p.forknum, p.blknum, \
                    p.lsn, p.is_image, p.will_init, p.payload \
             FROM \"{schema}\".pages p \
             WHERE p.tenant_id=$1 AND p.timeline_id=$2 \
               AND p.spcnode=$3 AND p.dbnode=$4 AND p.relnode=$5 \
               AND p.forknum=$6 AND p.blknum=$7 \
               AND p.lsn <= $8 \
               AND p.lsn >= COALESCE(( \
                     SELECT a.lsn FROM \"{schema}\".pages a \
                     WHERE a.tenant_id=$1 AND a.timeline_id=$2 \
                       AND a.spcnode=$3 AND a.dbnode=$4 AND a.relnode=$5 \
                       AND a.forknum=$6 AND a.blknum=$7 \
                       AND a.is_image=TRUE AND a.lsn <= $8 \
                     ORDER BY a.lsn DESC LIMIT 1), 0) \
             ORDER BY p.lsn DESC, p.is_image DESC",
            schema = self.schema
        );
        let rows = self
            .ysql_client
            .query(
                &sql,
                &[&tenant_s, &timeline_s, &sp, &db, &rel, &fork, &blk, &target],
            )
            .await
            .context("read_chain")?;
        let mut chain = Vec::with_capacity(rows.len());
        let mut order_check: Vec<(Lsn, bool, bool)> = Vec::with_capacity(rows.len());
        for r in rows {
            let row = row_to_page(r)?;
            order_check.push((row.lsn, row.is_image, row.will_init));
            assert_chain_order_for_key(&order_check, "read_chain");
            let anchor = row.will_init;
            chain.push(row);
            if anchor {
                break;
            }
        }
        Ok(chain)
    }

    /// Range scan: return all chains for relation-page keys in `range`
    /// at LSN ≤ `target_lsn`.
    ///
    /// Returns a `HashMap` keyed by the 18-byte Key bytes. Each chain is
    /// LSN-descending and truncated at the first `will_init=true` row
    /// (image OR init-delta) — older rows are shadowed by that anchor.
    ///
    /// Implementation: detects the range shape and picks the most
    /// tablet-local SQL.
    ///
    /// - **Single-key range** (`range.end == range.start.next()`) →
    ///   delegates to `read_chain`. This is the getpage hot path —
    ///   every compute getpage RPC ends up here, so the dispatch
    ///   matters. No `GROUP BY` / `LEFT JOIN` overhead.
    /// - **One-fork range** (start and end share spcnode/dbnode/relnode
    ///   /forknum, differ only in blknum) → single-tablet hash-pinned
    ///   range scan on `blknum`. Used by basebackup, image-layer
    ///   creation, multi-block vectored gets.
    /// - **Cross-fork range** (start and end differ in some pre-blknum
    ///   field) → multi-column row comparison; scatters across hash
    ///   tablets. Rare in practice; we accept the cost.
    ///
    /// Non-page-key ranges (any Key with `field1 != 0` in the range)
    /// are rejected — callers must filter those out via
    /// `Key::is_rel_block_key` before calling this method.
    pub async fn scan_range(
        &self,
        tenant: TenantId,
        timeline: TimelineId,
        range: Range<Key>,
        target_lsn: Lsn,
    ) -> Result<HashMap<Bytes, Vec<PageRow>>> {
        if range.start.field1 != 0 || range.end.field1 > 1 {
            bail!(
                "PageStore::scan_range: range [{}, {}) extends outside the relation-page \
                 namespace (field1 must be 0); caller must filter non-page keys first",
                range.start,
                range.end,
            );
        }

        let one_fork = range.start.field2 == range.end.field2
            && range.start.field3 == range.end.field3
            && range.start.field4 == range.end.field4
            && range.start.field5 == range.end.field5;

        // Single-key fast path: every compute getpage RPC arrives as a
        // 1-key vectored read (`KeySpace::single(k..k.next())`). Detect
        // and dispatch to read_chain, which skips the multi-block
        // GROUP BY / LEFT JOIN entirely.
        if one_fork && range.end.field6 == range.start.field6.wrapping_add(1) {
            let mut key_bytes = vec![0u8; KEY_SIZE];
            range.start.write_to_byte_slice(&mut key_bytes);
            let chain = self
                .read_chain(tenant, timeline, &key_bytes, target_lsn)
                .await?;
            let mut out = HashMap::with_capacity(1);
            if !chain.is_empty() {
                out.insert(Bytes::from(key_bytes), chain);
            }
            return Ok(out);
        }

        let tenant_s = tenant.to_string();
        let timeline_s = timeline.to_string();
        let target = target_lsn.0 as i64;

        // Per-block image anchor via CTE: for each block in the range,
        // find the latest image at or below target_lsn, then return
        // only rows from that image onward. Blocks without an image get
        // their full chain (slow path) — app-side will still truncate
        // at the latest will_init delta.
        let rows = if one_fork {
            // Hash-pinned single-tablet scan.
            let sp = range.start.field2 as i64;
            let db = range.start.field3 as i64;
            let rel = range.start.field4 as i64;
            let fork = range.start.field5 as i16;
            let blk_lo = range.start.field6 as i64;
            let blk_hi = range.end.field6 as i64;
            // DISTINCT-driven LATERAL form: iterate the blocks that
            // actually exist (`SELECT DISTINCT blknum`, a Distinct
            // Index Scan ⇒ one row per block) and run the read_chain
            // pushdown per block via CROSS JOIN LATERAL. The
            // per-block InitPlan anchor becomes a pushable lower
            // bound so both ends of the lsn range are pushed into
            // the DocDB scan, same as the single-block case.
            let sql = format!(
                "WITH blocks AS ( \
                     SELECT DISTINCT blknum FROM \"{schema}\".pages \
                     WHERE tenant_id=$1 AND timeline_id=$2 \
                       AND spcnode=$3 AND dbnode=$4 AND relnode=$5 \
                       AND forknum=$6 \
                       AND blknum >= $7 AND blknum < $8 \
                       AND lsn <= $9 \
                 ) \
                 SELECT p.spcnode, p.dbnode, p.relnode, p.forknum, p.blknum, \
                        p.lsn, p.is_image, p.will_init, p.payload \
                 FROM blocks b \
                 CROSS JOIN LATERAL ( \
                     SELECT p.spcnode, p.dbnode, p.relnode, p.forknum, p.blknum, \
                            p.lsn, p.is_image, p.will_init, p.payload \
                     FROM \"{schema}\".pages p \
                     WHERE p.tenant_id=$1 AND p.timeline_id=$2 \
                       AND p.spcnode=$3 AND p.dbnode=$4 AND p.relnode=$5 \
                       AND p.forknum=$6 AND p.blknum = b.blknum \
                       AND p.lsn <= $9 \
                       AND p.lsn >= COALESCE(( \
                             SELECT a.lsn FROM \"{schema}\".pages a \
                             WHERE a.tenant_id=$1 AND a.timeline_id=$2 \
                               AND a.spcnode=$3 AND a.dbnode=$4 AND a.relnode=$5 \
                               AND a.forknum=$6 AND a.blknum = b.blknum \
                               AND a.is_image=TRUE AND a.lsn <= $9 \
                             ORDER BY a.lsn DESC LIMIT 1), 0) \
                     ORDER BY p.lsn DESC, p.is_image DESC \
                 ) p \
                 ORDER BY p.blknum, p.lsn DESC, p.is_image DESC",
                schema = self.schema
            );
            self.ysql_client
                .query(
                    &sql,
                    &[
                        &tenant_s,
                        &timeline_s,
                        &sp,
                        &db,
                        &rel,
                        &fork,
                        &blk_lo,
                        &blk_hi,
                        &target,
                    ],
                )
                .await
                .context("scan_range one-fork")?
        } else {
            // Cross-fork range. Same shape, with row-comparison
            // predicates. Joining the CTE on the full (spcnode, dbnode,
            // relnode, forknum, blknum) tuple keeps anchors paired with
            // the right block when a range crosses forks.
            let sp_lo = range.start.field2 as i64;
            let db_lo = range.start.field3 as i64;
            let rel_lo = range.start.field4 as i64;
            let fork_lo = range.start.field5 as i16;
            let blk_lo = range.start.field6 as i64;
            let sp_hi = range.end.field2 as i64;
            let db_hi = range.end.field3 as i64;
            let rel_hi = range.end.field4 as i64;
            let fork_hi = range.end.field5 as i16;
            let blk_hi = range.end.field6 as i64;
            // Cross-fork variant of the DISTINCT-driven LATERAL,
            // keyed on the full (spcnode, dbnode, relnode, forknum,
            // blknum) tuple so anchors stay paired with the right
            // block.
            let sql = format!(
                "WITH blocks AS ( \
                     SELECT DISTINCT spcnode, dbnode, relnode, forknum, blknum \
                     FROM \"{schema}\".pages \
                     WHERE tenant_id=$1 AND timeline_id=$2 \
                       AND (spcnode, dbnode, relnode, forknum, blknum) >= ($3, $4, $5, $6, $7) \
                       AND (spcnode, dbnode, relnode, forknum, blknum) <  ($8, $9, $10, $11, $12) \
                       AND lsn <= $13 \
                 ) \
                 SELECT p.spcnode, p.dbnode, p.relnode, p.forknum, p.blknum, \
                        p.lsn, p.is_image, p.will_init, p.payload \
                 FROM blocks b \
                 CROSS JOIN LATERAL ( \
                     SELECT p.spcnode, p.dbnode, p.relnode, p.forknum, p.blknum, \
                            p.lsn, p.is_image, p.will_init, p.payload \
                     FROM \"{schema}\".pages p \
                     WHERE p.tenant_id=$1 AND p.timeline_id=$2 \
                       AND p.spcnode = b.spcnode AND p.dbnode = b.dbnode \
                       AND p.relnode = b.relnode AND p.forknum = b.forknum \
                       AND p.blknum = b.blknum \
                       AND p.lsn <= $13 \
                       AND p.lsn >= COALESCE(( \
                             SELECT a.lsn FROM \"{schema}\".pages a \
                             WHERE a.tenant_id=$1 AND a.timeline_id=$2 \
                               AND a.spcnode = b.spcnode AND a.dbnode = b.dbnode \
                               AND a.relnode = b.relnode AND a.forknum = b.forknum \
                               AND a.blknum = b.blknum \
                               AND a.is_image=TRUE AND a.lsn <= $13 \
                             ORDER BY a.lsn DESC LIMIT 1), 0) \
                     ORDER BY p.lsn DESC, p.is_image DESC \
                 ) p \
                 ORDER BY p.spcnode, p.dbnode, p.relnode, p.forknum, p.blknum, p.lsn DESC, p.is_image DESC",
                schema = self.schema
            );
            self.ysql_client
                .query(
                    &sql,
                    &[
                        &tenant_s,
                        &timeline_s,
                        &sp_lo,
                        &db_lo,
                        &rel_lo,
                        &fork_lo,
                        &blk_lo,
                        &sp_hi,
                        &db_hi,
                        &rel_hi,
                        &fork_hi,
                        &blk_hi,
                        &target,
                    ],
                )
                .await
                .context("scan_range cross-fork")?
        };

        // Bucket rows by key. Each chain comes back LSN-descending (per
        // the SQL ORDER BY); truncate at the first will_init=true row
        // since older rows are shadowed by that anchor. For blocks with
        // an image, this kicks in at the image row (always will_init).
        // For blocks without an image, it finds the latest will_init
        // delta and trims older rows.
        let mut by_key: HashMap<Bytes, Vec<PageRow>> = HashMap::new();
        let mut order_check: HashMap<Bytes, Vec<(Lsn, bool, bool)>> = HashMap::new();
        for r in rows {
            let row = row_to_page(r)?;
            let key = row.key.clone();
            order_check.entry(key.clone()).or_default().push((
                row.lsn,
                row.is_image,
                row.will_init,
            ));
            assert_chain_order_for_key(order_check.get(&key).unwrap(), "scan_range");
            let chain = by_key.entry(key).or_default();
            if chain.last().is_some_and(|last| last.will_init) {
                continue;
            }
            chain.push(row);
        }
        Ok(by_key)
    }

    pub async fn delete_all_pages(&self, tenant: TenantId, timeline: TimelineId) -> Result<()> {
        let sql = format!(
            "DELETE FROM \"{}\".pages WHERE tenant_id = $1 AND timeline_id = $2",
            self.schema
        );
        self.ysql_client
            .execute(&sql, &[&tenant.to_string(), &timeline.to_string()])
            .await
            .context("delete_all_pages")?;
        Ok(())
    }

    // ------------------- slru_pages ---------------------------------------

    pub async fn put_slru_pages(
        &self,
        tenant: TenantId,
        timeline: TimelineId,
        rows: &[SlruPageRow],
    ) -> Result<()> {
        const COLS: usize = 9;
        const ROWS_PER_INSERT: usize = 500;
        let parallelism = pageserver_api::config::scale_count(2, self.scale_factor);
        let tenant_s = tenant.to_string();
        let timeline_s = timeline.to_string();

        let mut futs: Vec<futures::future::BoxFuture<'_, Result<()>>> = Vec::new();
        for chunk in rows.chunks(ROWS_PER_INSERT) {
            if chunk.is_empty() {
                continue;
            }
            let mut sql = format!(
                "INSERT INTO \"{}\".slru_pages \
                 (tenant_id, timeline_id, kind, segno, blknum, \
                  is_image, lsn, will_init, payload) VALUES ",
                self.schema
            );
            let mut params: Vec<Box<dyn ToSql + Sync + Send + 'static>> =
                Vec::with_capacity(chunk.len() * COLS);
            for (i, row) in chunk.iter().enumerate() {
                if i > 0 {
                    sql.push(',');
                }
                let base = i * COLS + 1;
                sql.push_str(&format!(
                    "(${}, ${}, ${}, ${}, ${}, ${}, ${}, ${}, ${})",
                    base,
                    base + 1,
                    base + 2,
                    base + 3,
                    base + 4,
                    base + 5,
                    base + 6,
                    base + 7,
                    base + 8,
                ));
                params.push(Box::new(tenant_s.clone()));
                params.push(Box::new(timeline_s.clone()));
                params.push(Box::new(slru_kind_to_i16(row.kind)));
                params.push(Box::new(row.segno as i64));
                params.push(Box::new(row.blknum as i64));
                params.push(Box::new(row.is_image));
                params.push(Box::new(row.lsn.0 as i64));
                params.push(Box::new(row.will_init));
                params.push(Box::new(row.payload.to_vec()));
            }
            let chunk_rows = chunk.len();
            let ysql_client = Arc::clone(&self.ysql_client);
            futs.push(Box::pin(async move {
                let params_ref: Vec<&(dyn ToSql + Sync)> =
                    params.iter().map(|p| p.as_ref() as &(dyn ToSql + Sync)).collect();
                ysql_client
                    .execute(&sql, &params_ref)
                    .await
                    .with_context(|| {
                        format!(
                            "put_slru_pages tenant={tenant} timeline={timeline} chunk_rows={chunk_rows}"
                        )
                    })?;
                Ok(())
            }));
        }

        futures::stream::iter(futs)
            .buffer_unordered(parallelism)
            .try_collect::<Vec<_>>()
            .await?;
        Ok(())
    }

    /// Per-block SLRU chain read — same image-anchored CTE shape as
    /// `read_chain`, just keyed by `(kind, segno, blknum)`.
    pub async fn read_slru_chain(
        &self,
        tenant: TenantId,
        timeline: TimelineId,
        kind: SlruKind,
        segno: u32,
        blknum: u32,
        target_lsn: Lsn,
    ) -> Result<Vec<SlruPageRow>> {
        let tenant_s = tenant.to_string();
        let timeline_s = timeline.to_string();
        let kind_i = slru_kind_to_i16(kind);
        let segno_i = segno as i64;
        let blk_i = blknum as i64;
        let target = target_lsn.0 as i64;

        // Scalar-subquery anchor + COALESCE(..., 0) lower bound so
        // both ends of the lsn range are pushed into the DocDB scan
        // (same shape as `pages::read_chain`).
        let sql = format!(
            "SELECT p.kind, p.segno, p.blknum, p.lsn, p.is_image, p.will_init, p.payload \
             FROM \"{schema}\".slru_pages p \
             WHERE p.tenant_id=$1 AND p.timeline_id=$2 AND p.kind=$3 \
               AND p.segno=$4 AND p.blknum=$5 \
               AND p.lsn <= $6 \
               AND p.lsn >= COALESCE(( \
                     SELECT a.lsn FROM \"{schema}\".slru_pages a \
                     WHERE a.tenant_id=$1 AND a.timeline_id=$2 AND a.kind=$3 \
                       AND a.segno=$4 AND a.blknum=$5 \
                       AND a.is_image=TRUE AND a.lsn <= $6 \
                     ORDER BY a.lsn DESC LIMIT 1), 0) \
             ORDER BY p.lsn DESC, p.is_image DESC",
            schema = self.schema
        );
        let rows = self
            .ysql_client
            .query(
                &sql,
                &[&tenant_s, &timeline_s, &kind_i, &segno_i, &blk_i, &target],
            )
            .await
            .context("read_slru_chain")?;
        let mut chain = Vec::with_capacity(rows.len());
        let mut order_check: Vec<(Lsn, bool, bool)> = Vec::with_capacity(rows.len());
        for r in rows {
            let row = row_to_slru_page(r)?;
            order_check.push((row.lsn, row.is_image, row.will_init));
            assert_chain_order_for_key(&order_check, "read_slru_chain");
            let anchor = row.will_init;
            chain.push(row);
            if anchor {
                break;
            }
        }
        Ok(chain)
    }

    /// Range scan over SLRU blocks in `range`. `range` must stay inside
    /// one `(kind, segno)` segment — that's how SLRU vectored reads are
    /// shaped today. Returns one entry per blknum, each chain
    /// LSN-descending and truncated at the latest will_init row.
    pub async fn scan_slru_range(
        &self,
        tenant: TenantId,
        timeline: TimelineId,
        kind: SlruKind,
        segno: u32,
        blknum_range: Range<u32>,
        target_lsn: Lsn,
    ) -> Result<HashMap<u32, Vec<SlruPageRow>>> {
        // Single-block fast path: delegate to read_slru_chain to skip
        // the multi-block JOIN.
        if blknum_range.end == blknum_range.start.saturating_add(1) {
            let chain = self
                .read_slru_chain(
                    tenant,
                    timeline,
                    kind,
                    segno,
                    blknum_range.start,
                    target_lsn,
                )
                .await?;
            let mut out = HashMap::with_capacity(1);
            if !chain.is_empty() {
                out.insert(blknum_range.start, chain);
            }
            return Ok(out);
        }

        let tenant_s = tenant.to_string();
        let timeline_s = timeline.to_string();
        let kind_i = slru_kind_to_i16(kind);
        let segno_i = segno as i64;
        let blk_lo = blknum_range.start as i64;
        let blk_hi = blknum_range.end as i64;
        let target = target_lsn.0 as i64;

        // DISTINCT-driven LATERAL so both ends of the lsn range are
        // pushed into the DocDB scan per block (same shape as
        // `pages::scan_range`).
        let sql = format!(
            "WITH blocks AS ( \
                 SELECT DISTINCT blknum FROM \"{schema}\".slru_pages \
                 WHERE tenant_id=$1 AND timeline_id=$2 AND kind=$3 AND segno=$4 \
                   AND blknum >= $5 AND blknum < $6 \
                   AND lsn <= $7 \
             ) \
             SELECT p.kind, p.segno, p.blknum, p.lsn, p.is_image, p.will_init, p.payload \
             FROM blocks b \
             CROSS JOIN LATERAL ( \
                 SELECT p.kind, p.segno, p.blknum, p.lsn, p.is_image, p.will_init, p.payload \
                 FROM \"{schema}\".slru_pages p \
                 WHERE p.tenant_id=$1 AND p.timeline_id=$2 AND p.kind=$3 AND p.segno=$4 \
                   AND p.blknum = b.blknum AND p.lsn <= $7 \
                   AND p.lsn >= COALESCE(( \
                         SELECT a.lsn FROM \"{schema}\".slru_pages a \
                         WHERE a.tenant_id=$1 AND a.timeline_id=$2 AND a.kind=$3 AND a.segno=$4 \
                           AND a.blknum = b.blknum AND a.is_image=TRUE AND a.lsn <= $7 \
                         ORDER BY a.lsn DESC LIMIT 1), 0) \
                 ORDER BY p.lsn DESC \
             ) p \
             ORDER BY p.blknum, p.lsn DESC",
            schema = self.schema
        );
        let rows = self
            .ysql_client
            .query(
                &sql,
                &[
                    &tenant_s,
                    &timeline_s,
                    &kind_i,
                    &segno_i,
                    &blk_lo,
                    &blk_hi,
                    &target,
                ],
            )
            .await
            .context("scan_slru_range")?;

        let mut by_block: HashMap<u32, Vec<SlruPageRow>> = HashMap::new();
        let mut order_check: HashMap<u32, Vec<(Lsn, bool, bool)>> = HashMap::new();
        for r in rows {
            let row = row_to_slru_page(r)?;
            order_check
                .entry(row.blknum)
                .or_default()
                .push((row.lsn, row.is_image, row.will_init));
            assert_chain_order_for_key(order_check.get(&row.blknum).unwrap(), "scan_slru_range");
            let entry = by_block.entry(row.blknum).or_default();
            if entry.last().is_some_and(|last| last.will_init) {
                continue;
            }
            entry.push(row);
        }
        Ok(by_block)
    }

    // ------------------- meta ---------------------------------------------

    pub async fn put_meta(
        &self,
        tenant: TenantId,
        timeline: TimelineId,
        rows: &[MetaRow],
    ) -> Result<()> {
        const COLS: usize = 5;
        const ROWS_PER_INSERT: usize = 2000;
        let parallelism = pageserver_api::config::scale_count(3, self.scale_factor);
        let tenant_s = tenant.to_string();
        let timeline_s = timeline.to_string();

        let mut futs: Vec<futures::future::BoxFuture<'_, Result<()>>> = Vec::new();
        for chunk in rows.chunks(ROWS_PER_INSERT) {
            if chunk.is_empty() {
                continue;
            }
            let mut sql = format!(
                "INSERT INTO \"{}\".meta (tenant_id, timeline_id, key, lsn, payload) VALUES ",
                self.schema
            );
            let mut params: Vec<Box<dyn ToSql + Sync + Send + 'static>> =
                Vec::with_capacity(chunk.len() * COLS);
            for (i, row) in chunk.iter().enumerate() {
                if i > 0 {
                    sql.push(',');
                }
                let base = i * COLS + 1;
                sql.push_str(&format!(
                    "(${}, ${}, ${}, ${}, ${})",
                    base,
                    base + 1,
                    base + 2,
                    base + 3,
                    base + 4,
                ));
                if row.key.len() != KEY_SIZE {
                    bail!(
                        "put_meta row {i}: key bytes wrong length ({})",
                        row.key.len()
                    );
                }
                params.push(Box::new(tenant_s.clone()));
                params.push(Box::new(timeline_s.clone()));
                params.push(Box::new(row.key.to_vec()));
                params.push(Box::new(row.lsn.0 as i64));
                params.push(Box::new(row.payload.to_vec()));
            }
            let chunk_rows = chunk.len();
            let ysql_client = Arc::clone(&self.ysql_client);
            futs.push(Box::pin(async move {
                let params_ref: Vec<&(dyn ToSql + Sync)> = params
                    .iter()
                    .map(|p| p.as_ref() as &(dyn ToSql + Sync))
                    .collect();
                ysql_client
                    .execute(&sql, &params_ref)
                    .await
                    .with_context(|| {
                        format!(
                            "put_meta tenant={tenant} timeline={timeline} chunk_rows={chunk_rows}"
                        )
                    })?;
                Ok(())
            }));
        }

        futures::stream::iter(futs)
            .buffer_unordered(parallelism)
            .try_collect::<Vec<_>>()
            .await?;
        Ok(())
    }

    /// Read the latest payload for `key` at LSN ≤ `target_lsn`.
    /// Returns `None` if no row exists at or below the target.
    pub async fn read_meta(
        &self,
        tenant: TenantId,
        timeline: TimelineId,
        key: &[u8],
        target_lsn: Lsn,
    ) -> Result<Option<MetaRow>> {
        if key.len() != KEY_SIZE {
            bail!("read_meta: key bytes wrong length ({})", key.len());
        }
        let sql = format!(
            "SELECT key, lsn, payload FROM \"{}\".meta \
             WHERE tenant_id=$1 AND timeline_id=$2 AND key=$3 AND lsn <= $4 \
             ORDER BY lsn DESC LIMIT 1",
            self.schema
        );
        let row_opt = self
            .ysql_client
            .query_opt(
                &sql,
                &[
                    &tenant.to_string(),
                    &timeline.to_string(),
                    &key,
                    &(target_lsn.0 as i64),
                ],
            )
            .await
            .context("read_meta")?;
        row_opt.map(row_to_meta).transpose()
    }

    /// Range scan over meta keys. Returns the latest version per key
    /// in `[range.start, range.end)` at LSN ≤ `target_lsn`. Uses a
    /// LATERAL LIMIT 1 anchor seek per distinct key.
    pub async fn scan_meta_range(
        &self,
        tenant: TenantId,
        timeline: TimelineId,
        range: Range<Key>,
        target_lsn: Lsn,
    ) -> Result<HashMap<Bytes, MetaRow>> {
        let tenant_s = tenant.to_string();
        let timeline_s = timeline.to_string();
        let target = target_lsn.0 as i64;

        let mut start_bytes = vec![0u8; KEY_SIZE];
        range.start.write_to_byte_slice(&mut start_bytes);
        let mut end_bytes = vec![0u8; KEY_SIZE];
        range.end.write_to_byte_slice(&mut end_bytes);

        // Single-key fast path: no GROUP BY / LATERAL — just LIMIT 1.
        if range.end == range.start.next() {
            let row = self
                .read_meta(tenant, timeline, &start_bytes, target_lsn)
                .await?;
            let mut out = HashMap::with_capacity(1);
            if let Some(r) = row {
                out.insert(r.key.clone(), r);
            }
            return Ok(out);
        }

        let sql = format!(
            "WITH keys AS ( \
                 SELECT DISTINCT key FROM \"{schema}\".meta \
                 WHERE tenant_id=$1 AND timeline_id=$2 \
                   AND key >= $3 AND key < $4 AND lsn <= $5 \
             ) \
             SELECT k.key, latest.lsn, latest.payload \
             FROM keys k \
             CROSS JOIN LATERAL ( \
                 SELECT lsn, payload FROM \"{schema}\".meta \
                 WHERE tenant_id=$1 AND timeline_id=$2 \
                   AND key = k.key AND lsn <= $5 \
                 ORDER BY lsn DESC LIMIT 1 \
             ) latest",
            schema = self.schema
        );
        let rows = self
            .ysql_client
            .query(
                &sql,
                &[&tenant_s, &timeline_s, &start_bytes, &end_bytes, &target],
            )
            .await
            .context("scan_meta_range")?;
        let mut out = HashMap::with_capacity(rows.len());
        for r in rows {
            let row = row_to_meta(r)?;
            out.insert(row.key.clone(), row);
        }
        Ok(out)
    }

    pub async fn delete_all_meta(&self, tenant: TenantId, timeline: TimelineId) -> Result<()> {
        let sql = format!(
            "DELETE FROM \"{}\".meta WHERE tenant_id = $1 AND timeline_id = $2",
            self.schema
        );
        self.ysql_client
            .execute(&sql, &[&tenant.to_string(), &timeline.to_string()])
            .await
            .context("delete_all_meta")?;
        Ok(())
    }

    pub async fn delete_all_slru_pages(
        &self,
        tenant: TenantId,
        timeline: TimelineId,
    ) -> Result<()> {
        let sql = format!(
            "DELETE FROM \"{}\".slru_pages WHERE tenant_id = $1 AND timeline_id = $2",
            self.schema
        );
        self.ysql_client
            .execute(&sql, &[&tenant.to_string(), &timeline.to_string()])
            .await
            .context("delete_all_slru_pages")?;
        Ok(())
    }

    /// Drop every row for `(tenant, timeline)` across all four
    /// pageserver tables (`pages`, `slru_pages`, `meta`,
    /// `timeline_state`). Used by the timeline-delete flow to
    /// reclaim space and erase persisted state in one go.
    ///
    /// Deletes are dispatched concurrently; YB commits each one
    /// independently. If any DELETE fails the whole call returns
    /// `Err` and the caller may retry — partial deletion is
    /// idempotent so a re-run safely catches up.
    pub async fn delete_all_for_timeline(
        &self,
        tenant: TenantId,
        timeline: TimelineId,
    ) -> Result<()> {
        let (a, b, c, d) = tokio::join!(
            self.delete_all_pages(tenant, timeline),
            self.delete_all_slru_pages(tenant, timeline),
            self.delete_all_meta(tenant, timeline),
            self.delete_timeline(tenant, timeline),
        );
        a?;
        b?;
        c?;
        d?;
        Ok(())
    }

    // ------------------- timelines ----------------------------------------

    /// One-shot INSERT used at timeline creation by Phase 5b's
    /// `Timeline::write_timeline_row_to_yb`. Tries to create the row;
    /// if a row at the same `(tenant, timeline)` already exists this
    /// is a no-op (the existing state is the source of truth — a
    /// just-created timeline raced with an attach-time replay or
    /// similar; either way we shouldn't clobber the live row).
    ///
    /// **Not for incremental updates.** Per-flush UPDATEs of
    /// `disk_consistent_lsn` go through [`Self::advance_disk_consistent_lsn`];
    /// per-`UploadMetadata` UPDATEs from RTC go through
    /// [`Self::update_timeline_state`]. Both of those are gen-fenced;
    /// this one isn't, because at creation the row by definition
    /// doesn't exist yet (or — race — exists in a state we shouldn't
    /// touch).
    /// Load the JSONB-serialised `TenantConfig` for a tenant from
    /// `pageserver.tenant_config`. storage_controller is the sole
    /// writer; the pageserver re-reads on every PUT location_config
    /// notification and at startup.
    ///
    /// Returns `Ok(None)` if no row exists yet (storage_controller
    /// hasn't written one, e.g. a freshly-created tenant). Callers
    /// should fall back to the default `TenantConfig` in that case.
    pub async fn load_tenant_config(
        &self,
        tenant: TenantId,
    ) -> Result<Option<pageserver_api::models::TenantConfig>> {
        let sql = format!(
            "SELECT config FROM \"{}\".tenant_config WHERE tenant_id = $1",
            self.schema
        );
        let row_opt = self
            .ysql_client
            .query_opt(&sql, &[&tenant.to_string()])
            .await
            .context("load_tenant_config")?;
        let Some(row) = row_opt else { return Ok(None) };
        let json: serde_json::Value = row.get(0);
        let cfg: pageserver_api::models::TenantConfig =
            serde_json::from_value(json).context("decode TenantConfig JSON")?;
        Ok(Some(cfg))
    }

    /// Upsert the JSONB-serialised `TenantConfig` for a tenant.
    /// Only storage_controller should call this on the write path
    /// (tenant create + config update). Exposed on `PageStore` so the
    /// storage_controller integration test harness can use it
    /// directly without going through Diesel.
    pub async fn save_tenant_config(
        &self,
        tenant: TenantId,
        config: &pageserver_api::models::TenantConfig,
    ) -> Result<()> {
        let sql = format!(
            "INSERT INTO \"{}\".tenant_config (tenant_id, config) \
             VALUES ($1, $2::jsonb) \
             ON CONFLICT (tenant_id) DO UPDATE \
               SET config = EXCLUDED.config, updated_at = NOW()",
            self.schema
        );
        let json = serde_json::to_string(config).context("encode TenantConfig JSON")?;
        self.ysql_client
            .execute(&sql, &[&tenant.to_string(), &json])
            .await
            .context("save_tenant_config")?;
        Ok(())
    }

    pub async fn insert_timeline_at_creation(&self, row: &TimelineRow) -> Result<()> {
        let sql = format!(
            "INSERT INTO \"{}\".timeline_state \
             (tenant_id, timeline_id, disk_consistent_lsn, \
              applied_gc_cutoff_lsn, ancestor_timeline_id, ancestor_lsn, \
              current_generation, pg_version, initdb_lsn, prev_record_lsn, \
              archived_at, deleted_at, rel_size_migration, \
              rel_size_migrated_at, lineage) \
             VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9, $10, $11, $12, $13, $14, $15) \
             ON CONFLICT (tenant_id, timeline_id) DO NOTHING",
            self.schema
        );
        let (rel_size_migration_text, lineage_json) = serialize_row_aux(row)?;
        self.ysql_client
            .execute(
                &sql,
                &[
                    &row.tenant_id.to_string(),
                    &row.timeline_id.to_string(),
                    &(row.disk_consistent_lsn.0 as i64),
                    &(row.applied_gc_cutoff_lsn.0 as i64),
                    &row.ancestor_timeline_id.map(|id| id.to_string()),
                    &row.ancestor_lsn.map(|l| l.0 as i64),
                    &row.current_generation,
                    &(row.pg_version as i32),
                    &row.initdb_lsn.map(|l| l.0 as i64),
                    &row.prev_record_lsn.map(|l| l.0 as i64),
                    &row.archived_at,
                    &row.deleted_at,
                    &rel_size_migration_text,
                    &row.rel_size_migrated_at.map(|l| l.0 as i64),
                    &lineage_json,
                ],
            )
            .await
            .context("insert_timeline_at_creation")?;
        Ok(())
    }

    /// Gen-fenced UPDATE of the index_part.json-derived fields on the
    /// row. Called from RTC's `perform_upload_task`
    /// (`UploadOp::UploadMetadata`) and from
    /// `persist_index_part_with_deleted_flag`.
    ///
    /// The WHERE clause requires `current_generation = $caller_gen`:
    /// stale pageservers — those whose generation no longer matches
    /// the live row — get 0 rows updated, returning
    /// [`UpdateTimelineStateOutcome::StaleGeneration`]. Caller should
    /// log and proceed (matching the pattern in
    /// [`Self::advance_disk_consistent_lsn`]); the eventual
    /// `task_mgr::shutdown_for_stale_generation` path on the next
    /// flush takes care of shutting the stale process down.
    ///
    /// Does not touch `tenant_id`, `timeline_id`, `current_generation`
    /// (owned by `cas_bump_generation_on_attach`), or `pg_version`
    /// (immutable per timeline). Other fields are full-overwrite from
    /// the caller's row.
    pub async fn update_timeline_state(
        &self,
        row: &TimelineRow,
        generation: utils::generation::Generation,
    ) -> Result<UpdateTimelineStateOutcome> {
        let gen_i32: i32 = generation.into().map(|v: u32| v as i32).unwrap_or(0);
        let sql = format!(
            "UPDATE \"{}\".timeline_state SET \
                disk_consistent_lsn   = $3, \
                applied_gc_cutoff_lsn = $4, \
                ancestor_timeline_id  = $5, \
                ancestor_lsn          = $6, \
                initdb_lsn            = $7, \
                prev_record_lsn       = $8, \
                archived_at           = $9, \
                deleted_at            = $10, \
                rel_size_migration    = $11, \
                rel_size_migrated_at  = $12, \
                lineage               = $13 \
             WHERE tenant_id          = $1 \
               AND timeline_id        = $2 \
               AND current_generation = $14",
            self.schema
        );
        let (rel_size_migration_text, lineage_json) = serialize_row_aux(row)?;
        let n = self
            .ysql_client
            .execute(
                &sql,
                &[
                    &row.tenant_id.to_string(),
                    &row.timeline_id.to_string(),
                    &(row.disk_consistent_lsn.0 as i64),
                    &(row.applied_gc_cutoff_lsn.0 as i64),
                    &row.ancestor_timeline_id.map(|id| id.to_string()),
                    &row.ancestor_lsn.map(|l| l.0 as i64),
                    &row.initdb_lsn.map(|l| l.0 as i64),
                    &row.prev_record_lsn.map(|l| l.0 as i64),
                    &row.archived_at,
                    &row.deleted_at,
                    &rel_size_migration_text,
                    &row.rel_size_migrated_at.map(|l| l.0 as i64),
                    &lineage_json,
                    &gen_i32,
                ],
            )
            .await
            .context("update_timeline_state")?;
        if n == 0 {
            Ok(UpdateTimelineStateOutcome::StaleGeneration)
        } else {
            Ok(UpdateTimelineStateOutcome::Updated)
        }
    }

    pub async fn get_timeline(
        &self,
        tenant: TenantId,
        timeline: TimelineId,
    ) -> Result<Option<TimelineRow>> {
        let sql = format!(
            "SELECT {} FROM \"{}\".timeline_state \
             WHERE tenant_id = $1 AND timeline_id = $2",
            TIMELINE_ROW_COLUMNS, self.schema
        );
        let row = self
            .ysql_client
            .query_opt(&sql, &[&tenant.to_string(), &timeline.to_string()])
            .await
            .context("get_timeline")?;
        row.map(row_to_timeline).transpose()
    }

    pub async fn list_timelines(&self, tenant: TenantId) -> Result<Vec<TimelineRow>> {
        let sql = format!(
            "SELECT {} FROM \"{}\".timeline_state \
             WHERE tenant_id = $1",
            TIMELINE_ROW_COLUMNS, self.schema
        );
        let rows = self
            .ysql_client
            .query(&sql, &[&tenant.to_string()])
            .await
            .context("list_timelines")?;
        rows.into_iter().map(row_to_timeline).collect()
    }

    pub async fn delete_timeline(&self, tenant: TenantId, timeline: TimelineId) -> Result<()> {
        let sql = format!(
            "DELETE FROM \"{}\".timeline_state WHERE tenant_id = $1 AND timeline_id = $2",
            self.schema
        );
        self.ysql_client
            .execute(&sql, &[&tenant.to_string(), &timeline.to_string()])
            .await
            .context("delete_timeline")?;
        Ok(())
    }

    /// Column-scoped UPDATE of `disk_consistent_lsn`. Caller guards
    /// monotonicity in memory — only call when the new value is
    /// strictly higher than the last value sent; no server-side
    /// monotonicity check.
    ///
    /// The base row is guaranteed to exist by Phase 5b
    /// (`upsert_timeline` at timeline creation), so a plain UPDATE
    /// is correct and avoids the INSERT branch's sentinel-default
    /// concerns. Returns an error if zero rows matched — that
    /// signals "flush ran before timeline_create wrote the row",
    /// which is a wiring bug worth surfacing.
    ///
    /// Scoped to one column → safe to run concurrently with
    /// `bump_generation` / `advance_gc_cutoff_lsn` /
    /// `upsert_timeline`. Each touches disjoint columns.
    ///
    /// Eager attach-time bump of `current_generation`. Called once
    /// per attach, before any flush. Pushes the controller-issued
    /// generation through to the YB column so the flush path's
    /// `current_generation <= $our` predicate matches on the first
    /// try (without this, the flush self-heals on its own — see
    /// [`Self::advance_disk_consistent_lsn`] — but a stale PS could
    /// briefly write redundant rows in the gap between attach and
    /// first flush).
    ///
    /// `WHERE current_generation < $3` is the CAS predicate. If 0
    /// rows updated, probe the stored value to disambiguate:
    /// - stored `== ours` → idempotent re-attach, return
    ///   [`CasBumpOutcome::AlreadyAtOurGen`].
    /// - stored `> ours` → another pageserver bumped past us
    ///   already, return [`CasBumpOutcome::StaleGeneration`].
    /// - row missing → bail (the row is supposed to be created at
    ///   timeline creation; arriving here without it is a wiring bug).
    pub async fn cas_bump_generation_on_attach(
        &self,
        tenant: TenantId,
        timeline: TimelineId,
        generation: utils::generation::Generation,
    ) -> Result<CasBumpOutcome> {
        let gen_i32: i32 = generation.into().map(|v: u32| v as i32).unwrap_or(0);
        let sql = format!(
            "UPDATE \"{}\".timeline_state \
             SET current_generation = $3 \
             WHERE tenant_id = $1 AND timeline_id = $2 \
               AND current_generation < $3",
            self.schema
        );
        let n = self
            .ysql_client
            .execute(
                &sql,
                &[&tenant.to_string(), &timeline.to_string(), &gen_i32],
            )
            .await
            .context("cas_bump_generation_on_attach")?;
        if n == 1 {
            return Ok(CasBumpOutcome::Bumped);
        }
        // n == 0 — probe.
        let probe_sql = format!(
            "SELECT current_generation FROM \"{}\".timeline_state \
             WHERE tenant_id = $1 AND timeline_id = $2",
            self.schema
        );
        let row = self
            .ysql_client
            .query_opt(&probe_sql, &[&tenant.to_string(), &timeline.to_string()])
            .await
            .context("cas_bump_generation_on_attach probe")?;
        match row {
            None => anyhow::bail!(
                "no timeline row for {tenant}/{timeline} — \
                 cas_bump_generation_on_attach ran before timeline_create wrote the row"
            ),
            Some(r) => {
                let stored: i32 = r.get(0);
                let stored_u32 = stored as u32;
                if stored_u32 == gen_i32 as u32 {
                    Ok(CasBumpOutcome::AlreadyAtOurGen)
                } else {
                    Ok(CasBumpOutcome::StaleGeneration {
                        live_generation: stored_u32,
                    })
                }
            }
        }
    }

    /// **Generation-fenced**: the UPDATE's WHERE clause checks
    /// `current_generation = $4`. The invariant `YB.current_generation
    /// == our_gen` is established once at attach time by
    /// [`Self::cas_bump_generation_on_attach`] (called from
    /// `Timeline::bump_generation_on_attach`) and held for the
    /// lifetime of the attached PS: only attach paths write the
    /// column, and they only move it forward. So a stale PS's flush
    /// finds `YB.current_generation > our_gen` → zero rows → probe
    /// → [`AdvanceDcLsnOutcome::StaleGeneration`] → fatal shutdown.
    ///
    /// History: an earlier draft of this predicate paired with
    /// nothing in the attach path; a controller re-attach bumped the
    /// PS's gen but the YB column lagged, and the strict `=` check
    /// mis-fired as "stale" on every restart. We considered making
    /// the predicate self-healing (`<=` + SET current_generation),
    /// but the eager attach-time CAS is sufficient and the strict
    /// `=` here is a sharper fence (it fails the moment a newer PS
    /// bumps past us, no race window where a stale PS could still
    /// write redundant rows).
    pub async fn advance_disk_consistent_lsn(
        &self,
        tenant: TenantId,
        timeline: TimelineId,
        generation: utils::generation::Generation,
        new_dc_lsn: Lsn,
        new_prev_record_lsn: Option<Lsn>,
    ) -> Result<AdvanceDcLsnOutcome> {
        // Persist `prev_record_lsn` alongside `disk_consistent_lsn`
        // so a restart can hand the safekeeper the right `xl_prev`
        // for the record at `disk_consistent_lsn`. `COALESCE($5,
        // prev_record_lsn)` keeps the existing column value when
        // the caller didn't resolve a fresh pairing (race —
        // pages flushed before their record's `finish_write` ran;
        // see `PageWriter::take_prev_at_or_below`). Without
        // COALESCE we'd overwrite a previously-good value with
        // NULL on every throttled flush window.
        let sql = format!(
            "UPDATE \"{}\".timeline_state \
             SET disk_consistent_lsn = $3, \
                 prev_record_lsn      = COALESCE($5, prev_record_lsn) \
             WHERE tenant_id = $1 AND timeline_id = $2 \
               AND current_generation = $4",
            self.schema
        );
        let gen_i32: i32 = generation.into().map(|v: u32| v as i32).unwrap_or(0);
        let prev_i64: Option<i64> = new_prev_record_lsn.map(|l| l.0 as i64);
        let n = self
            .ysql_client
            .execute(
                &sql,
                &[
                    &tenant.to_string(),
                    &timeline.to_string(),
                    &(new_dc_lsn.0 as i64),
                    &gen_i32,
                    &prev_i64,
                ],
            )
            .await
            .context("advance_disk_consistent_lsn")?;
        if n == 0 {
            // Distinguish "row missing entirely" (init bug) from
            // "generation moved on" (we got fenced). One extra single-row
            // SELECT to disambiguate. Only runs on the unhappy path.
            let probe_sql = format!(
                "SELECT current_generation FROM \"{}\".timeline_state \
                 WHERE tenant_id = $1 AND timeline_id = $2",
                self.schema
            );
            let row = self
                .ysql_client
                .query_opt(&probe_sql, &[&tenant.to_string(), &timeline.to_string()])
                .await
                .context("advance_disk_consistent_lsn probe")?;
            match row {
                None => anyhow::bail!(
                    "no timeline row for {tenant}/{timeline} — \
                     advance_disk_consistent_lsn called before timeline_create wrote the row"
                ),
                Some(r) => {
                    let live_gen: i32 = r.get(0);
                    Ok(AdvanceDcLsnOutcome::StaleGeneration {
                        live_generation: live_gen as u32,
                    })
                }
            }
        } else {
            Ok(AdvanceDcLsnOutcome::Updated)
        }
    }

    pub async fn bump_generation(&self, tenant: TenantId, timeline: TimelineId) -> Result<i32> {
        let sql = format!(
            "UPDATE \"{}\".timeline_state \
             SET current_generation = current_generation + 1 \
             WHERE tenant_id = $1 AND timeline_id = $2 \
             RETURNING current_generation",
            self.schema
        );
        let row = self
            .ysql_client
            .query_opt(&sql, &[&tenant.to_string(), &timeline.to_string()])
            .await
            .context("bump_generation")?
            .with_context(|| format!("no timeline row for {tenant}/{timeline}"))?;
        Ok(row.get::<_, i32>(0))
    }

    pub async fn read_generation(
        &self,
        tenant: TenantId,
        timeline: TimelineId,
    ) -> Result<Option<i32>> {
        let sql = format!(
            "SELECT current_generation FROM \"{}\".timeline_state \
             WHERE tenant_id = $1 AND timeline_id = $2",
            self.schema
        );
        let row = self
            .ysql_client
            .query_opt(&sql, &[&tenant.to_string(), &timeline.to_string()])
            .await
            .context("read_generation")?;
        Ok(row.map(|r| r.get::<_, i32>(0)))
    }

    /// Test-only escape hatch — runs raw SQL on the underlying ysql_client.
    /// Used by integration tests to create / drop per-test schemas. Not
    /// for production callers.
    #[doc(hidden)]
    pub async fn test_only_execute(&self, sql: &str) -> Result<u64> {
        self.ysql_client.execute(sql, &[]).await
    }
}

/// Invariants that every chain returned by `read_chain` / `scan_range` /
/// `read_slru_chain` / `scan_slru_range` must satisfy *for a single key*
/// when iterated in the SQL's `lsn DESC, is_image DESC` order — i.e.
/// in the order rows are pushed into the chain `Vec`.
///
/// 1. `lsn` is non-increasing as we step through the slice.
/// 2. At a tied LSN, an image (`is_image == true`) MUST come before a
///    delta (`is_image == false`). This guarantees the
///    "break at first will_init=true" trim sees the image first and
///    drops the same-LSN delta (which is the WAL record already baked
///    into the image — applying it would double-apply).
///
/// Violations indicate either a missing `ORDER BY ... is_image DESC`
/// tiebreaker (caller bug) or stale prepared-statement state. Panic
/// rather than silently feed walredo a chain that would yield wrong
/// page contents.
fn assert_chain_order_for_key(chain: &[(Lsn, bool, bool /* will_init */)], where_: &str) {
    let mut prev: Option<(Lsn, bool)> = None;
    for (i, &(lsn, is_image, _will_init)) in chain.iter().enumerate() {
        if let Some((plsn, pis_image)) = prev {
            if lsn > plsn {
                panic!("{where_}: chain row {i} lsn {lsn} > prev lsn {plsn} (must be DESC)",);
            }
            if lsn == plsn && !pis_image && is_image {
                panic!(
                    "{where_}: chain has image at lsn {lsn} after a delta at the same lsn (need ORDER BY ..., is_image DESC tiebreaker)",
                );
            }
        }
        prev = Some((lsn, is_image));
    }
}

#[cfg(test)]
mod chain_order_tests {
    //! Unit tests for [`assert_chain_order_for_key`]. The guard runs
    //! after every chain trim in `read_chain` / `scan_range` /
    //! `read_slru_chain` / `scan_slru_range`, so a regression in
    //! either the SQL `ORDER BY` clause or in the row-to-PageRow
    //! decode would be caught here without needing a live YB.
    use super::*;

    fn row(lsn: u64, is_image: bool, will_init: bool) -> (Lsn, bool, bool) {
        (Lsn(lsn), is_image, will_init)
    }

    /// The canonical happy-path chain. Walking the trim DESC: a delta
    /// at the higher LSN, then the image (with will_init) at the
    /// lower LSN — break. Strict-decreasing LSNs, no tied LSNs.
    #[test]
    fn pure_descending_lsn_is_accepted() {
        assert_chain_order_for_key(
            &[
                row(300, false, false),
                row(200, false, false),
                row(100, true, true),
            ],
            "test_pure_desc",
        );
    }

    /// The post-fix tied-LSN order. At LSN 100 the image is FIRST,
    /// then a same-LSN delta would appear (but the trim breaks
    /// on the image before reaching it). This represents what the
    /// SQL with `ORDER BY p.lsn DESC, p.is_image DESC` produces.
    #[test]
    fn image_first_at_tied_lsn_is_accepted() {
        assert_chain_order_for_key(
            &[
                row(200, false, false),
                row(100, true, true),
                row(100, false, false),
            ],
            "test_image_first_tie",
        );
    }

    /// Two deltas at the same LSN, then an image at a lower LSN.
    /// Same-LSN deltas are legal (different sub-records or same
    /// record decoded twice for different keys can land at the same
    /// LSN); only image-AFTER-delta-at-tied-LSN is forbidden.
    #[test]
    fn delta_delta_at_same_lsn_is_accepted() {
        assert_chain_order_for_key(
            &[
                row(200, false, false),
                row(200, false, false),
                row(100, true, true),
            ],
            "test_delta_delta_tie",
        );
    }

    /// Two images at the same LSN — pathological but legal under
    /// the rule. The first one anchors the trim; the second would
    /// be dropped by the will_init break in the caller. The guard
    /// only forbids image-AFTER-delta at the same LSN.
    #[test]
    fn image_image_at_same_lsn_is_accepted() {
        assert_chain_order_for_key(
            &[row(100, true, true), row(100, true, true)],
            "test_image_image_tie",
        );
    }

    /// Single-row chain is trivially ordered.
    #[test]
    fn single_row_is_accepted() {
        assert_chain_order_for_key(&[row(100, true, true)], "test_single");
    }

    /// Empty chain is trivially ordered.
    #[test]
    fn empty_chain_is_accepted() {
        assert_chain_order_for_key(&[], "test_empty");
    }

    /// LSN increasing across two rows must panic — the trim relies
    /// on DESC order, and walredo would then receive WAL records in
    /// the wrong replay order.
    #[test]
    #[should_panic(expected = "must be DESC")]
    fn lsn_ascending_panics() {
        assert_chain_order_for_key(
            &[row(100, false, false), row(200, false, false)],
            "test_lsn_asc",
        );
    }

    /// Image AFTER a delta at the same LSN is the pre-fix bug. The
    /// trim would push the delta, then break at the image, so the
    /// chain handed to walredo would contain a same-LSN delta whose
    /// effect is already baked into the image — double-apply.
    #[test]
    #[should_panic(expected = "after a delta at the same lsn")]
    fn image_after_delta_at_tied_lsn_panics() {
        assert_chain_order_for_key(
            &[
                row(200, false, false),
                row(100, false, false),
                row(100, true, true),
            ],
            "test_image_after_delta_tie",
        );
    }

    /// LSN strict-decreasing is fine; only an increase panics. The
    /// equal-LSN case is allowed (handled by the `lsn > plsn` check,
    /// not `>=`).
    #[test]
    fn equal_lsn_does_not_trigger_desc_panic() {
        // Two deltas at the same LSN — first row sets the prev, second
        // hits `lsn == plsn`, which does NOT trigger the DESC panic
        // (only `lsn > plsn` does). The image-after-delta check also
        // doesn't fire because both rows are deltas.
        assert_chain_order_for_key(
            &[row(100, false, false), row(100, false, false)],
            "test_equal_lsn_deltas",
        );
    }
}

fn row_to_page(r: tokio_postgres::Row) -> Result<PageRow> {
    let sp: i64 = r.try_get(0)?;
    let db: i64 = r.try_get(1)?;
    let rel: i64 = r.try_get(2)?;
    let fork: i16 = r.try_get(3)?;
    let blk: i64 = r.try_get(4)?;
    let lsn: i64 = r.try_get(5)?;
    let is_image: bool = r.try_get(6)?;
    let will_init: bool = r.try_get(7)?;
    let payload: Vec<u8> = r.try_get(8)?;
    Ok(PageRow {
        key: columns_to_key_bytes(sp, db, rel, fork, blk),
        lsn: Lsn(lsn as u64),
        is_image,
        will_init,
        payload: Bytes::from(payload),
    })
}

fn row_to_meta(r: tokio_postgres::Row) -> Result<MetaRow> {
    let key: Vec<u8> = r.try_get(0)?;
    let lsn: i64 = r.try_get(1)?;
    let payload: Vec<u8> = r.try_get(2)?;
    Ok(MetaRow {
        key: Bytes::from(key),
        lsn: Lsn(lsn as u64),
        payload: Bytes::from(payload),
    })
}

fn row_to_slru_page(r: tokio_postgres::Row) -> Result<SlruPageRow> {
    let kind_i: i16 = r.try_get(0)?;
    let segno: i64 = r.try_get(1)?;
    let blknum: i64 = r.try_get(2)?;
    let lsn: i64 = r.try_get(3)?;
    let is_image: bool = r.try_get(4)?;
    let will_init: bool = r.try_get(5)?;
    let payload: Vec<u8> = r.try_get(6)?;
    Ok(SlruPageRow {
        kind: slru_kind_from_i16(kind_i)?,
        segno: segno as u32,
        blknum: blknum as u32,
        lsn: Lsn(lsn as u64),
        is_image,
        will_init,
        payload: Bytes::from(payload),
    })
}

/// Column list shared by every `SELECT … FROM timeline_state` so the
/// query layout stays in sync with `row_to_timeline`'s positional
/// `try_get` indices.
const TIMELINE_ROW_COLUMNS: &str = "tenant_id, timeline_id, disk_consistent_lsn, \
     applied_gc_cutoff_lsn, ancestor_timeline_id, ancestor_lsn, \
     current_generation, pg_version, initdb_lsn, prev_record_lsn, \
     archived_at, deleted_at, rel_size_migration, \
     rel_size_migrated_at, lineage";

/// Convert the two awkwardly-typed `TimelineRow` fields (the
/// `RelSizeMigration` enum, persisted as serde-JSON text to match
/// the upstream `index_part.json` wire shape; and `lineage`,
/// persisted as a JSONB blob for forward-compat) into the values
/// the `tokio_postgres::Client::execute` parameter list expects.
/// Shared by `insert_timeline_at_creation` and
/// `update_timeline_state` so the serialisation stays in lockstep.
fn serialize_row_aux(row: &TimelineRow) -> Result<(Option<String>, serde_json::Value)> {
    let rel_size_migration_text = row
        .rel_size_migration
        .as_ref()
        .map(serde_json::to_string)
        .transpose()
        .context("serialising rel_size_migration")?;
    let lineage_json = serde_json::to_value(&row.lineage).context("serialising lineage")?;
    Ok((rel_size_migration_text, lineage_json))
}

fn row_to_timeline(r: tokio_postgres::Row) -> Result<TimelineRow> {
    let tenant_s: String = r.try_get(0)?;
    let timeline_s: String = r.try_get(1)?;
    let disk_consistent_lsn: i64 = r.try_get(2)?;
    let applied_gc_cutoff_lsn: i64 = r.try_get(3)?;
    let ancestor_tl: Option<String> = r.try_get(4)?;
    let ancestor_lsn: Option<i64> = r.try_get(5)?;
    let current_generation: i32 = r.try_get(6)?;
    let pg_version: i32 = r.try_get(7)?;
    let initdb_lsn: Option<i64> = r.try_get(8)?;
    let prev_record_lsn: Option<i64> = r.try_get(9)?;
    let archived_at: Option<chrono::NaiveDateTime> = r.try_get(10)?;
    let deleted_at: Option<chrono::NaiveDateTime> = r.try_get(11)?;
    let rel_size_migration_text: Option<String> = r.try_get(12)?;
    let rel_size_migrated_at: Option<i64> = r.try_get(13)?;
    let lineage_json: Option<serde_json::Value> = r.try_get(14)?;

    let rel_size_migration = rel_size_migration_text
        .as_deref()
        .map(serde_json::from_str)
        .transpose()
        .context("parsing rel_size_migration")?;
    let lineage = lineage_json
        .map(serde_json::from_value)
        .transpose()
        .context("parsing lineage")?
        .unwrap_or_default();

    Ok(TimelineRow {
        tenant_id: tenant_s.parse().context("parsing tenant_id")?,
        timeline_id: timeline_s.parse().context("parsing timeline_id")?,
        disk_consistent_lsn: Lsn(disk_consistent_lsn as u64),
        applied_gc_cutoff_lsn: Lsn(applied_gc_cutoff_lsn as u64),
        ancestor_timeline_id: ancestor_tl
            .map(|s| s.parse::<TimelineId>())
            .transpose()
            .context("parsing ancestor_timeline_id")?,
        ancestor_lsn: ancestor_lsn.map(|l| Lsn(l as u64)),
        current_generation,
        pg_version: pg_version as u32,
        initdb_lsn: initdb_lsn.map(|l| Lsn(l as u64)),
        prev_record_lsn: prev_record_lsn.map(|l| Lsn(l as u64)),
        archived_at,
        deleted_at,
        rel_size_migration,
        rel_size_migrated_at: rel_size_migrated_at.map(|l| Lsn(l as u64)),
        lineage,
    })
}

#[cfg(test)]
mod tests {
    use super::*;

    fn test_yb_config() -> YbStoreConfig {
        YbStoreConfig {
            host: std::env::var("YB_TEST_HOST").unwrap_or_else(|_| "127.0.0.1".to_string()),
            port: std::env::var("YB_TEST_PORT")
                .ok()
                .and_then(|s| s.parse().ok())
                .unwrap_or(5433),
            user: std::env::var("YB_TEST_USER").unwrap_or_else(|_| "yugabyte".to_string()),
            password_env: None,
            dbname: std::env::var("YB_TEST_DB").unwrap_or_else(|_| "yb_storage".to_string()),
            table_name: "pages".to_string(),
            prefix: Some("test/".to_string()),
            sslmode: None,
            sslrootcert: None,
            concurrency_limit: std::num::NonZeroUsize::new(8).unwrap(),
        }
    }

    /// All tests run against the production `pageserver.*` schema
    /// (created by storage_controller's diesel migration on a
    /// real `neon_local`-style cluster). Per-test isolation comes
    /// from `rand_tenant()` / `rand_timeline()` — every test writes
    /// to a distinct `(tenant_id, timeline_id)` namespace, so
    /// concurrent tests don't observe each other's data and rows
    /// from prior runs are harmless.
    ///
    /// Requires the YB cluster to have the diesel migration applied
    /// (e.g. `neon_local init && neon_local start` once). The tests
    /// don't create the schema themselves; that's the migration's
    /// job and there's only one source of truth for it.
    static SHARED_STORE: tokio::sync::OnceCell<PageStore> = tokio::sync::OnceCell::const_new();
    const PROD_SCHEMA: &str = "pageserver";

    async fn make_store(_suffix: &str) -> (PageStore, String) {
        let store = SHARED_STORE
            .get_or_init(|| async {
                let cfg = PageStoreConfig {
                    yb: test_yb_config(),
                    schema: PROD_SCHEMA.to_string(),
                    scale_factor: pageserver_api::config::DEFAULT_SCALE_FACTOR,
                };
                PageStore::connect(cfg, "page_store_test", true)
                    .await
                    .expect("connect to local YB; run `neon_local init && start` first to migrate the schema")
            })
            .await
            .clone();
        (store, PROD_SCHEMA.to_string())
    }

    async fn drop_schema(_store: &PageStore, _schema: &str) {
        // No-op: the schema is the production one, owned by the
        // diesel migration. Per-test isolation comes from random
        // tenant/timeline IDs.
    }

    fn rand_tenant() -> TenantId {
        TenantId::generate()
    }

    fn rand_timeline() -> TimelineId {
        TimelineId::generate()
    }

    /// Build a synthetic relation-page Key:
    /// `field1=0, spcnode, dbnode, relnode=16384+ord, forknum=0, blknum`.
    /// `field4 != 0` is required by `is_rel_block_key`.
    fn page_key(spcnode: u32, dbnode: u32, relnode: u32, fork: u8, blknum: u32) -> Bytes {
        columns_to_key_bytes(
            spcnode as i64,
            dbnode as i64,
            relnode as i64,
            fork as i16,
            blknum as i64,
        )
    }

    fn page_row(key: Bytes, lsn: u64, is_image: bool, will_init: bool, payload: &[u8]) -> PageRow {
        PageRow {
            key,
            lsn: Lsn(lsn),
            is_image,
            will_init,
            payload: Bytes::copy_from_slice(payload),
        }
    }

    #[tokio::test]
    async fn put_and_read_chain_round_trip() {
        let (store, schema) = make_store("rt").await;
        let tenant = rand_tenant();
        let timeline = rand_timeline();
        let k = page_key(1663, 5, 16384, 0, 7);

        store
            .put_pages(
                tenant,
                timeline,
                &[
                    page_row(k.clone(), 10, true, true, b"img"),
                    page_row(k.clone(), 20, false, false, b"delta-A"),
                    page_row(k.clone(), 30, false, false, b"delta-B"),
                ],
            )
            .await
            .unwrap();

        let got = store
            .read_chain(tenant, timeline, &k, Lsn(100))
            .await
            .unwrap();
        assert_eq!(got.len(), 3);
        assert_eq!(got[0].lsn, Lsn(30));
        assert_eq!(got[2].lsn, Lsn(10));
        assert!(got[2].is_image);
        assert!(got[2].will_init);
        drop_schema(&store, &schema).await;
    }

    #[tokio::test]
    async fn put_rejects_non_page_key() {
        let (store, schema) = make_store("reject_non_page").await;
        let tenant = rand_tenant();
        let timeline = rand_timeline();

        // field1 = 0x65 (REL_DIR_KEY_PREFIX) - not a relation page
        let mut buf = [0u8; KEY_SIZE];
        buf[0] = 0x65;
        let bad_key = Bytes::copy_from_slice(&buf);

        let err = store
            .put_pages(tenant, timeline, &[page_row(bad_key, 10, true, true, b"x")])
            .await
            .unwrap_err();
        let msg = format!("{err:#}");
        assert!(
            msg.contains("not a relation block") || msg.contains("non-page keys"),
            "{msg}"
        );
        drop_schema(&store, &schema).await;
    }

    #[tokio::test]
    async fn read_chain_returns_image_anchor_and_deltas_above() {
        // 1 image at LSN 10, then 12 deltas at LSNs 20..130.
        // read_chain at high target → image at 10 + all 12 deltas = 13 rows.
        // read_chain at LSN 50 → image at 10 + 4 deltas (20, 30, 40, 50) = 5 rows.
        // read_chain at LSN 5 → empty (no anchor at or below 5).
        let (store, schema) = make_store("anchor").await;
        let tenant = rand_tenant();
        let timeline = rand_timeline();
        let k = page_key(1663, 5, 16384, 0, 1);

        let mut rows = vec![page_row(k.clone(), 10, true, true, b"img")];
        for i in 1..=12 {
            rows.push(page_row(
                k.clone(),
                10 + 10 * i,
                false,
                false,
                &[b'd', i as u8],
            ));
        }
        store.put_pages(tenant, timeline, &rows).await.unwrap();

        let got = store
            .read_chain(tenant, timeline, &k, Lsn(10_000))
            .await
            .unwrap();
        assert_eq!(got.len(), 13);
        assert_eq!(got.first().unwrap().lsn, Lsn(130));
        assert_eq!(got.last().unwrap().lsn, Lsn(10));
        assert!(got.last().unwrap().is_image);

        let got = store
            .read_chain(tenant, timeline, &k, Lsn(50))
            .await
            .unwrap();
        assert_eq!(got.len(), 5);
        assert_eq!(got.first().unwrap().lsn, Lsn(50));
        assert_eq!(got.last().unwrap().lsn, Lsn(10));

        let got = store
            .read_chain(tenant, timeline, &k, Lsn(5))
            .await
            .unwrap();
        assert!(got.is_empty());
        drop_schema(&store, &schema).await;
    }

    /// Regression for the 2026-06-06 incident: PS materializes images
    /// at the same LSN as the WAL record they represent — the image
    /// row is the post-record state, the same-LSN delta row is the
    /// raw WAL record (its effect already baked into the image).
    ///
    /// Before the fix, the SQL was `ORDER BY p.lsn DESC` with no
    /// tiebreaker; DocDB was free to return the delta before the
    /// image at the tied LSN. The Rust trim breaks at the first
    /// `will_init = true` row, so the chain would include
    /// `[higher_delta, tied_delta, tied_image]` — walredo would then
    /// double-apply the tied WAL record (once via the image, once
    /// via the delta) and produce a corrupt page.
    ///
    /// After the fix (`ORDER BY p.lsn DESC, p.is_image DESC`) the
    /// image at the tied LSN ALWAYS comes first; the trim breaks
    /// on it and the same-LSN delta is dropped. Walredo sees
    /// `[higher_delta, tied_image]`, applies the higher delta once
    /// to the post-tied-record image, and produces the right page.
    ///
    /// Note: we intentionally insert the rows in the worst-case
    /// "delta first" insertion order so we don't accidentally test
    /// the trivial case where the storage layer happens to return
    /// images first regardless of ORDER BY.
    #[tokio::test]
    async fn read_chain_image_before_delta_at_tied_lsn() {
        let (store, schema) = make_store("tied_lsn_image_first").await;
        let tenant = rand_tenant();
        let timeline = rand_timeline();
        let k = page_key(1663, 5, 16384, 0, 99);

        // Same-LSN image+delta at 100, plus a later delta at 200.
        // Insert image LAST so any incidental storage order would
        // bury it after the same-LSN delta.
        store
            .put_pages(
                tenant,
                timeline,
                &[
                    page_row(k.clone(), 100, false, false, b"wal-rec-at-100"),
                    page_row(k.clone(), 200, false, false, b"wal-rec-at-200"),
                    page_row(k.clone(), 100, true, true, b"post-wal-100-image"),
                ],
            )
            .await
            .unwrap();

        let got = store
            .read_chain(tenant, timeline, &k, Lsn(300))
            .await
            .unwrap();

        // Expected: [delta@200, image@100]. Length 2 — NOT 3
        // (length 3 would mean the same-LSN delta slipped past the
        // image anchor, which is the pre-fix bug).
        assert_eq!(
            got.len(),
            2,
            "chain should be [delta@200, image@100], got: {:?}",
            got.iter()
                .map(|r| (r.lsn.0, r.is_image))
                .collect::<Vec<_>>()
        );
        assert_eq!(got[0].lsn, Lsn(200));
        assert!(!got[0].is_image);
        assert_eq!(got[1].lsn, Lsn(100));
        assert!(
            got[1].is_image,
            "trim must break on the image, not the same-LSN delta"
        );
        assert!(got[1].will_init);

        drop_schema(&store, &schema).await;
    }

    #[tokio::test]
    async fn read_chain_slow_path_walks_to_will_init_delta() {
        // No image. A will_init delta at LSN 10, then plain deltas at
        // 20, 30, 40. Older plain deltas at LSNs 1, 5 below the anchor.
        // Read at LSN 100 should return rows 40, 30, 20, 10 (anchor)
        // — older rows are shadowed.
        let (store, schema) = make_store("slow_will_init").await;
        let tenant = rand_tenant();
        let timeline = rand_timeline();
        let k = page_key(1663, 5, 16384, 0, 2);

        let rows = vec![
            page_row(k.clone(), 1, false, false, b"d-old1"),
            page_row(k.clone(), 5, false, false, b"d-old2"),
            page_row(k.clone(), 10, false, true, b"init-delta"),
            page_row(k.clone(), 20, false, false, b"d20"),
            page_row(k.clone(), 30, false, false, b"d30"),
            page_row(k.clone(), 40, false, false, b"d40"),
        ];
        store.put_pages(tenant, timeline, &rows).await.unwrap();
        let got = store
            .read_chain(tenant, timeline, &k, Lsn(100))
            .await
            .unwrap();
        assert_eq!(got.len(), 4);
        let lsns: Vec<u64> = got.iter().map(|r| r.lsn.0).collect();
        assert_eq!(lsns, vec![40, 30, 20, 10]);
        assert!(got.last().unwrap().will_init);
        drop_schema(&store, &schema).await;
    }

    /// Image at LSN 10, deltas above, then an init-delta at LSN 25, then
    /// more deltas to LSN 30. The merged `read_chain` SQL returns rows
    /// from the image's anchor (LSN 10) forward; the ysql_client-side trim
    /// must stop at the init-delta at LSN 25 — the chain past that point
    /// is what the image-then-deltas walredo needs, but the deltas
    /// between the image and the init-delta are superseded by the
    /// init-delta and must NOT be returned (applying them on the
    /// post-init page state would corrupt walredo).
    ///
    /// Regression for commit `0cd9085dd` (read_chain SQL collapse): the
    /// old fast path returned the wider `[image..target]` window
    /// silently in this shape; the new merged-SQL + unconditional trim
    /// must shorten the chain to `[init-delta..target]`.
    #[tokio::test]
    async fn read_chain_trims_at_init_delta_above_image_anchor() {
        let (store, schema) = make_store("trim_init_above_image").await;
        let tenant = rand_tenant();
        let timeline = rand_timeline();
        let k = page_key(1663, 5, 16384, 0, 3);

        store
            .put_pages(
                tenant,
                timeline,
                &[
                    page_row(k.clone(), 10, true, true, b"img"),
                    page_row(k.clone(), 15, false, false, b"d15"),
                    page_row(k.clone(), 20, false, false, b"d20"),
                    page_row(k.clone(), 25, false, true, b"init-delta-25"),
                    page_row(k.clone(), 28, false, false, b"d28"),
                    page_row(k.clone(), 30, false, false, b"d30"),
                ],
            )
            .await
            .unwrap();

        let got = store
            .read_chain(tenant, timeline, &k, Lsn(30))
            .await
            .unwrap();
        let lsns: Vec<u64> = got.iter().map(|r| r.lsn.0).collect();
        assert_eq!(
            lsns,
            vec![30, 28, 25],
            "chain must stop at the LSN-25 init-delta; image at LSN 10 and \
             pre-init deltas at LSNs 15/20 are shadowed"
        );
        assert!(
            got.last().unwrap().will_init,
            "anchor returned to caller must be a will_init row"
        );
        drop_schema(&store, &schema).await;
    }

    #[tokio::test]
    async fn scan_range_within_one_fork() {
        let (store, schema) = make_store("scan_fork").await;
        let tenant = rand_tenant();
        let timeline = rand_timeline();
        // 5 pages of one fork.
        let mut rows = Vec::new();
        for blk in 0u32..5 {
            let k = page_key(1663, 5, 16384, 0, blk);
            rows.push(page_row(
                k.clone(),
                10,
                true,
                true,
                format!("img{blk}").as_bytes(),
            ));
            rows.push(page_row(k, 20, false, false, format!("d{blk}").as_bytes()));
        }
        // Plus pages on a different rel (should NOT be returned).
        let other = page_key(1663, 5, 99999, 0, 0);
        rows.push(page_row(other, 10, true, true, b"other-img"));
        store.put_pages(tenant, timeline, &rows).await.unwrap();

        // Scan blocks 1..4 of (rel=16384, fork=0).
        let range = Key::from_slice(&page_key(1663, 5, 16384, 0, 1))
            ..Key::from_slice(&page_key(1663, 5, 16384, 0, 4));
        let got = store
            .scan_range(tenant, timeline, range, Lsn(100))
            .await
            .unwrap();
        // 3 keys (blknum 1, 2, 3); each with 2 rows.
        assert_eq!(got.len(), 3);
        for (_k, chain) in &got {
            assert_eq!(chain.len(), 2);
            assert_eq!(chain[0].lsn, Lsn(20));
            assert_eq!(chain[1].lsn, Lsn(10));
        }
        drop_schema(&store, &schema).await;
    }

    #[tokio::test]
    async fn scan_range_single_key_dispatch() {
        // Detects k..k.next() and uses the per-key fast path.
        // Verifies the result is the same as read_chain for that key.
        let (store, schema) = make_store("scan_single").await;
        let tenant = rand_tenant();
        let timeline = rand_timeline();
        let k_bytes = page_key(1663, 5, 16384, 0, 7);
        let k = Key::from_slice(&k_bytes);

        store
            .put_pages(
                tenant,
                timeline,
                &[
                    page_row(k_bytes.clone(), 10, true, true, b"img"),
                    page_row(k_bytes.clone(), 20, false, false, b"d20"),
                    page_row(k_bytes.clone(), 30, false, false, b"d30"),
                ],
            )
            .await
            .unwrap();

        let range = k..k.next();
        let got = store
            .scan_range(tenant, timeline, range, Lsn(100))
            .await
            .unwrap();
        assert_eq!(got.len(), 1);
        let chain = &got[&k_bytes];
        let lsns: Vec<u64> = chain.iter().map(|r| r.lsn.0).collect();
        assert_eq!(lsns, vec![30, 20, 10]);
        assert!(chain.last().unwrap().is_image);

        // Same target via read_chain — must match.
        let direct = store
            .read_chain(tenant, timeline, &k_bytes, Lsn(100))
            .await
            .unwrap();
        assert_eq!(chain.as_slice(), direct.as_slice());
        drop_schema(&store, &schema).await;
    }

    #[tokio::test]
    async fn scan_range_single_key_empty_when_no_anchor() {
        // A key with no rows ≤ target returns an empty map.
        let (store, schema) = make_store("scan_single_empty").await;
        let tenant = rand_tenant();
        let timeline = rand_timeline();
        let k_bytes = page_key(1663, 5, 16384, 0, 99);
        let k = Key::from_slice(&k_bytes);

        let got = store
            .scan_range(tenant, timeline, k..k.next(), Lsn(100))
            .await
            .unwrap();
        assert!(got.is_empty());
        drop_schema(&store, &schema).await;
    }

    #[tokio::test]
    async fn scan_range_anchors_on_per_block_anchor() {
        // Block 0: image at LSN 50, deltas at 20, 30, 40, 60, 70.
        //          Expected return: 60, 70 above image, plus 50 itself => 3 rows.
        //          (Rows 20, 30, 40 are shadowed by the image.)
        // Block 1: image at LSN 30 only. Expected: just the image (1 row).
        // Block 2: no image, will_init delta at LSN 20, deltas at 30, 40.
        //          Expected: 40, 30, 20 => 3 rows (slow path).
        // Block 3: no image, no will_init — only plain deltas at 30, 40.
        //          Expected: 40, 30 (full chain, no anchor — chain-decoder
        //          will fail walredo if reads ever ask for this, but the
        //          scan itself returns what's there).
        let (store, schema) = make_store("anchor_range").await;
        let tenant = rand_tenant();
        let timeline = rand_timeline();

        let k0 = page_key(1663, 5, 16384, 0, 0);
        let k1 = page_key(1663, 5, 16384, 0, 1);
        let k2 = page_key(1663, 5, 16384, 0, 2);
        let k3 = page_key(1663, 5, 16384, 0, 3);

        let rows = vec![
            // k0
            page_row(k0.clone(), 20, false, false, b"k0-20"),
            page_row(k0.clone(), 30, false, false, b"k0-30"),
            page_row(k0.clone(), 40, false, false, b"k0-40"),
            page_row(k0.clone(), 50, true, true, b"k0-img"),
            page_row(k0.clone(), 60, false, false, b"k0-60"),
            page_row(k0.clone(), 70, false, false, b"k0-70"),
            // k1
            page_row(k1.clone(), 30, true, true, b"k1-img"),
            // k2
            page_row(k2.clone(), 20, false, true, b"k2-init"),
            page_row(k2.clone(), 30, false, false, b"k2-30"),
            page_row(k2.clone(), 40, false, false, b"k2-40"),
            // k3
            page_row(k3.clone(), 30, false, false, b"k3-30"),
            page_row(k3.clone(), 40, false, false, b"k3-40"),
        ];
        store.put_pages(tenant, timeline, &rows).await.unwrap();

        let range = Key::from_slice(&k0)..Key::from_slice(&page_key(1663, 5, 16384, 0, 4));
        let got = store
            .scan_range(tenant, timeline, range, Lsn(100))
            .await
            .unwrap();
        assert_eq!(got.len(), 4);

        // k0: image at 50 + deltas 60, 70 => LSNs 70, 60, 50.
        let c0 = &got[&k0];
        let lsns0: Vec<u64> = c0.iter().map(|r| r.lsn.0).collect();
        assert_eq!(lsns0, vec![70, 60, 50], "k0 should anchor on image @ 50");
        assert!(c0.last().unwrap().is_image);

        // k1: just the image.
        let c1 = &got[&k1];
        assert_eq!(c1.len(), 1);
        assert_eq!(c1[0].lsn, Lsn(30));
        assert!(c1[0].is_image);

        // k2: slow path — no image — full chain to the will_init delta.
        let c2 = &got[&k2];
        let lsns2: Vec<u64> = c2.iter().map(|r| r.lsn.0).collect();
        assert_eq!(lsns2, vec![40, 30, 20]);
        assert!(c2.last().unwrap().will_init);
        assert!(!c2.last().unwrap().is_image);

        // k3: full chain returned; no anchor (would be a walredo error
        // upstream, but the store call itself succeeds).
        let c3 = &got[&k3];
        let lsns3: Vec<u64> = c3.iter().map(|r| r.lsn.0).collect();
        assert_eq!(lsns3, vec![40, 30]);

        drop_schema(&store, &schema).await;
    }

    #[tokio::test]
    async fn scan_range_cross_fork() {
        let (store, schema) = make_store("scan_cross").await;
        let tenant = rand_tenant();
        let timeline = rand_timeline();
        // Pages in two different rels.
        let mut rows = Vec::new();
        for rel in [16384u32, 16385u32] {
            for blk in 0u32..3 {
                let k = page_key(1663, 5, rel, 0, blk);
                rows.push(page_row(
                    k,
                    10,
                    true,
                    true,
                    format!("{rel}-{blk}").as_bytes(),
                ));
            }
        }
        store.put_pages(tenant, timeline, &rows).await.unwrap();

        // Range spans both rels.
        let range = Key::from_slice(&page_key(1663, 5, 16384, 0, 0))
            ..Key::from_slice(&page_key(1663, 5, 16386, 0, 0));
        let got = store
            .scan_range(tenant, timeline, range, Lsn(100))
            .await
            .unwrap();
        // 6 distinct keys (3 blocks × 2 rels).
        assert_eq!(got.len(), 6);
        drop_schema(&store, &schema).await;
    }

    #[tokio::test]
    async fn scan_range_rejects_non_page_namespace() {
        let (store, schema) = make_store("scan_reject").await;
        let tenant = rand_tenant();
        let timeline = rand_timeline();
        // field1 = 0x60 (RELATION_SIZE_PREFIX) - metadata namespace
        let mut s = [0u8; KEY_SIZE];
        s[0] = 0x60;
        let mut e = [0u8; KEY_SIZE];
        e[0] = 0x61;
        let range = Key::from_slice(&s)..Key::from_slice(&e);
        let err = store
            .scan_range(tenant, timeline, range, Lsn(100))
            .await
            .unwrap_err();
        let msg = format!("{err:#}");
        assert!(
            msg.contains("relation-page namespace") || msg.contains("non-page"),
            "{msg}"
        );
        drop_schema(&store, &schema).await;
    }

    #[tokio::test]
    async fn read_chain_isolates_by_tenant_and_timeline() {
        let (store, schema) = make_store("isolate").await;
        let k = page_key(1663, 5, 16384, 0, 2);
        let t1 = rand_tenant();
        let t2 = rand_tenant();
        let tl1 = rand_timeline();
        let tl2 = rand_timeline();
        store
            .put_pages(t1, tl1, &[page_row(k.clone(), 10, true, true, b"t1tl1")])
            .await
            .unwrap();
        store
            .put_pages(t2, tl1, &[page_row(k.clone(), 10, true, true, b"t2tl1")])
            .await
            .unwrap();
        store
            .put_pages(t1, tl2, &[page_row(k.clone(), 10, true, true, b"t1tl2")])
            .await
            .unwrap();
        assert_eq!(
            store.read_chain(t1, tl1, &k, Lsn(100)).await.unwrap()[0]
                .payload
                .as_ref(),
            b"t1tl1"
        );
        assert_eq!(
            store.read_chain(t2, tl1, &k, Lsn(100)).await.unwrap()[0]
                .payload
                .as_ref(),
            b"t2tl1"
        );
        assert_eq!(
            store.read_chain(t1, tl2, &k, Lsn(100)).await.unwrap()[0]
                .payload
                .as_ref(),
            b"t1tl2"
        );
        drop_schema(&store, &schema).await;
    }

    #[tokio::test]
    async fn high_lsn_round_trip() {
        let (store, schema) = make_store("high_lsn").await;
        let tenant = rand_tenant();
        let timeline = rand_timeline();
        let k = page_key(1663, 5, 16384, 0, 0xff);
        let high = 0x7fff_ffff_ffff_fff0u64;
        store
            .put_pages(
                tenant,
                timeline,
                &[page_row(k.clone(), high, true, true, b"x")],
            )
            .await
            .unwrap();
        let got = store
            .read_chain(tenant, timeline, &k, Lsn(u64::MAX >> 1))
            .await
            .unwrap();
        assert_eq!(got.len(), 1);
        assert_eq!(got[0].lsn.0, high);
        drop_schema(&store, &schema).await;
    }

    fn sample_timeline_row(t: TenantId, tl: TimelineId) -> TimelineRow {
        TimelineRow {
            tenant_id: t,
            timeline_id: tl,
            disk_consistent_lsn: Lsn(90),
            applied_gc_cutoff_lsn: Lsn(50),
            ancestor_timeline_id: None,
            ancestor_lsn: None,
            current_generation: 1,
            pg_version: 17,
            initdb_lsn: None,
            prev_record_lsn: None,
            archived_at: None,
            deleted_at: None,
            rel_size_migration: None,
            rel_size_migrated_at: None,
            lineage: Default::default(),
        }
    }

    #[tokio::test]
    async fn upsert_and_get_timeline_round_trip() {
        let (store, schema) = make_store("tl_rt").await;
        let tenant = rand_tenant();
        let timeline = rand_timeline();
        let row = sample_timeline_row(tenant, timeline);
        store.insert_timeline_at_creation(&row).await.unwrap();
        let got = store.get_timeline(tenant, timeline).await.unwrap().unwrap();
        assert_eq!(got, row);
        drop_schema(&store, &schema).await;
    }

    /// `cas_bump_generation_on_attach` covers the four outcomes:
    /// Bumped (stored < ours), AlreadyAtOurGen (stored == ours),
    /// StaleGeneration (stored > ours), and the wiring-bug error
    /// path (row missing). Regression for the symptom that hit prod
    /// where every PS restart shut itself down as "stale" because
    /// the controller bumped its gen but nothing pushed the new
    /// value to the YB column.
    #[tokio::test]
    async fn cas_bump_generation_on_attach_outcomes() {
        use utils::generation::Generation;
        use utils::lsn::Lsn;

        let (store, schema) = make_store("gen_cas").await;
        let tenant = rand_tenant();
        let timeline = rand_timeline();
        let mut row = sample_timeline_row(tenant, timeline);
        row.current_generation = 1;
        row.disk_consistent_lsn = Lsn(0x1000);
        store.insert_timeline_at_creation(&row).await.unwrap();

        // Bumped: stored gen=1, attach at gen=2 → succeeds, YB now 2.
        let res = store
            .cas_bump_generation_on_attach(tenant, timeline, Generation::new(2))
            .await
            .unwrap();
        assert_eq!(res, CasBumpOutcome::Bumped);
        let got = store.get_timeline(tenant, timeline).await.unwrap().unwrap();
        assert_eq!(got.current_generation, 2);

        // AlreadyAtOurGen: stored gen=2, attach at gen=2 → no-op.
        let res = store
            .cas_bump_generation_on_attach(tenant, timeline, Generation::new(2))
            .await
            .unwrap();
        assert_eq!(res, CasBumpOutcome::AlreadyAtOurGen);
        let got = store.get_timeline(tenant, timeline).await.unwrap().unwrap();
        assert_eq!(got.current_generation, 2);

        // Bumped again: stored gen=2, attach at gen=5 → succeeds, YB now 5.
        // (Mirrors the user's report: SC's tenant_shards.generation
        // moved 1→2→3 over restarts; this is the third restart
        // arriving at the PS, which must heal the lagging YB column.)
        let res = store
            .cas_bump_generation_on_attach(tenant, timeline, Generation::new(5))
            .await
            .unwrap();
        assert_eq!(res, CasBumpOutcome::Bumped);
        let got = store.get_timeline(tenant, timeline).await.unwrap().unwrap();
        assert_eq!(got.current_generation, 5);

        // StaleGeneration: stored gen=5, attach at gen=4 → another
        // PS bumped past us, return live=5 so caller can refuse.
        let res = store
            .cas_bump_generation_on_attach(tenant, timeline, Generation::new(4))
            .await
            .unwrap();
        assert_eq!(res, CasBumpOutcome::StaleGeneration { live_generation: 5 });
        let got = store.get_timeline(tenant, timeline).await.unwrap().unwrap();
        assert_eq!(got.current_generation, 5, "stale CAS must not regress");

        // Row missing: attach against a tenant/timeline pair that
        // doesn't exist → error (wiring bug, not a stale-gen case).
        let other_tenant = rand_tenant();
        let other_timeline = rand_timeline();
        let err = store
            .cas_bump_generation_on_attach(other_tenant, other_timeline, Generation::new(1))
            .await
            .unwrap_err();
        assert!(
            err.to_string().contains("no timeline row"),
            "expected 'no timeline row' err, got: {err}"
        );

        drop_schema(&store, &schema).await;
    }

    #[tokio::test]
    async fn advance_disk_consistent_lsn_gen_gate() {
        use utils::generation::Generation;
        use utils::lsn::Lsn;

        let (store, schema) = make_store("dc_gate").await;
        let tenant = rand_tenant();
        let timeline = rand_timeline();
        let mut row = sample_timeline_row(tenant, timeline);
        row.current_generation = 5;
        row.disk_consistent_lsn = Lsn(0x1000);
        store.insert_timeline_at_creation(&row).await.unwrap();

        // Matching generation → Updated, persisted value advances.
        let res = store
            .advance_disk_consistent_lsn(
                tenant,
                timeline,
                Generation::new(5),
                Lsn(0x2000),
                Some(Lsn(0x1F00)),
            )
            .await
            .unwrap();
        assert_eq!(res, AdvanceDcLsnOutcome::Updated);
        let got = store.get_timeline(tenant, timeline).await.unwrap().unwrap();
        assert_eq!(got.disk_consistent_lsn, Lsn(0x2000));
        assert_eq!(got.prev_record_lsn, Some(Lsn(0x1F00)));

        // Stale generation → StaleGeneration { live = 5 }, value unchanged.
        let res = store
            .advance_disk_consistent_lsn(
                tenant,
                timeline,
                Generation::new(4),
                Lsn(0x3000),
                Some(Lsn(0x2F00)),
            )
            .await
            .unwrap();
        assert_eq!(
            res,
            AdvanceDcLsnOutcome::StaleGeneration { live_generation: 5 }
        );
        let got = store.get_timeline(tenant, timeline).await.unwrap().unwrap();
        assert_eq!(
            got.disk_consistent_lsn,
            Lsn(0x2000),
            "stale-gen UPDATE must not advance"
        );

        // Generation ahead of persisted (we bumped in-memory but the
        // row hasn't been UPSERT'd yet) → also stale: persisted row is
        // the source of truth.
        let res = store
            .advance_disk_consistent_lsn(tenant, timeline, Generation::new(6), Lsn(0x4000), None)
            .await
            .unwrap();
        assert_eq!(
            res,
            AdvanceDcLsnOutcome::StaleGeneration { live_generation: 5 }
        );
        let got = store.get_timeline(tenant, timeline).await.unwrap().unwrap();
        assert_eq!(got.disk_consistent_lsn, Lsn(0x2000));

        drop_schema(&store, &schema).await;
    }

    #[tokio::test]
    async fn bump_generation_is_monotonic() {
        let (store, schema) = make_store("gen").await;
        let tenant = rand_tenant();
        let timeline = rand_timeline();
        store
            .insert_timeline_at_creation(&sample_timeline_row(tenant, timeline))
            .await
            .unwrap();
        for expected in 2..=5 {
            let g = store.bump_generation(tenant, timeline).await.unwrap();
            assert_eq!(g, expected);
        }
        drop_schema(&store, &schema).await;
    }

    /// put_pages must chunk INSERTs internally. Sending one giant
    /// INSERT with N × 11 parameters either hits Postgres's 65535-param
    /// hard cap or tokio-postgres's "value too large to transmit"
    /// internal message limit when the per-row payload is 8 KiB.
    ///
    /// Was hit in the first sustained-load perf test, fixed in
    /// c1f3fd44d (put_pages chunks at 500 rows/INSERT). This test
    /// pushes 1200 rows in a single put_pages call — three full
    /// chunks at the current limit — and verifies all rows land.
    #[tokio::test]
    async fn put_pages_chunks_large_batches() {
        let (store, schema) = make_store("chunk_pages").await;
        let tenant = rand_tenant();
        let timeline = rand_timeline();
        let n: u32 = 1200;
        let payload = vec![0xABu8; 8192];
        let mut rows = Vec::with_capacity(n as usize);
        for blk in 0..n {
            let k = page_key(1663, 5, 16384, 0, blk);
            rows.push(page_row(k, 10, true, true, &payload));
        }
        store.put_pages(tenant, timeline, &rows).await.unwrap();

        // Spot-check: first, middle, last row each read back.
        for blk in [0u32, n / 2, n - 1] {
            let k = page_key(1663, 5, 16384, 0, blk);
            let chain = store
                .read_chain(tenant, timeline, &k, Lsn(100))
                .await
                .unwrap();
            assert_eq!(chain.len(), 1, "blk {blk}");
            assert_eq!(chain[0].payload.as_ref(), payload.as_slice());
        }
        drop_schema(&store, &schema).await;
    }

    /// Same story for put_meta — same root cause, separate chunk
    /// constant. Meta values are small so the chunk is larger (2000),
    /// but the boundary still needs exercising.
    #[tokio::test]
    async fn put_meta_chunks_large_batches() {
        let (store, schema) = make_store("chunk_meta").await;
        let tenant = rand_tenant();
        let timeline = rand_timeline();
        let n: u32 = 3500; // above the 2000-row chunk boundary
        let mut rows = Vec::with_capacity(n as usize);
        for i in 0..n {
            let mut buf = vec![0u8; KEY_SIZE];
            // A non-rel-page meta-namespace key (field1=0x60), unique by i.
            buf[0] = 0x60;
            buf[14..18].copy_from_slice(&i.to_be_bytes());
            rows.push(MetaRow {
                key: Bytes::from(buf),
                lsn: Lsn(10),
                payload: Bytes::from(format!("v{i}")),
            });
        }
        store.put_meta(tenant, timeline, &rows).await.unwrap();

        // Spot-check first, middle, last.
        for i in [0u32, n / 2, n - 1] {
            let mut buf = vec![0u8; KEY_SIZE];
            buf[0] = 0x60;
            buf[14..18].copy_from_slice(&i.to_be_bytes());
            let got = store
                .read_meta(tenant, timeline, &buf, Lsn(100))
                .await
                .unwrap()
                .expect("row");
            assert_eq!(got.payload.as_ref(), format!("v{i}").as_bytes());
        }
        drop_schema(&store, &schema).await;
    }
}
