use std::fmt;
use std::str::FromStr;

use anyhow::Context;
use bytes::Bytes;
use hex::FromHex;
use rand::Rng;
use serde::de::Visitor;
use serde::{Deserialize, Serialize};
use thiserror::Error;
use uuid::Uuid;
use ysql_client::tokio_postgres::types::{FromSql, IsNull, ToSql, Type, to_sql_checked};

#[derive(Error, Debug)]
pub enum IdError {
    #[error("invalid id length {0}")]
    SliceParseError(usize),
}

/// Neon ID is a 128-bit random ID.
/// Used to represent various identifiers. Provides handy utility methods and impls.
///
/// NOTE: It (de)serializes as an array of hex bytes, so the string representation would look
/// like `[173,80,132,115,129,226,72,254,170,201,135,108,199,26,228,24]`.
#[derive(Clone, Copy, PartialEq, Eq, Hash, PartialOrd, Ord)]
struct Id([u8; 16]);

impl Serialize for Id {
    fn serialize<S>(&self, serializer: S) -> Result<S::Ok, S::Error>
    where
        S: serde::Serializer,
    {
        if serializer.is_human_readable() {
            serializer.collect_str(self)
        } else {
            self.0.serialize(serializer)
        }
    }
}

impl<'de> Deserialize<'de> for Id {
    fn deserialize<D>(deserializer: D) -> Result<Self, D::Error>
    where
        D: serde::Deserializer<'de>,
    {
        struct IdVisitor {
            is_human_readable_deserializer: bool,
        }

        impl<'de> Visitor<'de> for IdVisitor {
            type Value = Id;

            fn expecting(&self, formatter: &mut fmt::Formatter) -> fmt::Result {
                if self.is_human_readable_deserializer {
                    formatter.write_str("value in form of hex string")
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
                Ok(Id::from(id))
            }

            fn visit_str<E>(self, v: &str) -> Result<Self::Value, E>
            where
                E: serde::de::Error,
            {
                Id::from_str(v).map_err(E::custom)
            }
        }

        if deserializer.is_human_readable() {
            deserializer.deserialize_str(IdVisitor {
                is_human_readable_deserializer: true,
            })
        } else {
            deserializer.deserialize_tuple(
                16,
                IdVisitor {
                    is_human_readable_deserializer: false,
                },
            )
        }
    }
}

impl Id {
    pub fn from_slice(src: &[u8]) -> Result<Id, IdError> {
        if src.len() != 16 {
            return Err(IdError::SliceParseError(src.len()));
        }
        let mut id_array = [0u8; 16];
        id_array.copy_from_slice(src);
        Ok(id_array.into())
    }

    pub fn as_arr(&self) -> [u8; 16] {
        self.0
    }

    pub fn generate() -> Self {
        let mut tli_buf = [0u8; 16];
        rand::rng().fill(&mut tli_buf);
        Id::from(tli_buf)
    }

    fn hex_encode(&self) -> String {
        static HEX: &[u8] = b"0123456789abcdef";

        let mut buf = vec![0u8; self.0.len() * 2];
        for (&b, chunk) in self.0.as_ref().iter().zip(buf.chunks_exact_mut(2)) {
            chunk[0] = HEX[((b >> 4) & 0xf) as usize];
            chunk[1] = HEX[(b & 0xf) as usize];
        }

        // SAFETY: vec constructed out of `HEX`, it can only be ascii
        unsafe { String::from_utf8_unchecked(buf) }
    }
}

impl FromStr for Id {
    type Err = hex::FromHexError;

    fn from_str(s: &str) -> Result<Id, Self::Err> {
        Self::from_hex(s)
    }
}

// this is needed for pretty serialization and deserialization of Id's using serde integration with hex crate
impl FromHex for Id {
    type Error = hex::FromHexError;

    fn from_hex<T: AsRef<[u8]>>(hex: T) -> Result<Self, Self::Error> {
        let mut buf: [u8; 16] = [0u8; 16];
        hex::decode_to_slice(hex, &mut buf)?;
        Ok(Id(buf))
    }
}

impl AsRef<[u8]> for Id {
    fn as_ref(&self) -> &[u8] {
        &self.0
    }
}

impl From<[u8; 16]> for Id {
    fn from(b: [u8; 16]) -> Self {
        Id(b)
    }
}

impl From<Id> for u128 {
    fn from(id: Id) -> Self {
        u128::from_le_bytes(id.0)
    }
}

impl fmt::Display for Id {
    fn fmt(&self, f: &mut fmt::Formatter<'_>) -> fmt::Result {
        f.write_str(&self.hex_encode())
    }
}

impl fmt::Debug for Id {
    fn fmt(&self, f: &mut fmt::Formatter<'_>) -> fmt::Result {
        f.write_str(&self.hex_encode())
    }
}

macro_rules! id_newtype {
    ($t:ident) => {
        impl $t {
            pub fn from_slice(src: &[u8]) -> Result<$t, IdError> {
                Ok($t(Id::from_slice(src)?))
            }

            pub fn as_arr(&self) -> [u8; 16] {
                self.0.as_arr()
            }

            pub fn generate() -> Self {
                $t(Id::generate())
            }

            pub const fn from_array(b: [u8; 16]) -> Self {
                $t(Id(b))
            }
        }

        impl FromStr for $t {
            type Err = hex::FromHexError;

            fn from_str(s: &str) -> Result<$t, Self::Err> {
                let value = Id::from_str(s)?;
                Ok($t(value))
            }
        }

        impl From<[u8; 16]> for $t {
            fn from(b: [u8; 16]) -> Self {
                $t(Id::from(b))
            }
        }

        impl FromHex for $t {
            type Error = hex::FromHexError;

            fn from_hex<T: AsRef<[u8]>>(hex: T) -> Result<Self, Self::Error> {
                Ok($t(Id::from_hex(hex)?))
            }
        }

        impl AsRef<[u8]> for $t {
            fn as_ref(&self) -> &[u8] {
                &self.0.0
            }
        }

        impl From<$t> for u128 {
            fn from(id: $t) -> Self {
                u128::from(id.0)
            }
        }

        impl fmt::Display for $t {
            fn fmt(&self, f: &mut fmt::Formatter<'_>) -> fmt::Result {
                self.0.fmt(f)
            }
        }

        impl fmt::Debug for $t {
            fn fmt(&self, f: &mut fmt::Formatter<'_>) -> fmt::Result {
                self.0.fmt(f)
            }
        }
    };
}

/// yb-amp branch ID. Globally unique 128-bit identifier of a branch
/// (one branch == one history of a cluster). Formerly `TimelineId`;
/// the storage model is now flat (one branch, no enclosing tenant),
/// so this single id identifies a branch end to end.
///
/// They are different from PostgreSQL timeline
/// IDs, but serve a similar purpose: they differentiate
/// between different "histories" of the same cluster.  However,
/// PostgreSQL timeline IDs are a bit cumbersome, because they are only
/// 32-bits wide, and they must be in ascending order in any given
/// timeline history.  Those limitations mean that we cannot generate a
/// new PostgreSQL timeline ID by just generating a random number. And
/// that in turn is problematic for the "pull/push" workflow, where you
/// have a local copy of a Neon repository, and you periodically sync
/// the local changes with a remote server. When you work "detached"
/// from the remote server, you cannot create a PostgreSQL timeline ID
/// that's guaranteed to be different from all existing timelines in
/// the remote server. For example, if two people are having a clone of
/// the repository on their laptops, and they both create a new branch
/// with different name. What timeline ID would they assign to their
/// branches? If they pick the same one, and later try to push the
/// branches to the same remote server, they will get mixed up.
///
/// To avoid those issues, Neon has its own concept of timelines that
/// is separate from PostgreSQL timelines, and doesn't have those
/// limitations. A Neon timeline is identified by a 128-bit ID, which
/// is usually printed out as a hex string.
///
/// NOTE: It (de)serializes as an array of hex bytes, so the string representation would look
/// like `[173,80,132,115,129,226,72,254,170,201,135,108,199,26,228,24]`.
/// See [`Id`] for alternative ways to serialize it.
#[derive(Clone, Copy, PartialEq, Eq, Hash, Ord, PartialOrd, Serialize, Deserialize)]
pub struct BranchId(Id);

id_newtype!(BranchId);

/// `branch_id` is `uuid` in every table that has one, so a `BranchId` binds and
/// reads back as itself: `&branch_id` in a params array, `row.try_get::<_,
/// BranchId>(i)` on the way out. That is the point of these impls — a rendering
/// step at each call site is a step each site can get wrong, and a `String` there
/// compiles against any column and fails only when the statement runs.
///
/// The thin-client lane is not Postgres and keeps binding the raw bytes
/// ([`BranchId::to_bytes`]).
impl ToSql for BranchId {
    fn to_sql(
        &self,
        ty: &Type,
        out: &mut bytes::BytesMut,
    ) -> Result<IsNull, Box<dyn std::error::Error + Sync + Send>> {
        self.to_uuid().to_sql(ty, out)
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
        Ok(Self::from_uuid(Uuid::from_sql(ty, raw)?))
    }

    fn accepts(ty: &Type) -> bool {
        <Uuid as FromSql>::accepts(ty)
    }
}

impl BranchId {
    /// The id as the `uuid` every `branch_id` column holds — `amp_registry`
    /// and `amp_storage` alike. Prefer binding the `BranchId` itself; this is for
    /// the places that need the uuid on its own.
    pub fn to_uuid(self) -> Uuid {
        Uuid::from_bytes(self.as_arr())
    }

    /// Inverse of [`Self::to_uuid`]. Total — every 16-byte value is a
    /// BranchId — so reading one back cannot fail.
    pub fn from_uuid(id: Uuid) -> Self {
        Self::from(*id.as_bytes())
    }

    /// The same 16 bytes, for the thin-client lane: the shim reports a
    /// column's bind type from its DocDB type, and DocDB types `uuid`
    /// exactly as `bytea` (binary), so a `uuid` column binds as bytes.
    pub fn to_bytes(self) -> Bytes {
        Bytes::copy_from_slice(&self.as_arr())
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

    /// The flat 32-hex spelling (`Display` — what the API, the daemons' args
    /// and `BRANCH_ID` carry) and the `uuid` every `branch_id` column holds are
    /// the same 16 bytes in the same order. Everything in the storage layer
    /// addresses a branch through one or the other, so a byte-order slip here
    /// would point every query at a row that does not exist.
    #[test]
    fn branch_id_spellings_agree() {
        let id = BranchId::from_array([
            0x00, 0x11, 0x22, 0x33, 0x44, 0x55, 0x66, 0x77, 0x88, 0x99, 0xaa, 0xbb, 0xcc, 0xdd,
            0xee, 0xff,
        ]);
        assert_eq!(id.to_string(), "00112233445566778899aabbccddeeff");
        assert_eq!(id.to_uuid().simple().to_string(), id.to_string());
        assert_eq!(id.to_bytes().as_ref(), id.to_uuid().as_bytes());
        assert_eq!(BranchId::from_uuid(id.to_uuid()), id);
        // The uuid a column holds maps back to this id, whatever spelling it was
        // written or printed in.
        assert_eq!(
            BranchId::from_uuid(Uuid::parse_str("00112233-4455-6677-8899-aabbccddeeff").unwrap()),
            id
        );
    }

    #[test]
    fn test_id_serde_non_human_readable() {
        let original_id = Id([
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
        let deserialized_id = Id::deserialize(&mut deserializer).unwrap();
        assert_eq!(deserialized_id, original_id);
    }

    #[test]
    fn test_id_serde_human_readable() {
        let original_id = Id([
            173, 80, 132, 115, 129, 226, 72, 254, 170, 201, 135, 108, 199, 26, 228, 24,
        ]);
        let expected_tokens = Tokens(vec![Token::Str(String::from(
            "ad50847381e248feaac9876cc71ae418",
        ))]);

        let serializer = Serializer::builder().is_human_readable(true).build();
        let serialized_tokens = original_id.serialize(&serializer).unwrap();
        assert_eq!(serialized_tokens, expected_tokens);

        let mut deserializer = Deserializer::builder()
            .is_human_readable(true)
            .tokens(Tokens(vec![Token::Str(String::from(
                "ad50847381e248feaac9876cc71ae418",
            ))]))
            .build();
        assert_eq!(Id::deserialize(&mut deserializer).unwrap(), original_id);
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
    fn test_id_bincode_serde() {
        let expected_bytes = [
            173, 80, 132, 115, 129, 226, 72, 254, 170, 201, 135, 108, 199, 26, 228, 24,
        ];

        roundtrip_type!(Id, expected_bytes);
    }

    #[test]
    fn test_branch_id_bincode_serde() {
        let expected_bytes = [
            173, 80, 132, 115, 129, 226, 72, 254, 170, 201, 135, 108, 199, 26, 228, 24,
        ];

        roundtrip_type!(BranchId, expected_bytes);
    }
}
