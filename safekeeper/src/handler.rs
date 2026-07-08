//! Part of Safekeeper pretending to be Postgres, i.e. handling Postgres
//! protocol commands.

use std::future::Future;
use std::str::{self, FromStr};
use std::sync::Arc;

use anyhow::Context;
use jsonwebtoken::TokenData;
use postgres_backend::{PostgresBackend, QueryError};
use postgres_ffi::PG_TLI;
use pq_proto::{BeMessage, FeStartupPacket, INT4_OID, RowDescriptor, TEXT_OID};
use regex::Regex;
use safekeeper_api::models::ConnectionId;
use tokio::io::{AsyncRead, AsyncWrite};
use tracing::{Instrument, debug, info, info_span};
use utils::auth::{Claims, JwtAuth, Scope};
use utils::id::BranchId;
use utils::lsn::Lsn;
use utils::postgres_client::PostgresClientProtocol;

use crate::auth::check_permission;
use crate::metrics::{PG_QUERIES_GAUGE, TrafficMetrics};
use crate::timeline::TimelineError;
use crate::{GlobalTimelines, SafeKeeperConf};

/// Safekeeper handler of postgres commands
pub struct SafekeeperPostgresHandler {
    pub conf: Arc<SafeKeeperConf>,
    /// assigned application name
    pub appname: Option<String>,
    /// `branch_id` parsed from the libpq startup options (`branch_id=<id>`),
    /// before it's validated and promoted to [`Self::branch_id`].
    pub parsed_branch_id: Option<BranchId>,
    pub branch_id: BranchId,
    pub protocol: Option<PostgresClientProtocol>,
    /// Unique connection id is logged in spans for observability.
    pub conn_id: ConnectionId,
    pub global_timelines: Arc<GlobalTimelines>,
    /// Auth scope allowed on the connections and public key used to check auth tokens. None if auth is not configured.
    auth: Option<(Scope, Arc<JwtAuth>)>,
    claims: Option<Claims>,
    io_metrics: Option<TrafficMetrics>,
}

/// Parsed Postgres command.
enum SafekeeperPostgresCommand {
    StartWalPush {
        proto_version: u32,
        // Eventually timelines will be always created explicitly by storcon.
        // This option allows legacy behaviour for compute to do that until we
        // fully migrate.
        allow_timeline_creation: bool,
    },
    StartReplication {
        start_lsn: Lsn,
    },
    IdentifySystem,
    TimelineStatus,
}

fn parse_cmd(cmd: &str) -> anyhow::Result<SafekeeperPostgresCommand> {
    if cmd.starts_with("START_WAL_PUSH") {
        // Allow additional options in postgres START_REPLICATION style like
        //   START_WAL_PUSH (proto_version '3', allow_timeline_creation 'false').
        // Parsing here is very naive and breaks in case of commas or
        // whitespaces in values, but enough for our purposes.
        let re = Regex::new(r"START_WAL_PUSH(\s+?\((.*)\))?").unwrap();
        let caps = re
            .captures(cmd)
            .context(format!("failed to parse START_WAL_PUSH command {cmd}"))?;
        // capture () content
        let options = caps.get(2).map(|m| m.as_str()).unwrap_or("");
        // yb-light only speaks proto v3; v2 was dropped in Phase 9
        // Wave 2. Default matches the walproposer hardcode so an
        // older client missing the option is still parsed cleanly.
        let mut proto_version = 3;
        let mut allow_timeline_creation = true;
        for kvstr in options.split(",") {
            if kvstr.is_empty() {
                continue;
            }
            let mut kvit = kvstr.split_whitespace();
            let key = kvit.next().context(format!(
                "failed to parse key in kv {kvstr} in command {cmd}"
            ))?;
            let value = kvit.next().context(format!(
                "failed to parse value in kv {kvstr} in command {cmd}"
            ))?;
            let value_trimmed = value.trim_matches('\'');
            if key == "proto_version" {
                proto_version = value_trimmed.parse::<u32>().context(format!(
                    "failed to parse proto_version value {value} in command {cmd}"
                ))?;
            }
            if key == "allow_timeline_creation" {
                allow_timeline_creation = value_trimmed.parse::<bool>().context(format!(
                    "failed to parse allow_timeline_creation value {value} in command {cmd}"
                ))?;
            }
        }
        Ok(SafekeeperPostgresCommand::StartWalPush {
            proto_version,
            allow_timeline_creation,
        })
    } else if cmd.starts_with("START_REPLICATION") {
        let re = Regex::new(
            r"START_REPLICATION(?: SLOT [^ ]+)?(?: PHYSICAL)? ([[:xdigit:]]+/[[:xdigit:]]+)",
        )
        .unwrap();
        let caps = re
            .captures(cmd)
            .context(format!("failed to parse START_REPLICATION command {cmd}"))?;
        let start_lsn =
            Lsn::from_str(&caps[1]).context("parse start LSN from START_REPLICATION command")?;
        Ok(SafekeeperPostgresCommand::StartReplication { start_lsn })
    } else if cmd.starts_with("IDENTIFY_SYSTEM") {
        Ok(SafekeeperPostgresCommand::IdentifySystem)
    } else if cmd.starts_with("TIMELINE_STATUS") {
        Ok(SafekeeperPostgresCommand::TimelineStatus)
    } else {
        anyhow::bail!("unsupported command {cmd}");
    }
}

fn cmd_to_string(cmd: &SafekeeperPostgresCommand) -> &str {
    match cmd {
        SafekeeperPostgresCommand::StartWalPush { .. } => "START_WAL_PUSH",
        SafekeeperPostgresCommand::StartReplication { .. } => "START_REPLICATION",
        SafekeeperPostgresCommand::TimelineStatus => "TIMELINE_STATUS",
        SafekeeperPostgresCommand::IdentifySystem => "IDENTIFY_SYSTEM",
    }
}

impl<IO: AsyncRead + AsyncWrite + Unpin + Send> postgres_backend::Handler<IO>
    for SafekeeperPostgresHandler
{
    // branch_id is passed in connection string params
    fn startup(
        &mut self,
        _pgb: &mut PostgresBackend<IO>,
        sm: &FeStartupPacket,
    ) -> Result<(), QueryError> {
        if let FeStartupPacket::StartupMessage { params, .. } = sm {
            if let Some(options) = params.options_raw() {

                for opt in options {
                    match opt.split_once('=') {
                        Some(("protocol", value)) => {
                            self.protocol =
                                Some(serde_json::from_str(value).with_context(|| {
                                    format!("Failed to parse {value} as protocol")
                                })?);
                        }
                        Some(("branch_id", value)) => {
                            self.parsed_branch_id = Some(value.parse().with_context(|| {
                                format!("Failed to parse {value} as branch id")
                            })?);
                        }
                        Some(("availability_zone", client_az)) => {
                            if let Some(metrics) = self.io_metrics.as_ref() {
                                metrics.set_client_az(client_az)
                            }
                        }
                        // Unknown startup options are ignored.
                        _ => continue,
                    }
                }
            }

            if let Some(app_name) = params.get("application_name") {
                self.appname = Some(app_name.to_owned());
                if let Some(metrics) = self.io_metrics.as_ref() {
                    metrics.set_app_name(app_name)
                }
            }

            let branch_id = self
                .parsed_branch_id
                .unwrap_or(BranchId::from([0u8; 16]));
            tracing::Span::current()
                .record("branch_id", tracing::field::display(branch_id))
                .record(
                    "application_name",
                    tracing::field::debug(self.appname.clone()),
                );

            Ok(())
        } else {
            Err(QueryError::Other(anyhow::anyhow!(
                "Safekeeper received unexpected initial message: {sm:?}"
            )))
        }
    }

    fn check_auth_jwt(
        &mut self,
        _pgb: &mut PostgresBackend<IO>,
        jwt_response: &[u8],
    ) -> Result<(), QueryError> {
        // this unwrap is never triggered, because check_auth_jwt only called when auth_type is YbLightJWT
        // which requires auth to be present
        let (allowed_auth_scope, auth) = self
            .auth
            .as_ref()
            .expect("auth_type is configured but .auth of handler is missing");
        let data: TokenData<Claims> = auth
            .decode(str::from_utf8(jwt_response).context("jwt response is not UTF-8")?)
            .map_err(|e| QueryError::Unauthorized(e.0))?;

        // The handler might be configured to allow only tenant scope tokens.
        if matches!(allowed_auth_scope, Scope::Tenant)
            && !matches!(data.claims.scope, Scope::Tenant)
        {
            return Err(QueryError::Unauthorized(
                "passed JWT token is for full access, but only tenant scope is allowed".into(),
            ));
        }

        if matches!(data.claims.scope, Scope::Tenant) && data.claims.branch_id.is_none() {
            return Err(QueryError::Unauthorized(
                "jwt token scope is Tenant, but branch id is missing".into(),
            ));
        }

        debug!(
            "jwt scope check succeeded for scope: {:#?} by branch id: {:?}",
            data.claims.scope, data.claims.branch_id,
        );

        self.claims = Some(data.claims);
        Ok(())
    }

    fn process_query(
        &mut self,
        pgb: &mut PostgresBackend<IO>,
        query_string: &str,
    ) -> impl Future<Output = Result<(), QueryError>> {
        Box::pin(async move {
            if query_string
                .to_ascii_lowercase()
                .starts_with("set datestyle to ")
            {
                // important for debug because psycopg2 executes "SET datestyle TO 'ISO'" on connect
                pgb.write_message_noflush(&BeMessage::CommandComplete(b"SELECT 1"))?;
                return Ok(());
            }

            let cmd = parse_cmd(query_string)?;
            let cmd_str = cmd_to_string(&cmd);

            let _guard = PG_QUERIES_GAUGE.with_label_values(&[cmd_str]).guard();

            info!("got query {:?}", query_string);

            let branch_id = self.parsed_branch_id.context("branch_id is required")?;
            self.check_permission(Some(branch_id))?;
            self.branch_id = branch_id;

            // This process serves a single branch (single-branch-per-
            // process). Reject a connection for any other branch before it
            // can touch the registry — `safekeeper.timeline_state` is shared
            // across the cluster, so a misrouted walproposer must not create
            // or mutate a foreign branch here.
            if let Some(baked) = self.conf.branch_id {
                if self.branch_id != baked {
                    return Err(QueryError::Other(anyhow::anyhow!(
                        "this safekeeper serves {baked}, refusing connection for {}",
                        self.branch_id
                    )));
                }
            }

            match cmd {
                SafekeeperPostgresCommand::StartWalPush {
                    proto_version,
                    allow_timeline_creation,
                } => {
                    self.handle_start_wal_push(pgb, proto_version, allow_timeline_creation)
                        .instrument(info_span!("WAL receiver"))
                        .await
                }
                SafekeeperPostgresCommand::StartReplication { start_lsn } => {
                    self.handle_start_replication(pgb, start_lsn)
                        .instrument(info_span!("WAL sender"))
                        .await
                }
                SafekeeperPostgresCommand::IdentifySystem => self.handle_identify_system(pgb).await,
                SafekeeperPostgresCommand::TimelineStatus => self.handle_timeline_status(pgb).await,
            }
        })
    }
}

impl SafekeeperPostgresHandler {
    pub fn new(
        conf: Arc<SafeKeeperConf>,
        conn_id: u32,
        io_metrics: Option<TrafficMetrics>,
        auth: Option<(Scope, Arc<JwtAuth>)>,
        global_timelines: Arc<GlobalTimelines>,
    ) -> Self {
        SafekeeperPostgresHandler {
            conf,
            appname: None,
            parsed_branch_id: None,
            branch_id: BranchId::from_array([0u8; 16]),
            protocol: None,
            conn_id,
            claims: None,
            auth,
            io_metrics,
            global_timelines,
        }
    }

    pub fn protocol(&self) -> PostgresClientProtocol {
        // yb-light only supports the Interpreted protocol; default if
        // the client didn't specify one (older clients that used to
        // default to Vanilla now get Interpreted instead).
        self.protocol
            .unwrap_or(PostgresClientProtocol::Interpreted {
                format: utils::postgres_client::InterpretedFormat::Protobuf,
                compression: None,
            })
    }

    // when accessing management api supply None as an argument
    // when using to authorize a branch pass the corresponding branch id
    fn check_permission(&self, branch_id: Option<BranchId>) -> Result<(), QueryError> {
        if self.auth.is_none() {
            // auth is set to Trust, nothing to check so just return ok
            return Ok(());
        }
        // auth is some, just checked above, when auth is some
        // then claims are always present because of checks during connection init
        // so this expect won't trigger
        let claims = self
            .claims
            .as_ref()
            .expect("claims presence already checked");
        check_permission(claims, branch_id).map_err(|e| QueryError::Unauthorized(e.0))
    }

    async fn handle_timeline_status<IO: AsyncRead + AsyncWrite + Unpin>(
        &mut self,
        pgb: &mut PostgresBackend<IO>,
    ) -> Result<(), QueryError> {
        // Get timeline, handling "not found" error
        let tli = match self.global_timelines.get(self.branch_id) {
            Ok(tli) => Ok(Some(tli)),
            Err(TimelineError::NotFound(_)) => Ok(None),
            Err(e) => Err(QueryError::Other(e.into())),
        }?;

        // Write row description
        pgb.write_message_noflush(&BeMessage::RowDescription(&[
            RowDescriptor::text_col(b"flush_lsn"),
            RowDescriptor::text_col(b"commit_lsn"),
        ]))?;

        // Write row if timeline exists
        if let Some(tli) = tli {
            let flush_lsn = tli.get_flush_lsn().await;
            // yb-light: commit_lsn ≡ flush_lsn (single-SK quorum).
            pgb.write_message_noflush(&BeMessage::DataRow(&[
                Some(flush_lsn.to_string().as_bytes()),
                Some(flush_lsn.to_string().as_bytes()),
            ]))?;
        }

        pgb.write_message_noflush(&BeMessage::CommandComplete(b"TIMELINE_STATUS"))?;
        Ok(())
    }

    ///
    /// Handle IDENTIFY_SYSTEM replication command
    ///
    async fn handle_identify_system<IO: AsyncRead + AsyncWrite + Unpin>(
        &mut self,
        pgb: &mut PostgresBackend<IO>,
    ) -> Result<(), QueryError> {
        let tli = self.global_timelines.get(self.branch_id)?;

        // yb-light: commit_lsn ≡ flush_lsn (single-SK quorum), so both
        // branches return the same value.
        let lsn = tli.get_flush_lsn().await.to_string();

        let sysid = tli.get_state().await.1.server.system_id.to_string();
        let lsn_bytes = lsn.as_bytes();
        let tli = PG_TLI.to_string();
        let tli_bytes = tli.as_bytes();
        let sysid_bytes = sysid.as_bytes();

        pgb.write_message_noflush(&BeMessage::RowDescription(&[
            RowDescriptor {
                name: b"systemid",
                typoid: TEXT_OID,
                typlen: -1,
                ..Default::default()
            },
            RowDescriptor {
                name: b"timeline",
                typoid: INT4_OID,
                typlen: 4,
                ..Default::default()
            },
            RowDescriptor {
                name: b"xlogpos",
                typoid: TEXT_OID,
                typlen: -1,
                ..Default::default()
            },
            RowDescriptor {
                name: b"dbname",
                typoid: TEXT_OID,
                typlen: -1,
                ..Default::default()
            },
        ]))?
        .write_message_noflush(&BeMessage::DataRow(&[
            Some(sysid_bytes),
            Some(tli_bytes),
            Some(lsn_bytes),
            None,
        ]))?
        .write_message_noflush(&BeMessage::CommandComplete(b"IDENTIFY_SYSTEM"))?;
        Ok(())
    }

    /// Returns true if current connection is a replication connection, originating
    /// from a walproposer recovery function. This connection gets a special handling:
    /// safekeeper must stream all local WAL till the flush_lsn, whether committed or not.
    pub fn is_walproposer_recovery(&self) -> bool {
        match &self.appname {
            None => false,
            Some(appname) => {
                appname == "wal_proposer_recovery" ||
                // set by safekeeper peer recovery
                appname.starts_with("safekeeper")
            }
        }
    }
}

#[cfg(test)]
mod tests {
    use super::SafekeeperPostgresCommand;

    /// Test parsing of START_WAL_PUSH command
    #[test]
    fn test_start_wal_push_parse() {
        let cmd = "START_WAL_PUSH";
        let parsed = super::parse_cmd(cmd).expect("failed to parse");
        match parsed {
            SafekeeperPostgresCommand::StartWalPush {
                proto_version,
                allow_timeline_creation,
            } => {
                assert_eq!(proto_version, 3);
                assert!(allow_timeline_creation);
            }
            _ => panic!("unexpected command"),
        }

        let cmd =
            "START_WAL_PUSH (proto_version '3', allow_timeline_creation 'false', unknown 'hoho')";
        let parsed = super::parse_cmd(cmd).expect("failed to parse");
        match parsed {
            SafekeeperPostgresCommand::StartWalPush {
                proto_version,
                allow_timeline_creation,
            } => {
                assert_eq!(proto_version, 3);
                assert!(!allow_timeline_creation);
            }
            _ => panic!("unexpected command"),
        }
    }
}
