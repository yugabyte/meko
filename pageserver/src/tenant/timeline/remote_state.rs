//! Lightweight per-timeline state that's currently owned by
//! `RemoteTimelineClient` (RTC). Step 1 of the RTC unwind (see
//! `tasks/4.d`): introduce this struct alongside the existing
//! `Timeline.remote_client` field. Subsequent steps migrate each
//! caller of the LIVE RTC methods to read from here, then delete
//! the RTC field + module.
//!
//! After migration this owns:
//! - **`rc_lsn`** — the `Arc<AtomicLsn>` slot that
//!   `walreceiver_connection::PageserverFeedback` exposes to the
//!   safekeeper as `remote_consistent_lsn`. `PageWriter` installs
//!   this slot via `set_remote_consistent_lsn_slot` and bumps it
//!   on every successful flush so the SK can truncate WAL.
//! - **`archived`** — set by the storage_ctl
//!   "timeline_archival" RPC. While set, the periodic flusher
//!   skips its YB INSERT (the timeline is offline; no new WAL is
//!   coming).
//! - **`invisible`** — set by storage_ctl for tenants pending
//!   detach. Cosmetic/visibility-only today.
//! - **`deleting`** — set when a timeline-delete operation is
//!   in progress. Blocks attach + walreceiver feedback.
//! - **`gc_blocking_reasons`** — set of reasons GC must stay
//!   blocked (e.g. an in-progress detach-ancestor). Modified by
//!   `gc_block.rs`'s insert/remove helpers.

use std::sync::Mutex;
use std::sync::atomic::AtomicBool;

use enumset::EnumSet;
use utils::lsn::AtomicLsn;

use crate::tenant::remote_timeline_client::index::GcBlockingReason;

pub struct TimelineRemoteState {
    /// `remote_consistent_lsn` slot. `Arc` so `PageWriter` can hold a
    /// clone and bump it without touching this struct on the hot
    /// flush path.
    pub rc_lsn: std::sync::Arc<AtomicLsn>,
    pub archived: AtomicBool,
    pub invisible: AtomicBool,
    pub deleting: AtomicBool,
    pub gc_blocking_reasons: Mutex<EnumSet<GcBlockingReason>>,
}

impl TimelineRemoteState {
    pub fn new() -> Self {
        Self {
            rc_lsn: std::sync::Arc::new(AtomicLsn::new(0)),
            archived: AtomicBool::new(false),
            invisible: AtomicBool::new(false),
            deleting: AtomicBool::new(false),
            gc_blocking_reasons: Mutex::new(EnumSet::empty()),
        }
    }
}

impl Default for TimelineRemoteState {
    fn default() -> Self {
        Self::new()
    }
}
