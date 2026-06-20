use std::fmt::Display;
use std::sync::Arc;
use std::time::Duration;

use anyhow::{Context, anyhow};
use bytes::BytesMut;
use futures::StreamExt;
use futures::future::Either;
use postgres_backend::{CopyStreamHandlerEnd, PostgresBackend};
use postgres_ffi::waldecoder::{WalDecodeError, WalStreamDecoder};
use postgres_ffi::{PgMajorVersion, get_current_timestamp};
use pq_proto::{BeMessage, InterpretedWalRecordsBody, WalSndKeepAlive};
use tokio::io::{AsyncRead, AsyncWrite};
use tokio::sync::mpsc::error::SendError;
use tokio::task::JoinHandle;
use tokio::time::MissedTickBehavior;
use tracing::{Instrument, error, info, info_span};
use utils::lsn::Lsn;
use utils::postgres_client::{Compression, InterpretedFormat};
use wal_decoder::models::{InterpretedWalRecord, InterpretedWalRecords};

use crate::metrics::WAL_READERS;
use crate::send_wal::{EndWatchView, WalSenderGuard};
use crate::timeline::WalResidentTimeline;
use crate::wal_reader_stream::{StreamingWalReader, WalBytes};

/// Identifier used to differentiate between senders connected to the same
/// reader.
///
/// In the steady state there's only one, but two pageservers may
/// temporarily have the timeline attached and attempt to ingest WAL for it.
#[derive(Hash, Eq, PartialEq, Copy, Clone)]
struct SenderId(u8);

impl SenderId {
    fn first() -> Self {
        SenderId(0)
    }

    fn next(&self) -> Self {
        SenderId(self.0.checked_add(1).expect("few senders"))
    }
}

impl Display for SenderId {
    fn fmt(&self, f: &mut std::fmt::Formatter<'_>) -> std::fmt::Result {
        write!(f, "{}", self.0)
    }
}

/// Fan-out interpreted record reader.
/// Reads WAL from disk, decodes it, interprets it, and sends
/// it to any [`InterpretedWalSender`] connected to it.
pub(crate) struct InterpretedWalReader {
    wal_stream: StreamingWalReader,
    senders: smallvec::SmallVec<[SenderState; 1]>,
    sender_notification_rx: Option<tokio::sync::mpsc::UnboundedReceiver<AttachSenderNotification>>,
    state: Arc<std::sync::RwLock<InterpretedWalReaderState>>,
    pg_version: PgMajorVersion,
}

/// A handle for [`InterpretedWalReader`] which allows for interacting with it
/// when it runs as a separate tokio task.
#[derive(Debug)]
pub(crate) struct InterpretedWalReaderHandle {
    join_handle: JoinHandle<Result<(), InterpretedWalReaderError>>,
    state: Arc<std::sync::RwLock<InterpretedWalReaderState>>,
    sender_notification_tx: tokio::sync::mpsc::UnboundedSender<AttachSenderNotification>,
}

struct SenderState {
    sender_id: SenderId,
    tx: tokio::sync::mpsc::Sender<Batch>,
    next_record_lsn: Lsn,
}

/// State of [`InterpretedWalReader`] visible outside of the task running it.
#[derive(Debug)]
pub(crate) enum InterpretedWalReaderState {
    Running {
        current_position: Lsn,
        /// Tracks the start of the PG WAL LSN from which the current batch of
        /// interpreted records originated.
        current_batch_wal_start: Option<Lsn>,
    },
    Done,
}

pub(crate) struct Batch {
    wal_end_lsn: Lsn,
    available_wal_end_lsn: Lsn,
    records: InterpretedWalRecords,
}

#[derive(thiserror::Error, Debug)]
pub enum InterpretedWalReaderError {
    /// Handler initiates the end of streaming.
    #[error("decode error: {0}")]
    Decode(#[from] WalDecodeError),
    #[error("read or interpret error: {0}")]
    ReadOrInterpret(#[from] anyhow::Error),
    #[error("wal stream closed")]
    WalStreamClosed,
}

enum CurrentPositionUpdate {
    Reset { from: Lsn, to: Lsn },
    NotReset(Lsn),
}

impl CurrentPositionUpdate {
    fn current_position(&self) -> Lsn {
        match self {
            CurrentPositionUpdate::Reset { from: _, to } => *to,
            CurrentPositionUpdate::NotReset(lsn) => *lsn,
        }
    }

    fn previous_position(&self) -> Lsn {
        match self {
            CurrentPositionUpdate::Reset { from, to: _ } => *from,
            CurrentPositionUpdate::NotReset(lsn) => *lsn,
        }
    }
}

impl InterpretedWalReaderState {
    fn current_position(&self) -> Option<Lsn> {
        match self {
            InterpretedWalReaderState::Running {
                current_position, ..
            } => Some(*current_position),
            InterpretedWalReaderState::Done => None,
        }
    }

    // Reset the current position of the WAL reader if the requested starting position
    // of the new sender is smaller than the current value.
    fn maybe_reset(&mut self, new_sender_start_pos: Lsn) -> CurrentPositionUpdate {
        match self {
            InterpretedWalReaderState::Running {
                current_position,
                current_batch_wal_start,
            } => {
                if new_sender_start_pos < *current_position {
                    let from = *current_position;
                    *current_position = new_sender_start_pos;
                    *current_batch_wal_start = None;
                    CurrentPositionUpdate::Reset {
                        from,
                        to: *current_position,
                    }
                } else {
                    // Edge case: The new sender is at the same current position as
                    // the reader. Note that the current position is WAL record aligned,
                    // so the reader might have done some partial reads and updated the
                    // batch start. If that's the case, adjust the batch start to match
                    // the starting position of the new sender. It can lead to some senders
                    // seeing overlaps, but in that case the actual record LSNs are checked
                    // which should be fine based on the filtering logic.
                    if let Some(start) = current_batch_wal_start {
                        *start = std::cmp::min(*start, new_sender_start_pos);
                    }
                    CurrentPositionUpdate::NotReset(*current_position)
                }
            }
            InterpretedWalReaderState::Done => {
                panic!("maybe_reset called on finished reader")
            }
        }
    }

    fn update_current_batch_wal_start(&mut self, lsn: Lsn) {
        match self {
            InterpretedWalReaderState::Running {
                current_batch_wal_start,
                ..
            } => {
                if current_batch_wal_start.is_none() {
                    *current_batch_wal_start = Some(lsn);
                }
            }
            InterpretedWalReaderState::Done => {
                panic!("update_current_batch_wal_start called on finished reader")
            }
        }
    }

    fn replace_current_batch_wal_start(&mut self, with: Lsn) -> Lsn {
        match self {
            InterpretedWalReaderState::Running {
                current_batch_wal_start,
                ..
            } => current_batch_wal_start.replace(with).unwrap(),
            InterpretedWalReaderState::Done => {
                panic!("take_current_batch_wal_start called on finished reader")
            }
        }
    }

    fn update_current_position(&mut self, lsn: Lsn) {
        match self {
            InterpretedWalReaderState::Running {
                current_position, ..
            } => {
                *current_position = lsn;
            }
            InterpretedWalReaderState::Done => {
                panic!("update_current_position called on finished reader")
            }
        }
    }
}

pub(crate) struct AttachSenderNotification {
    sender: tokio::sync::mpsc::Sender<Batch>,
    start_pos: Lsn,
}

impl InterpretedWalReader {
    /// Spawn the reader in a separate tokio task and return a handle
    pub(crate) fn spawn(
        wal_stream: StreamingWalReader,
        start_pos: Lsn,
        tx: tokio::sync::mpsc::Sender<Batch>,
        pg_version: PgMajorVersion,
        appname: &Option<String>,
    ) -> InterpretedWalReaderHandle {
        let state = Arc::new(std::sync::RwLock::new(InterpretedWalReaderState::Running {
            current_position: start_pos,
            current_batch_wal_start: None,
        }));

        let (sender_notification_tx, sender_notification_rx) =
            tokio::sync::mpsc::unbounded_channel();

        let ttid = wal_stream.ttid;

        let reader = InterpretedWalReader {
            wal_stream,
            senders: smallvec::smallvec![SenderState {
                sender_id: SenderId::first(),
                tx,
                next_record_lsn: start_pos,
            }],
            sender_notification_rx: Some(sender_notification_rx),
            state: state.clone(),
            pg_version,
        };

        let metric = WAL_READERS
            .get_metric_with_label_values(&["task", appname.as_deref().unwrap_or("safekeeper")])
            .unwrap();

        let join_handle = tokio::task::spawn(
            async move {
                metric.inc();
                scopeguard::defer! {
                    metric.dec();
                }

                reader
                    .run_impl(start_pos)
                    .await
                    .inspect_err(|err| match err {
                        // Recoverable in yb-light: a decode error is almost
                        // always a transient torn read across a WAL
                        // rewrite/truncation window, not durable corruption.
                        // Exit cleanly so the client reconnects and re-reads
                        // from YB rather than wedging the cluster via
                        // `critical_timeline!` (panic in debug / corruption
                        // flag in release). A sustained metric rate is the
                        // real-corruption signal. See the `run()` arm and #139.
                        InterpretedWalReaderError::Decode(_) => {
                            crate::metrics::WAL_READER_DECODE_ERRORS.inc();
                            error!(
                                "interpreted wal reader: decode error (tenant {}, timeline {}; \
                                 recoverable — reconnect will re-read from YB): {err:?}",
                                ttid.tenant_id, ttid.timeline_id
                            );
                        }
                        err => error!("failed to read WAL record: {err}"),
                    })
            }
            .instrument(info_span!("interpreted wal reader")),
        );

        InterpretedWalReaderHandle {
            join_handle,
            state,
            sender_notification_tx,
        }
    }

    /// Construct the reader without spawning anything
    /// Callers should drive the future returned by [`Self::run`].
    pub(crate) fn new(
        wal_stream: StreamingWalReader,
        start_pos: Lsn,
        tx: tokio::sync::mpsc::Sender<Batch>,
        pg_version: PgMajorVersion,
        sender_notification_rx: Option<
            tokio::sync::mpsc::UnboundedReceiver<AttachSenderNotification>,
        >,
    ) -> InterpretedWalReader {
        let state = Arc::new(std::sync::RwLock::new(InterpretedWalReaderState::Running {
            current_position: start_pos,
            current_batch_wal_start: None,
        }));

        InterpretedWalReader {
            wal_stream,
            senders: smallvec::smallvec![SenderState {
                sender_id: SenderId::first(),
                tx,
                next_record_lsn: start_pos,
            }],
            sender_notification_rx,
            state: state.clone(),
            pg_version,
        }
    }

    /// Entry point for future (polling) based wal reader.
    pub(crate) async fn run(
        self,
        start_pos: Lsn,
        appname: &Option<String>,
    ) -> Result<(), CopyStreamHandlerEnd> {
        let metric = WAL_READERS
            .get_metric_with_label_values(&["future", appname.as_deref().unwrap_or("safekeeper")])
            .unwrap();

        metric.inc();
        scopeguard::defer! {
            metric.dec();
        }

        let ttid = self.wal_stream.ttid;
        let cause = match self.run_impl(start_pos).await {
            Err(err @ InterpretedWalReaderError::Decode(_)) => {
                // A WAL *decode* error here is, in yb-light, almost always a
                // TRANSIENT torn read rather than durable corruption: the WAL
                // read path blends the in-memory `WalCache` with YB
                // `safekeeper.wals` under concurrent writes, so a record can
                // briefly span bytes from two write epochs across a WAL
                // rewrite/truncation window (manifesting as an "invalid xlog
                // page header" at a page boundary). Upstream Neon reads
                // immutable object-storage WAL and so escalates a decode error
                // to `critical_timeline!` (which panics in debug and flags
                // corruption in release) — too aggressive here: it turns a
                // recoverable hiccup into a cluster-wide wedge (the WAL sender
                // dies, streaming to the pageserver stalls, every GetPage
                // blocks in wait_lsn). Instead exit the reader cleanly and
                // loudly; the pageserver walreceiver reconnects and re-reads
                // from its last applied (record-boundary) LSN, re-fetching
                // authoritative bytes from YB, which self-heals the transient
                // case. A SUSTAINED rate on the metric is the real-corruption
                // signal (#139).
                crate::metrics::WAL_READER_DECODE_ERRORS.inc();
                error!(
                    "interpreted wal reader: decode error at start_pos={start_pos} \
                     (tenant {}, timeline {}; recoverable — reconnect will re-read from YB): {err:?}",
                    ttid.tenant_id, ttid.timeline_id
                );
                format!("decode error at start_pos={start_pos}: {err}")
            }
            Err(err) => {
                // Log the underlying cause loudly — without it, callers (e.g. the
                // pageserver walreceiver) only see the generic "interpreted wal
                // reader finished" wrapper message and have no idea whether it
                // was an I/O error from YB, a missing WAL segment, a closed
                // stream, etc. See findings-ps-wal-replay-restart-bug-2026-06-04.
                //
                // {err:?} dumps the full anyhow source chain — Display alone
                // truncates to the outermost `.with_context()` and hides the
                // YB / IO root cause.
                error!(
                    "interpreted wal reader exiting with error at start_pos={start_pos}: {err:?}"
                );
                // Flatten the chain into the propagated message so the
                // pageserver-side error carries the root cause too.
                let chain: Vec<String> = match &err {
                    InterpretedWalReaderError::ReadOrInterpret(e) => {
                        e.chain().map(|c| c.to_string()).collect()
                    }
                    _ => vec![err.to_string()],
                };
                format!(
                    "{err} (start_pos={start_pos}, chain=[{}])",
                    chain.join(" / ")
                )
            }
            Ok(()) => {
                info!("interpreted wal reader exiting at start_pos={start_pos}");
                format!("ok (start_pos={start_pos})")
            }
        };

        Err(CopyStreamHandlerEnd::Other(anyhow!(
            "interpreted wal reader finished: {cause}"
        )))
    }

    /// Send interpreted WAL to one or more [`InterpretedWalSender`]s
    /// Stops when an error is encountered or when the [`InterpretedWalReaderHandle`]
    /// goes out of scope.
    async fn run_impl(mut self, start_pos: Lsn) -> Result<(), InterpretedWalReaderError> {
        let defer_state = self.state.clone();
        scopeguard::defer! {
            *defer_state.write().unwrap() = InterpretedWalReaderState::Done;
        }

        let mut wal_decoder = WalStreamDecoder::new(start_pos, self.pg_version);

        loop {
            tokio::select! {
                // Main branch for reading WAL and forwarding it
                wal_or_reset = self.wal_stream.next() => {
                    let wal = wal_or_reset.map(|wor| wor.get_wal().expect("reset handled in select branch below"));
                    let WalBytes {
                        wal,
                        wal_start_lsn,
                        wal_end_lsn,
                        available_wal_end_lsn,
                    } = match wal {
                        Some(some) => some.map_err(InterpretedWalReaderError::ReadOrInterpret)?,
                        None => {
                            // [`StreamingWalReader::next`] is an endless stream of WAL.
                            // It shouldn't ever finish unless it panicked or became internally
                            // inconsistent.
                            return Result::Err(InterpretedWalReaderError::WalStreamClosed);
                        }
                    };

                    self.state.write().unwrap().update_current_batch_wal_start(wal_start_lsn);

                    wal_decoder.feed_bytes(&wal);

                    // Deserialize and interpret WAL records from this batch of WAL.
                    let mut records = Vec::new();
                    let mut max_next_record_lsn = None;
                    let mut max_end_record_lsn = None;
                    while let Some((next_record_lsn, recdata)) = wal_decoder.poll_decode()?
                    {
                        assert!(next_record_lsn.is_aligned());
                        max_next_record_lsn = Some(next_record_lsn);
                        max_end_record_lsn = Some(wal_decoder.lsn());

                        let interpreted = InterpretedWalRecord::from_bytes(
                            recdata,
                            next_record_lsn,
                            self.pg_version,
                        )
                        .with_context(|| "Failed to interpret WAL")?;

                        records.push(interpreted);
                    }

                    let max_next_record_lsn = match max_next_record_lsn {
                        Some(lsn) => lsn,
                        None => {
                            continue;
                        }
                    };

                    // Update the current position such that new receivers can decide
                    // whether to attach to us or spawn a new WAL reader.
                    let batch_wal_start_lsn = {
                        let mut guard = self.state.write().unwrap();
                        guard.update_current_position(max_next_record_lsn);
                        guard.replace_current_batch_wal_start(max_end_record_lsn.unwrap())
                    };

                    // Send interpreted records downstream. Anything a sender has
                    // already seen is filtered out by its `next_record_lsn`.
                    let mut senders_to_remove = Vec::new();
                    let sender_count = self.senders.len();
                    for (idx, state) in self.senders.iter_mut().enumerate() {
                        let batch = if max_next_record_lsn > state.next_record_lsn {
                            // This batch contains at least one record this sender has
                            // not seen yet. The last sender can take ownership of the
                            // records; earlier ones get a clone.
                            let records = if idx == sender_count - 1 {
                                std::mem::take(&mut records)
                            } else {
                                records.clone()
                            };

                            InterpretedWalRecords {
                                records,
                                next_record_lsn: max_next_record_lsn,
                                raw_wal_start_lsn: Some(batch_wal_start_lsn),
                            }
                        } else if wal_end_lsn > state.next_record_lsn {
                            // All the records in this batch were seen by the sender.
                            // However, the batch maps to a chunk of WAL that the
                            // sender has not yet seen. Notify it of the start LSN
                            // of the PG WAL chunk such that it doesn't look like a gap.
                            InterpretedWalRecords {
                                records: Vec::default(),
                                next_record_lsn: state.next_record_lsn,
                                raw_wal_start_lsn: Some(batch_wal_start_lsn),
                            }
                        } else {
                            // The sender has seen this chunk of WAL before. Skip it.
                            continue;
                        };

                        let res = state.tx.send(Batch {
                            wal_end_lsn,
                            available_wal_end_lsn,
                            records: batch,
                        }).await;

                        if res.is_err() {
                            senders_to_remove.push(state.sender_id);
                        } else {
                            state.next_record_lsn = std::cmp::max(state.next_record_lsn, max_next_record_lsn);
                        }
                    }

                    // Clean up any senders that have dropped out.
                    // This is inefficient, but such events are rare (connection to PS termination)
                    // and the number of subscriptions is very small (only one for the steady state).
                    for sender_id in senders_to_remove {
                        if let Some(idx) = self.senders.iter().position(|s| s.sender_id == sender_id) {
                            self.senders.remove(idx);
                            tracing::info!("Removed sender {}", sender_id);
                        }
                    }
                },
                // Listen for new senders that want to attach to this reader.
                // If the reader is not running as a task, then this is not supported
                // (see the pending branch below).
                notification = match self.sender_notification_rx.as_mut() {
                        Some(rx) => Either::Left(rx.recv()),
                        None => Either::Right(std::future::pending())
                    } => {
                    if let Some(n) = notification {
                        let AttachSenderNotification { sender, start_pos } = n;

                        // Clean up any senders that have dropped out before adding the new
                        // one. This avoids a build up of dead senders.
                        self.senders.retain(|sender| {
                            let closed = sender.tx.is_closed();
                            if closed {
                                tracing::info!("Removed sender {}", sender.sender_id);
                            }
                            !closed
                        });

                        let new_sender_id = match self.senders.last() {
                            Some(sender) => sender.sender_id.next(),
                            None => SenderId::first()
                        };

                        self.senders.push(SenderState { sender_id: new_sender_id, tx: sender, next_record_lsn: start_pos});

                        // If the sender is subscribing below the current position then we need
                        // to update the cursor that tracks where we are at in the WAL
                        // ([`Self::state`]) and reset the WAL stream itself
                        // (`[Self::wal_stream`]). This must be done atomically from the POV of
                        // anything outside the select statement.
                        let position_reset = self.state.write().unwrap().maybe_reset(start_pos);
                        match position_reset {
                            CurrentPositionUpdate::Reset { from: _, to } => {
                                self.wal_stream.reset(to).await;
                                wal_decoder = WalStreamDecoder::new(to, self.pg_version);
                            },
                            CurrentPositionUpdate::NotReset(_) => {}
                        };

                        tracing::info!(
                            "Added sender {} with start_pos={} previous_pos={} current_pos={}",
                            new_sender_id,
                            start_pos,
                            position_reset.previous_position(),
                            position_reset.current_position(),
                        );
                    }
                }
            }
        }
    }
}

impl InterpretedWalReaderHandle {
    /// Fan-out the reader by attaching a new sender to it
    pub(crate) fn fanout(
        &self,
        sender: tokio::sync::mpsc::Sender<Batch>,
        start_pos: Lsn,
    ) -> Result<(), SendError<AttachSenderNotification>> {
        self.sender_notification_tx.send(AttachSenderNotification {
            sender,
            start_pos,
        })
    }

    /// Get the current WAL position of the reader
    pub(crate) fn current_position(&self) -> Option<Lsn> {
        self.state.read().unwrap().current_position()
    }

    pub(crate) fn abort(&self) {
        self.join_handle.abort()
    }
}

impl Drop for InterpretedWalReaderHandle {
    fn drop(&mut self) {
        tracing::info!("Aborting interpreted wal reader");
        self.abort()
    }
}

pub(crate) struct InterpretedWalSender<'a, IO> {
    pub(crate) format: InterpretedFormat,
    pub(crate) compression: Option<Compression>,
    pub(crate) appname: Option<String>,

    pub(crate) tli: WalResidentTimeline,
    pub(crate) start_lsn: Lsn,

    pub(crate) pgb: &'a mut PostgresBackend<IO>,
    pub(crate) end_watch_view: EndWatchView,
    pub(crate) wal_sender_guard: Arc<WalSenderGuard>,
    pub(crate) rx: tokio::sync::mpsc::Receiver<Batch>,

    /// Reused across batches to avoid a fresh wire-buffer allocation per send.
    pub(crate) wire_buf: BytesMut,
}

impl<IO: AsyncRead + AsyncWrite + Unpin> InterpretedWalSender<'_, IO> {
    /// Send interpreted WAL records over the network.
    /// Also manages keep-alives if nothing was sent for a while.
    pub(crate) async fn run(mut self) -> Result<(), CopyStreamHandlerEnd> {
        let mut keepalive_ticker = tokio::time::interval(Duration::from_secs(1));
        keepalive_ticker.set_missed_tick_behavior(MissedTickBehavior::Skip);
        keepalive_ticker.reset();

        let mut wal_position = self.start_lsn;

        loop {
            tokio::select! {
                batch = self.rx.recv() => {
                    let batch = match batch {
                        Some(b) => b,
                        None => {
                            return Result::Err(
                                CopyStreamHandlerEnd::Other(anyhow!("Interpreted WAL reader exited early"))
                            );
                        }
                    };

                    wal_position = batch.wal_end_lsn;

                    batch
                        .records
                        .encode_to_wire(self.format, self.compression, &mut self.wire_buf)
                        .await
                        .with_context(|| "Failed to serialize interpreted WAL")
                        .map_err(CopyStreamHandlerEnd::from)?;

                    // Reset the keep alive ticker since we are sending something
                    // over the wire now.
                    keepalive_ticker.reset();

                    self.pgb
                        .write_message(&BeMessage::InterpretedWalRecords(InterpretedWalRecordsBody {
                            streaming_lsn: batch.wal_end_lsn.0,
                            commit_lsn: batch.available_wal_end_lsn.0,
                            data: &self.wire_buf,
                        })).await?;
                }
                // Send a periodic keep alive when the connection has been idle for a while.
                // Since we've been idle, also check if we can stop streaming.
                _ = keepalive_ticker.tick() => {
                    if let Some(remote_consistent_lsn) = self.wal_sender_guard
                        .walsenders()
                        .get_ws_remote_consistent_lsn(self.wal_sender_guard.id())
                    {
                        if self.tli.should_walsender_stop(remote_consistent_lsn).await {
                            // Stop streaming if the receivers are caught up and
                            // there's no active compute. This causes the loop in
                            // [`crate::send_interpreted_wal::InterpretedWalSender::run`]
                            // to exit and terminate the WAL stream.
                            break;
                        }
                    }

                    self.pgb
                        .write_message(&BeMessage::KeepAlive(WalSndKeepAlive {
                            wal_end: self.end_watch_view.get().0,
                            timestamp: get_current_timestamp(),
                            request_reply: true,
                        }))
                        .await?;
                },
            }
        }

        Err(CopyStreamHandlerEnd::ServerInitiated(format!(
            "ending streaming to {:?} at {}, receiver is caughtup and there is no computes",
            self.appname, wal_position,
        )))
    }
}
