//! Single in-RAM structure for relation-page rows. Combines the
//! pre-flush write buffer with the post-flush read cache, evicting
//! at the **per-key** granularity so a cache hit either has the
//! complete chain for that key or no rows at all — never a partial
//! middle-of-chain hole.
//!
//! ## Per-key whole-eviction (Phase 4f)
//!
//! Eviction unit is the entire `KeyState` (one map entry +
//! everything in its `chain` BTreeMap). The `by_key` map keeps
//! insertion / promotion order — back = most recently written /
//! materialised, front = coldest — and `evict_to_capacity` walks
//! from the front dropping whole keys until `total_bytes` falls
//! back under `capacity_bytes`. There is no per-row FIFO and no
//! `evicted_below` watermark. Reads check anchor presence; if the
//! key is in `by_key`, its chain is complete from the latest
//! `will_init` row up to whatever was most recently inserted.
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

use bytes::Bytes;
use hashlink::LinkedHashMap;
use utils::lsn::Lsn;

use crate::page_store::PageRow;

/// A row plus its durability bit.
#[derive(Clone)]
struct PageEntry {
    row: PageRow,
    durable: bool,
}

/// Per-key bookkeeping. The chain is LSN-sorted (ascending). Unlike
/// the pre-4f layout, there is no `evicted_below`: a key is either
/// present (with its complete chain) or evicted entirely.
struct KeyState {
    chain: BTreeMap<Lsn, PageEntry>,
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
    /// LRU-ordered per-key state. Back = most recently inserted /
    /// promoted, front = coldest. Iteration is in this order.
    by_key: LinkedHashMap<Bytes, KeyState>,
    /// Insertion-ordered list of (key, lsn) pairs for unflushed
    /// entries. Drives `snapshot_unflushed` + `mark_first_n_flushed`
    /// so flushes hit YB in monotonic put-order.
    unflushed_fifo: VecDeque<(Bytes, Lsn)>,
    total_bytes: usize,
    unflushed_bytes: usize,
    capacity_bytes: usize,
}

impl UnifiedPages {
    pub fn new(capacity_bytes: usize) -> Self {
        Self {
            by_key: LinkedHashMap::new(),
            unflushed_fifo: VecDeque::new(),
            total_bytes: 0,
            unflushed_bytes: 0,
            capacity_bytes: capacity_bytes.max(1),
        }
    }

    pub fn unflushed_bytes(&self) -> usize {
        self.unflushed_bytes
    }

    pub fn total_bytes(&self) -> usize {
        self.total_bytes
    }

    pub fn unflushed_count(&self) -> usize {
        self.unflushed_fifo.len()
    }

    /// Number of distinct keys tracked. Drives the
    /// `pageserver_yb_unified_pages_by_key_entries` gauge.
    pub fn by_key_entries(&self) -> usize {
        self.by_key.len()
    }

    /// Append a row as unflushed. Same-`(key, lsn)` overwrites the
    /// existing entry's payload and adjusts the byte counters. The
    /// key is promoted to the back of the LRU.
    pub fn push(&mut self, row: PageRow) {
        let key = row.key.clone();
        let lsn = row.lsn;
        let prev = self.insert_entry(key.clone(), lsn, row, false);
        // Tracking: if we replaced a row that was unflushed, the old
        // entry is being superseded — no need to append a new fifo
        // entry, the existing one still applies. If the old row was
        // durable, the new row is unflushed and needs a fifo append.
        let need_fifo = match prev {
            Some(prev_was_unflushed) => !prev_was_unflushed,
            None => true,
        };
        if need_fifo {
            self.unflushed_fifo.push_back((key, lsn));
        }
    }

    /// Append a row that is already durable in YB. Used by the image
    /// materialiser after a successful `put_pages`. Promotes the key
    /// to the back of the LRU; eviction picks up cold keys first.
    pub fn push_durable(&mut self, row: PageRow) {
        let key = row.key.clone();
        let lsn = row.lsn;
        // Old entry's durability is irrelevant — the new row is
        // durable. If it was unflushed, switch counters; if it was
        // durable, just update bytes.
        let prev_was_unflushed = self.insert_entry(key, lsn, row, true);
        if matches!(prev_was_unflushed, Some(true)) {
            // The unflushed-fifo entry for this (key, lsn) is now
            // stale — pop it lazily during mark_first_n_flushed.
            // unflushed_bytes was already adjusted inside
            // insert_entry; nothing else to do here.
        }
    }

    /// Mark the first `n` unflushed entries durable. `n` must equal
    /// the snapshot count submitted to `try_write_rows`.
    pub fn mark_first_n_flushed(&mut self, n: usize) {
        let mut promoted = 0usize;
        while promoted < n {
            let Some((key, lsn)) = self.unflushed_fifo.pop_front() else {
                break;
            };
            // Look up the entry: it may have been trimmed,
            // overwritten with a durable row by the materialiser, or
            // already flipped (defensively).
            let Some(ks) = self.by_key.get_mut(&key) else {
                continue;
            };
            let Some(entry) = ks.chain.get_mut(&lsn) else {
                continue;
            };
            if entry.durable {
                // Already promoted (push_durable raced ahead). Don't
                // count this towards `n` — caller asked us to
                // promote n *unflushed* rows, this one wasn't one.
                continue;
            }
            entry.durable = true;
            let size = row_size(&entry.row);
            self.unflushed_bytes -= size;
            ks.unflushed_count = ks.unflushed_count.saturating_sub(1);
            promoted += 1;
        }
    }

    /// Drop whole keys from the front of the LRU until `total_bytes
    /// <= capacity_bytes`. Bounded by a 2 ms wall-clock budget so a
    /// big cap overshoot doesn't starve the writer lock. Pinned keys
    /// (`unflushed_count > 0`) are moved to the back so subsequent
    /// calls aren't blocked by them.
    pub fn evict_to_capacity(&mut self) {
        const TIME_BUDGET: std::time::Duration = std::time::Duration::from_millis(2);
        let start = std::time::Instant::now();

        // Pinned keys we've moved to the back during this call.
        // Once we've seen every key (front pointer is at one we've
        // already requeued), there's nothing left to evict and we
        // stop.
        let mut requeued_pins = 0usize;
        while self.total_bytes > self.capacity_bytes {
            let total_keys = self.by_key.len();
            if total_keys == 0 || requeued_pins >= total_keys {
                break;
            }
            let Some((front_key, front_state)) = self.by_key.front() else {
                break;
            };
            if front_state.unflushed_count > 0 {
                // Pinned — rotate to the back so we don't re-visit
                // it until we've cycled past every other key.
                let key = front_key.clone();
                self.by_key.to_back(&key);
                requeued_pins += 1;
                continue;
            }
            // Evictable. Pop the whole key.
            let (_, ks) = self.by_key.pop_front().expect("front existed");
            self.total_bytes -= ks.bytes;
            // `bytes` accounting was kept in sync; this can only
            // underflow on a real bug, in which case the panic is
            // exactly the right behaviour. We use `-=` directly so a
            // bug surfaces immediately.

            if start.elapsed() >= TIME_BUDGET {
                break;
            }
        }
    }

    /// Remove all durable entries for `key` with `lsn <= max_lsn`.
    /// Used by the materialiser to drop deltas the new image
    /// supersedes. Newer deltas and unflushed entries are untouched.
    pub fn trim_key_up_to(&mut self, key: &Bytes, max_lsn: Lsn) {
        let Some(ks) = self.by_key.get_mut(key) else {
            return;
        };
        let to_remove: Vec<Lsn> = ks
            .chain
            .range(..=max_lsn)
            .filter(|(_, e)| e.durable)
            .map(|(l, _)| *l)
            .collect();
        for lsn in to_remove {
            if let Some(entry) = ks.chain.remove(&lsn) {
                let size = row_size(&entry.row);
                ks.bytes -= size;
                self.total_bytes -= size;
            }
        }
        if ks.chain.is_empty() {
            self.by_key.remove(key);
        }
    }

    /// Return rows for `key` at `lsn <= target_lsn`, LSN-descending.
    /// Includes unflushed. Does NOT touch the LRU position — reads
    /// are `&self` and the lock is shared, so promotion on read
    /// would force serialisation. LRU order tracks write recency
    /// only, which is sufficient: hot keys keep getting written.
    pub fn lookup_chain(&self, key: &Bytes, target_lsn: Lsn) -> Vec<PageRow> {
        let Some(ks) = self.by_key.get(key) else {
            return Vec::new();
        };
        ks.chain
            .range(..=target_lsn)
            .rev()
            .map(|(_, e)| e.row.clone())
            .collect()
    }

    /// Like `lookup_chain` but only durable rows.
    pub fn lookup_chain_durable(&self, key: &Bytes, target_lsn: Lsn) -> Vec<PageRow> {
        let Some(ks) = self.by_key.get(key) else {
            return Vec::new();
        };
        ks.chain
            .range(..=target_lsn)
            .rev()
            .filter(|(_, e)| e.durable)
            .map(|(_, e)| e.row.clone())
            .collect()
    }

    /// Cache-only fast path. Returns `Some(chain)` (LSN-descending,
    /// including unflushed) iff `key` is present and the chain
    /// contains a `will_init` row at some `anchor_lsn ≤ target_lsn`.
    ///
    /// With per-key whole-eviction (Phase 4f) the cache cannot have
    /// a hole between the anchor and `target_lsn`: if the key is
    /// here, every row for it that we ever cached is here too.
    /// Anchor presence is therefore both necessary and sufficient.
    pub fn lookup_chain_if_complete(&self, key: &Bytes, target_lsn: Lsn) -> Option<Vec<PageRow>> {
        let ks = self.by_key.get(key)?;
        let mut out = Vec::new();
        let mut has_anchor = false;
        for (_, e) in ks.chain.range(..=target_lsn).rev() {
            out.push(e.row.clone());
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
    /// (post-flush `mark_first_n_flushed` promotes them all
    /// atomically), so this is a plain backward walk for an anchor.
    /// The `debug_assert!` guards the invariant so callers that
    /// regress to passing a pre-flush LSN crash in tests rather than
    /// silently feed walredo a partial chain.
    pub fn lookup_chain_durable_if_complete(
        &self,
        key: &Bytes,
        target_lsn: Lsn,
    ) -> Option<Vec<PageRow>> {
        let ks = self.by_key.get(key)?;
        let mut out = Vec::new();
        for (_, e) in ks.chain.range(..=target_lsn).rev() {
            debug_assert!(
                e.durable,
                "lookup_chain_durable_if_complete saw an unflushed row at \
                 lsn={} while walking ≤ target_lsn={} — caller passed a \
                 target_lsn above the flush horizon",
                e.row.lsn,
                target_lsn,
            );
            out.push(e.row.clone());
            if e.row.will_init {
                return Some(out);
            }
        }
        None
    }

    /// Range scan: rows for keys in `[start_key, end_key)` at `lsn ≤
    /// target_lsn`. Each per-key inner Vec is LSN-descending.
    pub fn lookup_range(
        &self,
        start_key: &Bytes,
        end_key: &Bytes,
        target_lsn: Lsn,
    ) -> HashMap<Bytes, Vec<PageRow>> {
        let mut out: HashMap<Bytes, Vec<PageRow>> = HashMap::new();
        for (k, ks) in self.by_key.iter() {
            if k.as_ref() < start_key.as_ref() || k.as_ref() >= end_key.as_ref() {
                continue;
            }
            let rows: Vec<PageRow> = ks
                .chain
                .range(..=target_lsn)
                .rev()
                .map(|(_, e)| e.row.clone())
                .collect();
            if !rows.is_empty() {
                out.insert(k.clone(), rows);
            }
        }
        out
    }

    /// Take a snapshot of up to `max` (or all) unflushed rows in
    /// insertion order. Caller (`flush`) uses this to drive the YB
    /// INSERT without holding the writer lock.
    pub fn snapshot_unflushed(&self, max: Option<usize>) -> Vec<PageRow> {
        let cap = max.unwrap_or(self.unflushed_fifo.len());
        let mut out = Vec::with_capacity(cap.min(self.unflushed_fifo.len()));
        for (key, lsn) in self.unflushed_fifo.iter().take(cap) {
            if let Some(ks) = self.by_key.get(key)
                && let Some(entry) = ks.chain.get(lsn)
                && !entry.durable
            {
                out.push(entry.row.clone());
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
    fn insert_entry(
        &mut self,
        key: Bytes,
        lsn: Lsn,
        row: PageRow,
        durable: bool,
    ) -> Option<bool> {
        let size = row_size(&row);
        let entry = PageEntry { row, durable };

        // Ensure the key exists in `by_key` and is at the back. If
        // it's already there, `to_back` returns the mut ref; else we
        // insert a fresh empty `KeyState` (which goes to the back).
        if !self.by_key.contains_key(&key) {
            self.by_key.insert(key.clone(), KeyState::new());
        } else {
            self.by_key.to_back(&key);
        }
        let ks = self.by_key.get_mut(&key).expect("just inserted/touched");

        let prev = ks.chain.insert(lsn, entry);
        let prev_was_unflushed = prev.as_ref().map(|p| !p.durable);

        if let Some(prev) = prev {
            let prev_size = row_size(&prev.row);
            ks.bytes -= prev_size;
            self.total_bytes -= prev_size;
            if !prev.durable {
                self.unflushed_bytes -= prev_size;
                ks.unflushed_count = ks.unflushed_count.saturating_sub(1);
            }
        }
        ks.bytes += size;
        self.total_bytes += size;
        if !durable {
            self.unflushed_bytes += size;
            ks.unflushed_count += 1;
        }
        prev_was_unflushed
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

    #[test]
    fn push_and_lookup_chain_descending() {
        let mut u = UnifiedPages::new(4096);
        u.push(mk(b"k", 10, false, b"a"));
        u.push(mk(b"k", 20, false, b"b"));
        u.push(mk(b"k", 5, true, b"c"));
        let chain = u.lookup_chain(&Bytes::from_static(b"k"), Lsn(100));
        let lsns: Vec<u64> = chain.iter().map(|r| r.lsn.0).collect();
        assert_eq!(lsns, vec![20, 10, 5]);
    }

    #[test]
    fn mark_flushed_promotes_without_moving_rows() {
        let mut u = UnifiedPages::new(4096);
        u.push(mk(b"k", 1, false, b"a"));
        u.push(mk(b"k", 2, false, b"b"));
        u.push(mk(b"k", 3, false, b"c"));
        let unflushed_before = u.unflushed_bytes;

        u.mark_first_n_flushed(2);
        assert!(u.unflushed_bytes > 0);
        assert!(u.unflushed_bytes < unflushed_before);

        let all = u.lookup_chain(&Bytes::from_static(b"k"), Lsn(100));
        assert_eq!(all.len(), 3, "all rows still visible after flush");
    }

    #[test]
    fn lookup_durable_filters_unflushed() {
        let mut u = UnifiedPages::new(4096);
        u.push(mk(b"k", 1, true, b"img"));
        u.push(mk(b"k", 2, false, b"d2"));
        u.mark_first_n_flushed(1);
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
        let mut u = UnifiedPages::new(64);
        for i in 0u32..1000 {
            u.push_durable(mk(&i.to_be_bytes(), i as u64, true, &[0u8; 32]));
        }
        let before = u.total_bytes;
        u.evict_to_capacity();
        assert!(
            u.total_bytes <= u.capacity_bytes || u.total_bytes < before / 2,
            "evict reclaimed too little: before={}, after={}, cap={}",
            before,
            u.total_bytes,
            u.capacity_bytes,
        );
    }

    #[test]
    fn push_durable_is_immediately_evictable() {
        let mut u = UnifiedPages::new(20);
        u.push_durable(mk(b"k1", 1, true, b"AAAAAAAAAAAAAAAA"));
        u.push_durable(mk(b"k2", 2, true, b"BBBBBBBBBBBBBBBB"));
        u.evict_to_capacity();
        // k1 was inserted first → at the front → evicted first.
        assert!(u.lookup_chain(&Bytes::from_static(b"k1"), Lsn(100)).is_empty());
    }

    #[test]
    fn evict_skips_pinned_keys() {
        // Every key has an unflushed row → all pinned → no-op.
        let mut u = UnifiedPages::new(10);
        u.push(mk(b"k1", 1, true, b"xxxxxxxx"));
        u.push(mk(b"k2", 2, true, b"xxxxxxxx"));
        u.evict_to_capacity();
        assert!(u.total_bytes > u.capacity_bytes);
        assert_eq!(u.by_key_entries(), 2);
    }

    #[test]
    fn evict_drops_only_unpinned_keys() {
        // k1 is durable (unpinned), k2 has an unflushed row (pinned).
        // Eviction must drop k1 and leave k2 alone, even with k1 at
        // the front initially.
        let mut u = UnifiedPages::new(20);
        u.push_durable(mk(b"k1", 1, true, b"AAAAAAAAAAAAAAAA"));
        u.push(mk(b"k2", 2, true, b"BBBBBBBBBBBBBBBB"));
        u.evict_to_capacity();
        assert!(u.lookup_chain(&Bytes::from_static(b"k1"), Lsn(100)).is_empty());
        assert!(!u.lookup_chain(&Bytes::from_static(b"k2"), Lsn(100)).is_empty());
    }

    #[test]
    fn trim_drops_only_durable_le_max_lsn() {
        let mut u = UnifiedPages::new(4096);
        u.push(mk(b"k", 10, false, b"d10"));
        u.push(mk(b"k", 20, false, b"d20"));
        u.push(mk(b"k", 30, false, b"d30"));
        u.mark_first_n_flushed(2);
        u.trim_key_up_to(&Bytes::from_static(b"k"), Lsn(25));
        let chain = u.lookup_chain(&Bytes::from_static(b"k"), Lsn(100));
        assert_eq!(chain.len(), 1);
        assert_eq!(chain[0].lsn, Lsn(30));
    }

    #[test]
    fn trim_leaves_unflushed_in_range() {
        let mut u = UnifiedPages::new(4096);
        u.push(mk(b"k", 10, false, b"d10"));
        u.mark_first_n_flushed(1);
        u.push(mk(b"k", 20, false, b"d20"));
        u.trim_key_up_to(&Bytes::from_static(b"k"), Lsn(50));
        let chain = u.lookup_chain(&Bytes::from_static(b"k"), Lsn(100));
        assert_eq!(chain.len(), 1);
        assert_eq!(chain[0].lsn, Lsn(20));
    }

    #[test]
    fn materializer_install_sequence_works_end_to_end() {
        let mut u = UnifiedPages::new(4096);
        for i in 1..=5 {
            u.push(mk(b"k", i * 10, false, &[i as u8; 4]));
        }
        u.mark_first_n_flushed(5);
        u.trim_key_up_to(&Bytes::from_static(b"k"), Lsn(50));
        u.push_durable(mk(b"k", 50, true, b"full-page-image"));
        let chain = u.lookup_chain_durable(&Bytes::from_static(b"k"), Lsn(100));
        assert_eq!(chain.len(), 1, "only the new image remains");
        assert!(chain[0].is_image);
        assert_eq!(chain[0].lsn, Lsn(50));
    }

    #[test]
    fn snapshot_unflushed_returns_pre_flush_only() {
        let mut u = UnifiedPages::new(4096);
        u.push(mk(b"k", 1, false, b"a"));
        u.push(mk(b"k", 2, false, b"b"));
        u.mark_first_n_flushed(1);
        u.push(mk(b"k", 3, false, b"c"));
        let snap = u.snapshot_unflushed(None);
        assert_eq!(snap.len(), 2);
        assert_eq!(snap[0].lsn, Lsn(2));
        assert_eq!(snap[1].lsn, Lsn(3));
    }

    #[test]
    fn complete_chain_with_image_anchor_in_cache() {
        let mut u = UnifiedPages::new(4096);
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
        let mut u = UnifiedPages::new(4096);
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
        let mut u = UnifiedPages::new(40);
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

    #[test]
    fn overwrite_same_key_lsn_adjusts_bytes() {
        let mut u = UnifiedPages::new(4096);
        u.push(mk(b"k", 10, false, b"small"));
        let b1 = u.total_bytes;
        u.push(mk(b"k", 10, true, b"larger-replacement"));
        assert!(u.total_bytes > b1);
        let chain = u.lookup_chain(&Bytes::from_static(b"k"), Lsn(100));
        assert_eq!(chain.len(), 1);
        assert!(chain[0].is_image);
    }

    #[test]
    fn lru_order_is_insertion_order_with_promotion_on_write() {
        // k1 then k2 then update k1 → k2 becomes the LRU front, but
        // the cap is sized so that only ONE key needs to be evicted
        // to fit. With per-key sizes around 60 B each, cap=120 fits
        // exactly two key states' worth — after the third push the
        // overshoot is small, so evict drops just the front key.
        // Per-key sizes: ~34 B each (16 payload + 2 key + 16 overhead).
        // After 3 pushes, total ≈ 102 B (k1 = 68, k2 = 34). Cap = 80
        // forces evicting one key — the LRU front.
        let mut u = UnifiedPages::new(80);
        u.push_durable(mk(b"k1", 1, true, b"AAAAAAAAAAAAAAAA"));
        u.push_durable(mk(b"k2", 2, true, b"BBBBBBBBBBBBBBBB"));
        // Write to k1 again — promotes it to the back of the LRU.
        u.push_durable(mk(b"k1", 3, false, b"CCCCCCCCCCCCCCCC"));
        u.evict_to_capacity();
        // k2 was at the front after the k1 promotion → evicted.
        assert!(u.lookup_chain(&Bytes::from_static(b"k2"), Lsn(100)).is_empty());
        assert!(!u.lookup_chain(&Bytes::from_static(b"k1"), Lsn(100)).is_empty());
    }

    /// Range-scan: confirm the half-open `[start, end)` filter and that
    /// each per-key Vec comes back LSN-descending.
    #[test]
    fn lookup_range_filters_and_orders_descending() {
        let mut u = UnifiedPages::new(4096);
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
        let mut u = UnifiedPages::new(8);
        for i in 0u32..5 {
            u.push(mk(&i.to_be_bytes(), i as u64, true, &[0u8; 32]));
        }
        let before = u.total_bytes;
        u.evict_to_capacity();
        assert_eq!(
            u.total_bytes, before,
            "all pinned ⇒ no eviction; total_bytes unchanged",
        );
        assert_eq!(u.by_key_entries(), 5, "all keys still resident");
    }

    /// A `push_durable` overwrite of an unflushed entry must keep the
    /// counters consistent: the unflushed slot disappears, the durable
    /// row replaces it, and a subsequent `mark_first_n_flushed` doesn't
    /// double-decrement (the stale fifo ref is recognised and skipped).
    #[test]
    fn push_durable_overwrites_unflushed_without_breaking_mark_flushed() {
        let mut u = UnifiedPages::new(4096);
        u.push(mk(b"k", 10, false, b"unflushed"));
        let unflushed_before = u.unflushed_bytes;
        // Materialiser races and installs a durable row at the same lsn.
        u.push_durable(mk(b"k", 10, true, b"image-replacement"));
        // The unflushed counter must have dropped to zero — the old
        // (key,10) entry is no longer unflushed.
        assert_eq!(
            u.unflushed_bytes, 0,
            "overwrite by push_durable promotes the entry; \
             unflushed_bytes should drain even though unflushed_fifo \
             still holds the stale ref",
        );
        // mark_first_n_flushed must NOT count the stale ref as a flush
        // (it's already durable; it shouldn't decrement unflushed below 0).
        u.mark_first_n_flushed(1);
        assert_eq!(u.unflushed_bytes, 0);
        // Sanity: the durable row is still there.
        let chain = u.lookup_chain(&Bytes::from_static(b"k"), Lsn(100));
        assert_eq!(chain.len(), 1);
        assert!(chain[0].is_image);
        // And it shouldn't have looked anything like the original.
        assert_ne!(unflushed_before, 0);
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
        let mut u = UnifiedPages::new(4096);
        u.push(mk(b"k", 10, true, b"anchor"));
        u.mark_first_n_flushed(1);
        u.push(mk(b"k", 20, false, b"d20"));
        u.mark_first_n_flushed(1);
        u.push(mk(b"k", 30, false, b"d30"));         // UNFLUSHED
        u.push_durable(mk(b"k", 40, false, b"d40")); // already durable
        // target_lsn=100 covers an unflushed row (d30); the assertion
        // fires.
        let _ = u.lookup_chain_durable_if_complete(&Bytes::from_static(b"k"), Lsn(100));
    }

    /// Materialiser install at LSN N MUST drop every durable row with
    /// LSN ≤ N for that key so we can't fall into the "old image plus
    /// new delta" trap described in task 11 / the AWS PANIC. The
    /// install path goes via `trim_key_up_to` + `push_durable`; this
    /// test exercises the combination from the materialiser's
    /// perspective.
    #[test]
    fn install_drops_all_older_durable_rows() {
        let mut u = UnifiedPages::new(4096);
        u.push(mk(b"k", 10, true, b"old-image"));
        u.push(mk(b"k", 20, false, b"d20"));
        u.push(mk(b"k", 30, false, b"d30"));
        u.mark_first_n_flushed(3);
        // Install fresh image at lsn 25.
        u.trim_key_up_to(&Bytes::from_static(b"k"), Lsn(25));
        u.push_durable(mk(b"k", 25, true, b"materialised"));
        let chain = u.lookup_chain_durable(&Bytes::from_static(b"k"), Lsn(100));
        let lsns: Vec<u64> = chain.iter().map(|r| r.lsn.0).collect();
        // The deltas/image ≤ 25 are gone; d30 stays because it's above
        // the new image.
        assert_eq!(lsns, vec![30, 25]);
    }
}
