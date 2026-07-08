//! #173: several compute connections drive the pipelined page_service
//! executor concurrently. A prior write-path change regressed only under
//! multiple connections because no test exercised that shape; this guards the
//! read pipeline against per-connection executor state bleeding across
//! connections. Each connection cold-seqscans the same table and must see the
//! identical fingerprint — a cross-connection response mixup would corrupt it.

use std::time::Duration;

use anyhow::ensure;

use crate::cluster::{Cluster, step};
use crate::{db, page_api};

const N_ROWS: i64 = 300_000;
const CONNS: usize = 6;

const FINGERPRINT: &str = "SELECT count(*)::text || ':' \
     || coalesce(sum(id::bigint), 0)::text || ':' \
     || coalesce(sum((v::bigint) * (id % 7 + 1)), 0)::text FROM pipe_mc";

pub async fn run(c: &Cluster) -> anyhow::Result<()> {
    let main = db::compute_conn(c.compute_port());

    step("seed a multi-page table and capture the reference fingerprint");
    db::batch(
        &main,
        "DROP TABLE IF EXISTS pipe_mc; \
         CREATE TABLE pipe_mc (id int, v int) WITH (autovacuum_enabled = off);",
    )
    .await?;
    db::batch(
        &main,
        &format!(
            "INSERT INTO pipe_mc \
             SELECT g, (g * 2654435761) % 1000000 FROM generate_series(1, {N_ROWS}) g"
        ),
    )
    .await?;
    let reference = db::query_text(&main, FINGERPRINT).await?;

    let toml = c
        .repo_dir()
        .join("endpoints/main/pageserver/pageserver.toml");
    let original = std::fs::read_to_string(&toml)?;
    let result = run_inner(c, &reference).await;
    page_api::restore_ps(c, &original);
    result?;

    db::batch(&main, "DROP TABLE IF EXISTS pipe_mc").await?;
    Ok(())
}

async fn run_inner(c: &Cluster, reference: &str) -> anyhow::Result<()> {
    let toml = c
        .repo_dir()
        .join("endpoints/main/pageserver/pageserver.toml");
    let original = std::fs::read_to_string(&toml)?;

    step(&format!(
        "pipeline_depth=16, cold PS + cold compute; run {CONNS} concurrent seqscans"
    ));
    page_api::restart_with_pipeline_depth(c, &original, 16)?;
    c.yb_quiet(&["endpoint", "stop", "main", "--mode", "immediate"]);
    c.yb(&["endpoint", "start", "main"])?;
    tokio::time::sleep(Duration::from_secs(3)).await;

    let conninfo = db::compute_conn(c.compute_port());
    let mut handles = Vec::with_capacity(CONNS);
    for _ in 0..CONNS {
        let ci = conninfo.clone();
        handles.push(tokio::spawn(
            async move { db::query_text(&ci, FINGERPRINT).await },
        ));
    }
    for (i, h) in handles.into_iter().enumerate() {
        let got = h.await??;
        ensure!(
            got == reference,
            "connection {i} fingerprint = {got}, want {reference} \
             — cross-connection response mixup under the read pipeline"
        );
    }
    crate::tee_println!("  {CONNS} concurrent cold seqscans all returned the correct fingerprint");
    Ok(())
}
