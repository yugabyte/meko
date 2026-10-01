//! Guard for #473's escape hatch: with the safekeeper down, a cancel no
//! longer ends a commit's sync-rep wait, so pg_terminate_backend() must, and
//! its WARNING must say the outcome is unknown.

use std::time::Duration;

use anyhow::{Context, bail};

use crate::cluster::{Cluster, step};
use crate::db;
use crate::syncrep::{self, Parked};
use crate::tee_println;

const EXIT_TIMEOUT: Duration = Duration::from_secs(10);
const TERMINATED: &str = "canceling the wait for synchronous replication and terminating connection due to administrator command";
const UNKNOWN_OUTCOME: &str =
    "The transaction was not yet safely stored, so it may or may not be committed.";

pub async fn run(c: &Cluster) -> anyhow::Result<()> {
    let conn = db::compute_conn(c.compute_port());
    db::batch(
        &conn,
        "DROP TABLE IF EXISTS syncrep_terminate; CREATE TABLE syncrep_terminate (id INT);",
    )
    .await?;
    syncrep::with_safekeeper_down(c, park_and_terminate(&conn)).await?;
    db::batch(&conn, "DROP TABLE syncrep_terminate").await?;
    Ok(())
}

async fn park_and_terminate(conn: &str) -> anyhow::Result<()> {
    let Parked {
        pid,
        insert,
        mut notices,
        ..
    } = syncrep::park_insert(conn, "syncrep_terminate").await?;

    step("terminate the blocked INSERT");
    let signalled =
        db::query_text(conn, &format!("SELECT pg_terminate_backend({pid})::text")).await?;
    if signalled != "true" {
        bail!("pg_terminate_backend({pid}) did not signal the backend");
    }
    let result = tokio::time::timeout(EXIT_TIMEOUT, insert)
        .await
        .with_context(|| {
            format!("pg_terminate_backend() did not end the sync-rep wait within {EXIT_TIMEOUT:?}")
        })?
        .context("insert task")?;
    match result {
        Ok(n) => bail!("the terminated INSERT reported success ({n} row)"),
        Err(e) if !e.is_closed() => {
            bail!("the terminated INSERT got an error instead of a closed connection: {e}")
        }
        Err(_) => {}
    }
    let warning = syncrep::wait_notice(&mut notices, TERMINATED, EXIT_TIMEOUT).await?;
    if warning.detail() != Some(UNKNOWN_OUTCOME) {
        bail!(
            "terminate WARNING DETAIL {:?}, expected {UNKNOWN_OUTCOME:?}",
            warning.detail()
        );
    }
    tee_println!("  ok: terminate ended the sync-rep wait with an unknown-outcome WARNING");
    Ok(())
}
