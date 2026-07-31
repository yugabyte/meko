use std::ops::Range;

use itertools::Itertools;

use crate::key::Key;

///
/// Represents a set of Keys, in a compact form.
///
#[derive(Clone, Debug, Default, PartialEq, Eq)]
pub struct KeySpace {
    /// Contiguous ranges of keys that belong to the key space. In key order,
    /// and with no overlap.
    pub ranges: Vec<Range<Key>>,
}

impl std::fmt::Display for KeySpace {
    fn fmt(&self, f: &mut std::fmt::Formatter<'_>) -> std::fmt::Result {
        write!(f, "[")?;
        for range in &self.ranges {
            write!(f, "{}..{},", range.start, range.end)?;
        }
        write!(f, "]")
    }
}

/// A wrapper type for sparse keyspaces.
#[derive(Clone, Debug, Default, PartialEq, Eq)]
pub struct SparseKeySpace(pub KeySpace);

/// A contiguous half-open range of the keyspace, with helpers to chunk it
/// by size. (Was `ShardedRange` upstream, where it masked keys by shard;
/// yb-amp is single-shard, so every key is present and the methods are
/// purely size-based.)
#[derive(Clone, Debug, PartialEq, Eq)]
pub struct KeyspaceRange {
    pub range: Range<Key>,
}

// Calculate the size of a range within the blocks of the same relation, or spanning only the
// top page in the previous relation's space.
pub fn contiguous_range_len(range: &Range<Key>) -> u32 {
    debug_assert!(is_contiguous_range(range));
    if range.start.field6 == 0xffffffff {
        range.end.field6 + 1
    } else {
        range.end.field6 - range.start.field6
    }
}

/// Return true if this key range includes only keys in the same relation's data blocks, or
/// just spanning one relation and the logical size (0xffffffff) block of the relation before it.
///
/// Contiguous in this context means we know the keys are in use _somewhere_, but it might not
/// be on our shard.
///
/// This matters, because:
/// - Within such ranges, keys are used contiguously.  Outside such ranges it is sparse.
/// - Within such ranges, we may calculate distances using simple subtraction of field6.
pub fn is_contiguous_range(range: &Range<Key>) -> bool {
    range.start.field1 == range.end.field1
        && range.start.field2 == range.end.field2
        && range.start.field3 == range.end.field3
        && range.start.field4 == range.end.field4
        && (range.start.field5 == range.end.field5
            || (range.start.field6 == 0xffffffff && range.start.field5 + 1 == range.end.field5))
}

impl KeyspaceRange {
    pub fn new(range: Range<Key>) -> Self {
        Self { range }
    }

    /// Break up this range into chunks of at most `target_nblocks`.
    /// Every block counts; the chunking is purely size-based.
    pub fn fragment(self, target_nblocks: u32) -> Vec<(u32, Range<Key>)> {
        if self.range.end == self.range.start.add(1) {
            return vec![(1, self.range)];
        }

        if !is_contiguous_range(&self.range) {
            return vec![(u32::MAX, self.range)];
        }

        let mut fragments: Vec<(u32, Range<Key>)> = Vec::new();
        let mut cursor = self.range.start;
        while cursor < self.range.end {
            let distance_to_end = contiguous_range_len(&(cursor..self.range.end));
            let want_blocks = match fragments.last() {
                Some(frag) if frag.0 < target_nblocks => target_nblocks - frag.0,
                _ => target_nblocks,
            };
            let advance_by = std::cmp::min(distance_to_end, want_blocks);
            let next_cursor = cursor.add(advance_by);
            let this_frag = (advance_by, cursor..next_cursor);
            cursor = next_cursor;

            match fragments.last_mut() {
                Some(frag) if frag.0 < target_nblocks => {
                    frag.0 += this_frag.0;
                    frag.1.end = this_frag.1.end;
                }
                _ => fragments.push(this_frag),
            }
        }

        fragments
    }

    /// Estimate the physical pages that are within this range: the
    /// contiguous range length (or `u32::MAX` for non-contiguous ranges).
    pub fn page_count(&self) -> u32 {
        if self.range.end == self.range.start.add(1) {
            return 1;
        }
        if !is_contiguous_range(&self.range) {
            return u32::MAX;
        }
        contiguous_range_len(&self.range)
    }

    /// Whereas `page_count` estimates the number of pages physically in this range on this shard,
    /// this function simply calculates the number of pages in the space, without accounting for those
    /// pages that would not actually be stored on this node.
    ///
    /// Don't use this function in code that works with physical entities like layer files.
    pub fn raw_size(range: &Range<Key>) -> u32 {
        if is_contiguous_range(range) {
            contiguous_range_len(range)
        } else {
            u32::MAX
        }
    }
}

impl KeySpace {
    /// Create a key space with a single range.
    pub fn single(key_range: Range<Key>) -> Self {
        Self {
            ranges: vec![key_range],
        }
    }

    /// Partition a key space into roughly chunks of roughly 'target_size' bytes
    /// in each partition.
    ///
    pub fn partition(&self, target_size: u64, block_size: u64) -> KeyPartitioning {
        let target_nblocks = (target_size / block_size) as u32;

        let mut parts = Vec::new();
        let mut current_part = Vec::new();
        let mut current_part_size: usize = 0;
        for range in &self.ranges {
            let range = KeyspaceRange::new(range.clone());

            // Chunk up the range into parts that each contain up to target_size local blocks
            for (frag_on_shard_size, frag_range) in range.fragment(target_nblocks) {
                // If appending the next contiguous range in the keyspace to the current
                // partition would cause it to be too large, and our current partition
                // covers at least one block that is physically present in this shard,
                // then start a new partition
                if current_part_size + frag_on_shard_size as usize > target_nblocks as usize
                    && current_part_size > 0
                {
                    parts.push(KeySpace {
                        ranges: current_part,
                    });
                    current_part = Vec::new();
                    current_part_size = 0;
                }
                current_part.push(frag_range.start..frag_range.end);
                current_part_size += frag_on_shard_size as usize;
            }
        }

        // add last partition that wasn't full yet.
        if !current_part.is_empty() {
            parts.push(KeySpace {
                ranges: current_part,
            });
        }

        KeyPartitioning { parts }
    }

    pub fn is_empty(&self) -> bool {
        self.total_raw_size() == 0
    }

    /// Merge another keyspace into the current one.
    /// Note: the keyspaces must not overlap (enforced via assertions). To merge overlapping key ranges, use `KeySpaceRandomAccum`.
    pub fn merge(&mut self, other: &KeySpace) {
        let all_ranges = self
            .ranges
            .iter()
            .merge_by(other.ranges.iter(), |lhs, rhs| lhs.start < rhs.start);

        let mut accum = KeySpaceAccum::new();
        let mut prev: Option<&Range<Key>> = None;
        for range in all_ranges {
            if let Some(prev) = prev {
                let overlap =
                    std::cmp::max(range.start, prev.start) < std::cmp::min(range.end, prev.end);
                assert!(
                    !overlap,
                    "Attempt to merge ovelapping keyspaces: {prev:?} overlaps {range:?}"
                );
            }

            accum.add_range(range.clone());
            prev = Some(range);
        }

        self.ranges = accum.to_keyspace().ranges;
    }

    /// Remove all keys in `other` from `self`.
    /// This can involve splitting or removing of existing ranges.
    /// Returns the removed keyspace
    pub fn remove_overlapping_with(&mut self, other: &KeySpace) -> KeySpace {
        let (self_start, self_end) = match (self.start(), self.end()) {
            (Some(start), Some(end)) => (start, end),
            _ => {
                // self is empty
                return KeySpace::default();
            }
        };

        // Key spaces are sorted by definition, so skip ahead to the first
        // potentially intersecting range. Similarly, ignore ranges that start
        // after the current keyspace ends.
        let other_ranges = other
            .ranges
            .iter()
            .skip_while(|range| self_start >= range.end)
            .take_while(|range| self_end > range.start);

        let mut removed_accum = KeySpaceRandomAccum::new();
        for range in other_ranges {
            while let Some(overlap_at) = self.overlaps_at(range) {
                let overlapped = self.ranges[overlap_at].clone();

                if overlapped.start < range.start && overlapped.end <= range.end {
                    // Higher part of the range is completely overlapped.
                    removed_accum.add_range(range.start..self.ranges[overlap_at].end);
                    self.ranges[overlap_at].end = range.start;
                }
                if overlapped.start >= range.start && overlapped.end > range.end {
                    // Lower part of the range is completely overlapped.
                    removed_accum.add_range(self.ranges[overlap_at].start..range.end);
                    self.ranges[overlap_at].start = range.end;
                }
                if overlapped.start < range.start && overlapped.end > range.end {
                    // Middle part of the range is overlapped.
                    removed_accum.add_range(range.clone());
                    self.ranges[overlap_at].end = range.start;
                    self.ranges
                        .insert(overlap_at + 1, range.end..overlapped.end);
                }
                if overlapped.start >= range.start && overlapped.end <= range.end {
                    // Whole range is overlapped
                    removed_accum.add_range(self.ranges[overlap_at].clone());
                    self.ranges.remove(overlap_at);
                }
            }
        }

        removed_accum.to_keyspace()
    }

    pub fn start(&self) -> Option<Key> {
        self.ranges.first().map(|range| range.start)
    }

    pub fn end(&self) -> Option<Key> {
        self.ranges.last().map(|range| range.end)
    }

    /// The size of the keyspace in pages, before accounting for sharding
    pub fn total_raw_size(&self) -> usize {
        self.ranges
            .iter()
            .map(|range| KeyspaceRange::raw_size(range) as usize)
            .sum()
    }

    fn overlaps_at(&self, range: &Range<Key>) -> Option<usize> {
        match self.ranges.binary_search_by_key(&range.end, |r| r.start) {
            Ok(0) => None,
            Err(0) => None,
            Ok(index) if self.ranges[index - 1].end > range.start => Some(index - 1),
            Err(index) if self.ranges[index - 1].end > range.start => Some(index - 1),
            _ => None,
        }
    }

    ///
    /// Check if key space contains overlapping range
    ///
    pub fn overlaps(&self, range: &Range<Key>) -> bool {
        self.overlaps_at(range).is_some()
    }

    /// Check if the keyspace contains a key
    pub fn contains(&self, key: &Key) -> bool {
        self.overlaps(&(*key..key.next()))
    }
}

///
/// Represents a partitioning of the key space.
///
/// The only kind of partitioning we do is to partition the key space into
/// partitions that are roughly equal in physical size (see KeySpace::partition).
/// But this data structure could represent any partitioning.
///
#[derive(Clone, Debug, Default)]
pub struct KeyPartitioning {
    pub parts: Vec<KeySpace>,
}

/// Represents a partitioning of the sparse key space.
#[derive(Clone, Debug, Default)]
pub struct SparseKeyPartitioning {
    pub parts: Vec<SparseKeySpace>,
}

impl KeyPartitioning {
    pub fn new() -> Self {
        KeyPartitioning { parts: Vec::new() }
    }

    /// Convert a key partitioning to a sparse partition.
    pub fn into_sparse(self) -> SparseKeyPartitioning {
        SparseKeyPartitioning {
            parts: self.parts.into_iter().map(SparseKeySpace).collect(),
        }
    }
}

impl SparseKeyPartitioning {
    /// Note: use this function with caution. Attempt to handle a sparse keyspace in the same way as a dense keyspace will
    /// cause long/dead loops.
    pub fn into_dense(self) -> KeyPartitioning {
        KeyPartitioning {
            parts: self.parts.into_iter().map(|x| x.0).collect(),
        }
    }
}

///
/// A helper object, to collect a set of keys and key ranges into a KeySpace
/// object. This takes care of merging adjacent keys and key ranges into
/// contiguous ranges.
///
#[derive(Clone, Debug, Default)]
pub struct KeySpaceAccum {
    accum: Option<Range<Key>>,

    ranges: Vec<Range<Key>>,
    size: u64,
}

impl KeySpaceAccum {
    pub fn new() -> Self {
        Self {
            accum: None,
            ranges: Vec::new(),
            size: 0,
        }
    }

    #[inline(always)]
    pub fn add_key(&mut self, key: Key) {
        self.add_range(singleton_range(key))
    }

    #[inline(always)]
    pub fn add_range(&mut self, range: Range<Key>) {
        self.size += KeyspaceRange::raw_size(&range) as u64;

        match self.accum.as_mut() {
            Some(accum) => {
                if range.start == accum.end {
                    accum.end = range.end;
                } else {
                    // TODO: to efficiently support small sharding stripe sizes, we should avoid starting
                    // a new range here if the skipped region was all keys that don't belong on this shard.
                    // (https://github.com/neondatabase/neon/issues/6247)
                    assert!(range.start > accum.end);
                    self.ranges.push(accum.clone());
                    *accum = range;
                }
            }
            None => self.accum = Some(range),
        }
    }

    pub fn to_keyspace(mut self) -> KeySpace {
        if let Some(accum) = self.accum.take() {
            self.ranges.push(accum);
        }
        KeySpace {
            ranges: self.ranges,
        }
    }

    pub fn consume_keyspace(&mut self) -> KeySpace {
        std::mem::take(self).to_keyspace()
    }

    // The total number of keys in this object, ignoring any sharding effects that might cause some of
    // the keys to be omitted in storage on this shard.
    pub fn raw_size(&self) -> u64 {
        self.size
    }
}

///
/// A helper object, to collect a set of keys and key ranges into a KeySpace
/// object. Key ranges may be inserted in any order and can overlap.
///
#[derive(Clone, Debug, Default)]
pub struct KeySpaceRandomAccum {
    ranges: Vec<Range<Key>>,
}

impl KeySpaceRandomAccum {
    pub fn new() -> Self {
        Self { ranges: Vec::new() }
    }

    pub fn add_key(&mut self, key: Key) {
        self.add_range(singleton_range(key))
    }

    pub fn add_range(&mut self, range: Range<Key>) {
        self.ranges.push(range);
    }

    pub fn add_keyspace(&mut self, keyspace: KeySpace) {
        for range in keyspace.ranges {
            self.add_range(range);
        }
    }

    pub fn to_keyspace(mut self) -> KeySpace {
        let mut ranges = Vec::new();
        if !self.ranges.is_empty() {
            self.ranges.sort_by_key(|r| r.start);
            let mut start = self.ranges.first().unwrap().start;
            let mut end = self.ranges.first().unwrap().end;
            for r in self.ranges {
                assert!(r.start >= start);
                if r.start > end {
                    ranges.push(start..end);
                    start = r.start;
                    end = r.end;
                } else if r.end > end {
                    end = r.end;
                }
            }
            ranges.push(start..end);
        }
        KeySpace { ranges }
    }

    pub fn consume_keyspace(&mut self) -> KeySpace {
        let mut prev_accum = KeySpaceRandomAccum::new();
        std::mem::swap(self, &mut prev_accum);

        prev_accum.to_keyspace()
    }
}

pub fn singleton_range(key: Key) -> Range<Key> {
    key..key.next()
}

#[cfg(test)]
mod tests {
    use std::fmt::Write;

    use rand::{RngCore, SeedableRng};

    use super::*;

    // Helper function to create a key range.
    //
    // Make the tests below less verbose.
    fn kr(irange: Range<i128>) -> Range<Key> {
        Key::from_i128(irange.start)..Key::from_i128(irange.end)
    }

    #[allow(dead_code)]
    fn dump_keyspace(ks: &KeySpace) {
        for r in ks.ranges.iter() {
            println!("  {}..{}", r.start.to_i128(), r.end.to_i128());
        }
    }

    fn assert_ks_eq(actual: &KeySpace, expected: Vec<Range<Key>>) {
        if actual.ranges != expected {
            let mut msg = String::new();

            writeln!(msg, "expected:").unwrap();
            for r in &expected {
                writeln!(msg, "  {}..{}", r.start.to_i128(), r.end.to_i128()).unwrap();
            }
            writeln!(msg, "got:").unwrap();
            for r in &actual.ranges {
                writeln!(msg, "  {}..{}", r.start.to_i128(), r.end.to_i128()).unwrap();
            }
            panic!("{}", msg);
        }
    }

    #[test]
    fn keyspace_consume() {
        let ranges = vec![kr(0..10), kr(20..35), kr(40..45)];

        let mut accum = KeySpaceAccum::new();
        for range in &ranges {
            accum.add_range(range.clone());
        }

        let expected_size: u64 = ranges
            .iter()
            .map(|r| KeyspaceRange::raw_size(r) as u64)
            .sum();
        assert_eq!(accum.raw_size(), expected_size);

        assert_ks_eq(&accum.consume_keyspace(), ranges.clone());
        assert_eq!(accum.raw_size(), 0);

        assert_ks_eq(&accum.consume_keyspace(), vec![]);
        assert_eq!(accum.raw_size(), 0);

        for range in &ranges {
            accum.add_range(range.clone());
        }
        assert_ks_eq(&accum.to_keyspace(), ranges);
    }

    #[test]
    fn keyspace_add_range() {
        // two separate ranges
        //
        // #####
        //         #####
        let mut ks = KeySpaceRandomAccum::default();
        ks.add_range(kr(0..10));
        ks.add_range(kr(20..30));
        assert_ks_eq(&ks.to_keyspace(), vec![kr(0..10), kr(20..30)]);

        // two separate ranges, added in reverse order
        //
        //         #####
        // #####
        let mut ks = KeySpaceRandomAccum::default();
        ks.add_range(kr(20..30));
        ks.add_range(kr(0..10));

        // add range that is adjacent to the end of an existing range
        //
        // #####
        //      #####
        ks.add_range(kr(0..10));
        ks.add_range(kr(10..30));
        assert_ks_eq(&ks.to_keyspace(), vec![kr(0..30)]);

        // add range that is adjacent to the start of an existing range
        //
        //      #####
        // #####
        let mut ks = KeySpaceRandomAccum::default();
        ks.add_range(kr(10..30));
        ks.add_range(kr(0..10));
        assert_ks_eq(&ks.to_keyspace(), vec![kr(0..30)]);

        // add range that overlaps with the end of an existing range
        //
        // #####
        //    #####
        let mut ks = KeySpaceRandomAccum::default();
        ks.add_range(kr(0..10));
        ks.add_range(kr(5..30));
        assert_ks_eq(&ks.to_keyspace(), vec![kr(0..30)]);

        // add range that overlaps with the start of an existing range
        //
        //    #####
        // #####
        let mut ks = KeySpaceRandomAccum::default();
        ks.add_range(kr(5..30));
        ks.add_range(kr(0..10));
        assert_ks_eq(&ks.to_keyspace(), vec![kr(0..30)]);

        // add range that is fully covered by an existing range
        //
        // #########
        //   #####
        let mut ks = KeySpaceRandomAccum::default();
        ks.add_range(kr(0..30));
        ks.add_range(kr(10..20));
        assert_ks_eq(&ks.to_keyspace(), vec![kr(0..30)]);

        // add range that extends an existing range from both ends
        //
        //   #####
        // #########
        let mut ks = KeySpaceRandomAccum::default();
        ks.add_range(kr(10..20));
        ks.add_range(kr(0..30));
        assert_ks_eq(&ks.to_keyspace(), vec![kr(0..30)]);

        // add a range that overlaps with two existing ranges, joining them
        //
        // #####   #####
        //    #######
        let mut ks = KeySpaceRandomAccum::default();
        ks.add_range(kr(0..10));
        ks.add_range(kr(20..30));
        ks.add_range(kr(5..25));
        assert_ks_eq(&ks.to_keyspace(), vec![kr(0..30)]);
    }

    #[test]
    fn keyspace_overlaps() {
        let mut ks = KeySpaceRandomAccum::default();
        ks.add_range(kr(10..20));
        ks.add_range(kr(30..40));
        let ks = ks.to_keyspace();

        //        #####      #####
        // xxxx
        assert!(!ks.overlaps(&kr(0..5)));

        //        #####      #####
        //   xxxx
        assert!(!ks.overlaps(&kr(5..9)));

        //        #####      #####
        //    xxxx
        assert!(!ks.overlaps(&kr(5..10)));

        //        #####      #####
        //     xxxx
        assert!(ks.overlaps(&kr(5..11)));

        //        #####      #####
        //        xxxx
        assert!(ks.overlaps(&kr(10..15)));

        //        #####      #####
        //         xxxx
        assert!(ks.overlaps(&kr(15..20)));

        //        #####      #####
        //           xxxx
        assert!(ks.overlaps(&kr(15..25)));

        //        #####      #####
        //              xxxx
        assert!(!ks.overlaps(&kr(22..28)));

        //        #####      #####
        //               xxxx
        assert!(!ks.overlaps(&kr(25..30)));

        //        #####      #####
        //                      xxxx
        assert!(ks.overlaps(&kr(35..35)));

        //        #####      #####
        //                        xxxx
        assert!(!ks.overlaps(&kr(40..45)));

        //        #####      #####
        //                        xxxx
        assert!(!ks.overlaps(&kr(45..50)));

        //        #####      #####
        //        xxxxxxxxxxx
        assert!(ks.overlaps(&kr(0..30))); // XXXXX This fails currently!
    }

    #[test]
    fn test_remove_full_overlapps() {
        let mut key_space1 = KeySpace {
            ranges: vec![
                Key::from_i128(1)..Key::from_i128(4),
                Key::from_i128(5)..Key::from_i128(8),
                Key::from_i128(10)..Key::from_i128(12),
            ],
        };
        let key_space2 = KeySpace {
            ranges: vec![
                Key::from_i128(2)..Key::from_i128(3),
                Key::from_i128(6)..Key::from_i128(7),
                Key::from_i128(11)..Key::from_i128(13),
            ],
        };
        let removed = key_space1.remove_overlapping_with(&key_space2);
        let removed_expected = KeySpace {
            ranges: vec![
                Key::from_i128(2)..Key::from_i128(3),
                Key::from_i128(6)..Key::from_i128(7),
                Key::from_i128(11)..Key::from_i128(12),
            ],
        };
        assert_eq!(removed, removed_expected);

        assert_eq!(
            key_space1.ranges,
            vec![
                Key::from_i128(1)..Key::from_i128(2),
                Key::from_i128(3)..Key::from_i128(4),
                Key::from_i128(5)..Key::from_i128(6),
                Key::from_i128(7)..Key::from_i128(8),
                Key::from_i128(10)..Key::from_i128(11)
            ]
        );
    }

    #[test]
    fn test_remove_partial_overlaps() {
        // Test partial ovelaps
        let mut key_space1 = KeySpace {
            ranges: vec![
                Key::from_i128(1)..Key::from_i128(5),
                Key::from_i128(7)..Key::from_i128(10),
                Key::from_i128(12)..Key::from_i128(15),
            ],
        };
        let key_space2 = KeySpace {
            ranges: vec![
                Key::from_i128(3)..Key::from_i128(6),
                Key::from_i128(8)..Key::from_i128(11),
                Key::from_i128(14)..Key::from_i128(17),
            ],
        };

        let removed = key_space1.remove_overlapping_with(&key_space2);
        let removed_expected = KeySpace {
            ranges: vec![
                Key::from_i128(3)..Key::from_i128(5),
                Key::from_i128(8)..Key::from_i128(10),
                Key::from_i128(14)..Key::from_i128(15),
            ],
        };
        assert_eq!(removed, removed_expected);

        assert_eq!(
            key_space1.ranges,
            vec![
                Key::from_i128(1)..Key::from_i128(3),
                Key::from_i128(7)..Key::from_i128(8),
                Key::from_i128(12)..Key::from_i128(14),
            ]
        );
    }

    #[test]
    fn test_remove_no_overlaps() {
        let mut key_space1 = KeySpace {
            ranges: vec![
                Key::from_i128(1)..Key::from_i128(5),
                Key::from_i128(7)..Key::from_i128(10),
                Key::from_i128(12)..Key::from_i128(15),
            ],
        };
        let key_space2 = KeySpace {
            ranges: vec![
                Key::from_i128(6)..Key::from_i128(7),
                Key::from_i128(11)..Key::from_i128(12),
                Key::from_i128(15)..Key::from_i128(17),
            ],
        };

        let removed = key_space1.remove_overlapping_with(&key_space2);
        let removed_expected = KeySpace::default();
        assert_eq!(removed, removed_expected);

        assert_eq!(
            key_space1.ranges,
            vec![
                Key::from_i128(1)..Key::from_i128(5),
                Key::from_i128(7)..Key::from_i128(10),
                Key::from_i128(12)..Key::from_i128(15),
            ]
        );
    }

    #[test]
    fn test_remove_one_range_overlaps_multiple() {
        let mut key_space1 = KeySpace {
            ranges: vec![
                Key::from_i128(1)..Key::from_i128(3),
                Key::from_i128(3)..Key::from_i128(6),
                Key::from_i128(6)..Key::from_i128(10),
                Key::from_i128(12)..Key::from_i128(15),
                Key::from_i128(17)..Key::from_i128(20),
                Key::from_i128(20)..Key::from_i128(30),
                Key::from_i128(30)..Key::from_i128(40),
            ],
        };
        let key_space2 = KeySpace {
            ranges: vec![Key::from_i128(9)..Key::from_i128(19)],
        };

        let removed = key_space1.remove_overlapping_with(&key_space2);
        let removed_expected = KeySpace {
            ranges: vec![
                Key::from_i128(9)..Key::from_i128(10),
                Key::from_i128(12)..Key::from_i128(15),
                Key::from_i128(17)..Key::from_i128(19),
            ],
        };
        assert_eq!(removed, removed_expected);

        assert_eq!(
            key_space1.ranges,
            vec![
                Key::from_i128(1)..Key::from_i128(3),
                Key::from_i128(3)..Key::from_i128(6),
                Key::from_i128(6)..Key::from_i128(9),
                Key::from_i128(19)..Key::from_i128(20),
                Key::from_i128(20)..Key::from_i128(30),
                Key::from_i128(30)..Key::from_i128(40),
            ]
        );
    }
    #[test]
    fn range_relation_gap() {
        let range = KeyspaceRange::new(Range {
            start: Key::from_hex("000000067F00000005000040100300000000").unwrap(),
            end: Key::from_hex("000000067F00000005000040130000004000").unwrap(),
        });

        // Key range spans relations, expect MAX
        assert_eq!(range.page_count(), u32::MAX);
    }

    #[test]
    fn range_single_logical_size_key() {
        let range = KeyspaceRange::new(Range {
            start: Key::from_hex("000000067f000000010000007000ffffffff").unwrap(),
            end: Key::from_hex("000000067f00000001000000700100000000").unwrap(),
        });
        // Single-key range on logical size key
        assert_eq!(range.page_count(), 1);
    }

    /// Test the helper that we use to identify ranges which go outside the data blocks of a single relation
    #[test]
    fn contiguous_range_check() {
        assert!(!is_contiguous_range(
            &(Key::from_hex("000000067f00000001000004df00fffffffe").unwrap()
                ..Key::from_hex("000000067f00000001000004df0100000003").unwrap())
        ),);

        // The ranges goes all the way up to the 0xffffffff, including it: this is
        // not considered a rel block range because 0xffffffff stores logical sizes,
        // not blocks.
        assert!(!is_contiguous_range(
            &(Key::from_hex("000000067f00000001000004df00fffffffe").unwrap()
                ..Key::from_hex("000000067f00000001000004df0100000000").unwrap())
        ),);

        // Keys within the normal data region of a relation
        assert!(is_contiguous_range(
            &(Key::from_hex("000000067f00000001000004df0000000000").unwrap()
                ..Key::from_hex("000000067f00000001000004df0000000080").unwrap())
        ),);

        // The logical size key of one forkno, then some blocks in the next
        assert!(is_contiguous_range(
            &(Key::from_hex("000000067f00000001000004df00ffffffff").unwrap()
                ..Key::from_hex("000000067f00000001000004df0100000080").unwrap())
        ),);
    }

    #[test]
    fn range_forkno_gap() {
        let range = KeyspaceRange::new(Range {
            start: Key::from_hex("000000067f00000001000004df00fffffffe").unwrap(),
            end: Key::from_hex("000000067f00000001000004df0100000003").unwrap(),
        });

        // Range spanning the end of one forkno and the start of the next: we do not attempt to
        // calculate a valid size, because we have no way to know if they keys between start
        // and end are actually in use.
        assert_eq!(range.page_count(), u32::MAX);
    }

    /// Test helper: construct a KeyspaceRange and call fragment() on it, returning
    /// the total page count in the range and the fragments.
    fn do_fragment(
        range_start: Key,
        range_end: Key,
        target_nblocks: u32,
    ) -> (u32, Vec<(u32, Range<Key>)>) {
        let range = KeyspaceRange::new(Range {
            start: range_start,
            end: range_end,
        });

        let page_count = range.page_count();
        let fragments = range.fragment(target_nblocks);

        // Invariant: we always get at least one fragment
        assert!(!fragments.is_empty());

        // Invariant: the first/last fragment start/end should equal the input start/end
        assert_eq!(fragments.first().unwrap().1.start, range_start);
        assert_eq!(fragments.last().unwrap().1.end, range_end);

        // Invariant: every fragment is non-empty (single-shard: every block is present)
        assert!(fragments.iter().all(|f| f.0 > 0));

        // Invariant: fragments must be ordered and non-overlapping
        let mut last: Option<Range<Key>> = None;
        for frag in &fragments {
            if let Some(last) = last {
                assert!(frag.1.start >= last.end);
                assert!(frag.1.start > last.start);
            }
            last = Some(frag.1.clone())
        }

        // Invariant: fragments respect target_nblocks
        for frag in &fragments {
            assert!(frag.0 == u32::MAX || frag.0 <= target_nblocks);
        }

        (page_count, fragments)
    }

    #[test]
    fn range_fragment_by_size() {
        let input_start = Key::from_hex("000000067f00000001000000ae0000000000").unwrap();
        let input_end = Key::from_hex("000000067f00000001000000ae0000010000").unwrap();
        assert_eq!(
            do_fragment(input_start, input_end, 0x8000),
            (
                0x10000,
                vec![
                    (0x8000, input_start..input_start.add(0x8000)),
                    (0x8000, input_start.add(0x8000)..input_start.add(0x10000))
                ]
            )
        );
    }

    #[test]
    fn range_fragment_cross_relation() {
        // A range that spans relations: expect fragmentation to give up and return a u32::MAX size
        let input_start = Key::from_hex("000000067f00000001000000ae0000000000").unwrap();
        let input_end = Key::from_hex("000000068f00000001000000ae0000010000").unwrap();
        assert_eq!(
            do_fragment(input_start, input_end, 0x8000),
            (u32::MAX, vec![(u32::MAX, input_start..input_end),])
        );
    }

    #[test]
    fn range_fragment_tiny_nblocks() {
        let input_start = Key::from_hex("000000067F00000001000004E10000000000").unwrap();
        let input_end = Key::from_hex("000000067F00000001000004E10000000038").unwrap();
        assert_eq!(
            do_fragment(input_start, input_end, 16),
            (
                0x38,
                vec![
                    (16, input_start..input_start.add(16)),
                    (16, input_start.add(16)..input_start.add(32)),
                    (16, input_start.add(32)..input_start.add(48)),
                    (8, input_start.add(48)..input_end),
                ]
            )
        );
    }

    #[test]
    fn range_fragment_fuzz() {
        // Use a fixed seed: we don't want to explicitly pick values, but we do want
        // the test to be reproducible.
        let mut prng = rand::rngs::StdRng::seed_from_u64(0xdeadbeef);

        for _i in 0..1000 {
            let target_nblocks = prng.next_u32() % 65536 + 1;

            let start_offset = prng.next_u32() % 16384;

            // Try ranges up to 4GiB in size, that are always at least 1
            let range_size = prng.next_u32() % 8192 + 1;

            let input_start = Key::from_hex("000000067F00000001000004E10000000000")
                .unwrap()
                .add(start_offset);
            let input_end = input_start.add(range_size);

            // This test's main success conditions are the invariants baked into do_fragment
            let (_total_size, fragments) = do_fragment(input_start, input_end, target_nblocks);

            // Pick a random key within the range and check it appears in the output
            let example_key = input_start.add(prng.next_u32() % range_size);

            // Panic on unwrap if it isn't found, and its fragment must be non-empty
            let example_key_frag = fragments
                .iter()
                .find(|f| f.1.contains(&example_key))
                .unwrap();
            assert!(example_key_frag.0 > 0);
        }
    }
}
