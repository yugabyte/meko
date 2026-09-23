//!
//! WAL redo. This service runs PostgreSQL in a special wal_redo mode
//! to apply given WAL records over an old page image and return new
//! page image.
//!
//! We rely on Postgres to perform WAL redo for us. We launch a
//! postgres process in special "wal redo" mode that's similar to
//! single-user mode. We then pass the previous page image, if any,
//! and all the WAL records we want to apply, to the postgres
//! process. Then we get the page image back. Communication with the
//! postgres process happens via stdin/stdout
//!
//! See pgxn/yb_amp_walredo/walredoproc.c for the other side of
//! this communication.
//!
//! The Postgres process is assumed to be secure against malicious WAL
//! records. It achieves it by dropping privileges before replaying
//! any WAL records, so that even if an attacker hijacks the Postgres
//! process, he cannot escape out of it.

/// Process lifecycle and abstracction for the IPC protocol.
mod process;

/// Code to apply [`NeonWalRecord`]s.
pub(crate) mod apply_neon;

use std::future::Future;
use std::sync::Arc;
use std::sync::atomic::{AtomicUsize, Ordering};
use std::time::{Duration, Instant};

use anyhow::Context;
use bytes::{Bytes, BytesMut};
use pageserver_api::key::Key;
use pageserver_api::models::{WalRedoManagerProcessStatus, WalRedoManagerStatus};
use postgres_ffi::PgMajorVersion;
use tracing::*;
use utils::id::BranchId;
use utils::lsn::Lsn;
use utils::sync::gate::GateError;
use utils::sync::heavier_once_cell;
use wal_decoder::models::record::NeonWalRecord;

use crate::config::PageServerConf;
use crate::metrics::{
    WAL_REDO_BYTES_HISTOGRAM, WAL_REDO_PROCESS_LAUNCH_DURATION_HISTOGRAM,
    WAL_REDO_RECORDS_HISTOGRAM, WAL_REDO_TIME, WalRedoPoisonOutcome,
};

/// The real implementation that uses a Postgres process to
/// perform WAL replay.
///
/// Per-tenant pool of walredo subprocesses. A single redo request
/// flows through one slot's process at a time (each subprocess is
/// stdin/stdout-bound), but the per-tenant cap on in-flight redo is
/// `walredo_pool_size` rather than 1. Slots are picked round-robin via
/// [`Self::next_slot`].
pub struct PostgresRedoManager {
    branch_id: BranchId,
    conf: &'static PageServerConf,
    last_redo_at: std::sync::Mutex<Option<Instant>>,
    /// One [`heavier_once_cell`] per pool slot. Each cell:
    ///
    /// 1. coalesces the lazy spawning of its walredo process
    ///    ([`ProcessOnceCell::Spawned`]).
    /// 2. is poisoned to [`ProcessOnceCell::ManagerShutDown`] on
    ///    [`Self::shutdown`] so no new process is spawned.
    ///
    /// Requests use the once-cell only to coalesce launch; they hold
    /// the process via `Arc::clone` so a faulting slot can be evicted
    /// (`take_and_deinit`) without disturbing other concurrent users
    /// of the same `Arc<Process>`.
    redo_pool: Vec<heavier_once_cell::OnceCell<ProcessOnceCell>>,

    /// Round-robin slot picker. Wraps modulo `redo_pool.len()`.
    next_slot: AtomicUsize,

    /// Gate that is entered when launching a walredo process and held open
    /// until the process has been `kill()`ed and `wait()`ed upon.
    ///
    /// Manager shutdown waits for this gate to close after setting every
    /// slot to [`ProcessOnceCell::ManagerShutDown`]. The gate tracks
    /// _processes_, not requests — a process may outlive any single
    /// request because we keep it around until quiesce and because the
    /// `Arc` may be held by concurrent requests.
    launched_processes: utils::sync::gate::Gate,
}

/// See [`PostgresRedoManager::redo_process`].
enum ProcessOnceCell {
    Spawned(Arc<Process>),
    ManagerShutDown,
}

struct Process {
    process: process::WalRedoProcess,
    /// This field is last in this struct so the guard gets dropped _after_ [`Self::process`].
    /// (Reminder: dropping [`Self::process`] synchronously sends SIGKILL and then `wait()`s for it to exit).
    _launched_processes_guard: utils::sync::gate::GateGuard,
}

impl std::ops::Deref for Process {
    type Target = process::WalRedoProcess;

    fn deref(&self) -> &Self::Target {
        &self.process
    }
}

#[derive(Debug, thiserror::Error)]
pub enum Error {
    #[error("cancelled")]
    Cancelled,
    /// An *earlier* caller was dropped mid-request and left this pooled child
    /// unusable. Never caused by this caller's input, so consumers must not
    /// treat it as a corruption signal.
    #[error(transparent)]
    Poisoned(anyhow::Error),
    #[error(transparent)]
    Other(#[from] anyhow::Error),
}

/// Sort a walredo failure into [`Error::Poisoned`] or [`Error::Other`].
///
/// `check_and_arm()` is the only producer of a [`utils::poison::Error`], so
/// finding one means an earlier caller was dropped mid-request. Classify here,
/// because whether a caller can still see the poison depends on what its
/// `.context(..)` makes the anyhow *root*:
///
/// - context over an `anyhow::Error` stacks, leaving the poison as the root
///   where `downcast_ref::<utils::poison::Error>()` finds it. The read and
///   ingest paths rely on that.
/// - context over a typed [`Error`] makes that the root, putting the poison in
///   a struct field where downcast cannot reach it. The materialiser is in
///   this position and matches on the variant instead.
fn classify(e: anyhow::Error) -> Error {
    if is_poison(&e) {
        Error::Poisoned(e)
    } else {
        Error::Other(e)
    }
}

/// Did the pool's poison guard produce this, rather than the records? The one
/// definition, so [`classify`], the dump suppression in
/// [`process::WalRedoProcess::apply_wal_records`] and
/// `PageReconstructError::is_walredo_poison` cannot drift apart.
pub(crate) fn is_poison(e: &anyhow::Error) -> bool {
    e.downcast_ref::<utils::poison::Error>().is_some()
}

macro_rules! bail {
    ($($arg:tt)*) => {
        return Err($crate::walredo::Error::Other(::anyhow::anyhow!($($arg)*)));
    }
}

#[derive(Debug, Clone, Copy)]
pub enum RedoAttemptType {
    /// Used for the read path. Will fire critical errors and retry twice if failure.
    ReadPage,
    // Used for legacy compaction (only used in image compaction). Will fire critical errors and retry once if failure.
    LegacyCompaction,
    // Used for gc compaction. Will not fire critical errors and not retry.
    GcCompaction,
}

impl std::fmt::Display for RedoAttemptType {
    fn fmt(&self, f: &mut std::fmt::Formatter<'_>) -> std::fmt::Result {
        match self {
            RedoAttemptType::ReadPage => write!(f, "read page"),
            RedoAttemptType::LegacyCompaction => write!(f, "legacy compaction"),
            RedoAttemptType::GcCompaction => write!(f, "gc compaction"),
        }
    }
}

/// Retry allowance for one redo, split by cause: retrying a failure the
/// records may have caused is a guess, retrying a poisoned draw is not.
#[derive(Debug, Clone, Copy)]
struct RetryBudget {
    /// For a failure the records may have caused.
    max_attempts: u32,
    /// For drawing a child an earlier caller poisoned. Pool size suffices
    /// when nothing re-poisons concurrently (each failed draw retires that
    /// child); 2x covers `pick_slot` being shared with the materialiser.
    /// Every attempt type gets this. `GcCompaction` has no production
    /// constructor today, so the live callers are `ReadPage` and
    /// `LegacyCompaction`.
    max_poison_attempts: u32,
}

impl RetryBudget {
    fn new(redo_attempt_type: RedoAttemptType, pool_size: usize) -> Self {
        Self {
            max_attempts: match redo_attempt_type {
                RedoAttemptType::ReadPage => 2,
                RedoAttemptType::LegacyCompaction => 1,
                RedoAttemptType::GcCompaction => 0,
            },
            max_poison_attempts: 2 * pool_size as u32,
        }
    }
}

/// What one redo has spent so far, one counter per allowance in
/// [`RetryBudget`].
#[derive(Debug, Default, Clone, Copy)]
struct Attempts {
    /// Failures the records may have caused.
    hard: u32,
    /// Draws of a child an earlier caller poisoned.
    poison: u32,
}

/// Charge one failed attempt to the allowance it belongs to. `Ok` means retry,
/// `Err` is terminal and is what the caller returns.
fn spend(e: Error, n: &mut Attempts, budget: RetryBudget) -> Result<(), Error> {
    match e {
        // The failed draw already evicted that child, so the retry draws
        // another one.
        Error::Poisoned(e) => {
            n.poison += 1;
            if n.poison > budget.max_poison_attempts {
                WalRedoPoisonOutcome::Exhausted.inc();
                return Err(Error::Poisoned(e));
            }
            WalRedoPoisonOutcome::Retried.inc();
            Ok(())
        }
        // Last error wins. An earlier hard error is not carried forward: its
        // child is already retired, and if the records are the problem they
        // fail again on the next attempt.
        e => {
            n.hard += 1;
            if n.hard > budget.max_attempts {
                return Err(e);
            }
            Ok(())
        }
    }
}

///
/// Public interface of WAL redo manager
///
impl PostgresRedoManager {
    ///
    /// Request the WAL redo manager to apply some WAL records
    ///
    /// The WAL redo is handled by a separate thread, so this just sends a request
    /// to the thread and waits for response.
    ///
    /// # Cancel-Safety
    ///
    /// This method is cancellation-safe.
    pub async fn request_redo(
        &self,
        key: Key,
        lsn: Lsn,
        base_img: Option<(Lsn, Bytes)>,
        records: Vec<(Lsn, NeonWalRecord)>,
        pg_version: PgMajorVersion,
        redo_attempt_type: RedoAttemptType,
    ) -> Result<Bytes, Error> {
        if records.is_empty() {
            bail!("invalid WAL redo request with no records");
        }

        let budget = RetryBudget::new(redo_attempt_type, self.redo_pool.len());

        let base_img_lsn = base_img.as_ref().map(|p| p.0).unwrap_or(Lsn::INVALID);
        let mut img = base_img.map(|p| p.1);
        let mut batch_neon = apply_neon::can_apply_in_neon(&records[0].1);
        let mut batch_start = 0;
        for (i, record) in records.iter().enumerate().skip(1) {
            let rec_neon = apply_neon::can_apply_in_neon(&record.1);

            if rec_neon != batch_neon {
                let result = if batch_neon {
                    self.apply_batch_neon(key, lsn, img, &records[batch_start..i])
                } else {
                    self.apply_batch_postgres(
                        key,
                        lsn,
                        img,
                        base_img_lsn,
                        &records[batch_start..i],
                        self.conf.wal_redo_timeout,
                        pg_version,
                        budget,
                        redo_attempt_type,
                    )
                    .await
                };
                img = Some(result?);

                batch_neon = rec_neon;
                batch_start = i;
            }
        }
        // last batch
        if batch_neon {
            self.apply_batch_neon(key, lsn, img, &records[batch_start..])
        } else {
            self.apply_batch_postgres(
                key,
                lsn,
                img,
                base_img_lsn,
                &records[batch_start..],
                self.conf.wal_redo_timeout,
                pg_version,
                budget,
                redo_attempt_type,
            )
            .await
        }
    }

    /// Do a ping request-response roundtrip.
    ///
    /// Not used in production, but by Rust benchmarks.
    ///
    /// # Cancel-Safety
    ///
    /// This method is cancellation-safe.
    pub async fn ping(&self, pg_version: PgMajorVersion) -> Result<(), Error> {
        self.do_with_walredo_process(pg_version, |proc| async move {
            proc.ping(Duration::from_secs(1)).await.map_err(classify)
        })
        .await
    }

    pub fn status(&self) -> WalRedoManagerStatus {
        WalRedoManagerStatus {
            last_redo_at: {
                let at = *self.last_redo_at.lock().unwrap();
                at.and_then(|at| {
                    let age = at.elapsed();
                    // map any chrono errors silently to None here
                    chrono::Utc::now().checked_sub_signed(chrono::Duration::from_std(age).ok()?)
                })
            },
            processes: self
                .redo_pool
                .iter()
                .filter_map(|slot| {
                    slot.get().and_then(|p| match &*p {
                        ProcessOnceCell::Spawned(p) => {
                            Some(WalRedoManagerProcessStatus { pid: p.id() })
                        }
                        ProcessOnceCell::ManagerShutDown => None,
                    })
                })
                .collect(),
        }
    }
}

impl PostgresRedoManager {
    ///
    /// Create a new PostgresRedoManager.
    ///
    pub fn new(conf: &'static PageServerConf, branch_id: BranchId) -> PostgresRedoManager {
        // The actual processes are launched lazily, on first request to each slot.
        let pool_size = conf.walredo_pool_size.get();
        let redo_pool = (0..pool_size)
            .map(|_| heavier_once_cell::OnceCell::default())
            .collect();
        PostgresRedoManager {
            branch_id,
            conf,
            last_redo_at: std::sync::Mutex::default(),
            redo_pool,
            next_slot: AtomicUsize::new(0),
            launched_processes: utils::sync::gate::Gate::default(),
        }
    }

    /// Shut down the WAL redo manager.
    ///
    /// Returns `true` if this call was the one that initiated shutdown.
    /// `true` may be observed by no caller if the first caller stops polling.
    ///
    /// After this future completes
    /// - no redo process is running
    /// - no new redo process will be spawned
    /// - redo requests that need walredo process will fail with [`Error::Cancelled`]
    /// - [`apply_neon`]-only redo requests may still work, but this may change in the future
    ///
    /// # Cancel-Safety
    ///
    /// This method is cancellation-safe.
    pub async fn shutdown(&self) -> bool {
        // Poison every slot so no slot can spawn a new process. Track
        // whether *any* slot transitioned (i.e. we initiated shutdown).
        let mut any_transitioned = false;
        for slot in &self.redo_pool {
            let maybe_permit = match slot.get_or_init_detached().await {
                Ok(guard) => {
                    if matches!(&*guard, ProcessOnceCell::ManagerShutDown) {
                        None
                    } else {
                        let (proc, permit) = guard.take_and_deinit();
                        drop(proc); // drops the Arc; refcount may not be zero yet
                        Some(permit)
                    }
                }
                Err(permit) => Some(permit),
            };
            if let Some(permit) = maybe_permit {
                slot.set(ProcessOnceCell::ManagerShutDown, permit);
                any_transitioned = true;
            }
        }
        // wait for ongoing requests to drain and the refcounts of all Arc<WalRedoProcess> that
        // we ever launched to drop to zero, which when it happens synchronously kill()s & wait()s
        // for the underlying process.
        self.launched_processes.close().await;
        any_transitioned
    }

    /// This type doesn't have its own background task to check for idleness: we
    /// rely on our owner calling this function periodically in its own housekeeping
    /// loops.
    pub(crate) fn maybe_quiesce(&self, idle_timeout: Duration) {
        if let Ok(g) = self.last_redo_at.try_lock() {
            if let Some(last_redo_at) = *g {
                if last_redo_at.elapsed() >= idle_timeout {
                    drop(g);
                    for slot in &self.redo_pool {
                        drop(slot.get().map(|guard| guard.take_and_deinit()));
                    }
                }
            }
        }
    }

    /// # Cancel-Safety
    ///
    /// This method is cancel-safe iff `closure` is cancel-safe.
    /// Pick a pool slot in round-robin order. Wraps modulo
    /// `redo_pool.len()`; safe to call concurrently.
    fn pick_slot(&self) -> usize {
        let idx = self.next_slot.fetch_add(1, Ordering::Relaxed);
        idx % self.redo_pool.len()
    }

    async fn do_with_walredo_process<
        F: FnOnce(Arc<Process>) -> Fut,
        Fut: Future<Output = Result<O, Error>>,
        O,
    >(
        &self,
        pg_version: PgMajorVersion,
        closure: F,
    ) -> Result<O, Error> {
        let slot_idx = self.pick_slot();
        let slot = &self.redo_pool[slot_idx];
        let proc: Arc<Process> = match slot.get_or_init_detached().await {
            Ok(guard) => match &*guard {
                ProcessOnceCell::Spawned(proc) => Arc::clone(proc),
                ProcessOnceCell::ManagerShutDown => {
                    return Err(Error::Cancelled);
                }
            },
            Err(permit) => {
                let start = Instant::now();
                // acquire guard before spawning process, so that we don't spawn new processes
                // if the gate is already closed.
                let _launched_processes_guard = match self.launched_processes.enter() {
                    Ok(guard) => guard,
                    Err(GateError::GateClosed) => unreachable!(
                        "shutdown sets every once cell to `ManagerShutDown` state before closing the gate"
                    ),
                };
                let proc = Arc::new(Process {
                    process: process::WalRedoProcess::launch(self.conf, self.branch_id, pg_version)
                        .context("launch walredo process")?,
                    _launched_processes_guard,
                });
                let duration = start.elapsed();
                WAL_REDO_PROCESS_LAUNCH_DURATION_HISTOGRAM.observe(duration.as_secs_f64());
                info!(
                    elapsed_ms = duration.as_millis(),
                    pid = proc.id(),
                    slot = slot_idx,
                    "launched walredo process"
                );
                slot.set(ProcessOnceCell::Spawned(Arc::clone(&proc)), permit);
                proc
            }
        };

        // async closures are unstable, would support &Process
        let result = closure(proc.clone()).await;

        if result.is_err() {
            // Take the faulty process out of its slot. Other concurrent
            // tasks may still hold the same `Arc<Process>`; the last one
            // to drop it will SIGKILL/wait the subprocess.
            //
            // NB: drop blocks on wait(). If walredo always errors
            // immediately, this caps runaway respawn at
            // $num_runtimes * $num_executor_threads.
            if let Some(guard) = slot.get() {
                match &*guard {
                    ProcessOnceCell::ManagerShutDown => {}
                    ProcessOnceCell::Spawned(guard_proc) => {
                        if Arc::ptr_eq(&proc, guard_proc) {
                            // First to observe the error — take it out of rotation.
                            guard.take_and_deinit();
                        } else {
                            // Another task already evicted this slot and put a fresh
                            // process there. Do nothing — our view is stale.
                        }
                    }
                }
            }
            // The last task to drop `proc` runs the blocking wait().
            drop(proc);
        }

        result
    }

    ///
    /// Process one request for WAL redo using wal-redo postgres
    ///
    /// # Cancel-Safety
    ///
    /// Cancellation safe.
    #[allow(clippy::too_many_arguments)]
    async fn apply_batch_postgres(
        &self,
        key: Key,
        lsn: Lsn,
        base_img: Option<Bytes>,
        base_img_lsn: Lsn,
        records: &[(Lsn, NeonWalRecord)],
        wal_redo_timeout: Duration,
        pg_version: PgMajorVersion,
        budget: RetryBudget,
        redo_attempt_type: RedoAttemptType,
    ) -> Result<Bytes, Error> {
        *(self.last_redo_at.lock().unwrap()) = Some(Instant::now());

        let (rel, blknum) = key.to_rel_block().context("invalid record")?;
        let mut attempts = Attempts::default();
        loop {
            let base_img = &base_img;
            let closure = |proc: Arc<Process>| async move {
                let started_at = std::time::Instant::now();

                // Relational WAL records are applied using wal-redo-postgres
                let result = proc
                    .apply_wal_records(rel, blknum, base_img, records, wal_redo_timeout)
                    .await
                    .context("apply_wal_records");

                let duration = started_at.elapsed();

                let len = records.len();
                let nbytes = records.iter().fold(0, |acumulator, record| {
                    acumulator
                        + match &record.1 {
                            NeonWalRecord::Postgres { rec, .. } => rec.len(),
                            _ => unreachable!("Only PostgreSQL records are accepted in this batch"),
                        }
                });

                WAL_REDO_TIME.observe(duration.as_secs_f64());
                WAL_REDO_RECORDS_HISTOGRAM.observe(len as f64);
                WAL_REDO_BYTES_HISTOGRAM.observe(nbytes as f64);

                debug!(
                    "postgres applied {} WAL records ({} bytes) in {} us to reconstruct page image at LSN {}",
                    len,
                    nbytes,
                    duration.as_micros(),
                    lsn
                );

                let result = result.map_err(classify);
                if let Err(e) = result.as_ref() {
                    macro_rules! message {
                        ($level:tt, $err:expr) => {
                            $level!(
                                "error applying {} WAL records {}..{} ({} bytes) to key {} during {}, from base image with LSN {} to reconstruct page image at LSN {} n_attempts={} n_poison_attempts={}: {:?}",
                                records.len(),
                                records.first().map(|p| p.0).unwrap_or(Lsn(0)),
                                records.last().map(|p| p.0).unwrap_or(Lsn(0)),
                                nbytes,
                                key,
                                redo_attempt_type,
                                base_img_lsn,
                                lsn,
                                attempts.hard,
                                attempts.poison,
                                $err,
                            )
                        }
                    }
                    match e {
                        // Expected and self-healing: the retry draws another child.
                        Error::Poisoned(e) => message!(warn, e),
                        Error::Other(e) => match redo_attempt_type {
                            RedoAttemptType::ReadPage => message!(error, e),
                            RedoAttemptType::LegacyCompaction => message!(error, e),
                            RedoAttemptType::GcCompaction => message!(warn, e),
                        },
                        // `classify` yields only the two above.
                        Error::Cancelled => {}
                    }
                }

                result
            };

            // Bound first: the closure borrows `attempts`, and that borrow must
            // end before `spend` takes it mutably.
            let result = self.do_with_walredo_process(pg_version, closure).await;

            match result {
                Ok(img) => {
                    if attempts.hard != 0 || attempts.poison != 0 {
                        info!(
                            n_attempts = attempts.hard,
                            n_poison_attempts = attempts.poison,
                            "retried walredo succeeded"
                        );
                    }
                    return Ok(img);
                }
                Err(e) => spend(e, &mut attempts, budget)?,
            }
        }
    }

    ///
    /// Process a batch of WAL records using bespoken Neon code.
    ///
    fn apply_batch_neon(
        &self,
        key: Key,
        lsn: Lsn,
        base_img: Option<Bytes>,
        records: &[(Lsn, NeonWalRecord)],
    ) -> Result<Bytes, Error> {
        let start_time = Instant::now();

        let mut page = BytesMut::new();
        if let Some(fpi) = base_img {
            // If full-page image is provided, then use it...
            page.extend_from_slice(&fpi[..]);
        } else {
            // All the current WAL record types that we can handle require a base image.
            bail!("invalid neon WAL redo request with no base image");
        }

        // Apply all the WAL records in the batch
        for (record_lsn, record) in records.iter() {
            self.apply_record_neon(key, &mut page, *record_lsn, record)?;
        }
        // Success!
        let duration = start_time.elapsed();
        // FIXME: using the same metric here creates a bimodal distribution by default, and because
        // there could be multiple batch sizes this would be N+1 modal.
        WAL_REDO_TIME.observe(duration.as_secs_f64());

        debug!(
            "neon applied {} WAL records in {} us to reconstruct page image at LSN {}",
            records.len(),
            duration.as_micros(),
            lsn
        );

        Ok(page.freeze())
    }

    fn apply_record_neon(
        &self,
        key: Key,
        page: &mut BytesMut,
        record_lsn: Lsn,
        record: &NeonWalRecord,
    ) -> anyhow::Result<()> {
        apply_neon::apply_in_neon(record, record_lsn, key, page)?;

        Ok(())
    }
}

#[cfg(test)]
mod tests {
    use anyhow::Context;

    use super::{Error, classify};
    use crate::metrics::WAL_REDO_POISON;

    /// The shape `check_and_arm()` produces, once a caller's `.context(..)` has
    /// stacked on top of it.
    fn poisoned() -> Error {
        classify(
            anyhow::Error::from(utils::poison::Error::Poisoned {
                what: "stdout",
                at: chrono::Utc::now(),
            })
            .context("apply_wal_records"),
        )
    }

    /// The safety-critical direction: a failure the records caused must never
    /// be classified as poison, or the materializer would skip a job that
    /// signalled corruption.
    #[test]
    fn classify_sorts_poison_from_everything_else() {
        let poisoned = anyhow::Error::from(utils::poison::Error::Poisoned {
            what: "stdout",
            at: chrono::Utc::now(),
        })
        .context("apply_wal_records");
        assert!(matches!(classify(poisoned), Error::Poisoned(_)));

        // What a PANIC'd, OOM-killed or externally SIGKILLed child looks like.
        let eof = anyhow::Error::new(std::io::Error::from(std::io::ErrorKind::UnexpectedEof))
            .context("read walredo stdout")
            .context("apply_wal_records");
        assert!(matches!(classify(eof), Error::Other(_)));

        let timeout = anyhow::anyhow!("WAL redo timed out").context("apply_wal_records");
        assert!(matches!(classify(timeout), Error::Other(_)));
    }

    /// Poison gets its own allowance so a disconnect storm cannot spend the
    /// budget meant for failures the records may have caused.
    #[test]
    fn poison_gets_its_own_budget_for_every_caller() {
        use super::{RedoAttemptType, RetryBudget};

        for (attempt, expected) in [
            (RedoAttemptType::ReadPage, 2),
            (RedoAttemptType::LegacyCompaction, 1),
            (RedoAttemptType::GcCompaction, 0),
        ] {
            let b = RetryBudget::new(attempt, 4);
            assert_eq!(b.max_attempts, expected, "{attempt}");
            // Including GcCompaction, which retries no records but is still
            // not at fault for another caller's poison.
            assert_eq!(b.max_poison_attempts, 8, "{attempt}");
        }
    }

    /// The branch a disconnect storm reaches: the budget runs out and the
    /// caller is still told it was the *pool* that failed. `Error::Other` here
    /// is what latches `corruption_detected` for a client disconnect.
    #[test]
    fn an_exhausted_poison_budget_still_returns_poison() {
        use super::{Attempts, RedoAttemptType, RetryBudget, spend};

        // Pool of 1, so the allowance is 2 and the third draw exhausts it.
        let budget = RetryBudget::new(RedoAttemptType::ReadPage, 1);
        let mut n = Attempts::default();
        let exhausted = || WAL_REDO_POISON.with_label_values(&["exhausted"]).get();
        let before = exhausted();

        assert!(spend(poisoned(), &mut n, budget).is_ok());
        assert!(spend(poisoned(), &mut n, budget).is_ok());
        let terminal = spend(poisoned(), &mut n, budget)
            .expect_err("three poisoned draws must outlast an allowance of two");

        assert!(
            matches!(terminal, Error::Poisoned(_)),
            "an exhausted allowance must not masquerade as a corruption signal: {terminal:#}"
        );
        assert_eq!(exhausted() - before, 1);
        assert_eq!(
            n.hard, 0,
            "poison must not spend the retries reserved for the records"
        );
    }

    /// The other direction: a failure the records may have caused spends the
    /// ordinary allowance and leaves the poison one untouched.
    #[test]
    fn a_hard_failure_spends_its_own_budget() {
        use super::{Attempts, RedoAttemptType, RetryBudget, spend};

        let budget = RetryBudget::new(RedoAttemptType::ReadPage, 1);
        let mut n = Attempts::default();
        let hard = || Error::Other(anyhow::anyhow!("WAL redo timed out"));

        assert!(spend(hard(), &mut n, budget).is_ok());
        assert!(spend(hard(), &mut n, budget).is_ok());
        assert!(
            spend(hard(), &mut n, budget).is_err(),
            "three failures must outlast max_attempts=2"
        );
        assert_eq!(n.poison, 0);
    }

    /// What the materializer sees after `redo_one`'s `.with_context(..)`: the
    /// `poison::Error` is out of reach, so it must match on the variant.
    #[test]
    fn the_variant_survives_a_callers_context() {
        let e = classify(
            anyhow::Error::from(utils::poison::Error::Poisoned {
                what: "stdout",
                at: chrono::Utc::now(),
            })
            .context("apply_wal_records"),
        );
        let wrapped = Err::<(), _>(e).context("request_redo").unwrap_err();
        assert!(wrapped.downcast_ref::<utils::poison::Error>().is_none());
        assert!(matches!(
            wrapped.downcast_ref::<Error>(),
            Some(Error::Poisoned(_))
        ));
    }
}
