//! WAL-retention contract for the safekeeper.
//!
//! `calc_horizon_lsn` decides which `amp_storage.wals` rows get deleted.
//! A horizon that is too low only wastes space; one that is too high
//! deletes WAL the pageserver still needs, and there is no S3 fallback
//! to refetch it from — the loss is one-way. Nothing in the suite
//! watched that boundary.
//!
//! The three properties are asserted through the floor's observable
//! effects — which segments survive, and what the safekeeper reports
//! after a restart — never through the column it happens to be stored
//! in, so the contract holds across a change of that storage.

use std::time::Duration;

use anyhow::{Context, bail};

use crate::cluster::{Cluster, mark_progress, step};
use crate::db;
use crate::tee_println;

/// Rows per batch, sized so [`BATCHES`] of them span several 16 MiB WAL
/// segments and there is genuinely something below the frontier to trim.
const BATCH_ROWS: i64 = 20_000;
const BATCHES: usize = 12;

/// The pageserver ingests asynchronously and the manager only re-checks
/// the horizon every 300 ms; both frontier and trim are polled, not
/// assumed.
const SETTLE_TIMEOUT: Duration = Duration::from_secs(120);
/// Budget for the first trim. Deliberately past the launcher's 5-minute
/// `control_file_save_interval`: while the horizon is driven by the
/// *persisted* floor, no trim can happen sooner than that save. Sourcing
/// the floor from the pageserver's `consistent_lsn` instead makes the
/// trim prompt, and this budget stops mattering.
const TRIM_TIMEOUT: Duration = Duration::from_secs(420);
const POLL: Duration = Duration::from_secs(2);

pub async fn run(c: &Cluster) -> anyhow::Result<()> {
    let compute = db::compute_conn(c.compute_port());
    let yb = db::yb_conn(c.yb_host(), c.yb_port(), c.db_name());
    let branch = c.branch().to_string();

    step("Seed the retention table");
    db::batch(
        &compute,
        "DROP TABLE IF EXISTS wal_retention; \
         CREATE TABLE wal_retention (a BIGINT, b TEXT);",
    )
    .await?;

    let seg_size = wal_seg_size(&yb, &branch).await?;
    let min_segno_before = min_segno(&yb, &branch).await?;

    step("Drive writes across several WAL segments");
    for _ in 0..BATCHES {
        db::batch(
            &compute,
            &format!(
                "INSERT INTO wal_retention SELECT g, repeat('x', 400) \
                 FROM generate_series(1, {BATCH_ROWS}) g"
            ),
        )
        .await?;
    }
    let rows_written = db::query_i64(&compute, "SELECT count(*) FROM wal_retention").await?;
    let sum_written = db::query_i64(&compute, "SELECT COALESCE(sum(a), 0)::BIGINT FROM wal_retention").await?;

    // ---- 1. trimming happens at all -------------------------------------
    //
    // A floor stuck at zero trims nothing and this never fires, which is
    // the regression a lost/never-restored floor produces.
    step("Wait for the pageserver to ingest and the safekeeper to trim");
    // An empty table beforehand reads as segment 0, so "trimmed" is the
    // same test either way: the surviving floor segment has moved up.
    let before = min_segno_before.unwrap_or(0);
    let deadline = tokio::time::Instant::now() + TRIM_TIMEOUT;
    let min_segno_after = loop {
        let m = min_segno(&yb, &branch).await?;
        if let Some(after) = m
            && after > before
        {
            break after;
        }
        if tokio::time::Instant::now() > deadline {
            bail!(
                "safekeeper never trimmed any WAL in {:?} — min(segno) still {:?} \
                 (frontier {:?}); the retention floor is not advancing",
                TRIM_TIMEOUT,
                m,
                c.consistent_lsn(&branch).await?,
            );
        }
        mark_progress();
        tokio::time::sleep(POLL).await;
    };
    tee_println!("trimmed: min(segno) {min_segno_before:?} -> {min_segno_after}");

    // ---- 2. trimming never passes the durable frontier ------------------
    //
    // The safety direction. The segment holding `consistent_lsn` — the
    // pageserver's durability commit point — must still be
    // present, or WAL it may still replay has been deleted.
    let frontier = c
        .consistent_lsn(&branch)
        .await?
        .context("branch_state.consistent_lsn is NULL after ingest")?;
    let frontier_segno = frontier / seg_size;
    if min_segno_after > frontier_segno {
        bail!(
            "WAL trimmed past the durable frontier: min(segno)={min_segno_after} > \
             frontier segno {frontier_segno} (consistent_lsn={frontier}, \
             wal_seg_size={seg_size}) — the pageserver can no longer replay from \
             its own resume point"
        );
    }

    // ---- 3. the floor survives a restart taken with no pageserver -------
    //
    // With the pageserver down there is no feedback to relearn from, and
    // `laggard_lsn()` returns None so nothing else clamps the horizon:
    // whatever the safekeeper reports here came purely from what it had
    // persisted. Zero would mean trimming stalls until the pageserver
    // returns.
    step("Stop the pageserver, restart the safekeeper, re-read the floor");
    c.yb(&["pageserver", "stop"])?;
    // Re-read with the pageserver down: it keeps ingesting after the trim
    // check above, so the earlier snapshot is already stale. Frozen now.
    let settled = c
        .consistent_lsn(&branch)
        .await?
        .context("branch_state.consistent_lsn is NULL")?;
    c.yb(&["safekeeper", "restart"])?;

    let floor = wait_for_floor(c, &branch).await?;
    if floor == 0 {
        bail!(
            "safekeeper restarted to a zero retention floor with the pageserver \
             down — the durable frontier ({settled}) was not recovered, so WAL \
             trimming stalls until the pageserver reattaches"
        );
    }
    if floor > settled {
        bail!(
            "safekeeper restored a retention floor ({floor}) above the durable \
             frontier ({settled}) — it would license trimming WAL the pageserver \
             has not ingested"
        );
    }
    tee_println!("floor after restart with no pageserver: {floor} (frontier {settled})");

    // ---- 4. nothing needed was actually cut -----------------------------
    step("Restart the pageserver and read the table back");
    c.yb(&["pageserver", "start"])?;

    let deadline = tokio::time::Instant::now() + SETTLE_TIMEOUT;
    loop {
        match db::query_i64(&compute, "SELECT count(*) FROM wal_retention").await {
            Ok(n) if n == rows_written => break,
            Ok(n) if tokio::time::Instant::now() > deadline => {
                bail!("after pageserver restart: {n} rows, expected {rows_written}")
            }
            Err(_) if tokio::time::Instant::now() > deadline => {
                bail!("table unreadable after pageserver restart within {SETTLE_TIMEOUT:?}")
            }
            _ => {
                mark_progress();
                tokio::time::sleep(POLL).await;
            }
        }
    }
    let sum_read = db::query_i64(&compute, "SELECT COALESCE(sum(a), 0)::BIGINT FROM wal_retention").await?;
    if sum_read != sum_written {
        bail!("checksum drift after pageserver restart: {sum_read} != {sum_written}");
    }

    Ok(())
}

/// `min(segno)` still present for the branch; `None` when the table has
/// no rows for it yet.
async fn min_segno(yb: &str, branch: &str) -> anyhow::Result<Option<i64>> {
    let client = db::connect(yb).await.context("connect to the run's YB db")?;
    let row = client
        .query_one(
            "SELECT min(segno)::BIGINT FROM amp_storage.wals WHERE branch_id = $1::text::uuid",
            &[&branch],
        )
        .await
        .context("min(segno) query")?;
    Ok(row.get(0))
}

async fn wal_seg_size(yb: &str, branch: &str) -> anyhow::Result<i64> {
    let client = db::connect(yb).await.context("connect to the run's YB db")?;
    let row = client
        .query_one(
            "SELECT wal_seg_size FROM amp_storage.branch_state WHERE branch_id = $1::text::uuid",
            &[&branch],
        )
        .await
        .context("wal_seg_size query")?;
    let size: Option<i32> = row.get(0);
    match size {
        Some(s) if s > 0 => Ok(s as i64),
        other => bail!("branch {branch} has no usable wal_seg_size: {other:?}"),
    }
}

/// Poll until the safekeeper exports its retention floor for this branch.
/// Read from `/metrics` rather than a table so the assertion does not depend
/// on where — or whether — the floor is persisted.
async fn wait_for_floor(c: &Cluster, branch: &str) -> anyhow::Result<i64> {
    let deadline = tokio::time::Instant::now() + SETTLE_TIMEOUT;
    loop {
        if let Some(v) = c
            .branch_metric(c.sk_http_port(), "safekeeper_consistent_lsn", branch)
            .await?
        {
            return Ok(v);
        }
        if tokio::time::Instant::now() > deadline {
            bail!(
                "safekeeper never exported safekeeper_consistent_lsn for \
                 branch {branch} after restart"
            );
        }
        mark_progress();
        tokio::time::sleep(POLL).await;
    }
}
