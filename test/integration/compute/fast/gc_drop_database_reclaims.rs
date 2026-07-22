//! DROP DATABASE reclamation: dropping a database queues a drop
//! event per relation fork (emitted under `drop_dbdir`'s reldir
//! enumeration) and the drain deletes every stored page of the
//! database once the cutoff passes the drop. Asserts the whole
//! `dbnode`'s `pages` and `pending_gc` rows reach zero, on a dedicated
//! branch so the signal is this test's alone.

use std::time::Duration;

use anyhow::bail;
use tokio::time::sleep;

use crate::cluster::{Cluster, step};
use crate::db;

pub async fn run(c: &Cluster) -> anyhow::Result<()> {
    let branch_pg_port = c.compute_port() + 230;
    let yb = db::yb_conn(c.yb_host(), c.yb_port(), c.db_name());

    step("Dedicated branch + endpoint");
    let b = c.branch_create_named("gc_dd", "main")?;
    c.endpoint_create_on_branch("ep_gc_dd", "gc_dd", branch_pg_port)?;
    let conn = db::compute_conn(branch_pg_port);

    step("Create a database with a couple of populated tables");
    db::batch(&conn, "CREATE DATABASE gc_dd_db").await?;
    let db_conn = format!("host=127.0.0.1 port={branch_pg_port} user=cloud_admin dbname=gc_dd_db");
    db::batch(
        &db_conn,
        "CREATE TABLE t1 (id INT PRIMARY KEY, payload TEXT NOT NULL);
         INSERT INTO t1 SELECT g, md5(g::text) FROM generate_series(1, 1000) g;
         CREATE TABLE t2 (id INT PRIMARY KEY, payload TEXT NOT NULL);
         INSERT INTO t2 SELECT g, md5(g::text) FROM generate_series(1, 1000) g;
         CHECKPOINT;",
    )
    .await?;
    let dbnode = db::query_i64(
        &conn,
        "SELECT oid::bigint FROM pg_database WHERE datname = 'gc_dd_db'",
    )
    .await?;
    let db_pages = format!(
        "SELECT count(*) FROM pageserver.pages WHERE branch_id = '{b}' AND dbnode = {dbnode}"
    );
    let db_queue = format!(
        "SELECT count(*) FROM pageserver.pending_gc WHERE branch_id = '{b}' AND dbnode = {dbnode}"
    );
    let mut stored = 0;
    for _ in 0..30 {
        stored = db::query_i64(&yb, &db_pages).await?;
        if stored > 0 {
            break;
        }
        sleep(Duration::from_secs(1)).await;
    }
    if stored == 0 {
        bail!("seeded database produced no stored pages");
    }

    step("DROP DATABASE, then churn so the cutoff passes the drop");
    // The per-query helper connections are dropped after each call, but
    // the backend may linger briefly — retry the drop.
    let mut dropped = false;
    for _ in 0..10 {
        if db::batch(&conn, "DROP DATABASE gc_dd_db").await.is_ok() {
            dropped = true;
            break;
        }
        sleep(Duration::from_secs(1)).await;
    }
    if !dropped {
        bail!("DROP DATABASE kept failing (lingering connection?)");
    }
    db::batch(&conn, "CREATE TABLE gc_dd_churn (id INT PRIMARY KEY, n INT)").await?;

    step("Database's pages and queue rows drain to zero");
    let (mut pages, mut queue) = (i64::MAX, i64::MAX);
    for i in 0..40 {
        db::batch(
            &conn,
            &format!("INSERT INTO gc_dd_churn VALUES ({i}, {i}); CHECKPOINT;"),
        )
        .await?;
        sleep(Duration::from_secs(1)).await;
        pages = db::query_i64(&yb, &db_pages).await?;
        queue = db::query_i64(&yb, &db_queue).await?;
        if pages == 0 && queue == 0 {
            break;
        }
    }
    let _ = c.yb(&["branch", "drop", "--branch-id", &b]);
    c.yb_quiet(&["pageserver", "stop", "ep_gc_dd", "-m", "immediate"]);
    if pages != 0 || queue != 0 {
        bail!(
            "dropped database not reclaimed: stored={stored} pages_left={pages} queue_left={queue}"
        );
    }
    Ok(())
}
