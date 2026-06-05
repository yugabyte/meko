//! In-memory cache of recently-written WAL chunks for YB-WAL mode.
//!
//! Without this cache, every `read_segment_from_yb` (used by the WAL sender
//! to stream WAL to the pageserver) hits YB. Worse, the previous
//! implementation reassembled the entire 16 MiB segment from a single SQL
//! query *at open_segment time*, then served from that frozen snapshot — so
//! chunks INSERT'd into `yb_storage.wals` *after* the snapshot were never
//! seen by the WAL sender, manifesting as `xlp_magic=0` / `xl_tot_len=0`
//! decode errors a few hundred KB into the segment.
//!
//! This cache is populated by `consumer_main` *after* the chunk has been
//! successfully INSERT'd into `yb_storage.wals` (NOT by the producer
//! before the INSERT — see the durability discussion in the
//! `yb_wal_storage` module docs). So the cache only ever holds chunks
//! that are also durable in YB. Reads through [`YbSegmentReader`]
//! consult the cache first; only if the chunk is no longer cached
//! (evicted FIFO once the cache outgrows its byte budget) does the read
//! fall through to YB, which has the chunk by construction.
//!
//! Why a plain `Mutex`: writes and reads naturally target disjoint
//! offsets — writes append past `flush_lsn`, reads consume up to it — so
//! the BTreeMap entries they touch never overlap. The lock holds only
//! across O(log n) metadata ops (no I/O), so a single coarse mutex is
//! fine.
//!
//! The cache is dropped on safekeeper shutdown. After restart, reads go
//! to YB until the next write_wal repopulates it. Correctness does not
//! depend on the cache surviving; it's purely a read-side accelerator.
//!
//! Init chunks (the 16 MiB blob produced by `initialize_first_segment`
//! for segno 0 only) are intentionally **not** cached — they would chew
//! the entire default capacity for a one-time read at boot, and the
//! per-byte cost of fetching them lazily from YB is negligible at that
//! point.
//!
//! Concrete bug fixed: see commit message; this is the second half of
//! the fix for the `xlp_magic=0` retry-storm we hit on the YB-WAL TPC-C
//! run.
//!
//! ## API summary
//!
//! - [`WalCache::new`] – construct with a byte capacity (e.g. 256 MiB).
//! - [`WalCache::insert`] – called by `consumer_main` once a batch of
//!   chunks has been successfully INSERT'd into `yb_storage.wals`.
//! - [`WalCache::lookup`] – called by `YbSegmentReader` to find the chunk
//!   covering a given `(segno, byte_offset)`. Returns the chunk's range
//!   and a clone of its `Arc<Vec<u8>>`.
//! - [`WalCache::truncate_after`] / [`WalCache::remove_segnos_below`] —
//!   called alongside the matching YB DELETE so the cache cannot serve
//!   stale data after a `truncate_wal` / `remove_up_to`.

use std::collections::{BTreeMap, HashMap, VecDeque};
use std::sync::Arc;

use crate::metrics::{
    WAL_CACHE_BYTES, WAL_CACHE_EVICTIONS, WAL_CACHE_HITS, WAL_CACHE_MISSES,
};

/// One cached chunk, mirroring a row in `yb_storage.wals` but without
/// the (tenant_id, timeline_id) prefix (those are captured at the
/// containing `WalCache`'s scope — one cache per `YbWalStorage`).
#[derive(Clone)]
struct CachedChunk {
    chunk_size: usize,
    data: Arc<Vec<u8>>,
}

/// FIFO eviction key: identifies a single cached chunk.
#[derive(Clone, Copy, Debug)]
struct CacheKey {
    segno: u64,
    chunk_offset: u64,
}

/// In-memory cache of recent WAL chunks. See module docs.
pub struct WalCache {
    by_segno: HashMap<u64, BTreeMap<u64, CachedChunk>>,
    /// Insertion order, used for FIFO eviction. May contain stale keys
    /// (entries whose chunk has already been removed by `truncate_after`
    /// or `remove_segnos_below`); evictor skips those.
    fifo: VecDeque<CacheKey>,
    total_bytes: usize,
    capacity_bytes: usize,
    hits: u64,
    misses: u64,
    evictions: u64,
}

impl WalCache {
    pub fn new(capacity_bytes: usize) -> Self {
        Self {
            by_segno: HashMap::new(),
            fifo: VecDeque::new(),
            total_bytes: 0,
            capacity_bytes,
            hits: 0,
            misses: 0,
            evictions: 0,
        }
    }

    /// Insert a chunk. Caller is responsible for not inserting the
    /// init-first-segment 16 MiB blob (see module docs).
    ///
    /// If a chunk with the same `(segno, chunk_offset)` already exists,
    /// it's an idempotent re-INSERT of the same bytes (the writer never
    /// produces overlapping chunks; see `yb_wal_storage` module docs).
    /// Treat it as a no-op rather than mutating the existing entry.
    pub fn insert(&mut self, segno: u64, chunk_offset: u64, data: Arc<Vec<u8>>) {
        let chunk_size = data.len();
        let new_chunk = CachedChunk { chunk_size, data };
        let key = CacheKey {
            segno,
            chunk_offset,
        };
        let seg_map = self.by_segno.entry(segno).or_default();
        use std::collections::btree_map::Entry::*;
        if let Vacant(v) = seg_map.entry(chunk_offset) {
            v.insert(new_chunk);
            self.fifo.push_back(key);
            self.total_bytes += chunk_size;
            WAL_CACHE_BYTES.add(chunk_size as i64);
        }
        self.evict_if_over_capacity();
    }

    /// Find the chunk that covers `(segno, byte_offset)`. Updates hit /
    /// miss counters.
    pub fn lookup(
        &mut self,
        segno: u64,
        byte_offset: u64,
    ) -> Option<(u64 /* chunk_offset */, u64 /* end_offset */, Arc<Vec<u8>>)> {
        let seg_map = self.by_segno.get(&segno);
        let result = seg_map.and_then(|m| {
            // Highest chunk_offset <= byte_offset.
            let (&chunk_offset, chunk) = m.range(..=byte_offset).next_back()?;
            let end = chunk_offset + chunk.chunk_size as u64;
            if byte_offset < end {
                Some((chunk_offset, end, chunk.data.clone()))
            } else {
                None
            }
        });
        if result.is_some() {
            self.hits += 1;
            WAL_CACHE_HITS.inc();
        } else {
            self.misses += 1;
            WAL_CACHE_MISSES.inc();
        }
        result
    }

    /// Drop cached chunks for `(segno, offset)` whose range extends past
    /// `end_offset` in segno `segno_end`, plus all chunks for segnos
    /// strictly greater than `segno_end`. Mirrors the SQL DELETE in
    /// `YbWalStorage::delete_after`.
    pub fn truncate_after(&mut self, segno_end: u64, end_offset: u64) {
        let drop_segnos: Vec<u64> = self
            .by_segno
            .keys()
            .copied()
            .filter(|s| *s > segno_end)
            .collect();
        for s in drop_segnos {
            if let Some(seg) = self.by_segno.remove(&s) {
                for (_, c) in seg {
                    self.total_bytes -= c.chunk_size;
                    WAL_CACHE_BYTES.sub(c.chunk_size as i64);
                }
            }
        }
        if let Some(seg) = self.by_segno.get_mut(&segno_end) {
            let drops: Vec<u64> = seg
                .iter()
                .filter(|(off, c)| *off + &(c.chunk_size as u64) > end_offset)
                .map(|(off, _)| *off)
                .collect();
            for off in drops {
                if let Some(c) = seg.remove(&off) {
                    self.total_bytes -= c.chunk_size;
                    WAL_CACHE_BYTES.sub(c.chunk_size as i64);
                }
            }
            if seg.is_empty() {
                self.by_segno.remove(&segno_end);
            }
        }
        // fifo may contain stale keys; lookup/evict tolerates that.
    }

    /// Drop cached chunks for all segnos strictly less than
    /// `segno_up_to`. Mirrors the SQL DELETE in
    /// `YbWalStorage::remove_up_to`.
    pub fn remove_segnos_below(&mut self, segno_up_to: u64) {
        let drop_segnos: Vec<u64> = self
            .by_segno
            .keys()
            .copied()
            .filter(|s| *s < segno_up_to)
            .collect();
        for s in drop_segnos {
            if let Some(seg) = self.by_segno.remove(&s) {
                for (_, c) in seg {
                    self.total_bytes -= c.chunk_size;
                    WAL_CACHE_BYTES.sub(c.chunk_size as i64);
                }
            }
        }
    }

    pub fn stats(&self) -> WalCacheStats {
        WalCacheStats {
            total_bytes: self.total_bytes,
            capacity_bytes: self.capacity_bytes,
            hits: self.hits,
            misses: self.misses,
            evictions: self.evictions,
            cached_segnos: self.by_segno.len(),
        }
    }

    fn evict_if_over_capacity(&mut self) {
        while self.total_bytes > self.capacity_bytes {
            let Some(k) = self.fifo.pop_front() else {
                // Inconsistency: nothing left in fifo but total_bytes > capacity.
                // Reset the counter to recover.
                self.total_bytes = 0;
                break;
            };
            let removed_size = self
                .by_segno
                .get_mut(&k.segno)
                .and_then(|seg| seg.remove(&k.chunk_offset))
                .map(|c| c.chunk_size);
            if let Some(size) = removed_size {
                self.total_bytes -= size;
                self.evictions += 1;
                WAL_CACHE_BYTES.sub(size as i64);
                WAL_CACHE_EVICTIONS.inc();
                if let Some(seg) = self.by_segno.get_mut(&k.segno) {
                    if seg.is_empty() {
                        self.by_segno.remove(&k.segno);
                    }
                }
            }
            // else: stale fifo key (already removed by truncate_after /
            // remove_segnos_below). Loop and try the next one.
        }
    }
}

#[derive(Debug, Clone, Copy)]
pub struct WalCacheStats {
    pub total_bytes: usize,
    pub capacity_bytes: usize,
    pub hits: u64,
    pub misses: u64,
    pub evictions: u64,
    pub cached_segnos: usize,
}

#[cfg(test)]
mod tests {
    use super::*;

    fn data(b: u8, n: usize) -> Arc<Vec<u8>> {
        Arc::new(vec![b; n])
    }

    #[test]
    fn insert_then_lookup_round_trip() {
        let mut c = WalCache::new(1024);
        c.insert(7, 0, data(0xAB, 100));
        c.insert(7, 100, data(0xCD, 50));

        let (off, end, d) = c.lookup(7, 0).unwrap();
        assert_eq!((off, end), (0, 100));
        assert_eq!(d[0], 0xAB);

        let (off, end, d) = c.lookup(7, 120).unwrap();
        assert_eq!((off, end), (100, 150));
        assert_eq!(d[0], 0xCD);

        assert!(c.lookup(7, 150).is_none()); // past end
        assert!(c.lookup(8, 0).is_none()); // wrong segno
    }

    #[test]
    fn fifo_eviction_when_over_capacity() {
        let mut c = WalCache::new(150);
        c.insert(1, 0, data(b'a', 100)); // 100 bytes
        c.insert(2, 0, data(b'b', 100)); // 200 → over 150 → evicts segno 1
        assert!(c.lookup(1, 0).is_none());
        assert!(c.lookup(2, 0).is_some());
    }

    #[test]
    fn truncate_after_drops_higher_segnos_and_overshoots() {
        let mut c = WalCache::new(10_000);
        c.insert(5, 0, data(b'x', 100));
        c.insert(5, 200, data(b'y', 100)); // end=300
        c.insert(6, 0, data(b'z', 100));
        c.truncate_after(5, 150); // keep segno 5 chunks ending <= 150; drop segno 6
        assert!(c.lookup(5, 50).is_some()); // [0,100) — survives
        assert!(c.lookup(5, 250).is_none()); // [200,300) — dropped (end > 150)
        assert!(c.lookup(6, 0).is_none()); // higher segno dropped
    }

    #[test]
    fn remove_segnos_below_clears_lower_segnos_only() {
        let mut c = WalCache::new(10_000);
        c.insert(1, 0, data(b'a', 10));
        c.insert(2, 0, data(b'b', 10));
        c.insert(3, 0, data(b'c', 10));
        c.remove_segnos_below(3);
        assert!(c.lookup(1, 0).is_none());
        assert!(c.lookup(2, 0).is_none());
        assert!(c.lookup(3, 0).is_some());
    }

    #[test]
    fn duplicate_insert_at_same_offset_is_noop() {
        // The writer never produces two chunks at the same
        // (segno, chunk_offset); a duplicate insert is by construction
        // an idempotent retry of the same bytes. Cache treats it as a
        // no-op (keep the first entry).
        let mut c = WalCache::new(10_000);
        c.insert(0, 0, data(b'n', 50));
        c.insert(0, 0, data(b'o', 50));
        let (_, _, d) = c.lookup(0, 0).unwrap();
        assert_eq!(d[0], b'n');
    }
}
