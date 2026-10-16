//! #1636: the materialiser folds relation pages from unflushed deltas and
//! writes the image straight to YB. A pageserver SIGKILL then loses those
//! deltas while their image survives, and a safekeeper restart makes the
//! live walreceiver replay over rows an image already replaced. Both must
//! leave every page readable and the btree consistent.
//!
//! The hot rightmost leaf of a bigserial index crosses the fold threshold
//! many times inside one flush window, which is also what exposed the
//! cache-trim hole fixed alongside the change. A failpoint holds the
//! pageserver's flushes until YB holds such an image, so the kill always
//! lands in that state.

use std::time::{Duration, Instant};

use anyhow::{bail, ensure};
use regex::Regex;

use crate::cluster::{Cluster, MARKERS, step};
use crate::db;
use crate::tee_println;

const TABLE: &str = "fold_unflushed";
const HOLD_FLUSHER: &str = "ps::page-writer::hold-flusher";
const CORRECTNESS: &str = "pageserver_yb_correctness_errors_total";

/// Single-row commits for `secs` on one connection; returns rows inserted.
async fn insert_for(main: &str, secs: u64) -> anyhow::Result<i64> {
    let client = db::connect(main).await?;
    let insert = format!("INSERT INTO {TABLE} (v, pad) VALUES (1, repeat('x', 40))");
    let deadline = Instant::now() + Duration::from_secs(secs);
    let mut rows = 0i64;
    while Instant::now() < deadline {
        client.batch_execute(&insert).await?;
        rows += 1;
    }
    Ok(rows)
}

async fn set_failpoint(c: &Cluster, actions: &str) -> anyhow::Result<()> {
    let url = format!("http://127.0.0.1:{}/v1/failpoints", c.ps_http_port());
    c.daemon_client(c.ps_http_port())?
        .put(&url)
        .json(&serde_json::json!([{ "name": HOLD_FLUSHER, "actions": actions }]))
        .send()
        .await?
        .error_for_status()?;
    Ok(())
}

/// Images of the table's blocks in YB above the persisted `consistent_lsn`.
/// With the flusher held after one explicit flush, every row above it is
/// unflushed, so each such image is a fold from unflushed deltas.
async fn images_above_unflushed(c: &Cluster, main: &str) -> anyhow::Result<i64> {
    let relnodes = db::query_text(
        main,
        &format!(
            "SELECT pg_relation_filenode('{TABLE}')::text || ',' || \
             pg_relation_filenode('{TABLE}_pkey')::text"
        ),
    )
    .await?;
    let yb = db::yb_conn(c.yb_host(), c.yb_port(), c.db_name());
    db::query_i64(
        &yb,
        &format!(
            "SELECT count(*) FROM amp_storage.pages i
             WHERE i.branch_id = '{branch}' AND i.is_image AND i.relnode IN ({relnodes})
               AND i.lsn > (SELECT consistent_lsn FROM amp_storage.branch_state
                            WHERE branch_id = '{branch}')",
            branch = c.branch(),
        ),
    )
    .await
}

pub async fn run(c: &Cluster) -> anyhow::Result<()> {
    let main = db::compute_conn(c.compute_port());
    let log_start = std::fs::metadata(c.ps_log())
        .map(|m| m.len() as usize)
        .unwrap_or(0);

    step("Create a table with a bigserial PK (one hot rightmost leaf)");
    db::batch(
        &main,
        &format!(
            "DROP TABLE IF EXISTS {TABLE};
             CREATE TABLE {TABLE} (id BIGSERIAL PRIMARY KEY, v INT NOT NULL, pad TEXT NOT NULL)
                 WITH (autovacuum_enabled = false);"
        ),
    )
    .await?;
    let mut expected = 0i64;

    step("Hold the pageserver's flushes; insert until YB has an image above unflushed deltas");
    set_failpoint(c, "return").await?;
    let errors_before = c.metric_sum(c.ps_http_port(), CORRECTNESS).await?;
    // A timeout, not a deadline checked between inserts: with flushes held,
    // a buffer at `backpressure_bytes` blocks ingest, and the inserts with it.
    let reached = tokio::time::timeout(Duration::from_secs(60), async {
        // Images the CREATE wrote (the index build logs whole pages) land below this.
        c.flush_branch(c.ps_http_port(), c.branch(), &main).await?;
        loop {
            expected += insert_for(&main, 1).await?;
            let held = images_above_unflushed(c, &main).await?;
            if held > 0 {
                return anyhow::Ok(held);
            }
        }
    })
    .await
    .unwrap_or_else(|_| Err(anyhow::anyhow!("no image above unflushed deltas within 60s")));
    let errors = c.metric_sum(c.ps_http_port(), CORRECTNESS).await? - errors_before;
    // The kill clears the failpoint; disarm it here only if the scenario stops first.
    let held = match reached {
        Ok(held) if errors == 0.0 => held,
        reached => {
            set_failpoint(c, "off").await?;
            ensure!(errors == 0.0, "{errors} correctness error(s) before the kill");
            return reached.map(|_| ());
        }
    };
    tee_println!("  {expected} rows, {held} image(s) above unflushed deltas; SIGKILL");
    c.kill9(
        &c.repo_dir()
            .join("endpoints/main/pageserver/pageserver.pid"),
        "pageserver",
    )?;
    tokio::time::sleep(Duration::from_secs(1)).await;
    c.yb(&["pageserver", "start"])?;
    tokio::time::sleep(Duration::from_secs(3)).await;

    step("Insert, then restart the safekeeper (live walreceiver replays over images)");
    expected += insert_for(&main, 4).await?;
    c.yb(&["safekeeper", "restart"])?;
    tokio::time::sleep(Duration::from_secs(5)).await;

    step("Insert after both restarts");
    expected += insert_for(&main, 2).await?;
    db::batch(&main, "CHECKPOINT;").await?;

    step("Read the table and its index back through the pageserver");
    // Best effort: drop the compute's cached copies so reads go to the pageserver.
    let _ = db::batch(
        &main,
        &format!(
            "CREATE EXTENSION IF NOT EXISTS pg_buffercache;
             SELECT pg_buffercache_evict_relation('{TABLE}'::regclass);
             SELECT pg_buffercache_evict_relation('{TABLE}_pkey'::regclass);"
        ),
    )
    .await;
    let client = db::connect(&main).await?;
    client
        .batch_execute("SET enable_indexscan = off; SET enable_indexonlyscan = off; SET enable_bitmapscan = off")
        .await?;
    let seq: i64 = client
        .query_one(&format!("SELECT count(*) FROM {TABLE}"), &[])
        .await?
        .get(0);
    client
        .batch_execute(
            "SET enable_seqscan = off; SET enable_indexscan = on; SET enable_indexonlyscan = on",
        )
        .await?;
    let idx: i64 = client
        .query_one(&format!("SELECT count(*) FROM {TABLE} WHERE id > 0"), &[])
        .await?
        .get(0);
    drop(client);
    if seq != expected || idx != expected {
        bail!("row counts after restarts: seq {seq}, index {idx}, expected {expected}");
    }
    if db::batch(&main, "CREATE EXTENSION IF NOT EXISTS amcheck;")
        .await
        .is_ok()
    {
        db::batch(
            &main,
            &format!("SELECT bt_index_check('{TABLE}_pkey'::regclass, true);"),
        )
        .await?;
        tee_println!("  bt_index_check passed");
    }
    tee_println!("  ok — {expected} rows by seq scan and by index");

    step("Scan pageserver log for corruption markers (this iteration only)");
    let full_log = std::fs::read_to_string(c.ps_log()).unwrap_or_default();
    let log = full_log.get(log_start..).unwrap_or(full_log.as_str());
    let markers = Regex::new(MARKERS).expect("static regex");
    let hits: Vec<&str> = log.lines().filter(|l| markers.is_match(l)).collect();
    if !hits.is_empty() {
        for l in hits.iter().take(5) {
            eprintln!("{l}");
        }
        bail!("{} corruption marker(s) in pageserver.log", hits.len());
    }

    let _ = db::batch(&main, &format!("DROP TABLE IF EXISTS {TABLE}")).await;
    Ok(())
}
