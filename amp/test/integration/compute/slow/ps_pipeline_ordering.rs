//! #173: adversarial ordering test for the pipelined page_service executor.
//!
//! A failpoint injects a random per-batch async delay so batches complete OUT
//! of submission order. The executor holds them in a `FuturesOrdered` and must
//! still write responses in SUBMISSION order — the pagestream protocol
//! correlates a response to its request purely by order (there is no request
//! id). If the executor ever emitted a response out of order, the compute
//! would associate the wrong page with a request, read the wrong tuples, and
//! the whole-scan fingerprint would diverge from the failpoint-free reference.
//!
//! We drive a Bitmap Heap Scan (a selective range on a scattered indexed
//! column): its heap prefetch is eic-driven, so it issues a concurrent burst
//! of scattered getpages that fills the pipeline even at the harness's tiny
//! shared_buffers — unlike a pin-limited seqscan, which would run ~serial and
//! never exercise depth > 1.

use std::time::Duration;

use anyhow::ensure;

use crate::cluster::{Cluster, step};
use crate::{db, page_api};

const N_ROWS: i64 = 300_000;
const FP: &str = "ps::pipeline-execute-batch";

/// Startup GUCs that force a Bitmap Heap Scan + a deep prefetch window, passed
/// as libpq `options` on the *read* connection so they apply to the (single,
/// prepared) fingerprint statement — an inline multi-statement `SET …; SELECT`
/// is rejected by the extended-query protocol ("cannot insert multiple commands
/// into a prepared statement"). The reference needs none of these: count+sum
/// over the range is identical under any plan.
const TUNE: &str = " options='-c enable_seqscan=off -c enable_indexscan=off \
     -c effective_io_concurrency=32'";
/// count+sum over the matched rows: a misattributed page changes `v`, so the
/// fingerprint catches any response reordering.
const FINGERPRINT: &str = "SELECT count(*)::text || ':' \
     || coalesce(sum(v::bigint), 0)::text FROM pipe_oow WHERE r BETWEEN 1 AND 30000";

pub async fn run(c: &Cluster) -> anyhow::Result<()> {
    let main = db::compute_conn(c.compute_port());

    step("seed a table with a scattered indexed column + capture the reference fingerprint");
    db::batch(
        &main,
        "DROP TABLE IF EXISTS pipe_oow; \
         CREATE TABLE pipe_oow (id int, r int, v int) WITH (autovacuum_enabled = off);",
    )
    .await?;
    db::batch(
        &main,
        &format!(
            "INSERT INTO pipe_oow \
             SELECT g, (g * 2654435761) % 1000000, g FROM generate_series(1, {N_ROWS}) g; \
             CREATE INDEX pipe_oow_r ON pipe_oow (r);"
        ),
    )
    .await?;
    let reference = db::query_text(&main, FINGERPRINT).await?;

    let toml = c
        .repo_dir()
        .join("endpoints/main/pageserver/pageserver.toml");
    let original = std::fs::read_to_string(&toml)?;
    let result = run_inner(c, &main, &original, &reference).await;
    let _ = set_failpoint(c, "off").await; // best-effort clear (restore_ps also restarts)
    result.and(page_api::restore_ps(c, &original))?;

    db::batch(&main, "DROP TABLE IF EXISTS pipe_oow").await?;
    Ok(())
}

async fn run_inner(
    c: &Cluster,
    main: &str,
    original: &str,
    reference: &str,
) -> anyhow::Result<()> {
    step("pipeline_depth=16 (cold PS); inject a random per-batch delay via failpoint");
    page_api::restart_with_pipeline_depth(c, original, 16)?;
    // 60% of batch executions async-sleep 80ms, so completion order is
    // scrambled vs submission order across the scan's many batches. Set after
    // the PS restart (which resets failpoints), before the read.
    set_failpoint(c, "60%return(80)").await?;
    // Cold compute buffers so the scan reaches the PS (and the pipeline).
    c.yb_quiet(&["endpoint", "stop", "main", "--mode", "immediate"]);
    c.yb(&["endpoint", "start", "main"])?;
    tokio::time::sleep(Duration::from_secs(3)).await;

    step("cold bitmap scan under forced out-of-order completion; fingerprint must match");
    let got = db::query_text(&format!("{main}{TUNE}"), FINGERPRINT).await?;
    ensure!(
        got == reference,
        "fingerprint under forced out-of-order completion = {got}, want {reference} \
         — the pipeline emitted responses out of submission order"
    );
    crate::tee_println!(
        "  ok — responses stayed in submission order under forced out-of-order completion ({got})"
    );
    Ok(())
}

/// Configure the `ps::pipeline-execute-batch` failpoint on the pageserver via
/// its management API. `actions` is a fail-crate action string (e.g.
/// `"60%return(80)"` or `"off"`).
async fn set_failpoint(c: &Cluster, actions: &str) -> anyhow::Result<()> {
    let url = format!("http://127.0.0.1:{}/v1/failpoints", c.ps_http_port());
    reqwest::Client::new()
        .put(&url)
        .json(&serde_json::json!([{ "name": FP, "actions": actions }]))
        .send()
        .await?
        .error_for_status()?;
    Ok(())
}
