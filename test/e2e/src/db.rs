//! Typed Postgres-wire query helpers for scenario assertions. Each call
//! opens a fresh connection, runs one query (or batch), and drops it —
//! the suite is sequential and never holds a connection across a
//! blocking subprocess call, so per-query connect cost is irrelevant and
//! the connection lifetime stays trivially correct.
//!
//! Connections speak plain Postgres wire over `NoTls` (the local
//! cluster runs `pg_auth_type = Trust`). Custom column types
//! (`pg_lsn`, etc.) are cast to `text` in SQL by callers rather than
//! relying on a `FromSql` impl.

use anyhow::Context;
use tokio_postgres::{Client, NoTls};

/// Connection string for a compute endpoint: trust auth, no password.
pub fn compute_conn(port: u16) -> String {
    format!("host=127.0.0.1 port={port} user=cloud_admin dbname=postgres")
}

/// Connection string for the YugabyteDB metadata database (the storage
/// backend), used by the restart scenarios to read
/// `pageserver.timeline_state` and the `wals` table directly.
pub fn yb_conn(host: &str, port: u16, dbname: &str) -> String {
    format!("host={host} port={port} user=yugabyte password=yugabyte dbname={dbname}")
}

/// Open a connection and spawn its driver task. The returned client
/// keeps the connection alive until dropped.
pub async fn connect(conninfo: &str) -> anyhow::Result<Client> {
    let (client, connection) = tokio_postgres::connect(conninfo, NoTls)
        .await
        .with_context(|| format!("connecting to {conninfo}"))?;
    tokio::spawn(async move {
        // The driver future resolves when the client is dropped; any
        // error here is a torn-down connection, not a test signal.
        let _ = connection.await;
    });
    Ok(client)
}

/// Run a single query returning one row, project column 0 as `i64`
/// (Postgres `count(*)` / `sum` over int come back as `int8`).
pub async fn query_i64(conninfo: &str, sql: &str) -> anyhow::Result<i64> {
    let client = connect(conninfo).await?;
    let row = client
        .query_one(sql, &[])
        .await
        .with_context(|| format!("query: {sql}"))?;
    Ok(row.get::<_, i64>(0))
}

/// Run a single query returning one row, project column 0 as `text`.
pub async fn query_text(conninfo: &str, sql: &str) -> anyhow::Result<String> {
    let client = connect(conninfo).await?;
    let row = client
        .query_one(sql, &[])
        .await
        .with_context(|| format!("query: {sql}"))?;
    Ok(row.get::<_, String>(0))
}

/// Run a query that may return zero rows or a NULL column-0; returns
/// `None` for either. Used to assert "was this column persisted?"
/// (the restart-correctness `prev_record_lsn` check).
pub async fn query_text_opt(conninfo: &str, sql: &str) -> anyhow::Result<Option<String>> {
    let client = connect(conninfo).await?;
    let rows = client
        .query(sql, &[])
        .await
        .with_context(|| format!("query: {sql}"))?;
    Ok(rows.first().and_then(|r| r.get::<_, Option<String>>(0)))
}

/// Execute a multi-statement SQL script (seed blocks, `CHECKPOINT`,
/// `CREATE PROCEDURE … $$ … $$`). Mirrors a `psql` heredoc.
pub async fn batch(conninfo: &str, sql: &str) -> anyhow::Result<()> {
    let client = connect(conninfo).await?;
    client
        .batch_execute(sql)
        .await
        .with_context(|| format!("batch: {}", sql.lines().next().unwrap_or("")))?;
    Ok(())
}
