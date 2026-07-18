//! This module contains per-tenant background processes, e.g. compaction and GC.

use std::cmp::max;
use std::future::Future;
use std::ops::RangeInclusive;
use std::pin::pin;
use std::sync::Arc;
use std::time::{Duration, Instant};

use once_cell::sync::Lazy;
/// Cadence of the tenant housekeeping loop. Originally
/// `default_tenant_conf.compaction_period`; the compaction-period
/// knob is gone in yb-amp, so we use a hardcoded 20s mirror of
/// upstream's default.
const TENANT_HOUSEKEEPING_PERIOD: Duration = Duration::from_secs(20);
use rand::Rng;
use scopeguard::defer;
use tokio::sync::{Semaphore, SemaphorePermit};
use tokio_util::sync::CancellationToken;
use tracing::*;
use utils::backoff::exponential_backoff_duration;
use utils::pausable_failpoint;

use crate::context::{DownloadBehavior, RequestContext};
use crate::metrics::{self, BackgroundLoopSemaphoreMetricsRecorder, TENANT_TASK_EVENTS};
use crate::page_server::PageServer;
use crate::page_server::throttle::Stats;
use crate::task_mgr::{self, BACKGROUND_RUNTIME, TOKIO_WORKER_THREADS, TaskKind};

/// Semaphore limiting concurrent background tasks (across all tenants).
///
/// We use 3/4 Tokio threads, to avoid blocking all threads in case we do any CPU-heavy work.
static CONCURRENT_BACKGROUND_TASKS: Lazy<Semaphore> = Lazy::new(|| {
    let total_threads = TOKIO_WORKER_THREADS.get();

    // ideally we should run at least one compaction task per tenant in order to (1) maximize
    // compaction throughput (2) avoid head-of-line blocking of large compactions. However doing
    // that may create too many compaction tasks with lots of memory overheads. So we limit the
    // number of compaction tasks based on the available CPU core count.
    // Need to revisit.
    // let tasks_per_thread = std::env::var("BG_TASKS_PER_THREAD")
    //     .ok()
    //     .and_then(|s| s.parse().ok())
    //     .unwrap_or(4);
    // let permits = usize::max(1, total_threads * tasks_per_thread);
    // // assert!(permits < total_threads, "need threads for other work");

    let permits = max(1, (total_threads * 3).checked_div(4).unwrap_or(0));
    assert_ne!(permits, 0, "we will not be adding in permits later");
    assert!(permits < total_threads, "need threads for other work");
    Semaphore::new(permits)
});

/// Background jobs.
///
/// NB: not all of these acquire a CONCURRENT_BACKGROUND_TASKS semaphore permit, only the ones that
/// do any significant IO or CPU work.
///
/// yb-amp only acquires `Gc`, `TenantHouseKeeping`, and
/// `InitialLogicalSizeCalculation`; the layer-stack-era variants
/// (`L0Compaction`, `Compaction`, `Eviction`, `HeatmapUpload`,
/// `SecondaryDownload`) and the upstream consumption-metrics workers
/// (`ConsumptionMetricsCollectMetrics`,
/// `ConsumptionMetricsSyntheticSizeWorker`) are gone with their loops.
#[derive(
    Debug,
    PartialEq,
    Eq,
    Clone,
    Copy,
    strum_macros::IntoStaticStr,
    strum_macros::Display,
    enum_map::Enum,
)]
#[strum(serialize_all = "snake_case")]
pub(crate) enum BackgroundLoopKind {
    Gc,
    TenantHouseKeeping,
    InitialLogicalSizeCalculation,
}

pub struct BackgroundLoopSemaphorePermit<'a> {
    _permit: SemaphorePermit<'static>,
    _recorder: BackgroundLoopSemaphoreMetricsRecorder<'a>,
}

/// Acquires a semaphore permit, to limit concurrent background jobs.
pub(crate) async fn acquire_concurrency_permit(
    loop_kind: BackgroundLoopKind,
    _ctx: &RequestContext,
) -> BackgroundLoopSemaphorePermit<'static> {
    let mut recorder = metrics::BACKGROUND_LOOP_SEMAPHORE.record(loop_kind);

    if loop_kind == BackgroundLoopKind::InitialLogicalSizeCalculation {
        pausable_failpoint!("initial-size-calculation-permit-pause");
    }

    // TODO: assert that we run on BACKGROUND_RUNTIME; requires tokio_unstable Handle::id();
    let permit = CONCURRENT_BACKGROUND_TASKS
        .acquire()
        .await
        .expect("should never close");

    recorder.acquired();

    BackgroundLoopSemaphorePermit {
        _permit: permit,
        _recorder: recorder,
    }
}

/// Start per tenant background loops: GC + ingest housekeeping.
///
/// yb-pageserver does not run compaction. The 10-delta image rule was
/// a write-time invariant; with the image-anchored read path it's gone,
/// and image-layer / L0→L1 work has nothing to do against
/// `pageserver.pages`. A background image materialiser (Phase 3e) will
/// replace this loop once we need it.
pub fn start_background_loops(tenant: &Arc<PageServer>) {
    let branch_id = tenant.id;

    task_mgr::spawn(
        BACKGROUND_RUNTIME.handle(),
        TaskKind::GarbageCollector,
        Some(branch_id),
        &format!("garbage collector for branch {branch_id}"),
        {
            let tenant = Arc::clone(tenant);
            async move {
                let cancel = task_mgr::shutdown_token(); // NB: must be in async context
                TENANT_TASK_EVENTS.with_label_values(&["start"]).inc();
                defer!(TENANT_TASK_EVENTS.with_label_values(&["stop"]).inc());
                gc_loop(tenant, cancel)
                    .instrument(info_span!("gc_loop", branch_id = %branch_id))
                    .await;
                Ok(())
            }
        },
    );

    task_mgr::spawn(
        BACKGROUND_RUNTIME.handle(),
        TaskKind::TenantHousekeeping,
        Some(branch_id),
        &format!("housekeeping for branch {branch_id}"),
        {
            let tenant = Arc::clone(tenant);
            async move {
                let cancel = task_mgr::shutdown_token(); // NB: must be in async context
                TENANT_TASK_EVENTS.with_label_values(&["start"]).inc();
                defer!(TENANT_TASK_EVENTS.with_label_values(&["stop"]).inc());
                tenant_housekeeping_loop(tenant, cancel)
                    .instrument(info_span!("tenant_housekeeping_loop", branch_id = %branch_id))
                    .await;
                Ok(())
            }
        },
    );
}

/// GC task's main loop.
async fn gc_loop(tenant: Arc<PageServer>, cancel: CancellationToken) {
    const MAX_BACKOFF_SECS: f64 = 300.0;
    let mut error_run = 0; // consecutive errors

    // GC might require downloading, to find the cutoff LSN that corresponds to the
    // cutoff specified as time.
    let ctx = RequestContext::todo_child(TaskKind::GarbageCollector, DownloadBehavior::Download);
    let mut first = true;

    loop {
        if cancel.is_cancelled() {
            return;
        }

        let period = tenant.get_gc_period();

        if first {
            first = false;
            if sleep_random(period, &cancel).await.is_err() {
                break;
            }
        }

        let gc_horizon = tenant.get_gc_horizon();
        let sleep_duration;
        if period == Duration::ZERO || gc_horizon == 0 {
            #[cfg(not(feature = "testing"))]
            info!("automatic GC is disabled");
            // check again in 10 seconds, in case it's been enabled again.
            sleep_duration = Duration::from_secs(10);
        } else {
            let iteration = Iteration {
                started_at: Instant::now(),
                period,
                kind: BackgroundLoopKind::Gc,
            };
            // Run gc
            let IterationResult { output } = iteration
                .run(tenant.gc_iteration(
                    None,
                    gc_horizon,
                    tenant.get_pitr_interval(),
                    &cancel,
                    &ctx,
                ))
                .await;
            match output {
                Ok(_) => {
                    error_run = 0;
                    sleep_duration = period;
                }
                Err(crate::page_server::GcError::TenantCancelled) => {
                    return;
                }
                Err(e) => {
                    error_run += 1;
                    let wait_duration =
                        exponential_backoff_duration(error_run, 1.0, MAX_BACKOFF_SECS);

                    if matches!(e, crate::page_server::GcError::TimelineCancelled) {
                        // Timeline was cancelled during gc. We might either be in an event
                        // that affects the entire tenant (tenant deletion, pageserver shutdown),
                        // or in one that affects the timeline only (timeline deletion).
                        // Therefore, don't exit the loop.
                        info!("Gc failed {error_run} times, retrying in {wait_duration:?}: {e:?}");
                    } else {
                        error!("Gc failed {error_run} times, retrying in {wait_duration:?}: {e:?}");
                    }

                    sleep_duration = wait_duration;
                }
            }
        };

        if tokio::time::timeout(sleep_duration, cancel.cancelled())
            .await
            .is_ok()
        {
            break;
        }
    }
}

/// Tenant housekeeping's main loop.
async fn tenant_housekeeping_loop(tenant: Arc<PageServer>, cancel: CancellationToken) {
    let mut last_throttle_flag_reset_at = Instant::now();
    loop {
        if cancel.is_cancelled() {
            return;
        }

        // Tenant housekeeping ticks at a constant cadence. Used to be
        // tied to `compaction_period`; that knob is gone in yb-amp.
        let period = TENANT_HOUSEKEEPING_PERIOD;

        let Ok(period) = sleep_jitter(period, period * 5 / 100, &cancel).await else {
            break;
        };

        // Do tenant housekeeping.
        let iteration = Iteration {
            started_at: Instant::now(),
            period,
            kind: BackgroundLoopKind::TenantHouseKeeping,
        };
        iteration.run(tenant.housekeeping()).await;

        // Log any getpage throttling.
        info_span!(parent: None, "pagestream_throttle", branch_id=%tenant.id).in_scope(|| {
            let now = Instant::now();
            let prev = std::mem::replace(&mut last_throttle_flag_reset_at, now);
            let Stats {
                count_accounted_start,
                count_accounted_finish,
                count_throttled,
                sum_throttled_usecs,
            } = tenant.pagestream_throttle.reset_stats();
            if count_throttled == 0 {
                return;
            }
            let allowed_rps = tenant.pagestream_throttle.steady_rps();
            let delta = now - prev;
            info!(
                n_seconds=%format_args!("{:.3}", delta.as_secs_f64()),
                count_accounted = count_accounted_finish,  // don't break existing log scraping
                count_throttled,
                sum_throttled_usecs,
                count_accounted_start, // log after pre-existing fields to not break existing log scraping
                allowed_rps=%format_args!("{allowed_rps:.0}"),
                "shard was throttled in the last n_seconds"
            );
        });
    }
}

#[derive(thiserror::Error, Debug)]
#[error("cancelled")]
pub(crate) struct Cancelled;

/// Sleeps for a random interval up to the given max value.
///
/// This delay prevents a thundering herd of background tasks and will likely keep them running on
/// different periods for more stable load.
pub(crate) async fn sleep_random(
    max: Duration,
    cancel: &CancellationToken,
) -> Result<Duration, Cancelled> {
    sleep_random_range(Duration::ZERO..=max, cancel).await
}

/// Sleeps for a random interval in the given range. Returns the duration.
pub(crate) async fn sleep_random_range(
    interval: RangeInclusive<Duration>,
    cancel: &CancellationToken,
) -> Result<Duration, Cancelled> {
    let delay = rand::rng().random_range(interval);
    if delay == Duration::ZERO {
        return Ok(delay);
    }
    tokio::select! {
        _ = cancel.cancelled() => Err(Cancelled),
        _ = tokio::time::sleep(delay) => Ok(delay),
    }
}

/// Sleeps for an interval with a random jitter.
pub(crate) async fn sleep_jitter(
    duration: Duration,
    jitter: Duration,
    cancel: &CancellationToken,
) -> Result<Duration, Cancelled> {
    let from = duration.saturating_sub(jitter);
    let to = duration.saturating_add(jitter);
    sleep_random_range(from..=to, cancel).await
}

struct Iteration {
    started_at: Instant,
    period: Duration,
    kind: BackgroundLoopKind,
}

struct IterationResult<O> {
    output: O,
}

impl Iteration {
    #[instrument(skip_all)]
    pub(crate) async fn run<F: Future<Output = O>, O>(self, fut: F) -> IterationResult<O> {
        let mut fut = pin!(fut);

        // Wrap `fut` into a future that logs a message every `period` so that we get a
        // very obvious breadcrumb in the logs _while_ a slow iteration is happening.
        let output = loop {
            match tokio::time::timeout(self.period, &mut fut).await {
                Ok(r) => break r,
                Err(_) => info!("still running"),
            }
        };
        let elapsed = self.started_at.elapsed();
        warn_when_period_overrun(elapsed, self.period, self.kind);

        IterationResult { output }
    }
}

// NB: the `task` and `period` are used for metrics labels.
pub(crate) fn warn_when_period_overrun(
    elapsed: Duration,
    period: Duration,
    task: BackgroundLoopKind,
) {
    // Duration::ZERO will happen because it's the "disable [bgtask]" value.
    if elapsed >= period && period != Duration::ZERO {
        // humantime does no significant digits clamping whereas Duration's debug is a bit more
        // intelligent. however it makes sense to keep the "configuration format" for period, even
        // though there's no way to output the actual config value.
        info!(
            ?elapsed,
            period = %humantime::format_duration(period),
            ?task,
            "task iteration took longer than the configured period"
        );
        metrics::BACKGROUND_LOOP_PERIOD_OVERRUN_COUNT
            .with_label_values(&[task.into(), &format!("{}", period.as_secs())])
            .inc();
    }
}
