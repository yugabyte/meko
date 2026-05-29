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
//!                  blknum, is_image, lsn DESC)
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
use pageserver_api::key::{KEY_SIZE, Key};
use remote_storage::{YbStoreConfig, YsqlClient};
use tokio_postgres::types::ToSql;
use utils::id::{TenantId, TimelineId};
use utils::lsn::Lsn;

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

#[derive(Debug, Clone, PartialEq, Eq)]
pub struct TimelineRow {
    pub tenant_id: TenantId,
    pub timeline_id: TimelineId,
    pub last_record_lsn: Lsn,
    pub disk_consistent_lsn: Lsn,
    pub applied_gc_cutoff_lsn: Lsn,
    pub ancestor_timeline_id: Option<TimelineId>,
    pub ancestor_lsn: Option<Lsn>,
    pub current_generation: i32,
    pub pg_version: u32,
}

pub const DEFAULT_SCHEMA: &str = "pageserver";

#[derive(Debug, Clone)]
pub struct PageStoreConfig {
    pub yb: YbStoreConfig,
    pub schema: String,
}

impl PageStoreConfig {
    pub fn new(yb: YbStoreConfig) -> Self {
        Self {
            yb,
            schema: DEFAULT_SCHEMA.to_string(),
        }
    }
}

#[derive(Clone)]
pub struct PageStore {
    client: Arc<YsqlClient>,
    schema: Arc<str>,
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
            key.field1, key.field4, key.field6
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
    pub async fn connect(config: PageStoreConfig, application_name: &str) -> Result<Self> {
        let client = YsqlClient::from_yb_config(&config.yb, application_name)
            .await
            .context("opening PageStore connection")?;
        Ok(Self {
            client: Arc::new(client),
            schema: Arc::from(config.schema),
        })
    }

    pub async fn ensure_schema(&self) -> Result<()> {
        let create_schema = format!("CREATE SCHEMA IF NOT EXISTS \"{}\"", self.schema);
        self.client
            .execute(&create_schema, &[])
            .await
            .with_context(|| format!("creating schema {}", self.schema))?;

        let create_pages = format!(
            "CREATE TABLE IF NOT EXISTS \"{}\".pages (
                tenant_id    TEXT     NOT NULL,
                timeline_id  TEXT     NOT NULL,
                spcnode      BIGINT   NOT NULL,
                dbnode       BIGINT   NOT NULL,
                relnode      BIGINT   NOT NULL,
                forknum      SMALLINT NOT NULL,
                blknum       BIGINT   NOT NULL,
                is_image     BOOLEAN  NOT NULL,
                lsn          BIGINT   NOT NULL,
                will_init    BOOLEAN  NOT NULL,
                payload      BYTEA    NOT NULL,
                PRIMARY KEY ((tenant_id, timeline_id, spcnode, dbnode, relnode, forknum) HASH,
                             blknum, is_image, lsn DESC)
            )",
            self.schema
        );
        self.client
            .execute(&create_pages, &[])
            .await
            .context("creating pages table")?;

        let create_timelines = format!(
            "CREATE TABLE IF NOT EXISTS \"{}\".timelines (
                tenant_id              TEXT    NOT NULL,
                timeline_id            TEXT    NOT NULL,
                last_record_lsn        BIGINT  NOT NULL,
                disk_consistent_lsn    BIGINT  NOT NULL,
                applied_gc_cutoff_lsn  BIGINT  NOT NULL,
                ancestor_timeline_id   TEXT,
                ancestor_lsn           BIGINT,
                current_generation     INT     NOT NULL,
                pg_version             INT     NOT NULL,
                PRIMARY KEY ((tenant_id) HASH, timeline_id)
            )",
            self.schema
        );
        self.client
            .execute(&create_timelines, &[])
            .await
            .context("creating timelines table")?;
        Ok(())
    }

    // ------------------- pages ---------------------------------------------

    pub async fn put_pages(
        &self,
        tenant: TenantId,
        timeline: TimelineId,
        rows: &[PageRow],
    ) -> Result<()> {
        if rows.is_empty() {
            return Ok(());
        }
        let tenant_s = tenant.to_string();
        let timeline_s = timeline.to_string();

        let mut sql = format!(
            "INSERT INTO \"{}\".pages \
             (tenant_id, timeline_id, spcnode, dbnode, relnode, forknum, blknum, \
              lsn, is_image, will_init, payload) VALUES ",
            self.schema
        );
        const COLS: usize = 11;
        let mut params: Vec<Box<dyn ToSql + Sync + Send>> = Vec::with_capacity(rows.len() * COLS);
        for (i, row) in rows.iter().enumerate() {
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
            let (sp, db, rel, fork, blk) = key_columns(&row.key)
                .with_context(|| format!("put_pages row {i}: bad key"))?;
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
        let params_ref: Vec<&(dyn ToSql + Sync)> =
            params.iter().map(|p| p.as_ref() as &(dyn ToSql + Sync)).collect();
        self.client
            .execute(&sql, &params_ref)
            .await
            .with_context(|| {
                format!(
                    "put_pages tenant={tenant} timeline={timeline} batch_rows={}",
                    rows.len()
                )
            })?;
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

        // Fast path: anchor on latest image ≤ target_lsn.
        let fast_sql = format!(
            "WITH anchor AS ( \
                 SELECT lsn AS anchor_lsn \
                 FROM \"{schema}\".pages \
                 WHERE tenant_id=$1 AND timeline_id=$2 \
                   AND spcnode=$3 AND dbnode=$4 AND relnode=$5 \
                   AND forknum=$6 AND blknum=$7 \
                   AND is_image=TRUE AND lsn <= $8 \
                 ORDER BY lsn DESC LIMIT 1 \
             ) \
             SELECT p.spcnode, p.dbnode, p.relnode, p.forknum, p.blknum, \
                    p.lsn, p.is_image, p.will_init, p.payload \
             FROM \"{schema}\".pages p, anchor \
             WHERE p.tenant_id=$1 AND p.timeline_id=$2 \
               AND p.spcnode=$3 AND p.dbnode=$4 AND p.relnode=$5 \
               AND p.forknum=$6 AND p.blknum=$7 \
               AND p.lsn >= anchor.anchor_lsn AND p.lsn <= $8 \
             ORDER BY p.lsn DESC",
            schema = self.schema
        );
        let fast_rows = self
            .client
            .query(
                &fast_sql,
                &[
                    &tenant_s, &timeline_s, &sp, &db, &rel, &fork, &blk, &target,
                ],
            )
            .await
            .context("read_chain fast")?;
        if !fast_rows.is_empty() {
            return fast_rows.into_iter().map(row_to_page).collect();
        }

        // Slow path: no image. Walk the chain LSN-desc until a will_init
        // row is found (image OR will_init delta). The is_image clustering
        // column means a single block's rows are split into two clusters
        // (true, false); the chain seen here lives entirely in the
        // is_image=false cluster.
        let slow_sql = format!(
            "SELECT spcnode, dbnode, relnode, forknum, blknum, \
                    lsn, is_image, will_init, payload \
             FROM \"{}\".pages \
             WHERE tenant_id=$1 AND timeline_id=$2 \
               AND spcnode=$3 AND dbnode=$4 AND relnode=$5 \
               AND forknum=$6 AND blknum=$7 \
               AND lsn <= $8 \
             ORDER BY lsn DESC",
            self.schema
        );
        let slow_rows = self
            .client
            .query(
                &slow_sql,
                &[
                    &tenant_s, &timeline_s, &sp, &db, &rel, &fork, &blk, &target,
                ],
            )
            .await
            .context("read_chain slow")?;
        let mut chain = Vec::with_capacity(slow_rows.len());
        for r in slow_rows {
            let row = row_to_page(r)?;
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
                range.start, range.end,
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
            // `blocks` enumerates distinct blknums in the range that have
            // any row at or below target_lsn. `per_block_image` then
            // does ONE seek per block via LATERAL + LIMIT 1 to find the
            // latest image (or NULL for slow-path blocks). The main
            // SELECT joins back and keeps only rows from the anchor up.
            let sql = format!(
                "WITH blocks AS ( \
                     SELECT DISTINCT blknum FROM \"{schema}\".pages \
                     WHERE tenant_id=$1 AND timeline_id=$2 \
                       AND spcnode=$3 AND dbnode=$4 AND relnode=$5 \
                       AND forknum=$6 \
                       AND blknum >= $7 AND blknum < $8 \
                       AND lsn <= $9 \
                 ), \
                 per_block_image AS ( \
                     SELECT b.blknum, i.image_lsn FROM blocks b \
                     LEFT JOIN LATERAL ( \
                         SELECT lsn AS image_lsn FROM \"{schema}\".pages \
                         WHERE tenant_id=$1 AND timeline_id=$2 \
                           AND spcnode=$3 AND dbnode=$4 AND relnode=$5 \
                           AND forknum=$6 AND blknum = b.blknum \
                           AND is_image=TRUE AND lsn <= $9 \
                         ORDER BY lsn DESC LIMIT 1 \
                     ) i ON TRUE \
                 ) \
                 SELECT p.spcnode, p.dbnode, p.relnode, p.forknum, p.blknum, \
                        p.lsn, p.is_image, p.will_init, p.payload \
                 FROM \"{schema}\".pages p \
                 LEFT JOIN per_block_image bi ON p.blknum = bi.blknum \
                 WHERE p.tenant_id=$1 AND p.timeline_id=$2 \
                   AND p.spcnode=$3 AND p.dbnode=$4 AND p.relnode=$5 \
                   AND p.forknum=$6 \
                   AND p.blknum >= $7 AND p.blknum < $8 \
                   AND p.lsn <= $9 \
                   AND (bi.image_lsn IS NULL OR p.lsn >= bi.image_lsn) \
                 ORDER BY p.blknum, p.lsn DESC",
                schema = self.schema
            );
            self.client
                .query(
                    &sql,
                    &[
                        &tenant_s, &timeline_s, &sp, &db, &rel, &fork, &blk_lo, &blk_hi, &target,
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
            let sql = format!(
                "WITH blocks AS ( \
                     SELECT DISTINCT spcnode, dbnode, relnode, forknum, blknum \
                     FROM \"{schema}\".pages \
                     WHERE tenant_id=$1 AND timeline_id=$2 \
                       AND (spcnode, dbnode, relnode, forknum, blknum) >= ($3, $4, $5, $6, $7) \
                       AND (spcnode, dbnode, relnode, forknum, blknum) <  ($8, $9, $10, $11, $12) \
                       AND lsn <= $13 \
                 ), \
                 per_block_image AS ( \
                     SELECT b.spcnode, b.dbnode, b.relnode, b.forknum, b.blknum, \
                            i.image_lsn \
                     FROM blocks b \
                     LEFT JOIN LATERAL ( \
                         SELECT lsn AS image_lsn FROM \"{schema}\".pages \
                         WHERE tenant_id=$1 AND timeline_id=$2 \
                           AND spcnode=b.spcnode AND dbnode=b.dbnode \
                           AND relnode=b.relnode AND forknum=b.forknum \
                           AND blknum=b.blknum \
                           AND is_image=TRUE AND lsn <= $13 \
                         ORDER BY lsn DESC LIMIT 1 \
                     ) i ON TRUE \
                 ) \
                 SELECT p.spcnode, p.dbnode, p.relnode, p.forknum, p.blknum, \
                        p.lsn, p.is_image, p.will_init, p.payload \
                 FROM \"{schema}\".pages p \
                 LEFT JOIN per_block_image bi \
                     ON p.spcnode = bi.spcnode AND p.dbnode = bi.dbnode \
                    AND p.relnode = bi.relnode AND p.forknum = bi.forknum \
                    AND p.blknum = bi.blknum \
                 WHERE p.tenant_id=$1 AND p.timeline_id=$2 \
                   AND (p.spcnode, p.dbnode, p.relnode, p.forknum, p.blknum) >= ($3, $4, $5, $6, $7) \
                   AND (p.spcnode, p.dbnode, p.relnode, p.forknum, p.blknum) <  ($8, $9, $10, $11, $12) \
                   AND p.lsn <= $13 \
                   AND (bi.image_lsn IS NULL OR p.lsn >= bi.image_lsn) \
                 ORDER BY p.spcnode, p.dbnode, p.relnode, p.forknum, p.blknum, p.lsn DESC",
                schema = self.schema
            );
            self.client
                .query(
                    &sql,
                    &[
                        &tenant_s, &timeline_s, &sp_lo, &db_lo, &rel_lo, &fork_lo, &blk_lo,
                        &sp_hi, &db_hi, &rel_hi, &fork_hi, &blk_hi, &target,
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
        for r in rows {
            let row = row_to_page(r)?;
            let key = row.key.clone();
            let chain = by_key.entry(key).or_default();
            if chain.last().is_some_and(|last| last.will_init) {
                continue;
            }
            chain.push(row);
        }
        Ok(by_key)
    }

    pub async fn delete_all_pages(
        &self,
        tenant: TenantId,
        timeline: TimelineId,
    ) -> Result<()> {
        let sql = format!(
            "DELETE FROM \"{}\".pages WHERE tenant_id = $1 AND timeline_id = $2",
            self.schema
        );
        self.client
            .execute(&sql, &[&tenant.to_string(), &timeline.to_string()])
            .await
            .context("delete_all_pages")?;
        Ok(())
    }

    // ------------------- timelines ----------------------------------------

    pub async fn upsert_timeline(&self, row: &TimelineRow) -> Result<()> {
        let sql = format!(
            "INSERT INTO \"{}\".timelines \
             (tenant_id, timeline_id, last_record_lsn, disk_consistent_lsn, \
              applied_gc_cutoff_lsn, ancestor_timeline_id, ancestor_lsn, \
              current_generation, pg_version) \
             VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9) \
             ON CONFLICT (tenant_id, timeline_id) DO UPDATE SET \
                last_record_lsn       = EXCLUDED.last_record_lsn, \
                disk_consistent_lsn   = EXCLUDED.disk_consistent_lsn, \
                applied_gc_cutoff_lsn = EXCLUDED.applied_gc_cutoff_lsn, \
                ancestor_timeline_id  = EXCLUDED.ancestor_timeline_id, \
                ancestor_lsn          = EXCLUDED.ancestor_lsn, \
                current_generation    = EXCLUDED.current_generation, \
                pg_version            = EXCLUDED.pg_version",
            self.schema
        );
        self.client
            .execute(
                &sql,
                &[
                    &row.tenant_id.to_string(),
                    &row.timeline_id.to_string(),
                    &(row.last_record_lsn.0 as i64),
                    &(row.disk_consistent_lsn.0 as i64),
                    &(row.applied_gc_cutoff_lsn.0 as i64),
                    &row.ancestor_timeline_id.map(|id| id.to_string()),
                    &row.ancestor_lsn.map(|l| l.0 as i64),
                    &row.current_generation,
                    &(row.pg_version as i32),
                ],
            )
            .await
            .context("upsert_timeline")?;
        Ok(())
    }

    pub async fn get_timeline(
        &self,
        tenant: TenantId,
        timeline: TimelineId,
    ) -> Result<Option<TimelineRow>> {
        let sql = format!(
            "SELECT tenant_id, timeline_id, last_record_lsn, disk_consistent_lsn, \
                    applied_gc_cutoff_lsn, ancestor_timeline_id, ancestor_lsn, \
                    current_generation, pg_version \
             FROM \"{}\".timelines \
             WHERE tenant_id = $1 AND timeline_id = $2",
            self.schema
        );
        let row = self
            .client
            .query_opt(&sql, &[&tenant.to_string(), &timeline.to_string()])
            .await
            .context("get_timeline")?;
        row.map(row_to_timeline).transpose()
    }

    pub async fn list_timelines(&self, tenant: TenantId) -> Result<Vec<TimelineRow>> {
        let sql = format!(
            "SELECT tenant_id, timeline_id, last_record_lsn, disk_consistent_lsn, \
                    applied_gc_cutoff_lsn, ancestor_timeline_id, ancestor_lsn, \
                    current_generation, pg_version \
             FROM \"{}\".timelines \
             WHERE tenant_id = $1",
            self.schema
        );
        let rows = self
            .client
            .query(&sql, &[&tenant.to_string()])
            .await
            .context("list_timelines")?;
        rows.into_iter().map(row_to_timeline).collect()
    }

    pub async fn delete_timeline(
        &self,
        tenant: TenantId,
        timeline: TimelineId,
    ) -> Result<()> {
        let sql = format!(
            "DELETE FROM \"{}\".timelines WHERE tenant_id = $1 AND timeline_id = $2",
            self.schema
        );
        self.client
            .execute(&sql, &[&tenant.to_string(), &timeline.to_string()])
            .await
            .context("delete_timeline")?;
        Ok(())
    }

    pub async fn bump_generation(
        &self,
        tenant: TenantId,
        timeline: TimelineId,
    ) -> Result<i32> {
        let sql = format!(
            "UPDATE \"{}\".timelines \
             SET current_generation = current_generation + 1 \
             WHERE tenant_id = $1 AND timeline_id = $2 \
             RETURNING current_generation",
            self.schema
        );
        let row = self
            .client
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
            "SELECT current_generation FROM \"{}\".timelines \
             WHERE tenant_id = $1 AND timeline_id = $2",
            self.schema
        );
        let row = self
            .client
            .query_opt(&sql, &[&tenant.to_string(), &timeline.to_string()])
            .await
            .context("read_generation")?;
        Ok(row.map(|r| r.get::<_, i32>(0)))
    }

    /// Test-only escape hatch — runs raw SQL on the underlying client.
    /// Used by integration tests to create / drop per-test schemas. Not
    /// for production callers.
    #[doc(hidden)]
    pub async fn test_only_execute(&self, sql: &str) -> Result<u64> {
        self.client.execute(sql, &[]).await
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

fn row_to_timeline(r: tokio_postgres::Row) -> Result<TimelineRow> {
    let tenant_s: String = r.try_get(0)?;
    let timeline_s: String = r.try_get(1)?;
    let last_record_lsn: i64 = r.try_get(2)?;
    let disk_consistent_lsn: i64 = r.try_get(3)?;
    let applied_gc_cutoff_lsn: i64 = r.try_get(4)?;
    let ancestor_tl: Option<String> = r.try_get(5)?;
    let ancestor_lsn: Option<i64> = r.try_get(6)?;
    let current_generation: i32 = r.try_get(7)?;
    let pg_version: i32 = r.try_get(8)?;
    Ok(TimelineRow {
        tenant_id: tenant_s.parse().context("parsing tenant_id")?,
        timeline_id: timeline_s.parse().context("parsing timeline_id")?,
        last_record_lsn: Lsn(last_record_lsn as u64),
        disk_consistent_lsn: Lsn(disk_consistent_lsn as u64),
        applied_gc_cutoff_lsn: Lsn(applied_gc_cutoff_lsn as u64),
        ancestor_timeline_id: ancestor_tl
            .map(|s| s.parse::<TimelineId>())
            .transpose()
            .context("parsing ancestor_timeline_id")?,
        ancestor_lsn: ancestor_lsn.map(|l| Lsn(l as u64)),
        current_generation,
        pg_version: pg_version as u32,
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

    async fn make_store(suffix: &str) -> (PageStore, String) {
        let schema = format!("pageserver_test_{suffix}");
        {
            let cfg = PageStoreConfig {
                yb: test_yb_config(),
                schema: schema.clone(),
            };
            let pre = PageStore::connect(cfg, "page_store_test_setup")
                .await
                .expect("connect to local YB");
            let _ = pre
                .test_only_execute(&format!("DROP SCHEMA IF EXISTS \"{schema}\" CASCADE"))
                .await;
        }
        let cfg = PageStoreConfig {
            yb: test_yb_config(),
            schema: schema.clone(),
        };
        let store = PageStore::connect(cfg, "page_store_test")
            .await
            .expect("connect to local YB");
        store.ensure_schema().await.expect("ensure_schema");
        (store, schema)
    }

    async fn drop_schema(store: &PageStore, schema: &str) {
        let _ = store
            .test_only_execute(&format!("DROP SCHEMA IF EXISTS \"{schema}\" CASCADE"))
            .await;
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
            rows.push(page_row(k.clone(), 10 + 10 * i, false, false, &[b'd', i as u8]));
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
        let got = store.read_chain(tenant, timeline, &k, Lsn(100)).await.unwrap();
        assert_eq!(got.len(), 4);
        let lsns: Vec<u64> = got.iter().map(|r| r.lsn.0).collect();
        assert_eq!(lsns, vec![40, 30, 20, 10]);
        assert!(got.last().unwrap().will_init);
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
            rows.push(page_row(k.clone(), 10, true, true, format!("img{blk}").as_bytes()));
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
    async fn scan_range_anchors_on_per_block_image() {
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
                rows.push(page_row(k, 10, true, true, format!("{rel}-{blk}").as_bytes()));
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
            store.read_chain(t1, tl1, &k, Lsn(100)).await.unwrap()[0].payload.as_ref(),
            b"t1tl1"
        );
        assert_eq!(
            store.read_chain(t2, tl1, &k, Lsn(100)).await.unwrap()[0].payload.as_ref(),
            b"t2tl1"
        );
        assert_eq!(
            store.read_chain(t1, tl2, &k, Lsn(100)).await.unwrap()[0].payload.as_ref(),
            b"t1tl2"
        );
        drop_schema(&store, &schema).await;
    }

    #[tokio::test]
    async fn ensure_schema_is_idempotent() {
        let (store, schema) = make_store("idem").await;
        for _ in 0..3 {
            store.ensure_schema().await.expect("second ensure_schema");
        }
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
            .put_pages(tenant, timeline, &[page_row(k.clone(), high, true, true, b"x")])
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
            last_record_lsn: Lsn(100),
            disk_consistent_lsn: Lsn(90),
            applied_gc_cutoff_lsn: Lsn(50),
            ancestor_timeline_id: None,
            ancestor_lsn: None,
            current_generation: 1,
            pg_version: 17,
        }
    }

    #[tokio::test]
    async fn upsert_and_get_timeline_round_trip() {
        let (store, schema) = make_store("tl_rt").await;
        let tenant = rand_tenant();
        let timeline = rand_timeline();
        let row = sample_timeline_row(tenant, timeline);
        store.upsert_timeline(&row).await.unwrap();
        let got = store.get_timeline(tenant, timeline).await.unwrap().unwrap();
        assert_eq!(got, row);
        drop_schema(&store, &schema).await;
    }

    #[tokio::test]
    async fn bump_generation_is_monotonic() {
        let (store, schema) = make_store("gen").await;
        let tenant = rand_tenant();
        let timeline = rand_timeline();
        store
            .upsert_timeline(&sample_timeline_row(tenant, timeline))
            .await
            .unwrap();
        for expected in 2..=5 {
            let g = store.bump_generation(tenant, timeline).await.unwrap();
            assert_eq!(g, expected);
        }
        drop_schema(&store, &schema).await;
    }
}
