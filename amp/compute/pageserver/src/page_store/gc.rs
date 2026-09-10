//! GC's storage layer: the `pending_gc` candidate queue, the types the drain
//! passes between its steps, and every SQL statement GC issues against
//! `pages`, `slru_pages` and `meta`.
//!
//! A submodule of `page_store` for size only — its items are re-exported
//! there, so callers still say `page_store::GcKind`. The other two thirds of
//! GC are `gc_trim_candidates.rs` (candidate emission) and
//! `page_server/timeline/gc.rs` (the drain: cutoff, select, act, dequeue).
//! Why the statements below are shaped the way they are:
//! `compute/architecture/pageserver-gc.md`.

use super::*;
use std::fmt::Debug;
use std::hash::Hash;

use crate::page_store::tokio_postgres::Row;
use crate::page_store::tokio_postgres::types::FromSql;

/// Placement buckets in `amp_storage.pending_gc`'s hash key; must match
/// the drain's per-bucket fan-out.
pub const GC_BUCKETS: i32 = 3;

/// Objects a drain tick acts on at once (scaled); the dedicated
/// `pageserver:gc` pool is sized to this. Not the statement count: a `pages` act
/// fans out again over `PAGE_BUCKETS` (#1182), and the pool bounds how many
/// backends serve those, not how many are issued.
pub const GC_ACTION_PARALLELISM: usize = 1;

pub use pageserver_api::config::GC_SWEEP_MAX_ROWS_PER_DELETE;

/// How a trim ended. Cancellation is an outcome, not an error: the rows it
/// reports are committed either way, and the difference is whether the caller
/// may take a key's `anchor` as its next watermark — a loop that stopped early
/// has not covered the range, though every statement it did commit has already
/// reported its own progress.
#[derive(Debug, Clone, Copy, PartialEq, Eq)]
#[must_use]
pub enum TrimOutcome {
    /// Drained every key's range. Rows deleted.
    Complete(u64),
    /// Stopped on a cancelled token between statements. Rows deleted so far.
    Cancelled(u64),
}

/// What a trim statement reports per key: rows deleted, and the highest `lsn`
/// among them, which becomes that key's watermark. Named because three builders
/// write these aliases and [`PageStore::run_trim_loop`] reads them back — a
/// rename on one side alone compiles, then panics.
const TRIM_DELETED_COUNT_COL: &str = "deleted_count";
const TRIM_DELETED_TO_COL: &str = "deleted_to";

/// The trims' hint once every statement selects ids and deletes by `ybctid`.
/// Two directives, where the predicate DELETE needed five.
///
/// `CROSS JOIN LATERAL` is what puts each key's `[watermark, anchor)` in the
/// Index Cond, and it does so by construction rather than by costing: a lateral
/// reference is only definable per outer row, so a parameterised inner index
/// scan is the *only* legal plan. The plain `USING unnest(...)` join merely
/// permits that plan among several, which is why it needed `Leading`, the
/// merge/hash/material bars, and still landed the per-key ranges in a Join
/// Filter at realistic widths. `gc_trim_blocks_anchors_sql` has always used a lateral
/// for the same reason.
///
/// `TidScan(del)` is required, not an optimisation: on a table nothing has
/// ANALYZEd — the state every universe starts in — the planner costs it at 1000
/// rows and drops the Tid Scan for a full scan above ~27 ids (#970).
///
/// The table appears twice, so the aliases are part of the hint's meaning:
/// `scan` is the half that picks the ids, `del` the half that removes them, and
/// a bare `TidScan(pages)` is ambiguous — it can force a Tid Scan onto the id
/// subquery, which is exactly where the index scan is wanted.
const TRIM_LATERAL_HINT: &str = "/*+ IndexScan(scan {index}_pkey) TidScan(del) */";

/// What one trim runs against: which table, how its keys arrive and how they
/// are named once deleted. Everything else about the loop is shared and lives
/// in [`PageStore::run_trim_loop`].
struct TrimTarget<T, K> {
    /// Description of the write for the write gate.
    gate_label: &'static str,
    /// Result column holding the key.
    key_col: &'static str,
    /// The key under which the drain stores this key's watermark. `T` is how
    /// the statement binds and returns a key, `K` how that map names it.
    to_watermark_key: fn(&T) -> K,
    /// Error context, e.g. `gc_act_trim_slru branch=… kind=… segno=…`. The
    /// loop appends the bound count and first key.
    err_context: String,
}

/// One key's delete range, `[watermark, anchor)`. The watermark is the anchor
/// that key's last successful trim reached, below which everything deletable is
/// already gone — `0` for a key that has none, which is its whole history. The
/// anchor is the newest base at-or-below the cutoff, which must survive.
///
/// Generic because `pages` and `slru_pages` key on a blknum and `meta` on
/// encoded key bytes.
#[derive(Debug, Clone, PartialEq, Eq)]
struct TrimBound<T> {
    key: T,
    /// Inclusive: rises to the highest `lsn` each committed statement deleted.
    watermark: i64,
    /// Exclusive — it is the base that has to survive. Fixed for the call.
    anchor: i64,
}

impl<T: Clone> TrimBound<T> {
    /// The three parallel arrays a trim statement's `unnest(...)` binds. Their
    /// order is the contract with the SQL — the same order as the fields above,
    /// which is why this lives here — so every trim goes through it.
    fn unzip(bounds: &[Self]) -> (Vec<T>, Vec<i64>, Vec<i64>) {
        bounds
            .iter()
            .map(|b| (b.key.clone(), b.watermark, b.anchor))
            .multiunzip()
    }
}

/// The bounds one trim statement covers.
type TrimBounds<T> = Vec<TrimBound<T>>;

/// FNV-1a over the key's 18 encoded bytes. FROZEN: must stay a fixed,
/// deterministic function of them (golden-value test) — changing it strands
/// existing candidates in their old buckets and breaks crash-replay dedup.
/// Deliberately not `std::hash` (unstable across Rust versions) and not a YB
/// generated column (the yb-client write path bypasses the YSQL layer). Kept
/// separate from the modulus so the test can pin it independently of
/// [`GC_BUCKETS`], which is not frozen while no deployment holds candidates.
fn key_hash(key: &Key) -> u64 {
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
    (key_hash(key) % GC_BUCKETS as u64) as i32
}

/// GC candidate kind — the `pending_gc.gc_kind` column. Each variant maps
/// 1:1 to the act function it dispatches to:
/// `TrimPages`→`gc_act_trim_blocks`, `TrimSlru`→`gc_act_trim_slru`,
/// `TrimMeta`→`gc_act_trim_meta`, `ShrinkPages`→`gc_act_shrink_fork`
/// (rel-only), `DropSlru`→
/// `gc_act_drop_slru`, `DropMeta`→`gc_act_drop_meta`,
/// `DropDb`/`DropRel`→`gc_act_sweep`. The emitter, which
/// knows the target table, stamps the kind — the key stays pure identity.
///
/// Stored as a `SMALLINT` via [`Self::to_i16`]/[`Self::from_i16`] (wire
/// values 0..=7, frozen). A value this version doesn't recognise decodes
/// to [`GcKind::Unknown`], which the drain alarms on and leaves queued.
#[derive(Debug, Clone, Copy, PartialEq, Eq, PartialOrd, Ord, Hash)]
pub enum GcKind {
    TrimPages,
    TrimSlru,
    TrimMeta,
    ShrinkPages,
    /// A whole dropped relation, keyed by its main fork's size sentinel — one
    /// candidate covering every fork's `pages` rows and size row.
    DropRel,
    DropSlru,
    DropMeta,
    /// A whole dropped database, keyed by its relmap key — one candidate
    /// covering every fork's `pages` rows and every one of its `meta` keys.
    DropDb,
    Unknown(i16),
}

impl GcKind {
    pub fn to_i16(self) -> i16 {
        match self {
            GcKind::TrimPages => 0,
            GcKind::TrimSlru => 1,
            GcKind::TrimMeta => 2,
            GcKind::ShrinkPages => 3,
            GcKind::DropRel => 4,
            GcKind::DropSlru => 5,
            GcKind::DropMeta => 6,
            GcKind::DropDb => 7,
            GcKind::Unknown(v) => v,
        }
    }

    pub fn from_i16(v: i16) -> GcKind {
        match v {
            0 => GcKind::TrimPages,
            1 => GcKind::TrimSlru,
            2 => GcKind::TrimMeta,
            3 => GcKind::ShrinkPages,
            4 => GcKind::DropRel,
            5 => GcKind::DropSlru,
            6 => GcKind::DropMeta,
            7 => GcKind::DropDb,
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

/// Database identity: (spcnode, dbnode) — what a [`GcKind::DropDb`]
/// candidate names, and the `pages` key prefix its sweep binds.
pub type GcDbIdent = (i64, i64);

/// One candidate's full identity: `key`, `gc_kind` and `due_lsn`, which
/// within a branch is `pending_gc`'s whole primary key. Any kind can be
/// named this way; only drop/shrink candidates are *dequeued* by it (a trim
/// dequeues by a range sweep over `(key, gc_kind)` instead).
pub(crate) type GcCandidateId = (Key, GcKind, Lsn);

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

/// What one prefix sweep reclaims: a whole dropped database, or one dropped
/// relation's forks. Both are a contiguous `pages` range per bucket plus one
/// or more `meta` key ranges — the relation case simply binds one more key
/// column.
#[derive(Debug, Clone, Copy)]
pub enum GcSweepScope {
    Database(GcDbIdent),
    Relation(GcDbIdent, i64),
}

impl GcSweepScope {
    /// The sweep a candidate names, or `None` if its kind is not a sweep — the
    /// sole kind -> scope decision in the GC path. Reached through
    /// [`GcSweepCandidate::of`], which is what pairs the answer with the
    /// candidate it was derived from.
    fn of(key: &Key, gc_kind: GcKind) -> Option<Self> {
        let db = db_of_rel_key(key);
        match gc_kind {
            GcKind::DropDb => Some(GcSweepScope::Database(db)),
            GcKind::DropRel => Some(GcSweepScope::Relation(db, key.field4 as i64)),
            _ => None,
        }
    }

    /// The database both cases bind.
    fn db(self) -> GcDbIdent {
        match self {
            GcSweepScope::Database(db) | GcSweepScope::Relation(db, _) => db,
        }
    }

    /// The extra `pages` column a relation sweep binds.
    fn relnode(self) -> Option<i64> {
        match self {
            GcSweepScope::Database(_) => None,
            GcSweepScope::Relation(_, relnode) => Some(relnode),
        }
    }

    /// Every rel-keyspace key this sweep covers — its `pages` blocks and its
    /// `meta` size rows. Also the range a materialise job must be outside
    /// before the candidate may be dequeued.
    pub(crate) fn rel_keyspace_range(self) -> std::ops::Range<Key> {
        let (spcnode, dbnode) = self.db();
        match self {
            GcSweepScope::Database(_) => {
                pageserver_api::key::db_rel_keyspace_range(spcnode as u32, dbnode as u32)
            }
            GcSweepScope::Relation(_, relnode) => pageserver_api::key::rel_all_forks_key_range(
                spcnode as u32,
                dbnode as u32,
                relnode as u32,
            ),
        }
    }

    /// The `meta` ranges this sweep reclaims, in the order it walks them. The
    /// count is this list's length, so a cursor cannot be sized for more
    /// ranges than the sweep visits (or fewer, and stop early).
    ///
    /// A relation's sparse rel-directory entry is deliberately absent: its
    /// `Removed` marker shadows an ancestor's `Exists` under copy-on-write, so
    /// only a `DROP DATABASE` — which removes the DbDir entry and shadows the
    /// whole range from readers — may delete one.
    fn meta_ranges(self) -> Vec<std::ops::Range<Key>> {
        let (spcnode, dbnode) = self.db();
        match self {
            GcSweepScope::Database(_) => {
                pageserver_api::key::db_meta_key_ranges(spcnode as u32, dbnode as u32).to_vec()
            }
            GcSweepScope::Relation(..) => vec![self.rel_keyspace_range()],
        }
    }
}

/// A prefix-sweep candidate: its `pending_gc` identity and what its sweep
/// covers, paired by [`Self::of`] — the one place that decides whether a kind
/// sweeps at all. They travel together because a cursor filed under one
/// candidate and sized for another would sweep the wrong ranges and report
/// itself finished; nothing else can construct the pair.
#[derive(Debug, Clone, Copy)]
pub(crate) struct GcSweepCandidate {
    key: Key,
    gc_kind: GcKind,
    due_lsn: Lsn,
    scope: GcSweepScope,
}

impl GcSweepCandidate {
    /// The sweep this row names, or `None` if its kind is not a sweep. That
    /// answer is also what routes a drop candidate to its act, so which rows
    /// sweep and what each one covers cannot disagree.
    pub(crate) fn of(row: &PendingGcRow) -> Option<Self> {
        Some(Self {
            key: row.key,
            gc_kind: row.gc_kind,
            due_lsn: row.due_lsn,
            scope: GcSweepScope::of(&row.key, row.gc_kind)?,
        })
    }

    /// Its row in the queue, and the key its resume cursor is filed under.
    pub(crate) fn id(self) -> GcCandidateId {
        (self.key, self.gc_kind, self.due_lsn)
    }

    pub(crate) fn scope(self) -> GcSweepScope {
        self.scope
    }

    /// The LSN its garbage became collectable at, which bounds every delete.
    pub(crate) fn due_lsn(self) -> Lsn {
        self.due_lsn
    }
}

/// Where a prefix sweep resumes: a position per `pages`
/// bucket, then one per `meta` key range.
///
/// In memory only (`GcDrainProgress`), and the candidate stays queued until
/// [`Self::is_reclaimed`]. Losing it to a restart costs a re-scan of the
/// tombstones the deleted rows left behind, not a re-delete.
#[derive(Debug, Clone)]
pub struct GcSweepProgress {
    /// What this sweep covers. Carried so `gc_act_sweep` takes its ranges and
    /// its stopping condition from one place — a count passed in alongside
    /// could be sized for a different scope than the sweep walks.
    scope: GcSweepScope,
    pages: [GcSweepBucket; PAGE_BUCKETS as usize],
    /// Which of [`GcSweepScope::meta_ranges`] the sweep is on, or `None` once
    /// every one is done — the `meta` counterpart of a `pages` bucket reaching
    /// `Done`, so no predicate has to re-derive the range count to ask.
    meta_range: Option<usize>,
    /// Position within `meta_range`: the last batch's final key.
    meta_cursor: Option<Vec<u8>>,
    /// A tick swept this scope while one of its forks could still receive a
    /// materialised image. The cursors may have moved past a row that landed
    /// behind them at a folded LSN `<= due_lsn`, so the sweep owes one more
    /// full pass before its candidate may be dequeued.
    needs_recheck: bool,
}

/// One `pages` bucket's place in the sweep. `NotStarted` is not the same as
/// `At(zero)`: a relation sweep's first seek starts at its own `relnode`, not
/// at the bucket minimum.
#[derive(Debug, Clone, Copy, PartialEq, Eq)]
enum GcSweepBucket {
    NotStarted,
    At(GcSweepPageCursor),
    /// Its SELECT came back empty: nothing of the swept scope is left there.
    Done,
}

/// A `pages` resume position: the trailing key columns below the bound
/// `(bucket_id, branch_id, spcnode, dbnode)` prefix.
#[derive(Debug, Clone, Copy, PartialEq, Eq)]
struct GcSweepPageCursor {
    relnode: i64,
    forknum: i16,
    blknum: i64,
}

impl GcSweepProgress {
    /// A sweep of `scope` that has not started.
    pub(crate) fn new(scope: GcSweepScope) -> Self {
        Self {
            scope,
            pages: [GcSweepBucket::NotStarted; PAGE_BUCKETS as usize],
            meta_range: Some(0),
            meta_cursor: None,
            needs_recheck: false,
        }
    }

    /// The swept rows are provably gone: every range came back empty and no
    /// re-check is owed. The condition for dequeueing its candidate.
    pub fn is_reclaimed(&self) -> bool {
        self.all_ranges_done() && !self.needs_recheck
    }

    /// Require one more full pass: a fork of this database was busy while the
    /// cursors were moving, so a straggling image may sit behind them.
    pub fn require_recheck(&mut self) {
        self.needs_recheck = true;
    }

    /// Put the sweep back at the start of its first range. Moves position
    /// only: the rows already deleted stay deleted, so the next pass is
    /// `PAGE_BUCKETS + 2` seeks over ranges that are already empty, plus
    /// whatever straggler it finds.
    fn start_over(&mut self) {
        *self = Self::new(self.scope);
    }

    /// If every range came back empty but a re-check is owed, put the sweep
    /// back at the start of its first range and clear the debt. Reports
    /// whether it did, which is what tells the caller a candidate is
    /// unfinished for that reason rather than for the row budget.
    pub fn begin_owed_pass(&mut self) -> bool {
        let owed = self.all_ranges_done() && self.needs_recheck;
        if owed {
            self.start_over();
        }
        owed
    }

    /// Every `pages` bucket and every `meta` range has come back empty, so
    /// there is nothing to walk right now. Weaker than
    /// [`Self::is_reclaimed`], which also requires that no re-check is owed.
    pub fn all_ranges_done(&self) -> bool {
        self.pages_done() && self.meta_range.is_none()
    }

    /// One spelling of "every bucket is `Done`", so the sweep's loop condition
    /// and [`Self::is_reclaimed`] cannot disagree.
    fn pages_done(&self) -> bool {
        self.pages.iter().all(|b| *b == GcSweepBucket::Done)
    }
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

/// A rel key's database, as the `i64`s the `pages` columns bind. Only
/// meaningful for rel-keyspace keys (`field1 == 0`), whose field2/field3 are
/// spcnode/dbnode — an SLRU key's are its kind and a constant.
///
/// The cast is the point, and why this isn't a `Key` method: a caller building
/// another `Key` wants the raw `Oid`s instead (see `drop_target_is_busy`).
pub(crate) fn db_of_rel_key(key: &Key) -> GcDbIdent {
    (key.field2 as i64, key.field3 as i64)
}

/// Decode an SLRU key's segment identity `(kind, segno)` from its fields
/// (kind = field2, segno = field4). Only meaningful for SLRU-keyspace keys
/// (`field1 == 1`) — a block or its seg-size sentinel.
pub(crate) fn slru_seg_of(key: &Key) -> GcSlruSeg {
    (key.field2 as i16, key.field4 as i64)
}

/// The busy-defer key of a materialisable *block* key: the object's
/// sentinel key (blknum → `field6` MAX), which is exactly the key a
/// `DropSlru`/`DropRel` candidate carries — so a drop is busy iff its own
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

impl PageStore {
    /// Insert GC candidates (the single insert path — `bucket_id` is
    /// computed here, input values ignored). Replayed duplicates dedup:
    /// the candidate's identity is its PK and the write is an upsert, so a
    /// replay overwrites in place. The lone non-PK column
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
        let p = self.thinclient.current().await;
        let started = std::time::Instant::now();
        self.gc_enqueue_candidates_thin(&p, branch_id, rows).await?;
        crate::metrics::YB_STORE_OP_SECONDS
            .with_label_values(&["gc_enqueue_candidates", "yb_thin_client"])
            .observe(started.elapsed().as_secs_f64());
        Ok(())
    }
    /// Direct-RPC lane for [`PageStore::gc_enqueue_candidates`].
    ///
    /// `key_values` are in PRIMARY KEY order — `((branch_id, bucket_id)
    /// HASH, due_lsn, key, gc_kind)`; `relsize` is the lone non-key column,
    /// and it is nullable (set on shrink candidates only), so a `None` binds
    /// as `BindValue::Null` rather than being omitted.
    ///
    /// `bucket_id` is computed here by `gc_bucket_id` — it is placement only,
    /// deliberately not a generated column precisely so this lane can write
    /// the table. Unlike `pages`, `pending_gc` carries no bucket CHECK
    /// constraint, so nothing is lost by writing it client-side; a
    /// mis-computed bucket would hide a candidate from the drain's per-bucket
    /// fan-out (a missed candidate, not a lost page).
    ///
    /// The write is an upsert: a replayed candidate overwrites in place, and
    /// since `relsize` is fixed by the PK identity the overwrite stays
    /// byte-identical.
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
        // Sequential: candidate volume is a rounding error next to the page
        // writes it trails.
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

    /// Run one GC delete of stored data, charging `GC_ROWS_DELETED` as the
    /// statement commits. Per statement and not per tick because each is its
    /// own autocommit transaction: a tick that fails on its tenth statement
    /// still committed the nine before it, and a tick running for an hour has
    /// committed everything it has finished — one increment at the end of the
    /// drain reported neither (#1142).
    async fn delete_rows(
        &self,
        sql: &str,
        params: &[&(dyn tokio_postgres::types::ToSql + Sync)],
    ) -> Result<u64> {
        let deleted = self.ysql_client.execute(sql, params).await?;
        crate::metrics::GC_ROWS_DELETED.inc_by(deleted);
        Ok(deleted)
    }

    /// The dequeue's twin of [`Self::delete_rows`]: `pending_gc` rows, so
    /// they charge `GC_CANDIDATES_DEQUEUED`.
    async fn dequeue_candidates(
        &self,
        sql: &str,
        params: &[&(dyn tokio_postgres::types::ToSql + Sync)],
    ) -> Result<u64> {
        let removed = self.ysql_client.execute(sql, params).await?;
        crate::metrics::GC_CANDIDATES_DEQUEUED.inc_by(removed);
        Ok(removed)
    }

    /// Dequeue the visited trim candidates: a bucket-grouped range delete
    /// over `(floor, cutoff]`, matching each `(key, gc_kind)` pair. Same
    /// window as the select, so an object's duplicate/straggler candidates
    /// all go, not just acted ones — safe because the caller acted each to
    /// the cutoff and the materialiser clamps a late base above it (see
    /// `PendingGcBuffer`). Matching each `(key, gc_kind)` pair confines the
    /// sweep to the object's own kind: a same-`key` drop/shrink row
    /// (`rel_size_to_key` carries DropRel / ShrinkPages *and* TrimMeta)
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
                .dequeue_candidates(
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
        candidates: &[GcCandidateId],
    ) -> Result<u64> {
        if candidates.is_empty() {
            return Ok(0);
        }
        let mut by_bucket: std::collections::HashMap<i32, Vec<&GcCandidateId>> =
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
                .dequeue_candidates(&sql, &[&branch_id, &bucket, &keyb, &kinds, &due])
                .await
                .with_context(|| {
                    format!(
                        "gc_dequeue_drop_shrink_candidates branch={branch_id} bucket={bucket} n={n}"
                    )
                })?;
        }
        Ok(removed)
    }

    /// Each block's anchor: the newest base `<= $7`. The trim's only anchor
    /// computation — the DELETE takes the result as a bind rather than
    /// recomputing it, so the value stored as the next watermark is exactly
    /// the one the DELETE trimmed to.
    ///
    /// `$8` is bucket_id parallel to `$6`, not a scalar, so one statement
    /// covers a fork's blocks whatever buckets they straddle. It stays the
    /// leading index bound either way: the lateral runs per outer row, so
    /// `bucket_id = b.bucket` is a constant within each iteration.
    ///
    /// It projects `$9` straight back out. The watermark does not bound the
    /// anchor — the lateral ignores it — but returning it makes each row carry
    /// the whole range the DELETE binds for that block, so the caller need not
    /// rejoin the watermark by blknum. A block with no base `<= $7` yields no
    /// row, which is what leaves it untouched.
    ///
    /// Trap: `ORDER BY lsn DESC LIMIT 1` is load-bearing. The PK orders a
    /// block's versions newest-first, so the first `will_init` row below the
    /// cutoff is already the newest one, and the `LIMIT` is what stops the scan
    /// there rather than at the end of the block (#1133).
    fn gc_trim_blocks_anchors_sql(&self) -> String {
        format!(
            r#"SELECT b.blknum, b.watermark, x.lsn AS anchor
            FROM unnest($6::bigint[], $8::smallint[], $9::bigint[])
                 AS b(blknum, bucket, watermark)
            CROSS JOIN LATERAL (
                SELECT lsn
                FROM "{schema}".pages
                WHERE bucket_id=b.bucket AND branch_id=$1 AND spcnode=$2 AND dbnode=$3
                  AND relnode=$4 AND forknum=$5
                  AND blknum = b.blknum
                  AND lsn <= $7 AND will_init
                ORDER BY lsn DESC LIMIT 1
            ) x"#,
            schema = self.schema
        )
    }

    /// The `pages` trim statement, split out so a test can EXPLAIN it. Per
    /// `(blknum, watermark, anchor)` bound, the oldest rows in
    /// `[watermark, anchor)`, capped at `$10` across the batch, deleted by
    /// `ybctid`. Five things here are load-bearing:
    ///
    /// - `CROSS JOIN LATERAL`, not `USING unnest(...)`: it makes each key's
    ///   range an Index Cond by construction rather than by costing — see
    ///   [`TRIM_LATERAL_HINT`].
    /// - `= ANY(ARRAY(subquery))`, never `IN (subquery)`. Same rows, 70x apart:
    ///   `IN` plans as a semi-join with the Tid Scan inner (`loops=N`), a read
    ///   request *and a flush* per row. `ARRAY` makes it an InitPlan feeding one
    ///   Tid Scan node (`loops=1`).
    /// - `MATERIALIZED`, so the id scan runs once though `ids` is read twice —
    ///   by the DELETE's InitPlan and by the projection.
    /// - The projection is over `ids`, not a `DELETE ... RETURNING` set: routing
    ///   the rows out through a second materialisation costs ~80x the reported
    ///   peak memory for identical counters, and `ids` is the truthful count
    ///   anyway, where the DELETE's own under-reports when `ysql_client`
    ///   replays a committed statement.
    /// - `ORDER BY scan.lsn ASC`, against the PK's `lsn DESC`, so an `Index Scan
    ///   Backward`. Deleting oldest-first is what lets each committed statement
    ///   raise the watermark; newest-first leaves a hole and can record nothing
    ///   until the range drains.
    fn gc_trim_blocks_sql(&self) -> String {
        format!(
            r#"{hint}
            WITH ids AS MATERIALIZED (
                SELECT x.blknum AS blknum, x.lsn AS lsn, x.tid AS tid
                FROM unnest($6::bigint[], $7::bigint[], $8::bigint[])
                     AS bound(blknum, watermark, anchor)
                CROSS JOIN LATERAL (
                    SELECT scan.ybctid AS tid, scan.blknum AS blknum, scan.lsn AS lsn
                    FROM "{schema}".pages AS scan
                    WHERE scan.bucket_id=$9 AND scan.branch_id=$1 AND scan.spcnode=$2
                      AND scan.dbnode=$3 AND scan.relnode=$4 AND scan.forknum=$5
                      AND scan.blknum = bound.blknum
                      AND scan.lsn >= bound.watermark
                      AND scan.lsn < bound.anchor
                    ORDER BY scan.lsn ASC
                ) x
                LIMIT $10
            ), d AS (
                DELETE FROM "{schema}".pages AS del
                WHERE del.ybctid = ANY(ARRAY(SELECT tid FROM ids))
            )
            SELECT blknum, count(*) AS {deleted_count}, max(lsn) AS {deleted_to}
            FROM ids GROUP BY blknum"#,
            schema = self.schema,
            deleted_count = TRIM_DELETED_COUNT_COL,
            deleted_to = TRIM_DELETED_TO_COL,
            hint = TRIM_LATERAL_HINT.replace("{index}", "pages")
        )
    }

    /// Issue `target`'s trim statement until it deletes nothing, committing
    /// between statements and raising each key's watermark as it goes.
    ///
    /// The watermark may rise per statement because the statement committed and
    /// deleted the *oldest* rows in each range, so everything below what it
    /// reports is gone. Top-down could not: it leaves a hole above the
    /// watermark, so nothing is recordable until the whole range is drained and
    /// a mid-loop failure discards the lot.
    ///
    /// Stops on a statement under its cap — proof the lateral ran every bound
    /// through to its anchor. The bind arrays go to `run_statement` by value so
    /// its future owns them.
    async fn run_trim_loop<T, K, F, Fut>(
        &self,
        target: TrimTarget<T, K>,
        bounds: TrimBounds<T>,
        rows_per_statement: usize,
        cancel: &CancellationToken,
        trimmed_to: &mut Vec<(K, Lsn)>,
        run_statement: F,
    ) -> Result<TrimOutcome>
    where
        T: Eq + Hash + Clone + for<'a> FromSql<'a>,
        K: Debug,
        F: Fn(Vec<T>, Vec<i64>, Vec<i64>, i64) -> Fut,
        Fut: Future<Output = Result<Vec<Row>>>,
    {
        // Floored at 1: `LIMIT 0` deletes nothing and never comes back under
        // its cap, so the loop would spin. No ceiling — that is the operator's.
        let limit = rows_per_statement.max(1) as i64;
        let mut cursors = bounds;
        // A statement reports one row per key it deleted from, and a tick funnels
        // up to `gc_drain_limit` keys into one loop — shallow keys all fit under
        // one cap, so both counts reach 10 000 together. Scanning `cursors` per
        // reported row would be 10^8 key comparisons on the single-threaded GC
        // task with no await in them.
        let slot: HashMap<T, usize> = cursors
            .iter()
            .enumerate()
            .map(|(i, c)| (c.key.clone(), i))
            .collect();
        let mut deleted_total = 0u64;
        loop {
            if cancel.is_cancelled() {
                return Ok(TrimOutcome::Cancelled(deleted_total));
            }
            let (keys, watermarks, anchors) = TrimBound::unzip(&cursors);
            self.write_gate(target.gate_label);
            // One row per key that lost rows; none at all once drained.
            let report = run_statement(keys, watermarks, anchors, limit)
                .await
                .with_context(|| {
                    format!(
                        "{} bounds={} first={:?}",
                        target.err_context,
                        cursors.len(),
                        (target.to_watermark_key)(&cursors[0].key)
                    )
                })?;
            let mut deleted = 0u64;
            for row in &report {
                let bound_key: T = row.get(target.key_col);
                deleted += row.get::<_, i64>(TRIM_DELETED_COUNT_COL) as u64;
                // Inclusive, so the next statement re-reads that one row:
                // `is_image` follows `lsn` in both block PKs, so two rows can
                // share an `lsn` and the cap can cut between them. `meta` could
                // move past; one rule beats saving it a tombstone.
                let watermark: i64 = row.get(TRIM_DELETED_TO_COL);
                if let Some(&i) = slot.get(&bound_key) {
                    cursors[i].watermark = watermark;
                }
                // Per statement, not once at the end: this one committed, and
                // a later failure must not cost that. Last write per key wins.
                trimmed_to.push(((target.to_watermark_key)(&bound_key), Lsn(watermark as u64)));
            }
            // Per statement: each is its own autocommit transaction, so a
            // loop that fails on its tenth committed the nine before (#1142).
            crate::metrics::GC_ROWS_DELETED.inc_by(deleted);
            deleted_total += deleted;
            if deleted < limit as u64 {
                break;
            }
        }
        // The only watermark a key that reported nothing ever gets, and the
        // reason is tombstones. After an eviction or a restart a key is back at
        // `0` with a trim's worth of tombstones below its old anchor and
        // nothing live left to delete, so every statement reports it zero rows
        // and no row at all — the push above never fires for it. It could then
        // never advance, because advancing would require deleting something,
        // and it would re-walk those tombstones every tick until compaction.
        //
        // For the keys that did report, this is also the stronger claim: the
        // range is drained, so everything below the anchor has gone, which no
        // single statement's `deleted_to` can say.
        trimmed_to.extend(
            cursors
                .iter()
                .map(|c| ((target.to_watermark_key)(&c.key), Lsn(c.anchor as u64))),
        );
        Ok(TrimOutcome::Complete(deleted_total))
    }

    /// Trim one fork's listed blocks: per block, keep the newest base
    /// (`will_init`) <= CUTOFF plus everything above; delete strictly
    /// below, which leaves the same-LSN delta beside it for the next tick
    /// (see [`Self::gc_trim_blocks_sql`]). Greedy to the cutoff (not
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
    /// Watermarks in, `trimmed_to` out: each block arrives with the watermark
    /// its last successful trim left — `Lsn(0)` for one that has none, which is
    /// the whole range — and leaves with the highest watermark this trim can
    /// prove, for the caller to store. Every committed statement reports its
    /// own, so a bucket the loop did not finish still hands back the progress
    /// it made; a drained bucket reports its anchor, which is further than any
    /// single statement could say. One recorded too high strands the rows below
    /// it for good, which is why it only ever rises behind a commit.
    ///
    /// Every statement is capped at `rows_per_statement` rows across the whole
    /// batch, and each round raises a block's watermark to the highest `lsn` it
    /// deleted, until a statement comes back under its cap. So a block with no
    /// watermark needs no special handling — it is a bound whose watermark is
    /// 0, and its depth costs iterations rather than one unbounded statement
    /// (#1209).
    ///
    /// TODO(gc_whole_fork_threshold): above a per-fork block count,
    /// switch to a whole-fork variant in blknum-range batches.
    // Wide by nature: a fork identity, the blocks and their ranges, the row
    // budget, the token, and where to report the anchors reached.
    #[allow(clippy::too_many_arguments)]
    pub async fn gc_act_trim_blocks(
        &self,
        branch_id: BranchId,
        (spcnode, dbnode, relnode, forknum): GcForkIdent,
        block_watermarks: &[(i64, Lsn)],
        cutoff: Lsn,
        rows_per_statement: usize,
        cancel: &CancellationToken,
        trimmed_to: &mut Vec<(i64, Lsn)>,
    ) -> Result<TrimOutcome> {
        if block_watermarks.is_empty() {
            return Ok(TrimOutcome::Complete(0));
        }
        let delete_sql = self.gc_trim_blocks_sql();
        // A `&str` view, because the statement closure below is `Fn` — one call
        // per statement — and its `async move` body would move the `String` out
        // of it on the first.
        let delete_sql = delete_sql.as_str();
        let cutoff_i = cutoff.0 as i64;

        // One anchors read for the whole fork: `bucket_id` rides in parallel to
        // the blknums, so blocks straddling buckets need no fan-out here. Read
        // before the DELETEs, never after — the materialiser stamps an image at
        // an existing delta's LSN, so a base can appear below the cutoff
        // mid-statement, and a watermark above the anchor a DELETE used would
        // strand the rows between them.
        // One pass, three parallel binds. Their order is the contract with
        // `gc_trim_blocks_anchors_sql`'s unnest, so they are built together.
        let (all_blknums, all_buckets, all_watermarks): (Vec<i64>, Vec<i16>, Vec<i64>) =
            block_watermarks
                .iter()
                .map(|&(blk, watermark)| (blk, page_bucket_id(relnode, blk), watermark.0 as i64))
                .multiunzip();

        // Each row is one block's whole DELETE range: blknum, watermark,
        // anchor. Blocks with no base <= cutoff are absent, which is what
        // leaves them untouched.
        let bound_rows = self
            .ysql_client
            .query(
                &self.gc_trim_blocks_anchors_sql(),
                &[
                    &branch_id,
                    &spcnode,
                    &dbnode,
                    &relnode,
                    &forknum,
                    &all_blknums,
                    &cutoff_i,
                    &all_buckets,
                    &all_watermarks,
                ],
            )
            .await
            .with_context(|| {
                format!(
                    "gc_trim_blocks_anchors branch={branch_id} rel={relnode} fork={forknum} blocks={}",
                    block_watermarks.len()
                )
            })?;
        // The DELETE stays per bucket: `bucket_id` leads that table's key, so it
        // has to be a bound value for the scan to prune to that bucket's
        // tablets.
        let mut bounds_by_bucket: Vec<TrimBounds<i64>> = vec![Vec::new(); PAGE_BUCKETS as usize];
        for r in &bound_rows {
            let blknum: i64 = r.get("blknum");
            bounds_by_bucket[page_bucket_id(relnode, blknum) as usize].push(TrimBound {
                key: blknum,
                watermark: r.get("watermark"),
                anchor: r.get("anchor"),
            });
        }

        // One loop per bucket, concurrently, where the per-object fan-out above
        // can only hand a fork one worker (#1182). Two traps in reading that
        // width: `page_bucket_id` folds `blknum / PAGE_BUCKET_STRIDE`, so a
        // sub-2MB fork is one bucket and this runs a single future; and
        // `YsqlClient` releases its pool slot before awaiting, so the pool gates
        // no statement, it picks the backend — making the realised concurrency
        // `min(pool size, PAGE_BUCKETS)`, i.e. 1 at `scale_factor = 1.0`.
        let bucket_loops = bounds_by_bucket
            .into_iter()
            .enumerate()
            .filter(|(_, bounds)| !bounds.is_empty())
            .map(|(bucket, bounds)| {
                let bucket = bucket as i16;
                async move {
                    let mut bucket_marks = Vec::new();
                    let outcome = self
                        .run_trim_loop(
                            TrimTarget {
                                gate_label: "GC trim of amp_storage.pages",
                                key_col: "blknum",
                                to_watermark_key: |blk| *blk,
                                err_context: format!(
                                    "gc_act_trim_blocks branch={branch_id} rel={relnode} \
                                     fork={forknum} bucket={bucket}"
                                ),
                            },
                            bounds,
                            rows_per_statement,
                            cancel,
                            &mut bucket_marks,
                            |blknums, watermarks, anchors, limit| async move {
                                self.ysql_client
                                    .query(
                                        delete_sql,
                                        &[
                                            &branch_id,
                                            &spcnode,
                                            &dbnode,
                                            &relnode,
                                            &forknum,
                                            &blknums,
                                            &watermarks,
                                            &anchors,
                                            &bucket,
                                            &limit,
                                        ],
                                    )
                                    .await
                            },
                        )
                        .await;
                    (bucket_marks, outcome)
                }
            });
        // `collect`, not `try_collect`: a dropped sibling loses the watermarks
        // it committed, and its rows go uncounted (#1142, #1209).
        let outcomes = futures::stream::iter(bucket_loops)
            .buffer_unordered(PAGE_BUCKETS as usize)
            .collect::<Vec<_>>()
            .await;

        let mut deleted = 0u64;
        let mut cancelled = false;
        let mut failed = None;
        for (bucket_marks, outcome) in outcomes {
            trimmed_to.extend(bucket_marks);
            match outcome {
                Ok(TrimOutcome::Complete(n)) => deleted += n,
                Ok(TrimOutcome::Cancelled(n)) => {
                    deleted += n;
                    cancelled = true;
                }
                // Completion order, so which error is kept is arbitrary — log
                // each one, or the others vanish.
                Err(e) => {
                    tracing::warn!(err = ?e, rel = relnode, fork = forknum, "GC trim bucket failed");
                    failed = failed.or(Some(e));
                }
            }
        }
        match failed {
            // A sibling failing as the runtime tears down must not turn a
            // clean shutdown into a GC failure tick. Only the error path
            // consults the token: whether the ranges drained is the loops'
            // answer, and a token set after they all finished means Complete.
            Some(e) if cancel.is_cancelled() => {
                tracing::info!(
                    err = ?e, rows = deleted, rel = relnode, fork = forknum,
                    "GC trim bucket failed during shutdown; reporting cancel"
                );
                Ok(TrimOutcome::Cancelled(deleted))
            }
            Some(e) => Err(e),
            None if cancelled => Ok(TrimOutcome::Cancelled(deleted)),
            None => Ok(TrimOutcome::Complete(deleted)),
        }
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
        self.delete_rows(&sql, &[&branch_id, &keys, &(due_lsn.0 as i64)])
            .await
    }

    /// Drop action for [`GcKind::DropDb`] and [`GcKind::DropRel`]: reclaim
    /// everything under one key prefix — a whole database, or one relation's
    /// forks — bounded by `lsn <= due_lsn` so an object recreated on the same
    /// OIDs keeps its newer rows.
    ///
    /// The two differ only in how many key columns are bound and which `meta`
    /// ranges go with them ([`GcSweepScope`]): a relation is one column
    /// deeper, so its forks are one contiguous `pages` range per bucket just
    /// as a database's relations are.
    ///
    /// Deletes at most `budget` rows and advances `progress`; the caller must
    /// leave the candidate queued until `progress.is_reclaimed()`. Idempotent.
    ///
    /// `pages` goes round-robin over the buckets, one batch each per pass:
    /// `bucket_id` leads that table's key and pre-splits it into
    /// [`PAGE_BUCKETS`] tablets, so a bucket at a time would aim a whole tick
    /// at one Raft group — one that live ingest is writing to — while the
    /// other five idle. Passes repeat until the budget is spent, so a database
    /// whose rows all land in one bucket still spends it all.
    ///
    /// TODO(#1224): fan the buckets out concurrently, as `gc_act_trim_blocks`
    /// does. Blocked on the per-tick row budget becoming a per-statement cap —
    /// the passes above share a running counter against it.
    ///
    /// Why a range works at all, and the alternatives discarded on the way:
    /// `compute/architecture/pageserver-gc.md`.
    pub async fn gc_act_sweep(
        &self,
        branch_id: BranchId,
        due_lsn: Lsn,
        budget: usize,
        progress: &mut GcSweepProgress,
    ) -> Result<u64> {
        self.write_gate("GC prefix sweep of amp_storage.pages");
        let scope = progress.scope;
        let meta_ranges = scope.meta_ranges();
        // A sixth of the budget, so one pass over the buckets is a tick's work.
        let rows_per_delete = budget
            .div_ceil(PAGE_BUCKETS as usize)
            .min(GC_SWEEP_MAX_ROWS_PER_DELETE);
        let mut deleted = 0usize;

        // Buckets in a cycle, one batch each: a pass spreads the tick over all
        // six tablets, and passes repeat so a database whose rows all land in
        // one bucket still spends the whole budget. Terminates because every
        // visit either marks a bucket Done or advances its cursor — true even
        // when a retried DELETE reports 0 rows and `deleted` doesn't move.
        for bucket in (0..PAGE_BUCKETS as usize).cycle() {
            if deleted >= budget || progress.pages_done() {
                break;
            }
            let resume = match progress.pages[bucket] {
                GcSweepBucket::Done => continue,
                GcSweepBucket::NotStarted => None,
                GcSweepBucket::At(cursor) => Some(cursor),
            };
            // One state write per visit, and the assignment is deliberately the
            // outermost operation: an error inside the match skips it, so a
            // failed batch is re-read from the same cursor rather than seeking
            // past rows it never deleted.
            progress.pages[bucket] = match self
                .gc_sweep_select_pages_batch(
                    branch_id,
                    scope,
                    due_lsn,
                    bucket,
                    resume,
                    rows_per_delete.min(budget - deleted),
                )
                .await?
            {
                None => GcSweepBucket::Done,
                Some((ybctids, cursor)) => {
                    deleted += self.gc_sweep_delete_batch("pages", &ybctids).await?;
                    GcSweepBucket::At(cursor)
                }
            };
        }

        // `meta` only once every bucket is Done: the loop above exits with
        // budget left over in no other case.
        while deleted < budget {
            let Some(range) = progress.meta_range else {
                break;
            };
            match self
                .gc_sweep_select_meta_batch(
                    branch_id,
                    scope,
                    due_lsn,
                    &meta_ranges[range],
                    progress.meta_cursor.as_deref(),
                    rows_per_delete.min(budget - deleted),
                )
                .await?
            {
                // This range is empty; move to the next one, or finish.
                None => {
                    progress.meta_range = Some(range + 1).filter(|r| *r < meta_ranges.len());
                    progress.meta_cursor = None;
                }
                Some((ybctids, cursor)) => {
                    deleted += self.gc_sweep_delete_batch("meta", &ybctids).await?;
                    progress.meta_cursor = Some(cursor);
                }
            }
        }

        Ok(deleted as u64)
    }

    /// One `pages` bucket's batch: its `ybctid`s and the position to resume
    /// from — the LAST row's cursor columns, since a batch is the lowest
    /// remaining rows in key order. `None` means the bucket holds nothing of
    /// this scope at `lsn <= due_lsn`.
    ///
    /// The bound `(bucket_id, branch_id, spcnode, dbnode)` prefix — plus
    /// `relnode` for a relation scope — is what confines the scan; the row
    /// constructor, anchored at the key's first column, is what seeks past the
    /// rows already deleted. Anything else (a constructor starting mid-key,
    /// `ybctid >`) degrades to a filter over the branch. A fresh sweep binds
    /// the trailing columns at their range minimum, so the clause is a no-op
    /// until there is a cursor.
    ///
    /// The cursor is `>=`, not `>`: it stops above the table's `lsn DESC` tail
    /// (row comparisons are lexicographic-ascending), so a `LIMIT` can cut a
    /// block's versions in half and the boundary group has to be re-included.
    /// It cannot spin — progress comes from the delete.
    async fn gc_sweep_select_pages_batch(
        &self,
        branch_id: BranchId,
        scope: GcSweepScope,
        due_lsn: Lsn,
        bucket: usize,
        resume: Option<GcSweepPageCursor>,
        limit: usize,
    ) -> Result<Option<(Vec<Vec<u8>>, GcSweepPageCursor)>> {
        let (spcnode, dbnode) = scope.db();
        // `$6` is the cursor's relnode, which for a relation scope is that
        // relation: a fresh cursor seeds it from the scope, and every resume
        // cursor comes from a row this same bound admitted. So the scope needs
        // no parameter of its own.
        let rel_bound = match scope.relnode() {
            Some(_) => " AND relnode=$6",
            None => "",
        };
        let sql = format!(
            "SELECT ybctid, relnode, forknum, blknum FROM \"{schema}\".pages \
             WHERE bucket_id=$1 AND branch_id=$2 AND spcnode=$3 AND dbnode=$4 \
               AND lsn <= $5{rel_bound} \
               AND (bucket_id, branch_id, spcnode, dbnode, relnode, forknum, blknum) \
                   >= ($1, $2, $3, $4, $6, $7, $8) \
             ORDER BY relnode, forknum, blknum, lsn DESC, is_image DESC \
             LIMIT $9",
            schema = self.schema
        );
        let at = resume.unwrap_or(GcSweepPageCursor {
            relnode: scope.relnode().unwrap_or(0),
            forknum: 0,
            blknum: 0,
        });
        let rows = self
            .ysql_client
            .query(
                &sql,
                &[
                    &(bucket as i16),
                    &branch_id,
                    &spcnode,
                    &dbnode,
                    &(due_lsn.0 as i64),
                    &at.relnode,
                    &at.forknum,
                    &at.blknum,
                    &(limit as i64),
                ],
            )
            .await
            .with_context(|| {
                format!("gc_act_sweep pages branch={branch_id} scope={scope:?} bucket={bucket}")
            })?;
        let Some(last) = rows.last() else {
            return Ok(None);
        };
        let cursor = GcSweepPageCursor {
            relnode: last.get(1),
            forknum: last.get(2),
            blknum: last.get(3),
        };
        Ok(Some((rows.iter().map(|r| r.get(0)).collect(), cursor)))
    }

    /// One `meta` range's batch, the [`Self::gc_sweep_select_pages_batch`]
    /// analog. `branch_id` is the whole hash key, so binding it already bounds
    /// the statement to one tablet — the `key` range is what scopes it.
    ///
    /// Batched rather than deleted as one range statement even though a scope
    /// holds few `meta` keys: the rows are *versions*, and `put_rel_extend`
    /// writes a size row per growth record, collapsed only by `TrimMeta` —
    /// which `enable_gc` gates off, so the first sweep on a branch that ran
    /// with GC off faces the whole untrimmed history. An unbounded statement
    /// would also spend an unmeasured part of the tick's row budget.
    async fn gc_sweep_select_meta_batch(
        &self,
        branch_id: BranchId,
        scope: GcSweepScope,
        due_lsn: Lsn,
        range: &std::ops::Range<Key>,
        resume: Option<&[u8]>,
        limit: usize,
    ) -> Result<Option<(Vec<Vec<u8>>, Vec<u8>)>> {
        let start = resume.map_or_else(|| key_to_bytes(&range.start), <[u8]>::to_vec);
        let sql = format!(
            "SELECT ybctid, key FROM \"{schema}\".meta \
             WHERE branch_id=$1 AND key >= $2 AND key < $3 AND lsn <= $4 \
             ORDER BY key, lsn DESC \
             LIMIT $5",
            schema = self.schema
        );
        let rows = self
            .ysql_client
            .query(
                &sql,
                &[
                    &branch_id,
                    &start,
                    &key_to_bytes(&range.end),
                    &(due_lsn.0 as i64),
                    &(limit as i64),
                ],
            )
            .await
            .with_context(|| {
                format!("gc_act_sweep meta branch={branch_id} scope={scope:?} range={range:?}")
            })?;
        let Some(last) = rows.last() else {
            return Ok(None);
        };
        let cursor = last.get::<_, Vec<u8>>(1);
        Ok(Some((rows.iter().map(|r| r.get(0)).collect(), cursor)))
    }

    /// TidScan hint: a never-ANALYZEd table is costed as 1000 rows, and the
    /// planner drops the Tid Scan for a full scan above ~27 ids (#970).
    fn gc_sweep_delete_sql(&self, table: &str) -> String {
        format!(
            "/*+ TidScan({table}) */ DELETE FROM \"{schema}\".{table} WHERE ybctid = ANY($1::bytea[])",
            schema = self.schema
        )
    }

    /// Delete one sweep batch by `ybctid`. Idempotent — a row another tick
    /// already removed just doesn't match.
    ///
    /// Returns `ybctids.len()`, not the DELETE's count: a retried statement
    /// re-runs as a no-op and reports 0 for rows that are gone, which would
    /// under-charge the budget and let one tick sweep the whole database.
    /// `GC_ROWS_DELETED` is charged that same figure, which is why this is one
    /// of the two GC deletes outside [`Self::delete_rows`] — not because the
    /// retry under-count is special (every idempotent GC delete reports 0 on a
    /// retry), but because this is the only one whose count feeds control flow
    /// rather than just a report. The trims are the other exception: their
    /// count arrives as a projection over the ids they selected.
    async fn gc_sweep_delete_batch(&self, table: &str, ybctids: &[Vec<u8>]) -> Result<usize> {
        self.ysql_client
            .execute(&self.gc_sweep_delete_sql(table), &[&ybctids])
            .await
            .with_context(|| format!("gc_act_sweep delete from {table} n={}", ybctids.len()))?;
        crate::metrics::GC_ROWS_DELETED.inc_by(ybctids.len() as u64);
        Ok(ybctids.len())
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
        // over all of them rather than dropping the leading-column bound, and
        // concurrently — distinct buckets are distinct tablets (#1182).
        let due = due_lsn.0 as i64;
        let sql = sql.as_str();
        // `collect`, not `try_collect`: a dropped sibling's DELETE still commits
        // but skips the `GC_ROWS_DELETED` charge `delete_rows` makes after it.
        let outcomes = futures::stream::iter((0..PAGE_BUCKETS as i16).map(|bucket| async move {
            self.delete_rows(
                sql,
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
            })
        }))
        .buffer_unordered(PAGE_BUCKETS as usize)
        .collect::<Vec<_>>()
        .await;

        let mut deleted = 0u64;
        let mut failed = None;
        for outcome in outcomes {
            match outcome {
                Ok(n) => deleted += n,
                Err(e) => {
                    tracing::warn!(err = ?e, rel = relnode, fork = forknum, "GC shrink bucket failed");
                    failed = failed.or(Some(e));
                }
            }
        }
        match failed {
            Some(e) => Err(e),
            None => Ok(deleted),
        }
    }

    /// Each SLRU block's anchor, on [`Self::gc_trim_blocks_anchors_sql`]'s contract.
    /// No `bucket_id`: `branch_id` is this table's whole hash key. The
    /// `LIMIT 1` early stop matters more here — `will_init` is sparser in clog
    /// than in `pages` (#1133).
    fn gc_trim_slru_anchors_sql(&self) -> String {
        format!(
            r#"SELECT b.blknum, b.watermark, x.lsn AS anchor
            FROM unnest($4::bigint[], $6::bigint[]) AS b(blknum, watermark)
            CROSS JOIN LATERAL (
                SELECT lsn
                FROM "{schema}".slru_pages
                WHERE branch_id=$1 AND kind=$2 AND segno=$3
                  AND blknum = b.blknum
                  AND lsn <= $5 AND will_init
                ORDER BY lsn DESC LIMIT 1
            ) x"#,
            schema = self.schema
        )
    }

    /// The `slru_pages` trim statement, the [`Self::gc_trim_blocks_sql`] shape
    /// over `(kind, segno, blknum)` instead of a rel fork's key. Same traps:
    /// the lateral is what puts each block's range in the Index Cond, and
    /// `= ANY(ARRAY(...))` is what keeps the Tid Scan at `loops=1`.
    ///
    /// No bucket column — `branch_id` is the whole hash key, so binding the
    /// branch already confines the statement to one tablet.
    fn gc_trim_slru_sql(&self) -> String {
        format!(
            r#"{hint}
            WITH ids AS MATERIALIZED (
                SELECT x.blknum AS blknum, x.lsn AS lsn, x.tid AS tid
                FROM unnest($4::bigint[], $5::bigint[], $6::bigint[])
                     AS bound(blknum, watermark, anchor)
                CROSS JOIN LATERAL (
                    SELECT scan.ybctid AS tid, scan.blknum AS blknum, scan.lsn AS lsn
                    FROM "{schema}".slru_pages AS scan
                    WHERE scan.branch_id=$1 AND scan.kind=$2 AND scan.segno=$3
                      AND scan.blknum = bound.blknum
                      AND scan.lsn >= bound.watermark
                      AND scan.lsn < bound.anchor
                    ORDER BY scan.lsn ASC
                ) x
                LIMIT $7
            ), d AS (
                DELETE FROM "{schema}".slru_pages AS del
                WHERE del.ybctid = ANY(ARRAY(SELECT tid FROM ids))
            )
            SELECT blknum, count(*) AS {deleted_count}, max(lsn) AS {deleted_to}
            FROM ids GROUP BY blknum"#,
            schema = self.schema,
            deleted_count = TRIM_DELETED_COUNT_COL,
            deleted_to = TRIM_DELETED_TO_COL,
            hint = TRIM_LATERAL_HINT.replace("{index}", "slru_pages")
        )
    }

    /// Trim one SLRU segment's listed blocks: per block keep the newest base
    /// (`will_init`) `<= cutoff` and everything above, delete strictly below.
    /// Single-tablet, so no bucket fan-out. Watermarks in, anchors out in
    /// `trimmed_to`, on [`Self::gc_act_trim_blocks`]'s contract.
    ///
    /// The materialiser stamps SLRU images too, so the same-LSN delta at the
    /// anchor survives here as it does in `pages`.
    // Wide by nature — see [`Self::gc_act_trim_blocks`].
    #[allow(clippy::too_many_arguments)]
    pub async fn gc_act_trim_slru(
        &self,
        branch_id: BranchId,
        (kind, segno): GcSlruSeg,
        block_watermarks: &[(i64, Lsn)],
        cutoff: Lsn,
        rows_per_statement: usize,
        cancel: &CancellationToken,
        trimmed_to: &mut Vec<(i64, Lsn)>,
    ) -> Result<TrimOutcome> {
        if block_watermarks.is_empty() {
            return Ok(TrimOutcome::Complete(0));
        }
        let cutoff_i = cutoff.0 as i64;
        let (all_blknums, all_watermarks): (Vec<i64>, Vec<i64>) = block_watermarks
            .iter()
            .map(|&(blk, watermark)| (blk, watermark.0 as i64))
            .unzip();

        // Read before the DELETE, never after: a base can appear below the
        // cutoff mid-statement, and a watermark above the anchor the DELETE
        // used would strand the rows between them.
        let bound_rows = self
            .ysql_client
            .query(
                &self.gc_trim_slru_anchors_sql(),
                &[
                    &branch_id,
                    &kind,
                    &segno,
                    &all_blknums,
                    &cutoff_i,
                    &all_watermarks,
                ],
            )
            .await
            .with_context(|| {
                format!(
                    "gc_trim_slru_anchors branch={branch_id} kind={kind} segno={segno} blocks={}",
                    block_watermarks.len()
                )
            })?;
        // Blocks with no base <= cutoff are absent, which is what leaves them
        // untouched.
        let bounds: TrimBounds<i64> = bound_rows
            .iter()
            .map(|r| TrimBound {
                key: r.get::<_, i64>("blknum"),
                watermark: r.get::<_, i64>("watermark"),
                anchor: r.get::<_, i64>("anchor"),
            })
            .collect();

        if bounds.is_empty() {
            return Ok(TrimOutcome::Complete(0));
        }
        let slru_sql = self.gc_trim_slru_sql();
        // A `&str` view, because the statement closure below is `Fn` — one call
        // per statement — and its `async move` body would move the `String` out
        // of it on the first.
        let slru_sql = slru_sql.as_str();
        self.run_trim_loop(
            TrimTarget {
                gate_label: "GC trim of amp_storage.slru_pages",
                key_col: "blknum",
                to_watermark_key: |blk| *blk,
                err_context: format!(
                    "gc_act_trim_slru branch={branch_id} kind={kind} segno={segno}"
                ),
            },
            bounds,
            rows_per_statement,
            cancel,
            trimmed_to,
            |blknums, watermarks, anchors, limit| async move {
                self.ysql_client
                    .query(
                        slru_sql,
                        &[
                            &branch_id,
                            &kind,
                            &segno,
                            &blknums,
                            &watermarks,
                            &anchors,
                            &limit,
                        ],
                    )
                    .await
            },
        )
        .await
    }

    /// Drop action for a truncated SLRU segment: delete every `slru_pages`
    /// row for `(kind, segno)` with `lsn <= due_lsn`, plus the segment's
    /// size-sentinel `meta` key (`seg_size_key` bytes) up to the same LSN.
    /// `<= due_lsn` keeps a wraparound-recreated segno (written above the
    /// truncation) alive. Idempotent. Returns rows deleted across both.
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
            .delete_rows(&pages_sql, &[&branch_id, &kind, &segno, &due])
            .await
            .with_context(|| {
                format!("gc_act_drop_slru pages branch={branch_id} kind={kind} segno={segno}")
            })?;
        let meta_deleted = self
            .delete_meta_up_to_lsn(branch_id, std::slice::from_ref(&seg_size_key), due_lsn)
            .await
            .with_context(|| {
                format!("gc_act_drop_slru meta branch={branch_id} kind={kind} segno={segno}")
            })?;
        Ok(deleted + meta_deleted)
    }

    /// Each `meta` key's anchor: its newest version `<= $3`. No `will_init`
    /// test (every `meta` put is a base) and no `bucket_id`.
    fn gc_trim_meta_anchors_sql(&self) -> String {
        format!(
            r#"SELECT b.key, b.watermark, x.lsn AS anchor
            FROM unnest($2::bytea[], $4::bigint[]) AS b(key, watermark)
            CROSS JOIN LATERAL (
                SELECT lsn
                FROM "{schema}".meta
                WHERE branch_id=$1 AND key = b.key AND lsn <= $3
                ORDER BY lsn DESC LIMIT 1
            ) x"#,
            schema = self.schema
        )
    }

    /// The `meta` trim statement — same shape and hint as
    /// [`Self::gc_trim_blocks_sql`]; `$2`/`$4`/`$5` are the key / watermark /
    /// anchor arrays.
    ///
    /// Trap: `<` here is a correctness requirement, not the block trims' cost
    /// trade. Every `meta` put is a base, so there is no same-LSN delta beside
    /// the anchor and `<=` would delete the anchor row itself — the key's live
    /// value.
    fn gc_trim_meta_sql(&self) -> String {
        format!(
            r#"{hint}
            WITH ids AS MATERIALIZED (
                SELECT x.key AS key, x.lsn AS lsn, x.tid AS tid
                FROM unnest($2::bytea[], $3::bigint[], $4::bigint[])
                     AS bound(key, watermark, anchor)
                CROSS JOIN LATERAL (
                    SELECT scan.ybctid AS tid, scan.key AS key, scan.lsn AS lsn
                    FROM "{schema}".meta AS scan
                    WHERE scan.branch_id=$1
                      AND scan.key = bound.key
                      AND scan.lsn >= bound.watermark
                      AND scan.lsn < bound.anchor
                    ORDER BY scan.lsn ASC
                ) x
                LIMIT $5
            ), d AS (
                DELETE FROM "{schema}".meta AS del
                WHERE del.ybctid = ANY(ARRAY(SELECT tid FROM ids))
            )
            SELECT key, count(*) AS {deleted_count}, max(lsn) AS {deleted_to}
            FROM ids GROUP BY key"#,
            schema = self.schema,
            deleted_count = TRIM_DELETED_COUNT_COL,
            deleted_to = TRIM_DELETED_TO_COL,
            hint = TRIM_LATERAL_HINT.replace("{index}", "meta")
        )
    }

    /// Trim `meta` keys: per key keep the newest version `<= cutoff` and
    /// delete below. A key with no version `<= cutoff` is absent from the
    /// anchors result, so untouched. Watermarks in, anchors out in
    /// `trimmed_to`, on [`Self::gc_act_trim_blocks`]'s contract.
    ///
    /// Trap: the watermark is the previous anchor and that row is still live,
    /// which is why the range is half-open.
    pub async fn gc_act_trim_meta(
        &self,
        branch_id: BranchId,
        key_watermarks: &[(Key, Lsn)],
        cutoff: Lsn,
        rows_per_statement: usize,
        cancel: &CancellationToken,
        trimmed_to: &mut Vec<(Key, Lsn)>,
    ) -> Result<TrimOutcome> {
        if key_watermarks.is_empty() {
            return Ok(TrimOutcome::Complete(0));
        }
        let cutoff_i = cutoff.0 as i64;
        let (all_key_bytes, all_watermarks): (Vec<Vec<u8>>, Vec<i64>) = key_watermarks
            .iter()
            .map(|(key, watermark)| (key_to_bytes(key), watermark.0 as i64))
            .unzip();

        let bound_rows = self
            .ysql_client
            .query(
                &self.gc_trim_meta_anchors_sql(),
                &[&branch_id, &all_key_bytes, &cutoff_i, &all_watermarks],
            )
            .await
            .with_context(|| {
                format!(
                    "gc_trim_meta_anchors branch={branch_id} keys={}",
                    key_watermarks.len()
                )
            })?;
        // A key with no version <= cutoff is absent, which is what leaves it
        // untouched.
        let bounds: TrimBounds<Vec<u8>> = bound_rows
            .iter()
            .map(|r| TrimBound {
                key: r.get::<_, Vec<u8>>("key"),
                watermark: r.get::<_, i64>("watermark"),
                anchor: r.get::<_, i64>("anchor"),
            })
            .collect();

        if bounds.is_empty() {
            return Ok(TrimOutcome::Complete(0));
        }
        let meta_sql = self.gc_trim_meta_sql();
        // A `&str` view, because the statement closure below is `Fn` — one call
        // per statement — and its `async move` body would move the `String` out
        // of it on the first.
        let meta_sql = meta_sql.as_str();
        self.run_trim_loop(
            TrimTarget {
                gate_label: "GC trim of amp_storage.meta",
                key_col: "key",
                to_watermark_key: |k: &Vec<u8>| Key::from_slice(k),
                err_context: format!("gc_act_trim_meta branch={branch_id}"),
            },
            bounds,
            rows_per_statement,
            cancel,
            trimmed_to,
            |keys, watermarks, anchors, limit| async move {
                self.ysql_client
                    .query(
                        meta_sql,
                        &[&branch_id, &keys, &watermarks, &anchors, &limit],
                    )
                    .await
            },
        )
        .await
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
}

#[cfg(test)]
mod tests {
    use super::*;
    use crate::page_store::test_support::*;
    use pageserver_api::key::{rel_block_to_key, rel_size_to_key};
    use pageserver_api::reltag::RelTag;

    /// GC candidates enqueued on the hot-path lane must be exactly what the
    /// YSQL drain then finds. This is the contract that matters for
    /// `pending_gc`: the writer computes the placement
    /// `bucket_id` client-side (`gc_bucket_id`) and the drain fans out
    /// per bucket, so a lane that bucketed differently would leave
    /// candidates invisible — silently, since a missed candidate just
    /// looks like an empty queue. Covers both `relsize` states: `None`
    /// (trim, binds NULL) and `Some` (shrink), the nullable non-key
    /// column being the one thing the upsert encoding could drop.
    #[tokio::test]
    async fn thin_lane_gc_enqueue_matches_ysql_drain() {
        let (pg, _) = make_store("thin_gc").await;
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
        let mut found = all_pending(&pg, branch).await;
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
            all_pending(&pg, branch).await.len(),
            batch.len(),
            "replayed thin-lane inserts must dedup on the PK"
        );

        assert!(
            thin_op_count("gc_enqueue_candidates") > before,
            "gc_enqueue_candidates did not reach the lane"
        );
    }

    const GC_SPC: i64 = 1663;
    const GC_DB: i64 = 5;
    /// The watermark each key ends a call with. `trimmed_to` carries a key's
    /// progress once per committed statement — that is the point, so a failure
    /// mid-loop keeps what went — and the caller's map keeps the last write per
    /// key. So the last value is the contract a test should assert, not the
    /// whole sequence.
    fn final_watermarks<K: Clone + Eq + std::hash::Hash + Ord>(
        trimmed: &[(K, Lsn)],
    ) -> Vec<(K, Lsn)> {
        let mut last: HashMap<K, Lsn> = HashMap::new();
        for (k, lsn) in trimmed {
            last.insert(k.clone(), *lsn);
        }
        let mut v: Vec<(K, Lsn)> = last.into_iter().collect();
        v.sort();
        v
    }

    /// A token no test cancels — the trim tests are not about shutdown.
    fn never_cancelled() -> CancellationToken {
        CancellationToken::new()
    }
    /// `rows_per_statement` for the trim tests that are not about batching:
    /// comfortably above the handful of versions they write, so a
    /// watermark-less block's bounded path runs as one statement.
    const TRIM_UNBATCHED: usize = 1_000;

    impl PageStore {
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

        /// Test-only: how much of one database is still stored — its
        /// `pages` rows, and its `meta` rows across both of the key prefixes
        /// a db drop sweeps.
        async fn test_db_row_counts(
            &self,
            branch_id: BranchId,
            spcnode: i64,
            dbnode: i64,
        ) -> Result<(i64, i64)> {
            let pages_sql = format!(
                "SELECT count(*) FROM \"{}\".pages \
                 WHERE branch_id=$1 AND spcnode=$2 AND dbnode=$3",
                self.schema
            );
            let pages: i64 = self
                .ysql_client
                .query(&pages_sql, &[&branch_id, &spcnode, &dbnode])
                .await
                .context("test_db_row_counts pages")?[0]
                .get(0);
            let meta_sql = format!(
                "SELECT count(*) FROM \"{}\".meta \
                 WHERE branch_id=$1 AND ((key >= $2 AND key < $3) OR (key >= $4 AND key < $5))",
                self.schema
            );
            // The keyspace definition is shared with the sweep; its batching
            // and stepping — what this test exercises — is not.
            let [rel, sparse] =
                pageserver_api::key::db_meta_key_ranges(spcnode as u32, dbnode as u32);
            let meta: i64 = self
                .ysql_client
                .query(
                    &meta_sql,
                    &[
                        &branch_id,
                        &key_to_bytes(&rel.start),
                        &key_to_bytes(&rel.end),
                        &key_to_bytes(&sparse.start),
                        &key_to_bytes(&sparse.end),
                    ],
                )
                .await
                .context("test_db_row_counts meta")?[0]
                .get(0);
            Ok((pages, meta))
        }

        /// Test-only: one database's remaining `pages` rows per `bucket_id`,
        /// which is what shows whether a sweep spread over the table's tablets
        /// or drained one at a time.
        async fn test_db_pages_per_bucket(
            &self,
            branch_id: BranchId,
            spcnode: i64,
            dbnode: i64,
        ) -> Result<Vec<i64>> {
            let sql = format!(
                "SELECT bucket_id, count(*) FROM \"{}\".pages \
                 WHERE branch_id=$1 AND spcnode=$2 AND dbnode=$3 \
                 GROUP BY bucket_id",
                self.schema
            );
            let rows = self
                .ysql_client
                .query(&sql, &[&branch_id, &spcnode, &dbnode])
                .await
                .context("test_db_pages_per_bucket")?;
            let mut out = vec![0i64; PAGE_BUCKETS as usize];
            for r in rows {
                out[r.get::<_, i16>(0) as usize] = r.get(1);
            }
            Ok(out)
        }

        /// Meta analog of [`Self::test_raw_block_versions`]: one key's stored
        /// LSNs, ordered.
        async fn test_raw_meta_versions(
            &self,
            branch_id: BranchId,
            key: &[u8],
        ) -> Result<Vec<u64>> {
            let sql = format!(
                "SELECT lsn FROM \"{}\".meta \
                 WHERE branch_id=$1 AND key=$2 ORDER BY lsn",
                self.schema
            );
            let key = key.to_vec();
            Ok(self
                .ysql_client
                .query(&sql, &[&branch_id, &key])
                .await?
                .iter()
                .map(|r| r.get::<_, i64>(0) as u64)
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
            (rel_size_to_key(gc_rel(relnode, forknum)), GcKind::DropRel)
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
            GcKind::DropRel | GcKind::DropSlru | GcKind::DropMeta => -2,
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
        for v in [0i16, 1, 2, 3, 4, 5, 6, 7, 8, -1, 42, i16::MAX, i16::MIN] {
            assert_eq!(GcKind::from_i16(v).to_i16(), v, "roundtrip for {v}");
        }
        assert_eq!(GcKind::from_i16(0), GcKind::TrimPages);
        assert_eq!(GcKind::from_i16(4), GcKind::DropRel);
        assert_eq!(GcKind::from_i16(6), GcKind::DropMeta);
        assert_eq!(GcKind::from_i16(7), GcKind::DropDb);
        assert_eq!(GcKind::from_i16(8), GcKind::Unknown(8));
        assert_eq!(GcKind::TrimPages.to_i16(), 0);
        assert_eq!(GcKind::DropRel.to_i16(), 4);
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
        assert_eq!(key_hash(&blk(1663, 5, 16400, 1)), 0xcd46_8f83_9ce4_ca9e);

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
        use pageserver_api::reltag::SlruKind;

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
                    (rel_size_to_key(gc_rel(16401, 0)), GcKind::DropRel, Lsn(10)),
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
    ///   lsn 70  plain delta  (same LSN)  → KEPT — the range stops below
    ///                                      the anchor, so the delta the
    ///                                      materialiser stamped over
    ///                                      survives to the next tick,
    ///                                      shadowed by the image (#1153)
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

        let _ = store
            .gc_act_trim_blocks(
                branch,
                (GC_SPC, GC_DB, rel as i64, 0),
                &[(blk_a as i64, Lsn(0)), (blk_b as i64, Lsn(0))],
                Lsn(100),
                TRIM_UNBATCHED,
                &never_cancelled(),
                &mut Vec::new(),
            )
            .await
            .unwrap();

        let a = store
            .test_raw_block_versions(branch, GC_SPC, GC_DB, rel as i64, 0, blk_a as i64)
            .await
            .unwrap();
        assert_eq!(
            a,
            vec![
                (70, false, false),
                (70, true, true),
                (90, false, false),
                (120, false, false)
            ],
            "block A must keep anchor + its same-LSN delta + everything above"
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

    /// Scenario: a fork whose blocks straddle every bucket, so the fan-out runs
    /// wide — every other trim test here uses single-digit blknums, which
    /// `page_bucket_id` puts in one bucket, so they exercise it at width 1.
    #[tokio::test]
    async fn gc_trim_blocks_spans_every_bucket() {
        let (store, _schema) = make_store("gc_trim_buckets").await;
        let branch = rand_branch();
        let rel = 16460u32;
        // One block per stretch: the relnode salt is constant, so `blknum /
        // stride` walking 0..6 is what spreads them.
        let blks: Vec<u32> = (0..PAGE_BUCKETS as u32)
            .map(|stretch| stretch * PAGE_BUCKET_STRIDE as u32 + 3)
            .collect();
        let buckets: std::collections::BTreeSet<i16> = blks
            .iter()
            .map(|b| page_bucket_id(rel as i64, *b as i64))
            .collect();
        assert_eq!(
            buckets.len(),
            PAGE_BUCKETS as usize,
            "fixture must straddle every bucket or it tests nothing"
        );

        let mut rows = Vec::new();
        for blk in &blks {
            let key = page_key(GC_SPC as u32, GC_DB as u32, rel, 0, *blk);
            rows.push(page_row(key.clone(), 10, false, false, b"d10"));
            rows.push(page_row(key.clone(), 30, true, true, b"i30"));
            rows.push(page_row(key, 50, false, false, b"d50"));
        }
        store.put_pages(branch, &rows).await.unwrap();

        let watermarks: Vec<(i64, Lsn)> = blks.iter().map(|b| (*b as i64, Lsn(0))).collect();
        let mut trimmed_to = Vec::new();
        let deleted = store
            .gc_act_trim_blocks(
                branch,
                (GC_SPC, GC_DB, rel as i64, 0),
                &watermarks,
                Lsn(100),
                TRIM_UNBATCHED,
                &never_cancelled(),
                &mut trimmed_to,
            )
            .await
            .unwrap();
        assert_eq!(
            deleted,
            TrimOutcome::Complete(PAGE_BUCKETS as u64),
            "one sub-anchor row per block, summed across buckets"
        );

        let final_marks = final_watermarks(&trimmed_to);
        assert_eq!(
            final_marks,
            blks.iter()
                .map(|b| (*b as i64, Lsn(30)))
                .collect::<Vec<_>>(),
            "each block's watermark must come back at its anchor"
        );

        for blk in &blks {
            let versions = store
                .test_raw_block_versions(branch, GC_SPC, GC_DB, rel as i64, 0, *blk as i64)
                .await
                .unwrap();
            assert_eq!(
                versions,
                vec![(30, true, true), (50, false, false)],
                "block {blk} keeps its anchor and everything above it"
            );
        }
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
        let _ = store
            .gc_act_trim_blocks(
                branch,
                (GC_SPC, GC_DB, rel as i64, 0),
                &[(blk as i64, Lsn(0))],
                Lsn(100),
                TRIM_UNBATCHED,
                &never_cancelled(),
                &mut Vec::new(),
            )
            .await
            .unwrap();

        let left = store
            .test_raw_block_versions(branch, GC_SPC, GC_DB, rel as i64, 0, blk as i64)
            .await
            .unwrap();
        assert_eq!(left, vec![(70, false, true)], "init delta is the anchor");
    }

    /// Two consecutive trims of one block, the second bounded by the
    /// watermark the first left (#1148). The second must delete exactly the
    /// rows between the two anchors — including the first anchor row itself,
    /// which sits at the watermark and is the obvious off-by-one here.
    #[tokio::test]
    async fn gc_trim_watermark_spans_consecutive_anchors() {
        let (store, _schema) = make_store("gc_trim_wm").await;
        let branch = rand_branch();
        let rel = 16430u32;
        let blk = 4u32;
        let key = page_key(GC_SPC as u32, GC_DB as u32, rel, 0, blk);

        store
            .put_pages(
                branch,
                &[
                    page_row(key.clone(), 10, true, true, b"i10"),
                    page_row(key.clone(), 20, false, false, b"d20"),
                    page_row(key.clone(), 30, true, true, b"i30"),
                    page_row(key.clone(), 40, false, false, b"d40"),
                    page_row(key.clone(), 50, true, true, b"i50"),
                    page_row(key.clone(), 60, false, false, b"d60"),
                ],
            )
            .await
            .unwrap();

        let fork = (GC_SPC, GC_DB, rel as i64, 0);
        let versions =
            || store.test_raw_block_versions(branch, GC_SPC, GC_DB, rel as i64, 0, blk as i64);

        // Tick 1, no watermark: the whole range below the anchor at 30.
        let mut wm = Vec::new();
        let _ = store
            .gc_act_trim_blocks(
                branch,
                fork,
                &[(blk as i64, Lsn(0))],
                Lsn(35),
                TRIM_UNBATCHED,
                &never_cancelled(),
                &mut wm,
            )
            .await
            .unwrap();
        assert_eq!(
            final_watermarks(&wm),
            vec![(blk as i64, Lsn(30))],
            "the anchor is the watermark"
        );
        assert_eq!(
            versions().await.unwrap(),
            vec![
                (30, true, true),
                (40, false, false),
                (50, true, true),
                (60, false, false)
            ]
        );

        // Tick 2, bounded below by 30: deletes 30 and 40, the rows between the
        // two anchors.
        let mut wm2 = Vec::new();
        let _ = store
            .gc_act_trim_blocks(
                branch,
                fork,
                &[(blk as i64, Lsn(30))],
                Lsn(55),
                TRIM_UNBATCHED,
                &never_cancelled(),
                &mut wm2,
            )
            .await
            .unwrap();
        assert_eq!(final_watermarks(&wm2), vec![(blk as i64, Lsn(50))]);
        assert_eq!(
            versions().await.unwrap(),
            vec![(50, true, true), (60, false, false)],
            "the previous anchor row goes with the range above it"
        );
    }

    /// A batch of blocks with different watermarks, in one statement (blocks 1
    /// and 2 share a bucket — `PAGE_BUCKET_STRIDE` = 256). Two things: each
    /// block is bounded by its own watermark rather than by the batch's floor
    /// (`$10`, which is only there to bound the scan if the plan slips), and a
    /// row below that watermark survives — which is why the watermark may only
    /// ever be set from an anchor a statement committed to, since one set too
    /// high strands those rows for good.
    #[tokio::test]
    async fn gc_trim_watermark_is_per_block_within_a_batch() {
        let (store, _schema) = make_store("gc_trim_wm_batch").await;
        let branch = rand_branch();
        let rel = 16450u32;
        let (blk_a, blk_b) = (1u32, 2u32);
        let key = |blk: u32| page_key(GC_SPC as u32, GC_DB as u32, rel, 0, blk);

        store
            .put_pages(
                branch,
                &[
                    // A, watermark 20, anchor 30: only lsn 20 is in range.
                    page_row(key(blk_a), 10, true, true, b"a10"),
                    page_row(key(blk_a), 20, false, false, b"a20"),
                    page_row(key(blk_a), 30, true, true, b"a30"),
                    page_row(key(blk_a), 40, false, false, b"a40"),
                    // B, watermark 60, anchor 60: nothing is. lsn 50 is the
                    // discriminator — above the batch floor of 20 and below
                    // B's anchor, so the floor alone would delete it.
                    page_row(key(blk_b), 10, true, true, b"b10"),
                    page_row(key(blk_b), 50, false, false, b"b50"),
                    page_row(key(blk_b), 60, true, true, b"b60"),
                    page_row(key(blk_b), 70, false, false, b"b70"),
                ],
            )
            .await
            .unwrap();

        let mut wm = Vec::new();
        let _ = store
            .gc_act_trim_blocks(
                branch,
                (GC_SPC, GC_DB, rel as i64, 0),
                &[(blk_a as i64, Lsn(20)), (blk_b as i64, Lsn(60))],
                Lsn(100),
                TRIM_UNBATCHED,
                &never_cancelled(),
                &mut wm,
            )
            .await
            .unwrap();
        assert_eq!(
            final_watermarks(&wm),
            vec![(blk_a as i64, Lsn(30)), (blk_b as i64, Lsn(60))]
        );
        assert_eq!(
            store
                .test_raw_block_versions(branch, GC_SPC, GC_DB, rel as i64, 0, blk_a as i64)
                .await
                .unwrap(),
            vec![(10, true, true), (30, true, true), (40, false, false)],
            "A is trimmed to its own watermark"
        );
        assert_eq!(
            store
                .test_raw_block_versions(branch, GC_SPC, GC_DB, rel as i64, 0, blk_b as i64)
                .await
                .unwrap(),
            vec![
                (10, true, true),
                (50, false, false),
                (60, true, true),
                (70, false, false)
            ],
            "B keeps lsn 50: above the batch floor, below its own watermark"
        );
    }

    /// A watermark-less block and a watermarked one in the same bucket, one
    /// call. A watermark-less block is just a bound with `watermark = 0`, and
    /// `bound.watermark` is per bound, so one shared statement gets both
    /// blocks' floors right — this is why the trim needs no separate path for
    /// the watermark-less case (#1160, #1209).
    #[tokio::test]
    async fn gc_trim_deletes_a_watermarkless_block_beside_a_watermarked_one() {
        let (store, _schema) = make_store("gc_trim_cold").await;
        let branch = rand_branch();
        let rel = 16460u32;
        let (blk_a, blk_b) = (1u32, 2u32);
        let key = |blk: u32| page_key(GC_SPC as u32, GC_DB as u32, rel, 0, blk);

        store
            .put_pages(
                branch,
                &[
                    // A, no watermark: the whole range below its anchor at 30.
                    page_row(key(blk_a), 10, true, true, b"a10"),
                    page_row(key(blk_a), 20, false, false, b"a20"),
                    page_row(key(blk_a), 30, true, true, b"a30"),
                    page_row(key(blk_a), 40, false, false, b"a40"),
                    // B, watermark 50, anchor 60. lsn 45 is the discriminator:
                    // below B's watermark but above A's floor of 0, so a shared
                    // statement would have taken it.
                    page_row(key(blk_b), 10, true, true, b"b10"),
                    page_row(key(blk_b), 45, false, false, b"b45"),
                    page_row(key(blk_b), 50, true, true, b"b50"),
                    page_row(key(blk_b), 55, false, false, b"b55"),
                    page_row(key(blk_b), 60, true, true, b"b60"),
                    page_row(key(blk_b), 70, false, false, b"b70"),
                ],
            )
            .await
            .unwrap();

        let mut wm = Vec::new();
        let _ = store
            .gc_act_trim_blocks(
                branch,
                (GC_SPC, GC_DB, rel as i64, 0),
                &[(blk_a as i64, Lsn(0)), (blk_b as i64, Lsn(50))],
                Lsn(100),
                TRIM_UNBATCHED,
                &never_cancelled(),
                &mut wm,
            )
            .await
            .unwrap();
        assert_eq!(
            final_watermarks(&wm),
            vec![(blk_a as i64, Lsn(30)), (blk_b as i64, Lsn(60))]
        );
        assert_eq!(
            store
                .test_raw_block_versions(branch, GC_SPC, GC_DB, rel as i64, 0, blk_a as i64)
                .await
                .unwrap(),
            vec![(30, true, true), (40, false, false)],
            "A is trimmed from 0"
        );
        assert_eq!(
            store
                .test_raw_block_versions(branch, GC_SPC, GC_DB, rel as i64, 0, blk_b as i64)
                .await
                .unwrap(),
            vec![
                (10, true, true),
                (45, false, false),
                (60, true, true),
                (70, false, false)
            ],
            "B keeps lsn 10 and 45: below its own watermark"
        );
    }

    /// A watermark-less block deeper than one statement's row budget: the
    /// bounded loop must still leave exactly the anchor and everything above
    /// it, and must cross a same-LSN image/delta pair that a batch boundary
    /// falls inside (#1209).
    ///
    /// `rows_per_statement` of 2 against 9 deletable rows, with the tie group
    /// at lsn 20 — the second and third rows ascending, so a cap of 2 cuts
    /// between them. A strict `lsn >` cursor would then skip the group's image
    /// and strand it, which is what the surviving-rows assertion catches. At
    /// lsn 30 the group would sit at positions 3-4 and be taken whole, and the
    /// test would pass either way.
    #[tokio::test]
    async fn gc_trim_bounds_a_deep_watermarkless_block() {
        let (store, _schema) = make_store("gc_trim_batched").await;
        let branch = rand_branch();
        let rel = 16480u32;
        let blk = 3u32;
        let fork = (GC_SPC, GC_DB, rel as i64, 0);
        let key = page_key(GC_SPC as u32, GC_DB as u32, rel, 0, blk);

        // 10, 20 (delta + the image the materialiser stamped over it), 30, 40,
        // 50, 60, 70, 80 below the anchor; 90 is the anchor; 100 above it.
        let mut rows = vec![page_row(key.clone(), 10, true, true, b"i10")];
        for lsn in [20u64, 30, 40, 50, 60, 70, 80] {
            rows.push(page_row(key.clone(), lsn, false, false, b"d"));
        }
        rows.push(page_row(key.clone(), 20, true, true, b"i20"));
        rows.push(page_row(key.clone(), 90, true, true, b"i90"));
        rows.push(page_row(key.clone(), 100, false, false, b"d100"));
        store.put_pages(branch, &rows).await.unwrap();

        let mut wm = Vec::new();
        let _ = store
            .gc_act_trim_blocks(
                branch,
                fork,
                &[(blk as i64, Lsn(0))],
                Lsn(95),
                2,
                &never_cancelled(),
                &mut wm,
            )
            .await
            .unwrap();

        assert_eq!(
            final_watermarks(&wm),
            vec![(blk as i64, Lsn(90))],
            "the watermark is the anchor, however many statements it took"
        );
        assert_eq!(
            store
                .test_raw_block_versions(branch, GC_SPC, GC_DB, rel as i64, 0, blk as i64)
                .await
                .unwrap(),
            vec![(90, true, true), (100, false, false)],
            "the batched path deletes the same rows the predicate DELETE would"
        );
    }

    /// A cancelled bounded trim stops between statements and keeps what it
    /// committed, rather than running a deep block to completion (#1209). One
    /// row a statement against a token cancelled up front, so nothing runs.
    #[tokio::test]
    async fn gc_trim_bounded_stops_on_cancel() {
        let (store, _schema) = make_store("gc_trim_cancel").await;
        let branch = rand_branch();
        let rel = 16490u32;
        let blk = 4u32;
        let fork = (GC_SPC, GC_DB, rel as i64, 0);
        let key = page_key(GC_SPC as u32, GC_DB as u32, rel, 0, blk);

        let mut rows = vec![page_row(key.clone(), 10, true, true, b"i10")];
        for lsn in [20u64, 30, 40] {
            rows.push(page_row(key.clone(), lsn, false, false, b"d"));
        }
        rows.push(page_row(key.clone(), 50, true, true, b"i50"));
        store.put_pages(branch, &rows).await.unwrap();

        let cancel = CancellationToken::new();
        cancel.cancel();
        let mut wm = Vec::new();
        let outcome = store
            .gc_act_trim_blocks(
                branch,
                fork,
                &[(blk as i64, Lsn(0))],
                Lsn(55),
                1,
                &cancel,
                &mut wm,
            )
            .await
            .expect("cancellation is an outcome, not an error");
        assert_eq!(
            outcome,
            TrimOutcome::Cancelled(0),
            "a cancelled trim must say so, and report what it committed"
        );
        assert!(
            wm.is_empty(),
            "no anchor may be stored for a block the loop did not finish"
        );
        assert_eq!(
            store
                .test_raw_block_versions(branch, GC_SPC, GC_DB, rel as i64, 0, blk as i64)
                .await
                .unwrap(),
            vec![
                (10, true, true),
                (20, false, false),
                (30, false, false),
                (40, false, false),
                (50, true, true)
            ],
            "the token was cancelled before the first statement, so nothing went"
        );
    }

    /// Rel-size meta row (LE u32 payload) for the drop/shrink tests.
    fn rel_size_row(relnode: u32, forknum: u8, lsn: u64, nblocks: u32) -> MetaRow {
        MetaRow {
            key: Bytes::from(key_to_bytes(&rel_size_to_key(gc_rel(
                relnode as i64,
                forknum as i16,
            )))),
            lsn: Lsn(lsn),
            payload: Bytes::copy_from_slice(&nblocks.to_le_bytes()),
        }
    }

    /// A full batch's DELETE is a Tid Scan on a table nothing has ANALYZEd —
    /// the state every universe starts in (#970).
    #[tokio::test]
    async fn gc_sweep_delete_plans_a_tid_scan() {
        let (store, _schema) = make_store("gc_sweep_plan").await;
        let ids: Vec<Vec<u8>> = (0..GC_SWEEP_MAX_ROWS_PER_DELETE as u64)
            .map(|i| i.to_be_bytes().to_vec())
            .collect();
        for table in ["pages", "meta"] {
            let plan = explain(&store.gc_sweep_delete_sql(table), &[&ids]).await;
            assert!(
                plan.contains("Tid Scan"),
                "{table}: DELETE of {} ybctids is not a Tid Scan:\n{plan}",
                ids.len()
            );
        }
    }

    /// EXPLAIN `sql` on the plan tests' own connection, where pg_hint_plan
    /// fails any statement whose hint it cannot bind. Returns the plan as one
    /// string — the tests assert on node names.
    ///
    /// pg_hint_plan silently no-ops a hint it cannot bind — a stale alias, a
    /// typo, a directive it does not understand. For these statements that is a
    /// large regression nothing would report, and asserting the hint text is
    /// present does not catch it, so every plan assertion is vacuous unless
    /// `yb_bad_hint_mode` is `error` in the session that runs the EXPLAIN.
    ///
    /// Hence the dedicated client. `make_store` hands every test in this binary
    /// one process-wide `PageStore` whose pool is a single connection, so
    /// setting that GUC through it makes it global for as long as it is set —
    /// pg_hint_plan then rejects an unrelated test's statement that carried no
    /// hint at all, whichever test runs next. The readback stays: a reconnect
    /// between the `SET` and the EXPLAIN would silently drop it.
    async fn explain(sql: &str, params: &[&(dyn tokio_postgres::types::ToSql + Sync)]) -> String {
        static PLAN_CLIENT: tokio::sync::OnceCell<ysql_client::YsqlClient> =
            tokio::sync::OnceCell::const_new();
        let client = PLAN_CLIENT
            .get_or_init(|| async {
                ysql_client::YsqlClient::from_yb_config(
                    &super::test_support::test_yb_config(),
                    "gc_plan_test",
                    false,
                )
                .await
                .expect("open the plan tests' own connection")
            })
            .await;
        client
            .execute("SET pg_hint_plan.yb_bad_hint_mode = error", &[])
            .await
            .expect("set yb_bad_hint_mode");
        let mode: String = client
            .query("SHOW pg_hint_plan.yb_bad_hint_mode", &[])
            .await
            .expect("read yb_bad_hint_mode back")[0]
            .get(0);
        assert_eq!(
            mode, "error",
            "the GUC did not stick, so every hint assertion is vacuous"
        );
        client
            .query(format!("EXPLAIN {sql}").as_str(), params)
            .await
            .unwrap_or_else(|e| panic!("EXPLAIN {sql} rejected by the server: {e}"))
            .iter()
            .map(|r| r.get::<_, String>(0))
            .collect::<Vec<_>>()
            .join("\n")
    }

    /// `gc_act_trim_slru`'s anchor: per block keep the newest `will_init`
    /// `<= cutoff` plus everything above, delete strictly below — so the
    /// same-LSN delta at the anchor survives, as in `pages`. The sibling of
    /// `gc_trim_anchor_keep_delete_semantics`, which covers only `pages` —
    /// the SLRU and meta anchors are their own SQL and need their own check.
    #[tokio::test]
    async fn gc_trim_slru_anchor_keep_delete_semantics() {
        let (store, _schema) = make_store("gc_trim_slru").await;
        let branch = rand_branch();
        let (seg, blk_a, blk_b) = (7u32, 1u32, 2u32);
        let row = |blk: u32, lsn: u64, is_image: bool, will_init: bool| SlruPageRow {
            kind: SlruKind::Clog,
            segno: seg,
            blknum: blk,
            lsn: Lsn(lsn),
            is_image,
            will_init,
            payload: Bytes::from_static(b"x"),
        };
        store
            .put_slru_pages(
                branch,
                &[
                    // block A: anchor is the will_init at 40; 10/15 go,
                    // 40 and the delta the image was stamped over stay, as
                    // does 55
                    row(blk_a, 10, false, true),
                    row(blk_a, 15, false, false),
                    row(blk_a, 40, false, false),
                    row(blk_a, 40, true, true),
                    row(blk_a, 55, false, false),
                    // block B: no base <= cutoff, so nothing is provably dead
                    row(blk_b, 20, false, false),
                    row(blk_b, 60, true, true),
                ],
            )
            .await
            .unwrap();

        let kind = SlruKind::Clog as i16;
        let _ = store
            .gc_act_trim_slru(
                branch,
                (kind, seg as i64),
                &[(blk_a as i64, Lsn(0)), (blk_b as i64, Lsn(0))],
                Lsn(50),
                TRIM_UNBATCHED,
                &never_cancelled(),
                &mut Vec::new(),
            )
            .await
            .unwrap();

        let a = store
            .test_raw_slru_versions(branch, kind, seg as i64, blk_a as i64)
            .await
            .unwrap();
        assert_eq!(
            a,
            vec![(40, false, false), (40, true, true), (55, false, false)],
            "block A must keep its anchor + its same-LSN delta + everything above"
        );
        let b = store
            .test_raw_slru_versions(branch, kind, seg as i64, blk_b as i64)
            .await
            .unwrap();
        assert_eq!(b.len(), 2, "no-anchor block must be untouched");
    }

    /// Two consecutive SLRU trims, the second bounded by the watermark the
    /// first left, plus a second block carrying its own in the same statement
    /// (#1155). Pins the two off-by-ones: the previous anchor row sits exactly
    /// at the watermark and must go with the range above it, and a row below
    /// one block's watermark must survive the batch's floor.
    #[tokio::test]
    async fn gc_trim_slru_watermark_is_per_block_across_ticks() {
        let (store, _schema) = make_store("gc_trim_slru_wm").await;
        let branch = rand_branch();
        let (seg, blk_a, blk_b) = (9u32, 1u32, 2u32);
        let kind = SlruKind::Clog as i16;
        let row = |blk: u32, lsn: u64, will_init: bool| SlruPageRow {
            kind: SlruKind::Clog,
            segno: seg,
            blknum: blk,
            lsn: Lsn(lsn),
            is_image: will_init,
            will_init,
            payload: Bytes::from_static(b"x"),
        };
        store
            .put_slru_pages(
                branch,
                &[
                    row(blk_a, 10, true),
                    row(blk_a, 20, false),
                    row(blk_a, 30, true),
                    row(blk_a, 40, false),
                    row(blk_a, 50, true),
                    row(blk_a, 60, false),
                    // B is only ever trimmed in tick 2, at watermark 50: lsn 45
                    // is the discriminator — above the batch's floor (30) and
                    // below B's own watermark, so the floor alone would take it.
                    row(blk_b, 45, false),
                    row(blk_b, 50, true),
                    row(blk_b, 55, false),
                    row(blk_b, 70, true),
                ],
            )
            .await
            .unwrap();

        let versions =
            |blk: u32| store.test_raw_slru_versions(branch, kind, seg as i64, blk as i64);

        // Tick 1, block A only, no watermark: the whole range below its anchor
        // at 30.
        let mut wm = Vec::new();
        let _ = store
            .gc_act_trim_slru(
                branch,
                (kind, seg as i64),
                &[(blk_a as i64, Lsn(0))],
                Lsn(35),
                TRIM_UNBATCHED,
                &never_cancelled(),
                &mut wm,
            )
            .await
            .unwrap();
        assert_eq!(
            final_watermarks(&wm),
            vec![(blk_a as i64, Lsn(30))],
            "the anchor is the watermark"
        );
        assert_eq!(
            versions(blk_a).await.unwrap(),
            vec![
                (30, true, true),
                (40, false, false),
                (50, true, true),
                (60, false, false)
            ]
        );

        // Tick 2, both blocks in one statement: A bounded below by 30, B by 50.
        let mut wm2 = Vec::new();
        let _ = store
            .gc_act_trim_slru(
                branch,
                (kind, seg as i64),
                &[(blk_a as i64, Lsn(30)), (blk_b as i64, Lsn(50))],
                Lsn(75),
                TRIM_UNBATCHED,
                &never_cancelled(),
                &mut wm2,
            )
            .await
            .unwrap();
        assert_eq!(
            final_watermarks(&wm2),
            vec![(blk_a as i64, Lsn(50)), (blk_b as i64, Lsn(70))]
        );
        assert_eq!(
            versions(blk_a).await.unwrap(),
            vec![(50, true, true), (60, false, false)],
            "the previous anchor row goes with the range above it"
        );
        assert_eq!(
            versions(blk_b).await.unwrap(),
            vec![(45, false, false), (70, true, true)],
            "B keeps lsn 45: above the batch floor, below its own watermark"
        );
    }

    /// The SLRU half of
    /// [`gc_trim_deletes_a_watermarkless_block_beside_a_watermarked_one`], for
    /// this table's own bind list (#1160).
    #[tokio::test]
    async fn gc_trim_slru_deletes_a_watermarkless_block() {
        let (store, _schema) = make_store("gc_trim_slru_cold").await;
        let branch = rand_branch();
        let (seg, blk_a, blk_b) = (11u32, 1u32, 2u32);
        let kind = SlruKind::Clog as i16;
        let row = |blk: u32, lsn: u64, will_init: bool| SlruPageRow {
            kind: SlruKind::Clog,
            segno: seg,
            blknum: blk,
            lsn: Lsn(lsn),
            is_image: will_init,
            will_init,
            payload: Bytes::from_static(b"x"),
        };
        store
            .put_slru_pages(
                branch,
                &[
                    row(blk_a, 10, true),
                    row(blk_a, 20, false),
                    row(blk_a, 30, true),
                    row(blk_a, 40, false),
                    // B is bounded by its own watermark of 50; lsn 45 is below
                    // it and above A's floor of 0.
                    row(blk_b, 10, true),
                    row(blk_b, 45, false),
                    row(blk_b, 50, true),
                    row(blk_b, 55, false),
                    row(blk_b, 60, true),
                    row(blk_b, 70, false),
                ],
            )
            .await
            .unwrap();

        let mut wm = Vec::new();
        let _ = store
            .gc_act_trim_slru(
                branch,
                (kind, seg as i64),
                &[(blk_a as i64, Lsn(0)), (blk_b as i64, Lsn(50))],
                Lsn(100),
                TRIM_UNBATCHED,
                &never_cancelled(),
                &mut wm,
            )
            .await
            .unwrap();
        assert_eq!(
            final_watermarks(&wm),
            vec![(blk_a as i64, Lsn(30)), (blk_b as i64, Lsn(60))]
        );
        assert_eq!(
            store
                .test_raw_slru_versions(branch, kind, seg as i64, blk_a as i64)
                .await
                .unwrap(),
            vec![(30, true, true), (40, false, false)],
            "A is trimmed from 0"
        );
        assert_eq!(
            store
                .test_raw_slru_versions(branch, kind, seg as i64, blk_b as i64)
                .await
                .unwrap(),
            vec![
                (10, true, true),
                (45, false, false),
                (60, true, true),
                (70, false, false)
            ],
            "B keeps lsn 10 and 45: below its own watermark"
        );
    }

    /// The `meta` twin, and the opposite off-by-one: the range is half-open,
    /// so the watermark row is the previous anchor and is still live. Tick 2
    /// must delete it only once a newer version has become the anchor (#1155).
    #[tokio::test]
    async fn gc_trim_meta_watermark_is_per_key_across_ticks() {
        use pageserver_api::key::twophase_file_key;

        let (store, _schema) = make_store("gc_trim_meta_wm").await;
        let branch = rand_branch();
        let (key_a, key_b) = (twophase_file_key(31), twophase_file_key(32));
        let row = |k: &Key, lsn: u64| MetaRow {
            key: Bytes::from(key_to_bytes(k)),
            lsn: Lsn(lsn),
            payload: Bytes::from_static(b"x"),
        };
        store
            .put_meta(
                branch,
                &[
                    row(&key_a, 10),
                    row(&key_a, 30),
                    row(&key_a, 40),
                    row(&key_a, 50),
                    // B is only trimmed in tick 2, at watermark 50: lsn 45 is
                    // above the batch's floor (30) and below B's own watermark.
                    row(&key_b, 45),
                    row(&key_b, 50),
                    row(&key_b, 60),
                ],
            )
            .await
            .unwrap();

        let (bytes_a, bytes_b) = (key_to_bytes(&key_a), key_to_bytes(&key_b));

        // Tick 1, key A only: anchor 30, so 10 goes and 30 stays.
        let mut wm = Vec::new();
        let _ = store
            .gc_act_trim_meta(
                branch,
                &[(key_a, Lsn(0))],
                Lsn(35),
                TRIM_UNBATCHED,
                &never_cancelled(),
                &mut wm,
            )
            .await
            .unwrap();
        assert_eq!(
            final_watermarks(&wm),
            vec![(key_a, Lsn(30))],
            "the anchor is the watermark"
        );
        assert_eq!(
            store
                .test_raw_meta_versions(branch, &bytes_a)
                .await
                .unwrap(),
            vec![30, 40, 50]
        );

        // Tick 2: A's watermark row (30) is now below a newer anchor (50), so
        // it goes with 40; B is bounded by its own watermark.
        let mut wm2 = Vec::new();
        let _ = store
            .gc_act_trim_meta(
                branch,
                &[(key_a, Lsn(30)), (key_b, Lsn(50))],
                Lsn(65),
                TRIM_UNBATCHED,
                &never_cancelled(),
                &mut wm2,
            )
            .await
            .unwrap();
        let mut expected = vec![(key_a, Lsn(50)), (key_b, Lsn(60))];
        expected.sort_unstable();
        assert_eq!(final_watermarks(&wm2), expected);
        assert_eq!(
            store
                .test_raw_meta_versions(branch, &bytes_a)
                .await
                .unwrap(),
            vec![50],
            "the previous anchor row goes once a newer version supersedes it"
        );
        assert_eq!(
            store
                .test_raw_meta_versions(branch, &bytes_b)
                .await
                .unwrap(),
            vec![45, 60],
            "B keeps lsn 45: above the batch floor, below its own watermark"
        );
    }

    /// The `meta` half of
    /// [`gc_trim_deletes_a_watermarkless_block_beside_a_watermarked_one`]
    /// (#1160). Half-open range here: the anchor row itself survives.
    #[tokio::test]
    async fn gc_trim_meta_deletes_a_watermarkless_key() {
        use pageserver_api::key::twophase_file_key;

        let (store, _schema) = make_store("gc_trim_meta_cold").await;
        let branch = rand_branch();
        let (key_a, key_b) = (twophase_file_key(41), twophase_file_key(42));
        let row = |k: &Key, lsn: u64| MetaRow {
            key: Bytes::from(key_to_bytes(k)),
            lsn: Lsn(lsn),
            payload: Bytes::from_static(b"x"),
        };
        store
            .put_meta(
                branch,
                &[
                    row(&key_a, 10),
                    row(&key_a, 20),
                    row(&key_a, 30),
                    // B is bounded by its own watermark of 50; lsn 45 is below
                    // it and above A's floor of 0.
                    row(&key_b, 10),
                    row(&key_b, 45),
                    row(&key_b, 50),
                    row(&key_b, 60),
                ],
            )
            .await
            .unwrap();

        let mut wm = Vec::new();
        let _ = store
            .gc_act_trim_meta(
                branch,
                &[(key_a, Lsn(0)), (key_b, Lsn(50))],
                Lsn(100),
                TRIM_UNBATCHED,
                &never_cancelled(),
                &mut wm,
            )
            .await
            .unwrap();

        let mut expected = vec![(key_a, Lsn(30)), (key_b, Lsn(60))];
        expected.sort_unstable();
        assert_eq!(final_watermarks(&wm), expected);
        assert_eq!(
            store
                .test_raw_meta_versions(branch, &key_to_bytes(&key_a))
                .await
                .unwrap(),
            vec![30],
            "A is trimmed from 0"
        );
        assert_eq!(
            store
                .test_raw_meta_versions(branch, &key_to_bytes(&key_b))
                .await
                .unwrap(),
            vec![10, 45, 60],
            "B keeps lsn 10 and 45: below its own watermark"
        );
    }

    /// `gc_act_trim_meta`'s anchor: per key keep the newest version `<= cutoff`
    /// plus everything above. `meta` has no `will_init` — every put is a base —
    /// so the anchor is simply that newest version.
    #[tokio::test]
    async fn gc_trim_meta_anchor_keep_delete_semantics() {
        use pageserver_api::key::twophase_file_key;

        let (store, _schema) = make_store("gc_trim_meta").await;
        let branch = rand_branch();
        let (key_a, key_b) = (twophase_file_key(11), twophase_file_key(22));
        let (bytes_a, bytes_b) = (key_to_bytes(&key_a), key_to_bytes(&key_b));
        let row = |k: &[u8], lsn: u64| MetaRow {
            key: Bytes::copy_from_slice(k),
            lsn: Lsn(lsn),
            payload: Bytes::from_static(b"x"),
        };
        store
            .put_meta(
                branch,
                &[
                    // key A: anchor is 40; 10/15 go, 40/55 stay
                    row(&bytes_a, 10),
                    row(&bytes_a, 15),
                    row(&bytes_a, 40),
                    row(&bytes_a, 55),
                    // key B: every version is above the cutoff
                    row(&bytes_b, 60),
                    row(&bytes_b, 70),
                ],
            )
            .await
            .unwrap();

        let _ = store
            .gc_act_trim_meta(
                branch,
                &[(key_a, Lsn(0)), (key_b, Lsn(0))],
                Lsn(50),
                TRIM_UNBATCHED,
                &never_cancelled(),
                &mut Vec::new(),
            )
            .await
            .unwrap();

        assert_eq!(
            store
                .test_raw_meta_versions(branch, &bytes_a)
                .await
                .unwrap(),
            vec![40, 55],
            "key A must keep its anchor + everything above"
        );
        assert_eq!(
            store
                .test_raw_meta_versions(branch, &bytes_b)
                .await
                .unwrap(),
            vec![60, 70],
            "a key entirely above the cutoff must be untouched"
        );
    }

    /// Every per-key bound of all three trims has to reach the index, and the
    /// DELETE has to consume the ids as one Tid Scan. A bound demoted to a Join
    /// Filter, a Storage Filter or a `Materialize` leaves the DELETE correct and
    /// the scan unbounded — it reads each key's whole chain and discards what
    /// falls outside the range (#1133, #1155, #1153).
    ///
    /// The lateral is what guarantees the first half: a lateral reference is
    /// only definable per outer row, so a parameterised inner index scan is the
    /// only legal plan and the join method is not the cost model's to pick.
    /// `TidScan(del)` guarantees the second, which the planner drops for a full
    /// scan above ~27 ids on a table nothing has ANALYZEd (#970).
    ///
    /// Hence the SQL-text half of each case: a directive deleted from the hint
    /// binds nothing and so errors nothing, and an EXPLAIN of a table this small
    /// can pick a safe plan unaided, so the text pins are what catch a hint that
    /// quietly stopped being there.
    #[tokio::test]
    async fn gc_trims_keep_every_per_key_bound_in_the_index_cond() {
        let (store, _schema) = make_store("gc_trim_plan").await;
        let branch = BranchId::generate();
        let oid: i64 = 1;
        let fork: i16 = 0;
        let bucket: i16 = 0;
        // A realistic batch width. At one or two bounds the planner picks a
        // clean plan unaided, which is what let a Join Filter hide here before
        // (#1209); production batches are a bucket's worth of a fork's
        // candidates, in the hundreds.
        const BOUNDS: usize = 200;
        let blknums: Vec<i64> = (0..BOUNDS as i64).collect();
        let keys: Vec<Vec<u8>> = (0..BOUNDS).map(|i| vec![i as u8; KEY_SIZE]).collect();
        let watermarks: Vec<i64> = vec![0; BOUNDS];
        let anchors: Vec<i64> = vec![1; BOUNDS];
        // The production default: a small cap lets the planner cost the id set
        // as a handful of rows and pick a plan the real statement never gets.
        let limit: i64 =
            pageserver_api::config::tenant_conf_defaults::DEFAULT_GC_ROWS_PER_STATEMENT as i64;

        type Params<'a> = Vec<&'a (dyn tokio_postgres::types::ToSql + Sync)>;
        // Per statement: its bind list, and the text that has to survive — the
        // lateral, the `ARRAY(subquery)` spelling, and the three per-key bounds
        // the lateral carries into the inner scan.
        let cases: Vec<(&str, String, Params, &[&str])> = vec![
            (
                "pages",
                store.gc_trim_blocks_sql(),
                vec![
                    &branch,
                    &oid,
                    &oid,
                    &oid,
                    &fork,
                    &blknums,
                    &watermarks,
                    &anchors,
                    &bucket,
                    &limit,
                ],
                &["scan.blknum = bound.blknum"],
            ),
            (
                "slru_pages",
                store.gc_trim_slru_sql(),
                vec![
                    &branch,
                    &fork,
                    &oid,
                    &blknums,
                    &watermarks,
                    &anchors,
                    &limit,
                ],
                &["scan.blknum = bound.blknum"],
            ),
            (
                "meta",
                store.gc_trim_meta_sql(),
                vec![&branch, &keys, &watermarks, &anchors, &limit],
                &["scan.key = bound.key"],
            ),
        ];

        // Every anchor moved out of its DELETE into its own statement, so the
        // early stop is asserted there. Nothing in the result distinguishes a
        // scan that stopped at the first base from one that read the whole
        // key, so the shape is the only thing that can be checked (#1133).
        for (label, anchors_sql) in [
            ("pages", store.gc_trim_blocks_anchors_sql()),
            ("slru_pages", store.gc_trim_slru_anchors_sql()),
            ("meta", store.gc_trim_meta_anchors_sql()),
        ] {
            assert!(
                anchors_sql.contains("ORDER BY lsn DESC LIMIT 1")
                    && !anchors_sql.contains("GROUP BY"),
                "{label}: anchor no longer stops at the first base:\n{anchors_sql}"
            );
        }
        for (label, sql, params, key_bound) in cases {
            // The hint directives included: `yb_bad_hint_mode = error` fails a
            // hint it cannot bind, but a *deleted* directive binds nothing and
            // errors nothing, so only these pins catch one going missing.
            let hint = [
                format!("IndexScan(scan {label}_pkey)"),
                "TidScan(del)".into(),
            ];
            for pin in [
                "CROSS JOIN LATERAL",
                "= ANY(ARRAY(",
                "scan.lsn >= bound.watermark",
                "scan.lsn < bound.anchor",
            ]
            .iter()
            .chain(key_bound)
            .copied()
            .chain(hint.iter().map(String::as_str))
            {
                assert!(
                    sql.contains(pin),
                    "{label}: trim statement lost `{pin}`, so its scan is no \
                     longer tight:\n{sql}"
                );
            }
            assert!(
                !sql.contains(" IN ("),
                "{label}: trim regressed to a semi-join spelling, a read \
                 request and a flush per row:\n{sql}"
            );
            let plan = explain(&sql, &params).await;
            // Backward because the statement deletes bottom-up (`ORDER BY lsn
            // ASC`) against a `lsn DESC` primary key; the direction does not
            // cost the per-key bounds their place in the Index Cond.
            assert!(
                plan.contains(&format!("Index Scan using {label}_pkey"))
                    || plan.contains(&format!("Index Scan Backward using {label}_pkey")),
                "{label}: the id subquery is no longer an index scan:\n{plan}"
            );
            assert!(
                plan.contains("Tid Scan"),
                "{label}: the DELETE no longer consumes the ids as a Tid \
                 Scan:\n{plan}"
            );
            for node in [
                "Seq Scan",
                "Merge Join",
                "Materialize",
                "Join Filter",
                "Storage Filter",
                // Batches only the equality column, leaving the lsn ranges as a
                // filter — 5x the rows on a fixture of 100 versions a block.
                "Batched Nested Loop",
            ] {
                assert!(
                    !plan.contains(node),
                    "{label}: trim plans a {node}, so a per-key range is no \
                     longer an index bound:\n{plan}"
                );
            }
        }
    }

    /// Scenario: one `DropDb` candidate reclaims a whole database — every
    /// fork's `pages` rows and every `meta` key under both of its prefixes,
    /// sparse-reldir tombstones included — at `lsn <= due_lsn`, while a
    /// database recreated on the same oids (rows above the drop) and a
    /// different database survive.
    ///
    /// The sweep is budgeted: a budget below the database's size stops
    /// mid-range and reports itself undrained, and resuming from the cursor
    /// finishes it. That is what keeps a 100 GB `DROP DATABASE` from being one
    /// unbounded delete.
    #[tokio::test]
    async fn gc_act_sweep_database_in_budgeted_batches() {
        use pageserver_api::key::{rel_tag_sparse_key, relmap_file_key};
        let (store, _schema) = make_store("gc_drop_db").await;
        let branch = rand_branch();
        // The dropped database and a bystander sharing its tablespace.
        let (db, other_db) = (GC_DB, GC_DB + 1);
        let due = Lsn(100);

        let meta_row = |key: Key, lsn: u64| MetaRow {
            key: Bytes::from(key_to_bytes(&key)),
            lsn: Lsn(lsn),
            payload: Bytes::from_static(b"m"),
        };
        let mut pages = Vec::new();
        let mut meta = Vec::new();
        for rel in 16400u32..16404 {
            for fork in 0u8..2 {
                // Blocks span `PAGE_BUCKET_STRIDE` boundaries so the database
                // lands in every bucket — `page_bucket_id` folds
                // `blknum / stride` in, and adjacent relnodes alone would put
                // the whole fixture in two buckets.
                for stride in 0..PAGE_BUCKETS as u32 {
                    for j in 0u32..7 {
                        let blk = stride * PAGE_BUCKET_STRIDE as u32 + j;
                        for lsn in [10u64, 20] {
                            pages.push(page_row(
                                page_key(GC_SPC as u32, db as u32, rel, fork, blk),
                                lsn,
                                true,
                                true,
                                b"x",
                            ));
                        }
                    }
                }
                meta.push(rel_size_row(rel, fork, 10, 42));
                // A live marker for the even rels, a DROP TABLE tombstone for
                // the odd ones — a range sweep clears both without listing.
                meta.push(meta_row(
                    rel_tag_sparse_key(GC_SPC as u32, db as u32, rel, fork),
                    10,
                ));
            }
        }
        meta.push(meta_row(relmap_file_key(GC_SPC as u32, db as u32), 10));
        // Recreated on the same oids above the drop: must survive.
        pages.push(page_row(
            page_key(GC_SPC as u32, db as u32, 16400, 0, 0),
            200,
            true,
            true,
            b"new",
        ));
        meta.push(meta_row(relmap_file_key(GC_SPC as u32, db as u32), 200));
        // A different database: must survive untouched.
        pages.push(page_row(
            page_key(GC_SPC as u32, other_db as u32, 16400, 0, 0),
            10,
            true,
            true,
            b"other",
        ));
        meta.push(meta_row(
            relmap_file_key(GC_SPC as u32, other_db as u32),
            10,
        ));
        meta.push(meta_row(
            rel_tag_sparse_key(GC_SPC as u32, other_db as u32, 16400, 0),
            10,
        ));
        store.put_pages(branch, &pages).await.unwrap();
        store.put_meta(branch, &meta).await.unwrap();

        let (pages_before, meta_before) =
            store.test_db_row_counts(branch, GC_SPC, db).await.unwrap();
        assert_eq!((pages_before, meta_before), (673, 18));
        let by_bucket_before = store
            .test_db_pages_per_bucket(branch, GC_SPC, db)
            .await
            .unwrap();
        assert!(
            by_bucket_before.iter().all(|n| *n > 20),
            "fixture must occupy every bucket to test the fan-out: \
             {by_bucket_before:?}"
        );

        // One budgeted pass: bounded, and not done.
        let db_scope = GcSweepScope::Database((GC_SPC, db));
        let mut progress = GcSweepProgress::new(db_scope);
        let first = store
            .gc_act_sweep(branch, due, 100, &mut progress)
            .await
            .unwrap();
        assert!(first <= 100, "the budget bounds the pass: {first}");
        assert!(
            !progress.is_reclaimed(),
            "a 100-row budget can't finish 689 rows"
        );

        // The pass took a batch from EVERY bucket rather than draining one:
        // `pages` is pre-split on `bucket_id`, so a bucket-at-a-time sweep
        // would aim a whole tick's deletes at one tablet.
        let by_bucket_after = store
            .test_db_pages_per_bucket(branch, GC_SPC, db)
            .await
            .unwrap();
        for (bucket, (before, after)) in by_bucket_before.iter().zip(&by_bucket_after).enumerate() {
            assert!(
                after < before,
                "bucket {bucket} untouched by the pass: {before} -> {after} \
                 (all buckets: {by_bucket_after:?})"
            );
        }

        // Resume from the same cursor until it reports done, and tally what the
        // sweep charged. The tally is exact because a batch charges the rows
        // its SELECT named, not the DELETE's reported count — which a retried
        // statement under-reports (see `gc_drop_db_delete_batch`).
        let mut total = first;
        for _ in 0..40 {
            if progress.is_reclaimed() {
                break;
            }
            total += store
                .gc_act_sweep(branch, due, 100, &mut progress)
                .await
                .unwrap();
        }
        assert!(progress.is_reclaimed(), "the sweep must converge");
        assert_eq!(
            total, 689,
            "every row at or below the drop LSN, charged once: 672 pages + 17 meta"
        );

        // What is left of the dropped database is exactly the recreation.
        assert_eq!(
            store.test_db_row_counts(branch, GC_SPC, db).await.unwrap(),
            (1, 1),
            "only the rows written above the drop survive"
        );
        assert_eq!(
            store
                .test_raw_block_versions(branch, GC_SPC, db, 16400, 0, 0)
                .await
                .unwrap(),
            vec![(200, true, true)]
        );
        assert_eq!(
            store
                .test_db_row_counts(branch, GC_SPC, other_db)
                .await
                .unwrap(),
            (1, 2),
            "a different database is untouched"
        );

        // A fork busy mid-sweep: the cursors moved past it, then a materialise
        // job inserted an image behind them at a folded LSN below the drop.
        // `require_recheck` is what stops the candidate being dequeued with
        // that row stranded — nothing would ever name it again.
        store
            .put_pages(
                branch,
                &[page_row(
                    page_key(GC_SPC as u32, db as u32, 16400, 0, 0),
                    50,
                    true,
                    true,
                    b"straggler",
                )],
            )
            .await
            .unwrap();
        progress.require_recheck();
        assert!(
            !progress.is_reclaimed(),
            "an owed re-check must hold the candidate queued"
        );
        // What `act_db_drops` does once a re-check is owed.
        assert!(progress.begin_owed_pass(), "the owed pass must begin");
        for _ in 0..40 {
            if progress.is_reclaimed() {
                break;
            }
            store
                .gc_act_sweep(branch, due, 100, &mut progress)
                .await
                .unwrap();
        }
        assert!(progress.is_reclaimed(), "the re-check pass must converge");
        assert_eq!(
            store.test_db_row_counts(branch, GC_SPC, db).await.unwrap(),
            (1, 1),
            "the straggler is gone; only the rows above the drop survive"
        );

        // Idempotent: a fresh sweep of the same candidate finds nothing left.
        let mut again = GcSweepProgress::new(db_scope);
        assert_eq!(
            store
                .gc_act_sweep(branch, due, 10_000, &mut again)
                .await
                .unwrap(),
            0
        );
        assert!(again.is_reclaimed());
    }

    /// Scenario: one `DropRel` candidate reclaims every fork of a dropped
    /// relation — `pages` rows and each fork's size row — at `lsn <= due_lsn`,
    /// while a relfilenode reused above the drop, a *different* relation, and
    /// the relation's own sparse rel-directory tombstone all survive. That
    /// tombstone is the one thing a relation sweep must not touch: it shadows
    /// an ancestor's `Exists` under copy-on-write.
    #[tokio::test]
    async fn gc_act_sweep_relation_covers_every_fork() {
        use pageserver_api::key::rel_tag_sparse_key;
        let (store, _schema) = make_store("gc_sweep_rel").await;
        let branch = rand_branch();
        let (rel, other) = (16500u32, 16501u32);
        let due = Lsn(100);

        let mut pages = Vec::new();
        let mut meta = Vec::new();
        for fork in 0u8..3 {
            for blk in 0u32..4 {
                pages.push(page_row(
                    page_key(GC_SPC as u32, GC_DB as u32, rel, fork, blk),
                    10,
                    true,
                    true,
                    b"x",
                ));
            }
            meta.push(rel_size_row(rel, fork, 10, 4));
        }
        // Reused relfilenode above the drop, and an unrelated relation.
        pages.push(page_row(
            page_key(GC_SPC as u32, GC_DB as u32, rel, 0, 0),
            200,
            true,
            true,
            b"new",
        ));
        pages.push(page_row(
            page_key(GC_SPC as u32, GC_DB as u32, other, 0, 0),
            10,
            true,
            true,
            b"other",
        ));
        meta.push(rel_size_row(other, 0, 10, 1));
        // The dropped relation's tombstone, which must survive.
        let tombstone = key_to_bytes(&rel_tag_sparse_key(GC_SPC as u32, GC_DB as u32, rel, 0));
        meta.push(MetaRow {
            key: Bytes::from(tombstone.clone()),
            lsn: Lsn(90),
            payload: Bytes::from_static(b"r"),
        });
        store.put_pages(branch, &pages).await.unwrap();
        store.put_meta(branch, &meta).await.unwrap();

        let scope = GcSweepScope::Relation((GC_SPC, GC_DB), rel as i64);
        let mut progress = GcSweepProgress::new(scope);
        for _ in 0..40 {
            if progress.is_reclaimed() {
                break;
            }
            store
                .gc_act_sweep(branch, due, 100, &mut progress)
                .await
                .unwrap();
        }
        assert!(progress.is_reclaimed(), "the sweep must converge");

        for fork in 0i16..3 {
            for blk in 0i64..4 {
                // (0, 0) is where the reused relfilenode wrote above the drop,
                // asserted separately below.
                if (fork, blk) == (0, 0) {
                    continue;
                }
                assert!(
                    store
                        .test_raw_block_versions(branch, GC_SPC, GC_DB, rel as i64, fork, blk)
                        .await
                        .unwrap()
                        .is_empty(),
                    "fork {fork} block {blk} should be gone"
                );
            }
            let size_key = key_to_bytes(&rel_size_to_key(gc_rel(rel as i64, fork)));
            assert!(
                store
                    .read_meta(branch, &size_key, Lsn(150))
                    .await
                    .unwrap()
                    .is_none(),
                "fork {fork}'s size row should be gone"
            );
        }
        assert_eq!(
            store
                .test_raw_block_versions(branch, GC_SPC, GC_DB, rel as i64, 0, 0)
                .await
                .unwrap(),
            vec![(200, true, true)],
            "the reused relfilenode's row survives"
        );
        assert_eq!(
            store
                .test_raw_block_versions(branch, GC_SPC, GC_DB, other as i64, 0, 0)
                .await
                .unwrap()
                .len(),
            1,
            "an unrelated relation is untouched"
        );
        assert!(
            store
                .read_meta(branch, &tombstone, Lsn(150))
                .await
                .unwrap()
                .is_some(),
            "the sparse rel-directory tombstone must survive a relation sweep"
        );

        drop_schema(&store, &_schema).await;
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
    /// may gain a same-LSN image — the materialiser pattern).
    /// A random cutoff is drawn, all blocks are trimmed, and the
    /// surviving `(lsn, is_image, will_init)` set of every block must
    /// equal a 5-line model:
    ///
    ///   anchor = max lsn with will_init && lsn ≤ cutoff
    ///   no anchor → everything survives
    ///   else survive ⇔ lsn >= anchor
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
                // Materialiser pattern: image stamped at a plain delta's LSN.
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
            let blks: Vec<(i64, Lsn)> = (0..nblocks as i64).map(|b| (b, Lsn(0))).collect();
            let _ = store
                .gc_act_trim_blocks(
                    branch,
                    (GC_SPC, GC_DB, rel as i64, 0),
                    &blks,
                    cutoff,
                    // One row a statement: every block here is watermark-less,
                    // so this walks each history a row at a time — including
                    // the same-LSN image pairs, where a batch boundary falls
                    // inside an `lsn` tie group (#1209).
                    1,
                    &never_cancelled(),
                    &mut Vec::new(),
                )
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
                        .filter(|(lsn, _, _)| *lsn >= a)
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
