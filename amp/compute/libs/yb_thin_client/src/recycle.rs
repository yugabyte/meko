//! Bounded lifetime for a thin-client connection.
//!
//! [`TsClient::connect`] resolves `tserver_addrs` and builds a session pool
//! ONCE; [`TsClient::open_table`] resolves a table's schema against that
//! client. Both are startup work, and until now both lasted as long as the
//! process. That is fine while the tservers keep their addresses and breaks the
//! moment they don't: replace the tserver StatefulSet (a rollout that renames
//! the pods) and a long-lived client holds a pool aimed at addresses nobody
//! answers on, while the endpoints the config actually names — a headless
//! service, a VIP — resolve to the live set the whole time. The read path's
//! twin of this on the YSQL smart driver wedged a pageserver's WAL ingest for
//! 3.5 days (#811).
//!
//! [`Recycling`] closes that gap the cheap way: it re-opens the client (and
//! whatever the caller opened on it) once the current one passes a configured
//! lifetime, re-resolving the configured addresses in the process. It is
//! deliberately time-driven rather than error-driven — an error-driven rebuild
//! only fires once something has already failed, and the failure this is for
//! presents as a hang, not an error.
//!
//! Recycling is best-effort by construction: [`Recycling::current`] cannot
//! fail. A rebuild that does not succeed leaves the existing generation in
//! place and is retried no more than once per [`RECYCLE_RETRY_COOLDOWN`], so a
//! bounded lifetime can only add a reconnect, never take away a working one.

use std::sync::Arc;
use std::time::{Duration, Instant};

use crate::client::{TlsOpts, TsClient, TsClientConfig};
use crate::error::YbError;

/// How long to wait before retrying a rebuild that failed. The current
/// generation keeps serving meanwhile, so this paces the retry only — no
/// caller waits on it.
pub const RECYCLE_RETRY_COOLDOWN: Duration = Duration::from_secs(10);

/// Builds the caller's per-connection state on a freshly-connected client:
/// the table handles it needs, plus the client itself so the bundle is a
/// self-contained generation. Called on a blocking thread — `connect` and
/// `open_table` both issue a synchronous startup RPC.
type BuildFn<T> = Arc<dyn Fn(Arc<TsClient>) -> Result<Arc<T>, YbError> + Send + Sync>;

/// Opens one whole generation: connect, then build. The indirection is what
/// makes the recycle machinery testable — the tests substitute an opener that
/// never touches the shim (a `Recycling` cannot otherwise be constructed
/// without a live tserver).
type OpenFut<T> =
    std::pin::Pin<Box<dyn std::future::Future<Output = Result<Arc<T>, YbError>> + Send>>;
type OpenFn<T> = Arc<dyn Fn() -> OpenFut<T> + Send + Sync>;

struct Live<T> {
    generation: Arc<T>,
    opened_at: Instant,
    /// Set when a rebuild failed: no further rebuild is attempted until then.
    recycle_after: Option<Instant>,
}

/// A thin-client generation — a [`TsClient`] plus the tables opened on it —
/// replaced on a bounded lifetime.
///
/// `T` is the caller's bundle: the pageserver's four page/meta tables, the
/// safekeeper's one `wals` table. It is handed out as an `Arc<T>`, so a caller
/// holding a generation across an await keeps using it even if a recycle
/// happens underneath — the old shim client stays alive until the last
/// `Arc` drops, and in-flight `Perform`s ride it out.
pub struct Recycling<T> {
    live: tokio::sync::RwLock<Live<T>>,
    /// Held by whoever is currently rebuilding. The `live` lock is
    /// deliberately NOT held across a rebuild: `TsClient::connect` is a
    /// startup RPC, bounded only by the shim's RPC timeout, and every read and
    /// write on this lane goes through [`Recycling::current`] — holding a
    /// write lock across that connect would park the hot path on it. So one
    /// caller rebuilds while everyone else keeps using the current generation.
    rebuild: tokio::sync::Mutex<()>,
    open: OpenFn<T>,
    max_lifetime: Option<Duration>,
    /// Bumped by [`Recycling::recycle_now`], and `acked` catches up once a
    /// rebuild that started after the bump succeeds. Counters rather than a
    /// flag: a `recycle_now` landing *during* a rebuild must stay pending, and
    /// clearing a flag afterwards would swallow it.
    forced: std::sync::atomic::AtomicU64,
    acked: std::sync::atomic::AtomicU64,
    /// Names this pool in log lines (`pageserver`, `safekeeper WAL`, ...),
    /// with the addresses a recycle re-resolves.
    what: String,
}

impl<T: Send + Sync + 'static> Recycling<T> {
    /// Connect and open the first generation, failing if that doesn't work —
    /// callers use this at startup, where an unreachable cluster or a missing
    /// table should stop the daemon rather than be discovered later.
    ///
    /// `max_lifetime` of `None` (or zero) never recycles, which is the old
    /// behaviour; see `YbConnConfig::connection_lifetime_ms` for the bound the
    /// daemons pass.
    pub async fn new(
        what: impl Into<String>,
        config: TsClientConfig,
        max_lifetime: Option<Duration>,
        build: impl Fn(Arc<TsClient>) -> Result<Arc<T>, YbError> + Send + Sync + 'static,
    ) -> Result<Arc<Self>, YbError> {
        let build: BuildFn<T> = Arc::new(build);
        let what = format!("{} {:?}", what.into(), config.tserver_addrs);
        // Read before the config moves into the opener. Empty without TLS, and
        // then nothing is watched.
        let tls_paths = config.tls.as_ref().map(TlsOpts::paths).unwrap_or_default();
        let open: OpenFn<T> = Arc::new(move || {
            let config = config.clone();
            let build = build.clone();
            Box::pin(connect_and_build(config, build))
        });
        let generation = (open)().await?;
        let pool = Self::from_parts(generation, open, max_lifetime, what.clone());
        // Self-wiring, so a caller cannot forget it and silently keep serving a
        // certificate that has been renewed underneath it.
        crate::tls_watch::spawn(
            Arc::downgrade(&pool),
            tls_paths,
            what,
            yb_tls::poll_interval(),
        );
        Ok(pool)
    }

    /// Wrap an already-built generation with no lifetime bound — for tests
    /// that construct a client by hand. [`Self::current`] always returns this
    /// generation; nothing is ever rebuilt.
    pub fn fixed(generation: Arc<T>) -> Arc<Self> {
        Self::from_parts(
            generation,
            Arc::new(|| {
                Box::pin(async {
                    Err(YbError::Invalid(
                        "a fixed thin-client generation cannot be rebuilt".into(),
                    ))
                })
            }),
            None,
            "fixed".to_string(),
        )
    }

    fn from_parts(
        generation: Arc<T>,
        open: OpenFn<T>,
        max_lifetime: Option<Duration>,
        what: String,
    ) -> Arc<Self> {
        Arc::new(Recycling {
            live: tokio::sync::RwLock::new(Live {
                generation,
                opened_at: Instant::now(),
                recycle_after: None,
            }),
            rebuild: tokio::sync::Mutex::new(()),
            open,
            max_lifetime: max_lifetime.filter(|l| !l.is_zero()),
            forced: std::sync::atomic::AtomicU64::new(0),
            acked: std::sync::atomic::AtomicU64::new(0),
            what,
        })
    }

    /// Rebuild the generation on its next use, whatever its age — the TLS
    /// files changed and the shim reads them at connect. Best-effort like a
    /// lifetime recycle: a rebuild that fails keeps the current generation and
    /// is retried after [`RECYCLE_RETRY_COOLDOWN`]; the request stands until
    /// one succeeds.
    pub fn recycle_now(&self) {
        self.forced
            .fetch_add(1, std::sync::atomic::Ordering::AcqRel);
    }

    /// Whether a `recycle_now` is outstanding, and which request it is.
    fn forced_request(&self) -> (bool, u64) {
        let want = self.forced.load(std::sync::atomic::Ordering::Acquire);
        (
            want != self.acked.load(std::sync::atomic::Ordering::Acquire),
            want,
        )
    }

    /// The generation to use for the next operation, rebuilding first if the
    /// current one has outlived its lifetime.
    ///
    /// Infallible on purpose: every hot-path call site goes through here, and
    /// a lifetime bound that could inject a new error class into getpage or
    /// WAL flush would be a worse bargain than the staleness it prevents. A
    /// failed rebuild is logged, counted by the caller's own error paths when
    /// the stale generation eventually fails, and retried later.
    ///
    /// At most one caller rebuilds at a time; the rest carry on with the
    /// current generation rather than queueing behind a connect they don't
    /// need to wait for.
    pub async fn current(&self) -> Arc<T> {
        let (generation, due) = {
            let live = self.live.read().await;
            (live.generation.clone(), self.due(&live))
        };
        if !due {
            return generation;
        }
        let Ok(_rebuilding) = self.rebuild.try_lock() else {
            return generation;
        };
        // The previous holder of `rebuild` may have just installed a fresh
        // generation, or installed a cooldown after failing to.
        let age = {
            let live = self.live.read().await;
            if !self.due(&live) {
                return live.generation.clone();
            }
            live.opened_at.elapsed()
        };
        let (forced, request) = self.forced_request();
        match (self.open)().await {
            Ok(fresh) => {
                tracing::info!(
                    "yb_thin_client {}: recycled the connection after {age:?} \
                     ({})",
                    self.what,
                    if forced {
                        "TLS files reloaded, addresses re-resolved"
                    } else {
                        "addresses re-resolved"
                    }
                );
                let mut live = self.live.write().await;
                live.generation = fresh.clone();
                live.opened_at = Instant::now();
                live.recycle_after = None;
                if forced {
                    // Only the request this rebuild started for. A later one
                    // leaves `forced` ahead of `acked`, so it stays due.
                    self.acked
                        .store(request, std::sync::atomic::Ordering::Release);
                }
                fresh
            }
            Err(e) => {
                tracing::warn!(
                    "yb_thin_client {}: could not recycle the connection after {age:?}, \
                     keeping the current one: {e}",
                    self.what
                );
                let mut live = self.live.write().await;
                live.recycle_after = Some(Instant::now() + RECYCLE_RETRY_COOLDOWN);
                live.generation.clone()
            }
        }
    }

    fn due(&self, live: &Live<T>) -> bool {
        due_at(
            live.opened_at,
            self.max_lifetime,
            live.recycle_after,
            Instant::now(),
            self.forced_request().0,
        )
    }
}

/// Connect and build one generation. On a blocking thread: `TsClient::connect`
/// does a synchronous startup RPC and each `open_table` another, and this runs
/// on runtimes that are also serving getpage requests.
async fn connect_and_build<T: Send + Sync + 'static>(
    config: TsClientConfig,
    build: BuildFn<T>,
) -> Result<Arc<T>, YbError> {
    tokio::task::spawn_blocking(move || {
        let client = TsClient::connect(&config)?;
        build(client)
    })
    .await
    .map_err(|e| YbError::Other(format!("thin-client connect task failed: {e}")))?
}

/// The recycle predicate, with the timestamps passed in so it is testable
/// without a live shim client. `max_lifetime: None` is the unbounded case;
/// `forced` is a pending [`Recycling::recycle_now`], due whatever the age;
/// `recycle_after` in the future is the cooldown a failed rebuild installed.
fn due_at(
    opened_at: Instant,
    max_lifetime: Option<Duration>,
    recycle_after: Option<Instant>,
    now: Instant,
    forced: bool,
) -> bool {
    if !forced {
        let Some(lifetime) = max_lifetime else {
            return false;
        };
        if now.saturating_duration_since(opened_at) < lifetime {
            return false;
        }
    }
    match recycle_after {
        Some(at) => now >= at,
        None => true,
    }
}

#[cfg(test)]
mod tests {
    use std::sync::Arc;
    use std::sync::atomic::{AtomicUsize, Ordering};
    use std::time::{Duration, Instant};

    use super::{OpenFn, Recycling, due_at};
    use crate::error::YbError;

    /// A pool over plain `usize` generations, with the shim swapped out: each
    /// rebuild hands out the next integer, or fails if `fail_from` has been
    /// reached. `age` back-dates the first generation so a lifetime of
    /// `lifetime` is already spent.
    async fn pool(
        lifetime: Duration,
        age: Duration,
        fail_from: usize,
    ) -> (Arc<Recycling<usize>>, Arc<AtomicUsize>) {
        let opens = Arc::new(AtomicUsize::new(0));
        let counter = opens.clone();
        let open: OpenFn<usize> = Arc::new(move || {
            let n = counter.fetch_add(1, Ordering::SeqCst) + 1;
            Box::pin(async move {
                if n >= fail_from {
                    Err(YbError::Network("tserver unreachable".into()))
                } else {
                    Ok(Arc::new(n))
                }
            })
        });
        let pool = Recycling::from_parts(Arc::new(0usize), open, Some(lifetime), "test".into());
        // Back-date the first generation instead of sleeping.
        {
            let mut live = pool.live.write().await;
            live.opened_at = Instant::now() - age;
        }
        (pool, opens)
    }

    /// The point of the whole file: a generation past its lifetime is replaced
    /// on next use, and the replacement then serves until *it* ages out.
    #[tokio::test(flavor = "multi_thread")]
    async fn an_expired_generation_is_replaced_on_next_use() {
        let (pool, opens) = pool(
            Duration::from_secs(900),
            Duration::from_secs(901),
            usize::MAX,
        )
        .await;
        assert_eq!(*pool.current().await, 1, "expired → rebuilt");
        assert_eq!(opens.load(Ordering::SeqCst), 1);
        // The fresh generation is young, so the next call must not rebuild.
        assert_eq!(*pool.current().await, 1);
        assert_eq!(opens.load(Ordering::SeqCst), 1, "no rebuild storm");
    }

    /// A rebuild that fails must not take the working connection away, and
    /// must not be retried on every subsequent call.
    #[tokio::test(flavor = "multi_thread")]
    async fn a_failed_rebuild_keeps_the_current_generation() {
        let (pool, opens) = pool(Duration::from_secs(900), Duration::from_secs(901), 1).await;
        assert_eq!(*pool.current().await, 0, "kept the old generation");
        assert_eq!(opens.load(Ordering::SeqCst), 1);
        for _ in 0..5 {
            assert_eq!(*pool.current().await, 0);
        }
        assert_eq!(
            opens.load(Ordering::SeqCst),
            1,
            "the cooldown must suppress further attempts"
        );
    }

    /// Unbounded pools never rebuild, however old the generation.
    #[tokio::test(flavor = "multi_thread")]
    async fn an_unbounded_pool_never_rebuilds() {
        let (pool, opens) = pool(
            Duration::from_secs(900),
            Duration::from_secs(901),
            usize::MAX,
        )
        .await;
        {
            let mut live = pool.live.write().await;
            live.opened_at = Instant::now() - Duration::from_secs(86_400);
        }
        // Rebuild once (it is due), then clear the bound and confirm silence.
        assert_eq!(*pool.current().await, 1);
        let unbounded = Recycling::from_parts(
            Arc::new(0usize),
            Arc::new(|| Box::pin(async { Err(YbError::Invalid("must not open".into())) })),
            None,
            "test".into(),
        );
        assert_eq!(*unbounded.current().await, 0);
        assert_eq!(opens.load(Ordering::SeqCst), 1);
    }

    /// `recycle_now` replaces a generation that is nowhere near its lifetime,
    /// once; a failed forced rebuild keeps the request pending past the
    /// cooldown.
    #[tokio::test(flavor = "multi_thread")]
    async fn recycle_now_replaces_a_young_generation_once() {
        let (pool, opens) = pool(Duration::from_secs(900), Duration::ZERO, usize::MAX).await;
        assert_eq!(*pool.current().await, 0, "young: nothing to do");
        pool.recycle_now();
        assert_eq!(*pool.current().await, 1, "forced → rebuilt");
        assert_eq!(*pool.current().await, 1, "request consumed");
        assert_eq!(opens.load(Ordering::SeqCst), 1);
    }

    /// A second renewal landing while the first rebuild is still connecting
    /// must not be swallowed by that rebuild's own acknowledgement — otherwise
    /// the pool serves the older material until its age deadline.
    #[tokio::test(flavor = "multi_thread")]
    async fn a_recycle_now_during_a_rebuild_stays_pending() {
        let opens = Arc::new(AtomicUsize::new(0));
        let counter = opens.clone();
        let gate = Arc::new(tokio::sync::Notify::new());
        let opened = Arc::new(tokio::sync::Notify::new());
        let (g, o) = (gate.clone(), opened.clone());
        let open: OpenFn<usize> = Arc::new(move || {
            let n = counter.fetch_add(1, Ordering::SeqCst) + 1;
            let (g, o) = (g.clone(), o.clone());
            Box::pin(async move {
                // Only the first forced rebuild waits, so the test can slip a
                // second recycle_now in while it is in flight.
                if n == 1 {
                    o.notify_one();
                    g.notified().await;
                }
                Ok(Arc::new(n))
            })
        });
        let pool = Recycling::from_parts(
            Arc::new(0usize),
            open,
            Some(Duration::from_secs(900)),
            "test".into(),
        );

        pool.recycle_now();
        let p = pool.clone();
        let first = tokio::spawn(async move { *p.current().await });
        opened.notified().await;
        pool.recycle_now(); // arrives mid-connect
        gate.notify_one();
        assert_eq!(first.await.unwrap(), 1, "the in-flight rebuild finished");

        assert_eq!(
            *pool.current().await,
            2,
            "the second request was not dropped"
        );
        assert_eq!(opens.load(Ordering::SeqCst), 2);
        assert_eq!(*pool.current().await, 2, "and then it is consumed");
    }

    #[tokio::test(flavor = "multi_thread")]
    async fn a_failed_forced_rebuild_stays_pending() {
        let (pool, opens) = pool(Duration::from_secs(900), Duration::ZERO, 1).await;
        pool.recycle_now();
        assert_eq!(*pool.current().await, 0, "kept the old generation");
        assert_eq!(opens.load(Ordering::SeqCst), 1);
        // Cooldown suppresses the retry, but the request is still standing.
        assert_eq!(*pool.current().await, 0);
        assert_eq!(opens.load(Ordering::SeqCst), 1);
        {
            let mut live = pool.live.write().await;
            live.recycle_after = Some(Instant::now() - Duration::from_millis(1));
        }
        assert_eq!(*pool.current().await, 0, "still failing");
        assert_eq!(
            opens.load(Ordering::SeqCst),
            2,
            "retried after the cooldown"
        );
    }

    /// The pool wires its own reload: rewriting a watched file rebuilds the
    /// generation with nobody having asked. `Recycling::new` needs a live
    /// tserver, so the watcher is started here the way `new` starts it.
    #[tokio::test(flavor = "multi_thread")]
    async fn a_rewritten_certificate_rebuilds_without_a_caller() {
        let dir = std::env::temp_dir().join(format!("thin-selfwire-{}", std::process::id()));
        std::fs::create_dir_all(&dir).unwrap();
        let cert = dir.join("tls.crt");
        std::fs::write(&cert, b"v1").unwrap();

        // Unbounded lifetime, so a rebuild can only come from the watcher.
        let (pool, opens) = pool(Duration::from_secs(900), Duration::ZERO, usize::MAX).await;
        crate::tls_watch::spawn(
            Arc::downgrade(&pool),
            vec![cert.to_string_lossy().into_owned()],
            "test".into(),
            Duration::from_millis(10),
        );
        assert_eq!(*pool.current().await, 0, "nothing changed yet");

        std::fs::write(&cert, b"v2").unwrap();
        for _ in 0..200 {
            tokio::time::sleep(Duration::from_millis(10)).await;
            if *pool.current().await == 1 {
                break;
            }
        }
        assert_eq!(
            *pool.current().await,
            1,
            "the rewritten file was not noticed"
        );
        assert_eq!(
            opens.load(Ordering::SeqCst),
            1,
            "rebuilt once, not per poll"
        );
    }

    #[test]
    fn unbounded_never_recycles() {
        let now = Instant::now();
        let ancient = now - Duration::from_secs(86_400 * 4);
        assert!(!due_at(ancient, None, None, now, false));
        assert!(due_at(ancient, None, None, now, true), "unless forced");
    }

    #[test]
    fn recycle_is_due_only_past_the_lifetime() {
        let now = Instant::now();
        let opened = now - Duration::from_secs(1_000);
        assert!(!due_at(
            opened,
            Some(Duration::from_secs(2_000)),
            None,
            now,
            false
        ));
        assert!(due_at(
            opened,
            Some(Duration::from_secs(2_000)),
            None,
            now,
            true
        ));
        assert!(due_at(
            opened,
            Some(Duration::from_secs(900)),
            None,
            now,
            false
        ));
    }

    #[test]
    fn a_failed_rebuild_holds_off_until_its_cooldown_expires() {
        let now = Instant::now();
        let opened = now - Duration::from_secs(1_000);
        let lifetime = Some(Duration::from_secs(900));
        assert!(!due_at(
            opened,
            lifetime,
            Some(now + Duration::from_secs(5)),
            now,
            false
        ));
        assert!(
            !due_at(
                opened,
                lifetime,
                Some(now + Duration::from_secs(5)),
                now,
                true
            ),
            "the cooldown also paces a forced rebuild"
        );
        assert!(due_at(
            opened,
            lifetime,
            Some(now - Duration::from_millis(1)),
            now,
            false
        ));
    }

    /// A `fixed` pool hands back the generation it was given and never tries
    /// to rebuild — its build closure would fail if it ever ran.
    #[tokio::test]
    async fn fixed_pool_serves_its_generation() {
        let pool = Recycling::fixed(Arc::new(7u8));
        assert_eq!(*pool.current().await, 7);
        assert_eq!(*pool.current().await, 7);
    }
}
