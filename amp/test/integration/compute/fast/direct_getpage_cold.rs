//! Direct GetPage@LSN reconstruction from a COLD cache, across a
//! pageserver restart.
//!
//! `direct_getpage` exercises fold/evict/reconstruct while the process
//! stays up. This complements it: after writing the history we **restart**
//! the pageserver (dropping the entire in-RAM `UnifiedPages` cache) and
//! only then read every historical LSN. Under the tiny read-cache the
//! re-ingest evicts as it goes, so the historical reads reconstruct purely
//! from durable YB rows — the no-cache half of the blend, and a check that
//! reconstruction survives losing all RAM state.

use std::str::FromStr;
use std::time::Duration;

use anyhow::{Context, ensure};
use utils::lsn::Lsn;

use crate::cluster::{Cluster, step};
use crate::{db, page_api};

/// One 4-byte int per single-row txn, fillfactor 100 → all on heap block 0.
const N_ROWS: u32 = 60;

pub async fn run(c: &Cluster) -> anyhow::Result<()> {
    let original = page_api::stress_ps(c)?;
    let result = run_inner(c).await;
    result.and(page_api::restore_ps(c, &original))
}

async fn run_inner(c: &Cluster) -> anyhow::Result<()> {
    let main = db::compute_conn(c.compute_port());

    step("seed a single-block table, one row per txn, capturing (rows, lsn)");
    db::batch(
        &main,
        "DROP TABLE IF EXISTS dgp_cold; \
         CREATE TABLE dgp_cold (id int) WITH (fillfactor = 100, autovacuum_enabled = off);",
    )
    .await?;

    let dbnode = db::query_i64(
        &main,
        "SELECT oid::int8 FROM pg_database WHERE datname = current_database()",
    )
    .await?;
    let relnode = db::query_i64(&main, "SELECT pg_relation_filenode('dgp_cold')::int8").await?;

    let mut history: Vec<(u32, Lsn)> = Vec::with_capacity(N_ROWS as usize);
    for i in 1..=N_ROWS {
        db::batch(&main, &format!("INSERT INTO dgp_cold VALUES ({i})")).await?;
        let lsn_s = db::query_text(&main, "SELECT pg_current_wal_insert_lsn()::text").await?;
        history.push((i, Lsn::from_str(&lsn_s).context("parse insert lsn")?));
    }
    let relbytes = db::query_i64(&main, "SELECT pg_relation_size('dgp_cold')::int8").await?;
    ensure!(
        relbytes == 8192,
        "table spilled to {relbytes} bytes (>1 page); lower N_ROWS"
    );

    // Drop the entire cache: restart the PS. The walreceiver re-ingests
    // from the durable consistent_lsn, but under the tiny read-cache the
    // older versions can't all stay resident, so the historical reads
    // below reconstruct from YB rather than a warm cache.
    step("restart PS to drop the in-RAM cache, then read every historical LSN cold");
    c.yb(&["pageserver", "restart"])?;
    tokio::time::sleep(Duration::from_secs(3)).await;

    let client = page_api::client(c).await?;
    let rel = page_api::heap_rel(dbnode as u32, relnode as u32);
    for (rows, lsn) in &history {
        let page = page_api::get_page(&client, rel, 0, *lsn, *lsn, *rows as u64).await?;
        let got = page_api::line_pointers(&page);
        ensure!(
            got == *rows as usize,
            "GetPage@{lsn} (after {rows} inserts) reconstructed {got} line pointers after a PS \
             restart, want {rows} — cold/post-restart reconstruction served a stale page"
        );
        let pd = page_api::pd_lsn(&page);
        ensure!(pd <= *lsn, "GetPage@{lsn} returned a page from the future (pd_lsn={pd})");
    }

    crate::tee_println!(
        "  validated {} historical GetPage@LSN reads after a PS restart (cold)",
        history.len()
    );
    db::batch(&main, "DROP TABLE IF EXISTS dgp_cold").await?;
    Ok(())
}
