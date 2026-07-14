//! Owned operation descriptions, resolved against a [`TableHandle`]'s
//! column metadata at build time and lowered to `PgsqlReadRequestPB` /
//! `PgsqlWriteRequestPB` protobufs at call time.

use bytes::Bytes;

use crate::error::YbError;
use crate::partition::{encode_hash_partition_key, pgsql_hash_code};
use crate::pb::yb::{
    PgsqlColRefPb, PgsqlColumnValuePb, PgsqlConditionPb, PgsqlExpressionPb, PgsqlReadRequestPb,
    PgsqlWriteRequestPb, QlClient, QlOperator, QlValuePb, pgsql_expression_pb, ql_value_pb,
    pgsql_write_request_pb::PgsqlStmtType,
};
use crate::rows::ValueType;
use crate::table::{ColumnKind, TableHandle};

/// A value bound into a read condition or write row.
#[derive(Debug, Clone, PartialEq, Eq)]
pub enum BindValue {
    Null,
    Bool(bool),
    I16(i16),
    I32(i32),
    I64(i64),
    Text(String),
    Bytes(Bytes),
}

impl BindValue {
    /// Lowers to the `QLValuePB` shape pggate binds (`string_value` for
    /// TEXT — a `bytes` field on the wire — `binary_value` for BYTEA,
    /// `int{16,32,64}_value` for the integer widths). `Null` is an empty
    /// `QLValuePB`.
    pub(crate) fn to_ql_value(&self) -> QlValuePb {
        use ql_value_pb::Value;
        let value = match self {
            BindValue::Null => None,
            BindValue::Bool(v) => Some(Value::BoolValue(*v)),
            BindValue::I16(v) => Some(Value::Int16Value(i32::from(*v))),
            BindValue::I32(v) => Some(Value::Int32Value(*v)),
            BindValue::I64(v) => Some(Value::Int64Value(*v)),
            BindValue::Text(v) => Some(Value::StringValue(v.as_bytes().to_vec())),
            BindValue::Bytes(v) => Some(Value::BinaryValue(v.to_vec())),
        };
        QlValuePb { value }
    }
}

fn expr_value(v: QlValuePb) -> PgsqlExpressionPb {
    PgsqlExpressionPb {
        expr: Some(pgsql_expression_pb::Expr::Value(v)),
    }
}

#[derive(Debug, Clone, Copy, PartialEq, Eq)]
pub enum CondOp {
    Eq,
    Lt,
    Le,
    Gt,
    Ge,
}

impl CondOp {
    fn to_ql_operator(self) -> QlOperator {
        match self {
            CondOp::Eq => QlOperator::QlOpEqual,
            CondOp::Lt => QlOperator::QlOpLessThan,
            CondOp::Le => QlOperator::QlOpLessThanEqual,
            CondOp::Gt => QlOperator::QlOpGreaterThan,
            CondOp::Ge => QlOperator::QlOpGreaterThanEqual,
        }
    }
}

/// A single-tablet clustered range read: equality on all hash columns,
/// bound conditions on range columns, returning `targets` in order.
#[derive(Debug, Clone)]
pub struct RangeRead {
    pub(crate) hash_values: Vec<BindValue>,
    pub(crate) conds: Vec<(i32, CondOp, BindValue)>,
    pub(crate) target_ids: Vec<i32>,
    pub(crate) target_types: Vec<ValueType>,
    pub(crate) forward: bool,
    pub(crate) limit: u64,
    pub(crate) read_time_ht: u64,
}

impl RangeRead {
    /// `hash_values` must cover every hash column, in schema order.
    /// Empty on a hash-partitioned table = full scan across tablets.
    pub fn new(table: &TableHandle, hash_values: Vec<BindValue>) -> Result<Self, YbError> {
        let n_hash = table.num_hash_columns();
        if !hash_values.is_empty() && hash_values.len() != n_hash {
            return Err(YbError::invalid(format!(
                "table {} has {n_hash} hash columns, got {} values",
                table.table_id,
                hash_values.len()
            )));
        }
        Ok(RangeRead {
            hash_values,
            conds: Vec::new(),
            target_ids: Vec::new(),
            target_types: Vec::new(),
            forward: true,
            limit: 0,
            read_time_ht: 0,
        })
    }

    /// Adds a condition on a range key column (becomes a DocDB scan bound).
    pub fn cond(
        mut self,
        table: &TableHandle,
        column: &str,
        op: CondOp,
        value: BindValue,
    ) -> Result<Self, YbError> {
        let col = table.col(column)?;
        if col.kind != ColumnKind::Range {
            return Err(YbError::invalid(format!(
                "column {column:?} is not a range key column"
            )));
        }
        self.conds.push((col.id, op, value));
        Ok(self)
    }

    /// Sets the returned columns; cells come back in this order.
    pub fn targets(mut self, table: &TableHandle, columns: &[&str]) -> Result<Self, YbError> {
        self.target_ids.clear();
        self.target_types.clear();
        for name in columns {
            let col = table.col(name)?;
            self.target_ids.push(col.id);
            self.target_types.push(col.value_type);
        }
        Ok(self)
    }

    /// Reverse scan (clustered order is `forward`); a PK clustered
    /// `lsn DESC` means *forward* already yields LSN-descending rows.
    pub fn descending(mut self) -> Self {
        self.forward = false;
        self
    }

    /// Rows per fetched page (server default when 0).
    pub fn limit(mut self, limit: u64) -> Self {
        self.limit = limit;
        self
    }

    /// Pins the read snapshot to a hybrid time from a prior page's
    /// `used_read_time_ht` (0 = server picks). Lowered to
    /// `PgPerformOptionsPB.read_time`.
    pub fn read_time_ht(mut self, ht: u64) -> Self {
        self.read_time_ht = ht;
        self
    }

    pub fn target_types(&self) -> &[ValueType] {
        &self.target_types
    }

    /// Lowers to a `PgsqlReadRequestPB`. Ops sent through
    /// `PgClientService.Perform` must carry their own routing:
    /// `hash_code` + 2-byte `partition_key` for hash-eq reads (the
    /// server only computes them for requests its own client layer
    /// builds — see `partition.rs`); full scans start with an empty
    /// partition key (first tablet). Range conditions become a
    /// `QL_OP_AND` condition tree.
    pub(crate) fn to_read_request(
        &self,
        table: &TableHandle,
        stmt_id: u64,
    ) -> Result<PgsqlReadRequestPb, YbError> {
        if self.target_ids.is_empty() {
            return Err(YbError::invalid("read has no target columns"));
        }
        let mut read = PgsqlReadRequestPb {
            client: Some(QlClient::YqlClientPgsql as i32),
            stmt_id: Some(stmt_id),
            table_id: Some(table.table_id.clone().into_bytes()),
            schema_version: Some(table.schema_version()),
            is_forward_scan: Some(self.forward),
            return_paging_state: Some(true),
            ..Default::default()
        };
        if self.limit > 0 {
            read.limit = Some(self.limit);
        }
        if !self.hash_values.is_empty() {
            let values: Vec<QlValuePb> = self.hash_values.iter().map(BindValue::to_ql_value).collect();
            let hash = pgsql_hash_code(&values)?;
            read.hash_code = Some(hash.into());
            read.partition_key = Some(encode_hash_partition_key(hash));
            for v in values {
                read.partition_column_values.push(expr_value(v));
            }
        }
        if !self.conds.is_empty() {
            let mut and = PgsqlConditionPb {
                op: Some(QlOperator::QlOpAnd as i32),
                operands: Vec::with_capacity(self.conds.len()),
            };
            for (column_id, op, value) in &self.conds {
                and.operands.push(PgsqlExpressionPb {
                    expr: Some(pgsql_expression_pb::Expr::Condition(PgsqlConditionPb {
                        op: Some(op.to_ql_operator() as i32),
                        operands: vec![
                            PgsqlExpressionPb {
                                expr: Some(pgsql_expression_pb::Expr::ColumnId(*column_id)),
                            },
                            expr_value(value.to_ql_value()),
                        ],
                    })),
                });
            }
            read.condition_expr = Some(PgsqlExpressionPb {
                expr: Some(pgsql_expression_pb::Expr::Condition(and)),
            });
        }
        for &id in &self.target_ids {
            read.targets.push(PgsqlExpressionPb {
                expr: Some(pgsql_expression_pb::Expr::ColumnId(id)),
            });
            read.col_refs.push(PgsqlColRefPb {
                column_id: Some(id),
                ..Default::default()
            });
        }
        Ok(read)
    }
}

/// One row of a batched upsert. `key_values` covers every key column
/// (hash then range) in schema order; `values` are (column name resolved
/// to id, value) pairs for non-key columns.
#[derive(Debug, Clone)]
pub struct UpsertRow {
    pub(crate) key_values: Vec<BindValue>,
    pub(crate) value_ids: Vec<i32>,
    pub(crate) values: Vec<BindValue>,
}

impl UpsertRow {
    pub fn new(
        table: &TableHandle,
        key_values: Vec<BindValue>,
        values: &[(&str, BindValue)],
    ) -> Result<Self, YbError> {
        let n_key = table.num_key_columns();
        if key_values.len() != n_key {
            return Err(YbError::invalid(format!(
                "table {} has {n_key} key columns, got {} values",
                table.table_id,
                key_values.len()
            )));
        }
        let mut value_ids = Vec::with_capacity(values.len());
        let mut owned = Vec::with_capacity(values.len());
        for (name, value) in values {
            let col = table.col(name)?;
            if col.kind != ColumnKind::Value {
                return Err(YbError::invalid(format!(
                    "column {name:?} is a key column, pass it in key_values"
                )));
            }
            value_ids.push(col.id);
            owned.push(value.clone());
        }
        Ok(UpsertRow {
            key_values,
            value_ids,
            values: owned,
        })
    }

    /// Lowers to a `PGSQL_UPSERT` write request, computing the routing
    /// `hash_code`/`partition_key` from the hash column values (see
    /// [`RangeRead::to_read_request`] for why the client must).
    pub(crate) fn to_write_request(
        &self,
        table: &TableHandle,
        stmt_id: u64,
    ) -> Result<PgsqlWriteRequestPb, YbError> {
        let n_hash = table.num_hash_columns();
        let mut write = PgsqlWriteRequestPb {
            client: Some(QlClient::YqlClientPgsql as i32),
            stmt_id: Some(stmt_id),
            stmt_type: Some(PgsqlStmtType::PgsqlUpsert as i32),
            table_id: Some(table.table_id.clone().into_bytes()),
            schema_version: Some(table.schema_version()),
            ..Default::default()
        };
        let key_values: Vec<QlValuePb> =
            self.key_values.iter().map(BindValue::to_ql_value).collect();
        if n_hash > 0 {
            let hash = pgsql_hash_code(&key_values[..n_hash])?;
            write.hash_code = Some(hash.into());
            write.partition_key = Some(encode_hash_partition_key(hash));
        }
        for (i, v) in key_values.into_iter().enumerate() {
            let expr = expr_value(v);
            if i < n_hash {
                write.partition_column_values.push(expr);
            } else {
                write.range_column_values.push(expr);
            }
        }
        for (column_id, v) in self.value_ids.iter().zip(&self.values) {
            write.column_values.push(PgsqlColumnValuePb {
                column_id: Some(*column_id),
                expr: Some(expr_value(v.to_ql_value())),
            });
        }
        Ok(write)
    }
}

#[cfg(test)]
mod tests {
    use super::*;
    use crate::table::{ColumnMeta, TableHandle};
    use std::sync::atomic::AtomicU32;

    fn test_table() -> TableHandle {
        let col = |name: &str, id: i32, kind: ColumnKind, value_type: ValueType| ColumnMeta {
            name: name.to_string(),
            id,
            kind,
            value_type,
        };
        TableHandle {
            table_id: crate::table::pgsql_table_id(1, 2),
            columns: vec![
                col("branch_id", 0, ColumnKind::Hash, ValueType::Text),
                col("lsn", 1, ColumnKind::Range, ValueType::I64),
                col("payload", 2, ColumnKind::Value, ValueType::Bytea),
            ],
            database_oid: 1,
            table_oid: 2,
            schema_version: AtomicU32::new(3),
        }
    }

    #[test]
    fn read_request_shape() {
        let table = test_table();
        let read = RangeRead::new(&table, vec![BindValue::Text("a".into())])
            .unwrap()
            .cond(&table, "lsn", CondOp::Le, BindValue::I64(30))
            .unwrap()
            .targets(&table, &["lsn", "payload"])
            .unwrap()
            .limit(7);
        let req = read.to_read_request(&table, 42).unwrap();
        assert_eq!(req.stmt_id, Some(42));
        assert_eq!(req.schema_version, Some(3));
        assert_eq!(req.limit, Some(7));
        assert_eq!(req.is_forward_scan, Some(true));
        assert_eq!(req.return_paging_state, Some(true));
        // Routing: hash code + 2-byte big-endian partition key.
        let hash = req.hash_code.unwrap();
        assert_eq!(
            req.partition_key.as_deref().unwrap(),
            (hash as u16).to_be_bytes()
        );
        assert_eq!(req.partition_column_values.len(), 1);
        // Condition tree: AND( LE(col 1, 30) ).
        let and = match req.condition_expr.unwrap().expr.unwrap() {
            pgsql_expression_pb::Expr::Condition(c) => c,
            other => panic!("expected condition, got {other:?}"),
        };
        assert_eq!(and.op, Some(QlOperator::QlOpAnd as i32));
        assert_eq!(and.operands.len(), 1);
        assert_eq!(req.targets.len(), 2);
        assert_eq!(req.col_refs.len(), 2);
        assert_eq!(read.target_types(), &[ValueType::I64, ValueType::Bytea]);
    }

    #[test]
    fn read_requires_full_hash_cover_or_none() {
        let table = test_table();
        assert!(RangeRead::new(&table, vec![]).is_ok()); // full scan
        assert!(RangeRead::new(&table, vec![BindValue::Text("a".into())]).is_ok());
        // cond on a non-range column is rejected
        let r = RangeRead::new(&table, vec![BindValue::Text("a".into())])
            .unwrap()
            .cond(&table, "payload", CondOp::Eq, BindValue::Null);
        assert!(r.is_err());
        // read without targets is rejected at lowering time
        let r = RangeRead::new(&table, vec![BindValue::Text("a".into())]).unwrap();
        assert!(r.to_read_request(&table, 1).is_err());
    }

    #[test]
    fn write_request_shape() {
        let table = test_table();
        let row = UpsertRow::new(
            &table,
            vec![BindValue::Text("a".into()), BindValue::I64(5)],
            &[("payload", BindValue::Bytes(Bytes::from_static(b"x")))],
        )
        .unwrap();
        let req = row.to_write_request(&table, 7).unwrap();
        assert_eq!(req.stmt_type, Some(PgsqlStmtType::PgsqlUpsert as i32));
        assert_eq!(req.partition_column_values.len(), 1);
        assert_eq!(req.range_column_values.len(), 1);
        assert_eq!(req.column_values.len(), 1);
        assert_eq!(req.column_values[0].column_id, Some(2));
        let hash = req.hash_code.unwrap();
        assert_eq!(
            req.partition_key.as_deref().unwrap(),
            (hash as u16).to_be_bytes()
        );

        // Key count is validated.
        assert!(UpsertRow::new(&table, vec![BindValue::Text("a".into())], &[]).is_err());
        // Key columns can't be passed as values.
        assert!(
            UpsertRow::new(
                &table,
                vec![BindValue::Text("a".into()), BindValue::I64(5)],
                &[("lsn", BindValue::I64(6))],
            )
            .is_err()
        );
    }

    /// A two-sided range (`lsn >= 10 AND lsn < 20`) on a descending scan,
    /// projecting only `payload` — the predicate is on a key column that
    /// isn't in the projection, which must still bind.
    #[test]
    fn read_bounded_range_descending_predicate_not_projected() {
        let table = test_table();
        let req = RangeRead::new(&table, vec![BindValue::Text("a".into())])
            .unwrap()
            .cond(&table, "lsn", CondOp::Ge, BindValue::I64(10))
            .unwrap()
            .cond(&table, "lsn", CondOp::Lt, BindValue::I64(20))
            .unwrap()
            .targets(&table, &["payload"])
            .unwrap()
            .descending()
            .to_read_request(&table, 1)
            .unwrap();
        assert_eq!(req.is_forward_scan, Some(false));
        assert_eq!(req.targets.len(), 1); // only payload projected
        let and = match req.condition_expr.unwrap().expr.unwrap() {
            pgsql_expression_pb::Expr::Condition(c) => c,
            other => panic!("expected condition, got {other:?}"),
        };
        assert_eq!(and.op, Some(QlOperator::QlOpAnd as i32));
        assert_eq!(and.operands.len(), 2); // both range bounds bound
    }

    /// No hash values = full scan across tablets: no routing hints emitted,
    /// so the read starts at the first tablet (empty partition key).
    #[test]
    fn read_full_scan_has_no_partition_routing() {
        let table = test_table();
        let req = RangeRead::new(&table, vec![])
            .unwrap()
            .targets(&table, &["lsn", "payload"])
            .unwrap()
            .to_read_request(&table, 1)
            .unwrap();
        assert_eq!(req.hash_code, None);
        assert_eq!(req.partition_key, None);
        assert!(req.partition_column_values.is_empty());
    }

    #[test]
    fn read_rejects_wrong_hash_count() {
        let table = test_table(); // one hash column
        assert!(
            RangeRead::new(
                &table,
                vec![BindValue::Text("a".into()), BindValue::I64(2)],
            )
            .is_err()
        );
    }
}
