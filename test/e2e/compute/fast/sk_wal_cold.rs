//! Safekeeper WAL read path, HOT vs COLD across a restart.
//!
//! Companion to `fast/sk_wal` (hot only). The safekeeper serves WAL from an
//! in-memory `WalCache` (hot) falling through to durable rows in YB's
//! `safekeeper.wals` (cold). This reads a fixed range `[start, target)`
//! over replication twice — once hot (right after writing), then once after
//! **restarting the safekeeper** (which drops the in-memory cache, so the
//! same range is re-read from YB) — and asserts both catch up to the target
//! and deliver WAL. If the cold/YB read path can't reconstruct the range
//! (the bug behind #139's "invalid xlog page header" decode failures), the
//! cold read stalls or errors and this fails.
//!
//! In `slow/` because it restarts the safekeeper on the shared cluster. The
//! restart makes the pageserver walreceiver reconnect and replay (#57),
//! which bumps the benign `durable_walk_unflushed_row` correctness counter;
//! the suite's final verdict reports that kind without failing (see
//! `Cluster::final_verdict`).

use std::str::FromStr;
use std::time::Duration;

use anyhow::{Context, ensure};
use utils::lsn::Lsn;

use crate::cluster::{Cluster, step};
use crate::{db, sk_api};

pub async fn run(c: &Cluster) -> anyhow::Result<()> {
    let main = db::compute_conn(c.compute_port());
    let branch = c.branch_id().await?;

    step("generate WAL on the compute; capture the [start, target) flush range");
    let start = Lsn::from_str(
        &db::query_text(&main, "SELECT pg_current_wal_flush_lsn()::text").await?,
    )
    .context("parse start lsn")?;
    db::batch(
        &main,
        "DROP TABLE IF EXISTS sk_wal_cold_t; \
         CREATE TABLE sk_wal_cold_t (id int, pad text); \
         INSERT INTO sk_wal_cold_t SELECT g, repeat('x', 200) FROM generate_series(1, 10000) g;",
    )
    .await?;
    let _ = db::batch(&main, "CHECKPOINT").await;
    let target = Lsn::from_str(
        &db::query_text(&main, "SELECT pg_current_wal_flush_lsn()::text").await?,
    )
    .context("parse target lsn")?;
    ensure!(target > start, "no WAL generated ({start}..{target}); raise the INSERT size");

    step(&format!("hot read of WAL [{start}, {target}) from the safekeeper cache"));
    let hot = sk_api::read_committed_wal(c, &branch, start, target)
        .await
        .context("hot WAL read")?;
    ensure!(hot > 0, "hot WAL read reached the target but delivered no records");

    step("restart the safekeeper (drops its in-memory WAL cache)");
    c.yb(&["safekeeper", "restart"])?;
    // Let it come back up and re-establish streaming before reading.
    tokio::time::sleep(Duration::from_secs(5)).await;

    step(&format!("cold read of the SAME range [{start}, {target}) — served from YB"));
    let cold = sk_api::read_committed_wal(c, &branch, start, target)
        .await
        .context("cold WAL read (after SK restart, from YB)")?;
    ensure!(cold > 0, "cold WAL read reached the target but delivered no records");

    crate::tee_println!(
        "  safekeeper hot ({hot} B) and cold ({cold} B) both streamed WAL to {target}"
    );
    let _ = db::batch(&main, "DROP TABLE IF EXISTS sk_wal_cold_t").await;
    Ok(())
}
