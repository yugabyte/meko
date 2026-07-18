//! Safekeeper WAL read path: stream a committed WAL range directly from the
//! safekeeper over the libpq replication protocol.
//!
//! This is the safekeeper analog of `direct_getpage` — it bypasses the
//! pageserver and reads WAL straight from the SK. yb-amp's safekeeper
//! serves only the **Interpreted** WAL protocol (decoded records, protobuf
//! framed — see `compute/safekeeper/src/handler.rs`), the same `InterpretedWalReader`
//! path whose decode failures surface as "invalid xlog page header" panics.
//! Reading a known range end-to-end exercises that reader and confirms the
//! SK streams committed WAL to a fresh client, catching up to the flush LSN
//! and delivering records.
//!
//! Hot vs cold: a cold (post-restart, served-from-YB) variant is a natural
//! extension, but restarting the safekeeper triggers the pageserver's
//! benign reconnect-replay (#57), which bumps the
//! `durable_walk_unflushed_row` correctness counter that the suite's
//! post-scenario gate treats as fatal. That variant therefore needs the
//! gate to whitelist the benign kind (or a non-restart cache-eviction
//! path); this scenario covers the read path without a restart.

use std::str::FromStr;

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
        "DROP TABLE IF EXISTS sk_wal_t; \
         CREATE TABLE sk_wal_t (id int, pad text); \
         INSERT INTO sk_wal_t SELECT g, repeat('x', 200) FROM generate_series(1, 10000) g;",
    )
    .await?;
    let _ = db::batch(&main, "CHECKPOINT").await;
    let target = Lsn::from_str(
        &db::query_text(&main, "SELECT pg_current_wal_flush_lsn()::text").await?,
    )
    .context("parse target lsn")?;
    ensure!(target > start, "no WAL generated ({start}..{target}); raise the INSERT size");

    step(&format!("read WAL [{start}, {target}) directly from the safekeeper"));
    let bytes = sk_api::read_committed_wal(c, &branch, start, target)
        .await
        .context("safekeeper WAL read")?;
    ensure!(
        bytes > 0,
        "safekeeper reached {target} but delivered no interpreted WAL records"
    );

    crate::tee_println!("  safekeeper streamed WAL to {target} ({bytes} B of interpreted records)");
    let _ = db::batch(&main, "DROP TABLE IF EXISTS sk_wal_t").await;
    Ok(())
}
