//! Shared helpers for the `direct_getpage*` scenario family — tests that
//! call the pageserver's gRPC `GetPage@LSN` directly (bypassing the
//! compute's prefetch / last-written-LSN / shared-buffers machinery) to
//! isolate the pageserver's own read/reconstruct path.
//!
//! Centralises the gRPC client construction, `RelTag` building, the
//! GetPage call, Postgres page-header decoding, and the
//! tiny-limits stress-config swap so each scenario asserts rather than
//! re-boilerplates.

use std::str::FromStr;

use anyhow::{Context, ensure};
use pageserver_api::reltag::RelTag;
use pageserver_client_grpc::PageserverClient;
use pageserver_page_api as page_api;
use utils::id::BranchId;
use utils::lsn::Lsn;

use crate::cluster::{Cluster, step};

/// SizeOfPageHeaderData — the fixed Postgres heap/index page header. Line
/// pointers (`ItemIdData`, 4 bytes each) start right after it.
pub const PAGE_HEADER_SIZE: usize = 24;
/// Size of one `ItemIdData` line pointer.
pub const ITEM_ID_SIZE: usize = 4;
/// pg_default tablespace oid (`spcnode` for a normal relation).
pub const DEFAULT_TABLESPACE: u32 = 1663;

/// Build a gRPC `PageserverClient` to node 1 for the given branch (hex).
/// Use [`client`] for the main branch, or pass a branch's id (see
/// [`Cluster::branch_id`]).
pub fn client_for_branch(c: &Cluster, branch_hex: &str) -> anyhow::Result<PageserverClient> {
    let branch_id = BranchId::from_str(branch_hex).context("parse branch id")?;
    let url = format!("grpc://127.0.0.1:{}", c.ps_grpc_port());
    PageserverClient::new(branch_id, url, None, None).context("connect PS gRPC")
}

/// A gRPC `PageserverClient` for the cluster's main branch.
pub async fn client(c: &Cluster) -> anyhow::Result<PageserverClient> {
    let br = c.branch_id().await?;
    client_for_branch(c, &br)
}

/// Build a client for the pageserver behind a compute's
/// `SHOW yb_amp.pageserver_connstring` — e.g. a branch endpoint's OWN
/// pageserver (this cluster runs one pageserver per endpoint, so a branch
/// timeline is not served by the main endpoint's PS).
///
/// The compute connstring is the **libpq** page_service address
/// (`postgres://user@host:PG?...`, optionally comma-separated shards). The
/// gRPC page_service of the same pageserver sits at `PG + 200` under the
/// yb_light port scheme (pageserver pg = base+300, grpc = base+500, and
/// every per-instance bump shifts both equally), so we derive the gRPC URL
/// from it.
pub fn client_at_connstring(
    _c: &Cluster,
    branch_hex: &str,
    connstring: &str,
) -> anyhow::Result<PageserverClient> {
    let branch_id = BranchId::from_str(branch_hex).context("parse branch id")?;

    let raw = connstring.split(',').next().unwrap_or(connstring).trim();
    let after_scheme = raw.split("://").nth(1).unwrap_or(raw);
    let host_part = after_scheme.rsplit('@').next().unwrap_or(after_scheme); // drop userinfo
    let host_port = host_part.split(['?', '/']).next().unwrap_or(host_part); // drop query/path
    let (host, pg_port) = host_port
        .rsplit_once(':')
        .with_context(|| format!("connstring {raw:?} has no :port"))?;
    let pg_port: u16 = pg_port
        .parse()
        .with_context(|| format!("parse pageserver pg port from {raw:?}"))?;
    let url = format!("grpc://{host}:{}", pg_port + 200);

    PageserverClient::new(branch_id, url, None, None).context("connect branch PS gRPC")
}

/// A heap/relation `RelTag` in the default tablespace, main fork.
pub fn heap_rel(dbnode: u32, relnode: u32) -> RelTag {
    RelTag {
        forknum: 0,
        spcnode: DEFAULT_TABLESPACE,
        dbnode,
        relnode,
    }
}

/// Issue a `GetPage@LSN` for `blocks` and return the 8 KiB page images,
/// one per requested block in request order. `request_lsn` is the read
/// LSN; `not_modified_since` is the LwLSN hint. `req_id` just has to be
/// unique within a connection.
pub async fn get_pages(
    client: &PageserverClient,
    rel: RelTag,
    blocks: &[u32],
    request_lsn: Lsn,
    not_modified_since: Lsn,
    req_id: u64,
) -> anyhow::Result<Vec<Vec<u8>>> {
    let req = page_api::GetPageRequest {
        request_id: page_api::RequestID::new(req_id),
        request_class: page_api::GetPageClass::Normal,
        read_lsn: page_api::ReadLsn {
            request_lsn,
            not_modified_since_lsn: Some(not_modified_since),
        },
        rel,
        block_numbers: blocks.to_vec(),
    };
    let resp = client
        .get_page(req)
        .await
        .map_err(|e| anyhow::anyhow!("GetPage@{request_lsn} blocks={blocks:?}: {e}"))?;
    ensure!(
        resp.pages.len() == blocks.len(),
        "GetPage@{request_lsn}: got {} pages, want {}",
        resp.pages.len(),
        blocks.len()
    );
    let mut out = Vec::with_capacity(blocks.len());
    for p in resp.pages {
        let img = p.image.to_vec();
        ensure!(
            img.len() == 8192,
            "GetPage@{request_lsn}: page len {} (want 8192)",
            img.len()
        );
        out.push(img);
    }
    Ok(out)
}

/// Single-block [`get_pages`].
pub async fn get_page(
    client: &PageserverClient,
    rel: RelTag,
    block: u32,
    request_lsn: Lsn,
    not_modified_since: Lsn,
    req_id: u64,
) -> anyhow::Result<Vec<u8>> {
    let mut pages = get_pages(client, rel, &[block], request_lsn, not_modified_since, req_id).await?;
    Ok(pages.pop().expect("one page"))
}

/// Number of line pointers on a heap/index page, derived from `pd_lower`.
pub fn line_pointers(page: &[u8]) -> usize {
    let pd_lower = u16::from_le_bytes([page[12], page[13]]) as usize;
    pd_lower.saturating_sub(PAGE_HEADER_SIZE) / ITEM_ID_SIZE
}

/// `pd_lsn` from the page header (first 8 bytes: `xlogid << 32 | xrecoff`).
pub fn pd_lsn(page: &[u8]) -> Lsn {
    let xlogid = u32::from_le_bytes([page[0], page[1], page[2], page[3]]);
    let xrecoff = u32::from_le_bytes([page[4], page[5], page[6], page[7]]);
    Lsn(((xlogid as u64) << 32) | xrecoff as u64)
}

/// The `[yb_page_writer]` block that forces the materialiser + cache into
/// constant fold/evict/reconstruct churn: a 2-delta image trigger and a
/// 64 KiB read cache. Appended to `pageserver.toml` by [`stress_ps`].
const STRESS_PAGE_WRITER: &str = "\n\
     [yb_page_writer]\n\
     image_chain_limit = 2\n\
     image_chain_limit_slru = 4\n\
     image_chain_payload_bytes_limit = 64\n\
     materialize_queue_capacity = 4\n\
     read_cache_bytes = 65536\n\
     backpressure_bytes = 1048576\n";

/// Restart pageserver node 1 under the tiny-limits stress config and
/// return the original `pageserver.toml` so the caller can [`restore_ps`]
/// it. Pair the two on every exit path — the suite shares one cluster, so
/// a stuck stressed config would change every later scenario.
pub fn stress_ps(c: &Cluster) -> anyhow::Result<String> {
    let toml = c
        .repo_dir()
        .join("endpoints/main/pageserver/pageserver.toml");
    let original = std::fs::read_to_string(&toml).context("read pageserver.toml")?;
    std::fs::write(&toml, format!("{original}{STRESS_PAGE_WRITER}"))
        .context("write stressed pageserver.toml")?;
    step("restart PS under tiny limits (image_chain_limit=2, read_cache_bytes=64KiB)");
    c.yb(&["pageserver", "restart"])?;
    Ok(original)
}

/// Restore the original `pageserver.toml` and restart node 1. Best-effort:
/// logs (rather than fails) so a restore failure doesn't mask the
/// scenario's own result, but a stuck stressed config is loud.
pub fn restore_ps(c: &Cluster, original: &str) {
    let toml = c
        .repo_dir()
        .join("endpoints/main/pageserver/pageserver.toml");
    if let Err(e) = std::fs::write(&toml, original) {
        crate::tee_println!("  (WARN: failed to restore pageserver.toml: {e:#})");
    }
    if let Err(e) = c.yb(&["pageserver", "restart"]) {
        crate::tee_println!("  (WARN: failed to restart PS after config restore: {e:#})");
    }
}
