//! yb-mode: the sharding layer has been collapsed.
//!
//! YugabyteDB does the actual tablet-sharding on the underlying row
//! store, so the pageserver runs as a single shard per tenant. The
//! upstream `ShardIdentity`/`ShardCount`/`ShardNumber`/`ShardIndex`/
//! `ShardSlug` machinery is reduced to degenerate stubs here so the
//! remaining call-sites keep compiling. Treat every method on
//! `ShardIdentity` as returning a fixed "unsharded, shard 0" answer;
//! treat `key_to_shard_number` as always `ShardNumber(0)`. New code
//! should not consult sharding at all.

use std::hash::{Hash, Hasher};

#[doc(inline)]
pub use ::utils::shard::*;
use serde::{Deserialize, Serialize};

use crate::key::Key;
use crate::models::ShardParameters;

/// yb-mode stub: an opaque "unsharded shard 0" marker.
///
/// Fields are retained as `pub` so the few call-sites that still read
/// `.number` / `.count` / `.stripe_size` keep compiling, but the
/// values are pinned to the unsharded constants.
#[derive(Clone, Copy, Serialize, Deserialize, Eq, PartialEq, Debug)]
pub struct ShardIdentity {
    pub number: ShardNumber,
    pub count: ShardCount,
    pub stripe_size: ShardStripeSize,
}

impl Hash for ShardIdentity {
    fn hash<H: Hasher>(&self, state: &mut H) {
        self.number.0.hash(state);
        self.count.0.hash(state);
    }
}

pub const DEFAULT_STRIPE_SIZE: ShardStripeSize = ShardStripeSize(16 * 1024 / 8);

#[derive(thiserror::Error, Debug, PartialEq, Eq)]
pub enum ShardConfigError {
    #[error("Invalid shard count")]
    InvalidCount,
    #[error("Invalid shard number")]
    InvalidNumber,
    #[error("Invalid stripe size")]
    InvalidStripeSize,
}

impl ShardIdentity {
    /// The one true `ShardIdentity` in yb-mode.
    pub const fn unsharded() -> Self {
        Self {
            number: ShardNumber(0),
            count: ShardCount(0),
            stripe_size: DEFAULT_STRIPE_SIZE,
        }
    }

    /// yb-mode: stripe size is irrelevant; preserved only because a
    /// few callers want to pass it through unchanged for serialization.
    pub fn unsharded_with_stripe_size(stripe_size: ShardStripeSize) -> Self {
        let mut id = Self::unsharded();
        if stripe_size.0 > 0 {
            id.stripe_size = stripe_size;
        }
        id
    }

    /// yb-mode: there is no broken/usable distinction; preserved only
    /// for the few call-sites that constructed broken identities to
    /// represent "tenant in `Broken` state".
    pub fn broken(number: ShardNumber, count: ShardCount) -> Self {
        Self {
            number,
            count,
            stripe_size: DEFAULT_STRIPE_SIZE,
        }
    }

    /// Always true in yb-mode.
    pub fn is_unsharded(&self) -> bool {
        true
    }

    /// yb-mode: `count` must be zero (unsharded). `number` and
    /// `stripe_size` are ignored — every tenant is shard 0.
    pub fn new(
        _number: ShardNumber,
        count: ShardCount,
        _stripe_size: ShardStripeSize,
    ) -> Result<Self, ShardConfigError> {
        if count.0 != 0 {
            return Err(ShardConfigError::InvalidCount);
        }
        Ok(Self::unsharded())
    }

    pub fn from_params(_number: ShardNumber, _params: ShardParameters) -> Self {
        Self::unsharded()
    }

    pub fn assert_equal(&self, _other: ShardIdentity) {}

    pub fn get_shard_number(&self, _key: &Key) -> ShardNumber {
        ShardNumber(0)
    }

    /// Always true in yb-mode — every key belongs to this (sole) shard.
    pub fn is_key_local(&self, _key: &Key) -> bool {
        true
    }

    /// Whether the key would be served by every shard (vs. one shard
    /// only) in a sharded tenant. Still surfaced because callers use
    /// it to identify SLRU / aux-file / shard0-only keys regardless
    /// of sharding mode.
    pub fn is_key_global(&self, key: &Key) -> bool {
        if key.is_slru_block_key()
            || key.is_slru_segment_size_key()
            || key.is_aux_file_key()
            || key.is_slru_dir_key()
        {
            false
        } else if key.is_rel_block_key() {
            false
        } else {
            true
        }
    }

    /// Always false in yb-mode — there is no "split aftermath" to
    /// dispose of, and every key remains local to this shard.
    pub fn is_key_disposable(&self, _key: &Key) -> bool {
        false
    }

    pub fn shard_index(&self) -> ShardIndex {
        ShardIndex::unsharded()
    }

    pub fn shard_slug(&self) -> String {
        String::new()
    }

    pub fn is_shard_zero(&self) -> bool {
        true
    }
}

pub fn key_to_shard_number(
    _count: ShardCount,
    _stripe_size: ShardStripeSize,
    _key: &Key,
) -> ShardNumber {
    ShardNumber(0)
}

pub fn describe(_key: &Key, _stripe_size: ShardStripeSize) -> impl std::fmt::Debug {
    "shard0"
}

#[cfg(test)]
mod tests {
    use std::str::FromStr;

    use utils::Hex;
    use utils::id::TenantId;

    use super::*;

    const EXAMPLE_TENANT_ID: &str = "1f359dd625e519a1a4e8d7509690f6fc";

    #[test]
    fn tenant_id_string_roundtrip() -> Result<(), hex::FromHexError> {
        let example = TenantId::from_str(EXAMPLE_TENANT_ID).unwrap();
        let encoded = format!("{example}");
        assert_eq!(&encoded, EXAMPLE_TENANT_ID);
        assert_eq!(example, TenantId::from_str(&encoded)?);
        Ok(())
    }

    #[test]
    fn tenant_id_binary_roundtrip() -> Result<(), hex::FromHexError> {
        let example = TenantId::from_str(EXAMPLE_TENANT_ID).unwrap();
        let encoded = bincode::serialize(&example).unwrap();
        let expected: [u8; 16] = [
            0x1f, 0x35, 0x9d, 0xd6, 0x25, 0xe5, 0x19, 0xa1, 0xa4, 0xe8, 0xd7, 0x50, 0x96, 0x90,
            0xf6, 0xfc,
        ];
        assert_eq!(Hex(&encoded), Hex(&expected));
        assert_eq!(example, bincode::deserialize(&encoded).unwrap());
        Ok(())
    }
}
