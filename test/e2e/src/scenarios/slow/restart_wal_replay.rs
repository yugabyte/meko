//! (slow) Restarted pageserver catches up to a safekeeper that is well
//! ahead of the PS's persisted `disk_consistent_lsn`. Reproduces the
//! PS-WAL-replay-restart failure mode: PS stops while the compute keeps
//! writing into the SK, so on PS restart the SK holds a sizeable WAL
//! backlog above the PS's resume LSN. With the fix the walreceiver
//! streams the backlog forward; without it the SK terminates the
//! interpreted-WAL stream immediately and the PS is wedged.
//!
//! Unlike the `-yb` fast variant, this one restarts the compute endpoint
//! (forcing a basebackup that requires PS catch-up) and reads `n` back.

use std::time::Duration;

use anyhow::bail;
use regex::Regex;

use crate::cluster::{Cluster, MARKERS, step};
use crate::{db, psql};

pub async fn run(c: &Cluster) -> anyhow::Result<()> {
    let main = db::compute_conn(c.compute_port());
    let soak: u64 = std::env::var("SOAK_SECS").ok().and_then(|s| s.parse().ok()).unwrap_or(10);
    let start_timeout: u64 = std::env::var("ENDPOINT_START_TIMEOUT")
        .ok()
        .and_then(|s| s.parse().ok())
        .unwrap_or(60);

    step("Seed a single-page table and checkpoint");
    db::batch(
        &main,
        "DROP TABLE IF EXISTS single_page;
         CREATE TABLE single_page (id INT PRIMARY KEY, n BIGINT NOT NULL)
             WITH (autovacuum_enabled = false);
         INSERT INTO single_page (id, n) VALUES (1, 0);
         CHECKPOINT;",
    )
    .await?;

    let ts_start_lsn = db::query_text(&main, "SELECT pg_current_wal_lsn()::text").await?;
    println!("starting compute LSN: {ts_start_lsn}");

    step("Create the bump procedure (each iteration is its own transaction)");
    db::batch(
        &main,
        "CREATE OR REPLACE PROCEDURE bump_loop(iters INT) AS $$
         BEGIN
             FOR i IN 1..iters LOOP
                 UPDATE single_page SET n = n + 1 WHERE id = 1;
                 COMMIT;
             END LOOP;
         END;
         $$ LANGUAGE plpgsql;",
    )
    .await?;

    step("Spawn background UPDATE loop (CALL bump_loop, COMMIT per iter)");
    let mut bg = psql::spawn_bg(c.compute_port(), "CALL bump_loop(2000000);")?;
    tokio::time::sleep(Duration::from_secs(2)).await;
    println!("background loop ran for 2s pre-stop");

    step("Snapshot n after warmup");
    let n_before_stop = db::query_i64(&main, "SELECT n FROM single_page WHERE id = 1").await?;
    println!("n at PS-stop time: {n_before_stop}");

    step("Stop pageserver (immediate — no GoingAway frame, simulates a kill -9 restart)");
    c.yb(&["pageserver", "stop", "-m", "immediate"])?;

    step(&format!("Soak for {soak}s with PS down — UPDATEs build a WAL gap on the SK"));
    tokio::time::sleep(Duration::from_secs(soak)).await;

    step("Snapshot SK commit LSN — this is the gap the PS must replay forward");
    let pre_sk = db::query_text(&main, "SELECT pg_current_wal_lsn()::text")
        .await
        .unwrap_or_else(|_| "(unavailable while PS is down)".to_string());
    println!("compute LSN before SK stop: {pre_sk}");

    step("Stop safekeeper -m immediate (this will break the background loop's next commit)");
    c.yb(&["safekeeper", "stop", "-m", "immediate"])?;

    step("Reap the background loop (it should be wedged on SK)");
    let _ = bg.kill();
    let _ = bg.wait();

    step("Restart safekeeper, then pageserver");
    c.yb(&["safekeeper", "start"])?;
    c.yb(&["pageserver", "start"])?;
    tokio::time::sleep(Duration::from_secs(2)).await;

    step("Restart compute endpoint (forces basebackup from PS)");
    c.yb(&["endpoint", "stop", "main", "--mode", "immediate"])?;
    let started = c
        .yb_timeout(&["endpoint", "start", "main"], Duration::from_secs(start_timeout))
        .await?;
    if !started {
        eprintln!("FAIL — endpoint failed to start within {start_timeout}s");
        eprintln!("  most likely the pageserver could not catch up to the safekeeper's WAL gap");
        dump_tail(&c.ps_log(), 500, "pageserver log tail");
        bail!("endpoint failed to start within {start_timeout}s");
    }

    step("Read back n and verify catch-up");
    let n_after = match db::query_i64(&main, "SELECT n FROM single_page WHERE id = 1").await {
        Ok(n) => n,
        Err(e) => {
            eprintln!("FAIL — could not read row back from restarted cluster");
            eprintln!("  error: {e}");
            dump_tail(&c.ps_log(), 80, "pageserver log tail");
            bail!("could not read row back");
        }
    };
    println!("n after restart: {n_after}  (before PS stop: {n_before_stop})");

    if n_after < n_before_stop {
        bail!(
            "n went backwards ({n_after} < {n_before_stop}): basebackup served a stale snapshot"
        );
    }
    if n_after == n_before_stop {
        bail!(
            "n did not advance during the SK-only window (still {n_after}); \
             the WAL gap was zero, so the test isn't exercising the catch-up path"
        );
    }
    println!("  ok — n advanced by {} during the SK-only window", n_after - n_before_stop);

    step("Scan pageserver log for catch-up failure markers");
    let log = std::fs::read_to_string(c.ps_log()).unwrap_or_default();
    if log.is_empty() {
        bail!("pageserver log not readable at {}", c.ps_log().display());
    }
    let wedge = log.lines().filter(|l| l.contains("interpreted wal reader finished")).count();
    if wedge > 5 {
        eprintln!("FAIL — pageserver log shows {wedge} 'interpreted wal reader finished' retries");
        bail!("walreceiver wedged ({wedge} retries)");
    }
    println!("  ok — {wedge} 'interpreted wal reader finished' occurrence(s) (threshold 5)");

    let markers = Regex::new(MARKERS).expect("static regex");
    let hits: Vec<&str> = log.lines().filter(|l| markers.is_match(l)).collect();
    if !hits.is_empty() {
        eprintln!("FAIL — {} corruption marker(s) in pageserver.log", hits.len());
        for l in hits.iter().take(5) {
            eprintln!("{l}");
        }
        bail!("corruption markers in pageserver.log");
    }
    println!("  pageserver log clean of corruption markers");

    Ok(())
}

fn dump_tail(path: &std::path::Path, n: usize, label: &str) {
    eprintln!("  --- {label} ---");
    if let Ok(s) = std::fs::read_to_string(path) {
        let lines: Vec<&str> = s.lines().collect();
        for l in lines.iter().rev().take(n).rev() {
            eprintln!("{l}");
        }
    }
}
