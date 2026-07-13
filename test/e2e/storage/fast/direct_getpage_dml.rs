//! Direct GetPage@LSN reconstruction under mixed DML (UPDATE/DELETE).
//!
//! `direct_getpage` is INSERT-only, so its chains never rewrite tuple
//! headers, set xmax, or grow the line-pointer array via updates. This
//! scenario drives the pageserver's reconstruct path over a chain that
//! includes HOT updates and deletes, under the same tiny cache/chain
//! limits that force materialise + evict + reconstruct.
//!
//! Ground truth comes from the compute itself: after each statement we
//! capture `(lsn, line_pointer_count)` for heap block 0 via pageinspect
//! `get_raw_page`/`page_header`. The pageserver, asked for GetPage@lsn,
//! must reconstruct a block-0 page with the SAME line-pointer count
//! (`pd_lower` is advanced by WAL-logged tuple/line-pointer allocation, so
//! a faithful walredo matches it) and a `pd_lsn` no newer than `lsn`. We
//! compare the structural line-pointer count rather than the raw bytes so
//! the check is robust to hint bits the compute may set lazily without WAL.

use std::str::FromStr;
use std::time::Duration;

use anyhow::{Context, ensure};
use utils::lsn::Lsn;

use crate::cluster::{Cluster, step};
use crate::{db, page_api};

pub async fn run(c: &Cluster) -> anyhow::Result<()> {
    let original = page_api::stress_ps(c)?;
    let result = run_inner(c).await;
    result.and(page_api::restore_ps(c, &original))
}

async fn run_inner(c: &Cluster) -> anyhow::Result<()> {
    let main = db::compute_conn(c.compute_port());

    step("seed a table + pageinspect, then run a DML sequence capturing (lsn, lp count)");
    db::batch(
        &main,
        "DROP TABLE IF EXISTS dgp_dml; \
         CREATE TABLE dgp_dml (id int, pad text) \
           WITH (fillfactor = 70, autovacuum_enabled = off); \
         CREATE EXTENSION IF NOT EXISTS pageinspect;",
    )
    .await?;

    let dbnode = db::query_i64(
        &main,
        "SELECT oid::int8 FROM pg_database WHERE datname = current_database()",
    )
    .await?;
    let relnode = db::query_i64(&main, "SELECT pg_relation_filenode('dgp_dml')::int8").await?;

    // A DML sequence on a few rows that stays anchored on heap block 0:
    // an initial insert, repeated whole-table HOT updates (non-indexed
    // column, fillfactor leaves room so new versions land on the same
    // page), a delete, and more updates — enough versions to blow past
    // image_chain_limit=2 and trigger the materialiser repeatedly.
    let stmts = [
        "INSERT INTO dgp_dml SELECT g, 'p' FROM generate_series(1, 8) g",
        "UPDATE dgp_dml SET pad = 'qq'",
        "UPDATE dgp_dml SET pad = 'rrr'",
        "DELETE FROM dgp_dml WHERE id % 2 = 0",
        "UPDATE dgp_dml SET pad = 'ssss'",
        "UPDATE dgp_dml SET pad = 'ttttt'",
    ];

    // (lsn_after_stmt, block-0 line-pointer count from the compute) per step.
    let mut history: Vec<(Lsn, usize)> = Vec::with_capacity(stmts.len());
    for s in stmts {
        db::batch(&main, s).await?;
        let lsn_s = db::query_text(&main, "SELECT pg_current_wal_insert_lsn()::text").await?;
        // pd_lower from page_header → line pointers = (lower - 24) / 4.
        let lp = db::query_i64(
            &main,
            "SELECT ((lower - 24) / 4)::int8 FROM page_header(get_raw_page('dgp_dml', 0))",
        )
        .await?;
        history.push((Lsn::from_str(&lsn_s).context("parse lsn")?, lp as usize));
    }

    // Let the materialiser fold the (now long) chain and the tiny cache
    // evict, so the historical reads below reconstruct from YB + images.
    tokio::time::sleep(Duration::from_secs(3)).await;

    step("GetPage@LSN block 0 for each historical LSN; match the compute's line-pointer count");
    let client = page_api::client(c).await?;
    let rel = page_api::heap_rel(dbnode as u32, relnode as u32);

    for (i, (lsn, expected_lp)) in history.iter().enumerate() {
        let page = page_api::get_page(&client, rel, 0, *lsn, *lsn, i as u64 + 1).await?;
        let got = page_api::line_pointers(&page);
        ensure!(
            got == *expected_lp,
            "GetPage@{lsn} (DML step {i}) reconstructed {got} line pointers, want {expected_lp} \
             (the compute's own page at that LSN) — DML reconstruction mismatch"
        );
        let pd = page_api::pd_lsn(&page);
        ensure!(
            pd <= *lsn,
            "GetPage@{lsn} (DML step {i}) returned a page from the future (pd_lsn={pd})"
        );
    }

    crate::tee_println!("  validated {} historical DML GetPage@LSN reads", history.len());
    db::batch(&main, "DROP TABLE IF EXISTS dgp_dml").await?;
    Ok(())
}
