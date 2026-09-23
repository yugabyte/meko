//! Regression tests for yugabyte/meko#482: a compute backend that disconnects
//! mid-getpage leaves a pooled walredo child permanently poisoned, and the
//! *next, unrelated* caller to draw that slot pays for it.
//!
//! The pool never retires a child at poison time — eviction happens when a
//! later caller draws the slot and errors — so N disconnects leave N landmines
//! in a pool of N. Before the fix those landmines spent the caller's ordinary
//! retry budget, and the error they produced was indistinguishable from a
//! walredo PANIC: the materialiser aborted the whole pageserver on one, and the
//! read path latched `corruption_detected`.
//!
//! These tests need a real walredo subprocess, so they need the PostgreSQL
//! install that CI builds before it runs the unit tests.

use std::num::NonZeroUsize;
use std::sync::Once;
use std::time::Duration;

use bytes::Bytes;
use pageserver::config::PageServerConf;
use pageserver::walredo::{self, PostgresRedoManager, RedoAttemptType};
use pageserver_api::key::Key;
use postgres_ffi::PgMajorVersion;
use tracing::Instrument;
use utils::id::BranchId;
use utils::lsn::Lsn;
use wal_decoder::models::record::NeonWalRecord;

const PG: PgMajorVersion = PgMajorVersion::PG18;

static LOGGING: Once = Once::new();

fn init_logging() {
    LOGGING.call_once(|| {
        utils::logging::init(
            utils::logging::LogFormat::Plain,
            utils::logging::TracingErrorLayerEnablement::EnableWithRustLogFilter,
            utils::logging::Output::Stdout,
        )
        .unwrap();
    });
}

fn sig(pid: u32, signal: &str) {
    let ok = std::process::Command::new("kill")
        .args([signal, &pid.to_string()])
        .status()
        .unwrap()
        .success();
    assert!(ok, "kill {signal} {pid} failed");
}

/// Build a manager over a pool of `pool` lazily-launched children and drive
/// `body` on a current-thread runtime. The span carries `branch_id` because
/// the pageserver's tracing-span assertions require it.
fn run<F>(pool: usize, tag: &str, body: impl FnOnce(PostgresRedoManager) -> F)
where
    F: std::future::Future<Output = ()>,
{
    init_logging();
    let repo_dir =
        camino::Utf8PathBuf::from(format!("/tmp/walredo_poison_{}_{tag}", std::process::id()));
    std::fs::create_dir_all(&repo_dir).unwrap();
    let mut conf = PageServerConf::dummy_conf(repo_dir);
    conf.walredo_pool_size = NonZeroUsize::new(pool).unwrap();
    let conf: &'static PageServerConf = Box::leak(Box::new(conf));
    let branch_id = BranchId::generate();
    let mgr = PostgresRedoManager::new(conf, branch_id);
    let span = tracing::info_span!("test", branch_id = %branch_id);
    tokio::runtime::Builder::new_current_thread()
        .enable_all()
        .build()
        .unwrap()
        .block_on(body(mgr).instrument(span));
}

/// Fill every pool slot with a live child and return their pids in slot order.
async fn fill_pool(mgr: &PostgresRedoManager, pool: usize) -> Vec<u32> {
    for _ in 0..pool {
        mgr.ping(PG).await.expect("baseline ping");
    }
    let pids: Vec<u32> = mgr.status().processes.iter().map(|p| p.pid).collect();
    assert_eq!(pids.len(), pool, "expected a full pool of {pool}");
    pids
}

/// Drop a redo future while the child is SIGSTOPped, i.e. while the caller is
/// parked on the stdout read with the poison guard armed. This is exactly what
/// the page_service executor does to in-flight getpage futures when a compute
/// backend disconnects.
async fn poison(mgr: &PostgresRedoManager, pid: u32) {
    sig(pid, "-STOP");
    let cancelled = tokio::time::timeout(Duration::from_millis(50), mgr.ping(PG)).await;
    sig(pid, "-CONT");
    assert!(
        cancelled.is_err(),
        "the redo future must be cancelled mid-apply, not completed: {cancelled:?}"
    );
}

/// `WAL_REDO_POISON` is one process-global counter, so two tests asserting an
/// exact delta on it must not overlap. `cargo nextest` (what CI runs) gives each
/// test its own process and needs no help; plain `cargo test` shares one, where
/// the tests are only staggered by their differing setup cost. Taken in the
/// synchronous test body, around the whole `run`, so no guard crosses an await.
static METRIC_WINDOW: std::sync::Mutex<()> = std::sync::Mutex::new(());

/// Serialise a test that asserts an exact `WAL_REDO_POISON` delta.
fn metric_window() -> std::sync::MutexGuard<'static, ()> {
    METRIC_WINDOW.lock().unwrap_or_else(|e| e.into_inner())
}

/// Current value of `pageserver_wal_redo_poison_total{outcome=...}`.
fn poison_metric(outcome: &str) -> u64 {
    metrics::gather()
        .iter()
        .filter(|f| f.get_name() == "pageserver_wal_redo_poison_total")
        .flat_map(|f| f.get_metric())
        .find(|m| {
            m.get_label()
                .iter()
                .any(|l| l.get_name() == "outcome" && l.get_value() == outcome)
        })
        .map(|m| m.get_counter().get_value() as u64)
        .unwrap_or(0)
}

/// A WAL record pair copied from `benches/bench_walredo.rs::short_input`, used
/// only to drive `request_redo` down `apply_batch_postgres` — the retry loop
/// under test.
///
/// These records no longer decode against this tree's walredo (yugabyte/meko#1213;
/// the bench is stale at HEAD for the same reason), so the redo currently fails
/// after the pool stops being the reason. The assertions below deliberately do
/// not depend on that: the contract under test is that poison is not what ends
/// the call, which holds whether or not the records themselves apply. Fixing
/// #1213 must not break these tests.
fn lsn(s: &str) -> Lsn {
    <Lsn as std::str::FromStr>::from_str(s).unwrap()
}

#[allow(clippy::octal_escapes)]
fn short_input() -> (Key, Lsn, Vec<(Lsn, NeonWalRecord)>) {
    fn rec(will_init: bool, bytes: &'static [u8]) -> NeonWalRecord {
        NeonWalRecord::Postgres {
            will_init,
            rec: Bytes::from_static(bytes),
        }
    }
    let key = Key {
        field1: 0,
        field2: 1663,
        field3: 13010,
        field4: 1259,
        field5: 0,
        field6: 0,
    };
    let records = vec![
        (
            lsn("0/16A9388"),
            rec(true, b"j\x03\0\0\0\x04\0\0\xe8\x7fj\x01\0\0\0\0\0\n\0\0\xd0\x16\x13Y\0\x10\0\04\x03\xd4\0\x05\x7f\x06\0\0\xd22\0\0\xeb\x04\0\0\0\0\0\0\xff\x03\0\0\0\0\x80\xeca\x01\0\0\x01\0\xd4\0\xa0\x1d\0 \x04 \0\0\0\0/\0\x01\0\xa0\x9dX\x01\0\0\0\0\0\0\0\0\0\0\0\0\0\0\0\0\0\0\0\0\0\0\0\0\0\0\0\0\0\0\0\0\0\0\0\0\0\0\0\0\0\0\0\0\0\0\0\0\0\0\0\0\0\0\0\0\0\0\0\0\0\0\0\0.\0\x01\0\0\0\0\0\0\0\0\0\0\0\0\0\0\0\0\0\0\0\0\0\0\0\0\0\0\0\0\0\0\0\0\0\0\0\0\0\0\0\0\0\0\0\0\0\0\0\0\0\0\0\0\0\0\0\0\0\0\0\0\0\0\0\0\0\0\0\0\0\0\0\0\0\0\0\0\0\0\0\0\0\0\0\0\0\0\0\0\0\0\0\0\0\0\0\0\0\0\0\0\0\0\0\0\00\x9f\x9a\x01P\x9e\xb2\x01\0\x04\0\0\0\0\0\0\0\0\0\0\0\0\0\0\x02\0!\0\x01\x08 \xff\xff\xff?\0\0\0\0\0\0@\0\0another_table\0\0\0\0\0\0\0\0\0\0\0\0\0\0\0\0\0\0\0\0\0\0\0\0\0\0\0\0\0\0\0\0\0\0\0\0\0\0\0\0\0\0\0\0\0\0\0\0\0\0\0\x98\x08\0\0\x02@\0\0\0\0\0\0\n\0\0\0\x02\0\0\0\0@\0\0\0\0\0\0\0\0\0\0\0\0\x80\xbf\0\0\0\0\0\0\0\0\0\0pr\x01\0\0\0\0\0\0\0\0\x01d\0\0\0\0\0\0\x04\0\0\x01\0\0\0\0\0\0\0\x0c\x02\0\0\0\0\0\0\0\0\0\0\0\0\0\0/\0!\x80\x03+ \xff\xff\xff\x7f\0\0\0\0\0\xdf\x04\0\0pg_type\0\0\0\0\0\0\0\0\0\0\0\0\0\0\0\0\0\0\0\0\0\0\0\0\0\0\0\0\0\0\0\0\0\0\0\0\0\0\0\0\0\0\0\0\0\0\0\0\0\0\0\0\0\0\0\0\0\x0b\0\0\0G\0\0\0\0\0\0\0\n\0\0\0\x02\0\0\0\0\0\0\0\0\0\0\0\x0e\0\0\0\0@\x16D\x0e\0\0\0K\x10\0\0\x01\0pr \0\0\0\0\0\0\0\0\x01n\0\0\0\0\0\xd6\x02\0\0\x01\0\0\0[\x01\0\0\0\0\0\0\0\t\x04\0\0\x02\0\0\0\x01\0\0\0\n\0\0\0\n\0\0\0\x7f\0\0\0\0\0\0\0\n\0\0\0\x02\0\0\0\0\0\0C\x01\0\0\x15\x01\0\0\0\0\0\0\0\0\0\0\0\0\0\0.\0!\x80\x03+ \xff\xff\xff\x7f\0\0\0\0\0;\n\0\0pg_statistic\0\0\0\0\0\0\0\0\0\0\0\0\0\0\0\0\0\0\0\0\0\0\0\0\0\0\0\0\0\0\0\0\0\0\0\0\0\0\0\0\0\0\0\0\0\0\0\0\0\0\0\0\x0b\0\0\0\xfd.\0\0\0\0\0\0\n\0\0\0\x02\0\0\0;\n\0\0\0\0\0\0\x13\0\0\0\0\0\xcbC\x13\0\0\0\x18\x0b\0\0\x01\0pr\x1f\0\0\0\0\0\0\0\0\x01n\0\0\0\0\0\xd6\x02\0\0\x01\0\0\0C\x01\0\0\0\0\0\0\0\t\x04\0\0\x01\0\0\0\x01\0\0\0\n\0\0\0\n\0\0\0\x7f\0\0\0\0\0\0\x02\0\x01"),
        ),
        (
            lsn("0/16D4080"),
            rec(false, b"\xbc\0\0\0\0\0\0\0h?m\x01\0\0\0\0p\n\0\09\x08\xa3\xea\0 \x8c\0\x7f\x06\0\0\xd22\0\0\xeb\x04\0\0\0\0\0\0\xff\x02\0@\0\0another_table\0\0\0\0\0\0\0\0\0\0\0\0\0\0\0\0\0\0\0\0\0\0\0\0\0\0\0\0\0\0\0\0\0\0\0\0\0\0\0\0\0\0\0\0\0\0\0\0\0\0\0\x98\x08\0\0\x02@\0\0\0\0\0\0\n\0\0\0\x02\0\0\0\0@\0\0\0\0\0\0\x05\0\0\0\0@zD\x05\0\0\0\0\0\0\0\0\0pr\x01\0\0\0\0\0\0\0\0\x01d\0\0\0\0\0\0\x04\0\0\x01\0\0\0\x02\0"),
        ),
    ];
    (key, lsn("0/16E2408"), records)
}

async fn redo(
    mgr: &PostgresRedoManager,
    attempt: RedoAttemptType,
) -> Result<Bytes, walredo::Error> {
    let (key, lsn, records) = short_input();
    mgr.request_redo(key, lsn, None, records, PG, attempt).await
}

/// The materialiser's attempt type gets 2 tries. With a pool of 2 fully
/// poisoned, both are landmines — so before poison had its own budget this
/// returned `Err`, and `image_materializer.rs` fed exactly that error to
/// `utils::abort::fatal_abort`, killing the pageserver.
#[test]
fn a_poisoned_pool_no_longer_aborts_the_materializer() {
    let _window = metric_window();
    run(2, "materializer", |mgr| async move {
        let pids = fill_pool(&mgr, 2).await;
        for pid in &pids {
            poison(&mgr, *pid).await;
        }
        assert_eq!(
            mgr.status().processes.len(),
            2,
            "nothing retires a poisoned child at poison time"
        );

        let retried = poison_metric("retried");
        let exhausted = poison_metric("exhausted");

        if let Err(e) = redo(&mgr, RedoAttemptType::LegacyCompaction).await {
            assert!(
                !matches!(e, walredo::Error::Poisoned(_)),
                "poison must no longer be the terminal error — this is what the \
                 materialiser fed to fatal_abort: {e:#}"
            );
        }
        assert_eq!(
            poison_metric("retried") - retried,
            2,
            "both poisoned draws are retries, and neither spent max_attempts"
        );
        assert_eq!(poison_metric("exhausted"), exhausted);
    });
}

/// The read path gets 3 tries. A pool of 3 fully poisoned therefore used to
/// exhaust it, and the resulting error was `walredo::Error::Other` — which
/// `reconstruct_value` turns into `critical_timeline!`, latching
/// `corruption_detected` for a routine client disconnect.
#[test]
fn a_poisoned_pool_no_longer_fails_a_read() {
    let _window = metric_window();
    run(3, "read_page", |mgr| async move {
        let pids = fill_pool(&mgr, 3).await;
        for pid in &pids {
            poison(&mgr, *pid).await;
        }

        let retried = poison_metric("retried");
        let exhausted = poison_metric("exhausted");

        if let Err(e) = redo(&mgr, RedoAttemptType::ReadPage).await {
            assert!(
                !matches!(e, walredo::Error::Poisoned(_)),
                "poison must not be what fails the read: {e:#}"
            );
        }
        assert_eq!(
            poison_metric("retried") - retried,
            3,
            "all three poisoned draws are retries; the read path's own \
             attempts stay available for the records"
        );
        assert_eq!(poison_metric("exhausted"), exhausted);
    });
}

/// Why the budget is `2 * pool_size` and not something tighter, and why the
/// `exhausted` branch is unreachable in a quiet pool: a poisoned child is
/// evicted by the first caller to error against it, so the number of poisoned
/// draws one caller can suffer in a row is capped at pool size.
///
/// Exhaustion therefore needs poison arriving *faster than eviction retires
/// it* — a disconnect storm — which is what the 2x margin buys headroom for.
#[test]
fn consecutive_poisoned_draws_are_capped_at_pool_size() {
    const POOL: usize = 4;
    run(POOL, "budget", |mgr| async move {
        let pids = fill_pool(&mgr, POOL).await;
        for pid in &pids {
            poison(&mgr, *pid).await;
        }

        let mut poisoned_draws = 0usize;
        loop {
            match mgr.ping(PG).await {
                Ok(()) => break,
                Err(e) => {
                    assert!(
                        format!("{e:#}").contains("poisoned"),
                        "unexpected error: {e:#}"
                    );
                    poisoned_draws += 1;
                    assert!(poisoned_draws <= 4 * POOL, "loop is not converging");
                }
            }
        }
        assert_eq!(
            poisoned_draws, POOL,
            "with nothing re-poisoning concurrently the ceiling is exactly pool size"
        );
        assert!(
            poisoned_draws < 2 * POOL,
            "and it stays under the budget, so `exhausted` is unreachable here"
        );
    });
}
