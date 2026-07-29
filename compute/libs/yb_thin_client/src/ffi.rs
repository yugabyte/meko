//! Hand-written `extern "C"` declarations mirroring `shim/yb_thin_client.h`.
//!
//! This is the entire unsafe surface of the crate. Everything above it
//! (a safe `TsClient` in `client.rs`) is thin: it marshals borrowed
//! slices into these POD structs, bridges the async callbacks to tokio
//! oneshots, and maps [`YbStatusCode`] onto the crate's error taxonomy.
//! No RPC framing, session, routing, or request building lives in Rust —
//! that is all behind this boundary in the C++ shim.
#![allow(non_camel_case_types, dead_code)] // FFI enums: variants come from C via repr(C)

use std::os::raw::{c_char, c_int};

#[repr(C)]
pub struct ybthin_client {
    _private: [u8; 0],
}

#[repr(C)]
pub struct ybthin_table {
    _private: [u8; 0],
}

#[repr(C)]
#[derive(Clone, Copy, PartialEq, Eq, Debug)]
pub enum YbStatusCode {
    Ok = 0,
    Invalid = 1,
    Network = 2,
    TryAgain = 3,
    ReadRestart = 4,
    Schema = 5,
    Other = 6,
}

/// Owned `message` (free with `ybthin_string_free`), null when `code == Ok`.
#[repr(C)]
pub struct YbStatus {
    pub code: YbStatusCode,
    pub message: *mut c_char,
}

#[repr(C)]
pub struct YbTlsOpts {
    pub ca_cert_path: *const c_char,
    pub cert_path: *const c_char,
    pub key_path: *const c_char,
}

/// Session/connection pool sizing (see `ybthin_pool_opts` in `yb_thin_client.h`).
/// A field of 0 selects the shim's default for that field.
#[repr(C)]
pub struct YbPoolOpts {
    pub read_sessions: u32,
    pub write_sessions: u32,
    pub sessions_per_conn: u32,
}

#[repr(C)]
#[derive(Clone, Copy, PartialEq, Eq, Debug)]
pub enum YbColKind {
    Hash = 0,
    Range = 1,
    Value = 2,
}

#[repr(C)]
#[derive(Clone, Copy, PartialEq, Eq, Debug)]
pub enum YbValueType {
    Bool = 0,
    I16 = 1,
    I32 = 2,
    I64 = 3,
    Text = 4,
    Bytea = 5,
}

#[repr(C)]
pub struct YbColumn {
    pub name: *const c_char,
    pub id: i32,
    pub kind: YbColKind,
    pub value_type: YbValueType,
}

#[repr(C)]
pub struct YbTableInfo {
    pub columns: *mut YbColumn,
    pub n_columns: usize,
}

#[repr(C)]
#[derive(Clone, Copy, PartialEq, Eq, Debug)]
pub enum YbBindTag {
    Null = 0,
    Bool = 1,
    I16 = 2,
    I32 = 3,
    I64 = 4,
    Text = 5,
    Bytea = 6,
}

#[repr(C)]
pub struct YbBind {
    pub tag: YbBindTag,
    pub i: i64,
    pub bytes: *const u8,
    pub bytes_len: usize,
}

#[repr(C)]
#[derive(Clone, Copy, PartialEq, Eq, Debug)]
pub enum YbCondOp {
    Eq = 0,
    Le = 1,
    Ge = 2,
    Lt = 3,
    Gt = 4,
}

#[repr(C)]
pub struct YbCond {
    pub column_id: i32,
    pub op: YbCondOp,
    pub value: YbBind,
}

#[repr(C)]
pub struct YbReadSpec {
    pub hash_values: *const YbBind,
    pub n_hash: usize,
    pub conds: *const YbCond,
    pub n_conds: usize,
    pub target_ids: *const i32,
    pub n_targets: usize,
    pub limit: u64,
    pub is_forward_scan: c_int,
    pub range_values: *const YbBind,
    pub n_range: usize,
}

/// A decoded read cell (mirrors [`YbBind`], the write side). `Null` is a
/// tag; `Bool`/`I16`/`I32`/`I64` read `i`; `Text`/`Bytea` read
/// `(bytes, bytes_len)`. `bytes` point into result-owned storage, valid
/// until `ybthin_read_result_free`.
#[repr(C)]
pub struct YbCell {
    pub tag: YbBindTag,
    pub i: i64,
    pub bytes: *const u8,
    pub bytes_len: usize,
}

/// One read op of a batch: which table, what to read, and (for a
/// continuation) the prior page's paging state.
#[repr(C)]
pub struct YbReadOp {
    pub table: *mut ybthin_table,
    pub spec: YbReadSpec,
    pub paging_state_in: *const u8,
    pub paging_state_in_len: usize,
}

/// One op's slice of a batch result: a row-major array of `n_rows * n_cols`
/// typed cells (row `r`, column `c` is `cells[r * n_cols + c]`, with
/// `n_cols == YbReadSpec.n_targets`), plus this op's paging state.
#[repr(C)]
pub struct YbReadOpResult {
    pub cells: *mut YbCell,
    pub n_rows: usize,
    pub n_cols: usize,
    pub paging_state: *mut u8,
    pub paging_state_len: usize,
}

/// The result of one read batch: `results[i]` corresponds to `ops[i]`;
/// `used_read_time_ht` is the one snapshot the whole batch ran at.
#[repr(C)]
pub struct YbReadResult {
    pub results: *mut YbReadOpResult,
    pub n_ops: usize,
    pub used_read_time_ht: u64,
}

#[repr(C)]
pub struct YbUpsertRow {
    pub table: *mut ybthin_table,
    pub key_values: *const YbBind,
    pub n_keys: usize,
    pub value_ids: *const i32,
    pub values: *const YbBind,
    pub n_values: usize,
}

pub type YbReadCb =
    extern "C" fn(ctx: *mut core::ffi::c_void, status: YbStatus, result: *mut YbReadResult);
pub type YbWriteCb = extern "C" fn(ctx: *mut core::ffi::c_void, status: YbStatus);

unsafe extern "C" {
    pub fn ybthin_client_create(
        tserver_addrs: *const *const c_char,
        n_addrs: usize,
        tls: *const YbTlsOpts,
        pool: *const YbPoolOpts,
        rpc_timeout_ms: u32,
        num_reactors: u32,
        out: *mut *mut ybthin_client,
    ) -> YbStatus;
    pub fn ybthin_client_destroy(client: *mut ybthin_client);

    pub fn ybthin_table_open(
        client: *mut ybthin_client,
        db_oid: u32,
        table_oid: u32,
        out: *mut *mut ybthin_table,
        info_out: *mut YbTableInfo,
    ) -> YbStatus;
    pub fn ybthin_table_close(table: *mut ybthin_table);
    pub fn ybthin_columns_free(columns: *mut YbColumn, n: usize);

    pub fn ybthin_read_async(
        client: *mut ybthin_client,
        ops: *const YbReadOp,
        n_ops: usize,
        read_time_ht: u64,
        cb: YbReadCb,
        ctx: *mut core::ffi::c_void,
    );
    pub fn ybthin_read_result_free(result: *mut YbReadResult);

    pub fn ybthin_upsert_batch_async(
        client: *mut ybthin_client,
        rows: *const YbUpsertRow,
        n_rows: usize,
        cb: YbWriteCb,
        ctx: *mut core::ffi::c_void,
    );

    pub fn ybthin_string_free(s: *mut c_char);
}
