//! See `pageserver_api::shard` for description on sharding.

use std::ops::RangeInclusive;
use std::str::FromStr;

use hex::FromHex;
use serde::{Deserialize, Serialize};

#[derive(Ord, PartialOrd, Eq, PartialEq, Clone, Copy, Serialize, Deserialize, Debug, Hash)]
pub struct ShardNumber(pub u8);

#[derive(Ord, PartialOrd, Eq, PartialEq, Clone, Copy, Serialize, Deserialize, Debug, Hash)]
pub struct ShardCount(pub u8);

/// Combination of ShardNumber and ShardCount.
///
/// For use within the context of a particular tenant, when we need to know which shard we're
/// dealing with, but do not need to know the full ShardIdentity (because we won't be doing
/// any page->shard mapping), and do not need to know the fully qualified TenantId.
#[derive(Eq, PartialEq, PartialOrd, Ord, Clone, Copy, Hash)]
pub struct ShardIndex {
    pub shard_number: ShardNumber,
    pub shard_count: ShardCount,
}

/// Stripe size as number of pages.
///
/// NB: don't implement Default, so callers don't lazily use it by mistake. See DEFAULT_STRIPE_SIZE.
#[derive(Clone, Copy, Serialize, Deserialize, Eq, PartialEq, Debug)]
pub struct ShardStripeSize(pub u32);

// yb-mode: tenants are never split across pageservers (YugabyteDB
// tablet-shards the keyspace itself), so the upstream
// `TenantShardId`/`TenantId` pair has been collapsed to a single
// `TenantId`. This module re-exports it (via `use`) and hangs the
// yb-mode shim helpers below.
pub use crate::id::TenantId;

impl ShardCount {
    pub const MAX: Self = Self(u8::MAX);
    pub const MIN: Self = Self(0);

    pub fn unsharded() -> Self {
        ShardCount(0)
    }

    /// The internal value of a ShardCount may be zero, which means "1 shard, but use
    /// legacy format for TenantId that excludes the shard suffix", also known
    /// as [`TenantId::unsharded`].
    ///
    /// This method returns the actual number of shards, i.e. if our internal value is
    /// zero, we return 1 (unsharded tenants have 1 shard).
    pub fn count(&self) -> u8 {
        if self.0 > 0 { self.0 } else { 1 }
    }

    /// The literal internal value: this is **not** the number of shards in the
    /// tenant, as we have a special zero value for legacy unsharded tenants.  Use
    /// [`Self::count`] if you want to know the cardinality of shards.
    pub fn literal(&self) -> u8 {
        self.0
    }

    /// Whether the `ShardCount` is for an unsharded tenant, so uses one shard but
    /// uses the legacy format for `TenantId`. See also the documentation for
    /// [`Self::count`].
    pub fn is_unsharded(&self) -> bool {
        self.0 == 0
    }

    /// `v` may be zero, or the number of shards in the tenant.  `v` is what
    /// [`Self::literal`] would return.
    pub const fn new(val: u8) -> Self {
        Self(val)
    }
}

impl ShardNumber {
    pub const MAX: Self = Self(u8::MAX);
}

/// yb-mode shims: `TenantId` is just `TenantId`, so these methods
/// live on `TenantId` itself. They exist so upstream call-sites that
/// reference the sharded API keep compiling without conditional code.
impl TenantId {
    /// Identity shim — yb-mode tenants are always unsharded.
    pub fn unsharded(tenant_id: TenantId) -> Self {
        tenant_id
    }

    /// Degenerate single-value range — yb-mode tenants are single-shard.
    pub fn tenant_range(tenant_id: TenantId) -> RangeInclusive<Self> {
        RangeInclusive::new(tenant_id, tenant_id)
    }

    pub fn range(&self) -> RangeInclusive<Self> {
        RangeInclusive::new(*self, *self)
    }

    pub fn shard_slug(&self) -> &'static str {
        "0000"
    }

    pub fn is_shard_zero(&self) -> bool {
        true
    }

    pub fn is_unsharded(&self) -> bool {
        true
    }

    pub fn to_index(&self) -> ShardIndex {
        ShardIndex::unsharded()
    }
}

impl std::fmt::Display for ShardNumber {
    fn fmt(&self, f: &mut std::fmt::Formatter<'_>) -> std::fmt::Result {
        self.0.fmt(f)
    }
}

impl std::fmt::Display for ShardCount {
    fn fmt(&self, f: &mut std::fmt::Formatter<'_>) -> std::fmt::Result {
        self.0.fmt(f)
    }
}

impl std::fmt::Display for ShardStripeSize {
    fn fmt(&self, f: &mut std::fmt::Formatter<'_>) -> std::fmt::Result {
        self.0.fmt(f)
    }
}

// yb-mode: `TenantId` is `TenantId`, so Display/Debug/FromStr
// come from `TenantId` directly — see `crate::id`. The encoded form
// is the 32-char tenant-id hex (no shard suffix).

impl ShardIndex {
    pub fn new(number: ShardNumber, count: ShardCount) -> Self {
        Self {
            shard_number: number,
            shard_count: count,
        }
    }
    pub fn unsharded() -> Self {
        Self {
            shard_number: ShardNumber(0),
            shard_count: ShardCount(0),
        }
    }

    /// The "unsharded" value is distinct from simply having a single shard: it represents
    /// a tenant which is not shard-aware at all, and whose storage paths will not include
    /// a shard suffix.
    pub fn is_unsharded(&self) -> bool {
        self.shard_number == ShardNumber(0) && self.shard_count == ShardCount(0)
    }

    /// For use in constructing remote storage paths: concatenate this with a TenantId
    /// to get a fully qualified TenantId.
    ///
    /// Backward compat: this function returns an empty string if Self::is_unsharded, such
    /// that the legacy pre-sharding remote key format is preserved.
    pub fn get_suffix(&self) -> String {
        if self.is_unsharded() {
            "".to_string()
        } else {
            format!("-{:02x}{:02x}", self.shard_number.0, self.shard_count.0)
        }
    }
}

impl std::fmt::Display for ShardIndex {
    fn fmt(&self, f: &mut std::fmt::Formatter<'_>) -> std::fmt::Result {
        write!(f, "{:02x}{:02x}", self.shard_number.0, self.shard_count.0)
    }
}

impl std::fmt::Debug for ShardIndex {
    fn fmt(&self, f: &mut std::fmt::Formatter<'_>) -> std::fmt::Result {
        // Debug is the same as Display: the compact hex representation
        write!(f, "{self}")
    }
}

impl std::str::FromStr for ShardIndex {
    type Err = hex::FromHexError;

    fn from_str(s: &str) -> Result<Self, Self::Err> {
        // Expect format: 1 byte shard number, 1 byte shard count
        if s.len() == 4 {
            let bytes = s.as_bytes();
            let mut shard_parts: [u8; 2] = [0u8; 2];
            hex::decode_to_slice(bytes, &mut shard_parts)?;
            Ok(Self {
                shard_number: ShardNumber(shard_parts[0]),
                shard_count: ShardCount(shard_parts[1]),
            })
        } else {
            Err(hex::FromHexError::InvalidStringLength)
        }
    }
}

impl From<[u8; 2]> for ShardIndex {
    fn from(b: [u8; 2]) -> Self {
        Self {
            shard_number: ShardNumber(b[0]),
            shard_count: ShardCount(b[1]),
        }
    }
}

// yb-mode: Serialize/Deserialize for TenantId come from `TenantId`.

impl Serialize for ShardIndex {
    fn serialize<S>(&self, serializer: S) -> Result<S::Ok, S::Error>
    where
        S: serde::Serializer,
    {
        if serializer.is_human_readable() {
            serializer.collect_str(self)
        } else {
            // Binary encoding is not used in index_part.json, but is included in anticipation of
            // switching various structures (e.g. inter-process communication, remote metadata) to more
            // compact binary encodings in future.
            let mut packed: [u8; 2] = [0; 2];
            packed[0] = self.shard_number.0;
            packed[1] = self.shard_count.0;
            packed.serialize(serializer)
        }
    }
}

impl<'de> Deserialize<'de> for ShardIndex {
    fn deserialize<D>(deserializer: D) -> Result<Self, D::Error>
    where
        D: serde::Deserializer<'de>,
    {
        struct IdVisitor {
            is_human_readable_deserializer: bool,
        }

        impl<'de> serde::de::Visitor<'de> for IdVisitor {
            type Value = ShardIndex;

            fn expecting(&self, formatter: &mut std::fmt::Formatter) -> std::fmt::Result {
                if self.is_human_readable_deserializer {
                    formatter.write_str("value in form of hex string")
                } else {
                    formatter.write_str("value in form of integer array([u8; 2])")
                }
            }

            fn visit_seq<A>(self, seq: A) -> Result<Self::Value, A::Error>
            where
                A: serde::de::SeqAccess<'de>,
            {
                let s = serde::de::value::SeqAccessDeserializer::new(seq);
                let id: [u8; 2] = Deserialize::deserialize(s)?;
                Ok(ShardIndex::from(id))
            }

            fn visit_str<E>(self, v: &str) -> Result<Self::Value, E>
            where
                E: serde::de::Error,
            {
                ShardIndex::from_str(v).map_err(E::custom)
            }
        }

        if deserializer.is_human_readable() {
            deserializer.deserialize_str(IdVisitor {
                is_human_readable_deserializer: true,
            })
        } else {
            deserializer.deserialize_tuple(
                2,
                IdVisitor {
                    is_human_readable_deserializer: false,
                },
            )
        }
    }
}
