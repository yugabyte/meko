//! Single in-RAM structure for relation-page rows, replacing the
//! separate "flush buffer" + "post-flush cache" pair.
//!
//! Externally one API; internally two FIFOs share one key index:
//! - `unflushed_fifo` — pre-flush entries pinned in RAM (the row is
//!   the only copy that exists; losing it loses accepted WAL).
//! - `durable_fifo` — entries already INSERTed to YB. Eligible for
//!   eviction (when `total_bytes > capacity_bytes`) or trim (when a
//!   materialised image supersedes them).
//! - `by_key: HashMap<Bytes, BTreeMap<Lsn, PageEntry>>` indexes both,
//!   so per-key chain lookups are one HashMap probe + one BTree range.
//!
//! Flush is a cursor flip — `mark_first_n_flushed(n)` pops `n` from
//! the front of `unflushed_fifo`, flips each entry's `durable` flag,
//! pushes its ref onto the back of `durable_fifo`. No row data is
//! copied; `unflushed_bytes` decrements; `total_bytes` unchanged.
//!
//! ## Lookup
//! - [`UnifiedPages::lookup_chain`] returns rows for `key` at `lsn ≤
//!   target_lsn`, including unflushed. Used by the regular `read_chain`
//!   blender — the caller (compute) is in the same process and
//!   observes its own pre-flush writes.
//! - [`UnifiedPages::lookup_chain_durable`] returns only durable rows.
//!   Used by the image materialiser, which must never build an image
//!   from pre-flush inputs (crash + WAL replay would reapply those
//!   deltas on top of the image → double-apply).
//!
//! ## Trim
//! [`UnifiedPages::trim_key_up_to`] removes durable rows for `key`
//! with `lsn ≤ max_lsn`. Called by the materialiser after a successful
//! image INSERT to drop the deltas the image superseded. Stale FIFO
//! refs are tolerated (the evictor skips them).

use std::collections::{BTreeMap, HashMap, VecDeque};

use bytes::Bytes;
use utils::lsn::Lsn;

use crate::page_store::PageRow;

/// A row plus its durability bit.
#[derive(Clone)]
struct PageEntry {
    row: PageRow,
    durable: bool,
}

/// A reference into a FIFO eviction queue. Carries the size so the
/// evictor doesn't need to re-look-up `by_key` to know how many bytes
/// it's freeing.
#[derive(Clone)]
struct EntryRef {
    key: Bytes,
    lsn: Lsn,
    size: usize,
}

pub struct UnifiedPages {
    by_key: HashMap<Bytes, BTreeMap<Lsn, PageEntry>>,
    unflushed_fifo: VecDeque<EntryRef>,
    durable_fifo: VecDeque<EntryRef>,
    total_bytes: usize,
    unflushed_bytes: usize,
    capacity_bytes: usize,
}

impl UnifiedPages {
    pub fn new(capacity_bytes: usize) -> Self {
        Self {
            by_key: HashMap::new(),
            unflushed_fifo: VecDeque::new(),
            durable_fifo: VecDeque::new(),
            total_bytes: 0,
            unflushed_bytes: 0,
            capacity_bytes: capacity_bytes.max(1), // guard against 0
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

    /// Append a row as unflushed. Same-`(key, lsn)` overwrites the
    /// existing entry's payload and adjusts the byte counters.
    pub fn push(&mut self, row: PageRow) {
        let size = row_size(&row);
        let key = row.key.clone();
        let lsn = row.lsn;
        self.insert_entry(key.clone(), lsn, row, false);
        self.unflushed_fifo.push_back(EntryRef { key, lsn, size });
        self.unflushed_bytes += size;
    }

    /// Append a row that is already durable in YB. Used by the image
    /// materialiser after a successful `put_pages` of the fresh image.
    /// O(1) — appended at the back of `durable_fifo` so it's evictable
    /// in LRU-ish order (most recently materialised images last).
    pub fn push_durable(&mut self, row: PageRow) {
        let size = row_size(&row);
        let key = row.key.clone();
        let lsn = row.lsn;
        self.insert_entry(key.clone(), lsn, row, true);
        self.durable_fifo.push_back(EntryRef { key, lsn, size });
    }

    /// Mark the first `n` unflushed entries durable. `n` must equal
    /// the snapshot count submitted to `try_write_rows`.
    pub fn mark_first_n_flushed(&mut self, n: usize) {
        for _ in 0..n {
            let Some(er) = self.unflushed_fifo.pop_front() else {
                break;
            };
            // Look up the live entry — it may have been replaced by a
            // newer overwrite at the same (key, lsn) since the
            // snapshot. The overwriting `push` would have left this
            // er in unflushed_fifo (the replacement inserted ITS own
            // er to the back), so we still flip it correctly here.
            if let Some(chain) = self.by_key.get_mut(&er.key)
                && let Some(entry) = chain.get_mut(&er.lsn)
                && !entry.durable
            {
                entry.durable = true;
                self.unflushed_bytes -= er.size;
                self.durable_fifo.push_back(er);
            }
            // else: entry was trimmed or already durable — drop the ref.
        }
    }

    /// Drop durable entries from the front of `durable_fifo` until
    /// `total_bytes <= capacity_bytes` or no durable entries remain.
    /// Pre-flush entries are never evicted.
    pub fn evict_to_capacity(&mut self) {
        while self.total_bytes > self.capacity_bytes {
            let Some(er) = self.durable_fifo.pop_front() else {
                break;
            };
            // Stale ref (entry was trimmed)?
            let removed = self
                .by_key
                .get_mut(&er.key)
                .and_then(|chain| chain.remove(&er.lsn));
            if removed.is_some() {
                self.total_bytes -= er.size;
                if let Some(chain) = self.by_key.get(&er.key)
                    && chain.is_empty()
                {
                    self.by_key.remove(&er.key);
                }
            }
        }
    }

    /// Remove all durable entries for `key` with `lsn <= max_lsn`.
    /// Used by the image materialiser to drop deltas the new image
    /// supersedes. Newer deltas and unflushed entries are untouched.
    /// FIFO refs become stale and are skipped by `evict_to_capacity`.
    pub fn trim_key_up_to(&mut self, key: &Bytes, max_lsn: Lsn) {
        let Some(chain) = self.by_key.get_mut(key) else {
            return;
        };
        let to_remove: Vec<Lsn> = chain
            .range(..=max_lsn)
            .filter(|(_, e)| e.durable)
            .map(|(l, _)| *l)
            .collect();
        for lsn in to_remove {
            if let Some(entry) = chain.remove(&lsn) {
                let size = row_size(&entry.row);
                self.total_bytes -= size;
            }
        }
        if chain.is_empty() {
            self.by_key.remove(key);
        }
    }

    /// Return rows for `key` at `lsn <= target_lsn`, LSN-descending.
    /// Includes unflushed.
    pub fn lookup_chain(&self, key: &Bytes, target_lsn: Lsn) -> Vec<PageRow> {
        let Some(chain) = self.by_key.get(key) else {
            return Vec::new();
        };
        chain
            .range(..=target_lsn)
            .rev()
            .map(|(_, e)| e.row.clone())
            .collect()
    }

    /// Like `lookup_chain` but only durable rows. The materialiser
    /// uses this so its image is built from inputs that survive a
    /// crash.
    pub fn lookup_chain_durable(&self, key: &Bytes, target_lsn: Lsn) -> Vec<PageRow> {
        let Some(chain) = self.by_key.get(key) else {
            return Vec::new();
        };
        chain
            .range(..=target_lsn)
            .rev()
            .filter(|(_, e)| e.durable)
            .map(|(_, e)| e.row.clone())
            .collect()
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
        for (k, chain) in &self.by_key {
            if k.as_ref() < start_key.as_ref() || k.as_ref() >= end_key.as_ref() {
                continue;
            }
            let rows: Vec<PageRow> = chain
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
        for er in self.unflushed_fifo.iter().take(cap) {
            if let Some(chain) = self.by_key.get(&er.key)
                && let Some(entry) = chain.get(&er.lsn)
                && !entry.durable
            {
                out.push(entry.row.clone());
            }
        }
        out
    }

    fn insert_entry(&mut self, key: Bytes, lsn: Lsn, row: PageRow, durable: bool) {
        let size = row_size(&row);
        let entry = PageEntry { row, durable };
        let prev = self.by_key.entry(key).or_default().insert(lsn, entry);
        if let Some(prev) = prev {
            let prev_size = row_size(&prev.row);
            self.total_bytes -= prev_size;
            if !prev.durable {
                self.unflushed_bytes -= prev_size;
            }
        }
        self.total_bytes += size;
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
        assert!(u.unflushed_bytes > 0); // one row still unflushed
        assert!(u.unflushed_bytes < unflushed_before);

        let all = u.lookup_chain(&Bytes::from_static(b"k"), Lsn(100));
        assert_eq!(all.len(), 3, "all rows still visible after flush");
    }

    #[test]
    fn lookup_durable_filters_unflushed() {
        let mut u = UnifiedPages::new(4096);
        u.push(mk(b"k", 1, true, b"img"));
        u.push(mk(b"k", 2, false, b"d2"));
        u.mark_first_n_flushed(1); // @1 durable; @2 still unflushed
        u.push(mk(b"k", 3, false, b"d3"));

        let all = u.lookup_chain(&Bytes::from_static(b"k"), Lsn(100));
        let durable = u.lookup_chain_durable(&Bytes::from_static(b"k"), Lsn(100));
        assert_eq!(all.len(), 3);
        assert_eq!(durable.len(), 1);
        assert_eq!(durable[0].lsn, Lsn(1));
    }

    #[test]
    fn push_durable_is_immediately_evictable() {
        let mut u = UnifiedPages::new(20); // tiny cap
        u.push_durable(mk(b"k1", 1, true, b"AAAAAAAAAAAAAAAA"));
        u.push_durable(mk(b"k2", 2, true, b"BBBBBBBBBBBBBBBB"));
        u.evict_to_capacity();
        // Both were durable; the front one should be evicted.
        assert!(u.lookup_chain(&Bytes::from_static(b"k1"), Lsn(100)).is_empty());
    }

    #[test]
    fn evict_skips_unflushed_front() {
        let mut u = UnifiedPages::new(10);
        u.push(mk(b"k1", 1, true, b"xxxxxxxx"));
        u.push(mk(b"k2", 2, true, b"xxxxxxxx"));
        u.evict_to_capacity();
        // Both unflushed → no-op.
        assert!(u.total_bytes > u.capacity_bytes);
        assert_eq!(u.unflushed_fifo.len(), 2);
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
        u.push(mk(b"k", 20, false, b"d20")); // unflushed
        u.trim_key_up_to(&Bytes::from_static(b"k"), Lsn(50));
        let chain = u.lookup_chain(&Bytes::from_static(b"k"), Lsn(100));
        assert_eq!(chain.len(), 1);
        assert_eq!(chain[0].lsn, Lsn(20));
    }

    #[test]
    fn materializer_install_sequence_works_end_to_end() {
        // 5 deltas, flushed → trim to fresh image at lsn 50.
        let mut u = UnifiedPages::new(4096);
        for i in 1..=5 {
            u.push(mk(b"k", i * 10, false, &[i as u8; 4]));
        }
        u.mark_first_n_flushed(5);
        // The materialiser's "install image" sequence:
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
}
