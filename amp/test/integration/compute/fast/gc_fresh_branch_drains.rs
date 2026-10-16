//! GC is per-branch: a freshly-provisioned child branch (its own
//! pageserver, created at `endpoint create`) must run its own drain —
//! its `pending_gc` queue populates and drains, and its `pages` rows
//! reclaim — over rows written only to the branch. Proves GC isn't
//! wired to `main` alone.

use anyhow::bail;

use crate::cluster::{Cluster, step};
use crate::db;
use crate::gc_reclaim::wait_reclaimed;

pub async fn run(c: &Cluster) -> anyhow::Result<()> {
    let branch_pg_port = c.endpoint_port();

    step("Create a child branch off main and start its endpoint");
    let b1 = c.branch_create_named("gc_fresh", c.branch())?;
    c.endpoint_create_on_branch("ep_gc_fresh", &b1, branch_pg_port)?;
    let bconn = db::compute_conn(branch_pg_port);
    let yb = db::yb_conn(c.yb_host(), c.yb_port(), c.db_name());
    let pages = format!("SELECT count(*) FROM amp_storage.pages WHERE branch_id = '{b1}'");
    let queue = format!("SELECT count(*) FROM amp_storage.pending_gc WHERE branch_id = '{b1}'");

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
    // Flush so the peak below counts every row the drain will reclaim.
    c.flush_branch(c.endpoint_ps_http_port("ep_gc_fresh")?, &b1, &bconn).await?;
    let want_sum = db::query_text(
        &bconn,
        "SELECT md5(string_agg(payload, ',' ORDER BY id)) FROM gc_fresh_t",
    )
    .await?;
    let first = db::query_i64(&yb, &pages).await?;
    if first == 0 {
        bail!("no pages rows recorded under the branch id {b1}");
    }

    step("The branch's own drain reclaims (queue → ~0, pages settle below the peak)");
    let r = wait_reclaimed(&yb, &pages, &queue, first, 99, 120).await?;
    if !r.settled {
        bail!(
            "branch GC did not reclaim: queue={} peak_pages={} now={}",
            r.queue,
            r.peak,
            r.now
        );
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
    c.yb_quiet(&["pageserver", "stop", "ep_gc_fresh", "-m", "immediate"]);
    Ok(())
}
