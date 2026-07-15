//! Pure-Rust direct-to-tserver YugabyteDB client for the yb-amp hot
//! paths, speaking the pggate RPC protocol
//! (`yb.tserver.PgClientService`): connect config, Arc'd
//! [`TableHandle`], [`TsClient::upsert_batch`], [`RangeRead`] builder,
//! the `ysql_client`-style error taxonomy — with no native library
//! dependency.
//!
//! ## Transport
//!
//! The YB RPC protocol is a Kudu derivative: a TCP connection opens with
//! a 3-byte preamble `"YB\x01"` (no negotiation, no connection-context
//! call), then carries length-prefixed call frames — a protobuf
//! `RequestHeader` (call id, service/method, deadline) followed by the
//! request message; responses mirror it with a `ResponseHeader` plus
//! optional *sidecars* (out-of-band byte ranges; row data comes back
//! that way). See [`framing`] for the exact byte layout and
//! `DESIGN.md` for the full protocol notes.
//!
//! All calls multiplex over ONE connection: responses arrive in any
//! order and are matched to callers by `call_id` (a demux task with a
//! oneshot per in-flight call).
//!
//! ## Session
//!
//! `PgClientService` is stateful: `Heartbeat` opens a server-side
//! session that Perform calls reference by id, expiring after 60s
//! without keep-alives (the client heartbeats every 10s). Each session
//! numbers its Perform calls with a strictly-increasing `serial_no`. On
//! connection loss or session expiry the client transparently
//! reconnects (failing over across the configured tserver endpoints),
//! opens a fresh session, re-validates cached tables, and restarts the
//! serial counter; in-flight operations retry per the error taxonomy.
//!
//! ## Routing
//!
//! The tserver routes each op to its tablet using the `partition_key`
//! *carried in the op*: unlike ops built by the server's own client
//! layer, Perform ops must bring their own routing. The client computes
//! the 16-bit YB hash of the hash-column values ([`partition`], verified
//! against YSQL's `yb_hash_code()`) for writes and hash-eq reads; scan
//! continuations use the `next_partition_key` from the previous page's
//! paging state. All read-time selection and cross-tablet fan-out stay
//! server-side.
//!
//! Scope: exactly what the hot tables (`pageserver.pages`,
//! `pageserver.slru_pages`, `pageserver.meta`, `safekeeper.wals`) need —
//! hash-eq + range-bound clustered scans with paging and snapshot
//! pinning, and batched `PGSQL_UPSERT` writes on hash-partitioned
//! tables. Everything else stays on `ysql_client`.

pub mod client;
pub mod connection;
pub mod error;
pub mod framing;
pub mod ops;
pub mod partition;
pub mod rows;
pub mod session;
pub mod table;

/// Generated protobuf types, nested per proto package so prost's
/// `super::...` cross-package references resolve.
///
/// Lint allows cover prost output only: doc lints fire on comments
/// copied verbatim from the .proto files, and `large_enum_variant` on
/// generated oneof enums.
#[allow(
    clippy::doc_lazy_continuation,
    clippy::doc_overindented_list_items,
    clippy::large_enum_variant
)]
pub mod pb {
    /// Package `yb` — the `yb/common/*.proto` types (QLValuePB,
    /// PgsqlWriteRequestPB, AppStatusPB, ...) live directly here.
    pub mod yb {
        include!(concat!(env!("OUT_DIR"), "/yb.rs"));

        pub mod cdc {
            include!(concat!(env!("OUT_DIR"), "/yb.cdc.rs"));
        }
        pub mod consensus {
            include!(concat!(env!("OUT_DIR"), "/yb.consensus.rs"));
        }
        pub mod docdb {
            include!(concat!(env!("OUT_DIR"), "/yb.docdb.rs"));
        }
        pub mod dockv {
            include!(concat!(env!("OUT_DIR"), "/yb.dockv.rs"));
        }
        pub mod master {
            include!(concat!(env!("OUT_DIR"), "/yb.master.rs"));
        }
        pub mod rpc {
            include!(concat!(env!("OUT_DIR"), "/yb.rpc.rs"));
        }
        pub mod tablet {
            include!(concat!(env!("OUT_DIR"), "/yb.tablet.rs"));
        }
        pub mod tserver {
            include!(concat!(env!("OUT_DIR"), "/yb.tserver.rs"));
        }
        pub mod xcluster {
            include!(concat!(env!("OUT_DIR"), "/yb.xcluster.rs"));
        }
    }
}

pub use client::{ClientObserver, TsClient, TsClientConfig, ReadPage};
pub use error::{ErrorClass, YbError};
pub use ops::{BindValue, CondOp, RangeRead, UpsertRow};
pub use table::{ColumnKind, ColumnMeta, TableHandle, pgsql_table_id};
// Rows-sidecar (pg_doc_data) value model and decoder.
pub use rows::{Row, Value, ValueType, decode_rows};
