//! #173: the pipelined page_service executor keeps up to `pipeline_depth`
//! getpage batch-reads in flight and must write their responses back in
//! SUBMISSION order — the pagestream protocol correlates a response to its
//! request purely by order (there is no request id). This asserts that a
//! cold, multi-page seqscan produces an identical whole-table fingerprint at
//! `pipeline_depth = 1` (serial) and `pipeline_depth = 16` (pipelined). If the
//! pipeline ever reordered responses, pages would be misattributed and the
//! fingerprint would diverge.

use std::time::Duration;

use anyhow::ensure;

use crate::cluster::{Cluster, step};
use crate::{db, page_api};

/// Enough rows to span many heap pages, so a cold seqscan issues a burst of
/// getpages that fills the pipeline at depth > 1.
const N_ROWS: i64 = 300_000;

/// Whole-table fingerprint: count + a sum over ids + a mixed sum. Every value
/// depends on every page carrying the right tuples, so a misattributed page
/// (double-counted / missing ids) changes the fingerprint.
const FINGERPRINT: &str = "SELECT count(*)::text || ':' \
     || coalesce(sum(id::bigint), 0)::text || ':' \
     || coalesce(sum((v::bigint) * (id % 7 + 1)), 0)::text FROM pipe_equiv";

pub async fn run(c: &Cluster) -> anyhow::Result<()> {
    let main = db::compute_conn(c.compute_port());

    step("seed a multi-page table and capture the warm reference fingerprint");
    db::batch(
        &main,
        "DROP TABLE IF EXISTS pipe_equiv; \
         CREATE TABLE pipe_equiv (id int, v int) WITH (autovacuum_enabled = off);",
    )
    .await?;
    db::batch(
        &main,
        &format!(
            "INSERT INTO pipe_equiv \
             SELECT g, (g * 2654435761) % 1000000 FROM generate_series(1, {N_ROWS}) g"
        ),
    )
    .await?;
    let reference = db::query_text(&main, FINGERPRINT).await?;

    let toml = c
        .repo_dir()
        .join("endpoints/main/pageserver/pageserver.toml");
    let original = std::fs::read_to_string(&toml)?;
    let result = run_depths(c, &main, &original, &reference).await;
    result.and(page_api::restore_ps(c, &original))?;

    db::batch(&main, "DROP TABLE IF EXISTS pipe_equiv").await?;
    Ok(())
}

async fn run_depths(
    c: &Cluster,
    main: &str,
    original: &str,
    reference: &str,
) -> anyhow::Result<()> {
    for depth in [1usize, 16] {
        step(&format!(
            "pipeline_depth={depth}: restart PS (cold cache) + compute (cold buffers), re-read cold"
        ));
        page_api::restart_with_pipeline_depth(c, original, depth)?;
        // Drop the compute's shared_buffers so the seqscan actually reaches
        // the PS (else it serves from local buffers and never exercises the
        // pipeline).
        c.yb_quiet(&["endpoint", "stop", "main", "--mode", "immediate"]);
        c.yb(&["endpoint", "start", "main"])?;
        tokio::time::sleep(Duration::from_secs(3)).await;

        let got = db::query_text(main, FINGERPRINT).await?;
        ensure!(
            got == reference,
            "cold seqscan fingerprint at pipeline_depth={depth} = {got}, want {reference} \
             — the pipelined executor returned misattributed/reordered pages"
        );
        crate::tee_println!("  pipeline_depth={depth}: fingerprint matches ({got})");
    }
    Ok(())
}
