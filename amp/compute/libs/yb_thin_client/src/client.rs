//! Safe wrapper over the shim ABI. `connect`/`open_table` are synchronous
//! (they do a startup RPC — call them from `spawn_blocking` if that
//! matters); reads and writes are async, bridging the shim's reactor-thread
//! callbacks to a `oneshot`.
//!
//! The one read primitive is [`TsClient::read`]: a *batch* of [`ReadOp`]s
//! that the shim runs as the ops of a single `Perform` — one RPC, one
//! session, one read snapshot. Single reads are just a one-op batch;
//! [`TsClient::read_page`]/[`TsClient::read_scan`] are RPC-free sugar over
//! it. Writes are the same shape: [`TsClient::upsert`] applies a batch of
//! [`UpsertRow`]s (which may span tables) as one `Perform`. Minimising RPCs
//! is the point — callers that can batch should.

use std::ffi::{CString, c_void};
use std::sync::Arc;

use bytes::Bytes;
use tokio::sync::oneshot;

use crate::error::{self, YbError};
use crate::ffi;

// ---- public value types ------------------------------------------------

#[derive(Clone, Copy, PartialEq, Eq, Debug)]
pub enum ColKind {
    Hash,
    Range,
    Value,
}

#[derive(Clone, Copy, PartialEq, Eq, Debug)]
pub enum ValueType {
    Bool,
    I16,
    I32,
    I64,
    Text,
    Bytea,
    /// DocDB UINT32, the storage type of an `oid` column.
    U32,
}

#[derive(Clone, Debug)]
pub struct Column {
    pub name: String,
    pub id: i32,
    pub kind: ColKind,
    pub value_type: ValueType,
}

/// A value bound into a read condition or an upsert cell.
#[derive(Clone, Debug, PartialEq, Eq)]
pub enum BindValue {
    Null,
    Bool(bool),
    I16(i16),
    I32(i32),
    I64(i64),
    Text(String),
    Bytes(Bytes),
    /// An `oid` column's value.
    U32(u32),
}

impl BindValue {
    /// Refuse a bind whose type is not `col`'s.
    ///
    /// DocDB encodes a key entry from the bound value's wire type and the
    /// tserver does not check it against the column, so an `I64` bound to an
    /// `INTEGER` key column builds a key no YSQL-written row has: reads miss
    /// silently, and an upsert lands a row that every later scan of the table
    /// trips over. Every builder that resolves a column runs this, so
    /// a column-width change fails the first request that binds the old width
    /// instead of corrupting the table. `Null` fits any value column, and no
    /// key column.
    fn check_fits(&self, col: &Column) -> Result<(), YbError> {
        let fits = match (self, col.value_type) {
            (BindValue::Null, _) => col.kind == ColKind::Value,
            (BindValue::Bool(_), ValueType::Bool)
            | (BindValue::I16(_), ValueType::I16)
            | (BindValue::I32(_), ValueType::I32)
            | (BindValue::I64(_), ValueType::I64)
            | (BindValue::U32(_), ValueType::U32)
            | (BindValue::Text(_), ValueType::Text)
            | (BindValue::Bytes(_), ValueType::Bytea) => true,
            _ => false,
        };
        if fits {
            Ok(())
        } else {
            Err(YbError::Invalid(format!(
                "column {:?} is {:?} ({:?}), cannot bind {self:?}",
                col.name, col.value_type, col.kind
            )))
        }
    }
}

#[derive(Clone, Copy, PartialEq, Eq, Debug)]
pub enum CondOp {
    Eq,
    Le,
    Ge,
    Lt,
    Gt,
}

// ---- decoded read values ----------------------------------------------

/// A decoded cell of a read result. The shim decodes the page (using
/// YugabyteDB's own `pg_doc_data` codec + the opened table's column types),
/// so this crate never parses a wire format — it just maps the shim's typed
/// cells onto these variants.
#[derive(Clone, Debug, PartialEq)]
pub enum Value {
    Null,
    Bool(bool),
    I16(i16),
    I32(i32),
    I64(i64),
    Text(Bytes),
    Bytes(Bytes),
    /// An `oid` column's value.
    U32(u32),
}

impl Value {
    pub fn is_null(&self) -> bool {
        matches!(self, Value::Null)
    }

    /// The value as an `i64` for the integral/bool variants.
    pub fn as_i64(&self) -> Result<i64, YbError> {
        match self {
            Value::Bool(v) => Ok(*v as i64),
            Value::I16(v) => Ok(*v as i64),
            Value::I32(v) => Ok(*v as i64),
            Value::I64(v) => Ok(*v),
            Value::U32(v) => Ok(i64::from(*v)),
            _ => Err(type_err("integer", self)),
        }
    }

    pub fn as_i32(&self) -> Result<i32, YbError> {
        match self {
            Value::I32(v) => Ok(*v),
            Value::I16(v) => Ok(*v as i32),
            _ => Err(type_err("i32", self)),
        }
    }

    pub fn as_i16(&self) -> Result<i16, YbError> {
        match self {
            Value::I16(v) => Ok(*v),
            _ => Err(type_err("i16", self)),
        }
    }

    pub fn as_bool(&self) -> Result<bool, YbError> {
        match self {
            Value::Bool(v) => Ok(*v),
            _ => Err(type_err("bool", self)),
        }
    }

    /// The raw bytes for the `Text`/`Bytes` variants.
    pub fn as_bytes(&self) -> Result<&Bytes, YbError> {
        match self {
            Value::Text(b) | Value::Bytes(b) => Ok(b),
            _ => Err(type_err("text/bytea", self)),
        }
    }
}

fn type_err(want: &str, got: &Value) -> YbError {
    let kind = match got {
        Value::Null => "null",
        Value::Bool(_) => "bool",
        Value::I16(_) => "i16",
        Value::I32(_) => "i32",
        Value::I64(_) => "i64",
        Value::U32(_) => "u32",
        Value::Text(_) => "text",
        Value::Bytes(_) => "bytea",
    };
    YbError::Invalid(format!("cell is {kind}, expected {want}"))
}

/// A decoded row: one [`Value`] per requested target column, in target order.
pub type Row = Vec<Value>;

// ---- read requests -----------------------------------------------------

/// A clustered range read: equality on all hash columns, bound conditions on
/// range columns, returning `targets` in order. On a hash-sharded table a
/// full hash prefix pins the read to one tablet; on a range-sharded one
/// (no hash columns) the conds alone bound the scan and it may span tablets,
/// which the tserver pages through transparently.
///
/// Column names are resolved to ids against the [`TableHandle`] at build
/// time (RPC-free); the read snapshot is a property of the *batch*, not the
/// read (see [`TsClient::read`]).
#[derive(Clone, Debug)]
pub struct RangeRead {
    hash_values: Vec<BindValue>,
    range_values: Vec<BindValue>,
    conds: Vec<(i32, CondOp, BindValue)>,
    target_ids: Vec<i32>,
    forward: bool,
    limit: u64,
}

impl RangeRead {
    /// `hash_values` must cover every hash column in schema order. Empty
    /// means no hash prefix: on a hash-sharded table that is a full scan
    /// across tablets, and on a range-sharded table it is the only legal
    /// value — bound the scan with [`Self::cond`] instead.
    pub fn new(table: &TableHandle, hash_values: Vec<BindValue>) -> Result<Self, YbError> {
        let n_hash = table.num_hash_columns();
        if !hash_values.is_empty() && hash_values.len() != n_hash {
            return Err(YbError::Invalid(format!(
                "table has {n_hash} hash columns, got {} values",
                hash_values.len()
            )));
        }
        check_prefix(table.key_columns(), &hash_values)?;
        Ok(RangeRead {
            hash_values,
            range_values: Vec::new(),
            conds: Vec::new(),
            target_ids: Vec::new(),
            forward: true,
            limit: 0,
        })
    }

    /// A read on a RANGE-sharded table (one with no hash columns), where the
    /// range key plays the part `hash_values` plays for a hash-sharded one.
    ///
    /// `range_values` binds a leading prefix of the range key in schema
    /// order: the full key targets a single row, a shorter prefix that
    /// prefix's range, and empty the whole table. Bound the columns *past*
    /// the prefix with [`Self::cond`].
    ///
    /// Separate from [`Self::new`] because the shim rejects a spec carrying
    /// both hash and range values — keeping them in different constructors
    /// makes that unrepresentable rather than a runtime `Invalid`.
    pub fn new_range(table: &TableHandle, range_values: Vec<BindValue>) -> Result<Self, YbError> {
        let n_hash = table.num_hash_columns();
        if n_hash != 0 {
            return Err(YbError::Invalid(format!(
                "new_range is for range-sharded tables, but this one has {n_hash} hash \
                 columns; use RangeRead::new with a hash prefix"
            )));
        }
        let n_range = table.num_key_columns();
        if range_values.len() > n_range {
            return Err(YbError::Invalid(format!(
                "table has {n_range} range key columns, got {} values",
                range_values.len()
            )));
        }
        check_prefix(table.key_columns(), &range_values)?;
        Ok(RangeRead {
            hash_values: Vec::new(),
            range_values,
            conds: Vec::new(),
            target_ids: Vec::new(),
            forward: true,
            limit: 0,
        })
    }

    /// Adds a bound on a range key column (a DocDB scan bound).
    pub fn cond(
        mut self,
        table: &TableHandle,
        column: &str,
        op: CondOp,
        value: BindValue,
    ) -> Result<Self, YbError> {
        let col = table.col(column)?;
        if col.kind != ColKind::Range {
            return Err(YbError::Invalid(format!(
                "column {column:?} is not a range key column"
            )));
        }
        value.check_fits(col)?;
        self.conds.push((col.id, op, value));
        Ok(self)
    }

    /// Sets the returned columns; cells come back in this order.
    pub fn targets(mut self, table: &TableHandle, columns: &[&str]) -> Result<Self, YbError> {
        self.target_ids.clear();
        for name in columns {
            self.target_ids.push(table.col(name)?.id);
        }
        Ok(self)
    }

    /// Reverse scan; a PK clustered `lsn DESC` means *forward* already
    /// yields LSN-descending rows.
    pub fn descending(mut self) -> Self {
        self.forward = false;
        self
    }

    /// Rows per fetched page (server default when 0).
    pub fn limit(mut self, limit: u64) -> Self {
        self.limit = limit;
        self
    }
}

/// One read op of a batch: which table, what to read, and (for a
/// continuation) the prior page's paging state.
pub struct ReadOp<'a> {
    pub table: &'a TableHandle,
    pub read: &'a RangeRead,
    pub paging_state: Option<&'a [u8]>,
}

/// One op's slice of a batch result. `paging_state` is `None` when that op's
/// scan is exhausted; otherwise feed it back as a continuation op's state.
#[derive(Clone, Debug)]
pub struct ReadResult {
    pub rows: Vec<Row>,
    pub paging_state: Option<Bytes>,
}

/// The result of one read batch: `ops[i]` corresponds to the i-th input op
/// (`TsClient::read` enforces the lengths agree, so callers may index by
/// position); `used_read_time_ht` is the single snapshot the whole batch ran at.
#[derive(Clone, Debug)]
pub struct ReadBatch {
    pub ops: Vec<ReadResult>,
    pub used_read_time_ht: u64,
}

/// A single read's page (sugar over a one-op [`ReadBatch`]).
#[derive(Clone, Debug)]
pub struct ReadPage {
    pub rows: Vec<Row>,
    pub paging_state: Option<Bytes>,
    pub used_read_time_ht: u64,
}

// ---- upsert rows -------------------------------------------------------

/// One row of a batched upsert. `key_values` covers every key column (hash
/// then range) in schema order; `values` are (column name, value) pairs for
/// non-key columns, resolved to ids against the table at build time.
#[derive(Clone)]
pub struct UpsertRow {
    table: Arc<TableHandle>,
    key_values: Vec<BindValue>,
    value_ids: Vec<i32>,
    values: Vec<BindValue>,
}

impl UpsertRow {
    pub fn new(
        table: &Arc<TableHandle>,
        key_values: Vec<BindValue>,
        values: &[(&str, BindValue)],
    ) -> Result<Self, YbError> {
        let n_key = table.num_key_columns();
        if key_values.len() != n_key {
            return Err(YbError::Invalid(format!(
                "table has {n_key} key columns, got {} values",
                key_values.len()
            )));
        }
        check_prefix(table.key_columns(), &key_values)?;
        let mut value_ids = Vec::with_capacity(values.len());
        let mut owned = Vec::with_capacity(values.len());
        for (name, value) in values {
            let col = table.col(name)?;
            if col.kind != ColKind::Value {
                return Err(YbError::Invalid(format!(
                    "column {name:?} is a key column, pass it in key_values"
                )));
            }
            value.check_fits(col)?;
            value_ids.push(col.id);
            owned.push(value.clone());
        }
        Ok(UpsertRow {
            table: table.clone(),
            key_values,
            value_ids,
            values: owned,
        })
    }
}

// ---- write deadline ----------------------------------------------------

/// The lease deadline a write is submitted under.
///
/// The leader applies a row only if the hybrid time it assigns the write is
/// before this instant, and otherwise refuses it with [`YbError::Fenced`]. It
/// is judged per tablet, so a batch whose rows span tablets and straddles the
/// deadline can be partly applied — the same cut a crash at that instant makes.
#[derive(Clone, Copy, Debug, PartialEq, Eq)]
pub enum WriteDeadline {
    /// No fence: the writer holds no lease.
    Unfenced,
    /// Microseconds since the Unix epoch, the unit the lease token is issued in.
    EpochMicros(u64),
}

impl From<Option<u64>> for WriteDeadline {
    fn from(us: Option<u64>) -> Self {
        us.map_or(WriteDeadline::Unfenced, WriteDeadline::EpochMicros)
    }
}

/// Bits a YugabyteDB `HybridTime` keeps for its logical component
/// (`src/yb/common/hybrid_time.h`): physical microseconds sit above them.
const HT_LOGICAL_BITS: u32 = 12;

impl WriteDeadline {
    /// The value for the shim's `ignore_after_hybrid_time`, and the only place
    /// the token becomes a hybrid time: `epoch_micros << 12`, logical bits 0.
    ///
    /// `0` on the wire means "no fence", so a zero deadline — an armed writer
    /// that was never issued a token — is sent as the earliest representable
    /// time instead, which every leader refuses. Collapsing it to `0` would
    /// turn the most fenced writer there is into an unfenced one.
    pub fn ignore_after_hybrid_time(self) -> Result<u64, YbError> {
        match self {
            WriteDeadline::Unfenced => Ok(0),
            WriteDeadline::EpochMicros(us) => {
                let us = us.max(1);
                if us > u64::MAX >> HT_LOGICAL_BITS {
                    return Err(YbError::Invalid(format!(
                        "write deadline {us}us does not fit a hybrid time"
                    )));
                }
                Ok(us << HT_LOGICAL_BITS)
            }
        }
    }
}

// ---- config ------------------------------------------------------------

#[derive(Clone, Debug, Default)]
pub struct TlsOpts {
    pub ca_cert_path: Option<String>,
    pub cert_path: Option<String>,
    pub key_path: Option<String>,
}

impl TlsOpts {
    /// TLS against `ca_cert_path`, authenticating with the `(cert, key)`
    /// client identity when one is given. The shim reads all three files at
    /// [`TsClient::connect`], so a recycled client picks up renewed files.
    pub fn from_paths(ca_cert_path: String, client: Option<(String, String)>) -> Self {
        let (cert_path, key_path) = match client {
            Some((cert, key)) => (Some(cert), Some(key)),
            None => (None, None),
        };
        TlsOpts {
            ca_cert_path: Some(ca_cert_path),
            cert_path,
            key_path,
        }
    }

    /// The files that are actually set, in CA / certificate / key order. What
    /// the reload watcher polls.
    pub fn paths(&self) -> Vec<String> {
        [&self.ca_cert_path, &self.cert_path, &self.key_path]
            .into_iter()
            .flatten()
            .cloned()
            .collect()
    }
}

#[derive(Clone, Debug)]
pub struct TsClientConfig {
    pub tserver_addrs: Vec<String>,
    pub tls: Option<TlsOpts>,
    pub rpc_timeout_ms: u32,
    pub num_reactors: u32,
    /// Session/connection pool sizing. The shim packs `sessions_per_conn`
    /// sessions onto each connection and opens
    /// `ceil((read_sessions + write_sessions) / sessions_per_conn)`
    /// connections; reads round-robin the read sessions, upserts the write
    /// sessions. A field of 0 selects the shim's default (read 4, write 1,
    /// per-conn 4).
    pub read_sessions: u32,
    pub write_sessions: u32,
    pub sessions_per_conn: u32,
}

impl Default for TsClientConfig {
    fn default() -> Self {
        TsClientConfig {
            tserver_addrs: Vec::new(),
            tls: None,
            rpc_timeout_ms: 10_000,
            num_reactors: 0,
            read_sessions: 0,
            write_sessions: 0,
            sessions_per_conn: 0,
        }
    }
}

// ---- marshalling helpers ----------------------------------------------

fn to_bind(b: &BindValue) -> ffi::YbBind {
    let mut out = ffi::YbBind {
        tag: ffi::YbBindTag::Null,
        int_value: 0,
        bytes: std::ptr::null(),
        bytes_len: 0,
    };
    match b {
        BindValue::Null => {}
        BindValue::Bool(v) => {
            out.tag = ffi::YbBindTag::Bool;
            out.int_value = *v as i64;
        }
        BindValue::I16(v) => {
            out.tag = ffi::YbBindTag::I16;
            out.int_value = *v as i64;
        }
        BindValue::I32(v) => {
            out.tag = ffi::YbBindTag::I32;
            out.int_value = *v as i64;
        }
        BindValue::I64(v) => {
            out.tag = ffi::YbBindTag::I64;
            out.int_value = *v;
        }
        BindValue::U32(v) => {
            out.tag = ffi::YbBindTag::U32;
            out.int_value = i64::from(*v);
        }
        BindValue::Text(s) => {
            out.tag = ffi::YbBindTag::Text;
            out.bytes = s.as_ptr();
            out.bytes_len = s.len();
        }
        BindValue::Bytes(b) => {
            out.tag = ffi::YbBindTag::Bytea;
            out.bytes = b.as_ptr();
            out.bytes_len = b.len();
        }
    }
    out
}

fn cond_op(op: CondOp) -> ffi::YbCondOp {
    match op {
        CondOp::Eq => ffi::YbCondOp::Eq,
        CondOp::Le => ffi::YbCondOp::Le,
        CondOp::Ge => ffi::YbCondOp::Ge,
        CondOp::Lt => ffi::YbCondOp::Lt,
        CondOp::Gt => ffi::YbCondOp::Gt,
    }
}

/// Attempts a single `Perform` gets before its error is surfaced. This lane
/// has nothing to fall back to, so it must ride out transient failures
/// (tablet-leader moves, `TRY_AGAIN`, network blips) itself — a hard failure
/// here stalls the WAL consumer or a getpage.
const MAX_ATTEMPTS: usize = 10;
/// First inter-attempt sleep; doubles per attempt up to [`BACKOFF_MAX`].
const BACKOFF_INITIAL: std::time::Duration = std::time::Duration::from_millis(50);
const BACKOFF_MAX: std::time::Duration = std::time::Duration::from_secs(2);
/// Whole-scan restarts [`TsClient::read_scan`] gets on a read-restart it can't
/// heal in place.
const MAX_SCAN_RESTARTS: usize = 5;

/// Run `f` until it succeeds, its error is non-retryable, `give_up` claims it,
/// or the attempt budget is spent; sleeping with exponential backoff between
/// attempts.
///
/// `give_up` marks errors that are *classified* retryable but can't be healed
/// by re-issuing this particular call (a read-restart on a pinned continuation
/// — see [`TsClient::read`]), so the caller can react instead of burning the
/// budget replaying the same failure.
async fn with_retries<T, F, Fut>(
    what: &str,
    give_up: impl Fn(&YbError) -> bool,
    f: F,
) -> Result<T, YbError>
where
    F: Fn() -> Fut,
    Fut: std::future::Future<Output = Result<T, YbError>>,
{
    let mut backoff = BACKOFF_INITIAL;
    for attempt in 1..=MAX_ATTEMPTS {
        match f().await {
            Ok(v) => return Ok(v),
            Err(e) => {
                if !e.is_retryable() || give_up(&e) || attempt == MAX_ATTEMPTS {
                    // WARN even the last attempt: a caller may absorb this
                    // error, and a failure that burned the whole budget must
                    // not vanish with it.
                    tracing::warn!(
                        "yb_thin_client {what} attempt {attempt}/{MAX_ATTEMPTS} failed, \
                         giving up: {e}"
                    );
                    return Err(e);
                }
                tracing::warn!(
                    "yb_thin_client {what} attempt {attempt}/{MAX_ATTEMPTS} failed, \
                     retrying in {backoff:?}: {e}"
                );
                tokio::time::sleep(backoff).await;
                backoff = (backoff * 2).min(BACKOFF_MAX);
            }
        }
    }
    unreachable!("loop returns on the final attempt")
}

/// Pagination decision for [`TsClient::read_scan`]: the continuation token to
/// fetch the next page with, or `None` to stop the scan.
///
/// An `on_page` callback that returns `true` has what it needs (a `limit(1)`
/// row, a chain's `will_init` anchor) and stops the scan even when the server
/// offered a continuation; otherwise the scan continues iff a continuation
/// exists. Getting this backwards — continuing only while `on_page` asks to
/// stop — truncates every multi-page scan after its first page.
fn next_scan_page(on_page_requested_stop: bool, paging_state: Option<Bytes>) -> Option<Bytes> {
    if on_page_requested_stop {
        None
    } else {
        paging_state
    }
}

// ---- handles -----------------------------------------------------------

/// A connected client. The underlying shim client owns the RPC messenger
/// + session pool and is internally thread-safe.
pub struct TsClient {
    raw: *mut ffi::ybthin_client,
}

// SAFETY: the C++ shim client (messenger/proxy/session pool) is shared and
// thread-safe; the Rust handle only forwards calls.
unsafe impl Send for TsClient {}
unsafe impl Sync for TsClient {}

impl Drop for TsClient {
    fn drop(&mut self) {
        unsafe { ffi::ybthin_client_destroy(self.raw) };
    }
}

pub struct TableHandle {
    raw: *mut ffi::ybthin_table,
    pub columns: Vec<Column>,
}

unsafe impl Send for TableHandle {}
unsafe impl Sync for TableHandle {}

impl Drop for TableHandle {
    fn drop(&mut self) {
        unsafe { ffi::ybthin_table_close(self.raw) };
    }
}

impl TableHandle {
    /// Look up a column by name.
    pub fn col(&self, name: &str) -> Result<&Column, YbError> {
        self.columns
            .iter()
            .find(|c| c.name == name)
            .ok_or_else(|| YbError::Invalid(format!("table has no column {name:?}")))
    }

    pub fn num_hash_columns(&self) -> usize {
        self.columns
            .iter()
            .filter(|c| c.kind == ColKind::Hash)
            .count()
    }

    pub fn num_key_columns(&self) -> usize {
        self.key_columns().count()
    }

    /// The key columns in schema order: hash, then range.
    fn key_columns(&self) -> impl Iterator<Item = &Column> {
        self.columns
            .iter()
            .filter(|c| matches!(c.kind, ColKind::Hash | ColKind::Range))
    }
}

/// [`BindValue::check_fits`] over `values` against the leading `columns`. The
/// callers check the counts first, so `zip` stopping at the shorter side
/// drops nothing.
fn check_prefix<'a>(
    columns: impl Iterator<Item = &'a Column>,
    values: &[BindValue],
) -> Result<(), YbError> {
    columns
        .zip(values)
        .try_for_each(|(col, value)| value.check_fits(col))
}

impl TsClient {
    /// Connect to the tserver RPC endpoints and open a session pool. Blocks
    /// on the connect/heartbeat RPC.
    pub fn connect(config: &TsClientConfig) -> Result<Arc<TsClient>, YbError> {
        if config.tserver_addrs.is_empty() {
            return Err(YbError::Invalid("tserver_addrs must not be empty".into()));
        }
        // Keep the CStrings alive across the call.
        let addrs: Vec<CString> = config
            .tserver_addrs
            .iter()
            .map(|a| CString::new(a.as_str()))
            .collect::<Result<_, _>>()
            .map_err(|_| YbError::Invalid("tserver address contains a NUL byte".into()))?;
        let addr_ptrs: Vec<*const std::os::raw::c_char> =
            addrs.iter().map(|c| c.as_ptr()).collect();

        // TLS paths (kept alive across the call via the CStrings).
        let tls_paths = config.tls.as_ref().map(|t| {
            let cs =
                |o: &Option<String>| o.as_deref().map(|s| CString::new(s).expect("tls path NUL"));
            (cs(&t.ca_cert_path), cs(&t.cert_path), cs(&t.key_path))
        });
        let tls = tls_paths.as_ref().map(|(ca, cert, key)| ffi::YbTlsOpts {
            ca_cert_path: ca.as_ref().map_or(std::ptr::null(), |c| c.as_ptr()),
            cert_path: cert.as_ref().map_or(std::ptr::null(), |c| c.as_ptr()),
            key_path: key.as_ref().map_or(std::ptr::null(), |c| c.as_ptr()),
        });

        let pool = ffi::YbPoolOpts {
            read_sessions: config.read_sessions,
            write_sessions: config.write_sessions,
            sessions_per_conn: config.sessions_per_conn,
        };
        let mut raw: *mut ffi::ybthin_client = std::ptr::null_mut();
        let status = unsafe {
            ffi::ybthin_client_create(
                addr_ptrs.as_ptr(),
                addr_ptrs.len(),
                tls.as_ref().map_or(std::ptr::null(), |t| t as *const _),
                &pool,
                config.rpc_timeout_ms,
                config.num_reactors,
                &mut raw,
            )
        };
        unsafe { error::check(status)? };
        if raw.is_null() {
            return Err(YbError::Other("shim returned OK but a null client".into()));
        }
        Ok(Arc::new(TsClient { raw }))
    }

    /// Resolve a table by `(db_oid, table_oid)`: its id + schema. Blocks.
    pub fn open_table(&self, db_oid: u32, table_oid: u32) -> Result<Arc<TableHandle>, YbError> {
        let mut raw: *mut ffi::ybthin_table = std::ptr::null_mut();
        let mut info = ffi::YbTableInfo {
            columns: std::ptr::null_mut(),
            n_columns: 0,
        };
        let status =
            unsafe { ffi::ybthin_table_open(self.raw, db_oid, table_oid, &mut raw, &mut info) };
        unsafe { error::check(status)? };
        if raw.is_null() {
            return Err(YbError::Other("shim returned OK but a null table".into()));
        }
        let columns = unsafe { columns_from(&info) };
        unsafe { ffi::ybthin_columns_free(info.columns, info.n_columns) };
        Ok(Arc::new(TableHandle { raw, columns }))
    }

    /// The read primitive: run `ops` as the ops of ONE `Perform` — one RPC,
    /// one read session, one snapshot. `read_time_ht == 0` lets the server
    /// pick the batch read point; nonzero pins it (e.g. continuing a scan
    /// under a prior page's `used_read_time_ht`, or restarting at a read
    /// time a prior restart returned). `YbError::ReadRestart` means the
    /// caller should restart the whole batch.
    pub async fn read(&self, ops: &[ReadOp<'_>], read_time_ht: u64) -> Result<ReadBatch, YbError> {
        // A read-restart on a PINNED read (a paging continuation, or any page
        // pinned to an earlier page's `read_time_ht`) can't be healed here:
        // every attempt would replay the same read point. Surface it so the
        // scan driver restarts the whole scan on a fresh one. A first,
        // unpinned page instead retries here — it gets a fresh read point.
        let pinned = read_time_ht != 0 || ops.iter().any(|o| o.paging_state.is_some());
        with_retries(
            "read",
            |e| pinned && e.is_read_restart(),
            || self.read_once(ops, read_time_ht),
        )
        .await
    }

    /// One `Perform`, no retries. [`Self::read`] wraps this with the retry
    /// driver; the paging loop in [`Self::read_scan`] drives whole-scan
    /// restarts on top.
    async fn read_once(&self, ops: &[ReadOp<'_>], read_time_ht: u64) -> Result<ReadBatch, YbError> {
        // Fire the RPC from a synchronous helper: the FFI marshalling holds
        // raw shim pointers (`YbReadOp` and the bind/cond arrays) that are
        // `!Send`, and doing it in a sync fn keeps them out of this future's
        // suspended state across the await below, so the future stays `Send`
        // for the tokio task pool.
        let batch = self
            .fire_read(ops, read_time_ht)
            .await
            .unwrap_or_else(|_| Err(YbError::Other("shim dropped the read callback".into())))?;
        // Position is the only thing pairing results to ops: no id travels on
        // either side to re-key by. A wrong-length batch would pair rows with
        // the wrong op, or for a paging driver drop the missing ops and end
        // their scans early, looking exactly like exhaustion.
        if batch.ops.len() != ops.len() {
            return Err(YbError::Other(format!(
                "read returned {} results for {} ops — results are positional, \
                 so a mismatch would pair them with the wrong op",
                batch.ops.len(),
                ops.len()
            )));
        }
        Ok(batch)
    }

    fn fire_read(
        &self,
        ops: &[ReadOp<'_>],
        read_time_ht: u64,
    ) -> oneshot::Receiver<Result<ReadBatch, YbError>> {
        // Own the per-op bind/cond arrays so their pointers stay valid for
        // the call; the byte buffers they point at live in the borrowed
        // `RangeRead`s.
        let hashes: Vec<Vec<ffi::YbBind>> = ops
            .iter()
            .map(|op| op.read.hash_values.iter().map(to_bind).collect())
            .collect();
        let ranges: Vec<Vec<ffi::YbBind>> = ops
            .iter()
            .map(|op| op.read.range_values.iter().map(to_bind).collect())
            .collect();
        let conds: Vec<Vec<ffi::YbCond>> = ops
            .iter()
            .map(|op| {
                op.read
                    .conds
                    .iter()
                    .map(|(id, o, v)| ffi::YbCond {
                        column_id: *id,
                        op: cond_op(*o),
                        value: to_bind(v),
                    })
                    .collect()
            })
            .collect();
        let c_ops: Vec<ffi::YbReadOp> = ops
            .iter()
            .enumerate()
            .map(|(i, op)| ffi::YbReadOp {
                table: op.table.raw,
                spec: ffi::YbReadSpec {
                    hash_values: hashes[i].as_ptr(),
                    n_hash: hashes[i].len(),
                    conds: conds[i].as_ptr(),
                    n_conds: conds[i].len(),
                    target_ids: op.read.target_ids.as_ptr(),
                    n_targets: op.read.target_ids.len(),
                    limit: op.read.limit,
                    is_forward_scan: op.read.forward as std::os::raw::c_int,
                    range_values: ranges[i].as_ptr(),
                    n_range: ranges[i].len(),
                },
                paging_state_in: op.paging_state.map_or(std::ptr::null(), |p| p.as_ptr()),
                paging_state_in_len: op.paging_state.map_or(0, |p| p.len()),
            })
            .collect();

        let (tx, rx) = oneshot::channel::<Result<ReadBatch, YbError>>();
        let ctx = Box::into_raw(Box::new(tx)) as *mut c_void;
        unsafe {
            ffi::ybthin_read_async(
                self.raw,
                c_ops.as_ptr(),
                c_ops.len(),
                read_time_ht,
                read_cb,
                ctx,
            );
        }
        // Keep the marshalled inputs alive until the shim has built the
        // request (the async call above returns after copying them).
        drop((hashes, conds, c_ops));
        rx
    }

    /// One page of a single read (a one-op batch). `paging_state == None`
    /// starts the scan; otherwise pass the previous page's state.
    pub async fn read_page(
        &self,
        table: &TableHandle,
        read: &RangeRead,
        paging_state: Option<&[u8]>,
    ) -> Result<ReadPage, YbError> {
        let mut batch = self
            .read(
                &[ReadOp {
                    table,
                    read,
                    paging_state,
                }],
                0,
            )
            .await?;
        let op = batch
            .ops
            .pop()
            .ok_or_else(|| YbError::Other("read batch returned no op result".into()))?;
        Ok(ReadPage {
            rows: op.rows,
            paging_state: op.paging_state,
            used_read_time_ht: batch.used_read_time_ht,
        })
    }

    /// Page a whole scan through the accumulator, all pages pinned to the
    /// first page's snapshot. `on_page` returns `true` to stop early.
    pub async fn read_scan<A, E, F, G>(
        &self,
        table: &TableHandle,
        read: &RangeRead,
        mut new_acc: F,
        mut on_page: G,
    ) -> Result<A, E>
    where
        F: FnMut() -> A,
        G: FnMut(&mut A, Vec<Row>) -> Result<bool, E>,
        E: From<YbError>,
    {
        // A read-restart on a pinned continuation can't be healed in place
        // (`TsClient::read` surfaces it rather than replaying the same read
        // point), so the whole scan restarts on a fresh one — dropping the
        // half-built accumulator, which is why `new_acc` is a closure.
        for scan_attempt in 1..=MAX_SCAN_RESTARTS {
            let mut acc = new_acc();
            let mut paging_state: Option<Bytes> = None;
            let mut read_time_ht: u64 = 0;
            let mut restart = false;
            // Consecutive empty-but-continuing pages; see `check_barren_round`.
            let mut barren = 0usize;
            loop {
                let mut batch = match self
                    .read(
                        &[ReadOp {
                            table,
                            read,
                            paging_state: paging_state.as_deref(),
                        }],
                        read_time_ht,
                    )
                    .await
                {
                    Ok(b) => b,
                    Err(e) if e.is_read_restart() && scan_attempt < MAX_SCAN_RESTARTS => {
                        tracing::warn!(
                            "yb_thin_client scan read-restart (attempt \
                             {scan_attempt}/{MAX_SCAN_RESTARTS}), restarting the scan: {e}"
                        );
                        restart = true;
                        break;
                    }
                    Err(e) => return Err(e.into()),
                };
                // Pin every subsequent page to the snapshot the first page ran at.
                read_time_ht = batch.used_read_time_ht;
                let op = batch
                    .ops
                    .pop()
                    .ok_or_else(|| YbError::Other("read batch returned no op result".into()))?;
                // `on_page` returns `true` to STOP early (e.g. a chain reached
                // its `will_init` anchor, or a `limit(1)` point read found its
                // row).
                let got = op.rows.len();
                let requested_stop = on_page(&mut acc, op.rows)?;
                match next_scan_page(requested_stop, op.paging_state) {
                    Some(ps) => {
                        // Nothing else bounds this loop: every RPC succeeds, so
                        // neither `with_retries` nor the RPC timeout ever fires.
                        crate::barren::check_barren_round(&mut barren, got)
                            .map_err(YbError::from)?;
                        paging_state = Some(ps);
                    }
                    None => break,
                }
            }
            if !restart {
                return Ok(acc);
            }
        }
        unreachable!("the final scan attempt returns or propagates")
    }

    /// Apply a batch of upserts as one `Perform` (one write RPC), every row
    /// fenced on `deadline`. Rows may span tables.
    ///
    /// A retry re-sends under the same deadline, so a write that outlives its
    /// lease comes back [`YbError::Fenced`] rather than landing late.
    pub async fn upsert(&self, rows: &[UpsertRow], deadline: WriteDeadline) -> Result<(), YbError> {
        let ignore_after = deadline.ignore_after_hybrid_time()?;
        // Upserts are idempotent (full-overwrite by primary key), so every
        // retryable class is safe to re-issue: nothing to give up early on.
        with_retries("upsert", |_| false, || self.upsert_once(rows, ignore_after)).await
    }

    /// One write `Perform`, no retries. [`Self::upsert`] wraps this with the
    /// retry driver.
    async fn upsert_once(&self, rows: &[UpsertRow], ignore_after: u64) -> Result<(), YbError> {
        // Fire from a synchronous helper for the same `Send` reason as
        // `read`: the FFI marshalling holds `!Send` raw shim pointers that
        // must not live in this future across the await.
        self.fire_upsert(rows, ignore_after)
            .await
            .unwrap_or_else(|_| Err(YbError::Other("shim dropped the write callback".into())))
    }

    fn fire_upsert(
        &self,
        rows: &[UpsertRow],
        ignore_after: u64,
    ) -> oneshot::Receiver<Result<(), YbError>> {
        // Own the per-row bind arrays so their pointers stay valid across
        // the call.
        let keys: Vec<Vec<ffi::YbBind>> = rows
            .iter()
            .map(|r| r.key_values.iter().map(to_bind).collect())
            .collect();
        let vals: Vec<Vec<ffi::YbBind>> = rows
            .iter()
            .map(|r| r.values.iter().map(to_bind).collect())
            .collect();
        let c_rows: Vec<ffi::YbUpsertRow> = rows
            .iter()
            .enumerate()
            .map(|(i, r)| ffi::YbUpsertRow {
                table: r.table.raw,
                key_values: keys[i].as_ptr(),
                n_keys: keys[i].len(),
                value_ids: r.value_ids.as_ptr(),
                values: vals[i].as_ptr(),
                n_values: vals[i].len(),
                ignore_after_hybrid_time: ignore_after,
            })
            .collect();

        let (tx, rx) = oneshot::channel::<Result<(), YbError>>();
        let ctx = Box::into_raw(Box::new(tx)) as *mut c_void;
        unsafe {
            ffi::ybthin_upsert_batch_async(self.raw, c_rows.as_ptr(), c_rows.len(), write_cb, ctx);
        }
        drop((keys, vals, c_rows));
        rx
    }
}

// ---- callbacks (run on shim reactor threads) ---------------------------

extern "C" fn read_cb(ctx: *mut c_void, status: ffi::YbStatus, result: *mut ffi::YbReadResult) {
    let tx = unsafe { Box::from_raw(ctx as *mut oneshot::Sender<Result<ReadBatch, YbError>>) };
    let outcome = unsafe { read_outcome(status, result) };
    let _ = tx.send(outcome);
}

extern "C" fn write_cb(ctx: *mut c_void, status: ffi::YbStatus) {
    let tx = unsafe { Box::from_raw(ctx as *mut oneshot::Sender<Result<(), YbError>>) };
    let outcome = unsafe { error::check(status) };
    let _ = tx.send(outcome);
}

// SAFETY: `result` (if non-null) is a shim-owned `YbReadResult`; its buffers
// are copied out and it is freed here exactly once. `status`'s message is
// freed by `error::check`.
unsafe fn read_outcome(
    status: ffi::YbStatus,
    result: *mut ffi::YbReadResult,
) -> Result<ReadBatch, YbError> {
    let checked = unsafe { error::check(status) };
    let out = match checked {
        Err(e) => Err(e),
        Ok(()) if result.is_null() => Err(YbError::Other(
            "shim returned OK with a null read result".into(),
        )),
        Ok(()) => {
            let r = unsafe { &*result };
            let ops = if r.results.is_null() || r.n_ops == 0 {
                Vec::new()
            } else {
                let slice = unsafe { std::slice::from_raw_parts(r.results, r.n_ops) };
                slice
                    .iter()
                    .map(|op| ReadResult {
                        rows: unsafe { rows_from_cells(op) },
                        paging_state: if op.paging_state.is_null() || op.paging_state_len == 0 {
                            None
                        } else {
                            Some(Bytes::copy_from_slice(unsafe {
                                std::slice::from_raw_parts(op.paging_state, op.paging_state_len)
                            }))
                        },
                    })
                    .collect()
            };
            Ok(ReadBatch {
                ops,
                used_read_time_ht: r.used_read_time_ht,
            })
        }
    };
    if !result.is_null() {
        unsafe { ffi::ybthin_read_result_free(result) };
    }
    out
}

// SAFETY: `op.cells` (if non-null) is a shim-owned row-major array of
// `op.n_rows * op.n_cols` cells; each `Text`/`Bytea` cell's `bytes` point
// into result-owned storage valid until `ybthin_read_result_free`. Every
// value is copied out here, before the caller frees the result.
unsafe fn rows_from_cells(op: &ffi::YbReadOpResult) -> Vec<Row> {
    if op.cells.is_null() || op.n_rows == 0 || op.n_cols == 0 {
        return Vec::new();
    }
    let cells = unsafe { std::slice::from_raw_parts(op.cells, op.n_rows * op.n_cols) };
    cells
        .chunks_exact(op.n_cols)
        .map(|row| row.iter().map(cell_to_value).collect())
        .collect()
}

fn cell_to_value(c: &ffi::YbCell) -> Value {
    let bytes = || {
        if c.bytes.is_null() || c.bytes_len == 0 {
            Bytes::new()
        } else {
            // SAFETY: see `rows_from_cells` — valid until the result is freed.
            Bytes::copy_from_slice(unsafe { std::slice::from_raw_parts(c.bytes, c.bytes_len) })
        }
    };
    match c.tag {
        ffi::YbBindTag::Null => Value::Null,
        ffi::YbBindTag::Bool => Value::Bool(c.int_value != 0),
        ffi::YbBindTag::I16 => Value::I16(c.int_value as i16),
        ffi::YbBindTag::I32 => Value::I32(c.int_value as i32),
        ffi::YbBindTag::I64 => Value::I64(c.int_value),
        ffi::YbBindTag::U32 => Value::U32(c.int_value as u32),
        ffi::YbBindTag::Text => Value::Text(bytes()),
        ffi::YbBindTag::Bytea => Value::Bytes(bytes()),
    }
}

// SAFETY: `info.columns` is a shim-owned array of `info.n_columns` entries,
// each with a shim-owned C-string `name` valid until `ybthin_columns_free`.
unsafe fn columns_from(info: &ffi::YbTableInfo) -> Vec<Column> {
    if info.columns.is_null() {
        return Vec::new();
    }
    let raw = unsafe { std::slice::from_raw_parts(info.columns, info.n_columns) };
    raw.iter()
        .map(|c| Column {
            name: if c.name.is_null() {
                String::new()
            } else {
                unsafe { std::ffi::CStr::from_ptr(c.name) }
                    .to_string_lossy()
                    .into_owned()
            },
            id: c.id,
            kind: match c.kind {
                ffi::YbColKind::Hash => ColKind::Hash,
                ffi::YbColKind::Range => ColKind::Range,
                ffi::YbColKind::Value => ColKind::Value,
            },
            value_type: match c.value_type {
                ffi::YbValueType::Bool => ValueType::Bool,
                ffi::YbValueType::I16 => ValueType::I16,
                ffi::YbValueType::I32 => ValueType::I32,
                ffi::YbValueType::I64 => ValueType::I64,
                ffi::YbValueType::U32 => ValueType::U32,
                ffi::YbValueType::Text => ValueType::Text,
                ffi::YbValueType::Bytea => ValueType::Bytea,
            },
        })
        .collect()
}

#[cfg(test)]
mod tests {
    use std::cell::Cell;

    use bytes::Bytes;

    use super::{MAX_ATTEMPTS, WriteDeadline, next_scan_page, with_retries};
    use crate::error::YbError;

    /// The encoding the leader compares against its own clock: physical
    /// microseconds above 12 logical bits. Sending raw micros would read as a
    /// 1970 hybrid time and fence every write; a wrong shift fences at the
    /// wrong instant. `live.rs` checks the same value against a real tserver.
    #[test]
    fn a_deadline_is_epoch_micros_shifted_past_the_logical_bits() {
        let us = 1_786_161_733_463_940_u64;
        assert_eq!(
            WriteDeadline::EpochMicros(us)
                .ignore_after_hybrid_time()
                .unwrap(),
            us << 12
        );
        assert_eq!(
            WriteDeadline::EpochMicros(us)
                .ignore_after_hybrid_time()
                .unwrap()
                >> 12,
            us,
            "the physical part must round-trip exactly"
        );
    }

    #[test]
    fn unfenced_is_zero_on_the_wire() {
        assert_eq!(
            WriteDeadline::Unfenced.ignore_after_hybrid_time().unwrap(),
            0
        );
        assert_eq!(WriteDeadline::from(None), WriteDeadline::Unfenced);
        assert_eq!(WriteDeadline::from(Some(7)), WriteDeadline::EpochMicros(7));
    }

    /// An armed writer with no token holds deadline 0. The wire's 0 means "no
    /// fence", so passing it through would unfence the one writer that must be
    /// refused. It has to become the earliest real hybrid time instead.
    #[test]
    fn a_zero_deadline_is_already_expired_not_unfenced() {
        let ht = WriteDeadline::EpochMicros(0)
            .ignore_after_hybrid_time()
            .unwrap();
        assert_ne!(ht, 0, "a zero deadline must never be sent as unfenced");
        assert_eq!(ht, 1 << 12);
    }

    #[test]
    fn a_deadline_past_the_hybrid_time_range_is_refused() {
        let max = u64::MAX >> 12;
        assert!(
            WriteDeadline::EpochMicros(max)
                .ignore_after_hybrid_time()
                .is_ok()
        );
        assert!(matches!(
            WriteDeadline::EpochMicros(max + 1).ignore_after_hybrid_time(),
            Err(YbError::Invalid(_))
        ));
    }

    /// Retryable errors are re-issued until one succeeds. Without this the
    /// hot path (now the default lane) turns a routine tablet-leader move or
    /// TRY_AGAIN into a hard failure — which, in the safekeeper's WAL
    /// consumer, terminates the flush task and blocks compute commits.
    #[tokio::test(start_paused = true)]
    async fn with_retries_rides_out_transient_failures() {
        let calls = Cell::new(0);
        let out: Result<u8, YbError> = with_retries(
            "t",
            |_| false,
            || async {
                calls.set(calls.get() + 1);
                if calls.get() < 3 {
                    Err(YbError::TryAgain("leader not ready".into()))
                } else {
                    Ok(7)
                }
            },
        )
        .await;
        assert_eq!(out.unwrap(), 7);
        assert_eq!(calls.get(), 3, "should retry until success");
    }

    /// A non-retryable error is surfaced on the first attempt — no budget burn.
    #[tokio::test(start_paused = true)]
    async fn with_retries_gives_up_immediately_on_fatal() {
        let calls = Cell::new(0);
        let out: Result<u8, YbError> = with_retries(
            "t",
            |_| false,
            || async {
                calls.set(calls.get() + 1);
                Err(YbError::Invalid("bad bind".into()))
            },
        )
        .await;
        assert!(out.is_err());
        assert_eq!(calls.get(), 1, "Invalid is not retryable");
    }

    /// `give_up` short-circuits an otherwise-retryable class: a read-restart on
    /// a pinned continuation must reach the scan driver (which restarts the
    /// whole scan) instead of replaying the same doomed read point.
    #[tokio::test(start_paused = true)]
    async fn with_retries_honours_give_up_for_pinned_read_restart() {
        let calls = Cell::new(0);
        let out: Result<u8, YbError> = with_retries(
            "t",
            |e| e.is_read_restart(),
            || async {
                calls.set(calls.get() + 1);
                Err(YbError::ReadRestart("restart_read_time".into()))
            },
        )
        .await;
        assert!(out.unwrap_err().is_read_restart());
        assert_eq!(calls.get(), 1, "give_up must not burn the budget");
    }

    /// The budget is bounded — a permanently-failing retryable error stops.
    #[tokio::test(start_paused = true)]
    async fn with_retries_bounds_the_budget() {
        let calls = Cell::new(0);
        let out: Result<u8, YbError> = with_retries(
            "t",
            |_| false,
            || async {
                calls.set(calls.get() + 1);
                Err(YbError::Network("connection reset".into()))
            },
        )
        .await;
        assert!(out.is_err());
        assert_eq!(calls.get(), MAX_ATTEMPTS);
    }

    /// A bind must carry its column's own type: the tserver builds the key
    /// entry from the bind, so a wider or narrower integer is a different key.
    #[test]
    fn a_bind_must_match_its_column_type() {
        use super::{BindValue, ColKind, Column, ValueType, check_prefix};
        let col = |kind, value_type| Column {
            name: "c".into(),
            id: 1,
            kind,
            value_type,
        };
        let key = |value_type| col(ColKind::Range, value_type);
        for (value_type, fits) in [
            (ValueType::I16, BindValue::I16(7)),
            (ValueType::I32, BindValue::I32(7)),
            (ValueType::I64, BindValue::I64(7)),
            (ValueType::U32, BindValue::U32(u32::MAX)),
            (ValueType::Bool, BindValue::Bool(true)),
            (ValueType::Text, BindValue::Text("t".into())),
            (ValueType::Bytea, BindValue::Bytes(Bytes::from_static(b"b"))),
        ] {
            assert!(fits.check_fits(&key(value_type)).is_ok(), "{fits:?}");
        }
        assert!(BindValue::I64(7).check_fits(&key(ValueType::I32)).is_err());
        assert!(BindValue::I32(7).check_fits(&key(ValueType::I64)).is_err());
        assert!(BindValue::I64(7).check_fits(&key(ValueType::U32)).is_err());
        assert!(BindValue::I32(7).check_fits(&key(ValueType::I16)).is_err());
        assert!(
            BindValue::Text("t".into())
                .check_fits(&key(ValueType::Bytea))
                .is_err()
        );
        // NULL is a value-column state; no key column can hold it.
        assert!(BindValue::Null.check_fits(&key(ValueType::I64)).is_err());
        assert!(
            BindValue::Null
                .check_fits(&col(ColKind::Value, ValueType::I64))
                .is_ok()
        );

        let columns = [key(ValueType::I16), key(ValueType::U32)];
        assert!(check_prefix(columns.iter(), &[BindValue::I16(1), BindValue::U32(2)]).is_ok());
        assert!(check_prefix(columns.iter(), &[BindValue::I16(1)]).is_ok());
        assert!(check_prefix(columns.iter(), &[BindValue::I16(1), BindValue::I64(2)]).is_err());
    }

    /// The `read_scan` pagination truth table. The regression: a scan must keep
    /// paging while `on_page` has NOT asked to stop and the server still offers
    /// a continuation. Continuing only while `on_page` asks to stop (the old
    /// bug) truncated multi-page chains after page 1, dropping their tail.
    #[test]
    fn next_scan_page_truth_table() {
        let cont = || Some(Bytes::from_static(b"paging-token"));

        // on_page not done + a continuation exists → keep paging with it.
        assert_eq!(next_scan_page(false, cont()), cont());
        // on_page not done + range exhausted → stop.
        assert_eq!(next_scan_page(false, None), None);
        // on_page said stop → stop, even though the server offered more.
        assert_eq!(next_scan_page(true, cont()), None);
        // on_page said stop + range exhausted → stop.
        assert_eq!(next_scan_page(true, None), None);
    }
}
