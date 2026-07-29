//! Meta trim reclamation: a relation's size sentinel (`rel_size_to_key`)
//! is a `meta` row rewritten on every extend. Growing a table many times
//! piles up superseded sentinel versions; the drain keeps the newest
//! `<= cutoff` and trims the rest (`gc_kind = trim_meta`). Asserts the
//! sentinel's version count converges to a small bound — not one row per
//! extend. Dedicated branch so the signal is this test's alone.

use std::time::Duration;

use anyhow::bail;
use tokio::time::sleep;

use crate::cluster::{Cluster, step};
use crate::db;

const EXTENDS: i64 = 40;

pub async fn run(c: &Cluster) -> anyhow::Result<()> {
    let branch_pg_port = c.compute_port() + 270;
    let yb = db::yb_conn(c.yb_host(), c.yb_port(), c.db_name());

    step("Dedicated branch + endpoint");
    let b = c.branch_create_named("gc_mt", "main")?;
    c.endpoint_create_on_branch("ep_gc_mt", "gc_mt", branch_pg_port)?;
    let conn = db::compute_conn(branch_pg_port);

    step("Create a table and extend it repeatedly, rewriting its rel-size sentinel");
    db::batch(
        &conn,
        "CREATE TABLE gc_mt (id INT PRIMARY KEY, payload TEXT NOT NULL)
             WITH (autovacuum_enabled = off);",
    )
    .await?;
    let relnode = db::query_i64(&conn, "SELECT pg_relation_filenode('gc_mt')::bigint").await?;
    // Versions of this rel's size sentinel in `meta`. The rel-size decode is
    // `rel_size spc=.. db=.. rel=<relnode> fork=0`; a rel BLOCK key would
    // decode `rel_block .. blk=..`, so this suffix match is sentinel-only.
    let sentinel_versions = format!(
        "SELECT count(*) FROM amp_storage.meta \
         WHERE branch_id = '{b}' AND amp_storage.gc_key_decode(key) LIKE '%rel={relnode} fork=0'"
    );
    // Each round grows the heap (new relsize → a new sentinel version at a
    // new LSN) and checkpoints so the write lands. GC runs concurrently
    // (gc_period=1s), so track the PEAK across the burst: it must clearly
    // exceed the trim bound below, or the convergence proves nothing — a
    // trivially small set would satisfy `settled <= 5` even WITHOUT trim.
    let mut peak = 0;
    for r in 0..EXTENDS {
        db::batch(
            &conn,
            &format!(
                "INSERT INTO gc_mt SELECT g, md5(g::text) \
                 FROM generate_series({}, {}) g; CHECKPOINT;",
                r * 500 + 1,
                r * 500 + 500,
            ),
        )
        .await?;
        peak = peak.max(db::query_i64(&yb, &sentinel_versions).await?);
    }
    if peak < 10 {
        bail!(
            "rel-size sentinel pile-up ({peak}) didn't clearly exceed the trim bound; \
             inconclusive (GC may have outpaced the churn — not a trim failure)"
        );
    }

    step("Drain trims the superseded sentinel versions down to a small bound");
    let mut settled = peak;
    for i in 0..40 {
        db::batch(
            &conn,
            &format!(
                "CREATE TABLE IF NOT EXISTS gc_mt_churn (id INT PRIMARY KEY, n INT);
                 INSERT INTO gc_mt_churn VALUES ({i}, {i});
                 CHECKPOINT;"
            ),
        )
        .await?;
        sleep(Duration::from_secs(1)).await;
        settled = db::query_i64(&yb, &sentinel_versions).await?;
        // Keep-newest-<=cutoff leaves the anchor plus at most a couple of
        // above-cutoff versions — nowhere near EXTENDS.
        if settled <= 5 {
            break;
        }
    }

    let _ = c.yb(&["branch", "drop", "--branch-id", &b]);
    c.yb_quiet(&["pageserver", "stop", "ep_gc_mt", "-m", "immediate"]);
    if settled > 5 {
        bail!(
            "meta trim did not reclaim: rel-size sentinel peaked at {peak} versions over \
             {EXTENDS} extends, still {settled} after 40s of drain"
        );
    }
    Ok(())
}
