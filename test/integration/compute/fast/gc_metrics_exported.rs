//! GC observability: after a workload that exercises the drain, the
//! pageserver exports the `pageserver_yb_gc_*` series and they carry
//! sane values — candidates get emitted, read, and turned into `pages`
//! deletions; the applied cutoff and a drained-tick count advance.
//! Reads the branch's OWN pageserver metrics (each branch PS has its own
//! series), so the assertion isn't confused by other scenarios.

use std::time::Duration;

use anyhow::bail;
use tokio::time::sleep;

use crate::cluster::{Cluster, step};
use crate::db;

pub async fn run(c: &Cluster) -> anyhow::Result<()> {
    let branch_pg_port = c.compute_port() + 160;

    step("Dedicated branch + endpoint");
    let b = c.branch_create_named("gc_metrics", "main")?;
    c.endpoint_create_on_branch("ep_gc_metrics", "gc_metrics", branch_pg_port)?;
    let conn = db::compute_conn(branch_pg_port);
    let http = c.endpoint_ps_http_port("ep_gc_metrics")?;

    step("Run a workload that produces and drains GC candidates");
    db::batch(
        &conn,
        "DROP TABLE IF EXISTS gc_metrics_t;
         CREATE TABLE gc_metrics_t (id INT PRIMARY KEY, payload TEXT NOT NULL);
         INSERT INTO gc_metrics_t SELECT g, md5(g::text) FROM generate_series(1, 800) g;",
    )
    .await?;
    for i in 0..15 {
        db::batch(&conn, &format!("UPDATE gc_metrics_t SET payload = md5(payload || '{i}')")).await?;
    }
    db::batch(&conn, "CHECKPOINT").await?;

    step("The GC counters advance and turn candidates into pages deletions");
    let mut emitted = 0.0;
    let mut deleted = 0.0;
    let mut read = 0.0;
    for _ in 0..45 {
        sleep(Duration::from_secs(1)).await;
        emitted = c.metric_sum(http, "pageserver_yb_gc_candidates_emitted_total").await?;
        read = c.metric_sum(http, "pageserver_yb_gc_candidates_read_total").await?;
        deleted = c.metric_sum(http, "pageserver_yb_gc_pages_rows_deleted_total").await?;
        if emitted > 0.0 && read > 0.0 && deleted > 0.0 {
            break;
        }
    }
    let cutoff = c.metric_sum(http, "pageserver_yb_gc_applied_cutoff_lsn").await?;
    let drained = c
        .metric_sum(http, "pageserver_yb_gc_drain_ticks_total{outcome=\"drained\"")
        .await?;

    let _ = c.yb(&["branch", "drop", "--branch-id", &b]);
    c.yb_quiet(&["pageserver", "stop", "ep_gc_metrics", "-m", "immediate"]);

    if emitted <= 0.0 {
        bail!("no GC candidates emitted (metric absent/zero)");
    }
    if read <= 0.0 {
        bail!("drain read no candidates ({read})");
    }
    if deleted <= 0.0 {
        bail!("no pages rows deleted by GC ({deleted})");
    }
    if cutoff <= 0.0 {
        bail!("applied cutoff lsn did not advance ({cutoff})");
    }
    if drained < 1.0 {
        bail!("no drained drain-ticks recorded ({drained})");
    }
    Ok(())
}
