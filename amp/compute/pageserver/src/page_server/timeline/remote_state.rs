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
//!   UPSERT, so every record with LSN ≤ this is durably
//!   in `amp_storage.pages` AND the `branch_state` row has been
//!   updated. Used for SK feedback, restart resume position and the
//!   HTTP API. Seeded at attach from the durable
//!   `branch_state.consistent_lsn` column so it's correct from the
//!   first feedback.
//! - **`shutting_down`** — set by `Timeline::shutdown`, read only by
//!   `await_initial_logical_size` to skip the computation on a timeline
//!   that is going away. It was called `deleting`, which it never was:
//!   every shutdown sets it, and nothing in attach or the walreceiver
//!   reads it.
//!
//! GC blocking reasons stay in `gc_block::GcBlock` (per-tenant
//! Mutex<HashMap<BranchId, …>>) — that struct already owns the
//! runtime authority; the RTC schedule_*_gc_block_reason calls
//! are only there to persist to `index_part.json`, which yb-amp
//! doesn't write. Step 4 removes those calls.

use std::sync::atomic::AtomicBool;

use utils::lsn::AtomicLsn;

pub struct TimelineRemoteState {
    /// Durable consistency watermark. `Arc` so `PageWriter` can hold a
    /// clone and bump it without touching this struct on the hot flush
    /// path. See module-level doc for the contract.
    pub consistent_lsn: std::sync::Arc<AtomicLsn>,
    pub shutting_down: AtomicBool,
}

impl TimelineRemoteState {
    pub fn new() -> Self {
        Self {
            consistent_lsn: std::sync::Arc::new(AtomicLsn::new(0)),
            shutting_down: AtomicBool::new(false),
        }
    }
}

impl Default for TimelineRemoteState {
    fn default() -> Self {
        Self::new()
    }
}
