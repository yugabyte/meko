//! Crash recovery across a torn / partial WAL write. Each sub-scenario
//! hammers a counter table with `INSERT id=i; COMMIT;` (so the durable
//! survivors must be a contiguous prefix `1..k`), then SIGKILLs a
//! component mid-flush — leaving WAL chunks queued in the producer
//! channel / mid-batch, never landed in `public.wals`:
//!
//!   A. safekeeper crash  — kill -9 the SK; the still-running compute's
//!      walproposer must reconcile with the SK's rehydrated `write_lsn`.
//!   B. compute crash     — kill -9 the postmaster; restart does crash
//!      recovery + basebackup, replaying WAL from the SK.
//!   C. both crash        — kill -9 SK + postmaster together; restart
//!      both and replay.
//!
//! The post-crash invariant in every case: `count(*) == max(id)` (a hole
//! means a committed record below a surviving one was lost — torn-replay
//! corruption), the compute reconnects (no basebackup deadlock), and
//! neither log shows a "YB WAL hole". The shared corruption gate
//! (`run_scenario`) scans the PS log for the marker set on top.
//!
//! KNOWN BUG: scenario A reproduces an open data-loss bug (tasks/34) — an
//! SK kill-9 with a surviving compute intermittently loses early acked
//! commits and trips the `durable_walk_unflushed_row` correctness counter
//! caught by the suite Final verdict. The contiguity hole is timing-flaky;
//! the counter is the reliable catch. Expect this test to fail until 34 is
//! fixed.

use std::path::Path;
use std::time::{Duration, Instant};

use anyhow::{Context, bail};

use crate::cluster::{Cluster, step};
use crate::{db, psql};

/// Seconds the background write loop runs before we yank a component —
/// long enough that WAL is actively flowing through the SK producer.
fn warmup_secs() -> u64 {
    std::env::var("TORN_WARMUP_SECS").ok().and_then(|s| s.parse().ok()).unwrap_or(3)
}

/// How long to wait for the compute to start serving reads again after a
/// restart. Exceeding it means a basebackup / walreceiver deadlock.
fn reconnect_timeout() -> Duration {
    Duration::from_secs(
        std::env::var("TORN_RECONNECT_SECS").ok().and_then(|s| s.parse().ok()).unwrap_or(90),
    )
}

pub async fn run(c: &Cluster) -> anyhow::Result<()> {
    // Run all three crash modes regardless of individual failures so one
    // invocation reports every verdict, then fail if any lost data.
    let mut results: Vec<(&str, anyhow::Result<()>)> = Vec::new();
    results.push(("A safekeeper-crash", sk_crash(c).await));
    results.push(("B compute-crash", compute_crash(c).await));
    results.push(("C both-crash", both_crash(c).await));

    step("torn-write summary");
    let mut failed = 0;
    for (label, r) in &results {
        match r {
            Ok(()) => println!("  PASS — {label}"),
            Err(e) => {
                failed += 1;
                println!("  FAIL — {label}: {e:#}");
            }
        }
    }
    if failed > 0 {
        bail!("{failed}/{} torn-write crash modes lost committed data", results.len());
    }
    Ok(())
}

/// A. SIGKILL the safekeeper mid-flush, restart it, and let the
/// still-running compute's walproposer reconnect to the rehydrated SK.
async fn sk_crash(c: &Cluster) -> anyhow::Result<()> {
    let conn = db::compute_conn(c.compute_port());
    let table = "torn_sk";
    step("A: seed counter table + commit-per-row fill procedure");
    seed(&conn, table).await?;

    step("A: spawn background commit-per-row loop");
    let mut bg = psql::spawn_bg(c.compute_port(), &format!("CALL {table}_fill(100000000);"))?;
    tokio::time::sleep(Duration::from_secs(warmup_secs())).await;

    step("A: SIGKILL safekeeper mid-flush (chunks still queued, not in `wals`)");
    kill9(c, "endpoints/main/safekeeper/safekeeper.pid", "safekeeper")?;
    let _ = bg.kill();
    let _ = bg.wait();

    step("A: restart safekeeper (rehydrates write_lsn from YB)");
    c.yb_quiet(&["safekeeper", "stop", "-m", "immediate"]);
    c.yb(&["safekeeper", "start"])?;
    // Give the surviving compute's walproposer a moment to reconnect.
    tokio::time::sleep(Duration::from_secs(2)).await;

    verify(c, &conn, table, "A").await
}

/// B. SIGKILL the compute postmaster mid-commit, restart the endpoint,
/// and replay WAL back from the (still-up) safekeeper.
async fn compute_crash(c: &Cluster) -> anyhow::Result<()> {
    recycle_compute(c)?;
    let conn = db::compute_conn(c.compute_port());
    let table = "torn_compute";
    step("B: seed counter table + commit-per-row fill procedure");
    seed(&conn, table).await?;

    step("B: spawn background commit-per-row loop");
    let mut bg = psql::spawn_bg(c.compute_port(), &format!("CALL {table}_fill(100000000);"))?;
    tokio::time::sleep(Duration::from_secs(warmup_secs())).await;

    step("B: SIGKILL compute postmaster mid-commit");
    kill9(c, "endpoints/main/pgdata/postmaster.pid", "postmaster")?;
    let _ = bg.kill();
    let _ = bg.wait();

    step("B: restart endpoint (crash recovery + basebackup, WAL replay from SK)");
    recycle_compute(c)?;

    verify(c, &conn, table, "B").await
}

/// C. SIGKILL both the safekeeper and the compute postmaster together,
/// restart both, and replay.
async fn both_crash(c: &Cluster) -> anyhow::Result<()> {
    recycle_compute(c)?;
    let conn = db::compute_conn(c.compute_port());
    let table = "torn_both";
    step("C: seed counter table + commit-per-row fill procedure");
    seed(&conn, table).await?;

    step("C: spawn background commit-per-row loop");
    let mut bg = psql::spawn_bg(c.compute_port(), &format!("CALL {table}_fill(100000000);"))?;
    tokio::time::sleep(Duration::from_secs(warmup_secs())).await;

    step("C: SIGKILL safekeeper + compute postmaster together");
    kill9(c, "endpoints/main/safekeeper/safekeeper.pid", "safekeeper")?;
    kill9(c, "endpoints/main/pgdata/postmaster.pid", "postmaster")?;
    let _ = bg.kill();
    let _ = bg.wait();

    step("C: restart safekeeper, then endpoint");
    c.yb_quiet(&["safekeeper", "stop", "-m", "immediate"]);
    c.yb(&["safekeeper", "start"])?;
    recycle_compute(c)?;

    verify(c, &conn, table, "C").await
}

/// Drop+create the counter table and a procedure that inserts sequential
/// ids one-commit-each, so the durable survivors form a contiguous
/// prefix. The `pad` column inflates per-record WAL so a few seconds of
/// loop produces real chunk traffic.
async fn seed(conn: &str, table: &str) -> anyhow::Result<()> {
    db::batch(
        conn,
        &format!(
            "DROP TABLE IF EXISTS {table};
             CREATE TABLE {table} (id BIGINT PRIMARY KEY, pad TEXT NOT NULL)
                 WITH (autovacuum_enabled = false);
             CREATE OR REPLACE PROCEDURE {table}_fill(iters INT) AS $$
             BEGIN
                 FOR i IN 1..iters LOOP
                     INSERT INTO {table} (id, pad) VALUES (i, repeat('x', 400));
                     COMMIT;
                 END LOOP;
             END;
             $$ LANGUAGE plpgsql;
             CHECKPOINT;"
        ),
    )
    .await
}

/// Post-crash assertions: compute reconnects, no log "YB WAL hole", the
/// durable rows are a contiguous prefix (`count == max(id)`), and a
/// post-recovery write still lands.
async fn verify(c: &Cluster, conn: &str, table: &str, label: &str) -> anyhow::Result<()> {
    // Read durable state from a fresh basebackup so the assertion can't be
    // masked by pages still warm in the surviving compute's shared_buffers
    // (scenario A keeps the compute alive across the crash). After this the
    // seqscan reconstructs every page from the pageserver at the recovered
    // LSN — a hole here is genuine durable data loss, not a cache artifact.
    step(&format!("{label}: recycle compute for a cache-free durable read"));
    recycle_compute(c)?;

    step(&format!("{label}: wait for compute to serve reads again"));
    let deadline = Instant::now() + reconnect_timeout();
    loop {
        match db::query_i64(conn, "SELECT 1::bigint").await {
            Ok(_) => break,
            Err(_) if Instant::now() < deadline => {
                tokio::time::sleep(Duration::from_millis(500)).await;
            }
            Err(e) => {
                return Err(e).context(format!(
                    "{label}: compute never served reads within timeout (basebackup / walreceiver deadlock?)"
                ));
            }
        }
    }

    scan_for_wal_hole(c, label)?;

    step(&format!("{label}: assert durable rows are a contiguous prefix"));
    let cnt = db::query_i64(conn, &format!("SELECT count(*) FROM {table}")).await?;
    let mx = db::query_i64(conn, &format!("SELECT COALESCE(max(id), 0) FROM {table}")).await?;
    println!("  {label}: count={cnt} max(id)={mx}");
    if cnt == 0 {
        bail!("{label}: no rows survived — workload never committed before the crash");
    }
    if cnt as i64 != mx {
        bail!(
            "{label}: count={cnt} but max(id)={mx} — WAL replay left a hole \
             (a committed record was lost below a surviving one)"
        );
    }

    step(&format!("{label}: post-recovery write must still land"));
    db::batch(
        conn,
        &format!(
            "INSERT INTO {table} (id, pad)
             SELECT g, repeat('x', 400)
             FROM generate_series({}, {}) g;",
            mx + 1,
            mx + 5
        ),
    )
    .await?;
    let cnt2 = db::query_i64(conn, &format!("SELECT count(*) FROM {table}")).await?;
    let mx2 = db::query_i64(conn, &format!("SELECT COALESCE(max(id), 0) FROM {table}")).await?;
    if cnt2 as i64 != mx2 || mx2 != mx + 5 {
        bail!("{label}: post-recovery write failed (count={cnt2} max={mx2}, expected {})", mx + 5);
    }
    println!("  {label}: ok — contiguous through {mx2} after post-recovery write");
    Ok(())
}

/// SIGKILL the process named in `rel` (a pidfile under the run dir).
/// First whitespace-delimited token of the first line is the pid
/// (postmaster.pid carries extra lines after it).
fn kill9(c: &Cluster, rel: &str, what: &str) -> anyhow::Result<()> {
    let pidfile = c.repo_dir().join(rel);
    let contents = std::fs::read_to_string(&pidfile)
        .with_context(|| format!("reading {what} pidfile {}", pidfile.display()))?;
    let pid = contents.lines().next().unwrap_or("").trim();
    if pid.is_empty() {
        bail!("{what} pidfile {} is empty", pidfile.display());
    }
    let status = std::process::Command::new("kill")
        .args(["-9", pid])
        .status()
        .with_context(|| format!("kill -9 {pid} ({what})"))?;
    if !status.success() {
        bail!("kill -9 {pid} ({what}) exited with {status}");
    }
    println!("  killed {what} pid {pid} (SIGKILL)");
    Ok(())
}

/// Recycle the compute so a sub-scenario starts (or resumes) from a fresh
/// endpoint. Tolerant of a still-running compute (immediate stop) and of a
/// `kill -9`'d one: a hard kill leaves a stale `postmaster.pid`, so
/// `status()` reads `Crashed` (not `Stopped`) and the control-plane
/// duplicate-endpoint guard refuses `endpoint start` — clear the dead
/// pidfile first.
fn recycle_compute(c: &Cluster) -> anyhow::Result<()> {
    c.yb_quiet(&["endpoint", "stop", "main", "--mode", "immediate"]);
    let pgdata = c.repo_dir().join("endpoints/main/pgdata");
    let pidfile = pgdata.join("postmaster.pid");
    if let Ok(contents) = std::fs::read_to_string(&pidfile) {
        let pid = contents.lines().next().unwrap_or("").trim();
        if !pid.is_empty() && !Path::new(&format!("/proc/{pid}")).exists() {
            let _ = std::fs::remove_file(&pidfile);
        }
    }
    // `pg_dynshmem` is a symlink to `/dev/shm` (root-owned, sticky). When a
    // post-crash restart re-fetches a full basebackup, the unpack tries to
    // chmod the symlink target and fails with EPERM. Drop the stale symlink
    // so the unpack recreates it cleanly.
    let _ = std::fs::remove_file(pgdata.join("pg_dynshmem"));
    c.yb(&["endpoint", "start", "main"])
}

/// Fail loudly on the task-23 "YB WAL hole" signature in either the SK or
/// PS log — the cold-cache mid-chunk resume bug that wedged the
/// walreceiver and deadlocked basebackup.
fn scan_for_wal_hole(c: &Cluster, label: &str) -> anyhow::Result<()> {
    let sk_log = c.repo_dir().join("endpoints/main/safekeeper/safekeeper-1.log");
    for (path, who) in [(c.ps_log(), "pageserver"), (sk_log, "safekeeper")] {
        let hits = count_matches(&path, "YB WAL hole");
        if hits > 0 {
            bail!("{label}: {hits} 'YB WAL hole' line(s) in {who} log — torn-write resume found no covering chunk");
        }
    }
    Ok(())
}

fn count_matches(path: &Path, needle: &str) -> usize {
    std::fs::read_to_string(path)
        .map(|s| s.lines().filter(|l| l.contains(needle)).count())
        .unwrap_or(0)
}
