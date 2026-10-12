//! Regression guard for #1564: under `synchronous_commit=local` a commit was
//! acknowledged before its WAL was durable in the safekeeper.
//!
//! `local` waits only for the compute's own WAL flush, which on amp is not a
//! durability point (`fsync=off`, and a restart rebuilds pgdata from the
//! safekeepers' flush position). The fork makes `local` wait for the
//! safekeeper flush like `on`.
//!
//! The `sk-yb-wal-flush` failpoint holds the safekeeper's durability watermark
//! still, so a commit that waits for it blocks. Every level but `off` must
//! block; `off` is the control that the compute itself is still serving.

use std::time::Duration;

use anyhow::{Context, bail};

use crate::cluster::{Cluster, step};
use crate::db;
use crate::tee_println;

/// Per-batch stall. Must outlast the failpoint check plus [`BLOCK_WINDOW`];
/// disarming does not cut short a sleep already in progress.
const FLUSH_STALL: Duration = Duration::from_secs(30);
/// How long a waiting commit must stay unacknowledged.
const BLOCK_WINDOW: Duration = Duration::from_secs(8);
const PROBE_TIMEOUT: Duration = Duration::from_secs(45);
/// The tail of the in-flight [`FLUSH_STALL`] plus the consumer's drain.
const RECOVERY_TIMEOUT: Duration = Duration::from_secs(120);

const WAITING_LEVELS: [&str; 3] = ["local", "remote_write", "on"];

pub async fn run(c: &Cluster) -> anyhow::Result<()> {
    let http = c.daemon_client(c.sk_http_port())?;
    let failpoints_url = format!("http://127.0.0.1:{}/v1/failpoints", c.sk_http_port());
    let result = exercise(c, &http, &failpoints_url).await;
    // A failpoint left armed would stall every later test on this cluster.
    let _ = set_failpoint(&http, &failpoints_url, "off").await;
    result
}

async fn exercise(c: &Cluster, http: &reqwest::Client, failpoints_url: &str) -> anyhow::Result<()> {
    let conninfo = db::compute_conn(c.compute_port());
    let hits_before = sk_log_count(c, "failpoint \"sk-yb-wal-flush\": sleeping");

    step("Seed table");
    tokio::time::timeout(
        PROBE_TIMEOUT,
        db::batch(
            &conninfo,
            "DROP TABLE IF EXISTS sync_commit_local; \
             CREATE TABLE sync_commit_local (level TEXT);",
        ),
    )
    .await
    .context("seed timed out")??;

    step("Arm sk-yb-wal-flush");
    set_failpoint(
        http,
        failpoints_url,
        &format!("return({})", FLUSH_STALL.as_millis()),
    )
    .await?;

    // Also hands the consumer the batch that trips the failpoint.
    step("synchronous_commit=off commits while the watermark is frozen");
    tokio::time::timeout(PROBE_TIMEOUT, commit_at(&conninfo, "off"))
        .await
        .context("an `off` commit blocked — the compute itself is stuck")??;

    let deadline = tokio::time::Instant::now() + Duration::from_secs(15);
    while sk_log_count(c, "failpoint \"sk-yb-wal-flush\": sleeping") <= hits_before {
        if tokio::time::Instant::now() > deadline {
            bail!(
                "sk-yb-wal-flush never hit — the watermark is not frozen, so this run \
                 proves nothing (is the SK built with --features testing?)"
            );
        }
        tokio::time::sleep(Duration::from_millis(500)).await;
    }

    // The commits are left running: each backend stays parked in
    // SyncRepWaitForLSN and commits once the watermark moves.
    step("Every other level waits for the safekeeper");
    let mut waiters: Vec<_> = WAITING_LEVELS
        .iter()
        .map(|&level| tokio::spawn(commit_at(conninfo.clone(), level)))
        .collect();
    tokio::time::sleep(BLOCK_WINDOW).await;
    let mut acked = Vec::new();
    for (level, w) in WAITING_LEVELS.iter().zip(&mut waiters) {
        // Finished is only an ack if the commit succeeded; an error here is
        // not a durability result, so report it as what it is.
        if w.is_finished() {
            w.await
                .context("waiter task")?
                .with_context(|| format!("{level} commit failed while the watermark was frozen"))?;
            acked.push(*level);
        }
    }
    if !acked.is_empty() {
        bail!(
            "synchronous_commit={acked:?} acknowledged a commit while the safekeeper's \
             durability watermark was frozen — a compute crash would lose it (#1564)"
        );
    }
    tee_println!("{WAITING_LEVELS:?} all blocked for {BLOCK_WINDOW:?}");

    step("Disarm and require the parked commits to land");
    set_failpoint(http, failpoints_url, "off").await?;
    for (level, w) in WAITING_LEVELS.iter().zip(waiters) {
        tokio::time::timeout(RECOVERY_TIMEOUT, w)
            .await
            .with_context(|| format!("{level} commit never landed after disarm"))??
            .with_context(|| format!("{level} commit"))?;
    }

    let rows = db::query_i64(&conninfo, "SELECT count(*) FROM sync_commit_local").await?;
    if rows != 4 {
        bail!("expected 4 committed rows, got {rows}");
    }
    db::batch(&conninfo, "DROP TABLE sync_commit_local").await?;
    Ok(())
}

async fn commit_at(conninfo: impl AsRef<str>, level: &'static str) -> anyhow::Result<()> {
    db::batch(
        conninfo.as_ref(),
        &format!(
            "SET synchronous_commit = {level}; \
             INSERT INTO sync_commit_local VALUES ('{level}');"
        ),
    )
    .await
}

async fn set_failpoint(http: &reqwest::Client, url: &str, actions: &str) -> anyhow::Result<()> {
    let resp = http
        .put(url)
        .json(&serde_json::json!([{ "name": "sk-yb-wal-flush", "actions": actions }]))
        .send()
        .await
        .context("PUT /v1/failpoints")?;
    if !resp.status().is_success() {
        bail!(
            "setting failpoint to {actions} failed: {} {}",
            resp.status(),
            resp.text().await.unwrap_or_default()
        );
    }
    Ok(())
}

fn sk_log_count(c: &Cluster, needle: &str) -> usize {
    let sk_dir = c.repo_dir().join("endpoints/main/safekeeper");
    let Some(log) = newest_log(&sk_dir) else {
        return 0;
    };
    std::fs::read_to_string(&log)
        .map(|text| text.matches(needle).count())
        .unwrap_or(0)
}

fn newest_log(dir: &std::path::Path) -> Option<std::path::PathBuf> {
    let mut logs: Vec<(std::time::SystemTime, std::path::PathBuf)> = std::fs::read_dir(dir)
        .ok()?
        .flatten()
        .filter(|e| e.path().extension().is_some_and(|x| x == "log"))
        .filter_map(|e| Some((e.metadata().ok()?.modified().ok()?, e.path())))
        .collect();
    logs.sort();
    logs.pop().map(|(_, p)| p)
}
