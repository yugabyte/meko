//! Exercises the pageserver's parallel WAL-apply path end to end.
//!
//! The shipping default only parallelizes large batches
//! (`ingest_parallelism = 4`, `ingest_parallelism_min_batch = 256`), so a
//! normal workload rarely fans the apply across workers. Here we restart the
//! PS with `ingest_parallelism_min_batch = 1` so EVERY ingest batch takes the
//! parallel path (partition-by-key, concurrent deserialize+build+push), then
//! run a multi-relation write workload and read it all back.
//!
//! Correctness is checked two ways: the row-count / content assertions below
//! (served by the pageserver's read path over the just-applied data) and the
//! harness's post-scenario corruption gate. A bug in the parallel push —
//! dropped, duplicated, reordered, or cross-contaminated page versions —
//! shows up as a wrong count, a content mismatch, or a corruption marker.
//!
//! Restart-on-exit restores the default config so later scenarios (this suite
//! shares one cluster) run under defaults.

use anyhow::{Context, ensure};

use crate::cluster::{Cluster, step};
use crate::{db, page_api};

pub async fn run(c: &Cluster) -> anyhow::Result<()> {
    let toml = c
        .repo_dir()
        .join("endpoints/main/pageserver/pageserver.toml");
    let original = std::fs::read_to_string(&toml).context("read pageserver.toml")?;
    // Merge into [yb_page_writer], creating it if absent — a duplicate
    // table header would fail the pageserver's toml parse.
    let forced = crate::cluster::merge_toml_section(
        &original,
        "yb_page_writer",
        "ingest_parallelism = 4\n\
         ingest_parallelism_min_batch = 1\n",
    );
    std::fs::write(&toml, &forced).context("write parallel pageserver.toml")?;
    step("restart PS with parallel apply forced on every batch (ingest_parallelism=4, min_batch=1)");
    c.yb_logged(&["pageserver", "restart"])?;

    let result = run_inner(c).await;

    // Restore the default config + restart on every exit path. The
    // scenario's own failure takes precedence over a restore failure.
    result.and(page_api::restore_ps(c, &original))
}

async fn run_inner(c: &Cluster) -> anyhow::Result<()> {
    let main = db::compute_conn(c.compute_port());

    step("write a multi-relation workload through the forced-parallel apply path");
    db::batch(
        &main,
        "DROP TABLE IF EXISTS pa_a, pa_b, pa_c; \
         CREATE TABLE pa_a (id int primary key, v text); \
         CREATE TABLE pa_b (id int primary key, v text); \
         CREATE TABLE pa_c (id int primary key, v text); \
         INSERT INTO pa_a SELECT g, repeat('a', 200) FROM generate_series(1, 5000) g; \
         INSERT INTO pa_b SELECT g, repeat('b', 200) FROM generate_series(1, 5000) g; \
         INSERT INTO pa_c SELECT g, md5(g::text) FROM generate_series(1, 5000) g; \
         UPDATE pa_a SET v = repeat('z', 200) WHERE id % 3 = 0; \
         DELETE FROM pa_b WHERE id % 5 = 0;",
    )
    .await?;

    step("read back through the pageserver and verify counts + content");
    let a = db::query_i64(&main, "SELECT count(*) FROM pa_a").await?;
    let b = db::query_i64(&main, "SELECT count(*) FROM pa_b").await?;
    let c_cnt = db::query_i64(&main, "SELECT count(*) FROM pa_c").await?;
    ensure!(a == 5000, "pa_a count {a} != 5000");
    ensure!(b == 4000, "pa_b count {b} != 4000 (5000 - 1000 deleted)");
    ensure!(c_cnt == 5000, "pa_c count {c_cnt} != 5000");

    // Content checks force reads of many distinct pages back through the
    // pageserver, so a mis-applied version surfaces as a mismatch.
    let updated = db::query_i64(
        &main,
        "SELECT count(*) FROM pa_a WHERE v = repeat('z', 200)",
    )
    .await?;
    ensure!(updated == 1666, "pa_a updated rows {updated} != 1666 (floor(5000/3))");
    let content_ok = db::query_i64(&main, "SELECT count(*) FROM pa_c WHERE v = md5(id::text)").await?;
    ensure!(content_ok == 5000, "pa_c content mismatch: {content_ok}/5000 rows intact");

    crate::tee_println!(
        "  parallel apply: wrote + verified pa_a/pa_b/pa_c through forced-parallel ingest"
    );
    db::batch(&main, "DROP TABLE IF EXISTS pa_a, pa_b, pa_c").await?;
    Ok(())
}
