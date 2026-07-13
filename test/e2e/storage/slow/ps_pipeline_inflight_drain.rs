//! #173: the pipelined page_service executor must DRAIN its in-flight getpage
//! batches and only THEN send the graceful-shutdown `GoingAway` frame. The
//! first cut sent `GoingAway` the moment the batcher exited (`SenderGone`) —
//! before draining — so the frame could race ahead of a connection's last
//! pipelined responses and the compute could disconnect before reading them
//! (caught by review; fixed by moving the send to the drain-complete exit).
//!
//! This keeps the pipeline (`pipeline_depth = 16`) busy with a background
//! cold-read loop, gracefully stops the pageserver while batches are in
//! flight, and asserts the shutdown drained cleanly: it emitted `GoingAway`,
//! did not hang, did not panic in the executor/drain path, and — after a
//! restart — the data is intact on a cold re-read (an interrupted pipeline
//! must not lose or corrupt responses).
//!
//! Note: how *deep* the pipeline actually fills depends on the endpoint's
//! prefetch config (shared_buffers / eic); the load-bearing signals here are
//! the clean drain (no hang, no panic), the emitted `GoingAway`, and the
//! post-restart integrity — all of which hold with even one batch in flight.

use std::time::{Duration, Instant};

use anyhow::{bail, ensure};

use crate::cluster::{Cluster, step};
use crate::tee_println;
use crate::{db, page_api, psql};

const N_ROWS: i64 = 300_000;

/// Whole-table fingerprint (count + id sum + a mixed sum): every value depends
/// on every page carrying the right tuples, so a lost/corrupted page changes it.
const FINGERPRINT: &str = "SELECT count(*)::text || ':' \
     || coalesce(sum(id::bigint), 0)::text || ':' \
     || coalesce(sum((v::bigint) * (id % 7 + 1)), 0)::text FROM pipe_drain";

pub async fn run(c: &Cluster) -> anyhow::Result<()> {
    let main = db::compute_conn(c.compute_port());

    step("seed a multi-page table and capture the reference fingerprint");
    db::batch(
        &main,
        "DROP TABLE IF EXISTS pipe_drain; \
         CREATE TABLE pipe_drain (id int, v int) WITH (autovacuum_enabled = off);",
    )
    .await?;
    db::batch(
        &main,
        &format!(
            "INSERT INTO pipe_drain \
             SELECT g, (g * 2654435761) % 1000000 FROM generate_series(1, {N_ROWS}) g"
        ),
    )
    .await?;
    let reference = db::query_text(&main, FINGERPRINT).await?;

    let toml = c
        .repo_dir()
        .join("endpoints/main/pageserver/pageserver.toml");
    let original = std::fs::read_to_string(&toml)?;
    let result = run_inner(c, &main, &original, &reference).await;
    result.and(page_api::restore_ps(c, &original))?;

    db::batch(&main, "DROP TABLE IF EXISTS pipe_drain").await?;
    Ok(())
}

async fn run_inner(
    c: &Cluster,
    main: &str,
    original: &str,
    reference: &str,
) -> anyhow::Result<()> {
    step("pipeline_depth=16, cold PS + cold compute");
    page_api::restart_with_pipeline_depth(c, original, 16)?;
    // Cold compute buffers so the seqscan reaches the PS (and thus the pipeline)
    // rather than serving from local shared_buffers.
    c.yb_quiet(&["endpoint", "stop", "main", "--mode", "immediate"]);
    c.yb(&["endpoint", "start", "main"])?;
    tokio::time::sleep(Duration::from_secs(3)).await;

    step("keep the pipeline busy with a background cold-read loop");
    // Repeated whole-table scans drive a sustained stream of getpage batches
    // (LFC off + tiny PS read cache from restart_with_pipeline_depth => reads
    // reach YB), so batches are still in flight when we stop the pageserver.
    let mut scan_sql = String::from("SET effective_io_concurrency = 32; ");
    for _ in 0..20 {
        scan_sql.push_str("SELECT count(*) FROM pipe_drain; SELECT pg_sleep(0.1); ");
    }
    let mut bg = psql::spawn_bg(c.compute_port(), &scan_sql)?;
    tokio::time::sleep(Duration::from_secs(1)).await;

    step("gracefully stop the pageserver while batches are in flight");
    let t0 = Instant::now();
    c.yb(&["pageserver", "stop"])?;
    let stop_secs = t0.elapsed().as_secs_f64();
    tee_println!("  pageserver stop took {stop_secs:.2}s");
    let _ = bg.kill();
    let _ = bg.wait();

    step("assert the graceful shutdown drained the pipeline cleanly");
    let ps_log = c.ps_log();
    let content = std::fs::read_to_string(&ps_log)
        .map_err(|e| anyhow::anyhow!("pageserver log unreadable at {}: {e}", ps_log.display()))?;
    // 1) did not hang — a deadlocked drain would wedge the graceful stop.
    ensure!(
        stop_secs < 10.0,
        "pageserver stop took {stop_secs:.2}s (>=10s) — likely a pipeline drain hang"
    );
    // 2) emitted GoingAway — the graceful path ran with the pipeline busy.
    let frames = content
        .lines()
        .filter(|l| l.contains("writing GoingAway frame on graceful shutdown"))
        .count();
    ensure!(
        frames >= 1,
        "pageserver wrote no GoingAway frames on graceful shutdown under a busy pipeline"
    );
    // 3) nothing panicked in the executor / drain path.
    if let Some(p) = content
        .lines()
        .find(|l| l.contains("panicked at") || l.contains("PANIC"))
    {
        bail!("pageserver panicked during pipelined graceful shutdown: {p}");
    }
    tee_println!("  ok — stop {stop_secs:.2}s, {frames} GoingAway frame(s), no panic");

    step("restart PS + cold re-read: an interrupted pipeline must not corrupt data");
    c.yb(&["pageserver", "start"])?;
    c.yb_quiet(&["endpoint", "stop", "main", "--mode", "immediate"]);
    c.yb(&["endpoint", "start", "main"])?;
    tokio::time::sleep(Duration::from_secs(3)).await;
    let got = db::query_text(main, FINGERPRINT).await?;
    ensure!(
        got == reference,
        "post-shutdown fingerprint = {got}, want {reference} \
         — an interrupted pipeline lost or corrupted pages"
    );
    tee_println!("  ok — data intact after an interrupted pipeline ({got})");
    Ok(())
}
