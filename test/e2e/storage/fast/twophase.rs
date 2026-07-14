//! 2PC (two-phase commit) round-trip through the pageserver (#137).
//!
//! The shared `main` endpoint runs with `max_prepared_transactions = 0`,
//! so the golden-`.sql` harness can't cover 2PC, as a scenario instead:
//! provision a dedicated branch + endpoint with
//! `--pg-setting max_prepared_transactions=5`, then
//!
//!   1. PREPARE three transactions (one to commit, one to roll back,
//!      one to survive a restart), CHECKPOINT.
//!   2. `clear_buffer_cache()` and resolve the first two — the prepared
//!      state and heap pages must reconstruct from the pageserver, not
//!      shared_buffers.
//!   3. Restart the endpoint: the fresh basebackup must rebuild
//!      `pg_twophase` from the pageserver (`list_twophase_files` /
//!      `add_twophase_file`), with the surviving prepared transaction
//!      intact and its rows still invisible.
//!   4. COMMIT PREPARED the survivor and verify final visibility, plus
//!      one post-recovery write.
//!
//! Ported from Neon's `test_twophase`.

use anyhow::bail;

use crate::cluster::{Cluster, step};
use crate::db;
use crate::tee_println;

pub async fn run(c: &Cluster) -> anyhow::Result<()> {
    // Compute lane above `main`; keep clear of multi_timeline (+138/+148).
    let port = c.compute_port() + 158;

    step("provision a 2PC-enabled branch endpoint (max_prepared_transactions=5)");
    let branch = c.branch_create()?;
    if branch.is_empty() {
        bail!("branch create failed");
    }
    c.yb(&[
        "endpoint", "create", "ep_2pc", "--branch-id", &branch, "--branch-name", "main",
        "--pg-version", "18", "--pg-port", &port.to_string(),
        "--pg-setting", "max_prepared_transactions=5",
    ])?;
    c.yb(&["endpoint", "start", "ep_2pc"])?;

    let result = run_inner(c, port).await;

    // Deprovision on every exit path (the endpoint owns a PS+SK pair).
    c.yb_quiet(&["endpoint", "stop", "ep_2pc", "--destroy"]);
    result
}

async fn run_inner(c: &Cluster, port: u16) -> anyhow::Result<()> {
    let conn = db::compute_conn(port);

    let mpt = db::query_text(&conn, "SHOW max_prepared_transactions").await?;
    if mpt.trim() != "5" {
        bail!("--pg-setting max_prepared_transactions did not take effect (got {mpt:?})");
    }

    step("prepare three transactions, checkpoint");
    // One batch per transaction: `batch` sends a single simple-protocol
    // Query, and PostgreSQL wraps a multi-statement message in one
    // implicit transaction — a PREPARE TRANSACTION inside would prepare
    // (and lock away) everything before it, including the CREATE TABLE.
    db::batch(
        &conn,
        "CREATE EXTENSION IF NOT EXISTS yb_amp_test_utils;
         DROP TABLE IF EXISTS t2pc;
         CREATE TABLE t2pc (id INT PRIMARY KEY, phase TEXT NOT NULL);",
    )
    .await?;
    db::batch(
        &conn,
        "BEGIN;
         INSERT INTO t2pc SELECT g, 'commit_now' FROM generate_series(1, 10) g;
         PREPARE TRANSACTION 'tp_commit_now';",
    )
    .await?;
    db::batch(
        &conn,
        "BEGIN;
         INSERT INTO t2pc SELECT g, 'rollback_now' FROM generate_series(11, 20) g;
         PREPARE TRANSACTION 'tp_rollback_now';",
    )
    .await?;
    db::batch(
        &conn,
        "BEGIN;
         INSERT INTO t2pc SELECT g, 'survive_restart' FROM generate_series(21, 30) g;
         PREPARE TRANSACTION 'tp_survivor';",
    )
    .await?;
    db::batch(&conn, "CHECKPOINT;").await?;
    let prepared = db::query_i64(&conn, "SELECT count(*) FROM pg_prepared_xacts").await?;
    if prepared != 3 {
        bail!("expected 3 prepared transactions, found {prepared}");
    }

    step("resolve two of them with a cold cache (state reconstructs from the pageserver)");
    db::batch(&conn, "SELECT clear_buffer_cache();").await?;
    db::batch(&conn, "COMMIT PREPARED 'tp_commit_now';").await?;
    db::batch(&conn, "ROLLBACK PREPARED 'tp_rollback_now';").await?;
    let visible = db::query_i64(&conn, "SELECT count(*) FROM t2pc").await?;
    if visible != 10 {
        bail!("after commit+rollback expected 10 visible rows, found {visible}");
    }

    step("restart the endpoint — basebackup must rebuild pg_twophase from the pageserver");
    c.yb(&["endpoint", "stop", "ep_2pc"])?;
    c.yb(&["endpoint", "start", "ep_2pc"])?;

    let survivor =
        db::query_text(&conn, "SELECT gid FROM pg_prepared_xacts").await?;
    if survivor.trim() != "tp_survivor" {
        bail!("prepared transaction did not survive the restart (pg_prepared_xacts: {survivor:?})");
    }
    let visible = db::query_i64(&conn, "SELECT count(*) FROM t2pc").await?;
    if visible != 10 {
        bail!("survivor's rows leaked before COMMIT PREPARED: {visible} visible (want 10)");
    }

    step("commit the survivor, verify final visibility");
    db::batch(&conn, "COMMIT PREPARED 'tp_survivor';").await?;
    let total = db::query_i64(&conn, "SELECT count(*) FROM t2pc").await?;
    let rolled_back =
        db::query_i64(&conn, "SELECT count(*) FROM t2pc WHERE id BETWEEN 11 AND 20").await?;
    let survived =
        db::query_i64(&conn, "SELECT count(*) FROM t2pc WHERE phase = 'survive_restart'").await?;
    if total != 20 || rolled_back != 0 || survived != 10 {
        bail!(
            "final visibility wrong: total={total} (want 20), rolled_back={rolled_back} \
             (want 0), survived={survived} (want 10)"
        );
    }

    step("post-recovery write still lands");
    db::batch(&conn, "INSERT INTO t2pc VALUES (31, 'after')").await?;
    let total = db::query_i64(&conn, "SELECT count(*) FROM t2pc").await?;
    if total != 21 {
        bail!("post-recovery insert missing: total={total} (want 21)");
    }

    tee_println!("  2pc: prepare/commit/rollback + restart survival verified");
    Ok(())
}
