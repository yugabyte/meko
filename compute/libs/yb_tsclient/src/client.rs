//! The public client: connection + session management, retries, writes,
//! and paged/pinned reads over `yb.tserver.PgClientService`.
//!
//! ## Session lifecycle
//!
//! `Heartbeat{session_id: 0}` creates a server-side session;
//! `Heartbeat{session_id: N}` keeps it alive. Sessions expire after
//! `pg_client_session_expiration_ms` (60s default), so a background task
//! heartbeats every [`HEARTBEAT_INTERVAL`] (10s). On connection loss or
//! a lost session ("Unknown session"), the client reconnects — walking
//! the endpoint list on connect failures — opens a fresh session,
//! re-validates every cached table (refreshing schema versions), and
//! restarts the per-session Perform `serial_no` at 0.
//!
//! The client holds a pool of sessions spread ACROSS all tserver
//! endpoints (see [`SessionPool`]): one read session per endpoint, plus
//! [`TsClientConfig::write_sessions`] dedicated write sessions
//! distributed round-robin over the endpoints, each its own TCP
//! connection + server session. Reads, table opens, and the first page
//! of a scan round-robin the read sessions so single-page reads (the
//! getpage hot path) fan out over every node; `upsert_batch` round-robins
//! the write sessions. Writes are stateless per Perform (fresh
//! `txn_serial_no`, no continuations), so any session on any node serves
//! any write; spreading them keeps multi-megabyte bulk-flush Performs from
//! serializing on one connection and one server session, and keeps one
//! tserver from taking all the traffic. A paged scan's continuations
//! must return to the read session that pinned page 1 (server read-point
//! history is per session), so continuations re-pin to that session while
//! only the first page round-robins. Recovery and heartbeats treat the
//! pool as one epoch/failure unit, but each new session reconnects its
//! predecessor's endpoint so the pool stays spread.
//!
//! ## Read-time pinning and Perform serial numbers
//!
//! `PgClientService` was built for pggate, which runs ONE statement at a
//! time per session; multiplexing concurrent Performs over one session
//! (which this client does) required mirroring pggate's serial-number
//! discipline, verified against `pg_client_session.cc` @ 2025.2.3.0:
//!
//! - **`txn_serial_no` is fresh per Perform.** The plain session arms a
//!   "pending used read time" whenever a Perform starts with no read
//!   point; a second Perform arriving with the *same* `txn_serial_no`
//!   while the first is in flight fails with `IllegalState: Expecting a
//!   used read time from the previous RPC` — a fresh serial takes the
//!   documented escape hatch (`CheckPlainSessionPendingUsedReadTime`).
//!
//! - **Reads run with `read_time_manipulation: ENSURE_READ_TIME_IS_SET`
//!   and a fresh `read_time_serial_no` per logical scan.** The proxy
//!   then picks the read time at setup and saves it into the session's
//!   `read_point_history_` keyed by our serial *before* the flush, so
//!   (a) no pending-used-read-time race exists, and (b) continuations
//!   that send the *same* `read_time_serial_no` restore the exact read
//!   point (`ReadPointHistory::Restore`) even when other Performs
//!   interleaved — multi-page scans are snapshot-consistent under
//!   concurrent writes.
//!
//! - **The server never returns the read time to the client** on this
//!   path: `pg_client_session.cc` deliberately clears
//!   `paging_state.read_time` from Perform responses (non-catalog), and
//!   `used_in_txn_limit_ht` is transaction-only. `ReadPage::
//!   used_read_time_ht` is therefore 0 unless the read was explicitly
//!   pinned. To put several reads on ONE snapshot, issue them as ops of
//!   one Perform ([`TsClient::read_pages`]); to pin to a known hybrid
//!   time (e.g. derived from a server clock), use
//!   [`RangeRead::read_time_ht`], which sets `options.read_time`
//!   explicitly.
//!
//! The scan serial travels inside the opaque `paging_state` bytes this
//! client hands back (an envelope around the server's paging state), so
//! `read_page` continuations stay on their snapshot with no caller
//! effort. Two guard rails on that mechanism (see [`PagingEnvelope`]
//! and `DESIGN.md`):
//!
//! - read serials are allocated per *attempt* — a retry reusing its
//!   failed attempt's serial would re-read at the exact read time that
//!   just failed (read-restarts would then fail every retry
//!   identically);
//! - the envelope is bound to the server session that created it —
//!   an unpinned continuation on a recovered session is rejected as
//!   fatal rather than silently served at a fresh read time.
//!
//! The session's read-point history is trimmed with
//! `read_time_serial_no_history_min` to a sliding window of the most
//! recent [`READ_SERIAL_HISTORY_WINDOW`] read serials; a scan paging
//! across more than that many later reads would silently lose its pin,
//! which our callers' bounded scans never approach.

use std::collections::HashMap;
use std::sync::atomic::{AtomicU64, Ordering};
use std::sync::{Arc, Weak};
use std::time::Duration;

use bytes::Bytes;
use prost::Message;

use crate::connection::Connection;
use crate::error::{ErrorClass, YbError, check_app_status, check_pgsql_response};
use crate::ops::{RangeRead, UpsertRow};
use crate::pb::yb::tserver::ReadTimeManipulation;
use crate::pb::yb::tserver::{
    PgHeartbeatRequestPb, PgHeartbeatResponsePb, PgOpenTableRequestPb, PgOpenTableResponsePb,
    PgPerformOpPb, PgPerformOptionsPb, PgPerformRequestPb, PgPerformResponsePb, pg_perform_op_pb,
};
use crate::pb::yb::{PgsqlPagingStatePb, PgsqlResponsePb, ReadHybridTimePb};
use crate::rows::{Row, ValueType};
use crate::session::{ConnState, Endpoints, SessionCore, SessionMeta, SessionPool};
use crate::table::{TableHandle, columns_from_schema, pgsql_table_id};

const SERVICE: &str = "yb.tserver.PgClientService";

const MAX_ATTEMPTS: usize = 10;
const BACKOFF_INITIAL: Duration = Duration::from_millis(50);
const BACKOFF_CAP: Duration = Duration::from_secs(1);

/// Keep-alive period; the server expires idle sessions after
/// `pg_client_session_expiration_ms` (60s default), so 10s gives five
/// missed beats of slack.
const HEARTBEAT_INTERVAL: Duration = Duration::from_secs(10);

/// How many recent read serials the server keeps restorable read points
/// for (`read_time_serial_no_history_min` trimming); bounds server-side
/// memory at ~64B per read-serial entry.
pub const READ_SERIAL_HISTORY_WINDOW: u64 = 65_536;

#[derive(Debug, Clone)]
pub struct TsClientConfig {
    /// tserver RPC endpoints, `host:port` (default tserver RPC port 9100).
    pub tserver_addrs: Vec<String>,
    /// Endpoint to prefer as the pool's lead (e.g. the co-located
    /// tserver). Must match one of `tserver_addrs` textually; when unset,
    /// a loopback address in the list leads, else the first entry. The
    /// pool opens a read session on EVERY endpoint regardless — this only
    /// picks which one leads (reported by `connected_addr`, used to
    /// re-validate tables on recovery) and the per-session failover order.
    pub preferred_addr: Option<String>,
    /// Per-RPC deadline (also bounds connect attempts).
    pub rpc_timeout: Duration,
    /// Dedicated write sessions (each its own TCP connection + server
    /// session), distributed round-robin across the endpoints, that
    /// [`TsClient::upsert_batch`] round-robins over — on top of the
    /// one-read-session-per-endpoint that serves everything else. 0 (the
    /// default) routes writes over the read sessions instead (still one
    /// per endpoint, so still spread across nodes).
    ///
    /// Why: multi-megabyte write Performs serialize on a single
    /// connection — frame encode/write on the client, frame read +
    /// protobuf decode on the server's one reactor thread for that
    /// connection, and Perform-start sequencing on the one server-side
    /// session — so concurrent bulk-flush chunks barely overlap, and
    /// reads sharing the connection queue behind the write frames.
    /// Callers that fan out bulk writes (the pageserver pagewriter) set
    /// this to their chunk-fanout width; one-batch-at-a-time callers
    /// (the safekeeper WAL flush) leave it 0.
    pub write_sessions: usize,
    /// Close a session's TCP connection (and let its server-side session
    /// expire) once it has been idle this long, reopening transparently on
    /// the next use. Keeping every session warm forever holds a
    /// fleet-wide fan of tserver connections + sessions + 10s heartbeat
    /// chatter on branches that go quiet (one pageserver was seen holding
    /// 131 connections to a single tserver); idle-close bounds that.
    ///
    /// `Some(d)`: sweep idle sessions closed after `d` (default 60s).
    /// `None`: never close — the previous always-warm behavior.
    ///
    /// The reopen is just reconnect + `Heartbeat` (fresh server session
    /// id); it does NOT re-run `OpenTable`, because the hot tables'
    /// schema is immutable, so the cached [`TableHandle`] stays valid.
    /// See `DESIGN.md` for the idle-close / reopen / scan-pin interaction.
    pub idle_timeout: Option<Duration>,
}

impl Default for TsClientConfig {
    fn default() -> Self {
        TsClientConfig {
            tserver_addrs: Vec::new(),
            preferred_addr: None,
            rpc_timeout: Duration::from_secs(10),
            write_sessions: 0,
            idle_timeout: Some(Duration::from_secs(60)),
        }
    }
}

/// Sink for client health events, wired by the owning daemon to its
/// metrics registry (e.g. `pageserver_yb_store_session_recoveries_total`).
/// This client retries transient failures internally — invisibly to
/// callers unless something exports the churn; a field incident where
/// reads stalled for hours with nothing in any dashboard motivated
/// making these first-class.
pub trait ClientObserver: Send + Sync + 'static {
    /// A session was re-established with a fresh server session id. Fires
    /// for both a pool-wide recovery (a Network/lost-session failure
    /// reconnected the whole pool) and an idle-close reopen (a session
    /// that had been swept Cold reconnected on its next use) — both mint a
    /// new server session, so callers counting `*_session_recoveries_total`
    /// see churn from either. The two are not distinguished; a reopen
    /// under a short idle timeout is expected background churn, not a
    /// fault.
    fn on_session_recovery(&self) {}
    /// One call attempt gave up waiting for its response
    /// ([`TsClientConfig::rpc_timeout`]); the transport was healthy.
    fn on_call_timeout(&self) {}
}

/// One fetched page of a range read.
pub struct ReadPage {
    pub rows: Vec<Row>,
    /// Pass into the next fetch to continue the scan; `None` = done.
    /// Opaque: an envelope of the server's paging state plus the scan's
    /// read serial, which keeps continuations on the scan's snapshot.
    pub paging_state: Option<Bytes>,
    /// The explicit hybrid time this read was pinned to, or 0 when the
    /// server picked the read time — which it keeps to itself on this
    /// RPC path (see the module docs): continuations still stay on the
    /// snapshot via the read serial in `paging_state`, and reads needing
    /// a *shared* snapshot go through [`TsClient::read_pages`].
    pub used_read_time_ht: u64,
}

/// Pure-Rust direct-to-tserver YB client speaking the pggate RPC
/// protocol (`yb.tserver.PgClientService`). One instance per component.
/// The session pool spans every tserver endpoint; reads round-robin the
/// per-endpoint read sessions (paged-scan continuations re-pin to their
/// scan's session) and `upsert_batch` round-robins the write sessions
/// ([`TsClientConfig::write_sessions`]), so both reads and writes spread
/// across nodes and bulk write chunks truly overlap.
pub struct TsClient {
    rpc_timeout: Duration,
    endpoints: Endpoints,
    /// See [`TsClientConfig::write_sessions`]; recovery re-creates the
    /// pool at this size.
    write_sessions: usize,
    /// See [`TsClientConfig::idle_timeout`]; `None` = never idle-close.
    idle_timeout: Option<Duration>,
    /// Current session pool; replaced wholesale by recovery.
    state: std::sync::RwLock<Arc<SessionPool>>,
    /// Serializes recovery so concurrent failures produce one new pool.
    recovery_lock: tokio::sync::Mutex<()>,
    /// Every table ever opened, re-validated on session recovery.
    tables: std::sync::Mutex<HashMap<(u32, u32), Weak<TableHandle>>>,
    /// Client-unique statement ids (the server only logs them).
    next_stmt_id: AtomicU64,
    /// Per-Perform `txn_serial_no` (see the module docs).
    next_txn_serial: AtomicU64,
    /// Per-logical-scan `read_time_serial_no` (see the module docs).
    next_read_serial: AtomicU64,
    /// Round-robin ticket for picking a write session.
    next_write_ticket: std::sync::atomic::AtomicUsize,
    heartbeat_task: std::sync::Mutex<Option<tokio::task::JoinHandle<()>>>,
    /// Health-event sink ([`Self::set_observer`]); None = drop events.
    observer: std::sync::RwLock<Option<Arc<dyn ClientObserver>>>,
}

/// Which session an operation's attempt runs on.
///
/// - `Read`: round-robin across the per-endpoint read sessions. First
///   pages of scans, single-page reads, and table opens — the spread that
///   stops one tserver from taking all read traffic.
/// - `PinnedRead(session_id)`: the exact read session that created a
///   paged scan. A continuation MUST return to it (the server keeps the
///   scan's read point in that session's read-point history), so once
///   page 1 picks a session by round-robin, every continuation re-pins to
///   that session id. Resolves to `None` after a recovery replaced the
///   pool — the continuation then fails loudly (its snapshot is gone).
/// - `Write`: round-robin across the dedicated write sessions (or the
///   read sessions when none are configured), spread across endpoints.
///   Writes are stateless per Perform, so any session serves any write.
#[derive(Clone, Copy)]
enum SessionSel {
    Read,
    PinnedRead(u64),
    Write,
}

impl Drop for TsClient {
    fn drop(&mut self) {
        if let Some(task) = self.heartbeat_task.lock().expect("heartbeat lock").take() {
            task.abort();
        }
    }
}

impl TsClient {
    pub async fn connect(config: TsClientConfig) -> Result<Arc<TsClient>, YbError> {
        let endpoints = Endpoints::new(config.tserver_addrs, config.preferred_addr.as_deref())?;
        let pool = establish_pool(&endpoints, config.rpc_timeout, 0, config.write_sessions).await?;
        let client = Arc::new(TsClient {
            rpc_timeout: config.rpc_timeout,
            endpoints,
            write_sessions: config.write_sessions,
            idle_timeout: config.idle_timeout,
            state: std::sync::RwLock::new(Arc::new(pool)),
            recovery_lock: tokio::sync::Mutex::new(()),
            tables: std::sync::Mutex::new(HashMap::new()),
            next_stmt_id: AtomicU64::new(1),
            next_txn_serial: AtomicU64::new(1),
            next_read_serial: AtomicU64::new(1),
            next_write_ticket: std::sync::atomic::AtomicUsize::new(0),
            heartbeat_task: std::sync::Mutex::new(None),
            observer: std::sync::RwLock::new(None),
        });
        let task = spawn_heartbeat(&client);
        *client.heartbeat_task.lock().expect("heartbeat lock") = Some(task);
        Ok(client)
    }

    /// Opens the YSQL table `(database_oid, table_oid)`. The OIDs come
    /// from a one-time catalog query on the caller's existing YSQL
    /// connection; `table_oid` must be the relation's `relfilenode`
    /// (equal to its oid unless the table was rewritten). Handles are
    /// cached: reopening an already-open table returns the same Arc.
    pub async fn open_table(
        self: &Arc<Self>,
        database_oid: u32,
        table_oid: u32,
    ) -> Result<Arc<TableHandle>, YbError> {
        let key = (database_oid, table_oid);
        if let Some(existing) = self
            .tables
            .lock()
            .expect("tables lock")
            .get(&key)
            .and_then(Weak::upgrade)
        {
            return Ok(existing);
        }
        let (version, columns) = self
            .with_retries("open_table", SessionSel::Read, |session| async move {
                let conn = self.warm_conn(&session, true).await?;
                raw_open_table(&conn, database_oid, table_oid, self.rpc_timeout).await
            })
            .await?;
        let handle = Arc::new(TableHandle {
            table_id: pgsql_table_id(database_oid, table_oid),
            columns,
            database_oid,
            table_oid,
            schema_version: version.into(),
        });
        self.tables
            .lock()
            .expect("tables lock")
            .insert(key, Arc::downgrade(&handle));
        Ok(handle)
    }

    /// Upserts the batch (PGSQL_UPSERT: no read-before-write) in a
    /// single Perform call. Retries replay the full batch — callers'
    /// rows are idempotent by contract.
    ///
    /// Atomicity: ops that share a partition key land on one tablet and
    /// the tserver session flushes them as one write batch (one Raft
    /// entry, one commit hybrid time), so readers see all rows of such a
    /// batch or none. Rows spanning partition keys may commit per
    /// tablet, independently.
    pub async fn upsert_batch(
        &self,
        table: &Arc<TableHandle>,
        rows: &[UpsertRow],
    ) -> Result<(), YbError> {
        self.upsert_batch_bounded(table, rows, MAX_ATTEMPTS).await
    }

    /// [`Self::upsert_batch`] with a caller-chosen retry budget. Callers
    /// that hold their own fallback transport (e.g. the safekeeper's
    /// YSQL lane behind the WAL write path) keep this small: with each
    /// attempt bounded by the RPC timeout, the full default budget can
    /// stall a caller for minutes before its fallback ever fires.
    pub async fn upsert_batch_bounded(
        &self,
        table: &Arc<TableHandle>,
        rows: &[UpsertRow],
        max_attempts: usize,
    ) -> Result<(), YbError> {
        if rows.is_empty() {
            return Ok(());
        }
        let mut ops = Vec::with_capacity(rows.len());
        for row in rows {
            ops.push(PgPerformOpPb {
                op: Some(pg_perform_op_pb::Op::Write(
                    row.to_write_request(table, self.take_stmt_id())?,
                )),
                ..Default::default()
            });
        }
        let ops = &ops;
        self.with_retries_bounded(
            "upsert_batch",
            SessionSel::Write,
            max_attempts,
            |session| async move {
                // Writes need no read point; a fresh read serial forces a
                // read-point reset so they never entangle with scans.
                let mut options = self.base_options();
                options.read_time_serial_no = self.alloc_read_serial();
                // Writes are stateless: any session serves any write, so
                // reopening a Cold one is fine.
                let (resp, _sidecars) = self
                    .perform_on(&session, ops.clone(), options, true)
                    .await?;
                if resp.responses.len() != ops.len() {
                    return Err(YbError::other(format!(
                        "upsert_batch: sent {} ops, got {} responses",
                        ops.len(),
                        resp.responses.len()
                    )));
                }
                for r in &resp.responses {
                    check_pgsql_response(r, "upsert")?;
                }
                Ok(())
            },
        )
        .await
    }

    /// One read RPC (one page). Retries transient failures; a mid-scan
    /// retry replays only the current page. Continuations pin the read
    /// time carried in `paging_state`, so pass pages onward untouched.
    ///
    /// Session selection is what makes single reads spread while paged
    /// scans stay coherent: the FIRST page (no `paging_state`) picks a
    /// read session round-robin across endpoints — this is the getpage
    /// hot path, spread across tservers — whereas a continuation re-pins
    /// to the exact session that started the scan (its snapshot lives in
    /// that session's server-side read-point history; a different session
    /// would silently re-read at a fresh read time). The session id
    /// travels in the paging envelope; on a recovered pool the pin misses
    /// and the continuation fails loudly.
    pub async fn read_page(
        &self,
        table: &Arc<TableHandle>,
        read: &RangeRead,
        paging_state: Option<Bytes>,
    ) -> Result<ReadPage, YbError> {
        // Pin continuations to their scan's session; round-robin only the
        // first page. An unpinned (explicit-read-time) scan can continue
        // on any session — its snapshot isn't in per-session history — but
        // pinning it too is harmless and keeps one scan on one connection.
        let sel = match &paging_state {
            None => SessionSel::Read,
            Some(bytes) => match PagingEnvelope::decode(bytes) {
                Ok(env) => SessionSel::PinnedRead(env.session_id),
                // Malformed envelope: let do_read_page surface the decode
                // error on a round-robin session rather than failing here.
                Err(_) => SessionSel::Read,
            },
        };
        let paging_state = &paging_state;
        self.with_retries("read", sel, |session| async move {
            self.do_read_page(&session, table, read, paging_state.clone())
                .await
        })
        .await
    }

    /// Reads the whole range, following paging. All pages stay on the
    /// first page's snapshot (via the scan's read serial carried in the
    /// paging envelope), so the result is one consistent cut. Prefer
    /// `read_page` loops with an early stop when the caller can
    /// terminate before the range is exhausted.
    pub async fn read_all(
        &self,
        table: &Arc<TableHandle>,
        read: &RangeRead,
    ) -> Result<Vec<Row>, YbError> {
        let mut rows = Vec::new();
        let mut paging = None;
        loop {
            let page = self.read_page(table, read, paging).await?;
            rows.extend(page.rows);
            match page.paging_state {
                Some(p) => paging = Some(p),
                None => return Ok(rows),
            }
        }
    }

    /// Issues several reads as ops of ONE Perform call: the server
    /// serves every op at the same read time, so the pages form one
    /// consistent snapshot with no explicit pinning. This replaces the
    /// read-then-pin two-step where the snapshot must cover reads that
    /// can't be expressed as a single range (e.g. several hash keys).
    ///
    /// Returns first pages only; continue each scan with [`Self::read_page`]
    /// passing the page's `paging_state` (continuations stay on the
    /// shared snapshot via the read time it carries). Reads that set
    /// [`RangeRead::read_time_ht`] must all agree on the value.
    pub async fn read_pages(
        &self,
        table: &Arc<TableHandle>,
        reads: &[RangeRead],
    ) -> Result<Vec<ReadPage>, YbError> {
        if reads.is_empty() {
            return Ok(Vec::new());
        }
        let mut pin_ht = 0u64;
        for read in reads {
            if read.read_time_ht != 0 {
                if pin_ht != 0 && pin_ht != read.read_time_ht {
                    return Err(YbError::invalid(
                        "read_pages: reads pin different read times; issue them separately",
                    ));
                }
                pin_ht = read.read_time_ht;
            }
        }
        let mut ops = Vec::with_capacity(reads.len());
        for read in reads {
            ops.push(PgPerformOpPb {
                op: Some(pg_perform_op_pb::Op::Read(
                    read.to_read_request(table, self.take_stmt_id())?,
                )),
                ..Default::default()
            });
        }
        let (ops, reads_ref) = (&ops, reads);
        self.with_retries("read_pages", SessionSel::Read, |session| async move {
            // One read serial for the whole op group: the proxy pins one
            // read point at setup (ENSURE_READ_TIME_IS_SET) and every op
            // — and every continuation carrying this serial — shares it.
            //
            // Allocated PER ATTEMPT, not per call: `ENSURE_READ_TIME_IS_
            // SET` only picks a new read time when the serial changes,
            // so a retry that reuses the serial re-reads at the read
            // time that just failed. For a read-restart (`TryAgainCode
            // restart_read_time`, routine on multi-node clusters where
            // writes commit inside the uncertainty window of a
            // proxy-picked read time) that retry fails the same way
            // every time — the retry budget burns down deterministically
            // and the caller sees a persistent read failure.
            let read_serial = self.alloc_read_serial();
            let options = self.read_options(read_serial, pin_ht);
            // First pages of fresh scans: no pin to lose, so reopen is OK.
            let (resp, sidecars) = self
                .perform_on(&session, ops.clone(), options, true)
                .await?;
            if resp.responses.len() != reads_ref.len() {
                return Err(YbError::other(format!(
                    "read_pages: sent {} ops, got {} responses",
                    reads_ref.len(),
                    resp.responses.len()
                )));
            }
            resp.responses
                .into_iter()
                .zip(reads_ref)
                .map(|(r, read)| {
                    page_from_response(
                        r,
                        &sidecars,
                        read.target_types(),
                        read_serial,
                        pin_ht,
                        session.meta.session_id(),
                    )
                })
                .collect()
        })
        .await
    }

    /// A tserver endpoint the client is currently connected to (the first
    /// read session's peer). The pool spans every endpoint; see
    /// [`Self::connected_peers`] for the full set.
    pub fn connected_addr(&self) -> String {
        self.current_pool().read_sessions[0].peer().to_string()
    }

    /// The peer address of every session in the pool (read sessions then
    /// write sessions), in order. Exposes the multi-endpoint spread for
    /// tests and diagnostics; `connected_peers().dedup()` counts distinct
    /// tservers the pool holds connections to.
    #[doc(hidden)]
    pub fn connected_peers(&self) -> Vec<String> {
        self.current_pool()
            .all()
            .map(|s| s.peer().to_string())
            .collect()
    }

    /// Per-lane distinct-endpoint counts `(read_endpoints,
    /// write_endpoints)` — how many different tservers the read sessions
    /// and the write sessions respectively span. A test hook for the
    /// spread assertions.
    #[doc(hidden)]
    pub fn endpoint_spread(&self) -> (usize, usize) {
        let pool = self.current_pool();
        let distinct = |it: &[Arc<SessionCore>]| {
            let mut peers: Vec<&str> = it.iter().map(|s| s.peer()).collect();
            peers.sort_unstable();
            peers.dedup();
            peers.len()
        };
        (
            distinct(&pool.read_sessions),
            distinct(&pool.write_sessions),
        )
    }

    /// The current server-side session id of the first read session
    /// (changes on recovery). Kept for the existing recovery tests; the
    /// pool now holds one session id per endpoint.
    pub fn session_id(&self) -> u64 {
        self.current_pool().read_sessions[0].meta.session_id()
    }

    /// Installs the health-event sink (session recoveries, per-attempt
    /// call timeouts). One observer per client; installing replaces any
    /// previous one.
    pub fn set_observer(&self, observer: Arc<dyn ClientObserver>) {
        *self.observer.write().expect("observer lock") = Some(observer);
    }

    fn observe(&self, f: impl FnOnce(&dyn ClientObserver)) {
        if let Some(observer) = self.observer.read().expect("observer lock").as_deref() {
            f(observer);
        }
    }

    /// Test hook: make the server reject our session ids on the next
    /// call, exercising the lost-session recovery path without waiting
    /// out the 60s server-side expiry. Poisons every pool slot.
    #[doc(hidden)]
    pub fn poison_session_for_test(&self) {
        for slot in self.current_pool().all() {
            slot.meta.poison();
        }
    }

    /// Test hook: run one idle-sweep pass over the pool right now (what a
    /// heartbeat tick does), so a test needn't wait out a real
    /// [`HEARTBEAT_INTERVAL`]. Returns how many sessions are Warm
    /// afterward — a way to assert the sweeper closed the idle ones
    /// without exposing internals.
    #[doc(hidden)]
    pub async fn sweep_idle_for_test(&self) -> usize {
        let pool = self.current_pool();
        for session in pool.all() {
            let _ = self.heartbeat_or_close_idle(session).await;
        }
        self.warm_session_count_for_test().await
    }

    /// Test hook: how many pool sessions currently hold a warm connection.
    #[doc(hidden)]
    pub async fn warm_session_count_for_test(&self) -> usize {
        let pool = self.current_pool();
        let mut warm = 0;
        for session in pool.all() {
            if matches!(&*session.conn.lock().await, ConnState::Warm(_)) {
                warm += 1;
            }
        }
        warm
    }

    fn current_pool(&self) -> Arc<SessionPool> {
        self.state.read().expect("state lock").clone()
    }

    /// Resolve the session an attempt runs on. `Read` and `Write`
    /// round-robin across endpoints (the spread); `PinnedRead(id)` returns
    /// the exact read session that started a paged scan, or `None` if the
    /// pool no longer holds it (a recovery replaced it) — the caller then
    /// fails the continuation loudly rather than switching its snapshot.
    fn session_for(&self, sel: SessionSel) -> Option<Arc<SessionCore>> {
        let pool = self.current_pool();
        match sel {
            SessionSel::Read => Some(Arc::clone(pool.read())),
            SessionSel::Write => Some(Arc::clone(
                pool.write(
                    self.next_write_ticket
                        .fetch_add(1, std::sync::atomic::Ordering::Relaxed),
                ),
            )),
            SessionSel::PinnedRead(id) => pool.by_session_id(id).map(Arc::clone),
        }
    }

    /// Return a warm connection for `session`, reopening it if the idle
    /// sweeper had closed it (Cold → Warm), and mark it used. Under the
    /// session's async connection lock, so a concurrent burst of callers
    /// on the same Cold session re-establishes EXACTLY ONCE — the first
    /// waiter reconnects, the rest observe Warm and reuse it (no
    /// thundering herd of reconnects) — and the sweeper (which takes the
    /// same lock to go Warm → Cold) can never close a connection out from
    /// under an in-flight caller.
    ///
    /// The reopen is [`establish_session_conn`] — connect + `Heartbeat`,
    /// **no `OpenTable`** (schema is immutable; the cached `TableHandle`
    /// stays valid). The new server session id replaces the old in
    /// `session.meta` and its Perform serials restart at 0. A reopen fires
    /// the `on_session_recovery` observer hook (see [`ClientObserver`]).
    ///
    /// `reopen == false` forbids reopening a Cold session and fails loudly
    /// instead: a paged-scan continuation is pinned to the server session
    /// that holds its read-point history, so reopening (which mints a NEW
    /// session id) would silently switch its snapshot — exactly the
    /// mismatch `do_read_page`'s session-bound guard rejects. Failing here
    /// makes idle-close indistinguishable from a recovery for a pinned
    /// scan: the caller restarts (or falls back).
    async fn warm_conn(
        &self,
        session: &SessionCore,
        reopen: bool,
    ) -> Result<Arc<Connection>, YbError> {
        session.touch();
        let mut guard = session.conn.lock().await;
        if let ConnState::Warm(conn) = &*guard {
            return Ok(Arc::clone(conn));
        }
        if !reopen {
            return Err(YbError::other(format!(
                "paging continuation's session (id {}) on {} was idle-closed; its \
                 read-point history is gone — restart the scan",
                session.meta.session_id(),
                session.peer(),
            )));
        }
        // Cold: this task won the lock, so it reconnects; others awaiting
        // the lock will see Warm below and reuse the connection.
        let established =
            establish_session_conn(&self.endpoints, session.endpoint_idx, self.rpc_timeout).await?;
        let conn = Arc::clone(&established.conn);
        session.meta.reopened_with(established.session_id);
        *guard = ConnState::Warm(established.conn);
        drop(guard);
        self.observe(ClientObserver::on_session_recovery);
        tracing::info!(
            "yb_tsclient: reopened idle session on {} (new session id {}, no OpenTable)",
            session.peer(),
            established.session_id,
        );
        Ok(conn)
    }

    /// One sweep tick for a single session (see [`spawn_heartbeat`]):
    /// close it if Warm-and-idle, heartbeat it if Warm-and-active, skip if
    /// Cold. Takes the session's connection lock so it can't race a
    /// concurrent user's reopen; a `Network` error from the heartbeat
    /// propagates to trigger pool recovery.
    async fn heartbeat_or_close_idle(&self, session: &SessionCore) -> Result<(), YbError> {
        let mut guard = session.conn.lock().await;
        let ConnState::Warm(conn) = &*guard else {
            return Ok(()); // Cold: nothing to keep alive.
        };
        // Idle-close: only if a timeout is configured AND still idle under
        // the lock (a user that reopened/used it just now refreshed
        // last_used, so it won't be idle and we keep it warm).
        if let Some(idle_timeout) = self.idle_timeout
            && session.idle_for_at_least(idle_timeout)
        {
            let session_id = session.meta.session_id();
            *guard = ConnState::Cold; // drops the Connection (aborts its tasks)
            drop(guard);
            tracing::info!(
                "yb_tsclient: closing idle session on {} (was session id {session_id})",
                session.peer(),
            );
            return Ok(());
        }
        // Warm and active: keep-alive as before. Clone the Arc so the
        // heartbeat RPC doesn't hold the connection lock (a concurrent
        // user must still be able to warm/use it).
        let conn = Arc::clone(conn);
        drop(guard);
        heartbeat(&conn, session.meta.session_id(), self.rpc_timeout).await?;
        session.touch();
        Ok(())
    }

    fn take_stmt_id(&self) -> u64 {
        self.next_stmt_id.fetch_add(1, Ordering::Relaxed)
    }

    fn alloc_read_serial(&self) -> u64 {
        self.next_read_serial.fetch_add(1, Ordering::Relaxed)
    }

    /// Options shared by every Perform: a fresh `txn_serial_no` (the
    /// escape hatch that makes concurrent Performs on one session legal;
    /// see the module docs) and the read-point history window.
    fn base_options(&self) -> PgPerformOptionsPb {
        let latest_read_serial = self
            .next_read_serial
            .load(Ordering::Relaxed)
            .saturating_sub(1);
        PgPerformOptionsPb {
            txn_serial_no: self.next_txn_serial.fetch_add(1, Ordering::Relaxed),
            read_time_serial_no_history_min: latest_read_serial
                .saturating_sub(READ_SERIAL_HISTORY_WINDOW),
            ..Default::default()
        }
    }

    /// Options for a read Perform on scan `read_serial`: an explicit pin
    /// when the caller supplied one, else `ENSURE_READ_TIME_IS_SET` so
    /// the proxy picks the read point at setup and files it under our
    /// serial for continuations (the two are mutually exclusive on the
    /// wire).
    ///
    /// Proxy-picked read points are CLAMPED (`clamp_uncertainty_window`,
    /// the `yb_read_after_commit_visibility=relaxed` semantics): read ==
    /// global limit, no clock-skew uncertainty window. Unclamped, the
    /// window (`read .. read+max_clock_skew`, 500ms default) stays open
    /// for the WHOLE scan, so any row committed into the scanned range
    /// while a paged scan runs fails a later continuation with
    /// `TryAgainCode restart_read_time` — and a continuation is pinned
    /// to the scan's snapshot, so no retry can ever heal it; rescanning
    /// from scratch under sustained writes livelocks the same way (the
    /// pages-table field hang). Clamping trades that for bounded
    /// staleness: commits acked through ANOTHER tserver within the skew
    /// window may be invisible. Writes acked through THIS client are
    /// always visible (the proxy's hybrid clock is >= every commit it
    /// acked), and explicit pins ([`read_hybrid_time`]) were already
    /// clamped, so scans stay internally consistent either way.
    fn read_options(&self, read_serial: u64, pin_ht: u64) -> PgPerformOptionsPb {
        let mut options = self.base_options();
        options.read_time_serial_no = read_serial;
        if pin_ht != 0 {
            options.read_time = Some(read_hybrid_time(pin_ht));
        } else {
            options.read_time_manipulation = ReadTimeManipulation::EnsureReadTimeIsSet as i32;
            options.clamp_uncertainty_window = true;
        }
        options
    }

    /// One page via one Perform. Continuations carry routing + pinning:
    /// `partition_key` comes from the paging state's `next_partition_key`
    /// (the server only rewrites it for requests its own client layer
    /// owns), and the scan's read serial from the paging envelope keeps
    /// the server on the scan's read point.
    async fn do_read_page(
        &self,
        session: &SessionCore,
        table: &Arc<TableHandle>,
        read: &RangeRead,
        paging_state: Option<Bytes>,
    ) -> Result<ReadPage, YbError> {
        let mut req = read.to_read_request(table, self.take_stmt_id())?;
        let mut sent_position = None;
        // A first page (no paging state) or an EXPLICITLY pinned scan can
        // reopen a Cold session — the former has no pin to lose, the
        // latter carries its own read_time and is restorable on any
        // session. An UNPINNED continuation must NOT reopen: its snapshot
        // lives only in the originating server session's read-point
        // history, which a reopen (new session id) destroys.
        let mut reopen_ok = true;
        let (read_serial, pin_ht) = match paging_state {
            None => (self.alloc_read_serial(), read.read_time_ht),
            Some(bytes) => {
                let envelope = PagingEnvelope::decode(&bytes)?;
                let pin_ht = if read.read_time_ht != 0 {
                    read.read_time_ht
                } else {
                    envelope.pin_ht
                };
                // Unpinned continuations restore their snapshot from the
                // originating session's read-point history; on any other
                // session the server would silently serve them at a NEW
                // read time (see [`PagingEnvelope`]). Fail them instead.
                if pin_ht == 0 && envelope.session_id != session.meta.session_id() {
                    return Err(YbError::other(format!(
                        "paging continuation is bound to server session {} but the \
                         client now holds session {} (recovered mid-scan); restart \
                         the scan",
                        envelope.session_id,
                        session.meta.session_id()
                    )));
                }
                reopen_ok = pin_ht != 0;
                sent_position = Some(ScanPosition::of(&envelope.state));
                req.partition_key = Some(envelope.state.next_partition_key().to_vec());
                req.paging_state = Some(envelope.state);
                (envelope.read_serial, pin_ht)
            }
        };
        let options = self.read_options(read_serial, pin_ht);
        let op = PgPerformOpPb {
            op: Some(pg_perform_op_pb::Op::Read(req)),
            ..Default::default()
        };
        let (resp, sidecars) = self
            .perform_on(session, vec![op], options, reopen_ok)
            .await?;
        let mut responses = resp.responses;
        if responses.len() != 1 {
            return Err(YbError::other(format!(
                "read: expected 1 response, got {}",
                responses.len()
            )));
        }
        let response = responses.remove(0);
        // Continuation progress guard: a follow-up page whose returned
        // paging state sits at the exact position we sent it CANNOT
        // advance — re-issuing it would return the same page forever.
        // Callers drive `paging_state` in a loop (`read_all`, the
        // pageserver chain/scan fetches), so without this check a
        // stuck server position becomes a silent infinite loop: every
        // RPC completes fast, no timeout fires, no retry WARNs — the
        // call site just never returns. Fail loudly and fatally
        // instead (retrying the same continuation cannot help).
        if let (Some(sent), Some(state)) = (&sent_position, &response.paging_state)
            && *sent == ScanPosition::of(state)
        {
            return Err(YbError::other(format!(
                "read continuation made no progress on session {} (server returned \
                 the position it was given: partition_key {:02x?}, row_key {:02x?}); \
                 restart the scan",
                session.meta.session_id(),
                sent.partition_key,
                sent.row_key,
            )));
        }
        page_from_response(
            response,
            &sidecars,
            read.target_types(),
            read_serial,
            pin_ht,
            session.meta.session_id(),
        )
    }

    /// One Perform call on a given session. Serial allocation and frame
    /// submission happen under the session's perform lock so the on-wire
    /// order matches the serial order (the server queues Performs by
    /// strictly increasing `serial_no`).
    async fn perform_on(
        &self,
        session: &SessionCore,
        ops: Vec<PgPerformOpPb>,
        options: PgPerformOptionsPb,
        reopen: bool,
    ) -> Result<(PgPerformResponsePb, Vec<Bytes>), YbError> {
        // Warm the connection first (reopening if the sweeper closed it,
        // unless this is a pinned continuation — see `warm_conn`); this
        // also refreshes last_used so the session isn't a sweep candidate
        // while its call is in flight.
        let conn = self.warm_conn(session, reopen).await?;
        let timeout_millis = u32::try_from(self.rpc_timeout.as_millis()).unwrap_or(u32::MAX);
        let rx = {
            let _order = session.perform_lock.lock().expect("perform lock");
            let req = PgPerformRequestPb {
                session_id: session.meta.session_id(),
                options: Some(options),
                ops,
                serial_no: session.meta.take_serial_no(),
                ..Default::default()
            };
            conn.start_call(SERVICE, "Perform", timeout_millis, &req)?
        };
        let resp = conn
            .await_response(rx, self.rpc_timeout, SERVICE, "Perform")
            .await?;
        let sidecars = resp.sidecars;
        let body = PgPerformResponsePb::decode(resp.body)
            .map_err(|e| YbError::decode(format!("decode Perform response: {e}")))?;
        check_app_status(body.status.as_ref(), "Perform")?;
        Ok((body, sidecars))
    }

    async fn with_retries<T, F, Fut>(&self, what: &str, sel: SessionSel, f: F) -> Result<T, YbError>
    where
        F: Fn(Arc<SessionCore>) -> Fut,
        Fut: std::future::Future<Output = Result<T, YbError>>,
    {
        self.with_retries_bounded(what, sel, MAX_ATTEMPTS, f).await
    }

    /// The retry driver: bounded attempts with jittered exponential
    /// backoff (same constants/shape as `ysql_client`). `Network`-class
    /// failures (transport loss, lost session) additionally run session
    /// recovery before the next attempt. Each attempt re-picks its
    /// session from the current pool by `sel`.
    ///
    /// A `PinnedRead` selection whose session is no longer in the pool
    /// (recovery replaced it) fails fatally: a paged scan's snapshot lives
    /// in that session's server-side read-point history, so it cannot be
    /// continued elsewhere — the caller must restart the scan.
    async fn with_retries_bounded<T, F, Fut>(
        &self,
        what: &str,
        sel: SessionSel,
        max_attempts: usize,
        f: F,
    ) -> Result<T, YbError>
    where
        F: Fn(Arc<SessionCore>) -> Fut,
        Fut: std::future::Future<Output = Result<T, YbError>>,
    {
        let max_attempts = max_attempts.max(1);
        let mut backoff = BACKOFF_INITIAL;
        for attempt in 1..=max_attempts {
            let Some(session) = self.session_for(sel) else {
                let SessionSel::PinnedRead(id) = sel else {
                    unreachable!("only PinnedRead can miss the pool");
                };
                return Err(YbError::other(format!(
                    "paging continuation is bound to server session {id} which the \
                     client no longer holds (recovered mid-scan); restart the scan"
                )));
            };
            let failed_epoch = session.meta.epoch();
            match f(session).await {
                Ok(v) => return Ok(v),
                Err(e) => {
                    if e.is_timeout() {
                        self.observe(ClientObserver::on_call_timeout);
                    }
                    if !e.is_retryable() || attempt == max_attempts {
                        // Every failed attempt is WARNed with its error
                        // kind — including the last: a caller may absorb
                        // this error (fallback lane), and a failure that
                        // burned the whole retry budget must not vanish
                        // with it.
                        tracing::warn!(
                            "yb_tsclient {what} attempt {attempt}/{max_attempts} failed \
                             ({:?}), giving up: {e}",
                            e.class()
                        );
                        return Err(e);
                    }
                    tracing::warn!(
                        "yb_tsclient {what} attempt {attempt}/{max_attempts} failed \
                         ({:?}), retrying in {backoff:?}: {e}",
                        e.class()
                    );
                    if e.class() == ErrorClass::Network
                        && let Err(re) = self.recover(failed_epoch).await
                    {
                        tracing::warn!("yb_tsclient session recovery failed: {re}");
                    }
                    tokio::time::sleep(jittered(backoff)).await;
                    backoff = (backoff * 2).min(BACKOFF_CAP);
                }
            }
        }
        unreachable!("retry loop returns from its last attempt")
    }

    /// Re-establishes the sessions after a connection drop or session
    /// loss: reconnect (each session reconnecting its OWN endpoint first,
    /// walking on only if that node is down), open a fresh pool
    /// (restarting every `serial_no`), and swap the new pool in.
    /// Idempotent per epoch: concurrent callers that observed the same
    /// failed pool produce one recovery. One bad session replaces the
    /// WHOLE pool — every session shares one epoch, so partial replacement
    /// isn't representable; but because each new session prefers its
    /// predecessor's endpoint, a recovery triggered by one dead node keeps
    /// the healthy nodes' sessions on those same nodes and the pool stays
    /// spread.
    ///
    /// Recovery does NOT re-open cached tables. Like the idle-reopen path,
    /// it relies on the hot tables' schema being immutable: the cached
    /// [`TableHandle`] (table id + schema version + column ids) stays
    /// valid across a reconnect, so `Perform` uses it directly and the old
    /// `OpenTable` re-validation loop (which only ever re-read an
    /// unchanging schema) is gone.
    async fn recover(&self, failed_epoch: u64) -> Result<(), YbError> {
        let _guard = self.recovery_lock.lock().await;
        let current_epoch = self.current_pool().epoch();
        if !SessionMeta::recovery_needed(current_epoch, failed_epoch) {
            return Ok(()); // someone already recovered
        }
        let pool = establish_pool(
            &self.endpoints,
            self.rpc_timeout,
            current_epoch + 1,
            self.write_sessions,
        )
        .await?;

        // Prune dead weak table handles (kept for symmetry with the
        // registry's use elsewhere; nothing to re-validate — schema is
        // immutable, so the cached handles stay valid across the reconnect).
        self.tables
            .lock()
            .expect("tables lock")
            .retain(|_, weak| weak.strong_count() > 0);

        let new_id = pool.read_sessions[0].meta.session_id();
        let mut peers: Vec<String> = pool.all().map(|s| s.peer().to_string()).collect();
        let sessions = peers.len();
        peers.sort_unstable();
        peers.dedup();
        let endpoints = peers.len();
        *self.state.write().expect("state lock") = Arc::new(pool);
        self.observe(ClientObserver::on_session_recovery);
        tracing::info!(
            "yb_tsclient: session pool recovered: first read session id {new_id}, \
             {sessions} session(s) across {endpoints} endpoint(s) {peers:?} \
             (epoch {})",
            current_epoch + 1
        );
        Ok(())
    }
}

/// Builds a pool spread across ALL tserver endpoints: one read session
/// per endpoint, plus `write_sessions` dedicated write sessions
/// distributed round-robin across the endpoints — each session its own
/// connection + server-side session.
///
/// Every session targets a specific endpoint index. If that endpoint is
/// down at open time, the session falls over to the next live endpoint
/// (walking the candidate order), and its `endpoint_idx` records where it
/// actually landed — so a later recovery reconnects the endpoint it's on
/// now, and the pool self-heals back toward full spread as nodes return
/// (each endpoint's own read session reconnects it). The whole pool
/// fails to establish only if NO endpoint is usable.
async fn establish_pool(
    endpoints: &Endpoints,
    rpc_timeout: Duration,
    epoch: u64,
    write_sessions: usize,
) -> Result<SessionPool, YbError> {
    let n = endpoints.addrs().len();
    let mut read_slots = Vec::with_capacity(n);
    for idx in 0..n {
        read_slots.push(Arc::new(
            open_session_preferring(endpoints, idx, rpc_timeout, epoch).await?,
        ));
    }
    // Write sessions round-robin across endpoints so bulk writes both
    // parallelize and hit every node.
    let mut write_slots = Vec::with_capacity(write_sessions);
    for w in 0..write_sessions {
        write_slots.push(Arc::new(
            open_session_preferring(endpoints, w % n, rpc_timeout, epoch).await?,
        ));
    }
    Ok(SessionPool::new(read_slots, write_slots))
}

/// One freshly established connection + server-side session, ready to
/// wrap in a [`SessionCore`] or install into a Cold one.
struct EstablishedConn {
    conn: Arc<Connection>,
    session_id: u64,
    instance_id: Vec<u8>,
    /// Which [`Endpoints`] entry the connection actually landed on (it may
    /// have walked past a down node) and that entry's address.
    endpoint_idx: usize,
    endpoint_addr: String,
}

/// The single establish primitive — **connect + `Heartbeat` only, NO
/// `OpenTable`** — shared by the initial pool build, pool recovery, and
/// idle-close reopen. It prefers endpoint `preferred_idx` and walks the
/// rest of the list round-robin on connect failure, returning the
/// endpoint it landed on. Errors only if every endpoint refused.
///
/// Reopen deliberately skips `OpenTable`: the hot tables
/// (`pageserver.{pages,slru_pages,meta}`, `safekeeper.wals`) are created
/// once and never altered, so their `schema_version` is immutable and the
/// already-cached [`TableHandle`] (table id + schema version + column
/// ids) stays valid across any reconnect. Perform uses that handle
/// directly; a fresh server session only needs its id, which the
/// `Heartbeat` returns.
async fn establish_session_conn(
    endpoints: &Endpoints,
    preferred_idx: usize,
    rpc_timeout: Duration,
) -> Result<EstablishedConn, YbError> {
    let addrs = endpoints.addrs();
    let n = addrs.len();
    let mut failures = Vec::new();
    for i in 0..n {
        let idx = (preferred_idx + i) % n;
        match connect_and_open_session(&addrs[idx], rpc_timeout).await {
            Ok((conn, session_id, instance_id)) => {
                return Ok(EstablishedConn {
                    conn,
                    session_id,
                    instance_id,
                    endpoint_idx: idx,
                    endpoint_addr: addrs[idx].clone(),
                });
            }
            Err(e) => {
                tracing::warn!(
                    "yb_tsclient: session open on {} (endpoint {idx}) failed, \
                     trying next endpoint: {e}",
                    addrs[idx]
                );
                failures.push(format!("{} (endpoint {idx}): {e}", addrs[idx]));
            }
        }
    }
    Err(YbError::Unavailable(format!(
        "no tserver endpoint of {:?} usable: [{}]",
        addrs,
        failures.join("; ")
    )))
}

/// [`establish_session_conn`] wrapped into a fresh [`SessionCore`] for the
/// pool build / recovery paths (where a new epoch's slots are created).
async fn open_session_preferring(
    endpoints: &Endpoints,
    preferred_idx: usize,
    rpc_timeout: Duration,
    epoch: u64,
) -> Result<SessionCore, YbError> {
    let e = establish_session_conn(endpoints, preferred_idx, rpc_timeout).await?;
    Ok(SessionCore::new(
        e.conn,
        e.session_id,
        e.instance_id,
        e.endpoint_idx,
        e.endpoint_addr,
        epoch,
    ))
}

/// One connection + one server-side session on `addr` (connect + preamble
/// + a `Heartbeat{session_id:0}` that mints the session). No `OpenTable`.
async fn connect_and_open_session(
    addr: &str,
    rpc_timeout: Duration,
) -> Result<(Arc<Connection>, u64, Vec<u8>), YbError> {
    let conn = Arc::new(Connection::connect(addr, rpc_timeout).await?);
    let resp = heartbeat(&conn, 0, rpc_timeout).await?;
    if resp.session_id == 0 {
        return Err(YbError::other("Heartbeat returned no session_id"));
    }
    Ok((conn, resp.session_id, resp.instance_id))
}

async fn heartbeat(
    conn: &Connection,
    session_id: u64,
    timeout: Duration,
) -> Result<PgHeartbeatResponsePb, YbError> {
    let req = PgHeartbeatRequestPb {
        session_id,
        pid: std::process::id(),
        cluster_config_version: 0,
    };
    let resp = conn.call(SERVICE, "Heartbeat", timeout, &req).await?;
    let resp = PgHeartbeatResponsePb::decode(resp.body)
        .map_err(|e| YbError::decode(format!("decode Heartbeat response: {e}")))?;
    check_app_status(resp.status.as_ref(), "Heartbeat")?;
    Ok(resp)
}

async fn raw_open_table(
    conn: &Connection,
    database_oid: u32,
    table_oid: u32,
    timeout: Duration,
) -> Result<(u32, Vec<crate::table::ColumnMeta>), YbError> {
    let req = PgOpenTableRequestPb {
        table_id: pgsql_table_id(database_oid, table_oid),
        ..Default::default()
    };
    let resp = conn.call(SERVICE, "OpenTable", timeout, &req).await?;
    let resp = PgOpenTableResponsePb::decode(resp.body)
        .map_err(|e| YbError::decode(format!("decode OpenTable response: {e}")))?;
    check_app_status(resp.status.as_ref(), "OpenTable")?;
    let info = resp
        .info
        .ok_or_else(|| YbError::other("OpenTable response missing info"))?;
    if let Some(err) = &info.error {
        return Err(YbError::other(format!("OpenTable master error: {err:?}")));
    }
    let schema = info
        .schema
        .ok_or_else(|| YbError::other("OpenTable response missing schema"))?;
    let version = info
        .version
        .ok_or_else(|| YbError::other("OpenTable response missing version"))?;
    Ok((version, columns_from_schema(&schema.columns)?))
}

/// `ReadHybridTime::FromUint64`: pin read and global limit to `ht`.
fn read_hybrid_time(ht: u64) -> ReadHybridTimePb {
    ReadHybridTimePb {
        read_ht: Some(ht),
        global_limit_ht: Some(ht),
        ..Default::default()
    }
}

/// A scan's resume position as carried in a paging state: the routing
/// key of the tablet to continue on plus the row key to continue from.
/// Two consecutive pages of one scan reporting the SAME position means
/// the scan is stuck — see the progress guard in `do_read_page`.
#[derive(Debug, PartialEq, Eq)]
struct ScanPosition {
    partition_key: Vec<u8>,
    row_key: Vec<u8>,
}

impl ScanPosition {
    fn of(state: &PgsqlPagingStatePb) -> ScanPosition {
        ScanPosition {
            partition_key: state.next_partition_key().to_vec(),
            row_key: state.next_row_key().to_vec(),
        }
    }
}

/// The opaque continuation token [`TsClient::read_page`] hands out: the
/// server's paging state wrapped with this client's scan bookkeeping —
/// the scan's `read_time_serial_no` (server-side snapshot pinning), the
/// explicit pin, if any, and the server session the scan started on.
///
/// The session id is a validity guard: the read point filed under the
/// scan's serial lives in THAT session's `read_point_history_`. On a
/// different (recovered) session the server does not fail an unknown
/// serial — `ENSURE_READ_TIME_IS_SET` silently picks a fresh read time,
/// switching the scan's snapshot mid-flight. `do_read_page` rejects such
/// continuations instead (fatal, non-retryable) so the caller restarts
/// the scan (or falls back) rather than reading a torn cut.
struct PagingEnvelope {
    read_serial: u64,
    pin_ht: u64,
    session_id: u64,
    state: PgsqlPagingStatePb,
}

impl PagingEnvelope {
    const PREFIX_LEN: usize = 24;

    fn encode(&self) -> Bytes {
        let mut buf = Vec::with_capacity(Self::PREFIX_LEN + self.state.encoded_len());
        buf.extend_from_slice(&self.read_serial.to_be_bytes());
        buf.extend_from_slice(&self.pin_ht.to_be_bytes());
        buf.extend_from_slice(&self.session_id.to_be_bytes());
        self.state
            .encode(&mut buf)
            .expect("Vec<u8> encode is infallible");
        Bytes::from(buf)
    }

    fn decode(bytes: &Bytes) -> Result<Self, YbError> {
        if bytes.len() < Self::PREFIX_LEN {
            return Err(YbError::decode(format!(
                "paging state too short: {} bytes",
                bytes.len()
            )));
        }
        let read_serial = u64::from_be_bytes(bytes[0..8].try_into().expect("8 bytes"));
        let pin_ht = u64::from_be_bytes(bytes[8..16].try_into().expect("8 bytes"));
        let session_id = u64::from_be_bytes(bytes[16..24].try_into().expect("8 bytes"));
        let state = PgsqlPagingStatePb::decode(&bytes[Self::PREFIX_LEN..])
            .map_err(|e| YbError::decode(format!("paging state: {e}")))?;
        Ok(PagingEnvelope {
            read_serial,
            pin_ht,
            session_id,
            state,
        })
    }
}

/// Converts one per-op read response into a [`ReadPage`], resolving the
/// rows sidecar and wrapping the paging state into the continuation
/// envelope.
fn page_from_response(
    response: PgsqlResponsePb,
    sidecars: &[Bytes],
    target_types: &[ValueType],
    read_serial: u64,
    pin_ht: u64,
    session_id: u64,
) -> Result<ReadPage, YbError> {
    check_pgsql_response(&response, "read")?;
    let rows_data = match response.rows_data_sidecar {
        Some(idx) => sidecars
            .get(usize::try_from(idx).unwrap_or(usize::MAX))
            .ok_or_else(|| {
                YbError::decode(format!(
                    "rows_data_sidecar {idx} out of {} sidecars",
                    sidecars.len()
                ))
            })?
            .clone(),
        None => Bytes::new(),
    };
    let rows = crate::decode_rows(&rows_data, target_types)?;
    // The server clears paging_state.read_time on this path; honor it
    // anyway should a build ever return it (catalog reads do).
    let mut used_read_time_ht = pin_ht;
    if used_read_time_ht == 0
        && let Some(ps) = &response.paging_state
        && let Some(rt) = &ps.read_time
    {
        used_read_time_ht = rt.read_ht();
    }
    let paging_state = response.paging_state.map(|state| {
        PagingEnvelope {
            read_serial,
            pin_ht,
            session_id,
            state,
        }
        .encode()
    });
    Ok(ReadPage {
        rows,
        paging_state,
        used_read_time_ht,
    })
}

/// Background keep-alive + idle sweeper; also the watchdog that notices a
/// dead connection or lost session between user calls and recovers
/// eagerly.
///
/// Each tick, for every session:
/// - **Cold** (already idle-closed): skip — nothing to keep alive, and a
///   user will reopen it on demand.
/// - **Warm but idle past [`TsClientConfig::idle_timeout`]**: transition
///   Warm → Cold, dropping the `Connection` (its reader/writer abort, the
///   socket closes, and the server-side session lapses on its own expiry)
///   instead of heartbeating. Under the session's connection lock and
///   re-checked idle, so it never races a concurrent user's reopen (that
///   user refreshed `last_used`, so the session is no longer idle) — the
///   lock guarantees at most one of {sweep-close, reopen} wins.
/// - **Warm and recently used**: heartbeat to keep the server session
///   alive, exactly as before.
///
/// With `idle_timeout = None` no session is ever closed, so this reduces
/// to the previous always-warm keep-alive.
fn spawn_heartbeat(client: &Arc<TsClient>) -> tokio::task::JoinHandle<()> {
    let weak = Arc::downgrade(client);
    tokio::spawn(async move {
        let mut ticker = tokio::time::interval(HEARTBEAT_INTERVAL);
        ticker.set_missed_tick_behavior(tokio::time::MissedTickBehavior::Delay);
        ticker.tick().await; // immediate first tick
        loop {
            ticker.tick().await;
            let Some(client) = weak.upgrade() else { return };
            // Every warm slot has its own server-side session with its own
            // idle-expiry clock, so each needs its own keep-alive. A
            // Network failure on any slot recovers the whole pool (one
            // epoch), so stop at the first one.
            let pool = client.current_pool();
            for session in pool.all() {
                match client.heartbeat_or_close_idle(session).await {
                    Ok(()) => {}
                    Err(e) => {
                        tracing::warn!("yb_tsclient: heartbeat failed: {e}");
                        if e.class() == ErrorClass::Network {
                            if let Err(re) = client.recover(pool.epoch()).await {
                                tracing::warn!(
                                    "yb_tsclient: heartbeat-triggered recovery failed: {re}"
                                );
                            }
                            break;
                        }
                    }
                }
            }
            // Drop the strong refs before parking on the next tick so
            // the client can be freed while we sleep.
            drop(pool);
            drop(client);
        }
    })
}

/// 0–25% multiplicative jitter without a rand dependency. A rotating
/// atomic counter picks the percentage: no clock syscall, always safe,
/// and it spreads concurrent retriers across the range (successive calls
/// get successive percentages) rather than clustering on a shared clock.
fn jittered(d: Duration) -> Duration {
    static COUNTER: std::sync::atomic::AtomicU64 = std::sync::atomic::AtomicU64::new(0);
    let pct = COUNTER.fetch_add(1, std::sync::atomic::Ordering::Relaxed) % 26; // 0..=25
    d + Duration::from_nanos((d.as_nanos() as u64).saturating_mul(pct) / 100)
}

#[cfg(test)]
mod tests {
    use super::*;

    #[test]
    fn paging_envelope_round_trip() {
        let envelope = PagingEnvelope {
            read_serial: 42,
            pin_ht: 0xdead_beef_0000_1234,
            session_id: 777,
            state: PgsqlPagingStatePb {
                next_partition_key: Some(vec![0x12, 0x34]),
                next_row_key: Some(b"rowkey".to_vec()),
                total_num_rows_read: Some(99),
                ..Default::default()
            },
        };
        let bytes = envelope.encode();
        let decoded = PagingEnvelope::decode(&bytes).unwrap();
        assert_eq!(decoded.read_serial, 42);
        assert_eq!(decoded.pin_ht, 0xdead_beef_0000_1234);
        assert_eq!(decoded.session_id, 777);
        assert_eq!(decoded.state.next_partition_key(), [0x12, 0x34]);
        assert_eq!(decoded.state.next_row_key(), b"rowkey");
        assert_eq!(decoded.state.total_num_rows_read(), 99);

        // Truncated envelopes are rejected, not misparsed.
        assert!(PagingEnvelope::decode(&bytes.slice(..10)).is_err());
        assert!(PagingEnvelope::decode(&bytes.slice(..PagingEnvelope::PREFIX_LEN - 1)).is_err());
    }

    #[test]
    fn read_hybrid_time_pins_read_and_global_limit() {
        let rht = read_hybrid_time(77);
        assert_eq!(rht.read_ht, Some(77));
        assert_eq!(rht.global_limit_ht, Some(77));
    }

    // ------------------------------------------------------------------
    // Live-transport tests against a scripted in-process "tserver" that
    // speaks just enough of the YB RPC protocol (preamble + framing +
    // Heartbeat) to let a real TsClient connect. Each test scripts the
    // Perform behavior it needs.
    // ------------------------------------------------------------------

    use std::sync::Mutex;
    use std::sync::atomic::AtomicU64;

    use tokio::io::{AsyncReadExt, AsyncWriteExt};
    use tokio::net::TcpStream;

    use crate::pb::yb::rpc::{RequestHeader, ResponseHeader};
    use crate::pb::yb::tserver::PgHeartbeatRequestPb;
    use crate::pb::yb::{PgsqlPagingStatePb as PagingPb, pgsql_response_pb::RequestStatus};
    use crate::rows::ValueType;
    use crate::table::{ColumnKind, ColumnMeta};

    /// How the scripted server answers `Perform` calls.
    #[derive(Clone, Copy)]
    enum PerformScript {
        /// Accept the call, never respond (server-side stall).
        Mute,
        /// Per-op `PGSQL_STATUS_RESTART_REQUIRED_ERROR` for every Perform
        /// carrying the FIRST `read_time_serial_no` ever seen; OK for any
        /// other serial. Models a read-restart pinned to a read time: the
        /// same read time fails forever, a fresh one succeeds.
        RestartFirstReadSerial,
        /// OK with the SAME `paging_state` continuation (and no rows) on
        /// every Perform: a scan position that never advances. Drives
        /// both the session-bound continuation test and the stuck-
        /// continuation progress-guard regression.
        PageWithContinuation,
        /// Plain success: every op returns `PGSQL_STATUS_OK`, no rows, no
        /// continuation. Used by the idle-close/reopen tests where a read
        /// just needs to succeed.
        PlainOk,
        /// Send a valid (large) length prefix for the Perform response,
        /// then stall mid-body forever: the reader task blocks in
        /// `read_exact` on a frame that never completes. Models a
        /// truncated/slow large response — the field shape where
        /// read_page returns big payloads and read_meta doesn't.
        PartialFrameThenStall,
    }

    /// RPC-method call counts the fake tserver accumulates across all its
    /// connections — used to assert, e.g., that an idle reopen issued a
    /// `Heartbeat` but NO `OpenTable`.
    #[derive(Default)]
    struct RpcCounts {
        heartbeat: AtomicU64,
        open_table: AtomicU64,
        perform: AtomicU64,
    }

    struct FakeTserver {
        addr: String,
        /// `read_time_serial_no` of every Perform received, in order.
        read_serials: Arc<Mutex<Vec<u64>>>,
        /// `clamp_uncertainty_window` of every Perform received, in order.
        clamps: Arc<Mutex<Vec<bool>>>,
        /// Per-method call counts (see [`RpcCounts`]).
        counts: Arc<RpcCounts>,
    }

    impl FakeTserver {
        async fn spawn(script: PerformScript) -> FakeTserver {
            let listener = tokio::net::TcpListener::bind("127.0.0.1:0").await.unwrap();
            let addr = listener.local_addr().unwrap().to_string();
            let read_serials = Arc::new(Mutex::new(Vec::new()));
            let clamps = Arc::new(Mutex::new(Vec::new()));
            let counts = Arc::new(RpcCounts::default());
            let serials = Arc::clone(&read_serials);
            let clamps_rec = Arc::clone(&clamps);
            let counts_rec = Arc::clone(&counts);
            let next_session = Arc::new(AtomicU64::new(1));
            tokio::spawn(async move {
                loop {
                    let Ok((stream, _)) = listener.accept().await else {
                        return;
                    };
                    tokio::spawn(serve_conn(
                        stream,
                        script,
                        Arc::clone(&serials),
                        Arc::clone(&clamps_rec),
                        Arc::clone(&counts_rec),
                        Arc::clone(&next_session),
                    ));
                }
            });
            FakeTserver {
                addr,
                read_serials,
                clamps,
                counts,
            }
        }
    }

    async fn serve_conn(
        mut stream: TcpStream,
        script: PerformScript,
        serials: Arc<Mutex<Vec<u64>>>,
        clamps: Arc<Mutex<Vec<bool>>>,
        counts: Arc<RpcCounts>,
        next_session: Arc<AtomicU64>,
    ) {
        let mut preamble = [0u8; 3];
        if stream.read_exact(&mut preamble).await.is_err() {
            return;
        }
        loop {
            let Ok(len) = stream.read_u32().await else {
                return;
            };
            let mut frame = vec![0u8; len as usize];
            if stream.read_exact(&mut frame).await.is_err() {
                return;
            }
            let mut rest = Bytes::from(frame);
            let header_len = prost::encoding::decode_varint(&mut rest).unwrap() as usize;
            let header = RequestHeader::decode(rest.split_to(header_len)).unwrap();
            let _body_len = prost::encoding::decode_varint(&mut rest).unwrap();
            let call_id = header.call_id.unwrap();
            let method = header
                .remote_method
                .map(|m| m.method_name)
                .unwrap_or_default();
            match method.as_str() {
                "Heartbeat" => {
                    counts.heartbeat.fetch_add(1, Ordering::Relaxed);
                    let req = PgHeartbeatRequestPb::decode(rest.clone()).unwrap();
                    let session_id = if req.session_id == 0 {
                        next_session.fetch_add(1, Ordering::Relaxed)
                    } else {
                        req.session_id
                    };
                    let body = PgHeartbeatResponsePb {
                        session_id,
                        ..Default::default()
                    };
                    send_response(&mut stream, call_id, &body).await;
                }
                "OpenTable" => {
                    // The reopen/recovery paths must NOT call this (schema
                    // is immutable); the count lets tests assert that.
                    counts.open_table.fetch_add(1, Ordering::Relaxed);
                    let body = crate::pb::yb::tserver::PgOpenTableResponsePb::default();
                    send_response(&mut stream, call_id, &body).await;
                }
                "Perform" => {
                    counts.perform.fetch_add(1, Ordering::Relaxed);
                    let req = PgPerformRequestPb::decode(rest.clone()).unwrap();
                    let serial = req.options.as_ref().map_or(0, |o| o.read_time_serial_no);
                    clamps.lock().unwrap().push(
                        req.options
                            .as_ref()
                            .is_some_and(|o| o.clamp_uncertainty_window),
                    );
                    let first = {
                        let mut seen = serials.lock().unwrap();
                        seen.push(serial);
                        seen[0]
                    };
                    let op_response = |status: RequestStatus| PgsqlResponsePb {
                        status: Some(status as i32),
                        ..Default::default()
                    };
                    if let PerformScript::PartialFrameThenStall = script {
                        // Announce a big frame, send a few bytes, then
                        // stall: the client reader blocks in read_exact.
                        let _ = stream.write_u32(64 << 20).await;
                        let _ = stream.write_all(&[0u8; 16]).await;
                        let _ = stream.flush().await;
                        std::future::pending::<()>().await;
                        continue;
                    }
                    let responses: Vec<PgsqlResponsePb> = match script {
                        PerformScript::Mute => continue, // stall: no response
                        PerformScript::PartialFrameThenStall => unreachable!(),
                        PerformScript::RestartFirstReadSerial => {
                            let status = if serial == first {
                                RequestStatus::PgsqlStatusRestartRequiredError
                            } else {
                                RequestStatus::PgsqlStatusOk
                            };
                            req.ops.iter().map(|_| op_response(status)).collect()
                        }
                        PerformScript::PageWithContinuation => req
                            .ops
                            .iter()
                            .map(|_| {
                                let mut r = op_response(RequestStatus::PgsqlStatusOk);
                                r.paging_state = Some(PagingPb {
                                    next_partition_key: Some(vec![0x00, 0x01]),
                                    ..Default::default()
                                });
                                r
                            })
                            .collect(),
                        PerformScript::PlainOk => req
                            .ops
                            .iter()
                            .map(|_| op_response(RequestStatus::PgsqlStatusOk))
                            .collect(),
                    };
                    let body = PgPerformResponsePb {
                        responses,
                        ..Default::default()
                    };
                    send_response(&mut stream, call_id, &body).await;
                }
                other => panic!("fake tserver got unexpected method {other:?}"),
            }
        }
    }

    async fn send_response(stream: &mut TcpStream, call_id: i32, body: &impl Message) {
        let header = ResponseHeader {
            call_id,
            is_error: Some(false),
            sidecar_offsets: vec![],
            crc: None,
        };
        let mut hdr = bytes::BytesMut::new();
        header.encode(&mut hdr).unwrap();
        let mut msg = bytes::BytesMut::new();
        body.encode(&mut msg).unwrap();
        let mut inner = bytes::BytesMut::new();
        prost::encoding::encode_varint(hdr.len() as u64, &mut inner);
        inner.extend_from_slice(&hdr);
        prost::encoding::encode_varint(msg.len() as u64, &mut inner);
        inner.extend_from_slice(&msg);
        // Best-effort: the peer may have hung up (end of test).
        let _ = stream.write_u32(inner.len() as u32).await;
        let _ = stream.write_all(&inner).await;
    }

    fn test_table() -> Arc<TableHandle> {
        let col = |name: &str, id: i32, kind: ColumnKind, value_type: ValueType| ColumnMeta {
            name: name.to_string(),
            id,
            kind,
            value_type,
        };
        Arc::new(TableHandle {
            table_id: crate::table::pgsql_table_id(1, 2),
            columns: vec![
                col("branch_id", 0, ColumnKind::Hash, ValueType::Text),
                col("lsn", 1, ColumnKind::Range, ValueType::I64),
                col("payload", 2, ColumnKind::Value, ValueType::Bytea),
            ],
            database_oid: 1,
            table_oid: 2,
            schema_version: std::sync::atomic::AtomicU32::new(3),
        })
    }

    fn test_read(table: &TableHandle) -> RangeRead {
        RangeRead::new(table, vec![crate::ops::BindValue::Text("a".into())])
            .unwrap()
            .targets(table, &["lsn", "payload"])
            .unwrap()
            .limit(10)
    }

    #[derive(Default)]
    struct CountingObserver {
        recoveries: AtomicU64,
        timeouts: AtomicU64,
    }

    impl ClientObserver for CountingObserver {
        fn on_session_recovery(&self) {
            self.recoveries.fetch_add(1, Ordering::Relaxed);
        }
        fn on_call_timeout(&self) {
            self.timeouts.fetch_add(1, Ordering::Relaxed);
        }
    }

    async fn connect_test_client(addr: &str, rpc_timeout: Duration) -> Arc<TsClient> {
        TsClient::connect(TsClientConfig {
            tserver_addrs: vec![addr.to_string()],
            rpc_timeout,
            ..Default::default()
        })
        .await
        .expect("connect to fake tserver")
    }

    async fn connect_test_client_idle(
        addr: &str,
        rpc_timeout: Duration,
        idle_timeout: Option<Duration>,
    ) -> Arc<TsClient> {
        TsClient::connect(TsClientConfig {
            tserver_addrs: vec![addr.to_string()],
            rpc_timeout,
            idle_timeout,
            ..Default::default()
        })
        .await
        .expect("connect to fake tserver")
    }

    /// The incident regression: a server that accepts a read Perform and
    /// never responds must surface a timeout error after bounded time —
    /// each attempt bounded by `rpc_timeout` — never hang, and the
    /// observer must see every per-attempt timeout plus the session
    /// recoveries the retries triggered.
    #[tokio::test(flavor = "multi_thread")]
    async fn read_page_times_out_against_unresponsive_server() {
        let server = FakeTserver::spawn(PerformScript::Mute).await;
        let rpc_timeout = Duration::from_millis(100);
        let client = connect_test_client(&server.addr, rpc_timeout).await;
        let observer = Arc::new(CountingObserver::default());
        client.set_observer(Arc::clone(&observer) as Arc<dyn ClientObserver>);
        let table = test_table();

        let started = std::time::Instant::now();
        let result = client.read_page(&table, &test_read(&table), None).await;
        let elapsed = started.elapsed();

        let err = match result {
            Ok(_) => panic!("stalled read must error, not hang"),
            Err(e) => e,
        };
        assert!(err.is_timeout(), "want a timeout error, got: {err}");
        assert!(
            elapsed >= rpc_timeout,
            "gave up before the rpc timeout: {elapsed:?}"
        );
        // 10 attempts x 100ms + capped backoffs + recoveries; far below
        // this bound unless something hangs.
        assert!(elapsed < Duration::from_secs(30), "took {elapsed:?}");
        assert_eq!(
            observer.timeouts.load(Ordering::Relaxed),
            MAX_ATTEMPTS as u64,
            "every attempt times out and is counted"
        );
        assert!(
            observer.recoveries.load(Ordering::Relaxed) >= 1,
            "timeout attempts recover the session between tries"
        );
    }

    /// Field-shape probe: the server starts a large response frame and
    /// stalls mid-body (reader task blocks in `read_exact`). The
    /// caller-side per-call timeout must STILL fire — a stuck reader
    /// must not turn into an unbounded await. If this hangs, the silent
    /// field stall is reproduced at the transport layer.
    #[tokio::test(flavor = "multi_thread")]
    async fn read_page_times_out_on_partial_response_frame() {
        let server = FakeTserver::spawn(PerformScript::PartialFrameThenStall).await;
        let rpc_timeout = Duration::from_millis(100);
        let client = connect_test_client(&server.addr, rpc_timeout).await;
        let table = test_table();

        let started = std::time::Instant::now();
        let result = tokio::time::timeout(
            Duration::from_secs(30),
            client.read_page(&table, &test_read(&table), None),
        )
        .await;
        let elapsed = started.elapsed();

        match result {
            Err(_) => panic!(
                "read_page HUNG on a partial response frame ({elapsed:?}) — \
                 caller-side timeout did not fire; silent field stall reproduced"
            ),
            Ok(Ok(_)) => panic!("partial frame must not decode to a success"),
            Ok(Err(e)) => assert!(e.is_timeout(), "want a timeout error, got: {e}"),
        }
    }

    /// The multi-node read-restart regression: `TryAgain` retries of a
    /// fresh read must carry a FRESH `read_time_serial_no`. The proxy
    /// only picks a new read time when the serial changes
    /// (`ENSURE_READ_TIME_IS_SET`), so retrying with the stale serial
    /// re-reads at the exact read time that just failed and the restart
    /// error repeats until the retry budget is gone.
    #[tokio::test(flavor = "multi_thread")]
    async fn read_retries_use_a_fresh_read_serial() {
        let server = FakeTserver::spawn(PerformScript::RestartFirstReadSerial).await;
        let client = connect_test_client(&server.addr, Duration::from_secs(5)).await;
        let table = test_table();

        // Multi-op Perform (read_pages): the historical offender — its
        // read serial was allocated once per call, not per attempt.
        let reads = vec![test_read(&table), test_read(&table)];
        let pages = client
            .read_pages(&table, &reads)
            .await
            .expect("retry with a fresh read serial must succeed");
        assert_eq!(pages.len(), 2);

        let serials = server.read_serials.lock().unwrap().clone();
        assert!(
            serials.len() >= 2,
            "expected a retry after the restart error, saw performs: {serials:?}"
        );
        assert_ne!(
            serials[0],
            *serials.last().unwrap(),
            "the retry reused the failed attempt's read serial: {serials:?}"
        );

        // Single-op fresh reads (read_page) allocate per attempt too.
        server.read_serials.lock().unwrap().clear();
        client
            .read_page(&table, &test_read(&table), None)
            .await
            .expect("read_page retry must succeed");
        let serials = server.read_serials.lock().unwrap().clone();
        assert!(serials.len() >= 2 && serials[0] != *serials.last().unwrap());
    }

    /// Unpinned paging continuations are bound to the server session
    /// that created them (their snapshot lives in that session's
    /// read-point history). After a session change they must fail loudly
    /// — the server would otherwise serve them at a silently different
    /// read time.
    #[tokio::test(flavor = "multi_thread")]
    async fn continuation_from_lost_session_fails_loudly() {
        let server = FakeTserver::spawn(PerformScript::PageWithContinuation).await;
        let client = connect_test_client(&server.addr, Duration::from_secs(5)).await;
        let table = test_table();

        let page = client
            .read_page(&table, &test_read(&table), None)
            .await
            .expect("first page");
        let continuation = page.paging_state.expect("scripted continuation");

        // The session id changes out from under the scan (as after a
        // recovery); the buffered continuation is now invalid.
        client.poison_session_for_test();
        let err = match client
            .read_page(&table, &test_read(&table), Some(continuation))
            .await
        {
            Ok(_) => panic!("continuation must not silently switch snapshots"),
            Err(e) => e,
        };
        assert_eq!(err.class(), ErrorClass::Other, "fatal, not retried: {err}");
        assert!(
            err.to_string().contains("session"),
            "unexpected error: {err}"
        );
    }

    /// The no-progress guard: a continuation whose response carries the
    /// exact scan position it was sent can never advance — re-issuing it
    /// returns the same page forever. Without the guard every paging
    /// loop over such a scan (`read_all`, the pageserver chain fetches)
    /// spins silently: fast OK responses, no timeout, no retry WARN.
    /// The guard must fail the read loudly and fatally (`Other`, not
    /// retried — retrying the same continuation cannot help).
    #[tokio::test(flavor = "multi_thread")]
    async fn stuck_continuation_fails_loudly() {
        let server = FakeTserver::spawn(PerformScript::PageWithContinuation).await;
        let client = connect_test_client(&server.addr, Duration::from_secs(5)).await;
        let table = test_table();

        // Page 1 sent no position, so its identical returned state is
        // fine (a fresh scan may legitimately stop anywhere).
        let page = client
            .read_page(&table, &test_read(&table), None)
            .await
            .expect("first page");
        let continuation = page.paging_state.expect("scripted continuation");

        // The continuation comes back at the position it was sent.
        let err = match client
            .read_page(&table, &test_read(&table), Some(continuation))
            .await
        {
            Ok(_) => panic!("a stuck continuation must fail, not loop"),
            Err(e) => e,
        };
        assert_eq!(err.class(), ErrorClass::Other, "fatal, not retried: {err}");
        assert!(
            err.to_string().contains("no progress"),
            "unexpected error: {err}"
        );

        // Exactly two Performs: the guard error must not burn retries.
        assert_eq!(server.read_serials.lock().unwrap().len(), 2);
    }

    /// Unpinned reads must ask the proxy for a CLAMPED read point
    /// (`clamp_uncertainty_window`): with the clock-skew uncertainty
    /// window left open, any row committed into the scanned range while
    /// a paged scan runs fails a later (snapshot-pinned, unhealable)
    /// continuation with a read restart — sustained writes then livelock
    /// every rescan. See `read_options`.
    #[tokio::test(flavor = "multi_thread")]
    async fn unpinned_reads_clamp_the_uncertainty_window() {
        let server = FakeTserver::spawn(PerformScript::PageWithContinuation).await;
        let client = connect_test_client(&server.addr, Duration::from_secs(5)).await;
        let table = test_table();

        client
            .read_page(&table, &test_read(&table), None)
            .await
            .expect("read");
        assert_eq!(*server.clamps.lock().unwrap(), vec![true]);

        // An explicitly pinned read carries `read_time` instead (already
        // clamped: read == global limit); the flag must stay off — the
        // two are mutually exclusive server-side.
        server.clamps.lock().unwrap().clear();
        client
            .read_page(&table, &test_read(&table).read_time_ht(12345), None)
            .await
            .expect("pinned read");
        assert_eq!(*server.clamps.lock().unwrap(), vec![false]);
    }

    /// Idle-close + reopen: after a session sits idle past its
    /// `idle_timeout`, the sweeper drops its `Connection` (Cold); the next
    /// read transparently reconnects (a NEW server session id) and
    /// succeeds — issuing a `Heartbeat` but NO `OpenTable`, because the
    /// cached table handle stays valid across the reopen (schema is
    /// immutable). Uses the sweep test hook instead of waiting out a real
    /// heartbeat interval.
    #[tokio::test(flavor = "multi_thread")]
    async fn idle_close_then_reopen_without_opentable() {
        let server = FakeTserver::spawn(PerformScript::PlainOk).await;
        // Idle timeout well below the elapsed time we'll simulate.
        let client = connect_test_client_idle(
            &server.addr,
            Duration::from_secs(5),
            Some(Duration::from_millis(1)),
        )
        .await;
        let table = test_table();

        // A first read warms + uses the single read session.
        client
            .read_page(&table, &test_read(&table), None)
            .await
            .expect("first read");
        let first_session = client.session_id();
        assert_eq!(client.warm_session_count_for_test().await, 1);
        let opens_before = server.counts.open_table.load(Ordering::Relaxed);
        let hb_before = server.counts.heartbeat.load(Ordering::Relaxed);

        // Go idle past idle_timeout, then sweep: the session goes Cold
        // (its Connection dropped) rather than being heartbeated.
        tokio::time::sleep(Duration::from_millis(20)).await;
        let warm_after_sweep = client.sweep_idle_for_test().await;
        assert_eq!(warm_after_sweep, 0, "idle session must be closed (Cold)");
        assert_eq!(
            server.counts.heartbeat.load(Ordering::Relaxed),
            hb_before,
            "an idle session is closed, not heartbeated"
        );

        // Next read reopens transparently and succeeds.
        client
            .read_page(&table, &test_read(&table), None)
            .await
            .expect("read after idle close must reopen and succeed");
        assert_eq!(client.warm_session_count_for_test().await, 1, "reopened");

        // A NEW server session id is in play, the reopen issued a
        // Heartbeat, and it did NOT call OpenTable.
        assert_ne!(
            client.session_id(),
            first_session,
            "reopen mints a fresh server session id"
        );
        assert!(
            server.counts.heartbeat.load(Ordering::Relaxed) > hb_before,
            "reopen issues a Heartbeat"
        );
        assert_eq!(
            server.counts.open_table.load(Ordering::Relaxed),
            opens_before,
            "reopen must NOT re-run OpenTable (schema is immutable)"
        );
    }

    /// A concurrent burst of reads on a Cold session must reopen it
    /// EXACTLY ONCE — the per-session connection lock funnels the herd so
    /// the first task reconnects and the rest reuse its warmed connection
    /// (no thundering herd of reconnects). Asserted via the Heartbeat
    /// count: one reopen = one extra `Heartbeat{session_id:0}`.
    #[tokio::test(flavor = "multi_thread")]
    async fn concurrent_burst_reopens_cold_session_once() {
        let server = FakeTserver::spawn(PerformScript::PlainOk).await;
        let client = connect_test_client_idle(
            &server.addr,
            Duration::from_secs(5),
            Some(Duration::from_millis(1)),
        )
        .await;
        let table = test_table();

        // Warm then close via the sweeper so exactly one read session is
        // Cold when the burst arrives.
        client
            .read_page(&table, &test_read(&table), None)
            .await
            .expect("warm-up read");
        tokio::time::sleep(Duration::from_millis(20)).await;
        assert_eq!(client.sweep_idle_for_test().await, 0, "closed to Cold");
        let hb_before = server.counts.heartbeat.load(Ordering::Relaxed);

        // Fire many reads at once; all target the single read session.
        let mut tasks = Vec::new();
        for _ in 0..32 {
            let client = Arc::clone(&client);
            let table = table.clone();
            tasks.push(tokio::spawn(async move {
                client.read_page(&table, &test_read(&table), None).await
            }));
        }
        for t in tasks {
            t.await.unwrap().expect("burst read");
        }

        // Exactly one reopen happened: one fresh-session Heartbeat.
        let reopens = server.counts.heartbeat.load(Ordering::Relaxed) - hb_before;
        assert_eq!(
            reopens, 1,
            "a Cold-session burst must reopen exactly once, saw {reopens} Heartbeats"
        );
        assert_eq!(client.warm_session_count_for_test().await, 1);
    }
}
