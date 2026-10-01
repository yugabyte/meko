//! Guard for #473: a session that exits during a safekeeper outage must not
//! hang in exit. Dropping its temp tables commits from proc_exit, where no
//! signal can end a sync-rep wait, so the fork skips the wait there.

use std::time::{Duration, Instant};

use anyhow::{Context, bail};

use crate::cluster::{self, Cluster, step};
use crate::db;
use crate::syncrep;
use crate::tee_println;

const EXIT_TIMEOUT: Duration = Duration::from_secs(15);
const RECOVERY_TIMEOUT: Duration = Duration::from_secs(60);

pub async fn run(c: &Cluster) -> anyhow::Result<()> {
    let conn = db::compute_conn(c.compute_port());
    db::batch(
        &conn,
        "DROP TABLE IF EXISTS syncrep_exit; CREATE TABLE syncrep_exit (id INT);",
    )
    .await?;
    let client = db::connect(&conn).await?;
    client
        .batch_execute("CREATE TEMP TABLE syncrep_exit_tmp (id INT)")
        .await?;
    let pid: i32 = client
        .query_one("SELECT pg_backend_pid()", &[])
        .await?
        .get(0);
    if !cluster::pid_alive(pid) {
        bail!("backend {pid} is not visible in /proc, so its exit cannot be watched");
    }

    let probe = syncrep::with_safekeeper_down(c, async {
        // A parked commit shows the outage is in effect before the session exits.
        let probe = syncrep::park_insert(&conn, "syncrep_exit").await?.insert;
        step("disconnect the session that owns a temp table");
        drop(client);
        wait_for_exit(&conn, pid).await?;
        Ok(probe)
    })
    .await?;

    tokio::time::timeout(RECOVERY_TIMEOUT, probe)
        .await
        .with_context(|| {
            format!(
                "probe INSERT did not complete within {RECOVERY_TIMEOUT:?} of safekeeper restart"
            )
        })?
        .context("insert task")?
        .context("probe INSERT failed after safekeeper restart")?;
    // Commits after the exit's temp-table drop, so that drop is durable too.
    db::batch(&conn, "DROP TABLE syncrep_exit").await?;
    Ok(())
}

async fn wait_for_exit(conn: &str, pid: i32) -> anyhow::Result<()> {
    let deadline = Instant::now() + EXIT_TIMEOUT;
    while cluster::pid_alive(pid) {
        if Instant::now() > deadline {
            let wait_event = db::query_text_opt(
                conn,
                &format!("SELECT wait_event FROM pg_stat_activity WHERE pid = {pid}"),
            )
            .await?;
            bail!("backend {pid} still exiting after {EXIT_TIMEOUT:?}, wait_event {wait_event:?}");
        }
        tokio::time::sleep(Duration::from_millis(200)).await;
    }
    tee_println!("  ok: the session's backend exited with the safekeeper down");
    Ok(())
}
