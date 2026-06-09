//! Single in-RAM structure for relation-page rows. Combines the
//! pre-flush write buffer with the post-flush read cache, evicting
//! at the **per-key** granularity so a cache hit either has the
//! complete chain for that key or no rows at all — never a partial
//! middle-of-chain hole.
//!
//! ## Concurrency
//!
//! `UnifiedPages` is internally thread-safe — every method takes
//! `&self`. Per-key state lives in a `DashMap`, which shards locks so
//! reads and writes on different keys never contend. Aggregate
//! counters (`total_bytes`, `unflushed_bytes`) are atomics. The
//! eviction LRU and the flush FIFO each live behind their own
//! `parking_lot::Mutex` — both are touched in O(1) on every push, but
//! the holds are nanosecond-scale and only contested by put + flush +
//! materialiser-install (single-digit task count), not by the read
//! path. The 300-backend getpage fan-in goes straight to DashMap
//! shards and never touches LRU / FIFO.
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

use bytes::Bytes;
use dashmap::DashMap;
use hashlink::LinkedHashMap;
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

/// Per-key bookkeeping stored as the DashMap value. DashMap gives us
/// per-shard locking for free, so this struct itself does not need a
/// lock — accessing it goes through a DashMap guard which already
/// serialises mutations to the same key.
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
    /// Per-key state, sharded for concurrent access. Reads
    /// (`lookup_chain*`) on different keys proceed in parallel;
    /// writes (`push*`, `trim`, `mark_flushed`) only contend
    /// when they hit the same shard.
    by_key: DashMap<Bytes, KeyState>,
    /// LRU sidecar: just the key set, in promotion order. Back =
    /// most recently inserted / promoted, front = coldest. Only
    /// `push*` / `evict_to_capacity` / `trim_key_up_to` touch this;
    /// reads do not. Holds are O(1).
    lru: Mutex<LinkedHashMap<Bytes, ()>>,
    /// Insertion-ordered list of (key, lsn) pairs for unflushed
    /// entries. Drives `snapshot_unflushed` + `mark_flushed`
    /// so flushes hit YB in monotonic put-order.
    unflushed_fifo: Mutex<VecDeque<(Bytes, Lsn)>>,
    total_bytes: AtomicUsize,
    unflushed_bytes: AtomicUsize,
    capacity_bytes: usize,
}

impl UnifiedPages {
    pub fn new(capacity_bytes: usize) -> Self {
        Self {
            by_key: DashMap::new(),
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

    /// Append a row as unflushed. Walreceiver's LSNs are strictly
    /// monotonic within a PS lifetime so this never overlaps an
    /// existing slot; on restart the cache is empty. Both invariants
    /// are checked by the debug-assert in `insert_entry`.
    pub fn push(&self, row: PageRow) {
        let key = row.key.clone();
        let lsn = row.lsn;
        self.insert_entry(key.clone(), lsn, row, false);
        self.unflushed_fifo.lock().push_back((key, lsn));
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
        let key = row.key.clone();
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
        let mut promoted_by_key: HashMap<Bytes, Vec<Lsn>> = HashMap::new();

        for entry in snapshot {
            if !entry.try_promote() {
                continue; // insert_entry already claimed this entry.
            }
            pageserver_api::write_trace::trace_stage_bytes(
                "durable",
                &entry.row.key,
                entry.row.lsn,
            );
            let size = row_size(&entry.row);
            self.unflushed_bytes.fetch_sub(size, Ordering::Relaxed);
            if let Some(mut ks_guard) = self.by_key.get_mut(&entry.row.key) {
                let ks = ks_guard.value_mut();
                ks.unflushed_count = ks.unflushed_count.saturating_sub(1);
            }
            promoted_by_key
                .entry(entry.row.key.clone())
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
                key.clone()
            };

            // Atomically check pinned + remove if evictable. Holding
            // a DashMap entry guard pins the shard for one key only,
            // so other shards stay free.
            let removed = match self.by_key.entry(candidate.clone()) {
                dashmap::mapref::entry::Entry::Occupied(o) => {
                    if o.get().unflushed_count > 0 {
                        // Pinned — leave in the map; we'll rotate
                        // the LRU below.
                        drop(o);
                        None
                    } else {
                        let bytes = o.get().bytes;
                        o.remove();
                        Some(bytes)
                    }
                }
                dashmap::mapref::entry::Entry::Vacant(_) => Some(0),
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
    pub fn trim_key_up_to(&self, key: &Bytes, max_lsn: Lsn) {
        let mut total_freed = 0usize;
        let key_empty = {
            let Some(mut entry) = self.by_key.get_mut(key) else {
                return;
            };
            let ks = entry.value_mut();
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
            ks.chain.is_empty()
        };
        if total_freed > 0 {
            self.total_bytes.fetch_sub(total_freed, Ordering::Relaxed);
        }
        if key_empty {
            self.by_key.remove(key);
            self.lru.lock().remove(key);
        }
    }

    /// Return rows for `key` at `lsn <= target_lsn`, LSN-descending.
    /// Includes unflushed. Does NOT touch the LRU position — LRU
    /// order tracks write recency only, which is sufficient: hot
    /// keys keep getting written.
    pub fn lookup_chain(&self, key: &Bytes, target_lsn: Lsn) -> Vec<Arc<PageRow>> {
        let Some(ks) = self.by_key.get(key) else {
            return Vec::new();
        };
        ks.chain
            .range(..=target_lsn)
            .rev()
            .map(|(_, e)| e.row_arc())
            .collect()
    }

    /// Like `lookup_chain` but only durable rows.
    pub fn lookup_chain_durable(&self, key: &Bytes, target_lsn: Lsn) -> Vec<Arc<PageRow>> {
        let Some(ks) = self.by_key.get(key) else {
            return Vec::new();
        };
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
    pub fn lookup_chain_if_complete(
        &self,
        key: &Bytes,
        target_lsn: Lsn,
    ) -> Option<Vec<Arc<PageRow>>> {
        let ks = self.by_key.get(key)?;
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
    /// (post-flush `mark_flushed` promotes them all
    /// atomically), so this is a plain backward walk for an anchor.
    /// The `debug_assert!` guards the invariant so callers that
    /// regress to passing a pre-flush LSN crash in tests rather than
    /// silently feed walredo a partial chain.
    pub fn lookup_chain_durable_if_complete(
        &self,
        key: &Bytes,
        target_lsn: Lsn,
    ) -> Option<Vec<Arc<PageRow>>> {
        let ks = self.by_key.get(key)?;
        let mut out = Vec::new();
        for (_, e) in ks.chain.range(..=target_lsn).rev() {
            debug_assert!(
                e.is_durable(),
                "lookup_chain_durable_if_complete saw an unflushed row at \
                 lsn={} while walking ≤ target_lsn={} — caller passed a \
                 target_lsn above the flush horizon",
                e.row.lsn, target_lsn,
            );
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
    /// Iterates the full DashMap, briefly read-locking each shard.
    /// Not on the getpage hot path — scan_range is called for
    /// vectored reads, which are rare.
    pub fn lookup_range(
        &self,
        start_key: &Bytes,
        end_key: &Bytes,
        target_lsn: Lsn,
    ) -> HashMap<Bytes, Vec<Arc<PageRow>>> {
        let mut out: HashMap<Bytes, Vec<Arc<PageRow>>> = HashMap::new();
        for entry in self.by_key.iter() {
            let k = entry.key();
            if k.as_ref() < start_key.as_ref() || k.as_ref() >= end_key.as_ref() {
                continue;
            }
            let rows: Vec<Arc<PageRow>> = entry
                .value()
                .chain
                .range(..=target_lsn)
                .rev()
                .map(|(_, e)| e.row_arc())
                .collect()
                ;
            if !rows.is_empty() {
                out.insert(k.clone(), rows);
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
        let pairs: Vec<(Bytes, Lsn)> = {
            let fifo = self.unflushed_fifo.lock();
            let cap = max.unwrap_or(fifo.len()).min(fifo.len());
            fifo.iter().take(cap).cloned().collect()
        };
        let mut out = Vec::with_capacity(pairs.len());
        for (key, lsn) in &pairs {
            if let Some(ks) = self.by_key.get(key)
                && let Some(entry) = ks.chain.get(lsn)
                && !entry.is_durable()
            {
                out.push(Arc::clone(entry));
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
    /// Lock order: LRU lock → DashMap shard lock. `evict_to_capacity`
    /// follows the same order, so no deadlock.
    fn insert_entry(&self, key: Bytes, lsn: Lsn, row: PageRow, durable: bool) {
        self.insert_arc_entry(key, lsn, Arc::new(row), durable);
    }

    fn insert_arc_entry(&self, key: Bytes, lsn: Lsn, row: Arc<PageRow>, durable: bool) {
        let size = row_size(&row);
        let entry = PageEntry::from_arc_row(row, durable);

        // Step 1: ensure the key is at the back of the LRU.
        {
            let mut lru = self.lru.lock();
            if lru.contains_key(&key) {
                lru.to_back(&key);
            } else {
                lru.insert(key.clone(), ());
            }
        }

        // Step 2: insert into the DashMap shard.
        let mut shard = self.by_key.entry(key).or_insert_with(KeyState::new);
        let ks = shard.value_mut();

        let prev = ks.chain.insert(lsn, entry);

        // Production invariant: if we overwrote an existing slot, that
        // slot was already durable.
        //   - `push()` is walreceiver-only and walreceiver's LSNs are
        //     strictly monotonic within a PS lifetime, so it never
        //     overlaps a same-LSN slot. (On restart the cache is empty.)
        //   - `push_durable()` is materialiser-only and the materialiser
        //     bounds its target_lsn ≤ flush_horizon_lsn (only advanced
        //     after `mark_flushed`), so any same-LSN slot is durable by
        //     the time the image arrives.
        // So only the byte counters move on overwrite — the unflushed
        // counters can't be touched.
        if let Some(prev) = prev {
            debug_assert!(
                prev.is_durable(),
                "insert_entry overwriting an unflushed slot at lsn={lsn}: \
                 violates the monotonic-LSN (push) / target_lsn≤horizon (push_durable) \
                 contract — the row in the unflushed slot would be lost if it isn't \
                 also in YB",
            );
            let prev_size = row_size(&prev.row);
            ks.bytes -= prev_size;
            self.total_bytes.fetch_sub(prev_size, Ordering::Relaxed);
        }
        ks.bytes += size;
        self.total_bytes.fetch_add(size, Ordering::Relaxed);
        if !durable {
            self.unflushed_bytes.fetch_add(size, Ordering::Relaxed);
            ks.unflushed_count += 1;
        }
    }
}

fn row_size(r: &PageRow) -> usize {
    r.payload.len() + r.key.len() + 16
}

#[cfg(test)]
mod tests {
    use super::*;

    fn mk(key: &[u8], lsn: u64, is_image: bool, payload: &[u8]) -> PageRow {
        PageRow {
            key: Bytes::copy_from_slice(key),
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
        let chain = u.lookup_chain(&Bytes::from_static(b"k"), Lsn(100));
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

        let all = u.lookup_chain(&Bytes::from_static(b"k"), Lsn(100));
        assert_eq!(all.len(), 3, "all rows still visible after flush");
    }

    #[test]
    fn lookup_durable_filters_unflushed() {
        let u = UnifiedPages::new(4096);
        u.push(mk(b"k", 1, true, b"img"));
        u.push(mk(b"k", 2, false, b"d2"));
        flush_first_n(&u, 1);
        u.push(mk(b"k", 3, false, b"d3"));

        let all = u.lookup_chain(&Bytes::from_static(b"k"), Lsn(100));
        let durable = u.lookup_chain_durable(&Bytes::from_static(b"k"), Lsn(100));
        assert_eq!(all.len(), 3);
        assert_eq!(durable.len(), 1);
        assert_eq!(durable[0].lsn, Lsn(1));
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
            u.lookup_chain(&Bytes::from_static(b"k1"), Lsn(100))
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
            u.lookup_chain(&Bytes::from_static(b"k1"), Lsn(100))
                .is_empty()
        );
        assert!(
            !u.lookup_chain(&Bytes::from_static(b"k2"), Lsn(100))
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
        u.trim_key_up_to(&Bytes::from_static(b"k"), Lsn(25));
        let chain = u.lookup_chain(&Bytes::from_static(b"k"), Lsn(100));
        assert_eq!(chain.len(), 1);
        assert_eq!(chain[0].lsn, Lsn(30));
    }

    #[test]
    fn trim_leaves_unflushed_in_range() {
        let u = UnifiedPages::new(4096);
        u.push(mk(b"k", 10, false, b"d10"));
        flush_first_n(&u, 1);
        u.push(mk(b"k", 20, false, b"d20"));
        u.trim_key_up_to(&Bytes::from_static(b"k"), Lsn(50));
        let chain = u.lookup_chain(&Bytes::from_static(b"k"), Lsn(100));
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
        u.trim_key_up_to(&Bytes::from_static(b"k"), Lsn(50));
        u.push_durable(mk(b"k", 50, true, b"full-page-image"));
        let chain = u.lookup_chain_durable(&Bytes::from_static(b"k"), Lsn(100));
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
            .lookup_chain_if_complete(&Bytes::from_static(b"k"), Lsn(100))
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
            u.lookup_chain_if_complete(&Bytes::from_static(b"k"), Lsn(100))
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

        let r = u.lookup_chain_durable_if_complete(&Bytes::from_static(b"hot"), Lsn(200));
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
                    u.lookup_chain(&Bytes::from_static(b"hot"), Lsn(200))
                        .is_empty(),
                    "key absent ⇒ no rows at all (no partial cache state)"
                );
            }
        }
    }

    /// The materialiser overwrite path: a durable delta at lsn=L gets
    /// replaced by a durable image at lsn=L. The byte counters must
    /// reflect the new payload size; the chain must show the image.
    /// (The pre-overwrite slot is durable — overwriting an unflushed
    /// slot is forbidden by `insert_entry`'s debug-assert.)
    #[test]
    fn overwrite_same_key_lsn_adjusts_bytes() {
        let u = UnifiedPages::new(4096);
        u.push(mk(b"k", 10, false, b"small"));
        flush_first_n(&u, 1); // promote to durable first
        let b1 = u.total_bytes();
        u.push_durable(mk(b"k", 10, true, b"larger-replacement"));
        assert!(u.total_bytes() > b1);
        let chain = u.lookup_chain(&Bytes::from_static(b"k"), Lsn(100));
        assert_eq!(chain.len(), 1);
        assert!(chain[0].is_image);
    }

    #[test]
    fn lru_order_is_insertion_order_with_promotion_on_write() {
        // k1 then k2 then update k1 → k2 becomes the LRU front, but
        // the cap is sized so that only ONE key needs to be evicted
        // to fit. Per-key sizes: ~34 B each (16 payload + 2 key + 16
        // overhead). After 3 pushes, total ≈ 102 B (k1 = 68, k2 = 34).
        // Cap = 80 forces evicting one key — the LRU front.
        let u = UnifiedPages::new(80);
        u.push_durable(mk(b"k1", 1, true, b"AAAAAAAAAAAAAAAA"));
        u.push_durable(mk(b"k2", 2, true, b"BBBBBBBBBBBBBBBB"));
        // Write to k1 again — promotes it to the back of the LRU.
        u.push_durable(mk(b"k1", 3, false, b"CCCCCCCCCCCCCCCC"));
        u.evict_to_capacity();
        // k2 was at the front after the k1 promotion → evicted.
        assert!(
            u.lookup_chain(&Bytes::from_static(b"k2"), Lsn(100))
                .is_empty()
        );
        assert!(
            !u.lookup_chain(&Bytes::from_static(b"k1"), Lsn(100))
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
            &Bytes::from_static(b"b"),
            &Bytes::from_static(b"c"),
            Lsn(100),
        );
        assert_eq!(out.len(), 1, "only 'b' falls in [b, c)");
        let b_chain = &out[&Bytes::from_static(b"b")];
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

    /// `push_durable` overwriting an unflushed slot violates the
    /// target_lsn ≤ horizon contract — the materialiser must never
    /// run for an LSN whose cache entry isn't already durable. The
    /// debug-assert in `insert_entry` catches the contract break.
    #[test]
    #[should_panic(expected = "insert_entry overwriting an unflushed slot")]
    fn push_durable_over_unflushed_panics_in_debug() {
        let u = UnifiedPages::new(4096);
        u.push(mk(b"k", 10, false, b"unflushed"));
        u.push_durable(mk(b"k", 10, true, b"image-replacement"));
    }

    /// `push` overwriting any existing slot violates walreceiver's
    /// monotonic-LSN invariant — the same WAL record can't land twice
    /// at the same LSN. The debug-assert catches it.
    #[test]
    #[should_panic(expected = "insert_entry overwriting an unflushed slot")]
    fn push_over_unflushed_panics_in_debug() {
        let u = UnifiedPages::new(4096);
        u.push(mk(b"k", 10, false, b"first"));
        u.push(mk(b"k", 10, false, b"second"));
    }

    /// Regression for the AWS BTREE walredo PANIC on 2026-06-02:
    /// After the materialiser-trigger-on-flush refactor, the caller
    /// invariant on `lookup_chain_durable_if_complete` is
    /// `target_lsn ≤ flush_horizon_lsn` — every row in the walk
    /// range is durable. A `debug_assert!` guards the invariant.
    /// In debug builds, passing an unflushed-in-range target_lsn
    /// panics; this test exercises that.
    #[test]
    #[should_panic(expected = "saw an unflushed row")]
    fn durable_if_complete_debug_asserts_on_unflushed_in_range() {
        let u = UnifiedPages::new(4096);
        u.push(mk(b"k", 10, true, b"anchor"));
        flush_first_n(&u, 1);
        u.push(mk(b"k", 20, false, b"d20"));
        flush_first_n(&u, 1);
        u.push(mk(b"k", 30, false, b"d30")); // UNFLUSHED
        u.push_durable(mk(b"k", 40, false, b"d40")); // already durable
        // target_lsn=100 covers an unflushed row (d30); the assertion
        // fires.
        let _ = u.lookup_chain_durable_if_complete(&Bytes::from_static(b"k"), Lsn(100));
    }

    /// Materialiser install at LSN N MUST drop every durable row with
    /// LSN ≤ N for that key so we can't fall into the "old image plus
    /// new delta" trap. The install path goes via `trim_key_up_to` +
    /// `push_durable`; this test exercises the combination from the
    /// materialiser's perspective.
    #[test]
    fn install_drops_all_older_durable_rows() {
        let u = UnifiedPages::new(4096);
        u.push(mk(b"k", 10, true, b"old-image"));
        u.push(mk(b"k", 20, false, b"d20"));
        u.push(mk(b"k", 30, false, b"d30"));
        flush_first_n(&u, 3);
        // Install fresh image at lsn 25.
        u.trim_key_up_to(&Bytes::from_static(b"k"), Lsn(25));
        u.push_durable(mk(b"k", 25, true, b"materialised"));
        let chain = u.lookup_chain_durable(&Bytes::from_static(b"k"), Lsn(100));
        let lsns: Vec<u64> = chain.iter().map(|r| r.lsn.0).collect();
        // The deltas/image ≤ 25 are gone; d30 stays because it's above
        // the new image.
        assert_eq!(lsns, vec![30, 25]);
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
                        let _ = u.lookup_chain(&Bytes::from_static(b"hot"), Lsn(100));
                    }
                })
            })
            .collect();
        for h in handles {
            h.join().unwrap();
        }
    }
}
