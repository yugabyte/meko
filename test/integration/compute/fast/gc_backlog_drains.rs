//! A heavy write burst builds a deep per-block version backlog; the
//! drain must reclaim ALL of it — the queue returns to ~empty and the
//! `pages` row count falls well below its post-burst peak (the drain
//! kept up / recovered rather than wedging capped). Reads stay correct
//! throughout. Runs on a dedicated branch so the reclaim signal is this
//! test's alone.

use std::time::Duration;

use anyhow::bail;
use tokio::time::sleep;

use crate::cluster::{Cluster, step};
use crate::db;

pub async fn run(c: &Cluster) -> anyhow::Result<()> {
    let branch_pg_port = c.compute_port() + 170;
    let yb = db::yb_conn(c.yb_host(), c.yb_port(), c.db_name());

    step("Dedicated branch + endpoint");
    let b = c.branch_create_named("gc_backlog", "main")?;
    c.endpoint_create_on_branch("ep_gc_backlog", "gc_backlog", branch_pg_port)?;
    let conn = db::compute_conn(branch_pg_port);
    let pages = format!("SELECT count(*) FROM pageserver.pages WHERE branch_id = '{b}'");
    let queue = format!("SELECT count(*) FROM pageserver.pending_gc WHERE branch_id = '{b}'");

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

    let want_sum = db::query_text(
        &conn,
        "SELECT md5(string_agg(payload, ',' ORDER BY id)) FROM gc_backlog",
    )
    .await?;
    let peak_pages = db::query_i64(&yb, &pages).await?;

    step("Drain reclaims the whole backlog (queue → ~0, pages fall)");
    let mut pages_now = peak_pages;
    let mut q = i64::MAX;
    for _ in 0..60 {
        sleep(Duration::from_secs(1)).await;
        pages_now = db::query_i64(&yb, &pages).await?;
        q = db::query_i64(&yb, &queue).await?;
        if q < 100 && pages_now < peak_pages {
            break;
        }
    }
    if q >= 100 || pages_now >= peak_pages {
        bail!("backlog did not drain: queue={q} peak={peak_pages} now={pages_now}");
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
