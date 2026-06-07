//! WAL receiver connection manager: keeps a single libpq streaming
//! connection to the safekeeper alive, reconnecting with exponential
//! backoff on disconnects / errors.

use std::ops::ControlFlow;
use std::sync::Arc;

use anyhow::Context;
use chrono::{NaiveDateTime, Utc};
use pageserver_api::models::TimelineState;
use postgres_connection::PgConnectionConfig;
use tokio_util::sync::CancellationToken;
use tracing::*;
use utils::id::{NodeId, TenantTimelineId};
use utils::postgres_client::{ConnectionConfigArgs, wal_stream_connection_config};

use super::walreceiver_connection::{WalConnectionStatus, WalReceiverError};
use super::{TaskEvent, TaskHandle, TaskStateUpdate, WalReceiverConf};
use crate::context::{DownloadBehavior, RequestContext};
use crate::metrics::{WALRECEIVER_ACTIVE_MANAGERS, WALRECEIVER_SWITCHES};
use crate::task_mgr::TaskKind;
use crate::tenant::{Timeline, debug_assert_current_span_has_tenant_and_timeline_id};

pub(crate) struct Cancelled;

/// `NodeId` used to label the safekeeper in metrics and logs. Single-SK
/// mode has no real node-id selection happening, so we synthesize a
/// fixed one.
const SAFEKEEPER_NODE_ID: NodeId = NodeId(1);

const RECONNECT_MIN_BACKOFF_SECONDS: f64 = 0.1;
const RECONNECT_MAX_BACKOFF_SECONDS: f64 = 15.0;
const RECONNECT_BACKOFF_MULTIPLIER: f64 = 1.5;

/// Runs one iteration of the connection-manager loop: waits for the
/// timeline to become active, then drives a single WAL streaming
/// connection until it disconnects or a state-transition cancels it.
///
/// # Cancel-Safety
///
/// Not cancellation-safe. Use `cancel` token to request cancellation.
pub(super) async fn connection_manager_loop_step(
    connection_manager_state: &mut ConnectionManagerState,
    ctx: &RequestContext,
    cancel: &CancellationToken,
    manager_status: &std::sync::RwLock<Option<ConnectionManagerStatus>>,
) -> Result<(), Cancelled> {
    match tokio::select! {
        _ = cancel.cancelled() => { return Err(Cancelled); },
        st = connection_manager_state.timeline.wait_to_become_active(ctx) => { st }
    } {
        Ok(()) => {}
        Err(new_state) => {
            debug!(
                ?new_state,
                "state changed, stopping wal connection manager loop"
            );
            return Err(Cancelled);
        }
    }

    WALRECEIVER_ACTIVE_MANAGERS.inc();
    scopeguard::defer! {
        WALRECEIVER_ACTIVE_MANAGERS.dec();
    }

    let mut timeline_state_updates = connection_manager_state
        .timeline
        .subscribe_for_state_updates();

    loop {
        // Make sure we have an active connection (or are waiting on a
        // retry backoff). `ensure_connection` returns immediately if a
        // connection is already running.
        connection_manager_state.ensure_connection(ctx).await;
        *manager_status.write().unwrap() = Some(connection_manager_state.manager_status());

        // NB: each select arm must be cancellation-safe.
        tokio::select! {
            _ = cancel.cancelled() => { return Err(Cancelled); }

            Some(wal_connection_update) = async {
                match connection_manager_state.wal_connection.as_mut() {
                    Some(wal_connection) => Some(wal_connection.connection_task.next_task_event().await),
                    None => None,
                }
            } => {
                let wal_connection = connection_manager_state.wal_connection.as_mut()
                    .expect("Some(_) checked by the select guard");
                match wal_connection_update {
                    TaskEvent::Update(TaskStateUpdate::Started) => {},
                    TaskEvent::Update(TaskStateUpdate::Progress(new_status)) => {
                        if new_status.has_processed_wal {
                            // Successful WAL progress: reset the backoff so a
                            // future disconnect retries quickly.
                            connection_manager_state.retry.reset();
                        }
                        wal_connection.status = new_status;
                    }
                    TaskEvent::End(walreceiver_task_result) => {
                        match walreceiver_task_result {
                            Ok(()) => debug!("WAL receiving task finished"),
                            Err(e) => error!("wal receiver task finished with an error: {e:?}"),
                        }
                        connection_manager_state.drop_connection(false).await;
                    },
                }
            },

            new_event = async {
                loop {
                    if connection_manager_state.timeline.current_state() == TimelineState::Loading {
                        warn!("wal connection manager should only be launched after timeline has become active");
                    }
                    match timeline_state_updates.changed().await {
                        Ok(()) => {
                            let new_state = connection_manager_state.timeline.current_state();
                            match new_state {
                                TimelineState::Active => continue,
                                TimelineState::Broken { .. } | TimelineState::Stopping => {
                                    debug!("timeline entered terminal state {new_state:?}, stopping wal connection manager loop");
                                    return ControlFlow::Break(());
                                }
                                TimelineState::Loading => {
                                    warn!("timeline transitioned back to Loading state, that should not happen");
                                    return ControlFlow::Continue(());
                                }
                            }
                        }
                        Err(_sender_dropped_error) => return ControlFlow::Break(()),
                    }
                }
            } => match new_event {
                ControlFlow::Continue(()) => {
                    return Ok(());
                }
                ControlFlow::Break(()) => {
                    debug!("Timeline is no longer active, stopping wal connection manager loop");
                    return Err(Cancelled);
                }
            },
        }

        *manager_status.write().unwrap() = Some(connection_manager_state.manager_status());
    }
}

/// State for a single timeline's WAL streaming connection.
pub(super) struct ConnectionManagerState {
    timeline: Arc<Timeline>,
    /// Child token of [`super::WalReceiver::cancel`], inherited to all tasks we spawn.
    cancel: CancellationToken,
    conf: WalReceiverConf,
    /// Resolved libpq connection target for the single safekeeper.
    wal_source_connconf: PgConnectionConfig,
    /// Currently active WAL streaming connection, if any.
    wal_connection: Option<WalConnection>,
    /// Backoff for reconnect attempts.
    retry: Backoff,
}

/// Status snapshot exposed via the HTTP mgmt API
/// (`Timeline::walreceiver_status`).
#[derive(Debug, Clone)]
pub struct ConnectionManagerStatus {
    existing_connection: Option<WalConnectionStatus>,
    safekeeper_addr: String,
}

impl ConnectionManagerStatus {
    /// Generates a string describing the connection status in a form
    /// suitable for logging / HTTP responses.
    pub fn to_human_readable_string(&self) -> String {
        let mut resulting_string = String::new();
        match &self.existing_connection {
            Some(connection) => {
                if connection.has_processed_wal {
                    resulting_string.push_str(&format!(
                        " (update {}): streaming WAL from node {}, ",
                        connection.latest_wal_update.format("%Y-%m-%d %H:%M:%S"),
                        connection.node,
                    ));

                    match (connection.streaming_lsn, connection.commit_lsn) {
                        (None, None) => resulting_string.push_str("no streaming data"),
                        (None, Some(commit_lsn)) => {
                            resulting_string.push_str(&format!("commit Lsn: {commit_lsn}"))
                        }
                        (Some(streaming_lsn), None) => {
                            resulting_string.push_str(&format!("streaming Lsn: {streaming_lsn}"))
                        }
                        (Some(streaming_lsn), Some(commit_lsn)) => resulting_string.push_str(
                            &format!("commit|streaming Lsn: {commit_lsn}|{streaming_lsn}"),
                        ),
                    }
                } else if connection.is_connected {
                    resulting_string.push_str(&format!(
                        " (update {}): connecting to node {}",
                        connection
                            .latest_connection_update
                            .format("%Y-%m-%d %H:%M:%S"),
                        connection.node,
                    ));
                } else {
                    resulting_string.push_str(&format!(
                        " (update {}): initializing node {} connection",
                        connection
                            .latest_connection_update
                            .format("%Y-%m-%d %H:%M:%S"),
                        connection.node,
                    ));
                }
            }
            None => resulting_string.push_str(": disconnected"),
        }

        resulting_string.push_str(&format!(", safekeeper: {}", self.safekeeper_addr));
        resulting_string
    }
}

#[derive(Debug)]
struct WalConnection {
    status: WalConnectionStatus,
    connection_task: TaskHandle<WalConnectionStatus>,
}

/// Exponential backoff state for reconnect attempts.
#[derive(Debug)]
struct Backoff {
    /// Earliest absolute time at which the next connection attempt is permitted.
    next_attempt_at: Option<NaiveDateTime>,
    duration_seconds: f64,
}

impl Backoff {
    fn new() -> Self {
        Self {
            next_attempt_at: None,
            duration_seconds: RECONNECT_MIN_BACKOFF_SECONDS,
        }
    }

    /// Reset the backoff state, used after we successfully process WAL.
    fn reset(&mut self) {
        self.next_attempt_at = None;
        self.duration_seconds = RECONNECT_MIN_BACKOFF_SECONDS;
    }

    /// Record a connection attempt that began at `started_at`, scheduling
    /// the next earliest retry and growing the backoff.
    fn record_attempt(&mut self, started_at: NaiveDateTime) {
        self.next_attempt_at = started_at.checked_add_signed(chrono::Duration::milliseconds(
            (self.duration_seconds * 1000.0) as i64,
        ));
        let next = self.duration_seconds * RECONNECT_BACKOFF_MULTIPLIER;
        self.duration_seconds = next
            .min(RECONNECT_MAX_BACKOFF_SECONDS)
            .max(RECONNECT_MIN_BACKOFF_SECONDS);
    }

    /// How long to wait before the next attempt, or `None` if we may try
    /// immediately.
    fn time_until_next_attempt(&self) -> Option<std::time::Duration> {
        let next = self.next_attempt_at?;
        let now = Utc::now().naive_utc();
        (next - now).to_std().ok()
    }
}

impl ConnectionManagerState {
    pub(super) fn new(
        timeline: Arc<Timeline>,
        conf: WalReceiverConf,
        wal_source_connconf: PgConnectionConfig,
        cancel: CancellationToken,
    ) -> Self {
        Self {
            timeline,
            cancel,
            conf,
            wal_source_connconf,
            wal_connection: None,
            retry: Backoff::new(),
        }
    }

    /// Build the resolved `PgConnectionConfig` for the configured
    /// safekeeper. `safekeeper_pg_addr` is the `host:port` form
    /// published by neon_local's pageserver config / the safekeeper's
    /// own `listen_pg_addr`.
    pub(super) fn build_wal_source_connconf(
        timeline: &Timeline,
        conf: &WalReceiverConf,
        safekeeper_pg_addr: &str,
    ) -> anyhow::Result<PgConnectionConfig> {
        let ttid = TenantTimelineId {
            tenant_id: timeline.tenant_id,
            timeline_id: timeline.timeline_id,
        };
        let connection_conf_args = ConnectionConfigArgs {
            protocol: conf.protocol,
            ttid,
            // Single-shard yb-mode: carry the unsharded constants.
            shard_number: Some(0),
            shard_count: Some(0),
            shard_stripe_size: Some(0),
            listen_pg_addr_str: safekeeper_pg_addr,
            auth_token: conf.auth_token.as_ref().map(|t| t.as_str()),
            availability_zone: conf.availability_zone.as_deref(),
        };
        wal_stream_connection_config(connection_conf_args)
            .with_context(|| format!("build wal source connstr for {safekeeper_pg_addr}"))
    }

    /// If we don't have an active connection, sleep until the backoff
    /// permits and then spawn one. No-op if a connection task is
    /// already running.
    async fn ensure_connection(&mut self, ctx: &RequestContext) {
        if self.wal_connection.is_some() {
            return;
        }
        if let Some(wait) = self.retry.time_until_next_attempt() {
            tokio::select! {
                _ = tokio::time::sleep(wait) => {}
                _ = self.cancel.cancelled() => return,
            }
        }
        self.spawn_connection(ctx);
    }

    fn spawn_connection(&mut self, ctx: &RequestContext) {
        WALRECEIVER_SWITCHES.with_label_values(&["reconnect"]).inc();

        let started_at = Utc::now().naive_utc();
        self.retry.record_attempt(started_at);

        let connect_timeout = self.conf.wal_connect_timeout;
        let ingest_batch_size = self.conf.ingest_batch_size;
        let protocol = self.conf.protocol;
        let validate_wal_contiguity = self.conf.validate_wal_contiguity;
        let timeline = Arc::clone(&self.timeline);
        let wal_source_connconf = self.wal_source_connconf.clone();
        let ctx = ctx.detached_child(
            TaskKind::WalReceiverConnectionHandler,
            DownloadBehavior::Download,
        );
        let sk_id = SAFEKEEPER_NODE_ID;
        let span = info_span!("connection", node_id = %sk_id);

        let connection_handle =
            TaskHandle::spawn(&self.cancel, move |events_sender, cancellation| {
                async move {
                    debug_assert_current_span_has_tenant_and_timeline_id();

                    let res = super::walreceiver_connection::handle_walreceiver_connection(
                        timeline,
                        protocol,
                        wal_source_connconf,
                        events_sender,
                        cancellation.clone(),
                        connect_timeout,
                        ctx,
                        sk_id,
                        ingest_batch_size,
                        validate_wal_contiguity,
                    )
                    .await;

                    match res {
                        Ok(()) => Ok(()),
                        Err(e) => match e {
                            WalReceiverError::SuccessfulCompletion(msg) => {
                                info!("walreceiver connection handling ended with success: {msg}");
                                Ok(())
                            }
                            WalReceiverError::ExpectedSafekeeperError(e) => {
                                info!("walreceiver connection handling ended: {e}");
                                Ok(())
                            }
                            WalReceiverError::ClosedGate => {
                                info!(
                                    "walreceiver connection handling ended because of closed gate"
                                );
                                Ok(())
                            }
                            WalReceiverError::Cancelled => Ok(()),
                            WalReceiverError::Other(e) => {
                                if cancellation.is_cancelled() {
                                    Ok(())
                                } else {
                                    Err(e).context("walreceiver connection handling failure")
                                }
                            }
                        },
                    }
                }
                .instrument(span)
            });

        self.wal_connection = Some(WalConnection {
            status: WalConnectionStatus {
                is_connected: false,
                has_processed_wal: false,
                latest_connection_update: started_at,
                latest_wal_update: started_at,
                streaming_lsn: None,
                commit_lsn: None,
                node: sk_id,
            },
            connection_task: connection_handle,
        });
    }

    /// Drops the active connection (if any). If `needs_shutdown` is true,
    /// also cancels and awaits the task.
    ///
    /// # Cancel-Safety
    ///
    /// Not cancellation-safe.
    async fn drop_connection(&mut self, needs_shutdown: bool) {
        let Some(wal_connection) = self.wal_connection.take() else {
            return;
        };
        if needs_shutdown {
            wal_connection.connection_task.shutdown().await;
        }
    }

    /// # Cancel-Safety
    ///
    /// Not cancellation-safe.
    pub(super) async fn shutdown(mut self) {
        if let Some(wal_connection) = self.wal_connection.take() {
            wal_connection.connection_task.shutdown().await;
        }
    }

    fn manager_status(&self) -> ConnectionManagerStatus {
        ConnectionManagerStatus {
            existing_connection: self.wal_connection.as_ref().map(|conn| conn.status),
            safekeeper_addr: format!("{:?}", self.wal_source_connconf),
        }
    }
}
