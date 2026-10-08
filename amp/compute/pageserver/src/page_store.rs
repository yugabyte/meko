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
//!     version     SMALLINT NOT NULL,   -- always 0 for now (DEFAULT_VERSION)
//!     spcnode     OID      NOT NULL,
//!     dbnode      OID      NOT NULL,
//!     relnode     OID      NOT NULL,
//!     forknum     SMALLINT NOT NULL,
//!     blknum      OID      NOT NULL,
//!     is_image    BOOLEAN  NOT NULL,
//!     lsn         BIGINT   NOT NULL,
//!     will_init   BOOLEAN  NOT NULL,
//!     payload     BYTEA    NOT NULL,
//!     CHECK (will_init OR NOT is_image),
//!     PRIMARY KEY (bucket_id ASC, branch_id ASC, version ASC, spcnode ASC,
//!                  dbnode ASC, relnode ASC, forknum ASC, blknum ASC,
//!                  lsn DESC, will_init DESC)
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
//! a read whose blknum is known binds the whole key prefix and touches one
//! tablet, while a blknum *range* scan touches one bucket per 2MB stretch it
//! spans, capped at `PAGE_BUCKETS`. The thin-lane one-fork scans take the
//! first shape — a contiguous range and a scattered key set alike, one bound
//! read per block, so each stops at its own anchor (#1054) and each binds its
//! own bucket. A bucket bind is one VALUE, never a set —
//! [`yb_thin_client::RangeRead::new_range`] binds one value per key column,
//! and the shim's `range_values` ABI has no set form — so the readers that
//! cannot name their blocks (whole-fork and cross-fork) loop over buckets.
//!
//! The trailing `will_init` breaks `lsn` ties; it is DESC so the stored
//! order matches the read-path `ORDER BY lsn DESC, will_init DESC`
//! (base before non-base at a tie; see [`assert_chain_step`]). Reads
//! anchor the chain on the newest **base** at-or-below target_lsn
//! (`AND will_init ORDER BY lsn DESC LIMIT 1`) and return the deltas
//! above it. `is_image` is a value column: it says only whether the
//! payload is a page or a WAL record.
//! Chain length is not a write-time invariant: the background image
//! materialiser creates fresh images for hot keys to keep chains
//! short. Reads remain correct without it, just potentially slower.

#[cfg(test)]
use std::cell::RefCell;
use std::collections::HashMap;
use std::future::Future;
use std::ops::Range;
use std::sync::Arc;

use anyhow::{Context, Result, bail};
use bytes::Bytes;
use futures::stream::StreamExt;
use itertools::{Either, Itertools as _};
use pageserver_api::key::{KEY_SIZE, Key};
use pageserver_api::models::PayloadCompression;
use pageserver_api::reltag::SlruKind;

use remote_storage::{YbConnConfig, YsqlClient};
use tokio_util::sync::CancellationToken;
// We talk to YugabyteDB through the YB smart-driver fork of
// tokio-postgres; alias it so the type names below (`tokio_postgres::Row`,
// etc.) keep working without per-callsite churn. The Neon fork of tokio-postgres survives
// elsewhere in the workspace for the walproposer/walreceiver
// replication path.
use utils::id::BranchId;
use utils::lsn::Lsn;
use utils::payload_codec;
use yb_tokio_postgres as tokio_postgres;

mod batched_upsert;
mod gc;
#[cfg(test)]
mod test_support;

use batched_upsert::{META_LANE, PAGES_LANE, SLRU_PAGES_LANE};

// GC's storage layer is a submodule for size only; its public surface stays
// `page_store::…` so the drain and the emitter keep their imports.
pub use gc::{
    GC_BUCKETS, GC_SWEEP_MAX_ROWS_PER_DELETE, GcDbIdent, GcForkIdent, GcKind, GcSelectedCandidates,
    GcSlruSeg, GcSweepProgress, GcSweepScope, PendingGcRow, TrimOutcome, gc_bucket_id,
};
pub(crate) use gc::{
    GcCandidateId, GcSweepCandidate, materialise_object_key, rel_fork_of, slru_seg_of,
};

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

/// The two fields the lineage walk reads off a chain row, so
/// [`LineageSegment::shadowed_by`] serves rel-page and SLRU chains alike.
/// Implemented on the two types a chain is held as: rel-page chains
/// are `Arc<PageRow>`, SLRU chains bare [`SlruPageRow`].
pub(crate) trait AnchorRow {
    fn lsn(&self) -> Lsn;
    fn will_init(&self) -> bool;
}

impl AnchorRow for Arc<PageRow> {
    fn lsn(&self) -> Lsn {
        self.lsn
    }
    fn will_init(&self) -> bool {
        self.will_init
    }
}

impl AnchorRow for SlruPageRow {
    fn lsn(&self) -> Lsn {
        self.lsn
    }
    fn will_init(&self) -> bool {
        self.will_init
    }
}

/// Dedup an LSN-descending, image-first-on-ties page chain by LSN.
///
/// The chain MUST already be sorted descending by LSN with bases sorting
/// before non-bases on a tie
/// (`b.lsn.cmp(&a.lsn).then(b.will_init.cmp(&a.will_init))`).
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
/// What IS flagged: two non-base rows at one LSN whose CONTENT differs: a
/// *non-base* survivor over a row with a different payload. That means we are
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
    // (chain[i-1], which the dedup keeps) is not a base, so chain[i] is a
    // real record about to be silently dropped. Walk pairs so both rows are
    // in hand for the diagnostic dump. A base survivor shadows the row
    // below it either way, so that drop loses nothing.
    for i in 1..chain.len() {
        let survivor = &chain[i - 1];
        let dropped = &chain[i];
        if survivor.lsn != dropped.lsn || survivor.will_init {
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

/// Put a merged page chain into result form: LSN-descending, base before delta
/// on ties, one row per LSN, nothing below the newest `will_init`.
///
/// Every page-chain blend (lineage segments, or `PageWriter`'s cache with the
/// store) ends here. The base sorts first on ties so the dedup keeps it over
/// the delta it folded; the stable sort keeps the earlier of two tied bases.
/// [`merge_slru_chain`] is the SLRU twin.
pub(crate) fn merge_page_chain(chain: &mut Vec<Arc<PageRow>>) {
    chain.sort_by(|a, b| {
        b.lsn
            .cmp(&a.lsn)
            .then_with(|| b.will_init.cmp(&a.will_init))
    });
    dedup_page_chain_by_lsn(chain);
    if let Some(idx) = chain.iter().position(|r| r.will_init) {
        chain.truncate(idx + 1);
    }
}

fn slru_kind_to_i16(kind: SlruKind) -> i16 {
    match kind {
        SlruKind::Clog => 0,
        SlruKind::MultiXactMembers => 1,
        SlruKind::MultiXactOffsets => 2,
    }
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

    /// Whether `chain` holds a `will_init` row AT OR ABOVE this segment's cap,
    /// so every row the segment can return sits below that anchor and would be
    /// trimmed. An anchor below the cap does not suffice: a materialiser
    /// copy-up image can land under the ancestor's head, and the ancestor
    /// deltas in (anchor_lsn, cap] still apply. Caps are non-increasing, so a
    /// chain that shadows one segment shadows every deeper one.
    ///
    /// Scanned back to front: a merged chain ends at its anchor.
    pub(crate) fn shadowed_by<R: AnchorRow>(&self, chain: &[R]) -> bool {
        chain
            .iter()
            .rev()
            .any(|r| r.will_init() && r.lsn() >= self.cap_lsn)
    }

    /// Drop from `requested` every key whose chain in `have` already
    /// [shadows](Self::shadowed_by) this segment.
    fn retain_unshadowed<K, R>(&self, requested: &mut Vec<K>, have: &HashMap<K, Vec<R>>)
    where
        K: Eq + std::hash::Hash,
        R: AnchorRow,
    {
        requested.retain(|k| !have.get(k).is_some_and(|chain| self.shadowed_by(chain)));
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

/// The `version` key column of `pages`, `slru_pages`, `meta`, `pending_gc` and
/// `wals`: always 0 for now (see migration `0003_compute_storage`).
///
/// The controller's branch reaper (`controller/internal/branchgc`) reclaims
/// only `version = 0` from the hash-keyed `slru_pages`, `meta` and
/// `pending_gc`. Before any other version is written, fix it to delete every
/// version.
pub const DEFAULT_VERSION: i16 = 0;

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
    /// Sizes each chain-read round (see [`page_chain_read_limit`]). Plumbed through from
    /// `YbPageWriterConfig::image_chain_limit`.
    pub image_chain_limit: u32,
}

impl PageStoreConfig {
    pub fn new(yb: YbConnConfig) -> Self {
        Self {
            yb,
            schema: DEFAULT_SCHEMA.to_string(),
            scale_factor: pageserver_api::config::DEFAULT_SCALE_FACTOR,
            payload_compression: pageserver_api::config::defaults::DEFAULT_YB_PAYLOAD_COMPRESSION,
            image_chain_limit: pageserver_api::config::YbPageWriterConfig::unscaled_defaults()
                .image_chain_limit,
        }
    }

    pub fn with_image_chain_limit(mut self, image_chain_limit: u32) -> Self {
        self.image_chain_limit = image_chain_limit;
        self
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
    /// Row limit of each relation chain-read round, from [`page_chain_read_limit`].
    page_chain_read_limit: u64,
    /// The hot-path lane: `yb_thin_client` handles over the
    /// `libyb_thin_client` C++ shim for the tables this store owns, batching
    /// reads/writes into one Perform per call. Opened and health-checked at
    /// startup; the cold paths (GC drain, rehydration, branch state) stay on
    /// `ysql_client`.
    ///
    /// Held through a [`yb_thin_client::Recycling`], which re-connects the shim
    /// client and re-opens these tables once the current generation passes
    /// `YbConnConfig::connection_lifetime_ms` — re-resolving the configured
    /// tserver addresses, so a connection cannot outlive the tservers it was
    /// aimed at (#811). Each dispatch site takes the current generation for
    /// the duration of its call.
    pub thinclient: Arc<ThinClientPool>,
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

/// Anchor-stop accumulation for ONE block's chain over a clustered scan.
/// Rows arrive `lsn DESC`, base before non-base on an equal-LSN tie (the PKs
/// are `will_init DESC`), so the first `will_init` row ends the chain: what
/// follows it is shadowed.
struct ChainCollector<T> {
    blk: i64,
    rows: Vec<T>,
    complete: bool,
}

impl<T> ChainCollector<T> {
    fn new(blk: i64) -> Self {
        ChainCollector {
            blk,
            rows: Vec::new(),
            complete: false,
        }
    }

    /// The op binds `blk`, so a row for any other block means the server
    /// answered a different question than the one asked. Refuse it: filing it
    /// here would serve another block's pages as this one's.
    fn push(&mut self, blk: i64, will_init: bool, row: T) -> Result<()> {
        anyhow::ensure!(
            blk == self.blk,
            "chain scan op bound blk={} but a row came back for blk={blk}",
            self.blk
        );
        if self.complete {
            return Ok(());
        }
        self.complete = will_init;
        self.rows.push(row);
        Ok(())
    }

    /// True once the anchor is in hand, so further rows are dropped and the op
    /// can stop paging.
    fn complete(&self) -> bool {
        self.complete
    }
}

/// Drive a batched multi-op thin-lane block scan.
///
/// Every op rides one `Perform` and every continuation is pinned to the
/// snapshot round 1 ran at, so the fan-out resolves at a SINGLE read point
/// (#139); looping `read_scan` per op instead would take one read point each
/// and let a concurrent GC drain splice two snapshots into one scan.
///
/// Paging is mandatory, not an optimisation: an op's scan spans tablets, and a
/// tablet holding no matching row returns an EMPTY page plus a continuation —
/// "nothing here", not "scan done".
///
/// `block_reads[i]` is `(blk, read)` — the read bound to that one block. One
/// read per block is what lets each stop at its own anchor and what the
/// returned chains are keyed by; pairing them keeps the two from drifting.
async fn run_batched_block_scan(
    client: &yb_thin_client::TsClient,
    table: &yb_thin_client::TableHandle,
    block_reads: &[(u32, yb_thin_client::RangeRead)],
    stop: ScanStop,
    what: &str,
    op_label: impl Fn(usize) -> String,
) -> Result<Vec<Vec<yb_thin_client::Row>>> {
    for attempt in 1..=THIN_SCAN_RESTARTS {
        let mut collectors: Vec<ChainCollector<yb_thin_client::Row>> = block_reads
            .iter()
            .map(|&(blk, _)| ChainCollector::new(i64::from(blk)))
            .collect();
        let mut paging: Vec<Option<bytes::Bytes>> = vec![None; block_reads.len()];
        // Per-op consecutive-empty-round counters; see `check_barren_round`.
        let mut barren: Vec<usize> = vec![0; block_reads.len()];
        let mut active: Vec<usize> = (0..block_reads.len()).collect();
        let mut read_time_ht = 0u64;
        let mut restart = false;

        while !active.is_empty() {
            let ops: Vec<yb_thin_client::ReadOp> = active
                .iter()
                .map(|&i| yb_thin_client::ReadOp {
                    table,
                    read: &block_reads[i].1,
                    paging_state: paging[i].as_deref(),
                })
                .collect();
            let batch = match client.read(&ops, read_time_ht).await {
                Ok(b) => b,
                // Only an UNPINNED batch heals a read-restart inside
                // `TsClient::read`; from round 2 on every op is pinned.
                Err(e) if e.is_read_restart() && attempt < THIN_SCAN_RESTARTS => {
                    tracing::warn!(
                        "thin-lane {what} read-restart (attempt \
                         {attempt}/{THIN_SCAN_RESTARTS}), restarting the scan: {e}"
                    );
                    restart = true;
                    break;
                }
                Err(e) => return Err(e.into()),
            };
            read_time_ht = batch.used_read_time_ht;
            // `active[slot]` below is safe because `ops` is 1:1 with `active`
            // and `TsClient::read` guarantees one result per op sent.
            debug_assert_eq!(ops.len(), active.len());
            let mut next: Vec<usize> = Vec::new();
            for (slot, res) in batch.ops.into_iter().enumerate() {
                let i = active[slot];
                let got = res.rows.len();
                feed_scan_rows(&mut collectors[i], res.rows)
                    .with_context(|| format!("thin-lane {what} for {}", op_label(i)))?;
                if collectors[i].complete() || (stop == ScanStop::FirstRound && got > 0) {
                    continue;
                }
                if let Some(ps) = res.paging_state {
                    yb_thin_client::check_barren_round(&mut barren[i], got)
                        .with_context(|| format!("thin-lane {what} for {}", op_label(i)))?;
                    paging[i] = Some(ps);
                    next.push(i);
                }
            }
            active = next;
        }
        if !restart {
            return Ok(collectors.into_iter().map(|c| c.rows).collect());
        }
    }
    unreachable!("the final scan attempt returns or propagates")
}

/// When an op of [`run_batched_block_scan`] stops paging.
#[derive(Clone, Copy, PartialEq, Eq)]
enum ScanStop {
    /// At its block's `will_init` anchor, however many rounds that takes: the
    /// chain a page reconstruction needs.
    Anchor,
    /// After its first round that returns rows (the read's `limit` sizes it):
    /// the top of the chain. A round with no rows but a continuation is
    /// "nothing yet", not "nothing" — the tserver ran out of scan budget
    /// (tombstones, deadline) before the first match — so it is followed.
    FirstRound,
}

/// Which caller a [`PageStore::scan_one_fork_thin`] read belongs to.
#[derive(Clone, Copy, PartialEq, Eq)]
enum OneForkRead {
    Range,
    ScatteredSet,
}

impl OneForkRead {
    /// Labels the read in [`crate::metrics::YB_STORE_OP_SECONDS`].
    fn op_label(self) -> &'static str {
        match self {
            OneForkRead::Range => "scan_range",
            OneForkRead::ScatteredSet => "scan_scattered_set",
        }
    }
    /// Names the caller in chain assembly. The `_thin` suffix keeps a panic
    /// or decode error here from reading as the YSQL cross-fork arm's.
    fn caller_label(self) -> &'static str {
        match self {
            OneForkRead::Range => "scan_range_thin",
            OneForkRead::ScatteredSet => "scan_scattered_set_thin",
        }
    }
}

/// How much of each block's version chain an SLRU range read returns.
#[derive(Clone, Copy, Debug, PartialEq, Eq)]
pub enum SlruScan {
    /// Back to the newest `will_init` anchor: what a page reconstruction needs.
    ToAnchor,
    /// At most the newest `n` versions, one round per block — a cap, not a
    /// count: the round is also bounded by the tserver's own size and
    /// deadline budget, so fewer than `n` can come back for a block that has
    /// more. For a reader that decodes what it needs from the top of the
    /// chain (a clog block's trailer is the `timestamp` of its newest commit
    /// row) and never reconstructs the page; the result is not
    /// anchor-complete and must not be fed to reconstruction.
    NewestUpTo(u64),
}

impl SlruScan {
    fn page_rows(self) -> u64 {
        match self {
            SlruScan::ToAnchor => DIRECT_SLRU_CHAIN_PAGE_ROWS,
            SlruScan::NewestUpTo(n) => n,
        }
    }
    fn stop(self) -> ScanStop {
        match self {
            SlruScan::ToAnchor => ScanStop::Anchor,
            SlruScan::NewestUpTo(_) => ScanStop::FirstRound,
        }
    }
    fn op_label(self) -> &'static str {
        match self {
            SlruScan::ToAnchor => "scan_slru_range",
            SlruScan::NewestUpTo(_) => "scan_slru_newest",
        }
    }
}

/// Put a merged SLRU chain into result form: LSN-descending, base before
/// delta on ties, one row per LSN, nothing below the newest `will_init`, and
/// at most `n` rows for [`SlruScan::NewestUpTo`].
///
/// Every SLRU merge (two branch segments, or the writer's buffer with the
/// store) goes through here. The tie order makes the dedup keep the base,
/// which is the page after the equal-LSN delta; the dedup also collapses the
/// same flushed delta seen from both buffer and store.
pub(crate) fn merge_slru_chain(chain: &mut Vec<SlruPageRow>, mode: SlruScan) {
    chain.sort_by(|a, b| {
        b.lsn
            .cmp(&a.lsn)
            .then_with(|| b.will_init.cmp(&a.will_init))
    });
    chain.dedup_by(|a, b| a.lsn == b.lsn);
    if let Some(idx) = chain.iter().position(|r| r.will_init) {
        chain.truncate(idx + 1);
    }
    if let SlruScan::NewestUpTo(n) = mode {
        chain.truncate(n as usize);
    }
}

/// Whole-scan restarts for a batched thin-lane block scan
/// ([`run_batched_block_scan`]). A read-restart on a *pinned* batch —
/// which every paging round after the first is — cannot be healed inside
/// `TsClient::read`, so the scan driver retries the whole fan-out on a fresh
/// read point. Mirrors the client's own `MAX_SCAN_RESTARTS`.
const THIN_SCAN_RESTARTS: usize = 5;

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
        Ok(row_cell(self, i)?.as_i64()?)
    }
    fn cell_bool(&self, i: usize) -> Result<bool> {
        Ok(row_cell(self, i)?.as_bool()?)
    }
    fn cell_bytes(&self, i: usize) -> Result<Bytes> {
        Ok(row_cell(self, i)?.as_bytes()?.clone())
    }
}

/// Cell `i` of `row`; a row with fewer cells than the read targeted is an error, not a panic.
fn row_cell(row: &yb_thin_client::Row, i: usize) -> Result<&yb_thin_client::Value> {
    row.get(i).with_context(|| format!("row has {} cells, no cell {i}", row.len()))
}

/// Columns every chain read fetches, set by [`page_chain_read`] and [`slru_segment_read`]; rows
/// are read back at the `CHAIN_*` positions.
const CHAIN_TARGETS: [&str; 5] = ["lsn", "is_image", "will_init", "payload", "blknum"];
const CHAIN_LSN: usize = 0;
const CHAIN_IS_IMAGE: usize = 1;
const CHAIN_WILL_INIT: usize = 2;
const CHAIN_PAYLOAD: usize = 3;
const CHAIN_BLKNUM: usize = 4;
/// Columns a `meta` read fetches, and where each sits in the row.
const META_TARGETS: [&str; 2] = ["lsn", "payload"];
const META_LSN: usize = 0;
const META_PAYLOAD: usize = 1;

/// The bound key prefix selecting ONE page's chain in `pages`: all eight leading key columns, so
/// the rows it spans are exactly that page's versions and the read is a single clustered seek on
/// one tablet, fetching [`CHAIN_TARGETS`]. Callers append the `lsn` bound and limit that decide
/// how much of the chain they want.
///
/// Shared by [`PageStore::read_chain_thin`] and
/// [`PageStore::scan_one_fork_thin`] rather than restated: the bind ORDER is
/// the primary key's, and a drifted copy would read the wrong span.
fn page_chain_read(
    pages: &yb_thin_client::TableHandle,
    branch: &bytes::Bytes,
    sp: u32,
    db: u32,
    rel: u32,
    fork: i16,
    blk: u32,
) -> Result<yb_thin_client::RangeRead> {
    use yb_thin_client::{BindValue, RangeRead};
    Ok(RangeRead::new_range(
        pages,
        vec![
            BindValue::I16(page_bucket_id(rel, blk)),
            BindValue::Bytes(branch.clone()),
            BindValue::I16(DEFAULT_VERSION),
            BindValue::U32(sp),
            BindValue::U32(db),
            BindValue::U32(rel),
            BindValue::I16(fork),
            BindValue::U32(blk),
        ],
    )?
    .targets(pages, &CHAIN_TARGETS)?)
}

/// The shared prefix of every thin `slru_pages` read, fetching [`CHAIN_TARGETS`]: `branch` is the
/// whole hash key; version/kind/segno are range columns, bound as equality scan conds. Callers
/// append their own `blknum`/`lsn` conds.
fn slru_segment_read(
    slru: &yb_thin_client::TableHandle,
    branch: &bytes::Bytes,
    kind_i: i16,
    segno: u32,
) -> Result<yb_thin_client::RangeRead> {
    use yb_thin_client::{BindValue, CondOp, RangeRead};
    Ok(
        RangeRead::new(slru, vec![BindValue::Bytes(branch.clone())])?
            .cond(slru, "version", CondOp::Eq, BindValue::I16(DEFAULT_VERSION))?
            .cond(slru, "kind", CondOp::Eq, BindValue::I16(kind_i))?
            .cond(slru, "segno", CondOp::Eq, BindValue::U32(segno))?
            .targets(slru, &CHAIN_TARGETS)?,
    )
}

/// An SLRU block's `slru_pages.blknum`. The column is SMALLINT because the
/// block is a page's index within its segment, `0..SLRU_PAGES_PER_SEGMENT`,
/// and the importer refuses a segment file holding more pages than that.
/// An error rather than a panic, since the value arrives in a `Key`.
pub(crate) fn slru_blknum(blk: u32) -> Result<i16> {
    i16::try_from(blk).with_context(|| format!("SLRU blknum {blk} is not a page within a segment"))
}

/// Rows per round on a direct-RPC relation chain read: a chain at the materialiser's bound plus its base.
fn page_chain_read_limit(image_chain_limit: u32) -> u64 {
    match image_chain_limit {
        // Count trigger off: chains are unbounded, and n + 1 would page per row.
        0 => 32,
        n => u64::from(n) + 1,
    }
}

/// Rows per fetched page on direct-RPC single-key SLRU chain reads. Taller
/// than [`page_chain_read_limit`]: SLRU deltas are tiny and
/// `image_chain_limit_slru` allows chains up to ~1000 rows, so this sizes
/// the common case to one round trip.
const DIRECT_SLRU_CHAIN_PAGE_ROWS: u64 = 1024;

/// Paged single-key chain fetch on the direct-RPC lane. Rows arrive `lsn DESC, will_init DESC`,
/// so the first `will_init` row ends the chain.
async fn fetch_chain_rows_thin(
    client: &yb_thin_client::TsClient,
    table: &Arc<yb_thin_client::TableHandle>,
    read: yb_thin_client::RangeRead,
) -> Result<Vec<yb_thin_client::Row>> {
    client
        .read_scan(table, &read, Vec::new, |chain, rows| -> anyhow::Result<bool> {
            for row in rows {
                let anchor = row.cell_bool(CHAIN_WILL_INIT)?;
                chain.push(row);
                if anchor {
                    return Ok(true);
                }
            }
            Ok(false)
        })
        .await
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

/// Decode the raw rows of a chain fetch into [`PageRow`]s for `key`. The collectors hand them over
/// in PK order, cut at the base, so a row out of that order is a decode bug and fails the read.
fn assemble_page_chain<R: RowCells>(
    key: Key,
    raw: Vec<R>,
    where_: &str,
) -> Result<Vec<Arc<PageRow>>> {
    let mut last_row = None;
    raw.into_iter()
        .map(|row| {
            let lsn = Lsn(row.cell_i64(CHAIN_LSN)? as u64);
            let will_init = row.cell_bool(CHAIN_WILL_INIT)?;
            check_chain_step(&mut last_row, lsn, will_init, || {
                format!("pages key={key} ({where_})")
            })?;
            Ok(Arc::new(PageRow {
                key,
                lsn,
                is_image: row.cell_bool(CHAIN_IS_IMAGE)?,
                will_init,
                payload: decode_payload(&row.cell_bytes(CHAIN_PAYLOAD)?, || {
                    format!("pages row key={key} lsn={lsn} ({where_})")
                })?,
            }))
        })
        .collect()
}

/// Fail if a decoded chain row breaks `lsn DESC, will_init DESC` order after `last_row`, then
/// make it the new `last_row`.
fn check_chain_step<C: std::fmt::Display>(
    last_row: &mut Option<(Lsn, bool)>,
    lsn: Lsn,
    will_init: bool,
    what: impl FnOnce() -> C,
) -> Result<()> {
    if let Some(prev_row) = last_row.replace((lsn, will_init)) {
        if let Some(violation) = chain_step_error(prev_row, (lsn, will_init)) {
            bail!("{}: {violation}", what());
        }
    }
    Ok(())
}

/// [`assemble_page_chain`] for SLRU rows.
fn assemble_slru_chain<R: RowCells>(
    kind: SlruKind,
    segno: u32,
    blknum: u32,
    raw: Vec<R>,
    where_: &str,
) -> Result<Vec<SlruPageRow>> {
    let mut last_row = None;
    raw.into_iter()
        .map(|row| {
            let lsn = Lsn(row.cell_i64(CHAIN_LSN)? as u64);
            let will_init = row.cell_bool(CHAIN_WILL_INIT)?;
            check_chain_step(&mut last_row, lsn, will_init, || {
                format!("slru_pages kind={kind:?} segno={segno} blknum={blknum} ({where_})")
            })?;
            Ok(SlruPageRow {
                kind,
                segno,
                blknum,
                lsn,
                is_image: row.cell_bool(CHAIN_IS_IMAGE)?,
                will_init,
                payload: decode_payload(&row.cell_bytes(CHAIN_PAYLOAD)?, || {
                    format!(
                        "slru_pages row kind={kind:?} segno={segno} \
                             blknum={blknum} lsn={lsn} ({where_})"
                    )
                })?,
            })
        })
        .collect()
}

/// Feed one fetched page of a multi-block scan into the per-block collector. Used by the
/// direct-RPC scan loops.
fn feed_scan_rows<R: RowCells>(collector: &mut ChainCollector<R>, rows: Vec<R>) -> Result<()> {
    for row in rows {
        let blk = row.cell_i64(CHAIN_BLKNUM)?;
        let will_init = row.cell_bool(CHAIN_WILL_INIT)?;
        collector.push(blk, will_init, row)?;
    }
    Ok(())
}

/// Decode a `meta` row fetched with [`META_TARGETS`] for `key`. Used by the direct-RPC lane.
fn decode_meta_row<R: RowCells>(key: &[u8], row: &R) -> Result<MetaRow> {
    let lsn = Lsn(row.cell_i64(META_LSN)? as u64);
    Ok(MetaRow {
        key: Bytes::copy_from_slice(key),
        lsn,
        payload: decode_payload(&row.cell_bytes(META_PAYLOAD)?, || {
            format!("meta row key={} lsn={lsn}", hex::encode(key))
        })?,
    })
}

impl ThinClientTables {
    /// Connects the `yb_thin_client` (shim) client and opens the store's
    /// tables — the pageserver's hot-path startup healthcheck.
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
        let tls = yb
            .thin_client_tls()
            .context("TLS for the thin-client lane")?;
        let tls_desc = match &tls {
            None => "plaintext",
            Some(t) if t.client.is_some() => "mTLS",
            Some(_) => "TLS",
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
             connection, across {tserver_count} configured tserver(s), {tls_desc})",
        );
        Ok(pool)
    }
}

/// Decompose an 18-byte relation-page Key into the typed columns.
/// Returns `Err` if the bytes don't parse as `field1 = 0` (the
/// relation-page namespace).
fn key_columns(key: &Key) -> Result<(u32, u32, u32, i16, u32)> {
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
        key.field2,
        key.field3,
        key.field4,
        key.field5 as i16,
        key.field6,
    ))
}

/// Inverse: rebuild the 18-byte BYTEA Key from typed columns.
fn columns_to_key_bytes(sp: u32, db: u32, rel: u32, fork: i16, blk: u32) -> Bytes {
    let mut buf = [0u8; KEY_SIZE];
    // field1 = 0 (relation-page namespace)
    buf[0] = 0;
    buf[1..5].copy_from_slice(&sp.to_be_bytes());
    buf[5..9].copy_from_slice(&db.to_be_bytes());
    buf[9..13].copy_from_slice(&rel.to_be_bytes());
    buf[13] = fork as u8;
    buf[14..18].copy_from_slice(&blk.to_be_bytes());
    Bytes::copy_from_slice(&buf)
}

/// Rows per upsert batch in `put_pages` / `put_slru_pages` /
/// `gc_enqueue_candidates`. The value is inherited from the multi-row INSERT
/// these replaced, where PostgreSQL's 65535-parameter cap set the ceiling;
/// on the thin lane it simply bounds how many rows ride one `Perform`. Each
/// writer's fan-out is on its `batched_upsert` lane.
const ROWS_PER_INSERT: usize = 500;

/// Rows per chunk in `put_meta`. Larger than [`ROWS_PER_INSERT`] because
/// meta's payloads are mostly tiny rel-size sentinels, so a chunk of them
/// is far smaller than a chunk of 8 KB pages.
const META_ROWS_PER_INSERT: usize = 2000;

/// Encode a `Key` to its fixed 18-byte wire form — the `pending_gc.key`
/// (and `meta.key`) bytea.
pub(crate) fn key_to_bytes(key: &Key) -> Vec<u8> {
    let mut b = vec![0u8; KEY_SIZE];
    key.write_to_byte_slice(&mut b);
    b
}

/// The `amp_storage.pages` placement bucket, shared with the operator tools
/// (`dump-pages` pins its queries with the same function). Defined once in
/// `pageserver_api` — a second copy of a frozen key function is how rows go
/// missing.
pub use pageserver_api::pages_bucket::{PAGE_BUCKET_STRIDE, PAGE_BUCKETS, page_bucket_id};

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
             WHERE bucket_id=$13 AND branch_id=$1 AND version = {DEFAULT_VERSION} \
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
             WHERE p.bucket_id=$13 AND p.branch_id=$1 AND p.version = {DEFAULT_VERSION} \
               AND p.spcnode = b.spcnode AND p.dbnode = b.dbnode \
               AND p.relnode = b.relnode AND p.forknum = b.forknum \
               AND p.blknum = b.blknum \
               AND p.lsn <= $12 \
               AND p.lsn >= COALESCE(( \
                     SELECT a.lsn FROM \"{schema}\".pages a \
                     WHERE a.bucket_id=$13 AND a.branch_id=$1 AND a.version = {DEFAULT_VERSION} \
                       AND a.spcnode = b.spcnode AND a.dbnode = b.dbnode \
                       AND a.relnode = b.relnode AND a.forknum = b.forknum \
                       AND a.blknum = b.blknum \
                       AND a.will_init AND a.lsn <= $12 \
                     ORDER BY a.lsn DESC LIMIT 1), 0) \
             ORDER BY p.lsn DESC, p.will_init DESC \
         ) p \
         ORDER BY p.spcnode, p.dbnode, p.relnode, p.forknum, p.blknum, p.lsn DESC, p.will_init DESC"
    )
}

/// Bucket flat SQL rows into per-key chains. Rows arrive LSN-descending per
/// block (per the scan `ORDER BY`); each chain is truncated at the first
/// `will_init` row, since older rows are shadowed by that anchor (the image
/// row for blocks with an image, else the latest will_init delta). `label`
/// names the caller for the ordering assertion. The tail of the one YSQL
/// page reader left, [`PageStore::scan_range`]'s cross-fork branch.
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

/// One lineage segment read, as [`record_segment_scan`] files it.
#[cfg(test)]
#[derive(Debug, PartialEq, Eq)]
struct SegmentScan {
    branch_id: BranchId,
    /// How many keys the read covered, or `None` for the whole
    /// target. Recorded by the scan itself, so the SLRU walker's widening of a
    /// scattered residual to [`blknum_span`] shows up here.
    keys: Option<usize>,
}

#[cfg(test)]
thread_local! {
    /// Cleared by each test before the scan it asserts on. Needs a
    /// current-thread runtime, the `#[tokio::test]` default.
    static SEGMENT_SCANS: RefCell<Vec<SegmentScan>> = const { RefCell::new(Vec::new()) };
}

/// Record one lineage segment read for the tests; a no-op outside them. Which
/// segments a scan reads is invisible in its result, since the merge trims the
/// surplus either way.
fn record_segment_scan(_branch_id: BranchId, _keys: Option<usize>) {
    #[cfg(test)]
    SEGMENT_SCANS.with_borrow_mut(|scans| {
        scans.push(SegmentScan {
            branch_id: _branch_id,
            keys: _keys,
        })
    });
}

/// The ancestor walk both multi-key lineage scans run: rel-page blocks keyed by
/// [`Key`], SLRU blocks keyed by `u32`.
///
/// Reads each segment through `scan` at `min(target_lsn, seg.cap_lsn)` and
/// folds the chains into one map. An ancestor is read only for the keys of
/// `requested` it is not yet [shadowed](LineageSegment::shadowed_by) for, and
/// once none are left the walk ends. `scan` gets `None` for the whole target:
/// at the tip, until some key drops out, and throughout when `requested` is
/// `None`. `merge` runs per key only if an ancestor returned rows.
async fn walk_lineage_scan<K, R, I, Fut>(
    label: &str,
    lineage: &[LineageSegment],
    target_lsn: Lsn,
    requested: Option<I>,
    scan: impl Fn(BranchId, Lsn, Option<&[K]>) -> Fut,
    merge: impl Fn(&mut Vec<R>),
) -> Result<HashMap<K, Vec<R>>>
where
    K: Eq + std::hash::Hash,
    R: AnchorRow,
    I: IntoIterator<Item = K>,
    // A named future, not `AsyncFn`: the page service polls these reads from a
    // `Send` task, and an `AsyncFn` bound cannot carry `Send` through to the
    // future it returns.
    Fut: Future<Output = Result<HashMap<K, Vec<R>>>>,
{
    let capped = |seg: &LineageSegment| std::cmp::min(target_lsn, seg.cap_lsn);
    let Some((first, rest)) = lineage.split_first() else {
        bail!("{label}: empty lineage");
    };
    let mut out = scan(first.branch_id, capped(first), None).await?;
    if rest.is_empty() {
        return Ok(out);
    }
    // Enumerated only past the tip, so an unbranched read never pays for it.
    let mut remaining: Option<Vec<K>> = requested.map(|keys| keys.into_iter().collect());
    let mut trimmed = false;
    let mut ancestor_rows = false;
    for seg in rest {
        // `out` carries the ancestors read so far, so an anchor found in the
        // parent drops that key before the grandparent is reached.
        if let Some(keys) = remaining.as_mut() {
            let before = keys.len();
            seg.retain_unshadowed(keys, &out);
            if keys.is_empty() {
                break;
            }
            trimmed |= keys.len() != before;
        }
        // Until a key drops out the residual is the whole target.
        let residual = remaining.as_deref().filter(|_| trimmed);
        for (key, rows) in scan(seg.branch_id, capped(seg), residual).await? {
            ancestor_rows = true;
            out.entry(key).or_default().extend(rows);
        }
    }
    if ancestor_rows {
        out.values_mut().for_each(merge);
    }
    Ok(out)
}

/// The narrowest block range covering `blks`, or `None` when it is empty or
/// ends at `u32::MAX`. What an SLRU ancestor read narrows to: `scan_slru_range`
/// takes a range, so a scattered residual still reads the blocks between its
/// ends, but not the anchored ones outside them. `None` falls back to the
/// caller's full range.
fn blknum_span(blks: &[u32]) -> Option<Range<u32>> {
    Some(*blks.iter().min()?..blks.iter().max()?.checked_add(1)?)
}

/// What a lineage scan resolves at each ancestor segment: a contiguous key
/// range ([`PageStore::scan_range`]) or a scattered set of same-fork keys
/// ([`PageStore::scan_scattered_set`]). Lets [`PageStore::scan_lineage`] share
/// the ancestor-walk skeleton across both callers.
enum LineageScanTarget<'a> {
    Range(Range<Key>),
    Set(&'a [Key]),
}

/// Whether `range` stays inside one relation fork, so it varies only in
/// `field6` and its members are exactly the blocks between the bounds. Not
/// `keyspace::is_contiguous_range`, which also admits a relsize-sentinel spill
/// into the next fork; there `start.field6..end.field6` is not the key set.
fn one_fork_range(range: &Range<Key>) -> bool {
    range.start.field1 == range.end.field1
        && range.start.field2 == range.end.field2
        && range.start.field3 == range.end.field3
        && range.start.field4 == range.end.field4
        && range.start.field5 == range.end.field5
}

/// Whether `range` names exactly one relation block: the shape every compute
/// getpage RPC arrives as (`KeySpace::single(k..k.next())`), which the read
/// paths pin to a single-key query instead of a scan.
pub(crate) fn single_block_range(range: &Range<Key>) -> bool {
    one_fork_range(range) && spans_one_block(range)
}

/// The block half of [`single_block_range`], split out for a caller that has
/// already established the fork. Keeps the `field6` test in one place.
fn spans_one_block(range: &Range<Key>) -> bool {
    range.end.field6 == range.start.field6.wrapping_add(1)
}

impl<'a> LineageScanTarget<'a> {
    /// Every key this target asks for, or `None` for a range that crosses
    /// forks, whose key set is not knowable without reading. A set names its
    /// keys; a one-fork range is exactly `start.field6..end.field6` on
    /// `start`'s relation and fork. Lazy: an unbranched read never enumerates,
    /// and a branched one pays a `Vec` of at most `max_get_vectored_keys`
    /// keys when [`walk_lineage_scan`] collects it.
    fn requested_keys(&self) -> Option<impl Iterator<Item = Key> + 'a> {
        match self {
            LineageScanTarget::Set(keys) => Some(Either::Left(keys.iter().copied())),
            LineageScanTarget::Range(r) => {
                if !one_fork_range(r) {
                    return None;
                }
                let proto = r.start;
                Some(Either::Right((r.start.field6..r.end.field6).map(
                    move |blk| {
                        let mut k = proto;
                        k.field6 = blk;
                        k
                    },
                )))
            }
        }
    }
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
    /// `set_fast_path_upsert` enables `yb_enable_upsert_mode = true` on each
    /// connection. The GUC only skips YB's read-before-write on INSERT, and
    /// the `put_*` writes it was added for now run on the thin-client lane,
    /// so no production INSERT reaches these connections any more — the
    /// remaining YSQL work is SELECTs, branch-state UPDATEs and GC DELETEs.
    /// The flag itself is kept: clearing it changes a live connection GUC,
    /// which is worth its own commit rather than riding along with a rename.
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
                &[],
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
        let thinclient = ThinClientTables::connect(
            &config.yb,
            &config.schema,
            &ysql_client,
            read_sessions,
            write_sessions,
            sessions_per_conn,
        )
        .await
        .context("thin-client hot path init")?;
        Ok(Self {
            ysql_client,
            schema: Arc::from(config.schema),
            scale_factor: config.scale_factor,
            payload_codec: match config.payload_compression {
                PayloadCompression::None => payload_codec::Codec::Raw,
                PayloadCompression::Lz4 => payload_codec::Codec::Lz4,
            },
            page_chain_read_limit: page_chain_read_limit(config.image_chain_limit),
            thinclient,
        })
    }

    // ------------------- payload compression --------------------------------

    /// Wrap `plain` in the [`payload_codec`] envelope, counting it in `tally`,
    /// before the `put_*` write path binds it.
    ///
    /// This is the *only* place the pageserver produces encoded payloads.
    /// Everything upstream of `PageStore` — `PageWriter`'s delta tallies,
    /// the image materializer's chain-byte accounting, walredo, GC's
    /// payload comparison — keeps working on plaintext, which is why
    /// compression needed no changes there. Keep it that way: an encoded
    /// payload escaping above this boundary would silently reinterpret
    /// page bytes as record bytes.
    fn encode_payload(&self, plain: &Bytes, tally: &mut PayloadTally) -> Bytes {
        let (encoded, outcome) = payload_codec::encode(plain, self.payload_codec);
        tally.add(plain.len(), encoded.len(), outcome);
        encoded
    }

    // ------------------- pages ---------------------------------------------

    pub async fn put_pages(&self, branch_id: BranchId, rows: &[Arc<PageRow>]) -> Result<()> {
        self.put_pages_reporting(branch_id, rows, &|_| {}).await
    }

    /// [`Self::put_pages`], calling `landed` with each chunk's rows as that
    /// chunk lands, so the page writer can release backpressure before the
    /// whole flush is in. A chunk is reported once, after the write that made
    /// it durable; a failed call may have reported some chunks.
    pub async fn put_pages_reporting(
        &self,
        branch_id: BranchId,
        rows: &[Arc<PageRow>],
        landed: &(dyn Fn(&[Arc<PageRow>]) + Send + Sync),
    ) -> Result<()> {
        let mut tally = PayloadTally::new("pages");
        let branch_b = branch_id.to_bytes();
        self.upsert_batched(
            &PAGES_LANE,
            branch_id,
            rows,
            |pages, i, row| {
                // Encode before binding: a bind of `row.payload` directly
                // writes plaintext with no codec tag, which the read path
                // then rejects.
                let payload = self.encode_payload(&row.payload, &mut tally);
                Self::page_upsert(pages, &branch_b, i, row, payload)
            },
            |chunk| {
                for row in chunk {
                    pageserver_api::write_trace::trace_stage("put_pages_ok", &row.key, row.lsn);
                }
                landed(chunk);
            },
        )
        .await?;
        tally.record();
        Ok(())
    }

    /// Row `i` of a [`Self::put_pages`] batch as its `amp_storage.pages`
    /// upsert, binding the already-encoded `payload`.
    fn page_upsert(
        pages: &Arc<yb_thin_client::TableHandle>,
        branch_b: &Bytes,
        i: usize,
        row: &PageRow,
        payload: Bytes,
    ) -> Result<yb_thin_client::UpsertRow> {
        use yb_thin_client::{BindValue, UpsertRow};
        let (sp, db, rel, fork, blk) =
            key_columns(&row.key).with_context(|| format!("put_pages row {i}: bad key"))?;
        // key_values are in PRIMARY KEY order, not declaration order:
        // bucket_id first, (lsn, will_init) last, is_image out in the value
        // list. Neither CHECK fires on this lane, so `page_bucket_id` here is
        // the only thing keeping these rows findable.
        let upsert = UpsertRow::new(
            pages,
            vec![
                BindValue::I16(page_bucket_id(rel, blk)),
                BindValue::Bytes(branch_b.clone()),
                BindValue::I16(DEFAULT_VERSION),
                BindValue::U32(sp),
                BindValue::U32(db),
                BindValue::U32(rel),
                BindValue::I16(fork),
                BindValue::U32(blk),
                BindValue::I64(row.lsn.0 as i64),
                BindValue::Bool(row.will_init),
            ],
            &[
                ("is_image", BindValue::Bool(row.is_image)),
                ("payload", BindValue::Bytes(payload)),
            ],
        )?;
        pageserver_api::write_trace::trace_stage("put_pages_in", &row.key, row.lsn);
        Ok(upsert)
    }

    /// Per-key chain read. `key` must be a valid relation-page Key.
    ///
    /// No anchor predicate. The direct-RPC lane binds the whole key
    /// prefix, streams the block's versions LSN-descending and stops at
    /// the first `will_init` row ([`fetch_chain_rows_thin`]), which
    /// `will_init DESC` puts at the head of its tie group.
    pub async fn read_chain(
        &self,
        branch_id: BranchId,
        key: &[u8],
        target_lsn: Lsn,
    ) -> Result<Vec<Arc<PageRow>>> {
        let (sp, db, rel, fork, blk) = key_columns(&Key::from_slice(key))?;
        let target = target_lsn.0 as i64;

        // `branch_id` is bound per call, never fixed: lineage reads call this
        // with ANCESTOR branch_ids (a branch reads its ancestors' pages), so
        // the branch is not fixed on the read path even though it is on the
        // write path. The chain assembly truncates at the first
        // `will_init=true` row walking LSN-desc, so the no-image case keeps
        // full-chain semantics and an init-delta between image and target_lsn
        // shortens the chain.
        let p = self.thinclient.current().await;
        let started = std::time::Instant::now();
        let chain = self
            .read_chain_thin(&p, branch_id, sp, db, rel, fork, blk, target)
            .await?;
        crate::metrics::YB_STORE_OP_SECONDS
            .with_label_values(&["read_chain", "yb_thin_client"])
            .observe(started.elapsed().as_secs_f64());
        Ok(chain)
    }
    #[allow(clippy::too_many_arguments)]
    async fn read_chain_thin(
        &self,
        p: &ThinClientTables,
        branch_id: BranchId,
        sp: u32,
        db: u32,
        rel: u32,
        fork: i16,
        blk: u32,
        target: i64,
    ) -> Result<Vec<Arc<PageRow>>> {
        use yb_thin_client::{BindValue, CondOp};
        let pages = &p.pages;
        let read = page_chain_read(pages, &branch_id.to_bytes(), sp, db, rel, fork, blk)?
            .cond(pages, "lsn", CondOp::Le, BindValue::I64(target))?
            .limit(self.page_chain_read_limit);
        let key = Key::from_slice(&columns_to_key_bytes(sp, db, rel, fork, blk));
        let raw = fetch_chain_rows_thin(&p.client, pages, read)
            .await
            .with_context(|| {
                format!("thin-lane chain read for branch={branch_id} key={key} lsn<={target}")
            })?;

        assemble_page_chain(key, raw, "read_chain_thin")
    }

    /// One bound single-block chain read per entry of `blks`, all riding one
    /// `Perform`. Shared by the contiguous range ([`Self::scan_range`]) and
    /// the scattered key set ([`Self::scan_scattered_set`]), which differ only
    /// in how they choose the blocks.
    ///
    /// `read` names the caller and supplies both labels. The observation
    /// lives here rather than at the call sites so a caller cannot take this
    /// lane without metering it — the omission #1025 was filed for.
    #[allow(clippy::too_many_arguments)]
    async fn scan_one_fork_thin(
        &self,
        p: &ThinClientTables,
        branch_id: BranchId,
        sp: u32,
        db: u32,
        rel: u32,
        fork: i16,
        blks: &[u32],
        target: i64,
        read: OneForkRead,
    ) -> Result<HashMap<Key, Vec<Arc<PageRow>>>> {
        use yb_thin_client::{BindValue, CondOp, RangeRead};
        let started = std::time::Instant::now();
        let pages = &p.pages;

        // One read per block, for the reason `scan_slru_range_thin` is (#1042):
        // the anchor is per block, and the early stop halts the whole scan. The
        // reads ride one `Perform`, so the fan-out resolves at one read point.
        let branch_b = branch_id.to_bytes();
        let block_reads = blks
            .iter()
            .map(|&blk| -> Result<(u32, RangeRead)> {
                Ok((
                    blk,
                    page_chain_read(pages, &branch_b, sp, db, rel, fork, blk)?
                        .cond(pages, "lsn", CondOp::Le, BindValue::I64(target))?
                        .limit(self.page_chain_read_limit),
                ))
            })
            .collect::<Result<Vec<_>>>()?;

        let chains = run_batched_block_scan(
            &p.client,
            pages,
            &block_reads,
            ScanStop::Anchor,
            "page scan",
            |i| format!("rel={rel} fork={fork} blk={}", block_reads[i].0),
        )
        .await?;

        let mut by_key = HashMap::with_capacity(chains.len());
        for (&(blk, _), raw) in block_reads.iter().zip(chains) {
            let key = Key::from_slice(&columns_to_key_bytes(sp, db, rel, fork, blk));
            let chain = assemble_page_chain(key, raw, read.caller_label())?;
            if !chain.is_empty() {
                by_key.insert(key, chain);
            }
        }
        crate::metrics::YB_STORE_OP_SECONDS
            .with_label_values(&[read.op_label(), "yb_thin_client"])
            .observe(started.elapsed().as_secs_f64());
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
    /// tablet-local read.
    ///
    /// - **Single-key range** (`range.end == range.start.next()`) →
    ///   delegates to `read_chain`. This is the getpage hot path —
    ///   every compute getpage RPC ends up here, so the dispatch
    ///   matters. No `GROUP BY` / `LEFT JOIN` overhead.
    /// - **One-fork range** (start and end share spcnode/dbnode/relnode
    ///   /forknum, differ only in blknum) → [`Self::scan_one_fork_thin`]:
    ///   one hash-pinned thin-lane read per block, all riding one
    ///   `Perform`. Used by basebackup, image-layer creation, multi-block
    ///   vectored gets.
    /// - **Cross-fork range** (start and end differ in some pre-blknum
    ///   field) → the one YSQL arm left here: multi-column row
    ///   comparison, fanned out over every bucket because the bounds
    ///   cannot name one. Rare in practice; we accept the cost.
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

        // `field1` counts: the bail above pins it only for `range.start`, so an
        // end at `field1 == 1` reaches here, and `start.field6..end.field6` is
        // then not the key set. Classing it cross-fork keeps `requested_keys`
        // from naming the wrong keys; the cross-fork SQL ignores `field1` too,
        // so that shape is not read correctly either way.
        let one_fork = one_fork_range(&range);

        // Single-key fast path: every compute getpage RPC arrives as a
        // 1-key vectored read (`KeySpace::single(k..k.next())`). Detect
        // and dispatch to read_chain, which skips the multi-block
        // GROUP BY / LEFT JOIN entirely.
        if one_fork && spans_one_block(&range) {
            let mut key_bytes = vec![0u8; KEY_SIZE];
            range.start.write_to_byte_slice(&mut key_bytes);
            let chain = self.read_chain(branch_id, &key_bytes, target_lsn).await?;
            let mut out = HashMap::with_capacity(1);
            if !chain.is_empty() {
                out.insert(range.start, chain);
            }
            return Ok(out);
        }

        // `branch_id` is bound per call — same lineage reason as
        // `read_chain` (ancestor timelines hit this path).
        let target = target_lsn.0 as i64;

        // Per-block anchor: for each block, the newest base at or below
        // target_lsn, returning only rows from there onward. The thin arm
        // stops there via `ScanStop::Anchor`, the cross-fork arm via its CTE
        // plus `group_page_rows_by_key`. A block with no base below the
        // target yields its full chain.
        if one_fork {
            let sp = range.start.field2;
            let db = range.start.field3;
            let rel = range.start.field4;
            let fork = range.start.field5 as i16;
            let blks: Vec<u32> = (range.start.field6..range.end.field6).collect();
            let p = self.thinclient.current().await;
            return self
                .scan_one_fork_thin(
                    &p,
                    branch_id,
                    sp,
                    db,
                    rel,
                    fork,
                    &blks,
                    target,
                    OneForkRead::Range,
                )
                .await;
        }

        // Cross-fork range. Same shape, with row-comparison
        // predicates. Joining the CTE on the full (spcnode, dbnode,
        // relnode, forknum, blknum) tuple keeps anchors paired with
        // the right block when a range crosses forks.
        // The oid columns are `OID`; tokio-postgres binds only `u32` to that type.
        let sp_lo = range.start.field2;
        let db_lo = range.start.field3;
        let rel_lo = range.start.field4;
        let fork_lo = range.start.field5 as i16;
        let blk_lo = range.start.field6;
        let sp_hi = range.end.field2;
        let db_hi = range.end.field3;
        let rel_hi = range.end.field4;
        let fork_hi = range.end.field5 as i16;
        let blk_hi = range.end.field6;
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

        group_page_rows_by_key(acc, "scan_range")
    }

    /// Multi-key ("scattered") variant of [`Self::scan_range`]: fetch the
    /// chains for an arbitrary SET of same-fork relation-page keys in one
    /// batch instead of a round trip per block. All keys must share
    /// (spcnode, dbnode, relnode, forknum) — the caller buckets by fork.
    /// Per-block `will_init` anchor and trim semantics are `scan_range`'s,
    /// via the shared [`Self::scan_one_fork_thin`].
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
        let mut blknums: Vec<u32> = Vec::with_capacity(keys.len());
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
        // Distinct blknums only: a repeated key would issue that block's read
        // twice in the same `Perform`, and the second chain would merely
        // overwrite the first in the result map. Callers pass disjoint keys
        // today, but harden here rather than pay for the redundant read.
        blknums.sort_unstable();
        blknums.dedup();

        // `branch_id` is bound per call, never fixed — same lineage reason
        // as `read_chain`/`scan_range` (ancestor branches hit this path).
        let target = target_lsn.0 as i64;
        let p = self.thinclient.current().await;
        self.scan_one_fork_thin(
            &p,
            branch_id,
            sp,
            db,
            rel,
            fork,
            &blknums,
            target,
            OneForkRead::ScatteredSet,
        )
        .await
    }

    // ------------------- slru_pages ---------------------------------------

    pub async fn put_slru_pages(&self, branch_id: BranchId, rows: &[SlruPageRow]) -> Result<()> {
        use yb_thin_client::{BindValue, UpsertRow};
        let mut tally = PayloadTally::new("slru_pages");
        let branch_b = branch_id.to_bytes();
        self.upsert_batched(
            &SLRU_PAGES_LANE,
            branch_id,
            rows,
            |slru, _, row| {
                let payload = self.encode_payload(&row.payload, &mut tally);
                Ok(UpsertRow::new(
                    slru,
                    vec![
                        BindValue::Bytes(branch_b.clone()),
                        BindValue::I16(DEFAULT_VERSION),
                        BindValue::I16(slru_kind_to_i16(row.kind)),
                        BindValue::U32(row.segno),
                        BindValue::I16(slru_blknum(row.blknum)?),
                        BindValue::I64(row.lsn.0 as i64),
                        BindValue::Bool(row.will_init),
                    ],
                    &[
                        ("is_image", BindValue::Bool(row.is_image)),
                        ("payload", BindValue::Bytes(payload)),
                    ],
                )?)
            },
            |_| {},
        )
        .await?;
        tally.record();
        Ok(())
    }
    /// Per-block SLRU chain read: the same direct-RPC, anchor-stopped
    /// shape as `read_chain`, just keyed by `(kind, segno, blknum)`.
    pub async fn read_slru_chain(
        &self,
        branch_id: BranchId,
        kind: SlruKind,
        segno: u32,
        blknum: u32,
        target_lsn: Lsn,
    ) -> Result<Vec<SlruPageRow>> {
        let kind_i = slru_kind_to_i16(kind);
        let target = target_lsn.0 as i64;

        let p = self.thinclient.current().await;
        let started = std::time::Instant::now();
        let chain = self
            .read_slru_chain_thin(&p, branch_id, kind, kind_i, segno, blknum, target)
            .await?;
        crate::metrics::YB_STORE_OP_SECONDS
            .with_label_values(&["read_slru_chain", "yb_thin_client"])
            .observe(started.elapsed().as_secs_f64());
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
        segno: u32,
        blknum: u32,
        target: i64,
    ) -> Result<Vec<SlruPageRow>> {
        use yb_thin_client::{BindValue, CondOp};
        let slru = &p.slru_pages;
        let read = slru_segment_read(slru, &branch_id.to_bytes(), kind_i, segno)?
            .cond(
                slru,
                "blknum",
                CondOp::Eq,
                BindValue::I16(slru_blknum(blknum)?),
            )?
            .cond(slru, "lsn", CondOp::Le, BindValue::I64(target))?
            .limit(DIRECT_SLRU_CHAIN_PAGE_ROWS);
        let raw = fetch_chain_rows_thin(&p.client, slru, read)
            .await
            .with_context(|| {
                format!(
                    "thin-lane slru chain read for branch={branch_id} kind={kind:?} \
                     segno={segno} blk={blknum} lsn<={target}"
                )
            })?;

        assemble_slru_chain(kind, segno, blknum, raw, "read_slru_chain_thin")
    }

    #[allow(clippy::too_many_arguments)]
    async fn scan_slru_range_thin(
        &self,
        p: &ThinClientTables,
        branch_id: BranchId,
        kind: SlruKind,
        kind_i: i16,
        segno: u32,
        blknums: Range<u32>,
        target: i64,
        mode: SlruScan,
    ) -> Result<HashMap<u32, Vec<SlruPageRow>>> {
        use yb_thin_client::{BindValue, CondOp};
        let slru = &p.slru_pages;

        // One read per block: the anchor is per block and `read_scan` halts
        // the whole scan rather than skipping ahead, so a range-wide read
        // cannot stop without abandoning later blocks (#1042). They still ride
        // one `Perform`, so the fan-out resolves at one read point.
        let branch = branch_id.to_bytes();
        let block_reads = blknums
            .map(|blk| -> Result<(u32, yb_thin_client::RangeRead)> {
                Ok((
                    blk,
                    slru_segment_read(slru, &branch, kind_i, segno)?
                        .cond(
                            slru,
                            "blknum",
                            CondOp::Eq,
                            BindValue::I16(slru_blknum(blk)?),
                        )?
                        .cond(slru, "lsn", CondOp::Le, BindValue::I64(target))?
                        .limit(mode.page_rows()),
                ))
            })
            .collect::<Result<Vec<_>>>()?;

        let chains = run_batched_block_scan(
            &p.client,
            slru,
            &block_reads,
            mode.stop(),
            "slru scan",
            |i| format!("kind={kind:?} segno={segno} blk={}", block_reads[i].0),
        )
        .await?;

        let mut by_block = HashMap::with_capacity(chains.len());
        for (&(blk, _), raw) in block_reads.iter().zip(chains) {
            let chain = assemble_slru_chain(kind, segno, blk, raw, "scan_slru_range_thin")?;
            if !chain.is_empty() {
                by_block.insert(blk, chain);
            }
        }
        Ok(by_block)
    }

    /// Range scan over SLRU blocks in `range`. `range` must stay inside
    /// one `(kind, segno)` segment — that's how SLRU vectored reads are
    /// shaped today. Returns one entry per blknum, each chain
    /// LSN-descending and truncated at the latest will_init row, or cut to at
    /// most its newest `n` rows for [`SlruScan::NewestUpTo`].
    pub async fn scan_slru_range(
        &self,
        branch_id: BranchId,
        kind: SlruKind,
        segno: u32,
        blknum_range: Range<u32>,
        target_lsn: Lsn,
        mode: SlruScan,
    ) -> Result<HashMap<u32, Vec<SlruPageRow>>> {
        // Single-block fast path: delegate to read_slru_chain to skip
        // the multi-block JOIN. ToAnchor only — it reads the whole chain.
        if mode == SlruScan::ToAnchor && blknum_range.end == blknum_range.start.saturating_add(1) {
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
        let target = target_lsn.0 as i64;

        let p = self.thinclient.current().await;
        let started = std::time::Instant::now();
        let by_block = self
            .scan_slru_range_thin(
                &p,
                branch_id,
                kind,
                kind_i,
                segno,
                blknum_range,
                target,
                mode,
            )
            .await?;
        crate::metrics::YB_STORE_OP_SECONDS
            .with_label_values(&[mode.op_label(), "yb_thin_client"])
            .observe(started.elapsed().as_secs_f64());
        Ok(by_block)
    }

    // ------------------- meta ---------------------------------------------

    /// Upsert `rows` into `amp_storage.meta`.
    ///
    /// `key_values` are in PRIMARY KEY order — `((branch_id) HASH, version
    /// ASC, key ASC, lsn DESC)` — not declaration order; `payload` is the lone
    /// non-key column. Meta's hash key is `branch_id` alone, so unlike
    /// `pages`/`slru_pages` there is no bucket column to compute here.
    ///
    /// The write is an upsert: a row replayed by the ingest window's
    /// re-flush overwrites in place instead of failing on duplicate PK.
    pub async fn put_meta(&self, branch_id: BranchId, rows: &[MetaRow]) -> Result<()> {
        use yb_thin_client::{BindValue, UpsertRow};
        // Most meta payloads — rel-size sentinels are 4 bytes — fall under
        // `MIN_COMPRESS_BYTES` and stay raw; directory listings and aux files
        // are the ones that benefit.
        let mut tally = PayloadTally::new("meta");
        let branch_b = branch_id.to_bytes();
        self.upsert_batched(
            &META_LANE,
            branch_id,
            rows,
            |meta, i, row| {
                if row.key.len() != KEY_SIZE {
                    bail!(
                        "put_meta row {i}: key bytes wrong length ({})",
                        row.key.len()
                    );
                }
                let payload = self.encode_payload(&row.payload, &mut tally);
                Ok(UpsertRow::new(
                    meta,
                    vec![
                        BindValue::Bytes(branch_b.clone()),
                        BindValue::I16(DEFAULT_VERSION),
                        BindValue::Bytes(row.key.clone()),
                        BindValue::I64(row.lsn.0 as i64),
                    ],
                    &[("payload", BindValue::Bytes(payload))],
                )?)
            },
            |_| {},
        )
        .await?;
        tally.record();
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
        let p = self.thinclient.current().await;
        let started = std::time::Instant::now();
        let row = self.read_meta_thin(&p, branch_id, key, target_lsn).await?;
        crate::metrics::YB_STORE_OP_SECONDS
            .with_label_values(&["read_meta", "yb_thin_client"])
            .observe(started.elapsed().as_secs_f64());
        Ok(row)
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
            .cond(meta, "version", CondOp::Eq, BindValue::I16(DEFAULT_VERSION))?
            .cond(
                meta,
                "key",
                CondOp::Eq,
                BindValue::Bytes(Bytes::copy_from_slice(key)),
            )?
            .cond(meta, "lsn", CondOp::Le, BindValue::I64(target_lsn.0 as i64))?
            .targets(meta, &META_TARGETS)?
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
            .await
            .with_context(|| {
                format!(
                    "thin-lane meta read for branch={branch_id} key={} lsn<={target_lsn}",
                    hex::encode(key)
                )
            })?;
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
                 WHERE branch_id=$1 AND version = {DEFAULT_VERSION} \
                   AND key >= $2 AND key < $3 AND lsn <= $4 \
             ) \
             SELECT k.key, latest.lsn, latest.payload \
             FROM keys k \
             CROSS JOIN LATERAL ( \
                 SELECT lsn, payload FROM \"{schema}\".meta \
                 WHERE branch_id=$1 AND version = {DEFAULT_VERSION} \
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

    // ------------------- lineage-aware reads -------------------------------
    //
    // A branch's pre-branch-point rows live under the ANCESTOR's
    // branch_id. Each method below walks the lineage segments
    // nearest-first, capping the per-segment read LSN at
    // `min(target_lsn, seg.cap_lsn)`, and stops as soon as the chain
    // is anchored (page/SLRU; see `LineageSegment::shadowed_by`) or the
    // key is found (meta). The multi-block scans apply that per key.
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
            if seg.shadowed_by(&chain) {
                break;
            }
            let eff = std::cmp::min(target_lsn, seg.cap_lsn);
            let rows = self.read_chain(seg.branch_id, key, eff).await?;
            chain.extend(rows);
        }
        if lineage.len() > 1 {
            merge_page_chain(&mut chain);
        }
        Ok(chain)
    }

    /// Lineage-aware [`Self::scan_range`]. An ancestor segment is read only
    /// while some requested block is not yet anchored at or above its cap (see
    /// [`walk_lineage_scan`]); a range that crosses forks cannot enumerate its
    /// keys, so its ancestors are always read, since keys written only before
    /// the branch point are otherwise undiscoverable from the descendant's
    /// segment. Per-key chains are merged nearest-first, then
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

    /// Resolve one lineage segment's chains: `residual` keys if the walk has
    /// narrowed to some, else the whole `target`, a contiguous range or a
    /// scattered key set. The per-segment step of [`Self::scan_lineage`].
    ///
    /// `residual` is owned rather than borrowed because this is the one future
    /// type [`walk_lineage_scan`]'s `scan` returns, so it cannot hold a slice
    /// borrowed from the walk.
    async fn scan_lineage_segment(
        &self,
        target: &LineageScanTarget<'_>,
        branch_id: BranchId,
        lsn: Lsn,
        residual: Option<Vec<Key>>,
    ) -> Result<HashMap<Key, Vec<Arc<PageRow>>>> {
        if let Some(keys) = residual.as_deref() {
            return self.scan_scattered_set(branch_id, keys, lsn).await;
        }
        match target {
            LineageScanTarget::Range(range) => self.scan_range(branch_id, range.clone(), lsn).await,
            LineageScanTarget::Set(keys) => self.scan_scattered_set(branch_id, keys, lsn).await,
        }
    }

    /// Shared body of [`Self::scan_range_lineage`] and
    /// [`Self::scan_scattered_set_lineage`]: [`walk_lineage_scan`] over
    /// `target`'s keys. A narrowed ancestor is read as a scattered set of the
    /// unanchored blocks, all on `target`'s one fork, since a cross-fork range
    /// never enumerates.
    async fn scan_lineage(
        &self,
        label: &str,
        lineage: &[LineageSegment],
        target_lsn: Lsn,
        target: LineageScanTarget<'_>,
    ) -> Result<HashMap<Key, Vec<Arc<PageRow>>>> {
        let target = &target;
        walk_lineage_scan(
            label,
            lineage,
            target_lsn,
            target.requested_keys(),
            move |branch_id, lsn, residual: Option<&[Key]>| {
                record_segment_scan(branch_id, residual.map(<[Key]>::len));
                self.scan_lineage_segment(target, branch_id, lsn, residual.map(<[Key]>::to_vec))
            },
            merge_page_chain,
        )
        .await
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
            if seg.shadowed_by(&chain) {
                break;
            }
            let eff = std::cmp::min(target_lsn, seg.cap_lsn);
            let rows = self
                .read_slru_chain(seg.branch_id, kind, segno, blknum, eff)
                .await?;
            chain.extend(rows);
        }
        if lineage.len() > 1 {
            merge_slru_chain(&mut chain, SlruScan::ToAnchor);
        }
        Ok(chain)
    }

    /// Lineage-aware [`Self::scan_slru_range`]. Ancestor rows all lie below
    /// the branch point, so after the merge the tip's rows lead and a
    /// [`SlruScan::NewestUpTo`] cut keeps the newest overall. An ancestor is
    /// read only for the blocks it is not yet shadowed for: `merge_slru_chain`
    /// trims at the anchor before the newest-`n` cut, so a shadowed ancestor's
    /// rows would be dropped in either mode.
    pub async fn scan_slru_range_lineage(
        &self,
        lineage: &[LineageSegment],
        kind: SlruKind,
        segno: u32,
        blknum_range: Range<u32>,
        target_lsn: Lsn,
        mode: SlruScan,
    ) -> Result<HashMap<u32, Vec<SlruPageRow>>> {
        // An SLRU range is always its own key set, so there is no
        // read-everything case here.
        walk_lineage_scan(
            "scan_slru_range_lineage",
            lineage,
            target_lsn,
            Some(blknum_range.clone()),
            move |branch_id, lsn, residual: Option<&[u32]>| {
                // No scattered SLRU read, so an ancestor is narrowed to the
                // span of the blocks still unanchored rather than to the set.
                let range = residual
                    .and_then(blknum_span)
                    .unwrap_or_else(|| blknum_range.clone());
                // The span, not the residual: a gap between two unanchored
                // blocks is read too, and the tests hold that to account.
                record_segment_scan(branch_id, residual.map(|_| range.len()));
                self.scan_slru_range(branch_id, kind, segno, range, lsn, mode)
            },
            |chain| merge_slru_chain(chain, mode),
        )
        .await
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

/// A write error the leader returned because the write's lease deadline had
/// passed — which `WriteFence::write_fenced` resubmits under a renewed token or
/// exits on — as opposed to a failure worth retrying.
pub(crate) fn server_fenced(e: &anyhow::Error) -> bool {
    e.downcast_ref::<yb_thin_client::YbError>()
        .is_some_and(yb_thin_client::YbError::is_fenced)
}

/// Invariants that every chain returned by `read_chain` / `scan_range` /
/// `read_slru_chain` / `scan_slru_range` must satisfy *for a single key*
/// when iterated in the SQL's `lsn DESC, will_init DESC` order, i.e.
/// in the order rows are pushed into the chain `Vec`.
///
/// 1. `lsn` is non-increasing as we step through the slice.
/// 2. At a tied LSN, a base (`will_init == true`) MUST come before a
///    non-base, so the "break at first will_init=true" trim drops the
///    same-LSN non-base rather than double-applying it.
///
/// Violations indicate either a missing `ORDER BY ... will_init DESC`
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
fn assert_chain_step(chain: &[(Lsn, bool /* is_image */, bool /* will_init */)], where_: &str) {
    let n = chain.len();
    if n < 2 {
        return;
    }
    let (prev_lsn, _, prev_will_init) = chain[n - 2];
    let (lsn, _, will_init) = chain[n - 1];
    if let Some(violation) = chain_step_error((prev_lsn, prev_will_init), (lsn, will_init)) {
        panic!("{where_}: chain row {}: {violation}", n - 1);
    }
}

/// The `lsn DESC, will_init DESC` rule `(lsn, will_init)` breaks by following
/// `(prev_lsn, prev_will_init)`, if any.
fn chain_step_error(
    (prev_lsn, prev_will_init): (Lsn, bool),
    (lsn, will_init): (Lsn, bool),
) -> Option<String> {
    if lsn > prev_lsn {
        return Some(format!("lsn {lsn} > prev lsn {prev_lsn} (must be DESC)"));
    }
    if lsn == prev_lsn && !prev_will_init && will_init {
        return Some(format!(
            "base at lsn {lsn} after a non-base at the same lsn (need ORDER BY ..., will_init DESC tiebreaker)"
        ));
    }
    None
}

#[cfg(test)]
mod chain_collector_tests {
    //! [`ChainCollector`] consumes ONE block's chain in PK order and must stop
    //! buffering at the anchor.
    use super::*;

    fn collect(blk: i64, rows: &[(i64, bool)]) -> Vec<i64> {
        let mut c = ChainCollector::new(blk);
        for &(lsn, will_init) in rows {
            c.push(blk, will_init, lsn).unwrap();
        }
        c.rows
    }

    /// Flips on the anchor row itself, so a round ending on it pages no further.
    #[test]
    fn complete_tracks_the_anchor() {
        let mut c = ChainCollector::new(1);
        assert!(!c.complete(), "nothing collected yet");
        c.push(1, false, 90).unwrap();
        assert!(!c.complete(), "above the anchor");
        c.push(1, true, 70).unwrap();
        assert!(c.complete(), "the anchor ends the chain");
    }

    #[test]
    fn a_chain_with_no_anchor_never_completes() {
        let mut c = ChainCollector::new(3);
        c.push(3, false, 30).unwrap();
        c.push(3, false, 20).unwrap();
        assert!(!c.complete());
    }

    #[test]
    fn drops_everything_after_the_anchor() {
        assert_eq!(
            collect(1, &[(90, false), (70, true), (60, false), (50, true)]),
            vec![90, 70]
        );
        // The delta the materialiser stamped its image over sorts after it.
        assert_eq!(
            collect(2, &[(80, true), (80, false), (40, false)]),
            vec![80]
        );
    }

    /// The op binds one block, so a row for another block means the server
    /// answered a different question — filing it here would serve that block's
    /// pages as this one's.
    #[test]
    fn rejects_a_row_for_another_block() {
        let mut c = ChainCollector::new(7);
        c.push(7, false, 90).unwrap();
        let err = c.push(8, false, 80).unwrap_err().to_string();
        assert!(err.contains("bound blk=7"), "{err}");
        assert!(err.contains("blk=8"), "{err}");
    }
}

#[cfg(test)]
mod column_layout_tests {
    //! The column positions rows are read back by must name the columns each read fetches.
    use super::*;

    #[test]
    fn positions_name_their_columns() {
        assert_eq!(CHAIN_TARGETS[CHAIN_LSN], "lsn");
        assert_eq!(CHAIN_TARGETS[CHAIN_IS_IMAGE], "is_image");
        assert_eq!(CHAIN_TARGETS[CHAIN_WILL_INIT], "will_init");
        assert_eq!(CHAIN_TARGETS[CHAIN_PAYLOAD], "payload");
        assert_eq!(CHAIN_TARGETS[CHAIN_BLKNUM], "blknum");
        assert_eq!(META_TARGETS[META_LSN], "lsn");
        assert_eq!(META_TARGETS[META_PAYLOAD], "payload");
    }
}

#[cfg(test)]
mod assemble_chain_tests {
    //! `assemble_page_chain` / `assemble_slru_chain` reject a chain whose decoded rows are out of
    //! `lsn DESC, will_init DESC` order.
    use super::test_support::page_key;
    use super::*;

    /// `(lsn, is_image, will_init)` with a raw-codec payload, laid out as [`CHAIN_TARGETS`].
    struct Cells(i64, bool, bool);

    impl RowCells for Cells {
        fn cell_i64(&self, i: usize) -> Result<i64> {
            anyhow::ensure!(i == CHAIN_LSN, "cell {i} is not the lsn");
            Ok(self.0)
        }
        fn cell_bool(&self, i: usize) -> Result<bool> {
            match i {
                CHAIN_IS_IMAGE => Ok(self.1),
                CHAIN_WILL_INIT => Ok(self.2),
                _ => anyhow::bail!("cell {i} is not a bool"),
            }
        }
        fn cell_bytes(&self, i: usize) -> Result<Bytes> {
            anyhow::ensure!(i == CHAIN_PAYLOAD, "cell {i} is not the payload");
            Ok(Bytes::from_static(b"\x00p"))
        }
    }

    fn page(rows: Vec<Cells>) -> Result<Vec<Arc<PageRow>>> {
        let key = Key::from_slice(&page_key(1663, 5, 16384, 0, 1));
        assemble_page_chain(key, rows, "test")
    }

    fn slru(rows: Vec<Cells>) -> Result<Vec<SlruPageRow>> {
        assemble_slru_chain(SlruKind::Clog, 0, 1, rows, "test")
    }

    fn error<T: std::fmt::Debug>(result: Result<T>) -> String {
        format!("{:#}", result.unwrap_err())
    }

    #[test]
    fn accepts_pk_order() {
        let rows = || vec![Cells(90, false, false), Cells(70, true, true)];
        assert_eq!(page(rows()).unwrap().len(), 2);
        assert_eq!(slru(rows()).unwrap().len(), 2);
    }

    #[test]
    fn rejects_ascending_lsn() {
        let rows = || vec![Cells(70, false, false), Cells(90, true, true)];
        for err in [error(page(rows())), error(slru(rows()))] {
            assert!(err.contains("must be DESC"), "{err}");
        }
    }

    #[test]
    fn rejects_base_after_a_tied_delta() {
        let rows = || vec![Cells(70, false, false), Cells(70, true, true)];
        for err in [error(page(rows())), error(slru(rows()))] {
            assert!(err.contains("after a non-base at the same lsn"), "{err}");
        }
    }
}

#[cfg(test)]
mod chain_order_tests {
    //! Unit tests for [`assert_chain_step`], the per-row chain-order guard
    //! run by the cross-fork YSQL scan. [`check`] mirrors the production usage — push
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

    /// The post-fix tied-LSN order. At LSN 100 the base is FIRST,
    /// then a same-LSN delta would appear (but the trim breaks
    /// on the base before reaching it). This represents what the
    /// SQL with `ORDER BY p.lsn DESC, p.will_init DESC` produces.
    #[test]
    fn base_first_at_tied_lsn_is_accepted() {
        check(
            &[
                row(200, false, false),
                row(100, true, true),
                row(100, false, false),
            ],
            "test_base_first_tie",
        );
    }

    /// Two plain deltas at the same LSN, then an image at a lower LSN.
    /// Same-LSN non-bases are legal; only base-AFTER-non-base at a tied
    /// LSN is forbidden.
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

    /// Two bases at one LSN: pathological but legal (only
    /// base-after-non-base is forbidden). `will_init` trails the PK, so
    /// these are one row in the store and only a merge can pair them.
    #[test]
    fn base_base_at_same_lsn_is_accepted() {
        check(
            &[row(100, true, true), row(100, true, true)],
            "test_base_base_tie",
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

    /// A base AFTER a non-base at the same LSN is the pre-fix bug: the
    /// chain handed to walredo would contain a same-LSN delta whose effect
    /// is already baked into the base, a double-apply.
    #[test]
    #[should_panic(expected = "after a non-base at the same lsn")]
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

    /// Same violation with an init record as the base, which an
    /// `is_image` check cannot see: both rows are `is_image=false`.
    #[test]
    #[should_panic(expected = "after a non-base at the same lsn")]
    fn init_record_after_delta_at_tied_lsn_panics() {
        check(
            &[
                row(200, false, false),
                row(100, false, false),
                row(100, false, true),
            ],
            "test_init_after_delta_tie",
        );
    }

    /// Equal LSNs are fine; only an increase panics (`lsn > prev_lsn`, not
    /// `>=`). Two same-LSN deltas trip neither the DESC nor the base check.
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

    /// Every write path must bind the `encode_payload` output, never a
    /// row's plaintext `payload`.
    ///
    /// This is a source-level guard because the failure it catches is
    /// invisible to the type system and to every other test here. A new
    /// `put_*` variant is *new code* — it conflicts with nothing, compiles
    /// fine, and silently writes untagged plaintext that the read path then
    /// rejects. That is exactly how the `yb_thin_client` writes shipped
    /// broken: clean auto-merge, clippy clean, every unit test green, and the
    /// cluster failed to start because basebackup could not decode its own
    /// pages. So: assert on the source.
    #[test]
    fn every_write_lane_binds_encoded_payloads() {
        let src = include_str!("page_store.rs");

        // Needles are assembled from fragments so this test's own source
        // does not match them — otherwise the guard flags itself.
        let bind_prefix = format!("(\"pay{}\", BindValue::Bytes(", "load");
        let row_payload = format!("row.pay{}", "load");
        let payload_clone = format!(".pay{}.clone()", "load");

        // Writes bind through `BindValue::Bytes(..)`. Any occurrence naming a
        // row's payload directly is a write that skipped the codec.
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
            "these write paths bind a row's PLAINTEXT payload instead of the \
             encoded buffer from `encode_payload` — pages written this way \
             have no codec tag and `decode_payload` will reject them on \
             read:\n{}",
            raw_binds.join("\n")
        );

        // Sanity: the guard is only meaningful if it can see the binds it is
        // policing. If the bind syntax is refactored, this fails loudly
        // rather than silently passing on zero matches.
        // Three: the `pages`, `slru_pages` and `meta` writes — all three bind
        // `payload` by name through this prefix.
        let encoded_binds = src.matches(&bind_prefix).count();
        assert!(
            encoded_binds >= 3,
            "expected at least 3 payload binds to police, saw \
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

    #[test]
    fn page_chain_read_limit_covers_a_full_chain_and_its_base() {
        assert_eq!(page_chain_read_limit(20), 21);
        assert_eq!(page_chain_read_limit(0), 32);
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
    // `OID` columns decode only as `u32`.
    let sp: u32 = r.try_get(0)?;
    let db: u32 = r.try_get(1)?;
    let rel: u32 = r.try_get(2)?;
    let fork: i16 = r.try_get(3)?;
    let blk: u32 = r.try_get(4)?;
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
    use super::test_support::*;
    use super::*;

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

    /// The page write carries the lease to the leader. Rows submitted under a
    /// deadline that has passed are refused and none of them land, across the
    /// buckets — so tablets — one flush spreads them over; the refusal reads as
    /// the fence, which `write_fenced_each` resubmits or exits on; and the same
    /// rows under a live deadline land.
    ///
    /// The rows go through `upsert_chunk`, the write `upsert_batched` fences,
    /// with the deadline passed in rather than read from the process's fence,
    /// so the write is reached with its lease already over: the stalled writer
    /// the local check cannot stop. Going through `put_pages` itself would exit
    /// the test on the refusal, as a lapsed lease should.
    #[tokio::test]
    async fn a_page_write_past_its_deadline_is_refused_and_none_land() {
        let (store, schema) = make_store("fence").await;
        let branch = rand_branch();
        // Far-apart blocks, so the rows hash to different buckets and the one
        // `Perform` spans tablets: every leader has to refuse its share.
        let keys: Vec<Bytes> = (0..8u32)
            .map(|i| page_key(1663, 5, 16384, 0, i * 100_003))
            .collect();
        let rows: Vec<Arc<PageRow>> = keys
            .iter()
            .map(|k| page_row(k.clone(), 10, true, true, b"img"))
            .collect();
        let p = store.thinclient.current().await;
        let now_us = std::time::SystemTime::now()
            .duration_since(std::time::UNIX_EPOCH)
            .unwrap()
            .as_micros() as u64;
        let minute_us = 60_000_000;

        let (mut tally, branch_b) = (PayloadTally::new("pages"), branch.to_bytes());
        let upserts = batched_upsert::build_upserts(&p, &PAGES_LANE, &rows, |pages, i, row| {
            let payload = store.encode_payload(&row.payload, &mut tally);
            PageStore::page_upsert(pages, &branch_b, i, row, payload)
        })
        .unwrap();
        let write_under = |deadline_us: u64| {
            batched_upsert::upsert_chunk(
                &p.client,
                &PAGES_LANE,
                branch,
                &upserts,
                yb_thin_client::WriteDeadline::EpochMicros(deadline_us),
            )
        };

        let err = write_under(now_us - minute_us)
            .await
            .expect_err("a page write under a lapsed deadline must be refused");
        assert!(
            server_fenced(&err),
            "the refusal must read as the fence, not a retryable failure: {err:#}"
        );
        for k in &keys {
            assert!(
                store
                    .read_chain(branch, k, Lsn(10_000))
                    .await
                    .unwrap()
                    .is_empty(),
                "a refused page write must not land"
            );
        }

        write_under(now_us + minute_us)
            .await
            .expect("a page write under a live deadline lands");
        for k in &keys {
            assert_eq!(
                store
                    .read_chain(branch, k, Lsn(10_000))
                    .await
                    .unwrap()
                    .len(),
                1
            );
        }
        drop_schema(&store, &schema).await;
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
    /// Now the PK's trailing `will_init DESC` puts the image at the
    /// tied LSN first; the trim breaks on it and the same-LSN delta
    /// is dropped. Walredo sees `[higher_delta, tied_image]`, applies
    /// the higher delta once to the post-tied-record image, and
    /// produces the right page.
    ///
    /// Note: we intentionally insert the rows in the worst-case
    /// "delta first" insertion order so we don't accidentally test
    /// the trivial case where the storage layer happens to return
    /// images first regardless of key order.
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

    /// One read per block derives the bucket per block, so a range straddling a
    /// 2MB stretch boundary must still find every block, and each block must
    /// trim at its OWN anchor rather than the range's shallowest.
    ///
    /// Each block carries 40 rows of pre-anchor history,
    /// so a stop predicate that fired before the anchor would return a short
    /// chain here. It does NOT distinguish stopping from reading on and
    /// discarding — that is a fetch-side property with no API-visible effect.
    #[tokio::test]
    async fn scan_range_spans_stretch_boundary_and_trims_at_each_block_anchor() {
        let (store, schema) = make_store("scan_stretch").await;
        let branch = rand_branch();
        let rel = 16384u32;
        // 250..262 straddles blknum 256, so the blocks land in two buckets.
        let lo = 250u32;
        let hi = 262u32;
        assert_ne!(
            page_bucket_id(rel, lo),
            page_bucket_id(rel, hi - 1),
            "range must straddle a stretch boundary for this test to mean anything"
        );

        // Per block: 40 shadowed rows below the anchor, the anchor image,
        // then blk-lo+1 deltas above it — a different depth per block.
        let mut rows = Vec::new();
        for blk in lo..hi {
            let k = page_key(1663, 5, rel, 0, blk);
            rows.push(page_row(k.clone(), 2, true, true, b"shadowed-img"));
            for d in 0..40u64 {
                rows.push(page_row(k.clone(), 4 + d * 2, false, false, b"shadowed-d"));
            }
            rows.push(page_row(k.clone(), 1000, true, true, b"img"));
            for d in 0..=(blk - lo) {
                rows.push(page_row(k.clone(), 1010 + d as u64, false, false, b"d"));
            }
        }
        store.put_pages(branch, &rows).await.unwrap();

        let range = Key::from_slice(&page_key(1663, 5, rel, 0, lo))
            ..Key::from_slice(&page_key(1663, 5, rel, 0, hi));
        let got = store.scan_range(branch, range, Lsn(100000)).await.unwrap();
        assert_eq!(got.len(), (hi - lo) as usize);
        for blk in lo..hi {
            let chain = &got[&Key::from_slice(&page_key(1663, 5, rel, 0, blk))];
            assert_eq!(
                chain.len(),
                (blk - lo) as usize + 2,
                "blk={blk} must return its own deltas plus its anchor image, \
                 and nothing shadowed by that anchor"
            );
            let anchor = chain.last().unwrap();
            assert!(anchor.will_init, "blk={blk} ends at its anchor");
            assert_eq!(
                anchor.lsn,
                Lsn(1000),
                "blk={blk} anchors on the newer image"
            );
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

    /// Same again for put_slru_pages: two full chunks and part of a third.
    #[tokio::test]
    async fn put_slru_pages_chunks_large_batches() {
        let (store, schema) = make_store("chunk_slru").await;
        let branch = rand_branch();
        let n: u32 = 1100;
        let rows: Vec<SlruPageRow> = (0..n)
            .map(|blk| clog_row(blk, 10, true, true, format!("s{blk}").as_bytes()))
            .collect();
        store.put_slru_pages(branch, &rows).await.unwrap();

        for blk in [0u32, n / 2, n - 1] {
            let chain = store
                .read_slru_chain(branch, SlruKind::Clog, 1, blk, Lsn(100))
                .await
                .unwrap();
            assert_eq!(chain.len(), 1, "blk {blk}");
            assert_eq!(chain[0].payload.as_ref(), format!("s{blk}").as_bytes());
        }
        drop_schema(&store, &schema).await;
    }

    /// Every row of a batch is built before its first chunk is sent, so a bad
    /// row in a later chunk fails the call with nothing written. The error
    /// names the row by its index in the batch, not in its chunk.
    #[tokio::test]
    async fn put_meta_bad_row_fails_before_any_chunk_lands() {
        let (store, schema) = make_store("meta_bad_row").await;
        let branch = rand_branch();
        let key = |i: u32| {
            let mut buf = vec![0u8; KEY_SIZE];
            buf[0] = 0x60;
            buf[14..18].copy_from_slice(&i.to_be_bytes());
            Bytes::from(buf)
        };
        // Past the first 2000-row chunk.
        let mut rows: Vec<MetaRow> = (0..2500)
            .map(|i| MetaRow {
                key: key(i),
                lsn: Lsn(10),
                payload: Bytes::from_static(b"v"),
            })
            .collect();
        rows[2100].key = Bytes::from_static(b"short");

        let err = store
            .put_meta(branch, &rows)
            .await
            .expect_err("a bad key must fail the write");
        assert!(format!("{err:#}").contains("put_meta row 2100:"), "{err:#}");
        for i in [0, 1999, 2000] {
            assert!(
                store
                    .read_meta(branch, &key(i), Lsn(100))
                    .await
                    .unwrap()
                    .is_none(),
                "row {i} landed although the batch had a bad row"
            );
        }
        drop_schema(&store, &schema).await;
    }

    /// A write publishes its payload tally under its table once it lands.
    #[tokio::test]
    async fn put_meta_publishes_its_payload_bytes() {
        let (store, schema) = make_store("meta_tally").await;
        let branch = rand_branch();
        let logical = || {
            crate::metrics::YB_PAYLOAD_LOGICAL_BYTES
                .with_label_values(&["meta"])
                .get()
        };
        let rows: Vec<MetaRow> = (0..3u32)
            .map(|i| {
                let mut buf = vec![0u8; KEY_SIZE];
                buf[0] = 0x60;
                buf[14..18].copy_from_slice(&i.to_be_bytes());
                MetaRow {
                    key: Bytes::from(buf),
                    lsn: Lsn(10),
                    payload: Bytes::from(vec![7u8; 100]),
                }
            })
            .collect();

        let before = logical();
        store.put_meta(branch, &rows).await.unwrap();
        // At least: other tests write meta through the same counter.
        assert!(logical() - before >= 300, "{} -> {}", before, logical());
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

    /// Three-segment lineage: `child` (MAX) off `parent` at `cap_parent`, which
    /// is itself off `grand` at `cap_grand` (caps are non-increasing).
    fn lineage3(
        child: BranchId,
        parent: BranchId,
        grand: BranchId,
        cap_parent: u64,
        cap_grand: u64,
    ) -> Vec<LineageSegment> {
        assert!(cap_grand <= cap_parent);
        let mut segs = lineage2(child, parent, cap_parent);
        segs.push(LineageSegment {
            branch_id: grand,
            cap_lsn: Lsn(cap_grand),
        });
        segs
    }

    /// The one relation and fork these tests key off.
    fn blk_key(blknum: u32) -> Key {
        Key::from_slice(&page_key(1663, 5, 16384, 0, blknum))
    }

    fn blk_range(lo: u32, hi: u32) -> Range<Key> {
        blk_key(lo)..blk_key(hi)
    }

    /// One relation-page row on the fork [`blk_key`] builds keys in.
    fn blk_row(blknum: u32, lsn: u64, is_image: bool, will_init: bool, p: &[u8]) -> Arc<PageRow> {
        page_row(
            page_key(1663, 5, 16384, 0, blknum),
            lsn,
            is_image,
            will_init,
            p,
        )
    }

    /// One `pg_xact` row in segment 1, the SLRU these tests use throughout.
    fn clog_row(
        blknum: u32,
        lsn: u64,
        is_image: bool,
        will_init: bool,
        payload: &[u8],
    ) -> SlruPageRow {
        SlruPageRow {
            kind: SlruKind::Clog,
            segno: 1,
            blknum,
            lsn: Lsn(lsn),
            is_image,
            will_init,
            payload: Bytes::copy_from_slice(payload),
        }
    }

    /// LSNs of a chain, newest first. Generic over [`AnchorRow`], so it serves
    /// rel-page and SLRU chains alike.
    fn lsns<R: AnchorRow>(chain: &[R]) -> Vec<u64> {
        chain.iter().map(|r| r.lsn().0).collect()
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
        assert_eq!(lsns(&chain), vec![40, 30, 20, 10]);
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
            lsns(&chain),
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
            lsns(&chain),
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
            lsns(&chain),
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
        store
            .put_pages(
                anc,
                &[
                    blk_row(1, 10, true, true, b"a-img"),
                    blk_row(2, 10, true, true, b"s-img"),
                ],
            )
            .await
            .unwrap();
        store
            .put_pages(child, &[blk_row(2, 30, false, false, b"s-child")])
            .await
            .unwrap();

        let out = store
            .scan_range_lineage(&lineage2(child, anc, 25), blk_range(0, 10), Lsn(100))
            .await
            .unwrap();
        assert_eq!(out.len(), 2);
        assert_eq!(
            lsns(&out[&blk_key(1)]),
            vec![10],
            "ancestor-only key must be discovered"
        );
        assert_eq!(
            lsns(&out[&blk_key(2)]),
            vec![30, 10],
            "shared key blends child delta over ancestor image"
        );
        drop_schema(&store, &schema).await;
    }

    /// Core correctness of the batched-reads change: `scan_scattered_set` on a
    /// set of non-contiguous same-fork keys must return byte-identical chains
    /// to reading each key on its own (same image anchor, same `will_init`
    /// trim, same order). Folding the keys into one batch is purely an
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

        let k_absent = blk_key(999);
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
        let cross_fork = vec![blk_key(1), Key::from_slice(&page_key(1663, 5, 16384, 1, 1))];
        assert!(
            store
                .scan_scattered_set(br, &cross_fork, target)
                .await
                .is_err(),
            "keys spanning forks must error"
        );
        drop_schema(&store, &schema).await;
    }

    /// The anchor subquery has to find the *nearest* base. Anchoring on
    /// `is_image` instead starts the LATERAL below any init record above the
    /// newest image and ships the versions between, which
    /// [`group_page_rows_by_key`] then drops. Only the raw row count shows
    /// it, so this test counts rows rather than chain entries (#1171).
    ///
    /// Driven through the cross-fork builder because it is the YSQL reader
    /// that outlives #1173, which moves the scattered set to the thin lane.
    #[tokio::test]
    async fn cross_fork_scan_anchors_on_the_nearest_base() {
        let (store, schema) = make_store("cross_fork_anchor").await;
        let br = rand_branch();
        let k = page_key(1663, 5, 16384, 0, 3);
        store
            .put_pages(
                br,
                &[
                    page_row(k.clone(), 10, true, true, b"img"),
                    page_row(k.clone(), 20, false, false, b"d20"),
                    page_row(k.clone(), 30, false, false, b"d30"),
                    // A base that is not an image.
                    page_row(k.clone(), 40, false, true, b"init"),
                    page_row(k.clone(), 50, false, false, b"d50"),
                    page_row(k.clone(), 60, false, false, b"d60"),
                ],
            )
            .await
            .unwrap();

        let key = Key::from_slice(&k);
        let (sp, db, rel, fork, blk) = key_columns(&key).unwrap();
        let bucket = page_bucket_id(rel, blk);
        let (blk_hi, target) = (blk + 1, 100i64);
        let raw = store
            .ysql_client
            .query(
                &build_scan_cross_fork_sql(&schema),
                &[
                    &br, &sp, &db, &rel, &fork, &blk, &sp, &db, &rel, &fork, &blk_hi, &target,
                    &bucket,
                ],
            )
            .await
            .expect("raw cross-fork scan");
        let raw_lsns: Vec<i64> = raw.iter().map(|r| r.get::<_, i64>(5)).collect();
        assert_eq!(
            raw_lsns,
            vec![60, 50, 40],
            "the LATERAL must start at the newest base, the init record at 40, \
             not the image at 10 below it"
        );

        // The served chain is unchanged. The range spans forks so this goes
        // through the cross-fork builder; `key..key.next()` would take
        // `scan_range`'s single-key fast path to `read_chain` instead.
        let fork_end = Key::from_slice(&page_key(1663, 5, 16384, 1, 0));
        let chain = store
            .scan_range(br, key..fork_end, Lsn(target as u64))
            .await
            .unwrap();
        assert_eq!(lsns(&chain[&key]), vec![60, 50, 40],);
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
        // Non-adjacent blocks, so the set is scattered rather than a run.
        store
            .put_pages(
                anc,
                &[
                    blk_row(1, 10, true, true, b"a-img"),
                    blk_row(8, 10, true, true, b"s-img"),
                ],
            )
            .await
            .unwrap();
        store
            .put_pages(child, &[blk_row(8, 30, false, false, b"s-child")])
            .await
            .unwrap();

        let out = store
            .scan_scattered_set_lineage(
                &lineage2(child, anc, 25),
                &[blk_key(1), blk_key(8)],
                Lsn(100),
            )
            .await
            .unwrap();
        assert_eq!(out.len(), 2);
        assert_eq!(
            lsns(&out[&blk_key(1)]),
            vec![10],
            "ancestor-only scattered key must be discovered"
        );
        assert_eq!(
            lsns(&out[&blk_key(8)]),
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
        store
            .put_slru_pages(anc, &[clog_row(0, 10, true, true, b"anc-img")])
            .await
            .unwrap();
        store
            .put_slru_pages(child, &[clog_row(0, 30, false, false, b"child-delta")])
            .await
            .unwrap();

        let lineage = lineage2(child, anc, 25);
        let chain = store
            .read_slru_chain_lineage(&lineage, SlruKind::Clog, 1, 0, Lsn(100))
            .await
            .unwrap();
        assert_eq!(lsns(&chain), vec![30, 10]);

        // And the range-scan variant sees the ancestor-only block.
        let scanned = store
            .scan_slru_range_lineage(
                &lineage,
                SlruKind::Clog,
                1,
                0..4,
                Lsn(100),
                SlruScan::ToAnchor,
            )
            .await
            .unwrap();
        assert_eq!(lsns(&scanned[&0]), vec![30, 10]);
        drop_schema(&store, &schema).await;
    }

    /// `SlruScan::NewestUpTo(n)`: at most the newest `n` rows per block, the
    /// anchor trim still applied; `ToAnchor` on the same data is unchanged.
    #[tokio::test]
    async fn scan_slru_newest_caps_each_block() {
        let (store, schema) = make_store("slru_newest").await;
        let branch = rand_branch();
        // blk 0: an image at 10 under six deltas; blk 1: the image is the
        // newest row; blk 2: nothing.
        store
            .put_slru_pages(
                branch,
                &[
                    clog_row(0, 10, true, true, b"b0-img"),
                    clog_row(0, 20, false, false, b"b0-d20"),
                    clog_row(0, 30, false, false, b"b0-d30"),
                    clog_row(0, 40, false, false, b"b0-d40"),
                    clog_row(0, 50, false, false, b"b0-d50"),
                    clog_row(0, 60, false, false, b"b0-d60"),
                    clog_row(0, 70, false, false, b"b0-d70"),
                    clog_row(1, 40, false, false, b"b1-d40"),
                    clog_row(1, 50, true, true, b"b1-img"),
                ],
            )
            .await
            .unwrap();
        let newest = store
            .scan_slru_range(
                branch,
                SlruKind::Clog,
                1,
                0..3,
                Lsn(1000),
                SlruScan::NewestUpTo(3),
            )
            .await
            .unwrap();
        assert_eq!(lsns(&newest[&0]), vec![70, 60, 50]);
        assert_eq!(lsns(&newest[&1]), vec![50], "anchor trim still applies");
        assert!(!newest.contains_key(&2));

        let capped = store
            .scan_slru_range(
                branch,
                SlruKind::Clog,
                1,
                0..1,
                Lsn(45),
                SlruScan::NewestUpTo(2),
            )
            .await
            .unwrap();
        assert_eq!(lsns(&capped[&0]), vec![40, 30], "target bound then cap");

        let chains = store
            .scan_slru_range(
                branch,
                SlruKind::Clog,
                1,
                0..3,
                Lsn(1000),
                SlruScan::ToAnchor,
            )
            .await
            .unwrap();
        assert_eq!(lsns(&chains[&0]), vec![70, 60, 50, 40, 30, 20, 10]);
        assert_eq!(lsns(&chains[&1]), vec![50]);
        drop_schema(&store, &schema).await;
    }

    /// Lineage under `NewestUpTo(n)`: the tip's rows lead the ancestor's and the
    /// cap applies to the merged chain, not per segment.
    #[tokio::test]
    async fn scan_slru_newest_lineage_tip_leads() {
        let (store, schema) = make_store("slru_newest_lin").await;
        let anc = rand_branch();
        let child = rand_branch();
        store
            .put_slru_pages(
                anc,
                &[
                    clog_row(0, 10, true, true, b"anc-img"),
                    clog_row(0, 20, false, false, b"anc-d20"),
                ],
            )
            .await
            .unwrap();
        store
            .put_slru_pages(
                child,
                &[
                    clog_row(0, 30, false, false, b"child-d30"),
                    clog_row(0, 40, false, false, b"child-d40"),
                ],
            )
            .await
            .unwrap();
        let lineage = lineage2(child, anc, 25);
        for (n, want) in [
            (3u64, vec![40, 30, 20]),
            (1, vec![40]),
            (10, vec![40, 30, 20, 10]),
        ] {
            let got = store
                .scan_slru_range_lineage(
                    &lineage,
                    SlruKind::Clog,
                    1,
                    0..1,
                    Lsn(100),
                    SlruScan::NewestUpTo(n),
                )
                .await
                .unwrap();
            assert_eq!(lsns(&got[&0]), want, "NewestUpTo({n})");
        }
        drop_schema(&store, &schema).await;
    }

    // ------------------- #1077 anchor-stop contract ------------------------
    //
    // Which ancestor segments a lineage scan reads is invisible in what it
    // returns, so these tests assert on what the walkers record under
    // `#[cfg(test)]` (see `record_segment_scan` / `take_segment_scans`).

    /// The segments scanned on this thread since the last call, in read order.
    fn take_segment_scans() -> Vec<SegmentScan> {
        SEGMENT_SCANS.with_borrow_mut(std::mem::take)
    }

    /// [`take_segment_scans`] reduced to the branches, for the tests that only
    /// care which segments were read.
    fn take_scanned_branches() -> Vec<BranchId> {
        take_segment_scans()
            .into_iter()
            .map(|s| s.branch_id)
            .collect()
    }

    /// Every requested block is anchored in the child at or above the
    /// ancestor's cap, so nothing the ancestor could return survives the merge.
    /// The walk must stop before reading it, exactly as `read_chain_lineage`
    /// already does for a single block.
    #[tokio::test]
    async fn scan_range_lineage_stops_at_the_anchor() {
        let (store, schema) = make_store("lin_stop_range").await;
        let anc = rand_branch();
        let child = rand_branch();
        store
            .put_pages(
                anc,
                &[
                    blk_row(1, 10, true, true, b"a1"),
                    blk_row(2, 10, true, true, b"a2"),
                ],
            )
            .await
            .unwrap();
        store
            .put_pages(
                child,
                &[
                    blk_row(1, 30, true, true, b"c1"),
                    blk_row(2, 30, true, true, b"c2"),
                ],
            )
            .await
            .unwrap();

        take_segment_scans();
        let out = store
            .scan_range_lineage(&lineage2(child, anc, 25), blk_range(1, 3), Lsn(100))
            .await
            .unwrap();
        assert_eq!(lsns(&out[&blk_key(1)]), vec![30]);
        assert_eq!(lsns(&out[&blk_key(2)]), vec![30]);
        assert_eq!(
            take_scanned_branches(),
            vec![child],
            "the child anchors every block at or above the cap; the ancestor is surplus"
        );
        drop_schema(&store, &schema).await;
    }

    /// Scattered-set twin of `scan_range_lineage_stops_at_the_anchor`.
    #[tokio::test]
    async fn scan_scattered_set_lineage_stops_at_the_anchor() {
        let (store, schema) = make_store("lin_stop_set").await;
        let anc = rand_branch();
        let child = rand_branch();
        store
            .put_pages(
                anc,
                &[
                    blk_row(1, 10, true, true, b"a1"),
                    blk_row(8, 10, true, true, b"a8"),
                ],
            )
            .await
            .unwrap();
        store
            .put_pages(
                child,
                &[
                    blk_row(1, 30, true, true, b"c1"),
                    blk_row(8, 30, true, true, b"c8"),
                ],
            )
            .await
            .unwrap();

        let keys = [blk_key(1), blk_key(8)];
        take_segment_scans();
        let out = store
            .scan_scattered_set_lineage(&lineage2(child, anc, 25), &keys, Lsn(100))
            .await
            .unwrap();
        assert_eq!(lsns(&out[&blk_key(1)]), vec![30]);
        assert_eq!(lsns(&out[&blk_key(8)]), vec![30]);
        assert_eq!(
            take_scanned_branches(),
            vec![child],
            "the child anchors every key at or above the cap; the ancestor is surplus"
        );
        drop_schema(&store, &schema).await;
    }

    /// SLRU twin of `scan_range_lineage_stops_at_the_anchor`.
    #[tokio::test]
    async fn scan_slru_range_lineage_stops_at_the_anchor() {
        let (store, schema) = make_store("lin_stop_slru").await;
        let anc = rand_branch();
        let child = rand_branch();
        store
            .put_slru_pages(
                anc,
                &[
                    clog_row(0, 10, true, true, b"a0"),
                    clog_row(1, 10, true, true, b"a1"),
                ],
            )
            .await
            .unwrap();
        store
            .put_slru_pages(
                child,
                &[
                    clog_row(0, 30, true, true, b"c0"),
                    clog_row(1, 30, true, true, b"c1"),
                ],
            )
            .await
            .unwrap();

        take_segment_scans();
        let out = store
            .scan_slru_range_lineage(
                &lineage2(child, anc, 25),
                SlruKind::Clog,
                1,
                0..2,
                Lsn(100),
                SlruScan::ToAnchor,
            )
            .await
            .unwrap();
        assert_eq!(lsns(&out[&0]), vec![30]);
        assert_eq!(lsns(&out[&1]), vec![30]);
        assert_eq!(
            take_scanned_branches(),
            vec![child],
            "the child anchors every block at or above the cap; the ancestor is surplus"
        );
        drop_schema(&store, &schema).await;
    }

    /// The same stop under [`SlruScan::NewestUpTo`], whose tip chain is a
    /// top-of-chain cut rather than anchor-complete (`clog_trailers` is the
    /// production caller). The cut cannot hide an anchor the skip needs: the
    /// merge trims at the newest `will_init` before taking the newest `n`, so
    /// a shadowed ancestor's rows are dropped whichever mode asked for them.
    #[tokio::test]
    async fn scan_slru_newest_lineage_stops_at_the_anchor() {
        let (store, schema) = make_store("lin_stop_slru_newest").await;
        let anc = rand_branch();
        let child = rand_branch();
        store
            .put_slru_pages(
                anc,
                &[
                    clog_row(0, 10, true, true, b"a0-img"),
                    clog_row(0, 20, false, false, b"a0-d20"),
                    clog_row(1, 10, true, true, b"a1-img"),
                ],
            )
            .await
            .unwrap();
        store
            .put_slru_pages(
                child,
                &[
                    clog_row(0, 30, true, true, b"c0-img"),
                    clog_row(0, 40, false, false, b"c0-d40"),
                    clog_row(1, 30, true, true, b"c1-img"),
                ],
            )
            .await
            .unwrap();

        take_segment_scans();
        let out = store
            .scan_slru_range_lineage(
                &lineage2(child, anc, 25),
                SlruKind::Clog,
                1,
                0..2,
                Lsn(100),
                SlruScan::NewestUpTo(3),
            )
            .await
            .unwrap();
        assert_eq!(lsns(&out[&0]), vec![40, 30]);
        assert_eq!(lsns(&out[&1]), vec![30]);
        assert_eq!(
            take_scanned_branches(),
            vec![child],
            "every block's image sits at or above the cap, so the ancestor's \
             pre-branch rows are below an anchor the merge trims at"
        );
        drop_schema(&store, &schema).await;
    }

    /// Depth 3: an anchor at or above the PARENT's cap shadows the parent, and
    /// because caps are non-increasing it shadows the grandparent too. One
    /// scan, not three.
    #[tokio::test]
    async fn scan_range_lineage_stops_before_a_shadowed_grandparent() {
        let (store, schema) = make_store("lin_stop_depth3").await;
        let grand = rand_branch();
        let parent = rand_branch();
        let child = rand_branch();
        store
            .put_pages(grand, &[blk_row(1, 10, true, true, b"g1")])
            .await
            .unwrap();
        store
            .put_pages(parent, &[blk_row(1, 40, false, false, b"p1")])
            .await
            .unwrap();
        store
            .put_pages(child, &[blk_row(1, 60, true, true, b"c1")])
            .await
            .unwrap();

        take_segment_scans();
        let out = store
            .scan_range_lineage(
                &lineage3(child, parent, grand, 50, 20),
                blk_range(1, 2),
                Lsn(100),
            )
            .await
            .unwrap();
        assert_eq!(lsns(&out[&blk_key(1)]), vec![60]);
        assert_eq!(
            take_scanned_branches(),
            vec![child],
            "an anchor above the parent's cap shadows every deeper segment too"
        );
        drop_schema(&store, &schema).await;
    }

    /// Depth 3 with the anchor one level down: the child carries only a delta, so
    /// the parent must be read, and the parent's own image then shadows the
    /// grandparent. The walk folds each ancestor into `out` before judging the
    /// next, so two segments are read, not three. Planning every ancestor from the
    /// child's rows alone would miss this and read the grandparent as well.
    #[tokio::test]
    async fn scan_range_lineage_stops_when_the_parent_shadows_the_grandparent() {
        let (store, schema) = make_store("lin_parent_shadows").await;
        let grand = rand_branch();
        let parent = rand_branch();
        let child = rand_branch();
        store
            .put_pages(grand, &[blk_row(1, 10, true, true, b"g1")])
            .await
            .unwrap();
        // At or above the grandparent's cap (20), below the parent's own (50).
        store
            .put_pages(parent, &[blk_row(1, 30, true, true, b"p1")])
            .await
            .unwrap();
        // A delta anchors nothing, so the child cannot end the walk by itself.
        store
            .put_pages(child, &[blk_row(1, 60, false, false, b"c1")])
            .await
            .unwrap();

        take_segment_scans();
        let out = store
            .scan_range_lineage(
                &lineage3(child, parent, grand, 50, 20),
                blk_range(1, 2),
                Lsn(100),
            )
            .await
            .unwrap();
        assert_eq!(
            lsns(&out[&blk_key(1)]),
            vec![60, 30],
            "the child delta layers on the parent's image, and the walk trims there"
        );
        assert_eq!(
            take_scanned_branches(),
            vec![child, parent],
            "the parent's image anchors block 1 at or above the grandparent's cap"
        );
        drop_schema(&store, &schema).await;
    }

    /// An anchor BELOW the segment's cap does not shadow it: a materialiser
    /// copy-up image lands in the child under the ancestor's head, and the
    /// ancestor deltas in (anchor_lsn, cap] still have to be layered on top.
    /// The ancestor must still be read, and its delta must survive.
    #[tokio::test]
    async fn scan_range_lineage_reads_an_ancestor_under_a_sub_cap_anchor() {
        let (store, schema) = make_store("lin_subcap").await;
        let anc = rand_branch();
        let child = rand_branch();
        store
            .put_pages(anc, &[blk_row(1, 25, false, false, b"a-delta")])
            .await
            .unwrap();
        store
            .put_pages(child, &[blk_row(1, 24, true, true, b"c-image")])
            .await
            .unwrap();

        take_segment_scans();
        let out = store
            .scan_range_lineage(&lineage2(child, anc, 25), blk_range(1, 2), Lsn(100))
            .await
            .unwrap();
        assert_eq!(
            lsns(&out[&blk_key(1)]),
            vec![25, 24],
            "the ancestor delta in (24, 25] layers on the sub-cap copy-up image"
        );
        assert_eq!(
            take_scanned_branches(),
            vec![child, anc],
            "an anchor below the cap leaves the ancestor's rows above it uncovered"
        );
        drop_schema(&store, &schema).await;
    }

    /// A mixed range: one block anchored in the child, one carrying only a
    /// child delta, one that exists only in the ancestor. The ancestor is still
    /// needed, but only for the two unanchored blocks, and the ancestor-only
    /// block must still be discovered.
    #[tokio::test]
    async fn scan_range_lineage_reads_an_ancestor_for_unanchored_blocks() {
        let (store, schema) = make_store("lin_mixed").await;
        let anc = rand_branch();
        let child = rand_branch();
        store
            .put_pages(
                anc,
                &[
                    blk_row(1, 10, true, true, b"a1"),
                    blk_row(2, 10, true, true, b"a2"),
                    blk_row(3, 10, true, true, b"a3"),
                ],
            )
            .await
            .unwrap();
        store
            .put_pages(
                child,
                &[
                    blk_row(1, 30, true, true, b"c1"),
                    blk_row(2, 30, false, false, b"c2"),
                ],
            )
            .await
            .unwrap();

        take_segment_scans();
        let out = store
            .scan_range_lineage(&lineage2(child, anc, 25), blk_range(1, 4), Lsn(100))
            .await
            .unwrap();
        assert_eq!(lsns(&out[&blk_key(1)]), vec![30], "anchored in the child");
        assert_eq!(
            lsns(&out[&blk_key(2)]),
            vec![30, 10],
            "a child delta over the ancestor image"
        );
        assert_eq!(
            lsns(&out[&blk_key(3)]),
            vec![10],
            "a block written only before the branch point is still discovered"
        );
        assert_eq!(
            take_segment_scans(),
            vec![
                SegmentScan {
                    branch_id: child,
                    keys: None
                },
                SegmentScan {
                    branch_id: anc,
                    keys: Some(2)
                },
            ],
            "block 1 is anchored in the child, so the ancestor is read for the other two"
        );
        drop_schema(&store, &schema).await;
    }

    /// SLRU twin of `scan_range_lineage_reads_an_ancestor_for_unanchored_blocks`:
    /// the residual reaches the scan, which narrows the ancestor read to its
    /// span (`blknum_span`) rather than re-reading the anchored head blocks.
    #[tokio::test]
    async fn scan_slru_range_lineage_reads_an_ancestor_for_unanchored_blocks() {
        let (store, schema) = make_store("lin_slru_mixed").await;
        let anc = rand_branch();
        let child = rand_branch();
        store
            .put_slru_pages(
                anc,
                &[
                    clog_row(0, 10, true, true, b"a0"),
                    clog_row(1, 10, true, true, b"a1"),
                    clog_row(2, 10, true, true, b"a2"),
                ],
            )
            .await
            .unwrap();
        store
            .put_slru_pages(
                child,
                &[
                    clog_row(0, 30, true, true, b"c0"),
                    clog_row(1, 30, false, false, b"c1"),
                ],
            )
            .await
            .unwrap();

        take_segment_scans();
        let out = store
            .scan_slru_range_lineage(
                &lineage2(child, anc, 25),
                SlruKind::Clog,
                1,
                0..3,
                Lsn(100),
                SlruScan::ToAnchor,
            )
            .await
            .unwrap();
        assert_eq!(lsns(&out[&0]), vec![30], "anchored in the child");
        assert_eq!(
            lsns(&out[&1]),
            vec![30, 10],
            "a child delta over the ancestor image"
        );
        assert_eq!(
            lsns(&out[&2]),
            vec![10],
            "a block written only before the branch point is still discovered"
        );
        assert_eq!(
            take_segment_scans(),
            vec![
                SegmentScan {
                    branch_id: child,
                    keys: None
                },
                SegmentScan {
                    branch_id: anc,
                    keys: Some(2)
                },
            ],
            "block 0 is anchored in the child, so the ancestor is read for blocks 1..3"
        );
        drop_schema(&store, &schema).await;
    }

    /// A residual with a hole in it. The child anchors blocks 0, 2 and 4, so
    /// only 1 and 3 still need the ancestor, which the rel path asks for
    /// exactly, since a scattered set need not be contiguous.
    #[tokio::test]
    async fn scan_range_lineage_reads_an_ancestor_for_a_scattered_residual() {
        let (store, schema) = make_store("lin_gap_rel").await;
        let anc = rand_branch();
        let child = rand_branch();
        store
            .put_pages(
                anc,
                &(0..5)
                    .map(|b| blk_row(b, 10, true, true, b"a"))
                    .collect::<Vec<_>>(),
            )
            .await
            .unwrap();
        store
            .put_pages(
                child,
                &[
                    blk_row(0, 30, true, true, b"c0"),
                    blk_row(2, 30, true, true, b"c2"),
                    blk_row(4, 30, true, true, b"c4"),
                ],
            )
            .await
            .unwrap();

        take_segment_scans();
        let out = store
            .scan_range_lineage(&lineage2(child, anc, 25), blk_range(0, 5), Lsn(100))
            .await
            .unwrap();
        for blk in [0, 2, 4] {
            assert_eq!(lsns(&out[&blk_key(blk)]), vec![30], "anchored in the child");
        }
        for blk in [1, 3] {
            assert_eq!(lsns(&out[&blk_key(blk)]), vec![10], "ancestor-only block");
        }
        assert_eq!(
            take_segment_scans(),
            vec![
                SegmentScan {
                    branch_id: child,
                    keys: None
                },
                SegmentScan {
                    branch_id: anc,
                    keys: Some(2)
                },
            ],
            "the gap costs nothing on the rel path: blocks 1 and 3 and no others"
        );
        drop_schema(&store, &schema).await;
    }

    /// SLRU twin of the scattered residual, pinning the cost the rel path does
    /// not pay: with no scattered SLRU read the ancestor is narrowed to the
    /// residual's SPAN, so the anchored block between 1 and 3 is read again and
    /// discarded by the merge. Three blocks read for two that were needed.
    #[tokio::test]
    async fn scan_slru_range_lineage_widens_a_scattered_residual_to_its_span() {
        let (store, schema) = make_store("lin_gap_slru").await;
        let anc = rand_branch();
        let child = rand_branch();
        store
            .put_slru_pages(
                anc,
                &(0..5)
                    .map(|b| clog_row(b, 10, true, true, b"a"))
                    .collect::<Vec<_>>(),
            )
            .await
            .unwrap();
        store
            .put_slru_pages(
                child,
                &[
                    clog_row(0, 30, true, true, b"c0"),
                    clog_row(2, 30, true, true, b"c2"),
                    clog_row(4, 30, true, true, b"c4"),
                ],
            )
            .await
            .unwrap();

        take_segment_scans();
        let out = store
            .scan_slru_range_lineage(
                &lineage2(child, anc, 25),
                SlruKind::Clog,
                1,
                0..5,
                Lsn(100),
                SlruScan::ToAnchor,
            )
            .await
            .unwrap();
        for blk in [0, 2, 4] {
            assert_eq!(lsns(&out[&blk]), vec![30], "anchored in the child");
        }
        for blk in [1, 3] {
            assert_eq!(lsns(&out[&blk]), vec![10], "ancestor-only block");
        }
        assert_eq!(
            take_segment_scans(),
            vec![
                SegmentScan {
                    branch_id: child,
                    keys: None
                },
                SegmentScan {
                    branch_id: anc,
                    keys: Some(3)
                },
            ],
            "blocks 1 and 3 are needed, so the span 1..4 is read, not 0..5, \
             and not the two blocks a scattered read would have taken"
        );
        drop_schema(&store, &schema).await;
    }

    /// A range crossing a fork boundary cannot enumerate its keys, so the walk
    /// cannot prove any block is anchored and must keep reading every ancestor.
    #[tokio::test]
    async fn scan_range_lineage_reads_every_ancestor_for_a_cross_fork_range() {
        let (store, schema) = make_store("lin_crossfork").await;
        let anc = rand_branch();
        let child = rand_branch();
        store
            .put_pages(
                anc,
                &[page_row(
                    page_key(1663, 5, 16384, 1, 0),
                    10,
                    true,
                    true,
                    b"afsm",
                )],
            )
            .await
            .unwrap();
        store
            .put_pages(child, &[blk_row(0, 30, true, true, b"c0")])
            .await
            .unwrap();

        let range = blk_key(0)..Key::from_slice(&page_key(1663, 5, 16384, 1, 1));
        take_segment_scans();
        let out = store
            .scan_range_lineage(&lineage2(child, anc, 25), range, Lsn(100))
            .await
            .unwrap();
        assert_eq!(
            lsns(&out[&Key::from_slice(&page_key(1663, 5, 16384, 1, 0))]),
            vec![10],
            "the ancestor-only key in the other fork is still found"
        );
        assert_eq!(
            take_scanned_branches(),
            vec![child, anc],
            "an unenumerable range keeps the read-everything walk"
        );
        drop_schema(&store, &schema).await;
    }

    /// Scattered-set twin of `scan_range_lineage_reads_an_ancestor_for_unanchored_blocks`.
    #[tokio::test]
    async fn scan_scattered_set_lineage_reads_an_ancestor_for_unanchored_keys() {
        let (store, schema) = make_store("lin_set_mixed").await;
        let anc = rand_branch();
        let child = rand_branch();
        store
            .put_pages(
                anc,
                &[
                    blk_row(1, 10, true, true, b"a1"),
                    blk_row(8, 10, true, true, b"a8"),
                    blk_row(40, 10, true, true, b"a40"),
                ],
            )
            .await
            .unwrap();
        store
            .put_pages(
                child,
                &[
                    blk_row(1, 30, true, true, b"c1"),
                    blk_row(8, 30, false, false, b"c8"),
                ],
            )
            .await
            .unwrap();

        let keys = [blk_key(1), blk_key(8), blk_key(40)];
        take_segment_scans();
        let out = store
            .scan_scattered_set_lineage(&lineage2(child, anc, 25), &keys, Lsn(100))
            .await
            .unwrap();
        assert_eq!(lsns(&out[&blk_key(1)]), vec![30], "anchored in the child");
        assert_eq!(lsns(&out[&blk_key(8)]), vec![30, 10]);
        assert_eq!(lsns(&out[&blk_key(40)]), vec![10], "ancestor-only key");
        assert_eq!(
            take_segment_scans(),
            vec![
                SegmentScan {
                    branch_id: child,
                    keys: None
                },
                SegmentScan {
                    branch_id: anc,
                    keys: Some(2)
                },
            ],
            "key 1 is anchored in the child, so the ancestor is read for 8 and 40"
        );
        drop_schema(&store, &schema).await;
    }

    /// Nothing anchors in the child, so the parent is read in full; the parent's
    /// image then shadows the grandparent for block 1 only, so narrowing starts
    /// one segment down.
    #[tokio::test]
    async fn scan_range_lineage_narrows_first_at_the_grandparent() {
        let (store, schema) = make_store("lin_narrow_grand").await;
        let grand = rand_branch();
        let parent = rand_branch();
        let child = rand_branch();
        store
            .put_pages(
                grand,
                &[
                    blk_row(1, 10, true, true, b"g1"),
                    blk_row(2, 10, true, true, b"g2"),
                ],
            )
            .await
            .unwrap();
        store
            .put_pages(
                parent,
                &[
                    blk_row(1, 30, true, true, b"p1"),
                    blk_row(2, 40, false, false, b"p2"),
                ],
            )
            .await
            .unwrap();
        store
            .put_pages(
                child,
                &[
                    blk_row(1, 60, false, false, b"c1"),
                    blk_row(2, 60, false, false, b"c2"),
                ],
            )
            .await
            .unwrap();

        take_segment_scans();
        let out = store
            .scan_range_lineage(
                &lineage3(child, parent, grand, 50, 20),
                blk_range(1, 3),
                Lsn(100),
            )
            .await
            .unwrap();
        assert_eq!(lsns(&out[&blk_key(1)]), vec![60, 30]);
        assert_eq!(lsns(&out[&blk_key(2)]), vec![60, 40, 10]);
        assert_eq!(
            take_segment_scans(),
            vec![
                SegmentScan {
                    branch_id: child,
                    keys: None
                },
                SegmentScan {
                    branch_id: parent,
                    keys: None
                },
                SegmentScan {
                    branch_id: grand,
                    keys: Some(1)
                },
            ],
            "only block 2 still needs the grandparent"
        );
        drop_schema(&store, &schema).await;
    }

    /// Partial narrowing under [`SlruScan::NewestUpTo`]: block 0 is anchored in
    /// the child at or above the cap, block 1 is not.
    #[tokio::test]
    async fn scan_slru_newest_lineage_reads_an_ancestor_for_unanchored_blocks() {
        let (store, schema) = make_store("lin_slru_newest_mixed").await;
        let anc = rand_branch();
        let child = rand_branch();
        store
            .put_slru_pages(
                anc,
                &[
                    clog_row(0, 10, true, true, b"a0-img"),
                    clog_row(1, 10, true, true, b"a1-img"),
                    clog_row(1, 20, false, false, b"a1-d20"),
                ],
            )
            .await
            .unwrap();
        store
            .put_slru_pages(
                child,
                &[
                    clog_row(0, 30, true, true, b"c0-img"),
                    clog_row(0, 40, false, false, b"c0-d40"),
                    clog_row(1, 30, false, false, b"c1-d30"),
                ],
            )
            .await
            .unwrap();

        take_segment_scans();
        let out = store
            .scan_slru_range_lineage(
                &lineage2(child, anc, 25),
                SlruKind::Clog,
                1,
                0..2,
                Lsn(100),
                SlruScan::NewestUpTo(3),
            )
            .await
            .unwrap();
        assert_eq!(lsns(&out[&0]), vec![40, 30]);
        assert_eq!(lsns(&out[&1]), vec![30, 20, 10]);
        assert_eq!(
            take_segment_scans(),
            vec![
                SegmentScan {
                    branch_id: child,
                    keys: None
                },
                SegmentScan {
                    branch_id: anc,
                    keys: Some(1)
                },
            ],
            "block 0 is anchored in the child, so the ancestor is read for block 1"
        );
        drop_schema(&store, &schema).await;
    }

    // ------------------- #1077 anchor predicate (no YB) --------------------

    #[test]
    fn shadowed_by_needs_a_will_init_at_or_above_the_cap() {
        let seg = LineageSegment {
            branch_id: rand_branch(),
            cap_lsn: Lsn(25),
        };
        let k = page_key(1663, 5, 16384, 0, 1);
        assert!(seg.shadowed_by(&[page_row(k.clone(), 25, true, true, b"")]));
        assert!(seg.shadowed_by(&[
            page_row(k.clone(), 40, false, false, b""),
            page_row(k.clone(), 30, true, true, b""),
        ]));
        assert!(
            !seg.shadowed_by(&[page_row(k.clone(), 24, true, true, b"")]),
            "a sub-cap copy-up leaves the ancestor deltas in (24, 25] uncovered"
        );
        assert!(
            !seg.shadowed_by(&[page_row(k.clone(), 30, false, false, b"")]),
            "a delta is not an anchor"
        );
        assert!(!seg.shadowed_by::<Arc<PageRow>>(&[]));
    }

    #[test]
    fn retain_unshadowed_keeps_the_keys_the_segment_is_still_needed_for() {
        let parent = LineageSegment {
            branch_id: rand_branch(),
            cap_lsn: Lsn(50),
        };
        let grand = LineageSegment {
            branch_id: rand_branch(),
            cap_lsn: Lsn(20),
        };
        let row =
            |blk: u32, lsn: u64, will_init: bool| blk_row(blk, lsn, will_init, will_init, b"");
        let have: HashMap<Key, Vec<Arc<PageRow>>> = HashMap::from([
            (blk_key(1), vec![row(1, 60, true)]),  // anchors both segments
            (blk_key(2), vec![row(2, 30, true)]),  // anchors the grandparent only
            (blk_key(3), vec![row(3, 70, false)]), // a delta anchors nothing
        ]);
        let left = |seg: &LineageSegment, keys: &[Key]| {
            let mut keys = keys.to_vec();
            seg.retain_unshadowed(&mut keys, &have);
            keys
        };
        assert_eq!(left(&parent, &[blk_key(1)]), vec![]);
        assert_eq!(left(&parent, &[blk_key(2)]), vec![blk_key(2)]);
        assert_eq!(
            left(&grand, &[blk_key(2)]),
            vec![],
            "the deeper cap is lower, so the same anchor covers it"
        );
        assert_eq!(
            left(&parent, &[blk_key(3)]),
            vec![blk_key(3)],
            "a delta-only chain anchors nothing"
        );
        assert_eq!(
            left(&parent, &[blk_key(4)]),
            vec![blk_key(4)],
            "a key with no chain at all could live only in an ancestor"
        );
        assert_eq!(
            left(&parent, &[blk_key(1), blk_key(3)]),
            vec![blk_key(3)],
            "the anchored key drops out; the segment is read for the rest"
        );
    }

    #[test]
    fn blknum_span_covers_the_residual_and_nothing_outside_it() {
        assert_eq!(blknum_span(&[4]), Some(4..5));
        assert_eq!(blknum_span(&[7, 2, 5]), Some(2..8), "order does not matter");
        assert_eq!(blknum_span(&[]), None);
        assert_eq!(blknum_span(&[u32::MAX]), None, "no range can end past MAX");
    }

    #[test]
    fn requested_keys_enumerates_one_fork_ranges_only() {
        let enumerate = |t: &LineageScanTarget| t.requested_keys().map(Iterator::collect::<Vec<_>>);

        let range = LineageScanTarget::Range(blk_range(3, 7));
        assert_eq!(
            enumerate(&range),
            Some(vec![blk_key(3), blk_key(4), blk_key(5), blk_key(6)])
        );
        let keys = [blk_key(1), blk_key(8), blk_key(40)];
        assert_eq!(
            enumerate(&LineageScanTarget::Set(&keys)),
            Some(keys.to_vec())
        );
        let cross_fork =
            LineageScanTarget::Range(blk_key(0)..Key::from_slice(&page_key(1663, 5, 16384, 1, 0)));
        assert_eq!(
            enumerate(&cross_fork),
            None,
            "a range crossing forks cannot enumerate its keys"
        );
    }

    // ------------------- #1077 perf harness (ignored) ----------------------

    /// Wall-clock cost of one vectored read on a branched timeline whose own
    /// rows anchor every requested block at or above the ancestor's cap, so
    /// every ancestor scan the walk issues is provably surplus. Ignored by
    /// default: needs a live YB and runs for seconds. Run it with
    ///
    ///     cargo test -p pageserver --lib bench_lineage_scan -- --ignored --nocapture
    #[tokio::test]
    #[ignore = "perf harness: needs a live YB and runs for seconds"]
    async fn bench_lineage_scan_shadowed_ancestor() {
        const BLOCKS: u32 = 128;
        const ITERS: usize = 100;
        let (store, schema) = make_store("bench_lineage").await;
        let anc = rand_branch();
        let child = rand_branch();

        let anc_rows: Vec<_> = (0..BLOCKS)
            .map(|b| blk_row(b, 10, true, true, b"anc"))
            .collect();
        let child_rows: Vec<_> = (0..BLOCKS)
            .map(|b| blk_row(b, 30, true, true, b"child"))
            .collect();
        store.put_pages(anc, &anc_rows).await.unwrap();
        store.put_pages(child, &child_rows).await.unwrap();

        let lineage = lineage2(child, anc, 25);
        let range = blk_range(0, BLOCKS);

        // Warm the pool, the tablet caches and the prepared statements.
        for _ in 0..10 {
            store
                .scan_range_lineage(&lineage, range.clone(), Lsn(100))
                .await
                .unwrap();
        }

        let mut samples = Vec::with_capacity(ITERS);
        for _ in 0..ITERS {
            let t = std::time::Instant::now();
            let out = store
                .scan_range_lineage(&lineage, range.clone(), Lsn(100))
                .await
                .unwrap();
            samples.push(t.elapsed());
            assert_eq!(out.len(), BLOCKS as usize);
        }
        samples.sort();
        let median = samples[ITERS / 2];
        let p90 = samples[ITERS * 9 / 10];
        let mean = samples.iter().sum::<std::time::Duration>() / ITERS as u32;
        println!(
            "BENCH #1077 scan_range_lineage blocks={BLOCKS} iters={ITERS} \
             median={median:?} mean={mean:?} p90={p90:?}"
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

    // ------------------- hot-path (thin-client) lane tests ------------------

    /// Live hot-path round trip: put pages/SLRU pages/meta through the lane,
    /// then read chains, multi-block scans and meta back through it. Covers
    /// the anchor semantics the chain assembly must preserve — an image with
    /// deltas above and shadowed history below, a same-LSN image/delta tie,
    /// and a will_init delta with no image at all — and each op's lane sample
    /// count must advance, so a silently erroring lane can't pass unnoticed.
    #[tokio::test]
    async fn hot_path_chain_scan_put_round_trip() {
        let (store, _) = make_store("hot_path_round_trip").await;
        let branch = rand_branch();
        let ops = [
            "put_pages",
            "put_slru_pages",
            "read_chain",
            "scan_range",
            "scan_scattered_set",
            "read_slru_chain",
            "scan_slru_range",
            "put_meta",
            "read_meta",
        ];
        let before: Vec<u64> = ops.iter().map(|op| thin_op_count(op)).collect();

        //   blk 0: image anchor + deltas above + shadowed history below;
        //   blk 1: same-LSN image/delta tie (image inserted LAST so
        //          storage order is worst-case delta-first);
        //   blk 2: no image — will_init delta anchor (slow path).
        let k0 = page_key(1663, 5, 16384, 0, 0);
        let k1 = page_key(1663, 5, 16384, 0, 1);
        let k2 = page_key(1663, 5, 16384, 0, 2);
        store
            .put_pages(
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

        // `(key, target_lsn)` → the payloads the chain must carry, newest
        // first, truncated at the anchor.
        let want: &[(&Bytes, u64, &[&[u8]])] = &[
            (&k0, 1000, &[b"k0-d30", b"k0-d20", b"k0-img"]),
            (&k0, 25, &[b"k0-d20", b"k0-img"]),
            // The image wins the LSN-100 tie, so the plain delta at the same
            // LSN is shadowed and never applied.
            (&k1, 1000, &[b"k1-d200", b"k1-img-100"]),
            (&k1, 25, &[]),
            (&k2, 1000, &[b"k2-d30", b"k2-init"]),
            (&k2, 25, &[b"k2-init"]),
        ];
        for (k, target, payloads) in want {
            let chain = store.read_chain(branch, k, Lsn(*target)).await.unwrap();
            let got: Vec<&[u8]> = chain.iter().map(|r| r.payload.as_ref()).collect();
            assert_eq!(&got, payloads, "read_chain key={k:?} target={target}");
        }

        // Multi-block one-fork scan: same chains, one entry per block.
        let range = Key::from_slice(&k0)..Key::from_slice(&page_key(1663, 5, 16384, 0, 3));
        let by_key = store.scan_range(branch, range, Lsn(1000)).await.unwrap();
        assert_eq!(by_key.len(), 3);
        for (k, target, payloads) in want.iter().filter(|(_, t, _)| *t == 1000) {
            let chain = &by_key[&Key::from_slice(k)];
            let got: Vec<&[u8]> = chain.iter().map(|r| r.payload.as_ref()).collect();
            assert_eq!(&got, payloads, "scan_range key={k:?} target={target}");
        }

        // The same fork minus the middle block: the non-contiguous >=2-key
        // fold `get_vectored` routes to the scattered path. Chain equivalence
        // is `scan_scattered_set_matches_per_key_reads`' job; here it only has
        // to reach the lane (#1025).
        let scattered = store
            .scan_scattered_set(
                branch,
                &[Key::from_slice(&k0), Key::from_slice(&k2)],
                Lsn(1000),
            )
            .await
            .unwrap();
        assert_eq!(scattered.len(), 2);

        // SLRU: put through the lane, then chain + range scan.
        store
            .put_slru_pages(
                branch,
                &[
                    clog_row(0, 10, true, true, b"s0-img"),
                    clog_row(0, 20, false, false, b"s0-d20"),
                    clog_row(1, 30, false, true, b"s1-init"),
                    clog_row(1, 40, false, false, b"s1-d40"),
                ],
            )
            .await
            .unwrap();
        let chain = store
            .read_slru_chain(branch, SlruKind::Clog, 1, 0, Lsn(1000))
            .await
            .unwrap();
        let got: Vec<&[u8]> = chain.iter().map(|r| r.payload.as_ref()).collect();
        assert_eq!(got, [b"s0-d20".as_ref(), b"s0-img".as_ref()]);
        let by_block = store
            .scan_slru_range(
                branch,
                SlruKind::Clog,
                1,
                0..4,
                Lsn(1000),
                SlruScan::ToAnchor,
            )
            .await
            .unwrap();
        assert_eq!(by_block.len(), 2);
        let got: Vec<&[u8]> = by_block[&1].iter().map(|r| r.payload.as_ref()).collect();
        assert_eq!(got, [b"s1-d40".as_ref(), b"s1-init".as_ref()]);

        // Meta: two versions of one key, so the `lsn DESC` clustering has to
        // place them for the targeted reads below to differ.
        let mk = meta_key(7);
        store
            .put_meta(
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
        for (target, payload) in [
            (1000u64, Some(b"m20".as_ref())),
            (15, Some(b"m10".as_ref())),
            (5, None),
        ] {
            let row = store.read_meta(branch, &mk, Lsn(target)).await.unwrap();
            assert_eq!(
                row.as_ref().map(|r| r.payload.as_ref()),
                payload,
                "read_meta target={target}"
            );
        }

        for (op, b) in ops.iter().zip(before) {
            assert!(thin_op_count(op) > b, "op {op} did not reach the lane");
        }
    }

    /// A batch above `ROWS_PER_INSERT` must land fully via multiple
    /// Performs, and a chain read longer than one round must walk the
    /// paging envelope on one snapshot.
    #[tokio::test]
    async fn thin_lane_chunks_and_pages() {
        let (pg, _) = make_store("thin_chunks").await;
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
        // 60 deltas — deeper than a chain-read round, so the chain
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
        // The paged deep-chain read: image at 10 plus all 60 deltas above
        // it, newest first, walked across several rounds.
        let chain = pg.read_chain(branch, &deep, Lsn(1000)).await.unwrap();
        assert_eq!(chain.len(), 61);
        let lsns: Vec<u64> = chain.iter().map(|r| r.lsn.0).collect();
        assert_eq!(lsns, (10..=70u64).rev().collect::<Vec<_>>());
        assert!(chain.last().unwrap().is_image);

        // Both put_pages and the chain reads reached the lane.
        assert!(
            thin_op_count("put_pages") > put_before,
            "put_pages did not reach the lane"
        );
        assert!(
            thin_op_count("read_chain") > read_before,
            "read_chain did not reach the lane"
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
}
