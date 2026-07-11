//! Regression for the getpage fd leak (ISSUE-pageserver-getpage-fd-leak):
//! when the compute disconnects, the pipelined page_service executor must
//! ABORT its in-flight getpage reads and release the socket, not DRAIN them.
//!
//! Draining a *stalled* read to a peer that has already gone away holds the
//! connection's fd in CLOSE-WAIT for the whole stall; under a getpage-stall
//! storm those pile up until the pageserver dies with
//! `accept(): Too many open files`.
//!
//! Repro: stall getpage via the `ps::pipeline-execute-batch` failpoint, fault
//! a cold page from a throwaway backend so a batch is in flight, `kill -9`
//! that backend mid-stall (immediate FIN/RST to the PS), and assert the PS
//! *finishes* the page_service connection promptly — well under the stall.
//! With the fix the connection is released ~instantly (abort); without it the
//! connection lingers until the stalled read drains (~STALL_MS).

use std::time::{Duration, Instant};

use anyhow::{bail, ensure};

use crate::cluster::{Cluster, step};
use crate::tee_println;
use crate::{db, page_api};

const N_ROWS: i64 = 300_000;
const STALL_MS: u64 = 15_000;
/// A healthy abort releases the connection ~instantly; this ceiling tolerates
/// host contention while still tripping on the ~STALL_MS drain of the unfixed
/// path.
const RELEASE_CEILING: Duration = Duration::from_secs(5);

pub async fn run(c: &Cluster) -> anyhow::Result<()> {
    let conninfo = db::compute_conn(c.compute_port());

    step("seed a multi-page table");
    db::batch(
        &conninfo,
        "DROP TABLE IF EXISTS ps_disc; \
         CREATE TABLE ps_disc (id int, v int) WITH (autovacuum_enabled = off);",
    )
    .await?;
    db::batch(
        &conninfo,
        &format!(
            "INSERT INTO ps_disc \
             SELECT g, (g * 2654435761) % 1000000 FROM generate_series(1, {N_ROWS}) g"
        ),
    )
    .await?;

    let toml = c
        .repo_dir()
        .join("endpoints/main/pageserver/pageserver.toml");
    let original = std::fs::read_to_string(&toml)?;

    let result = run_inner(c, &conninfo, &original).await;

    // Cleanup is best-effort and must not mask the test result: the killed
    // backend + PS restart can leave the compute briefly unavailable. Clear the
    // failpoint and drop the scratch table while the cluster is still up, then
    // restore the PS config.
    let _ = clear_failpoint(c).await;
    let _ = db::batch(&conninfo, "DROP TABLE IF EXISTS ps_disc").await;
    page_api::restore_ps(c, &original);
    result
}

async fn run_inner(c: &Cluster, conninfo: &str, original: &str) -> anyhow::Result<()> {
    step("pipelined PS (depth 16), cold PS + cold compute so reads reach the pipeline");
    page_api::restart_with_pipeline_depth(c, original, 16)?;
    c.yb_quiet(&["endpoint", "stop", "main", "--mode", "immediate"]);
    c.yb(&["endpoint", "start", "main"])?;
    tokio::time::sleep(Duration::from_secs(3)).await;

    // Throwaway backend, opened BEFORE arming the failpoint (its startup
    // catalog faults must not stall). Capture its OS pid so we can kill it
    // mid-stall to force an immediate disconnect at the PS.
    step("open a throwaway backend and capture its pid");
    let client = db::connect(conninfo).await?;
    let pid: i32 = client.query_one("SELECT pg_backend_pid()", &[]).await?.get(0);
    tee_println!("  throwaway backend pid = {pid}");

    step(&format!("arm ps::pipeline-execute-batch to stall {STALL_MS}ms"));
    set_failpoint(c, "ps::pipeline-execute-batch", &format!("return({STALL_MS})")).await?;

    step("fault a cold page from the throwaway backend (stalls in the pipeline)");
    let stalling = tokio::spawn(async move {
        // Errors when we kill the backend — expected; we only need the getpage
        // to be in flight at the PS.
        let _ = client
            .batch_execute("SET effective_io_concurrency = 32; SELECT count(*) FROM ps_disc;")
            .await;
    });
    // Give the getpage time to reach the PS and stall.
    tokio::time::sleep(Duration::from_secs(2)).await;

    let before = ps_page_service_finished(c).await?;
    tee_println!("  page_service finished counter before kill = {before}");

    step("kill the throwaway backend mid-stall (immediate disconnect at the PS)");
    let killed = std::process::Command::new("kill")
        .args(["-9", &pid.to_string()])
        .status()?
        .success();
    ensure!(killed, "failed to kill backend pid {pid}");
    let t0 = Instant::now();

    step("assert the PS releases the connection promptly (abort, not drain)");
    let deadline = Instant::now() + Duration::from_millis(STALL_MS + 8_000);
    let mut released = None;
    while Instant::now() < deadline {
        if ps_page_service_finished(c).await? > before {
            released = Some(t0.elapsed());
            break;
        }
        tokio::time::sleep(Duration::from_millis(200)).await;
    }
    stalling.abort();

    match released {
        Some(e) => {
            tee_println!(
                "  page_service connection released {:.2}s after disconnect",
                e.as_secs_f64()
            );
            ensure!(
                e < RELEASE_CEILING,
                "PS took {:.2}s to release the page_service connection after the compute \
                 disconnected (ceiling {:.0}s) — it drained the stalled getpage instead of \
                 aborting (the fd leak)",
                e.as_secs_f64(),
                RELEASE_CEILING.as_secs_f64()
            );
            tee_println!("  ok — aborted in-flight and released the fd promptly");
        }
        None => bail!(
            "page_service `finished` counter never advanced after killing the backend \
             (connection not released within {}s)",
            (STALL_MS + 8_000) / 1000
        ),
    }
    Ok(())
}

/// Live `pageserver_live_connections_finished{...="page_service"}` count.
async fn ps_page_service_finished(c: &Cluster) -> anyhow::Result<i64> {
    let body = reqwest::get(format!("http://127.0.0.1:{}/metrics", c.ps_http_port()))
        .await?
        .error_for_status()?
        .text()
        .await?;
    for line in body.lines() {
        if line.starts_with("pageserver_live_connections_finished{")
            && line.contains("pageserver_connection_kind=\"page_service\"")
        {
            if let Some(v) = line.rsplit(' ').next() {
                return Ok(v.trim().parse::<f64>().unwrap_or(0.0) as i64);
            }
        }
    }
    Ok(0)
}

async fn set_failpoint(c: &Cluster, name: &str, actions: &str) -> anyhow::Result<()> {
    let url = format!("http://127.0.0.1:{}/v1/failpoints", c.ps_http_port());
    let body = serde_json::json!([{ "name": name, "actions": actions }]);
    let resp = reqwest::Client::new().put(&url).json(&body).send().await?;
    ensure!(
        resp.status().is_success(),
        "PUT {name} failpoint -> {}",
        resp.status()
    );
    Ok(())
}

async fn clear_failpoint(c: &Cluster) -> anyhow::Result<()> {
    set_failpoint(c, "ps::pipeline-execute-batch", "off").await
}
