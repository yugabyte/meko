//! DROP TABLE reclamation: dropping a relation queues a drop event
//! (`pending_gc.gc_kind = 4`, DropRel) and the drain then deletes every stored
//! page of the dropped relfilenode once the cutoff passes the drop.
//! Asserts the strongest end-state — the rel's `pages` rows AND its
//! `pending_gc` rows both reach zero — on a dedicated branch so the
//! signal is this test's alone.

use std::time::Duration;

use anyhow::bail;
use tokio::time::sleep;

use crate::cluster::{Cluster, step};
use crate::db;

pub async fn run(c: &Cluster) -> anyhow::Result<()> {
    let branch_pg_port = c.endpoint_port();
    let yb = db::yb_conn(c.yb_host(), c.yb_port(), c.db_name());

    step("Dedicated branch + endpoint");
    let b = c.branch_create_named("gc_dt", c.branch())?;
    c.endpoint_create_on_branch("ep_gc_dt", &b, branch_pg_port)?;
    let conn = db::compute_conn(branch_pg_port);

    step("Seed a table and capture its relfilenode");
    db::batch(
        &conn,
        "CREATE TABLE gc_dt (id INT PRIMARY KEY, payload TEXT NOT NULL);
         INSERT INTO gc_dt SELECT g, md5(g::text) FROM generate_series(1, 2000) g;
         CHECKPOINT;",
    )
    .await?;
    let relnode = db::query_i64(&conn, "SELECT pg_relation_filenode('gc_dt')::bigint").await?;
    let rel_pages = format!(
        "SELECT count(*) FROM amp_storage.pages WHERE branch_id = '{b}' AND relnode = {relnode}"
    );
    // pending_gc keys the rel by its encoded Key; field4 (bytes 10-13,
    // 1-indexed) holds the relnode — match it as a 4-byte big-endian slice.
    let rel_queue = format!(
        "SELECT count(*) FROM amp_storage.pending_gc \
         WHERE branch_id = '{b}' AND substring(key FROM 10 FOR 4) = '\\x{relnode:08x}'::bytea"
    );
    // Wait for the seed writes to actually flush to YB before dropping.
    let mut stored = 0;
    for _ in 0..30 {
        stored = db::query_i64(&yb, &rel_pages).await?;
        if stored > 0 {
            break;
        }
        sleep(Duration::from_secs(1)).await;
    }
    if stored == 0 {
        bail!("seeded table produced no stored pages");
    }

    step("DROP TABLE, then churn so the cutoff passes the drop");
    db::batch(&conn, "DROP TABLE gc_dt").await?;
    db::batch(&conn, "CREATE TABLE gc_dt_churn (id INT PRIMARY KEY, n INT)").await?;

    step("Dropped rel's pages and queue rows drain to zero");
    let (mut pages, mut queue) = (i64::MAX, i64::MAX);
    for i in 0..40 {
        // Keep the WAL frontier moving so the cutoff can advance.
        db::batch(
            &conn,
            &format!("INSERT INTO gc_dt_churn VALUES ({i}, {i}); CHECKPOINT;"),
        )
        .await?;
        sleep(Duration::from_secs(1)).await;
        pages = db::query_i64(&yb, &rel_pages).await?;
        queue = db::query_i64(&yb, &rel_queue).await?;
        if pages == 0 && queue == 0 {
            break;
        }
    }
    let _ = c.yb(&["branch", "drop", "--branch-id", &b]);
    c.yb_quiet(&["pageserver", "stop", "ep_gc_dt", "-m", "immediate"]);
    if pages != 0 || queue != 0 {
        bail!(
            "dropped table not reclaimed: stored={stored} pages_left={pages} queue_left={queue}"
        );
    }
    Ok(())
}
