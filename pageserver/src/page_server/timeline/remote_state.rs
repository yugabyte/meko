//! Lightweight per-timeline state that's currently owned by
//! `RemoteTimelineClient` (RTC). Step 1 of the RTC unwind (see
//! #42): introduce this struct alongside the existing
//! `Timeline.remote_client` field. Subsequent steps migrate each
//! caller of the LIVE RTC methods to read from here, then delete
//! the RTC field + module.
//!
//! After migration this owns:
//! - **`consistent_lsn`** — the `Arc<AtomicLsn>` slot exposed as
//!   `Timeline::get_consistent_lsn()`. `PageWriter` installs it via
//!   `set_consistent_lsn_slot` and bumps it on every successful
//!   gen-fenced UPSERT, so every record with LSN ≤ this is durably
//!   in `pageserver.pages` AND the `timeline_state` row has been
//!   updated. Used for SK feedback (`remote_consistent_lsn` wire
//!   field), restart resume position, and the HTTP API
//!   `disk_consistent_lsn` field. Seeded at attach from the durable
//!   `timeline_state.consistent_lsn` column so it's correct from the
//!   first feedback.
//! - **`archived`** — set by the storage_ctl
//!   "timeline_archival" RPC. While set, the periodic flusher
//!   skips its YB INSERT (the timeline is offline; no new WAL is
//!   coming).
//! - **`invisible`** — set by storage_ctl for tenants pending
//!   detach. Cosmetic/visibility-only today.
//! - **`deleting`** — set when a timeline-delete operation is
//!   in progress. Blocks attach + walreceiver feedback.
//!
//! GC blocking reasons stay in `gc_block::GcBlock` (per-tenant
//! Mutex<HashMap<BranchId, …>>) — that struct already owns the
//! runtime authority; the RTC schedule_*_gc_block_reason calls
//! are only there to persist to `index_part.json`, which yb-light
//! doesn't write. Step 4 removes those calls.

use std::sync::atomic::AtomicBool;

use utils::lsn::AtomicLsn;

pub struct TimelineRemoteState {
    /// Durable consistency watermark. `Arc` so `PageWriter` can hold a
    /// clone and bump it without touching this struct on the hot flush
    /// path. See module-level doc for the contract.
    pub consistent_lsn: std::sync::Arc<AtomicLsn>,
    pub archived: AtomicBool,
    pub invisible: AtomicBool,
    pub deleting: AtomicBool,
}

impl TimelineRemoteState {
    pub fn new() -> Self {
        Self {
            consistent_lsn: std::sync::Arc::new(AtomicLsn::new(0)),
            archived: AtomicBool::new(false),
            invisible: AtomicBool::new(false),
            deleting: AtomicBool::new(false),
        }
    }
}

impl Default for TimelineRemoteState {
    fn default() -> Self {
        Self::new()
    }
}
