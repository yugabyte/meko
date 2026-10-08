//! Thin, safe client to a yb-tserver's `ThinClientService.Perform`.
//!
//! A minimal wrapper over the `libyb_thin_client` C ABI (`shim/yb_thin_client.h`):
//! the shim sends `Perform` to a tserver and the tserver does the tablet
//! routing, so there is no metacache / batcher / tablet invoker on this
//! side. All YugabyteDB C++ lives behind the C boundary; this crate only
//! marshals arguments, bridges the shim's callbacks to `oneshot`s, and
//! maps status codes onto [`YbError`]. See `README.md` for the design and
//! the C++ shim spec.

mod barren;
mod client;
mod error;
mod ffi;
mod metrics;
mod recycle;
mod tls_watch;

pub use barren::{StuckContinuation, THIN_SCAN_BARREN_ROUNDS, check_barren_round};
pub use client::{
    BindValue, ColKind, Column, CondOp, RangeRead, ReadBatch, ReadOp, ReadPage, ReadResult, Row,
    TableHandle, TlsOpts, TsClient, TsClientConfig, UpsertRow, Value, ValueType, WriteDeadline,
};
pub use error::YbError;
pub use metrics::init as init_metrics;
pub use recycle::{RECYCLE_RETRY_COOLDOWN, Recycling};
