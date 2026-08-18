use std::fmt;
use std::str::FromStr;

use anyhow::Context;
use bytes::Bytes;
use serde::de::Visitor;
use serde::{Deserialize, Serialize};
use uuid::Uuid;
use ysql_client::tokio_postgres::types::{FromSql, IsNull, ToSql, Type, to_sql_checked};

/// yb-amp branch ID: the globally unique identifier of a branch (one branch ==
/// one history of a cluster). Formerly `TimelineId`; the storage model is now
/// flat (one branch, no enclosing tenant), so this single id identifies a branch
/// end to end.
///
/// It is a `uuid` — the type every `branch_id` column holds — so it binds and
/// reads back as itself and renders in one spelling everywhere. A newtype rather
/// than a bare `Uuid` so a branch id cannot be passed where another id is meant.
///
/// Distinct from a PostgreSQL timeline ID, which is only 32 bits wide and must
/// ascend within a history; a branch id is 128 bits of which [`generate`] fills
/// 122 at random (UUIDv4 fixes the version and variant nibbles), so one can be
/// minted without coordinating with anyone. Nothing may read those nibbles: ids
/// minted before this was a uuid, by `yb_amp_local` and the integration harness,
/// are 16 fully random bytes and carry no version.
///
/// [`generate`]: BranchId::generate
#[derive(Clone, Copy, PartialEq, Eq, Hash, Ord, PartialOrd)]
pub struct BranchId(Uuid);

impl BranchId {
    pub fn generate() -> Self {
        BranchId(Uuid::new_v4())
    }

    pub const fn from_array(b: [u8; 16]) -> Self {
        BranchId(Uuid::from_bytes(b))
    }

    /// The same 16 bytes, for the thin-client lane: the shim reports a column's
    /// bind type from its DocDB type, and DocDB types `uuid` exactly as `bytea`
    /// (binary), so a `uuid` column binds as bytes.
    pub fn to_bytes(self) -> Bytes {
        Bytes::copy_from_slice(self.0.as_bytes())
    }
}

impl FromStr for BranchId {
    type Err = uuid::Error;

    /// Accepts every spelling `Uuid` does — hyphenated, simple, braced, urn — so
    /// an id copied out of psql parses as readily as one off the wire.
    fn from_str(s: &str) -> Result<BranchId, Self::Err> {
        Ok(BranchId(Uuid::parse_str(s)?))
    }
}

impl From<[u8; 16]> for BranchId {
    fn from(b: [u8; 16]) -> Self {
        BranchId::from_array(b)
    }
}

impl AsRef<[u8]> for BranchId {
    fn as_ref(&self) -> &[u8] {
        self.0.as_bytes()
    }
}

impl From<BranchId> for u128 {
    fn from(id: BranchId) -> Self {
        u128::from_le_bytes(*id.0.as_bytes())
    }
}

impl fmt::Display for BranchId {
    fn fmt(&self, f: &mut fmt::Formatter<'_>) -> fmt::Result {
        fmt::Display::fmt(&self.0, f)
    }
}

impl fmt::Debug for BranchId {
    fn fmt(&self, f: &mut fmt::Formatter<'_>) -> fmt::Result {
        fmt::Display::fmt(&self.0, f)
    }
}

/// Human-readable formats carry the uuid's own spelling; binary ones keep the bare
/// 16-byte array, as [`Lsn`](crate::lsn::Lsn) does. Hand-written rather than derived
/// because `Uuid`'s own impl calls `serialize_bytes`, which bincode length-prefixes —
/// 24 bytes where this writes 16. Nothing in amp bincodes a branch id today, so that
/// is a layout this preserves rather than one it protects: the safekeeper's
/// `TimelinePersistentState` is stored as SQL columns, not a control file.
impl Serialize for BranchId {
    fn serialize<S>(&self, serializer: S) -> Result<S::Ok, S::Error>
    where
        S: serde::Serializer,
    {
        if serializer.is_human_readable() {
            serializer.collect_str(&self.0)
        } else {
            self.0.as_bytes().serialize(serializer)
        }
    }
}

impl<'de> Deserialize<'de> for BranchId {
    fn deserialize<D>(deserializer: D) -> Result<Self, D::Error>
    where
        D: serde::Deserializer<'de>,
    {
        struct BranchIdVisitor {
            is_human_readable_deserializer: bool,
        }

        impl<'de> Visitor<'de> for BranchIdVisitor {
            type Value = BranchId;

            fn expecting(&self, formatter: &mut fmt::Formatter) -> fmt::Result {
                if self.is_human_readable_deserializer {
                    formatter.write_str("value in form of uuid string")
                } else {
                    formatter.write_str("value in form of integer array([u8; 16])")
                }
            }

            fn visit_seq<A>(self, seq: A) -> Result<Self::Value, A::Error>
            where
                A: serde::de::SeqAccess<'de>,
            {
                let s = serde::de::value::SeqAccessDeserializer::new(seq);
                let id: [u8; 16] = Deserialize::deserialize(s)?;
                Ok(BranchId::from_array(id))
            }

            fn visit_str<E>(self, v: &str) -> Result<Self::Value, E>
            where
                E: serde::de::Error,
            {
                BranchId::from_str(v).map_err(E::custom)
            }
        }

        if deserializer.is_human_readable() {
            deserializer.deserialize_str(BranchIdVisitor {
                is_human_readable_deserializer: true,
            })
        } else {
            deserializer.deserialize_tuple(
                16,
                BranchIdVisitor {
                    is_human_readable_deserializer: false,
                },
            )
        }
    }
}

/// `branch_id` is `uuid` in every table that has one, so a `BranchId` binds and
/// reads back as itself: `&branch_id` in a params array, `row.try_get::<_,
/// BranchId>(i)` on the way out. A `String` there would compile against any
/// column and fail only when the statement runs.
impl ToSql for BranchId {
    fn to_sql(
        &self,
        ty: &Type,
        out: &mut bytes::BytesMut,
    ) -> Result<IsNull, Box<dyn std::error::Error + Sync + Send>> {
        self.0.to_sql(ty, out)
    }

    fn accepts(ty: &Type) -> bool {
        <Uuid as ToSql>::accepts(ty)
    }

    to_sql_checked!();
}

impl<'a> FromSql<'a> for BranchId {
    fn from_sql(
        ty: &Type,
        raw: &'a [u8],
    ) -> Result<Self, Box<dyn std::error::Error + Sync + Send>> {
        Ok(BranchId(Uuid::from_sql(ty, raw)?))
    }

    fn accepts(ty: &Type) -> bool {
        <Uuid as FromSql>::accepts(ty)
    }
}

impl TryFrom<Option<&str>> for BranchId {
    type Error = anyhow::Error;

    fn try_from(value: Option<&str>) -> Result<Self, Self::Error> {
        value
            .unwrap_or_default()
            .parse::<BranchId>()
            .with_context(|| format!("Could not parse branch id from {value:?}"))
    }
}

/// If needed, reuse small string from proxy/src/types.rc
pub type EndpointId = String;

#[cfg(test)]
mod tests {
    use serde_assert::{Deserializer, Serializer, Token, Tokens};

    use super::*;
    use crate::bin_ser::BeSer;

    /// One spelling, and it is the uuid's own: what `Display` prints is what a
    /// `branch_id` column prints, and what `FromStr` takes back. A byte-order
    /// slip here would point every query at a row that does not exist.
    #[test]
    fn branch_id_renders_and_parses_as_its_uuid() {
        let id = BranchId::from_array([
            0x00, 0x11, 0x22, 0x33, 0x44, 0x55, 0x66, 0x77, 0x88, 0x99, 0xaa, 0xbb, 0xcc, 0xdd,
            0xee, 0xff,
        ]);
        assert_eq!(id.to_string(), "00112233-4455-6677-8899-aabbccddeeff");
        assert_eq!(
            id.to_bytes().as_ref(),
            Uuid::parse_str(&id.to_string()).unwrap().as_bytes()
        );
    }

    /// Every spelling a uuid can be written in parses, so an id pasted out of
    /// psql works wherever one off the wire does. This is what the flat-only
    /// `hex::from_hex` parser used to reject.
    #[test]
    fn branch_id_parses_every_uuid_spelling() {
        let want = BranchId::from_array([
            0x00, 0x11, 0x22, 0x33, 0x44, 0x55, 0x66, 0x77, 0x88, 0x99, 0xaa, 0xbb, 0xcc, 0xdd,
            0xee, 0xff,
        ]);
        for spelling in [
            "00112233-4455-6677-8899-aabbccddeeff",
            "00112233445566778899aabbccddeeff",
            "{00112233-4455-6677-8899-aabbccddeeff}",
            "urn:uuid:00112233-4455-6677-8899-aabbccddeeff",
        ] {
            assert_eq!(spelling.parse::<BranchId>().unwrap(), want, "{spelling}");
        }
        assert!("not-a-branch-id".parse::<BranchId>().is_err());
    }

    #[test]
    fn test_branch_id_serde_non_human_readable() {
        let original_id = BranchId::from_array([
            173, 80, 132, 115, 129, 226, 72, 254, 170, 201, 135, 108, 199, 26, 228, 24,
        ]);
        let expected_tokens = Tokens(vec![
            Token::Tuple { len: 16 },
            Token::U8(173),
            Token::U8(80),
            Token::U8(132),
            Token::U8(115),
            Token::U8(129),
            Token::U8(226),
            Token::U8(72),
            Token::U8(254),
            Token::U8(170),
            Token::U8(201),
            Token::U8(135),
            Token::U8(108),
            Token::U8(199),
            Token::U8(26),
            Token::U8(228),
            Token::U8(24),
            Token::TupleEnd,
        ]);

        let serializer = Serializer::builder().is_human_readable(false).build();
        let serialized_tokens = original_id.serialize(&serializer).unwrap();
        assert_eq!(serialized_tokens, expected_tokens);

        let mut deserializer = Deserializer::builder()
            .is_human_readable(false)
            .tokens(serialized_tokens)
            .build();
        let deserialized_id = BranchId::deserialize(&mut deserializer).unwrap();
        assert_eq!(deserialized_id, original_id);
    }

    #[test]
    fn test_branch_id_serde_human_readable() {
        let original_id = BranchId::from_array([
            173, 80, 132, 115, 129, 226, 72, 254, 170, 201, 135, 108, 199, 26, 228, 24,
        ]);
        let expected_tokens = Tokens(vec![Token::Str(String::from(
            "ad508473-81e2-48fe-aac9-876cc71ae418",
        ))]);

        let serializer = Serializer::builder().is_human_readable(true).build();
        let serialized_tokens = original_id.serialize(&serializer).unwrap();
        assert_eq!(serialized_tokens, expected_tokens);

        let mut deserializer = Deserializer::builder()
            .is_human_readable(true)
            .tokens(expected_tokens)
            .build();
        assert_eq!(
            BranchId::deserialize(&mut deserializer).unwrap(),
            original_id
        );
    }

    macro_rules! roundtrip_type {
        ($type:ty, $expected_bytes:expr) => {{
            let expected_bytes: [u8; 16] = $expected_bytes;
            let original_id = <$type>::from(expected_bytes);

            let ser_bytes = original_id.ser().unwrap();
            assert_eq!(ser_bytes, expected_bytes);

            let des_id = <$type>::des(&ser_bytes).unwrap();
            assert_eq!(des_id, original_id);
        }};
    }

    #[test]
    fn test_branch_id_bincode_serde() {
        let expected_bytes = [
            173, 80, 132, 115, 129, 226, 72, 254, 170, 201, 135, 108, 199, 26, 228, 24,
        ];

        roundtrip_type!(BranchId, expected_bytes);
    }
}
