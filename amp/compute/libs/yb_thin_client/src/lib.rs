//! Thin, safe client to a yb-tserver's `PgClientService.Perform`.
//!
//! A minimal wrapper over the `libyb_thin_client` C ABI (`shim/yb_thin_client.h`):
//! the shim sends `Perform` to a tserver and the tserver does the tablet
//! routing, so there is no metacache / batcher / tablet invoker on this
//! side. All YugabyteDB C++ lives behind the C boundary; this crate only
//! marshals arguments, bridges the shim's callbacks to `oneshot`s, and
//! maps status codes onto [`YbError`]. See `README.md` for the design and
//! the C++ shim spec.

mod client;
mod error;
mod ffi;
mod recycle;
mod tls_watch;

pub use client::{
    BindValue, ColKind, Column, CondOp, RangeRead, ReadBatch, ReadOp, ReadPage, ReadResult, Row,
    TableHandle, TlsOpts, TsClient, TsClientConfig, UpsertRow, Value, ValueType,
};
pub use error::YbError;
pub use recycle::{RECYCLE_RETRY_COOLDOWN, Recycling};
