//! Helpers for scenarios that park a commit in the sync-rep wait while the
//! safekeeper is down.

use std::future::Future;
use std::time::{Duration, Instant};

use anyhow::{Context, bail};
use tokio::sync::mpsc::UnboundedReceiver;
use tokio::task::JoinHandle;
use tokio_postgres::CancelToken;
use tokio_postgres::error::DbError;

use crate::cluster::{Cluster, step};
use crate::db;

const PARK_TIMEOUT: Duration = Duration::from_secs(30);

pub type Insert = JoinHandle<Result<u64, tokio_postgres::Error>>;

/// An INSERT blocked in the sync-rep wait, and the notices its session gets.
pub struct Parked {
    pub pid: i32,
    pub token: CancelToken,
    pub insert: Insert,
    pub notices: UnboundedReceiver<DbError>,
}

/// Runs `body` with the safekeeper stopped. The restart comes before the
/// result is returned, so a failure cannot stall later tests on the cluster.
pub async fn with_safekeeper_down<T>(
    c: &Cluster,
    body: impl Future<Output = anyhow::Result<T>>,
) -> anyhow::Result<T> {
    step("stop safekeeper so commits block in the sync-rep wait");
    c.yb(&["safekeeper", "stop", "-m", "immediate"])?;
    let result = body.await;
    step("restart safekeeper");
    match (result, c.yb(&["safekeeper", "start"])) {
        (Err(e), Err(restart)) => bail!("{e:#}; the safekeeper restart also failed: {restart:#}"),
        (result, restarted) => {
            restarted?;
            result
        }
    }
}

/// Whether `pid` is in the sync-rep wait.
pub async fn in_syncrep(conn: &str, pid: i32) -> anyhow::Result<bool> {
    let sql = format!(
        "SELECT count(*) FROM pg_stat_activity WHERE pid = {pid} AND wait_event = 'SyncRep'"
    );
    Ok(db::query_i64(conn, &sql).await? == 1)
}

/// Starts `INSERT INTO <table> VALUES (1)` on a new session and waits for it
/// to block in the sync-rep wait.
pub async fn park_insert(conn: &str, table: &str) -> anyhow::Result<Parked> {
    let (client, notices) = db::connect_with_notices(conn).await?;
    let pid: i32 = client
        .query_one("SELECT pg_backend_pid()", &[])
        .await?
        .get(0);
    let token = client.cancel_token();
    let sql = format!("INSERT INTO {table} VALUES (1)");
    let insert: Insert = tokio::spawn(async move { client.execute(sql.as_str(), &[]).await });

    step("wait for the INSERT to block on SyncRep");
    let deadline = Instant::now() + PARK_TIMEOUT;
    while !in_syncrep(conn, pid).await? {
        if insert.is_finished() {
            let result = insert.await.context("insert task")?;
            bail!("the INSERT finished instead of blocking in the sync-rep wait: {result:?}");
        }
        if Instant::now() > deadline {
            bail!("the INSERT did not block in the sync-rep wait within {PARK_TIMEOUT:?}");
        }
        tokio::time::sleep(Duration::from_millis(200)).await;
    }
    Ok(Parked {
        pid,
        token,
        insert,
        notices,
    })
}

/// Waits for the notice whose message is `message`, skipping any other the
/// session got.
pub async fn wait_notice(
    notices: &mut UnboundedReceiver<DbError>,
    message: &str,
    timeout: Duration,
) -> anyhow::Result<DbError> {
    let next = async {
        while let Some(notice) = notices.recv().await {
            if notice.message() == message {
                return Ok(notice);
            }
        }
        bail!("the session closed without the notice {message:?}")
    };
    tokio::time::timeout(timeout, next)
        .await
        .with_context(|| format!("no notice {message:?} within {timeout:?}"))?
}
