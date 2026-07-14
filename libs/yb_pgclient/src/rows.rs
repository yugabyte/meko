//! Decoder for the YSQL row wire format returned in read sidecars, and
//! the value model it produces.
//!
//! Format (see yugabyte-db `src/yb/yql/pggate/util/pg_doc_data.cc`):
//! an i64 big-endian row count, then per row one cell per requested
//! target, each cell a 1-byte header (bit 0x01 = NULL) followed by the
//! value: BOOL as u8; INT2/4/8 as big-endian; TEXT as u64 BE length
//! (including the trailing NUL) + bytes + NUL; BYTEA as u64 BE length +
//! bytes.

use bytes::Bytes;

use crate::error::YbError;

#[derive(Debug, Clone, Copy, PartialEq, Eq)]
pub enum ValueType {
    Bool,
    I16,
    I32,
    I64,
    Text,
    Bytea,
}

#[derive(Debug, Clone, PartialEq, Eq)]
pub enum Value {
    Null,
    Bool(bool),
    I16(i16),
    I32(i32),
    I64(i64),
    /// TEXT column bytes, without the trailing NUL (UTF-8 in practice).
    Text(Bytes),
    Bytes(Bytes),
}

impl Value {
    pub fn as_i64(&self) -> Result<i64, YbError> {
        match self {
            Value::I64(v) => Ok(*v),
            other => Err(YbError::decode(format!("expected i64, got {other:?}"))),
        }
    }

    pub fn as_i32(&self) -> Result<i32, YbError> {
        match self {
            Value::I32(v) => Ok(*v),
            other => Err(YbError::decode(format!("expected i32, got {other:?}"))),
        }
    }

    pub fn as_i16(&self) -> Result<i16, YbError> {
        match self {
            Value::I16(v) => Ok(*v),
            other => Err(YbError::decode(format!("expected i16, got {other:?}"))),
        }
    }

    pub fn as_bool(&self) -> Result<bool, YbError> {
        match self {
            Value::Bool(v) => Ok(*v),
            other => Err(YbError::decode(format!("expected bool, got {other:?}"))),
        }
    }

    pub fn as_bytes(&self) -> Result<&Bytes, YbError> {
        match self {
            Value::Bytes(v) | Value::Text(v) => Ok(v),
            other => Err(YbError::decode(format!("expected bytes, got {other:?}"))),
        }
    }
}

pub type Row = Vec<Value>;

struct Cursor<'a> {
    buf: &'a Bytes,
    pos: usize,
}

impl<'a> Cursor<'a> {
    fn take(&mut self, n: usize) -> Result<&'a [u8], YbError> {
        let end = self.pos.checked_add(n).filter(|&e| e <= self.buf.len());
        let Some(end) = end else {
            return Err(YbError::decode(format!(
                "row data truncated: need {n} bytes at offset {} of {}",
                self.pos,
                self.buf.len()
            )));
        };
        let out = &self.buf[self.pos..end];
        self.pos = end;
        Ok(out)
    }

    fn take_u8(&mut self) -> Result<u8, YbError> {
        Ok(self.take(1)?[0])
    }

    fn take_u64(&mut self) -> Result<u64, YbError> {
        Ok(u64::from_be_bytes(self.take(8)?.try_into().unwrap()))
    }

    /// Zero-copy slice of the underlying `Bytes`.
    fn take_bytes(&mut self, n: usize) -> Result<Bytes, YbError> {
        let start = self.pos;
        self.take(n)?;
        Ok(self.buf.slice(start..start + n))
    }
}

fn decode_cell(cursor: &mut Cursor<'_>, ty: ValueType) -> Result<Value, YbError> {
    let header = cursor.take_u8()?;
    if header & 0x01 != 0 {
        return Ok(Value::Null);
    }
    Ok(match ty {
        ValueType::Bool => Value::Bool(cursor.take_u8()? != 0),
        ValueType::I16 => Value::I16(i16::from_be_bytes(cursor.take(2)?.try_into().unwrap())),
        ValueType::I32 => Value::I32(i32::from_be_bytes(cursor.take(4)?.try_into().unwrap())),
        ValueType::I64 => Value::I64(i64::from_be_bytes(cursor.take(8)?.try_into().unwrap())),
        ValueType::Text => {
            let len = usize::try_from(cursor.take_u64()?)
                .map_err(|_| YbError::decode("TEXT cell length overflows usize"))?;
            if len == 0 {
                return Err(YbError::decode("TEXT cell with zero length (missing NUL)"));
            }
            let bytes = cursor.take_bytes(len - 1)?;
            let nul = cursor.take_u8()?;
            if nul != 0 {
                return Err(YbError::decode("TEXT cell not NUL-terminated"));
            }
            Value::Text(bytes)
        }
        ValueType::Bytea => {
            let len = usize::try_from(cursor.take_u64()?)
                .map_err(|_| YbError::decode("BYTEA cell length overflows usize"))?;
            Value::Bytes(cursor.take_bytes(len)?)
        }
    })
}

/// Decodes a read sidecar into rows of `types.len()` cells each. `Text`/
/// `Bytes` values are zero-copy slices of `data`.
pub fn decode_rows(data: &Bytes, types: &[ValueType]) -> Result<Vec<Row>, YbError> {
    if data.is_empty() {
        return Ok(Vec::new());
    }
    let mut cursor = Cursor { buf: data, pos: 0 };
    let row_count = cursor.take_u64()? as i64;
    if row_count < 0 {
        return Err(YbError::decode(format!("negative row count {row_count}")));
    }
    // With an empty projection each row consumes zero bytes, so nothing in the
    // decode loop below bounds row_count against the buffer — a corrupt/hostile
    // count would spin the loop and allocate empty rows without limit. We never
    // issue targetless reads (to_read_request requires >=1 target), so any such
    // response is bogus; cap it. Non-empty projections are self-bounding:
    // decode_cell consumes >=1 byte per cell and errors once the buffer runs out.
    const MAX_EMPTY_PROJECTION_ROWS: i64 = 1 << 16;
    if types.is_empty() && row_count > MAX_EMPTY_PROJECTION_ROWS {
        return Err(YbError::decode(format!(
            "row count {row_count} for an empty projection exceeds cap {MAX_EMPTY_PROJECTION_ROWS}"
        )));
    }
    // Bound the pre-allocation by what the buffer could actually hold, so a
    // corrupt/hostile row_count can't drive a huge allocation before we ever
    // read the rows. Each row occupies at least one byte per target cell (the
    // header); empty projections have zero-byte rows, so floor per-row size at 1.
    let remaining = data.len() - cursor.pos;
    let min_row_size = types.len().max(1);
    let capacity = usize::try_from(row_count)
        .unwrap_or(usize::MAX)
        .min(remaining / min_row_size);
    let mut rows = Vec::with_capacity(capacity);
    for _ in 0..row_count {
        let mut row = Vec::with_capacity(types.len());
        for &ty in types {
            row.push(decode_cell(&mut cursor, ty)?);
        }
        rows.push(row);
    }
    if cursor.pos != data.len() {
        return Err(YbError::decode(format!(
            "trailing garbage: {} bytes after {} rows",
            data.len() - cursor.pos,
            row_count
        )));
    }
    Ok(rows)
}

#[cfg(test)]
mod tests {
    use super::*;

    fn encode(rows: &[Vec<Value>]) -> Bytes {
        let mut out = Vec::new();
        out.extend_from_slice(&(rows.len() as i64).to_be_bytes());
        for row in rows {
            for value in row {
                match value {
                    Value::Null => out.push(0x01),
                    Value::Bool(v) => {
                        out.push(0);
                        out.push(*v as u8);
                    }
                    Value::I16(v) => {
                        out.push(0);
                        out.extend_from_slice(&v.to_be_bytes());
                    }
                    Value::I32(v) => {
                        out.push(0);
                        out.extend_from_slice(&v.to_be_bytes());
                    }
                    Value::I64(v) => {
                        out.push(0);
                        out.extend_from_slice(&v.to_be_bytes());
                    }
                    Value::Text(v) => {
                        out.push(0);
                        out.extend_from_slice(&((v.len() + 1) as u64).to_be_bytes());
                        out.extend_from_slice(v);
                        out.push(0);
                    }
                    Value::Bytes(v) => {
                        out.push(0);
                        out.extend_from_slice(&(v.len() as u64).to_be_bytes());
                        out.extend_from_slice(v);
                    }
                }
            }
        }
        Bytes::from(out)
    }

    #[test]
    fn empty_sidecar_is_zero_rows() {
        assert!(
            decode_rows(&Bytes::new(), &[ValueType::I64])
                .unwrap()
                .is_empty()
        );
    }

    #[test]
    fn round_trip_all_types() {
        let types = [
            ValueType::Bool,
            ValueType::I16,
            ValueType::I32,
            ValueType::I64,
            ValueType::Text,
            ValueType::Bytea,
        ];
        let rows = vec![
            vec![
                Value::Bool(true),
                Value::I16(-5),
                Value::I32(1 << 20),
                Value::I64(i64::MIN),
                Value::Text(Bytes::from_static(b"branch-a")),
                Value::Bytes(Bytes::from_static(&[0u8, 1, 2, 255])),
            ],
            vec![
                Value::Null,
                Value::Null,
                Value::Null,
                Value::I64(42),
                Value::Text(Bytes::new()),
                Value::Bytes(Bytes::new()),
            ],
        ];
        let decoded = decode_rows(&encode(&rows), &types).unwrap();
        assert_eq!(decoded, rows);
    }

    #[test]
    fn golden_bytes_single_row() {
        // 1 row: (INT8 0x0102030405060708, BYTEA "hi") — hand-built per
        // pg_doc_data.cc: count, header 0x00, BE value, header, u64 len, bytes.
        let mut data = vec![0, 0, 0, 0, 0, 0, 0, 1];
        data.push(0);
        data.extend_from_slice(&[1, 2, 3, 4, 5, 6, 7, 8]);
        data.push(0);
        data.extend_from_slice(&[0, 0, 0, 0, 0, 0, 0, 2]);
        data.extend_from_slice(b"hi");
        let rows = decode_rows(&Bytes::from(data), &[ValueType::I64, ValueType::Bytea]).unwrap();
        assert_eq!(
            rows,
            vec![vec![
                Value::I64(0x0102030405060708),
                Value::Bytes(Bytes::from_static(b"hi")),
            ]]
        );
    }

    #[test]
    fn truncated_data_errors() {
        let mut data = vec![0, 0, 0, 0, 0, 0, 0, 1];
        data.push(0);
        data.extend_from_slice(&[1, 2, 3]); // i64 needs 8 bytes
        assert!(decode_rows(&Bytes::from(data), &[ValueType::I64]).is_err());
    }

    #[test]
    fn trailing_garbage_errors() {
        let mut data = vec![0u8; 8]; // zero rows
        data.push(7);
        assert!(decode_rows(&Bytes::from(data), &[ValueType::I64]).is_err());
    }

    /// Empty projection (no target columns): the server still returns a
    /// row count with no cell payload — the `COUNT(*)`-style read. Decode
    /// must yield that many zero-cell rows and consume the buffer exactly.
    #[test]
    fn empty_projection_yields_counted_empty_rows() {
        let decoded = decode_rows(&encode(&[vec![], vec![], vec![]]), &[]).unwrap();
        assert_eq!(decoded.len(), 3);
        assert!(decoded.iter().all(|row| row.is_empty()));
    }

    /// TEXT is length-prefixed (len includes the trailing NUL), not
    /// NUL-scanned, so an interior NUL byte round-trips intact.
    #[test]
    fn text_preserves_interior_nul() {
        let rows = vec![vec![Value::Text(Bytes::from_static(b"a\0b"))]];
        assert_eq!(decode_rows(&encode(&rows), &[ValueType::Text]).unwrap(), rows);
    }

    #[test]
    fn empty_projection_huge_row_count_errors() {
        // A targetless read with an absurd count must be rejected before it
        // can spin the decode loop (each empty row consumes no bytes).
        let data = ((1u64 << 16) + 1).to_be_bytes().to_vec();
        assert!(decode_rows(&Bytes::from(data), &[]).is_err());
    }

    #[test]
    fn text_zero_length_errors() {
        // header (not null) + u64 length 0 — no room for the trailing NUL.
        let mut data = vec![0, 0, 0, 0, 0, 0, 0, 1];
        data.push(0);
        data.extend_from_slice(&0u64.to_be_bytes());
        assert!(decode_rows(&Bytes::from(data), &[ValueType::Text]).is_err());
    }

    #[test]
    fn text_missing_nul_terminator_errors() {
        // len 3 = 2 bytes + terminator, but the terminator byte is non-zero.
        let mut data = vec![0, 0, 0, 0, 0, 0, 0, 1];
        data.push(0);
        data.extend_from_slice(&3u64.to_be_bytes());
        data.extend_from_slice(b"ab");
        data.push(0x01);
        assert!(decode_rows(&Bytes::from(data), &[ValueType::Text]).is_err());
    }

    #[test]
    fn negative_row_count_errors() {
        // count = 0xFFFF... reads back as a negative i64.
        assert!(decode_rows(&Bytes::from(vec![0xffu8; 8]), &[ValueType::I64]).is_err());
    }
}
