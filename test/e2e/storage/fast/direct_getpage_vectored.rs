//! Direct GetPage@LSN over MULTIPLE blocks in one request — the vectored
//! read path (`lookup_range` / multi-block `GetPageRequest`).
//!
//! `direct_getpage` only ever asks for block 0. This seeds a relation that
//! spans several heap blocks and asks for all of them in a single
//! `GetPageRequest`, validating every returned block against the compute's
//! own per-block line-pointer count (pageinspect ground truth) and a
//! `pd_lsn` no newer than the read LSN. Run under the tiny cache/chain
//! limits so the per-block chains are materialised and evicted.

use std::str::FromStr;
use std::time::Duration;

use anyhow::{Context, ensure};
use utils::lsn::Lsn;

use crate::cluster::{Cluster, step};
use crate::{db, page_api};

/// Enough rows (fillfactor 100, 4-byte ints) to span several heap blocks.
const N_ROWS: u32 = 2000;

pub async fn run(c: &Cluster) -> anyhow::Result<()> {
    let original = page_api::stress_ps(c)?;
    let result = run_inner(c).await;
    page_api::restore_ps(c, &original);
    result
}

async fn run_inner(c: &Cluster) -> anyhow::Result<()> {
    let main = db::compute_conn(c.compute_port());

    step("seed a multi-block table + pageinspect");
    db::batch(
        &main,
        "DROP TABLE IF EXISTS dgp_vec; \
         CREATE TABLE dgp_vec (id int) WITH (fillfactor = 100, autovacuum_enabled = off); \
         CREATE EXTENSION IF NOT EXISTS pageinspect;",
    )
    .await?;
    db::batch(
        &main,
        &format!("INSERT INTO dgp_vec SELECT g FROM generate_series(1, {N_ROWS}) g"),
    )
    .await?;

    let dbnode = db::query_i64(
        &main,
        "SELECT oid::int8 FROM pg_database WHERE datname = current_database()",
    )
    .await?;
    let relnode = db::query_i64(&main, "SELECT pg_relation_filenode('dgp_vec')::int8").await?;
    let nblocks = db::query_i64(&main, "SELECT (pg_relation_size('dgp_vec') / 8192)::int8").await? as u32;
    ensure!(
        nblocks >= 2,
        "table only spans {nblocks} block(s); raise N_ROWS for a vectored read"
    );
    let lsn = Lsn::from_str(
        &db::query_text(&main, "SELECT pg_current_wal_insert_lsn()::text").await?,
    )
    .context("parse lsn")?;

    // Per-block ground-truth line-pointer count from the compute.
    let mut expected = Vec::with_capacity(nblocks as usize);
    for blk in 0..nblocks {
        let lp = db::query_i64(
            &main,
            &format!(
                "SELECT ((lower - 24) / 4)::int8 FROM page_header(get_raw_page('dgp_vec', {blk}))"
            ),
        )
        .await?;
        expected.push(lp as usize);
    }

    tokio::time::sleep(Duration::from_secs(3)).await;

    step(&format!(
        "GetPage@LSN for all {nblocks} blocks in one request; validate each"
    ));
    let client = page_api::client(c).await?;
    let rel = page_api::heap_rel(dbnode as u32, relnode as u32);
    let blocks: Vec<u32> = (0..nblocks).collect();
    let pages = page_api::get_pages(&client, rel, &blocks, lsn, lsn, 1).await?;
    ensure!(
        pages.len() == nblocks as usize,
        "vectored GetPage returned {} pages, want {nblocks}",
        pages.len()
    );

    for (blk, page) in pages.iter().enumerate() {
        let got = page_api::line_pointers(page);
        ensure!(
            got == expected[blk],
            "vectored GetPage@{lsn} block {blk} reconstructed {got} line pointers, want {} \
             (compute ground truth)",
            expected[blk]
        );
        let pd = page_api::pd_lsn(page);
        ensure!(
            pd <= lsn,
            "vectored GetPage@{lsn} block {blk} returned a page from the future (pd_lsn={pd})"
        );
    }

    crate::tee_println!("  validated a vectored GetPage@LSN over {nblocks} blocks");
    db::batch(&main, "DROP TABLE IF EXISTS dgp_vec").await?;
    Ok(())
}
