//! Postgres client connection code common to other crates (safekeeper and
//! pageserver) which depends on tenant/timeline ids and thus not fitting into
//! postgres_connection crate.

use anyhow::Context;
use postgres_connection::{PgConnectionConfig, parse_host_port};

use crate::id::BranchId;

#[derive(Copy, Clone, PartialEq, Eq, Debug, serde::Serialize, serde::Deserialize)]
#[serde(rename_all = "kebab-case")]
pub enum InterpretedFormat {
    Bincode,
    Protobuf,
}

#[derive(Debug, Clone, Copy, PartialEq, Eq, serde::Serialize, serde::Deserialize)]
#[serde(rename_all = "kebab-case")]
pub enum Compression {
    Zstd { level: i8 },
}

/// The wire protocol the pageserver uses to ingest WAL from the
/// safekeeper.
///
/// Upstream Neon also defined a `Vanilla` variant — raw Postgres
/// physical replication that arrived as `XLogData` blocks and was
/// decoded on the pageserver. That variant was disabled upstream
/// ("Vanilla WAL receiver protocol is no longer supported for
/// ingest") and removed entirely in lane N; yb-light always speaks
/// `Interpreted`, where the safekeeper decodes records once and
/// streams `(Key, Lsn, Value)` tuples directly. Kept as a
/// single-variant enum so the on-the-wire `protocol={"type":"interpreted",...}`
/// JSON shape doesn't change.
#[derive(Debug, Clone, Copy, PartialEq, Eq, serde::Serialize, serde::Deserialize)]
#[serde(tag = "type", content = "args")]
#[serde(rename_all = "kebab-case")]
pub enum PostgresClientProtocol {
    /// Custom shard-aware protocol that replicates interpreted records.
    /// Used to send wal from safekeeper to pageserver.
    Interpreted {
        format: InterpretedFormat,
        compression: Option<Compression>,
    },
}

pub struct ConnectionConfigArgs<'a> {
    pub protocol: PostgresClientProtocol,

    pub branch_id: BranchId,

    pub listen_pg_addr_str: &'a str,

    pub auth_token: Option<&'a str>,
}

impl<'a> ConnectionConfigArgs<'a> {
    fn options(&'a self) -> Vec<String> {
        let options = vec![
            "-c".to_owned(),
            format!("branch_id={}", self.branch_id),
            format!(
                "protocol={}",
                serde_json::to_string(&self.protocol).unwrap()
            ),
        ];

        options
    }
}

/// Create client config for fetching WAL from safekeeper on particular timeline.
/// listen_pg_addr_str is in form host:\[port\].
pub fn wal_stream_connection_config(
    args: ConnectionConfigArgs,
) -> anyhow::Result<PgConnectionConfig> {
    let (host, port) =
        parse_host_port(args.listen_pg_addr_str).context("Unable to parse listen_pg_addr_str")?;
    let port = port.unwrap_or(5432);
    let connstr = PgConnectionConfig::new_host_port(host, port)
        .extend_options(args.options())
        .set_password(args.auth_token.map(|s| s.to_owned()));

    Ok(connstr)
}
