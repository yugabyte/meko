//! A WAL batch that crosses a segment boundary is two `wals` rows. Once the
//! bucket's tablet has split they can land on different tablets, and a flush
//! cut short — a fence refusal followed by the exit, or a crash — can store the
//! upper row without the lower one. Nothing in the batch was acked, but the next
//! load must find the hole: rehydrating past it reports WAL flushed across bytes
//! nobody can serve, and the pageserver stalls on `YB WAL hole`.
//!
//! `sk-wal-flush-drop-leading-run` builds that state without a tablet split: it
//! lands every run of a multi-run batch but the lowest, then exits the way a
//! lapsed fence does. The safekeeper the supervisor restarts has to trim back to
//! the hole, take the compute's re-stream from there, and let the pageserver
//! catch up.

use std::path::Path;
use std::time::{Duration, Instant};

use anyhow::{Context, bail};

use crate::cluster::{Cluster, step};
use crate::{db, psql, tee_println};

const TABLE: &str = "partial_batch_hole";
const ROWS_PER_COMMIT: i64 = 2000;
const FAILPOINT_MARKER: &str = "failpoint sk-wal-flush-drop-leading-run: landed";
const HOLE_WARNING: &str = "hole in the persisted WAL tail; end-of-WAL is the contiguous end";
const LOAD_REFUSED: &str = "so the load refuses to delete them";
/// Long enough to stream several segments of WAL from a debug compute.
const EXIT_TIMEOUT: Duration = Duration::from_secs(180);
const RESTART_TIMEOUT: Duration = Duration::from_secs(60);
const PROBE_TIMEOUT: Duration = Duration::from_secs(60);
const CATCH_UP_TIMEOUT: Duration = Duration::from_secs(120);

pub async fn run(c: &Cluster) -> anyhow::Result<()> {
    let conninfo = db::compute_conn(c.compute_port());
    let http = c.daemon_client(c.sk_http_port())?;
    let sk_dir = c.repo_dir().join("endpoints/main/safekeeper");

    step("Seed the table and the fill procedure");
    db::batch(
        &conninfo,
        &format!(
            "DROP TABLE IF EXISTS {TABLE};
             CREATE TABLE {TABLE} (a BIGINT, b TEXT);
             CREATE OR REPLACE PROCEDURE {TABLE}_fill(iters INT) AS $$
             BEGIN
                 FOR i IN 1..iters LOOP
                     INSERT INTO {TABLE} SELECT g, repeat('x', 500)
                       FROM generate_series(1, {ROWS_PER_COMMIT}) g;
                     COMMIT;
                 END LOOP;
             END;
             $$ LANGUAGE plpgsql;"
        ),
    )
    .await?;
    // `main`'s logs outlive `-n` iterations, so every match counts from here.
    let markers_before = log_count(&sk_dir, FAILPOINT_MARKER);
    let holes_before = log_count(&sk_dir, HOLE_WARNING);
    let refusals_before = log_count(&sk_dir, LOAD_REFUSED);

    step("Arm sk-wal-flush-drop-leading-run");
    let resp = http
        .put(format!("http://127.0.0.1:{}/v1/failpoints", c.sk_http_port()))
        .json(&serde_json::json!([{
            "name": "sk-wal-flush-drop-leading-run",
            "actions": "return",
        }]))
        .send()
        .await
        .context("PUT /v1/failpoints")?;
    if !resp.status().is_success() {
        bail!(
            "arming failpoint failed: {} {}",
            resp.status(),
            resp.text().await.unwrap_or_default()
        );
    }

    // The load runs in the background: commits stall while the safekeeper is
    // down. The endpoint supervisor restarts it on exit, as in production, so
    // the test follows the logs rather than the process.
    step("Stream WAL across segment boundaries until the failpoint fires");
    let mut bg = psql::spawn_bg(c.compute_port(), &format!("CALL {TABLE}_fill(100000);"))?;
    let fired = wait_for_any_log(&sk_dir, &[(FAILPOINT_MARKER, markers_before)], EXIT_TIMEOUT)
        .await
        .is_some();
    let restart = if fired {
        wait_for_any_log(
            &sk_dir,
            &[(HOLE_WARNING, holes_before), (LOAD_REFUSED, refusals_before)],
            RESTART_TIMEOUT,
        )
        .await
    } else {
        None
    };
    let _ = bg.kill();
    let _ = bg.wait();
    // The backend running the fill outlives its psql and would keep committing.
    let _ = db::batch(
        &conninfo,
        &format!(
            "SELECT pg_terminate_backend(pid) FROM pg_stat_activity \
              WHERE query LIKE 'CALL {TABLE}_fill%' AND pid <> pg_backend_pid()"
        ),
    )
    .await;
    if !fired {
        bail!(
            "the failpoint never fired within {EXIT_TIMEOUT:?}: no batch crossed a segment \
             boundary, or the SK was not built with --features testing"
        );
    }
    match restart {
        Some(0) => {}
        Some(_) => bail!(
            "the restarted safekeeper refused the load instead of trimming: it saw more than one \
             batch above the hole, which one cut-short flush cannot leave"
        ),
        None => bail!(
            "the restarted safekeeper did not report the hole the failpoint left \
             within {RESTART_TIMEOUT:?}"
        ),
    }
    tee_println!("  the restarted safekeeper found the partial batch and trimmed to it");

    step("Commits land again");
    tokio::time::timeout(
        PROBE_TIMEOUT,
        db::batch(&conninfo, &format!("INSERT INTO {TABLE} VALUES (-1, 'probe')")),
    )
    .await
    .context("probe commit did not land")??;

    step("The pageserver ingests past the hole");
    let tip = db::query_text(&conninfo, "SELECT pg_current_wal_flush_lsn()::text").await?;
    let url = format!(
        "http://127.0.0.1:{}/v1/branch/{}/flush?lsn={tip}",
        c.ps_http_port(),
        c.branch()
    );
    let ps = c.daemon_client(c.ps_http_port())?;
    let resp = tokio::time::timeout(CATCH_UP_TIMEOUT, ps.put(&url).send())
        .await
        .with_context(|| {
            format!("the pageserver did not reach {tip} within {CATCH_UP_TIMEOUT:?}")
        })??;
    if !resp.status().is_success() {
        bail!("pageserver flush to {tip} returned {}", resp.status());
    }
    if log_count(&c.ps_log(), "YB WAL hole") > 0 {
        bail!("the pageserver hit the hole: the safekeeper served WAL across it");
    }

    step("Every committed transaction is whole");
    let rows = db::query_i64(&conninfo, &format!("SELECT count(*) FROM {TABLE} WHERE a > 0")).await?;
    if rows % ROWS_PER_COMMIT != 0 {
        bail!("{rows} fill rows is not a whole number of {ROWS_PER_COMMIT}-row commits");
    }
    let probes =
        db::query_i64(&conninfo, &format!("SELECT count(*) FROM {TABLE} WHERE a < 0")).await?;
    if probes != 1 {
        bail!("expected the one probe row, found {probes}");
    }
    tee_println!("  {rows} fill rows and the probe survived");

    db::batch(&conninfo, &format!("DROP TABLE {TABLE}; DROP PROCEDURE {TABLE}_fill")).await?;
    Ok(())
}

/// Waits until `dir`'s logs hold more copies of one of `needles` than its
/// baseline, and returns which one; `None` on timeout.
async fn wait_for_any_log(
    dir: &Path,
    needles: &[(&str, usize)],
    timeout: Duration,
) -> Option<usize> {
    let deadline = Instant::now() + timeout;
    while Instant::now() < deadline {
        if let Some(i) = needles
            .iter()
            .position(|&(needle, seen)| log_count(dir, needle) > seen)
        {
            return Some(i);
        }
        tokio::time::sleep(Duration::from_millis(250)).await;
    }
    None
}

/// Occurrences of `needle` in `path`, or across every `*.log` under it when it
/// is a directory — a restarted daemon may log to a new file.
fn log_count(path: &Path, needle: &str) -> usize {
    let files: Vec<std::path::PathBuf> = if path.is_dir() {
        std::fs::read_dir(path)
            .map(|d| {
                d.flatten()
                    .map(|e| e.path())
                    .filter(|p| p.extension().is_some_and(|x| x == "log"))
                    .collect()
            })
            .unwrap_or_default()
    } else {
        vec![path.to_path_buf()]
    };
    files
        .iter()
        .filter_map(|f| std::fs::read_to_string(f).ok())
        .map(|text| text.matches(needle).count())
        .sum()
}
