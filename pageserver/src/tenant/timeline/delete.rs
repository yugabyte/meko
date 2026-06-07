use std::ops::{Deref, DerefMut};
use std::sync::Arc;

use pageserver_api::models::TimelineState;
use tokio::sync::OwnedMutexGuard;
use tracing::{Instrument, error, info, instrument};
use utils::id::{TenantId, TimelineId};
use utils::{crashsafe, fs_ext, pausable_failpoint};

use crate::config::PageServerConf;
use crate::task_mgr::{self, TaskKind};
use crate::tenant::{DeleteTimelineError, TenantShard, Timeline};
use crate::virtual_file::MaybeFatalIo;

/// Grab the compaction and gc locks, and actually perform the deletion.
///
/// The locks prevent GC or compaction from running at the same time. The background tasks do not
/// register themselves with the timeline it's operating on, so it might still be running even
/// though we called `shutdown_tasks`.
///
/// Note that there are still other race conditions between
/// GC, compaction and timeline deletion. See
/// <https://github.com/neondatabase/neon/issues/2671>
///
/// No timeout here, GC & Compaction should be responsive to the
/// `TimelineState::Stopping` change.
// pub(super): documentation link
pub(super) async fn delete_local_timeline_directory(
    conf: &PageServerConf,
    tenant_shard_id: TenantId,
    timeline: &Timeline,
) {
    // Always ensure the lock order is compaction -> gc.
    let compaction_lock = timeline.compaction_lock.lock();
    let _compaction_lock = crate::timed(
        compaction_lock,
        "acquires compaction lock",
        std::time::Duration::from_secs(5),
    )
    .await;

    let gc_lock = timeline.gc_lock.lock();
    let _gc_lock = crate::timed(
        gc_lock,
        "acquires gc lock",
        std::time::Duration::from_secs(5),
    )
    .await;

    // NB: storage_sync upload tasks that reference these layers have been cancelled
    //     by the caller.

    let local_timeline_directory = conf.timeline_path(&tenant_shard_id, &timeline.timeline_id);

    // NB: This need not be atomic because the deleted flag in the IndexPart
    // will be observed during tenant/timeline load. The deletion will be resumed there.
    //
    // ErrorKind::NotFound can happen e.g. if we race with tenant detach, because,
    // no locks are shared.
    tokio::fs::remove_dir_all(local_timeline_directory)
        .await
        .or_else(fs_ext::ignore_not_found)
        .fatal_err("removing timeline directory");

    // Make sure previous deletions are ordered before mark removal.
    // Otherwise there is no guarantee that they reach the disk before mark deletion.
    // So its possible for mark to reach disk first and for other deletions
    // to be reordered later and thus missed if a crash occurs.
    // Note that we dont need to sync after mark file is removed
    // because we can tolerate the case when mark file reappears on startup.
    let timeline_path = conf.timelines_path(&tenant_shard_id);
    crashsafe::fsync_async(timeline_path)
        .await
        .fatal_err("fsync after removing timeline directory");

    info!("finished deleting layer files, releasing locks");
}

/// It is important that this gets called when DeletionGuard is being held.
/// For more context see comments in [`make_timeline_delete_guard`]
async fn remove_maybe_offloaded_timeline_from_tenant(
    tenant: &TenantShard,
    timeline: &Arc<Timeline>,
    _: &DeletionGuard, // using it as a witness
) -> anyhow::Result<()> {
    let mut timelines = tenant.timelines.lock().unwrap();
    let children_exist = timelines
        .iter()
        .any(|(_, entry)| entry.get_ancestor_timeline_id() == Some(timeline.timeline_id));
    // XXX this can happen because of race conditions with branch creation.
    // We already deleted the remote layer files, so it's probably best to panic.
    if children_exist {
        panic!("Timeline grew children while we removed layer files");
    }

    timelines.remove(&timeline.timeline_id).expect(
        "timeline that we were deleting was concurrently removed from 'timelines' map",
    );
    let _ = tenant; // scheduled_compaction_tasks removed in phase 4d.2

    drop(timelines);

    Ok(())
}

/// Orchestrates timeline shut down of all timeline tasks, removes its in-memory structures,
/// and deletes its data from both disk and s3.
/// The sequence of steps:
/// 1. Set deleted_at in remote index part.
/// 2. Create local mark file.
/// 3. Delete local files except metadata (it is simpler this way, to be able to reuse timeline initialization code that expects metadata)
/// 4. Delete remote layers
/// 5. Delete index part
/// 6. Delete meta, timeline directory
/// 7. Delete mark file
///
/// It is resumable from any step in case a crash/restart occurs.
/// There are two entrypoints to the process:
/// 1. [`DeleteTimelineFlow::run`] this is the main one called by a management api handler.
/// 2. [`DeleteTimelineFlow::resume_deletion`] is called during restarts when local metadata is still present
///    and we possibly neeed to continue deletion of remote files.
///
/// Note the only other place that messes around timeline delete mark is the logic that scans directory with timelines during tenant load.
#[derive(Default)]
pub enum DeleteTimelineFlow {
    #[default]
    NotStarted,
    InProgress,
    Finished,
}

impl DeleteTimelineFlow {
    // These steps are run in the context of management api request handler.
    // Long running steps are continued to run in the background.
    // NB: If this fails half-way through, and is retried, the retry will go through
    // all the same steps again. Make sure the code here is idempotent, and don't
    // error out if some of the shutdown tasks have already been completed!
    #[instrument(skip_all)]
    pub async fn run(
        tenant: &Arc<TenantShard>,
        timeline_id: TimelineId,
    ) -> Result<(), DeleteTimelineError> {
        super::debug_assert_current_span_has_tenant_and_timeline_id();

        let (timeline, mut guard) = make_timeline_delete_guard(tenant, timeline_id)?;

        guard.mark_in_progress()?;

        // Now that the Timeline is in Stopping state, request all the related tasks to shut down.
        timeline.shutdown(super::ShutdownMode::Hard).await;

        tenant.gc_block.before_delete(&timeline.timeline_id);

        fail::fail_point!("timeline-delete-before-index-deleted-at", |_| {
            Err(anyhow::anyhow!(
                "failpoint: timeline-delete-before-index-deleted-at"
            ))?
        });

        fail::fail_point!("timeline-delete-before-schedule", |_| {
            Err(anyhow::anyhow!(
                "failpoint: timeline-delete-before-schedule"
            ))?
        });

        Self::schedule_background(guard, tenant.conf, Arc::clone(tenant), timeline);

        Ok(())
    }

    fn mark_in_progress(&mut self) -> anyhow::Result<()> {
        match self {
            Self::Finished => anyhow::bail!("Bug. Is in finished state"),
            Self::InProgress { .. } => { /* We're in a retry */ }
            Self::NotStarted => { /* Fresh start */ }
        }

        *self = Self::InProgress;

        Ok(())
    }

    fn schedule_background(
        guard: DeletionGuard,
        conf: &'static PageServerConf,
        tenant: Arc<TenantShard>,
        timeline: Arc<Timeline>,
    ) {
        let tenant_shard_id = timeline.tenant_id;
        let timeline_id = timeline.timeline_id;

        // Take a tenant gate guard, because timeline deletion needs access to the tenant to update its manifest.
        let Ok(tenant_guard) = tenant.gate.enter() else {
            // It is safe to simply skip here, because we only schedule background work once the timeline is durably marked for deletion.
            info!("Tenant is shutting down, timeline deletion will be resumed when it next starts");
            return;
        };

        task_mgr::spawn(
            task_mgr::BACKGROUND_RUNTIME.handle(),
            TaskKind::TimelineDeletionWorker,
            tenant_shard_id,
            Some(timeline_id),
            "timeline_delete",
            async move {
                let _guard = tenant_guard;

                if let Err(err) = Self::background(guard, conf, &tenant, &timeline).await {
                    // Only log as an error if it's not a cancellation.
                    if matches!(err, DeleteTimelineError::Cancelled) {
                        info!("Shutdown during timeline deletion");
                    } else {
                        error!("Error: {err:#}");
                    }
                    timeline.set_broken(format!("{err:#}"));
                };
                Ok(())
            }
            .instrument(tracing::info_span!(parent: None, "delete_timeline", tenant_id=%tenant_shard_id,timeline_id=%timeline_id)),
        );
    }

    async fn background(
        mut guard: DeletionGuard,
        conf: &PageServerConf,
        tenant: &TenantShard,
        timeline: &Arc<Timeline>,
    ) -> Result<(), DeleteTimelineError> {
        fail::fail_point!("timeline-delete-before-rm", |_| {
            Err(anyhow::anyhow!("failpoint: timeline-delete-before-rm"))?
        });

        delete_local_timeline_directory(conf, tenant.id, timeline).await;

        fail::fail_point!("timeline-delete-after-rm", |_| {
            Err(anyhow::anyhow!("failpoint: timeline-delete-after-rm"))?
        });

        // Drop every row for this (tenant, timeline) across
        // `pages`, `slru_pages`, `meta`, `timeline_state`.
        tenant
            .fpu_page_store
            .delete_all_for_timeline(tenant.id, timeline.timeline_id)
            .await
            .map_err(DeleteTimelineError::Other)?;

        pausable_failpoint!("in_progress_delete");

        remove_maybe_offloaded_timeline_from_tenant(tenant, timeline, &guard).await?;

        *guard = Self::Finished;

        Ok(())
    }

}

pub(super) fn make_timeline_delete_guard(
    tenant: &TenantShard,
    timeline_id: TimelineId,
) -> Result<(Arc<Timeline>, DeletionGuard), DeleteTimelineError> {
    // Note the interaction between this guard and deletion guard.
    // Here we attempt to lock deletion guard when we're holding a lock on timelines.
    // This is important because when you take into account `remove_timeline_from_tenant`
    // we remove timeline from memory when we still hold the deletion guard.
    // So here when timeline deletion is finished timeline wont be present in timelines map at all
    // which makes the following sequence impossible:
    // T1: get preempted right before the try_lock on `Timeline::delete_progress`
    // T2: do a full deletion, acquire and drop `Timeline::delete_progress`
    // T1: acquire deletion lock, do another `DeleteTimelineFlow::run`
    // For more context see this discussion: `https://github.com/neondatabase/neon/pull/4552#discussion_r1253437346`
    let timelines = tenant.timelines.lock().unwrap();

    let timeline = timelines
        .get(&timeline_id)
        .map(Arc::clone)
        .ok_or(DeleteTimelineError::NotFound)?;

    // Ensure that there are no child timelines, because we are about to remove files,
    // which will break child branches
    let children: Vec<_> = timelines
        .iter()
        .filter_map(|(id, entry)| {
            (entry.get_ancestor_timeline_id() == Some(timeline_id)).then_some(*id)
        })
        .collect();

    if !children.is_empty() {
        return Err(DeleteTimelineError::HasChildren(children));
    }

    // Note that using try_lock here is important to avoid a deadlock.
    let delete_progress = Arc::clone(&timeline.delete_progress);
    let delete_lock_guard = match delete_progress.try_lock_owned() {
        Ok(guard) => DeletionGuard(guard),
        Err(_) => {
            return Err(DeleteTimelineError::AlreadyInProgress(Arc::clone(
                &timeline.delete_progress,
            )));
        }
    };

    timeline.set_state(TimelineState::Stopping);

    Ok((timeline, delete_lock_guard))
}

pub(super) struct DeletionGuard(OwnedMutexGuard<DeleteTimelineFlow>);

impl Deref for DeletionGuard {
    type Target = DeleteTimelineFlow;

    fn deref(&self) -> &Self::Target {
        &self.0
    }
}

impl DerefMut for DeletionGuard {
    fn deref_mut(&mut self) -> &mut Self::Target {
        &mut self.0
    }
}
