//! Waiting for GC to reclaim a write burst's page versions.
//!
//! A GC test writes a burst, then checks that the `pages` row count falls
//! back. The peak to compare against is the highest count seen while
//! waiting, not a sample taken when the compute's `CHECKPOINT` returns: the
//! pageserver is still ingesting and flushing the burst then, and rows keep
//! landing for seconds afterwards (one such sample read 5.7k of a 44k peak).

use std::time::Duration;

use tokio::time::sleep;

use crate::db;

/// What [`wait_reclaimed`] saw.
pub struct Reclaim {
    /// Highest `pages` count observed, the sample passed in included.
    pub peak: i64,
    /// `pages` count at the last sample.
    pub now: i64,
    /// Queue depth at the last sample.
    pub queue: i64,
    /// The queue was at most `queue_max`, and the count was below `peak`
    /// and unchanged for [`SETTLE_SAMPLES`] samples in a row: the burst had
    /// finished landing and the drain had finished trimming it.
    pub settled: bool,
}

/// Consecutive unchanged one-second samples that count as settled.
pub const SETTLE_SAMPLES: u32 = 3;

/// Poll `pages_sql` and `queue_sql` (each a `SELECT count(*)` on `yb`) once
/// a second for up to `secs`, starting from `first_sample`, until the count
/// settles below the peak with the queue at most `queue_max`.
pub async fn wait_reclaimed(
    yb: &str,
    pages_sql: &str,
    queue_sql: &str,
    first_sample: i64,
    queue_max: i64,
    secs: u32,
) -> anyhow::Result<Reclaim> {
    let mut r = Reclaim {
        peak: first_sample,
        now: first_sample,
        queue: i64::MAX,
        settled: false,
    };
    let mut unchanged = 0;
    for _ in 0..secs {
        sleep(Duration::from_secs(1)).await;
        let prev = r.now;
        r.now = db::query_i64(yb, pages_sql).await?;
        r.queue = db::query_i64(yb, queue_sql).await?;
        r.peak = r.peak.max(r.now);
        unchanged = if r.now == prev { unchanged + 1 } else { 0 };
        if r.queue <= queue_max && r.now < r.peak && unchanged >= SETTLE_SAMPLES {
            r.settled = true;
            break;
        }
    }
    Ok(r)
}
