//! A heavy write burst builds a deep per-block version backlog; the
//! drain must reclaim ALL of it — the queue returns to ~empty and the
//! `pages` row count falls well below its post-burst peak (the drain
//! kept up / recovered rather than wedging capped). Reads stay correct
//! throughout. Runs on a dedicated branch so the reclaim signal is this
//! test's alone.

use anyhow::bail;

use crate::cluster::{Cluster, step};
use crate::db;
use crate::gc_reclaim::wait_reclaimed;

pub async fn run(c: &Cluster) -> anyhow::Result<()> {
    let branch_pg_port = c.endpoint_port();
    let yb = db::yb_conn(c.yb_host(), c.yb_port(), c.db_name());

    step("Dedicated branch + endpoint");
    let b = c.branch_create_named("gc_backlog", c.branch())?;
    c.endpoint_create_on_branch("ep_gc_backlog", &b, branch_pg_port)?;
    let conn = db::compute_conn(branch_pg_port);
    let pages = format!("SELECT count(*) FROM amp_storage.pages WHERE branch_id = '{b}'");
    let queue = format!("SELECT count(*) FROM amp_storage.pending_gc WHERE branch_id = '{b}'");

    step("Build a deep backlog: many blocks, many versions each");
    db::batch(
        &conn,
        "DROP TABLE IF EXISTS gc_backlog;
         CREATE TABLE gc_backlog (id INT PRIMARY KEY, payload TEXT NOT NULL);
         INSERT INTO gc_backlog SELECT g, md5(g::text) FROM generate_series(1, 1200) g;",
    )
    .await?;
    for i in 0..20 {
        db::batch(&conn, &format!("UPDATE gc_backlog SET payload = md5(payload || '{i}')")).await?;
    }
    db::batch(&conn, "CHECKPOINT").await?;
    // Flush so the peak below counts every row the drain will reclaim.
    c.flush_branch(c.endpoint_ps_http_port("ep_gc_backlog")?, &b, &conn).await?;

    let want_sum = db::query_text(
        &conn,
        "SELECT md5(string_agg(payload, ',' ORDER BY id)) FROM gc_backlog",
    )
    .await?;
    let first = db::query_i64(&yb, &pages).await?;

    step("Drain reclaims the whole backlog (queue → ~0, pages settle below the peak)");
    let r = wait_reclaimed(&yb, &pages, &queue, first, 99, 120).await?;
    if !r.settled {
        bail!("backlog did not drain: queue={} peak={} now={}", r.queue, r.peak, r.now);
    }

    step("Reads are still correct after the whole backlog was trimmed");
    let got_sum = db::query_text(
        &conn,
        "SELECT md5(string_agg(payload, ',' ORDER BY id)) FROM gc_backlog",
    )
    .await;
    let _ = c.yb(&["branch", "drop", "--branch-id", &b]);
    c.yb_quiet(&["pageserver", "stop", "ep_gc_backlog", "-m", "immediate"]);
    if got_sum? != want_sum {
        bail!("checksum changed after backlog drain (a needed version was trimmed)");
    }
    Ok(())
}
