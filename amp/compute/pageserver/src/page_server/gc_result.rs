use std::time::Duration;

use anyhow::Result;
use serde::Serialize;

///
/// Result of performing GC
///
#[derive(Default, Serialize, Debug)]
pub struct GcResult {
    /// yb-amp PageServer GC: candidate rows dequeued from `pending_gc`
    /// this iteration. Can exceed the acted candidates — the block-range
    /// delete also clears a visited block's duplicate/straggler rows.
    pub candidates_dequeued: u64,
    /// yb-amp PageServer GC: rows deleted by the drain across
    /// `amp_storage.pages`, `slru_pages`, and `meta` (trim / drop / shrink).
    pub rows_deleted: u64,

    #[serde(serialize_with = "serialize_duration_as_millis")]
    pub elapsed: Duration,
}

// helper function for `GcResult`, serializing a `Duration` as an integer number of milliseconds
fn serialize_duration_as_millis<S>(d: &Duration, serializer: S) -> Result<S::Ok, S::Error>
where
    S: serde::Serializer,
{
    d.as_millis().serialize(serializer)
}
