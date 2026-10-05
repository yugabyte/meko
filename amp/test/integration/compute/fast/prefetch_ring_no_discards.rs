//! A cold seq scan at PG18's default `effective_io_concurrency` must not
//! overflow the compute's prefetch ring (#1518).
//!
//! The scan prefetches up to `effective_io_concurrency × io_combine_limit`
//! pages (16 × 16 = 256). A ring smaller than that discards prefetched pages
//! before the scan reads them, so each page is fetched twice.
//! `shared_buffers` is 512MB so the backend's pin share (~284 pages) doesn't
//! cap the read-ahead below the ring and hide the overflow.

use anyhow::{bail, ensure};

use crate::cluster::{Cluster, step};
use crate::db;

const ROWS: u32 = 20_000;

pub async fn run(c: &Cluster) -> anyhow::Result<()> {
    let port = c.endpoint_port();

    step("provision an endpoint with 512MB shared_buffers and the LFC off");
    let branch_id = c.branch_create_named("prf_ring", c.branch())?;
    c.yb(&[
        "endpoint", "create", "ep_prf_ring", "--branch-id", &branch_id,
        "--pg-version", "18", "--pg-port", &port.to_string(),
        "--pg-setting", "shared_buffers=512MB",
        "--pg-setting", "yb_amp.file_cache_size_limit=0",
    ])?;
    c.yb(&["endpoint", "start", "ep_prf_ring"])?;

    let result = run_scan(c, port).await;

    step("cleanup: deprovision the endpoint");
    c.yb_quiet(&["endpoint", "stop", "ep_prf_ring", "--destroy"]);
    result
}

async fn run_scan(c: &Cluster, port: u16) -> anyhow::Result<()> {
    let ep = db::compute_conn(port);

    let sb = db::query_text(&ep, "SHOW shared_buffers").await?;
    ensure!(sb == "512MB", "shared_buffers is {sb}, want 512MB: the pin share would cap the read-ahead");

    step("load a ~22MB table");
    db::batch(&ep, "CREATE EXTENSION IF NOT EXISTS yb_amp").await?;
    db::batch(
        &ep,
        &format!(
            "CREATE TABLE prf_t (id int, pad text) WITH (autovacuum_enabled = off); \
             INSERT INTO prf_t SELECT g, repeat('x', 900) FROM generate_series(1, {ROWS}) g;"
        ),
    )
    .await?;
    db::batch(
        &ep,
        "CREATE FUNCTION prf_scan() RETURNS text LANGUAGE plpgsql AS $$
         DECLARE d0 float8; d1 float8; r0 float8; r1 float8; n bigint;
         BEGIN
           PERFORM set_config('effective_io_concurrency', '16', true);
           PERFORM set_config('max_parallel_workers_per_gather', '0', true);
           SELECT value INTO d0 FROM yb_amp_backend_perf_counters
             WHERE pid = pg_backend_pid() AND metric = 'getpage_prefetch_discards_total';
           SELECT value INTO r0 FROM yb_amp_backend_perf_counters
             WHERE pid = pg_backend_pid() AND metric = 'getpage_prefetch_requests_total';
           SELECT count(*) INTO n FROM prf_t;
           SELECT value INTO d1 FROM yb_amp_backend_perf_counters
             WHERE pid = pg_backend_pid() AND metric = 'getpage_prefetch_discards_total';
           SELECT value INTO r1 FROM yb_amp_backend_perf_counters
             WHERE pid = pg_backend_pid() AND metric = 'getpage_prefetch_requests_total';
           RETURN (d1 - d0)::bigint || ',' || (r1 - r0)::bigint || ',' || n;
         END $$",
    )
    .await?;
    let pages = db::query_i64(&ep, "SELECT pg_relation_size('prf_t') / 8192").await?;

    step("restart the endpoint so the scan starts cold");
    c.yb(&["endpoint", "stop", "ep_prf_ring"])?;
    c.yb(&["endpoint", "start", "ep_prf_ring"])?;

    step("scan at effective_io_concurrency 16 and count discarded prefetches");
    let out = db::query_text(&ep, "SELECT prf_scan()").await?;
    let v: Vec<i64> = out.split(',').map(|s| s.parse()).collect::<Result<_, _>>()?;
    let (discards, prefetches, rows) = (v[0], v[1], v[2]);
    crate::tee_println!("  {pages} pages: {prefetches} prefetches, {discards} discarded");

    ensure!(rows == ROWS as i64, "scan returned {rows} rows, want {ROWS}");
    ensure!(
        prefetches * 10 >= pages * 9,
        "only {prefetches} prefetches for {pages} pages: the scan didn't read ahead, so the check is vacuous"
    );
    if discards * 100 > pages {
        bail!(
            "{discards} of {prefetches} prefetches discarded on a {pages}-page scan: \
             yb_amp.readahead_buffer_size is smaller than the scan's read-ahead \
             (effective_io_concurrency × io_combine_limit), so pages are fetched twice"
        );
    }
    Ok(())
}
