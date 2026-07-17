//! Helpers for the `sk_wal*` scenarios — read WAL directly from a
//! safekeeper over the libpq replication protocol. The analog of
//! [`crate::page_api`] for the safekeeper's WAL read path.
//!
//! yb-light's safekeeper only serves the **Interpreted** WAL protocol
//! (decoded records framed as protobuf), not vanilla `XLogData` — see
//! `compute/safekeeper/src/handler.rs` (`protocol()` defaults to Interpreted). So a
//! replication client receives `ReplicationMessage::RawInterpretedWalRecords`
//! batches, whose framing/batching is timing-dependent. We therefore read
//! by **LSN reached**, not by byte-identical content: catch up to a target
//! LSN and report the bytes delivered.

use std::pin::pin;
use std::time::Duration;

use anyhow::{Context, bail};
use futures::StreamExt;
use postgres_protocol::message::backend::ReplicationMessage;
use tokio_postgres::replication::ReplicationStream;
use utils::lsn::Lsn;

use crate::cluster::Cluster;

/// Connect to safekeeper node 1 in replication mode for the cluster's
/// branch, stream interpreted WAL from `start`, and read until the
/// stream's `streaming_lsn` reaches `target`. Returns the total interpreted
/// payload bytes delivered (a liveness signal — content framing is not
/// stable across reads, so callers compare *reaching the target*, not
/// bytes). Errors if the stream ends or stalls before `target`.
pub async fn read_committed_wal(
    c: &Cluster,
    branch_hex: &str,
    start: Lsn,
    target: Lsn,
) -> anyhow::Result<usize> {
    let mut cfg = tokio_postgres::Config::new();
    cfg.host("127.0.0.1")
        .port(c.sk_pg_port())
        .user("cloud_admin")
        .dbname("postgres")
        // The SK reads the branch id from the `options` startup param
        // (`key=value`, parsed by its handler as `branch_id=<id>`).
        .options(&format!("branch_id={branch_hex}"))
        .replication_mode(tokio_postgres::config::ReplicationMode::Physical);

    let (client, connection) = cfg
        .connect(tokio_postgres::NoTls)
        .await
        .context("connect safekeeper in replication mode")?;
    let conn = tokio::spawn(async move {
        let _ = connection.await;
    });

    let query = format!("START_REPLICATION PHYSICAL {start}");
    let copy = client
        .copy_both_simple(&query)
        .await
        .context("START_REPLICATION PHYSICAL")?;
    let mut stream = pin!(ReplicationStream::new(copy));

    let mut total = 0usize;
    let mut reached = start;
    let result = loop {
        if reached >= target {
            break Ok(total);
        }
        match tokio::time::timeout(Duration::from_secs(30), stream.next()).await {
            Ok(Some(msg)) => match msg.context("replication stream error")? {
                ReplicationMessage::RawInterpretedWalRecords(raw) => {
                    total += raw.data().len();
                    reached = Lsn(raw.streaming_lsn());
                }
                ReplicationMessage::PrimaryKeepAlive(ka) => {
                    // A keepalive's wal_end tracks how far the SK has WAL;
                    // once it's at/over the target with no more records to
                    // send, we've caught up.
                    reached = reached.max(Lsn(ka.wal_end()));
                }
                _ => {}
            },
            Ok(None) => {
                break Err(anyhow::anyhow!(
                    "safekeeper stream ended at {reached} before target {target}"
                ));
            }
            Err(_) => {
                break Err(anyhow::anyhow!(
                    "safekeeper stalled at {reached} before target {target} (got {total} bytes)"
                ));
            }
        }
    };

    drop(client);
    conn.abort();
    match result {
        Ok(t) => Ok(t),
        Err(e) => bail!(e),
    }
}
