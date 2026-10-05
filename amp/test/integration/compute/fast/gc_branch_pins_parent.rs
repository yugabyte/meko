//! Branch safety under GC: a child branch reads its ancestor's data as
//! of the branch point. The child's existence pins the parent's GC
//! cutoff, so even after the parent overwrites those same blocks the
//! branch-point versions survive — the child reads them back
//! byte-for-byte on a COLD read (endpoint restarted, answer from the
//! store). The child is dropped at the end only as teardown, and every
//! read this test asserts on is captured BEFORE that drop — the drop
//! releases the pin (the controller's reaper deletes the child's
//! `branch_state` row, which is what pinned the parent), so the parent's
//! cutoff is free to advance past the branch point afterwards. That
//! reclamation is `gc_branch_drop_reclaims`'s subject, not this test's.

use std::time::Duration;

use anyhow::bail;
use tokio::time::sleep;

use crate::cluster::{Cluster, step};
use crate::db;

pub async fn run(c: &Cluster) -> anyhow::Result<()> {
    let main = db::compute_conn(c.compute_port());
    let branch_pg_port = c.endpoint_port();

    step("Seed the ancestor table on main, capture the branch-point checksum");
    db::batch(
        &main,
        "DROP TABLE IF EXISTS gc_pin;
         CREATE TABLE gc_pin (id INT PRIMARY KEY, payload TEXT NOT NULL);
         INSERT INTO gc_pin SELECT g, 'orig-' || md5(g::text) FROM generate_series(1, 600) g;
         CHECKPOINT;",
    )
    .await?;
    let want_sum = db::query_text(
        &main,
        "SELECT md5(string_agg(payload, ',' ORDER BY id)) FROM gc_pin",
    )
    .await?;

    step("Branch off main and start the child endpoint (pins the parent's cutoff)");
    let b1 = c.branch_create_named("gc_pin_b1", c.branch())?;
    c.endpoint_create_on_branch("ep_gc_pin", &b1, branch_pg_port)?;
    let bconn = db::compute_conn(branch_pg_port);

    step("Parent overwrites those same blocks hard and its drain runs");
    for i in 0..15 {
        db::batch(&main, &format!("UPDATE gc_pin SET payload = 'new{i}-' || md5(payload)")).await?;
    }
    db::batch(&main, "CHECKPOINT").await?;
    // The pin must keep the parent's drain from trimming the branch-point
    // versions, so the assertion needs drain ticks to have run after the churn —
    // but not many: one tick would trim if the pin were broken. Wait on the
    // parent's own tick counter rather than the clock, because the cadence is
    // adaptive and the pin itself keeps every tick empty (nothing is due below a
    // pinned cutoff, however much the parent churns), so the drain sits at its
    // `8 x gc_period` ceiling here and a fixed 10 s sleep can buy one tick.
    let ps = c.ps_http_port();
    let ticks_before = c.metric_sum(ps, "pageserver_yb_gc_drain_ticks_total").await?;
    let mut ticks = ticks_before;
    for _ in 0..60 {
        sleep(Duration::from_secs(1)).await;
        ticks = c.metric_sum(ps, "pageserver_yb_gc_drain_ticks_total").await?;
        if ticks - ticks_before >= 3.0 {
            break;
        }
    }
    if ticks - ticks_before < 3.0 {
        bail!(
            "parent drain ran only {} ticks; the pin assertion would be vacuous",
            ticks - ticks_before
        );
    }

    step("Cold child read: restart the branch endpoint, re-read from the store");
    c.yb(&["endpoint", "stop", "ep_gc_pin"])?;
    c.yb(&["endpoint", "start", "ep_gc_pin"])?;
    for _ in 0..30 {
        if db::query_i64(&bconn, "SELECT 1::bigint").await.is_ok() {
            break;
        }
        sleep(Duration::from_secs(1)).await;
    }
    let child_sum = db::query_text(
        &bconn,
        "SELECT md5(string_agg(payload, ',' ORDER BY id)) FROM gc_pin",
    )
    .await;

    // Tear down the child (branch drop + stop its pageserver) BEFORE
    // asserting, so a checksum mismatch can't leave the child endpoint
    // running. The checksum was already read above, so whether or not the
    // drop goes on to unpin main — the reaper deleting the child's
    // branch_state row is what would — cannot affect the verdict below.
    let _ = c.yb(&["branch", "drop", "--branch-id", &b1]);
    c.yb_quiet(&["pageserver", "stop", "ep_gc_pin", "-m", "immediate"]);

    let child_sum = child_sum?;
    if child_sum != want_sum {
        bail!("child no longer sees the branch-point data — GC trimmed a version a branch needs");
    }
    Ok(())
}
