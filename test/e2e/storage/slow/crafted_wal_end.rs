//! (slow) Ported from Neon's test_crafted_wal_end. Use the `wal_craft` tool to
//! push the WAL *end* into specific tricky-but-valid shapes (an XLOG_SWITCH
//! record, an XLOG_SWITCH ending exactly on a page boundary), then restart the
//! safekeeper + pageserver and verify the branch is still readable and writable
//! with correct data — exercising the interpreted-WAL reader's boundary
//! handling on pageserver catch-up (plus the corruption-marker + correctness
//! gates).
//!
//! Only the LSN-relative shapes are ported. wal_craft's segment-crossing shapes
//! (`*_crossing_segment*`) assert the WAL insert LSN is below ~32 MB, i.e. they
//! need a freshly-initdb'd compute; the shared `main` endpoint here is well past
//! that, so those two are out of scope for this harness.

use std::time::Duration;

use anyhow::bail;

use crate::cluster::{Cluster, step};
use crate::db;
use crate::tee_println;

const WAL_TYPES: &[&str] = &[
    "simple",
    "last_wal_record_xlog_switch",
    "last_wal_record_xlog_switch_ends_on_page_boundary",
];

/// Run a wal_craft Crafter against `conn` (sync `postgres` client, off the async
/// runtime). The crafters create a helper table `t`; the caller drops it.
fn craft(conn: &str, wal_type: &str) -> anyhow::Result<()> {
    use wal_craft::Crafter;
    let mut client = postgres::Client::connect(conn, postgres::NoTls)?;
    match wal_type {
        "simple" => wal_craft::Simple::craft(&mut client)?,
        "last_wal_record_xlog_switch" => wal_craft::LastWalRecordXlogSwitch::craft(&mut client)?,
        "last_wal_record_xlog_switch_ends_on_page_boundary" => {
            wal_craft::LastWalRecordXlogSwitchEndsOnPageBoundary::craft(&mut client)?
        }
        other => bail!("unsupported wal_craft type {other}"),
    };
    Ok(())
}

/// Read `sum(key)` from the table, retrying briefly while the pageserver warms
/// up after a restart.
async fn sum_with_retry(conn: &str) -> anyhow::Result<i64> {
    let mut last_err = None;
    for _ in 0..30 {
        match db::query_i64(conn, "SELECT coalesce(sum(key), 0) FROM cwe_keys").await {
            Ok(v) => return Ok(v),
            Err(e) => {
                last_err = Some(e);
                tokio::time::sleep(Duration::from_secs(1)).await;
            }
        }
    }
    Err(last_err.unwrap())
}

pub async fn run(c: &Cluster) -> anyhow::Result<()> {
    let main = db::compute_conn(c.compute_port());

    step("Apply wal_craft's required postgres config (SIGHUP) + reload");
    // shared_preload_libraries=yb_amp is already set on the yb-light compute.
    db::batch(&main, "ALTER SYSTEM SET wal_keep_size = '50MB'").await?;
    db::batch(&main, "ALTER SYSTEM SET wal_writer_delay = '10s'").await?;
    db::batch(&main, "ALTER SYSTEM SET autovacuum = off").await?;
    db::batch(&main, "SELECT pg_reload_conf()").await?;

    step("Seed the table");
    db::batch(&main, "DROP TABLE IF EXISTS cwe_keys").await?;
    db::batch(&main, "CREATE TABLE cwe_keys(key int primary key)").await?;
    db::batch(&main, "INSERT INTO cwe_keys SELECT generate_series(1, 100)").await?;
    let mut expected: i64 = (1..=100).sum();
    let mut next_lo: i64 = 101;

    for wal_type in WAL_TYPES {
        step(&format!("craft WAL end shape: {wal_type}"));
        // The crafters create a helper table `t`; ensure it's clear first.
        db::batch(&main, "DROP TABLE IF EXISTS t").await?;
        let conn = main.clone();
        let wt = wal_type.to_string();
        tokio::task::spawn_blocking(move || craft(&conn, &wt)).await??;

        step("Restart safekeeper + pageserver (graceful)");
        c.yb(&["pageserver", "stop"])?;
        c.yb(&["safekeeper", "stop"])?;
        c.yb(&["safekeeper", "start"])?;
        c.yb(&["pageserver", "start"])?;

        step("Read back after restart — data intact");
        let got = sum_with_retry(&main).await?;
        if got != expected {
            bail!("after crafting {wal_type}: read sum={got}, want {expected}");
        }

        step("Write more after restart, then read back — ingest path healthy");
        let hi = next_lo + 99;
        db::batch(
            &main,
            &format!("INSERT INTO cwe_keys SELECT generate_series({next_lo}, {hi})"),
        )
        .await?;
        expected += (next_lo..=hi).sum::<i64>();
        next_lo = hi + 1;
        let got = db::query_i64(&main, "SELECT coalesce(sum(key), 0) FROM cwe_keys").await?;
        if got != expected {
            bail!("after post-restart insert ({wal_type}): sum={got}, want {expected}");
        }
        tee_println!("  ok — {wal_type}: read+write correct across restart");
    }

    step("Cleanup");
    db::batch(&main, "DROP TABLE IF EXISTS t").await?;
    db::batch(&main, "DROP TABLE IF EXISTS cwe_keys").await?;
    // wal_craft does `CREATE EXTENSION IF NOT EXISTS yb_amp_test_utils`
    // (for yb_amp_xlogflush). Drop it so a later golden on the shared compute
    // doesn't see a spurious "extension already exists" NOTICE (e.g. oid-overflow).
    db::batch(&main, "DROP EXTENSION IF EXISTS yb_amp_test_utils").await?;
    db::batch(&main, "ALTER SYSTEM RESET wal_keep_size").await?;
    db::batch(&main, "ALTER SYSTEM RESET wal_writer_delay").await?;
    db::batch(&main, "ALTER SYSTEM RESET autovacuum").await?;
    db::batch(&main, "SELECT pg_reload_conf()").await?;

    Ok(())
}
