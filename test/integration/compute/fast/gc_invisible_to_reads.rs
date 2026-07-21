//! GC Part 1's cardinal invariant: trimming superseded versions is
//! invisible to reads. On a dedicated branch (so the reclaim signal is
//! isolated from other scenarios' churn on `main`), build deep per-block
//! version chains, let the drain reclaim them (pages rows drop, queue
//! drains), then read from a COLD compute and store: restart the
//! branch's PAGESERVER *and* recycle its compute (endpoint stop →
//! pageserver stop → pageserver start → endpoint start), so the read
//! misses the buffer cache AND is served by a freshly-attached
//! pageserver. The contents must be byte-identical to before GC.

use std::time::Duration;

use anyhow::bail;
use tokio::time::sleep;

use crate::cluster::{Cluster, step};
use crate::db;

pub async fn run(c: &Cluster) -> anyhow::Result<()> {
    let branch_pg_port = c.compute_port() + 150;
    let yb = db::yb_conn(c.yb_host(), c.yb_port(), c.db_name());

    step("Dedicated branch + endpoint");
    let b = c.branch_create_named("gc_invis", "main")?;
    let ps_id = c.endpoint_create_on_branch("ep_gc_invis", "gc_invis", branch_pg_port)?;
    let conn = db::compute_conn(branch_pg_port);
    let id = ps_id.to_string();
    let pages = format!("SELECT count(*) FROM pageserver.pages WHERE branch_id = '{b}'");
    let queue = format!("SELECT count(*) FROM pageserver.pending_gc WHERE branch_id = '{b}'");

    step("Seed a table and stack many versions per block");
    db::batch(
        &conn,
        "DROP TABLE IF EXISTS gc_invisible;
         CREATE TABLE gc_invisible (id INT PRIMARY KEY, payload TEXT NOT NULL);
         INSERT INTO gc_invisible SELECT g, md5(g::text) FROM generate_series(1, 800) g;",
    )
    .await?;
    for i in 0..18 {
        db::batch(&conn, &format!("UPDATE gc_invisible SET payload = md5(payload || '{i}')")).await?;
    }
    db::batch(&conn, "CHECKPOINT").await?;

    let want_sum = db::query_text(
        &conn,
        "SELECT md5(string_agg(payload, ',' ORDER BY id)) FROM gc_invisible",
    )
    .await?;
    let want_cnt = db::query_i64(&conn, "SELECT count(*) FROM gc_invisible").await?;

    step("Wait for the drain to reclaim (pages rows drop, queue drains)");
    let peak = db::query_i64(&yb, &pages).await?;
    let mut pages_after = peak;
    let mut q = i64::MAX;
    for _ in 0..40 {
        sleep(Duration::from_secs(1)).await;
        pages_after = db::query_i64(&yb, &pages).await?;
        q = db::query_i64(&yb, &queue).await?;
        if pages_after < peak && q < 200 {
            break;
        }
    }
    if pages_after >= peak {
        bail!("GC did not reclaim: pages peak={peak} after={pages_after} queue={q}");
    }

    step("Cold read: recycle compute + restart the branch pageserver, then read the trimmed store");
    // Restart the PS (not just the compute) so the cold read
    // reconstructs the chain from the GC-trimmed YB store: the drain
    // doesn't evict the PS's in-memory pages cache, so a running PS
    // could serve the still-cached pre-GC chain and mask an over-delete.
    c.yb(&["endpoint", "stop", "ep_gc_invis"])?;
    c.yb(&["pageserver", "stop", "--id", &id])?;
    c.yb(&["pageserver", "start", "--id", &id])?;
    c.yb(&["endpoint", "start", "ep_gc_invis"])?;
    let mut ready = false;
    for _ in 0..30 {
        if db::query_i64(&conn, "SELECT 1::bigint").await.is_ok() {
            ready = true;
            break;
        }
        sleep(Duration::from_secs(1)).await;
    }
    if !ready {
        bail!("compute did not come back after restart");
    }

    let got_sum = db::query_text(
        &conn,
        "SELECT md5(string_agg(payload, ',' ORDER BY id)) FROM gc_invisible",
    )
    .await;
    let got_cnt = db::query_i64(&conn, "SELECT count(*) FROM gc_invisible").await;

    let _ = c.yb(&["branch", "drop", "--branch-id", &b]);
    c.yb_quiet(&["pageserver", "stop", "--id", &id, "-m", "immediate"]);

    let (got_sum, got_cnt) = (got_sum?, got_cnt?);
    if got_cnt != want_cnt {
        bail!("row count changed across GC: {want_cnt} -> {got_cnt}");
    }
    if got_sum != want_sum {
        bail!("table checksum changed across GC (a needed version was trimmed)");
    }
    Ok(())
}
