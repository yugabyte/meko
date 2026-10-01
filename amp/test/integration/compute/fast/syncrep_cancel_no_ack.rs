//! Regression guard for #473: a cancel during a sync-rep wait must not ack
//! the commit. With the safekeeper down the commit exists only in scratch
//! local WAL, so the fork ignores the cancel and keeps waiting; the INSERT
//! succeeds only once the safekeeper is back and has its WAL.

use std::time::Duration;

use anyhow::{Context, bail};
use tokio_postgres::NoTls;

use crate::cluster::{Cluster, step};
use crate::db;
use crate::syncrep::{self, Insert, Parked};
use crate::tee_println;

/// How long a cancelled commit must stay unacknowledged.
const BLOCK_WINDOW: Duration = Duration::from_secs(8);
const RECOVERY_TIMEOUT: Duration = Duration::from_secs(60);
const NOTICE_TIMEOUT: Duration = Duration::from_secs(10);
const IGNORED_CANCEL: &str = "ignoring cancel request until the transaction is safely stored";

pub async fn run(c: &Cluster) -> anyhow::Result<()> {
    let conn = db::compute_conn(c.compute_port());
    db::batch(
        &conn,
        "DROP TABLE IF EXISTS syncrep_cancel; CREATE TABLE syncrep_cancel (id INT);",
    )
    .await?;

    let insert = syncrep::with_safekeeper_down(c, park_and_cancel(&conn)).await?;

    step("the cancelled INSERT lands once the safekeeper has its WAL");
    let n = tokio::time::timeout(RECOVERY_TIMEOUT, insert)
        .await
        .with_context(|| {
            format!("cancelled INSERT did not complete within {RECOVERY_TIMEOUT:?} of safekeeper restart")
        })?
        .context("insert task")?
        .context("cancelled INSERT failed after safekeeper restart")?;
    if n != 1 {
        bail!("expected the cancelled INSERT to insert 1 row, got {n}");
    }
    let rows = db::query_i64(&conn, "SELECT count(*) FROM syncrep_cancel").await?;
    if rows != 1 {
        bail!("expected 1 committed row, got {rows}");
    }
    db::batch(&conn, "DROP TABLE syncrep_cancel").await?;
    Ok(())
}

/// Parks an INSERT in the sync-rep wait, cancels it twice, and checks both
/// cancels were ignored. Returns the still-running INSERT.
async fn park_and_cancel(conn: &str) -> anyhow::Result<Insert> {
    let Parked {
        pid,
        token,
        insert,
        mut notices,
    } = syncrep::park_insert(conn, "syncrep_cancel").await?;

    step("cancel the blocked INSERT twice");
    token.cancel_query(NoTls).await?;
    // The WARNING shows the cancel reached the wait; the repeat cancel then
    // takes the fork's path that skips it.
    let warned = syncrep::wait_notice(&mut notices, IGNORED_CANCEL, NOTICE_TIMEOUT).await;
    token.cancel_query(NoTls).await?;

    tokio::time::sleep(BLOCK_WINDOW).await;
    if insert.is_finished() {
        match insert.await.context("insert task")? {
            Ok(n) => bail!("cancel acked the INSERT ({n} row) with the safekeeper down (#473)"),
            Err(e) => bail!("cancel ended the sync-rep wait instead of being ignored: {e}"),
        }
    }
    if !syncrep::in_syncrep(conn, pid).await? {
        bail!("the cancelled INSERT left the sync-rep wait without finishing");
    }
    // The row turns visible to other sessions only once the sync-rep wait
    // ends, and with the safekeeper down only the cancel can end it.
    let rows = db::query_i64(conn, "SELECT count(*) FROM syncrep_cancel").await?;
    if rows != 0 {
        bail!("the INSERT's row is visible with the safekeeper down (#473)");
    }
    // Checked last, so a fork without the fix fails on the ack above.
    warned.context("the cancel never reached the sync-rep wait")?;
    while let Ok(extra) = notices.try_recv() {
        if extra.message() == IGNORED_CANCEL {
            bail!("the repeat cancel logged the WARNING again");
        }
    }
    tee_println!("  ok: cancelled INSERT still waiting after {BLOCK_WINDOW:?}");
    Ok(insert)
}
