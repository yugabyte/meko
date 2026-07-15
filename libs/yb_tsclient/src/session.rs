//! Session bookkeeping: endpoint selection/rotation and the per-session
//! state (`session_id`, `serial_no`, epoch) that must be reset whenever
//! the client re-establishes its tserver session.

use std::sync::Arc;
use std::sync::atomic::{AtomicU64, AtomicUsize, Ordering};
use std::time::Duration;

use crate::connection::Connection;
use crate::error::YbError;

/// The tserver endpoint list, reordered once at construction so the
/// preferred endpoint (the configured `preferred_addr` if present, else a
/// loopback address, else the original first entry) sits at index 0.
///
/// The pool opens one read session per endpoint and write sessions
/// round-robin over them, so EVERY endpoint carries traffic regardless of
/// order. The ordering only sets which endpoint each positional session
/// prefers: read session `i` (and write session `i % n`) targets endpoint
/// `i`, failing over to the next only if that node is down. Putting the
/// preferred/loopback endpoint at index 0 means the first read session —
/// the one `connected_addr()`/`session_id()` report and the one used to
/// re-validate tables on recovery — is the co-located node when there is
/// one.
pub(crate) struct Endpoints {
    addrs: Vec<String>,
}

impl Endpoints {
    pub(crate) fn new(mut addrs: Vec<String>, preferred: Option<&str>) -> Result<Self, YbError> {
        if addrs.is_empty() {
            return Err(YbError::invalid("tserver_addrs must not be empty"));
        }
        let start = preferred
            .and_then(|p| addrs.iter().position(|a| a == p))
            .or_else(|| addrs.iter().position(|a| is_local_addr(a)))
            .unwrap_or(0);
        addrs.rotate_left(start);
        Ok(Endpoints { addrs })
    }

    pub(crate) fn addrs(&self) -> &[String] {
        &self.addrs
    }
}

fn is_local_addr(addr: &str) -> bool {
    let mut host = addr.rsplit_once(':').map_or(addr, |(h, _)| h);
    // IPv6 literals arrive bracketed, e.g. `[::1]:9100` -> host `[::1]`.
    if let Some(inner) = host.strip_prefix('[').and_then(|h| h.strip_suffix(']')) {
        host = inner;
    }
    host == "localhost" || host == "::1" || host.starts_with("127.")
}

/// The connection-independent per-session state machine.
///
/// `serial_no` is *per-session* bookkeeping: the server queues Perform
/// requests by strictly increasing serial number
/// (`TsClientSession::RegisterForProcessing`, "Too old request" on
/// reuse), so a fresh session restarts it at 0. `epoch` increases with
/// each recovery so concurrent failures can tell "this session was
/// already replaced" from "recovery still needed".
///
/// The `session_id` is atomic because it is replaced in place by two
/// paths that keep the surrounding [`SessionCore`] alive: the test-only
/// poison hook, and an idle-close *reopen* (a Cold session dropped its
/// connection when it went idle; the next use reconnects and installs the
/// new server session id here, resetting `next_serial_no` to 0). A
/// pool-wide recovery, by contrast, replaces the whole `SessionMeta`.
pub(crate) struct SessionMeta {
    session_id: AtomicU64,
    epoch: u64,
    next_serial_no: AtomicU64,
}

impl SessionMeta {
    pub(crate) fn new(session_id: u64, epoch: u64) -> Self {
        SessionMeta {
            session_id: AtomicU64::new(session_id),
            epoch,
            next_serial_no: AtomicU64::new(0),
        }
    }

    pub(crate) fn session_id(&self) -> u64 {
        self.session_id.load(Ordering::Relaxed)
    }

    pub(crate) fn epoch(&self) -> u64 {
        self.epoch
    }

    /// Whether a failure observed on `failed_epoch` still requires a
    /// recovery, given the currently installed epoch.
    pub(crate) fn recovery_needed(current_epoch: u64, failed_epoch: u64) -> bool {
        current_epoch == failed_epoch
    }

    /// Next Perform serial number; call only while holding the perform
    /// submission lock so wire order matches serial order.
    pub(crate) fn take_serial_no(&self) -> u64 {
        self.next_serial_no.fetch_add(1, Ordering::Relaxed)
    }

    /// Install the server session id from an idle-close *reopen* and
    /// restart the Perform serial counter (a fresh server session numbers
    /// its Performs from 0). Called under the session's connection lock,
    /// so no Perform can be allocating a serial concurrently. The `epoch`
    /// is untouched — reopen re-establishes one session's transport, not
    /// the pool's failure unit.
    pub(crate) fn reopened_with(&self, session_id: u64) {
        self.session_id.store(session_id, Ordering::Relaxed);
        self.next_serial_no.store(0, Ordering::Relaxed);
    }

    /// Test hook: make the server reject this session id.
    pub(crate) fn poison(&self) {
        self.session_id.store(u64::MAX - 1, Ordering::Relaxed);
    }
}

/// A session's connection, which is dropped when the session goes idle
/// and re-established on next use (idle-connection closing). `Cold` holds
/// no `Connection`, so its reader/writer tasks and its TCP socket +
/// server-side session are gone; the next user reconnects.
pub(crate) enum ConnState {
    Warm(Arc<Connection>),
    Cold,
}

/// One PgClientService session slot: a re-establishable connection plus
/// its [`SessionMeta`]. Perform submission (serial allocation + frame
/// enqueue) happens under `perform_lock` so the on-wire order always
/// matches the serial order even with concurrent callers.
///
/// **Idle-close / reopen.** The connection lives behind `conn`, a
/// `tokio::sync::Mutex<ConnState>`. When the session sits idle longer than
/// the configured idle timeout, the heartbeat/sweep task takes that lock
/// and transitions [`ConnState::Warm`] → [`ConnState::Cold`], dropping the
/// `Connection` (its reader/writer abort, the TCP socket closes, and the
/// server's idle session eventually expires). The next user takes the
/// same lock and re-establishes: reconnect + a `Heartbeat{session_id:0}`
/// yields a fresh server session id which lands in `meta`
/// ([`SessionMeta::reopened_with`]) — but NO `OpenTable`, because the hot
/// tables' schema is immutable (the cached [`crate::table::TableHandle`]
/// stays valid across the reopen). The async mutex serializes both the
/// Warm→Cold sweep and any Cold→Warm reopen, so a concurrent burst of
/// users re-establishes exactly once (the rest await and reuse the warmed
/// connection) and the sweeper can never close a connection out from
/// under an in-flight caller.
///
/// `last_used` (epoch millis) is refreshed on every use and drives the
/// idle decision; a session with an in-flight call has a fresh
/// `last_used` by construction, so the sweeper won't pick it.
///
/// `endpoint_idx`/`endpoint_addr` record which [`Endpoints`] entry this
/// session's connection landed on, so recovery can reconnect the SAME
/// tserver first (falling back to the next only if that node is down) —
/// this is what keeps the pool spread across nodes after a single
/// session's recovery instead of collapsing everything onto one — and so
/// diagnostics (`connected_peers`, `endpoint_spread`) can report the peer
/// even while the session is Cold.
pub(crate) struct SessionCore {
    pub(crate) conn: tokio::sync::Mutex<ConnState>,
    pub(crate) meta: SessionMeta,
    #[allow(dead_code)] // kept for diagnostics; reported by Heartbeat
    pub(crate) instance_id: Vec<u8>,
    pub(crate) endpoint_idx: usize,
    pub(crate) endpoint_addr: String,
    pub(crate) perform_lock: std::sync::Mutex<()>,
    last_used: AtomicU64,
}

impl SessionCore {
    pub(crate) fn new(
        conn: Arc<Connection>,
        session_id: u64,
        instance_id: Vec<u8>,
        endpoint_idx: usize,
        endpoint_addr: String,
        epoch: u64,
    ) -> Self {
        SessionCore {
            conn: tokio::sync::Mutex::new(ConnState::Warm(conn)),
            meta: SessionMeta::new(session_id, epoch),
            instance_id,
            endpoint_idx,
            endpoint_addr,
            perform_lock: std::sync::Mutex::new(()),
            last_used: AtomicU64::new(now_millis()),
        }
    }

    /// The tserver this session is bound to. Available while Cold (unlike
    /// the connection's `peer`), so diagnostics never need a warm socket.
    pub(crate) fn peer(&self) -> &str {
        &self.endpoint_addr
    }

    /// Mark the session used *now* — called on every Perform and on every
    /// successful heartbeat, so the idle clock measures real quiescence.
    pub(crate) fn touch(&self) {
        self.last_used.store(now_millis(), Ordering::Relaxed);
    }

    /// Whether the session has been idle for at least `idle_timeout`.
    pub(crate) fn idle_for_at_least(&self, idle_timeout: Duration) -> bool {
        let idle = now_millis().saturating_sub(self.last_used.load(Ordering::Relaxed));
        idle >= idle_timeout.as_millis() as u64
    }
}

/// Monotonic milliseconds since first call, for idle-timeout measurement.
/// Uses `Instant` (not `SystemTime`) so NTP steps or manual wall-clock
/// changes can't make an idle session look freshly used, or force a live
/// session closed early.
fn now_millis() -> u64 {
    static BASE: std::sync::OnceLock<std::time::Instant> = std::sync::OnceLock::new();
    BASE.get_or_init(std::time::Instant::now)
        .elapsed()
        .as_millis() as u64
}

/// The installed session set, spread ACROSS all tserver endpoints so both
/// reads and writes hit every node.
///
/// - `read_sessions`: one session per endpoint. Reads, table opens, and
///   paging continuations run here; `read()` round-robins across them so
///   single-page reads (the getpage hot path) spread over all tservers.
///   A paged scan's continuations must return to the session that pinned
///   their snapshot (server read-point history is per-session), so the
///   client pins a scan to one read session via [`Self::by_session_id`]
///   after page 1 — the round-robin only picks the FIRST page's session.
/// - `write_sessions`: [`TsClientConfig::write_sessions`] dedicated
///   write-only sessions, distributed round-robin across the endpoints
///   at establish time; `write()` round-robins over them so bulk write
///   Performs both parallelize and land on different nodes. Empty when
///   `write_sessions == 0`, in which case writes fall back to the read
///   sessions (still spread, one per endpoint).
///
/// Every session shares one epoch; recovery replaces the whole pool but
/// each new session reconnects its predecessor's endpoint (see
/// [`SessionCore::endpoint_idx`]).
pub(crate) struct SessionPool {
    pub(crate) read_sessions: Vec<Arc<SessionCore>>,
    pub(crate) write_sessions: Vec<Arc<SessionCore>>,
    read_cursor: AtomicUsize,
}

impl SessionPool {
    pub(crate) fn new(
        read_sessions: Vec<Arc<SessionCore>>,
        write_sessions: Vec<Arc<SessionCore>>,
    ) -> Self {
        assert!(
            !read_sessions.is_empty(),
            "a pool needs at least one read session"
        );
        SessionPool {
            read_sessions,
            write_sessions,
            read_cursor: AtomicUsize::new(0),
        }
    }

    /// A read session, round-robin across endpoints. Used for the first
    /// page of a scan and for single-page reads; continuations re-pin via
    /// [`Self::by_session_id`].
    pub(crate) fn read(&self) -> &Arc<SessionCore> {
        let n = self.read_sessions.len();
        let i = self.read_cursor.fetch_add(1, Ordering::Relaxed) % n;
        &self.read_sessions[i]
    }

    /// Write session for round-robin ticket `ticket`, spread across
    /// endpoints. With no dedicated write sessions, writes round-robin
    /// the read sessions instead (still one per endpoint).
    pub(crate) fn write(&self, ticket: usize) -> &Arc<SessionCore> {
        if self.write_sessions.is_empty() {
            let n = self.read_sessions.len();
            return &self.read_sessions[ticket % n];
        }
        &self.write_sessions[ticket % self.write_sessions.len()]
    }

    /// The session with the given server-side session id, if it's in this
    /// pool. Used to re-pin a paged scan's continuations to the exact read
    /// session that created the scan (its snapshot lives in that session's
    /// read-point history). `None` after a recovery replaced the pool —
    /// the caller then fails the continuation loudly.
    pub(crate) fn by_session_id(&self, session_id: u64) -> Option<&Arc<SessionCore>> {
        self.read_sessions
            .iter()
            .chain(&self.write_sessions)
            .find(|s| s.meta.session_id() == session_id)
    }

    /// Every session in the pool (reads then writes) — for heartbeats and
    /// the test poison hook.
    pub(crate) fn all(&self) -> impl Iterator<Item = &Arc<SessionCore>> {
        self.read_sessions.iter().chain(&self.write_sessions)
    }

    pub(crate) fn epoch(&self) -> u64 {
        self.read_sessions[0].meta.epoch()
    }
}

#[cfg(test)]
mod tests {
    use super::*;

    #[test]
    fn endpoints_reject_empty() {
        assert!(Endpoints::new(vec![], None).is_err());
    }

    #[test]
    fn local_addr_detects_loopback_forms() {
        assert!(is_local_addr("127.0.0.1:9100"));
        assert!(is_local_addr("localhost:9100"));
        assert!(is_local_addr("[::1]:9100")); // bracketed IPv6 loopback + port
        assert!(!is_local_addr("10.0.0.2:9100"));
        assert!(!is_local_addr("[2001:db8::1]:9100"));
    }

    #[test]
    fn endpoints_rotate_preferred_to_front_but_keep_all() {
        let addrs = vec![
            "10.0.0.1:9100".to_string(),
            "127.0.0.1:9100".to_string(),
            "10.0.0.2:9100".to_string(),
        ];
        // Configured preference rotates to index 0; every endpoint kept.
        let e = Endpoints::new(addrs.clone(), Some("10.0.0.2:9100")).unwrap();
        assert_eq!(
            e.addrs(),
            ["10.0.0.2:9100", "10.0.0.1:9100", "127.0.0.1:9100"]
        );
        // Otherwise the loopback address leads.
        let e = Endpoints::new(addrs.clone(), None).unwrap();
        assert_eq!(e.addrs()[0], "127.0.0.1:9100");
        assert_eq!(e.addrs().len(), 3);
        // Unknown preference falls back to loopback detection.
        let e = Endpoints::new(addrs.clone(), Some("elsewhere:1")).unwrap();
        assert_eq!(e.addrs()[0], "127.0.0.1:9100");
        // No loopback, no preference: original order preserved.
        let remote = vec!["10.0.0.1:9100".to_string(), "10.0.0.2:9100".to_string()];
        let e = Endpoints::new(remote, None).unwrap();
        assert_eq!(e.addrs(), ["10.0.0.1:9100", "10.0.0.2:9100"]);
    }

    #[test]
    fn serial_no_is_strictly_increasing_and_restarts_per_session() {
        let s1 = SessionMeta::new(101, 0);
        assert_eq!(s1.take_serial_no(), 0);
        assert_eq!(s1.take_serial_no(), 1);
        assert_eq!(s1.take_serial_no(), 2);

        // A fresh session (post-recovery) restarts serials and bumps the
        // epoch.
        let s2 = SessionMeta::new(202, s1.epoch() + 1);
        assert_eq!(s2.take_serial_no(), 0);
        assert_eq!(s2.epoch(), 1);
        assert_eq!(s2.session_id(), 202);
    }

    #[test]
    fn recovery_runs_once_per_epoch() {
        let failed = SessionMeta::new(7, 3);
        // Current epoch still the failed one: recover.
        assert!(SessionMeta::recovery_needed(3, failed.epoch()));
        // Another caller already recovered (epoch moved on): don't.
        assert!(!SessionMeta::recovery_needed(4, failed.epoch()));
    }

    #[test]
    fn poison_changes_session_id() {
        let s = SessionMeta::new(55, 0);
        s.poison();
        assert_ne!(s.session_id(), 55);
    }

    /// Opens `n` throwaway loopback connections (a listener absorbs the
    /// preambles); returns them as [`SessionCore`]s with session ids
    /// `base, base+1, ...`, endpoint index `i % endpoints`, epoch 7.
    async fn test_sessions(n: usize, base: u64, endpoints: usize) -> Vec<Arc<SessionCore>> {
        let listener = tokio::net::TcpListener::bind("127.0.0.1:0").await.unwrap();
        let addr = listener.local_addr().unwrap().to_string();
        tokio::spawn(async move {
            let mut held = Vec::new();
            while let Ok((stream, _)) = listener.accept().await {
                held.push(stream);
            }
        });
        let mut out = Vec::new();
        for i in 0..n {
            let conn = Connection::connect(&addr, std::time::Duration::from_secs(5))
                .await
                .unwrap();
            out.push(Arc::new(SessionCore::new(
                Arc::new(conn),
                base + i as u64,
                vec![],
                i % endpoints,
                addr.clone(),
                7,
            )));
        }
        out
    }

    #[tokio::test]
    async fn pool_single_endpoint_no_write_sessions() {
        // One endpoint, no dedicated write sessions: reads and writes both
        // land on the single read session.
        let reads = test_sessions(1, 100, 1).await;
        let pool = SessionPool::new(reads, Vec::new());
        assert_eq!(pool.read().meta.session_id(), 100);
        for ticket in 0..5 {
            assert_eq!(pool.write(ticket).meta.session_id(), 100);
        }
        assert_eq!(pool.epoch(), 7);
    }

    #[tokio::test]
    async fn pool_reads_round_robin_across_endpoints() {
        // Three endpoints => three read sessions; read() cycles them.
        let reads = test_sessions(3, 100, 3).await;
        let pool = SessionPool::new(reads, Vec::new());
        let picks: Vec<u64> = (0..7).map(|_| pool.read().meta.session_id()).collect();
        assert_eq!(picks, [100, 101, 102, 100, 101, 102, 100]);
        // by_session_id pins a continuation back to its scan's session.
        assert_eq!(pool.by_session_id(101).unwrap().meta.session_id(), 101);
        assert!(pool.by_session_id(999).is_none());
    }

    #[tokio::test]
    async fn idle_state_machine_touch_and_sweep_decision() {
        // A fresh session is Warm and NOT idle.
        let s = &test_sessions(1, 100, 1).await[0];
        assert!(matches!(&*s.conn.lock().await, ConnState::Warm(_)));
        assert!(!s.idle_for_at_least(Duration::from_secs(60)));

        // A zero idle_timeout means "always idle" — the boundary the
        // sweeper uses to decide Warm→Cold.
        assert!(s.idle_for_at_least(Duration::ZERO));

        // Let real (monotonic) time elapse past a short timeout: now idle.
        // touch() resets last_used to the present, clearing it.
        tokio::time::sleep(Duration::from_millis(40)).await;
        assert!(s.idle_for_at_least(Duration::from_millis(20)));
        s.touch();
        assert!(!s.idle_for_at_least(Duration::from_millis(20)));

        // Transition Warm→Cold (as the sweeper does) and back to Warm (as
        // a reopen does), driving a fresh server session id + serial reset.
        *s.conn.lock().await = ConnState::Cold;
        assert!(matches!(&*s.conn.lock().await, ConnState::Cold));
        s.meta.reopened_with(555);
        assert_eq!(s.meta.session_id(), 555);
        assert_eq!(s.meta.take_serial_no(), 0, "reopen restarts serials");
    }

    #[tokio::test]
    async fn pool_writes_round_robin_over_dedicated_sessions() {
        // 3 read sessions (ids 100..103) + 4 write sessions (200..204)
        // spread over 3 endpoints; write tickets cycle the write set.
        let reads = test_sessions(3, 100, 3).await;
        let writes = test_sessions(4, 200, 3).await;
        let pool = SessionPool::new(reads, writes);
        let picks: Vec<u64> = (0..8).map(|t| pool.write(t).meta.session_id()).collect();
        assert_eq!(picks, [200, 201, 202, 203, 200, 201, 202, 203]);
        // Reads never touch the write sessions.
        let read_picks: Vec<u64> = (0..3).map(|_| pool.read().meta.session_id()).collect();
        assert_eq!(read_picks, [100, 101, 102]);
        // `all()` covers reads then writes (heartbeats/poison).
        assert_eq!(pool.all().count(), 7);
    }
}
