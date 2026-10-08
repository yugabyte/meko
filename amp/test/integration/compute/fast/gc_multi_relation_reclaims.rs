//! GC reclaims across many relations at once: several churned tables
//! (distinct relfilenodes / forks) all get their superseded versions
//! trimmed in the same drains — the queue empties, `pages` falls, and
//! every table still reads back correctly. Guards the per-fork trim
//! loop against single-relation tunnel vision. On a dedicated branch so
//! the reclaim signal is this test's alone.

use anyhow::bail;

use crate::cluster::{Cluster, step};
use crate::db;
use crate::gc_reclaim::wait_reclaimed;

const TABLES: usize = 4;

pub async fn run(c: &Cluster) -> anyhow::Result<()> {
    let branch_pg_port = c.endpoint_port();
    let yb = db::yb_conn(c.yb_host(), c.yb_port(), c.db_name());

    step("Dedicated branch + endpoint");
    let b = c.branch_create_named("gc_mr", c.branch())?;
    c.endpoint_create_on_branch("ep_gc_mr", &b, branch_pg_port)?;
    let conn = db::compute_conn(branch_pg_port);
    let pages = format!("SELECT count(*) FROM amp_storage.pages WHERE branch_id = '{b}'");
    let queue = format!("SELECT count(*) FROM amp_storage.pending_gc WHERE branch_id = '{b}'");

    step("Seed and churn several small tables");
    for t in 0..TABLES {
        db::batch(
            &conn,
            &format!(
                "DROP TABLE IF EXISTS gc_mr_{t};
                 CREATE TABLE gc_mr_{t} (id INT PRIMARY KEY, payload TEXT NOT NULL);
                 INSERT INTO gc_mr_{t} SELECT g, md5(g::text) FROM generate_series(1, 200) g;"
            ),
        )
        .await?;
    }
    for i in 0..10 {
        for t in 0..TABLES {
            db::batch(&conn, &format!("UPDATE gc_mr_{t} SET payload = md5(payload || '{i}')")).await?;
        }
    }
    db::batch(&conn, "CHECKPOINT").await?;

    let mut want = Vec::with_capacity(TABLES);
    for t in 0..TABLES {
        want.push(
            db::query_text(
                &conn,
                &format!("SELECT md5(string_agg(payload, ',' ORDER BY id)) FROM gc_mr_{t}"),
            )
            .await?,
        );
    }
    let first = db::query_i64(&yb, &pages).await?;

    step("All relations' versions drain (queue → ~0, pages settle below the peak)");
    let r = wait_reclaimed(&yb, &pages, &queue, first, 99, 120).await?;
    if !r.settled {
        bail!(
            "multi-relation GC did not reclaim: queue={} peak={} now={}",
            r.queue,
            r.peak,
            r.now
        );
    }

    step("Every table still reads back correctly");
    let mut got = Vec::with_capacity(TABLES);
    for t in 0..TABLES {
        got.push(
            db::query_text(
                &conn,
                &format!("SELECT md5(string_agg(payload, ',' ORDER BY id)) FROM gc_mr_{t}"),
            )
            .await?,
        );
    }
    let _ = c.yb(&["branch", "drop", "--branch-id", &b]);
    c.yb_quiet(&["pageserver", "stop", "ep_gc_mr", "-m", "immediate"]);
    for t in 0..TABLES {
        if got[t] != want[t] {
            bail!("table gc_mr_{t} checksum changed after GC");
        }
    }
    Ok(())
}
