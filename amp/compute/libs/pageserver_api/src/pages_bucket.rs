//! Placement bucket for `amp_storage.pages` — the leading column of that
//! table's all-range primary key.
//!
//! Lives here, rather than in the pageserver's `page_store`, because it is
//! shared: the pageserver computes it on every write and binds it on every
//! read, and the operator tools (`dump-pages`) must compute the same value
//! to pin their queries. Two copies of a frozen key function is how rows go
//! missing, so there is exactly one.

/// Placement buckets in `amp_storage.pages`'s range key, and the number of
/// tablets the migration pre-splits it into. 6 keeps the leader count even
/// on the common 3-node RF=3 storage cluster (2 leaders per node).
pub const PAGE_BUCKETS: i64 = 6;

/// Blocks kept together in one bucket — 256 x 8KB = 2MB. This is what stops
/// a sequential scan from spraying every block across every tablet: it walks
/// whole 2MB stretches, one bucket at a time.
pub const PAGE_BUCKET_STRIDE: i64 = 256;

/// Placement bucket for a relation page — the leading column of
/// `amp_storage.pages`'s key.
///
/// FROZEN, and in a stronger sense than `page_store::gc_bucket_id`: a wrong
/// bucket there only strands a GC candidate, whereas here it puts the row
/// outside the key range every reader looks in, i.e. loses a page. Changing
/// this function invalidates every stored row.
///
/// Must stay in lockstep with the `pages_bucket_id_matches_formula` CHECK in
/// migration 0002 — including the literal 7, 13, 256 and 6, which are spelled
/// out there rather than derived, so a change here needs a change there.
/// Deliberately not a generated column: the thin-client write path bypasses
/// the YSQL layer, so the writer must compute it.
///
/// `relnode / 7 + relnode / 13` salts sub-2MB relations, which are a single
/// stretch and would otherwise all land in bucket 0. Two coprime divisors
/// because one collapses when the OID allocation stride resonates with the
/// modulus — with `relnode / 7` alone a stride of 42 puts every relation in
/// one bucket. `forknum` is deliberately absent: the main fork holds nearly
/// all the data, so including it would only shuffle FSM/VM.
///
/// Integer division throughout, matching PostgreSQL's truncated `/` and `%`
/// exactly. The inputs are a `Key`'s u32 fields, and the arithmetic runs in
/// `i64` — as the CHECK's `::bigint` casts do — so neither overflows nor goes
/// negative.
pub fn page_bucket_id(relnode: u32, blknum: u32) -> i16 {
    bucket_of(i64::from(relnode), i64::from(blknum))
}

fn bucket_of(relnode: i64, blknum: i64) -> i16 {
    ((relnode / 7 + relnode / 13 + blknum / PAGE_BUCKET_STRIDE) % PAGE_BUCKETS) as i16
}

/// The buckets a one-fork scan of `[blk_lo, blk_hi)` can touch, ascending and
/// deduplicated. Callers issue one bucket-pinned query per entry: binding the
/// leading key column as a VALUE is what keeps each query pruned to its
/// tablets, so a scan spanning stretches fans out rather than dropping the
/// bound.
///
/// Empty when the range is empty. Saturates at every bucket once the range
/// spans [`PAGE_BUCKETS`] stretches, which is the common case for a large
/// relation.
pub fn page_buckets_for_range(relnode: u32, blk_lo: i64, blk_hi: i64) -> Vec<i16> {
    if blk_lo >= blk_hi {
        return Vec::new();
    }
    let first = blk_lo / PAGE_BUCKET_STRIDE;
    let last = (blk_hi - 1) / PAGE_BUCKET_STRIDE;
    if last - first >= PAGE_BUCKETS - 1 {
        return (0..PAGE_BUCKETS as i16).collect();
    }
    let mut buckets: Vec<i16> = (first..=last)
        .map(|stretch| bucket_of(i64::from(relnode), stretch * PAGE_BUCKET_STRIDE))
        .collect();
    buckets.sort_unstable();
    buckets.dedup();
    buckets
}

#[cfg(test)]
mod tests {
    use super::*;

    /// `page_bucket_id` leads `pages`' key, so it is frozen hard: a change
    /// relocates every row and loses every page at the old bucket. Golden
    /// values are cross-checked against the SQL
    /// `((relnode/7)+(relnode/13)+(blknum/256))%6` in migration 0002 — both
    /// languages truncate `/` and `%`, so the two must agree exactly.
    #[test]
    fn page_bucket_id_is_frozen_and_bounded() {
        // Golden values (change ⇒ you broke the frozen bucket — intended?).
        assert_eq!(page_bucket_id(16400, 0), 3);
        assert_eq!(page_bucket_id(16401, 0), 4);
        assert_eq!(page_bucket_id(16402, 0), 4);
        assert_eq!(page_bucket_id(0, 0), 0);
        assert_eq!(page_bucket_id(1663, 999_999), 4);
        // No overflow or sign flip at the top of the u32 OID / blknum space.
        assert_eq!(page_bucket_id(4_294_967_295, 4_294_967_295), 4);

        // A whole 2MB stretch shares one bucket; the next stretch is the next
        // bucket; six stretches wrap. This is the sequential-scan locality
        // property — without it a scan sprays across every tablet.
        let stride = PAGE_BUCKET_STRIDE as u32;
        for blk in 0..stride {
            assert_eq!(page_bucket_id(16400, blk), 3, "stretch 0 is one bucket");
        }
        assert_eq!(page_bucket_id(16400, stride), 4);
        assert_eq!(page_bucket_id(16400, stride * PAGE_BUCKETS as u32), 3);

        // Deterministic + in range over a spread of blocks.
        for g in 0..10_000u32 {
            let (rel, blk) = (16400 + (g % 50), g);
            let b = page_bucket_id(rel, blk);
            assert_eq!(b, page_bucket_id(rel, blk), "stable");
            assert!((0..PAGE_BUCKETS as i16).contains(&b), "in range: {b}");
        }

        // The case the two-divisor salt exists for: many sub-2MB relations
        // (one stretch each, so `blknum / 256` contributes nothing) laid out
        // at a fixed OID stride. A single-divisor salt collapses here for
        // stride 42; this must stay near-even for every plausible stride.
        // Bound is 1.5x the ideal share, against a measured worst of 1.39x
        // for strides <= 30.
        for stride in 1..=30u32 {
            let mut counts = [0usize; PAGE_BUCKETS as usize];
            for g in 0..600u32 {
                counts[page_bucket_id(16384 + stride * g, 0) as usize] += 1;
            }
            let worst = *counts.iter().max().unwrap();
            assert!(
                worst <= 150,
                "stride {stride} skews small relations: {counts:?}"
            );
        }
    }

    /// The scan fan-out helper: one bucket per 2MB stretch touched, saturating
    /// at every bucket. Callers issue one bucket-pinned query per entry, so a
    /// wrong answer here means either a missed page or a needless full scan.
    #[test]
    fn page_buckets_for_range_covers_every_touched_stretch() {
        // Empty range ⇒ no queries at all.
        assert!(page_buckets_for_range(16400, 5, 5).is_empty());
        assert!(page_buckets_for_range(16400, 9, 5).is_empty());

        // Wholly inside one stretch ⇒ exactly the block's own bucket.
        assert_eq!(page_buckets_for_range(16400, 0, 256), vec![3]);
        assert_eq!(page_buckets_for_range(16400, 10, 200), vec![3]);
        // Straddling a stretch boundary ⇒ both, ascending.
        assert_eq!(page_buckets_for_range(16400, 200, 300), vec![3, 4]);
        // Spanning every bucket ⇒ all of them, once each.
        assert_eq!(
            page_buckets_for_range(16400, 0, 256 * 6),
            vec![0, 1, 2, 3, 4, 5]
        );
        // Beyond saturation stays saturated (no duplicates).
        assert_eq!(
            page_buckets_for_range(16400, 0, 256 * 400),
            vec![0, 1, 2, 3, 4, 5]
        );

        // Whatever the helper returns must actually contain every block's own
        // bucket — the property the callers depend on for correctness.
        for (lo, hi) in [(0i64, 1i64), (0, 700), (250, 260), (1000, 4000), (7, 9)] {
            let buckets = page_buckets_for_range(16400, lo, hi);
            for blk in lo..hi {
                let b = page_bucket_id(16400, blk as u32);
                assert!(
                    buckets.contains(&b),
                    "range [{lo},{hi}) dropped bucket {b} for blknum {blk}"
                );
            }
        }
    }
}
