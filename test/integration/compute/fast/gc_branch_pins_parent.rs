//! Branch safety under GC: a child branch reads its ancestor's data as
//! of the branch point. The child's existence pins the parent's GC
//! cutoff, so even after the parent overwrites those same blocks the
//! branch-point versions survive — the child reads them back
//! byte-for-byte on a COLD read (endpoint restarted, answer from the
//! store). The child is dropped at the end only as teardown: the drop
//! does NOT release the pin today (nothing marks the child's
//! timeline_state deleted), so parent reclaim after a child drop is out
//! of scope here — a separate revision handles that.

use std::time::Duration;

use anyhow::bail;
use tokio::time::sleep;

use crate::cluster::{Cluster, step};
use crate::db;

pub async fn run(c: &Cluster) -> anyhow::Result<()> {
    let main = db::compute_conn(c.compute_port());
    let branch_pg_port = c.compute_port() + 130;

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
    let b1 = c.branch_create_named("gc_pin_b1", "main")?;
    let ps_id = c.endpoint_create_on_branch("ep_gc_pin", "gc_pin_b1", branch_pg_port)?;
    let bconn = db::compute_conn(branch_pg_port);

    step("Parent overwrites those same blocks hard and its drain runs");
    for i in 0..15 {
        db::batch(&main, &format!("UPDATE gc_pin SET payload = 'new{i}-' || md5(payload)")).await?;
    }
    db::batch(&main, "CHECKPOINT").await?;
    // Give the drain several ticks to run against the churned parent; the
    // pin must keep it from trimming the branch-point versions.
    sleep(Duration::from_secs(10)).await;

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
    // running. NB: the drop does NOT unpin main — nothing marks the
    // child's timeline_state deleted yet — so the pin outlives this test.
    let _ = c.yb(&["branch", "drop", "--branch-id", &b1]);
    c.yb_quiet(&["pageserver", "stop", "--id", &ps_id.to_string(), "-m", "immediate"]);

    let child_sum = child_sum?;
    if child_sum != want_sum {
        bail!("child no longer sees the branch-point data — GC trimmed a version a branch needs");
    }
    Ok(())
}
