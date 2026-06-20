//! Single in-RAM structure for relation-page rows. Combines the
//! pre-flush write buffer with the post-flush read cache, evicting
//! at the **per-key** granularity so a cache hit either has the
//! complete chain for that key or no rows at all — never a partial
//! middle-of-chain hole.
//!
//! ## Concurrency
//!
//! `UnifiedPages` is internally thread-safe — every method takes
//! `&self`. Per-key state lives in a lock-free ordered `SkipMap`
//! keyed by the page key; each value is a `parking_lot::Mutex<KeyState>`,
//! so reads and writes on different keys never contend (the map itself
//! is never globally locked). Aggregate counters (`total_bytes`,
//! `unflushed_bytes`) are atomics. The eviction LRU and the flush FIFO
//! each live behind their own `parking_lot::Mutex` — both are touched
//! in O(1) on every push, but the holds are nanosecond-scale and only
//! contested by put + flush + materialiser-install (single-digit task
//! count), not by the read path. The concurrent getpage fan-in goes
//! straight to per-key mutexes and never touches LRU / FIFO.
//!
//! Map membership and per-key contents are kept consistent without a
//! global lock by a single rule: a node is only ever **removed** while
//! its own `KeyState` mutex is held (`evict_to_capacity` /
//! `trim_key_up_to`), and every mutator re-checks `Entry::is_removed`
//! after taking that mutex. A writer that raced an eviction onto a
//! now-detached node simply retries onto a fresh one, so no accepted
//! row is ever stranded on a removed node. The skiplist's ordered
//! `range()` is what makes [`UnifiedPages::lookup_range`] a real
//! key-range walk instead of a full-map scan.
//!
//! ## Per-key whole-eviction
//!
//! Eviction unit is the entire `KeyState` (one map entry +
//! everything in its `chain` BTreeMap). The LRU keeps insertion /
//! promotion order — back = most recently written / materialised,
//! front = coldest — and `evict_to_capacity` walks from the front
//! dropping whole keys until `total_bytes` falls back under
//! `capacity_bytes`. There is no per-row FIFO and no `evicted_below`
//! watermark. Reads check anchor presence; if the key is in the
//! cache, every row for it that we ever cached is here too.
//!
//! Keys with `unflushed_count > 0` are **pinned**: they hold rows
//! that haven't been INSERTed to YB yet and are the only copy that
//! exists, so dropping them would lose accepted WAL. Eviction skips
//! pinned keys (and on skip moves them to the back so the next
//! eviction call doesn't keep tripping over them).
//!
//! ## Flush ordering
//!
//! Unflushed rows are visited in put-order so the YB flush is
//! monotonic in LSN. We track this with a small auxiliary FIFO of
//! `(key, lsn)` pairs (`unflushed_fifo`), kept in sync with
//! `KeyState::unflushed_count` and the `PageEntry::durable` bits.
//!
//! ## Lookup
//! - [`UnifiedPages::lookup_chain`] returns rows for `key` at `lsn ≤
//!   target_lsn`, including unflushed.
//! - [`UnifiedPages::lookup_chain_durable`] returns only durable rows.
//!   Used by the materialiser, which must never fold pre-flush deltas
//!   into an image (crash + WAL replay would reapply those deltas on
//!   top of the image → double-apply).
//! - [`UnifiedPages::lookup_chain_if_complete`] /
//!   [`UnifiedPages::lookup_chain_durable_if_complete`] are the
//!   cache-only fast paths: with per-key whole-eviction, a key either
//!   has its full chain or is absent, so anchor presence is
//!   sufficient — no `evicted_below` check.
//!
//! ## Trim
//! [`UnifiedPages::trim_key_up_to`] removes durable rows for `key`
//! with `lsn ≤ max_lsn`. Called by the materialiser after a
//! successful image INSERT to drop the deltas the image superseded.

use std::collections::{BTreeMap, HashMap, VecDeque};
use std::sync::Arc;
use std::sync::atomic::{AtomicBool, AtomicUsize, Ordering};

use crossbeam_skiplist::SkipMap;
use hashlink::LinkedHashMap;
use pageserver_api::key::{KEY_SIZE, Key};
use parking_lot::Mutex;
use utils::lsn::Lsn;

use crate::page_store::PageRow;

/// A row plus its durability bit. Held by `Arc` so the cache slot and
/// any in-flight flush snapshot share the same entry — `mark_flushed`
/// flips the `durable` bit on the very `Arc` it pulled out of the
/// snapshot, never going back through `(key, lsn)`.
///
/// `durable` is `AtomicBool` so `mark_flushed` can flip the bit
/// without holding the per-key shard lock — flush bookkeeping touches
/// the snapshot's Arcs directly, and the lock is only acquired briefly
/// to decrement the per-key `unflushed_count`. The CAS form also makes
/// the (rare, defensively-handled) double-mark case a no-op.
pub(crate) struct PageEntry {
    row: Arc<PageRow>,
    durable: AtomicBool,
}

impl PageEntry {
    fn from_arc_row(row: Arc<PageRow>, durable: bool) -> Arc<Self> {
        Arc::new(Self {
            row,
            durable: AtomicBool::new(durable),
        })
    }

    /// Cheap refcount bump, no clone of the row's bytes. Used by
    /// `mark_flushed` callers that need to hand the `PageRow` off
    /// to the YB INSERT path without copying the chain bytes.
    pub(crate) fn row_arc(&self) -> Arc<PageRow> {
        Arc::clone(&self.row)
    }

    fn is_durable(&self) -> bool {
        self.durable.load(Ordering::Acquire)
    }

    /// Try to flip `false → true`. Returns `true` iff this call did the
    /// transition (so the caller is responsible for counter bookkeeping).
    fn try_promote(&self) -> bool {
        self.durable
            .compare_exchange(false, true, Ordering::AcqRel, Ordering::Relaxed)
            .is_ok()
    }
}

/// Per-key bookkeeping stored as the `SkipMap` value, wrapped in a
/// `parking_lot::Mutex` so mutations to one key serialise without
/// locking the whole map. The skiplist node holds the mutex; callers
/// take `Entry::value().lock()` to read or mutate this struct.
struct KeyState {
    chain: BTreeMap<Lsn, Arc<PageEntry>>,
    /// Count of `PageEntry`s in `chain` with `durable == false`.
    /// When > 0 the key is pinned and `evict_to_capacity` skips it.
    unflushed_count: u32,
    /// Sum of `row_size(entry.row)` for every entry in `chain`.
    /// Tracked so eviction can subtract the right amount from
    /// `total_bytes` in one shot.
    bytes: usize,
}

impl KeyState {
    fn new() -> Self {
        Self {
            chain: BTreeMap::new(),
            unflushed_count: 0,
            bytes: 0,
        }
    }
}

pub struct UnifiedPages {
    /// Per-key state in a lock-free ordered map. Reads
    /// (`lookup_chain*`) on different keys proceed in parallel; writes
    /// (`push*`, `trim`, `mark_flushed`) only contend when they hit the
    /// same key's mutex. The ordering powers `lookup_range`'s native
    /// key-range walk.
    by_key: SkipMap<Key, Mutex<KeyState>>,
    /// LRU sidecar: just the key set, in promotion order. Back =
    /// most recently inserted / promoted, front = coldest. Only
    /// `push*` / `evict_to_capacity` / `trim_key_up_to` touch this;
    /// reads do not. Holds are O(1).
    lru: Mutex<LinkedHashMap<Key, ()>>,
    /// Insertion-ordered list of (key, lsn) pairs for unflushed
    /// entries. Drives `snapshot_unflushed` + `mark_flushed`
    /// so flushes hit YB in monotonic put-order.
    unflushed_fifo: Mutex<VecDeque<(Key, Lsn)>>,
    total_bytes: AtomicUsize,
    unflushed_bytes: AtomicUsize,
    capacity_bytes: usize,
}

impl UnifiedPages {
    pub fn new(capacity_bytes: usize) -> Self {
        Self {
            by_key: SkipMap::new(),
            lru: Mutex::new(LinkedHashMap::new()),
            unflushed_fifo: Mutex::new(VecDeque::new()),
            total_bytes: AtomicUsize::new(0),
            unflushed_bytes: AtomicUsize::new(0),
            capacity_bytes: capacity_bytes.max(1),
        }
    }

    pub fn unflushed_bytes(&self) -> usize {
        self.unflushed_bytes.load(Ordering::Relaxed)
    }

    pub fn total_bytes(&self) -> usize {
        self.total_bytes.load(Ordering::Relaxed)
    }

    pub fn unflushed_count(&self) -> usize {
        self.unflushed_fifo.lock().len()
    }

    /// Number of distinct keys tracked. Drives the
    /// `pageserver_yb_unified_pages_by_key_entries` gauge.
    pub fn by_key_entries(&self) -> usize {
        self.by_key.len()
    }

    /// Append a row as unflushed.
    ///
    /// Walreceiver's LSNs are monotonic within a single connection, but
    /// the cache is **not** guaranteed empty across (re)connects: when
    /// only the safekeeper restarts (the pageserver stays up), the
    /// walreceiver reconnects and replays from the durable
    /// `consistent_lsn` over rows still buffered from the first pass
    /// (#57). A WAL record at a given LSN is deterministic, so a
    /// pre-existing slot at the same `(key, lsn)` already represents
    /// this row — `insert_entry` keeps it and reports back that nothing
    /// new was added, so we don't double-record it in the FIFO.
    pub fn push(&self, row: PageRow) {
        let key = row.key;
        let lsn = row.lsn;
        if self.insert_entry(key, lsn, row, false) {
            self.unflushed_fifo.lock().push_back((key, lsn));
        }
    }

    /// Append a row that is already durable in YB. Used by the image
    /// materialiser after a successful `put_pages`. Promotes the key
    /// to the back of the LRU; eviction picks up cold keys first.
    pub fn push_durable(&self, row: PageRow) {
        self.push_durable_arc(Arc::new(row));
    }

    /// Same as `push_durable`, but takes a pre-wrapped `Arc<PageRow>`.
    /// The materialiser builds its image as an `Arc<PageRow>` so the
    /// same Arc reaches both the YB INSERT (`put_pages`) and the cache
    /// install — no struct copy at the boundary.
    pub fn push_durable_arc(&self, row: Arc<PageRow>) {
        let key = row.key;
        let lsn = row.lsn;
        self.insert_arc_entry(key, lsn, row, true);
    }

    /// Mark each entry in the snapshot durable. The snapshot holds an
    /// `Arc<PageEntry>` per row, so we flip the bit on the very same
    /// entry the caller flushed to YB.
    /// If the cache slot was concurrently overwritten
    /// (materialiser raced an image into the same LSN) the old entry's
    /// `durable` bit was already flipped to `true` by `insert_entry`;
    /// our CAS observes `Err(true)` and we skip without touching the
    /// counters (insert_entry already adjusted them).
    pub(crate) fn mark_flushed(&self, snapshot: &[Arc<PageEntry>]) {
        // Per-key list of promoted lsns, so we drop their stale FIFO
        // refs in one pass at the end instead of scanning per row.
        let mut promoted_by_key: HashMap<Key, Vec<Lsn>> = HashMap::new();

        for entry in snapshot {
            if !entry.try_promote() {
                continue; // insert_entry already claimed this entry.
            }
            pageserver_api::write_trace::trace_stage("durable", &entry.row.key, entry.row.lsn);
            let size = row_size(&entry.row);
            self.unflushed_bytes.fetch_sub(size, Ordering::Relaxed);
            if let Some(node) = self.by_key.get(&entry.row.key) {
                let mut ks = node.value().lock();
                ks.unflushed_count = ks.unflushed_count.saturating_sub(1);
            }
            promoted_by_key
                .entry(entry.row.key)
                .or_default()
                .push(entry.row.lsn);
        }

        if !promoted_by_key.is_empty() {
            let mut fifo = self.unflushed_fifo.lock();
            fifo.retain(|(k, l)| {
                promoted_by_key
                    .get(k)
                    .is_none_or(|lsns| !lsns.contains(l))
            });
        }
    }

    /// Drop whole keys from the front of the LRU until `total_bytes
    /// <= capacity_bytes`. Bounded by a 2 ms wall-clock budget so a
    /// big cap overshoot doesn't starve the writer lock. Pinned keys
    /// (`unflushed_count > 0`) are moved to the back so subsequent
    /// calls aren't blocked by them.
    pub fn evict_to_capacity(&self) {
        const TIME_BUDGET: std::time::Duration = std::time::Duration::from_millis(2);
        let start = std::time::Instant::now();

        // Pinned keys we've moved to the back during this call.
        // Once we've seen every key (front pointer is at one we've
        // already requeued), there's nothing left to evict and we
        // stop.
        let mut requeued_pins = 0usize;
        while self.total_bytes.load(Ordering::Relaxed) > self.capacity_bytes {
            // Peek the front key under the LRU lock and drop it
            // immediately. Subsequent shard access can then run in
            // parallel with other operations on different keys.
            let candidate = {
                let lru = self.lru.lock();
                if lru.is_empty() || requeued_pins >= lru.len() {
                    break;
                }
                let Some((key, _)) = lru.front() else { break };
                *key
            };

            // Check pinned + remove if evictable, holding the key's
            // own mutex across the decision so a concurrent `push` onto
            // this node either lands before the removal (bumping
            // `unflushed_count`, which pins it) or retries onto a fresh
            // node after seeing `is_removed`. Removing the node *under*
            // its mutex is the rule the whole map relies on.
            let removed = match self.by_key.get(&candidate) {
                None => Some(0),
                Some(node) => {
                    let ks = node.value().lock();
                    if ks.unflushed_count > 0 {
                        // Pinned — leave in the map; we'll rotate
                        // the LRU below.
                        drop(ks);
                        None
                    } else {
                        let bytes = ks.bytes;
                        node.remove();
                        drop(ks);
                        Some(bytes)
                    }
                }
            };

            match removed {
                Some(bytes) => {
                    if bytes > 0 {
                        self.total_bytes.fetch_sub(bytes, Ordering::Relaxed);
                    }
                    // Remove from LRU regardless — by_key may have
                    // been concurrently emptied but LRU still holds
                    // the key.
                    self.lru.lock().remove(&candidate);
                }
                None => {
                    // Pinned. Rotate to the back so we don't keep
                    // tripping over it.
                    self.lru.lock().to_back(&candidate);
                    requeued_pins += 1;
                }
            }

            if start.elapsed() >= TIME_BUDGET {
                break;
            }
        }
    }

    /// Remove all durable entries for `key` with `lsn <= max_lsn`.
    /// Used by the materialiser to drop deltas the new image
    /// supersedes. Newer deltas and unflushed entries are untouched.
    pub fn trim_key_up_to(&self, key: &Key, max_lsn: Lsn) {
        let mut total_freed = 0usize;
        let key_empty = {
            let Some(node) = self.by_key.get(key) else {
                return;
            };
            let mut ks_guard = node.value().lock();
            if node.is_removed() {
                // Raced with an eviction that already dropped this node.
                return;
            }
            let ks = &mut *ks_guard;
            let to_remove: Vec<Lsn> = ks
                .chain
                .range(..=max_lsn)
                .filter(|(_, e)| e.is_durable())
                .map(|(l, _)| *l)
                .collect();
            for lsn in to_remove {
                if let Some(page_entry) = ks.chain.remove(&lsn) {
                    let size = row_size(&page_entry.row);
                    ks.bytes -= size;
                    total_freed += size;
                }
            }
            let empty = ks.chain.is_empty();
            // Remove the node under its own mutex (the map's removal rule)
            // so a racing `push` retries onto a fresh node.
            if empty {
                node.remove();
            }
            empty
        };
        if total_freed > 0 {
            self.total_bytes.fetch_sub(total_freed, Ordering::Relaxed);
        }
        if key_empty {
            self.lru.lock().remove(key);
        }
    }

    /// Atomic guarded materialiser-image install: trim the durable prefix
    /// `≤ image.lsn`, then insert `image` as a durable anchor — but ONLY
    /// if that keeps the key's cache a complete suffix. Returns `true` if
    /// installed, `false` if skipped.
    ///
    /// Skipping is always safe: the image is already durable in YB (the
    /// materialiser `put_pages`'d it before calling this), so a skip merely
    /// means reads of this key fall through the cache fast path
    /// (`lookup_chain_if_complete`) to the YB blend — correct, just not
    /// cache-accelerated.
    ///
    /// **Why the guard.** The fast path trusts any cached `will_init` row
    /// as an anchor with a *complete* chain above it (see
    /// `lookup_chain_if_complete`). A materialiser image installed below
    /// the head would break that: the rows between the image and the head
    /// can live only in YB (evicted, or never cached), so a later read
    /// would anchor on the image and miss them — a silent lost-update. The
    /// old `trim_key_up_to` + `push_durable_arc` pairing did exactly that,
    /// and was *also* a TOCTOU (two separate lock holds — an eviction could
    /// drop the key between them, turning the push into a lone stale
    /// anchor). This method closes both: check + trim + insert run under
    /// one continuous hold of the key's shard mutex, and we install only
    /// when
    ///   * the key is **present** (an absent / just-evicted key has no
    ///     cached rows above the image to vouch for it — seeding a lone
    ///     anchor there is precisely the hole), and
    ///   * `image.lsn >= min(cached lsns)` (installing below the lowest
    ///     cached row would leave a gap `(image.lsn, min_cached)` living
    ///     only in YB).
    ///
    /// Those two conditions are *sufficient* because the materialiser
    /// bounds `image.lsn ≤ flush_horizon_lsn`: every cached row
    /// `≤ image.lsn` is therefore durable (so the prefix trim is total)
    /// and every unflushed row is strictly above it; and a present key's
    /// **top** row is never selectively removed (trim only drops a prefix,
    /// eviction drops the whole key), so a present key's chain is exactly
    /// the complete suffix `[min_cached, head]`. Trimming to `image.lsn`
    /// and re-anchoring there yields a complete `[image.lsn, head]`.
    pub fn install_image_if_extends(&self, image: Arc<PageRow>) -> bool {
        use std::collections::btree_map::Entry;

        let key = image.key;
        let lsn = image.lsn;
        let size = row_size(&image);

        // Bytes leaving total_bytes (trimmed durable rows + any overwritten
        // same-LSN slot) and unflushed_bytes (only an overwritten unflushed
        // slot), tallied under the lock and applied to the atomics after.
        let mut total_sub = 0usize;
        let mut unflushed_sub = 0usize;
        let mut overwrote_unflushed = false;

        {
            // Present-only: deliberately NOT `get_or_insert`. An absent (or
            // just-evicted) key must not be re-seeded with a below-head
            // anchor — that is the hole this guard exists to prevent.
            let Some(node) = self.by_key.get(&key) else {
                return false;
            };
            let mut ks_guard = node.value().lock();
            if node.is_removed() {
                // Raced an eviction/trim that detached this node. Treat as
                // absent and skip (do not retry onto a fresh node — that
                // would recreate the absent-key hole).
                return false;
            }
            let ks = &mut *ks_guard;

            // Guard: only install if it extends the existing suffix, never
            // gaps below it. An empty chain can't happen for a live node,
            // but treat it as "absent" defensively.
            match ks.chain.keys().next() {
                Some(&min_lsn) if lsn >= min_lsn => {}
                _ => return false,
            }

            // Trim the durable prefix `≤ image.lsn` the image subsumes.
            // Unflushed rows `≤ image.lsn` only occur via the benign #57
            // reconnect re-push; leave them — an exact-LSN collision is
            // handled by the insert's Occupied arm below.
            let to_remove: Vec<Lsn> = ks
                .chain
                .range(..=lsn)
                .filter(|(_, e)| e.is_durable())
                .map(|(l, _)| *l)
                .collect();
            for l in to_remove {
                if let Some(e) = ks.chain.remove(&l) {
                    let s = row_size(&e.row);
                    ks.bytes -= s;
                    total_sub += s;
                }
            }

            // Insert the image as a durable anchor.
            match ks.chain.entry(lsn) {
                Entry::Occupied(mut o) => {
                    // A same-LSN slot survived the trim ⇒ it was unflushed
                    // (a #57 re-push of an already-durable record; an exact
                    // image-LSN collision with a durable row would have been
                    // trimmed). Overwrite and reverse its unflushed pin.
                    let prev = row_size(&o.get().row);
                    overwrote_unflushed = !o.get().is_durable();
                    ks.bytes = ks.bytes + size - prev;
                    total_sub += prev;
                    if overwrote_unflushed {
                        unflushed_sub = prev;
                        ks.unflushed_count = ks.unflushed_count.saturating_sub(1);
                    }
                    o.insert(PageEntry::from_arc_row(image, true));
                }
                Entry::Vacant(v) => {
                    ks.bytes += size;
                    v.insert(PageEntry::from_arc_row(image, true));
                }
            }
        }

        self.total_bytes.fetch_add(size, Ordering::Relaxed);
        if total_sub > 0 {
            self.total_bytes.fetch_sub(total_sub, Ordering::Relaxed);
        }
        if unflushed_sub > 0 {
            self.unflushed_bytes.fetch_sub(unflushed_sub, Ordering::Relaxed);
        }

        // Bump the (present) key to the back of the LRU — most recently
        // used. Lock order matches `trim_key_up_to`: node mutex (released
        // above) → LRU, never nested.
        {
            let mut lru = self.lru.lock();
            if lru.contains_key(&key) {
                lru.to_back(&key);
            }
        }

        // Drop the now-stale FIFO entry the overwritten re-push left behind
        // (the slot is durable now), mirroring `insert_arc_entry`.
        if overwrote_unflushed {
            let mut fifo = self.unflushed_fifo.lock();
            if let Some(pos) = fifo.iter().position(|(k, l)| *l == lsn && k == &key) {
                fifo.remove(pos);
            }
        }
        true
    }

    /// Return rows for `key` at `lsn <= target_lsn`, LSN-descending.
    /// Includes unflushed. Does NOT touch the LRU position — LRU
    /// order tracks write recency only, which is sufficient: hot
    /// keys keep getting written.
    pub fn lookup_chain(&self, key: &Key, target_lsn: Lsn) -> Vec<Arc<PageRow>> {
        let Some(node) = self.by_key.get(key) else {
            return Vec::new();
        };
        let ks = node.value().lock();
        ks.chain
            .range(..=target_lsn)
            .rev()
            .map(|(_, e)| e.row_arc())
            .collect()
    }

    /// Like `lookup_chain` but only durable rows.
    pub fn lookup_chain_durable(&self, key: &Key, target_lsn: Lsn) -> Vec<Arc<PageRow>> {
        let Some(node) = self.by_key.get(key) else {
            return Vec::new();
        };
        let ks = node.value().lock();
        ks.chain
            .range(..=target_lsn)
            .rev()
            .filter(|(_, e)| e.is_durable())
            .map(|(_, e)| e.row_arc())
            .collect()
    }

    /// Cache-only fast path. Returns `Some(chain)` (LSN-descending,
    /// including unflushed) iff `key` is present and the chain
    /// contains a `will_init` row at some `anchor_lsn ≤ target_lsn`.
    ///
    /// With per-key whole-eviction the cache cannot have a hole
    /// between the anchor and `target_lsn`: if the key is here,
    /// every row for it that we ever cached is here too. Anchor
    /// presence is therefore both necessary and sufficient.
    ///
    /// This holds only because every mutator preserves "present key ⇒
    /// complete suffix": head `push`, prefix `trim`, whole-key evict —
    /// and, crucially, materialiser installs go through
    /// `install_image_if_extends`, which REFUSES to seed an anchor that
    /// would break it (an image below the lowest cached row, or into an
    /// absent key). Were an image installed below the head with YB-only
    /// rows above it, this fast path would anchor on it and silently drop
    /// those rows.
    pub fn lookup_chain_if_complete(
        &self,
        key: &Key,
        target_lsn: Lsn,
    ) -> Option<Vec<Arc<PageRow>>> {
        let node = self.by_key.get(key)?;
        let ks = node.value().lock();
        let mut out = Vec::new();
        let mut has_anchor = false;
        for (_, e) in ks.chain.range(..=target_lsn).rev() {
            out.push(e.row_arc());
            if e.row.will_init {
                has_anchor = true;
                break;
            }
        }
        has_anchor.then_some(out)
    }

    /// Like `lookup_chain_if_complete` but only durable rows. Used
    /// by the materialiser's cache-only fast path so it never folds
    /// pre-flush deltas into an image.
    ///
    /// Caller invariant: `target_lsn ≤ flush_horizon_lsn`. Under that
    /// invariant every cache row with LSN ≤ target_lsn is durable
    /// (post-flush `mark_flushed` promotes them all atomically), so
    /// this is a plain backward walk for an anchor.
    ///
    /// Seeing an unflushed row here is therefore either (a) the
    /// benign attach-time race — legacy compaction overlaps the
    /// walreceiver's replay, which re-puts rows that are already
    /// durable in YB from the first pass as fresh (unflushed) cache
    /// entries the cache can't tell apart — or (b) a real horizon
    /// violation at a dispatch site (#22 bug B family). Both are
    /// answered the same way: bail out of the fast path and let
    /// `read_chain_durable` consult YB, which holds the durable
    /// truth in case (a) and surfaces the violation as a retryable
    /// redo failure in case (b), instead of silently folding a
    /// pre-flush row into an image. The counter + warn replace what
    /// used to be a `debug_assert!` — release builds had no tripwire
    /// at all.
    pub fn lookup_chain_durable_if_complete(
        &self,
        key: &Key,
        target_lsn: Lsn,
    ) -> Option<Vec<Arc<PageRow>>> {
        let node = self.by_key.get(key)?;
        let ks = node.value().lock();
        let mut out = Vec::new();
        for (_, e) in ks.chain.range(..=target_lsn).rev() {
            if !e.is_durable() {
                crate::metrics::CORRECTNESS_ERRORS
                    .with_label_values(&["durable_walk_unflushed_row"])
                    .inc();
                use std::sync::Mutex;

                use once_cell::sync::Lazy;
                use utils::rate_limit::RateLimit;
                static LOGGED: Lazy<Mutex<RateLimit>> = Lazy::new(|| {
                    Mutex::new(RateLimit::new(std::time::Duration::from_secs(10)))
                });
                if let Ok(mut limiter) = LOGGED.try_lock() {
                    let (lsn, tgt) = (e.row.lsn, target_lsn);
                    limiter.call(|| {
                        tracing::warn!(
                            "durable chain walk saw an unflushed row at lsn={lsn} \
                             ≤ target_lsn={tgt}; falling back to the YB read. \
                             Benign during attach replay; a sustained rate \
                             outside attach means a flush-horizon violation \
                             (metric correctness_errors{{durable_walk_unflushed_row}})",
                        );
                    });
                }
                return None;
            }
            out.push(e.row_arc());
            if e.row.will_init {
                return Some(out);
            }
        }
        None
    }

    /// Range scan: rows for keys in `[start_key, end_key)` at `lsn ≤
    /// target_lsn`. Each per-key inner Vec is LSN-descending.
    ///
    /// Walks only the keys in `[start_key, end_key)` via the skiplist's
    /// ordered `range()` — O(log n + k) in the number of matching keys,
    /// not O(n) over the whole map. Not on the getpage hot path —
    /// scan_range is called for vectored reads, which are rare.
    pub fn lookup_range(
        &self,
        start_key: &Key,
        end_key: &Key,
        target_lsn: Lsn,
    ) -> HashMap<Key, Vec<Arc<PageRow>>> {
        let mut out: HashMap<Key, Vec<Arc<PageRow>>> = HashMap::new();
        for node in self.by_key.range(*start_key..*end_key) {
            let rows: Vec<Arc<PageRow>> = node
                .value()
                .lock()
                .chain
                .range(..=target_lsn)
                .rev()
                .map(|(_, e)| e.row_arc())
                .collect();
            if !rows.is_empty() {
                out.insert(*node.key(), rows);
            }
        }
        out
    }

    /// Take a snapshot of up to `max` (or all) unflushed entries in
    /// insertion order. Caller (`flush`) uses this to drive the YB
    /// INSERT without holding any per-shard or per-key lock for the
    /// duration of the round-trip. Each entry is returned as an
    /// `Arc<PageEntry>` shared with the cache slot, so the matching
    /// `mark_flushed` call flips `durable` on the exact entry the
    /// caller wrote — no `(key, lsn)` lookup, no risk of the FIFO
    /// pointing at a stale slot.
    pub(crate) fn snapshot_unflushed(&self, max: Option<usize>) -> Vec<Arc<PageEntry>> {
        // Clone the FIFO order so we can release the lock before
        // walking it. The FIFO is small (one entry per unflushed
        // row), and snapshot is called once per flush — cheap.
        let pairs: Vec<(Key, Lsn)> = {
            let fifo = self.unflushed_fifo.lock();
            let cap = max.unwrap_or(fifo.len()).min(fifo.len());
            fifo.iter().take(cap).cloned().collect()
        };
        let mut out = Vec::with_capacity(pairs.len());
        for (key, lsn) in &pairs {
            // #57 probe: every FIFO pair MUST resolve to a live,
            // non-durable slot. `flush_serialize` means no concurrent
            // flush/`mark_flushed`, and an unflushed entry pins its key
            // against eviction/trim — so a missing slot, a missing key,
            // or an already-durable slot means an unflushed row is about
            // to be DROPPED from this flush (it won't be written, but its
            // record advanced `ingest_frontier`) → durable write loss.
            let anomaly: Option<&'static str> = match self.by_key.get(key) {
                None => Some("key-gone"),
                Some(node) => match node.value().lock().chain.get(lsn) {
                    None => Some("slot-gone"),
                    Some(entry) if entry.is_durable() => Some("already-durable"),
                    Some(entry) => {
                        out.push(Arc::clone(entry));
                        None
                    }
                },
            };
            // Diagnostics only — debug builds (e2e). Zero release cost;
            // the functional skip above is identical in both profiles.
            if cfg!(debug_assertions)
                && let Some(why) = anomaly
            {
                crate::metrics::CORRECTNESS_ERRORS
                    .with_label_values(&["snapshot_fifo_orphan"])
                    .inc();
                let mut kb = [0u8; KEY_SIZE];
                key.write_to_byte_slice(&mut kb);
                let prefix: String =
                    kb.iter().map(|b| format!("{b:02x}")).collect();
                tracing::error!(
                    key = %prefix,
                    %lsn,
                    why,
                    "snapshot_unflushed: FIFO entry has no flushable unflushed slot \
                     — pending write will be dropped from this flush (#57)"
                );
            }
        }
        out
    }

    /// Insert or overwrite a `PageEntry` for `(key, lsn)`. Returns
    /// `Some(prev_was_unflushed)` if this overwrote an existing
    /// entry, `None` if it's fresh. Adjusts `total_bytes`,
    /// `unflushed_bytes`, and per-key `bytes` / `unflushed_count`.
    /// The key is promoted to the back of the LRU on every call —
    /// new keys land at the back via `insert`, existing keys are
    /// moved with `to_back`.
    ///
    /// LRU and the per-key mutex are never held at the same time, so
    /// there is no lock-order edge between them to deadlock on.
    /// Returns `true` iff a brand-new **unflushed** entry was added, so
    /// the caller (`push`) knows to record it in `unflushed_fifo`.
    /// Durable inserts and idempotent re-pushes return `false`.
    fn insert_entry(&self, key: Key, lsn: Lsn, row: PageRow, durable: bool) -> bool {
        self.insert_arc_entry(key, lsn, Arc::new(row), durable)
    }

    fn insert_arc_entry(&self, key: Key, lsn: Lsn, row: Arc<PageRow>, durable: bool) -> bool {
        use std::collections::btree_map::Entry;

        let size = row_size(&row);
        // Wrapped in an Option so the (rare) `is_removed` retry below can
        // re-take ownership; it is consumed exactly once, on the iteration
        // that lands on a live node.
        let mut row = Some(row);

        // Set when a durable image overwrites a still-unflushed slot. Acted
        // on after the per-key mutex drops so the FIFO lock never nests under
        // it (lock order: per-key mutex → FIFO).
        let mut overwrote_unflushed = false;
        let added_unflushed = loop {
            // Step 1: ensure the key is at the back of the LRU.
            {
                let mut lru = self.lru.lock();
                if lru.contains_key(&key) {
                    lru.to_back(&key);
                } else {
                    lru.insert(key, ());
                }
            }

            // Step 2: get-or-create this key's node and lock its mutex. The
            // BTreeMap `entry` API below is one tree traversal covering both
            // the existence check and the insert (no separate `get` + `insert`).
            //
            // A slot may already exist at this (key, lsn):
            //   - `push()` (walreceiver, durable=false): the connection
            //     re-streamed an already-ingested record — the cache is not
            //     empty across a safekeeper-only restart (#57). The WAL
            //     record at a given LSN is deterministic, so the existing
            //     slot already represents this row, whether it's still
            //     unflushed (pending the same flush, and already in the
            //     FIFO) or already durable. Keep it untouched and report
            //     "nothing added": re-inserting would double-count the
            //     unflushed FIFO / pin, and overwriting could drop a row
            //     that isn't yet in YB.
            //   - `push_durable()` (materialiser, durable=true): bounds its
            //     target_lsn ≤ flush_horizon_lsn (only advanced after
            //     `mark_flushed`), so a same-LSN slot is normally already
            //     durable. It can also be a still-UNFLUSHED slot from a benign
            //     reconnect-replay re-push (see the durable arm below); in that
            //     case we overwrite it with the image and reverse the unflushed
            //     accounting + drop the stale FIFO entry.
            let node = self
                .by_key
                .get_or_insert(key, Mutex::new(KeyState::new()));
            let mut ks_guard = node.value().lock();
            if node.is_removed() {
                // Raced an eviction/trim that removed this node between
                // `get_or_insert` and the lock. It's detached; retry to land
                // on (or create) a live node. The LRU re-insert at the top of
                // the loop restores the key if the eviction also dropped it.
                drop(ks_guard);
                continue;
            }
            let ks = &mut *ks_guard;

            // Returns (added_unflushed, add_bytes, sub_bytes); bookkeeping is
            // applied after the chain borrow ends.
            let (added_unflushed, add_bytes, sub_bytes): (bool, usize, usize) =
                match ks.chain.entry(lsn) {
                    Entry::Occupied(mut o) => {
                        if !durable {
                            // Idempotent re-push; keep the existing slot. Two
                            // legitimate cases:
                            //   (a) existing is a delta — the walreceiver
                            //       re-streamed the same deterministic WAL
                            //       record, so it must be byte-identical;
                            //   (b) existing is a materialiser IMAGE at this
                            //       LSN — folded at a record's max LSN, which
                            //       collides with that record's own delta LSN.
                            //       The image supersedes the same-LSN delta
                            //       (`read_chain`'s tie-break), so keeping it
                            //       is correct even though it differs.
                            // Only assert determinism in case (a); never panic
                            // on (b) and never overwrite an image with a delta.
                            let new_row = row.as_ref().unwrap();
                            debug_assert!(
                                o.get().row.is_image || *o.get().row == **new_row,
                                "re-push at lsn={lsn} differs from the buffered delta \
                                 (WAL replay not deterministic / LSN collision): \
                                 existing=(is_image={} will_init={} {}B) \
                                 new=(is_image={} will_init={} {}B)",
                                o.get().row.is_image,
                                o.get().row.will_init,
                                o.get().row.payload.len(),
                                new_row.is_image,
                                new_row.will_init,
                                new_row.payload.len(),
                            );
                            return false;
                        }
                        let prev_size = row_size(&o.get().row);
                        // push_durable (materialiser image). The slot is
                        // normally already durable. It can also be a NON-
                        // durable slot from a benign reconnect-replay re-push:
                        // after a walreceiver reconnect the replay re-`put`s a
                        // row that was already flushed (hence evicted/trimmed)
                        // as a fresh unflushed entry, and the image — folded
                        // from that same durable row in YB — lands on the same
                        // (key, lsn). The image's LSN can equal this slot's
                        // LSN only if a DURABLE row existed here (that LSN is
                        // where the fold's chain head came from), so the
                        // unflushed slot is a deterministic re-push of an
                        // already-durable record and overwriting it loses
                        // nothing. Record it so we reverse the unflushed
                        // accounting (count + bytes below, FIFO entry after
                        // the guard drops).
                        overwrote_unflushed = !o.get().is_durable();
                        o.insert(PageEntry::from_arc_row(row.take().unwrap(), durable));
                        (false, size, prev_size)
                    }
                    Entry::Vacant(v) => {
                        v.insert(PageEntry::from_arc_row(row.take().unwrap(), durable));
                        (!durable, size, 0)
                    }
                };

            ks.bytes = ks.bytes + add_bytes - sub_bytes;
            if added_unflushed {
                ks.unflushed_count += 1;
            } else if overwrote_unflushed {
                // The overwritten slot was pinned as unflushed; it's durable
                // now, so drop the pin.
                ks.unflushed_count = ks.unflushed_count.saturating_sub(1);
            }
            self.total_bytes.fetch_add(add_bytes, Ordering::Relaxed);
            if sub_bytes > 0 {
                self.total_bytes.fetch_sub(sub_bytes, Ordering::Relaxed);
            }
            if added_unflushed {
                self.unflushed_bytes.fetch_add(add_bytes, Ordering::Relaxed);
            } else if overwrote_unflushed {
                // Its bytes (== sub_bytes) were in unflushed_bytes; the image
                // is durable, so remove them.
                self.unflushed_bytes.fetch_sub(sub_bytes, Ordering::Relaxed);
            }
            break added_unflushed;
        };

        if overwrote_unflushed {
            // Drop the now-stale FIFO entry the re-push left behind. The slot
            // is durable now, so without this `snapshot_unflushed` would
            // re-probe and skip it on every future flush — a slow FIFO leak.
            // The shard guard is released above, so taking the FIFO lock here
            // respects the shard → FIFO lock order.
            let mut fifo = self.unflushed_fifo.lock();
            if let Some(pos) = fifo.iter().position(|(k, l)| *l == lsn && k == &key) {
                fifo.remove(pos);
            }
        }
        added_unflushed
    }
}

fn row_size(r: &PageRow) -> usize {
    r.payload.len() + KEY_SIZE + 16
}

#[cfg(test)]
mod tests {
    use super::*;
    use bytes::Bytes;

    /// Build a `Key` from a short test byte string, order-preserving:
    /// zero-pad to KEY_SIZE and decode. Lexicographic byte order maps
    /// to the same `Key` ordering, so range/ordering assertions hold.
    fn tkey(b: &[u8]) -> Key {
        let mut a = [0u8; KEY_SIZE];
        let n = b.len().min(KEY_SIZE);
        a[..n].copy_from_slice(&b[..n]);
        Key::from_slice(&a)
    }

    fn mk(key: &[u8], lsn: u64, is_image: bool, payload: &[u8]) -> PageRow {
        PageRow {
            key: tkey(key),
            lsn: Lsn(lsn),
            is_image,
            will_init: is_image,
            payload: Bytes::copy_from_slice(payload),
        }
    }

    /// Test shortcut: snapshot the first `n` unflushed entries and
    /// flip them durable. Mirrors prod's flush sequence
    /// (`snapshot_unflushed(Some(n))` + `mark_flushed`) so the tests
    /// exercise the same code path as `PageWriter::flush`.
    fn flush_first_n(u: &UnifiedPages, n: usize) {
        let snap = u.snapshot_unflushed(Some(n));
        u.mark_flushed(&snap);
    }

    #[test]
    fn push_and_lookup_chain_descending() {
        let u = UnifiedPages::new(4096);
        u.push(mk(b"k", 10, false, b"a"));
        u.push(mk(b"k", 20, false, b"b"));
        u.push(mk(b"k", 5, true, b"c"));
        let chain = u.lookup_chain(&tkey(b"k"), Lsn(100));
        let lsns: Vec<u64> = chain.iter().map(|r| r.lsn.0).collect();
        assert_eq!(lsns, vec![20, 10, 5]);
    }

    #[test]
    fn mark_flushed_promotes_without_moving_rows() {
        let u = UnifiedPages::new(4096);
        u.push(mk(b"k", 1, false, b"a"));
        u.push(mk(b"k", 2, false, b"b"));
        u.push(mk(b"k", 3, false, b"c"));
        let unflushed_before = u.unflushed_bytes();

        flush_first_n(&u, 2);
        assert!(u.unflushed_bytes() > 0);
        assert!(u.unflushed_bytes() < unflushed_before);

        let all = u.lookup_chain(&tkey(b"k"), Lsn(100));
        assert_eq!(all.len(), 3, "all rows still visible after flush");
    }

    #[test]
    fn lookup_durable_filters_unflushed() {
        let u = UnifiedPages::new(4096);
        u.push(mk(b"k", 1, true, b"img"));
        u.push(mk(b"k", 2, false, b"d2"));
        flush_first_n(&u, 1);
        u.push(mk(b"k", 3, false, b"d3"));

        let all = u.lookup_chain(&tkey(b"k"), Lsn(100));
        let durable = u.lookup_chain_durable(&tkey(b"k"), Lsn(100));
        assert_eq!(all.len(), 3);
        assert_eq!(durable.len(), 1);
        assert_eq!(durable[0].lsn, Lsn(1));
    }

    /// An unflushed row at/below the target makes the durable fast
    /// path bail out (return None) instead of panicking (the old
    /// `debug_assert!`, absent in release) or folding the pre-flush
    /// row into the chain. The caller then consults YB, which is
    /// correct both for the benign attach-replay race and for a real
    /// flush-horizon violation.
    #[test]
    fn durable_walk_bails_on_unflushed_row() {
        let u = UnifiedPages::new(4096);
        u.push(mk(b"k", 1, true, b"img"));
        u.push(mk(b"k", 2, false, b"d2"));
        flush_first_n(&u, 2);
        // Complete durable chain below the target: fast path serves it.
        assert!(
            u.lookup_chain_durable_if_complete(&tkey(b"k"), Lsn(100))
                .is_some()
        );
        // An unflushed row enters at lsn 3 ≤ target: fast path must
        // bail rather than serve a chain it can't prove durable.
        u.push(mk(b"k", 3, false, b"d3"));
        assert!(
            u.lookup_chain_durable_if_complete(&tkey(b"k"), Lsn(100))
                .is_none()
        );
        // A target below the unflushed row keeps the fast path.
        assert!(
            u.lookup_chain_durable_if_complete(&tkey(b"k"), Lsn(2))
                .is_some()
        );
    }

    #[test]
    fn evict_drops_whole_cold_keys() {
        // Per-key eviction: many small durable keys, push hard until
        // we blow the cap; eviction should drop coldest keys whole.
        // The 2 ms eviction budget can cut a single call short under
        // CI / cargo-test parallelism, so loop until the cap is met
        // (no infinite loop: each iteration makes forward progress
        // because the keys are unpinned).
        let u = UnifiedPages::new(64);
        for i in 0u32..1000 {
            u.push_durable(mk(&i.to_be_bytes(), i as u64, true, &[0u8; 32]));
        }
        while u.total_bytes() > u.capacity_bytes {
            let before = u.total_bytes();
            u.evict_to_capacity();
            // Every call must make forward progress as long as there
            // are unpinned keys above the cap.
            assert!(u.total_bytes() < before, "evict made no progress");
        }
    }

    #[test]
    fn push_durable_is_immediately_evictable() {
        let u = UnifiedPages::new(20);
        u.push_durable(mk(b"k1", 1, true, b"AAAAAAAAAAAAAAAA"));
        u.push_durable(mk(b"k2", 2, true, b"BBBBBBBBBBBBBBBB"));
        u.evict_to_capacity();
        // k1 was inserted first → at the front → evicted first.
        assert!(
            u.lookup_chain(&tkey(b"k1"), Lsn(100))
                .is_empty()
        );
    }

    #[test]
    fn evict_skips_pinned_keys() {
        // Every key has an unflushed row → all pinned → no-op.
        let u = UnifiedPages::new(10);
        u.push(mk(b"k1", 1, true, b"xxxxxxxx"));
        u.push(mk(b"k2", 2, true, b"xxxxxxxx"));
        u.evict_to_capacity();
        assert!(u.total_bytes() > u.capacity_bytes);
        assert_eq!(u.by_key_entries(), 2);
    }

    #[test]
    fn evict_drops_only_unpinned_keys() {
        // k1 is durable (unpinned), k2 has an unflushed row (pinned).
        // Eviction must drop k1 and leave k2 alone, even with k1 at
        // the front initially.
        let u = UnifiedPages::new(20);
        u.push_durable(mk(b"k1", 1, true, b"AAAAAAAAAAAAAAAA"));
        u.push(mk(b"k2", 2, true, b"BBBBBBBBBBBBBBBB"));
        u.evict_to_capacity();
        assert!(
            u.lookup_chain(&tkey(b"k1"), Lsn(100))
                .is_empty()
        );
        assert!(
            !u.lookup_chain(&tkey(b"k2"), Lsn(100))
                .is_empty()
        );
    }

    #[test]
    fn trim_drops_only_durable_le_max_lsn() {
        let u = UnifiedPages::new(4096);
        u.push(mk(b"k", 10, false, b"d10"));
        u.push(mk(b"k", 20, false, b"d20"));
        u.push(mk(b"k", 30, false, b"d30"));
        flush_first_n(&u, 2);
        u.trim_key_up_to(&tkey(b"k"), Lsn(25));
        let chain = u.lookup_chain(&tkey(b"k"), Lsn(100));
        assert_eq!(chain.len(), 1);
        assert_eq!(chain[0].lsn, Lsn(30));
    }

    #[test]
    fn trim_leaves_unflushed_in_range() {
        let u = UnifiedPages::new(4096);
        u.push(mk(b"k", 10, false, b"d10"));
        flush_first_n(&u, 1);
        u.push(mk(b"k", 20, false, b"d20"));
        u.trim_key_up_to(&tkey(b"k"), Lsn(50));
        let chain = u.lookup_chain(&tkey(b"k"), Lsn(100));
        assert_eq!(chain.len(), 1);
        assert_eq!(chain[0].lsn, Lsn(20));
    }

    #[test]
    fn materializer_install_sequence_works_end_to_end() {
        let u = UnifiedPages::new(4096);
        for i in 1..=5 {
            u.push(mk(b"k", i * 10, false, &[i as u8; 4]));
        }
        flush_first_n(&u, 5);
        u.trim_key_up_to(&tkey(b"k"), Lsn(50));
        u.push_durable(mk(b"k", 50, true, b"full-page-image"));
        let chain = u.lookup_chain_durable(&tkey(b"k"), Lsn(100));
        assert_eq!(chain.len(), 1, "only the new image remains");
        assert!(chain[0].is_image);
        assert_eq!(chain[0].lsn, Lsn(50));
    }

    #[test]
    fn snapshot_unflushed_returns_pre_flush_only() {
        let u = UnifiedPages::new(4096);
        u.push(mk(b"k", 1, false, b"a"));
        u.push(mk(b"k", 2, false, b"b"));
        flush_first_n(&u, 1);
        u.push(mk(b"k", 3, false, b"c"));
        let snap = u.snapshot_unflushed(None);
        assert_eq!(snap.len(), 2);
        assert_eq!(snap[0].row.lsn, Lsn(2));
        assert_eq!(snap[1].row.lsn, Lsn(3));
    }

    #[test]
    fn complete_chain_with_image_anchor_in_cache() {
        let u = UnifiedPages::new(4096);
        u.push_durable(mk(b"k", 50, true, b"image"));
        u.push(mk(b"k", 60, false, b"d60"));
        u.push(mk(b"k", 70, false, b"d70"));
        let chain = u
            .lookup_chain_if_complete(&tkey(b"k"), Lsn(100))
            .expect("anchor in cache");
        let lsns: Vec<u64> = chain.iter().map(|r| r.lsn.0).collect();
        assert_eq!(lsns, vec![70, 60, 50]);
    }

    #[test]
    fn incomplete_when_no_anchor_in_cache() {
        let u = UnifiedPages::new(4096);
        u.push(mk(b"k", 60, false, b"d60"));
        u.push(mk(b"k", 70, false, b"d70"));
        assert!(
            u.lookup_chain_if_complete(&tkey(b"k"), Lsn(100))
                .is_none(),
            "no will_init → must consult YB"
        );
    }

    /// Regression for the AWS BTREE PANIC: with per-row FIFO eviction,
    /// an intermediate delta could be popped while the anchor stayed,
    /// leaving a mid-chain hole that `lookup_chain_durable_if_complete`
    /// served as "complete". Per-key whole-eviction structurally
    /// prevents this — either the key is present (full chain) or
    /// absent.
    #[test]
    fn whole_key_eviction_never_leaves_mid_chain_hole() {
        let u = UnifiedPages::new(40);
        // Hot key with chain: anchor + 3 deltas. Each row is ~40 B
        // (16 B payload + 4 B key + 16 B overhead = 36).
        u.push_durable(mk(b"hot", 100, true, b"AAAAAAAAAAAAAAAA"));
        u.push_durable(mk(b"hot", 110, false, b"BBBBBBBBBBBBBBBB"));
        u.push_durable(mk(b"hot", 120, false, b"CCCCCCCCCCCCCCCC"));
        u.push_durable(mk(b"hot", 130, false, b"DDDDDDDDDDDDDDDD"));
        // Cold key, even bigger so total bytes well exceed cap.
        u.push_durable(mk(b"cold", 1, true, b"EEEEEEEEEEEEEEEE"));
        u.evict_to_capacity();

        let r = u.lookup_chain_durable_if_complete(&tkey(b"hot"), Lsn(200));
        match r {
            Some(chain) => {
                // If hot is still cached, it must have the anchor and
                // all 3 deltas — never a hole.
                let lsns: Vec<u64> = chain.iter().map(|r| r.lsn.0).collect();
                assert_eq!(lsns, vec![130, 120, 110, 100]);
            }
            None => {
                // Acceptable: hot got evicted whole.
                assert!(
                    u.lookup_chain(&tkey(b"hot"), Lsn(200))
                        .is_empty(),
                    "key absent ⇒ no rows at all (no partial cache state)"
                );
            }
        }
    }

    /// The materialiser overwrite path: a durable delta at lsn=L gets
    /// replaced by a durable image at lsn=L. The byte counters must
    /// reflect the new payload size; the chain must show the image.
    /// (The pre-overwrite slot is durable — `push_durable` overwriting
    /// an unflushed slot is forbidden by its debug-assert.)
    #[test]
    fn overwrite_same_key_lsn_adjusts_bytes() {
        let u = UnifiedPages::new(4096);
        u.push(mk(b"k", 10, false, b"small"));
        flush_first_n(&u, 1); // promote to durable first
        let b1 = u.total_bytes();
        u.push_durable(mk(b"k", 10, true, b"larger-replacement"));
        assert!(u.total_bytes() > b1);
        let chain = u.lookup_chain(&tkey(b"k"), Lsn(100));
        assert_eq!(chain.len(), 1);
        assert!(chain[0].is_image);
    }

    #[test]
    fn lru_order_is_insertion_order_with_promotion_on_write() {
        // k1 then k2 then update k1 → k2 becomes the LRU front, but
        // the cap is sized so that only ONE key needs to be evicted
        // to fit. Per-row size: 50 B (16 payload + KEY_SIZE=18 key + 16
        // overhead). After 3 pushes, total = 150 B (k1 = 2 rows = 100,
        // k2 = 1 row = 50). Cap = 120 evicts just the LRU front (k2):
        // dropping it leaves 100 ≤ 120, so k1 survives.
        let u = UnifiedPages::new(120);
        u.push_durable(mk(b"k1", 1, true, b"AAAAAAAAAAAAAAAA"));
        u.push_durable(mk(b"k2", 2, true, b"BBBBBBBBBBBBBBBB"));
        // Write to k1 again — promotes it to the back of the LRU.
        u.push_durable(mk(b"k1", 3, false, b"CCCCCCCCCCCCCCCC"));
        u.evict_to_capacity();
        // k2 was at the front after the k1 promotion → evicted.
        assert!(
            u.lookup_chain(&tkey(b"k2"), Lsn(100))
                .is_empty()
        );
        assert!(
            !u.lookup_chain(&tkey(b"k1"), Lsn(100))
                .is_empty()
        );
    }

    /// Range-scan: confirm the half-open `[start, end)` filter and that
    /// each per-key Vec comes back LSN-descending.
    #[test]
    fn lookup_range_filters_and_orders_descending() {
        let u = UnifiedPages::new(4096);
        u.push(mk(b"a", 10, false, b"x"));
        u.push(mk(b"b", 20, true, b"y"));
        u.push(mk(b"b", 30, false, b"z"));
        u.push(mk(b"c", 40, true, b"w"));
        let out = u.lookup_range(
            &tkey(b"b"),
            &tkey(b"c"),
            Lsn(100),
        );
        assert_eq!(out.len(), 1, "only 'b' falls in [b, c)");
        let b_chain = &out[&tkey(b"b")];
        let lsns: Vec<u64> = b_chain.iter().map(|r| r.lsn.0).collect();
        assert_eq!(lsns, vec![30, 20], "per-key chain is LSN-desc");
    }

    /// Regression: every key in the cache is pinned (unflushed) →
    /// `evict_to_capacity` must terminate without making progress
    /// rather than spinning forever rotating pins to the back.
    #[test]
    fn evict_all_pinned_terminates_as_noop() {
        let u = UnifiedPages::new(8);
        for i in 0u32..5 {
            u.push(mk(&i.to_be_bytes(), i as u64, true, &[0u8; 32]));
        }
        let before = u.total_bytes();
        u.evict_to_capacity();
        assert_eq!(
            u.total_bytes(),
            before,
            "all pinned ⇒ no eviction; total_bytes unchanged",
        );
        assert_eq!(u.by_key_entries(), 5, "all keys still resident");
    }

    /// `push_durable` landing on a still-unflushed slot is the benign
    /// reconnect-replay re-push (#57): the walreceiver replayed an
    /// already-durable-in-YB row as a fresh unflushed entry, and the
    /// materialiser's image — folded from that same durable row — collides
    /// on the same `(key, lsn)`. Overwriting with the image is safe; the
    /// unflushed accounting (pin count, byte tally, and FIFO entry) must be
    /// reversed so the now-durable slot isn't left counted as pending.
    /// (Earlier this tripped a debug-assert.)
    #[test]
    fn push_durable_over_unflushed_overwrites_and_fixes_bookkeeping() {
        let u = UnifiedPages::new(4096);
        u.push(mk(b"k", 10, false, b"unflushed"));
        assert_eq!(u.unflushed_count(), 1);
        assert!(u.unflushed_bytes() > 0);

        u.push_durable(mk(b"k", 10, true, b"image-replacement"));

        // Pin count, unflushed byte tally, and the stale FIFO entry are
        // all reversed — the slot is durable now, not pending.
        assert_eq!(u.unflushed_count(), 0, "stale FIFO entry dropped");
        assert_eq!(u.unflushed_bytes(), 0, "unflushed byte tally reversed");

        // The slot holds the durable image, and a flush finds nothing to
        // snapshot (no phantom pending row left behind).
        let chain = u.lookup_chain_durable(&tkey(b"k"), Lsn(100));
        assert_eq!(chain.len(), 1, "durable image present at (k, 10)");
        assert!(chain[0].is_image, "slot replaced by the materialiser image");
        assert_eq!(
            u.snapshot_unflushed(None).len(),
            0,
            "nothing left to flush after the overwrite"
        );
    }

    /// Regression (#57): a safekeeper-only restart makes the
    /// walreceiver reconnect and replay records that are still buffered
    /// as unflushed cache rows (the pageserver never restarted, so the
    /// cache isn't empty). Re-`push`ing the same `(key, lsn)` must be
    /// idempotent — the existing entry is kept, the unflushed FIFO and
    /// pin are not double-counted, and a durable slot is not reverted
    /// to unflushed. Before the fix this panicked (debug) or silently
    /// overwrote and lost the pending row (release).
    #[test]
    fn push_same_key_lsn_is_idempotent() {
        let u = UnifiedPages::new(4096);
        u.push(mk(b"k", 10, false, b"first"));
        let bytes_after_first = u.total_bytes();

        // Replay of the same record while still unflushed: no-op.
        u.push(mk(b"k", 10, false, b"first"));
        assert_eq!(u.unflushed_count(), 1, "FIFO must not double-count the replayed row");
        assert_eq!(
            u.total_bytes(),
            bytes_after_first,
            "byte counters unchanged on idempotent re-push"
        );
        assert_eq!(
            u.lookup_chain(&tkey(b"k"), Lsn(100)).len(),
            1,
            "still a single slot at (k, 10)"
        );

        // The single kept entry promotes to durable exactly once.
        flush_first_n(&u, 1);
        assert_eq!(u.unflushed_count(), 0, "single entry promoted, pin cleared");

        // Replay after the row is durable: still a no-op — the slot
        // stays durable rather than being re-pinned as unflushed.
        u.push(mk(b"k", 10, false, b"first"));
        assert_eq!(u.unflushed_count(), 0, "re-push over a durable slot must not re-pin it");
    }

    /// The idempotent skip is only sound if the replayed row is
    /// identical. A divergent re-push at the same `(key, lsn)` — which
    /// would mean WAL replay wasn't deterministic or two records
    /// collided on an LSN — trips the debug-assert instead of silently
    /// keeping the wrong page.
    #[test]
    #[should_panic(expected = "differs from the buffered delta")]
    fn push_same_key_lsn_divergent_payload_panics_in_debug() {
        let u = UnifiedPages::new(4096);
        u.push(mk(b"k", 10, false, b"first"));
        u.push(mk(b"k", 10, false, b"DIFFERENT"));
    }

    /// #57 part 2: the materialiser installs an image at a record's
    /// max LSN, which collides with that record's own delta LSN. When the
    /// walreceiver later re-streams the record (SK-only restart replay),
    /// `push` of the raw delta lands on the existing image at the same
    /// LSN. The image is the folded result and supersedes the delta, so
    /// `push` must keep it WITHOUT panicking on the (legitimate) content
    /// difference — the over-strict byte-identity assert here is what
    /// wedged the walreceiver in a panic loop (debug) and deadlocked the
    /// basebackup waiting for the un-ingested tail.
    #[test]
    fn push_delta_over_materialised_image_keeps_image() {
        let u = UnifiedPages::new(4096);
        // Materialiser image at lsn 10 (folded at a record's lsn).
        u.push_durable(mk(b"k", 10, true, b"folded-image"));
        // Walreceiver re-streams that record's raw delta at the same lsn.
        // Must NOT panic and must keep the image.
        u.push(mk(b"k", 10, false, b"raw-delta-record"));
        let chain = u.lookup_chain(&tkey(b"k"), Lsn(100));
        assert_eq!(chain.len(), 1, "single slot at (k, 10)");
        assert!(chain[0].is_image, "image kept over the same-lsn delta");
        assert_eq!(chain[0].payload.as_ref(), b"folded-image");
    }

    /// Regression for the AWS BTREE walredo PANIC on 2026-06-02,
    /// updated for the always-on tripwire (#22 hardening): a
    /// target_lsn covering an unflushed row no longer panics (the old
    /// `debug_assert!` was compiled out of release builds anyway) —
    /// the durable fast path bails with None in every build profile
    /// and the caller falls back to the YB read. The richer scenario
    /// (interleaved durable rows above the unflushed one) is kept
    /// from the original test.
    #[test]
    fn durable_if_complete_bails_on_unflushed_in_range() {
        let u = UnifiedPages::new(4096);
        u.push(mk(b"k", 10, true, b"anchor"));
        flush_first_n(&u, 1);
        u.push(mk(b"k", 20, false, b"d20"));
        flush_first_n(&u, 1);
        u.push(mk(b"k", 30, false, b"d30")); // UNFLUSHED
        u.push_durable(mk(b"k", 40, false, b"d40")); // already durable
        // target_lsn=100 covers the unflushed d30: bail.
        assert!(
            u.lookup_chain_durable_if_complete(&tkey(b"k"), Lsn(100))
                .is_none()
        );
        // target_lsn=20 walks only durable rows: fast path serves.
        assert!(
            u.lookup_chain_durable_if_complete(&tkey(b"k"), Lsn(20))
                .is_some()
        );
    }

    /// Materialiser install at LSN N MUST drop every durable row with
    /// LSN ≤ N for that key so we can't fall into the "old image plus
    /// new delta" trap. Exercised through `install_image_if_extends`, the
    /// atomic guarded install the materialiser actually uses.
    #[test]
    fn install_drops_all_older_durable_rows() {
        let u = UnifiedPages::new(4096);
        u.push(mk(b"k", 10, true, b"old-image"));
        u.push(mk(b"k", 20, false, b"d20"));
        u.push(mk(b"k", 30, false, b"d30"));
        flush_first_n(&u, 3);
        // Install fresh image at lsn 25: key present, 25 ≥ min(10) → installs.
        assert!(u.install_image_if_extends(Arc::new(mk(b"k", 25, true, b"materialised"))));
        let chain = u.lookup_chain_durable(&tkey(b"k"), Lsn(100));
        let lsns: Vec<u64> = chain.iter().map(|r| r.lsn.0).collect();
        // The deltas/image ≤ 25 are gone; d30 stays because it's above
        // the new image.
        assert_eq!(lsns, vec![30, 25]);
    }

    /// The install guard refuses to seed an anchor into an ABSENT key.
    /// That is the read-side-materialise hole: the materialiser folds an
    /// image, the key gets evicted before the install lands, and a lone
    /// `image@L` anchor would then serve a chain missing the rows above L
    /// that live only in YB. Skipping leaves the key absent → reads fall
    /// through to the YB blend, which is correct.
    #[test]
    fn install_skips_absent_key() {
        let u = UnifiedPages::new(4096);
        // Nothing cached for "k".
        assert!(!u.install_image_if_extends(Arc::new(mk(b"k", 50, true, b"image"))));
        assert_eq!(u.by_key_entries(), 0, "absent key must not be re-seeded");
        assert!(u.lookup_chain_if_complete(&tkey(b"k"), Lsn(100)).is_none());
    }

    /// The install guard refuses to install an image BELOW the lowest
    /// cached row: that would leave a gap `(image.lsn, min_cached)` living
    /// only in YB, and the image would become a stale mid-chain anchor for
    /// reads into the gap.
    #[test]
    fn install_skips_below_min_cached() {
        let u = UnifiedPages::new(4096);
        // Cache holds a complete suffix starting at lsn 30 (older rows
        // already evicted/trimmed away — they live only in YB now).
        u.push(mk(b"k", 30, true, b"image30"));
        u.push(mk(b"k", 40, false, b"d40"));
        flush_first_n(&u, 2);
        // A stale image folded at lsn 20 (< min_cached 30) must be refused.
        assert!(!u.install_image_if_extends(Arc::new(mk(b"k", 20, true, b"stale"))));
        // The chain is untouched: still anchored at 30, no lsn-20 anchor.
        let chain = u.lookup_chain_durable(&tkey(b"k"), Lsn(100));
        let lsns: Vec<u64> = chain.iter().map(|r| r.lsn.0).collect();
        assert_eq!(lsns, vec![40, 30]);
        // A read into the gap finds no fast-path anchor (≥ lsn 20, < 30)
        // and correctly falls through to YB.
        assert!(u.lookup_chain_if_complete(&tkey(b"k"), Lsn(25)).is_none());
    }

    /// A HISTORICAL image install is allowed (and useful — static/branch
    /// readers anchor on it) as long as the key is present with a complete
    /// suffix above the image: `image.lsn ≥ min_cached`. The result must
    /// stay correct for reads both at/above and below the historical image.
    #[test]
    fn install_historical_when_suffix_complete() {
        let u = UnifiedPages::new(4096);
        u.push(mk(b"k", 10, true, b"image10"));
        u.push(mk(b"k", 20, false, b"d20"));
        u.push(mk(b"k", 30, false, b"d30"));
        flush_first_n(&u, 3);
        // Historical image at lsn 20: present, 20 ≥ min(10) → installs.
        assert!(u.install_image_if_extends(Arc::new(mk(b"k", 20, true, b"hist20"))));
        // Read at the head: anchor on the image@20 + d30 above it.
        let at_head = u.lookup_chain_if_complete(&tkey(b"k"), Lsn(100)).unwrap();
        let lsns: Vec<u64> = at_head.iter().map(|r| r.lsn.0).collect();
        assert_eq!(lsns, vec![30, 20]);
        // Read exactly at the image: just the image (a complete suffix).
        let at_img = u.lookup_chain_if_complete(&tkey(b"k"), Lsn(20)).unwrap();
        assert_eq!(at_img.iter().map(|r| r.lsn.0).collect::<Vec<_>>(), vec![20]);
        // Read below the image (lsn 15): image@20 is excluded, image@10 was
        // trimmed by the install, so no anchor remains → falls to YB.
        assert!(u.lookup_chain_if_complete(&tkey(b"k"), Lsn(15)).is_none());
    }

    /// Concurrent reads on a hot key should not serialize. We can't
    /// timing-test that directly in a unit test, but we can verify
    /// `lookup_chain` is callable through an `Arc` from multiple
    /// threads without panicking — exercises the &self contract.
    #[test]
    fn concurrent_reads_share_self() {
        use std::sync::Arc;
        let u = Arc::new(UnifiedPages::new(4096));
        u.push_durable(mk(b"hot", 1, true, b"image"));
        u.push_durable(mk(b"hot", 2, false, b"d2"));
        let handles: Vec<_> = (0..8)
            .map(|_| {
                let u = u.clone();
                std::thread::spawn(move || {
                    for _ in 0..1000 {
                        let _ = u.lookup_chain(&tkey(b"hot"), Lsn(100));
                    }
                })
            })
            .collect();
        for h in handles {
            h.join().unwrap();
        }
    }

    /// Concurrency regression for the SkipMap rewrite: a writer adding
    /// an **unflushed** row must never lose it to a racing eviction. An
    /// unflushed row pins its key, so a correct cache evicts nothing
    /// here regardless of how the `push` / `evict_to_capacity` calls
    /// interleave. The `is_removed` retry in `insert_arc_entry` is what
    /// guarantees a `push` that raced an eviction onto a now-detached
    /// node retries onto a live one instead of stranding the row.
    ///
    /// The cache is sized far below the working set so eviction is
    /// constantly trying to run; every key is pinned, so a buggy
    /// removal-vs-insert race would surface as a missing row or a
    /// miscounted pin at the end.
    #[test]
    fn concurrent_push_unflushed_never_lost_under_eviction() {
        use std::sync::Arc;
        use std::sync::atomic::{AtomicBool, Ordering};

        const THREADS: u32 = 8;
        const PER_THREAD: u32 = 2000;

        // Tiny cap → eviction runs aggressively and keeps colliding with
        // the writers on the skiplist.
        let u = Arc::new(UnifiedPages::new(64));
        let stop = Arc::new(AtomicBool::new(false));

        // Background evictor: hammers eviction the whole time.
        let evictor = {
            let u = u.clone();
            let stop = stop.clone();
            std::thread::spawn(move || {
                while !stop.load(Ordering::Relaxed) {
                    u.evict_to_capacity();
                }
            })
        };

        let writers: Vec<_> = (0..THREADS)
            .map(|t| {
                let u = u.clone();
                std::thread::spawn(move || {
                    for i in 0..PER_THREAD {
                        // Disjoint key space per thread so each push is a
                        // fresh unflushed key (which pins itself).
                        let key = ((t as u64) << 32) | i as u64;
                        u.push(mk(&key.to_be_bytes(), key, false, b"x"));
                    }
                })
            })
            .collect();

        for w in writers {
            w.join().unwrap();
        }
        stop.store(true, Ordering::Relaxed);
        evictor.join().unwrap();

        // Every unflushed row pinned its key, so none could be evicted:
        // all of them must still be present and counted as pending.
        let total = (THREADS * PER_THREAD) as usize;
        assert_eq!(
            u.by_key_entries(),
            total,
            "an unflushed-pinned key was evicted (row lost)"
        );
        assert_eq!(
            u.unflushed_count(),
            total,
            "unflushed FIFO count diverged from the rows actually buffered"
        );
        for t in 0..THREADS {
            for i in 0..PER_THREAD {
                let key = ((t as u64) << 32) | i as u64;
                assert_eq!(
                    u.lookup_chain(&tkey(&key.to_be_bytes()), Lsn(key))
                        .len(),
                    1,
                    "missing unflushed row for key {key}"
                );
            }
        }
    }
}
