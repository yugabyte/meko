//! Truncation (shrink) reclamation: VACUUM's tail truncation emits an
//! `XLOG_SMGR_TRUNCATE` record, which queues a shrink event
//! (`pending_gc.blknum = -1`); the drain then deletes the stored pages
//! past the post-truncation size once the cutoff passes it. Asserts
//! the tail's `pages` rows reach zero while the kept prefix survives
//! and still reads back correctly. Dedicated branch so the signal is
//! this test's alone.

use std::time::Duration;

use anyhow::bail;
use tokio::time::sleep;

use crate::cluster::{Cluster, step};
use crate::db;

pub async fn run(c: &Cluster) -> anyhow::Result<()> {
    let branch_pg_port = c.compute_port() + 240;
    let yb = db::yb_conn(c.yb_host(), c.yb_port(), c.db_name());

    step("Dedicated branch + endpoint");
    let b = c.branch_create_named("gc_tr", "main")?;
    c.endpoint_create_on_branch("ep_gc_tr", "gc_tr", branch_pg_port)?;
    let conn = db::compute_conn(branch_pg_port);

    step("Seed a multi-block table, then DELETE the tail and VACUUM");
    db::batch(
        &conn,
        "CREATE TABLE gc_tr (id INT PRIMARY KEY, payload TEXT NOT NULL)
             WITH (autovacuum_enabled = off);
         INSERT INTO gc_tr SELECT g, md5(g::text) FROM generate_series(1, 20000) g;
         CHECKPOINT;",
    )
    .await?;
    let relnode = db::query_i64(&conn, "SELECT pg_relation_filenode('gc_tr')::bigint").await?;
    let old_blocks = db::query_i64(&conn, "SELECT pg_relation_size('gc_tr') / 8192").await?;

    db::batch(&conn, "DELETE FROM gc_tr WHERE id > 1000").await?;
    // Own batch: VACUUM cannot run inside the implicit transaction a
    // multi-statement batch gets.
    db::batch(&conn, "VACUUM gc_tr").await?;
    db::batch(&conn, "CHECKPOINT").await?;
    let new_blocks = db::query_i64(&conn, "SELECT pg_relation_size('gc_tr') / 8192").await?;
    if new_blocks >= old_blocks {
        bail!("VACUUM did not truncate the tail: {old_blocks} -> {new_blocks} blocks");
    }
    let want = db::query_text(
        &conn,
        "SELECT md5(string_agg(payload, ',' ORDER BY id)) FROM gc_tr",
    )
    .await?;

    step("Tail pages past the new size drain to zero");
    let tail_pages = format!(
        "SELECT count(*) FROM pageserver.pages \
         WHERE branch_id = '{b}' AND relnode = {relnode} AND blknum >= {new_blocks}"
    );
    let mut tail = i64::MAX;
    for i in 0..40 {
        // Churn a side table to keep the WAL frontier moving so the
        // cutoff passes the truncation (gc_tr itself must stay put).
        db::batch(
            &conn,
            &format!(
                "CREATE TABLE IF NOT EXISTS gc_tr_churn (id INT PRIMARY KEY, n INT);
                 INSERT INTO gc_tr_churn VALUES ({i}, {i});
                 CHECKPOINT;"
            ),
        )
        .await?;
        sleep(Duration::from_secs(1)).await;
        tail = db::query_i64(&yb, &tail_pages).await?;
        if tail == 0 {
            break;
        }
    }

    step("Kept prefix still reads back correctly");
    let got = db::query_text(
        &conn,
        "SELECT md5(string_agg(payload, ',' ORDER BY id)) FROM gc_tr",
    )
    .await?;
    let _ = c.yb(&["branch", "drop", "--branch-id", &b]);
    c.yb_quiet(&["pageserver", "stop", "ep_gc_tr", "-m", "immediate"]);
    if tail != 0 {
        bail!("truncated tail not reclaimed: {tail} pages rows past block {new_blocks}");
    }
    if got != want {
        bail!("kept prefix checksum changed after shrink GC");
    }
    Ok(())
}
