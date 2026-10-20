//! A reconnect replay keeps each record's `prev_record_lsn`.
//!
//! Records replayed on a reconnect reached `Timeline::finish_write` with `last_record_lsn` already
//! past them, and were noted with it as their prev. A flush after the replay then persisted
//! `prev_record_lsn == consistent_lsn`, and the next basebackup gave the compute a back-pointer to
//! its own first record.
//!
//! The `fail-advance-consistent-lsn` failpoint keeps the writes above `consistent_lsn`, so the
//! reconnect replays them; the safekeeper is SIGKILLed so the pageserver stays up. The check only
//! means something when nothing was written after the replay, since a later record would be the
//! one persisted, with a correct prev; an attempt where that happened is retried.

use std::time::{Duration, Instant};

use anyhow::{Context, bail};
use tokio::time::sleep;

use crate::cluster::{Cluster, parse_lsn, step};
use crate::db;
use crate::tee_println;

const CLUSTER: &str = "prev_lsn_replay";
const ENDPOINT_ID: &str = "ep_prev_lsn_replay";
const HOLD_CONSISTENT_LSN: &str = "ps::page-store::fail-advance-consistent-lsn";
const TIMEOUT: Duration = Duration::from_secs(60);
const ATTEMPTS: usize = 3;

pub async fn run(c: &Cluster) -> anyhow::Result<()> {
    let pg_port = c.endpoint_port();

    step("provision an endpoint on its own amp cluster");
    let branch = c.cluster_create(CLUSTER)?;
    c.endpoint_create_on_branch(ENDPOINT_ID, &branch, pg_port)?;

    let result = run_inner(c, &branch, pg_port).await;

    step("Tear down");
    if let Ok(ps_http) = c.endpoint_ps_http_port(ENDPOINT_ID) {
        let _ = c.set_failpoint(ps_http, HOLD_CONSISTENT_LSN, "off").await;
    }
    c.yb_quiet(&["endpoint", "stop", ENDPOINT_ID, "--mode", "immediate"]);
    let _ = c.yb(&["branch", "drop", "--branch-id", &branch]);
    c.yb_quiet(&["pageserver", "stop", ENDPOINT_ID, "-m", "immediate"]);
    c.yb_quiet(&["safekeeper", "stop", ENDPOINT_ID, "-m", "immediate"]);
    result
}

async fn run_inner(c: &Cluster, branch: &str, pg_port: u16) -> anyhow::Result<()> {
    let compute = db::compute_conn(pg_port);
    db::batch(&compute, "CREATE TABLE prev_lsn (id INT PRIMARY KEY)").await?;
    for attempt in 1..=ATTEMPTS {
        step(&format!(
            "attempt {attempt}: write above a held consistent_lsn, then reconnect"
        ));
        if replay_then_check(c, branch, &compute, attempt).await? {
            return Ok(());
        }
        tee_println!("  inconclusive, retrying");
    }
    bail!("no attempt persisted the replayed record's own prev; it was never checked")
}

/// One reconnect replay followed by a flush. `Ok(false)` when the persisted prev isn't the
/// replayed record's: WAL arrived after the replay (a later record's prev was persisted), or a
/// flush race left it NULL.
async fn replay_then_check(
    c: &Cluster,
    branch: &str,
    compute: &str,
    attempt: usize,
) -> anyhow::Result<bool> {
    let ep_dir = c.repo_dir().join("endpoints").join(ENDPOINT_ID);
    let sk_pid = ep_dir.join("safekeeper/safekeeper.pid");
    let ps_log = ep_dir.join("pageserver/pageserver.log");
    let ps_http = c.endpoint_ps_http_port(ENDPOINT_ID)?;

    c.flush_branch(ps_http, branch, compute).await?;
    c.set_failpoint(ps_http, HOLD_CONSISTENT_LSN, "return")
        .await?;
    for i in 0..3 {
        let id = attempt * 10 + i;
        db::batch(compute, &format!("INSERT INTO prev_lsn VALUES ({id})")).await?;
    }
    let tip =
        parse_lsn(&db::query_text(compute, "SELECT pg_current_wal_flush_lsn()::text").await?)?;
    poll(|| async { Ok(parse_lsn(&c.ps_last_record_lsn(ps_http, branch).await?)? >= tip) })
        .await
        .context("the pageserver never ingested the writes")?;
    let held = c.consistent_lsn(branch).await?.unwrap_or(0) as u64;
    if held >= tip {
        bail!("consistent_lsn {held:#x} reached the tip {tip:#x}: nothing left to replay");
    }

    // The reconnect logs "caught up" once it has re-ingested up to the safekeeper's end.
    let log_start = std::fs::metadata(&ps_log)
        .map(|m| m.len() as usize)
        .unwrap_or(0);
    c.kill9(&sk_pid, "safekeeper")?;
    c.yb_quiet(&["safekeeper", "stop", ENDPOINT_ID, "-m", "immediate"]);
    c.yb(&["safekeeper", "start", ENDPOINT_ID])?;
    poll(|| async {
        let log = std::fs::read(&ps_log).unwrap_or_default();
        Ok(log
            .get(log_start..)
            .is_some_and(|new| String::from_utf8_lossy(new).contains("caught up at LSN")))
    })
    .await
    .context("the reconnect never caught up")?;
    let log = std::fs::read(&ps_log).unwrap_or_default();
    let reconnect_log = String::from_utf8_lossy(log.get(log_start..).unwrap_or_default());
    let startpoint = reconnect_log
        .rsplit("starting replication from ")
        .next()
        .and_then(|rest| rest.split(',').next())
        .context("no \"starting replication from\" line for the reconnect")?;
    let startpoint = parse_lsn(startpoint)?;
    if startpoint >= tip {
        bail!(
            "the reconnect started at {startpoint:#x}, not below the tip {tip:#x}: nothing was replayed"
        );
    }

    c.set_failpoint(ps_http, HOLD_CONSISTENT_LSN, "off").await?;
    c.flush_branch(ps_http, branch, compute).await?;
    let (consistent, prev) = c.branch_state_lsns(branch).await?;
    let (consistent, prev) = (consistent.unwrap_or(0) as u64, prev.unwrap_or(0) as u64);
    tee_println!(
        "  tip {tip:#x}; persisted consistent_lsn {consistent:#x}, prev_record_lsn {prev:#x}"
    );
    if consistent < tip {
        bail!("consistent_lsn {consistent:#x} is below the tip {tip:#x}");
    }
    if consistent > tip {
        tee_println!("  WAL arrived after the replay");
        return Ok(false);
    }
    if prev == 0 {
        tee_println!("  prev_record_lsn is NULL (a flush raced finish_write)");
        return Ok(false);
    }
    if prev >= consistent {
        bail!(
            "prev_record_lsn {prev:#x} is not below consistent_lsn {consistent:#x}: \
             the replay noted its records with last_record_lsn as their prev"
        );
    }
    Ok(true)
}

async fn poll<F, Fut>(mut f: F) -> anyhow::Result<()>
where
    F: FnMut() -> Fut,
    Fut: std::future::Future<Output = anyhow::Result<bool>>,
{
    let deadline = Instant::now() + TIMEOUT;
    loop {
        if f().await? {
            return Ok(());
        }
        if Instant::now() >= deadline {
            bail!("timed out after {TIMEOUT:?}");
        }
        sleep(Duration::from_millis(500)).await;
    }
}
