//! SLRU (clog) trim reclamation: every write-commit stamps a SETBIT
//! delta on the active `pg_xact` (clog) SLRU page, so a burst of tiny
//! txns piles deltas onto one block. The materialiser periodically
//! images that block (a fresh `will_init` anchor), and the drain then
//! trims the deltas below the anchor (`gc_kind = trim_slru`). Asserts the
//! busiest clog block's version count is *reclaimed* — it stays far below
//! one row per commit.
//!
//! NOTE: this is timing-dependent on the background materialiser imaging
//! the clog page; the bound is a heuristic, not an exact count. If it
//! never settles it `bail!`s with the observed numbers — which would
//! itself be a finding (SLRU trim not reclaiming). Dedicated branch so
//! the clog signal is this test's alone.
//!
//! Doubles as the regression test for #420: it runs on a dedicated
//! BRANCH, whose active clog page carries only SETBIT deltas because its
//! `ZEROPAGE` anchor predates the fork and lives under the ancestor's
//! `branch_id`. Before the fix the materialiser read that chain
//! branch-locally, handed walredo a base-less chain, and fatal-aborted
//! the pageserver at `image_chain_limit_slru` (1000) deltas — this test
//! failed with `could not connect to pageserver` long before reaching
//! its trim assertion.

use std::time::Duration;

use anyhow::bail;
use tokio::time::sleep;

use crate::cluster::{Cluster, step};
use crate::db;

const COMMITS: i64 = 3000;

/// `image_chain_limit_slru` default. A healthy reclaimed block still holds up
/// to this many deltas — the ones above the anchor, which trim cannot touch.
const SLRU_IMAGE_CHAIN_LIMIT: i64 = 1000;

/// Reclamation bound. One delta row per commit, and only the trim drain
/// deletes them (`gc_act_drop_slru` needs ~1M xids to fire), so landing under
/// this means trim ran. Sits near the unreclaimed end (`COMMITS`) rather than
/// the midpoint: unreclaimed is 3x the healthy value, never a near miss, so
/// the distance above `SLRU_IMAGE_CHAIN_LIMIT` is better spent as margin
/// against a merely-slow drain.
const MAX_RECLAIMED: i64 = 2 * SLRU_IMAGE_CHAIN_LIMIT;

pub async fn run(c: &Cluster) -> anyhow::Result<()> {
    let branch_pg_port = c.compute_port() + 260;
    let yb = db::yb_conn(c.yb_host(), c.yb_port(), c.db_name());

    step("Dedicated branch + endpoint");
    let b = c.branch_create_named("gc_st", c.branch())?;
    c.endpoint_create_on_branch("ep_gc_st", &b, branch_pg_port)?;
    let conn = db::compute_conn(branch_pg_port);

    step("Churn many committed txns so the active clog page piles up deltas");
    // A server-side loop that COMMITs per row: one clog SETBIT delta each.
    // (COMMIT can't run inside the implicit txn of a multi-statement batch,
    // so the procedure body + the CALL go in separate batches.)
    db::batch(&conn, "CREATE TABLE gc_st (id BIGINT)").await?;
    db::batch(
        &conn,
        "CREATE PROCEDURE gc_st_churn(n INT) LANGUAGE plpgsql AS $$
         BEGIN FOR i IN 1..n LOOP INSERT INTO gc_st VALUES (i); COMMIT; END LOOP; END $$;",
    )
    .await?;
    db::batch(&conn, &format!("CALL gc_st_churn({COMMITS})")).await?;
    db::batch(&conn, "CHECKPOINT").await?;

    // Witness that the churn actually ran, read from the *user table* rather
    // than from `slru_pages`. Nothing trims `gc_st`, so its row count is a
    // stable record of how many txns committed. A floor on the surviving
    // clog row count could not carry this: trim drives that number toward
    // zero by design, so a healthy, aggressively-trimmed run can leave a
    // handful of rows and would trip any such floor spuriously — the same
    // mistake as the peak-relative form this replaced.
    let committed = db::query_i64(&conn, "SELECT count(*) FROM gc_st").await?;
    if committed != COMMITS {
        bail!("churn did not commit {COMMITS} txns: gc_st holds {committed} rows");
    }

    // Total clog rows for this branch (kind=0 = Clog), SUMMED over blocks
    // rather than the busiest block's count. `COMMITS` xids nominally fit one
    // 32768-xid clog page, but the branch inherits the parent's xid counter,
    // so the churn can straddle a page boundary and split across two blocks.
    // A per-block max would then read low with nothing reclaimed at all —
    // passing vacuously. The sum is straddle-proof: however the xids divide,
    // unreclaimed still totals >= COMMITS.
    let clog_rows = format!(
        "SELECT count(*) FROM amp_storage.slru_pages \
         WHERE branch_id = '{b}' AND kind = 0"
    );
    let observed = db::query_i64(&yb, &clog_rows).await?;

    // Bounded wait for the bound to be met, not a fixed drain. Reclamation is
    // asynchronous (the materialiser images the block, then the drain trims
    // below that anchor), so on a slow run this has to advance the GC cutoff
    // and wait; on a fast one the churn phase already finished reclaiming and
    // `observed` is under the bound on entry, so the loop exits without
    // iterating. That early exit is a legitimate pass, not a skipped
    // assertion: the real check is `settled < MAX_RECLAIMED` below, and
    // getting there from COMMITS inserted rows is only possible if trim
    // deleted rows. The loop's job is to give a slow drain time to finish,
    // never to be the assertion itself.
    step("Keep the WAL frontier moving; the drain trims the clog deltas below the anchor");
    let mut settled = observed;
    for i in 0..40 {
        if settled < MAX_RECLAIMED {
            break;
        }
        // Advance the cutoff (a side table so gc_st's own pages stay put),
        // and nudge the materialiser with a checkpoint.
        db::batch(
            &conn,
            &format!(
                "CREATE TABLE IF NOT EXISTS gc_st_churn_side (id INT PRIMARY KEY, n INT);
                 INSERT INTO gc_st_churn_side VALUES ({i}, {i});
                 CHECKPOINT;"
            ),
        )
        .await?;
        sleep(Duration::from_secs(1)).await;
        settled = db::query_i64(&yb, &clog_rows).await?;
    }

    let _ = c.yb(&["branch", "drop", "--branch-id", &b]);
    c.yb_quiet(&["pageserver", "stop", "ep_gc_st", "-m", "immediate"]);
    if settled >= MAX_RECLAIMED {
        bail!(
            "clog trim did not reclaim: {COMMITS} committed txns left {settled} clog rows \
             on this branch (first sample {observed}), still >= {MAX_RECLAIMED} after 40s \
             of drain — materialiser not imaging the clog page, or trim not firing"
        );
    }
    Ok(())
}
