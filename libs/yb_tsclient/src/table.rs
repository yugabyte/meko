//! Opened-table metadata.

use std::sync::atomic::{AtomicU32, Ordering};

use crate::error::YbError;
use crate::pb::yb::{ColumnSchemaPb, PersistentDataType};
use crate::rows::ValueType;

#[derive(Debug, Clone, Copy, PartialEq, Eq)]
pub enum ColumnKind {
    Hash,
    Range,
    Value,
}

#[derive(Debug, Clone)]
pub struct ColumnMeta {
    pub name: String,
    pub id: i32,
    pub kind: ColumnKind,
    /// Decode type, derived from the DocDB column type. Only the types the
    /// yb-amp tables use are supported.
    pub value_type: ValueType,
}

/// DocDB table id for a YSQL relation: 16-byte UUID-v3-shaped id with
/// the database oid in bytes 0..4 and the table oid (relfilenode) in
/// bytes 12..16 (`GetPgsqlTableId` in `yb/common/entity_ids.cc`; the
/// middle is fixed to `0000 3000 8000 0000` for user tables).
pub fn pgsql_table_id(database_oid: u32, table_oid: u32) -> String {
    format!("{database_oid:08x}0000300080000000{table_oid:08x}")
}

/// An opened YSQL table: DocDB table id, schema version, and column
/// metadata (in schema order: hash keys, range keys, then value columns).
///
/// Purely client-side metadata — Perform ops carry the `table_id` and
/// `schema_version` themselves; there is no server-side handle to close.
/// The owning [`crate::TsClient`] caches handles in a weak registry.
///
/// The hot tables (`pages`, `slru_pages`, `meta`, `wals`) are provisioned
/// once by the migration and never altered, so `schema_version` is
/// immutable: a cached handle is reused across session reopen/recovery
/// without re-running `OpenTable`, and is never refreshed. A runtime
/// `SchemaMismatch` therefore is not expected on the hot path; if the
/// schema ever did change it would need a redeploy of the hot-path
/// daemons (there is deliberately no YSQL fallback to mask it).
pub struct TableHandle {
    pub table_id: String,
    pub columns: Vec<ColumnMeta>,
    // The originating YSQL OIDs. Kept for identity/diagnostics; no longer
    // read on the hot paths since neither recovery nor idle-reopen re-runs
    // OpenTable (the hot tables' schema is immutable).
    #[allow(dead_code)]
    pub(crate) database_oid: u32,
    #[allow(dead_code)]
    pub(crate) table_oid: u32,
    /// The table's schema version, sent on every op. For the hot tables
    /// this is immutable (they are created once and never altered), which
    /// is what lets a session reopen/recovery skip OpenTable and keep
    /// using this cached handle. The field stays atomic for the general
    /// case; the hot-path callers never see it change.
    pub(crate) schema_version: AtomicU32,
}

impl std::fmt::Debug for TableHandle {
    fn fmt(&self, f: &mut std::fmt::Formatter<'_>) -> std::fmt::Result {
        f.debug_struct("TableHandle")
            .field("table_id", &self.table_id)
            .field("schema_version", &self.schema_version())
            .field("columns", &self.columns)
            .finish()
    }
}

impl TableHandle {
    pub fn schema_version(&self) -> u32 {
        self.schema_version.load(Ordering::Relaxed)
    }

    pub fn col(&self, name: &str) -> Result<&ColumnMeta, YbError> {
        self.columns.iter().find(|c| c.name == name).ok_or_else(|| {
            YbError::invalid(format!("table {} has no column {name:?}", self.table_id))
        })
    }

    pub fn num_hash_columns(&self) -> usize {
        self.columns
            .iter()
            .filter(|c| c.kind == ColumnKind::Hash)
            .count()
    }

    pub fn num_key_columns(&self) -> usize {
        self.columns
            .iter()
            .filter(|c| c.kind != ColumnKind::Value)
            .count()
    }
}

pub(crate) fn value_type_for(
    data_type: PersistentDataType,
    column: &str,
) -> Result<ValueType, YbError> {
    match data_type {
        PersistentDataType::Int8 | PersistentDataType::Int16 => Ok(ValueType::I16),
        PersistentDataType::Int32 => Ok(ValueType::I32),
        PersistentDataType::Int64 => Ok(ValueType::I64),
        PersistentDataType::String => Ok(ValueType::Text),
        PersistentDataType::Bool => Ok(ValueType::Bool),
        PersistentDataType::Binary => Ok(ValueType::Bytea),
        other => Err(YbError::invalid(format!(
            "unsupported YB data type {other:?} on column {column:?}"
        ))),
    }
}

/// Builds column metadata from an OpenTable schema. Columns arrive in
/// schema order (hash keys, range keys, values); kinds come from the
/// `is_key`/`is_hash_key` flags.
pub(crate) fn columns_from_schema(columns: &[ColumnSchemaPb]) -> Result<Vec<ColumnMeta>, YbError> {
    columns
        .iter()
        .map(|c| {
            let id =
                c.id.ok_or_else(|| YbError::other(format!("column {:?} has no id", c.name)))?;
            let id = i32::try_from(id)
                .map_err(|_| YbError::other(format!("column {:?} id {id} overflows", c.name)))?;
            let kind = if c.is_hash_key() {
                ColumnKind::Hash
            } else if c.is_key() {
                ColumnKind::Range
            } else {
                ColumnKind::Value
            };
            // `type` is a required proto2 field; `main` is optional.
            let data_type = c
                .r#type
                .main
                .and_then(|m| PersistentDataType::try_from(m).ok())
                .ok_or_else(|| YbError::other(format!("column {:?} has no type", c.name)))?;
            Ok(ColumnMeta {
                name: c.name.clone(),
                id,
                kind,
                value_type: value_type_for(data_type, &c.name)?,
            })
        })
        .collect()
}

#[cfg(test)]
mod tests {
    use super::*;

    #[test]
    fn table_id_layout() {
        // yugabyte db oid 13248 / table oid 16384 (values seen live).
        assert_eq!(
            pgsql_table_id(0x33c0, 0x4000),
            "000033c0000030008000000000004000"
        );
        assert_eq!(pgsql_table_id(1, 2).len(), 32);
    }

    #[test]
    fn column_kinds_and_types() {
        let make =
            |name: &str, id: u32, hash: bool, key: bool, ty: PersistentDataType| ColumnSchemaPb {
                id: Some(id),
                name: name.to_string(),
                r#type: crate::pb::yb::QlTypePb {
                    main: Some(ty as i32),
                    ..Default::default()
                },
                is_key: Some(key),
                is_hash_key: Some(hash),
                ..Default::default()
            };
        let cols = columns_from_schema(&[
            make("h", 0, true, true, PersistentDataType::String),
            make("r", 1, false, true, PersistentDataType::Int64),
            make("v", 2, false, false, PersistentDataType::Binary),
        ])
        .unwrap();
        assert_eq!(cols[0].kind, ColumnKind::Hash);
        assert_eq!(cols[0].value_type, ValueType::Text);
        assert_eq!(cols[1].kind, ColumnKind::Range);
        assert_eq!(cols[1].value_type, ValueType::I64);
        assert_eq!(cols[2].kind, ColumnKind::Value);
        assert_eq!(cols[2].value_type, ValueType::Bytea);

        let float = columns_from_schema(&[make("f", 3, false, false, PersistentDataType::Float)]);
        assert!(float.is_err());
    }

    fn schema_col(
        name: &str,
        id: Option<u32>,
        hash: bool,
        key: bool,
        ty: Option<PersistentDataType>,
    ) -> ColumnSchemaPb {
        ColumnSchemaPb {
            id,
            name: name.to_string(),
            r#type: crate::pb::yb::QlTypePb {
                main: ty.map(|t| t as i32),
                ..Default::default()
            },
            is_key: Some(key),
            is_hash_key: Some(hash),
            ..Default::default()
        }
    }

    #[test]
    fn schema_rejects_missing_id_or_type() {
        assert!(
            columns_from_schema(&[schema_col(
                "h",
                None,
                true,
                true,
                Some(PersistentDataType::String)
            )])
            .is_err()
        );
        assert!(columns_from_schema(&[schema_col("h", Some(0), true, true, None)]).is_err());
    }

    #[test]
    fn int8_maps_to_i16() {
        let cols = columns_from_schema(&[schema_col(
            "s",
            Some(0),
            false,
            true,
            Some(PersistentDataType::Int8),
        )])
        .unwrap();
        assert_eq!(cols[0].value_type, ValueType::I16);
        assert_eq!(cols[0].kind, ColumnKind::Range); // is_key && !is_hash_key
    }

    #[test]
    fn key_counts_and_column_lookup() {
        let cols = columns_from_schema(&[
            schema_col("h1", Some(0), true, true, Some(PersistentDataType::String)),
            schema_col("h2", Some(1), true, true, Some(PersistentDataType::Int16)),
            schema_col("r", Some(2), false, true, Some(PersistentDataType::Int64)),
            schema_col("v", Some(3), false, false, Some(PersistentDataType::Binary)),
        ])
        .unwrap();
        let h = TableHandle {
            table_id: pgsql_table_id(1, 2),
            columns: cols,
            database_oid: 1,
            table_oid: 2,
            schema_version: AtomicU32::new(1),
        };
        assert_eq!(h.num_hash_columns(), 2);
        assert_eq!(h.num_key_columns(), 3); // 2 hash + 1 range
        assert_eq!(h.col("r").unwrap().id, 2);
        assert!(h.col("missing").is_err());
    }
}
