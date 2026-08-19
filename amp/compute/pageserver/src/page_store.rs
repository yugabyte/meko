//! `PageStore` — YugabyteDB row store for **relation pages only**.
//!
//! This is pageserver's replacement for the upstream layer-file
//! machinery, but **only for relation-page keys** — i.e. keys where
//! `Key::is_rel_block_key()` holds (`field1 == 0x00`, `field4 != 0`,
//! `field6 != 0xffffffff`). Non-page keys (SLRUs, metadata, dir
//! listings, aux files, replication origins, rel-size sentinels)
//! continue to flow through the existing layer-file path in
//! `amp_storage.blobs`.
//!
//! Schema:
//! ```sql
//! CREATE TABLE amp_storage.pages (
//!     bucket_id   SMALLINT NOT NULL,
//!     branch_id   uuid     NOT NULL,
//!     spcnode     BIGINT   NOT NULL,
//!     dbnode      BIGINT   NOT NULL,
//!     relnode     BIGINT   NOT NULL,
//!     forknum     SMALLINT NOT NULL,
//!     blknum      BIGINT   NOT NULL,
//!     is_image    BOOLEAN  NOT NULL,
//!     lsn         BIGINT   NOT NULL,
//!     will_init   BOOLEAN  NOT NULL,
//!     payload     BYTEA    NOT NULL,
//!     PRIMARY KEY (bucket_id ASC, branch_id ASC, spcnode ASC, dbnode ASC,
//!                  relnode ASC, forknum ASC, blknum ASC,
//!                  lsn DESC, is_image DESC)
//! ) SPLIT AT VALUES ((1), (2), (3), (4), (5));
//! ```
//!
//! The key is all-range, led by the [`PAGE_BUCKETS`]-way placement
//! bucket [`page_bucket_id`], so the table pre-splits into exactly
//! `PAGE_BUCKETS` tablets and writes spread from the first row. Within a
//! bucket the key is clustered as before, so a block's chain is still
//! one contiguous run and a per-fork scan is still a range scan.
//!
//! Because the bucket is `(… + blknum / PAGE_BUCKET_STRIDE) % PAGE_BUCKETS`,
//! a read whose blknum is known (a chain read) binds the whole key prefix
//! and touches one tablet, while a blknum *range* scan touches one bucket
//! per 2MB stretch it spans, capped at `PAGE_BUCKETS`. Every query here
//! binds bucket_id as a single VALUE and callers that span buckets loop.
//! The thin lane has no alternative — [`yb_thin_client::RangeRead::new_range`]
//! binds one value per key column, and the shim's `range_values` ABI has no
//! set form. YSQL could equally write `bucket_id = ANY(…)`, which YB does
//! push into the index condition; both lanes use the one-bucket-per-query
//! shape so the two read alike.
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
use pageserver_api::key::{KEY_SIZE, Key, rel_size_to_key};
use pageserver_api::models::PayloadCompression;
use pageserver_api::reltag::{RelTag, SlruKind};

use remote_storage::{YbConnConfig, YsqlClient};
// We talk to YugabyteDB through the YB smart-driver fork of
// tokio-postgres; alias it so the type names below (`tokio_postgres::Row`,
// `tokio_postgres::types::ToSql`, etc.) keep working without
// per-callsite churn. The Neon fork of tokio-postgres survives
// elsewhere in the workspace for the walproposer/walreceiver
// replication path.
use utils::id::BranchId;
use utils::lsn::Lsn;
use utils::payload_codec;
use yb_tokio_postgres as tokio_postgres;
use yb_tokio_postgres::types::ToSql;

/// One row in `amp_storage.pages`. `key` is the relation-page
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

/// One row in `amp_storage.meta`. The key is the 18-byte BYTEA encoding
/// of any `Key` that isn't a relation-page or SLRU-block (those have
/// their own tables). Full-overwrite semantics: read = latest row at
/// LSN ≤ target.
#[derive(Debug, Clone, PartialEq, Eq)]
pub struct MetaRow {
    pub key: Bytes,
    pub lsn: Lsn,
    pub payload: Bytes,
}

/// One row in `amp_storage.slru_pages`.
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

/// The controller-seeded part of a branch's `branch_state` row — what the
/// controller records at branch create, before any daemon has written its own
/// state. Attach reads this to decide how to create the timeline: a NULL
/// ancestor means bootstrap (initdb + import at `pg_version`), a set one means
/// branch off that ancestor at `ancestor_lsn`.
#[derive(Debug, Clone, PartialEq, Eq)]
pub struct BranchSeed {
    /// Full `PgVersionId` (e.g. 180002); divide by 10000 for the major version.
    pub pg_version: u32,
    pub ancestor_branch_id: Option<BranchId>,
    pub ancestor_lsn: Option<Lsn>,
}

/// How attach should create a branch's timeline, derived from its
/// [`BranchSeed`]. The two ancestor columns are written together by the
/// controller, so this collapses them into the one decision attach makes.
#[derive(Debug, Clone, PartialEq, Eq)]
pub enum BranchInitMode {
    /// No ancestor: run initdb and import at this major version.
    Bootstrap { pg_major: u32 },
    /// Branch off `ancestor_branch_id` at `ancestor_lsn`.
    Branch {
        ancestor_branch_id: BranchId,
        ancestor_lsn: Lsn,
    },
}

impl BranchSeed {
    /// PG major version, from the full `PgVersionId` the column holds.
    pub fn pg_major(&self) -> u32 {
        self.pg_version / 10000
    }

    /// Decide how to create the timeline. Errors only if the controller wrote a
    /// half-populated ancestry, which it never does — the two columns are set in
    /// the same INSERT.
    pub fn init_mode(&self) -> Result<BranchInitMode> {
        match (self.ancestor_branch_id, self.ancestor_lsn) {
            (None, _) => Ok(BranchInitMode::Bootstrap {
                pg_major: self.pg_major(),
            }),
            (Some(ancestor_branch_id), Some(ancestor_lsn)) => Ok(BranchInitMode::Branch {
                ancestor_branch_id,
                ancestor_lsn,
            }),
            (Some(_), None) => {
                anyhow::bail!("branch_state seed has an ancestor_branch_id but no ancestor_lsn")
            }
        }
    }
}

#[derive(Debug, Clone, PartialEq, Eq)]
pub struct BranchStateRow {
    pub branch_id: BranchId,
    /// Highest LSN durably present in `amp_storage.pages`/`slru_pages`/`meta`.
    /// Advances on every successful `PageWriter::flush`. Drives both
    /// safekeeper WAL truncation (SK keeps WAL ≥ this) and ingest
    /// resume on restart (walreceiver replays from this LSN forward).
    pub consistent_lsn: Lsn,
    pub applied_gc_cutoff_lsn: Lsn,
    pub ancestor_branch_id: Option<BranchId>,
    pub ancestor_lsn: Option<Lsn>,
    /// Full `PgVersionId` (e.g. 180002), not a major version — divide by 10000
    /// for the PG major version.
    ///
    /// Read-only for both daemons: the controller seeds this column at branch
    /// create and is its only writer, so [`PageStore::update_branch_initial_state`]
    /// ignores whatever this field holds.
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
}

pub const DEFAULT_SCHEMA: &str = "amp_storage";

#[derive(Debug, Clone)]
pub struct PageStoreConfig {
    pub yb: YbConnConfig,
    pub schema: String,
    /// Multiplier applied to the per-statement INSERT fan-out
    /// (`put_pages` / `put_slru_pages` / `put_meta`). Plumbed through
    /// from `PageServerConf::scale_factor`; tests default to 1.0.
    pub scale_factor: f64,
    /// Codec the `put_*` paths wrap payloads with. Write-side only —
    /// reads decode whatever tag each row carries. Plumbed through from
    /// `PageServerConf::yb_payload_compression`.
    pub payload_compression: PayloadCompression,
}

impl PageStoreConfig {
    pub fn new(yb: YbConnConfig) -> Self {
        Self {
            yb,
            schema: DEFAULT_SCHEMA.to_string(),
            scale_factor: pageserver_api::config::DEFAULT_SCALE_FACTOR,
            payload_compression: pageserver_api::config::defaults::DEFAULT_YB_PAYLOAD_COMPRESSION,
        }
    }

    pub fn with_scale_factor(mut self, scale_factor: f64) -> Self {
        self.scale_factor = scale_factor;
        self
    }

    pub fn with_payload_compression(mut self, codec: PayloadCompression) -> Self {
        self.payload_compression = codec;
        self
    }
}

#[derive(Clone)]
pub struct PageStore {
    ysql_client: Arc<YsqlClient>,
    schema: Arc<str>,
    scale_factor: f64,
    /// Codec applied to `payload` on the way *out* to YB. Deliberately
    /// not consulted on the read path: every stored value carries its own
    /// codec tag, so a chain may freely mix rows written under different
    /// settings and a config change needs no migration.
    payload_codec: payload_codec::Codec,
    /// Full-chunk INSERT SQL with the `branch_id` baked in as a
    /// literal, memoized per `branch_id` on first flush and
    /// shared across clones. In production this holds exactly one entry
    /// (a process serves a single branch, #16); keying the map
    /// rather than asserting a single slot also lets the shared-store
    /// test harness drive many branches through one `PageStore`.
    baked: Arc<DashMap<BranchId, Arc<BakedSql>>>,
    /// Direct-RPC lane (`hot_path = "yb_thin_client"`, the default):
    /// `yb_thin_client` handles over the `libyb_thin_client` C++ shim for the
    /// tables this store owns, batching reads/writes into one Perform
    /// per call. `None` on the YSQL path; when set, per-op dispatch takes this
    /// lane and falls back to YSQL. Populated and health-checked at startup.
    ///
    /// Held through a [`yb_thin_client::Recycling`], which re-connects the shim
    /// client and re-opens these tables once the current generation passes
    /// `YbConnConfig::connection_lifetime_ms` — re-resolving the configured
    /// tserver addresses, so a connection cannot outlive the tservers it was
    /// aimed at (#811). Each dispatch site takes the current generation for
    /// the duration of its call.
    pub thinclient: Option<Arc<ThinClientPool>>,
}

/// The recycled holder of [`ThinClientTables`] — see [`PageStore::thinclient`].
pub type ThinClientPool = yb_thin_client::Recycling<ThinClientTables>;

/// Opened `yb_thin_client` table handles (the batch-first Perform client).
/// One generation of [`ThinClientPool`]: the handles all belong to `client`,
/// so they are replaced together. See [`PageStore::thinclient`].
pub struct ThinClientTables {
    pub client: Arc<yb_thin_client::TsClient>,
    pub pages: Arc<yb_thin_client::TableHandle>,
    pub slru_pages: Arc<yb_thin_client::TableHandle>,
    pub meta: Arc<yb_thin_client::TableHandle>,
    /// Candidate *inserts* only. The GC drain's select/delete/dequeue
    /// stay on YSQL — they run on their own `pageserver:gc` pool and
    /// have no thin lane.
    pub pending_gc: Arc<yb_thin_client::TableHandle>,
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

/// Rows per fetched page on direct-RPC multi-block range scans.
const DIRECT_SCAN_PAGE_ROWS: u64 = 128;

/// Whole-scan restarts for the batched thin-lane page scan
/// ([`PageStore::scan_one_fork_thin`]). A read-restart on a *pinned* batch —
/// which every paging round after the first is — cannot be healed inside
/// `TsClient::read`, so the scan driver retries the whole fan-out on a fresh
/// read point. Mirrors the client's own `MAX_SCAN_RESTARTS`.
const THIN_SCAN_RESTARTS: usize = 5;

/// Consecutive empty-but-continuing pages tolerated per op before the scan
/// fails loudly. Deliberately NOT a cap on total rounds: a legitimate large
/// scan needs unboundedly many, so a round cap would false-fail it. An op
/// crossing tablets that hold no matching row returns empty pages with a
/// continuation, which is normal for a handful of rounds; only a token that
/// never advances does it indefinitely (an earlier shim did exactly that and
/// spun at ~35% CPU forever).
const THIN_SCAN_BARREN_ROUNDS: usize = 64;

/// Read-side cell access for the hot-path client's row type
/// (`yb_thin_client::Row`), keeping the chain/scan decode helpers below
/// generic over the row rather than the client. The client's `Value`
/// accessors return its `YbError`, which folds into `anyhow`.
trait RowCells {
    fn cell_i64(&self, i: usize) -> Result<i64>;
    fn cell_bool(&self, i: usize) -> Result<bool>;
    fn cell_bytes(&self, i: usize) -> Result<Bytes>;
}

impl RowCells for yb_thin_client::Row {
    fn cell_i64(&self, i: usize) -> Result<i64> {
        Ok(self[i].as_i64()?)
    }
    fn cell_bool(&self, i: usize) -> Result<bool> {
        Ok(self[i].as_bool()?)
    }
    fn cell_bytes(&self, i: usize) -> Result<Bytes> {
        Ok(self[i].as_bytes()?.clone())
    }
}

/// [`int_bind_pg`] for the `yb_thin_client` lane.
fn int_bind_thin(col: &yb_thin_client::Column, v: i64) -> yb_thin_client::BindValue {
    match col.value_type {
        yb_thin_client::ValueType::I16 => yb_thin_client::BindValue::I16(v as i16),
        yb_thin_client::ValueType::I32 => yb_thin_client::BindValue::I32(v as i32),
        _ => yb_thin_client::BindValue::I64(v),
    }
}

/// The shared prefix of every thin `slru_pages` read: `branch_id` is the
/// whole hash key; kind/segno are range columns, bound as equality scan
/// conds. Callers append their own `blknum`/`lsn` conds and targets.
fn slru_segment_read(
    slru: &yb_thin_client::TableHandle,
    branch_id: BranchId,
    kind_i: i16,
    segno_i: i64,
) -> Result<yb_thin_client::RangeRead> {
    use yb_thin_client::{BindValue, CondOp, RangeRead};
    Ok(
        RangeRead::new(slru, vec![BindValue::Bytes(branch_id.to_bytes())])?
            .cond(
                slru,
                "kind",
                CondOp::Eq,
                int_bind_thin(slru.col("kind")?, kind_i as i64),
            )?
            .cond(
                slru,
                "segno",
                CondOp::Eq,
                int_bind_thin(slru.col("segno")?, segno_i),
            )?,
    )
}

/// Normalize a chain fetched in DocDB storage order into SQL result
/// order and trim it to the served prefix.
///
/// The pages/slru PKs cluster `(lsn DESC, is_image ASC)`, so a direct-RPC
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

/// Rows per fetched page on direct-RPC single-key chain reads. Sized so
/// the common case (chain bounded by `image_chain_limit` = 20 plus a
/// little materialiser lag) is one round trip.
const DIRECT_CHAIN_PAGE_ROWS: u64 = 32;

/// Rows per fetched page on direct-RPC single-key SLRU chain reads. Taller
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
struct ChainFetchState<R> {
    raw: Vec<R>,
    anchor_lsn: Option<i64>,
    lsn_idx: usize,
    will_init_idx: usize,
}

impl<R: RowCells> ChainFetchState<R> {
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
    fn push(&mut self, row: R) -> Result<bool> {
        let lsn = row.cell_i64(self.lsn_idx)?;
        if let Some(anchor) = self.anchor_lsn {
            if lsn < anchor {
                return Ok(true);
            }
        }
        if self.anchor_lsn.is_none() && row.cell_bool(self.will_init_idx)? {
            self.anchor_lsn = Some(lsn);
        }
        self.raw.push(row);
        Ok(false)
    }
}

/// Paged single-key chain fetch on the direct-RPC lane; the anchor-stop
/// logic lives in [`ChainFetchState`].
async fn fetch_chain_rows_thin(
    client: &yb_thin_client::TsClient,
    table: &Arc<yb_thin_client::TableHandle>,
    read: yb_thin_client::RangeRead,
    lsn_idx: usize,
    will_init_idx: usize,
) -> Result<Vec<yb_thin_client::Row>> {
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

/// Per-`put_*` accumulation of what the codec did, held until the YB write
/// succeeds.
///
/// The counters are deliberately NOT bumped at encode time. `PageWriter`
/// leaves its buffer intact when a flush fails and re-submits the same rows
/// on the next attempt, so encode-time accounting would count a row once per
/// attempt — inflating the totals exactly during a YB incident, when the
/// graphs matter most. It would also contradict the metrics' own help text
/// ("bytes written to YB") and diverge from `YB_STORE_OP_SECONDS`, which is
/// only observed on success.
///
/// The compression *ratio* is unaffected either way (logical and stored
/// inflate together); it is the absolute volumes this protects.
struct PayloadTally {
    table: &'static str,
    logical: usize,
    stored: usize,
    /// Row counts per `YB_PAYLOAD_ROWS` outcome label.
    lz4: u64,
    raw_configured: u64,
    raw_too_small: u64,
    raw_incompressible: u64,
}

impl PayloadTally {
    fn new(table: &'static str) -> Self {
        Self {
            table,
            logical: 0,
            stored: 0,
            lz4: 0,
            raw_configured: 0,
            raw_too_small: 0,
            raw_incompressible: 0,
        }
    }

    fn add(&mut self, logical: usize, stored: usize, outcome: payload_codec::Outcome) {
        use payload_codec::{Codec, Outcome, RawReason};
        self.logical += logical;
        self.stored += stored;
        match outcome {
            Outcome::Compressed(Codec::Lz4) => self.lz4 += 1,
            // `encode` cannot report Raw as a *compressed* outcome, but
            // spelling the arm out keeps this exhaustive if a codec is
            // added later.
            Outcome::Compressed(Codec::Raw) | Outcome::Raw(RawReason::Configured) => {
                self.raw_configured += 1
            }
            Outcome::Raw(RawReason::TooSmall) => self.raw_too_small += 1,
            Outcome::Raw(RawReason::Incompressible) => self.raw_incompressible += 1,
        }
    }

    /// Publish. Call only after the YB write has succeeded.
    fn record(&self) {
        crate::metrics::YB_PAYLOAD_LOGICAL_BYTES
            .with_label_values(&[self.table])
            .inc_by(self.logical as u64);
        crate::metrics::YB_PAYLOAD_STORED_BYTES
            .with_label_values(&[self.table])
            .inc_by(self.stored as u64);
        for (label, n) in [
            ("lz4", self.lz4),
            ("raw_configured", self.raw_configured),
            ("raw_too_small", self.raw_too_small),
            ("raw_incompressible", self.raw_incompressible),
        ] {
            if n > 0 {
                crate::metrics::YB_PAYLOAD_ROWS
                    .with_label_values(&[self.table, label])
                    .inc_by(n);
            }
        }
    }
}

/// Unwrap a stored `payload` envelope, naming the row in any error.
///
/// Every read path funnels through here, and it is deliberately
/// independent of `PageStore::payload_codec`: the envelope's tag says how
/// the row was written, so rows produced under different settings — even
/// within one page's delta chain — all decode with one reader. That is what
/// makes changing the write codec a restart rather than a migration.
///
/// `what` is a **closure**, not a string: a chain read decodes an image
/// plus every delta above it, so an eagerly-built context string would
/// allocate (and `Display` a `Key`'s six hex fields) once per row on the
/// getpage path, for a message only ever read when decoding fails.
fn decode_payload<C, F>(stored: &Bytes, what: F) -> Result<Bytes>
where
    F: FnOnce() -> C,
    C: std::fmt::Display,
{
    payload_codec::decode(stored).with_context(|| {
        format!(
            "{}: decoding stored payload ({} bytes, codec {})",
            what(),
            stored.len(),
            payload_codec::stored_codec_name(stored),
        )
    })
}

/// Decode the raw rows of a single-key chain fetch (targets
/// `[.., lsn, is_image, will_init, payload]` starting at `off`) into
/// [`PageRow`]s for `key`, normalize storage order into SQL result
/// order, and trim at the anchor.
fn assemble_page_chain<R: RowCells>(
    key: Key,
    raw: Vec<R>,
    off: usize,
    where_: &str,
) -> Result<Vec<Arc<PageRow>>> {
    let rows = raw
        .into_iter()
        .map(|r| {
            let lsn = Lsn(r.cell_i64(off)? as u64);
            Ok(PageRow {
                key,
                lsn,
                is_image: r.cell_bool(off + 1)?,
                will_init: r.cell_bool(off + 2)?,
                payload: decode_payload(&r.cell_bytes(off + 3)?, || {
                    format!("pages row key={key} lsn={lsn} ({where_})")
                })?,
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
fn assemble_slru_chain<R: RowCells>(
    kind: SlruKind,
    segno: u32,
    blknum: u32,
    raw: Vec<R>,
    off: usize,
    where_: &str,
) -> Result<Vec<SlruPageRow>> {
    let rows = raw
        .into_iter()
        .map(|r| {
            let lsn = Lsn(r.cell_i64(off)? as u64);
            Ok(SlruPageRow {
                kind,
                segno,
                blknum,
                lsn,
                is_image: r.cell_bool(off + 1)?,
                will_init: r.cell_bool(off + 2)?,
                payload: decode_payload(&r.cell_bytes(off + 3)?, || {
                    format!(
                        "slru_pages row kind={kind:?} segno={segno} \
                             blknum={blknum} lsn={lsn} ({where_})"
                    )
                })?,
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
/// collector. Used by the direct-RPC scan loops.
fn feed_scan_rows<R: RowCells>(collector: &mut BlockChainCollector<R>, rows: Vec<R>) -> Result<()> {
    for row in rows {
        let blk = row.cell_i64(0)?;
        let lsn = row.cell_i64(1)?;
        let will_init = row.cell_bool(3)?;
        collector.push(blk, lsn, will_init, row);
    }
    Ok(())
}

/// Assemble a one-fork multi-block scan's collected per-block chains
/// into the keyed map [`PageStore::scan_range`] returns. Used by the
/// direct-RPC lane.
fn assemble_page_scan<R: RowCells>(
    sp: i64,
    db: i64,
    rel: i64,
    fork: i16,
    collector: BlockChainCollector<R>,
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
/// Used by the direct-RPC lane.
fn decode_meta_row<R: RowCells>(key: &[u8], r: &R) -> Result<MetaRow> {
    let lsn = Lsn(r.cell_i64(0)? as u64);
    Ok(MetaRow {
        key: Bytes::copy_from_slice(key),
        lsn,
        payload: decode_payload(&r.cell_bytes(1)?, || {
            format!("meta row key={} lsn={lsn}", hex::encode(key))
        })?,
    })
}

/// [`assemble_page_scan`] for SLRU rows, keyed by blknum.
fn assemble_slru_scan<R: RowCells>(
    kind: SlruKind,
    segno: u32,
    collector: BlockChainCollector<R>,
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

impl ThinClientTables {
    /// Connects the `yb_thin_client` (shim) client and opens the store's
    /// tables — the startup healthcheck for `hot_path = "yb_thin_client"`.
    /// `connect`/`open_table` are synchronous (a one-shot startup RPC).
    ///
    /// Returns the recycling holder rather than the tables themselves: the
    /// catalog lookup that resolves the table OIDs happens once here (OIDs
    /// don't move), while the connect + `open_table` calls are packaged into a
    /// closure the holder re-runs on each lifetime-driven recycle.
    async fn connect(
        yb: &YbConnConfig,
        schema: &str,
        ysql_client: &Arc<YsqlClient>,
        read_sessions: usize,
        write_sessions: usize,
        sessions_per_conn: usize,
    ) -> Result<Arc<ThinClientPool>> {
        let (db_oid, oids) = ysql_client::lookup_table_oids(
            ysql_client,
            schema,
            &["pages", "slru_pages", "meta", "pending_gc"],
        )
        .await?;
        let tserver_addrs = yb.effective_tserver_rpc_addrs();
        let tserver_count = tserver_addrs.len();
        let config = yb_thin_client::TsClientConfig {
            tserver_addrs,
            read_sessions: read_sessions as u32,
            write_sessions: write_sessions as u32,
            sessions_per_conn: sessions_per_conn as u32,
            ..Default::default()
        };
        let schema_owned = schema.to_string();
        let pool = yb_thin_client::Recycling::new(
            "pageserver",
            config,
            yb.max_connection_lifetime(),
            move |client: Arc<yb_thin_client::TsClient>| {
                let open = |table: &str| {
                    client.open_table(db_oid, oids[table]).map_err(|e| {
                        yb_thin_client::YbError::Other(format!(
                            "opening {schema_owned}.{table} via the thin-client lane: {e}"
                        ))
                    })
                };
                Ok(Arc::new(ThinClientTables {
                    pages: open("pages")?,
                    slru_pages: open("slru_pages")?,
                    meta: open("meta")?,
                    pending_gc: open("pending_gc")?,
                    client,
                }))
            },
        )
        .await
        .context("connecting the yb_thin_client")?;
        tracing::info!(
            "yb_thin_client hot path ready: {schema}.pages/slru_pages/meta/pending_gc opened \
             ({read_sessions} read + {write_sessions} write sessions, {sessions_per_conn} per \
             connection, across {tserver_count} configured tserver(s))",
        );
        Ok(pool)
    }
}

/// Decompose an 18-byte relation-page Key into the typed columns.
/// Returns `Err` if the bytes don't parse as `field1 = 0` (the
/// relation-page namespace).
fn key_columns(key: &Key) -> Result<(i64, i64, i64, i16, i64)> {
    if !key.is_rel_block_key() {
        bail!(
            "PageStore: key {key} is not a relation block (field1=0x{:02x}, field4={}, field6=0x{:x}); \
             non-page keys belong in the layer-file path, not amp_storage.pages",
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
/// is `pages` at 10 params/row → 500×10 = 5000 params, well under.
/// Chunks dispatch in parallel — tokio-postgres pipelines them on the
/// shared connection so YB absorbs them concurrently.
const ROWS_PER_INSERT: usize = 500;

/// Rows per chunk in `put_meta`, on both lanes. Larger than
/// [`ROWS_PER_INSERT`] because meta binds only 4 params/row (2000×4 =
/// 8000, still well under PostgreSQL's 65535 cap) and its payloads are
/// mostly tiny rel-size sentinels. Shared by the YSQL and direct-RPC
/// lanes so a batch splits identically on both.
const META_ROWS_PER_INSERT: usize = 2000;

/// Placement buckets in `amp_storage.pending_gc`'s hash key; must match
/// the drain's per-bucket fan-out.
pub const GC_BUCKETS: i32 = 3;

/// Concurrent GC action DELETEs per drain tick (scaled); the dedicated
/// `pageserver:gc` pool is sized to this.
pub const GC_ACTION_PARALLELISM: usize = 1;

/// Encode a `Key` to its fixed 18-byte wire form — the `pending_gc.key`
/// (and `meta.key`) bytea.
pub(crate) fn key_to_bytes(key: &Key) -> Vec<u8> {
    let mut b = vec![0u8; KEY_SIZE];
    key.write_to_byte_slice(&mut b);
    b
}

/// FNV-1a over the key's 18 encoded bytes. FROZEN: must stay a fixed,
/// deterministic function of them (golden-value test) — changing it strands
/// existing candidates in their old buckets and breaks crash-replay dedup.
/// Deliberately not `std::hash` (unstable across Rust versions) and not a YB
/// generated column (the yb-client write path bypasses the YSQL layer). Kept
/// separate from the modulus so the test can pin it independently of
/// [`GC_BUCKETS`], which is not frozen while no deployment holds candidates.
fn gc_key_hash(key: &Key) -> u64 {
    let mut kb = [0u8; KEY_SIZE];
    key.write_to_byte_slice(&mut kb);
    let mut h: u64 = 0xcbf2_9ce4_8422_2325;
    for b in kb {
        h ^= b as u64;
        h = h.wrapping_mul(0x0000_0100_0000_01b3);
    }
    h
}

/// Placement bucket for a candidate.
pub fn gc_bucket_id(key: &Key) -> i32 {
    (gc_key_hash(key) % GC_BUCKETS as u64) as i32
}

/// The `amp_storage.pages` placement bucket, shared with the operator tools
/// (`dump-pages` pins its queries with the same function). Defined once in
/// `pageserver_api` — a second copy of a frozen key function is how rows go
/// missing.
pub use pageserver_api::pages_bucket::{
    PAGE_BUCKET_STRIDE, PAGE_BUCKETS, page_bucket_id, page_buckets_for_range,
};

/// The `meta` key bytes of a fork's rel-size entry (`rel_size_to_key`),
/// from a fork's i64/i16 identity columns.
fn rel_size_key_bytes(spcnode: i64, dbnode: i64, relnode: i64, forknum: i16) -> Vec<u8> {
    let rel = RelTag {
        spcnode: spcnode as u32,
        dbnode: dbnode as u32,
        relnode: relnode as u32,
        forknum: forknum as u8,
    };
    key_to_bytes(&rel_size_to_key(rel))
}

/// GC candidate kind — the `pending_gc.gc_kind` column. Each variant maps
/// 1:1 to the act function it dispatches to:
/// `TrimPages`→`gc_act_trim_blocks`, `TrimSlru`→`gc_act_trim_slru`,
/// `TrimMeta`→`gc_act_trim_meta`, `ShrinkPages`→`gc_act_shrink_fork`
/// (rel-only), `DropPages`→`gc_act_drop_fork`, `DropSlru`→
/// `gc_act_drop_slru`, `DropMeta`→`gc_act_drop_meta`. The emitter, which
/// knows the target table, stamps the kind — the key stays pure identity.
///
/// Stored as a `SMALLINT` via [`Self::to_i16`]/[`Self::from_i16`] (wire
/// values 0..=6, frozen). A value this version doesn't recognise decodes
/// to [`GcKind::Unknown`], which the drain alarms on and leaves queued.
#[derive(Debug, Clone, Copy, PartialEq, Eq, PartialOrd, Ord)]
pub enum GcKind {
    TrimPages,
    TrimSlru,
    TrimMeta,
    ShrinkPages,
    DropPages,
    DropSlru,
    DropMeta,
    Unknown(i16),
}

impl GcKind {
    pub fn to_i16(self) -> i16 {
        match self {
            GcKind::TrimPages => 0,
            GcKind::TrimSlru => 1,
            GcKind::TrimMeta => 2,
            GcKind::ShrinkPages => 3,
            GcKind::DropPages => 4,
            GcKind::DropSlru => 5,
            GcKind::DropMeta => 6,
            GcKind::Unknown(v) => v,
        }
    }

    pub fn from_i16(v: i16) -> GcKind {
        match v {
            0 => GcKind::TrimPages,
            1 => GcKind::TrimSlru,
            2 => GcKind::TrimMeta,
            3 => GcKind::ShrinkPages,
            4 => GcKind::DropPages,
            5 => GcKind::DropSlru,
            6 => GcKind::DropMeta,
            other => GcKind::Unknown(other),
        }
    }
}

/// Fork identity: (spcnode, dbnode, relnode, forknum). The grouping key
/// for per-fork rel trims and the rel drop/shrink acts, passed as a unit
/// to the rel `gc_act_*` functions (which bind the columns).
pub type GcForkIdent = (i64, i64, i64, i16);

/// SLRU segment identity: (kind, segno). The grouping key for per-segment
/// SLRU trims and the SLRU drop act, passed as a unit to the SLRU
/// `gc_act_*` functions.
pub type GcSlruSeg = (i16, i64);

/// The full identity (PK) of one drop/shrink candidate — its object
/// `key`, `gc_kind`, and `due_lsn`.
pub(crate) type GcDropShrinkCandidateId = (Key, GcKind, Lsn);

/// One GC candidate: "reclaim the garbage named by `key` once the cutoff
/// reaches `due_lsn`", per `gc_kind`. `key` is an encoded [`Key`] — a rel
/// or SLRU block (trim), or a rel / SLRU / meta object (drop/shrink); the
/// drain decodes it and routes by its Key predicates.
///
/// `relsize`: shrink's truncation target (rel-only); `None` for trim/drop.
#[derive(Debug, Clone, Copy, PartialEq, Eq)]
pub struct PendingGcRow {
    pub key: Key,
    pub gc_kind: GcKind,
    pub due_lsn: Lsn,
    pub relsize: Option<i64>,
}

/// What one `gc_select_due_candidates` sweep found.
pub struct GcSelectedCandidates {
    /// The `pending_gc` rows with a valid key, oldest `due_lsn` first.
    pub rows: Vec<PendingGcRow>,
    /// Rows selected, malformed ones included — so not `rows.len()`.
    /// `< limit` ⇒ the tick saw the whole due span.
    pub raw_count: usize,
    /// Highest `due_lsn` selected, malformed ones included. `None` only when
    /// nothing was due.
    pub max_due_lsn: Option<Lsn>,
}

/// Decode a rel key's fork identity `(spcnode, dbnode, relnode, forknum)`
/// from its Key fields (field2..field5). Only meaningful for rel-keyspace
/// keys (`field1 == 0`).
pub(crate) fn rel_fork_of(key: &Key) -> GcForkIdent {
    (
        key.field2 as i64,
        key.field3 as i64,
        key.field4 as i64,
        key.field5 as i16,
    )
}

/// Decode an SLRU key's segment identity `(kind, segno)` from its fields
/// (kind = field2, segno = field4). Only meaningful for SLRU-keyspace keys
/// (`field1 == 1`) — a block or its seg-size sentinel.
pub(crate) fn slru_seg_of(key: &Key) -> GcSlruSeg {
    (key.field2 as i16, key.field4 as i64)
}

/// The busy-defer key of a materialisable *block* key: the object's
/// sentinel key (blknum → `field6` MAX), which is exactly the key a
/// `DropPages`/`DropSlru` candidate carries — so a drop is busy iff its own
/// key is in the set. A rel block maps to its rel-size sentinel, an SLRU
/// block to its seg-size sentinel (`field6 == MAX` is the sentinel
/// invariant; see [`Key::is_rel_size_key`]/[`Key::is_slru_segment_size_key`]).
/// `None` for anything that isn't a materialisable page block (e.g. meta,
/// never materialised).
///
/// A drop is left queued while an image job for the same object is in
/// flight, so a straggling image INSERT after the delete can't leak.
pub(crate) fn materialise_object_key(key: &Key) -> Option<Key> {
    key.is_block_key().then_some(Key {
        field6: 0xFFFF_FFFF,
        ..*key
    })
}

/// `branch_id` stays first: [`build_insert_sql`] bakes it into every row
/// tuple as a literal, so it must lead and `bucket_id` follows as the first
/// bound column.
const PAGES_COLUMNS: &str = "branch_id, bucket_id, spcnode, dbnode, relnode, forknum, blknum, \
                             lsn, is_image, will_init, payload";
/// Per-row bound columns for `pages` (everything after the baked
/// `branch_id` literal): bucket_id, spcnode, dbnode, relnode,
/// forknum, blknum, lsn, is_image, will_init, payload.
const PAGES_PER_ROW: usize = 10;

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
/// `$2..$7` = spcnode, dbnode, relnode, forknum, blknum, target_lsn;
/// `$8`=bucket_id.
///
/// `bucket_id` is appended rather than placed first so the existing
/// placeholders keep their numbers — the leading key column has to be *bound*,
/// but nothing requires it to be `$1`. A single blknum is one bucket, so this
/// read is always pinned to one tablet. The anchor subquery is its own scan of
/// `pages` and needs the same bound, or it loses the leading column and reads
/// every bucket.
fn build_read_chain_sql(schema: &str) -> String {
    format!(
        "SELECT p.spcnode, p.dbnode, p.relnode, p.forknum, p.blknum, \
                p.lsn, p.is_image, p.will_init, p.payload \
         FROM \"{schema}\".pages p \
         WHERE p.bucket_id=$8 AND p.branch_id=$1 \
           AND p.spcnode=$2 AND p.dbnode=$3 AND p.relnode=$4 \
           AND p.forknum=$5 AND p.blknum=$6 \
           AND p.lsn <= $7 \
           AND p.lsn >= COALESCE(( \
                 SELECT a.lsn FROM \"{schema}\".pages a \
                 WHERE a.bucket_id=$8 AND a.branch_id=$1 \
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
/// - `bucket`: the placeholder holding this query's single `bucket_id`
///   (`$9` for range, `$8` for set).
///
/// One `bucket_id` per query, bound as a VALUE: the caller runs this once per
/// bucket its blknum span touches and merges. `bucket_id = ANY(...)` would also
/// work on this lane — YB pushes an IN-list into the index condition — but the
/// per-bucket shape matches the thin lane, which cannot express a set at all,
/// so both lanes fan out the same way.
fn build_scan_one_fork_lateral_sql(
    schema: &str,
    prefix: &str,
    driver: &str,
    lsn: &str,
    bucket: &str,
) -> String {
    format!(
        "{prefix}\
         SELECT p.spcnode, p.dbnode, p.relnode, p.forknum, p.blknum, \
                p.lsn, p.is_image, p.will_init, p.payload \
         FROM {driver} \
         CROSS JOIN LATERAL ( \
             SELECT p.spcnode, p.dbnode, p.relnode, p.forknum, p.blknum, \
                    p.lsn, p.is_image, p.will_init, p.payload \
             FROM \"{schema}\".pages p \
             WHERE p.bucket_id={bucket} AND p.branch_id=$1 \
               AND p.spcnode=$2 AND p.dbnode=$3 AND p.relnode=$4 \
               AND p.forknum=$5 AND p.blknum = b.blknum \
               AND p.lsn <= {lsn} \
               AND p.lsn >= COALESCE(( \
                     SELECT a.lsn FROM \"{schema}\".pages a \
                     WHERE a.bucket_id={bucket} AND a.branch_id=$1 \
                       AND a.spcnode=$2 AND a.dbnode=$3 AND a.relnode=$4 \
                       AND a.forknum=$5 AND a.blknum = b.blknum \
                       AND a.is_image=TRUE AND a.lsn <= {lsn} \
                     ORDER BY a.lsn DESC LIMIT 1), 0) \
             ORDER BY p.lsn DESC, p.is_image DESC \
         ) p \
         ORDER BY p.blknum, p.lsn DESC, p.is_image DESC"
    )
}

/// One-fork range scan (`scan_range`), for ONE bucket. `$1`=branch_id;
/// `$2..$8` = spcnode, dbnode, relnode, forknum, blknum_lo, blknum_hi,
/// target_lsn; `$9`=bucket_id. Driver: the DISTINCT blknums that actually
/// exist in `[$6, $7)` (a Distinct Index Scan ⇒ one row per block) feeding
/// the shared LATERAL.
///
/// The caller runs this once per bucket in
/// [`page_buckets_for_range`] and merges; `[$6, $7)` may span more of the
/// fork than this bucket holds, and the `bucket_id` bound is what narrows the
/// CTE to the stretches actually stored here.
fn build_scan_one_fork_sql(schema: &str) -> String {
    let prefix = format!(
        "WITH blocks AS ( \
             SELECT DISTINCT blknum FROM \"{schema}\".pages \
             WHERE bucket_id=$9 AND branch_id=$1 \
               AND spcnode=$2 AND dbnode=$3 AND relnode=$4 \
               AND forknum=$5 \
               AND blknum >= $6 AND blknum < $7 \
               AND lsn <= $8 \
         ) "
    );
    build_scan_one_fork_lateral_sql(schema, &prefix, "blocks b", "$8", "$9")
}

/// Multi-key ("scattered") one-fork scan (`scan_scattered_set`). Same LATERAL
/// body as [`build_scan_one_fork_sql`], but driven directly from the
/// caller-supplied blknum array via `unnest` rather than a contiguous `[lo, hi)`
/// range discovered by a `DISTINCT` pages scan. `$1`=branch_id; `$2..$5`=
/// spcnode, dbnode, relnode, forknum; `$6`=blknum bigint[]; `$7`=target_lsn;
/// `$8`=bucket_id.
///
/// We already know the blocks, so we do NOT re-scan pages with a
/// `SELECT DISTINCT ... blknum = ANY(...)` (that would bet on YB skip-scanning
/// the `= ANY` on the range column). `unnest` makes every tablet touch below a
/// single-blknum point seek (bucket `=` + the rest of the key prefix `=`). A
/// blknum absent from the table yields an empty LATERAL → CROSS JOIN drops it
/// (no row), same as the old DISTINCT-CTE excluding it.
///
/// The caller partitions its blknums by [`page_bucket_id`] and issues one
/// query per bucket with only that bucket's blknums in `$6`, so `$8` is a
/// single value and every point seek stays pruned.
fn build_scan_one_fork_scattered_sql(schema: &str) -> String {
    build_scan_one_fork_lateral_sql(schema, "", "unnest($6::bigint[]) AS b(blknum)", "$7", "$8")
}

/// Cross-fork range scan (`scan_range`), for ONE bucket. `$1`=branch_id;
/// `$2..$12` = (spc,db,rel,fork,blk)_lo, (spc,db,rel,fork,blk)_hi,
/// target_lsn; `$13`=bucket_id.
///
/// A cross-fork range spans relations, so the bucket cannot be derived from
/// the bounds — the caller loops over all [`PAGE_BUCKETS`] and merges. That is
/// still strictly better than before: with a hash key and no hash column
/// bound, this query was a full sequential scan of `pages`. Now it is six
/// bucket-pinned range scans.
///
/// Note the row-comparison bounds stay on `(spcnode, …, blknum)` only. They
/// deliberately do not mention `bucket_id`, which sits *above* them in the key
/// and is unrelated to fork ordering; the `bucket_id=$13` equality is what
/// prunes, and the row comparison narrows within it.
fn build_scan_cross_fork_sql(schema: &str) -> String {
    format!(
        "WITH blocks AS ( \
             SELECT DISTINCT spcnode, dbnode, relnode, forknum, blknum \
             FROM \"{schema}\".pages \
             WHERE bucket_id=$13 AND branch_id=$1 \
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
             WHERE p.bucket_id=$13 AND p.branch_id=$1 \
               AND p.spcnode = b.spcnode AND p.dbnode = b.dbnode \
               AND p.relnode = b.relnode AND p.forknum = b.forknum \
               AND p.blknum = b.blknum \
               AND p.lsn <= $12 \
               AND p.lsn >= COALESCE(( \
                     SELECT a.lsn FROM \"{schema}\".pages a \
                     WHERE a.bucket_id=$13 AND a.branch_id=$1 \
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
        let ysql_client = Arc::new(
            ysql_client::YsqlClient::from_yb_config_pooled(
                &config.yb,
                application_name,
                pool_size,
                set_fast_path_upsert,
            )
            .await
            .context("opening PageStore connection")?,
        );
        // Read/write session (connection) counts cap the getpage +
        // put_pages lane fan-out across the discovered tservers. The
        // `yb_storage_conn` config can pin them; otherwise both default to
        // `scale_count(2, scale_factor)`.
        let read_sessions = config
            .yb
            .tsclient_read_sessions
            .unwrap_or_else(|| pageserver_api::config::scale_count(2, config.scale_factor));
        let write_sessions = config
            .yb
            .tsclient_write_sessions
            .unwrap_or_else(|| pageserver_api::config::scale_count(2, config.scale_factor));
        let sessions_per_conn = config.yb.tsclient_sessions_per_conn.unwrap_or(4);
        let thinclient = match config.yb.hot_path {
            ysql_client::HotPath::ThinClient => Some(
                ThinClientTables::connect(
                    &config.yb,
                    &config.schema,
                    &ysql_client,
                    read_sessions,
                    write_sessions,
                    sessions_per_conn,
                )
                .await
                .context("thin-client hot path init (hot_path = \"yb_thin_client\")")?,
            ),
            _ => None,
        };
        Ok(Self {
            ysql_client,
            schema: Arc::from(config.schema),
            scale_factor: config.scale_factor,
            payload_codec: match config.payload_compression {
                PayloadCompression::None => payload_codec::Codec::Raw,
                PayloadCompression::Lz4 => payload_codec::Codec::Lz4,
            },
            baked: Arc::new(DashMap::new()),
            thinclient,
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

    // ------------------- payload compression --------------------------------

    /// Wrap every payload in the [`payload_codec`] envelope, once, before
    /// the `put_*` lane dispatch — so the YSQL and thin-RPC lanes bind
    /// identical bytes and neither can drift from the other.
    ///
    /// This is the *only* place the pageserver produces encoded payloads.
    /// Everything upstream of `PageStore` — `PageWriter`'s delta tallies,
    /// the image materializer's chain-byte accounting, walredo, GC's
    /// payload comparison — keeps working on plaintext, which is why
    /// compression needed no changes there. Keep it that way: an encoded
    /// payload escaping above this boundary would silently reinterpret
    /// page bytes as record bytes.
    fn encode_payloads<'a>(
        &self,
        table: &'static str,
        payloads: impl ExactSizeIterator<Item = &'a Bytes>,
    ) -> (Vec<Bytes>, PayloadTally) {
        let mut out = Vec::with_capacity(payloads.len());
        let mut tally = PayloadTally::new(table);
        for plain in payloads {
            let (encoded, outcome) = payload_codec::encode(plain, self.payload_codec);
            tally.add(plain.len(), encoded.len(), outcome);
            out.push(encoded);
        }
        (out, tally)
    }

    // ------------------- pages ---------------------------------------------

    pub async fn put_pages(&self, branch_id: BranchId, rows: &[Arc<PageRow>]) -> Result<()> {
        self.write_gate("write to amp_storage.pages");
        // Encode once, ahead of the lane split: EVERY lane binds these bytes.
        // A lane that binds `row.payload` directly writes plaintext with no
        // codec tag, which the read path then rejects — so a newly added lane
        // must be threaded through here too.
        let (encoded, tally) = self.encode_payloads("pages", rows.iter().map(|r| &r.payload));
        if let Some(pool) = &self.thinclient {
            let p = pool.current().await;
            let started = std::time::Instant::now();
            self.put_pages_thin(&p, branch_id, rows, &encoded).await?;
            crate::metrics::YB_STORE_OP_SECONDS
                .with_label_values(&["put_pages", "yb_thin_client"])
                .observe(started.elapsed().as_secs_f64());
            tally.record();
            return Ok(());
        }

        // Chunks dispatch in parallel — tokio-postgres pipelines them
        // on the shared connection so YB can absorb them concurrently.
        let started = std::time::Instant::now();
        let parallelism = pageserver_api::config::scale_count(2, self.scale_factor);
        let pages_full = Arc::clone(&self.baked_sql(branch_id).pages_full);

        let mut futs: Vec<futures::future::BoxFuture<'_, Result<()>>> = Vec::new();
        // `encoded` is 1:1 with `rows`, so the two chunk iterators stay
        // aligned and `enc_chunk[i]` is `chunk[i]`'s payload.
        for (chunk, enc_chunk) in rows
            .chunks(ROWS_PER_INSERT)
            .zip(encoded.chunks(ROWS_PER_INSERT))
        {
            if chunk.is_empty() {
                continue;
            }
            let n = chunk.len();
            // Decode + validate keys up front: a bad key fails the whole
            // flush before any chunk is sent (fail-fast), and the per-row
            // scalar columns land in owned storage we can bind by
            // reference — no per-cell `Box<dyn ToSql>`.
            let mut bucket = Vec::with_capacity(n);
            let mut spc = Vec::with_capacity(n);
            let mut dbn = Vec::with_capacity(n);
            let mut rel = Vec::with_capacity(n);
            let mut fork = Vec::with_capacity(n);
            let mut blk = Vec::with_capacity(n);
            let mut lsn = Vec::with_capacity(n);
            for (i, row) in chunk.iter().enumerate() {
                let (s, d, r, f, b) =
                    key_columns(&row.key).with_context(|| format!("put_pages row {i}: bad key"))?;
                bucket.push(page_bucket_id(r, b));
                spc.push(s);
                dbn.push(d);
                rel.push(r);
                fork.push(f);
                blk.push(b);
                lsn.push(row.lsn.0 as i64);
            }
            // Cheap Arc-clone per row keeps the scalar columns alive; the
            // encoded payloads are `Bytes`, so cloning them is a refcount
            // bump and never copies page bytes.
            let arcs: Vec<Arc<PageRow>> = chunk.to_vec();
            let enc: Vec<Bytes> = enc_chunk.to_vec();
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
                let payloads: Vec<&[u8]> = enc.iter().map(|b| b.as_ref()).collect();
                let mut params: Vec<&(dyn ToSql + Sync)> = Vec::with_capacity(n * PAGES_PER_ROW);
                for i in 0..n {
                    params.push(&bucket[i]);
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
        tally.record();
        Ok(())
    }

    async fn put_pages_thin(
        &self,
        p: &ThinClientTables,
        branch_id: BranchId,
        rows: &[Arc<PageRow>],
        encoded: &[Bytes],
    ) -> Result<()> {
        use yb_thin_client::{BindValue, UpsertRow};
        let pages = &p.pages;
        // Column metadata is constant for the whole flush — resolve once
        // instead of per row (ROWS_PER_INSERT * chunks lookups otherwise).
        let col_bucket = pages.col("bucket_id")?;
        let col_spc = pages.col("spcnode")?;
        let col_db = pages.col("dbnode")?;
        let col_rel = pages.col("relnode")?;
        let col_fork = pages.col("forknum")?;
        let parallelism = pageserver_api::config::scale_count(2, self.scale_factor);
        let branch_b = branch_id.to_bytes();

        let mut futs: Vec<futures::future::BoxFuture<'_, Result<()>>> = Vec::new();
        for (chunk, enc_chunk) in rows
            .chunks(ROWS_PER_INSERT)
            .zip(encoded.chunks(ROWS_PER_INSERT))
        {
            if chunk.is_empty() {
                continue;
            }
            let mut batch = Vec::with_capacity(chunk.len());
            for (i, row) in chunk.iter().enumerate() {
                let (sp, db, rel, fork, blk) =
                    key_columns(&row.key).with_context(|| format!("put_pages row {i}: bad key"))?;
                // key_values are in PRIMARY KEY order, not declaration order —
                // hence bucket_id first and (lsn, is_image) rather than
                // (is_image, lsn). The CHECK constraint in the migration does
                // NOT fire on this lane, so `page_bucket_id` here is the only
                // thing keeping these rows findable.
                batch.push(UpsertRow::new(
                    pages,
                    vec![
                        int_bind_thin(col_bucket, page_bucket_id(rel, blk) as i64),
                        BindValue::Bytes(branch_b.clone()),
                        int_bind_thin(col_spc, sp),
                        int_bind_thin(col_db, db),
                        int_bind_thin(col_rel, rel),
                        int_bind_thin(col_fork, fork as i64),
                        BindValue::I64(blk),
                        BindValue::I64(row.lsn.0 as i64),
                        BindValue::Bool(row.is_image),
                    ],
                    &[
                        ("will_init", BindValue::Bool(row.will_init)),
                        ("payload", BindValue::Bytes(enc_chunk[i].clone())),
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
            let chunk_rows = batch.len();
            futs.push(Box::pin(async move {
                // Full retry budget (inside `upsert`): this lane has no YSQL
                // fallback, so it rides out transient failures itself. There is
                // no per-attempt session rotation — the shim owns the session
                // pool and the tserver does the routing. A terminal failure
                // fails the flush.
                client.upsert(&batch).await.with_context(|| {
                    format!("put_pages(yb_thin_client) branch={branch_id} chunk_rows={chunk_rows}")
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
        if let Some(pool) = &self.thinclient {
            let p = pool.current().await;
            let started = std::time::Instant::now();
            let chain = self
                .read_chain_thin(&p, branch_id, sp, db, rel, fork, blk, target)
                .await?;
            crate::metrics::YB_STORE_OP_SECONDS
                .with_label_values(&["read_chain", "yb_thin_client"])
                .observe(started.elapsed().as_secs_f64());
            return Ok(chain);
        }

        let started = std::time::Instant::now();
        let sql = build_read_chain_sql(&self.schema);
        // One blknum ⇒ one bucket ⇒ one tablet.
        let bucket = page_bucket_id(rel, blk);
        let rows = self
            .ysql_client
            .query(
                &sql,
                &[&branch_id, &sp, &db, &rel, &fork, &blk, &target, &bucket],
            )
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

    #[allow(clippy::too_many_arguments)]
    async fn read_chain_thin(
        &self,
        p: &ThinClientTables,
        branch_id: BranchId,
        sp: i64,
        db: i64,
        rel: i64,
        fork: i16,
        blk: i64,
        target: i64,
    ) -> Result<Vec<Arc<PageRow>>> {
        use yb_thin_client::{BindValue, CondOp, RangeRead};
        let pages = &p.pages;
        // All-range key, so the prefix goes in as `range_values` (there is no
        // hash key to bind). A known blknum determines the bucket, so all
        // seven leading key columns are bound and this stays a single-tablet
        // clustered read — `blknum` is part of the prefix now rather than a
        // trailing `cond`.
        let read = RangeRead::new_range(
            pages,
            vec![
                int_bind_thin(pages.col("bucket_id")?, page_bucket_id(rel, blk) as i64),
                BindValue::Bytes(branch_id.to_bytes()),
                int_bind_thin(pages.col("spcnode")?, sp),
                int_bind_thin(pages.col("dbnode")?, db),
                int_bind_thin(pages.col("relnode")?, rel),
                int_bind_thin(pages.col("forknum")?, fork as i64),
                BindValue::I64(blk),
            ],
        )?
        .cond(pages, "lsn", CondOp::Le, BindValue::I64(target))?
        .targets(pages, &["lsn", "is_image", "will_init", "payload"])?
        .limit(DIRECT_CHAIN_PAGE_ROWS);
        let raw = fetch_chain_rows_thin(&p.client, pages, read, 0, 2).await?;

        let key = Key::from_slice(&columns_to_key_bytes(sp, db, rel, fork, blk));
        assemble_page_chain(key, raw, 0, "read_chain_thin")
    }

    #[allow(clippy::too_many_arguments)]
    async fn scan_one_fork_thin(
        &self,
        p: &ThinClientTables,
        branch_id: BranchId,
        sp: i64,
        db: i64,
        rel: i64,
        fork: i16,
        blk_lo: i64,
        blk_hi: i64,
        target: i64,
    ) -> Result<HashMap<Key, Vec<Arc<PageRow>>>> {
        use yb_thin_client::{BindValue, CondOp, RangeRead};
        let pages = &p.pages;
        // A blknum span can cross 2MB stretch boundaries, so it can live in
        // several buckets. `bucket_id` leads the key, so it cannot be a `cond`
        // and cannot be an IN-list either — it has to be one bound read per
        // bucket. In practice that is 1, or 2 when the span straddles a
        // stretch boundary: `max_get_vectored_keys` caps a vectored read at 32
        // blocks against a 256-block stride.
        let buckets = page_buckets_for_range(rel, blk_lo, blk_hi);
        if buckets.is_empty() {
            return Ok(HashMap::new());
        }
        let mut reads: Vec<RangeRead> = Vec::with_capacity(buckets.len());
        let branch_b = branch_id.to_bytes();
        for &bucket in &buckets {
            reads.push(
                RangeRead::new_range(
                    pages,
                    vec![
                        int_bind_thin(pages.col("bucket_id")?, bucket as i64),
                        BindValue::Bytes(branch_b.clone()),
                        int_bind_thin(pages.col("spcnode")?, sp),
                        int_bind_thin(pages.col("dbnode")?, db),
                        int_bind_thin(pages.col("relnode")?, rel),
                        int_bind_thin(pages.col("forknum")?, fork as i64),
                    ],
                )?
                .cond(pages, "blknum", CondOp::Ge, BindValue::I64(blk_lo))?
                .cond(pages, "blknum", CondOp::Lt, BindValue::I64(blk_hi))?
                .cond(pages, "lsn", CondOp::Le, BindValue::I64(target))?
                .targets(
                    pages,
                    &["blknum", "lsn", "is_image", "will_init", "payload"],
                )?
                .limit(DIRECT_SCAN_PAGE_ROWS),
            );
        }

        // Every bucket's read rides ONE `Perform`, and every continuation is
        // pinned to the snapshot round 1 ran at — so the whole fan-out resolves
        // at a SINGLE read point (#139), exactly as the one query it replaced
        // did. Looping `read_scan` per bucket instead would be one RPC *and one
        // read point* per bucket, letting a concurrent GC drain land between
        // them and splice rows from two different snapshots into one scan.
        // Same shape as the safekeeper's `fetch_chunks_thin` fan-out.
        //
        // Paging is mandatory, not an optimisation: an op's scan spans tablets,
        // and a tablet holding no matching row returns an EMPTY page plus a
        // continuation — "nothing here", not "scan done". Stopping at the first
        // page would silently drop whole tablets' worth of blocks.
        let mut fetched: Option<Vec<BlockChainCollector<yb_thin_client::Row>>> = None;
        for attempt in 1..=THIN_SCAN_RESTARTS {
            let mut collectors: Vec<BlockChainCollector<yb_thin_client::Row>> = (0..reads.len())
                .map(|_| BlockChainCollector::new())
                .collect();
            let mut paging: Vec<Option<bytes::Bytes>> = vec![None; reads.len()];
            // Consecutive rounds an op returned nothing while still handing back
            // a continuation. Crossing empty tablets legitimately does this a
            // few times; forever means the token is not advancing.
            let mut barren: Vec<usize> = vec![0; reads.len()];
            let mut active: Vec<usize> = (0..reads.len()).collect();
            let mut read_time_ht = 0u64;
            let mut restart = false;

            while !active.is_empty() {
                let ops: Vec<yb_thin_client::ReadOp> = active
                    .iter()
                    .map(|&i| yb_thin_client::ReadOp {
                        table: pages.as_ref(),
                        read: &reads[i],
                        paging_state: paging[i].as_deref(),
                    })
                    .collect();
                let batch = match p.client.read(&ops, read_time_ht).await {
                    Ok(b) => b,
                    // Only an UNPINNED batch heals a read-restart inside
                    // `TsClient::read`; from round 2 on every op carries a
                    // paging state, so the restart surfaces here and this loop
                    // is the driver that owns it — drop the half-built
                    // collectors and start over on a fresh read point.
                    Err(e) if e.is_read_restart() && attempt < THIN_SCAN_RESTARTS => {
                        tracing::warn!(
                            "thin-lane page scan read-restart (attempt \
                             {attempt}/{THIN_SCAN_RESTARTS}), restarting the scan: {e}"
                        );
                        restart = true;
                        break;
                    }
                    Err(e) => return Err(e.into()),
                };
                read_time_ht = batch.used_read_time_ht;
                let mut next: Vec<usize> = Vec::new();
                for (slot, res) in batch.ops.into_iter().enumerate() {
                    let Some(&i) = active.get(slot) else { continue };
                    let got = res.rows.len();
                    feed_scan_rows(&mut collectors[i], res.rows)?;
                    if let Some(ps) = res.paging_state {
                        barren[i] = if got == 0 { barren[i] + 1 } else { 0 };
                        anyhow::ensure!(
                            barren[i] <= THIN_SCAN_BARREN_ROUNDS,
                            "thin-lane page scan for rel={rel} fork={fork} \
                             blk=[{blk_lo},{blk_hi}) bucket={} returned {THIN_SCAN_BARREN_ROUNDS} \
                             empty pages in a row while still continuing — the continuation \
                             token is not advancing",
                            buckets[i]
                        );
                        paging[i] = Some(ps);
                        next.push(i);
                    }
                }
                active = next;
            }
            if !restart {
                fetched = Some(collectors);
                break;
            }
        }
        // The final attempt either converges or propagates its error above.
        let collectors = fetched.expect("the final scan attempt returns or propagates");

        // Blocks are disjoint across buckets (the bucket is a function of
        // blknum), so the per-bucket key maps never collide and `extend` cannot
        // clobber a chain.
        let mut by_key = HashMap::new();
        for collector in collectors {
            by_key.extend(assemble_page_scan(
                sp,
                db,
                rel,
                fork,
                collector,
                "scan_range_thin",
            )?);
        }
        Ok(by_key)
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
            if let Some(pool) = &self.thinclient {
                let p = pool.current().await;
                let started = std::time::Instant::now();
                let by_key = self
                    .scan_one_fork_thin(&p, branch_id, sp, db, rel, fork, blk_lo, blk_hi, target)
                    .await?;
                crate::metrics::YB_STORE_OP_SECONDS
                    .with_label_values(&["scan_range", "yb_thin_client"])
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
            // `[blk_lo, blk_hi)` can cross 2MB stretch boundaries, so the
            // fork's blocks in it may live in several buckets — one pinned
            // query each, results concatenated. Every key belongs to exactly
            // one bucket, so a key's chain arrives entirely from one query and
            // stays correctly ordered within itself; `group_page_rows_by_key`
            // buckets by key and never relies on order across queries.
            let mut acc = Vec::new();
            for bucket in page_buckets_for_range(rel, blk_lo, blk_hi) {
                let mut got = self
                    .ysql_client
                    .query(
                        &sql,
                        &[
                            &branch_id, &sp, &db, &rel, &fork, &blk_lo, &blk_hi, &target, &bucket,
                        ],
                    )
                    .await
                    .context("scan_range one-fork")?;
                acc.append(&mut got);
            }
            acc
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
            // A cross-fork range spans relations, so its buckets can't be
            // derived from the bounds — fan out over all of them. Still an
            // improvement: with the old hash key this query bound no hash
            // column at all and seq-scanned the whole table.
            let mut acc = Vec::new();
            for bucket in 0..PAGE_BUCKETS as i16 {
                let mut got = self
                    .ysql_client
                    .query(
                        &sql,
                        &[
                            &branch_id, &sp_lo, &db_lo, &rel_lo, &fork_lo, &blk_lo, &sp_hi, &db_hi,
                            &rel_hi, &fork_hi, &blk_hi, &target, &bucket,
                        ],
                    )
                    .await
                    .context("scan_range cross-fork")?;
                acc.append(&mut got);
            }
            acc
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
        let target = target_lsn.0 as i64;
        let sql = build_scan_one_fork_scattered_sql(&self.schema);
        // Partition the blknums by bucket and send each group to its own
        // bucket-pinned query. Indexed by bucket rather than a map so the
        // order of the queries is deterministic; `blknums` is already sorted,
        // so each group stays sorted and each query's `ORDER BY blknum` still
        // means something.
        let mut by_bucket: Vec<Vec<i64>> = vec![Vec::new(); PAGE_BUCKETS as usize];
        for blk in blknums {
            by_bucket[page_bucket_id(rel, blk) as usize].push(blk);
        }
        let mut rows = Vec::new();
        for (bucket, blks) in by_bucket.iter().enumerate() {
            if blks.is_empty() {
                continue;
            }
            let bucket = bucket as i16;
            let mut got = self
                .ysql_client
                .query(
                    &sql,
                    &[&branch_id, &sp, &db, &rel, &fork, blks, &target, &bucket],
                )
                .await
                .context("scan_scattered_set one-fork")?;
            rows.append(&mut got);
        }

        group_page_rows_by_key(rows, "scan_scattered_set")
    }

    // ------------------- slru_pages ---------------------------------------

    pub async fn put_slru_pages(&self, branch_id: BranchId, rows: &[SlruPageRow]) -> Result<()> {
        self.write_gate("write to amp_storage.slru_pages");
        let (encoded, tally) = self.encode_payloads("slru_pages", rows.iter().map(|r| &r.payload));
        if let Some(pool) = &self.thinclient {
            let p = pool.current().await;
            let started = std::time::Instant::now();
            self.put_slru_pages_thin(&p, branch_id, rows, &encoded)
                .await?;
            crate::metrics::YB_STORE_OP_SECONDS
                .with_label_values(&["put_slru_pages", "yb_thin_client"])
                .observe(started.elapsed().as_secs_f64());
            tally.record();
            return Ok(());
        }

        let started = std::time::Instant::now();
        let parallelism = pageserver_api::config::scale_count(1, self.scale_factor);
        let slru_full = Arc::clone(&self.baked_sql(branch_id).slru_full);

        let mut futs: Vec<futures::future::BoxFuture<'_, Result<()>>> = Vec::new();
        for (chunk, enc_chunk) in rows
            .chunks(ROWS_PER_INSERT)
            .zip(encoded.chunks(ROWS_PER_INSERT))
        {
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
            for (row, enc) in chunk.iter().zip(enc_chunk) {
                kind.push(slru_kind_to_i16(row.kind));
                segno.push(row.segno as i64);
                blknum.push(row.blknum as i64);
                is_image.push(row.is_image);
                lsn.push(row.lsn.0 as i64);
                will_init.push(row.will_init);
                payloads_owned.push(enc.clone());
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
        tally.record();
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
        let kind_i = slru_kind_to_i16(kind);
        let segno_i = segno as i64;
        let blk_i = blknum as i64;
        let target = target_lsn.0 as i64;

        if let Some(pool) = &self.thinclient {
            let p = pool.current().await;
            let started = std::time::Instant::now();
            let chain = self
                .read_slru_chain_thin(&p, branch_id, kind, kind_i, segno_i, blk_i, target)
                .await?;
            crate::metrics::YB_STORE_OP_SECONDS
                .with_label_values(&["read_slru_chain", "yb_thin_client"])
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
            .query(&sql, &[&branch_id, &kind_i, &segno_i, &blk_i, &target])
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

    // Wide signature: the caller already decomposed the key columns.
    #[allow(clippy::too_many_arguments)]
    async fn read_slru_chain_thin(
        &self,
        p: &ThinClientTables,
        branch_id: BranchId,
        kind: SlruKind,
        kind_i: i16,
        segno_i: i64,
        blk_i: i64,
        target: i64,
    ) -> Result<Vec<SlruPageRow>> {
        use yb_thin_client::{BindValue, CondOp};
        let slru = &p.slru_pages;
        let read = slru_segment_read(slru, branch_id, kind_i, segno_i)?
            .cond(slru, "blknum", CondOp::Eq, BindValue::I64(blk_i))?
            .cond(slru, "lsn", CondOp::Le, BindValue::I64(target))?
            .targets(slru, &["lsn", "is_image", "will_init", "payload"])?
            .limit(DIRECT_SLRU_CHAIN_PAGE_ROWS);
        let raw = fetch_chain_rows_thin(&p.client, slru, read, 0, 2).await?;

        assemble_slru_chain(
            kind,
            segno_i as u32,
            blk_i as u32,
            raw,
            0,
            "read_slru_chain_thin",
        )
    }

    #[allow(clippy::too_many_arguments)]
    async fn scan_slru_range_thin(
        &self,
        p: &ThinClientTables,
        branch_id: BranchId,
        kind: SlruKind,
        kind_i: i16,
        segno_i: i64,
        blk_lo: i64,
        blk_hi: i64,
        target: i64,
    ) -> Result<HashMap<u32, Vec<SlruPageRow>>> {
        use yb_thin_client::{BindValue, CondOp};
        let slru = &p.slru_pages;
        let read = slru_segment_read(slru, branch_id, kind_i, segno_i)?
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

        assemble_slru_scan(kind, segno_i as u32, collector, "scan_slru_range_thin")
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

        let kind_i = slru_kind_to_i16(kind);
        let segno_i = segno as i64;
        let blk_lo = blknum_range.start as i64;
        let blk_hi = blknum_range.end as i64;
        let target = target_lsn.0 as i64;

        if let Some(pool) = &self.thinclient {
            let p = pool.current().await;
            let started = std::time::Instant::now();
            let by_block = self
                .scan_slru_range_thin(&p, branch_id, kind, kind_i, segno_i, blk_lo, blk_hi, target)
                .await?;
            crate::metrics::YB_STORE_OP_SECONDS
                .with_label_values(&["scan_slru_range", "yb_thin_client"])
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
                &[&branch_id, &kind_i, &segno_i, &blk_lo, &blk_hi, &target],
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

    async fn put_slru_pages_thin(
        &self,
        p: &ThinClientTables,
        branch_id: BranchId,
        rows: &[SlruPageRow],
        encoded: &[Bytes],
    ) -> Result<()> {
        use yb_thin_client::{BindValue, UpsertRow};
        let slru = &p.slru_pages;
        // Column metadata is constant for the whole flush — resolve once.
        let col_kind = slru.col("kind")?;
        let col_segno = slru.col("segno")?;
        let parallelism = pageserver_api::config::scale_count(1, self.scale_factor);
        let branch_b = branch_id.to_bytes();

        let mut futs: Vec<futures::future::BoxFuture<'_, Result<()>>> = Vec::new();
        for (chunk, enc_chunk) in rows
            .chunks(ROWS_PER_INSERT)
            .zip(encoded.chunks(ROWS_PER_INSERT))
        {
            if chunk.is_empty() {
                continue;
            }
            let mut batch = Vec::with_capacity(chunk.len());
            for (row, enc) in chunk.iter().zip(enc_chunk) {
                batch.push(UpsertRow::new(
                    slru,
                    vec![
                        BindValue::Bytes(branch_b.clone()),
                        int_bind_thin(col_kind, slru_kind_to_i16(row.kind) as i64),
                        int_bind_thin(col_segno, row.segno as i64),
                        BindValue::I64(row.blknum as i64),
                        BindValue::I64(row.lsn.0 as i64),
                        BindValue::Bool(row.is_image),
                    ],
                    &[
                        ("will_init", BindValue::Bool(row.will_init)),
                        ("payload", BindValue::Bytes(enc.clone())),
                    ],
                )?);
            }
            let client = Arc::clone(&p.client);
            let chunk_rows = batch.len();
            futs.push(Box::pin(async move {
                // Full retry budget inside `upsert`, no session rotation
                // (see put_pages' thin lane).
                client.upsert(&batch).await.with_context(|| {
                    format!(
                        "put_slru_pages(yb_thin_client) branch={branch_id} chunk_rows={chunk_rows}"
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

    // ------------------- meta ---------------------------------------------

    pub async fn put_meta(&self, branch_id: BranchId, rows: &[MetaRow]) -> Result<()> {
        self.write_gate("write to amp_storage.meta");
        const COLS: usize = 4;
        // Encode once, ahead of the lane split: EVERY lane binds these bytes
        // (same discipline as `put_pages`). Most meta payloads — rel-size
        // sentinels are 4 bytes — fall under `MIN_COMPRESS_BYTES` and stay
        // raw; directory listings and aux files are the ones that benefit.
        let (encoded, tally) = self.encode_payloads("meta", rows.iter().map(|r| &r.payload));
        if let Some(pool) = &self.thinclient {
            let p = pool.current().await;
            let started = std::time::Instant::now();
            self.put_meta_thin(&p, branch_id, rows, &encoded).await?;
            crate::metrics::YB_STORE_OP_SECONDS
                .with_label_values(&["put_meta", "yb_thin_client"])
                .observe(started.elapsed().as_secs_f64());
            tally.record();
            return Ok(());
        }

        let started = std::time::Instant::now();
        let parallelism = pageserver_api::config::scale_count(2, self.scale_factor);

        let mut futs: Vec<futures::future::BoxFuture<'_, Result<()>>> = Vec::new();
        for (chunk, enc_chunk) in rows
            .chunks(META_ROWS_PER_INSERT)
            .zip(encoded.chunks(META_ROWS_PER_INSERT))
        {
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
                params.push(Box::new(branch_id));
                params.push(Box::new(row.key.to_vec()));
                params.push(Box::new(row.lsn.0 as i64));
                params.push(Box::new(enc_chunk[i].to_vec()));
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
        crate::metrics::YB_STORE_OP_SECONDS
            .with_label_values(&["put_meta", "ysql"])
            .observe(started.elapsed().as_secs_f64());
        tally.record();
        Ok(())
    }

    /// Direct-RPC lane for [`PageStore::put_meta`].
    ///
    /// `key_values` are in PRIMARY KEY order — `((branch_id) HASH, key
    /// ASC, lsn DESC)` — not declaration order; `payload` is the lone
    /// non-key column. Meta's hash key is `branch_id` alone, so unlike
    /// `pages`/`slru_pages` there is no bucket column to compute here.
    ///
    /// The write is an upsert, matching what the YSQL lane gets from
    /// `yb_enable_upsert_mode`: a row replayed by the ingest window's
    /// re-flush overwrites in place instead of failing on duplicate PK.
    async fn put_meta_thin(
        &self,
        p: &ThinClientTables,
        branch_id: BranchId,
        rows: &[MetaRow],
        encoded: &[Bytes],
    ) -> Result<()> {
        use yb_thin_client::{BindValue, UpsertRow};
        let meta = &p.meta;
        let parallelism = pageserver_api::config::scale_count(2, self.scale_factor);
        let branch_b = branch_id.to_bytes();

        let mut futs: Vec<futures::future::BoxFuture<'_, Result<()>>> = Vec::new();
        for (chunk, enc_chunk) in rows
            .chunks(META_ROWS_PER_INSERT)
            .zip(encoded.chunks(META_ROWS_PER_INSERT))
        {
            if chunk.is_empty() {
                continue;
            }
            let mut batch = Vec::with_capacity(chunk.len());
            for (i, (row, enc)) in chunk.iter().zip(enc_chunk).enumerate() {
                // Same length guard the YSQL lane applies. Nothing is
                // dispatched yet (the futures below are only polled by
                // `buffer_unordered`), so a bad key fails the whole batch
                // before any chunk reaches YB — fail-fast, as on that lane.
                if row.key.len() != KEY_SIZE {
                    bail!(
                        "put_meta row {i}: key bytes wrong length ({})",
                        row.key.len()
                    );
                }
                batch.push(UpsertRow::new(
                    meta,
                    vec![
                        BindValue::Bytes(branch_b.clone()),
                        BindValue::Bytes(row.key.clone()),
                        BindValue::I64(row.lsn.0 as i64),
                    ],
                    &[("payload", BindValue::Bytes(enc.clone()))],
                )?);
            }
            let client = Arc::clone(&p.client);
            let chunk_rows = batch.len();
            futs.push(Box::pin(async move {
                // Full retry budget inside `upsert`, no session rotation
                // (see put_pages' thin lane).
                client.upsert(&batch).await.with_context(|| {
                    format!("put_meta(yb_thin_client) branch={branch_id} chunk_rows={chunk_rows}")
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
        if let Some(pool) = &self.thinclient {
            let p = pool.current().await;
            let started = std::time::Instant::now();
            let row = self.read_meta_thin(&p, branch_id, key, target_lsn).await?;
            crate::metrics::YB_STORE_OP_SECONDS
                .with_label_values(&["read_meta", "yb_thin_client"])
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
            .query_opt(&sql, &[&branch_id, &key, &(target_lsn.0 as i64)])
            .await
            .context("read_meta")?;
        crate::metrics::YB_STORE_OP_SECONDS
            .with_label_values(&["read_meta", "ysql"])
            .observe(started.elapsed().as_secs_f64());
        row_opt.map(row_to_meta).transpose()
    }

    async fn read_meta_thin(
        &self,
        p: &ThinClientTables,
        branch_id: BranchId,
        key: &[u8],
        target_lsn: Lsn,
    ) -> Result<Option<MetaRow>> {
        use yb_thin_client::{BindValue, CondOp, RangeRead};
        let meta = &p.meta;
        // `branch_id` is the whole hash key; `key` is an equality cond.
        let read = RangeRead::new(meta, vec![BindValue::Bytes(branch_id.to_bytes())])?
            .cond(
                meta,
                "key",
                CondOp::Eq,
                BindValue::Bytes(Bytes::copy_from_slice(key)),
            )?
            .cond(meta, "lsn", CondOp::Le, BindValue::I64(target_lsn.0 as i64))?
            .targets(meta, &["lsn", "payload"])?
            .limit(1);
        // The meta PK clusters `lsn DESC`, so the forward scan's first row is the
        // newest version at or below the target — exactly what we want. `on_page`
        // returns `true` to stop as soon as that row is in hand; `read_scan`
        // still follows an empty deadline-paging continuation to reach it.
        let row = p
            .client
            .read_scan(
                meta,
                &read,
                || None::<yb_thin_client::Row>,
                |found: &mut Option<yb_thin_client::Row>, rows| -> anyhow::Result<bool> {
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
            .query(&sql, &[&branch_id, &start_bytes, &end_bytes, &target])
            .await
            .context("scan_meta_range")?;
        let mut out = HashMap::with_capacity(rows.len());
        for r in rows {
            let row = row_to_meta(r)?;
            out.insert(row.key.clone(), row);
        }
        Ok(out)
    }

    // ------------------- GC candidate queue --------------------------------

    /// Insert GC candidates (the single insert path — `bucket_id` is
    /// computed here, input values ignored). Replayed duplicates dedup:
    /// the candidate's identity is its PK and the connection runs upsert
    /// mode, so a re-insert overwrites in place. The lone non-PK column
    /// `relsize` is fixed by that identity (a shrink's size as of its
    /// `due_lsn`), so the overwrite stays byte-identical.
    pub async fn gc_enqueue_candidates(
        &self,
        branch_id: BranchId,
        rows: &[PendingGcRow],
    ) -> Result<()> {
        if rows.is_empty() {
            return Ok(());
        }
        if let Some(pool) = &self.thinclient {
            let p = pool.current().await;
            let started = std::time::Instant::now();
            self.gc_enqueue_candidates_thin(&p, branch_id, rows).await?;
            crate::metrics::YB_STORE_OP_SECONDS
                .with_label_values(&["gc_enqueue_candidates", "yb_thin_client"])
                .observe(started.elapsed().as_secs_f64());
            return Ok(());
        }

        let started = std::time::Instant::now();
        const COLS: usize = 6;
        // Sequential by design: candidate volume is tiny (a buffered
        // flush is ~one ≤500-row chunk); only the GC action deletes
        // get parallelism (`GC_ACTION_PARALLELISM`, own pool).
        let parallelism = 1;

        let mut futs: Vec<futures::future::BoxFuture<'_, Result<()>>> = Vec::new();
        for chunk in rows.chunks(ROWS_PER_INSERT) {
            let mut sql = format!(
                "INSERT INTO \"{}\".pending_gc \
                 (branch_id, bucket_id, gc_kind, key, due_lsn, relsize) \
                 VALUES ",
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
                    "(${}, ${}, ${}, ${}, ${}, ${})",
                    base,
                    base + 1,
                    base + 2,
                    base + 3,
                    base + 4,
                    base + 5,
                ));
                let bucket = gc_bucket_id(&row.key);
                params.push(Box::new(branch_id));
                params.push(Box::new(bucket));
                params.push(Box::new(row.gc_kind.to_i16()));
                params.push(Box::new(key_to_bytes(&row.key)));
                params.push(Box::new(row.due_lsn.0 as i64));
                params.push(Box::new(row.relsize));
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
                        format!("gc_enqueue_candidates branch={branch_id} chunk_rows={chunk_rows}")
                    })?;
                Ok(())
            }));
        }

        futures::stream::iter(futs)
            .buffer_unordered(parallelism)
            .try_collect::<Vec<_>>()
            .await?;
        crate::metrics::YB_STORE_OP_SECONDS
            .with_label_values(&["gc_enqueue_candidates", "ysql"])
            .observe(started.elapsed().as_secs_f64());
        Ok(())
    }

    /// Direct-RPC lane for [`PageStore::gc_enqueue_candidates`].
    ///
    /// `key_values` are in PRIMARY KEY order — `((branch_id, bucket_id)
    /// HASH, due_lsn, key, gc_kind)` — which is NOT the declaration order
    /// the YSQL INSERT's column list uses; `relsize` is the lone non-key
    /// column, and it is nullable (set on shrink candidates only), so a
    /// `None` binds as `BindValue::Null` rather than being omitted.
    ///
    /// `bucket_id` is computed here by `gc_bucket_id`, exactly as on the
    /// YSQL lane — it is placement only, deliberately not a generated
    /// column precisely so this lane can write the table. Unlike `pages`,
    /// `pending_gc` carries no bucket CHECK constraint, so nothing is lost
    /// by bypassing YSQL; a mis-computed bucket here would hide a
    /// candidate from the drain's per-bucket fan-out (a missed candidate,
    /// not a lost page).
    ///
    /// The upsert matches the YSQL lane's `yb_enable_upsert_mode` INSERT:
    /// a replayed candidate overwrites in place, and since `relsize` is
    /// fixed by the PK identity the overwrite stays byte-identical.
    async fn gc_enqueue_candidates_thin(
        &self,
        p: &ThinClientTables,
        branch_id: BranchId,
        rows: &[PendingGcRow],
    ) -> Result<()> {
        use yb_thin_client::{BindValue, UpsertRow};
        let pending_gc = &p.pending_gc;
        // Column metadata is constant for the batch — resolve once.
        let col_bucket = pending_gc.col("bucket_id")?;
        let col_kind = pending_gc.col("gc_kind")?;
        // Sequential, matching the YSQL lane: candidate volume is a
        // rounding error next to the page writes it trails.
        let parallelism = 1;
        let branch_b = branch_id.to_bytes();

        let mut futs: Vec<futures::future::BoxFuture<'_, Result<()>>> = Vec::new();
        for chunk in rows.chunks(ROWS_PER_INSERT) {
            let mut batch = Vec::with_capacity(chunk.len());
            for row in chunk {
                batch.push(UpsertRow::new(
                    pending_gc,
                    vec![
                        BindValue::Bytes(branch_b.clone()),
                        int_bind_thin(col_bucket, gc_bucket_id(&row.key) as i64),
                        BindValue::I64(row.due_lsn.0 as i64),
                        BindValue::Bytes(Bytes::from(key_to_bytes(&row.key))),
                        int_bind_thin(col_kind, row.gc_kind.to_i16() as i64),
                    ],
                    &[(
                        "relsize",
                        match row.relsize {
                            Some(n) => BindValue::I64(n),
                            None => BindValue::Null,
                        },
                    )],
                )?);
            }
            let client = Arc::clone(&p.client);
            let chunk_rows = batch.len();
            futs.push(Box::pin(async move {
                // Full retry budget inside `upsert`, no session rotation
                // (see put_pages' thin lane).
                client.upsert(&batch).await.with_context(|| {
                    format!(
                        "gc_enqueue_candidates(yb_thin_client) branch={branch_id} \
                         chunk_rows={chunk_rows}"
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

    /// Fetch up to `limit` due candidates (`floor < due_lsn <= cutoff`),
    /// globally oldest-first.
    ///
    /// `floor` is the caller's drain progress — a seek hint that skips
    /// the tombstones of already-drained rows; `Lsn(0)` scans from the
    /// head.
    pub async fn gc_select_due_candidates(
        &self,
        branch_id: BranchId,
        floor: Lsn,
        cutoff: Lsn,
        limit: usize,
    ) -> Result<GcSelectedCandidates> {
        let floor_i = floor.0 as i64;
        let cutoff_i = cutoff.0 as i64;
        let limit_i = limit as i64;

        // Per-bucket UNION ALL: YB plans it as an early-terminating
        // Merge Append over PK index scans (verified by EXPLAIN); the
        // inner ORDER BY + LIMIT per branch are what make it
        // merge-appendable. NB: a pending_gc query WITHOUT bucket_id
        // equality seq-scans and sorts — the hash key is
        // (branch_id, bucket_id).
        let sql = {
            use std::fmt::Write as _;
            let mut s = String::with_capacity(GC_BUCKETS as usize * 240 + 96);
            s.push_str("SELECT * FROM (");
            for bucket in 0..GC_BUCKETS {
                if bucket > 0 {
                    s.push_str(" UNION ALL ");
                }
                write!(
                    s,
                    "(SELECT gc_kind, key, due_lsn, relsize \
                      FROM \"{}\".pending_gc \
                      WHERE branch_id = $1 AND bucket_id = {bucket} \
                        AND due_lsn > $4 AND due_lsn <= $2 \
                      ORDER BY due_lsn LIMIT $3)",
                    self.schema
                )
                .unwrap();
            }
            s.push_str(") u ORDER BY due_lsn LIMIT $3");
            s
        };

        let rows = self
            .ysql_client
            .query(&sql, &[&branch_id, &cutoff_i, &limit_i, &floor_i])
            .await
            .context("gc_select_due_candidates")?;
        // Before the malformed filter, so a dropped row still counts (ASC
        // order ⇒ the last row is the max).
        let raw_count = rows.len();
        let max_due_lsn = rows.last().map(|r| Lsn(r.get::<_, i64>(2) as u64));
        let rows = rows
            .iter()
            .filter_map(|r| {
                let kb: Vec<u8> = r.get(1);
                // `Key::from_slice` assumes exactly KEY_SIZE bytes and would
                // panic (out-of-bounds) on a shorter `key` bytea; skip + alarm
                // a wrong-length key so one corrupt/foreign row can't take
                // down the drain. Not dequeued (no valid key to match on), so
                // it re-alarms on each floor reset.
                if kb.len() != pageserver_api::key::KEY_SIZE {
                    tracing::error!(
                        len = kb.len(),
                        "pending_gc row has a malformed key; skipped"
                    );
                    crate::metrics::CORRECTNESS_ERRORS
                        .with_label_values(&["gc_malformed_key"])
                        .inc();
                    return None;
                }
                Some(PendingGcRow {
                    gc_kind: GcKind::from_i16(r.get::<_, i16>(0)),
                    key: Key::from_slice(&kb),
                    due_lsn: Lsn(r.get::<_, i64>(2) as u64),
                    relsize: r.get(3),
                })
            })
            .collect();
        Ok(GcSelectedCandidates {
            rows,
            raw_count,
            max_due_lsn,
        })
    }

    /// Dequeue the visited trim candidates: a bucket-grouped range delete
    /// over `(floor, cutoff]`, matching each `(key, gc_kind)` pair. Same
    /// window as the select, so an object's duplicate/straggler candidates
    /// all go, not just acted ones — safe because the caller acted each to
    /// the cutoff and the materialiser clamps a late base above it (see
    /// `PendingGcBuffer`). Matching each `(key, gc_kind)` pair confines the
    /// sweep to the object's own kind: a same-`key` drop/shrink row
    /// (`rel_size_to_key` carries DropPages / ShrinkPages *and* TrimMeta)
    /// isn't matched — its pair was never passed. A busy-fork drop isn't
    /// passed here (acted, left queued). Returns rows removed.
    ///
    /// Corner: the materialiser's due_lsn clamp only holds against the
    /// cutoff as the flush read it: a flush that read it before this tick
    /// raised it can insert a block's image just after the trim acted,
    /// clamped below the now higher cutoff, so the dequeue clears that
    /// candidate unacted. Vanishingly rare, and the block's next base
    /// re-covers the garbage.
    pub async fn gc_dequeue_trim_candidates(
        &self,
        branch_id: BranchId,
        trims: &[(Key, GcKind)],
        floor: Lsn,
        cutoff: Lsn,
    ) -> Result<u64> {
        if trims.is_empty() {
            return Ok(0);
        }
        let floor_i = floor.0 as i64;
        let cutoff_i = cutoff.0 as i64;
        // Bucket by key (bucket_id is the hash of the key); within a bucket,
        // carry the parallel (key, gc_kind) arrays the `IN unnest(...)` zips.
        let mut by_bucket: std::collections::HashMap<i32, (Vec<Vec<u8>>, Vec<i16>)> =
            std::collections::HashMap::new();
        for (k, kind) in trims {
            let (keyb, kinds) = by_bucket.entry(gc_bucket_id(k)).or_default();
            keyb.push(key_to_bytes(k));
            kinds.push(kind.to_i16());
        }
        let sql = format!(
            "DELETE FROM \"{}\".pending_gc \
             WHERE branch_id = $1 AND bucket_id = $2 \
               AND due_lsn > $3 AND due_lsn <= $4 \
               AND (key, gc_kind) IN (SELECT * FROM unnest($5::bytea[], $6::smallint[]))",
            self.schema
        );
        let mut removed = 0u64;
        for (bucket, (keyb, kinds)) in by_bucket {
            let n = keyb.len();
            removed += self
                .ysql_client
                .execute(
                    &sql,
                    &[&branch_id, &bucket, &floor_i, &cutoff_i, &keyb, &kinds],
                )
                .await
                .with_context(|| {
                    format!("gc_dequeue_trim_candidates branch={branch_id} bucket={bucket} n={n}")
                })?;
        }
        Ok(removed)
    }

    /// Dequeue the given drop/shrink candidates by their PK
    /// `(key, gc_kind, due_lsn)`. Returns rows removed.
    pub async fn gc_dequeue_drop_shrink_candidates(
        &self,
        branch_id: BranchId,
        candidates: &[GcDropShrinkCandidateId],
    ) -> Result<u64> {
        if candidates.is_empty() {
            return Ok(0);
        }
        let mut by_bucket: std::collections::HashMap<i32, Vec<&GcDropShrinkCandidateId>> =
            std::collections::HashMap::new();
        for c in candidates {
            by_bucket.entry(gc_bucket_id(&c.0)).or_default().push(c);
        }
        let sql = format!(
            "DELETE FROM \"{}\".pending_gc \
             WHERE branch_id = $1 AND bucket_id = $2 \
               AND (key, gc_kind, due_lsn) IN ( \
                   SELECT * FROM unnest($3::bytea[], $4::smallint[], $5::bigint[]))",
            self.schema
        );
        let mut removed = 0u64;
        for (bucket, cs) in by_bucket {
            let mut keyb = Vec::with_capacity(cs.len());
            let mut kinds = Vec::with_capacity(cs.len());
            let mut due = Vec::with_capacity(cs.len());
            for c in &cs {
                keyb.push(key_to_bytes(&c.0));
                kinds.push(c.1.to_i16());
                due.push(c.2.0 as i64);
            }
            let n = cs.len();
            removed += self
                .ysql_client
                .execute(&sql, &[&branch_id, &bucket, &keyb, &kinds, &due])
                .await
                .with_context(|| {
                    format!(
                        "gc_dequeue_drop_shrink_candidates branch={branch_id} bucket={bucket} n={n}"
                    )
                })?;
        }
        Ok(removed)
    }

    /// Trim one fork's listed blocks: per block, keep the newest base
    /// (`will_init`) <= CUTOFF plus everything above; delete below,
    /// incl. same-LSN non-base twins. Greedy to the cutoff (not
    /// due_lsn); no base <= cutoff = block untouched. Returns rows
    /// deleted.
    ///
    /// Anchoring on `will_init` matches the read base (`is_image OR
    /// will_init`) because an image initializes the page (is_image =>
    /// will_init, true for all current writers). If that broke, the
    /// trim would under-delete (keep the image) — safe, never over.
    ///
    /// Deletes YB rows only — the in-RAM `UnifiedPages` read cache is
    /// NOT invalidated. Safe: trimmed rows are below the cutoff/anchor
    /// and admitted reads are >= cutoff, so reconstruction never reads
    /// them; they just linger in RAM until the key is evicted.
    /// TODO(gc_cache_invalidation): evict trimmed keys from
    /// `UnifiedPages` at the drain to reclaim that RAM promptly.
    ///
    /// TODO(gc_whole_fork_threshold): above a per-fork block count,
    /// switch to a whole-fork variant in blknum-range batches.
    pub async fn gc_act_trim_blocks(
        &self,
        branch_id: BranchId,
        (spcnode, dbnode, relnode, forknum): GcForkIdent,
        blknums: &[i64],
        cutoff: Lsn,
    ) -> Result<u64> {
        self.write_gate("GC trim of amp_storage.pages");
        if blknums.is_empty() {
            return Ok(0);
        }
        // `$8`=bucket_id, on both the anchor CTE and the DELETE — each is its
        // own scan of `pages` and needs the leading key column bound.
        let sql = format!(
            "WITH anchor AS ( \
                 SELECT blknum, MAX(lsn) AS lsn \
                 FROM \"{schema}\".pages \
                 WHERE bucket_id=$8 AND branch_id=$1 AND spcnode=$2 AND dbnode=$3 \
                   AND relnode=$4 AND forknum=$5 \
                   AND blknum = ANY($6) \
                   AND lsn <= $7 AND will_init \
                 GROUP BY blknum \
             ) \
             DELETE FROM \"{schema}\".pages p \
             USING anchor a \
             WHERE p.bucket_id=$8 AND p.branch_id=$1 AND p.spcnode=$2 AND p.dbnode=$3 \
               AND p.relnode=$4 AND p.forknum=$5 \
               AND p.blknum = a.blknum \
               AND (p.lsn < a.lsn \
                 OR (p.lsn = a.lsn AND NOT p.will_init))",
            schema = self.schema
        );
        // Candidate blocks are an arbitrary set, so they can straddle buckets:
        // group them and issue one bucket-pinned DELETE per group. Each block's
        // anchor is computed within its own bucket, which is where all of that
        // block's rows live, so the per-block semantics are unchanged.
        let mut by_bucket: Vec<Vec<i64>> = vec![Vec::new(); PAGE_BUCKETS as usize];
        for &blk in blknums {
            by_bucket[page_bucket_id(relnode, blk) as usize].push(blk);
        }
        let cutoff_i = cutoff.0 as i64;
        let mut deleted = 0u64;
        for (bucket, blks) in by_bucket.iter().enumerate() {
            if blks.is_empty() {
                continue;
            }
            let bucket = bucket as i16;
            deleted += self
                .ysql_client
                .execute(
                    &sql,
                    &[
                        &branch_id, &spcnode, &dbnode, &relnode, &forknum, blks, &cutoff_i, &bucket,
                    ],
                )
                .await
                .with_context(|| {
                    format!(
                        "gc_act_trim_blocks branch={branch_id} rel={relnode} fork={forknum} \
                         bucket={bucket} blocks={}",
                        blks.len()
                    )
                })?;
        }
        Ok(deleted)
    }

    /// Delete every `meta` row for the listed keys with `lsn <= due_lsn`.
    /// The shared drop primitive: a dropped rel fork / SLRU segment clears
    /// its size-sentinel key, and [`Self::gc_act_drop_meta`] clears a
    /// dead-key set — the `= ANY` form covers the single-key case too.
    /// Idempotent. Returns rows deleted.
    async fn delete_meta_up_to_lsn(
        &self,
        branch_id: BranchId,
        keys: &[Vec<u8>],
        due_lsn: Lsn,
    ) -> Result<u64> {
        if keys.is_empty() {
            return Ok(0);
        }
        let sql = format!(
            "DELETE FROM \"{schema}\".meta \
             WHERE branch_id=$1 AND key = ANY($2::bytea[]) AND lsn <= $3",
            schema = self.schema
        );
        self.ysql_client
            .execute(&sql, &[&branch_id, &keys, &(due_lsn.0 as i64)])
            .await
    }

    /// Drop action ([`GcKind::DropPages`]): reclaim a dropped fork. Delete
    /// every `pages` row with `lsn <= due_lsn` (= the drop's LSN) plus
    /// the fork's rel-size `meta` rows up to the same LSN. Scoped to
    /// `due_lsn`, NOT the cutoff, so a relnode reused above the drop
    /// keeps its newer versions/size. Idempotent. Returns `pages` rows
    /// deleted.
    ///
    /// Deliberately leaves the v2 sparse-reldir tombstone: under
    /// copy-on-write child reads an absent sparse key falls through to
    /// the ancestor, so the `Removed` marker is load-bearing (it shadows
    /// a parent's `Exists`) — deleting it could resurrect the rel in a
    /// child.
    pub async fn gc_act_drop_fork(
        &self,
        branch_id: BranchId,
        (spcnode, dbnode, relnode, forknum): GcForkIdent,
        due_lsn: Lsn,
    ) -> Result<u64> {
        self.write_gate("GC drop of an amp_storage.pages fork");
        let due = due_lsn.0 as i64;
        let pages_sql = format!(
            "DELETE FROM \"{schema}\".pages \
             WHERE bucket_id=$7 AND branch_id=$1 AND spcnode=$2 AND dbnode=$3 \
               AND relnode=$4 AND forknum=$5 AND lsn <= $6",
            schema = self.schema
        );
        // A whole fork spans every bucket it has blocks in, and we don't know
        // its size here, so fan out over all of them. One pinned DELETE per
        // bucket rather than one unpinned DELETE that would scan the table.
        let mut deleted = 0u64;
        for bucket in 0..PAGE_BUCKETS as i16 {
            deleted += self
                .ysql_client
                .execute(
                    &pages_sql,
                    &[
                        &branch_id, &spcnode, &dbnode, &relnode, &forknum, &due, &bucket,
                    ],
                )
                .await
                .with_context(|| {
                    format!(
                        "gc_act_drop_fork pages branch={branch_id} rel={relnode} \
                         fork={forknum} bucket={bucket}"
                    )
                })?;
        }

        // The fork's own rel-size meta key (<= due_lsn).
        let key_bytes = rel_size_key_bytes(spcnode, dbnode, relnode, forknum);
        self.delete_meta_up_to_lsn(branch_id, std::slice::from_ref(&key_bytes), due_lsn)
            .await
            .with_context(|| {
                format!("gc_act_drop_fork meta branch={branch_id} rel={relnode} fork={forknum}")
            })?;
        Ok(deleted)
    }

    /// Shrink action ([`GcKind::ShrinkPages`]): delete `pages` with
    /// `blknum >= relsize AND lsn <= due_lsn` (both from the candidate).
    /// The `lsn <= due_lsn` bound keeps a re-extended block live — its new
    /// base sits above `due_lsn`. Trap: candidates can't be merged per
    /// fork (a wider `blknum` × higher `lsn` bound would delete a block
    /// re-extended between two truncations). Returns rows deleted.
    pub async fn gc_act_shrink_fork(
        &self,
        branch_id: BranchId,
        (spcnode, dbnode, relnode, forknum): GcForkIdent,
        relsize: i64,
        due_lsn: Lsn,
    ) -> Result<u64> {
        self.write_gate("GC shrink of an amp_storage.pages fork");
        let sql = format!(
            "DELETE FROM \"{schema}\".pages \
             WHERE bucket_id=$8 AND branch_id=$1 AND spcnode=$2 AND dbnode=$3 \
               AND relnode=$4 AND forknum=$5 AND blknum >= $6 AND lsn <= $7",
            schema = self.schema
        );
        // `blknum >= relsize` has no upper bound — the fork's old length is
        // unknown here — so every bucket can hold matching blocks. Fan out
        // over all of them rather than dropping the leading-column bound.
        let due = due_lsn.0 as i64;
        let mut deleted = 0u64;
        for bucket in 0..PAGE_BUCKETS as i16 {
            deleted += self
                .ysql_client
                .execute(
                    &sql,
                    &[
                        &branch_id, &spcnode, &dbnode, &relnode, &forknum, &relsize, &due, &bucket,
                    ],
                )
                .await
                .with_context(|| {
                    format!(
                        "gc_act_shrink_fork branch={branch_id} rel={relnode} fork={forknum} \
                         relsize={relsize} due_lsn={due_lsn} bucket={bucket}"
                    )
                })?;
        }
        Ok(deleted)
    }

    /// Trim action for one SLRU segment's listed blocks: per block, keep
    /// the newest base (`will_init`) `<= cutoff` plus everything above;
    /// delete below. Single-tablet (`branch_id` is the hash key and
    /// `(kind, segno)` a bound range prefix within it). The `slru_pages`
    /// analog of [`Self::gc_act_trim_blocks`].
    pub async fn gc_act_trim_slru(
        &self,
        branch_id: BranchId,
        (kind, segno): GcSlruSeg,
        blknums: &[i64],
        cutoff: Lsn,
    ) -> Result<u64> {
        self.write_gate("GC trim of amp_storage.slru_pages");
        if blknums.is_empty() {
            return Ok(0);
        }
        let sql = format!(
            "WITH anchor AS ( \
                 SELECT blknum, MAX(lsn) AS lsn \
                 FROM \"{schema}\".slru_pages \
                 WHERE branch_id=$1 AND kind=$2 AND segno=$3 \
                   AND blknum = ANY($4) AND lsn <= $5 AND will_init \
                 GROUP BY blknum \
             ) \
             DELETE FROM \"{schema}\".slru_pages p \
             USING anchor a \
             WHERE p.branch_id=$1 AND p.kind=$2 AND p.segno=$3 \
               AND p.blknum = a.blknum \
               AND (p.lsn < a.lsn \
                 OR (p.lsn = a.lsn AND NOT p.will_init))",
            schema = self.schema
        );
        self.ysql_client
            .execute(
                &sql,
                &[&branch_id, &kind, &segno, &blknums, &(cutoff.0 as i64)],
            )
            .await
            .with_context(|| {
                format!(
                    "gc_act_trim_slru branch={branch_id} kind={kind} segno={segno} blocks={}",
                    blknums.len()
                )
            })
    }

    /// Drop action for a truncated SLRU segment: delete every `slru_pages`
    /// row for `(kind, segno)` with `lsn <= due_lsn`, plus the segment's
    /// size-sentinel `meta` key (`seg_size_key` bytes) up to the same LSN.
    /// `<= due_lsn` keeps a wraparound-recreated segno (written above the
    /// truncation) alive. Idempotent. Returns `slru_pages` rows deleted.
    pub async fn gc_act_drop_slru(
        &self,
        branch_id: BranchId,
        (kind, segno): GcSlruSeg,
        seg_size_key: Vec<u8>,
        due_lsn: Lsn,
    ) -> Result<u64> {
        self.write_gate("GC drop of amp_storage.slru_pages");
        let due = due_lsn.0 as i64;
        let pages_sql = format!(
            "DELETE FROM \"{schema}\".slru_pages \
             WHERE branch_id=$1 AND kind=$2 AND segno=$3 AND lsn <= $4",
            schema = self.schema
        );
        let deleted = self
            .ysql_client
            .execute(&pages_sql, &[&branch_id, &kind, &segno, &due])
            .await
            .with_context(|| {
                format!("gc_act_drop_slru pages branch={branch_id} kind={kind} segno={segno}")
            })?;
        self.delete_meta_up_to_lsn(branch_id, std::slice::from_ref(&seg_size_key), due_lsn)
            .await
            .with_context(|| {
                format!("gc_act_drop_slru meta branch={branch_id} kind={kind} segno={segno}")
            })?;
        Ok(deleted)
    }

    /// Trim action for `meta` keys: per key keep the newest version
    /// `<= cutoff`, delete everything below it. `meta` is full-overwrite
    /// (no `will_init`/chain), so the anchor is just each key's newest
    /// version `<= cutoff` (a grouped `MAX`, computed once per key then
    /// joined — the `meta` analog of [`Self::gc_act_trim_blocks`]). A key
    /// with no version `<= cutoff` is untouched (it's absent from the
    /// anchor). Returns rows deleted.
    pub async fn gc_act_trim_meta(
        &self,
        branch_id: BranchId,
        keys: &[Vec<u8>],
        cutoff: Lsn,
    ) -> Result<u64> {
        self.write_gate("GC trim of amp_storage.meta");
        if keys.is_empty() {
            return Ok(0);
        }
        let sql = format!(
            "WITH anchor AS ( \
                 SELECT key, MAX(lsn) AS lsn \
                 FROM \"{schema}\".meta \
                 WHERE branch_id=$1 AND key = ANY($2::bytea[]) AND lsn <= $3 \
                 GROUP BY key \
             ) \
             DELETE FROM \"{schema}\".meta m \
             USING anchor a \
             WHERE m.branch_id=$1 AND m.key = a.key AND m.lsn < a.lsn",
            schema = self.schema
        );
        self.ysql_client
            .execute(&sql, &[&branch_id, &keys, &(cutoff.0 as i64)])
            .await
            .with_context(|| format!("gc_act_trim_meta branch={branch_id} keys={}", keys.len()))
    }

    /// Drop action for dead `meta` keys (a retired object's directory /
    /// sentinel / two-phase / DROP DATABASE keys): delete every listed
    /// key's rows `<= due_lsn`. `<= due_lsn` keeps a recreated key (written
    /// above the drop) alive. Idempotent. Returns rows deleted.
    pub async fn gc_act_drop_meta(
        &self,
        branch_id: BranchId,
        keys: &[Key],
        due_lsn: Lsn,
    ) -> Result<u64> {
        self.write_gate("GC drop of amp_storage.meta");
        let key_bytes: Vec<Vec<u8>> = keys.iter().map(key_to_bytes).collect();
        self.delete_meta_up_to_lsn(branch_id, &key_bytes, due_lsn)
            .await
            .with_context(|| format!("gc_act_drop_meta branch={branch_id} keys={}", keys.len()))
    }

    /// Persist the GC read floor, monotonically, BEFORE any drain
    /// deletes. `GREATEST` keeps the column monotone even if the
    /// freshly-computed cutoff regressed (inputs can move backwards —
    /// e.g. a raised pitr_interval — while deletion can't).
    /// The write fence makes a superseded pageserver stop before draining:
    /// its in-memory lease set is stale, so its cutoff must not be
    /// allowed to delete anything.
    ///
    /// The LEAST subquery returns the minimum fork point among the
    /// branch's children, if any, so the applied cutoff =
    /// min(cutoff, min_children_fork_point). All child rows are
    /// accounted for: a deleted child can still be read until
    /// branch-deletion GC cleans it up (for instance, to restore it
    /// within the PITR interval) — only when that GC removes the
    /// child's entry from `branch_state` can the parent safely ignore
    /// it.
    ///
    /// The child min is deliberately read INSIDE this UPDATE: forking a
    /// child makes a dummy UPDATE to this same row (controller
    /// `RecordBranch`), so the fork and the cutoff advance serialize.
    /// That rests on the conflict loser re-reading: on a write-write
    /// conflict this statement re-executes at a fresh read time (read
    /// committed) or errors and the next GC tick retries — either way
    /// the child min is re-read past the fork's commit.
    pub async fn publish_gc_cutoff(&self, branch_id: BranchId, cutoff: Lsn) -> Result<Lsn> {
        self.write_gate("GC cutoff publish (the gate on every GC delete)");

        let sql = format!(
            "UPDATE \"{schema}\".branch_state \
             SET applied_gc_cutoff_lsn = GREATEST(applied_gc_cutoff_lsn, \
                 LEAST($2, (SELECT min(ancestor_lsn) \
                            FROM \"{schema}\".branch_state \
                            WHERE ancestor_branch_id = $1))) \
             WHERE branch_id = $1 \
             RETURNING applied_gc_cutoff_lsn",
            schema = self.schema
        );
        let row = self
            .ysql_client
            .query_opt(&sql, &[&branch_id, &(cutoff.0 as i64)])
            .await
            .context("publish_gc_cutoff")?
            .with_context(|| {
                format!(
                    "no branch_state row for {branch_id} — \
                     publish_gc_cutoff called before the creation write ran"
                )
            })?;
        Ok(Lsn(row.get::<_, i64>(0) as u64))
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

    /// Fill in the pageserver's initial column group on the branch's
    /// `branch_state` row, at branch creation (Phase 5b's
    /// `Timeline::write_timeline_row_to_yb`).
    ///
    /// The row itself is provisioned by the controller at branch-create time
    /// (`RecordBranch`), before any pageserver or safekeeper starts, and
    /// the safekeeper owns a separate group of columns on it. So this only ever
    /// UPDATEs — it never creates the row, and it must not disturb the
    /// safekeeper's columns.
    ///
    /// Sets `pageserver_initialized`, the flag readers key bootstrap-vs-load
    /// off. This is the flag's only writer and it flips in the same statement
    /// that fills the group, so the two can't disagree.
    ///
    /// `consistent_lsn` and `prev_record_lsn` are written `COALESCE(col, $n)`
    /// because [`Self::advance_consistent_lsn`] also writes them and may get
    /// there first: `UninitializedTimeline::write` spawns the flush loop and
    /// flushes before `finish_creation` calls us. "Only if absent" is the right
    /// rule for both — on bootstrap the flush owns them (we pass `None` for
    /// `prev_record_lsn`, which would otherwise erase a good value), and on a
    /// fork no flush has run and `create_branch_row` must supply them. The
    /// other columns have no second writer before creation.
    ///
    /// **Not for incremental updates.** Per-flush `consistent_lsn` UPDATEs go
    /// through `advance_consistent_lsn` and the GC cutoff through
    /// [`Self::publish_gc_cutoff`].
    pub async fn update_branch_initial_state(&self, row: &BranchStateRow) -> Result<()> {
        let sql = format!(
            // `pg_version` is deliberately absent: the controller seeds it at
            // branch create and is its only writer, so setting it here would
            // clobber its owner.
            "UPDATE \"{}\".branch_state SET \
                 consistent_lsn        = COALESCE(consistent_lsn, $2), \
                 prev_record_lsn       = COALESCE(prev_record_lsn, $7), \
                 applied_gc_cutoff_lsn = $3, \
                 ancestor_branch_id    = $4, \
                 ancestor_lsn          = $5, \
                 initdb_lsn            = $6, \
                 pageserver_initialized = true \
             WHERE branch_id = $1",
            self.schema
        );
        let n = self
            .ysql_client
            .execute(
                &sql,
                &[
                    &row.branch_id,
                    &(row.consistent_lsn.0 as i64),
                    &(row.applied_gc_cutoff_lsn.0 as i64),
                    &row.ancestor_branch_id,
                    &row.ancestor_lsn.map(|l| l.0 as i64),
                    &row.initdb_lsn.map(|l| l.0 as i64),
                    &row.prev_record_lsn.map(|l| l.0 as i64),
                ],
            )
            .await
            .context("update_branch_initial_state")?;
        if n == 0 {
            // The predicate is the primary key alone, so zero rows can only
            // mean the row isn't there — the probe turns that into the "branch
            // was never provisioned" message. It must not be a silent success
            // any more: that's what hid the dropped creation write.
            self.assert_branch_state_row_exists(row.branch_id).await?;
            anyhow::bail!(
                "update_branch_initial_state matched no row for {} even though \
                 the row exists — unreachable, branch_state is keyed on \
                 (branch_id) alone",
                row.branch_id
            );
        }
        Ok(())
    }

    /// Test-only: provision the branch's `branch_state` row the way the
    /// controller does at branch create (`Store.RecordBranch`). Production
    /// code never creates this row — the tests need it because they run against
    /// the production schema with random branch ids and no controller.
    #[cfg(test)]
    pub(crate) async fn provision_branch_row_for_test(
        &self,
        branch_id: BranchId,
        pg_version: u32,
    ) -> Result<()> {
        let sql = format!(
            "INSERT INTO \"{}\".branch_state (branch_id, pg_version) VALUES ($1, $2) \
             ON CONFLICT (branch_id) DO NOTHING",
            self.schema
        );
        self.ysql_client
            .execute(&sql, &[&branch_id, &(pg_version as i32)])
            .await
            .context("provision_branch_row_for_test")?;
        Ok(())
    }

    /// Read the controller-seeded fields of the branch's row. Unlike
    /// [`Self::get_branch_state`] this does **not** require the pageserver's own
    /// column group to be populated — it is what attach consults *before*
    /// creating the timeline. `None` means the controller never provisioned the
    /// branch.
    pub async fn get_branch_seed(&self, branch_id: BranchId) -> Result<Option<BranchSeed>> {
        let sql = format!(
            "SELECT pg_version, ancestor_branch_id, ancestor_lsn \
             FROM \"{}\".branch_state WHERE branch_id = $1",
            self.schema
        );
        let Some(r) = self
            .ysql_client
            .query_opt(&sql, &[&branch_id])
            .await
            .context("get_branch_seed")?
        else {
            return Ok(None);
        };
        let pg_version = r.try_get::<_, i32>(0)? as u32;
        let ancestor_branch_id = r.try_get::<_, Option<BranchId>>(1)?;
        let ancestor_lsn: Option<i64> = r.try_get(2)?;
        Ok(Some(BranchSeed {
            pg_version,
            ancestor_branch_id,
            ancestor_lsn: ancestor_lsn.map(|l| Lsn(l as u64)),
        }))
    }

    /// Confirm the branch's `branch_state` row exists — used when a
    /// pageserver-group UPDATE matched nothing, to tell "already populated"
    /// (fine) from "never provisioned" (a wiring bug).
    async fn assert_branch_state_row_exists(&self, branch_id: BranchId) -> Result<()> {
        let sql = format!(
            "SELECT 1 FROM \"{}\".branch_state WHERE branch_id = $1",
            self.schema
        );
        let found = self
            .ysql_client
            .query_opt(&sql, &[&branch_id])
            .await
            .context("branch_state existence probe")?;
        if found.is_none() {
            anyhow::bail!(
                "no branch_state row for {branch_id} — the controller provisions \
                 it at branch create, so the pageserver started against an \
                 unprovisioned branch"
            );
        }
        Ok(())
    }

    pub async fn get_branch_state(&self, branch_id: BranchId) -> Result<Option<BranchStateRow>> {
        let sql = format!(
            // `pageserver_initialized` is the pageserver's presence predicate:
            // the row is shared with the safekeeper, so a row that exists with
            // our group unpopulated means *our* state doesn't yet exist.
            // Callers (preload / attach) key bootstrap-vs-load off this `None`,
            // so it has to mean "no pageserver state", not "no row".
            //
            // The flag is written only by `update_branch_initial_state`, in the
            // same statement that fills the group. A data column can't do this
            // job: `consistent_lsn` held it until #552, and
            // `advance_consistent_lsn` sets that column first on the bootstrap
            // path, so the predicate went true before the group existed.
            //
            // The flag says the group was filled, but `row_to_branch_state`
            // still deserializes it as nullable — defence in depth against a
            // NULL, whose failure mode is a crash-loop on attach (#531).
            "SELECT {} FROM \"{}\".branch_state \
             WHERE branch_id = $1 AND pageserver_initialized",
            BRANCH_STATE_COLUMNS, self.schema
        );
        let row = self
            .ysql_client
            .query_opt(&sql, &[&branch_id])
            .await
            .context("get_branch_state")?;
        row.map(row_to_branch_state).transpose()
    }

    /// Build the ordered lineage for `branch_id` by walking
    /// `ancestor_branch_id` / `ancestor_lsn` links in
    /// `branch_state`. Segment 0 is the timeline itself at
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
        let mut row = self.get_branch_state(branch_id).await?.with_context(|| {
            format!("load_lineage: timeline {branch_id} has no branch_state row")
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
                .get_branch_state(anc_id)
                .await?
                .with_context(|| format!("load_lineage: ancestor {anc_id} row missing"))?;
        }
        Ok(segments)
    }

    /// Create the `branch_state` row for a branch of `ancestor` at
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
    ///   (defense-in-depth: the controller's `RecordBranch` already
    ///   validated this at registration, and once the child row is
    ///   committed the ancestor's cutoff cannot advance past its fork
    ///   point — see [`Self::publish_gc_cutoff`])
    ///
    /// The new row's `consistent_lsn` is set to `ancestor_lsn` — the
    /// walreceiver resume point. It must never be 0 (a 0 would make
    /// the safekeeper stream from segment 1). Idempotent on crash-retry
    /// via the column-scoped UPDATE in [`Self::update_branch_initial_state`].
    pub async fn create_branch_row(
        &self,
        new_branch: BranchId,
        ancestor: BranchId,
        ancestor_lsn: Lsn,
    ) -> Result<BranchStateRow> {
        let anc = self.get_branch_state(ancestor).await?.with_context(|| {
            format!("create_branch_row: ancestor {ancestor} has no branch_state row")
        })?;
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
        let row = BranchStateRow {
            branch_id: new_branch,
            consistent_lsn: ancestor_lsn,
            applied_gc_cutoff_lsn: anc.applied_gc_cutoff_lsn,
            ancestor_branch_id: Some(ancestor),
            ancestor_lsn: Some(ancestor_lsn),
            pg_version: anc.pg_version,
            initdb_lsn: anc.initdb_lsn,
            prev_record_lsn,
        };
        self.update_branch_initial_state(&row).await?;
        Ok(row)
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
    /// Scoped to two columns → safe to run concurrently with
    /// `advance_gc_cutoff_lsn` / `upsert_timeline`. Each touches disjoint
    /// columns.
    ///
    /// Not fenced by this statement: the endpoint's write lease is checked by
    /// [`Self::write_gate`] just below, before the write rather than being
    /// discovered after it, and it covers every process in the endpoint.
    pub async fn advance_consistent_lsn(
        &self,
        branch_id: BranchId,
        new_dc_lsn: Lsn,
        new_prev_record_lsn: Option<Lsn>,
    ) -> Result<()> {
        self.write_gate("durability commit (branch_state.consistent_lsn)");

        // Persist `prev_record_lsn` alongside `consistent_lsn`
        // so a restart can hand the safekeeper the right `xl_prev`
        // for the record at `consistent_lsn`. `COALESCE($3,
        // prev_record_lsn)` keeps the existing column value when
        // the caller didn't resolve a fresh pairing (race —
        // pages flushed before their record's `finish_write` ran;
        // see `PageWriter::take_prev_at_or_below`). Without
        // COALESCE we'd overwrite a previously-good value with
        // NULL on every throttled flush window.
        let sql = format!(
            "UPDATE \"{}\".branch_state \
             SET consistent_lsn = $2, \
                 prev_record_lsn      = COALESCE($3, prev_record_lsn) \
             WHERE branch_id = $1",
            self.schema
        );
        let prev_i64: Option<i64> = new_prev_record_lsn.map(|l| l.0 as i64);
        let n = self
            .ysql_client
            .execute(&sql, &[&branch_id, &(new_dc_lsn.0 as i64), &prev_i64])
            .await
            .context("advance_consistent_lsn")?;
        if n == 0 {
            anyhow::bail!(
                "no branch_state row for {branch_id} — \
                 advance_consistent_lsn called before the creation write ran"
            );
        }
        Ok(())
    }

    /// The write fence, checked immediately before a write reaches YugabyteDB.
    ///
    /// Armed unless `require_write_token` is turned off, and one relaxed load
    /// either way. A lapsed endpoint lease ends this process here rather than
    /// letting the write land — the
    /// fence has to remove the writer, not the write. See
    /// `compute/architecture/compute-fencing.md`.
    #[inline]
    fn write_gate(&self, what: &str) {
        utils::write_fence::global().enforce(what);
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
    //! [`normalize_chain`] turns direct-lane storage order (`lsn DESC,
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

/// Boundary tests for the payload compression envelope. The codec itself is
/// covered in `utils::payload_codec`; what matters here is that
/// `PageStore`'s single decode funnel is the exact inverse of what the
/// `put_*` paths encode, for every codec — including codecs this build is
/// not configured to write.
#[cfg(test)]
mod payload_envelope_tests {
    use super::*;
    use utils::payload_codec::Codec;

    /// Every write lane must bind the `encode_payloads` output, never a
    /// row's plaintext `payload`.
    ///
    /// This is a source-level guard because the failure it catches is
    /// invisible to the type system and to every other test here. When a new
    /// client lane is added upstream, its `put_*` variant is *new code* — it
    /// conflicts with nothing, compiles fine, and silently writes untagged
    /// plaintext that the read path then rejects. That is exactly how the
    /// `yb_thin_client` lane shipped broken: clean auto-merge, clippy clean,
    /// every unit test green, and the cluster failed to start because
    /// basebackup could not decode its own pages.
    ///
    /// Unit tests can't cover it because they exercise one lane at a time
    /// against whichever client is configured; only an end-to-end run on the
    /// *default* lane surfaces it. So: assert on the source.
    #[test]
    fn every_write_lane_binds_encoded_payloads() {
        let src = include_str!("page_store.rs");

        // Needles are assembled from fragments so this test's own source
        // does not match them — otherwise the guard flags itself.
        let bind_prefix = format!("(\"pay{}\", BindValue::Bytes(", "load");
        let row_payload = format!("row.pay{}", "load");
        let payload_clone = format!(".pay{}.clone()", "load");
        let ysql_box = format!("params.push(Box::new(row.pay{}", "load");
        let ysql_map = format!("map(|r| r.pay{}.as_ref())", "load");

        // The thin lane binds through `BindValue::Bytes(..)`. Any occurrence
        // naming a row's payload directly is a lane that skipped the codec.
        let raw_binds: Vec<_> = src
            .lines()
            .enumerate()
            .filter(|(_, l)| {
                l.contains(&bind_prefix) && (l.contains(&row_payload) || l.contains(&payload_clone))
            })
            .map(|(i, l)| format!("  line {}: {}", i + 1, l.trim()))
            .collect();
        assert!(
            raw_binds.is_empty(),
            "these write lanes bind a row's PLAINTEXT payload instead of the \
             encoded buffer from `encode_payloads` — pages written this way \
             have no codec tag and `decode_payload` will reject them on \
             read:\n{}",
            raw_binds.join("\n")
        );

        // The YSQL lanes bind through positional params.
        let raw_params: Vec<_> = src
            .lines()
            .enumerate()
            .filter(|(_, l)| l.contains(&ysql_box) || l.contains(&ysql_map))
            .map(|(i, l)| format!("  line {}: {}", i + 1, l.trim()))
            .collect();
        assert!(
            raw_params.is_empty(),
            "these YSQL write paths bind a row's PLAINTEXT payload:\n{}",
            raw_params.join("\n")
        );

        // Sanity: the guard is only meaningful if it can see the binds it is
        // policing. If the bind syntax is refactored, this fails loudly
        // rather than silently passing on zero matches.
        // Two: the thin lane's `pages` and `slru_pages` writes. (It was four
        // while the retired direct lane had its own pair.) Meta has no direct
        // write lane, so it contributes none.
        let encoded_binds = src.matches(&bind_prefix).count();
        assert!(
            encoded_binds >= 2,
            "expected at least 2 thin-lane payload binds to police, saw \
             {encoded_binds} — the bind syntax changed and this guard has \
             gone blind"
        );
    }

    fn pagelike() -> Bytes {
        let mut p = vec![0u8; 8192];
        for (i, b) in p[..64].iter_mut().enumerate() {
            *b = i as u8;
        }
        for (i, b) in p[6000..].iter_mut().enumerate() {
            *b = (i % 17) as u8;
        }
        Bytes::from(p)
    }

    /// Whatever a writer emits, the read funnel returns the original bytes.
    #[test]
    fn decode_payload_inverts_every_codec() {
        let plain = pagelike();
        for codec in [Codec::Raw, Codec::Lz4] {
            let (stored, _) = payload_codec::encode(&plain, codec);
            let back = decode_payload(&stored, || "test row").expect("decode");
            assert_eq!(back, plain, "roundtrip broken for {codec:?}");
        }
    }

    /// The property that makes a codec change a restart rather than a
    /// migration: one reader, rows written under different settings. A real
    /// delta chain looks like this after the config is flipped.
    #[test]
    fn one_reader_handles_a_mixed_codec_chain() {
        let plain = pagelike();
        let chain: Vec<Bytes> = [Codec::Lz4, Codec::Raw, Codec::Lz4, Codec::Raw]
            .into_iter()
            .map(|c| payload_codec::encode(&plain, c).0)
            .collect();
        // The rows genuinely differ on the wire...
        assert_ne!(chain[0], chain[1]);
        // ...and all decode to the same page.
        for (i, stored) in chain.iter().enumerate() {
            assert_eq!(
                decode_payload(stored, || "test row").expect("decode"),
                plain,
                "chain step {i}"
            );
        }
    }

    /// A tag this build does not know must fail loudly, with the row named
    /// and the offending tag reported — never be reinterpreted as page
    /// bytes, which would surface later as walredo corruption.
    #[test]
    fn decode_payload_rejects_unknown_codec_with_context() {
        let err = decode_payload(
            &Bytes::from_static(&[0x42, 0x00]),
            || "pages row key=K lsn=L",
        )
        .expect_err("unknown tag must fail");
        let chain = format!("{err:#}");
        assert!(chain.contains("pages row key=K lsn=L"), "{chain}");
        assert!(chain.contains("0x42"), "{chain}");
    }

    /// An empty `BYTEA` is not a valid envelope — every encode emits at
    /// least a tag byte, so a zero-length value means the row was written by
    /// something that bypassed the codec.
    #[test]
    fn decode_payload_rejects_empty_value() {
        assert!(decode_payload(&Bytes::new(), || "pages row").is_err());
    }

    /// Deltas dominate the row count and are mostly a few bytes; they must
    /// stay raw so the hot write path spends nothing on them.
    #[test]
    fn tiny_delta_payload_is_stored_raw() {
        let (stored, outcome) = payload_codec::encode(&[0x01], Codec::Lz4);
        assert_eq!(
            outcome,
            payload_codec::Outcome::Raw(payload_codec::RawReason::TooSmall)
        );
        assert_eq!(stored.len(), 2, "one tag byte + the payload");
        assert_eq!(
            decode_payload(&stored, || "delta row").unwrap(),
            &[0x01][..]
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
    let key = Key::from_slice(&columns_to_key_bytes(sp, db, rel, fork, blk));
    let lsn = Lsn(lsn as u64);
    Ok(PageRow {
        key,
        lsn,
        is_image,
        will_init,
        payload: decode_payload(&Bytes::from(payload), || {
            format!("pages row key={key} lsn={lsn} (ysql)")
        })?,
    })
}

fn row_to_meta(r: tokio_postgres::Row) -> Result<MetaRow> {
    let key: Vec<u8> = r.try_get(0)?;
    let lsn: i64 = r.try_get(1)?;
    let payload: Vec<u8> = r.try_get(2)?;
    let lsn = Lsn(lsn as u64);
    Ok(MetaRow {
        payload: decode_payload(&Bytes::from(payload), || {
            format!("meta row key={} lsn={lsn} (ysql)", hex::encode(&key))
        })?,
        key: Bytes::from(key),
        lsn,
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
    let kind = slru_kind_from_i16(kind_i)?;
    let lsn = Lsn(lsn as u64);
    Ok(SlruPageRow {
        kind,
        segno: segno as u32,
        blknum: blknum as u32,
        lsn,
        is_image,
        will_init,
        payload: decode_payload(&Bytes::from(payload), || {
            format!("slru_pages row kind={kind:?} segno={segno} blknum={blknum} lsn={lsn} (ysql)")
        })?,
    })
}

/// Column list shared by every `SELECT … FROM branch_state` so the
/// query layout stays in sync with `row_to_branch_state`'s positional
/// `try_get` indices.
const BRANCH_STATE_COLUMNS: &str = "branch_id, consistent_lsn, \
     applied_gc_cutoff_lsn, ancestor_branch_id, ancestor_lsn, \
     pg_version, initdb_lsn, prev_record_lsn";

fn row_to_branch_state(r: tokio_postgres::Row) -> Result<BranchStateRow> {
    let branch_id: BranchId = r.try_get(0)?;
    let consistent_lsn: i64 = r.try_get(1)?;
    // Nullable on purpose, as defence in depth. `pageserver_initialized` plus
    // the `COALESCE` creation write mean a bootstrapped branch now always has
    // this column, but the cost of tolerating a NULL is one `unwrap_or(0)` and
    // the cost of not tolerating it is a crash-loop, so this stays.
    // Reading it as a plain `i64` made a NULL a hard deserialize error
    // that killed the process during attach — and since attach is retried
    // forever, a crash-loop (#531). `NULL` means the cutoff was never
    // stamped, i.e. nothing has been GC'd on this branch yet, so 0 is both
    // true and safe: it cannot resurrect reclaimed data, and the next
    // `publish_gc_cutoff` (`GREATEST(applied_gc_cutoff_lsn, …)`, NULL-
    // tolerant) stamps the real value.
    let applied_gc_cutoff_lsn: i64 = r.try_get::<_, Option<i64>>(2)?.unwrap_or(0);
    let ancestor_tl: Option<BranchId> = r.try_get(3)?;
    let ancestor_lsn: Option<i64> = r.try_get(4)?;
    let pg_version: i32 = r.try_get(5)?;
    let initdb_lsn: Option<i64> = r.try_get(6)?;
    let prev_record_lsn: Option<i64> = r.try_get(7)?;

    Ok(BranchStateRow {
        branch_id,
        consistent_lsn: Lsn(consistent_lsn as u64),
        applied_gc_cutoff_lsn: Lsn(applied_gc_cutoff_lsn as u64),
        ancestor_branch_id: ancestor_tl,
        ancestor_lsn: ancestor_lsn.map(|l| Lsn(l as u64)),
        pg_version: pg_version as u32,
        initdb_lsn: initdb_lsn.map(|l| Lsn(l as u64)),
        prev_record_lsn: prev_record_lsn.map(|l| Lsn(l as u64)),
    })
}

#[cfg(test)]
mod tests {
    use super::*;
    use pageserver_api::key::rel_block_to_key;

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
            tsclient_read_sessions: None,
            tsclient_write_sessions: None,
            tsclient_sessions_per_conn: None,
            connect_timeout_ms: ysql_client::DEFAULT_CONNECT_TIMEOUT_MS,
            statement_timeout_ms: ysql_client::DEFAULT_STATEMENT_TIMEOUT_MS,
            tcp_user_timeout_ms: 0,
            connection_lifetime_ms: ysql_client::DEFAULT_CONNECTION_LIFETIME_MS,
        }
    }

    /// All tests run against the production `amp_storage` schema
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
    // Never assert exact DELETE rowcounts in these tests: ysql_client
    // retries transient YB errors at-least-once, so a retry after a
    // committed-but-unacked first attempt reports the SECOND
    // execution's count (undercounts under load — seen in CI). Assert
    // the surviving rows instead; `== 0` counts are retry-safe.
    const PROD_SCHEMA: &str = "amp_storage";

    /// Create a branch the way production does: the controller provisions the
    /// row, then the pageserver fills in its own column group.
    async fn create_branch_state_row(
        store: &PageStore,
        row: &BranchStateRow,
    ) -> anyhow::Result<()> {
        store
            .provision_branch_row_for_test(row.branch_id, row.pg_version)
            .await?;
        store.update_branch_initial_state(row).await
    }

    async fn make_store(_suffix: &str) -> (PageStore, String) {
        let store = SHARED_STORE
            .get_or_init(|| async {
                let cfg = PageStoreConfig {
                    yb: test_yb_config(),
                    schema: PROD_SCHEMA.to_string(),
                    scale_factor: pageserver_api::config::DEFAULT_SCALE_FACTOR,
                    payload_compression:
                        pageserver_api::config::defaults::DEFAULT_YB_PAYLOAD_COMPRESSION,
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

        // pages: 10 per-row params (bucket_id leads the bound columns, right
        // after the baked branch_id); 2 rows → $1..$20, id baked per row.
        let pages = build_insert_sql(
            "amp_storage",
            "pages",
            PAGES_COLUMNS,
            branch,
            PAGES_PER_ROW,
            2,
        );
        let expected_pages = format!(
            "INSERT INTO \"amp_storage\".pages ({PAGES_COLUMNS}) VALUES \
             ({id},$1,$2,$3,$4,$5,$6,$7,$8,$9,$10),\
             ({id},$11,$12,$13,$14,$15,$16,$17,$18,$19,$20)"
        );
        assert_eq!(pages, expected_pages);

        // slru: 7 per-row params; 1 row → $1..$7.
        let slru = build_insert_sql(
            "amp_storage",
            "slru_pages",
            SLRU_COLUMNS,
            branch,
            SLRU_PER_ROW,
            1,
        );
        let expected_slru = format!(
            "INSERT INTO \"amp_storage\".slru_pages ({SLRU_COLUMNS}) VALUES \
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

    fn sample_branch_state_row(branch: BranchId) -> BranchStateRow {
        BranchStateRow {
            branch_id: branch,
            consistent_lsn: Lsn(90),
            applied_gc_cutoff_lsn: Lsn(50),
            ancestor_branch_id: None,
            ancestor_lsn: None,
            pg_version: 180_002,
            initdb_lsn: None,
            prev_record_lsn: None,
        }
    }

    #[tokio::test]
    async fn update_branch_initial_state_requires_a_provisioned_row() {
        // The controller provisions `branch_state` at branch create; the
        // pageserver only ever fills in its own column group. If the row is
        // missing, that is a wiring bug (a daemon started against a branch the
        // controller never recorded) and must fail loudly rather than silently
        // dropping the state — this is the contract the whole shared-row design
        // rests on, so pin it.
        let (store, schema) = make_store("unprovisioned").await;
        let row = sample_branch_state_row(rand_branch());
        // Deliberately skip provision_branch_row_for_test.
        let err = store.update_branch_initial_state(&row).await.unwrap_err();
        assert!(
            err.to_string().contains("no branch_state row"),
            "expected the unprovisioned-branch bail, got: {err}"
        );
        drop_schema(&store, &schema).await;
    }

    #[tokio::test]
    async fn update_branch_initial_state_is_idempotent_once_populated() {
        // Re-running creation must not move `consistent_lsn` — that column is
        // `COALESCE`d because the flush loop co-owns it — and must not be
        // mistaken for the missing-row case above.
        let (store, schema) = make_store("recreate").await;
        let branch = rand_branch();
        let row = sample_branch_state_row(branch);
        create_branch_state_row(&store, &row).await.unwrap();

        let mut second = row.clone();
        second.consistent_lsn = Lsn(row.consistent_lsn.0 + 1000);
        store.update_branch_initial_state(&second).await.unwrap();

        let got = store.get_branch_state(branch).await.unwrap().unwrap();
        assert_eq!(got, row, "existing state is the source of truth");
        drop_schema(&store, &schema).await;
    }

    #[tokio::test]
    async fn write_and_read_branch_state_round_trip() {
        let (store, schema) = make_store("tl_rt").await;
        let branch = rand_branch();
        let row = sample_branch_state_row(branch);
        create_branch_state_row(&store, &row).await.unwrap();
        let got = store.get_branch_state(branch).await.unwrap().unwrap();
        assert_eq!(got, row);
        drop_schema(&store, &schema).await;
    }

    /// #552: the bootstrap write order, reproduced exactly. The initdb import's
    /// flush lands `consistent_lsn` via `advance_consistent_lsn` — which writes
    /// that column and nothing else — and only then does `finish_creation` run
    /// the creation write. Scoped `WHERE consistent_lsn IS NULL` it matched zero
    /// rows and silently dropped the whole group; per-column `COALESCE` fills in
    /// what is still missing.
    #[tokio::test]
    async fn creation_write_fills_columns_left_by_a_prior_consistent_lsn() {
        let (store, schema) = make_store("creation_after_flush").await;
        let branch = rand_branch();
        let row = sample_branch_state_row(branch);

        // 1. The controller seeds the row.
        store
            .provision_branch_row_for_test(branch, row.pg_version)
            .await
            .unwrap();

        // 2. The bootstrap import's flush sets `consistent_lsn` alone — the
        //    write that falsifies the old `consistent_lsn IS NULL` predicate.
        let flushed = Lsn(row.consistent_lsn.0 + 4096);
        store
            .advance_consistent_lsn(branch, flushed, None)
            .await
            .unwrap();

        // 3. finish_creation's write must still populate the rest.
        let mut created = row.clone();
        created.initdb_lsn = Some(Lsn(0x2000));
        store.update_branch_initial_state(&created).await.unwrap();

        let got = store.get_branch_state(branch).await.unwrap().unwrap();
        assert_eq!(
            got.consistent_lsn, flushed,
            "the flushed value is live state and must win"
        );
        assert_eq!(got.applied_gc_cutoff_lsn, created.applied_gc_cutoff_lsn);
        assert_eq!(got.initdb_lsn, created.initdb_lsn);

        drop_schema(&store, &schema).await;
    }

    /// The presence predicate is `pageserver_initialized`, not any data column.
    /// A `consistent_lsn` landed by the bootstrap import's flush must NOT make
    /// the branch look loadable: the import hasn't finished, so a crash here
    /// has to re-bootstrap. Under the old `consistent_lsn IS NOT NULL`
    /// predicate the next attach took the *load* path over a half-imported
    /// datadir.
    #[tokio::test]
    async fn consistent_lsn_alone_does_not_mark_the_branch_initialized() {
        let (store, schema) = make_store("init_flag").await;
        let branch = rand_branch();
        let row = sample_branch_state_row(branch);

        store
            .provision_branch_row_for_test(branch, row.pg_version)
            .await
            .unwrap();
        store
            .advance_consistent_lsn(branch, Lsn(0x9000), None)
            .await
            .unwrap();

        assert!(
            store.get_branch_state(branch).await.unwrap().is_none(),
            "consistent_lsn is set, but the creation write never ran — attach \
             must still choose bootstrap"
        );

        // Completing creation flips the flag and the branch becomes loadable.
        store.update_branch_initial_state(&row).await.unwrap();
        assert!(store.get_branch_state(branch).await.unwrap().is_some());

        drop_schema(&store, &schema).await;
    }

    /// #531: `get_branch_state`'s presence predicate is `consistent_lsn IS
    /// NOT NULL` alone, so it can select a row whose other pageserver-owned
    /// columns are still the controller's NULL seed. Reading
    /// `applied_gc_cutoff_lsn` as a non-nullable `i64` turned that into
    /// "error deserializing column 2: a Postgres value was `NULL`", which
    /// aborted attach and crash-looped the pageserver. A NULL cutoff means
    /// nothing has been GC'd yet, so it must read back as 0.
    #[tokio::test]
    async fn get_branch_state_tolerates_null_gc_cutoff() {
        let (store, schema) = make_store("null_cutoff").await;
        let branch = rand_branch();
        let row = sample_branch_state_row(branch);
        create_branch_state_row(&store, &row).await.unwrap();

        store
            .test_only_execute(&format!(
                "UPDATE \"{schema}\".branch_state SET applied_gc_cutoff_lsn = NULL \
                 WHERE branch_id = '{branch}'"
            ))
            .await
            .unwrap();

        let got = store
            .get_branch_state(branch)
            .await
            .expect("a NULL cutoff must not be a deserialize error")
            .expect("the row is still visible to the presence predicate");
        assert_eq!(got.applied_gc_cutoff_lsn, Lsn(0));
        assert_eq!(
            got.consistent_lsn, row.consistent_lsn,
            "the rest of the row still deserializes"
        );

        // And the next cutoff publish stamps the real value over the NULL —
        // `GREATEST(NULL, x)` is `x` in SQL, so the row self-heals.
        let published = store.publish_gc_cutoff(branch, Lsn(80)).await.unwrap();
        assert_eq!(published, Lsn(80));

        drop_schema(&store, &schema).await;
    }

    /// `advance_consistent_lsn` persists the pair and reports a missing row as
    /// an error rather than silently doing nothing. Fencing is the endpoint's
    /// write lease (compute/architecture/compute-fencing.md), so the only
    /// failure here is "the creation write never ran".
    #[tokio::test]
    async fn advance_consistent_lsn_persists_and_needs_a_row() {
        use utils::lsn::Lsn;

        let (store, schema) = make_store("dc_advance").await;
        let branch = rand_branch();
        let mut row = sample_branch_state_row(branch);
        row.consistent_lsn = Lsn(0x1000);
        create_branch_state_row(&store, &row).await.unwrap();

        store
            .advance_consistent_lsn(branch, Lsn(0x2000), Some(Lsn(0x1F00)))
            .await
            .unwrap();
        let got = store.get_branch_state(branch).await.unwrap().unwrap();
        assert_eq!(got.consistent_lsn, Lsn(0x2000));
        assert_eq!(got.prev_record_lsn, Some(Lsn(0x1F00)));

        // A NULL `prev` keeps whatever was there — the walreceiver re-derives
        // it, and overwriting a good value with NULL on every throttled flush
        // window is what the COALESCE exists to prevent.
        store
            .advance_consistent_lsn(branch, Lsn(0x3000), None)
            .await
            .unwrap();
        let got = store.get_branch_state(branch).await.unwrap().unwrap();
        assert_eq!(got.consistent_lsn, Lsn(0x3000));
        assert_eq!(got.prev_record_lsn, Some(Lsn(0x1F00)));

        // No row at all is a wiring bug, not a no-op.
        let err = store
            .advance_consistent_lsn(rand_branch(), Lsn(0x4000), None)
            .await
            .expect_err("advance against a nonexistent branch must fail");
        assert!(
            format!("{err:#}").contains("no branch_state row"),
            "{err:#}"
        );

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
        let mut anc_row = sample_branch_state_row(anc);
        anc_row.prev_record_lsn = Some(Lsn(85));
        create_branch_state_row(&store, &anc_row).await.unwrap();

        // Branch point above the durable head → rejected.
        let err = store
            .create_branch_row(rand_branch(), anc, Lsn(95))
            .await
            .unwrap_err();
        assert!(err.to_string().contains("not yet durable"), "{err}");

        // Branch point below the gc cutoff → rejected.
        let err = store
            .create_branch_row(rand_branch(), anc, Lsn(40))
            .await
            .unwrap_err();
        assert!(err.to_string().contains("gc cutoff"), "{err}");

        // Missing ancestor → rejected.
        let err = store
            .create_branch_row(rand_branch(), rand_branch(), Lsn(70))
            .await
            .unwrap_err();
        assert!(err.to_string().contains("no branch_state row"), "{err}");

        // Happy path mid-history: prev_record_lsn unknown → None.
        let child = rand_branch();
        store
            .provision_branch_row_for_test(child, anc_row.pg_version)
            .await
            .unwrap();
        let row = store.create_branch_row(child, anc, Lsn(70)).await.unwrap();
        assert_eq!(row.consistent_lsn, Lsn(70), "walreceiver resume point");
        assert_eq!(row.ancestor_branch_id, Some(anc));
        assert_eq!(row.ancestor_lsn, Some(Lsn(70)));
        assert_eq!(row.pg_version, anc_row.pg_version);
        assert_eq!(row.prev_record_lsn, None);
        let persisted = store.get_branch_state(child).await.unwrap().unwrap();
        assert_eq!(persisted, row);

        // Happy path at the durable head: prev_record_lsn inherited.
        let child2 = rand_branch();
        store
            .provision_branch_row_for_test(child2, anc_row.pg_version)
            .await
            .unwrap();
        let row = store.create_branch_row(child2, anc, Lsn(90)).await.unwrap();
        assert_eq!(row.prev_record_lsn, Some(Lsn(85)));

        // Branching below the ancestor's own branch point → rejected.
        let grandchild_bad = rand_branch();
        let err = store
            .create_branch_row(grandchild_bad, child, Lsn(60))
            .await
            .unwrap_err();
        assert!(err.to_string().contains("own branch point"), "{err}");

        drop_schema(&store, &schema).await;
    }

    // ------------------- direct-RPC (thin) lane tests -----------------------

    /// Attach the direct-RPC (`yb_thin_client`) lane to a clone of the
    /// shared store, connecting against dev YB's tserver RPC port
    /// (`YB_TEST_TSERVER_RPC`, default `127.0.0.1:9100`). Mirrors
    /// `ThinClientTables::connect`, which is bound to the config-driven
    /// startup path.
    async fn thin_store() -> PageStore {
        let (mut store, _) = make_store("thin").await;
        let (db_oid, oids) = ysql_client::lookup_table_oids(
            &store.ysql_client,
            PROD_SCHEMA,
            &["pages", "slru_pages", "meta", "pending_gc"],
        )
        .await
        .expect("lookup pageserver table OIDs");
        let tserver =
            std::env::var("YB_TEST_TSERVER_RPC").unwrap_or_else(|_| "127.0.0.1:9100".into());
        let client = yb_thin_client::TsClient::connect(&yb_thin_client::TsClientConfig {
            tserver_addrs: vec![tserver],
            // Same write-session fan-out the startup path derives from
            // scale_count(2, 1.0), so these tests cover pooled writes.
            write_sessions: 2,
            ..Default::default()
        })
        .expect("connect thin lane to dev YB; set YB_TEST_TSERVER_RPC to override");
        // The thin client's `open_table` is synchronous — the shim caches the
        // schema.
        let open = |t: &str| {
            client
                .open_table(db_oid, oids[t])
                .unwrap_or_else(|e| panic!("open {t} via thin client: {e}"))
        };
        // No lifetime bound: these tests want the connection they opened, for
        // as long as the test runs. `Recycling::fixed` never rebuilds.
        store.thinclient = Some(yb_thin_client::Recycling::fixed(Arc::new(
            ThinClientTables {
                pages: open("pages"),
                slru_pages: open("slru_pages"),
                meta: open("meta"),
                pending_gc: open("pending_gc"),
                client,
            },
        )));
        store
    }

    /// Sample count of each op served by the direct-RPC lane
    /// (`pageserver_yb_store_op_seconds{op, path="yb_thin_client"}`, observed
    /// only on the lane's success arm). Lets a test assert an op actually
    /// ran on the lane: a dispatch bug that routed to YSQL instead would
    /// leave these flat while the YSQL cross-checks still passed.
    fn thin_op_count(op: &str) -> u64 {
        crate::metrics::YB_STORE_OP_SECONDS
            .with_label_values(&[op, "yb_thin_client"])
            .get_sample_count()
    }

    /// Live direct-RPC round trip (`hot_path = "yb_thin_client"`): put
    /// pages/SLRU pages through the lane, then read chains, multi-block
    /// scans, and meta through it, cross-checking every result against
    /// the plain-YSQL store. A lane error surfaces as a test failure, and
    /// each op's lane sample count must advance so a silent YSQL routing
    /// can't pass unnoticed.
    #[tokio::test]
    async fn thin_lane_chain_scan_put_match_ysql() {
        let pg = thin_store().await;
        let (ysql, _) = make_store("thin_ysql").await;
        assert!(ysql.thinclient.is_none());
        let branch = rand_branch();
        let ops = [
            "put_pages",
            "put_slru_pages",
            "read_chain",
            "scan_range",
            "read_slru_chain",
            "scan_slru_range",
            "put_meta",
            "read_meta",
        ];
        let before: Vec<u64> = ops.iter().map(|op| thin_op_count(op)).collect();

        // Pages, written through the thin lane. Shapes cover the
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

        // Chain reads: thin lane result must equal the YSQL result,
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

        // Meta: written through the lane, read back on both. The second
        // row shares nothing but the key, so the `lsn DESC` clustering
        // has to place them for the targeted reads below to differ.
        let mk = meta_key(7);
        pg.put_meta(
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

        // Every op above ran on the thin lane, not silently on YSQL:
        // its lane sample count advanced.
        for (op, b) in ops.iter().zip(before) {
            assert!(
                thin_op_count(op) > b,
                "op {op} did not run on the thin lane"
            );
        }
    }

    /// The thin write path must chunk like the YSQL lane: a
    /// batch above `ROWS_PER_INSERT` lands fully via multiple Performs,
    /// and a paged chain read (> one 32-row page) walks the paging
    /// envelope on one snapshot.
    #[tokio::test]
    async fn thin_lane_chunks_and_pages() {
        let pg = thin_store().await;
        let (ysql, _) = make_store("thin_chunks_ysql").await;
        let branch = rand_branch();
        let (put_before, read_before) = (thin_op_count("put_pages"), thin_op_count("read_chain"));

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

        // Both put_pages and the chain reads ran on the thin lane.
        assert!(
            thin_op_count("put_pages") > put_before,
            "put_pages did not run on the thin lane"
        );
        assert!(
            thin_op_count("read_chain") > read_before,
            "read_chain did not run on the thin lane"
        );
    }

    /// GC candidates enqueued on the direct-RPC lane must be exactly what
    /// the YSQL drain then finds. This is the cross-lane contract that
    /// matters for `pending_gc`: the writer computes the placement
    /// `bucket_id` client-side (`gc_bucket_id`) and the drain fans out
    /// per bucket, so a lane that bucketed differently would leave
    /// candidates invisible — silently, since a missed candidate just
    /// looks like an empty queue. Covers both `relsize` states: `None`
    /// (trim, binds NULL) and `Some` (shrink), the nullable non-key
    /// column being the one thing the upsert encoding could drop.
    #[tokio::test]
    async fn thin_lane_gc_enqueue_matches_ysql_drain() {
        let pg = thin_store().await;
        let (ysql, _) = make_store("thin_gc_ysql").await;
        let branch = rand_branch();
        let before = thin_op_count("gc_enqueue_candidates");

        // Spread across keys so the batch spans several `gc_bucket_id`
        // buckets — a single-bucket batch would pass even if the lane
        // hard-coded one.
        let mut batch: Vec<PendingGcRow> = (0..24i64)
            .map(|i| gc_cand(16401 + i, 0, i, 100 + i as u64))
            .collect();
        // One shrink candidate, the only kind carrying a relsize.
        batch.push(PendingGcRow {
            relsize: Some(7),
            ..gc_cand(16999, 0, -1, 300)
        });
        pg.gc_enqueue_candidates(branch, &batch).await.unwrap();

        // The YSQL drain sees every row, relsize included.
        let mut found = all_pending(&ysql, branch).await;
        let mut want = batch.clone();
        let by_id = |r: &PendingGcRow| (r.due_lsn, key_to_bytes(&r.key), r.gc_kind.to_i16());
        found.sort_by_key(by_id);
        want.sort_by_key(by_id);
        assert_eq!(
            found, want,
            "thin-lane candidates as seen by the YSQL drain"
        );

        // Replay dedups on the lane too: same PK, upsert overwrites.
        pg.gc_enqueue_candidates(branch, &batch).await.unwrap();
        assert_eq!(
            all_pending(&ysql, branch).await.len(),
            batch.len(),
            "replayed thin-lane inserts must dedup on the PK"
        );

        assert!(
            thin_op_count("gc_enqueue_candidates") > before,
            "gc_enqueue_candidates did not run on the thin lane"
        );
    }

    /// `load_lineage` walks ancestor links with running-min caps.
    #[tokio::test]
    async fn load_lineage_walks_ancestor_links() {
        let (store, schema) = make_store("lin_load").await;
        let grandparent = rand_branch();
        let parent = rand_branch();
        let child = rand_branch();

        create_branch_state_row(&store, &sample_branch_state_row(grandparent))
            .await
            .unwrap();
        let mut parent_row = sample_branch_state_row(parent);
        parent_row.ancestor_branch_id = Some(grandparent);
        parent_row.ancestor_lsn = Some(Lsn(50));
        create_branch_state_row(&store, &parent_row).await.unwrap();
        let mut child_row = sample_branch_state_row(child);
        child_row.ancestor_branch_id = Some(parent);
        child_row.ancestor_lsn = Some(Lsn(70));
        create_branch_state_row(&store, &child_row).await.unwrap();

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
        assert!(err.to_string().contains("no branch_state row"), "{err}");

        drop_schema(&store, &schema).await;
    }

    // ===================== GC: pending_gc queue + trim ==================

    const GC_SPC: i64 = 1663;
    const GC_DB: i64 = 5;

    impl super::PageStore {
        /// Test-only: every stored version of one block, unfiltered —
        /// `(lsn, is_image, will_init)` ascending. The production read
        /// paths anchor-trim their results, which hides exactly the rows
        /// GC tests must assert about (deleted sub-anchor versions).
        async fn test_raw_block_versions(
            &self,
            branch_id: BranchId,
            spcnode: i64,
            dbnode: i64,
            relnode: i64,
            forknum: i16,
            blknum: i64,
        ) -> Result<Vec<(u64, bool, bool)>> {
            let sql = format!(
                "SELECT lsn, is_image, will_init FROM \"{}\".pages \
                 WHERE branch_id=$1 AND spcnode=$2 AND dbnode=$3 \
                   AND relnode=$4 AND forknum=$5 AND blknum=$6 \
                 ORDER BY lsn, is_image",
                self.schema
            );
            let rows = self
                .ysql_client
                .query(
                    &sql,
                    &[&branch_id, &spcnode, &dbnode, &relnode, &forknum, &blknum],
                )
                .await
                .context("test_raw_block_versions")?;
            Ok(rows
                .iter()
                .map(|r| (r.get::<_, i64>(0) as u64, r.get(1), r.get(2)))
                .collect())
        }

        /// SLRU analog of [`Self::test_raw_block_versions`]: one block's
        /// stored versions for `(kind, segno, blknum)`, ordered by lsn.
        async fn test_raw_slru_versions(
            &self,
            branch_id: BranchId,
            kind: i16,
            segno: i64,
            blknum: i64,
        ) -> Result<Vec<(u64, bool, bool)>> {
            let sql = format!(
                "SELECT lsn, is_image, will_init FROM \"{}\".slru_pages \
                 WHERE branch_id=$1 AND kind=$2 AND segno=$3 AND blknum=$4 \
                 ORDER BY lsn, is_image",
                self.schema
            );
            let rows = self
                .ysql_client
                .query(&sql, &[&branch_id, &kind, &segno, &blknum])
                .await
                .context("test_raw_slru_versions")?;
            Ok(rows
                .iter()
                .map(|r| (r.get::<_, i64>(0) as u64, r.get(1), r.get(2)))
                .collect())
        }
    }

    fn gc_rel(relnode: i64, forknum: i16) -> RelTag {
        RelTag {
            spcnode: GC_SPC as u32,
            dbnode: GC_DB as u32,
            relnode: relnode as u32,
            forknum: forknum as u8,
        }
    }

    /// Build a candidate. The `sel` selector keeps the old blknum-sentinel
    /// convention for these tests: `sel >= 0` → a trim candidate for that
    /// block; `sel == -2` → a drop; `sel == -1` → a shrink (both keyed by
    /// the fork's rel-size object key).
    fn gc_cand(relnode: i64, forknum: i16, sel: i64, due: u64) -> PendingGcRow {
        let (key, gc_kind) = if sel >= 0 {
            (
                rel_block_to_key(gc_rel(relnode, forknum), sel as u32),
                GcKind::TrimPages,
            )
        } else if sel == -2 {
            (rel_size_to_key(gc_rel(relnode, forknum)), GcKind::DropPages)
        } else {
            (
                rel_size_to_key(gc_rel(relnode, forknum)),
                GcKind::ShrinkPages,
            )
        };
        PendingGcRow {
            key,
            gc_kind,
            due_lsn: Lsn(due),
            relsize: None,
        }
    }

    /// The trim candidates' keys in a selected batch — what the drain
    /// feeds [`PageStore::gc_dequeue_trim_candidates`].
    fn blocks_of(rows: &[PendingGcRow]) -> Vec<(Key, GcKind)> {
        rows.iter()
            .filter(|r| is_trim_kind(r.gc_kind))
            .map(|r| (r.key, r.gc_kind))
            .collect()
    }

    /// The drop/shrink candidates' exact PKs in a selected batch — what the
    /// drain feeds [`PageStore::gc_dequeue_drop_shrink_candidates`].
    fn drop_shrink_ids(rows: &[PendingGcRow]) -> Vec<(Key, GcKind, Lsn)> {
        rows.iter()
            .filter(|r| !is_trim_kind(r.gc_kind))
            .map(|r| (r.key, r.gc_kind, r.due_lsn))
            .collect()
    }

    fn is_trim_kind(k: GcKind) -> bool {
        matches!(k, GcKind::TrimPages | GcKind::TrimSlru | GcKind::TrimMeta)
    }

    /// A candidate's block number (trim) or a signed kind marker
    /// (drop = -2, shrink = -1) — reconstructs the old blknum-sentinel
    /// view these DB tests assert on.
    fn blk_marker(r: &PendingGcRow) -> i64 {
        match r.gc_kind {
            GcKind::TrimPages | GcKind::TrimSlru | GcKind::TrimMeta => r.key.field6 as i64,
            GcKind::DropPages | GcKind::DropSlru | GcKind::DropMeta => -2,
            _ => -1,
        }
    }

    /// `gc_bucket_id` is the client-side placement hash that replaced the
    /// YB `GENERATED ALWAYS AS (yb_hash_code(...) % N)` column. It must
    /// stay **frozen** — a change would strand a block's older candidates
    /// in their old bucket and defeat crash-replay dedup — so these are
    /// `GcKind` <-> `i16` round-trips for every value (known → variant,
    /// out-of-range → `Unknown`, preserving the raw wire value). No DB.
    #[test]
    fn gc_kind_i16_roundtrip() {
        for v in [0i16, 1, 2, 3, 4, 5, 6, 7, -1, 42, i16::MAX, i16::MIN] {
            assert_eq!(GcKind::from_i16(v).to_i16(), v, "roundtrip for {v}");
        }
        assert_eq!(GcKind::from_i16(0), GcKind::TrimPages);
        assert_eq!(GcKind::from_i16(6), GcKind::DropMeta);
        assert_eq!(GcKind::from_i16(7), GcKind::Unknown(7));
        assert_eq!(GcKind::TrimPages.to_i16(), 0);
        assert_eq!(GcKind::DropMeta.to_i16(), 6);
    }

    /// golden values, not just property checks. No DB needed (pure fn).
    #[test]
    fn gc_bucket_id_is_frozen_and_bounded() {
        let blk = |spc: u32, db: u32, rel: u32, blk: u32| {
            rel_block_to_key(
                RelTag {
                    spcnode: spc,
                    dbnode: db,
                    relnode: rel,
                    forknum: 0,
                },
                blk,
            )
        };
        // The digest is the frozen half; the buckets below also move with
        // GC_BUCKETS, and at modulus 3 they cannot tell the two apart.
        assert_eq!(gc_key_hash(&blk(1663, 5, 16400, 1)), 0xcd46_8f83_9ce4_ca9e);

        // Golden values (change ⇒ the hash or GC_BUCKETS moved — intended?).
        assert_eq!(gc_bucket_id(&blk(1663, 5, 16400, 1)), 0);
        assert_eq!(gc_bucket_id(&blk(1663, 5, 16400, 2)), 2);
        assert_eq!(gc_bucket_id(&blk(1663, 5, 16401, 0)), 0);
        assert_eq!(gc_bucket_id(&blk(1663, 5, 16400, 999)), 0);
        assert_eq!(gc_bucket_id(&blk(0, 0, 0, 0)), 1);

        // Deterministic + in range over a spread of blocks.
        let mut counts = [0usize; GC_BUCKETS as usize];
        for g in 0..10_000u32 {
            let key = blk(1663, 5, 16400 + (g % 50), g);
            let b = gc_bucket_id(&key);
            assert_eq!(b, gc_bucket_id(&key), "stable");
            assert!((0..GC_BUCKETS).contains(&b), "in range");
            counts[b as usize] += 1;
        }
        // No bucket is starved or dominant (even spread ⇒ no tablet hotspot).
        assert!(
            counts.iter().all(|&c| (3000..3700).contains(&c)),
            "uneven bucket distribution: {counts:?}"
        );
    }

    /// Everything queued for `branch`, regardless of due-ness.
    /// (Not `Lsn::MAX` — the cutoff crosses through i64 in SQL.)
    async fn all_pending(store: &PageStore, branch: BranchId) -> Vec<PendingGcRow> {
        store
            .gc_select_due_candidates(branch, Lsn(0), Lsn(i64::MAX as u64), 1_000_000)
            .await
            .unwrap()
            .rows
    }

    /// Cross-check the SQL `gc_key_decode` inspection helper against the
    /// Rust Key codec: for each keyspace build a key with its Rust
    /// constructor, run the decoder on the encoded bytes, and assert the
    /// formatted output — plus the not-18-bytes fallback. Guards the plpgsql
    /// decoder and the Rust codec from drifting apart (the migration comment
    /// on `gc_key_decode` points here).
    #[tokio::test]
    async fn gc_key_decode_matches_key_codec() {
        use pageserver_api::key::{
            rel_block_to_key, rel_size_to_key, slru_block_to_key, slru_segment_size_to_key,
            twophase_file_key,
        };
        use pageserver_api::reltag::{RelTag, SlruKind};

        let (store, schema) = make_store("gc_key_decode").await;
        let rel = RelTag {
            spcnode: 1663,
            dbnode: 5,
            relnode: 16400,
            forknum: 0,
        };
        let cases: Vec<(Vec<u8>, &str)> = vec![
            (
                key_to_bytes(&rel_block_to_key(rel, 7)),
                "rel_block spc=1663 db=5 rel=16400 fork=0 blk=7",
            ),
            (
                key_to_bytes(&rel_size_to_key(rel)),
                "rel_size spc=1663 db=5 rel=16400 fork=0",
            ),
            (
                key_to_bytes(&slru_block_to_key(SlruKind::Clog, 9, 3)),
                "slru_block kind=0 seg=9 blk=3",
            ),
            (
                key_to_bytes(&slru_block_to_key(SlruKind::MultiXactMembers, 2, 5)),
                "slru_block kind=1 seg=2 blk=5",
            ),
            (
                key_to_bytes(&slru_segment_size_to_key(SlruKind::Clog, 9)),
                "slru_seg_size kind=0 seg=9",
            ),
            (
                key_to_bytes(&twophase_file_key(753)),
                "meta f1=2 f2=0 f3=0 f4=0 f5=0 f6=753",
            ),
        ];
        let sql = format!("SELECT \"{schema}\".gc_key_decode($1)");
        for (bytes, expected) in cases {
            let rows = store.ysql_client.query(&sql, &[&bytes]).await.unwrap();
            let got: String = rows[0].get(0);
            assert_eq!(got, expected, "decode of {bytes:?}");
        }
        // A bytea that isn't a full 18-byte Key decodes to the raw fallback,
        // never panics.
        let short = vec![1u8, 2, 3];
        let rows = store.ysql_client.query(&sql, &[&short]).await.unwrap();
        assert_eq!(rows[0].get::<_, String>(0), "raw(3 bytes)");

        drop_schema(&store, &schema).await;
    }

    /// Scenario: the same candidates are inserted twice — as crash +
    /// WAL replay does — and must dedup to one row each: the candidate's
    /// identity is its PK and the writers run upsert-mode, so a replay
    /// overwrites in place (the lone non-PK column `relsize` is fixed by
    /// that identity). (The placement `bucket_id` is part of that PK; its
    /// stability and range are covered by `gc_bucket_id_is_frozen_and_bounded`.)
    #[tokio::test]
    async fn pending_gc_replay_dedups_on_pk() {
        let (store, _schema) = make_store("gc_dedup").await;
        let branch = rand_branch();

        let batch = vec![
            gc_cand(16401, 0, 42, 100),
            gc_cand(16401, 0, 43, 100),
            gc_cand(16402, 1, 0, 200),
        ];
        store.gc_enqueue_candidates(branch, &batch).await.unwrap();
        // Replay: byte-identical re-insert.
        store.gc_enqueue_candidates(branch, &batch).await.unwrap();

        let rows = all_pending(&store, branch).await;
        assert_eq!(rows.len(), 3, "replayed inserts must dedup: {rows:?}");
    }

    /// Scenario: candidates with due_lsns 1..=20; the drain must see
    /// exactly the due ones (due_lsn ≤ cutoff — the "collectable once
    /// the cutoff reaches it" gate), oldest-first, and a limit must
    /// keep the OLDEST candidates (nothing starves: per-bucket LIMIT
    /// then a merge-sort truncate can only drop the newest).
    #[tokio::test]
    async fn pending_gc_select_gates_orders_and_limits() {
        let (store, _schema) = make_store("gc_select").await;
        let branch = rand_branch();

        let batch: Vec<PendingGcRow> = (1..=20u64)
            .map(|i| gc_cand(16401, 0, i as i64, i))
            .collect();
        store.gc_enqueue_candidates(branch, &batch).await.unwrap();

        // Gate: only due_lsn ≤ 10.
        let due = store
            .gc_select_due_candidates(branch, Lsn(0), Lsn(10), 1000)
            .await
            .unwrap()
            .rows;
        let dues: Vec<u64> = due.iter().map(|r| r.due_lsn.0).collect();
        assert_eq!(dues, (1..=10).collect::<Vec<_>>(), "gating/order wrong");

        // Limit keeps the oldest.
        let due = store
            .gc_select_due_candidates(branch, Lsn(0), Lsn(20), 5)
            .await
            .unwrap()
            .rows;
        let dues: Vec<u64> = due.iter().map(|r| r.due_lsn.0).collect();
        assert_eq!(dues, vec![1, 2, 3, 4, 5], "limit must keep oldest");
    }

    /// Scenario: the drain resumes from its progress floor
    /// (`gc_completed_lsn`). The floor is exclusive (`due_lsn > floor`)
    /// because it is set to `last drained due_lsn − 1`: a limit can
    /// split a group of equal due_lsns mid-group, and the −1 keeps the
    /// boundary group selectable on the next tick.
    #[tokio::test]
    async fn pending_gc_select_floor_gates_and_keeps_boundary_ties() {
        let (store, _schema) = make_store("gc_floor").await;
        let branch = rand_branch();

        // Three blocks tied at due 10 (one per distinct blknum), plus
        // singles at 5 and 15.
        let batch = vec![
            gc_cand(16401, 0, 1, 5),
            gc_cand(16401, 0, 2, 10),
            gc_cand(16401, 0, 3, 10),
            gc_cand(16401, 0, 4, 10),
            gc_cand(16401, 0, 5, 15),
        ];
        store.gc_enqueue_candidates(branch, &batch).await.unwrap();

        // A capped tick drains {5, 10} — splitting the tie group at 10.
        let due = store
            .gc_select_due_candidates(branch, Lsn(0), Lsn(20), 2)
            .await
            .unwrap()
            .rows;
        assert_eq!(due.len(), 2);
        assert_eq!(due.last().unwrap().due_lsn, Lsn(10));
        store
            .gc_dequeue_trim_candidates(branch, &blocks_of(&due), Lsn(0), Lsn(20))
            .await
            .unwrap();

        // floor = last due − 1 = 9: the tie group's survivors at 10 must
        // still be selectable (an exclusive floor at 10 would skip them).
        let due = store
            .gc_select_due_candidates(branch, Lsn(9), Lsn(20), 1000)
            .await
            .unwrap()
            .rows;
        let dues: Vec<u64> = due.iter().map(|r| r.due_lsn.0).collect();
        assert_eq!(dues, vec![10, 10, 15], "boundary ties must re-select");

        // The floor gate itself is exclusive: floor = 10 skips the ties.
        let due = store
            .gc_select_due_candidates(branch, Lsn(10), Lsn(20), 1000)
            .await
            .unwrap()
            .rows;
        let dues: Vec<u64> = due.iter().map(|r| r.due_lsn.0).collect();
        assert_eq!(dues, vec![15], "floor is exclusive");
    }

    /// Scenario: the dequeue removes the selected due rows and ONLY
    /// them — not-yet-due candidates stay queued for their own tick.
    #[tokio::test]
    async fn pending_gc_dequeue_removes_exactly_drained() {
        let (store, _schema) = make_store("gc_dequeue").await;
        let branch = rand_branch();

        let batch: Vec<PendingGcRow> = (1..=6u64)
            .map(|i| gc_cand(16401, 0, i as i64, i * 10))
            .collect();
        store.gc_enqueue_candidates(branch, &batch).await.unwrap();

        let drained = store
            .gc_select_due_candidates(branch, Lsn(0), Lsn(30), 1000)
            .await
            .unwrap()
            .rows;
        assert_eq!(drained.len(), 3);
        store
            .gc_dequeue_trim_candidates(branch, &blocks_of(&drained), Lsn(0), Lsn(30))
            .await
            .unwrap();

        let left: Vec<u64> = all_pending(&store, branch)
            .await
            .iter()
            .map(|r| r.due_lsn.0)
            .collect();
        assert_eq!(left, vec![40, 50, 60], "exactly the due rows must go");
    }

    /// Scenario: the block-range dequeue sweeps ALL of a visited block's
    /// candidates at/under the cutoff — duplicates and stragglers, not
    /// just named PKs — while leaving that block's above-cutoff
    /// candidate, other blocks, and a same-fork drop sentinel (keyed on
    /// exact block identity, so blknum -2 is never matched).
    #[tokio::test]
    async fn pending_gc_dequeue_sweeps_the_visited_block() {
        let (store, _schema) = make_store("gc_trim_sweep").await;
        let branch = rand_branch();

        let batch = vec![
            gc_cand(16401, 0, 7, 10),
            gc_cand(16401, 0, 7, 20),
            gc_cand(16401, 0, 7, 30),
            gc_cand(16401, 0, 7, 200), // above cutoff
            gc_cand(16401, 0, 8, 25),  // other block
            gc_cand(16401, 0, -2, 40), // drop sentinel, same fork
        ];
        store.gc_enqueue_candidates(branch, &batch).await.unwrap();

        let block_a: Key = rel_block_to_key(gc_rel(16401, 0), 7);
        store
            .gc_dequeue_trim_candidates(branch, &[(block_a, GcKind::TrimPages)], Lsn(0), Lsn(100))
            .await
            .unwrap();

        let mut left: Vec<(i64, u64)> = all_pending(&store, branch)
            .await
            .iter()
            .map(|r| (blk_marker(r), r.due_lsn.0))
            .collect();
        left.sort();
        assert_eq!(
            left,
            vec![(-2, 40), (7, 200), (8, 25)],
            "block 7's <= cutoff candidates swept; its above-cutoff one, block 8, and the drop survive"
        );
    }

    /// Scenario: the dequeue window is `(floor, cutoff]`, matching the
    /// select — a visited block's below-floor candidate is left untouched
    /// (already handled under an earlier floor), so the delete never
    /// reaches rows outside the tick's own span.
    #[tokio::test]
    async fn pending_gc_dequeue_respects_floor() {
        let (store, _schema) = make_store("gc_floor").await;
        let branch = rand_branch();

        store
            .gc_enqueue_candidates(
                branch,
                &[
                    gc_cand(16401, 0, 7, 10), // below floor — survives
                    gc_cand(16401, 0, 7, 40), // in (floor, cutoff] — swept
                    gc_cand(16401, 0, 7, 60), // in (floor, cutoff] — swept
                ],
            )
            .await
            .unwrap();

        let block_a: Key = rel_block_to_key(gc_rel(16401, 0), 7);
        store
            .gc_dequeue_trim_candidates(branch, &[(block_a, GcKind::TrimPages)], Lsn(20), Lsn(100))
            .await
            .unwrap();

        let left: Vec<u64> = all_pending(&store, branch)
            .await
            .iter()
            .map(|r| r.due_lsn.0)
            .collect();
        assert_eq!(
            left,
            vec![10],
            "the below-floor candidate survives; (floor, cutoff] swept"
        );
    }

    /// Scenario: a budget-capped drain visits a block through ONE of
    /// its queued candidates, but the range dequeue collapses the whole
    /// block: acting the block trimmed it to the cutoff, so every
    /// candidate `<= cutoff` there is moot and swept in one delete — no
    /// straggler duplicates to re-select on later ticks. The block's
    /// above-cutoff candidate and an unvisited block's due candidate
    /// survive.
    #[tokio::test]
    async fn pending_gc_dequeue_collapses_visited_block_duplicates() {
        let (store, _schema) = make_store("gc_collapse").await;
        let branch = rand_branch();

        store
            .gc_enqueue_candidates(
                branch,
                &[
                    gc_cand(16401, 0, 7, 5),  // visited (selected)
                    gc_cand(16401, 0, 7, 15), // duplicate, due — swept too
                    gc_cand(16401, 0, 7, 25), // duplicate, due — swept too
                    gc_cand(16401, 0, 7, 45), // above cutoff — survives
                    gc_cand(16401, 0, 8, 12), // other block, due, NOT visited
                ],
            )
            .await
            .unwrap();

        // limit 1: the tick only selects (5, blk 7) — capped.
        let drained = store
            .gc_select_due_candidates(branch, Lsn(0), Lsn(30), 1)
            .await
            .unwrap()
            .rows;
        assert_eq!(drained.len(), 1);
        assert_eq!((blk_marker(&drained[0]), drained[0].due_lsn.0), (7, 5));
        store
            .gc_dequeue_trim_candidates(branch, &blocks_of(&drained), Lsn(0), Lsn(30))
            .await
            .unwrap();

        let left: Vec<(i64, u64)> = all_pending(&store, branch)
            .await
            .iter()
            .map(|r| (blk_marker(r), r.due_lsn.0))
            .collect();
        assert_eq!(
            left,
            vec![(8, 12), (7, 45)],
            "block 7's <= cutoff duplicates collapse; its above-cutoff one and block 8 survive"
        );
        // Only the unvisited block re-selects; block 7 is fully drained.
        let next = store
            .gc_select_due_candidates(branch, Lsn(0), Lsn(30), 1000)
            .await
            .unwrap()
            .rows;
        assert_eq!(next.len(), 1, "only the unvisited block remains due");
    }

    /// Scenario: a tie group wider than the select cap — a DROP
    /// DATABASE fan-out queues many drop events at ONE due_lsn (one
    /// per fork). Each capped tick selects a batch, acts, and dequeues
    /// exactly those forks' drop sentinels; the remainder re-selects
    /// until the group drains. No tick may wedge (select the same rows
    /// forever) or over-delete (remove a fork it didn't act — each fork
    /// is a distinct block, so the cap still bounds the fan-out).
    #[tokio::test]
    async fn pending_gc_dequeue_tie_group_drains_across_ticks() {
        let (store, _schema) = make_store("gc_tie_group").await;
        let branch = rand_branch();

        let fanout: Vec<PendingGcRow> = (0..5)
            .map(|i| gc_cand(16401 + i, 0, -2, 10)) // one due_lsn
            .collect();
        store.gc_enqueue_candidates(branch, &fanout).await.unwrap();

        let mut ticks_used = 0;
        for _tick in 0..5 {
            let batch = store
                .gc_select_due_candidates(branch, Lsn(0), Lsn(20), 2)
                .await
                .unwrap()
                .rows;
            if batch.is_empty() {
                break;
            }
            assert!(batch.len() <= 2, "select respects the cap");
            ticks_used += 1;
            store
                .gc_dequeue_drop_shrink_candidates(branch, &drop_shrink_ids(&batch))
                .await
                .unwrap();
        }
        // 5 rows at cap 2 need >= 3 ticks — a tick can't over-drain.
        assert!(ticks_used >= 3, "drained in {ticks_used} ticks");
        assert!(
            all_pending(&store, branch).await.is_empty(),
            "no stragglers"
        );
    }

    /// Scenario: drop/shrink dequeue by exact `(ident, due_lsn)`; trim by
    /// range-sweep. A fork has an acted drop (due 10) and shrink (due 20),
    /// plus a SECOND drop for the same fork at due 300 that is due
    /// (`<= cutoff`) but NOT acted this tick (a drain-limit split). Exact-PK
    /// dequeue clears only the two acted candidates and leaves drop@300
    /// queued for its own tick — the old range-sweep would have clobbered
    /// it (same fork ident, inside the window) and leaked its rows. The
    /// trim row (block 7) dequeues via the range-sweep.
    #[tokio::test]
    async fn pending_gc_dequeue_drop_shrink_exact_vs_trim_sweep() {
        let (store, _schema) = make_store("gc_dequeue_exact").await;
        let branch = rand_branch();

        store
            .gc_enqueue_candidates(
                branch,
                &[
                    gc_cand(16401, 0, -2, 10),  // acted
                    gc_cand(16401, 0, -1, 20),  // acted
                    gc_cand(16401, 0, -2, 300), // due, NOT acted — must survive
                    gc_cand(16401, 0, 7, 15),   // trim, acted (range-swept)
                ],
            )
            .await
            .unwrap();

        // Drop/shrink: dequeue only the two acted candidates, by exact PK.
        store
            .gc_dequeue_drop_shrink_candidates(
                branch,
                &[
                    (
                        rel_size_to_key(gc_rel(16401, 0)),
                        GcKind::DropPages,
                        Lsn(10),
                    ),
                    (
                        rel_size_to_key(gc_rel(16401, 0)),
                        GcKind::ShrinkPages,
                        Lsn(20),
                    ),
                ],
            )
            .await
            .unwrap();
        // Trim: range-sweep block 7's candidates in (0, 100].
        store
            .gc_dequeue_trim_candidates(
                branch,
                &[(rel_block_to_key(gc_rel(16401, 0), 7), GcKind::TrimPages)],
                Lsn(0),
                Lsn(100),
            )
            .await
            .unwrap();

        let left: Vec<(i64, u64)> = all_pending(&store, branch)
            .await
            .iter()
            .map(|r| (blk_marker(r), r.due_lsn.0))
            .collect();
        assert_eq!(
            left,
            vec![(-2, 300)],
            "the un-acted same-fork drop@300 survives exact-PK dequeue"
        );
    }

    /// Scenario: the trim's keep-rule on a block whose history has all
    /// the interesting shapes at once (cutoff = 100):
    ///
    ///   lsn 10  init delta   (base)      → deleted (below anchor)
    ///   lsn 15  plain delta              → deleted
    ///   lsn 40  image        (base)      → deleted (superseded base)
    ///   lsn 55  plain delta              → deleted
    ///   lsn 70  image        (base)      → KEPT — the anchor
    ///   lsn 70  plain delta  (twin)      → deleted (same-LSN non-base:
    ///                                      the materialiser stamps its
    ///                                      image at the folded delta's
    ///                                      LSN, shadowing it)
    ///   lsn 90  plain delta              → kept (above anchor)
    ///   lsn 120 plain delta              → kept (above cutoff)
    ///
    /// Plus: a block with NO base ≤ cutoff is untouched (no anchor —
    /// nothing below it is provably unreachable), and an unlisted
    /// block is untouched (the trim only visits what it's told to).
    #[tokio::test]
    async fn gc_trim_anchor_keep_delete_semantics() {
        let (store, _schema) = make_store("gc_trim").await;
        let branch = rand_branch();
        let rel = 16410u32;
        let (blk_a, blk_b, blk_c) = (1u32, 2u32, 3u32);

        let rows = vec![
            // block A: the full scenario
            page_row(
                page_key(GC_SPC as u32, GC_DB as u32, rel, 0, blk_a),
                10,
                false,
                true,
                b"d10i",
            ),
            page_row(
                page_key(GC_SPC as u32, GC_DB as u32, rel, 0, blk_a),
                15,
                false,
                false,
                b"d15",
            ),
            page_row(
                page_key(GC_SPC as u32, GC_DB as u32, rel, 0, blk_a),
                40,
                true,
                true,
                b"i40",
            ),
            page_row(
                page_key(GC_SPC as u32, GC_DB as u32, rel, 0, blk_a),
                55,
                false,
                false,
                b"d55",
            ),
            page_row(
                page_key(GC_SPC as u32, GC_DB as u32, rel, 0, blk_a),
                70,
                true,
                true,
                b"i70",
            ),
            page_row(
                page_key(GC_SPC as u32, GC_DB as u32, rel, 0, blk_a),
                70,
                false,
                false,
                b"d70",
            ),
            page_row(
                page_key(GC_SPC as u32, GC_DB as u32, rel, 0, blk_a),
                90,
                false,
                false,
                b"d90",
            ),
            page_row(
                page_key(GC_SPC as u32, GC_DB as u32, rel, 0, blk_a),
                120,
                false,
                false,
                b"d120",
            ),
            // block B: no base at all — must be untouched
            page_row(
                page_key(GC_SPC as u32, GC_DB as u32, rel, 0, blk_b),
                20,
                false,
                false,
                b"d20",
            ),
            page_row(
                page_key(GC_SPC as u32, GC_DB as u32, rel, 0, blk_b),
                30,
                false,
                false,
                b"d30",
            ),
            // block C: has garbage but is NOT in the visit list
            page_row(
                page_key(GC_SPC as u32, GC_DB as u32, rel, 0, blk_c),
                10,
                true,
                true,
                b"i10",
            ),
            page_row(
                page_key(GC_SPC as u32, GC_DB as u32, rel, 0, blk_c),
                50,
                true,
                true,
                b"i50",
            ),
        ];
        store.put_pages(branch, &rows).await.unwrap();

        store
            .gc_act_trim_blocks(
                branch,
                (GC_SPC, GC_DB, rel as i64, 0),
                &[blk_a as i64, blk_b as i64],
                Lsn(100),
            )
            .await
            .unwrap();

        let a = store
            .test_raw_block_versions(branch, GC_SPC, GC_DB, rel as i64, 0, blk_a as i64)
            .await
            .unwrap();
        assert_eq!(
            a,
            vec![(70, true, true), (90, false, false), (120, false, false)],
            "block A must keep anchor + everything above"
        );
        let b = store
            .test_raw_block_versions(branch, GC_SPC, GC_DB, rel as i64, 0, blk_b as i64)
            .await
            .unwrap();
        assert_eq!(b.len(), 2, "no-anchor block must be untouched");
        let c = store
            .test_raw_block_versions(branch, GC_SPC, GC_DB, rel as i64, 0, blk_c as i64)
            .await
            .unwrap();
        assert_eq!(c.len(), 2, "unlisted block must be untouched");
    }

    /// Scenario: greed. The trim boundary is the CUTOFF's anchor, not
    /// any candidate's due_lsn — one visit collects garbage from ALL
    /// bases ≤ cutoff, and a `will_init` DELTA (not only an image)
    /// anchors: an all-COPY chain has no image at all.
    #[tokio::test]
    async fn gc_trim_is_greedy_and_init_delta_anchors() {
        let (store, _schema) = make_store("gc_greedy").await;
        let branch = rand_branch();
        let rel = 16420u32;
        let blk = 7u32;

        let rows = vec![
            page_row(
                page_key(GC_SPC as u32, GC_DB as u32, rel, 0, blk),
                30,
                true,
                true,
                b"i30",
            ),
            page_row(
                page_key(GC_SPC as u32, GC_DB as u32, rel, 0, blk),
                40,
                false,
                false,
                b"d40",
            ),
            page_row(
                page_key(GC_SPC as u32, GC_DB as u32, rel, 0, blk),
                55,
                false,
                false,
                b"d55",
            ),
            // newest base ≤ cutoff is an init DELTA, not an image
            page_row(
                page_key(GC_SPC as u32, GC_DB as u32, rel, 0, blk),
                70,
                false,
                true,
                b"d70i",
            ),
        ];
        store.put_pages(branch, &rows).await.unwrap();

        // A drain triggered by the (oldest) candidate at due=30 still
        // trims to the cutoff's anchor at 70.
        store
            .gc_act_trim_blocks(
                branch,
                (GC_SPC, GC_DB, rel as i64, 0),
                &[blk as i64],
                Lsn(100),
            )
            .await
            .unwrap();

        let left = store
            .test_raw_block_versions(branch, GC_SPC, GC_DB, rel as i64, 0, blk as i64)
            .await
            .unwrap();
        assert_eq!(left, vec![(70, false, true)], "init delta is the anchor");
    }

    /// Rel-size meta row (LE u32 payload) for the drop/shrink tests.
    fn rel_size_row(relnode: u32, forknum: u8, lsn: u64, nblocks: u32) -> MetaRow {
        MetaRow {
            key: Bytes::from(rel_size_key_bytes(
                GC_SPC,
                GC_DB,
                relnode as i64,
                forknum as i16,
            )),
            lsn: Lsn(lsn),
            payload: Bytes::copy_from_slice(&nblocks.to_le_bytes()),
        }
    }

    /// Scenario: drop reclaims the whole fork `<= due_lsn` — pages AND
    /// the rel-size meta key — while (a) the same relnode's rows ABOVE
    /// the drop LSN (relfilenode reused after the drop) and (b) an
    /// unrelated rel survive. Idempotent: a re-run deletes nothing.
    #[tokio::test]
    async fn gc_act_drop_fork_scoped_to_due_lsn() {
        let (store, _schema) = make_store("gc_drop").await;
        let branch = rand_branch();
        let (rel, other) = (16420u32, 16421u32);

        let key = |r: u32, blk: u32| page_key(GC_SPC as u32, GC_DB as u32, r, 0, blk);
        store
            .put_pages(
                branch,
                &[
                    // dropped incarnation: everything <= due (100) must go
                    page_row(key(rel, 0), 10, true, true, b"i10"),
                    page_row(key(rel, 0), 30, false, false, b"d30"),
                    page_row(key(rel, 1), 20, true, true, b"i20"),
                    // relnode reused above the drop: must survive
                    page_row(key(rel, 0), 200, true, true, b"i200"),
                    // unrelated rel: must survive
                    page_row(key(other, 0), 10, true, true, b"i10"),
                ],
            )
            .await
            .unwrap();
        store
            .put_meta(
                branch,
                &[
                    rel_size_row(rel, 0, 10, 1),
                    rel_size_row(rel, 0, 20, 2),
                    // reused incarnation's size: must survive
                    rel_size_row(rel, 0, 200, 1),
                    rel_size_row(other, 0, 10, 1),
                ],
            )
            .await
            .unwrap();

        store
            .gc_act_drop_fork(branch, (GC_SPC, GC_DB, rel as i64, 0), Lsn(100))
            .await
            .unwrap();

        let b0 = store
            .test_raw_block_versions(branch, GC_SPC, GC_DB, rel as i64, 0, 0)
            .await
            .unwrap();
        assert_eq!(b0, vec![(200, true, true)], "reused relnode's row survives");
        let b1 = store
            .test_raw_block_versions(branch, GC_SPC, GC_DB, rel as i64, 0, 1)
            .await
            .unwrap();
        assert!(b1.is_empty(), "dropped incarnation fully reclaimed");
        let o = store
            .test_raw_block_versions(branch, GC_SPC, GC_DB, other as i64, 0, 0)
            .await
            .unwrap();
        assert_eq!(o.len(), 1, "unrelated rel untouched");

        // Rel-size meta: versions <= due gone, the reuse's kept.
        let size_key = rel_size_key_bytes(GC_SPC, GC_DB, rel as i64, 0);
        let below = store.read_meta(branch, &size_key, Lsn(199)).await.unwrap();
        assert!(below.is_none(), "dropped incarnation's size rows gone");
        let above = store.read_meta(branch, &size_key, Lsn(1000)).await.unwrap();
        assert_eq!(above.unwrap().lsn, Lsn(200), "reused size row survives");
        let other_key = rel_size_key_bytes(GC_SPC, GC_DB, other as i64, 0);
        assert!(
            store
                .read_meta(branch, &other_key, Lsn(1000))
                .await
                .unwrap()
                .is_some(),
            "unrelated rel's size untouched"
        );

        // Idempotent.
        let again = store
            .gc_act_drop_fork(branch, (GC_SPC, GC_DB, rel as i64, 0), Lsn(100))
            .await
            .unwrap();
        assert_eq!(again, 0);
    }

    /// Scenario: an SLRU segment drop deletes the segment's `slru_pages`
    /// rows and its seg-size sentinel `meta` key at `lsn <= due_lsn`; a
    /// wraparound-reused segno written ABOVE `due_lsn` survives, and a
    /// different segment is untouched. Idempotent.
    #[tokio::test]
    async fn gc_act_drop_slru_scoped_to_due_lsn() {
        use pageserver_api::key::slru_segment_size_to_key;
        let (store, _schema) = make_store("gc_drop_slru").await;
        let branch = rand_branch();
        let (seg, other) = (5u32, 6u32);
        let clog = 0i16; // SlruKind::Clog

        let seg_size_key = |s: u32| {
            let mut b = vec![0u8; KEY_SIZE];
            slru_segment_size_to_key(SlruKind::Clog, s).write_to_byte_slice(&mut b);
            b
        };
        let slru = |s: u32, blk: u32, lsn: u64, is_image: bool, will_init: bool| SlruPageRow {
            kind: SlruKind::Clog,
            segno: s,
            blknum: blk,
            lsn: Lsn(lsn),
            is_image,
            will_init,
            payload: Bytes::from_static(b"x"),
        };
        let seg_size_row = |s: u32, lsn: u64| MetaRow {
            key: Bytes::from(seg_size_key(s)),
            lsn: Lsn(lsn),
            payload: Bytes::from_static(b"sz"),
        };

        store
            .put_slru_pages(
                branch,
                &[
                    // dropped incarnation: everything <= due (100) must go
                    slru(seg, 0, 10, true, true),
                    slru(seg, 0, 30, false, false),
                    slru(seg, 1, 20, true, true),
                    // segno reused above the drop: must survive
                    slru(seg, 0, 200, true, true),
                    // unrelated segment: must survive
                    slru(other, 0, 10, true, true),
                ],
            )
            .await
            .unwrap();
        store
            .put_meta(
                branch,
                &[
                    seg_size_row(seg, 10),
                    seg_size_row(seg, 20),
                    // reused incarnation's size: must survive
                    seg_size_row(seg, 200),
                    seg_size_row(other, 10),
                ],
            )
            .await
            .unwrap();

        store
            .gc_act_drop_slru(branch, (clog, seg as i64), seg_size_key(seg), Lsn(100))
            .await
            .unwrap();

        let b0 = store
            .test_raw_slru_versions(branch, clog, seg as i64, 0)
            .await
            .unwrap();
        assert_eq!(b0, vec![(200, true, true)], "reused segno's row survives");
        let b1 = store
            .test_raw_slru_versions(branch, clog, seg as i64, 1)
            .await
            .unwrap();
        assert!(b1.is_empty(), "dropped incarnation fully reclaimed");
        let o = store
            .test_raw_slru_versions(branch, clog, other as i64, 0)
            .await
            .unwrap();
        assert_eq!(o.len(), 1, "unrelated segment untouched");

        // Seg-size meta: versions <= due gone, the reuse's kept.
        let below = store
            .read_meta(branch, &seg_size_key(seg), Lsn(199))
            .await
            .unwrap();
        assert!(below.is_none(), "dropped incarnation's seg-size gone");
        let above = store
            .read_meta(branch, &seg_size_key(seg), Lsn(1000))
            .await
            .unwrap();
        assert_eq!(above.unwrap().lsn, Lsn(200), "reused seg-size survives");
        assert!(
            store
                .read_meta(branch, &seg_size_key(other), Lsn(1000))
                .await
                .unwrap()
                .is_some(),
            "unrelated segment's seg-size untouched"
        );

        // Idempotent (== 0 is retry-safe).
        let again = store
            .gc_act_drop_slru(branch, (clog, seg as i64), seg_size_key(seg), Lsn(100))
            .await
            .unwrap();
        assert_eq!(again, 0);
    }

    /// Scenario: shrink deletes the truncated tail `blknum >= relsize`
    /// at `lsn <= due_lsn` — both carried by the candidate, no meta read.
    /// Blocks under the target size are left alone, and a tail block
    /// re-extended ABOVE `due_lsn` (a later `will_init` base) survives:
    /// the `lsn <= due_lsn` bound is exactly what keeps a re-extended
    /// block live. A fork with no matching rows is a clean no-op.
    #[tokio::test]
    async fn gc_act_shrink_fork_tail_at_due_lsn() {
        let (store, _schema) = make_store("gc_shrink").await;
        let branch = rand_branch();
        let rel = 16430u32;

        let key = |blk: u32| page_key(GC_SPC as u32, GC_DB as u32, rel, 0, blk);
        store
            .put_pages(
                branch,
                &[
                    // 4 blocks created at lsn 10, block 1 churned at 30.
                    page_row(key(0), 10, true, true, b"i10"),
                    page_row(key(1), 10, true, true, b"i10"),
                    page_row(key(1), 30, false, false, b"d30"),
                    page_row(key(2), 10, true, true, b"i10"),
                    page_row(key(3), 10, true, true, b"i10"),
                    // block 2 re-extended ABOVE due_lsn (50): must survive.
                    page_row(key(2), 80, true, true, b"i80"),
                ],
            )
            .await
            .unwrap();

        // Truncated to 2 at lsn 50 ⇒ the candidate carries relsize=2,
        // due_lsn=50. Deletes block 2@10 and block 3@10 (>= 2, <= 50);
        // the surviving-row checks below pin the outcome (exact DELETE
        // counts aren't retry-safe — see the module note).
        store
            .gc_act_shrink_fork(branch, (GC_SPC, GC_DB, rel as i64, 0), 2, Lsn(50))
            .await
            .unwrap();

        for blk in [0u32, 1] {
            let rows = store
                .test_raw_block_versions(branch, GC_SPC, GC_DB, rel as i64, 0, blk as i64)
                .await
                .unwrap();
            assert!(!rows.is_empty(), "block {blk} under the size untouched");
        }
        let b2 = store
            .test_raw_block_versions(branch, GC_SPC, GC_DB, rel as i64, 0, 2)
            .await
            .unwrap();
        assert_eq!(
            b2,
            vec![(80, true, true)],
            "re-extension above due_lsn survives, lsn-10 tail row gone"
        );
        let b3 = store
            .test_raw_block_versions(branch, GC_SPC, GC_DB, rel as i64, 0, 3)
            .await
            .unwrap();
        assert!(b3.is_empty(), "tail block fully reclaimed");

        // Idempotent, and a fork with no matching rows is a clean no-op.
        let again = store
            .gc_act_shrink_fork(branch, (GC_SPC, GC_DB, rel as i64, 0), 2, Lsn(50))
            .await
            .unwrap();
        assert_eq!(again, 0, "nothing left below the tail to delete");
        let none = store
            .gc_act_shrink_fork(branch, (GC_SPC, GC_DB, 16431, 0), 2, Lsn(50))
            .await
            .unwrap();
        assert_eq!(none, 0);
    }

    /// Scenario: the persisted read floor is monotone (GREATEST — a recomputed
    /// cutoff can regress when inputs move backwards, the column must not). It
    /// used to be generation-fenced too; keeping a superseded pageserver from
    /// draining under a live one's leases is the endpoint's write lease now
    /// (compute/architecture/compute-fencing.md).
    #[tokio::test]
    async fn publish_gc_cutoff_is_monotone() {
        let (store, _schema) = make_store("gc_publish").await;
        let branch = rand_branch();
        let mut row = sample_branch_state_row(branch);
        row.applied_gc_cutoff_lsn = Lsn(50);
        create_branch_state_row(&store, &row).await.unwrap();

        // Advance.
        assert_eq!(
            store.publish_gc_cutoff(branch, Lsn(80)).await.unwrap(),
            Lsn(80)
        );

        // A regressing input leaves the floor where it was (GREATEST), and the
        // returned value is the authoritative floor rather than what was asked
        // for.
        assert_eq!(
            store.publish_gc_cutoff(branch, Lsn(60)).await.unwrap(),
            Lsn(80)
        );

        let reread = store.get_branch_state(branch).await.unwrap().unwrap();
        assert_eq!(reread.applied_gc_cutoff_lsn, Lsn(80));

        // Missing row → hard error: the publish ran before the creation write.
        assert!(
            store
                .publish_gc_cutoff(rand_branch(), Lsn(10))
                .await
                .is_err()
        );
    }

    /// Scenario: the fork↔cutoff interlock, GC half. The publish clamps
    /// to min(ancestor_lsn) over child ROWS inside the same UPDATE that
    /// writes the cutoff — so a fork registered before the statement's
    /// snapshot is never cut past. The pin is the row's *presence*:
    /// only removing it (what branch-deletion GC will do) releases the
    /// pin. (The no-children case is the monotone test above: its
    /// publishes advance unclamped.)
    #[tokio::test]
    async fn publish_gc_cutoff_clamps_to_child_rows() {
        let (store, _schema) = make_store("gc_publish_clamp").await;
        let parent = rand_branch();
        let mut row = sample_branch_state_row(parent);
        row.applied_gc_cutoff_lsn = Lsn(50);
        create_branch_state_row(&store, &row).await.unwrap();

        // Children forked at 110, 150 and 90.
        create_child_row(&store, parent, 110).await;
        create_child_row(&store, parent, 150).await;
        let lowest = create_child_row(&store, parent, 90).await;

        // The lowest child pins: 105 clamps to 90.
        let out = store.publish_gc_cutoff(parent, Lsn(105)).await.unwrap();
        assert_eq!(out, Lsn(90));

        // Removing its row releases the pin: the same target advances.
        let delete_sql = format!("DELETE FROM \"{PROD_SCHEMA}\".branch_state WHERE branch_id = $1");
        store
            .ysql_client
            .execute(&delete_sql, &[&lowest])
            .await
            .unwrap();
        let out = store.publish_gc_cutoff(parent, Lsn(105)).await.unwrap();
        assert_eq!(out, Lsn(105));

        // Target past the remaining children clamps to their MIN fork
        // point (110, not 150).
        let out = store.publish_gc_cutoff(parent, Lsn(200)).await.unwrap();
        assert_eq!(out, Lsn(110));
    }

    /// A child `branch_state` row of `parent` forked at `lsn`. Returns
    /// the child's id.
    async fn create_child_row(store: &PageStore, parent: BranchId, lsn: u64) -> BranchId {
        let child = rand_branch();
        let mut row = sample_branch_state_row(child);
        row.ancestor_branch_id = Some(parent);
        row.ancestor_lsn = Some(Lsn(lsn));
        create_branch_state_row(store, &row).await.unwrap();
        child
    }

    /// A raw SQL session for the interlock stress test's fork side — the
    /// controller registers forks over its own pgx connection (with a real
    /// multi-statement transaction), which `YsqlClient` doesn't model.
    /// Connection defaults come from `test_yb_config()` so the raw session
    /// always hits the same cluster as the store under test.
    async fn raw_pg_client() -> tokio_postgres::Client {
        let url = test_yb_config().to_database_url().unwrap();
        let (client, connection) = tokio_postgres::connect(&url, tokio_postgres::NoTls)
            .await
            .expect("connect raw client to local YB");
        tokio::spawn(async move {
            let _ = connection.await;
        });
        client
    }

    /// A fork-shaped peer for the race: the same transaction shape as
    /// the controller's `RecordBranch` — child row INSERT + conditional
    /// dummy UPDATE on the parent row — standing in for the Go
    /// controller, which a Rust test cannot invoke. `Ok(true)` =
    /// registered, `Ok(false)` = rejected (rolled back, child row never
    /// lands), `Err` = transient (the caller just retries the attempt).
    async fn fork_txn(
        client: &mut tokio_postgres::Client,
        parent: BranchId,
        child: BranchId,
        fork_lsn: i64,
    ) -> Result<bool> {
        let insert_sql = format!(
            "INSERT INTO \"{PROD_SCHEMA}\".branch_state \
             (branch_id, pg_version, ancestor_branch_id, ancestor_lsn) \
             VALUES ($1, 180002, $2, $3) ON CONFLICT (branch_id) DO NOTHING"
        );
        let touch_sql = format!(
            "UPDATE \"{PROD_SCHEMA}\".branch_state \
             SET applied_gc_cutoff_lsn = applied_gc_cutoff_lsn \
             WHERE branch_id = $1 \
               AND COALESCE(applied_gc_cutoff_lsn, 0) <= $2"
        );
        let tx = client.transaction().await?;
        tx.execute(&insert_sql, &[&child, &parent, &fork_lsn])
            .await?;
        let touched = tx.execute(&touch_sql, &[&parent, &fork_lsn]).await?;
        if touched == 1 {
            tx.commit().await?;
            Ok(true)
        } else {
            tx.rollback().await?;
            Ok(false)
        }
    }

    /// The fork ↔ cutoff write-skew race, one pair at a time: each round
    /// fires ONE fork transaction (the controller's, via [`fork_txn`])
    /// and ONE `publish_gc_cutoff` — the fork just above the current
    /// cutoff, the publish target past it. Rounds cycle through three
    /// shapes:
    ///
    /// - fork committed, then publish: must accept, and the published
    ///   cutoff must stop at the fork point (deterministic);
    /// - publish committed, then fork: must reject (deterministic);
    /// - dead heat (`tokio::join!`): either serialization order is
    ///   legal; the round asserts the invariant either way.
    ///
    /// Without the interlock (the clamp inside the publish UPDATE + the
    /// touch in the fork txn) a third outcome exists — the fork accepted
    /// AND the cutoff past its fork point, each side committed on a
    /// stale view of the other. The fork-then-publish shape catches that
    /// deterministically in its first round; the dead-heat shape hunts
    /// it in genuine interleavings. ~30 rounds of milliseconds each —
    /// the whole test runs in about a second.
    #[tokio::test]
    async fn fork_registration_vs_cutoff_advance_race() {
        const ROUNDS: u64 = 30;
        const STEP: u64 = 1_000;

        let (store, _schema) = make_store("gc_interlock_race").await;
        let parent = rand_branch();
        let mut row = sample_branch_state_row(parent);
        row.applied_gc_cutoff_lsn = Lsn(0);
        create_branch_state_row(&store, &row).await.unwrap();

        let mut client = raw_pg_client().await;
        let delete_sql = format!("DELETE FROM \"{PROD_SCHEMA}\".branch_state WHERE branch_id = $1");

        for round in 0..ROUNDS {
            let cutoff = store
                .get_branch_state(parent)
                .await
                .unwrap()
                .expect("race parent row exists")
                .applied_gc_cutoff_lsn;
            let fork_lsn = cutoff.0 as i64 + 1;
            let target = Lsn(cutoff.0 + STEP);
            let child = rand_branch();

            let publish = || store.publish_gc_cutoff(parent, target);
            let (forked, out) = match round % 3 {
                // Fork committed first: the publish must see the child.
                0 => {
                    let forked = fork_txn(&mut client, parent, child, fork_lsn)
                        .await
                        .unwrap();
                    assert!(
                        forked,
                        "round {round}: a fork above the cutoff with no \
                         publish in flight must register"
                    );
                    (forked, publish().await)
                }
                // Publish committed first: the fork must see the cutoff.
                1 => {
                    let out = publish().await;
                    let forked = fork_txn(&mut client, parent, child, fork_lsn)
                        .await
                        .unwrap();
                    assert!(
                        !forked,
                        "round {round}: a fork below the just-published \
                         cutoff must be rejected"
                    );
                    (forked, out)
                }
                // Dead heat: the genuine race; either order is legal.
                _ => {
                    let (forked, out) =
                        tokio::join!(fork_txn(&mut client, parent, child, fork_lsn), publish());
                    (forked.unwrap(), out)
                }
            };
            let applied = out.unwrap();

            if forked {
                assert!(
                    applied.0 as i64 <= fork_lsn,
                    "round {round}: write skew — fork at {fork_lsn} accepted \
                     while the cutoff advanced to {applied}"
                );
                // Unpin (row removal, as branch-deletion GC will do) so
                // the next round's cutoff can advance.
                store
                    .ysql_client
                    .execute(&delete_sql, &[&child])
                    .await
                    .unwrap();
            }
        }

        // All children were unpinned by row removal; the parent must be
        // free to advance exactly to a fresh target.
        let target = Lsn((ROUNDS + 10) * STEP * 2);
        let out = store.publish_gc_cutoff(parent, target).await.unwrap();
        assert_eq!(out, target);

        // branch_state is shared with the whole suite: remove this run's
        // parent (children are already gone — accepted ones deleted,
        // rejected ones rolled back).
        store
            .ysql_client
            .execute(&delete_sql, &[&parent])
            .await
            .unwrap();
    }

    /// Randomized model check of the trim, fixed seeds (reproducible).
    ///
    /// Per seed: 6 blocks get random version histories (1..=8 versions
    /// at distinct random LSNs; the oldest version of a block is
    /// always a base for physical validity; later versions are plain
    /// deltas / init deltas / images at random; one random plain delta
    /// may gain a same-LSN image twin — the materialiser pattern).
    /// A random cutoff is drawn, all blocks are trimmed, and the
    /// surviving `(lsn, is_image, will_init)` set of every block must
    /// equal a 5-line model:
    ///
    ///   anchor = max lsn with will_init && lsn ≤ cutoff
    ///   no anchor → everything survives
    ///   else survive ⇔ lsn > anchor, or lsn == anchor && will_init
    #[tokio::test]
    async fn gc_trim_randomized_model_check() {
        use rand::rngs::StdRng;
        use rand::{Rng, SeedableRng};

        let (store, _schema) = make_store("gc_random").await;

        for seed in [1u64, 7, 42, 1337] {
            let mut rng = StdRng::seed_from_u64(seed);
            let branch = rand_branch();
            let rel = 30000 + seed as u32;
            let nblocks = 6u32;

            // ---- generate ------------------------------------------
            let mut rows: Vec<Arc<PageRow>> = Vec::new();
            // (blk → Vec<(lsn, is_image, will_init)>)
            let mut model: Vec<Vec<(u64, bool, bool)>> = Vec::new();
            for blk in 0..nblocks {
                let nversions = rng.random_range(1..=8usize);
                let mut lsns: Vec<u64> = Vec::new();
                while lsns.len() < nversions {
                    let l = rng.random_range(1..=300u64);
                    if !lsns.contains(&l) {
                        lsns.push(l);
                    }
                }
                lsns.sort_unstable();
                let mut versions: Vec<(u64, bool, bool)> = Vec::new();
                for (i, &lsn) in lsns.iter().enumerate() {
                    let (is_image, will_init) = if i == 0 {
                        // A chain's first version must be a base.
                        if rng.random_bool(0.5) {
                            (true, true)
                        } else {
                            (false, true)
                        }
                    } else {
                        match rng.random_range(0..100u32) {
                            0..=54 => (false, false), // plain delta
                            55..=69 => (false, true), // init delta
                            _ => (true, true),        // image
                        }
                    };
                    versions.push((lsn, is_image, will_init));
                }
                // Materialiser twin: image stamped at a plain delta's LSN.
                if let Some(&(lsn, _, _)) = versions
                    .iter()
                    .find(|(_, im, wi)| !im && !wi)
                    .filter(|_| rng.random_bool(0.5))
                {
                    versions.push((lsn, true, true));
                }
                for &(lsn, is_image, will_init) in &versions {
                    rows.push(page_row(
                        page_key(GC_SPC as u32, GC_DB as u32, rel, 0, blk),
                        lsn,
                        is_image,
                        will_init,
                        format!("s{seed}b{blk}l{lsn}").as_bytes(),
                    ));
                }
                model.push(versions);
            }
            store.put_pages(branch, &rows).await.unwrap();

            // ---- trim ----------------------------------------------
            let cutoff = Lsn(rng.random_range(0..=350u64));
            let blks: Vec<i64> = (0..nblocks as i64).collect();
            store
                .gc_act_trim_blocks(branch, (GC_SPC, GC_DB, rel as i64, 0), &blks, cutoff)
                .await
                .unwrap();

            // ---- compare against the model -------------------------
            for (blk, versions) in model.iter().enumerate() {
                let anchor = versions
                    .iter()
                    .filter(|(lsn, _, wi)| *wi && *lsn <= cutoff.0)
                    .map(|(lsn, _, _)| *lsn)
                    .max();
                let mut expected: Vec<(u64, bool, bool)> = match anchor {
                    None => versions.clone(),
                    Some(a) => versions
                        .iter()
                        .copied()
                        .filter(|(lsn, _, wi)| *lsn > a || (*lsn == a && *wi))
                        .collect(),
                };
                expected.sort_by_key(|(lsn, im, _)| (*lsn, *im));
                let got = store
                    .test_raw_block_versions(branch, GC_SPC, GC_DB, rel as i64, 0, blk as i64)
                    .await
                    .unwrap();
                assert_eq!(
                    got, expected,
                    "seed={seed} blk={blk} cutoff={cutoff} versions={versions:?}"
                );
            }
        }
    }
}
