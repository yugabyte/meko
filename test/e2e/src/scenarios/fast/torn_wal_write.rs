//! Crash recovery across a torn / partial WAL write. Each sub-scenario
//! hammers a counter table with `INSERT id=i; COMMIT;` (so the durable
//! survivors must be a contiguous prefix `1..k`), then SIGKILLs one or
//! more components mid-flush — leaving WAL chunks queued in the producer
//! channel / mid-batch, never landed in `public.wals`. The full crash
//! matrix over {safekeeper (SK), pageserver (PS), compute}:
//!
//!   A. SK            B. compute        C. PS              (singles)
//!   D. SK + PS       E. SK + compute   F. PS + compute    (pairs)
//!   G. SK + PS + compute                                  (all)
//!
//! The post-crash invariant in every case: `count(*) == max(id)` read from
//! a fresh basebackup (a hole means a committed record below a surviving
//! one was lost — torn-replay corruption), the compute reconnects (no
//! basebackup deadlock), and neither log shows a "YB WAL hole". The shared
//! corruption gate (`run_scenario`) scans the PS log for the marker set on
//! top, and the suite Final verdict checks the correctness counter.
//!
//! KNOWN BUG: scenario A (safekeeper crash, surviving compute) reproduces
//! an open data-loss bug (tasks/34) — the SK kill-9 loses early acked
//! commits and trips the `durable_walk_unflushed_row` correctness counter
//! caught by the Final verdict. The loss is specific to crashing *only* the
//! safekeeper with a live compute; the pairs/triple that also restart the
//! compute or PS (D–G) recover cleanly. Expect this test to fail until 34
//! is fixed.

use std::path::Path;
use std::time::{Duration, Instant};

use anyhow::{Context, bail};

use crate::cluster::{Cluster, step};
use crate::tee_println;
use crate::{db, psql};

const SK_PID: &str = "endpoints/main/safekeeper/safekeeper.pid";
const PS_PID: &str = "endpoints/main/pageserver/pageserver.pid";
const PM_PID: &str = "endpoints/main/pgdata/postmaster.pid";

/// Seconds the background write loop runs before we yank components —
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

/// One crash mode: which components to SIGKILL mid-flush.
struct Mode {
    label: &'static str,
    table: &'static str,
    sk: bool,
    ps: bool,
    compute: bool,
}

pub async fn run(c: &Cluster) -> anyhow::Result<()> {
    let modes = [
        Mode { label: "A safekeeper", table: "torn_a", sk: true, ps: false, compute: false },
        Mode { label: "B compute", table: "torn_b", sk: false, ps: false, compute: true },
        Mode { label: "C pageserver", table: "torn_c", sk: false, ps: true, compute: false },
        Mode { label: "D safekeeper+pageserver", table: "torn_d", sk: true, ps: true, compute: false },
        Mode { label: "E safekeeper+compute", table: "torn_e", sk: true, ps: false, compute: true },
        Mode { label: "F pageserver+compute", table: "torn_f", sk: false, ps: true, compute: true },
        Mode { label: "G all three", table: "torn_g", sk: true, ps: true, compute: true },
    ];

    // Run every mode regardless of individual failures so one invocation
    // reports the whole matrix, then fail if any lost data.
    let mut results: Vec<(&str, anyhow::Result<()>)> = Vec::new();
    for m in &modes {
        results.push((m.label, crash_scenario(c, m).await));
    }

    step("torn-write summary");
    let mut failed = 0;
    for (label, r) in &results {
        match r {
            Ok(()) => tee_println!("  PASS — {label}"),
            Err(e) => {
                failed += 1;
                tee_println!("  FAIL — {label}: {e:#}");
            }
        }
    }
    if failed > 0 {
        bail!("{failed}/{} torn-write crash modes lost committed data", results.len());
    }
    Ok(())
}

/// Seed the counter table, hammer it with commit-per-row inserts, SIGKILL
/// the targeted components mid-flush, restart the crashed infra (SK then
/// PS — the compute is recycled inside `verify`), and assert recovery.
async fn crash_scenario(c: &Cluster, m: &Mode) -> anyhow::Result<()> {
    let conn = db::compute_conn(c.compute_port());

    step(&format!("{}: seed counter table + commit-per-row fill procedure", m.label));
    seed(&conn, m.table).await?;

    step(&format!("{}: spawn background commit-per-row loop", m.label));
    let mut bg = psql::spawn_bg(c.compute_port(), &format!("CALL {}_fill(100000000);", m.table))?;
    tokio::time::sleep(Duration::from_secs(warmup_secs())).await;

    step(&format!("{}: SIGKILL {} mid-flush", m.label, crashed_label(m)));
    if m.sk {
        kill9(c, SK_PID, "safekeeper")?;
    }
    if m.ps {
        kill9(c, PS_PID, "pageserver")?;
    }
    if m.compute {
        kill9(c, PM_PID, "postmaster")?;
    }
    let _ = bg.kill();
    let _ = bg.wait();

    step(&format!("{}: restart crashed infra (SK, then PS)", m.label));
    if m.sk {
        c.yb_quiet(&["safekeeper", "stop", "-m", "immediate"]);
        c.yb(&["safekeeper", "start"])?;
    }
    if m.ps {
        c.yb_quiet(&["pageserver", "stop", "-m", "immediate"]);
        c.yb(&["pageserver", "start"])?;
    }

    verify(c, &conn, m.table, m.label).await
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
    // masked by pages still warm in a surviving compute's shared_buffers
    // (several modes keep the compute alive across the crash). After this
    // the seqscan reconstructs every page from the pageserver at the
    // recovered LSN — a hole here is genuine durable data loss.
    step(&format!("{label}: recycle compute for a cache-free durable read"));
    recycle_compute(c)?;

    step(&format!("{label}: wait for compute to serve reads, then for replay to quiesce"));
    let deadline = Instant::now() + reconnect_timeout();
    // First, wait until the recycled compute accepts queries.
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
    // Then let post-crash WAL replay settle: a freshly-basebackup'd compute
    // catches up to the durable WAL tail in the background, so count/max
    // climb for a beat after it starts serving. Poll until two consecutive
    // reads agree (or the deadline) and assert on the settled values — a
    // genuine hole (scenario A) stays a hole once settled.
    let mut prev: Option<(i64, i64)> = None;
    let (cnt, mx) = loop {
        let cnt = db::query_i64(conn, &format!("SELECT count(*) FROM {table}")).await?;
        let mx = db::query_i64(conn, &format!("SELECT COALESCE(max(id), 0) FROM {table}")).await?;
        if prev == Some((cnt, mx)) || Instant::now() > deadline {
            break (cnt, mx);
        }
        prev = Some((cnt, mx));
        tokio::time::sleep(Duration::from_millis(1500)).await;
    };

    scan_for_wal_hole(c, label)?;

    step(&format!("{label}: assert durable rows are a contiguous prefix"));
    tee_println!("  {label}: count={cnt} max(id)={mx}");
    if cnt == 0 {
        bail!("{label}: no rows survived — workload never committed before the crash");
    }
    if cnt != mx {
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
    if cnt2 != mx2 || mx2 != mx + 5 {
        bail!("{label}: post-recovery write failed (count={cnt2} max={mx2}, expected {})", mx + 5);
    }
    tee_println!("  {label}: ok — contiguous through {mx2} after post-recovery write");
    Ok(())
}

/// Human label for the set of components a mode crashes.
fn crashed_label(m: &Mode) -> String {
    let mut v = Vec::new();
    if m.sk {
        v.push("safekeeper");
    }
    if m.ps {
        v.push("pageserver");
    }
    if m.compute {
        v.push("compute postmaster");
    }
    v.join(" + ")
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
    tee_println!("  killed {what} pid {pid} (SIGKILL)");
    Ok(())
}

/// Recycle the compute so a sub-scenario resumes from a fresh endpoint.
/// Tolerant of a still-running compute (immediate stop) and of a
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
