//! AMP-74: yb_amp_start_unlogged_build sets the build state before it adds
//! the relation to the shared list of unlogged builds. A build that fails
//! there because the list is full, inside a savepoint, must still forget that
//! state when the savepoint is rolled back: a retry must fail the same way,
//! and COMMIT must keep the transaction's work.
//!
//! The list holds MaxBackends entries, and each failed CREATE INDEX
//! CONCURRENTLY keeps one until its invalid index is dropped. The endpoint
//! lowers its backend settings so that few failed builds fill it.

use anyhow::{Context, anyhow, bail, ensure};
use tokio_postgres::error::SqlState;

use crate::cluster::{Cluster, step};
use crate::db;
use crate::tee_println;

const EP: &str = "ep_ubfull";
/// Well above MaxBackends under the endpoint's settings below.
const MAX_FAILED_CICS: u32 = 200;

pub async fn run(c: &Cluster) -> anyhow::Result<()> {
    let port = c.endpoint_port();

    step("provision an endpoint with few backend slots");
    let branch_id = c.branch_create_named("ubfull", c.branch())?;
    c.yb(&[
        "endpoint",
        "create",
        EP,
        "--branch-id",
        &branch_id,
        "--pg-version",
        "18",
        "--pg-port",
        &port.to_string(),
        "--pg-setting",
        "max_connections=20",
        "--pg-setting",
        "max_worker_processes=8",
        "--pg-setting",
        "autovacuum_worker_slots=4",
        "--pg-setting",
        "max_wal_senders=2",
    ])?;
    c.yb(&["endpoint", "start", EP])?;

    let result = run_inner(port).await;

    step("cleanup: deprovision the endpoint");
    c.yb_quiet(&["endpoint", "stop", EP, "--destroy"]);
    result
}

async fn run_inner(port: u16) -> anyhow::Result<()> {
    let ep = db::compute_conn(port);
    db::batch(
        &ep,
        "CREATE TABLE ubfull_t (id int, a int[]);
         INSERT INTO ubfull_t SELECT g, ARRAY[g % 10, g] FROM generate_series(1, 1000) g;",
    )
    .await?;
    let client = db::connect(&ep).await?;

    step("fill the shared list with failed CREATE INDEX CONCURRENTLY builds");
    let mut failed = 0;
    loop {
        ensure!(
            failed < MAX_FAILED_CICS,
            "{failed} failed concurrent builds did not fill the list"
        );
        let cic = format!(
            "CREATE INDEX CONCURRENTLY ubfull_cic_{failed} ON ubfull_t \
             USING gin ((ARRAY[100 / (id - 500)]))"
        );
        match client.batch_execute(&cic).await {
            Err(e) if e.code() == Some(&SqlState::DIVISION_BY_ZERO) => failed += 1,
            Err(e) if e.code() == Some(&SqlState::CONFIGURATION_LIMIT_EXCEEDED) => break,
            Err(e) => return Err(e).context("failing concurrent build"),
            Ok(()) => bail!("a concurrent build that divides by zero succeeded"),
        }
    }
    tee_println!("  the list was full after {failed} failed builds");

    step("a build in a savepoint finds the list full, then a retry and COMMIT");
    client
        .batch_execute("BEGIN; INSERT INTO ubfull_t VALUES (0, ARRAY[0]); SAVEPOINT s1")
        .await?;
    for attempt in ["first build", "retry"] {
        match client
            .batch_execute("CREATE INDEX ubfull_gin ON ubfull_t USING gin (a)")
            .await
        {
            Err(e) if e.code() == Some(&SqlState::CONFIGURATION_LIMIT_EXCEEDED) => {}
            Err(e) => bail!("{attempt} in the savepoint: {}", db_message(&e)),
            Ok(()) => bail!("{attempt} in the savepoint succeeded with the list full"),
        }
        client.batch_execute("ROLLBACK TO SAVEPOINT s1").await?;
    }
    client
        .batch_execute("COMMIT")
        .await
        .map_err(|e| anyhow!("COMMIT: {}", db_message(&e)))?;
    let kept = db::query_i64(&ep, "SELECT count(*) FROM ubfull_t WHERE id = 0").await?;
    ensure!(
        kept == 1,
        "COMMIT kept {kept} rows inserted before the savepoint, want 1"
    );
    Ok(())
}

fn db_message(e: &tokio_postgres::Error) -> String {
    e.as_db_error()
        .map_or_else(|| e.to_string(), |d| d.message().to_owned())
}
