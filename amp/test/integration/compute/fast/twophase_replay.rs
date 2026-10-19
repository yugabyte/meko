//! Replayed 2PC records read the state before their own LSN (#1627).
//!
//! The pageserver re-applies WAL above the persisted `consistent_lsn` on a
//! safekeeper reconnect and on an unclean restart. A replayed PREPARE used to
//! read the twophase directory at or after its own LSN, find its own xid and
//! fail `FileAlreadyExists` on every reconnect, and a replayed record earlier
//! in the window rewrote the directory from later state.
//!
//! The `fail-advance-consistent-lsn` failpoint lets rows reach YB while the
//! `consistent_lsn` UPDATE fails, so every record after the seed stays in the
//! replay window. Both kills are SIGKILL: `-m immediate` runs the shutdown
//! drain, which closes the window. The pageserver SIGKILL also clears the
//! failpoint.

use std::time::{Duration, Instant};

use anyhow::{Context, bail};
use tokio::time::sleep;

use crate::cluster::{Cluster, step};
use crate::db;
use crate::tee_println;

const CLUSTER: &str = "twophase_replay";
const ENDPOINT_ID: &str = "ep_2pc_replay";
const HOLD_CONSISTENT_LSN: &str = "ps::page-store::fail-advance-consistent-lsn";
const TIMEOUT: Duration = Duration::from_secs(60);

pub async fn run(c: &Cluster) -> anyhow::Result<()> {
    let pg_port = c.endpoint_port();

    step("provision a 2PC-enabled endpoint on its own amp cluster");
    let branch = c.cluster_create(CLUSTER)?;
    c.yb(&[
        "endpoint",
        "create",
        ENDPOINT_ID,
        "--branch-id",
        &branch,
        "--pg-version",
        "18",
        "--pg-port",
        &pg_port.to_string(),
        "--pg-setting",
        "max_prepared_transactions=5",
    ])?;
    c.yb(&["endpoint", "start", ENDPOINT_ID])?;

    let result = run_inner(c, &branch, pg_port).await;

    step("Tear down");
    c.yb_quiet(&["endpoint", "stop", ENDPOINT_ID, "--mode", "immediate"]);
    let _ = c.yb(&["branch", "drop", "--branch-id", &branch]);
    c.yb_quiet(&["pageserver", "stop", ENDPOINT_ID, "-m", "immediate"]);
    c.yb_quiet(&["safekeeper", "stop", ENDPOINT_ID, "-m", "immediate"]);
    result
}

async fn run_inner(c: &Cluster, branch: &str, pg_port: u16) -> anyhow::Result<()> {
    let compute = db::compute_conn(pg_port);
    let yb = db::yb_conn(c.yb_host(), c.yb_port(), c.db_name());
    let ep_dir = c.repo_dir().join("endpoints").join(ENDPOINT_ID);
    let ps_pid = ep_dir.join("pageserver/pageserver.pid");
    let sk_pid = ep_dir.join("safekeeper/safekeeper.pid");

    let ps_http = c.endpoint_ps_http_port(ENDPOINT_ID)?;

    step("seed, then hold consistent_lsn with the failpoint");
    db::batch(&compute, "CREATE TABLE tp_replay (id INT PRIMARY KEY)").await?;
    c.flush_branch(ps_http, branch, &compute).await?;
    set_failpoint(c, ps_http, "return").await?;
    // An UPDATE that started before the failpoint was armed can still land:
    // take the frontier once two reads a moment apart agree.
    let mut frontier = c
        .consistent_lsn(branch)
        .await?
        .context("no consistent_lsn")?;
    let mut stable = false;
    for _ in 0..10 {
        sleep(Duration::from_millis(500)).await;
        let again = c
            .consistent_lsn(branch)
            .await?
            .context("no consistent_lsn")?;
        if again == frontier {
            stable = true;
            break;
        }
        frontier = again;
    }
    if !stable {
        bail!("consistent_lsn kept moving with the failpoint armed (last {frontier})");
    }

    step("PREPARE a, COMMIT PREPARED a, PREPARE b");
    prepare(&compute, "a", 1).await?;
    db::batch(&compute, "COMMIT PREPARED 'a'").await?;
    let xid_b = prepare(&compute, "b", 2).await?;
    wait_twophase_file(&yb, branch, xid_b)
        .await
        .context("PREPARE b never reached YB")?;

    let dir_rows = |upto: i64| {
        format!(
            "SELECT COALESCE(string_agg(lsn::text || ':' || encode(payload, 'hex'), ',' \
                                        ORDER BY lsn), '') \
             FROM amp_storage.meta WHERE branch_id = '{branch}' \
               AND amp_storage.gc_key_decode(key) = 'meta f1=2 f2=0 f3=0 f4=0 f5=0 f6=0' \
               AND lsn > {frontier} AND lsn <= {upto}"
        )
    };
    let window_dir = db::query_text(&yb, &dir_rows(i64::MAX)).await?;
    let rows: Vec<&str> = window_dir.split(',').filter(|s| !s.is_empty()).collect();
    if rows.len() < 3 {
        bail!(
            "expected 3 twophase directory rows above consistent_lsn {frontier}, got {window_dir:?}"
        );
    }
    let window_end: i64 = rows[rows.len() - 1]
        .split_once(':')
        .context("malformed directory row")?
        .0
        .parse()?;
    tee_println!("  replay window ({frontier}, {window_end}]: {window_dir}");

    step("SIGKILL the safekeeper; the running PS reconnects and replays the window");
    c.kill9(&sk_pid, "safekeeper")?;
    c.yb_quiet(&["safekeeper", "stop", ENDPOINT_ID, "-m", "immediate"]);
    c.yb(&["safekeeper", "start", ENDPOINT_ID])?;

    let xid_c = prepare(&compute, "c", 3).await?;
    wait_twophase_file(&yb, branch, xid_c)
        .await
        .context("ingest did not get past the replayed PREPAREs after a reconnect")?;
    if c.consistent_lsn(branch).await? != Some(frontier) {
        bail!("consistent_lsn moved off {frontier}: the reconnect did not replay the window");
    }
    let replayed_dir = db::query_text(&yb, &dir_rows(window_end)).await?;
    if replayed_dir != window_dir {
        bail!(
            "reconnect replay rewrote the twophase directory:\n  before {window_dir}\n  after  {replayed_dir}"
        );
    }

    step("SIGKILL the PS, which also clears the failpoint, and restart it");
    c.kill9(&ps_pid, "pageserver")?;
    c.yb_quiet(&["pageserver", "stop", ENDPOINT_ID, "-m", "immediate"]);
    c.yb(&["pageserver", "start", ENDPOINT_ID])?;

    step("wait for ingest to pass the replayed PREPAREs");
    // Retried: the restarted PS answers 503 until the timeline is active.
    poll(|| async { Ok(c.flush_branch(ps_http, branch, &compute).await.ok()) })
        .await
        .context("ingest did not reach the tip after an unclean restart")?;

    step("restart the compute: the basebackup must list b and c as prepared");
    c.yb(&["endpoint", "stop", ENDPOINT_ID, "--mode", "immediate"])?;
    c.yb(&["endpoint", "start", ENDPOINT_ID])?;
    let gids = db::query_text(
        &compute,
        "SELECT COALESCE(string_agg(gid, ',' ORDER BY gid), '') FROM pg_prepared_xacts",
    )
    .await?;
    if gids != "b,c" {
        bail!("prepared transactions after restart: {gids:?}, want \"b,c\"");
    }
    db::batch(&compute, "COMMIT PREPARED 'b'").await?;
    db::batch(&compute, "COMMIT PREPARED 'c'").await?;
    let n = db::query_text(
        &compute,
        "SELECT string_agg(id::text, ',' ORDER BY id) FROM tp_replay",
    )
    .await?;
    if n != "1,2,3" {
        bail!("rows after COMMIT PREPARED: {n:?}, want \"1,2,3\"");
    }
    Ok(())
}

async fn set_failpoint(c: &Cluster, ps_http: u16, actions: &str) -> anyhow::Result<()> {
    c.daemon_client(ps_http)?
        .put(format!("http://127.0.0.1:{ps_http}/v1/failpoints"))
        .json(&serde_json::json!([{ "name": HOLD_CONSISTENT_LSN, "actions": actions }]))
        .send()
        .await?
        .error_for_status()?;
    Ok(())
}

async fn prepare(conn: &str, gid: &str, id: i32) -> anyhow::Result<i64> {
    db::batch(
        conn,
        &format!("BEGIN; INSERT INTO tp_replay VALUES ({id}); PREPARE TRANSACTION '{gid}';"),
    )
    .await?;
    db::query_i64(
        conn,
        &format!("SELECT transaction::text::bigint FROM pg_prepared_xacts WHERE gid = '{gid}'"),
    )
    .await
}

async fn wait_twophase_file(yb: &str, branch: &str, xid: i64) -> anyhow::Result<()> {
    let sql = format!(
        "SELECT count(*) FROM amp_storage.meta WHERE branch_id = '{branch}' \
           AND amp_storage.gc_key_decode(key) = 'meta f1=2 f2=0 f3=0 f4=0 f5=0 f6={xid}'"
    );
    poll(|| async { Ok((db::query_i64(yb, &sql).await? > 0).then_some(())) }).await
}

async fn poll<T, F, Fut>(mut f: F) -> anyhow::Result<T>
where
    F: FnMut() -> Fut,
    Fut: std::future::Future<Output = anyhow::Result<Option<T>>>,
{
    let deadline = Instant::now() + TIMEOUT;
    loop {
        if let Some(v) = f().await? {
            return Ok(v);
        }
        if Instant::now() >= deadline {
            bail!("timed out after {TIMEOUT:?}");
        }
        sleep(Duration::from_millis(500)).await;
    }
}
