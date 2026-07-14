//! Client-side hash partitioning for PGSQL tables.
//!
//! The server computes partition keys only for requests its own client
//! layer builds (`own_request_` in `yb/client/yb_op.cc`); ops arriving
//! through `PgClientService.Perform` must already carry
//! `partition_key` (and `hash_code`) — pggate computes them before
//! sending. This mirrors that computation:
//!
//! - each hash column value is serialized with
//!   `AppendToKey`/`YBPartition::AppendIntToKey` semantics
//!   (`yb/common/ql_value.cc`): raw big-endian for integers/bool, raw
//!   bytes for string/binary — *not* the DocDB key encoding;
//! - the compound buffer is hashed with gutil's 64-bit Jenkins
//!   `Hash64StringWithSeed` (`yb/gutil/hash/jenkins.cc`) with seed 97
//!   and folded to 16 bits (`PartitionSchema::HashColumnCompoundValue`
//!   in `yb/dockv/partition.cc`);
//! - the partition key is the 16-bit hash, big-endian
//!   (`EncodeMultiColumnHashValue`).

use crate::error::YbError;
use crate::pb::yb::{QlValuePb, ql_value_pb};

/// 16-bit YB hash code for one row's hash column values (in schema
/// order). Mirrors `PartitionSchema::PgsqlHashColumnCompoundValue`;
/// equals YSQL's `yb_hash_code(...)` over the same values (the golden
/// vectors in the tests below were generated that way against a live
/// 2025.2.3.0 cluster).
pub fn pgsql_hash_code(hash_values: &[QlValuePb]) -> Result<u16, YbError> {
    let mut compound = Vec::new();
    for v in hash_values {
        append_to_key(v, &mut compound)?;
    }
    Ok(hash_column_compound_value(&compound))
}

/// `PartitionSchema::EncodeMultiColumnHashValue`: 2 bytes big-endian.
pub fn encode_hash_partition_key(hash_code: u16) -> Vec<u8> {
    hash_code.to_be_bytes().to_vec()
}

/// `DoAppendToKey` in `yb/common/ql_value.cc`, for the types our tables
/// use (bool, int widths, text, binary; extend as needed —
/// float/double/decimal hash keys are unsupported).
fn append_to_key(value: &QlValuePb, out: &mut Vec<u8>) -> Result<(), YbError> {
    use ql_value_pb::Value;
    match &value.value {
        Some(Value::BoolValue(b)) => out.push(u8::from(*b)),
        Some(Value::Int8Value(v)) => out.extend_from_slice(&(*v as i8).to_be_bytes()),
        Some(Value::Int16Value(v)) => out.extend_from_slice(&(*v as i16).to_be_bytes()),
        Some(Value::Int32Value(v)) => out.extend_from_slice(&v.to_be_bytes()),
        Some(Value::Int64Value(v)) => out.extend_from_slice(&v.to_be_bytes()),
        Some(Value::Uint32Value(v)) => out.extend_from_slice(&v.to_be_bytes()),
        Some(Value::Uint64Value(v)) => out.extend_from_slice(&v.to_be_bytes()),
        Some(Value::StringValue(s)) => out.extend_from_slice(s),
        Some(Value::BinaryValue(b)) => out.extend_from_slice(b),
        other => {
            return Err(YbError::invalid(format!(
                "unsupported hash key value {other:?}"
            )));
        }
    }
    Ok(())
}

/// `PartitionSchema::HashColumnCompoundValue`: 64-bit Jenkins hash with
/// seed 97, folded to 16 bits.
fn hash_column_compound_value(compound: &[u8]) -> u16 {
    let hv = jenkins_hash64_with_seed(compound, 97);
    let h1 = hv >> 48;
    let h2 = 3u64.wrapping_mul(hv >> 32);
    let h3 = 5u64.wrapping_mul(hv >> 16);
    let h4 = 7u64.wrapping_mul(hv & 0xffff);
    ((h1 ^ h2 ^ h3 ^ h4) & 0xffff) as u16
}

/// gutil `Hash64StringWithSeed` (Bob Jenkins' 64-bit hash).
fn jenkins_hash64_with_seed(s: &[u8], seed: u64) -> u64 {
    const GOLDEN: u64 = 0xe08c_1d66_8b75_6f82;
    let mut a = GOLDEN;
    let mut b = GOLDEN;
    let mut c = seed;

    let mut rest = s;
    while rest.len() >= 24 {
        a = a.wrapping_add(word64(rest, 0));
        b = b.wrapping_add(word64(rest, 8));
        c = c.wrapping_add(word64(rest, 16));
        mix(&mut a, &mut b, &mut c);
        rest = &rest[24..];
    }

    c = c.wrapping_add(s.len() as u64);
    // Tail bytes 16.. go into c (shifted <<8..<<56; c's low byte is
    // reserved for the length), 8..15 into b, 0..7 into a — exactly the
    // fall-through switch in jenkins.cc.
    if rest.len() >= 16 {
        for (i, &byte) in rest[16..].iter().enumerate() {
            c = c.wrapping_add((byte as u64) << (8 * (i + 1)));
        }
        a = a.wrapping_add(word64(rest, 0));
        b = b.wrapping_add(word64(rest, 8));
    } else {
        if rest.len() > 8 {
            for (i, &byte) in rest[8..].iter().enumerate() {
                b = b.wrapping_add((byte as u64) << (8 * i));
            }
        }
        if rest.len() >= 8 {
            a = a.wrapping_add(word64(rest, 0));
        } else {
            for (i, &byte) in rest.iter().enumerate() {
                a = a.wrapping_add((byte as u64) << (8 * i));
            }
        }
    }
    mix(&mut a, &mut b, &mut c);
    c
}

/// Unaligned little-endian 64-bit load (`Word64At`).
fn word64(s: &[u8], at: usize) -> u64 {
    u64::from_le_bytes(s[at..at + 8].try_into().unwrap())
}

/// gutil `mix()` 64-bit version (`jenkins_lookup2.h`).
fn mix(a: &mut u64, b: &mut u64, c: &mut u64) {
    *a = a.wrapping_sub(*b).wrapping_sub(*c) ^ (*c >> 43);
    *b = b.wrapping_sub(*c).wrapping_sub(*a) ^ (*a << 9);
    *c = c.wrapping_sub(*a).wrapping_sub(*b) ^ (*b >> 8);
    *a = a.wrapping_sub(*b).wrapping_sub(*c) ^ (*c >> 38);
    *b = b.wrapping_sub(*c).wrapping_sub(*a) ^ (*a << 23);
    *c = c.wrapping_sub(*a).wrapping_sub(*b) ^ (*b >> 5);
    *a = a.wrapping_sub(*b).wrapping_sub(*c) ^ (*c >> 35);
    *b = b.wrapping_sub(*c).wrapping_sub(*a) ^ (*a << 49);
    *c = c.wrapping_sub(*a).wrapping_sub(*b) ^ (*b >> 11);
    *a = a.wrapping_sub(*b).wrapping_sub(*c) ^ (*c >> 12);
    *b = b.wrapping_sub(*c).wrapping_sub(*a) ^ (*a << 18);
    *c = c.wrapping_sub(*a).wrapping_sub(*b) ^ (*b >> 22);
}

#[cfg(test)]
mod tests {
    use super::*;
    use ql_value_pb::Value;

    fn v(value: Value) -> QlValuePb {
        QlValuePb { value: Some(value) }
    }

    fn hash(values: &[QlValuePb]) -> u16 {
        pgsql_hash_code(values).unwrap()
    }

    /// Golden vectors generated against a live YugabyteDB 2025.2.3.0
    /// cluster via `SELECT yb_hash_code(<value>::<type>)` (YSQL port
    /// 5433). Our client-side hash must agree byte-for-byte, or writes
    /// route to the wrong tablet.
    #[test]
    fn golden_vectors_match_yb_hash_code() {
        // BIGINT
        assert_eq!(hash(&[v(Value::Int64Value(1))]), 60841);
        assert_eq!(hash(&[v(Value::Int64Value(42))]), 48975);
        assert_eq!(hash(&[v(Value::Int64Value(-7))]), 2137);
        assert_eq!(hash(&[v(Value::Int64Value(123456789012345))]), 37156);
        // INT / SMALLINT / BOOL
        assert_eq!(hash(&[v(Value::Int32Value(42))]), 21465);
        assert_eq!(hash(&[v(Value::Int16Value(42))]), 63260);
        assert_eq!(hash(&[v(Value::Int16Value(-1))]), 60647);
        assert_eq!(hash(&[v(Value::BoolValue(true))]), 36855);
        // TEXT
        assert_eq!(hash(&[v(Value::StringValue(b"".to_vec()))]), 19780);
        assert_eq!(hash(&[v(Value::StringValue(b"a".to_vec()))]), 44389);
        assert_eq!(hash(&[v(Value::StringValue(b"branch-42".to_vec()))]), 61859);
        assert_eq!(
            hash(&[v(Value::StringValue("é世".as_bytes().to_vec()))]),
            4745
        );
        // BYTEA
        assert_eq!(
            hash(&[v(Value::BinaryValue(vec![0xde, 0xad, 0xbe, 0xef]))]),
            54485
        );
        assert_eq!(hash(&[v(Value::BinaryValue(vec![0x00, 0xff, 0x10]))]), 64699);
        // Multi-column hash keys concatenate in schema order.
        assert_eq!(
            hash(&[
                v(Value::StringValue(b"a".to_vec())),
                v(Value::Int16Value(5)),
            ]),
            65337
        );
        assert_eq!(
            hash(&[
                v(Value::StringValue(b"branch".to_vec())),
                v(Value::Int16Value(3)),
                v(Value::Int64Value(9)),
            ]),
            37845
        );
    }

    /// Compound lengths straddling the Jenkins 24-byte block and the
    /// 8/16-byte tail branches (the fall-through switch in jenkins.cc).
    #[test]
    fn golden_vectors_jenkins_tail_branches() {
        let text = |s: &str| hash(&[v(Value::StringValue(s.as_bytes().to_vec()))]);
        assert_eq!(text("abcdefgh"), 20601); // exactly 8-byte tail
        assert_eq!(text("abcdefghijklmnopq"), 35523); // 17 bytes: >16 tail
        assert_eq!(text("abcdefghijklmnopqrstuvwx"), 24810); // one full block
        assert_eq!(text("abcdefghijklmnopqrstuvwxy"), 30460); // block + 1
    }

    #[test]
    fn partition_key_is_big_endian_hash() {
        assert_eq!(encode_hash_partition_key(0x1234), vec![0x12, 0x34]);
        assert_eq!(encode_hash_partition_key(0), vec![0, 0]);
        assert_eq!(encode_hash_partition_key(u16::MAX), vec![0xff, 0xff]);
    }

    #[test]
    fn unsupported_hash_types_are_invalid() {
        let err = pgsql_hash_code(&[v(Value::FloatValue(1.5))]).unwrap_err();
        assert_eq!(err.class(), crate::ErrorClass::Invalid);
        assert!(pgsql_hash_code(&[QlValuePb { value: None }]).is_err());
    }
}
