//! Crash recovery across a torn / partial WAL write. Each sub-scenario
//! hammers a counter table with `INSERT id=i; COMMIT;` (so the durable
//! survivors must be a contiguous prefix `1..k`), then SIGKILLs one or
//! more components mid-flush — leaving WAL chunks queued in the producer
//! channel / mid-batch, never landed in `safekeeper.wals`. The full crash
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
//! This is a **slow-suite** test: the failures in this family
//! (torn-WAL/catalog corruption #57, the FSM-truncation walreceiver wedge
//! #36, the compute-Init recycle race #37) are timing- and churn-dependent
//! — a single matrix pass reliably passes, and they only surface once
//! several iterations accumulate catalog/clog/rel state. `run` therefore
//! repeats the whole matrix `TORN_ITERATIONS` times (default 3). All of
//! #57/#36/#37 are fixed; this stays in `slow/` as their regression guard.

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

    // Slow-suite test: the torn-WAL / catalog corruption (and the
    // FSM-truncation wedge + recycle races in its family) are timing- and
    // churn-dependent — a single pass over the modes reliably passes; the
    // failures only surface once several iterations accumulate
    // catalog/clog/rel state. Repeat the matrix `iterations` times
    // (default 3, env-overridable for deeper hunts), recycling the compute
    // between iterations exactly as the suite runner does between
    // scenarios. Stop at the first failed mode: one losing mode condemns
    // the build, and `--no-fail-stop` leaves the cluster at that point for
    // forensics.
    let iterations: u32 = std::env::var("TORN_ITERATIONS")
        .ok()
        .and_then(|s| s.parse().ok())
        .filter(|&n| n >= 1)
        .unwrap_or(3);

    for iter in 1..=iterations {
        step(&format!("torn_wal_write iteration {iter}/{iterations}"));
        for m in &modes {
            match crash_scenario(c, m).await {
                Ok(()) => tee_println!("  PASS — {}", m.label),
                Err(e) => {
                    tee_println!("  FAIL — {}: {e:#}", m.label);
                    return Err(e).with_context(|| {
                        format!(
                            "torn-write iter {iter}/{iterations} mode {} lost committed data",
                            m.label
                        )
                    });
                }
            }
        }
        // Recycle between iterations so the accumulated cross-iteration
        // churn (compute restart) matches a `-n` run of the matrix.
        if iter < iterations {
            c.prep_for_next_test().await?;
        }
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
                // A common cause of this timeout is a caught PS thread
                // panic (e.g. walreceiver worker) that loops silently and
                // stalls ingest → basebackup `wait_lsn` never completes.
                // Name the panic if there is one, so the failure points at
                // the root rather than the downstream deadlock.
                let panic = first_panic_line(c)
                    .map(|p| format!(" — {p}"))
                    .unwrap_or_default();
                return Err(e).context(format!(
                    "{label}: compute never served reads within timeout \
                     (basebackup / walreceiver deadlock?){panic}"
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
        // The recycled compute can transiently accept a connection and then
        // crash during startup recovery (the walproposer "retake basebackup"
        // PANIC, #71) — compute_ctl re-basebackups and brings PG back up. So a
        // read here can hit a closing connection even though `SELECT 1` just
        // succeeded against the about-to-crash incarnation. Treat a query error
        // as "not settled yet" and retry within the deadline, the same way the
        // readiness loop above does; a fresh `query_i64` reconnects to the
        // restarted compute and the durable data is intact there. A genuine
        // hole stays a hole once the reads settle.
        let read = async {
            let cnt = db::query_i64(conn, &format!("SELECT count(*) FROM {table}")).await?;
            let mx =
                db::query_i64(conn, &format!("SELECT COALESCE(max(id), 0) FROM {table}")).await?;
            anyhow::Ok((cnt, mx))
        }
        .await;
        match read {
            Ok((cnt, mx)) => {
                if prev == Some((cnt, mx)) || Instant::now() > deadline {
                    break (cnt, mx);
                }
                prev = Some((cnt, mx));
                tokio::time::sleep(Duration::from_millis(1500)).await;
            }
            Err(_) if Instant::now() < deadline => {
                tokio::time::sleep(Duration::from_millis(500)).await;
            }
            Err(e) => {
                return Err(e).context(format!(
                    "{label}: durable read never settled within timeout \
                     (compute recycle did not converge)"
                ));
            }
        }
    };

    scan_for_wal_hole(c, label)?;
    scan_for_panic(c, label)?;
    assert_catalog_consistent(conn, label).await?;

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

/// Catch durable *catalog* tears that the user-data counter misses
/// (#57 part 2): a committed multi-page catalog transaction whose
/// pages didn't all survive recovery leaves the system catalogs
/// internally inconsistent. The sharpest signal seen so far is an orphan
/// composite rowtype — a `pg_type` (typtype='c') whose backing relation
/// (`typrelid`) is gone from `pg_class` — which then breaks the next
/// `CREATE TABLE` with `type "…" already exists`. Check both directions
/// for any `public`-schema table.
async fn assert_catalog_consistent(conn: &str, label: &str) -> anyhow::Result<()> {
    // Orphan rowtype: composite type with no live backing relation.
    let orphans = db::query_text(
        conn,
        "SELECT COALESCE(string_agg(t.typname, ', ' ORDER BY t.typname), '')
         FROM pg_type t
         JOIN pg_namespace n ON n.oid = t.typnamespace
         WHERE t.typtype = 'c' AND t.typrelid <> 0 AND n.nspname = 'public'
           AND NOT EXISTS (SELECT 1 FROM pg_class c WHERE c.oid = t.typrelid)",
    )
    .await?;
    if !orphans.is_empty() {
        bail!(
            "{label}: torn catalog — orphan composite rowtype(s) with no backing \
             pg_class relation: [{orphans}] (committed catalog pages lost in recovery)"
        );
    }
    // Reverse skew: a relation in `pg_class` whose declared rowtype
    // (`reltype`) is missing from `pg_type`.
    let missing_types = db::query_text(
        conn,
        "SELECT COALESCE(string_agg(c.relname, ', ' ORDER BY c.relname), '')
         FROM pg_class c
         JOIN pg_namespace n ON n.oid = c.relnamespace
         WHERE c.relkind = 'r' AND c.reltype <> 0 AND n.nspname = 'public'
           AND NOT EXISTS (SELECT 1 FROM pg_type t WHERE t.oid = c.reltype)",
    )
    .await?;
    if !missing_types.is_empty() {
        bail!(
            "{label}: torn catalog — relation(s) whose rowtype is missing from \
             pg_type: [{missing_types}] (committed catalog pages lost in recovery)"
        );
    }
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

/// Fail loudly on the #17 "YB WAL hole" signature in either the SK or
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

/// First panic line (truncated) from the PS or SK log, if any. A caught
/// walreceiver-worker panic otherwise only shows up as a `compute never
/// served reads` timeout (the panic loops, ingest stalls, basebackup
/// `wait_lsn` deadlocks) — surfacing the panic text turns that opaque
/// timeout into a named root cause.
fn first_panic_line(c: &Cluster) -> Option<String> {
    let sk_log = c.repo_dir().join("endpoints/main/safekeeper/safekeeper-1.log");
    for path in [c.ps_log(), sk_log] {
        if let Ok(s) = std::fs::read_to_string(&path) {
            if let Some(line) = s.lines().find(|l| l.contains("panicked") || l.contains("panic{thread")) {
                let who = if path.to_string_lossy().contains("safekeeper") { "safekeeper" } else { "pageserver" };
                // Truncate: panic lines can carry an 8 KB page-payload dump.
                let truncated: String = line.chars().take(400).collect();
                return Some(format!("{who} log panic: {truncated}"));
            }
        }
    }
    None
}

/// Fail loudly if any PS/SK thread panicked (e.g. a caught walreceiver
/// worker panic that would otherwise just reconnect-loop). Surfaces the
/// panic text so the failure names the root cause instead of a downstream
/// timeout.
fn scan_for_panic(c: &Cluster, label: &str) -> anyhow::Result<()> {
    if let Some(panic) = first_panic_line(c) {
        bail!("{label}: {panic}");
    }
    Ok(())
}
