//! PS-restart catch-up over YB-WAL: write loop → stop PS → soak → stop
//! SK → restart both → verify the PS's `last_record_lsn` advanced past
//! the start LSN (observed via the PS HTTP timeline-detail, to avoid the
//! compute-restart instability the slow variant tolerates). Exercises
//! the YB-WAL read path on PS catch-up.

use std::time::{Duration, Instant};

use anyhow::bail;
use regex::Regex;

use crate::cluster::{Cluster, MARKERS, step};
use crate::tee_println;
use crate::{db, psql};

pub async fn run(c: &Cluster) -> anyhow::Result<()> {
    let main = db::compute_conn(c.compute_port());
    let branch = c.branch();
    let soak: u64 = std::env::var("SOAK_SECS").ok().and_then(|s| s.parse().ok()).unwrap_or(10);
    let http_timeout: u64 = std::env::var("PS_HTTP_TIMEOUT")
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
         CHECKPOINT;
         CREATE OR REPLACE PROCEDURE bump_loop(iters INT) AS $$
         BEGIN
             FOR i IN 1..iters LOOP
                 UPDATE single_page SET n = n + 1 WHERE id = 1;
                 COMMIT;
             END LOOP;
         END;
         $$ LANGUAGE plpgsql;",
    )
    .await?;

    step("Snapshot starting LSN (for the catch-up assertion)");
    let ts_start_lsn = db::query_text(&main, "SELECT pg_current_wal_lsn()::text").await?;
    tee_println!("starting compute LSN: {ts_start_lsn}");

    step("Spawn background UPDATE loop");
    let mut bg = psql::spawn_bg(c.compute_port(), "CALL bump_loop(2000000);")?;
    tokio::time::sleep(Duration::from_secs(2)).await;
    tee_println!("background loop ran for 2s pre-stop");

    let n_before_stop = db::query_i64(&main, "SELECT n FROM single_page WHERE id = 1").await?;
    tee_println!("n at PS-stop time: {n_before_stop}");

    step("Stop pageserver (immediate)");
    c.yb(&["pageserver", "stop", "-m", "immediate"])?;

    step(&format!("Soak {soak}s — WAL flushes into safekeeper.wals via yb_storage_conn"));
    tokio::time::sleep(Duration::from_secs(soak)).await;

    step("Stop safekeeper -m immediate");
    c.yb(&["safekeeper", "stop", "-m", "immediate"])?;

    step("Reap background loop");
    let _ = bg.kill();
    let _ = bg.wait();

    step("Restart safekeeper (loads control file from remote storage / YB blobs)");
    c.yb(&["safekeeper", "start"])?;

    step("Restart pageserver");
    c.yb(&["pageserver", "start"])?;
    tokio::time::sleep(Duration::from_secs(2)).await;

    step("Verify PS catch-up via HTTP API (avoid compute restart instability)");
    let http = reqwest::Client::new();
    let deadline = Instant::now() + Duration::from_secs(http_timeout);
    let mut last_record_lsn = String::new();
    let ps_http = c.ps_http_port();
    while Instant::now() < deadline {
        if let Some(br) = first_branch_id(&http, ps_http, branch).await {
            if let Some(lsn) = last_record_lsn_of(&http, ps_http, &br).await {
                if !lsn.is_empty() && lsn != "0/0" && lsn != ts_start_lsn {
                    last_record_lsn = lsn;
                    break;
                }
            }
        }
        tokio::time::sleep(Duration::from_secs(1)).await;
    }

    if last_record_lsn.is_empty() || last_record_lsn == "0/0" {
        eprintln!(
            "FAIL — PS last_record_lsn never advanced past {ts_start_lsn} within {http_timeout}s"
        );
        dump_tail(&c.ps_log(), 80, "pageserver log tail");
        dump_tail(
            &c.repo_dir().join("endpoints/main/safekeeper/safekeeper-1.log"),
            80,
            "safekeeper log tail",
        );
        bail!("PS last_record_lsn never advanced");
    }
    tee_println!("  ok — PS last_record_lsn advanced to {last_record_lsn} (start was {ts_start_lsn})");

    step("Scan logs for catch-up failure markers");
    let log = std::fs::read_to_string(c.ps_log()).unwrap_or_default();
    if log.is_empty() {
        bail!("pageserver log not readable at {}", c.ps_log().display());
    }
    let wedge = log.lines().filter(|l| l.contains("interpreted wal reader finished")).count();
    if wedge > 5 {
        eprintln!("FAIL — pageserver log shows {wedge} 'interpreted wal reader finished' retries");
        bail!("walreceiver wedged ({wedge} retries)");
    }
    tee_println!("  ok — {wedge} 'interpreted wal reader finished' occurrence(s) (threshold 5)");

    let markers = Regex::new(MARKERS).expect("static regex");
    let hits: Vec<&str> = log.lines().filter(|l| markers.is_match(l)).collect();
    if !hits.is_empty() {
        eprintln!("FAIL — {} corruption marker(s) in pageserver.log", hits.len());
        for l in hits.iter().take(5) {
            eprintln!("{l}");
        }
        bail!("corruption markers in pageserver.log");
    }
    tee_println!("  pageserver log clean of corruption markers");

    Ok(())
}

/// First branch id from the PS `/v1/branch/<id>/timeline` list.
async fn first_branch_id(http: &reqwest::Client, ps_http: u16, branch: &str) -> Option<String> {
    let url = format!("http://127.0.0.1:{ps_http}/v1/branch/{branch}/timeline");
    let v: serde_json::Value = http.get(&url).send().await.ok()?.json().await.ok()?;
    v.get(0)?.get("branch_id")?.as_str().map(str::to_string)
}

/// `last_record_lsn` from the PS branch-detail endpoint.
async fn last_record_lsn_of(
    http: &reqwest::Client,
    ps_http: u16,
    branch: &str,
) -> Option<String> {
    let url = format!("http://127.0.0.1:{ps_http}/v1/branch/{branch}");
    let v: serde_json::Value = http.get(&url).send().await.ok()?.json().await.ok()?;
    v.get("last_record_lsn")?.as_str().map(str::to_string)
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
