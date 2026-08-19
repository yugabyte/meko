use std::time::Duration;

use anyhow::Result;
use serde::Serialize;

/// The state of the candidate queue a drain tick left behind, which is what
/// picks the next tick's cadence ([`Self::next_sleep`]). The
/// default is `NothingDue`: a tick that skipped the drain (disabled, lease
/// grace, GC block) did no work either, so it backs off like an idle one.
#[derive(Default, Debug, Clone, Copy, PartialEq)]
pub enum GcTickOutcome {
    /// Nothing was due, so there was nothing to act on.
    #[default]
    NothingDue,
    /// Everything due was acted and dequeued.
    CaughtUp,
    /// Stopped on `gc_drain_limit` with due candidates still queued.
    BacklogRemains,
}

/// The adaptive sleep's bounds, as ratios of `gc_period` (the base): a
/// backlogged tick retries at the floor, and each idle tick doubles up to the
/// ceiling. 5s and 4min at the 30s default.
const GC_SLEEP_FLOOR_DIVISOR: u32 = 6;
const GC_SLEEP_CEILING_MULTIPLE: u32 = 8;
/// Absolute floor: a sixth of a short `gc_period` re-selects every few hundred ms.
const GC_SLEEP_FLOOR_MIN: Duration = Duration::from_secs(1);

impl GcTickOutcome {
    /// `gc_drain_ticks_total` label — the spelling dashboards already query,
    /// so it outlives a variant rename.
    pub(crate) fn label(self) -> &'static str {
        match self {
            GcTickOutcome::NothingDue => "empty",
            GcTickOutcome::CaughtUp => "drained",
            GcTickOutcome::BacklogRemains => "capped",
        }
    }

    /// How long the drain loop (`page_server::tasks::gc_loop`) sleeps after
    /// this tick, from `period` as the base. `last_idle` is the sleep the
    /// previous `NothingDue` tick picked, `None` once any tick did work — so an
    /// idle run doubles from the base and one working tick resets to it.
    pub(crate) fn next_sleep(self, period: Duration, last_idle: Option<Duration>) -> Duration {
        match self {
            GcTickOutcome::CaughtUp => period,
            // .min so a sub-second base can't outwait a caught-up tick.
            GcTickOutcome::BacklogRemains => (period / GC_SLEEP_FLOOR_DIVISOR)
                .max(GC_SLEEP_FLOOR_MIN)
                .min(period),
            GcTickOutcome::NothingDue => last_idle
                .unwrap_or(period)
                .saturating_mul(2)
                .min(period.saturating_mul(GC_SLEEP_CEILING_MULTIPLE)),
        }
    }
}

///
/// Result of performing GC
///
#[derive(Default, Serialize, Debug)]
pub struct GcResult {
    /// Candidate rows dequeued from `pending_gc` this iteration. Can exceed
    /// the acted candidates — the block-range delete also clears a visited
    /// block's duplicate/straggler rows.
    pub candidates_dequeued: u64,
    /// Rows deleted by the drain across `amp_storage.pages`, `slru_pages`,
    /// and `meta` (trim / drop / shrink).
    pub rows_deleted: u64,
    /// The queue state the tick left, which sets the next sleep. Internal to
    /// the drain loop, so it stays out of the `do_gc` response.
    #[serde(skip)]
    pub outcome: GcTickOutcome,

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

#[cfg(test)]
mod tests {
    use super::*;

    #[test]
    fn next_sleep_adapts_to_the_queue_the_tick_left() {
        let base = Duration::from_secs(30);
        assert_eq!(
            GcTickOutcome::CaughtUp.next_sleep(base, None),
            Duration::from_secs(30)
        );
        assert_eq!(
            GcTickOutcome::BacklogRemains.next_sleep(base, None),
            Duration::from_secs(5)
        );

        // An idle run doubles from the base up to the ceiling, and stays there.
        let mut idle = None;
        for expected in [60, 120, 240, 240] {
            let sleep = GcTickOutcome::NothingDue.next_sleep(base, idle);
            assert_eq!(sleep, Duration::from_secs(expected));
            idle = Some(sleep);
        }

        // The first working tick resets to the base: a backlogged tick still
        // picks the floor even after a long backoff.
        assert_eq!(
            GcTickOutcome::BacklogRemains.next_sleep(base, idle),
            Duration::from_secs(5)
        );

        // A short base (the integration suite runs gc_period = 1s) hits the
        // absolute floor, and never sleeps longer than the base itself.
        let short = Duration::from_secs(1);
        assert_eq!(GcTickOutcome::BacklogRemains.next_sleep(short, None), short);
        assert_eq!(
            GcTickOutcome::BacklogRemains.next_sleep(Duration::from_millis(500), None),
            Duration::from_millis(500)
        );
    }
}
