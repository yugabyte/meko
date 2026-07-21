//! Direct GetPage@LSN pageserver correctness test.
//!
//! Unlike the rest of the suite, this scenario bypasses the Postgres
//! compute's read path entirely (prefetch ring, last-written-LSN cache,
//! shared buffers) and calls the pageserver's gRPC `GetPage@LSN` RPC
//! directly. That isolates the pageserver's own read/reconstruct path —
//! YB-row chain reconstruction, the `UnifiedPages` cache, the image
//! materialiser, and `wait_lsn` — so a bug there can't be masked by the
//! compute reading at a higher LSN.
//!
//! It runs the pageserver under deliberately tiny limits (a 2-delta image
//! trigger and a 64 KiB read cache) so a long single-page chain is
//! materialised and evicted many times over, forcing reconstruction from
//! YB + freshly-installed images rather than a warm cache.
//!
//! The check is byte-level and independent of any walredo the compute
//! would do: we INSERT rows one-per-transaction onto a single heap block,
//! recording (row_count, lsn) after each commit, then GetPage@LSN at each
//! historical LSN and decode the page header. After `n` inserts the block
//! must have exactly `n` line pointers and a page LSN no newer than the
//! requested LSN. A page that comes back with fewer line pointers is the
//! pageserver serving a version missing committed writes ≤ the requested
//! LSN — the exact failure class this suite has been chasing.

use std::str::FromStr;
use std::time::Duration;

use anyhow::{Context, ensure};
use pageserver_api::reltag::RelTag;
use pageserver_client_grpc::PageserverClient;
use pageserver_page_api as page_api;
use utils::id::BranchId;
use utils::lsn::Lsn;

use crate::cluster::{Cluster, step};
use crate::db;

/// Rows to write. Each is a 4-byte int in its own transaction; with
/// fillfactor 100 they all sit on heap block 0 (asserted via
/// `pg_relation_size` == one 8 KiB page below), so the whole history is a
/// single-page delta chain.
const N_ROWS: u32 = 120;

/// SizeOfPageHeaderData — the fixed Postgres heap/index page header. Line
/// pointers (`ItemIdData`, 4 bytes each) start right after it.
const PAGE_HEADER_SIZE: usize = 24;

pub async fn run(c: &Cluster) -> anyhow::Result<()> {
    // Swap in tiny limits and restart the PS, then always restore the
    // original config + restart so later scenarios run under defaults
    // (this suite shares one cluster). Restore happens on every exit path.
    let toml = c
        .repo_dir()
        .join("endpoints/main/pageserver/pageserver.toml");
    let original = std::fs::read_to_string(&toml).context("read pageserver.toml")?;
    // Merge into the existing [yb_page_writer] (the harness boots one with
    // enable_gc) — a duplicate table header fails the pageserver's toml parse.
    let stressed = crate::cluster::merge_toml_section(
        &original,
        "yb_page_writer",
        "image_chain_limit = 2\n\
         image_chain_limit_slru = 4\n\
         image_chain_payload_bytes_limit = 64\n\
         materialize_queue_capacity = 4\n\
         read_cache_bytes = 65536\n\
         backpressure_bytes = 1048576\n",
    );
    std::fs::write(&toml, &stressed).context("write stressed pageserver.toml")?;
    step("restart PS under tiny limits (image_chain_limit=2, read_cache_bytes=64KiB)");
    c.yb(&["pageserver", "restart"])?;

    let result = run_inner(c).await;

    // Restore: best-effort, but log if it fails — a stuck stressed config
    // would silently change every later scenario's behaviour.
    if let Err(e) = std::fs::write(&toml, &original) {
        crate::tee_println!("  (WARN: failed to restore pageserver.toml: {e:#})");
    }
    if let Err(e) = c.yb(&["pageserver", "restart"]) {
        crate::tee_println!("  (WARN: failed to restart PS after config restore: {e:#})");
    }

    result
}

async fn run_inner(c: &Cluster) -> anyhow::Result<()> {
    let main = db::compute_conn(c.compute_port());

    step("seed a single-block table, one row per txn, capturing (rows, lsn)");
    db::batch(
        &main,
        "DROP TABLE IF EXISTS dgp_t; \
         CREATE TABLE dgp_t (id int) WITH (fillfactor = 100, autovacuum_enabled = off);",
    )
    .await?;

    let dbnode = db::query_i64(
        &main,
        "SELECT oid::int8 FROM pg_database WHERE datname = current_database()",
    )
    .await?;
    let relnode = db::query_i64(&main, "SELECT pg_relation_filenode('dgp_t')::int8").await?;

    // (rows_after_commit, lsn_after_commit) for every insert.
    let mut history: Vec<(u32, Lsn)> = Vec::with_capacity(N_ROWS as usize);
    for i in 1..=N_ROWS {
        db::batch(&main, &format!("INSERT INTO dgp_t VALUES ({i})")).await?;
        let lsn_s = db::query_text(&main, "SELECT pg_current_wal_insert_lsn()::text").await?;
        history.push((i, Lsn::from_str(&lsn_s).context("parse insert lsn")?));
    }

    // All rows must be on heap block 0 (one 8 KiB page) so the per-LSN
    // line-pointer count equals the row count. If they spilled, the test's
    // invariant wouldn't hold — fail loudly rather than silently.
    let relbytes = db::query_i64(&main, "SELECT pg_relation_size('dgp_t')::int8").await?;
    ensure!(
        relbytes == 8192,
        "table spilled to {relbytes} bytes (>1 page); lower N_ROWS"
    );

    // Give the background materialiser time to fold the chain into images
    // (image_chain_limit=2 fires constantly) and the tiny read cache time
    // to evict, so the historical reads below reconstruct from YB.
    tokio::time::sleep(Duration::from_secs(3)).await;

    step("GetPage@LSN directly from the PS for every historical LSN; validate");
    let branch_id = BranchId::from_str(&c.branch_id().await?).context("parse branch id")?;
    let url = format!("grpc://127.0.0.1:{}", c.ps_grpc_port());
    let client = PageserverClient::new(branch_id, url, None, None)
        .context("connect PS gRPC")?;

    let rel = RelTag {
        forknum: 0,
        spcnode: 1663, // pg_default tablespace
        dbnode: dbnode as u32,
        relnode: relnode as u32,
    };

    for (rows, lsn) in &history {
        let req = page_api::GetPageRequest {
            request_id: page_api::RequestID::new(*rows as u64),
            request_class: page_api::GetPageClass::Normal,
            read_lsn: page_api::ReadLsn {
                request_lsn: *lsn,
                not_modified_since_lsn: Some(*lsn),
            },
            rel,
            block_numbers: vec![0],
        };
        let resp = client
            .get_page(req)
            .await
            .map_err(|e| anyhow::anyhow!("GetPage@{lsn} (after {rows} inserts): {e}"))?;

        ensure!(
            resp.pages.len() == 1,
            "GetPage@{lsn}: got {} pages, want 1",
            resp.pages.len()
        );
        let page = &resp.pages[0].image;
        ensure!(page.len() == 8192, "GetPage@{lsn}: page len {}", page.len());

        // Line-pointer count from pd_lower: the killer assertion. A short
        // count means the PS reconstructed a page missing committed inserts
        // ≤ this LSN.
        let pd_lower = u16::from_le_bytes([page[12], page[13]]) as usize;
        let line_pointers = pd_lower.saturating_sub(PAGE_HEADER_SIZE) / 4;
        ensure!(
            line_pointers == *rows as usize,
            "GetPage@{lsn} (after {rows} inserts) reconstructed {line_pointers} line pointers, \
             want {rows} — pageserver served a page missing committed writes ≤ the requested LSN"
        );

        // pd_lsn (page header, first 8 bytes: xlogid<<32 | xrecoff) must
        // not be newer than what we asked for.
        let xlogid = u32::from_le_bytes([page[0], page[1], page[2], page[3]]);
        let xrecoff = u32::from_le_bytes([page[4], page[5], page[6], page[7]]);
        let pd_lsn = Lsn(((xlogid as u64) << 32) | xrecoff as u64);
        ensure!(
            pd_lsn <= *lsn,
            "GetPage@{lsn} returned a page from the future (pd_lsn={pd_lsn})"
        );
    }

    crate::tee_println!("  validated {} historical GetPage@LSN reads", history.len());
    db::batch(&main, "DROP TABLE IF EXISTS dgp_t").await?;
    Ok(())
}
