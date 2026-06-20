//! Direct GetPage@LSN on a BRANCH timeline — the lineage walk.
//!
//! A branch shares its ancestor's pages up to the branch point and copies
//! nothing at branch time. Reading the branch's timeline directly via gRPC
//! must reconstruct the ancestor's page AS OF the branch point — and must
//! NOT see writes the parent makes afterwards. This exercises
//! `read_chain_lineage` capping ancestor deltas at the branch point and the
//! `cache_anchor_covers_lineage` fast-path guard, neither of which the
//! single-timeline `direct_getpage` touches.

use std::str::FromStr;

use anyhow::{Context, ensure};
use utils::lsn::Lsn;

use crate::cluster::{Cluster, step};
use crate::{db, page_api};

/// Rows on main BEFORE branching (the ancestor state the branch inherits).
const N_BEFORE: u32 = 40;
/// Rows added to main AFTER branching (must NOT appear on the branch).
const M_AFTER: u32 = 30;

pub async fn run(c: &Cluster) -> anyhow::Result<()> {
    let main = db::compute_conn(c.compute_port());
    let tid = c.tid();
    // Distinct from branch_ancestor's +158/+168 compute lanes.
    let br_port = c.compute_port() + 178;

    step("seed ancestor rows on main (single block), capture branch-point LSN");
    db::batch(
        &main,
        "DROP TABLE IF EXISTS dgp_br; \
         CREATE TABLE dgp_br (id int) WITH (fillfactor = 100, autovacuum_enabled = off);",
    )
    .await?;
    db::batch(
        &main,
        &format!("INSERT INTO dgp_br SELECT g FROM generate_series(1, {N_BEFORE}) g"),
    )
    .await?;
    let dbnode = db::query_i64(
        &main,
        "SELECT oid::int8 FROM pg_database WHERE datname = current_database()",
    )
    .await?;
    let relnode = db::query_i64(&main, "SELECT pg_relation_filenode('dgp_br')::int8").await?;
    let relbytes = db::query_i64(&main, "SELECT pg_relation_size('dgp_br')::int8").await?;
    ensure!(relbytes == 8192, "ancestor table spans {relbytes} bytes (>1 page)");
    let branch_lsn = Lsn::from_str(
        &db::query_text(&main, "SELECT pg_current_wal_insert_lsn()::text").await?,
    )
    .context("parse branch lsn")?;

    step("branch dgp_br off main + provision its endpoint (attaches the branch timeline)");
    c.yb(&["timeline", "branch", "--tenant-id", tid, "--branch-name", "dgp_br"])?;
    c.yb(&[
        "endpoint", "create", "ep_dgp_br", "--tenant-id", tid, "--branch-name", "dgp_br",
        "--pg-version", "17", "--pg-port", &br_port.to_string(),
    ])?;
    c.yb(&["endpoint", "start", "ep_dgp_br"])?;

    let result = run_branch_checks(c, &main, br_port, dbnode as u32, relnode as u32, branch_lsn).await;

    step("cleanup: deprovision the branch endpoint");
    c.yb_quiet(&["endpoint", "stop", "ep_dgp_br", "--destroy"]);
    let _ = db::batch(&main, "DROP TABLE IF EXISTS dgp_br").await;
    result
}

async fn run_branch_checks(
    c: &Cluster,
    main: &str,
    br_port: u16,
    dbnode: u32,
    relnode: u32,
    branch_lsn: Lsn,
) -> anyhow::Result<()> {
    // The branch compute knows its own timeline id (it sends it to the PS).
    let branch = db::compute_conn(br_port);
    let branch_tl = db::query_text(&branch, "SHOW yb_amp.timeline_id")
        .await
        .context("read branch timeline id from the branch endpoint")?;
    let branch_tl = branch_tl.trim().to_string();
    ensure!(
        branch_tl.len() == 32,
        "unexpected branch timeline id {branch_tl:?}"
    );
    // This cluster runs one pageserver per endpoint, so the branch timeline
    // is served by the BRANCH endpoint's own PS — address it via the
    // compute's pageserver connstring, not the main endpoint's gRPC port.
    let branch_connstr = db::query_text(&branch, "SHOW yb_amp.pageserver_connstring")
        .await
        .context("read branch pageserver connstring")?;

    // Diverge the parent ABOVE the branch point — these must not leak onto
    // the branch.
    db::batch(
        main,
        &format!("INSERT INTO dgp_br SELECT g FROM generate_series(1000, {}) g", 1000 + M_AFTER - 1),
    )
    .await?;

    let rel = page_api::heap_rel(dbnode, relnode);

    step("GetPage@branch-point on the BRANCH timeline sees only the ancestor's rows");
    let branch_client = page_api::client_at_connstring(c, &branch_tl, &branch_connstr)?;
    // Provisioning a branch endpoint can re-attach the tenant; a fresh gRPC
    // client may briefly see "not found" until the attach settles. Retry.
    let mut bpage = None;
    let mut last_err = None;
    for attempt in 0..15 {
        match page_api::get_page(&branch_client, rel, 0, branch_lsn, branch_lsn, attempt + 1).await {
            Ok(p) => {
                bpage = Some(p);
                break;
            }
            Err(e) => {
                last_err = Some(e);
                tokio::time::sleep(std::time::Duration::from_millis(700)).await;
            }
        }
    }
    let bpage = bpage.ok_or_else(|| {
        anyhow::anyhow!(
            "branch GetPage@{branch_lsn} never succeeded: {}",
            last_err.map(|e| e.to_string()).unwrap_or_default()
        )
    })?;
    let bgot = page_api::line_pointers(&bpage);
    ensure!(
        bgot == N_BEFORE as usize,
        "branch GetPage@{branch_lsn} reconstructed {bgot} line pointers, want {N_BEFORE} — \
         the branch read the parent's post-branch writes (lineage cap leaked) or lost ancestor rows"
    );
    let bpd = page_api::pd_lsn(&bpage);
    ensure!(
        bpd <= branch_lsn,
        "branch GetPage@{branch_lsn} returned a page newer than the branch point (pd_lsn={bpd})"
    );

    step("sanity: the MAIN timeline diverged — it sees the parent's later writes");
    let main_client = page_api::client(c).await?;
    let head = Lsn::from_str(
        &db::query_text(main, "SELECT pg_current_wal_insert_lsn()::text").await?,
    )
    .context("parse head lsn")?;
    let mpage = page_api::get_page(&main_client, rel, 0, head, head, 2).await?;
    let mgot = page_api::line_pointers(&mpage);
    ensure!(
        mgot == (N_BEFORE + M_AFTER) as usize,
        "main GetPage@{head} reconstructed {mgot} line pointers, want {} (the branch must not \
         have affected the parent)",
        N_BEFORE + M_AFTER
    );

    crate::tee_println!(
        "  branch sees {N_BEFORE} ancestor rows; main diverged to {} — lineage isolation holds",
        N_BEFORE + M_AFTER
    );
    Ok(())
}
