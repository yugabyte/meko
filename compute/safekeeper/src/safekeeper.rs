//! Acceptor part of the proposer–acceptor WAL streaming protocol.

use std::cmp::max;
use std::str::FromStr;

use anyhow::{Context, Result, bail};
use bytes::{Buf, BufMut, Bytes, BytesMut};
use postgres_ffi::MAX_SEND_SIZE;
use postgres_versioninfo::{PgMajorVersion, PgVersionId};
use pq_proto::SystemId;
use safekeeper_api::models::HotStandbyFeedback;
use serde::{Deserialize, Serialize};
use tracing::*;
use utils::id::BranchId;
use utils::lsn::Lsn;
use utils::pageserver_feedback::PageserverFeedback;

use crate::metrics::{MISC_OPERATION_SECONDS, PROPOSER_ACCEPTOR_MESSAGES_TOTAL};
use crate::state::TimelineState;
use crate::{control_file, wal_storage};

/// The compute ↔ safekeeper wire-protocol version supported by this
/// safekeeper. yb-amp dropped v2 support in Phase 9 Wave 2;
/// walproposer hardcodes v3 too. The constant is retained (rather
/// than fully eliminated) because the handler still parses the
/// `START_WAL_PUSH (proto_version 'N', ...)` libpq command and
/// validates it.
pub const SK_PROTO_VERSION_3: u32 = 3;
pub const UNKNOWN_SERVER_VERSION: PgVersionId = PgVersionId::UNKNOWN;

// protocol messages

/// Initial Proposer -> Acceptor message
#[derive(Debug, Deserialize)]
pub struct ProposerGreeting {
    pub branch_id: BranchId,
    /// Postgres server version
    pub pg_version: PgVersionId,
    pub system_id: SystemId,
    pub wal_seg_size: u32,
}

/// Acceptor -> Proposer initial response. Carries the safekeeper's current
/// end-of-WAL (`flush_lsn`), from which the walproposer derives the point at
/// which it resumes streaming (and which the safekeeper truncates to).
#[derive(Debug, Serialize)]
pub struct AcceptorGreeting {
    flush_lsn: Lsn,
}

/*
 * Proposer -> Acceptor message announcing the proposer and communicating the
 * point from which it will stream WAL. The safekeeper truncates its WAL beyond
 * this point.
 */
#[derive(Debug, Clone)]
pub struct ProposerElected {
    pub start_streaming_at: Lsn,
}

/// Request with WAL message sent from proposer to safekeeper.
#[derive(Debug)]
pub struct AppendRequest {
    pub h: AppendRequestHeader,
    pub wal_data: Bytes,
}
#[derive(Debug, Clone, Deserialize)]
pub struct AppendRequestHeader {
    /// start position of message in WAL
    pub begin_lsn: Lsn,
    /// end position of message in WAL
    pub end_lsn: Lsn,
}

/// Report safekeeper state to proposer
#[derive(Debug, Serialize, Clone)]
pub struct AppendResponse {
    // Flushed end of wal on safekeeper.
    pub flush_lsn: Lsn,
    pub hs_feedback: HotStandbyFeedback,
    pub pageserver_feedback: Option<PageserverFeedback>,
}

/// Proposer -> Acceptor messages
#[derive(Debug)]
pub enum ProposerAcceptorMessage {
    Greeting(ProposerGreeting),
    Elected(ProposerElected),
    AppendRequest(AppendRequest),
    NoFlushAppendRequest(AppendRequest),
    FlushWAL,
}

/// Augment Bytes with fallible get_uN where N is number of bytes methods.
/// All reads are in network (big endian) order.
trait BytesF {
    fn get_u8_f(&mut self) -> Result<u8>;
    fn get_u32_f(&mut self) -> Result<u32>;
    fn get_u64_f(&mut self) -> Result<u64>;
}

impl BytesF for Bytes {
    fn get_u8_f(&mut self) -> Result<u8> {
        if self.is_empty() {
            bail!("no bytes left, expected 1");
        }
        Ok(self.get_u8())
    }
    fn get_u32_f(&mut self) -> Result<u32> {
        if self.remaining() < 4 {
            bail!("only {} bytes left, expected 4", self.remaining());
        }
        Ok(self.get_u32())
    }
    fn get_u64_f(&mut self) -> Result<u64> {
        if self.remaining() < 8 {
            bail!("only {} bytes left, expected 8", self.remaining());
        }
        Ok(self.get_u64())
    }
}

impl ProposerAcceptorMessage {
    /// Read cstring from Bytes.
    fn get_cstr(buf: &mut Bytes) -> Result<String> {
        let pos = buf
            .iter()
            .position(|x| *x == 0)
            .ok_or_else(|| anyhow::anyhow!("missing cstring terminator"))?;
        let result = buf.split_to(pos);
        buf.advance(1); // drop the null terminator
        match std::str::from_utf8(&result) {
            Ok(s) => Ok(s.to_string()),
            Err(e) => bail!("invalid utf8 in cstring: {}", e),
        }
    }

    /// Parse proposer message. yb-amp only supports v3; v2 was
    /// dropped in Phase 9 Wave 2.
    pub fn parse(mut msg_bytes: Bytes, proto_version: u32) -> Result<ProposerAcceptorMessage> {
        if proto_version != SK_PROTO_VERSION_3 {
            bail!("unsupported protocol version {proto_version}; only v3 is supported");
        }
        {
            if msg_bytes.is_empty() {
                bail!("ProposerAcceptorMessage is not complete: missing tag");
            }
            let tag = msg_bytes.get_u8_f().with_context(|| {
                "ProposerAcceptorMessage is not complete: missing tag".to_string()
            })? as char;
            match tag {
                'g' => {
                    let branch_id_str =
                        Self::get_cstr(&mut msg_bytes).with_context(|| "reading branch_id")?;
                    let branch_id = BranchId::from_str(&branch_id_str)?;
                    let pg_version = msg_bytes
                        .get_u32_f()
                        .with_context(|| "reading pg_version")?;
                    let system_id = msg_bytes.get_u64_f().with_context(|| "reading system_id")?;
                    let wal_seg_size = msg_bytes
                        .get_u32_f()
                        .with_context(|| "reading wal_seg_size")?;
                    let g = ProposerGreeting {
                        branch_id,
                        pg_version: PgVersionId::from_full_pg_version(pg_version),
                        system_id,
                        wal_seg_size,
                    };
                    Ok(ProposerAcceptorMessage::Greeting(g))
                }
                'e' => {
                    let start_streaming_at: Lsn = msg_bytes
                        .get_u64_f()
                        .with_context(|| "reading start_streaming_at")?
                        .into();
                    let msg = ProposerElected { start_streaming_at };
                    Ok(ProposerAcceptorMessage::Elected(msg))
                }
                'a' => {
                    let begin_lsn: Lsn = msg_bytes
                        .get_u64_f()
                        .with_context(|| "reading begin_lsn")?
                        .into();
                    let end_lsn: Lsn = msg_bytes
                        .get_u64_f()
                        .with_context(|| "reading end_lsn")?
                        .into();
                    let hdr = AppendRequestHeader { begin_lsn, end_lsn };
                    let rec_size = hdr
                        .end_lsn
                        .checked_sub(hdr.begin_lsn)
                        .context("begin_lsn > end_lsn in AppendRequest")?
                        .0 as usize;
                    if rec_size > MAX_SEND_SIZE {
                        bail!(
                            "AppendRequest is longer than MAX_SEND_SIZE ({})",
                            MAX_SEND_SIZE
                        );
                    }
                    if msg_bytes.remaining() < rec_size {
                        bail!(
                            "reading WAL: only {} bytes left, wanted {}",
                            msg_bytes.remaining(),
                            rec_size
                        );
                    }
                    let wal_data = msg_bytes.copy_to_bytes(rec_size);
                    let msg = AppendRequest { h: hdr, wal_data };

                    Ok(ProposerAcceptorMessage::AppendRequest(msg))
                }
                _ => bail!("unknown proposer-acceptor message tag: {}", tag),
            }
        }
    }

    /// The memory size of the message, including byte slices.
    pub fn size(&self) -> usize {
        const BASE_SIZE: usize = std::mem::size_of::<ProposerAcceptorMessage>();

        // For most types, the size is just the base enum size including the nested structs. Some
        // types also contain byte slices; add them.
        //
        // We explicitly list all fields, to draw attention here when new fields are added.
        let mut size = BASE_SIZE;
        size += match self {
            Self::Greeting(_) => 0,

            Self::Elected(_) => 0,

            Self::AppendRequest(AppendRequest {
                h:
                    AppendRequestHeader {
                        begin_lsn: _,
                        end_lsn: _,
                    },
                wal_data,
            }) => wal_data.len(),

            Self::NoFlushAppendRequest(AppendRequest {
                h:
                    AppendRequestHeader {
                        begin_lsn: _,
                        end_lsn: _,
                    },
                wal_data,
            }) => wal_data.len(),

            Self::FlushWAL => 0,
        };

        size
    }
}

/// Acceptor -> Proposer messages
#[derive(Debug)]
pub enum AcceptorProposerMessage {
    Greeting(AcceptorGreeting),
    AppendResponse(AppendResponse),
}

impl AcceptorProposerMessage {
    /// Serialize acceptor -> proposer message. yb-amp only supports
    /// v3; v2 was dropped in Phase 9 Wave 2.
    pub fn serialize(&self, buf: &mut BytesMut, proto_version: u32) -> Result<()> {
        if proto_version != SK_PROTO_VERSION_3 {
            bail!("unsupported protocol version {proto_version}; only v3 is supported");
        }
        {
            match self {
                AcceptorProposerMessage::Greeting(msg) => {
                    buf.put_u8(b'g');
                    buf.put_u64(msg.flush_lsn.into());
                }
                AcceptorProposerMessage::AppendResponse(msg) => {
                    buf.put_u8(b'a');
                    buf.put_u64(msg.flush_lsn.into());
                    buf.put_i64(msg.hs_feedback.ts);
                    buf.put_u64(msg.hs_feedback.xmin);
                    buf.put_u64(msg.hs_feedback.catalog_xmin);

                    // AsyncReadMessage in walproposer.c will not try to decode pageserver_feedback
                    // if it is not present.
                    if let Some(ref msg) = msg.pageserver_feedback {
                        msg.serialize(buf);
                    }
                }
            }
            Ok(())
        }
    }
}

/// Safekeeper reliably persists WAL received from the walproposer.
/// It controls all WAL disk writes and updates of control file.
///
/// Currently safekeeper processes:
/// - messages from compute (proposers) and provides replies
/// - messages from broker peers
pub struct SafeKeeper<CTRL: control_file::Storage, WAL: wal_storage::Storage> {
    pub state: TimelineState<CTRL>, // persistent state storage
    pub wal_store: WAL,
}

impl<CTRL, WAL> SafeKeeper<CTRL, WAL>
where
    CTRL: control_file::Storage,
    WAL: wal_storage::Storage,
{
    /// Accepts a control file storage containing the safekeeper state.
    /// State must be initialized, i.e. contain filled `branch_id`
    /// and `server` (`wal_seg_size` inside it) fields.
    pub fn new(state: TimelineState<CTRL>, wal_store: WAL) -> Result<SafeKeeper<CTRL, WAL>> {
        if state.branch_id == BranchId::from([0u8; 16]) {
            bail!(
                "Calling SafeKeeper::new with empty branch_id ({})",
                state.branch_id
            );
        }

        Ok(SafeKeeper { state, wal_store })
    }

    /// wal_store wrapper avoiding flush_lsn < timeline_start_lsn when we don't
    /// have WAL yet.
    pub fn flush_lsn(&self) -> Lsn {
        max(self.wal_store.flush_lsn(), self.state.timeline_start_lsn)
    }

    /// Process message from proposer and possibly form reply. Concurrent
    /// callers must exclude each other.
    pub async fn process_msg(
        &mut self,
        msg: &ProposerAcceptorMessage,
    ) -> Result<Option<AcceptorProposerMessage>> {
        let res = match msg {
            ProposerAcceptorMessage::Greeting(msg) => self.handle_greeting(msg).await,
            ProposerAcceptorMessage::Elected(msg) => self.handle_elected(msg).await,
            ProposerAcceptorMessage::AppendRequest(msg) => {
                self.handle_append_request(msg, true).await
            }
            ProposerAcceptorMessage::NoFlushAppendRequest(msg) => {
                self.handle_append_request(msg, false).await
            }
            ProposerAcceptorMessage::FlushWAL => self.handle_flush().await,
        };

        match &res {
            Ok(_) => PROPOSER_ACCEPTOR_MESSAGES_TOTAL
                .with_label_values(&["success"])
                .inc(),
            Err(_) => PROPOSER_ACCEPTOR_MESSAGES_TOTAL
                .with_label_values(&["error"])
                .inc(),
        };

        res
    }

    /// Handle initial message from proposer: check its sanity and reply
    /// with our node id and flush LSN.
    async fn handle_greeting(
        &mut self,
        msg: &ProposerGreeting,
    ) -> Result<Option<AcceptorProposerMessage>> {
        /* Postgres major version mismatch is treated as fatal error
         * because safekeepers parse WAL headers and the format
         * may change between versions.
         */
        if PgMajorVersion::try_from(msg.pg_version)?
            != PgMajorVersion::try_from(self.state.server.pg_version)?
            && self.state.server.pg_version != UNKNOWN_SERVER_VERSION
        {
            bail!(
                "incompatible server version {}, expected {}",
                msg.pg_version,
                self.state.server.pg_version
            );
        }

        if msg.branch_id != self.state.branch_id {
            bail!(
                "invalid branch ID, got {}, expected {}",
                msg.branch_id,
                self.state.branch_id
            );
        }
        if self.state.server.wal_seg_size != msg.wal_seg_size {
            bail!(
                "invalid wal_seg_size, got {}, expected {}",
                msg.wal_seg_size,
                self.state.server.wal_seg_size
            );
        }

        // system_id will be updated on mismatch
        // sync-safekeepers doesn't know sysid and sends 0, ignore it
        if self.state.server.system_id != msg.system_id && msg.system_id != 0 {
            if self.state.server.system_id != 0 {
                warn!(
                    "unexpected system ID arrived, got {}, expected {}",
                    msg.system_id, self.state.server.system_id
                );
            }

            let mut state = self.state.start_change();
            state.server.system_id = msg.system_id;
            if msg.pg_version != UNKNOWN_SERVER_VERSION {
                state.server.pg_version = msg.pg_version;
            }
            self.state.finish_change(&state).await?;
        }

        // Flush everything we've already received so the walproposer resumes
        // streaming at the end of our WAL, without overlap. On a reconnect the
        // Elected message truncates WAL back to this point.
        self.wal_store.flush_wal().await?;

        let apg = AcceptorGreeting {
            flush_lsn: self.flush_lsn(),
        };
        info!(
            "processed greeting {:?} from walproposer, sending {:?}",
            msg, apg
        );
        Ok(Some(AcceptorProposerMessage::Greeting(apg)))
    }

    /// Form AppendResponse from current state.
    fn append_response(&self) -> AppendResponse {
        let ar = AppendResponse {
            flush_lsn: self.flush_lsn(),
            // will be filled by the upper code to avoid bothering safekeeper
            hs_feedback: HotStandbyFeedback::empty(),
            pageserver_feedback: None,
        };
        trace!("formed AppendResponse {:?}", ar);
        ar
    }

    async fn handle_elected(
        &mut self,
        msg: &ProposerElected,
    ) -> Result<Option<AcceptorProposerMessage>> {
        let _timer = MISC_OPERATION_SECONDS
            .with_label_values(&["handle_elected"])
            .start_timer();

        info!(
            "received ProposerElected {:?}, flush_lsn={}",
            msg,
            self.flush_lsn()
        );

        // We are expected to never attempt to truncate committed data.
        // yb-amp: commit_lsn ≡ flush_lsn, so the invariant is identical to
        // "don't truncate before our durable end-of-WAL". The walproposer
        // derives `start_streaming_at` from the `flush_lsn` we reported in the
        // greeting, so in steady state this is exactly our end-of-WAL.
        assert!(
            msg.start_streaming_at >= self.flush_lsn(),
            "attempt to truncate committed data: start_streaming_at={}, flush_lsn={}",
            msg.start_streaming_at,
            self.flush_lsn(),
        );

        // Before first WAL write initialize its segment. It makes first segment
        // pg_waldump'able because stream from compute doesn't include its
        // segment and page headers.
        //
        // If we fail before first WAL write flush this action would be
        // repeated, that's ok because it is idempotent.
        if self.wal_store.flush_lsn() == Lsn::INVALID {
            self.wal_store
                .initialize_first_segment(msg.start_streaming_at)
                .await?;
        }

        // truncate wal, update the LSNs
        self.wal_store.truncate_wal(msg.start_streaming_at).await?;

        {
            let mut state = self.state.start_change();

            // Here we learn the initial LSN for the first time. On the very
            // first ProposerElected message `start_streaming_at` is the
            // timeline's WAL start.
            if state.timeline_start_lsn == Lsn(0) {
                state.timeline_start_lsn = msg.start_streaming_at;
                info!(
                    "setting timeline_start_lsn to {:?}",
                    state.timeline_start_lsn
                );
            }

            if state.local_start_lsn == Lsn(0) {
                state.local_start_lsn = msg.start_streaming_at;
                info!("setting local_start_lsn to {:?}", state.local_start_lsn);
            }

            // Seed remote_consistent_lsn at the timeline start so that
            // post-restart catch-up doesn't read a pre-timeline LSN.
            state.remote_consistent_lsn =
                max(state.remote_consistent_lsn, state.timeline_start_lsn);

            self.state.finish_change(&state).await?;
        }

        // Force a control-file sync at the truncation/resume point so
        // `postgres --sync-safekeepers` polls a durable, aligned LSN.
        self.state.flush().await?;

        info!("start receiving WAL since {:?}", msg.start_streaming_at);

        Ok(None)
    }

    /// Handle request to append WAL.
    #[allow(clippy::comparison_chain)]
    async fn handle_append_request(
        &mut self,
        msg: &AppendRequest,
        require_flush: bool,
    ) -> Result<Option<AcceptorProposerMessage>> {
        // Disallow any non-sequential writes, which can result in gaps or
        // overwrites. If we need to move the pointer, ProposerElected message
        // should have truncated WAL first accordingly. Note that the first
        // condition (WAL rewrite) is quite expected in real world; it happens
        // when walproposer reconnects to safekeeper and writes some more data
        // while first connection still gets some packets later. It might be
        // better to not log this as error! above.
        let write_lsn = self.wal_store.write_lsn();
        let flush_lsn = self.wal_store.flush_lsn();
        if write_lsn > msg.h.begin_lsn {
            bail!(
                "append request rewrites WAL written before, write_lsn={}, msg lsn={}",
                write_lsn,
                msg.h.begin_lsn
            );
        }
        if write_lsn < msg.h.begin_lsn && write_lsn != Lsn(0) {
            bail!(
                "append request creates gap in written WAL, write_lsn={}, msg lsn={}",
                write_lsn,
                msg.h.begin_lsn,
            );
        }

        // do the job
        if !msg.wal_data.is_empty() {
            self.wal_store
                .write_wal(msg.h.begin_lsn, &msg.wal_data)
                .await?;
        }

        // flush wal to the disk, if required
        if require_flush {
            self.wal_store.flush_wal().await?;
        }

        trace!(
            "processed AppendRequest of len {}, begin_lsn={}, end_lsn={:?}, flushed={:?}",
            msg.wal_data.len(),
            msg.h.begin_lsn,
            msg.h.end_lsn,
            require_flush,
        );

        // If flush_lsn hasn't updated, AppendResponse is not very useful.
        // This is the common case for !require_flush, but a flush can still
        // happen on segment bounds.
        if !require_flush && flush_lsn == self.flush_lsn() {
            return Ok(None);
        }

        let resp = self.append_response();
        Ok(Some(AcceptorProposerMessage::AppendResponse(resp)))
    }

    /// Flush WAL to disk. Return AppendResponse with latest LSNs.
    ///
    /// Notifies the consumer and returns immediately, so ingest keeps
    /// flowing. `flush_lsn` stays capped at true durability; acks follow as
    /// durability advances via `Storage::yb_durable_watch`.
    async fn handle_flush(&mut self) -> Result<Option<AcceptorProposerMessage>> {
        self.wal_store
            .yb_flush(crate::yb_wal_storage::YbFlush::Notify)
            .await?;
        Ok(Some(AcceptorProposerMessage::AppendResponse(
            self.append_response(),
        )))
    }

    /// YB durability-ack path: advance the local inner watermark to the new
    /// durable point, then snapshot the `AppendResponse`.
    pub(crate) async fn yb_emit_append_response(&mut self) -> Result<AppendResponse> {
        self.wal_store
            .yb_flush(crate::yb_wal_storage::YbFlush::LocalAdvanceOnly)
            .await?;
        Ok(self.append_response())
    }
}

#[cfg(test)]
mod tests {
    use std::ops::Deref;
    use std::str::FromStr;
    use std::time::{Instant, UNIX_EPOCH};

    use futures::future::BoxFuture;
    use postgres_ffi::{WAL_SEGMENT_SIZE, XLogSegNo};
    use safekeeper_api::ServerInfo;
    use utils::bin_ser::BeSer;

    use super::*;
    use crate::state::TimelinePersistentState;

    // fake storage for tests
    struct InMemoryState {
        persisted_state: TimelinePersistentState,
    }

    impl control_file::Storage for InMemoryState {
        async fn persist(&mut self, s: &TimelinePersistentState) -> Result<()> {
            self.persisted_state = s.clone();
            Ok(())
        }

        fn last_persist_at(&self) -> Instant {
            Instant::now()
        }
    }

    impl Deref for InMemoryState {
        type Target = TimelinePersistentState;

        fn deref(&self) -> &Self::Target {
            &self.persisted_state
        }
    }

    fn test_sk_state() -> TimelinePersistentState {
        let mut state = TimelinePersistentState::empty();
        state.server.wal_seg_size = WAL_SEGMENT_SIZE as u32;
        state.branch_id = BranchId::from([1u8; 16]);
        state
    }

    struct DummyWalStore {
        lsn: Lsn,
    }

    impl wal_storage::Storage for DummyWalStore {
        fn write_lsn(&self) -> Lsn {
            self.lsn
        }

        fn flush_lsn(&self) -> Lsn {
            self.lsn
        }

        async fn initialize_first_segment(&mut self, _init_lsn: Lsn) -> Result<()> {
            Ok(())
        }

        async fn write_wal(&mut self, startpos: Lsn, buf: &[u8]) -> Result<()> {
            self.lsn = startpos + buf.len() as u64;
            Ok(())
        }

        async fn truncate_wal(&mut self, end_pos: Lsn) -> Result<()> {
            self.lsn = end_pos;
            Ok(())
        }

        async fn flush_wal(&mut self) -> Result<()> {
            Ok(())
        }

        fn remove_up_to(&self, _segno_up_to: XLogSegNo) -> BoxFuture<'static, anyhow::Result<()>> {
            Box::pin(async { Ok(()) })
        }

        fn get_metrics(&self) -> crate::metrics::WalStorageMetrics {
            crate::metrics::WalStorageMetrics::default()
        }
    }

    #[tokio::test]
    async fn test_non_consecutive_write() {
        let storage = InMemoryState {
            persisted_state: test_sk_state(),
        };
        let wal_store = DummyWalStore { lsn: Lsn(0) };

        let mut sk = SafeKeeper::new(TimelineState::new(storage), wal_store).unwrap();

        let pem = ProposerElected {
            start_streaming_at: Lsn(1),
        };
        sk.process_msg(&ProposerAcceptorMessage::Elected(pem))
            .await
            .unwrap();

        let ar_hdr = AppendRequestHeader {
            begin_lsn: Lsn(1),
            end_lsn: Lsn(2),
        };
        let append_request = AppendRequest {
            h: ar_hdr.clone(),
            wal_data: Bytes::from_static(b"b"),
        };

        // do write ending at 2, it should be ok
        sk.process_msg(&ProposerAcceptorMessage::AppendRequest(append_request))
            .await
            .unwrap();
        let mut ar_hrd2 = ar_hdr.clone();
        ar_hrd2.begin_lsn = Lsn(4);
        ar_hrd2.end_lsn = Lsn(5);
        let append_request = AppendRequest {
            h: ar_hrd2,
            wal_data: Bytes::from_static(b"b"),
        };
        // and now starting at 4, it must fail
        sk.process_msg(&ProposerAcceptorMessage::AppendRequest(append_request))
            .await
            .unwrap_err();
    }

    #[test]
    fn test_sk_state_bincode_serde_roundtrip() {
        let branch_id = BranchId::from_str("112ded66422aa5e953e5440fa5427ac4").unwrap();
        let state = TimelinePersistentState {
            branch_id,
            server: ServerInfo {
                pg_version: PgVersionId::from_full_pg_version(180000),
                system_id: 0x1234567887654321,
                wal_seg_size: 0x12345678,
            },
            timeline_start_lsn: Lsn(0x12345600),
            local_start_lsn: Lsn(0x12),
            remote_consistent_lsn: Lsn(1234560000),
            creation_ts: UNIX_EPOCH,
        };

        let ser = state.ser().unwrap();

        let deser = TimelinePersistentState::des(&ser).unwrap();

        assert_eq!(deser, state);
    }
}
