//! GC is per-branch: a freshly-provisioned child branch (its own
//! pageserver, created at `endpoint create`) must run its own drain —
//! its `pending_gc` queue populates and drains, and its `pages` rows
//! reclaim — over rows written only to the branch. Proves GC isn't
//! wired to `main` alone.

use std::time::Duration;

use anyhow::bail;
use tokio::time::sleep;

use crate::cluster::{Cluster, step};
use crate::db;

pub async fn run(c: &Cluster) -> anyhow::Result<()> {
    let branch_pg_port = c.compute_port() + 120;

    step("Create a child branch off main and start its endpoint");
    let b1 = c.branch_create_named("gc_fresh", "main")?;
    let ps_id = c.endpoint_create_on_branch("ep_gc_fresh", "gc_fresh", branch_pg_port)?;
    let bconn = db::compute_conn(branch_pg_port);
    let yb = db::yb_conn(c.yb_host(), c.yb_port(), c.db_name());
    let pages = format!("SELECT count(*) FROM pageserver.pages WHERE branch_id = '{b1}'");
    let queue = format!("SELECT count(*) FROM pageserver.pending_gc WHERE branch_id = '{b1}'");

    step("Write + churn a table that lives only on the branch");
    db::batch(
        &bconn,
        "DROP TABLE IF EXISTS gc_fresh_t;
         CREATE TABLE gc_fresh_t (id INT PRIMARY KEY, payload TEXT NOT NULL);
         INSERT INTO gc_fresh_t SELECT g, md5(g::text) FROM generate_series(1, 600) g;",
    )
    .await?;
    for i in 0..15 {
        db::batch(
            &bconn,
            &format!("UPDATE gc_fresh_t SET payload = md5(payload || '{i}')"),
        )
        .await?;
    }
    db::batch(&bconn, "CHECKPOINT").await?;
    let want_sum = db::query_text(
        &bconn,
        "SELECT md5(string_agg(payload, ',' ORDER BY id)) FROM gc_fresh_t",
    )
    .await?;
    let peak_pages = db::query_i64(&yb, &pages).await?;
    if peak_pages == 0 {
        bail!("no pages rows recorded under the branch id {b1}");
    }

    step("The branch's own drain reclaims (queue → ~0, pages fall)");
    let mut pages_now = peak_pages;
    let mut q = i64::MAX;
    for _ in 0..50 {
        sleep(Duration::from_secs(1)).await;
        pages_now = db::query_i64(&yb, &pages).await?;
        q = db::query_i64(&yb, &queue).await?;
        if q < 100 && pages_now < peak_pages {
            break;
        }
    }
    if q >= 100 || pages_now >= peak_pages {
        bail!("branch GC did not reclaim: queue={q} peak_pages={peak_pages} now={pages_now}");
    }

    step("Branch reads stay correct after its GC ran");
    let got_sum = db::query_text(
        &bconn,
        "SELECT md5(string_agg(payload, ',' ORDER BY id)) FROM gc_fresh_t",
    )
    .await?;
    if got_sum != want_sum {
        bail!("branch checksum changed after GC");
    }

    // Drop the child branch and stop its pageserver so it stops pinning
    // main's cutoff and doesn't accumulate idle branch PSes for later
    // scenarios.
    let _ = c.yb(&["branch", "drop", "--branch-id", &b1]);
    c.yb_quiet(&["pageserver", "stop", "--id", &ps_id.to_string(), "-m", "immediate"]);
    Ok(())
}
