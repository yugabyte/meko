//! 10-delta image rule.
//!
//! `PageStore` reads are bounded by `SELECT … ORDER BY lsn DESC LIMIT
//! IMAGE_CHAIN_LIMIT` (default 10). For that bound to guarantee an image is
//! always in the returned chain, the write path must intersperse an image
//! every `IMAGE_CHAIN_LIMIT` rows per key. This module is the bookkeeper:
//! it tracks per-key "deltas since last image" and returns a `WriteAction`
//! telling the caller whether the next entry can be written as-is or must
//! be replaced with a walredo-materialised image at the same LSN.
//!
//! The rule:
//! - First time we see a key in this session: if the entry is `will_init`
//!   (`Value::Image` or a `WalRecord` with `will_init=true`), pass through
//!   and start the counter at 0. Otherwise force an image — we can't tell
//!   whether the PageStore already has an image for this key, so be safe.
//! - Subsequent entries: a `will_init` entry resets the counter to 0.
//!   A normal delta increments. When the counter would reach the
//!   threshold, force an image instead and reset the counter to 0.
//!
//! This module is pure logic — no DB, no walredo, no async. Callers
//! perform the actual walredo and the actual `PageStore` write.

use std::collections::HashMap;
use std::collections::hash_map::Entry;

use pageserver_api::key::Key;
use wal_decoder::models::value::Value;

/// Maximum chain length the read path expects. With the 10-delta rule
/// enforced, `SELECT … ORDER BY lsn DESC LIMIT IMAGE_CHAIN_LIMIT` always
/// includes at least one row with `will_init=true`.
pub const IMAGE_CHAIN_LIMIT: usize = 10;

/// What to do with the next (Key, Value) entry.
#[derive(Debug, Clone, Copy, PartialEq, Eq)]
pub enum WriteAction {
    /// Write the entry verbatim — use `Value::is_image()` / `Value::will_init()`
    /// to populate the row's flags, and the value's bytes as `payload`.
    Passthrough,
    /// Replace the entry's payload with a walredo-materialised image at the
    /// entry's LSN. Write the row with `is_image=true, will_init=true`.
    /// Caller is responsible for the walredo step.
    ForceImage,
}

/// Per-key counter for the 10-delta rule.
pub struct ImageRule {
    threshold: usize,
    /// Number of delta rows written since the last image, per key. Absent
    /// entries mean "we've never seen this key in this session".
    deltas_since_image: HashMap<Key, usize>,
}

impl ImageRule {
    /// Build a rule that forces an image every `threshold` rows per key.
    /// `threshold` must be ≥ 2 so there's at least one delta between
    /// images.
    pub fn new(threshold: usize) -> Self {
        assert!(
            threshold >= 2,
            "threshold must be >= 2; got {threshold}"
        );
        Self {
            threshold,
            deltas_since_image: HashMap::new(),
        }
    }

    /// `ImageRule::new(IMAGE_CHAIN_LIMIT)`.
    pub fn default_threshold() -> Self {
        Self::new(IMAGE_CHAIN_LIMIT)
    }

    pub fn threshold(&self) -> usize {
        self.threshold
    }

    /// Tracked-key count — for tests and metrics.
    pub fn tracked_keys(&self) -> usize {
        self.deltas_since_image.len()
    }

    /// Decide what to do with the next entry.
    pub fn decide(&mut self, key: Key, value: &Value) -> WriteAction {
        match self.deltas_since_image.entry(key) {
            Entry::Occupied(mut slot) => {
                if value.will_init() {
                    // Image or will_init delta — chain anchor. Reset.
                    *slot.get_mut() = 0;
                    WriteAction::Passthrough
                } else {
                    let next = slot.get() + 1;
                    if next >= self.threshold {
                        // This delta would push the chain over the limit.
                        // Replace it with a materialised image.
                        *slot.get_mut() = 0;
                        WriteAction::ForceImage
                    } else {
                        *slot.get_mut() = next;
                        WriteAction::Passthrough
                    }
                }
            }
            Entry::Vacant(slot) => {
                if value.will_init() {
                    slot.insert(0);
                    WriteAction::Passthrough
                } else {
                    // First write to this key in this session is a plain
                    // delta. We don't know whether PageStore already holds
                    // a chain anchor at a recent enough LSN. Force an
                    // image to be safe — bounded read invariant survives
                    // pageserver restarts.
                    slot.insert(0);
                    WriteAction::ForceImage
                }
            }
        }
    }

    /// Drop a key from the counter map — e.g., when a timeline detaches.
    pub fn forget(&mut self, key: Key) {
        self.deltas_since_image.remove(&key);
    }

    /// Reset every counter — e.g., on full session restart.
    pub fn clear(&mut self) {
        self.deltas_since_image.clear();
    }
}

#[cfg(test)]
mod tests {
    use super::*;
    use bytes::Bytes;
    use wal_decoder::models::record::NeonWalRecord;

    fn k(n: u32) -> Key {
        // Synthetic key in the relation-page namespace — content irrelevant
        // to ImageRule, which keys on Key equality only.
        Key {
            field1: 0,
            field2: 0,
            field3: 0,
            field4: 0,
            field5: 0,
            field6: n,
        }
    }

    fn image() -> Value {
        Value::Image(Bytes::from_static(b"8k-page"))
    }

    fn delta() -> Value {
        Value::WalRecord(NeonWalRecord::ClearVisibilityMapFlags {
            new_heap_blkno: Some(1),
            old_heap_blkno: None,
            flags: 0x01,
        })
    }

    fn will_init_delta() -> Value {
        Value::WalRecord(NeonWalRecord::Postgres {
            will_init: true,
            rec: Bytes::from_static(b"init-rec"),
        })
    }

    #[test]
    fn first_image_passes_through() {
        let mut r = ImageRule::default_threshold();
        assert_eq!(r.decide(k(1), &image()), WriteAction::Passthrough);
        assert_eq!(r.tracked_keys(), 1);
    }

    #[test]
    fn first_will_init_delta_passes_through() {
        let mut r = ImageRule::default_threshold();
        assert_eq!(r.decide(k(1), &will_init_delta()), WriteAction::Passthrough);
    }

    #[test]
    fn first_plain_delta_for_a_key_forces_image() {
        let mut r = ImageRule::default_threshold();
        // We've never seen this key — to maintain the 10-row chain
        // invariant across pageserver restarts, we force an image rather
        // than trust that PageStore has one within reach.
        assert_eq!(r.decide(k(1), &delta()), WriteAction::ForceImage);
        // After force-image, counter is 0 — next delta is a normal delta.
        assert_eq!(r.decide(k(1), &delta()), WriteAction::Passthrough);
    }

    #[test]
    fn nine_deltas_pass_then_tenth_forces_image() {
        let mut r = ImageRule::new(10);
        // Anchor with an image.
        r.decide(k(1), &image());
        for i in 0..9 {
            assert_eq!(
                r.decide(k(1), &delta()),
                WriteAction::Passthrough,
                "delta #{i}"
            );
        }
        assert_eq!(r.decide(k(1), &delta()), WriteAction::ForceImage);
        // Counter reset — next 9 deltas pass.
        for _ in 0..9 {
            assert_eq!(r.decide(k(1), &delta()), WriteAction::Passthrough);
        }
        assert_eq!(r.decide(k(1), &delta()), WriteAction::ForceImage);
    }

    #[test]
    fn keys_are_independent() {
        let mut r = ImageRule::new(10);
        r.decide(k(1), &image());
        r.decide(k(2), &image());
        for _ in 0..9 {
            assert_eq!(r.decide(k(1), &delta()), WriteAction::Passthrough);
        }
        // k(2)'s counter is still 0 — its 9th-from-anchor delta passes.
        for _ in 0..9 {
            assert_eq!(r.decide(k(2), &delta()), WriteAction::Passthrough);
        }
        assert_eq!(r.decide(k(1), &delta()), WriteAction::ForceImage);
        assert_eq!(r.decide(k(2), &delta()), WriteAction::ForceImage);
    }

    #[test]
    fn will_init_delta_acts_as_chain_anchor() {
        let mut r = ImageRule::new(10);
        r.decide(k(1), &image());
        for _ in 0..9 {
            r.decide(k(1), &delta());
        }
        // A will_init delta resets — no force-image needed even though
        // we'd be at the threshold.
        assert_eq!(r.decide(k(1), &will_init_delta()), WriteAction::Passthrough);
        // Fresh counter — 9 more deltas pass.
        for _ in 0..9 {
            assert_eq!(r.decide(k(1), &delta()), WriteAction::Passthrough);
        }
        assert_eq!(r.decide(k(1), &delta()), WriteAction::ForceImage);
    }

    #[test]
    fn natural_image_mid_chain_resets_counter() {
        let mut r = ImageRule::new(10);
        r.decide(k(1), &image());
        for _ in 0..5 {
            r.decide(k(1), &delta());
        }
        // Counter is 5; a natural image resets it.
        assert_eq!(r.decide(k(1), &image()), WriteAction::Passthrough);
        for _ in 0..9 {
            assert_eq!(r.decide(k(1), &delta()), WriteAction::Passthrough);
        }
        assert_eq!(r.decide(k(1), &delta()), WriteAction::ForceImage);
    }

    #[test]
    fn threshold_three_minimal_useful_setting() {
        let mut r = ImageRule::new(3);
        r.decide(k(1), &image());
        assert_eq!(r.decide(k(1), &delta()), WriteAction::Passthrough); // counter=1
        assert_eq!(r.decide(k(1), &delta()), WriteAction::Passthrough); // counter=2
        assert_eq!(r.decide(k(1), &delta()), WriteAction::ForceImage); // would have been 3
    }

    #[test]
    #[should_panic(expected = "threshold must be >= 2")]
    fn threshold_one_panics() {
        let _ = ImageRule::new(1);
    }

    #[test]
    fn forget_drops_counter() {
        let mut r = ImageRule::new(10);
        r.decide(k(1), &image());
        r.decide(k(1), &delta());
        r.decide(k(1), &delta());
        assert_eq!(r.tracked_keys(), 1);
        r.forget(k(1));
        assert_eq!(r.tracked_keys(), 0);
        // After forget, next delta is treated as first-ever — forces image.
        assert_eq!(r.decide(k(1), &delta()), WriteAction::ForceImage);
    }

    #[test]
    fn clear_drops_all_counters() {
        let mut r = ImageRule::new(10);
        for i in 0..20 {
            r.decide(k(i), &image());
        }
        assert_eq!(r.tracked_keys(), 20);
        r.clear();
        assert_eq!(r.tracked_keys(), 0);
    }

    #[test]
    fn no_op_action_pairs_with_value_will_init() {
        // Cross-check: WriteAction::Passthrough always corresponds to one
        // of (image, will_init delta, normal delta with counter < threshold-1).
        // The caller relies on `value.will_init()` and `value.is_image()` to
        // populate row flags, so we just confirm the contract.
        let v_img = image();
        let v_init = will_init_delta();
        let v_d = delta();
        assert!(v_img.will_init());
        assert!(v_img.is_image());
        assert!(v_init.will_init());
        assert!(!v_init.is_image());
        assert!(!v_d.will_init());
        assert!(!v_d.is_image());
    }
}
