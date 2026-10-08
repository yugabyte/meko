//! AMP-73: a GiST, SP-GiST or GIN build leaves its pages unlogged until the
//! build ends, so any process that evicts one must write it to the build's
//! local file. Only the building backend used to know that: a checkpoint or
//! another backend evicting such a page PANICked on its zero LSN, or dropped
//! it outright for GiST, whose build pages carry the fake LSN 1.
//!
//! A 2MB buffer pool, a CHECKPOINT loop and random reads in other sessions
//! evict the index pages while each build runs. The indexes must then answer
//! like a seqscan, also after a restart that reads them from the pageserver.
//! A parallel GIN build's workers evict pages its leader dirtied before
//! starting them, so the leader must list its build first. A cancelled
//! CREATE INDEX CONCURRENTLY leaves its unlogged pages behind, so its entry
//! must survive the abort and go away when the index or its database is
//! dropped.

use std::sync::Arc;
use std::sync::atomic::{AtomicBool, Ordering};
use std::time::{Duration, Instant};

use anyhow::{Context, anyhow, bail, ensure};
use tokio::task::JoinHandle;
use tokio_postgres::error::SqlState;

use crate::cluster::{Cluster, step};
use crate::db;
use crate::tee_println;

const EP: &str = "ep_ubuild";
const GEO_ROWS: u32 = 100_000;
const CHURN_ROWS: u32 = 20_000;

/// (index, DDL, predicate only that index can serve, parallel workers)
const INDEXES: [(&str, &str, &str, u32); 4] = [
    (
        "ubuild_gist",
        "gist (b1)",
        "b1 && box '((100,10),(300,60))'",
        0,
    ),
    (
        "ubuild_spgist",
        "spgist (b2)",
        "b2 && box '((100,10),(300,60))'",
        0,
    ),
    ("ubuild_gin", "gin (a)", "a @> ARRAY[5]", 0),
    ("ubuild_gin_par", "gin (a2)", "a2 @> ARRAY[5]", 2),
];

pub async fn run(c: &Cluster) -> anyhow::Result<()> {
    let port = c.compute_port() + 185;

    step("provision an endpoint with a 2MB buffer pool and the LFC off");
    let branch_id = c.branch_create_named("ubuild", c.branch())?;
    c.yb(&[
        "endpoint",
        "create",
        EP,
        "--branch-id",
        &branch_id,
        "--pg-version",
        "18",
        "--pg-port",
        &port.to_string(),
        "--pg-setting",
        "shared_buffers=2MB",
        "--pg-setting",
        "yb_amp.file_cache_size_limit=0",
    ])?;
    c.yb(&["endpoint", "start", EP])?;

    let mut result = run_inner(c, port).await;
    if let Some(line) = compute_crash(c) {
        result = Err(anyhow!("compute crashed:\n  {line}"));
    }

    step("cleanup: deprovision the endpoint");
    c.yb_quiet(&["endpoint", "stop", EP, "--destroy"]);
    result
}

async fn run_inner(c: &Cluster, port: u16) -> anyhow::Result<()> {
    let ep = db::compute_conn(port);
    let sb = db::query_text(&ep, "SHOW shared_buffers").await?;
    ensure!(
        sb == "2MB",
        "shared_buffers is {sb}, want 2MB: index pages might never be evicted"
    );

    step("load a table to index and a churn table 10x the buffer pool");
    db::batch(
        &ep,
        &format!(
            "CREATE TABLE ubuild_geo (b1 box, b2 box, a int[], a2 int[])
               WITH (autovacuum_enabled = off, parallel_workers = 2);
             INSERT INTO ubuild_geo
               SELECT bx, bx, ARRAY[g % 997, g % 991, g % 983], ARRAY[g % 977, g % 971]
               FROM generate_series(1, {GEO_ROWS}) g,
                    LATERAL (SELECT box(point(g % 1000, g / 1000), point(g % 1000 + 3, g / 1000 + 3))) AS s(bx);
             CREATE TABLE ubuild_churn (id int PRIMARY KEY, pad text) WITH (autovacuum_enabled = off);
             INSERT INTO ubuild_churn SELECT g, repeat('x', 900) FROM generate_series(1, {CHURN_ROWS}) g;"
        ),
    )
    .await?;
    let started = postmaster_start(&ep).await?;

    let mut checkpoints = 0;
    for (index, using, _, workers) in INDEXES {
        step(&format!(
            "build {index} while other processes evict its pages"
        ));
        let ddl = format!(
            "SET max_parallel_maintenance_workers = {workers};
             CREATE INDEX {index} ON ubuild_geo USING {using}"
        );
        let (n, saw_worker) = under_eviction(&ep, &ddl)
            .await
            .with_context(|| format!("building {index}"))?;
        ensure!(n >= 1, "no checkpoint ran during the {index} build");
        ensure!(
            workers == 0 || saw_worker,
            "no parallel worker ran during the {index} build"
        );
        checkpoints += n;
        ensure_no_restart(&ep, &started).await?;
    }
    tee_println!("  {checkpoints} checkpoints ran during the builds");

    step("cancel a build under the same pressure, then build it again");
    let cancelled = under_eviction(
        &ep,
        "SET statement_timeout = '1s'; CREATE INDEX ubuild_gist2 ON ubuild_geo USING gist (b1);",
    )
    .await;
    match cancelled {
        Err(e) if db_code(&e) == Some(&SqlState::QUERY_CANCELED) => {}
        Err(e) => return Err(e).context("cancelled build"),
        Ok(_) => tee_println!("  build finished inside 1s, so it was not cancelled"),
    }
    db::batch(&ep, "DROP INDEX IF EXISTS ubuild_gist2").await?;
    under_eviction(
        &ep,
        "CREATE INDEX ubuild_gist2 ON ubuild_geo USING gist (b1)",
    )
    .await?;
    ensure_no_restart(&ep, &started).await?;
    db::batch(&ep, "DROP INDEX ubuild_gist2").await?;

    step("cancel CREATE INDEX CONCURRENTLY mid-build, then drop the index, then the database");
    failed_cics_dropped(&ep, port).await?;
    ensure_no_restart(&ep, &started).await?;

    step("each index answers like a seqscan");
    check_indexes(&ep).await?;

    step("restart the endpoint so the indexes come from the pageserver, check again");
    c.yb(&["endpoint", "stop", EP])?;
    c.yb(&["endpoint", "start", EP])?;
    check_indexes(&ep).await
}

/// Runs `sql` while a CHECKPOINT loop and random reads evict buffers from
/// other processes. Returns how many checkpoints completed meanwhile, and
/// whether a parallel worker ran.
async fn under_eviction(ep: &str, sql: &str) -> anyhow::Result<(u64, bool)> {
    let stop = Arc::new(AtomicBool::new(false));
    // The key goes through a variable: `id = <volatile expr>` can't use the
    // index and would seqscan the table on every lookup.
    let reads = format!(
        "DO $$ DECLARE k int; BEGIN FOR i IN 1..200 LOOP \
           k := 1 + (random() * {})::int; \
           PERFORM pad FROM ubuild_churn WHERE id = k; \
         END LOOP; END $$",
        CHURN_ROWS - 1
    );
    let checkpointer = churn(ep, &stop, "CHECKPOINT".into(), Duration::from_millis(100));
    let reader = churn(ep, &stop, reads, Duration::ZERO);
    let watcher = watch_workers(ep, &stop);

    let built = db::batch(ep, sql).await;
    stop.store(true, Ordering::Relaxed);
    let checkpoints = checkpointer.await.context("checkpoint loop panicked")?;
    let read = reader.await.context("read loop panicked")?;
    let saw_worker = watcher.await.context("worker watch panicked")?;
    built?;
    read?;
    Ok((checkpoints?, saw_worker?))
}

/// Polls until a parallel worker shows up or `stop` is set.
fn watch_workers(ep: &str, stop: &Arc<AtomicBool>) -> JoinHandle<anyhow::Result<bool>> {
    let (ep, stop) = (ep.to_owned(), stop.clone());
    tokio::spawn(async move {
        let client = db::connect(&ep).await?;
        while !stop.load(Ordering::Relaxed) {
            let row = client
                .query_one(
                    "SELECT count(*) FROM pg_stat_activity WHERE backend_type = 'parallel worker'",
                    &[],
                )
                .await?;
            if row.get::<_, i64>(0) > 0 {
                return Ok(true);
            }
            tokio::time::sleep(Duration::from_millis(5)).await;
        }
        Ok(false)
    })
}

fn churn(
    ep: &str,
    stop: &Arc<AtomicBool>,
    sql: String,
    pause: Duration,
) -> JoinHandle<anyhow::Result<u64>> {
    let (ep, stop) = (ep.to_owned(), stop.clone());
    tokio::spawn(async move {
        let client = db::connect(&ep).await?;
        let mut n = 0;
        while !stop.load(Ordering::Relaxed) {
            client
                .batch_execute(&sql)
                .await
                .with_context(|| format!("churn: {sql}"))?;
            n += 1;
            tokio::time::sleep(pause).await;
        }
        Ok(n)
    })
}

async fn failed_cics_dropped(ep: &str, port: u16) -> anyhow::Result<()> {
    db::batch(ep, "CREATE EXTENSION IF NOT EXISTS yb_amp").await?;
    db::batch(ep, "DROP DATABASE IF EXISTS ubuild_db").await?;
    db::batch(ep, "CREATE DATABASE ubuild_db").await?;
    let n = unlogged_builds(ep).await?;
    ensure!(
        n == 0,
        "{n} unlogged builds registered after the builds above ended"
    );

    let db_conn = format!("host=127.0.0.1 port={port} user=cloud_admin dbname=ubuild_db");
    db::batch(
        &db_conn,
        &format!(
            "CREATE TABLE ubuild_cic_t (b box) WITH (autovacuum_enabled = off);
             INSERT INTO ubuild_cic_t
               SELECT box(point(g % 1000, g / 1000), point(g % 1000 + 3, g / 1000 + 3))
               FROM generate_series(1, {GEO_ROWS}) g;"
        ),
    )
    .await?;

    cancel_cic(ep, &db_conn, "ubuild_cic_a").await?;
    db::batch(&db_conn, "DROP INDEX ubuild_cic_a").await?;
    let n = unlogged_builds(ep).await?;
    ensure!(
        n == 0,
        "{n} unlogged builds still registered after DROP INDEX"
    );

    cancel_cic(ep, &db_conn, "ubuild_cic_b").await?;
    db::batch(ep, "DROP DATABASE ubuild_db WITH (FORCE)").await?;
    let n = unlogged_builds(ep).await?;
    ensure!(
        n == 0,
        "{n} unlogged builds still registered after DROP DATABASE"
    );
    Ok(())
}

/// Cancels a CREATE INDEX CONCURRENTLY once it has inserted tuples, leaving an
/// invalid index with unlogged pages, and checks its entry outlives the abort.
async fn cancel_cic(ep: &str, db_conn: &str, index: &str) -> anyhow::Result<()> {
    let conn = db_conn.to_owned();
    let sql = format!("CREATE INDEX CONCURRENTLY {index} ON ubuild_cic_t USING gist (b)");
    let cic = tokio::spawn(async move { db::batch(&conn, &sql).await });

    // Cancel once the build has inserted tuples, so the index has unlogged pages.
    let client = db::connect(ep).await?;
    let deadline = Instant::now() + Duration::from_secs(120);
    loop {
        let cancelled = client
            .query(
                "SELECT pg_cancel_backend(pid) FROM pg_stat_progress_create_index \
                 WHERE datname = 'ubuild_db' AND phase LIKE 'building index%' AND blocks_done > 0",
                &[],
            )
            .await?;
        if !cancelled.is_empty() {
            break;
        }
        ensure!(
            !cic.is_finished(),
            "the concurrent build ended before it could be cancelled"
        );
        ensure!(
            Instant::now() < deadline,
            "the concurrent build never reached its build phase"
        );
        tokio::time::sleep(Duration::from_millis(20)).await;
    }
    match cic.await.context("concurrent build task panicked")? {
        Err(e) if db_code(&e) == Some(&SqlState::QUERY_CANCELED) => {}
        Err(e) => return Err(e).context("cancelled concurrent build"),
        Ok(()) => bail!("the concurrent build finished despite the cancel"),
    }

    let n = unlogged_builds(ep).await?;
    ensure!(
        n == 1,
        "{n} unlogged builds registered after cancelling {index}, want 1 for the invalid index"
    );
    // Flushes the invalid index's leftover pages, which only its entry keeps.
    db::batch(ep, "CHECKPOINT").await
}

async fn unlogged_builds(ep: &str) -> anyhow::Result<i64> {
    db::query_i64(
        ep,
        "SELECT value::bigint FROM yb_amp_perf_counters WHERE metric = 'unlogged_builds'",
    )
    .await
}

async fn check_indexes(ep: &str) -> anyhow::Result<()> {
    let client = db::connect(ep).await?;
    for (index, _, pred, _) in INDEXES {
        let q = format!("SELECT count(*) FROM ubuild_geo WHERE {pred}");
        client
            .batch_execute(
                "SET enable_seqscan = on; SET enable_indexscan = off; SET enable_bitmapscan = off",
            )
            .await?;
        let want: i64 = client.query_one(&q, &[]).await?.get(0);
        client
            .batch_execute(
                "SET enable_seqscan = off; SET enable_indexscan = on; SET enable_bitmapscan = on",
            )
            .await?;
        let plan: Vec<String> = client
            .query(&format!("EXPLAIN {q}"), &[])
            .await?
            .iter()
            .map(|r| r.get(0))
            .collect();
        ensure!(
            plan.iter().any(|l| l.contains(index)),
            "{q} does not use {index}:\n{}",
            plan.join("\n")
        );
        let got: i64 = client.query_one(&q, &[]).await?.get(0);
        ensure!(want > 0, "{q} matches no rows, so it checks nothing");
        if got != want {
            bail!("{index} returns {got} rows for {pred}, a seqscan returns {want}");
        }
    }
    Ok(())
}

async fn postmaster_start(ep: &str) -> anyhow::Result<String> {
    db::query_text(ep, "SELECT pg_postmaster_start_time()::text").await
}

async fn ensure_no_restart(ep: &str, started: &str) -> anyhow::Result<()> {
    let now = postmaster_start(ep).await?;
    ensure!(
        now == started,
        "postmaster restarted (started {started}, now {now}): a process crashed"
    );
    Ok(())
}

fn db_code(e: &anyhow::Error) -> Option<&SqlState> {
    e.chain()
        .find_map(|c| c.downcast_ref::<tokio_postgres::Error>())
        .and_then(|e| e.code())
}

/// The first PANIC or failed assertion in the endpoint's compute log, cut
/// before the backtrace.
fn compute_crash(c: &Cluster) -> Option<String> {
    let log = std::fs::read_to_string(c.repo_dir().join("endpoints").join(EP).join("compute.log"))
        .ok()?;
    let at = |l: &str| l.find("PANIC:").or_else(|| l.find("TRAP:"));
    let line = log.lines().find(|l| at(l).is_some())?;
    Some(line[at(line)?..].chars().take(300).collect())
}
