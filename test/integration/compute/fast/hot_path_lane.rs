//! Prove the page + WAL hot path actually rides the thin-RPC (`tsclient`)
//! lane on BOTH the safekeeper and the pageserver — not a silent fallback
//! to the YSQL lane.
//!
//! Both components expose a `path`-labeled latency histogram whose series
//! only materializes once that lane serves an op:
//! `safekeeper_yb_wal_insert_seconds{path}` (the consumer's WAL flush to
//! YB) and `pageserver_yb_store_op_seconds{op,path}` (every PageStore
//! round trip — `put_pages` on the materialize flush, `read_chain` on
//! getpage). We snapshot the `tsclient` lane's op count on each, then:
//!   1. seed a table + `CHECKPOINT` — flushes WAL to the safekeeper and
//!      drives the pageserver to materialize + flush pages to YB;
//!   2. restart the endpoint to drop the compute buffer cache, then read
//!      the table back — a cold read that forces getpage (`read_chain`).
//!
//! Finally we require both lanes' counts to advance. If the traffic
//! silently ran on the YSQL lane, the `tsclient` count stays flat and this
//! fails — the corruption gate can't catch that, since both lanes return
//! identical bytes.

use std::time::{Duration, Instant};

use anyhow::{Context, bail};

use crate::cluster::{Cluster, step};
use crate::{db, tee_println};

/// The hot-path transport under test. `tsclient` is the thin-RPC lane the
/// page + WAL hot path is meant to ride (the daemon default); its
/// path-labeled metrics only appear once it serves an op.
const LANE: &str = "tsclient";
const PS_METRIC: &str = "pageserver_yb_store_op_seconds";
const SK_METRIC: &str = "safekeeper_yb_wal_insert_seconds";

pub async fn run(c: &Cluster) -> anyhow::Result<()> {
    step(&format!("hot-path lane check (transport: {LANE})"));

    // Baseline the lane's op count on each component before we drive any
    // traffic.
    let ps_before = lane_op_samples(c.ps_http_port(), PS_METRIC, LANE)
        .await
        .context("scrape pageserver lane baseline")?;
    let sk_before = lane_op_samples(c.sk_http_port(), SK_METRIC, LANE)
        .await
        .context("scrape safekeeper lane baseline")?;

    let compute = db::compute_conn(c.compute_port());

    // WAL hop (safekeeper) + page materialize/flush hop (pageserver): the
    // INSERT's commit flushes WAL to the safekeeper, and the CHECKPOINT
    // drives the pageserver to flush the new pages to YB.
    step("write a table + CHECKPOINT through the compute");
    db::batch(
        &compute,
        "DROP TABLE IF EXISTS hot_path_lane_probe;\n\
         CREATE TABLE hot_path_lane_probe (id int primary key, v text);\n\
         INSERT INTO hot_path_lane_probe SELECT g, 'r' || g FROM generate_series(1, 200) g;\n\
         CHECKPOINT;",
    )
    .await
    .context("seed hot_path_lane_probe")?;

    // Getpage hop (pageserver read): restart the endpoint to drop the
    // compute buffer cache, so the read below misses cache and pulls every
    // heap page from the pageserver — the getpage (`read_chain`) hot path.
    step("restart the endpoint (drop compute cache), then read the table back cold");
    c.yb_quiet(&["endpoint", "stop", "main", "--mode", "immediate"]);
    c.yb(&["endpoint", "start", "main"])
        .context("restart main endpoint")?;
    let n = db::query_i64(&compute, "SELECT count(*) FROM hot_path_lane_probe")
        .await
        .context("cold read hot_path_lane_probe")?;
    if n != 200 {
        bail!("cold read saw {n} rows, want 200");
    }

    // The safekeeper WAL flush and the pageserver→YB page flush are
    // asynchronous (the page flush lags the CHECKPOINT), so poll each
    // lane's count until it advances rather than reading once.
    step("wait for both lanes' op counts to advance");
    let deadline = Instant::now() + Duration::from_secs(45);
    let (mut ps_now, mut sk_now) = (ps_before, sk_before);
    loop {
        if ps_now <= ps_before {
            ps_now = lane_op_samples(c.ps_http_port(), PS_METRIC, LANE).await?;
        }
        if sk_now <= sk_before {
            sk_now = lane_op_samples(c.sk_http_port(), SK_METRIC, LANE).await?;
        }
        if ps_now > ps_before && sk_now > sk_before {
            break;
        }
        if Instant::now() > deadline {
            let mut stuck = Vec::new();
            if ps_now <= ps_before {
                stuck.push(format!(
                    "pageserver served no op on the {LANE} lane \
                     ({PS_METRIC}{{path=\"{LANE}\"}} stuck at {ps_before})"
                ));
            }
            if sk_now <= sk_before {
                stuck.push(format!(
                    "safekeeper committed no WAL on the {LANE} lane \
                     ({SK_METRIC}{{path=\"{LANE}\"}} stuck at {sk_before})"
                ));
            }
            bail!(
                "after a write + CHECKPOINT + cold read: {} \
                 — hot-path traffic silently ran on the YSQL lane",
                stuck.join("; ")
            );
        }
        tokio::time::sleep(Duration::from_millis(500)).await;
    }

    tee_println!(
        "  both hot paths rode the {LANE} lane \
         (pageserver {ps_before}->{ps_now}, safekeeper {sk_before}->{sk_now} ops)"
    );

    step("cleanup: drop the probe table");
    db::batch(&compute, "DROP TABLE IF EXISTS hot_path_lane_probe;")
        .await
        .context("drop hot_path_lane_probe")?;

    Ok(())
}

/// Scrape a component's `/metrics` and return the largest `<metric>_count`
/// across every series of the `metric` histogram that carries
/// `path="<path>"` — how many ops that lane has served. A reachable
/// endpoint with no such series yields 0 (a histogram series only
/// materializes once its lane observes its first sample); an unreachable
/// endpoint is an error.
async fn lane_op_samples(port: u16, metric: &str, path: &str) -> anyhow::Result<u64> {
    let body = reqwest::get(format!("http://127.0.0.1:{port}/metrics"))
        .await
        .and_then(|r| r.error_for_status())
        .with_context(|| format!("scrape /metrics on port {port}"))?
        .text()
        .await
        .context("read /metrics body")?;
    let prefix = format!("{metric}_count{{");
    let label = format!("path=\"{path}\"");
    Ok(body
        .lines()
        .filter(|l| l.starts_with(&prefix) && l.contains(&label))
        .filter_map(|l| l.split_whitespace().last())
        .filter_map(|v| v.parse::<f64>().ok())
        .map(|v| v as u64)
        .max()
        .unwrap_or(0))
}
