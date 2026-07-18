//! Error taxonomy, mirroring the `ysql_client` retry classes:
//!
//! - transport failures (TCP connect/read/write, RPC timeouts, RPC-layer
//!   error frames that kill the connection) → [`ErrorClass::Network`],
//!   the "ConnectionDrop" class: the retry driver re-establishes the
//!   connection + session before the next attempt;
//! - transient server conditions (`TryAgain`, `LeaderNotReadyToServe`,
//!   `Busy`, `ServiceUnavailable`, tablet splits, ...) →
//!   [`ErrorClass::TryAgain`]: retried in place with backoff;
//! - schema-version mismatches → [`ErrorClass::SchemaMismatch`]: the
//!   caller re-opens the table;
//! - invalid arguments / misuse → [`ErrorClass::Invalid`], everything
//!   else → [`ErrorClass::Other`]; both are fatal (never retried).
//!
//! A lost server session ("Unknown session" from
//! `PgClientServiceImpl::GetSession`) is classified as `Network`: it has
//! the same remedy as a dropped connection (new session, re-opened
//! tables, restarted `serial_no`), and [`YbError::is_session_lost`] lets
//! the recovery path tell the two apart when it matters.

use crate::pb::yb::rpc::error_status_pb::RpcErrorCodePb;
use crate::pb::yb::{AppStatusPb, PgsqlResponsePb, app_status_pb, pgsql_response_pb};

/// Coarse failure classes, mirroring the `ysql_client` retry taxonomy.
#[derive(Debug, Clone, Copy, PartialEq, Eq)]
pub enum ErrorClass {
    /// Bad arguments / misuse — never retried.
    Invalid,
    /// Transport error, timeout, or lost session — retried after the
    /// client reconnects and re-establishes its session.
    Network,
    /// Transient server condition (leader change, busy) — retried with backoff.
    TryAgain,
    /// Read-restart (`TryAgainCode restart_read_time`): the read point is
    /// ambiguous against a concurrently-committed row. Retrying a *first*
    /// page (fresh serial → new read point) heals it, but a paged
    /// continuation must reuse its serial and would replay the same
    /// failing read point forever — so the whole scan has to restart. Kept
    /// separate from `TryAgain` for exactly that reason (see
    /// [`YbError::is_read_restart`] and the retry driver).
    ReadRestart,
    /// Table schema version changed — caller re-opens the table and retries.
    SchemaMismatch,
    /// Anything else — surfaced as fatal.
    Other,
}

#[derive(Debug, Clone, thiserror::Error)]
pub enum YbError {
    /// No endpoint of the cluster is reachable.
    #[error("yb tsclient unavailable: {0}")]
    Unavailable(String),
    #[error("yb tsclient error ({class:?}): {message}")]
    Call { class: ErrorClass, message: String },
    #[error("yb row decode error: {0}")]
    Decode(String),
}

impl YbError {
    pub fn class(&self) -> ErrorClass {
        match self {
            YbError::Call { class, .. } => *class,
            YbError::Unavailable(_) => ErrorClass::Network,
            YbError::Decode(_) => ErrorClass::Other,
        }
    }

    /// Whether a blind retry (after backoff, and for `Network` after a
    /// session re-establishment) may succeed. Safe for every caller by
    /// contract: reads are snapshots and writes are idempotent upserts.
    pub fn is_retryable(&self) -> bool {
        matches!(
            self.class(),
            ErrorClass::Network | ErrorClass::TryAgain | ErrorClass::ReadRestart
        )
    }

    /// A read-restart (`restart_read_time`). Retryable in place only for a
    /// first page or a single-Perform `read_pages` (each attempt takes a
    /// fresh read point); a paged continuation instead surfaces this so
    /// the caller can restart the whole scan (see the retry driver and
    /// [`TsClient::read_scan`]).
    pub fn is_read_restart(&self) -> bool {
        self.class() == ErrorClass::ReadRestart
    }

    /// True when this attempt gave up waiting for a response (per-call
    /// deadline): the RPC may still be executing server-side. Timeouts
    /// are `Network`-class (the remedy — reconnect + retry — is the
    /// same), but they're counted separately: a run of them with a
    /// healthy transport is the signature of a server-side stall, which
    /// this client has been bitten by silently before.
    pub fn is_timeout(&self) -> bool {
        match self {
            YbError::Call { class, message } => {
                *class == ErrorClass::Network && message.contains("timed out")
            }
            _ => false,
        }
    }

    /// True when the tserver rejected our `session_id` (e.g.
    /// "Connection terminated unexpectedly due to unknown session N"):
    /// the connection is healthy but the server-side session expired
    /// (60s idle default) or the tserver restarted.
    pub fn is_session_lost(&self) -> bool {
        match self {
            YbError::Call { class, message } => {
                matches!(class, ErrorClass::Network) && is_unknown_session_message(message)
            }
            _ => false,
        }
    }

    pub(crate) fn network(message: impl Into<String>) -> Self {
        YbError::Call {
            class: ErrorClass::Network,
            message: message.into(),
        }
    }

    pub(crate) fn invalid(message: impl Into<String>) -> Self {
        YbError::Call {
            class: ErrorClass::Invalid,
            message: message.into(),
        }
    }

    pub(crate) fn other(message: impl Into<String>) -> Self {
        YbError::Call {
            class: ErrorClass::Other,
            message: message.into(),
        }
    }

    /// A mid-scan snapshot loss whose remedy is to restart the whole scan
    /// (e.g. the pinned server session was recovered out from under a
    /// paged continuation). Same class as a `restart_read_time` because
    /// the fix is identical — see [`YbError::is_read_restart`].
    pub(crate) fn read_restart(message: impl Into<String>) -> Self {
        YbError::Call {
            class: ErrorClass::ReadRestart,
            message: message.into(),
        }
    }

    pub(crate) fn decode(message: impl Into<String>) -> Self {
        YbError::Decode(message.into())
    }
}

fn is_unknown_session_message(message: &str) -> bool {
    message.to_ascii_lowercase().contains("unknown session")
}

/// Classifies a tserver `AppStatusPB` (the `status` field of every
/// PgClientService response) into the retry taxonomy, covering the codes
/// that surface through Perform.
pub(crate) fn check_app_status(status: Option<&AppStatusPb>, what: &str) -> Result<(), YbError> {
    use app_status_pb::ErrorCode;
    let Some(status) = status else {
        return Ok(());
    };
    if status.code == ErrorCode::Ok as i32 {
        return Ok(());
    }
    let message = format!(
        "{what} failed: {:?} {}",
        status.code(),
        status.message.as_deref().unwrap_or("")
    );
    let class = match status.code() {
        ErrorCode::NetworkError | ErrorCode::TimedOut | ErrorCode::Aborted => ErrorClass::Network,
        ErrorCode::TryAgainCode
        | ErrorCode::ServiceUnavailable
        | ErrorCode::Busy
        | ErrorCode::LeaderNotReadyToServe
        | ErrorCode::LeaderHasNoLease
        | ErrorCode::TabletSplit
        | ErrorCode::ShutdownInProgress => ErrorClass::TryAgain,
        ErrorCode::NotSupported => ErrorClass::Invalid,
        // A lost session surfaces as InvalidArgument "Connection
        // terminated unexpectedly due to unknown session N" (verified
        // live against 2025.2.3.0); it means the server-side session
        // expired and must be re-established — same remedy as a
        // connection drop, so classify it as Network (see
        // `YbError::is_session_lost`).
        ErrorCode::InvalidArgument | ErrorCode::NotFound
            if status
                .message
                .as_deref()
                .is_some_and(is_unknown_session_message) =>
        {
            ErrorClass::Network
        }
        ErrorCode::InvalidArgument => ErrorClass::Invalid,
        _ => ErrorClass::Other,
    };
    Err(YbError::Call { class, message })
}

/// Classifies a per-op `PgsqlResponsePB` status.
pub(crate) fn check_pgsql_response(r: &PgsqlResponsePb, what: &str) -> Result<(), YbError> {
    use pgsql_response_pb::RequestStatus;
    let message = || {
        format!(
            "{what} op failed: {:?} {}",
            r.status(),
            r.error_message.as_deref().unwrap_or("")
        )
    };
    match r.status() {
        RequestStatus::PgsqlStatusOk => Ok(()),
        RequestStatus::PgsqlStatusSchemaVersionMismatch => Err(YbError::Call {
            class: ErrorClass::SchemaMismatch,
            message: message(),
        }),
        RequestStatus::PgsqlStatusRestartRequiredError => Err(YbError::Call {
            class: ErrorClass::ReadRestart,
            message: message(),
        }),
        _ => Err(YbError::other(message())),
    }
}

/// Classifies an RPC-layer error frame (`ResponseHeader.is_error` →
/// `ErrorStatusPB`). These are rare: the service/method is registered on
/// every tserver, so in practice only overload and shutdown show up.
pub(crate) fn classify_rpc_error(code: RpcErrorCodePb, message: &str) -> YbError {
    let text = format!("RPC error ({code:?}): {message}");
    let class = match code {
        RpcErrorCodePb::ErrorServerTooBusy => ErrorClass::TryAgain,
        RpcErrorCodePb::FatalServerShuttingDown => ErrorClass::Network,
        RpcErrorCodePb::ErrorInvalidRequest
        | RpcErrorCodePb::ErrorNoSuchMethod
        | RpcErrorCodePb::ErrorNoSuchService => ErrorClass::Invalid,
        _ => ErrorClass::Other,
    };
    YbError::Call {
        class,
        message: text,
    }
}

#[cfg(test)]
mod tests {
    use super::*;

    fn app_status(code: app_status_pb::ErrorCode, message: &str) -> AppStatusPb {
        AppStatusPb {
            code: code as i32,
            message: Some(message.to_string()),
            ..Default::default()
        }
    }

    #[test]
    fn app_status_classes() {
        use app_status_pb::ErrorCode;
        let class = |code, msg: &str| {
            check_app_status(Some(&app_status(code, msg)), "t")
                .unwrap_err()
                .class()
        };
        assert!(check_app_status(None, "t").is_ok());
        assert!(check_app_status(Some(&app_status(ErrorCode::Ok, "")), "t").is_ok());
        assert_eq!(class(ErrorCode::TryAgainCode, "x"), ErrorClass::TryAgain);
        assert_eq!(class(ErrorCode::Busy, "x"), ErrorClass::TryAgain);
        assert_eq!(
            class(ErrorCode::LeaderNotReadyToServe, "x"),
            ErrorClass::TryAgain
        );
        assert_eq!(class(ErrorCode::NetworkError, "x"), ErrorClass::Network);
        assert_eq!(class(ErrorCode::TimedOut, "x"), ErrorClass::Network);
        assert_eq!(class(ErrorCode::InvalidArgument, "x"), ErrorClass::Invalid);
        assert_eq!(class(ErrorCode::IllegalState, "x"), ErrorClass::Other);
    }

    #[test]
    fn unknown_session_is_network_and_flagged() {
        // The exact message observed live against YB 2025.2.3.0.
        let err = check_app_status(
            Some(&app_status(
                app_status_pb::ErrorCode::InvalidArgument,
                "Connection terminated unexpectedly due to unknown session 42",
            )),
            "Perform",
        )
        .unwrap_err();
        assert_eq!(err.class(), ErrorClass::Network);
        assert!(err.is_session_lost());
        assert!(err.is_retryable());

        let plain = check_app_status(
            Some(&app_status(
                app_status_pb::ErrorCode::InvalidArgument,
                "bad column",
            )),
            "Perform",
        )
        .unwrap_err();
        assert_eq!(plain.class(), ErrorClass::Invalid);
        assert!(!plain.is_session_lost());
    }

    #[test]
    fn timeouts_are_network_class_and_flagged() {
        // The exact shapes connection.rs produces.
        let call = YbError::network(
            "yb.tserver.PgClientService.Perform to 10.0.0.1:9100 timed out after 10s",
        );
        assert!(call.is_timeout());
        assert!(call.is_retryable());
        let connect = YbError::network("connect 10.0.0.1:9100: timed out");
        assert!(connect.is_timeout());
        // Other network failures are not timeouts...
        assert!(!YbError::network("connection closed: read reset").is_timeout());
        // ...and non-network errors never are.
        assert!(!YbError::other("Perform failed: TimedOut sub-op timed out").is_timeout());
    }

    #[test]
    fn pgsql_response_classes() {
        use pgsql_response_pb::RequestStatus;
        let resp = |status: RequestStatus| PgsqlResponsePb {
            status: Some(status as i32),
            ..Default::default()
        };
        assert!(check_pgsql_response(&resp(RequestStatus::PgsqlStatusOk), "t").is_ok());
        assert_eq!(
            check_pgsql_response(&resp(RequestStatus::PgsqlStatusSchemaVersionMismatch), "t")
                .unwrap_err()
                .class(),
            ErrorClass::SchemaMismatch
        );
        assert_eq!(
            check_pgsql_response(&resp(RequestStatus::PgsqlStatusRestartRequiredError), "t")
                .unwrap_err()
                .class(),
            ErrorClass::ReadRestart
        );
        assert_eq!(
            check_pgsql_response(&resp(RequestStatus::PgsqlStatusRuntimeError), "t")
                .unwrap_err()
                .class(),
            ErrorClass::Other
        );
    }
}
