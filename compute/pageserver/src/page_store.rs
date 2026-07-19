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
//!     branch_id   TEXT     NOT NULL,
//!     spcnode     BIGINT   NOT NULL,
//!     dbnode      BIGINT   NOT NULL,
//!     relnode     BIGINT   NOT NULL,
//!     forknum     SMALLINT NOT NULL,
//!     blknum      BIGINT   NOT NULL,
//!     is_image    BOOLEAN  NOT NULL,
//!     lsn         BIGINT   NOT NULL,
//!     will_init   BOOLEAN  NOT NULL,
//!     payload     BYTEA    NOT NULL,
//!     PRIMARY KEY ((branch_id, spcnode, dbnode, relnode, forknum) HASH,
//!                  blknum, lsn DESC, is_image DESC)
//! );
//! ```
//!
//! Hash partition `(branch, spcnode, dbnode, relnode, forknum)`
//! means each fork's whole block-set lives on one tablet. Per-block
//! reads are one tablet hash lookup; per-fork range scans (basebackup,
//! image-layer materialisation) are one tablet's range scan.
//!
//! The trailing `is_image` breaks `lsn` ties; it is DESC so the stored
//! order matches the read-path `ORDER BY lsn DESC, is_image DESC`
//! (image before delta at a tie — see [`assert_chain_step`]). Reads
//! anchor the chain on the newest image at-or-below target_lsn
//! (`AND is_image=TRUE ORDER BY lsn DESC LIMIT 1`) and return the
//! deltas above it. Chain length is not a write-time invariant — the
//! background image materialiser creates fresh images for hot keys to
//! keep chains short. Reads remain correct without it, just
//! potentially slower.

use std::collections::HashMap;
use std::ops::Range;
use std::sync::Arc;

use anyhow::{Context, Result, bail};
use bytes::Bytes;
use dashmap::DashMap;
use futures::stream::{StreamExt, TryStreamExt};
use pageserver_api::key::{KEY_SIZE, Key};
use pageserver_api::models::RelSizeMigration;
use pageserver_api::reltag::SlruKind;

use crate::page_server::metadata::Lineage;
use remote_storage::{YbConnConfig, YsqlClient};
// We talk to YugabyteDB through the YB smart-driver fork of
// tokio-postgres; alias it so the type names below (`tokio_postgres::Row`,
// `tokio_postgres::types::ToSql`, etc.) keep working without
// per-callsite churn. The Neon fork of tokio-postgres survives
// elsewhere in the workspace for the walproposer/walreceiver
// replication path.
use utils::id::BranchId;
use utils::lsn::Lsn;
use yb_tokio_postgres as tokio_postgres;
use yb_tokio_postgres::types::ToSql;

/// One row in `pageserver.pages`. `key` is the relation-page
/// `pageserver_api::key::Key`; it must satisfy `Key::is_rel_block_key()`
/// or `put_pages` returns an error. Held as the fixed-size `Key` (not a
/// `Bytes` encoding) so the page cache keys on a `Copy` value — no
/// per-row heap alloc / refcount churn and integer-compare ordering.
#[derive(Debug, Clone, PartialEq, Eq)]
pub struct PageRow {
    pub key: Key,
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

/// Dedup an LSN-descending, image-first-on-ties page chain by LSN.
///
/// The chain MUST already be sorted descending by LSN with images sorting
/// before deltas on a tie (`b.lsn.cmp(&a.lsn).then(b.is_image.cmp(&a.is_image))`).
/// Like `Vec::dedup_by`, the first row of each equal-LSN run is kept and the
/// rest dropped.
///
/// Two same-LSN collisions are legitimate and NOT flagged:
///   1. a materialised image stamped at the LSN of the delta it folded — the
///      image sorts first, is kept, and the now-superseded delta is dropped;
///   2. the same flushed delta listed twice because the cache+YB merge found
///      it in both sources — byte-identical, so dropping the duplicate is
///      lossless.
///
/// What IS flagged: two non-image rows at one LSN whose CONTENT differs — a
/// *non-image* survivor over a row with a different payload. That means we are
/// silently dropping a real, distinct record. It reads back as a hole in the chain
/// and surfaces downstream as a walredo `invalid max offset number` /
/// `failed to add new item` PANIC (issue #22, the "catch me if you can"
/// corruption, which only repros under concurrent load — exactly when a
/// non-atomic chain merge could momentarily collide two distinct rows on one
/// LSN). Trip a counter + rate-limited warn so the silent drop is observable
/// instead of corrupting a materialised image; the dedup still runs (we don't
/// want to amplify by aborting), but now it leaves a fingerprint.
pub(crate) fn dedup_page_chain_by_lsn(chain: &mut Vec<Arc<PageRow>>) {
    // A collision is `chain[i].lsn == chain[i-1].lsn` where the survivor
    // (chain[i-1], which the dedup keeps) is a *delta*, not an image — so
    // chain[i] is a real record about to be silently dropped. Walk pairs so
    // both rows are in hand for the diagnostic dump.
    for i in 1..chain.len() {
        let survivor = &chain[i - 1];
        let dropped = &chain[i];
        if survivor.lsn != dropped.lsn || survivor.is_image {
            continue;
        }
        // Benign and common: the SAME flushed delta is present in BOTH the
        // in-RAM cache (lookup_chain*) and YB (read_chain*), so the cache+YB
        // merge lists it twice. Byte-identical rows → dropping the duplicate
        // loses nothing; do not flag it. Only a genuine content divergence at
        // one LSN is a real record loss (the #22 signature).
        if survivor.will_init == dropped.will_init && survivor.payload == dropped.payload {
            continue;
        }
        crate::metrics::CORRECTNESS_ERRORS
            .with_label_values(&["chain_dedup_dropped_delta"])
            .inc();
        // Dump full detail for the first N collisions (capture the attach
        // burst), then rate-limit. The backtrace pins which read path built
        // the colliding chain (materialiser vs getpage vs scan).
        use std::sync::Mutex;
        use std::sync::atomic::{AtomicU64, Ordering};

        use once_cell::sync::Lazy;
        use utils::rate_limit::RateLimit;
        static DETAILED: AtomicU64 = AtomicU64::new(0);
        static LOGGED: Lazy<Mutex<RateLimit>> =
            Lazy::new(|| Mutex::new(RateLimit::new(std::time::Duration::from_secs(5))));
        let in_burst = DETAILED.fetch_add(1, Ordering::Relaxed) < 64;
        let throttled_ok = LOGGED.try_lock().is_ok_and(|mut l| {
            let mut fired = false;
            l.call(|| fired = true);
            fired
        });
        if in_burst || throttled_ok {
            let hex = |b: &[u8]| b.iter().map(|x| format!("{x:02x}")).collect::<String>();
            let human = match key_columns(&dropped.key) {
                Ok((spc, db, rel, fork, blk)) => {
                    format!("rel={rel} fork={fork} blk={blk} (spc={spc} db={db})")
                }
                Err(e) => format!("<non-page key: {e}>"),
            };
            let row = |r: &Arc<PageRow>| {
                format!(
                    "is_image={} will_init={} pl={} payload_head={}",
                    r.is_image,
                    r.will_init,
                    r.payload.len(),
                    hex(&r.payload[..r.payload.len().min(48)]),
                )
            };
            let bt = std::backtrace::Backtrace::force_capture();
            tracing::warn!(
                "chain dedup dropping a non-superseded row at lsn={lsn}\n\
                 key={human}\n\
                 key_hex={key_hex}\n\
                 survivor (kept):   {survivor_row}\n\
                 dropped  (lost):   {dropped_row}\n\
                 two non-image rows share an LSN, so a real record is being lost \
                 (reads back as a chain gap → walredo invalid-max-offset PANIC, #22; \
                 metric correctness_errors{{chain_dedup_dropped_delta}})\n\
                 backtrace:\n{bt}",
                lsn = dropped.lsn,
                key_hex = {
                    let mut kb = [0u8; KEY_SIZE];
                    dropped.key.write_to_byte_slice(&mut kb);
                    hex(&kb)
                },
                survivor_row = row(survivor),
                dropped_row = row(dropped),
            );
        }
    }
    chain.dedup_by(|a, b| a.lsn == b.lsn);
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

/// One segment of a timeline's ancestry chain, ordered nearest-first
/// (segment 0 is the timeline itself with `cap_lsn = Lsn::MAX`).
///
/// A branch's pre-branch-point pages live in YB under the ANCESTOR's
/// `branch_id`; lineage-aware reads walk these segments so a branch
/// pageserver can serve reads without hosting the ancestor timeline
/// in-process. `cap_lsn` is the highest LSN at which the segment's
/// rows are visible to the descendant: `min` of every `ancestor_lsn`
/// on the path down to (and including) that segment, so caps are
/// non-increasing along the chain.
///
/// The lineage is built once at startup by [`PageStore::load_lineage`]
/// and is immutable for the process lifetime — ancestry never changes
/// for a live timeline (detach-ancestor is unsupported in yb-amp).
#[derive(Debug, Clone, Copy, PartialEq, Eq)]
pub struct LineageSegment {
    pub branch_id: BranchId,
    pub cap_lsn: Lsn,
}

impl LineageSegment {
    /// The trivial single-segment lineage of a timeline with no
    /// ancestor (and the safe default before `load_lineage` runs).
    pub fn single(branch_id: BranchId) -> Vec<LineageSegment> {
        vec![LineageSegment {
            branch_id,
            cap_lsn: Lsn::MAX,
        }]
    }
}

#[derive(Debug, Clone, PartialEq, Eq)]
pub struct TimelineRow {
    pub branch_id: BranchId,
    /// Highest LSN durably present in `pageserver.pages`/`slru_pages`/`meta`.
    /// Advances on every successful `PageWriter::flush`. Drives both
    /// safekeeper WAL truncation (SK keeps WAL ≥ this) and ingest
    /// resume on restart (walreceiver replays from this LSN forward).
    pub consistent_lsn: Lsn,
    pub applied_gc_cutoff_lsn: Lsn,
    pub ancestor_branch_id: Option<BranchId>,
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
    /// always `Default` in yb-amp today (detach_ancestor is
    /// unsupported) but the column is wired up for when the feature
    /// lands.
    pub lineage: Lineage,
}

pub const DEFAULT_SCHEMA: &str = "pageserver";

/// Result of `PageStore::advance_consistent_lsn`. Distinguishes a
/// successful UPDATE from generation-fence failure, the latter being
/// fatal: a higher-generation pageserver has taken over and this one
/// must shut down (its in-flight YB INSERTs will start being rejected
/// by the same generation check, so it can't make forward progress).
#[derive(Debug, Clone, Copy, PartialEq, Eq)]
pub enum AdvanceConsistentLsnOutcome {
    Updated,
    StaleGeneration { live_generation: u32 },
}

/// Result of [`PageStore::update_timeline_state`]. The UPDATE's WHERE
/// clause enforces `current_generation = $caller_gen`, so a stale
/// pageserver — one whose generation has been bumped past since
/// attach — gets `StaleGeneration`. The caller logs the skip; the
/// next flush's `advance_consistent_lsn` is what actually trips
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
    pub yb: YbConnConfig,
    pub schema: String,
    /// Multiplier applied to the per-statement INSERT fan-out
    /// (`put_pages` / `put_slru_pages` / `put_meta`). Plumbed through
    /// from `PageServerConf::scale_factor`; tests default to 1.0.
    pub scale_factor: f64,
}

impl PageStoreConfig {
    pub fn new(yb: YbConnConfig) -> Self {
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
    /// Full-chunk INSERT SQL with the `branch_id` baked in as a
    /// literal, memoized per `branch_id` on first flush and
    /// shared across clones. In production this holds exactly one entry
    /// (a process serves a single branch, #16); keying the map
    /// rather than asserting a single slot also lets the shared-store
    /// test harness drive many branches through one `PageStore`.
    baked: Arc<DashMap<BranchId, Arc<BakedSql>>>,
    /// Thin-RPC lane (`hot_path = "tsclient"`): pure-Rust `yb_tsclient`
    /// handles for the three tables this store owns, speaking the pggate
    /// RPC protocol to a tserver's RPC port. `None` on the default YSQL
    /// path; when set, per-op dispatch tries this lane first and falls
    /// back to YSQL. Populated and health-checked at startup.
    pub tsclient: Option<Arc<TsClientTables>>,
}

/// Opened thin-RPC (`yb_tsclient`) table handles. See
/// [`PageStore::tsclient`] and `architecture/yb-pageserver.md`.
pub struct TsClientTables {
    pub client: Arc<yb_tsclient::TsClient>,
    pub pages: Arc<yb_tsclient::TableHandle>,
    pub slru_pages: Arc<yb_tsclient::TableHandle>,
    pub meta: Arc<yb_tsclient::TableHandle>,
}

/// Exports `yb_tsclient` health events (which its internal retries
/// would otherwise absorb invisibly) into this daemon's metrics.
struct TsClientLaneObserver;

impl yb_tsclient::ClientObserver for TsClientLaneObserver {
    fn on_session_recovery(&self) {
        crate::metrics::YB_STORE_SESSION_RECOVERIES.inc();
    }
    fn on_call_timeout(&self) {
        crate::metrics::YB_STORE_CALL_TIMEOUTS.inc();
    }
}

/// Per-block chain accumulation over a multi-block clustered range
/// scan. Rows arrive in PK order — `blknum ASC`, then `lsn DESC` with
/// equal-LSN ties delta-before-image — so once a block's `will_init`
/// anchor and its tie group are in hand, the rest of that block's
/// (shadowed) history streams past without being buffered.
struct BlockChainCollector<T> {
    chains: Vec<(i64, Vec<T>)>,
    anchor_lsn: Option<i64>,
    skipping: bool,
}

impl<T> BlockChainCollector<T> {
    fn new() -> Self {
        BlockChainCollector {
            chains: Vec::new(),
            anchor_lsn: None,
            skipping: false,
        }
    }

    fn push(&mut self, blk: i64, lsn: i64, will_init: bool, row: T) {
        if self.chains.last().is_none_or(|(b, _)| *b != blk) {
            self.chains.push((blk, Vec::new()));
            self.anchor_lsn = None;
            self.skipping = false;
        }
        if self.skipping {
            return;
        }
        if let Some(anchor) = self.anchor_lsn {
            if lsn < anchor {
                self.skipping = true;
                return;
            }
        }
        if self.anchor_lsn.is_none() && will_init {
            self.anchor_lsn = Some(lsn);
        }
        self.chains.last_mut().expect("pushed above").1.push(row);
    }
}

/// Rows per fetched page on tsclient multi-block range scans.
const DIRECT_SCAN_PAGE_ROWS: u64 = 128;

/// Coerce an integer bind to the DocDB column's width so the tsclient
/// lane sends the exact type DocDB stores (SMALLINT vs INT vs BIGINT).
fn int_bind_pg(col: &yb_tsclient::ColumnMeta, v: i64) -> yb_tsclient::BindValue {
    match col.value_type {
        yb_tsclient::ValueType::I16 => yb_tsclient::BindValue::I16(v as i16),
        yb_tsclient::ValueType::I32 => yb_tsclient::BindValue::I32(v as i32),
        _ => yb_tsclient::BindValue::I64(v),
    }
}

/// Normalize a chain fetched in DocDB storage order into SQL result
/// order and trim it to the served prefix.
///
/// The pages/slru PKs cluster `(lsn DESC, is_image ASC)`, so a tsclient
/// clustered scan yields equal-LSN pairs delta-before-image, while the SQL readers
/// order `is_image DESC` (image first). A stable sort fixes the ties,
/// then the walk mirrors the SQL readers: validate order per step
/// ([`assert_chain_step`]) and stop at the first `will_init` row — an
/// equal-LSN delta sorted after its image, and everything below the
/// anchor, is shadowed and dropped.
fn normalize_chain<T>(
    mut rows: Vec<T>,
    meta: impl Fn(&T) -> (Lsn, bool, bool),
    where_: &str,
) -> Vec<T> {
    rows.sort_by(|a, b| {
        let (a_lsn, a_img, _) = meta(a);
        let (b_lsn, b_img, _) = meta(b);
        b_lsn.cmp(&a_lsn).then(b_img.cmp(&a_img))
    });
    let mut chain = Vec::with_capacity(rows.len());
    let mut order_check: Vec<(Lsn, bool, bool)> = Vec::with_capacity(rows.len());
    for row in rows {
        let m = meta(&row);
        order_check.push(m);
        let anchor = m.2;
        chain.push(row);
        if anchor {
            break;
        }
    }
    // Validate once over the collected prefix — a violation in any prefix
    // shows up here too, so per-iteration checks were needlessly O(N²).
    assert_chain_step(&order_check, where_);
    chain
}

/// Rows per fetched page on tsclient single-key chain reads. Sized so
/// the common case (chain bounded by `image_chain_limit` = 20 plus a
/// little materialiser lag) is one round trip.
const DIRECT_CHAIN_PAGE_ROWS: u64 = 32;

/// Rows per fetched page on tsclient single-key SLRU chain reads. Taller
/// than [`DIRECT_CHAIN_PAGE_ROWS`]: SLRU deltas are tiny and
/// `image_chain_limit_slru` allows chains up to ~1000 rows, so this sizes
/// the common case to one round trip.
const DIRECT_SLRU_CHAIN_PAGE_ROWS: u64 = 1024;

/// Streaming anchor-stop accumulator for a paged single-key chain fetch
/// in clustered order (LSN-descending). Accumulation stops once a
/// `will_init` anchor AND its complete equal-LSN tie group are in hand
/// (a row with a lower LSN, or range exhaustion, proves the group
/// complete); rows below the anchor within the last page are discarded
/// later by [`normalize_chain`].
struct ChainFetchState {
    raw: Vec<yb_tsclient::Row>,
    anchor_lsn: Option<i64>,
    lsn_idx: usize,
    will_init_idx: usize,
}

impl ChainFetchState {
    fn new(lsn_idx: usize, will_init_idx: usize) -> Self {
        ChainFetchState {
            raw: Vec::new(),
            anchor_lsn: None,
            lsn_idx,
            will_init_idx,
        }
    }

    /// Push one row; returns `true` when the chain is complete and the
    /// fetch can stop (the pushed row itself is below the anchor and
    /// dropped).
    fn push(&mut self, row: yb_tsclient::Row) -> Result<bool> {
        let lsn = row[self.lsn_idx].as_i64()?;
        if let Some(anchor) = self.anchor_lsn {
            if lsn < anchor {
                return Ok(true);
            }
        }
        if self.anchor_lsn.is_none() && row[self.will_init_idx].as_bool()? {
            self.anchor_lsn = Some(lsn);
        }
        self.raw.push(row);
        Ok(false)
    }
}

/// Paged single-key chain fetch via the tsclient lane
/// ([`ChainFetchState`] holds the anchor-stop logic). Runs through
/// [`yb_tsclient::TsClient::read_scan`], which pages the range on one
/// snapshot and, on a read-restart (`restart_read_time` or a mid-scan
/// pin loss), restarts the whole scan on a fresh read point — a paged
/// continuation can't heal one in place (see `libs/yb_tsclient/DESIGN.md`).
async fn fetch_chain_rows_tsclient(
    client: &yb_tsclient::TsClient,
    table: &Arc<yb_tsclient::TableHandle>,
    read: yb_tsclient::RangeRead,
    lsn_idx: usize,
    will_init_idx: usize,
) -> Result<Vec<yb_tsclient::Row>> {
    let state = client
        .read_scan(
            table,
            &read,
            || ChainFetchState::new(lsn_idx, will_init_idx),
            |state, rows| -> anyhow::Result<bool> {
                for row in rows {
                    if state.push(row)? {
                        return Ok(true);
                    }
                }
                Ok(false)
            },
        )
        .await?;
    Ok(state.raw)
}

/// Decode the raw rows of a single-key chain fetch (targets
/// `[.., lsn, is_image, will_init, payload]` starting at `off`) into
/// [`PageRow`]s for `key`, normalize storage order into SQL result
/// order, and trim at the anchor. Used by the tsclient lane.
fn assemble_page_chain(
    key: Key,
    raw: Vec<yb_tsclient::Row>,
    off: usize,
    where_: &str,
) -> Result<Vec<Arc<PageRow>>> {
    let rows = raw
        .into_iter()
        .map(|r| {
            Ok(PageRow {
                key,
                lsn: Lsn(r[off].as_i64()? as u64),
                is_image: r[off + 1].as_bool()?,
                will_init: r[off + 2].as_bool()?,
                payload: r[off + 3].as_bytes()?.clone(),
            })
        })
        .collect::<Result<Vec<_>>>()?;
    Ok(
        normalize_chain(rows, |r| (r.lsn, r.is_image, r.will_init), where_)
            .into_iter()
            .map(Arc::new)
            .collect(),
    )
}

/// [`assemble_page_chain`] for SLRU rows.
fn assemble_slru_chain(
    kind: SlruKind,
    segno: u32,
    blknum: u32,
    raw: Vec<yb_tsclient::Row>,
    off: usize,
    where_: &str,
) -> Result<Vec<SlruPageRow>> {
    let rows = raw
        .into_iter()
        .map(|r| {
            Ok(SlruPageRow {
                kind,
                segno,
                blknum,
                lsn: Lsn(r[off].as_i64()? as u64),
                is_image: r[off + 1].as_bool()?,
                will_init: r[off + 2].as_bool()?,
                payload: r[off + 3].as_bytes()?.clone(),
            })
        })
        .collect::<Result<Vec<_>>>()?;
    Ok(normalize_chain(
        rows,
        |r| (r.lsn, r.is_image, r.will_init),
        where_,
    ))
}

/// Feed one fetched page of a multi-block scan (targets
/// `[blknum, lsn, is_image, will_init, payload]`) into the per-block
/// collector. Used by the tsclient scan loops.
fn feed_scan_rows(
    collector: &mut BlockChainCollector<yb_tsclient::Row>,
    rows: Vec<yb_tsclient::Row>,
) -> Result<()> {
    for row in rows {
        let blk = row[0].as_i64()?;
        let lsn = row[1].as_i64()?;
        let will_init = row[3].as_bool()?;
        collector.push(blk, lsn, will_init, row);
    }
    Ok(())
}

/// Assemble a one-fork multi-block scan's collected per-block chains
/// into the keyed map [`PageStore::scan_range`] returns. Used by the
/// tsclient lane.
fn assemble_page_scan(
    sp: i64,
    db: i64,
    rel: i64,
    fork: i16,
    collector: BlockChainCollector<yb_tsclient::Row>,
    where_: &str,
) -> Result<HashMap<Key, Vec<Arc<PageRow>>>> {
    let mut by_key = HashMap::with_capacity(collector.chains.len());
    for (blk, raw) in collector.chains {
        let key = Key::from_slice(&columns_to_key_bytes(sp, db, rel, fork, blk));
        let chain = assemble_page_chain(key, raw, 1, where_)?;
        if !chain.is_empty() {
            by_key.insert(key, chain);
        }
    }
    Ok(by_key)
}

/// Decode a `meta` row fetched with targets `[lsn, payload]` for `key`.
/// Used by the tsclient lane.
fn decode_meta_row(key: &[u8], r: &yb_tsclient::Row) -> Result<MetaRow> {
    Ok(MetaRow {
        key: Bytes::copy_from_slice(key),
        lsn: Lsn(r[0].as_i64()? as u64),
        payload: r[1].as_bytes()?.clone(),
    })
}

/// [`assemble_page_scan`] for SLRU rows, keyed by blknum.
fn assemble_slru_scan(
    kind: SlruKind,
    segno: u32,
    collector: BlockChainCollector<yb_tsclient::Row>,
    where_: &str,
) -> Result<HashMap<u32, Vec<SlruPageRow>>> {
    let mut by_block = HashMap::with_capacity(collector.chains.len());
    for (blk, raw) in collector.chains {
        let chain = assemble_slru_chain(kind, segno, blk as u32, raw, 1, where_)?;
        if !chain.is_empty() {
            by_block.insert(blk as u32, chain);
        }
    }
    Ok(by_block)
}

impl TsClientTables {
    /// Connects the thin-RPC (`yb_tsclient`) client and opens the
    /// store's tables, validating tserver-RPC connectivity, table
    /// existence, and column layout in one shot — the startup
    /// healthcheck for `hot_path = "tsclient"` (fail-fast).
    async fn connect(
        yb: &YbConnConfig,
        schema: &str,
        ysql_client: &YsqlClient,
        write_sessions: usize,
    ) -> Result<Arc<TsClientTables>> {
        let (db_oid, oids) =
            ysql_client::lookup_table_oids(ysql_client, schema, &["pages", "slru_pages", "meta"])
                .await?;
        let client = yb_tsclient::TsClient::connect(yb_tsclient::TsClientConfig {
            tserver_addrs: yb.effective_tserver_rpc_addrs(),
            write_sessions,
            ..Default::default()
        })
        .await
        .context("connecting the thin-RPC (tsclient) client")?;
        client.set_observer(Arc::new(TsClientLaneObserver));
        let open = async |table: &str| {
            client
                .open_table(db_oid, oids[table])
                .await
                .with_context(|| format!("opening {schema}.{table} via the tsclient lane"))
        };
        let tables = Arc::new(TsClientTables {
            pages: open("pages").await?,
            slru_pages: open("slru_pages").await?,
            meta: open("meta").await?,
            client,
        });
        tracing::info!(
            "thin-RPC (tsclient) hot path ready: {schema}.pages/slru_pages/meta opened \
             (pages table_id {}) on {}",
            tables.pages.table_id,
            tables.client.connected_addr(),
        );
        Ok(tables)
    }
}

/// Decompose an 18-byte relation-page Key into the typed columns.
/// Returns `Err` if the bytes don't parse as `field1 = 0` (the
/// relation-page namespace).
fn key_columns(key: &Key) -> Result<(i64, i64, i64, i16, i64)> {
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

/// Rows per multi-row INSERT in `put_pages` / `put_slru_pages`.
/// PostgreSQL caps a query at 65535 parameters; with `branch_id`
/// baked into the SQL as a literal (this is a single-branch
/// pageserver — see #16), the largest fan-out
/// is `pages` at 9 params/row → 500×9 = 4500 params, well under.
/// Chunks dispatch in parallel — tokio-postgres pipelines them on the
/// shared connection so YB absorbs them concurrently.
const ROWS_PER_INSERT: usize = 500;

const PAGES_COLUMNS: &str =
    "branch_id, spcnode, dbnode, relnode, forknum, blknum, lsn, is_image, will_init, payload";
/// Per-row bound columns for `pages` (everything after the baked
/// `branch_id` literal): spcnode, dbnode, relnode,
/// forknum, blknum, lsn, is_image, will_init, payload.
const PAGES_PER_ROW: usize = 9;

const SLRU_COLUMNS: &str = "branch_id, kind, segno, blknum, is_image, lsn, will_init, payload";
/// Per-row bound columns for `slru_pages`: kind, segno, blknum,
/// is_image, lsn, will_init, payload.
const SLRU_PER_ROW: usize = 7;

/// Build a multi-row `INSERT ... VALUES`. `branch_id` is baked
/// into every row tuple as a quoted literal — it is fixed for the
/// process lifetime (single-branch pageserver, #16), so
/// there is no point round-tripping it as a bind param. It renders as
/// fixed-width hex (`BranchId` `Display`), so the literal
/// is injection-safe. The `per_row` per-row columns bind `$1..`.
///
/// The result is a pure function of its inputs and stable across
/// flushes, so the common full-chunk shape is built once (memoized on
/// the `PageStore`) and `ysql_client` caches the prepared statement by
/// this exact text.
fn build_insert_sql(
    schema: &str,
    table: &str,
    columns: &str,
    branch_id: BranchId,
    per_row: usize,
    n_rows: usize,
) -> String {
    use std::fmt::Write as _;
    // ~6 chars per "$NNNN," placeholder + the baked id literal.
    let mut sql = String::with_capacity(64 + columns.len() + n_rows * (per_row * 6 + 80));
    write!(sql, "INSERT INTO \"{schema}\".{table} ({columns}) VALUES ").unwrap();
    for i in 0..n_rows {
        if i > 0 {
            sql.push(',');
        }
        write!(sql, "('{branch_id}'").unwrap();
        let base = i * per_row + 1;
        for j in 0..per_row {
            write!(sql, ",${}", base + j).unwrap();
        }
        sql.push(')');
    }
    sql
}

/// Single-key chain read (`read_chain`). `$1`=branch_id;
/// `$2..$7` = spcnode, dbnode, relnode, forknum, blknum, target_lsn.
fn build_read_chain_sql(schema: &str) -> String {
    format!(
        "SELECT p.spcnode, p.dbnode, p.relnode, p.forknum, p.blknum, \
                p.lsn, p.is_image, p.will_init, p.payload \
         FROM \"{schema}\".pages p \
         WHERE p.branch_id=$1 \
           AND p.spcnode=$2 AND p.dbnode=$3 AND p.relnode=$4 \
           AND p.forknum=$5 AND p.blknum=$6 \
           AND p.lsn <= $7 \
           AND p.lsn >= COALESCE(( \
                 SELECT a.lsn FROM \"{schema}\".pages a \
                 WHERE a.branch_id=$1 \
                   AND a.spcnode=$2 AND a.dbnode=$3 AND a.relnode=$4 \
                   AND a.forknum=$5 AND a.blknum=$6 \
                   AND a.is_image=TRUE AND a.lsn <= $7 \
                 ORDER BY a.lsn DESC LIMIT 1), 0) \
         ORDER BY p.lsn DESC, p.is_image DESC"
    )
}

/// Shared body of the one-fork block-chain scan, used by both the contiguous
/// range scan ([`build_scan_one_fork_sql`]) and the scattered set scan
/// ([`build_scan_one_fork_scattered_sql`]). For each `b(blknum)` row the
/// `driver` produces, a CROSS JOIN LATERAL resolves that block's chain: rows
/// from the newest image at-or-below `lsn` onward, newest-first (the per-block
/// InitPlan anchor is a pushable lower bound, so both ends of the lsn range
/// push into the DocDB scan). Results are ordered by blknum then lsn.
///
/// - `prefix`: any leading `WITH` clause the driver needs (empty for the set).
/// - `driver`: the `FROM` source yielding `b(blknum)` — a `blocks` CTE for the
///   range, `unnest($6::bigint[])` for the set.
/// - `lsn`: the placeholder holding target_lsn (`$8` for range, `$7` for set).
fn build_scan_one_fork_lateral_sql(schema: &str, prefix: &str, driver: &str, lsn: &str) -> String {
    format!(
        "{prefix}\
         SELECT p.spcnode, p.dbnode, p.relnode, p.forknum, p.blknum, \
                p.lsn, p.is_image, p.will_init, p.payload \
         FROM {driver} \
         CROSS JOIN LATERAL ( \
             SELECT p.spcnode, p.dbnode, p.relnode, p.forknum, p.blknum, \
                    p.lsn, p.is_image, p.will_init, p.payload \
             FROM \"{schema}\".pages p \
             WHERE p.branch_id=$1 \
               AND p.spcnode=$2 AND p.dbnode=$3 AND p.relnode=$4 \
               AND p.forknum=$5 AND p.blknum = b.blknum \
               AND p.lsn <= {lsn} \
               AND p.lsn >= COALESCE(( \
                     SELECT a.lsn FROM \"{schema}\".pages a \
                     WHERE a.branch_id=$1 \
                       AND a.spcnode=$2 AND a.dbnode=$3 AND a.relnode=$4 \
                       AND a.forknum=$5 AND a.blknum = b.blknum \
                       AND a.is_image=TRUE AND a.lsn <= {lsn} \
                     ORDER BY a.lsn DESC LIMIT 1), 0) \
             ORDER BY p.lsn DESC, p.is_image DESC \
         ) p \
         ORDER BY p.blknum, p.lsn DESC, p.is_image DESC"
    )
}

/// One-fork range scan (`scan_range`). `$1`=branch_id;
/// `$2..$8` = spcnode, dbnode, relnode, forknum, blknum_lo, blknum_hi,
/// target_lsn. Driver: the DISTINCT blknums that actually exist in `[$6, $7)`
/// (a Distinct Index Scan ⇒ one row per block) feeding the shared LATERAL.
fn build_scan_one_fork_sql(schema: &str) -> String {
    let prefix = format!(
        "WITH blocks AS ( \
             SELECT DISTINCT blknum FROM \"{schema}\".pages \
             WHERE branch_id=$1 \
               AND spcnode=$2 AND dbnode=$3 AND relnode=$4 \
               AND forknum=$5 \
               AND blknum >= $6 AND blknum < $7 \
               AND lsn <= $8 \
         ) "
    );
    build_scan_one_fork_lateral_sql(schema, &prefix, "blocks b", "$8")
}

/// Multi-key ("scattered") one-fork scan (`scan_scattered_set`). Same LATERAL
/// body as [`build_scan_one_fork_sql`], but driven directly from the
/// caller-supplied blknum array via `unnest` rather than a contiguous `[lo, hi)`
/// range discovered by a `DISTINCT` pages scan. `$1`=branch_id; `$2..$5`=
/// spcnode, dbnode, relnode, forknum; `$6`=blknum bigint[]; `$7`=target_lsn.
///
/// We already know the blocks, so we do NOT re-scan pages with a
/// `SELECT DISTINCT ... blknum = ANY(...)` (that would bet on YB skip-scanning
/// the `= ANY` on the range column). `unnest` makes every tablet touch below a
/// single-blknum point seek (hash `=` + blknum `=`). A blknum absent from the
/// table yields an empty LATERAL → CROSS JOIN drops it (no row), same as the
/// old DISTINCT-CTE excluding it.
fn build_scan_one_fork_scattered_sql(schema: &str) -> String {
    build_scan_one_fork_lateral_sql(schema, "", "unnest($6::bigint[]) AS b(blknum)", "$7")
}

/// Cross-fork range scan (`scan_range`). `$1`=branch_id;
/// `$2..$12` = (spc,db,rel,fork,blk)_lo, (spc,db,rel,fork,blk)_hi,
/// target_lsn.
fn build_scan_cross_fork_sql(schema: &str) -> String {
    format!(
        "WITH blocks AS ( \
             SELECT DISTINCT spcnode, dbnode, relnode, forknum, blknum \
             FROM \"{schema}\".pages \
             WHERE branch_id=$1 \
               AND (spcnode, dbnode, relnode, forknum, blknum) >= ($2, $3, $4, $5, $6) \
               AND (spcnode, dbnode, relnode, forknum, blknum) <  ($7, $8, $9, $10, $11) \
               AND lsn <= $12 \
         ) \
         SELECT p.spcnode, p.dbnode, p.relnode, p.forknum, p.blknum, \
                p.lsn, p.is_image, p.will_init, p.payload \
         FROM blocks b \
         CROSS JOIN LATERAL ( \
             SELECT p.spcnode, p.dbnode, p.relnode, p.forknum, p.blknum, \
                    p.lsn, p.is_image, p.will_init, p.payload \
             FROM \"{schema}\".pages p \
             WHERE p.branch_id=$1 \
               AND p.spcnode = b.spcnode AND p.dbnode = b.dbnode \
               AND p.relnode = b.relnode AND p.forknum = b.forknum \
               AND p.blknum = b.blknum \
               AND p.lsn <= $12 \
               AND p.lsn >= COALESCE(( \
                     SELECT a.lsn FROM \"{schema}\".pages a \
                     WHERE a.branch_id=$1 \
                       AND a.spcnode = b.spcnode AND a.dbnode = b.dbnode \
                       AND a.relnode = b.relnode AND a.forknum = b.forknum \
                       AND a.blknum = b.blknum \
                       AND a.is_image=TRUE AND a.lsn <= $12 \
                     ORDER BY a.lsn DESC LIMIT 1), 0) \
             ORDER BY p.lsn DESC, p.is_image DESC \
         ) p \
         ORDER BY p.spcnode, p.dbnode, p.relnode, p.forknum, p.blknum, p.lsn DESC, p.is_image DESC"
    )
}

/// Bucket flat SQL rows into per-key chains. Rows arrive LSN-descending per
/// block (per the scan `ORDER BY`); each chain is truncated at the first
/// `will_init` row, since older rows are shadowed by that anchor (the image
/// row for blocks with an image, else the latest will_init delta). `label`
/// names the caller for the ordering assertion. Shared tail of
/// [`PageStore::scan_range`] and [`PageStore::scan_scattered_set`].
fn group_page_rows_by_key(
    rows: Vec<tokio_postgres::Row>,
    label: &str,
) -> Result<HashMap<Key, Vec<Arc<PageRow>>>> {
    let mut by_key: HashMap<Key, Vec<Arc<PageRow>>> = HashMap::new();
    let mut order_check: HashMap<Key, Vec<(Lsn, bool, bool)>> = HashMap::new();
    for r in rows {
        let row = row_to_page(r)?;
        let key = row.key;
        order_check
            .entry(key)
            .or_default()
            .push((row.lsn, row.is_image, row.will_init));
        assert_chain_step(order_check.get(&key).unwrap(), label);
        let chain = by_key.entry(key).or_default();
        if chain.last().is_some_and(|last| last.will_init) {
            continue;
        }
        chain.push(Arc::new(row));
    }
    Ok(by_key)
}

/// What a lineage scan resolves at each ancestor segment: a contiguous key
/// range ([`PageStore::scan_range`]) or a scattered set of same-fork keys
/// ([`PageStore::scan_scattered_set`]). Lets [`PageStore::scan_lineage`] share
/// the ancestor-walk skeleton across both callers.
enum LineageScanTarget<'a> {
    Range(Range<Key>),
    Set(&'a [Key]),
}

/// Memoized full-chunk INSERT SQL for a `branch_id`, with the
/// id baked in as a literal. Writes only ever target the primary
/// timeline, so baking is safe here (unlike the read path, where lineage
/// spans ancestor timelines). Built once on first flush, keyed by
/// `branch_id` in `PageStore::baked`.
struct BakedSql {
    /// Full-chunk `pages` / `slru_pages` INSERTs (see `build_insert_sql`).
    pages_full: Arc<str>,
    slru_full: Arc<str>,
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
            &config.yb,
            application_name,
            pool_size,
            set_fast_path_upsert,
        )
        .await
        .context("opening PageStore connection")?;
        let tsclient = match config.yb.hot_path {
            ysql_client::HotPath::Ysql => None,
            ysql_client::HotPath::TsClient => Some(
                // Write sessions match put_pages' chunk fan-out so
                // concurrent bulk-flush chunks each get their own
                // connection + server session (see
                // `TsClientConfig::write_sessions`).
                TsClientTables::connect(
                    &config.yb,
                    &config.schema,
                    &ysql_client,
                    pageserver_api::config::scale_count(4, config.scale_factor),
                )
                .await
                .context("thin-RPC hot path init (hot_path = \"tsclient\")")?,
            ),
        };
        Ok(Self {
            ysql_client: Arc::new(ysql_client),
            schema: Arc::from(config.schema),
            scale_factor: config.scale_factor,
            baked: Arc::new(DashMap::new()),
            tsclient,
        })
    }

    /// Memoized per-`branch_id` SQL with the id baked in as a
    /// literal. Built once per `branch_id` on first use; later
    /// calls reuse the cached `Arc`. In production the map holds a single
    /// entry (#16).
    fn baked_sql(&self, branch_id: BranchId) -> Arc<BakedSql> {
        Arc::clone(
            self.baked
                .entry(branch_id)
                .or_insert_with(|| {
                    Arc::new(BakedSql {
                        pages_full: Arc::from(build_insert_sql(
                            &self.schema,
                            "pages",
                            PAGES_COLUMNS,
                            branch_id,
                            PAGES_PER_ROW,
                            ROWS_PER_INSERT,
                        )),
                        slru_full: Arc::from(build_insert_sql(
                            &self.schema,
                            "slru_pages",
                            SLRU_COLUMNS,
                            branch_id,
                            SLRU_PER_ROW,
                            ROWS_PER_INSERT,
                        )),
                    })
                })
                .value(),
        )
    }

    // ------------------- pages ---------------------------------------------

    pub async fn put_pages(&self, branch_id: BranchId, rows: &[Arc<PageRow>]) -> Result<()> {
        if let Some(p) = &self.tsclient {
            let started = std::time::Instant::now();
            self.put_pages_tsclient(p, branch_id, rows).await?;
            crate::metrics::YB_STORE_OP_SECONDS
                .with_label_values(&["put_pages", "tsclient"])
                .observe(started.elapsed().as_secs_f64());
            return Ok(());
        }

        // Chunks dispatch in parallel — tokio-postgres pipelines them
        // on the shared connection so YB can absorb them concurrently.
        let started = std::time::Instant::now();
        let parallelism = pageserver_api::config::scale_count(4, self.scale_factor);
        let pages_full = Arc::clone(&self.baked_sql(branch_id).pages_full);

        let mut futs: Vec<futures::future::BoxFuture<'_, Result<()>>> = Vec::new();
        for chunk in rows.chunks(ROWS_PER_INSERT) {
            if chunk.is_empty() {
                continue;
            }
            let n = chunk.len();
            // Decode + validate keys up front: a bad key fails the whole
            // flush before any chunk is sent (fail-fast), and the per-row
            // scalar columns land in owned storage we can bind by
            // reference — no per-cell `Box<dyn ToSql>`.
            let mut spc = Vec::with_capacity(n);
            let mut dbn = Vec::with_capacity(n);
            let mut rel = Vec::with_capacity(n);
            let mut fork = Vec::with_capacity(n);
            let mut blk = Vec::with_capacity(n);
            let mut lsn = Vec::with_capacity(n);
            for (i, row) in chunk.iter().enumerate() {
                let (s, d, r, f, b) =
                    key_columns(&row.key).with_context(|| format!("put_pages row {i}: bad key"))?;
                spc.push(s);
                dbn.push(d);
                rel.push(r);
                fork.push(f);
                blk.push(b);
                lsn.push(row.lsn.0 as i64);
            }
            // Cheap Arc-clone per row keeps its `payload` alive so we can
            // bind `&[u8]` slices below without copying the page bytes.
            let arcs: Vec<Arc<PageRow>> = chunk.to_vec();
            // Common full chunk reuses the memoized SQL; the rare short
            // final chunk rebuilds (one alloc, ids baked in).
            let sql: Arc<str> = if n == ROWS_PER_INSERT {
                Arc::clone(&pages_full)
            } else {
                Arc::from(build_insert_sql(
                    &self.schema,
                    "pages",
                    PAGES_COLUMNS,
                    branch_id,
                    PAGES_PER_ROW,
                    n,
                ))
            };
            let chunk_rows = n;
            let ysql_client = Arc::clone(&self.ysql_client);
            // Per-traced-row pre-INSERT event for the env-gated trace,
            // plus first/last (key, lsn) capture for the CRITICAL log if
            // YB's affected-row count comes back short.
            let traced_in_chunk: Vec<(Key, Lsn)> = chunk
                .iter()
                .filter(|row| pageserver_api::write_trace::is_traced(&row.key))
                .map(|row| (row.key, row.lsn))
                .collect();
            for (k, l) in &traced_in_chunk {
                pageserver_api::write_trace::trace_stage("put_pages_in", k, *l);
            }
            let chunk_first = chunk.first().map(|r| (r.key, r.lsn));
            let chunk_last = chunk.last().map(|r| (r.key, r.lsn));
            futs.push(Box::pin(async move {
                let payloads: Vec<&[u8]> = arcs.iter().map(|r| r.payload.as_ref()).collect();
                let mut params: Vec<&(dyn ToSql + Sync)> = Vec::with_capacity(n * PAGES_PER_ROW);
                for i in 0..n {
                    params.push(&spc[i]);
                    params.push(&dbn[i]);
                    params.push(&rel[i]);
                    params.push(&fork[i]);
                    params.push(&blk[i]);
                    params.push(&lsn[i]);
                    params.push(&arcs[i].is_image);
                    params.push(&arcs[i].will_init);
                    params.push(&payloads[i]);
                }
                let affected = ysql_client
                    .execute(&sql, &params)
                    .await
                    .with_context(|| {
                        format!("put_pages branch={branch_id} chunk_rows={chunk_rows}")
                    })?;
                // Hard invariant: every row in `chunk` is a distinct
                // INSERT target — same (branch, key, lsn,
                // is_image) PK can only appear once per chunk because
                // walreceiver's LSNs are monotonic per key and
                // materialiser writes don't share an LSN with another
                // image. So the YB `affected` count must equal
                // `chunk_rows`. With `yb_enable_upsert_mode=true` an
                // idempotent rewrite of the same PK still counts as
                // affected. If `affected < chunk_rows`, YB silently
                // dropped row(s) — log a CRITICAL with the chunk's
                // boundary keys so an operator can grep.
                if (affected as usize) < chunk_rows {
                    tracing::error!(
                        target: "yb_ps_trace",
                        branch = %branch_id,
                        chunk_rows,
                        affected,
                        first = ?chunk_first,
                        last = ?chunk_last,
                        "CRITICAL: put_pages chunk affected fewer rows than it sent — YB silently dropped row(s)"
                    );
                }
                for (k, l) in &traced_in_chunk {
                    pageserver_api::write_trace::trace_stage("put_pages_ok", k, *l);
                }
                Ok(())
            }));
        }

        futures::stream::iter(futs)
            .buffer_unordered(parallelism)
            .try_collect::<Vec<_>>()
            .await?;
        crate::metrics::YB_STORE_OP_SECONDS
            .with_label_values(&["put_pages", "ysql"])
            .observe(started.elapsed().as_secs_f64());
        Ok(())
    }

    /// Thin-RPC variant of [`Self::put_pages`]: chunks become
    /// PGSQL_UPSERT batches (semantically the INSERT under
    /// `yb_enable_upsert_mode` the YSQL lane uses), fanned out with the
    /// same parallelism. The SQL path's affected-count CRITICAL check
    /// has no analog here — each chunk is one Perform of PGSQL_UPSERT
    /// ops whose per-op response statuses the client verifies, so a
    /// dropped row surfaces as a batch error (propagated to the caller)
    /// rather than a short affected count. A chunk can span forks
    /// (multiple tablets → multiple write batches), so partial
    /// application is possible on failure; consistent_lsn only advances
    /// after the whole flush succeeds, and the retried full-flush replay
    /// is byte-identical by the idempotency contract.
    async fn put_pages_tsclient(
        &self,
        p: &TsClientTables,
        branch_id: BranchId,
        rows: &[Arc<PageRow>],
    ) -> Result<()> {
        use yb_tsclient::{BindValue, UpsertRow};
        let pages = &p.pages;
        // Column metadata is constant for the whole flush — resolve once
        // instead of per row (ROWS_PER_INSERT * chunks lookups otherwise).
        let col_spc = pages.col("spcnode")?;
        let col_db = pages.col("dbnode")?;
        let col_rel = pages.col("relnode")?;
        let col_fork = pages.col("forknum")?;
        let parallelism = pageserver_api::config::scale_count(4, self.scale_factor);
        let branch_s = branch_id.to_string();

        let mut futs: Vec<futures::future::BoxFuture<'_, Result<()>>> = Vec::new();
        for chunk in rows.chunks(ROWS_PER_INSERT) {
            if chunk.is_empty() {
                continue;
            }
            let mut batch = Vec::with_capacity(chunk.len());
            for (i, row) in chunk.iter().enumerate() {
                let (sp, db, rel, fork, blk) =
                    key_columns(&row.key).with_context(|| format!("put_pages row {i}: bad key"))?;
                batch.push(UpsertRow::new(
                    pages,
                    vec![
                        BindValue::Text(branch_s.clone()),
                        int_bind_pg(col_spc, sp),
                        int_bind_pg(col_db, db),
                        int_bind_pg(col_rel, rel),
                        int_bind_pg(col_fork, fork as i64),
                        BindValue::I64(blk),
                        BindValue::I64(row.lsn.0 as i64),
                        BindValue::Bool(row.is_image),
                    ],
                    &[
                        ("will_init", BindValue::Bool(row.will_init)),
                        ("payload", BindValue::Bytes(row.payload.clone())),
                    ],
                )?);
            }
            let traced_in_chunk: Vec<(Key, Lsn)> = chunk
                .iter()
                .filter(|row| pageserver_api::write_trace::is_traced(&row.key))
                .map(|row| (row.key, row.lsn))
                .collect();
            for (k, l) in &traced_in_chunk {
                pageserver_api::write_trace::trace_stage("put_pages_in", k, *l);
            }
            let client = Arc::clone(&p.client);
            let table = Arc::clone(pages);
            let chunk_rows = batch.len();
            futs.push(Box::pin(async move {
                // Full retry budget: each attempt round-robins to the next
                // tserver write session, so a stalled/failed node costs
                // ~one attempt before the next lands on a healthy one. A
                // terminal failure fails the flush.
                client.upsert_batch(&table, &batch).await.with_context(|| {
                    format!("put_pages(tsclient) branch={branch_id} chunk_rows={chunk_rows}")
                })?;
                for (k, l) in &traced_in_chunk {
                    pageserver_api::write_trace::trace_stage("put_pages_ok", k, *l);
                }
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
        branch_id: BranchId,
        key: &[u8],
        target_lsn: Lsn,
    ) -> Result<Vec<Arc<PageRow>>> {
        let (sp, db, rel, fork, blk) = key_columns(&Key::from_slice(key))?;
        let branch_s = branch_id.to_string();
        let target = target_lsn.0 as i64;

        // branch_id stays bound ($1), NOT baked: lineage reads call
        // this with ANCESTOR branch_ids (a branch reads its ancestors'
        // pages), so the branch is not fixed on the read path even though
        // it is on the write path. The scalar-subquery anchor +
        // COALESCE(..., 0) lower bound push both ends of the lsn range into
        // the DocDB scan; `lsn >= 0` covers the no-image case. The Rust-side
        // trim below truncates at the first `will_init=true` row walking
        // LSN-desc, so the no-image case keeps full-chain semantics and an
        // init-delta between image and target_lsn shortens the chain.
        if let Some(p) = &self.tsclient {
            let started = std::time::Instant::now();
            let chain = self
                .read_chain_tsclient(p, &branch_s, sp, db, rel, fork, blk, target)
                .await?;
            crate::metrics::YB_STORE_OP_SECONDS
                .with_label_values(&["read_chain", "tsclient"])
                .observe(started.elapsed().as_secs_f64());
            return Ok(chain);
        }

        let started = std::time::Instant::now();
        let sql = build_read_chain_sql(&self.schema);
        let rows = self
            .ysql_client
            .query(&sql, &[&branch_s, &sp, &db, &rel, &fork, &blk, &target])
            .await
            .context("read_chain")?;
        crate::metrics::YB_STORE_OP_SECONDS
            .with_label_values(&["read_chain", "ysql"])
            .observe(started.elapsed().as_secs_f64());
        let mut chain = Vec::with_capacity(rows.len());
        let mut order_check: Vec<(Lsn, bool, bool)> = Vec::with_capacity(rows.len());
        for r in rows {
            let row = row_to_page(r)?;
            order_check.push((row.lsn, row.is_image, row.will_init));
            assert_chain_step(&order_check, "read_chain");
            let anchor = row.will_init;
            chain.push(Arc::new(row));
            if anchor {
                break;
            }
        }
        Ok(chain)
    }

    /// Thin-RPC variant of [`Self::read_chain`]: a paged clustered scan
    /// from `target` downward (the PK stores `lsn DESC`, so a forward
    /// scan is already LSN-descending), stopping at the image /
    /// init-delta anchor client-side. The SQL image-anchor subquery
    /// disappears — the walk stops at the same anchor row. One
    /// `RangeRead` with paging; continuations stay on the first page's
    /// snapshot via the tsclient paging envelope, so no explicit
    /// read-time pin is needed.
    #[allow(clippy::too_many_arguments)]
    async fn read_chain_tsclient(
        &self,
        p: &TsClientTables,
        branch_s: &str,
        sp: i64,
        db: i64,
        rel: i64,
        fork: i16,
        blk: i64,
        target: i64,
    ) -> Result<Vec<Arc<PageRow>>> {
        use yb_tsclient::{BindValue, CondOp, RangeRead};
        let pages = &p.pages;
        let read = RangeRead::new(
            pages,
            vec![
                BindValue::Text(branch_s.to_string()),
                int_bind_pg(pages.col("spcnode")?, sp),
                int_bind_pg(pages.col("dbnode")?, db),
                int_bind_pg(pages.col("relnode")?, rel),
                int_bind_pg(pages.col("forknum")?, fork as i64),
            ],
        )?
        .cond(pages, "blknum", CondOp::Eq, BindValue::I64(blk))?
        .cond(pages, "lsn", CondOp::Le, BindValue::I64(target))?
        .targets(pages, &["lsn", "is_image", "will_init", "payload"])?
        .limit(DIRECT_CHAIN_PAGE_ROWS);
        let raw = fetch_chain_rows_tsclient(&p.client, pages, read, 0, 2).await?;

        let key = Key::from_slice(&columns_to_key_bytes(sp, db, rel, fork, blk));
        assemble_page_chain(key, raw, 0, "read_chain_tsclient")
    }

    /// Thin-RPC variant of the one-fork multi-block scan: ONE clustered
    /// range scan over `blknum ∈ [lo, hi), lsn ≤ target` in PK order,
    /// with per-block anchor-stop collection client-side
    /// ([`BlockChainCollector`]). Reads each block's below-anchor
    /// history off the wire only until the anchor lands (the collector
    /// skips the rest of the block without buffering). The tsclient
    /// paging envelope keeps the whole scan on one read snapshot — no
    /// explicit read-time pin.
    #[allow(clippy::too_many_arguments)]
    async fn scan_one_fork_tsclient(
        &self,
        p: &TsClientTables,
        branch_s: &str,
        sp: i64,
        db: i64,
        rel: i64,
        fork: i16,
        blk_lo: i64,
        blk_hi: i64,
        target: i64,
    ) -> Result<HashMap<Key, Vec<Arc<PageRow>>>> {
        use yb_tsclient::{BindValue, CondOp, RangeRead};
        let pages = &p.pages;
        let read = RangeRead::new(
            pages,
            vec![
                BindValue::Text(branch_s.to_string()),
                int_bind_pg(pages.col("spcnode")?, sp),
                int_bind_pg(pages.col("dbnode")?, db),
                int_bind_pg(pages.col("relnode")?, rel),
                int_bind_pg(pages.col("forknum")?, fork as i64),
            ],
        )?
        .cond(pages, "blknum", CondOp::Ge, BindValue::I64(blk_lo))?
        .cond(pages, "blknum", CondOp::Lt, BindValue::I64(blk_hi))?
        .cond(pages, "lsn", CondOp::Le, BindValue::I64(target))?
        .targets(
            pages,
            &["blknum", "lsn", "is_image", "will_init", "payload"],
        )?
        .limit(DIRECT_SCAN_PAGE_ROWS);

        let collector = p
            .client
            .read_scan(
                pages,
                &read,
                BlockChainCollector::new,
                |collector, rows| -> anyhow::Result<bool> {
                    feed_scan_rows(collector, rows)?;
                    Ok(false)
                },
            )
            .await?;

        assemble_page_scan(sp, db, rel, fork, collector, "scan_range_tsclient")
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
        branch_id: BranchId,
        range: Range<Key>,
        target_lsn: Lsn,
    ) -> Result<HashMap<Key, Vec<Arc<PageRow>>>> {
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
            let chain = self.read_chain(branch_id, &key_bytes, target_lsn).await?;
            let mut out = HashMap::with_capacity(1);
            if !chain.is_empty() {
                out.insert(range.start, chain);
            }
            return Ok(out);
        }

        // branch_id stays bound ($1), NOT baked — same lineage
        // reason as `read_chain` (ancestor timelines hit this path).
        let branch_s = branch_id.to_string();
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
            if let Some(p) = &self.tsclient {
                let started = std::time::Instant::now();
                let by_key = self
                    .scan_one_fork_tsclient(p, &branch_s, sp, db, rel, fork, blk_lo, blk_hi, target)
                    .await?;
                crate::metrics::YB_STORE_OP_SECONDS
                    .with_label_values(&["scan_range", "tsclient"])
                    .observe(started.elapsed().as_secs_f64());
                return Ok(by_key);
            }
            // DISTINCT-driven LATERAL form (memoized, ids baked — see
            // `build_scan_one_fork_sql`): iterate the blocks that actually
            // exist (`SELECT DISTINCT blknum`, a Distinct Index Scan ⇒ one
            // row per block) and run the read_chain pushdown per block via
            // CROSS JOIN LATERAL. The per-block InitPlan anchor becomes a
            // pushable lower bound so both ends of the lsn range are pushed
            // into the DocDB scan, same as the single-block case.
            let sql = build_scan_one_fork_sql(&self.schema);
            self.ysql_client
                .query(
                    &sql,
                    &[&branch_s, &sp, &db, &rel, &fork, &blk_lo, &blk_hi, &target],
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
            // Cross-fork variant of the DISTINCT-driven LATERAL (memoized,
            // ids baked — see `build_scan_cross_fork_sql`), keyed on the
            // full (spcnode, dbnode, relnode, forknum, blknum) tuple so
            // anchors stay paired with the right block.
            let sql = build_scan_cross_fork_sql(&self.schema);
            self.ysql_client
                .query(
                    &sql,
                    &[
                        &branch_s, &sp_lo, &db_lo, &rel_lo, &fork_lo, &blk_lo, &sp_hi, &db_hi,
                        &rel_hi, &fork_hi, &blk_hi, &target,
                    ],
                )
                .await
                .context("scan_range cross-fork")?
        };

        group_page_rows_by_key(rows, "scan_range")
    }

    /// Multi-key ("scattered") variant of [`Self::scan_range`]: fetch
    /// the chains for an arbitrary SET of same-fork relation-page keys
    /// in ONE query (`unnest($6::bigint[])`) instead of one query per
    /// block. All keys must share (spcnode, dbnode, relnode, forknum) —
    /// the caller buckets by fork. Per-block image-anchor + will_init
    /// trim semantics are identical to `scan_range` (the SQL body is the
    /// same LATERAL, only the block-selection driver differs).
    pub async fn scan_scattered_set(
        &self,
        branch_id: BranchId,
        keys: &[Key],
        target_lsn: Lsn,
    ) -> Result<HashMap<Key, Vec<Arc<PageRow>>>> {
        if keys.is_empty() {
            return Ok(HashMap::new());
        }
        let (sp, db, rel, fork, _) = key_columns(&keys[0])?;
        let mut blknums: Vec<i64> = Vec::with_capacity(keys.len());
        for k in keys {
            let (sp2, db2, rel2, fork2, blk) = key_columns(k)?;
            if sp2 != sp || db2 != db || rel2 != rel || fork2 != fork {
                bail!(
                    "PageStore::scan_scattered_set: keys span multiple forks: \
                     ({sp},{db},{rel},{fork}) vs ({sp2},{db2},{rel2},{fork2}); \
                     caller must bucket keys by fork before calling"
                );
            }
            blknums.push(blk);
        }
        // Distinct blknums only: a repeated key would emit a duplicate
        // `unnest` row → a duplicate chain entry (this path doesn't dedup by
        // lsn). Callers pass disjoint keys today, but harden here and hand YB
        // a clean array.
        blknums.sort_unstable();
        blknums.dedup();

        // branch_id stays bound ($1), NOT baked — same lineage reason
        // as `read_chain`/`scan_range` (ancestor timelines hit this).
        let branch_s = branch_id.to_string();
        let target = target_lsn.0 as i64;
        let sql = build_scan_one_fork_scattered_sql(&self.schema);
        let rows = self
            .ysql_client
            .query(&sql, &[&branch_s, &sp, &db, &rel, &fork, &blknums, &target])
            .await
            .context("scan_scattered_set one-fork")?;

        group_page_rows_by_key(rows, "scan_scattered_set")
    }

    pub async fn delete_all_pages(&self, branch_id: BranchId) -> Result<()> {
        let sql = format!("DELETE FROM \"{}\".pages WHERE branch_id = $1", self.schema);
        self.ysql_client
            .execute(&sql, &[&branch_id.to_string()])
            .await
            .context("delete_all_pages")?;
        Ok(())
    }

    // ------------------- slru_pages ---------------------------------------

    pub async fn put_slru_pages(&self, branch_id: BranchId, rows: &[SlruPageRow]) -> Result<()> {
        if let Some(p) = &self.tsclient {
            let started = std::time::Instant::now();
            self.put_slru_pages_tsclient(p, branch_id, rows).await?;
            crate::metrics::YB_STORE_OP_SECONDS
                .with_label_values(&["put_slru_pages", "tsclient"])
                .observe(started.elapsed().as_secs_f64());
            return Ok(());
        }

        let started = std::time::Instant::now();
        let parallelism = pageserver_api::config::scale_count(2, self.scale_factor);
        let slru_full = Arc::clone(&self.baked_sql(branch_id).slru_full);

        let mut futs: Vec<futures::future::BoxFuture<'_, Result<()>>> = Vec::new();
        for chunk in rows.chunks(ROWS_PER_INSERT) {
            if chunk.is_empty() {
                continue;
            }
            let n = chunk.len();
            // Owned per-row columns, bound by reference below (no per-cell
            // `Box<dyn ToSql>`); payloads are cheap `Bytes` clones so the
            // `&[u8]` binds avoid copying the page bytes.
            let mut kind = Vec::with_capacity(n);
            let mut segno = Vec::with_capacity(n);
            let mut blknum = Vec::with_capacity(n);
            let mut is_image = Vec::with_capacity(n);
            let mut lsn = Vec::with_capacity(n);
            let mut will_init = Vec::with_capacity(n);
            let mut payloads_owned: Vec<Bytes> = Vec::with_capacity(n);
            for row in chunk {
                kind.push(slru_kind_to_i16(row.kind));
                segno.push(row.segno as i64);
                blknum.push(row.blknum as i64);
                is_image.push(row.is_image);
                lsn.push(row.lsn.0 as i64);
                will_init.push(row.will_init);
                payloads_owned.push(row.payload.clone());
            }
            let sql: Arc<str> = if n == ROWS_PER_INSERT {
                Arc::clone(&slru_full)
            } else {
                Arc::from(build_insert_sql(
                    &self.schema,
                    "slru_pages",
                    SLRU_COLUMNS,
                    branch_id,
                    SLRU_PER_ROW,
                    n,
                ))
            };
            let chunk_rows = n;
            let ysql_client = Arc::clone(&self.ysql_client);
            futs.push(Box::pin(async move {
                let payloads: Vec<&[u8]> = payloads_owned.iter().map(|b| b.as_ref()).collect();
                let mut params: Vec<&(dyn ToSql + Sync)> = Vec::with_capacity(n * SLRU_PER_ROW);
                for i in 0..n {
                    params.push(&kind[i]);
                    params.push(&segno[i]);
                    params.push(&blknum[i]);
                    params.push(&is_image[i]);
                    params.push(&lsn[i]);
                    params.push(&will_init[i]);
                    params.push(&payloads[i]);
                }
                ysql_client.execute(&sql, &params).await.with_context(|| {
                    format!("put_slru_pages branch={branch_id} chunk_rows={chunk_rows}")
                })?;
                Ok(())
            }));
        }

        futures::stream::iter(futs)
            .buffer_unordered(parallelism)
            .try_collect::<Vec<_>>()
            .await?;
        crate::metrics::YB_STORE_OP_SECONDS
            .with_label_values(&["put_slru_pages", "ysql"])
            .observe(started.elapsed().as_secs_f64());
        Ok(())
    }

    /// Per-block SLRU chain read — same image-anchored CTE shape as
    /// `read_chain`, just keyed by `(kind, segno, blknum)`.
    pub async fn read_slru_chain(
        &self,
        branch_id: BranchId,
        kind: SlruKind,
        segno: u32,
        blknum: u32,
        target_lsn: Lsn,
    ) -> Result<Vec<SlruPageRow>> {
        let branch_s = branch_id.to_string();
        let kind_i = slru_kind_to_i16(kind);
        let segno_i = segno as i64;
        let blk_i = blknum as i64;
        let target = target_lsn.0 as i64;

        if let Some(p) = &self.tsclient {
            let started = std::time::Instant::now();
            let chain = self
                .read_slru_chain_tsclient(p, &branch_s, kind, kind_i, segno_i, blk_i, target)
                .await?;
            crate::metrics::YB_STORE_OP_SECONDS
                .with_label_values(&["read_slru_chain", "tsclient"])
                .observe(started.elapsed().as_secs_f64());
            return Ok(chain);
        }

        // Scalar-subquery anchor + COALESCE(..., 0) lower bound so
        // both ends of the lsn range are pushed into the DocDB scan
        // (same shape as `pages::read_chain`).
        let sql = format!(
            "SELECT p.kind, p.segno, p.blknum, p.lsn, p.is_image, p.will_init, p.payload \
             FROM \"{schema}\".slru_pages p \
             WHERE p.branch_id=$1 AND p.kind=$2 \
               AND p.segno=$3 AND p.blknum=$4 \
               AND p.lsn <= $5 \
               AND p.lsn >= COALESCE(( \
                     SELECT a.lsn FROM \"{schema}\".slru_pages a \
                     WHERE a.branch_id=$1 AND a.kind=$2 \
                       AND a.segno=$3 AND a.blknum=$4 \
                       AND a.is_image=TRUE AND a.lsn <= $5 \
                     ORDER BY a.lsn DESC LIMIT 1), 0) \
             ORDER BY p.lsn DESC, p.is_image DESC",
            schema = self.schema
        );
        let started = std::time::Instant::now();
        let rows = self
            .ysql_client
            .query(&sql, &[&branch_s, &kind_i, &segno_i, &blk_i, &target])
            .await
            .context("read_slru_chain")?;
        crate::metrics::YB_STORE_OP_SECONDS
            .with_label_values(&["read_slru_chain", "ysql"])
            .observe(started.elapsed().as_secs_f64());
        let mut chain = Vec::with_capacity(rows.len());
        let mut order_check: Vec<(Lsn, bool, bool)> = Vec::with_capacity(rows.len());
        for r in rows {
            let row = row_to_slru_page(r)?;
            order_check.push((row.lsn, row.is_image, row.will_init));
            assert_chain_step(&order_check, "read_slru_chain");
            let anchor = row.will_init;
            chain.push(row);
            if anchor {
                break;
            }
        }
        Ok(chain)
    }

    /// Thin-RPC variant of [`Self::read_slru_chain`]; same shape as
    /// [`Self::read_chain_tsclient`] on the `(branch, kind, segno)` hash
    /// bucket. Uses the taller [`DIRECT_SLRU_CHAIN_PAGE_ROWS`] page.
    /// Snapshot held by the paging envelope.
    // Wide signature: the caller already decomposed the key columns.
    #[allow(clippy::too_many_arguments)]
    async fn read_slru_chain_tsclient(
        &self,
        p: &TsClientTables,
        branch_s: &str,
        kind: SlruKind,
        kind_i: i16,
        segno_i: i64,
        blk_i: i64,
        target: i64,
    ) -> Result<Vec<SlruPageRow>> {
        use yb_tsclient::{BindValue, CondOp, RangeRead};
        let slru = &p.slru_pages;
        let read = RangeRead::new(
            slru,
            vec![
                BindValue::Text(branch_s.to_string()),
                int_bind_pg(slru.col("kind")?, kind_i as i64),
                int_bind_pg(slru.col("segno")?, segno_i),
            ],
        )?
        .cond(slru, "blknum", CondOp::Eq, BindValue::I64(blk_i))?
        .cond(slru, "lsn", CondOp::Le, BindValue::I64(target))?
        .targets(slru, &["lsn", "is_image", "will_init", "payload"])?
        .limit(DIRECT_SLRU_CHAIN_PAGE_ROWS);
        let raw = fetch_chain_rows_tsclient(&p.client, slru, read, 0, 2).await?;

        assemble_slru_chain(
            kind,
            segno_i as u32,
            blk_i as u32,
            raw,
            0,
            "read_slru_chain_tsclient",
        )
    }

    /// Thin-RPC variant of the multi-block SLRU scan; same shape as
    /// [`Self::scan_one_fork_tsclient`] on the `(branch, kind, segno)`
    /// hash bucket, snapshot held by the paging envelope.
    #[allow(clippy::too_many_arguments)]
    async fn scan_slru_range_tsclient(
        &self,
        p: &TsClientTables,
        branch_s: &str,
        kind: SlruKind,
        kind_i: i16,
        segno_i: i64,
        blk_lo: i64,
        blk_hi: i64,
        target: i64,
    ) -> Result<HashMap<u32, Vec<SlruPageRow>>> {
        use yb_tsclient::{BindValue, CondOp, RangeRead};
        let slru = &p.slru_pages;
        let read = RangeRead::new(
            slru,
            vec![
                BindValue::Text(branch_s.to_string()),
                int_bind_pg(slru.col("kind")?, kind_i as i64),
                int_bind_pg(slru.col("segno")?, segno_i),
            ],
        )?
        .cond(slru, "blknum", CondOp::Ge, BindValue::I64(blk_lo))?
        .cond(slru, "blknum", CondOp::Lt, BindValue::I64(blk_hi))?
        .cond(slru, "lsn", CondOp::Le, BindValue::I64(target))?
        .targets(slru, &["blknum", "lsn", "is_image", "will_init", "payload"])?
        .limit(DIRECT_SCAN_PAGE_ROWS);

        let collector = p
            .client
            .read_scan(
                slru,
                &read,
                BlockChainCollector::new,
                |collector, rows| -> anyhow::Result<bool> {
                    feed_scan_rows(collector, rows)?;
                    Ok(false)
                },
            )
            .await?;

        assemble_slru_scan(kind, segno_i as u32, collector, "scan_slru_range_tsclient")
    }

    /// Range scan over SLRU blocks in `range`. `range` must stay inside
    /// one `(kind, segno)` segment — that's how SLRU vectored reads are
    /// shaped today. Returns one entry per blknum, each chain
    /// LSN-descending and truncated at the latest will_init row.
    pub async fn scan_slru_range(
        &self,
        branch_id: BranchId,
        kind: SlruKind,
        segno: u32,
        blknum_range: Range<u32>,
        target_lsn: Lsn,
    ) -> Result<HashMap<u32, Vec<SlruPageRow>>> {
        // Single-block fast path: delegate to read_slru_chain to skip
        // the multi-block JOIN.
        if blknum_range.end == blknum_range.start.saturating_add(1) {
            let chain = self
                .read_slru_chain(branch_id, kind, segno, blknum_range.start, target_lsn)
                .await?;
            let mut out = HashMap::with_capacity(1);
            if !chain.is_empty() {
                out.insert(blknum_range.start, chain);
            }
            return Ok(out);
        }

        let branch_s = branch_id.to_string();
        let kind_i = slru_kind_to_i16(kind);
        let segno_i = segno as i64;
        let blk_lo = blknum_range.start as i64;
        let blk_hi = blknum_range.end as i64;
        let target = target_lsn.0 as i64;

        if let Some(p) = &self.tsclient {
            let started = std::time::Instant::now();
            let by_block = self
                .scan_slru_range_tsclient(
                    p, &branch_s, kind, kind_i, segno_i, blk_lo, blk_hi, target,
                )
                .await?;
            crate::metrics::YB_STORE_OP_SECONDS
                .with_label_values(&["scan_slru_range", "tsclient"])
                .observe(started.elapsed().as_secs_f64());
            return Ok(by_block);
        }

        // DISTINCT-driven LATERAL so both ends of the lsn range are
        // pushed into the DocDB scan per block (same shape as
        // `pages::scan_range`).
        let sql = format!(
            "WITH blocks AS ( \
                 SELECT DISTINCT blknum FROM \"{schema}\".slru_pages \
                 WHERE branch_id=$1 AND kind=$2 AND segno=$3 \
                   AND blknum >= $4 AND blknum < $5 \
                   AND lsn <= $6 \
             ) \
             SELECT p.kind, p.segno, p.blknum, p.lsn, p.is_image, p.will_init, p.payload \
             FROM blocks b \
             CROSS JOIN LATERAL ( \
                 SELECT p.kind, p.segno, p.blknum, p.lsn, p.is_image, p.will_init, p.payload \
                 FROM \"{schema}\".slru_pages p \
                 WHERE p.branch_id=$1 AND p.kind=$2 AND p.segno=$3 \
                   AND p.blknum = b.blknum AND p.lsn <= $6 \
                   AND p.lsn >= COALESCE(( \
                         SELECT a.lsn FROM \"{schema}\".slru_pages a \
                         WHERE a.branch_id=$1 AND a.kind=$2 AND a.segno=$3 \
                           AND a.blknum = b.blknum AND a.is_image=TRUE AND a.lsn <= $6 \
                         ORDER BY a.lsn DESC LIMIT 1), 0) \
                 ORDER BY p.lsn DESC, p.is_image DESC \
             ) p \
             ORDER BY p.blknum, p.lsn DESC, p.is_image DESC",
            schema = self.schema
        );
        let rows = self
            .ysql_client
            .query(
                &sql,
                &[&branch_s, &kind_i, &segno_i, &blk_lo, &blk_hi, &target],
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
            assert_chain_step(order_check.get(&row.blknum).unwrap(), "scan_slru_range");
            let entry = by_block.entry(row.blknum).or_default();
            if entry.last().is_some_and(|last| last.will_init) {
                continue;
            }
            entry.push(row);
        }
        Ok(by_block)
    }

    /// Thin-RPC variant of [`Self::put_slru_pages`]; see
    /// [`Self::put_pages_tsclient`] for the affected-rows story. One
    /// `(kind, segno)` hash bucket per row, so a chunk usually lands on
    /// one tablet.
    async fn put_slru_pages_tsclient(
        &self,
        p: &TsClientTables,
        branch_id: BranchId,
        rows: &[SlruPageRow],
    ) -> Result<()> {
        use yb_tsclient::{BindValue, UpsertRow};
        let slru = &p.slru_pages;
        // Column metadata is constant for the whole flush — resolve once.
        let col_kind = slru.col("kind")?;
        let col_segno = slru.col("segno")?;
        let parallelism = pageserver_api::config::scale_count(2, self.scale_factor);
        let branch_s = branch_id.to_string();

        let mut futs: Vec<futures::future::BoxFuture<'_, Result<()>>> = Vec::new();
        for chunk in rows.chunks(ROWS_PER_INSERT) {
            if chunk.is_empty() {
                continue;
            }
            let mut batch = Vec::with_capacity(chunk.len());
            for row in chunk {
                batch.push(UpsertRow::new(
                    slru,
                    vec![
                        BindValue::Text(branch_s.clone()),
                        int_bind_pg(col_kind, slru_kind_to_i16(row.kind) as i64),
                        int_bind_pg(col_segno, row.segno as i64),
                        BindValue::I64(row.blknum as i64),
                        BindValue::I64(row.lsn.0 as i64),
                        BindValue::Bool(row.is_image),
                    ],
                    &[
                        ("will_init", BindValue::Bool(row.will_init)),
                        ("payload", BindValue::Bytes(row.payload.clone())),
                    ],
                )?);
            }
            let client = Arc::clone(&p.client);
            let table = Arc::clone(slru);
            let chunk_rows = batch.len();
            futs.push(Box::pin(async move {
                // Full retry budget (see put_pages): each attempt
                // round-robins to the next tserver write session.
                client.upsert_batch(&table, &batch).await.with_context(|| {
                    format!("put_slru_pages(tsclient) branch={branch_id} chunk_rows={chunk_rows}")
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

    // ------------------- meta ---------------------------------------------

    pub async fn put_meta(&self, branch_id: BranchId, rows: &[MetaRow]) -> Result<()> {
        const COLS: usize = 4;
        const ROWS_PER_INSERT: usize = 2000;
        let parallelism = pageserver_api::config::scale_count(3, self.scale_factor);
        let branch_s = branch_id.to_string();

        let mut futs: Vec<futures::future::BoxFuture<'_, Result<()>>> = Vec::new();
        for chunk in rows.chunks(ROWS_PER_INSERT) {
            if chunk.is_empty() {
                continue;
            }
            let mut sql = format!(
                "INSERT INTO \"{}\".meta (branch_id, key, lsn, payload) VALUES ",
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
                    "(${}, ${}, ${}, ${})",
                    base,
                    base + 1,
                    base + 2,
                    base + 3,
                ));
                if row.key.len() != KEY_SIZE {
                    bail!(
                        "put_meta row {i}: key bytes wrong length ({})",
                        row.key.len()
                    );
                }
                params.push(Box::new(branch_s.clone()));
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
                        format!("put_meta branch={branch_id} chunk_rows={chunk_rows}")
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
        branch_id: BranchId,
        key: &[u8],
        target_lsn: Lsn,
    ) -> Result<Option<MetaRow>> {
        if key.len() != KEY_SIZE {
            bail!("read_meta: key bytes wrong length ({})", key.len());
        }
        if let Some(p) = &self.tsclient {
            let started = std::time::Instant::now();
            let row = self
                .read_meta_tsclient(p, branch_id, key, target_lsn)
                .await?;
            crate::metrics::YB_STORE_OP_SECONDS
                .with_label_values(&["read_meta", "tsclient"])
                .observe(started.elapsed().as_secs_f64());
            return Ok(row);
        }

        let started = std::time::Instant::now();
        let sql = format!(
            "SELECT key, lsn, payload FROM \"{}\".meta \
             WHERE branch_id=$1 AND key=$2 AND lsn <= $3 \
             ORDER BY lsn DESC LIMIT 1",
            self.schema
        );
        let row_opt = self
            .ysql_client
            .query_opt(
                &sql,
                &[&branch_id.to_string(), &key, &(target_lsn.0 as i64)],
            )
            .await
            .context("read_meta")?;
        crate::metrics::YB_STORE_OP_SECONDS
            .with_label_values(&["read_meta", "ysql"])
            .observe(started.elapsed().as_secs_f64());
        row_opt.map(row_to_meta).transpose()
    }

    /// Thin-RPC variant of [`Self::read_meta`]. The meta PK hashes on
    /// `(branch_id, key)` with `lsn DESC` clustering, so the latest row
    /// at or below the target is the first row of a forward scan.
    async fn read_meta_tsclient(
        &self,
        p: &TsClientTables,
        branch_id: BranchId,
        key: &[u8],
        target_lsn: Lsn,
    ) -> Result<Option<MetaRow>> {
        use yb_tsclient::{BindValue, CondOp, RangeRead};
        let meta = &p.meta;
        let read = RangeRead::new(
            meta,
            vec![
                BindValue::Text(branch_id.to_string()),
                BindValue::Bytes(Bytes::copy_from_slice(key)),
            ],
        )?
        .cond(meta, "lsn", CondOp::Le, BindValue::I64(target_lsn.0 as i64))?
        .targets(meta, &["lsn", "payload"])?
        .limit(1);
        // DocDB may return an empty page with a continuation (deadline
        // paging); keep following until a row or range exhaustion.
        let row = p
            .client
            .read_scan(
                meta,
                &read,
                || None::<yb_tsclient::Row>,
                |found: &mut Option<yb_tsclient::Row>, rows| -> anyhow::Result<bool> {
                    if let Some(row) = rows.into_iter().next() {
                        *found = Some(row);
                        return Ok(true);
                    }
                    Ok(false)
                },
            )
            .await?;
        row.map(|r| decode_meta_row(key, &r)).transpose()
    }

    /// Range scan over meta keys. Returns the latest version per key
    /// in `[range.start, range.end)` at LSN ≤ `target_lsn`. Uses a
    /// LATERAL LIMIT 1 anchor seek per distinct key.
    pub async fn scan_meta_range(
        &self,
        branch_id: BranchId,
        range: Range<Key>,
        target_lsn: Lsn,
    ) -> Result<HashMap<Bytes, MetaRow>> {
        let branch_s = branch_id.to_string();
        let target = target_lsn.0 as i64;

        let mut start_bytes = vec![0u8; KEY_SIZE];
        range.start.write_to_byte_slice(&mut start_bytes);
        let mut end_bytes = vec![0u8; KEY_SIZE];
        range.end.write_to_byte_slice(&mut end_bytes);

        // Single-key fast path: no GROUP BY / LATERAL — just LIMIT 1.
        if range.end == range.start.next() {
            let row = self.read_meta(branch_id, &start_bytes, target_lsn).await?;
            let mut out = HashMap::with_capacity(1);
            if let Some(r) = row {
                out.insert(r.key.clone(), r);
            }
            return Ok(out);
        }

        let sql = format!(
            "WITH keys AS ( \
                 SELECT DISTINCT key FROM \"{schema}\".meta \
                 WHERE branch_id=$1 \
                   AND key >= $2 AND key < $3 AND lsn <= $4 \
             ) \
             SELECT k.key, latest.lsn, latest.payload \
             FROM keys k \
             CROSS JOIN LATERAL ( \
                 SELECT lsn, payload FROM \"{schema}\".meta \
                 WHERE branch_id=$1 \
                   AND key = k.key AND lsn <= $4 \
                 ORDER BY lsn DESC LIMIT 1 \
             ) latest",
            schema = self.schema
        );
        let rows = self
            .ysql_client
            .query(&sql, &[&branch_s, &start_bytes, &end_bytes, &target])
            .await
            .context("scan_meta_range")?;
        let mut out = HashMap::with_capacity(rows.len());
        for r in rows {
            let row = row_to_meta(r)?;
            out.insert(row.key.clone(), row);
        }
        Ok(out)
    }

    pub async fn delete_all_meta(&self, branch_id: BranchId) -> Result<()> {
        let sql = format!("DELETE FROM \"{}\".meta WHERE branch_id = $1", self.schema);
        self.ysql_client
            .execute(&sql, &[&branch_id.to_string()])
            .await
            .context("delete_all_meta")?;
        Ok(())
    }

    pub async fn delete_all_slru_pages(&self, branch_id: BranchId) -> Result<()> {
        let sql = format!(
            "DELETE FROM \"{}\".slru_pages WHERE branch_id = $1",
            self.schema
        );
        self.ysql_client
            .execute(&sql, &[&branch_id.to_string()])
            .await
            .context("delete_all_slru_pages")?;
        Ok(())
    }

    /// Drop every row for `branch_id` across all four
    /// pageserver tables (`pages`, `slru_pages`, `meta`,
    /// `timeline_state`). Used by the timeline-delete flow to
    /// reclaim space and erase persisted state in one go.
    ///
    /// Deletes are dispatched concurrently; YB commits each one
    /// independently. If any DELETE fails the whole call returns
    /// `Err` and the caller may retry — partial deletion is
    /// idempotent so a re-run safely catches up.
    pub async fn delete_all_for_timeline(&self, branch_id: BranchId) -> Result<()> {
        let (a, b, c, d) = tokio::join!(
            self.delete_all_pages(branch_id),
            self.delete_all_slru_pages(branch_id),
            self.delete_all_meta(branch_id),
            self.delete_timeline(branch_id),
        );
        a?;
        b?;
        c?;
        d?;
        Ok(())
    }

    // ------------------- lineage-aware reads -------------------------------
    //
    // A branch's pre-branch-point rows live under the ANCESTOR's
    // branch_id. Each method below walks the lineage segments
    // nearest-first, capping the per-segment read LSN at
    // `min(target_lsn, seg.cap_lsn)`, and stops as soon as the chain
    // is anchored (page/SLRU) or the key is found (meta).
    //
    // Iterative per-segment queries (not one UNION ALL) on purpose:
    // `branch_id` is in the hash partition key, so segments live on
    // different tablets. The iterative walk touches ONE tablet in the
    // steady state — always for unbranched timelines, and for branch
    // keys once the image materialiser has copied an anchor up into
    // the descendant's own branch_id. Only cold branch keys pay the
    // extra round trip, and each such read self-heals via the
    // read-side materialise trigger.

    /// Lineage-aware [`Self::read_chain`]. `lineage` is the ordered
    /// segment list from [`Self::load_lineage`]; `lineage[0]` must be
    /// the reading timeline itself.
    pub async fn read_chain_lineage(
        &self,
        lineage: &[LineageSegment],
        key: &[u8],
        target_lsn: Lsn,
    ) -> Result<Vec<Arc<PageRow>>> {
        let mut chain: Vec<Arc<PageRow>> = Vec::new();
        for seg in lineage {
            // Skip this (and every deeper) segment only when the chain
            // is already anchored AT OR ABOVE the segment's cap: the
            // segment's rows are all ≤ cap, so none can sit above the
            // anchor. An anchor BELOW the cap does NOT suffice — a
            // materialiser copy-up image lands in the descendant at
            // its chain-head LSN, which can be below the ancestor's
            // head at the cap, and the ancestor deltas in
            // (anchor_lsn, cap] must still be layered on top.
            if chain.iter().any(|r| r.will_init && r.lsn >= seg.cap_lsn) {
                break;
            }
            let eff = std::cmp::min(target_lsn, seg.cap_lsn);
            let rows = self.read_chain(seg.branch_id, key, eff).await?;
            chain.extend(rows);
        }
        if lineage.len() > 1 {
            // Cross-segment rows can interleave (an ancestor delta
            // above a descendant copy-up image) and tie at the branch
            // boundary. Re-sort (image first on ties), dedup by LSN,
            // and trim at the first anchor — same blend rules as
            // PageWriter's in-RAM merge.
            chain.sort_by(|a, b| b.lsn.cmp(&a.lsn).then_with(|| b.is_image.cmp(&a.is_image)));
            dedup_page_chain_by_lsn(&mut chain);
            if let Some(idx) = chain.iter().position(|r| r.will_init) {
                chain.truncate(idx + 1);
            }
        }
        Ok(chain)
    }

    /// Lineage-aware [`Self::scan_range`]. Ancestor segments are
    /// always scanned (when present): keys written only before the
    /// branch point are undiscoverable from the descendant's segment
    /// alone. Per-key chains are merged nearest-first, then
    /// sorted/deduped/trimmed at the anchor.
    pub async fn scan_range_lineage(
        &self,
        lineage: &[LineageSegment],
        range: Range<Key>,
        target_lsn: Lsn,
    ) -> Result<HashMap<Key, Vec<Arc<PageRow>>>> {
        self.scan_lineage(
            "scan_range_lineage",
            lineage,
            target_lsn,
            LineageScanTarget::Range(range),
        )
        .await
    }

    /// Lineage-aware [`Self::scan_scattered_set`]. Mirrors `scan_range_lineage`
    /// exactly, walking the same ancestor segments, but with a scattered
    /// key set instead of a range.
    pub async fn scan_scattered_set_lineage(
        &self,
        lineage: &[LineageSegment],
        keys: &[Key],
        target_lsn: Lsn,
    ) -> Result<HashMap<Key, Vec<Arc<PageRow>>>> {
        self.scan_lineage(
            "scan_scattered_set_lineage",
            lineage,
            target_lsn,
            LineageScanTarget::Set(keys),
        )
        .await
    }

    /// Resolve one lineage segment's chains — a contiguous range or a scattered
    /// key set, per `target`. The per-segment step of [`Self::scan_lineage`].
    async fn scan_lineage_segment(
        &self,
        target: &LineageScanTarget<'_>,
        branch_id: BranchId,
        lsn: Lsn,
    ) -> Result<HashMap<Key, Vec<Arc<PageRow>>>> {
        match target {
            LineageScanTarget::Range(range) => self.scan_range(branch_id, range.clone(), lsn).await,
            LineageScanTarget::Set(keys) => self.scan_scattered_set(branch_id, keys, lsn).await,
        }
    }

    /// Shared skeleton of [`Self::scan_range_lineage`] and
    /// [`Self::scan_scattered_set_lineage`]. Walk `lineage` newest-branch-first,
    /// scanning each segment via [`Self::scan_lineage_segment`] (capped at that
    /// segment's `cap_lsn`), fold every segment's chains into one map, then
    /// merge (sort/dedup/truncate) per key. Only the per-segment scan differs
    /// between the two callers — a contiguous range vs a scattered key set —
    /// carried by `target`. `label` names the caller for the empty-lineage
    /// error. The single-segment case returns the segment's output directly (it
    /// is already ordered); the merge only runs when ancestors contribute rows.
    async fn scan_lineage(
        &self,
        label: &str,
        lineage: &[LineageSegment],
        target_lsn: Lsn,
        target: LineageScanTarget<'_>,
    ) -> Result<HashMap<Key, Vec<Arc<PageRow>>>> {
        let Some((first, rest)) = lineage.split_first() else {
            bail!("{label}: empty lineage");
        };
        let mut out = self
            .scan_lineage_segment(
                &target,
                first.branch_id,
                std::cmp::min(target_lsn, first.cap_lsn),
            )
            .await?;
        if rest.is_empty() {
            return Ok(out);
        }
        for seg in rest {
            let eff = std::cmp::min(target_lsn, seg.cap_lsn);
            let anc = self
                .scan_lineage_segment(&target, seg.branch_id, eff)
                .await?;
            for (key, rows) in anc {
                out.entry(key).or_default().extend(rows);
            }
        }
        // Concatenating chains across ancestor segments breaks per-key LSN
        // order, so re-merge each: sort newest-first, dedup by lsn, truncate at
        // the newest will_init.
        for chain in out.values_mut() {
            chain.sort_by(|a, b| b.lsn.cmp(&a.lsn).then_with(|| b.is_image.cmp(&a.is_image)));
            dedup_page_chain_by_lsn(chain);
            if let Some(idx) = chain.iter().position(|r| r.will_init) {
                chain.truncate(idx + 1);
            }
        }
        Ok(out)
    }

    /// Lineage-aware [`Self::read_slru_chain`]. Required for branches:
    /// branching copies nothing, so a fresh branch's pg_xact / SLRU
    /// state lives entirely under the ancestor's branch_id.
    pub async fn read_slru_chain_lineage(
        &self,
        lineage: &[LineageSegment],
        kind: SlruKind,
        segno: u32,
        blknum: u32,
        target_lsn: Lsn,
    ) -> Result<Vec<SlruPageRow>> {
        let mut chain: Vec<SlruPageRow> = Vec::new();
        for seg in lineage {
            // See read_chain_lineage: an anchor below this segment's
            // cap doesn't cover the segment's rows above it.
            if chain.iter().any(|r| r.will_init && r.lsn >= seg.cap_lsn) {
                break;
            }
            let eff = std::cmp::min(target_lsn, seg.cap_lsn);
            let rows = self
                .read_slru_chain(seg.branch_id, kind, segno, blknum, eff)
                .await?;
            chain.extend(rows);
        }
        if lineage.len() > 1 {
            chain.sort_by(|a, b| b.lsn.cmp(&a.lsn).then_with(|| b.is_image.cmp(&a.is_image)));
            chain.dedup_by(|a, b| a.lsn == b.lsn);
            if let Some(idx) = chain.iter().position(|r| r.will_init) {
                chain.truncate(idx + 1);
            }
        }
        Ok(chain)
    }

    /// Lineage-aware [`Self::scan_slru_range`].
    pub async fn scan_slru_range_lineage(
        &self,
        lineage: &[LineageSegment],
        kind: SlruKind,
        segno: u32,
        blknum_range: Range<u32>,
        target_lsn: Lsn,
    ) -> Result<HashMap<u32, Vec<SlruPageRow>>> {
        let Some((first, rest)) = lineage.split_first() else {
            bail!("scan_slru_range_lineage: empty lineage");
        };
        let mut out = self
            .scan_slru_range(
                first.branch_id,
                kind,
                segno,
                blknum_range.clone(),
                std::cmp::min(target_lsn, first.cap_lsn),
            )
            .await?;
        if rest.is_empty() {
            return Ok(out);
        }
        for seg in rest {
            let eff = std::cmp::min(target_lsn, seg.cap_lsn);
            let anc = self
                .scan_slru_range(seg.branch_id, kind, segno, blknum_range.clone(), eff)
                .await?;
            for (blk, rows) in anc {
                out.entry(blk).or_default().extend(rows);
            }
        }
        for chain in out.values_mut() {
            chain.sort_by(|a, b| b.lsn.cmp(&a.lsn).then_with(|| b.is_image.cmp(&a.is_image)));
            chain.dedup_by(|a, b| a.lsn == b.lsn);
            if let Some(idx) = chain.iter().position(|r| r.will_init) {
                chain.truncate(idx + 1);
            }
        }
        Ok(out)
    }

    /// Lineage-aware [`Self::scan_meta_range`]. Nearest-segment-wins
    /// per key: a descendant's version of a key (reldir listing,
    /// relsize sentinel, …) shadows the ancestor's.
    pub async fn scan_meta_range_lineage(
        &self,
        lineage: &[LineageSegment],
        range: Range<Key>,
        target_lsn: Lsn,
    ) -> Result<HashMap<Bytes, MetaRow>> {
        let mut out: HashMap<Bytes, MetaRow> = HashMap::new();
        for seg in lineage {
            let eff = std::cmp::min(target_lsn, seg.cap_lsn);
            let rows = self
                .scan_meta_range(seg.branch_id, range.clone(), eff)
                .await?;
            for (k, v) in rows {
                out.entry(k).or_insert(v);
            }
        }
        Ok(out)
    }

    // ------------------- timelines ----------------------------------------

    /// One-shot INSERT used at timeline creation by Phase 5b's
    /// `Timeline::write_timeline_row_to_yb`. Tries to create the row;
    /// if a row at the same `branch_id` already exists this
    /// is a no-op (the existing state is the source of truth — a
    /// just-created timeline raced with an attach-time replay or
    /// similar; either way we shouldn't clobber the live row).
    ///
    /// **Not for incremental updates.** Per-flush UPDATEs of
    /// `consistent_lsn` go through [`Self::advance_consistent_lsn`];
    /// per-`UploadMetadata` UPDATEs from RTC go through
    /// [`Self::update_timeline_state`]. Both of those are gen-fenced;
    /// this one isn't, because at creation the row by definition
    /// doesn't exist yet (or — race — exists in a state we shouldn't
    /// touch).
    pub async fn insert_timeline_at_creation(&self, row: &TimelineRow) -> Result<()> {
        let sql = format!(
            "INSERT INTO \"{}\".timeline_state \
             (branch_id, consistent_lsn, \
              applied_gc_cutoff_lsn, ancestor_branch_id, ancestor_lsn, \
              current_generation, pg_version, initdb_lsn, prev_record_lsn, \
              archived_at, deleted_at, rel_size_migration, \
              rel_size_migrated_at, lineage) \
             VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9, $10, $11, $12, $13, $14) \
             ON CONFLICT (branch_id) DO NOTHING",
            self.schema
        );
        let (rel_size_migration_text, lineage_json) = serialize_row_aux(row)?;
        self.ysql_client
            .execute(
                &sql,
                &[
                    &row.branch_id.to_string(),
                    &(row.consistent_lsn.0 as i64),
                    &(row.applied_gc_cutoff_lsn.0 as i64),
                    &row.ancestor_branch_id.map(|id| id.to_string()),
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
    /// [`Self::advance_consistent_lsn`]); the eventual
    /// `task_mgr::shutdown_for_stale_generation` path on the next
    /// flush takes care of shutting the stale process down.
    ///
    /// Does not touch `branch_id`, `current_generation`
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
                consistent_lsn   = $2, \
                applied_gc_cutoff_lsn = $3, \
                ancestor_branch_id    = $4, \
                ancestor_lsn          = $5, \
                initdb_lsn            = $6, \
                prev_record_lsn       = $7, \
                archived_at           = $8, \
                deleted_at            = $9, \
                rel_size_migration    = $10, \
                rel_size_migrated_at  = $11, \
                lineage               = $12 \
             WHERE branch_id          = $1 \
               AND current_generation = $13",
            self.schema
        );
        let (rel_size_migration_text, lineage_json) = serialize_row_aux(row)?;
        let n = self
            .ysql_client
            .execute(
                &sql,
                &[
                    &row.branch_id.to_string(),
                    &(row.consistent_lsn.0 as i64),
                    &(row.applied_gc_cutoff_lsn.0 as i64),
                    &row.ancestor_branch_id.map(|id| id.to_string()),
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

    pub async fn get_timeline(&self, branch_id: BranchId) -> Result<Option<TimelineRow>> {
        let sql = format!(
            "SELECT {} FROM \"{}\".timeline_state \
             WHERE branch_id = $1",
            TIMELINE_ROW_COLUMNS, self.schema
        );
        let row = self
            .ysql_client
            .query_opt(&sql, &[&branch_id.to_string()])
            .await
            .context("get_timeline")?;
        row.map(row_to_timeline).transpose()
    }

    /// Build the ordered lineage for `branch_id` by walking
    /// `ancestor_branch_id` / `ancestor_lsn` links in
    /// `timeline_state`. Segment 0 is the timeline itself at
    /// `Lsn::MAX`; each ancestor's cap is the running `min` of the
    /// `ancestor_lsn`s on the path (non-increasing by the branch
    /// validation invariant — the `min` is belt-and-braces).
    ///
    /// Called once at startup; ancestry never changes for a live
    /// process. The `lineage` JSONB column is NOT consulted — it
    /// carries upstream detach-ancestor state, not the ancestry chain.
    pub async fn load_lineage(&self, branch_id: BranchId) -> Result<Vec<LineageSegment>> {
        const MAX_DEPTH: usize = 64;
        let mut segments = LineageSegment::single(branch_id);
        let mut seen: std::collections::HashSet<BranchId> = std::iter::once(branch_id).collect();
        let mut row = self.get_timeline(branch_id).await?.with_context(|| {
            format!("load_lineage: timeline {branch_id} has no timeline_state row")
        })?;
        let mut cap = Lsn::MAX;
        while let Some(anc_id) = row.ancestor_branch_id {
            let anc_lsn = row.ancestor_lsn.with_context(|| {
                format!(
                    "load_lineage: timeline {} has ancestor_branch_id but no ancestor_lsn",
                    row.branch_id
                )
            })?;
            cap = std::cmp::min(cap, anc_lsn);
            if !seen.insert(anc_id) {
                bail!("load_lineage: ancestry cycle at {anc_id}");
            }
            if segments.len() >= MAX_DEPTH {
                bail!("load_lineage: ancestry deeper than {MAX_DEPTH} segments");
            }
            segments.push(LineageSegment {
                branch_id: anc_id,
                cap_lsn: cap,
            });
            row = self
                .get_timeline(anc_id)
                .await?
                .with_context(|| format!("load_lineage: ancestor {anc_id} row missing"))?;
        }
        Ok(segments)
    }

    /// Create the `timeline_state` row for a branch of `ancestor` at
    /// `ancestor_lsn`, validating against the ancestor's persisted row
    /// only — no in-process ancestor Timeline required. This is the
    /// cross-process distillation of the old `branch_timeline_impl`:
    ///
    /// - branch point must not predate the ancestor's own branch point
    /// - branch point must already be durable on the ancestor
    ///   (`anc.consistent_lsn >= ancestor_lsn` replaces the in-process
    ///   `wait_lsn`; the launcher picked an LSN ≤ the ancestor's
    ///   `last_record_lsn`, so durability catches up within one flush
    ///   interval — callers may retry briefly on this error)
    /// - branch point must not be below the ancestor's GC cutoff
    ///
    /// The new row's `consistent_lsn` is set to `ancestor_lsn` — the
    /// walreceiver resume point. It must never be 0 (a 0 would make
    /// the safekeeper stream from segment 1). Idempotent on crash-retry
    /// via the `ON CONFLICT DO NOTHING` insert.
    pub async fn create_branch_row(
        &self,
        new_branch: BranchId,
        ancestor: BranchId,
        ancestor_lsn: Lsn,
        generation: i32,
    ) -> Result<TimelineRow> {
        let anc = self.get_timeline(ancestor).await?.with_context(|| {
            format!("create_branch_row: ancestor {ancestor} has no timeline_state row")
        })?;
        if anc.deleted_at.is_some() {
            bail!("create_branch_row: ancestor {ancestor} is deleted");
        }
        if anc.archived_at.is_some() {
            bail!("create_branch_row: ancestor {ancestor} is archived");
        }
        if let Some(anc_anc_lsn) = anc.ancestor_lsn {
            if ancestor_lsn < anc_anc_lsn {
                bail!(
                    "create_branch_row: branch point {ancestor_lsn} predates ancestor \
                     {ancestor}'s own branch point {anc_anc_lsn}"
                );
            }
        }
        if anc.consistent_lsn < ancestor_lsn {
            bail!(
                "create_branch_row: branch point {ancestor_lsn} not yet durable on ancestor \
                 {ancestor} (consistent_lsn {})",
                anc.consistent_lsn
            );
        }
        if ancestor_lsn < anc.applied_gc_cutoff_lsn {
            bail!(
                "create_branch_row: branch point {ancestor_lsn} is below ancestor {ancestor}'s \
                 gc cutoff {}",
                anc.applied_gc_cutoff_lsn
            );
        }
        // `prev_record_lsn` is only knowable when the branch point is
        // exactly the ancestor's durable head; otherwise leave None
        // and let basebackup emit "PREV LSN: none" (valid at a branch
        // point — see basebackup.rs is_ancestor_lsn handling).
        let prev_record_lsn = if anc.consistent_lsn == ancestor_lsn {
            anc.prev_record_lsn
        } else {
            None
        };
        let row = TimelineRow {
            branch_id: new_branch,
            consistent_lsn: ancestor_lsn,
            applied_gc_cutoff_lsn: anc.applied_gc_cutoff_lsn,
            ancestor_branch_id: Some(ancestor),
            ancestor_lsn: Some(ancestor_lsn),
            current_generation: generation,
            pg_version: anc.pg_version,
            initdb_lsn: anc.initdb_lsn,
            prev_record_lsn,
            archived_at: None,
            deleted_at: None,
            // A branch reads the ancestor's rows, so it must interpret
            // relsize keys with the same migration scheme.
            rel_size_migration: anc.rel_size_migration.clone(),
            rel_size_migrated_at: anc.rel_size_migrated_at,
            lineage: Lineage::default(),
        };
        self.insert_timeline_at_creation(&row).await?;
        Ok(row)
    }

    pub async fn delete_timeline(&self, branch_id: BranchId) -> Result<()> {
        let sql = format!(
            "DELETE FROM \"{}\".timeline_state WHERE branch_id = $1",
            self.schema
        );
        self.ysql_client
            .execute(&sql, &[&branch_id.to_string()])
            .await
            .context("delete_timeline")?;
        Ok(())
    }

    /// Column-scoped UPDATE of `consistent_lsn`. Caller guards
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
    /// [`Self::advance_consistent_lsn`] — but a stale PS could
    /// briefly write redundant rows in the gap between attach and
    /// first flush).
    ///
    /// `WHERE current_generation < $2` is the CAS predicate. If 0
    /// rows updated, probe the stored value to disambiguate:
    /// - stored `== ours` → idempotent re-attach, return
    ///   [`CasBumpOutcome::AlreadyAtOurGen`].
    /// - stored `> ours` → another pageserver bumped past us
    ///   already, return [`CasBumpOutcome::StaleGeneration`].
    /// - row missing → bail (the row is supposed to be created at
    ///   timeline creation; arriving here without it is a wiring bug).
    pub async fn cas_bump_generation_on_attach(
        &self,
        branch_id: BranchId,
        generation: utils::generation::Generation,
    ) -> Result<CasBumpOutcome> {
        let gen_i32: i32 = generation.into().map(|v: u32| v as i32).unwrap_or(0);
        let sql = format!(
            "UPDATE \"{}\".timeline_state \
             SET current_generation = $2 \
             WHERE branch_id = $1 \
               AND current_generation < $2",
            self.schema
        );
        let n = self
            .ysql_client
            .execute(&sql, &[&branch_id.to_string(), &gen_i32])
            .await
            .context("cas_bump_generation_on_attach")?;
        if n == 1 {
            return Ok(CasBumpOutcome::Bumped);
        }
        // n == 0 — probe.
        let probe_sql = format!(
            "SELECT current_generation FROM \"{}\".timeline_state \
             WHERE branch_id = $1",
            self.schema
        );
        let row = self
            .ysql_client
            .query_opt(&probe_sql, &[&branch_id.to_string()])
            .await
            .context("cas_bump_generation_on_attach probe")?;
        match row {
            None => anyhow::bail!(
                "no timeline row for {branch_id} — \
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
    /// → [`AdvanceConsistentLsnOutcome::StaleGeneration`] → fatal shutdown.
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
    pub async fn advance_consistent_lsn(
        &self,
        branch_id: BranchId,
        generation: utils::generation::Generation,
        new_dc_lsn: Lsn,
        new_prev_record_lsn: Option<Lsn>,
    ) -> Result<AdvanceConsistentLsnOutcome> {
        // Persist `prev_record_lsn` alongside `consistent_lsn`
        // so a restart can hand the safekeeper the right `xl_prev`
        // for the record at `consistent_lsn`. `COALESCE($4,
        // prev_record_lsn)` keeps the existing column value when
        // the caller didn't resolve a fresh pairing (race —
        // pages flushed before their record's `finish_write` ran;
        // see `PageWriter::take_prev_at_or_below`). Without
        // COALESCE we'd overwrite a previously-good value with
        // NULL on every throttled flush window.
        let sql = format!(
            "UPDATE \"{}\".timeline_state \
             SET consistent_lsn = $2, \
                 prev_record_lsn      = COALESCE($4, prev_record_lsn) \
             WHERE branch_id = $1 \
               AND current_generation = $3",
            self.schema
        );
        let gen_i32: i32 = generation.into().map(|v: u32| v as i32).unwrap_or(0);
        let prev_i64: Option<i64> = new_prev_record_lsn.map(|l| l.0 as i64);
        let n = self
            .ysql_client
            .execute(
                &sql,
                &[
                    &branch_id.to_string(),
                    &(new_dc_lsn.0 as i64),
                    &gen_i32,
                    &prev_i64,
                ],
            )
            .await
            .context("advance_consistent_lsn")?;
        if n == 0 {
            // Distinguish "row missing entirely" (init bug) from
            // "generation moved on" (we got fenced). One extra single-row
            // SELECT to disambiguate. Only runs on the unhappy path.
            let probe_sql = format!(
                "SELECT current_generation FROM \"{}\".timeline_state \
                 WHERE branch_id = $1",
                self.schema
            );
            let row = self
                .ysql_client
                .query_opt(&probe_sql, &[&branch_id.to_string()])
                .await
                .context("advance_consistent_lsn probe")?;
            match row {
                None => anyhow::bail!(
                    "no timeline row for {branch_id} — \
                     advance_consistent_lsn called before timeline_create wrote the row"
                ),
                Some(r) => {
                    let live_gen: i32 = r.get(0);
                    Ok(AdvanceConsistentLsnOutcome::StaleGeneration {
                        live_generation: live_gen as u32,
                    })
                }
            }
        } else {
            Ok(AdvanceConsistentLsnOutcome::Updated)
        }
    }

    pub async fn bump_generation(&self, branch_id: BranchId) -> Result<i32> {
        let sql = format!(
            "UPDATE \"{}\".timeline_state \
             SET current_generation = current_generation + 1 \
             WHERE branch_id = $1 \
             RETURNING current_generation",
            self.schema
        );
        let row = self
            .ysql_client
            .query_opt(&sql, &[&branch_id.to_string()])
            .await
            .context("bump_generation")?
            .with_context(|| format!("no timeline row for {branch_id}"))?;
        Ok(row.get::<_, i32>(0))
    }

    pub async fn read_generation(&self, branch_id: BranchId) -> Result<Option<i32>> {
        let sql = format!(
            "SELECT current_generation FROM \"{}\".timeline_state \
             WHERE branch_id = $1",
            self.schema
        );
        let row = self
            .ysql_client
            .query_opt(&sql, &[&branch_id.to_string()])
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
///
/// This is the **incremental** form: it validates only the just-appended
/// row (`chain.last()`) against its predecessor, so calling it once per
/// appended row inside the read loop is O(n) total and is equivalent to a
/// single full scan after the loop. (Re-scanning the whole accumulated
/// chain per row would be O(n²) — on clog's runaway CLOG chains of tens of
/// thousands of rows that was ~15s of pure assertion overhead per chain,
/// the dominant cost behind slow post-burst materialise / basebackup
/// reconstruction; see #115.)
fn assert_chain_step(chain: &[(Lsn, bool, bool /* will_init */)], where_: &str) {
    let n = chain.len();
    if n < 2 {
        return;
    }
    let (plsn, pis_image, _) = chain[n - 2];
    let (lsn, is_image, _) = chain[n - 1];
    if lsn > plsn {
        panic!(
            "{where_}: chain row {} lsn {lsn} > prev lsn {plsn} (must be DESC)",
            n - 1
        );
    }
    if lsn == plsn && !pis_image && is_image {
        panic!(
            "{where_}: chain has image at lsn {lsn} after a delta at the same lsn (need ORDER BY ..., is_image DESC tiebreaker)",
        );
    }
}

#[cfg(test)]
mod normalize_chain_tests {
    //! [`normalize_chain`] turns tsclient-lane storage order (`lsn DESC,
    //! is_image ASC` — delta before image on an LSN tie) into the SQL
    //! readers' result order (`is_image DESC`) and trims at the anchor.
    use super::*;

    fn walk(rows: &[(u64, bool, bool)]) -> Vec<(u64, bool, bool)> {
        normalize_chain(
            rows.to_vec(),
            |&(lsn, img, init)| (Lsn(lsn), img, init),
            "test",
        )
        .into_iter()
        .collect()
    }

    #[test]
    fn trims_at_image_anchor() {
        // storage order: deltas at 90/80, image at 70, older history below.
        let rows = [
            (90, false, false),
            (80, false, false),
            (70, true, true),
            (60, false, false),
            (50, true, true),
        ];
        assert_eq!(
            walk(&rows),
            vec![(90, false, false), (80, false, false), (70, true, true)]
        );
    }

    #[test]
    fn equal_lsn_tie_serves_image_and_drops_the_delta() {
        // Storage order puts the delta BEFORE the image at the tied LSN
        // (PK trailing is_image is ASC). The SQL readers order the image
        // first and anchor there, dropping the tied delta. The
        // materialiser stamps its image at the folded delta's LSN, so
        // this shape is common.
        let rows = [
            (90, false, false),
            (70, false, false), // folded delta, arrives first in storage order
            (70, true, true),   // image at the same LSN
        ];
        assert_eq!(walk(&rows), vec![(90, false, false), (70, true, true)]);
    }

    #[test]
    fn init_delta_anchors_without_image() {
        let rows = [(90, false, false), (40, false, true), (30, false, false)];
        assert_eq!(walk(&rows), vec![(90, false, false), (40, false, true)]);
    }

    #[test]
    fn no_anchor_returns_full_chain() {
        let rows = [(90, false, false), (80, false, false)];
        assert_eq!(walk(&rows), rows.to_vec());
    }

    #[test]
    fn empty_chain() {
        assert!(walk(&[]).is_empty());
    }
}

#[cfg(test)]
mod block_chain_collector_tests {
    //! [`BlockChainCollector`] consumes a multi-block scan in PK order
    //! and must stop buffering a block once its anchor tie group is
    //! complete, while resetting state at each block boundary.
    use super::*;

    fn collect(rows: &[(i64, i64, bool)]) -> Vec<(i64, Vec<i64>)> {
        let mut c = BlockChainCollector::new();
        for &(blk, lsn, will_init) in rows {
            c.push(blk, lsn, will_init, lsn);
        }
        c.chains
    }

    #[test]
    fn skips_below_anchor_history_and_resets_per_block() {
        let rows = [
            // block 1: deltas above an anchor at 70, shadowed history below.
            (1, 90, false),
            (1, 70, true),
            (1, 60, false),
            (1, 50, true),
            // block 2: anchor tie group — the equal-LSN sibling at the
            // anchor LSN is kept (normalize_chain sorts and trims it).
            (2, 80, false),
            (2, 80, true),
            (2, 40, false),
            // block 3: no anchor — full chain kept.
            (3, 30, false),
            (3, 20, false),
        ];
        assert_eq!(
            collect(&rows),
            vec![(1, vec![90, 70]), (2, vec![80, 80]), (3, vec![30, 20]),]
        );
    }
}

#[cfg(test)]
mod chain_order_tests {
    //! Unit tests for [`assert_chain_step`], the per-row chain-order guard
    //! run inside `read_chain` / `scan_range` / `read_slru_chain` /
    //! `scan_slru_range`. [`check`] mirrors the production usage — push
    //! rows one at a time and validate the new tail each step — so a
    //! regression in the SQL `ORDER BY` clause or in the row-to-PageRow
    //! decode is caught without needing a live YB.
    use super::*;

    fn row(lsn: u64, is_image: bool, will_init: bool) -> (Lsn, bool, bool) {
        (Lsn(lsn), is_image, will_init)
    }

    /// Drive `assert_chain_step` exactly as the read loops do: append each
    /// row to a growing chain and validate against its predecessor every
    /// step. Equivalent to validating the whole chain, but O(n) overall.
    fn check(rows: &[(Lsn, bool, bool)], where_: &str) {
        let mut chain: Vec<(Lsn, bool, bool)> = Vec::with_capacity(rows.len());
        for &r in rows {
            chain.push(r);
            assert_chain_step(&chain, where_);
        }
    }

    /// The canonical happy-path chain. Walking the trim DESC: a delta
    /// at the higher LSN, then the image (with will_init) at the
    /// lower LSN — break. Strict-decreasing LSNs, no tied LSNs.
    #[test]
    fn pure_descending_lsn_is_accepted() {
        check(
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
        check(
            &[
                row(200, false, false),
                row(100, true, true),
                row(100, false, false),
            ],
            "test_image_first_tie",
        );
    }

    /// Two deltas at the same LSN, then an image at a lower LSN.
    /// Same-LSN deltas are legal; only image-AFTER-delta-at-tied-LSN
    /// is forbidden.
    #[test]
    fn delta_delta_at_same_lsn_is_accepted() {
        check(
            &[
                row(200, false, false),
                row(200, false, false),
                row(100, true, true),
            ],
            "test_delta_delta_tie",
        );
    }

    /// Two images at the same LSN — pathological but legal under the
    /// rule (only image-AFTER-delta at the same LSN is forbidden).
    #[test]
    fn image_image_at_same_lsn_is_accepted() {
        check(
            &[row(100, true, true), row(100, true, true)],
            "test_image_image_tie",
        );
    }

    /// Single-row and empty chains never reach a predecessor, so the
    /// `n < 2` short-circuit makes them trivially ordered.
    #[test]
    fn single_and_empty_are_accepted() {
        check(&[row(100, true, true)], "test_single");
        check(&[], "test_empty");
        // Direct calls exercise the short-circuit itself.
        assert_chain_step(&[], "test_empty_direct");
        assert_chain_step(&[row(100, true, true)], "test_single_direct");
    }

    /// A long valid chain validates without panicking — the regression
    /// guard for the O(n²)→O(n) fix (#115): each step is O(1), and a long
    /// strictly-descending chain stays accepted.
    #[test]
    fn long_descending_chain_is_accepted() {
        let rows: Vec<_> = (0..10_000u64)
            .rev()
            .map(|l| row(l + 1, false, false))
            .collect();
        check(&rows, "test_long_desc");
    }

    /// LSN increasing across two rows must panic — the trim relies on
    /// DESC order, and walredo would then receive WAL records in the
    /// wrong replay order.
    #[test]
    #[should_panic(expected = "must be DESC")]
    fn lsn_ascending_panics() {
        check(
            &[row(100, false, false), row(200, false, false)],
            "test_lsn_asc",
        );
    }

    /// Image AFTER a delta at the same LSN is the pre-fix bug: the chain
    /// handed to walredo would contain a same-LSN delta whose effect is
    /// already baked into the image — double-apply.
    #[test]
    #[should_panic(expected = "after a delta at the same lsn")]
    fn image_after_delta_at_tied_lsn_panics() {
        check(
            &[
                row(200, false, false),
                row(100, false, false),
                row(100, true, true),
            ],
            "test_image_after_delta_tie",
        );
    }

    /// Equal LSNs are fine; only an increase panics (`lsn > plsn`, not
    /// `>=`). Two same-LSN deltas don't trip the DESC or image checks.
    #[test]
    fn equal_lsn_does_not_trigger_desc_panic() {
        check(
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
        key: Key::from_slice(&columns_to_key_bytes(sp, db, rel, fork, blk)),
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
const TIMELINE_ROW_COLUMNS: &str = "branch_id, consistent_lsn, \
     applied_gc_cutoff_lsn, ancestor_branch_id, ancestor_lsn, \
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
    let branch_s: String = r.try_get(0)?;
    let consistent_lsn: i64 = r.try_get(1)?;
    let applied_gc_cutoff_lsn: i64 = r.try_get(2)?;
    let ancestor_tl: Option<String> = r.try_get(3)?;
    let ancestor_lsn: Option<i64> = r.try_get(4)?;
    let current_generation: i32 = r.try_get(5)?;
    let pg_version: i32 = r.try_get(6)?;
    let initdb_lsn: Option<i64> = r.try_get(7)?;
    let prev_record_lsn: Option<i64> = r.try_get(8)?;
    let archived_at: Option<chrono::NaiveDateTime> = r.try_get(9)?;
    let deleted_at: Option<chrono::NaiveDateTime> = r.try_get(10)?;
    let rel_size_migration_text: Option<String> = r.try_get(11)?;
    let rel_size_migrated_at: Option<i64> = r.try_get(12)?;
    let lineage_json: Option<serde_json::Value> = r.try_get(13)?;

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
        branch_id: branch_s.parse().context("parsing branch_id")?,
        consistent_lsn: Lsn(consistent_lsn as u64),
        applied_gc_cutoff_lsn: Lsn(applied_gc_cutoff_lsn as u64),
        ancestor_branch_id: ancestor_tl
            .map(|s| s.parse::<BranchId>())
            .transpose()
            .context("parsing ancestor_branch_id")?,
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

    fn test_yb_config() -> YbConnConfig {
        let host = std::env::var("YB_TEST_HOST").unwrap_or_else(|_| "127.0.0.1".to_string());
        let port = std::env::var("YB_TEST_PORT").unwrap_or_else(|_| "5433".to_string());
        YbConnConfig {
            host_ports: format!("{host}:{port}").parse().unwrap(),
            user: std::env::var("YB_TEST_USER").unwrap_or_else(|_| "yugabyte".to_string()),
            password_env: None,
            dbname: std::env::var("YB_TEST_DB").unwrap_or_else(|_| "yb_storage".to_string()),
            sslmode: None,
            sslrootcert: None,
            disable_transactional_writes: false,
            hot_path: ysql_client::HotPath::Ysql,
            tserver_rpc_addrs: None,
        }
    }

    /// All tests run against the production `pageserver.*` schema
    /// (created by the controller's migration on a
    /// real `yb_amp_local`-style cluster). Per-test isolation comes
    /// from `rand_branch()` — every test writes
    /// to a distinct `branch_id` namespace, so
    /// concurrent tests don't observe each other's data and rows
    /// from prior runs are harmless.
    ///
    /// Requires the YB cluster to have the diesel migration applied
    /// (e.g. `yb_amp_local init && yb_amp_local start` once). The tests
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
                    .expect("connect to local YB; run `yb_amp_local init && start` first to migrate the schema")
            })
            .await
            .clone();
        (store, PROD_SCHEMA.to_string())
    }

    async fn drop_schema(_store: &PageStore, _schema: &str) {
        // No-op: the schema is the production one, owned by the
        // diesel migration. Per-test isolation comes from random
        // branch IDs.
    }

    fn rand_branch() -> BranchId {
        BranchId::generate()
    }

    /// `build_insert_sql` bakes the branch_id literal into
    /// every row tuple and numbers the per-row bind params `$1..` with
    /// no gaps — no live YB needed. Locks the placeholder math that the
    /// column order in `put_pages` / `put_slru_pages` depends on.
    #[test]
    fn build_insert_sql_bakes_ids_and_numbers_params() {
        let branch = rand_branch();
        let id = format!("'{branch}'");

        // pages: 9 per-row params; 2 rows → $1..$18, id baked per row.
        let pages = build_insert_sql(
            "pageserver",
            "pages",
            PAGES_COLUMNS,
            branch,
            PAGES_PER_ROW,
            2,
        );
        let expected_pages = format!(
            "INSERT INTO \"pageserver\".pages ({PAGES_COLUMNS}) VALUES \
             ({id},$1,$2,$3,$4,$5,$6,$7,$8,$9),\
             ({id},$10,$11,$12,$13,$14,$15,$16,$17,$18)"
        );
        assert_eq!(pages, expected_pages);

        // slru: 7 per-row params; 1 row → $1..$7.
        let slru = build_insert_sql(
            "pageserver",
            "slru_pages",
            SLRU_COLUMNS,
            branch,
            SLRU_PER_ROW,
            1,
        );
        let expected_slru = format!(
            "INSERT INTO \"pageserver\".slru_pages ({SLRU_COLUMNS}) VALUES \
             ({id},$1,$2,$3,$4,$5,$6,$7)"
        );
        assert_eq!(slru, expected_slru);
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

    fn page_row(
        key: Bytes,
        lsn: u64,
        is_image: bool,
        will_init: bool,
        payload: &[u8],
    ) -> Arc<PageRow> {
        Arc::new(PageRow {
            key: Key::from_slice(&key),
            lsn: Lsn(lsn),
            is_image,
            will_init,
            payload: Bytes::copy_from_slice(payload),
        })
    }

    #[test]
    fn dedup_keeps_image_on_collision_and_flags_dropped_delta() {
        let k = page_key(1663, 5, 16384, 0, 7);
        let label = ["chain_dedup_dropped_delta"];
        let count = || {
            crate::metrics::CORRECTNESS_ERRORS
                .with_label_values(&label)
                .get()
        };

        // Legit same-LSN collision: a materialised image stamped at the LSN
        // of the delta it folded. Pre-sorted descending, image-first on ties
        // (as every caller does before calling the helper). Dedup must keep
        // the image, drop the superseded delta, and NOT trip the counter.
        let before = count();
        let mut chain = vec![
            page_row(k.clone(), 0x30, false, false, b"d30"), // newer delta
            page_row(k.clone(), 0x20, true, true, b"img20"), // image @0x20 (kept)
            page_row(k.clone(), 0x20, false, false, b"d20"), // folded delta (dropped)
            page_row(k.clone(), 0x10, false, true, b"d10init"),
        ];
        dedup_page_chain_by_lsn(&mut chain);
        let shape: Vec<(u64, bool)> = chain.iter().map(|r| (r.lsn.0, r.is_image)).collect();
        assert_eq!(shape, vec![(0x30, false), (0x20, true), (0x10, false)]);
        assert_eq!(
            count(),
            before,
            "image/delta collision is legitimate; must not trip"
        );

        // Benign same-LSN duplicate: the SAME delta in both cache and YB
        // (byte-identical). Dropping it is lossless — must NOT trip.
        let before = count();
        let mut dup = vec![
            page_row(k.clone(), 0x20, false, false, b"d20"),
            page_row(k.clone(), 0x20, false, false, b"d20"),
            page_row(k.clone(), 0x10, false, true, b"d10init"),
        ];
        dedup_page_chain_by_lsn(&mut dup);
        assert_eq!(dup.len(), 2, "dedup collapses the identical duplicate");
        assert_eq!(
            count(),
            before,
            "an identical cache/YB duplicate must not trip"
        );

        // Dangerous same-LSN collision: two distinct deltas (different
        // payloads) on one LSN, no image. The survivor is a delta, so a real
        // record is dropped — the silent chain gap that surfaces as a walredo
        // PANIC (#22). The dedup still collapses the run, but the counter fires.
        let before = count();
        let mut bad = vec![
            page_row(k.clone(), 0x20, false, false, b"d20a"),
            page_row(k.clone(), 0x20, false, false, b"d20b"),
            page_row(k.clone(), 0x10, false, true, b"d10init"),
        ];
        dedup_page_chain_by_lsn(&mut bad);
        assert_eq!(bad.len(), 2, "dedup still collapses the equal-LSN run");
        assert_eq!(
            count(),
            before + 1,
            "dropping a DISTINCT non-superseded delta must trip"
        );
    }

    #[tokio::test]
    async fn put_and_read_chain_round_trip() {
        let (store, schema) = make_store("rt").await;
        let branch = rand_branch();
        let k = page_key(1663, 5, 16384, 0, 7);

        store
            .put_pages(
                branch,
                &[
                    page_row(k.clone(), 10, true, true, b"img"),
                    page_row(k.clone(), 20, false, false, b"delta-A"),
                    page_row(k.clone(), 30, false, false, b"delta-B"),
                ],
            )
            .await
            .unwrap();

        let got = store.read_chain(branch, &k, Lsn(100)).await.unwrap();
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
        let branch = rand_branch();

        // field1 = 0x65 (REL_DIR_KEY_PREFIX) - not a relation page
        let mut buf = [0u8; KEY_SIZE];
        buf[0] = 0x65;
        let bad_key = Bytes::copy_from_slice(&buf);

        let err = store
            .put_pages(branch, &[page_row(bad_key, 10, true, true, b"x")])
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
        let branch = rand_branch();
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
        store.put_pages(branch, &rows).await.unwrap();

        let got = store.read_chain(branch, &k, Lsn(10_000)).await.unwrap();
        assert_eq!(got.len(), 13);
        assert_eq!(got.first().unwrap().lsn, Lsn(130));
        assert_eq!(got.last().unwrap().lsn, Lsn(10));
        assert!(got.last().unwrap().is_image);

        let got = store.read_chain(branch, &k, Lsn(50)).await.unwrap();
        assert_eq!(got.len(), 5);
        assert_eq!(got.first().unwrap().lsn, Lsn(50));
        assert_eq!(got.last().unwrap().lsn, Lsn(10));

        let got = store.read_chain(branch, &k, Lsn(5)).await.unwrap();
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
        let branch = rand_branch();
        let k = page_key(1663, 5, 16384, 0, 99);

        // Same-LSN image+delta at 100, plus a later delta at 200.
        // Insert image LAST so any incidental storage order would
        // bury it after the same-LSN delta.
        store
            .put_pages(
                branch,
                &[
                    page_row(k.clone(), 100, false, false, b"wal-rec-at-100"),
                    page_row(k.clone(), 200, false, false, b"wal-rec-at-200"),
                    page_row(k.clone(), 100, true, true, b"post-wal-100-image"),
                ],
            )
            .await
            .unwrap();

        let got = store.read_chain(branch, &k, Lsn(300)).await.unwrap();

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
        let branch = rand_branch();
        let k = page_key(1663, 5, 16384, 0, 2);

        let rows = vec![
            page_row(k.clone(), 1, false, false, b"d-old1"),
            page_row(k.clone(), 5, false, false, b"d-old2"),
            page_row(k.clone(), 10, false, true, b"init-delta"),
            page_row(k.clone(), 20, false, false, b"d20"),
            page_row(k.clone(), 30, false, false, b"d30"),
            page_row(k.clone(), 40, false, false, b"d40"),
        ];
        store.put_pages(branch, &rows).await.unwrap();
        let got = store.read_chain(branch, &k, Lsn(100)).await.unwrap();
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
        let branch = rand_branch();
        let k = page_key(1663, 5, 16384, 0, 3);

        store
            .put_pages(
                branch,
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

        let got = store.read_chain(branch, &k, Lsn(30)).await.unwrap();
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
        let branch = rand_branch();
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
        store.put_pages(branch, &rows).await.unwrap();

        // Scan blocks 1..4 of (rel=16384, fork=0).
        let range = Key::from_slice(&page_key(1663, 5, 16384, 0, 1))
            ..Key::from_slice(&page_key(1663, 5, 16384, 0, 4));
        let got = store.scan_range(branch, range, Lsn(100)).await.unwrap();
        // 3 keys (blknum 1, 2, 3); each with 2 rows.
        assert_eq!(got.len(), 3);
        for chain in got.values() {
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
        let branch = rand_branch();
        let k_bytes = page_key(1663, 5, 16384, 0, 7);
        let k = Key::from_slice(&k_bytes);

        store
            .put_pages(
                branch,
                &[
                    page_row(k_bytes.clone(), 10, true, true, b"img"),
                    page_row(k_bytes.clone(), 20, false, false, b"d20"),
                    page_row(k_bytes.clone(), 30, false, false, b"d30"),
                ],
            )
            .await
            .unwrap();

        let range = k..k.next();
        let got = store.scan_range(branch, range, Lsn(100)).await.unwrap();
        assert_eq!(got.len(), 1);
        let chain = &got[&Key::from_slice(&k_bytes)];
        let lsns: Vec<u64> = chain.iter().map(|r| r.lsn.0).collect();
        assert_eq!(lsns, vec![30, 20, 10]);
        assert!(chain.last().unwrap().is_image);

        // Same target via read_chain — must match.
        let direct = store.read_chain(branch, &k_bytes, Lsn(100)).await.unwrap();
        assert_eq!(chain.as_slice(), direct.as_slice());
        drop_schema(&store, &schema).await;
    }

    #[tokio::test]
    async fn scan_range_single_key_empty_when_no_anchor() {
        // A key with no rows ≤ target returns an empty map.
        let (store, schema) = make_store("scan_single_empty").await;
        let branch = rand_branch();
        let k_bytes = page_key(1663, 5, 16384, 0, 99);
        let k = Key::from_slice(&k_bytes);

        let got = store
            .scan_range(branch, k..k.next(), Lsn(100))
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
        let branch = rand_branch();

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
        store.put_pages(branch, &rows).await.unwrap();

        let range = Key::from_slice(&k0)..Key::from_slice(&page_key(1663, 5, 16384, 0, 4));
        let got = store.scan_range(branch, range, Lsn(100)).await.unwrap();
        assert_eq!(got.len(), 4);

        // k0: image at 50 + deltas 60, 70 => LSNs 70, 60, 50.
        let c0 = &got[&Key::from_slice(&k0)];
        let lsns0: Vec<u64> = c0.iter().map(|r| r.lsn.0).collect();
        assert_eq!(lsns0, vec![70, 60, 50], "k0 should anchor on image @ 50");
        assert!(c0.last().unwrap().is_image);

        // k1: just the image.
        let c1 = &got[&Key::from_slice(&k1)];
        assert_eq!(c1.len(), 1);
        assert_eq!(c1[0].lsn, Lsn(30));
        assert!(c1[0].is_image);

        // k2: slow path — no image — full chain to the will_init delta.
        let c2 = &got[&Key::from_slice(&k2)];
        let lsns2: Vec<u64> = c2.iter().map(|r| r.lsn.0).collect();
        assert_eq!(lsns2, vec![40, 30, 20]);
        assert!(c2.last().unwrap().will_init);
        assert!(!c2.last().unwrap().is_image);

        // k3: full chain returned; no anchor (would be a walredo error
        // upstream, but the store call itself succeeds).
        let c3 = &got[&Key::from_slice(&k3)];
        let lsns3: Vec<u64> = c3.iter().map(|r| r.lsn.0).collect();
        assert_eq!(lsns3, vec![40, 30]);

        drop_schema(&store, &schema).await;
    }

    #[tokio::test]
    async fn scan_range_cross_fork() {
        let (store, schema) = make_store("scan_cross").await;
        let branch = rand_branch();
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
        store.put_pages(branch, &rows).await.unwrap();

        // Range spans both rels.
        let range = Key::from_slice(&page_key(1663, 5, 16384, 0, 0))
            ..Key::from_slice(&page_key(1663, 5, 16386, 0, 0));
        let got = store.scan_range(branch, range, Lsn(100)).await.unwrap();
        // 6 distinct keys (3 blocks × 2 rels).
        assert_eq!(got.len(), 6);
        drop_schema(&store, &schema).await;
    }

    #[tokio::test]
    async fn scan_range_rejects_non_page_namespace() {
        let (store, schema) = make_store("scan_reject").await;
        let branch = rand_branch();
        // field1 = 0x60 (RELATION_SIZE_PREFIX) - metadata namespace
        let mut s = [0u8; KEY_SIZE];
        s[0] = 0x60;
        let mut e = [0u8; KEY_SIZE];
        e[0] = 0x61;
        let range = Key::from_slice(&s)..Key::from_slice(&e);
        let err = store.scan_range(branch, range, Lsn(100)).await.unwrap_err();
        let msg = format!("{err:#}");
        assert!(
            msg.contains("relation-page namespace") || msg.contains("non-page"),
            "{msg}"
        );
        drop_schema(&store, &schema).await;
    }

    #[tokio::test]
    async fn read_chain_isolates_by_branch() {
        let (store, schema) = make_store("isolate").await;
        let k = page_key(1663, 5, 16384, 0, 2);
        let b1 = rand_branch();
        let b2 = rand_branch();
        let b3 = rand_branch();
        store
            .put_pages(b1, &[page_row(k.clone(), 10, true, true, b"b1")])
            .await
            .unwrap();
        store
            .put_pages(b2, &[page_row(k.clone(), 10, true, true, b"b2")])
            .await
            .unwrap();
        store
            .put_pages(b3, &[page_row(k.clone(), 10, true, true, b"b3")])
            .await
            .unwrap();
        assert_eq!(
            store.read_chain(b1, &k, Lsn(100)).await.unwrap()[0]
                .payload
                .as_ref(),
            b"b1"
        );
        assert_eq!(
            store.read_chain(b2, &k, Lsn(100)).await.unwrap()[0]
                .payload
                .as_ref(),
            b"b2"
        );
        assert_eq!(
            store.read_chain(b3, &k, Lsn(100)).await.unwrap()[0]
                .payload
                .as_ref(),
            b"b3"
        );
        drop_schema(&store, &schema).await;
    }

    #[tokio::test]
    async fn high_lsn_round_trip() {
        let (store, schema) = make_store("high_lsn").await;
        let branch = rand_branch();
        let k = page_key(1663, 5, 16384, 0, 0xff);
        let high = 0x7fff_ffff_ffff_fff0u64;
        store
            .put_pages(branch, &[page_row(k.clone(), high, true, true, b"x")])
            .await
            .unwrap();
        let got = store
            .read_chain(branch, &k, Lsn(u64::MAX >> 1))
            .await
            .unwrap();
        assert_eq!(got.len(), 1);
        assert_eq!(got[0].lsn.0, high);
        drop_schema(&store, &schema).await;
    }

    fn sample_timeline_row(branch: BranchId) -> TimelineRow {
        TimelineRow {
            branch_id: branch,
            consistent_lsn: Lsn(90),
            applied_gc_cutoff_lsn: Lsn(50),
            ancestor_branch_id: None,
            ancestor_lsn: None,
            current_generation: 1,
            pg_version: 18,
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
        let branch = rand_branch();
        let row = sample_timeline_row(branch);
        store.insert_timeline_at_creation(&row).await.unwrap();
        let got = store.get_timeline(branch).await.unwrap().unwrap();
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
        let branch = rand_branch();
        let mut row = sample_timeline_row(branch);
        row.current_generation = 1;
        row.consistent_lsn = Lsn(0x1000);
        store.insert_timeline_at_creation(&row).await.unwrap();

        // Bumped: stored gen=1, attach at gen=2 → succeeds, YB now 2.
        //
        // Accept AlreadyAtOurGen too: `cas_bump_generation_on_attach`
        // runs through `YsqlClient`'s at-least-once retry, so if the
        // UPDATE commits server-side but the connection drops before the
        // ack (the "unknown session" class seen under CI load), the
        // retried UPDATE matches 0 rows (already bumped) and the probe
        // reports AlreadyAtOurGen for what was really a bump. Both leave
        // YB at our generation and the caller
        // (`bump_generation_on_attach`) treats them identically; only the
        // resulting persisted value is load-bearing, so assert on that.
        let res = store
            .cas_bump_generation_on_attach(branch, Generation::new(2))
            .await
            .unwrap();
        assert!(
            matches!(
                res,
                CasBumpOutcome::Bumped | CasBumpOutcome::AlreadyAtOurGen
            ),
            "expected Bumped (or AlreadyAtOurGen under a post-commit retry), got: {res:?}"
        );
        let got = store.get_timeline(branch).await.unwrap().unwrap();
        assert_eq!(got.current_generation, 2);

        // AlreadyAtOurGen: stored gen=2, attach at gen=2 → no-op.
        let res = store
            .cas_bump_generation_on_attach(branch, Generation::new(2))
            .await
            .unwrap();
        assert_eq!(res, CasBumpOutcome::AlreadyAtOurGen);
        let got = store.get_timeline(branch).await.unwrap().unwrap();
        assert_eq!(got.current_generation, 2);

        // Bumped again: stored gen=2, attach at gen=5 → succeeds, YB now 5.
        // (Mirrors the user's report: SC's tenants.generation
        // moved 1→2→3 over restarts; this is the third restart
        // arriving at the PS, which must heal the lagging YB column.)
        // Accept AlreadyAtOurGen for the same post-commit-retry reason as
        // the first bump above.
        let res = store
            .cas_bump_generation_on_attach(branch, Generation::new(5))
            .await
            .unwrap();
        assert!(
            matches!(
                res,
                CasBumpOutcome::Bumped | CasBumpOutcome::AlreadyAtOurGen
            ),
            "expected Bumped (or AlreadyAtOurGen under a post-commit retry), got: {res:?}"
        );
        let got = store.get_timeline(branch).await.unwrap().unwrap();
        assert_eq!(got.current_generation, 5);

        // StaleGeneration: stored gen=5, attach at gen=4 → another
        // PS bumped past us, return live=5 so caller can refuse.
        let res = store
            .cas_bump_generation_on_attach(branch, Generation::new(4))
            .await
            .unwrap();
        assert_eq!(res, CasBumpOutcome::StaleGeneration { live_generation: 5 });
        let got = store.get_timeline(branch).await.unwrap().unwrap();
        assert_eq!(got.current_generation, 5, "stale CAS must not regress");

        // Row missing: attach against a branch that
        // doesn't exist → error (wiring bug, not a stale-gen case).
        let other_branch = rand_branch();
        let err = store
            .cas_bump_generation_on_attach(other_branch, Generation::new(1))
            .await
            .unwrap_err();
        assert!(
            err.to_string().contains("no timeline row"),
            "expected 'no timeline row' err, got: {err}"
        );

        drop_schema(&store, &schema).await;
    }

    #[tokio::test]
    async fn advance_consistent_lsn_gen_gate() {
        use utils::generation::Generation;
        use utils::lsn::Lsn;

        let (store, schema) = make_store("dc_gate").await;
        let branch = rand_branch();
        let mut row = sample_timeline_row(branch);
        row.current_generation = 5;
        row.consistent_lsn = Lsn(0x1000);
        store.insert_timeline_at_creation(&row).await.unwrap();

        // Matching generation → Updated, persisted value advances.
        let res = store
            .advance_consistent_lsn(branch, Generation::new(5), Lsn(0x2000), Some(Lsn(0x1F00)))
            .await
            .unwrap();
        assert_eq!(res, AdvanceConsistentLsnOutcome::Updated);
        let got = store.get_timeline(branch).await.unwrap().unwrap();
        assert_eq!(got.consistent_lsn, Lsn(0x2000));
        assert_eq!(got.prev_record_lsn, Some(Lsn(0x1F00)));

        // Stale generation → StaleGeneration { live = 5 }, value unchanged.
        let res = store
            .advance_consistent_lsn(branch, Generation::new(4), Lsn(0x3000), Some(Lsn(0x2F00)))
            .await
            .unwrap();
        assert_eq!(
            res,
            AdvanceConsistentLsnOutcome::StaleGeneration { live_generation: 5 }
        );
        let got = store.get_timeline(branch).await.unwrap().unwrap();
        assert_eq!(
            got.consistent_lsn,
            Lsn(0x2000),
            "stale-gen UPDATE must not advance"
        );

        // Generation ahead of persisted (we bumped in-memory but the
        // row hasn't been UPSERT'd yet) → also stale: persisted row is
        // the source of truth.
        let res = store
            .advance_consistent_lsn(branch, Generation::new(6), Lsn(0x4000), None)
            .await
            .unwrap();
        assert_eq!(
            res,
            AdvanceConsistentLsnOutcome::StaleGeneration { live_generation: 5 }
        );
        let got = store.get_timeline(branch).await.unwrap().unwrap();
        assert_eq!(got.consistent_lsn, Lsn(0x2000));

        drop_schema(&store, &schema).await;
    }

    #[tokio::test]
    async fn bump_generation_is_monotonic() {
        let (store, schema) = make_store("gen").await;
        let branch = rand_branch();
        store
            .insert_timeline_at_creation(&sample_timeline_row(branch))
            .await
            .unwrap();
        for expected in 2..=5 {
            let g = store.bump_generation(branch).await.unwrap();
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
        let branch = rand_branch();
        let n: u32 = 1200;
        let payload = vec![0xABu8; 8192];
        let mut rows = Vec::with_capacity(n as usize);
        for blk in 0..n {
            let k = page_key(1663, 5, 16384, 0, blk);
            rows.push(page_row(k, 10, true, true, &payload));
        }
        store.put_pages(branch, &rows).await.unwrap();

        // Spot-check: first, middle, last row each read back.
        for blk in [0u32, n / 2, n - 1] {
            let k = page_key(1663, 5, 16384, 0, blk);
            let chain = store.read_chain(branch, &k, Lsn(100)).await.unwrap();
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
        let branch = rand_branch();
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
        store.put_meta(branch, &rows).await.unwrap();

        // Spot-check first, middle, last.
        for i in [0u32, n / 2, n - 1] {
            let mut buf = vec![0u8; KEY_SIZE];
            buf[0] = 0x60;
            buf[14..18].copy_from_slice(&i.to_be_bytes());
            let got = store
                .read_meta(branch, &buf, Lsn(100))
                .await
                .unwrap()
                .expect("row");
            assert_eq!(got.payload.as_ref(), format!("v{i}").as_bytes());
        }
        drop_schema(&store, &schema).await;
    }

    // ------------------- lineage tests -------------------------------------

    /// Two-segment lineage: `child` (at MAX) branched off `anc` at `cap`.
    fn lineage2(child: BranchId, anc: BranchId, cap: u64) -> Vec<LineageSegment> {
        vec![
            LineageSegment {
                branch_id: child,
                cap_lsn: Lsn::MAX,
            },
            LineageSegment {
                branch_id: anc,
                cap_lsn: Lsn(cap),
            },
        ]
    }

    /// A branch chain anchors in the ancestor: pre-branch rows live
    /// under the ancestor's branch_id and the lineage walk stitches
    /// them below the descendant's deltas.
    #[tokio::test]
    async fn read_chain_lineage_anchors_in_ancestor() {
        let (store, schema) = make_store("lin_anchor").await;
        let anc = rand_branch();
        let child = rand_branch();
        let k = page_key(1663, 5, 16384, 0, 1);

        store
            .put_pages(
                anc,
                &[
                    page_row(k.clone(), 10, true, true, b"img"),
                    page_row(k.clone(), 20, false, false, b"anc-delta"),
                ],
            )
            .await
            .unwrap();
        store
            .put_pages(
                child,
                &[
                    page_row(k.clone(), 30, false, false, b"child-A"),
                    page_row(k.clone(), 40, false, false, b"child-B"),
                ],
            )
            .await
            .unwrap();

        let lineage = lineage2(child, anc, 25);
        let chain = store
            .read_chain_lineage(&lineage, &k, Lsn(100))
            .await
            .unwrap();
        assert_eq!(
            chain.iter().map(|r| r.lsn.0).collect::<Vec<_>>(),
            vec![40, 30, 20, 10]
        );
        assert!(chain[3].is_image, "anchor is the ancestor's image");
        drop_schema(&store, &schema).await;
    }

    /// Ancestor rows ABOVE the branch point must be invisible to the
    /// descendant — they belong to the ancestor's diverged future.
    #[tokio::test]
    async fn read_chain_lineage_caps_ancestor_at_branch_point() {
        let (store, schema) = make_store("lin_cap").await;
        let anc = rand_branch();
        let child = rand_branch();
        let k = page_key(1663, 5, 16384, 0, 2);

        store
            .put_pages(
                anc,
                &[
                    page_row(k.clone(), 10, true, true, b"img"),
                    page_row(k.clone(), 20, false, false, b"pre-branch"),
                    page_row(k.clone(), 30, false, false, b"anc-diverged"),
                ],
            )
            .await
            .unwrap();
        store
            .put_pages(child, &[page_row(k.clone(), 40, false, false, b"child")])
            .await
            .unwrap();

        let lineage = lineage2(child, anc, 25);
        let chain = store
            .read_chain_lineage(&lineage, &k, Lsn(100))
            .await
            .unwrap();
        assert_eq!(
            chain.iter().map(|r| r.lsn.0).collect::<Vec<_>>(),
            vec![40, 20, 10],
            "ancestor row at lsn 30 (> branch point 25) must be excluded"
        );
        drop_schema(&store, &schema).await;
    }

    /// Regression (#16 branch debugging): a materialiser copy-up
    /// image lands in the DESCENDANT at its chain-head LSN, which can
    /// be BELOW the ancestor's head at the branch cap. The walk must
    /// still descend and layer the ancestor deltas in
    /// (anchor_lsn, cap] on top of the copy-up anchor — stopping at
    /// the own-segment anchor loses them (observed as a branch
    /// serving stale pg_class/heap pages).
    #[tokio::test]
    async fn read_chain_lineage_layers_ancestor_deltas_above_sub_cap_anchor() {
        let (store, schema) = make_store("lin_subcap").await;
        let anc = rand_branch();
        let child = rand_branch();
        let k = page_key(1663, 5, 16384, 0, 7);

        store
            .put_pages(
                anc,
                &[
                    page_row(k.clone(), 10, true, true, b"anc-img"),
                    page_row(k.clone(), 20, false, false, b"anc-delta-A"),
                    // deltas BETWEEN the copy-up anchor (25) and the cap (40)
                    page_row(k.clone(), 30, false, false, b"anc-delta-B"),
                    page_row(k.clone(), 38, false, false, b"anc-delta-C"),
                    // diverged past the cap — must stay invisible
                    page_row(k.clone(), 50, false, false, b"anc-diverged"),
                ],
            )
            .await
            .unwrap();
        // Copy-up image in the child at LSN 25 — below the cap.
        store
            .put_pages(
                child,
                &[page_row(k.clone(), 25, true, true, b"copy-up-img")],
            )
            .await
            .unwrap();

        let lineage = lineage2(child, anc, 40);
        let chain = store
            .read_chain_lineage(&lineage, &k, Lsn(100))
            .await
            .unwrap();
        assert_eq!(
            chain.iter().map(|r| r.lsn.0).collect::<Vec<_>>(),
            vec![38, 30, 25],
            "ancestor deltas above the sub-cap copy-up anchor must be layered on top"
        );
        assert!(chain[2].is_image, "anchored on the copy-up image");

        // An own anchor AT or ABOVE the cap covers everything — the
        // walk must not descend (fast path preserved).
        store
            .put_pages(
                child,
                &[page_row(k.clone(), 45, true, true, b"post-cap-img")],
            )
            .await
            .unwrap();
        let chain = store
            .read_chain_lineage(&lineage, &k, Lsn(100))
            .await
            .unwrap();
        assert_eq!(
            chain.iter().map(|r| r.lsn.0).collect::<Vec<_>>(),
            vec![45],
            "post-cap own anchor shadows the whole ancestor history"
        );
        drop_schema(&store, &schema).await;
    }

    /// Once the descendant's own segment is anchored (e.g. a copy-up
    /// image from the materialiser), the walk must not descend.
    #[tokio::test]
    async fn read_chain_lineage_stops_at_own_anchor() {
        let (store, schema) = make_store("lin_own").await;
        let anc = rand_branch();
        let child = rand_branch();
        let k = page_key(1663, 5, 16384, 0, 3);

        store
            .put_pages(
                anc,
                &[
                    page_row(k.clone(), 10, true, true, b"anc-img"),
                    page_row(k.clone(), 20, false, false, b"anc-delta"),
                ],
            )
            .await
            .unwrap();
        store
            .put_pages(
                child,
                &[page_row(k.clone(), 35, true, true, b"copy-up-img")],
            )
            .await
            .unwrap();

        let lineage = lineage2(child, anc, 25);
        let chain = store
            .read_chain_lineage(&lineage, &k, Lsn(100))
            .await
            .unwrap();
        assert_eq!(chain.len(), 1, "own image shadows the whole ancestor chain");
        assert_eq!(chain[0].lsn, Lsn(35));
        drop_schema(&store, &schema).await;
    }

    /// Range scans must surface keys that exist ONLY in the ancestor
    /// (written before the branch point, untouched since).
    #[tokio::test]
    async fn scan_range_lineage_discovers_ancestor_only_keys() {
        let (store, schema) = make_store("lin_scan").await;
        let anc = rand_branch();
        let child = rand_branch();
        let k_anc_only = page_key(1663, 5, 16384, 0, 1);
        let k_shared = page_key(1663, 5, 16384, 0, 2);

        store
            .put_pages(
                anc,
                &[
                    page_row(k_anc_only.clone(), 10, true, true, b"a-img"),
                    page_row(k_shared.clone(), 10, true, true, b"s-img"),
                ],
            )
            .await
            .unwrap();
        store
            .put_pages(
                child,
                &[page_row(k_shared.clone(), 30, false, false, b"s-child")],
            )
            .await
            .unwrap();

        let lineage = lineage2(child, anc, 25);
        let start = Key::from_slice(&page_key(1663, 5, 16384, 0, 0));
        let end = Key::from_slice(&page_key(1663, 5, 16384, 0, 10));
        let out = store
            .scan_range_lineage(&lineage, start..end, Lsn(100))
            .await
            .unwrap();
        assert_eq!(out.len(), 2);
        assert_eq!(
            out[&Key::from_slice(&k_anc_only)]
                .iter()
                .map(|r| r.lsn.0)
                .collect::<Vec<_>>(),
            vec![10],
            "ancestor-only key must be discovered"
        );
        assert_eq!(
            out[&Key::from_slice(&k_shared)]
                .iter()
                .map(|r| r.lsn.0)
                .collect::<Vec<_>>(),
            vec![30, 10],
            "shared key blends child delta over ancestor image"
        );
        drop_schema(&store, &schema).await;
    }

    /// Core correctness of the batched-reads change: `scan_scattered_set` on a
    /// set of non-contiguous same-fork keys must return byte-identical chains
    /// to reading each key on its own (same image anchor, same `will_init`
    /// trim, same order). The multi-key `unnest` query is purely a batching
    /// optimization — it must not change what a read returns. Also covers the
    /// edge cases: an absent block is simply missing from the result, a
    /// repeated key is de-duplicated, and keys spanning forks are rejected.
    #[tokio::test]
    async fn scan_scattered_set_matches_per_key_reads() {
        let (store, schema) = make_store("scattered_eq").await;
        let br = rand_branch();
        // Non-contiguous blocks in one fork: an image+delta chain, an
        // image-only block, and a delta-only (image-less) block.
        let k1 = page_key(1663, 5, 16384, 0, 1);
        let k4 = page_key(1663, 5, 16384, 0, 4);
        let k9 = page_key(1663, 5, 16384, 0, 9);
        store
            .put_pages(
                br,
                &[
                    page_row(k1.clone(), 10, true, true, b"k1-img"),
                    page_row(k1.clone(), 20, false, false, b"k1-d20"),
                    page_row(k4.clone(), 10, true, true, b"k4-img"),
                    page_row(k9.clone(), 15, false, false, b"k9-d15"),
                    page_row(k9.clone(), 25, false, false, b"k9-d25"),
                ],
            )
            .await
            .unwrap();

        let k_absent = Key::from_slice(&page_key(1663, 5, 16384, 0, 999));
        let keys: Vec<Key> = [&k1, &k4, &k9]
            .iter()
            .map(|b| Key::from_slice(b))
            .chain(std::iter::once(k_absent))
            .collect();
        let target = Lsn(100);

        let set = store.scan_scattered_set(br, &keys, target).await.unwrap();

        // Full-payload shape comparison against the single-key path (read_chain).
        let shape = |c: &[Arc<PageRow>]| {
            c.iter()
                .map(|r| (r.lsn.0, r.is_image, r.will_init, r.payload.clone()))
                .collect::<Vec<_>>()
        };
        for k in [
            Key::from_slice(&k1),
            Key::from_slice(&k4),
            Key::from_slice(&k9),
        ] {
            let one = store.scan_range(br, k..k.next(), target).await.unwrap();
            let expect = one.get(&k).cloned().unwrap_or_default();
            let got = set.get(&k).cloned().unwrap_or_default();
            assert_eq!(
                shape(&got),
                shape(&expect),
                "scattered chain must match per-key read for {k}"
            );
        }
        // Absent block: no rows → key missing from the result (not an error,
        // not an empty-chain entry).
        assert!(
            !set.contains_key(&k_absent),
            "absent block must not appear in the result"
        );

        // Repeated key is de-duplicated: same result as the unique set.
        let dup = store
            .scan_scattered_set(br, &[Key::from_slice(&k1), Key::from_slice(&k1)], target)
            .await
            .unwrap();
        assert_eq!(
            shape(&dup[&Key::from_slice(&k1)]),
            shape(&set[&Key::from_slice(&k1)]),
            "a repeated key must not duplicate chain rows"
        );

        // Keys spanning forks are rejected (caller must bucket by fork).
        let cross_fork = vec![
            Key::from_slice(&page_key(1663, 5, 16384, 0, 1)),
            Key::from_slice(&page_key(1663, 5, 16384, 1, 1)),
        ];
        assert!(
            store
                .scan_scattered_set(br, &cross_fork, target)
                .await
                .is_err(),
            "keys spanning forks must error"
        );
        drop_schema(&store, &schema).await;
    }

    /// Lineage variant: `scan_scattered_set_lineage` must surface scattered
    /// keys that exist ONLY in the ancestor and blend child deltas over
    /// ancestor images — the scattered analog of
    /// `scan_range_lineage_discovers_ancestor_only_keys`.
    #[tokio::test]
    async fn scan_scattered_set_lineage_discovers_ancestor_only_keys() {
        let (store, schema) = make_store("scattered_lin").await;
        let anc = rand_branch();
        let child = rand_branch();
        // Non-contiguous keys so the set is genuinely scattered.
        let k_anc_only = page_key(1663, 5, 16384, 0, 1);
        let k_shared = page_key(1663, 5, 16384, 0, 8);

        store
            .put_pages(
                anc,
                &[
                    page_row(k_anc_only.clone(), 10, true, true, b"a-img"),
                    page_row(k_shared.clone(), 10, true, true, b"s-img"),
                ],
            )
            .await
            .unwrap();
        store
            .put_pages(
                child,
                &[page_row(k_shared.clone(), 30, false, false, b"s-child")],
            )
            .await
            .unwrap();

        let lineage = lineage2(child, anc, 25);
        let keys = vec![Key::from_slice(&k_anc_only), Key::from_slice(&k_shared)];
        let out = store
            .scan_scattered_set_lineage(&lineage, &keys, Lsn(100))
            .await
            .unwrap();
        assert_eq!(out.len(), 2);
        assert_eq!(
            out[&Key::from_slice(&k_anc_only)]
                .iter()
                .map(|r| r.lsn.0)
                .collect::<Vec<_>>(),
            vec![10],
            "ancestor-only scattered key must be discovered"
        );
        assert_eq!(
            out[&Key::from_slice(&k_shared)]
                .iter()
                .map(|r| r.lsn.0)
                .collect::<Vec<_>>(),
            vec![30, 10],
            "shared key blends child delta over ancestor image"
        );
        drop_schema(&store, &schema).await;
    }

    /// SLRU lineage: a fresh branch's pg_xact lives entirely under the
    /// ancestor; the walk must stitch it in.
    #[tokio::test]
    async fn read_slru_chain_lineage_blends() {
        let (store, schema) = make_store("lin_slru").await;
        let anc = rand_branch();
        let child = rand_branch();
        let slru_row = |lsn: u64, is_image: bool, will_init: bool, payload: &[u8]| SlruPageRow {
            kind: SlruKind::Clog,
            segno: 1,
            blknum: 0,
            lsn: Lsn(lsn),
            is_image,
            will_init,
            payload: Bytes::copy_from_slice(payload),
        };

        store
            .put_slru_pages(anc, &[slru_row(10, true, true, b"anc-img")])
            .await
            .unwrap();
        store
            .put_slru_pages(child, &[slru_row(30, false, false, b"child-delta")])
            .await
            .unwrap();

        let lineage = lineage2(child, anc, 25);
        let chain = store
            .read_slru_chain_lineage(&lineage, SlruKind::Clog, 1, 0, Lsn(100))
            .await
            .unwrap();
        assert_eq!(
            chain.iter().map(|r| r.lsn.0).collect::<Vec<_>>(),
            vec![30, 10]
        );

        // And the range-scan variant sees the ancestor-only block.
        let scanned = store
            .scan_slru_range_lineage(&lineage, SlruKind::Clog, 1, 0..4, Lsn(100))
            .await
            .unwrap();
        assert_eq!(
            scanned[&0].iter().map(|r| r.lsn.0).collect::<Vec<_>>(),
            vec![30, 10]
        );
        drop_schema(&store, &schema).await;
    }

    fn meta_key(suffix: u32) -> Vec<u8> {
        let mut buf = vec![0u8; KEY_SIZE];
        buf[0] = 0x60;
        buf[14..18].copy_from_slice(&suffix.to_be_bytes());
        buf
    }

    /// Meta lineage: nearest segment wins; ancestor rows above the
    /// branch point stay invisible.
    #[tokio::test]
    async fn meta_lineage_nearest_segment_wins() {
        let (store, schema) = make_store("lin_meta").await;
        let anc = rand_branch();
        let child = rand_branch();
        let shared = meta_key(1);
        let anc_late = meta_key(2); // only exists in anc, ABOVE the cap
        let anc_only = meta_key(3);

        store
            .put_meta(
                anc,
                &[
                    MetaRow {
                        key: Bytes::from(shared.clone()),
                        lsn: Lsn(10),
                        payload: Bytes::from_static(b"anc"),
                    },
                    MetaRow {
                        key: Bytes::from(anc_late.clone()),
                        lsn: Lsn(28),
                        payload: Bytes::from_static(b"anc-late"),
                    },
                    MetaRow {
                        key: Bytes::from(anc_only.clone()),
                        lsn: Lsn(12),
                        payload: Bytes::from_static(b"anc-only"),
                    },
                ],
            )
            .await
            .unwrap();
        store
            .put_meta(
                child,
                &[MetaRow {
                    key: Bytes::from(shared.clone()),
                    lsn: Lsn(30),
                    payload: Bytes::from_static(b"child"),
                }],
            )
            .await
            .unwrap();

        let lineage = lineage2(child, anc, 25);

        // Nearest segment wins for the shared key.
        let s = Key::from_slice(&shared);
        let out = store
            .scan_meta_range_lineage(&lineage, s..s.next(), Lsn(100))
            .await
            .unwrap();
        assert_eq!(out[shared.as_slice()].payload.as_ref(), b"child");

        // Below the child's version (lsn 30), the ancestor's value shows through.
        let out = store
            .scan_meta_range_lineage(&lineage, s..s.next(), Lsn(15))
            .await
            .unwrap();
        assert_eq!(out[shared.as_slice()].payload.as_ref(), b"anc");

        // Ancestor row above the branch point: invisible.
        let al = Key::from_slice(&anc_late);
        let out = store
            .scan_meta_range_lineage(&lineage, al..al.next(), Lsn(100))
            .await
            .unwrap();
        assert!(out.is_empty(), "lsn 28 > cap 25 must be invisible");

        // Range scan: shared key takes the child version, ancestor-only
        // key is discovered, capped key is absent.
        let start = Key::from_slice(&meta_key(0));
        let end = Key::from_slice(&meta_key(10));
        let out = store
            .scan_meta_range_lineage(&lineage, start..end, Lsn(100))
            .await
            .unwrap();
        assert_eq!(out.len(), 2);
        assert_eq!(out[shared.as_slice()].payload.as_ref(), b"child");
        assert_eq!(out[anc_only.as_slice()].payload.as_ref(), b"anc-only");
        drop_schema(&store, &schema).await;
    }

    /// `create_branch_row` validation matrix + the happy paths.
    #[tokio::test]
    async fn create_branch_row_validations() {
        let (store, schema) = make_store("branch_row").await;
        let anc = rand_branch();
        // Ancestor: consistent 90, gc cutoff 50, prev_record 85.
        let mut anc_row = sample_timeline_row(anc);
        anc_row.prev_record_lsn = Some(Lsn(85));
        store.insert_timeline_at_creation(&anc_row).await.unwrap();

        // Branch point above the durable head → rejected.
        let err = store
            .create_branch_row(rand_branch(), anc, Lsn(95), 1)
            .await
            .unwrap_err();
        assert!(err.to_string().contains("not yet durable"), "{err}");

        // Branch point below the gc cutoff → rejected.
        let err = store
            .create_branch_row(rand_branch(), anc, Lsn(40), 1)
            .await
            .unwrap_err();
        assert!(err.to_string().contains("gc cutoff"), "{err}");

        // Missing ancestor → rejected.
        let err = store
            .create_branch_row(rand_branch(), rand_branch(), Lsn(70), 1)
            .await
            .unwrap_err();
        assert!(err.to_string().contains("no timeline_state row"), "{err}");

        // Happy path mid-history: prev_record_lsn unknown → None.
        let child = rand_branch();
        let row = store
            .create_branch_row(child, anc, Lsn(70), 3)
            .await
            .unwrap();
        assert_eq!(row.consistent_lsn, Lsn(70), "walreceiver resume point");
        assert_eq!(row.ancestor_branch_id, Some(anc));
        assert_eq!(row.ancestor_lsn, Some(Lsn(70)));
        assert_eq!(row.current_generation, 3);
        assert_eq!(row.pg_version, anc_row.pg_version);
        assert_eq!(row.prev_record_lsn, None);
        let persisted = store.get_timeline(child).await.unwrap().unwrap();
        assert_eq!(persisted, row);

        // Happy path at the durable head: prev_record_lsn inherited.
        let child2 = rand_branch();
        let row = store
            .create_branch_row(child2, anc, Lsn(90), 1)
            .await
            .unwrap();
        assert_eq!(row.prev_record_lsn, Some(Lsn(85)));

        // Branching below the ancestor's own branch point → rejected.
        let grandchild_bad = rand_branch();
        let err = store
            .create_branch_row(grandchild_bad, child, Lsn(60), 1)
            .await
            .unwrap_err();
        assert!(err.to_string().contains("own branch point"), "{err}");

        // Deleted ancestor → rejected.
        let dead = rand_branch();
        let mut dead_row = sample_timeline_row(dead);
        dead_row.deleted_at = Some(chrono::Utc::now().naive_utc());
        store.insert_timeline_at_creation(&dead_row).await.unwrap();
        let err = store
            .create_branch_row(rand_branch(), dead, Lsn(70), 1)
            .await
            .unwrap_err();
        assert!(err.to_string().contains("deleted"), "{err}");

        drop_schema(&store, &schema).await;
    }

    // ------------------- tsclient-lane tests --------------------------------

    /// Attach the thin-RPC (`yb_tsclient`) lane to a clone of the shared
    /// store, connecting against dev YB's tserver RPC port
    /// (`YB_TEST_TSERVER_RPC`, default `127.0.0.1:9100`). Mirrors
    /// `TsClientTables::connect`, which is bound to the config-driven
    /// startup path.
    async fn tsclient_store() -> PageStore {
        let (mut store, _) = make_store("tsclient").await;
        let (db_oid, oids) = ysql_client::lookup_table_oids(
            &store.ysql_client,
            PROD_SCHEMA,
            &["pages", "slru_pages", "meta"],
        )
        .await
        .expect("lookup pageserver table OIDs");
        let tserver =
            std::env::var("YB_TEST_TSERVER_RPC").unwrap_or_else(|_| "127.0.0.1:9100".into());
        let client = yb_tsclient::TsClient::connect(yb_tsclient::TsClientConfig {
            tserver_addrs: vec![tserver],
            // Same write-session fan-out the startup path derives from
            // scale_count(4, 1.0), so these tests cover pooled writes.
            write_sessions: 4,
            ..Default::default()
        })
        .await
        .expect("connect tsclient lane to dev YB; set YB_TEST_TSERVER_RPC to override");
        let open = async |t: &str| {
            client
                .open_table(db_oid, oids[t])
                .await
                .unwrap_or_else(|e| panic!("open {t} via tsclient: {e}"))
        };
        store.tsclient = Some(Arc::new(TsClientTables {
            pages: open("pages").await,
            slru_pages: open("slru_pages").await,
            meta: open("meta").await,
            client,
        }));
        store
    }

    /// Sample count of each op served by the tsclient lane
    /// (`pageserver_yb_store_op_seconds{op, path="tsclient"}`, observed
    /// only on the lane's success arm). Lets a test assert an op actually
    /// ran on the lane: a dispatch bug that routed to YSQL instead would
    /// leave these flat while the YSQL cross-checks still passed.
    fn tsclient_op_count(op: &str) -> u64 {
        crate::metrics::YB_STORE_OP_SECONDS
            .with_label_values(&[op, "tsclient"])
            .get_sample_count()
    }

    /// Live tsclient-lane round trip (`hot_path = "tsclient"`): put
    /// pages/SLRU pages through the lane, then read chains, multi-block
    /// scans, and meta through it, cross-checking every result against
    /// the plain-YSQL store. A lane error surfaces as a test failure, and
    /// each op's lane sample count must advance so a silent YSQL routing
    /// can't pass unnoticed.
    #[tokio::test]
    async fn tsclient_lane_chain_scan_put_match_ysql() {
        let pg = tsclient_store().await;
        let (ysql, _) = make_store("tsclient_ysql").await;
        assert!(ysql.tsclient.is_none());
        let branch = rand_branch();
        let ops = [
            "put_pages",
            "put_slru_pages",
            "read_chain",
            "scan_range",
            "read_slru_chain",
            "scan_slru_range",
            "read_meta",
        ];
        let before: Vec<u64> = ops.iter().map(|op| tsclient_op_count(op)).collect();

        // Pages, written through the tsclient lane. Shapes cover the
        // anchor semantics normalize_chain must preserve:
        //   blk 0: image anchor + deltas above + shadowed history below;
        //   blk 1: same-LSN image/delta tie (image inserted LAST so
        //          storage order is worst-case delta-first);
        //   blk 2: no image — will_init delta anchor (slow path).
        let k0 = page_key(1663, 5, 16384, 0, 0);
        let k1 = page_key(1663, 5, 16384, 0, 1);
        let k2 = page_key(1663, 5, 16384, 0, 2);
        pg.put_pages(
            branch,
            &[
                page_row(k0.clone(), 5, false, true, b"k0-shadowed-init"),
                page_row(k0.clone(), 10, true, true, b"k0-img"),
                page_row(k0.clone(), 20, false, false, b"k0-d20"),
                page_row(k0.clone(), 30, false, false, b"k0-d30"),
                page_row(k1.clone(), 100, false, false, b"k1-wal-rec-100"),
                page_row(k1.clone(), 200, false, false, b"k1-d200"),
                page_row(k1.clone(), 100, true, true, b"k1-img-100"),
                page_row(k2.clone(), 20, false, true, b"k2-init"),
                page_row(k2.clone(), 30, false, false, b"k2-d30"),
            ],
        )
        .await
        .unwrap();

        // Chain reads: tsclient lane result must equal the YSQL result,
        // at a high target and at a mid-chain target.
        for k in [&k0, &k1, &k2] {
            for target in [Lsn(1000), Lsn(25)] {
                let via_pg = pg.read_chain(branch, k, target).await.unwrap();
                let via_ysql = ysql.read_chain(branch, k, target).await.unwrap();
                assert_eq!(via_pg, via_ysql, "read_chain key={k:?} target={target}");
            }
        }

        // Multi-block one-fork scan.
        let range = Key::from_slice(&k0)..Key::from_slice(&page_key(1663, 5, 16384, 0, 3));
        let via_pg = pg
            .scan_range(branch, range.clone(), Lsn(1000))
            .await
            .unwrap();
        let via_ysql = ysql.scan_range(branch, range, Lsn(1000)).await.unwrap();
        assert_eq!(via_pg, via_ysql, "scan_range");
        assert_eq!(via_pg.len(), 3);

        // SLRU: put through the lane, then chain + range scan.
        let slru_row =
            |blk: u32, lsn: u64, is_image: bool, will_init: bool, pl: &[u8]| SlruPageRow {
                kind: SlruKind::Clog,
                segno: 1,
                blknum: blk,
                lsn: Lsn(lsn),
                is_image,
                will_init,
                payload: Bytes::copy_from_slice(pl),
            };
        pg.put_slru_pages(
            branch,
            &[
                slru_row(0, 10, true, true, b"s0-img"),
                slru_row(0, 20, false, false, b"s0-d20"),
                slru_row(1, 30, false, true, b"s1-init"),
                slru_row(1, 40, false, false, b"s1-d40"),
            ],
        )
        .await
        .unwrap();
        let via_pg = pg
            .read_slru_chain(branch, SlruKind::Clog, 1, 0, Lsn(1000))
            .await
            .unwrap();
        let via_ysql = ysql
            .read_slru_chain(branch, SlruKind::Clog, 1, 0, Lsn(1000))
            .await
            .unwrap();
        assert_eq!(via_pg, via_ysql, "read_slru_chain");
        let via_pg = pg
            .scan_slru_range(branch, SlruKind::Clog, 1, 0..4, Lsn(1000))
            .await
            .unwrap();
        let via_ysql = ysql
            .scan_slru_range(branch, SlruKind::Clog, 1, 0..4, Lsn(1000))
            .await
            .unwrap();
        assert_eq!(via_pg, via_ysql, "scan_slru_range");
        assert_eq!(via_pg.len(), 2);

        // Meta: writes stay on YSQL (put_meta has no hot-path lane);
        // the single-key read runs on the lane.
        let mk = meta_key(7);
        ysql.put_meta(
            branch,
            &[
                MetaRow {
                    key: Bytes::from(mk.clone()),
                    lsn: Lsn(10),
                    payload: Bytes::from_static(b"m10"),
                },
                MetaRow {
                    key: Bytes::from(mk.clone()),
                    lsn: Lsn(20),
                    payload: Bytes::from_static(b"m20"),
                },
            ],
        )
        .await
        .unwrap();
        for target in [Lsn(1000), Lsn(15), Lsn(5)] {
            let via_pg = pg.read_meta(branch, &mk, target).await.unwrap();
            let via_ysql = ysql.read_meta(branch, &mk, target).await.unwrap();
            assert_eq!(via_pg, via_ysql, "read_meta target={target}");
        }

        // Every op above ran on the tsclient lane, not silently on YSQL:
        // its lane sample count advanced.
        for (op, b) in ops.iter().zip(before) {
            assert!(
                tsclient_op_count(op) > b,
                "op {op} did not run on the tsclient lane"
            );
        }
    }

    /// The tsclient write path must chunk like the YSQL lane: a
    /// batch above `ROWS_PER_INSERT` lands fully via multiple Performs,
    /// and a paged chain read (> one 32-row page) walks the paging
    /// envelope on one snapshot.
    #[tokio::test]
    async fn tsclient_lane_chunks_and_pages() {
        let pg = tsclient_store().await;
        let (ysql, _) = make_store("tsclient_chunks_ysql").await;
        let branch = rand_branch();
        let (put_before, read_before) = (
            tsclient_op_count("put_pages"),
            tsclient_op_count("read_chain"),
        );

        // 2 full chunks + change, one row per block.
        let n: u32 = 1100;
        let mut rows = Vec::with_capacity(n as usize);
        for blk in 0..n {
            let k = page_key(1663, 5, 16385, 0, blk);
            rows.push(page_row(k, 10, true, true, format!("b{blk}").as_bytes()));
        }
        // Plus one deep chain on a single block: an image at 10 under
        // 60 deltas — deeper than DIRECT_CHAIN_PAGE_ROWS, so the chain
        // read must follow the paging state.
        let deep = page_key(1663, 5, 16385, 1, 0);
        rows.push(page_row(deep.clone(), 10, true, true, b"deep-img"));
        for i in 1..=60u64 {
            rows.push(page_row(
                deep.clone(),
                10 + i,
                false,
                false,
                format!("d{i}").as_bytes(),
            ));
        }
        pg.put_pages(branch, &rows).await.unwrap();

        for blk in [0u32, n / 2, n - 1] {
            let k = page_key(1663, 5, 16385, 0, blk);
            let chain = pg.read_chain(branch, &k, Lsn(100)).await.unwrap();
            assert_eq!(chain.len(), 1, "blk {blk}");
            assert_eq!(chain[0].payload.as_ref(), format!("b{blk}").as_bytes());
        }
        let via_pg = pg.read_chain(branch, &deep, Lsn(1000)).await.unwrap();
        let via_ysql = ysql.read_chain(branch, &deep, Lsn(1000)).await.unwrap();
        assert_eq!(via_pg.len(), 61);
        assert_eq!(via_pg, via_ysql, "paged deep-chain read");

        // Both put_pages and the chain reads ran on the tsclient lane.
        assert!(
            tsclient_op_count("put_pages") > put_before,
            "put_pages did not run on the tsclient lane"
        );
        assert!(
            tsclient_op_count("read_chain") > read_before,
            "read_chain did not run on the tsclient lane"
        );
    }

    /// `load_lineage` walks ancestor links with running-min caps.
    #[tokio::test]
    async fn load_lineage_walks_ancestor_links() {
        let (store, schema) = make_store("lin_load").await;
        let grandparent = rand_branch();
        let parent = rand_branch();
        let child = rand_branch();

        store
            .insert_timeline_at_creation(&sample_timeline_row(grandparent))
            .await
            .unwrap();
        let mut parent_row = sample_timeline_row(parent);
        parent_row.ancestor_branch_id = Some(grandparent);
        parent_row.ancestor_lsn = Some(Lsn(50));
        store
            .insert_timeline_at_creation(&parent_row)
            .await
            .unwrap();
        let mut child_row = sample_timeline_row(child);
        child_row.ancestor_branch_id = Some(parent);
        child_row.ancestor_lsn = Some(Lsn(70));
        store.insert_timeline_at_creation(&child_row).await.unwrap();

        let lineage = store.load_lineage(child).await.unwrap();
        assert_eq!(
            lineage,
            vec![
                LineageSegment {
                    branch_id: child,
                    cap_lsn: Lsn::MAX
                },
                LineageSegment {
                    branch_id: parent,
                    cap_lsn: Lsn(70)
                },
                LineageSegment {
                    branch_id: grandparent,
                    cap_lsn: Lsn(50)
                },
            ]
        );

        // A root timeline gets the trivial single-segment lineage.
        let root_lineage = store.load_lineage(grandparent).await.unwrap();
        assert_eq!(root_lineage, LineageSegment::single(grandparent));

        // Missing row → error, not an empty lineage.
        let err = store.load_lineage(rand_branch()).await.unwrap_err();
        assert!(err.to_string().contains("no timeline_state row"), "{err}");

        drop_schema(&store, &schema).await;
    }
}
