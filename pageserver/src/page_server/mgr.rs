//! The single-branch holder: this pageserver process serves exactly one
//! branch for its entire lifetime, identified by the
//! `branch_id` / `generation` baked into `pageserver.toml`
//! by the launcher. The branch is opened synchronously at startup, before
//! any listener accepts traffic.

use std::collections::HashMap;
use std::sync::Arc;

use camino::{Utf8Path, Utf8PathBuf};
use http_utils::error::ApiError;
use pageserver_api::models::TimelineGcRequest;
use rand::Rng;
use rand::distr::Alphanumeric;
use tokio::fs;
use tokio_util::sync::CancellationToken;
use tracing::*;
use utils::crashsafe::path_with_suffix_extension;
use utils::generation::Generation;
use utils::id::BranchId;

use super::TenantSharedResources;
use crate::config::PageServerConf;
use crate::context::{DownloadBehavior, RequestContext};
use crate::metrics::TENANT;
use crate::task_mgr::{BACKGROUND_RUNTIME, TaskKind};
use crate::page_server::timeline::ShutdownMode;
use crate::page_server::{AttachedTenantConf, GcError, GcResult, PageServer};
use crate::virtual_file::MaybeFatalIo;
use crate::TEMP_FILE_SUFFIX;

/// Precursor to deletion of a tenant dir: we do a fast rename to a tmp path, and then
/// the slower actual deletion in the background.
///
/// This is "safe" in that that it won't leave behind a partially deleted directory
/// at the original path, because we rename with TEMP_FILE_SUFFIX before starting deleting
/// the contents.
///
/// This is pageserver-specific, as it relies on future processes after a crash to check
/// for TEMP_FILE_SUFFIX when loading things.
async fn safe_rename_tenant_dir(path: impl AsRef<Utf8Path>) -> std::io::Result<Utf8PathBuf> {
    let parent = path
        .as_ref()
        .parent()
        // It is invalid to call this function with a relative path.  Tenant directories
        // should always have a parent.
        .ok_or(std::io::Error::new(
            std::io::ErrorKind::InvalidInput,
            "Path must be absolute",
        ))?;
    let rand_suffix = rand::rng()
        .sample_iter(&Alphanumeric)
        .take(8)
        .map(char::from)
        .collect::<String>()
        + TEMP_FILE_SUFFIX;
    let tmp_path = path_with_suffix_extension(&path, &rand_suffix);
    fs::rename(path.as_ref(), &tmp_path).await?;
    fs::File::open(parent)
        .await?
        .sync_all()
        .await
        .maybe_fatal_err("safe_rename_tenant_dir")?;
    Ok(tmp_path)
}

/// See [`Self::spawn`].
#[derive(Clone, Default)]
pub struct BackgroundPurges(tokio_util::task::TaskTracker);

impl BackgroundPurges {
    /// When we have moved a tenant's content to a temporary directory, we may delete it lazily in
    /// the background, and thereby avoid blocking any API requests on this deletion completing.
    ///
    /// Although we are cleaning up the tenant, this task is not meant to be bound by the lifetime of the tenant in memory.
    /// Thus the [`BackgroundPurges`] type to keep track of these tasks.
    pub fn spawn(&self, tmp_path: Utf8PathBuf) {
        // because on shutdown we close and wait, we are misusing TaskTracker a bit.
        //
        // so first acquire a token, then check if the tracker has been closed. the tracker might get closed
        // right after, but at least the shutdown will wait for what we are spawning next.
        let token = self.0.token();

        if self.0.is_closed() {
            warn!(
                %tmp_path,
                "trying to spawn background purge during shutdown, ignoring"
            );
            return;
        }

        let span = info_span!(parent: None, "background_purge", %tmp_path);

        let task = move || {
            let _token = token;
            let _entered = span.entered();
            if let Err(error) = std::fs::remove_dir_all(tmp_path.as_path()) {
                // should we fatal_io_error here?
                warn!(%error, "failed to purge tenant directory");
            }
        };

        BACKGROUND_RUNTIME.spawn_blocking(task);
    }

    /// When this future completes, all background purges have completed.
    /// The first poll of the future will already lock out new background purges spawned via [`Self::spawn`].
    ///
    /// Concurrent calls will coalesce.
    ///
    /// # Cancellation-Safety
    ///
    /// If this future is dropped before polled to completion, concurrent and subsequent
    /// instances of this future will continue to be correct.
    #[instrument(skip_all)]
    pub async fn shutdown(&self) {
        // forbid new tasks (can be called many times)
        self.0.close();
        self.0.wait().await;
    }
}

/// Holds the single [`PageServer`] this pageserver process serves,
/// fully opened and active by the time the manager is constructed.
pub struct TenantManager {
    conf: &'static PageServerConf,
    tenant: Arc<PageServer>,

    // Long-running operations that happen outside of a [`PageServer`] lifetime should respect this token.
    cancel: CancellationToken,
}

/// Sweep the local `tenants/` directory for entries that don't correspond to the
/// configured tenant, and queue them for background purge. Temp dirs are removed
/// inline.
///
/// Stale entries can occur after a crash mid-detach or after a different timeline's
/// pageserver previously ran out of this directory. They hold only initdb tarballs
/// and basebackup caches in yb-light, so removing them is always safe.
async fn purge_stale_tenant_dirs(
    conf: &'static PageServerConf,
    attached: &HashMap<BranchId, Generation>,
    background_purges: &BackgroundPurges,
) {
    let branches_dir = conf.branches_path();
    let dentries = match tokio::task::spawn_blocking(move || {
        branches_dir
            .read_dir_utf8()
            .and_then(|it| it.collect::<Result<Vec<_>, _>>())
    })
    .await
    .expect("read branches dir task panicked")
    {
        Ok(d) => d,
        Err(e) => {
            warn!("Failed to read branches dir while purging stale entries: {e}");
            return;
        }
    };

    for dentry in dentries {
        let path = dentry.path().to_path_buf();
        if crate::is_temporary(&path) {
            info!("removing temporary tenant directory {path}");
            if let Err(e) = std::fs::remove_dir_all(&path) {
                warn!("failed to remove temporary tenant directory {path}: {e}");
            }
            continue;
        }
        let Ok(branch_id) = dentry.file_name().parse::<BranchId>() else {
            warn!(
                "Invalid branch path (garbage in our repo directory?): '{}'",
                dentry.file_name()
            );
            continue;
        };
        if attached.contains_key(&branch_id) {
            continue;
        }
        info!(%branch_id, "Purging local branch dir not owned by the configured branch");
        match safe_rename_tenant_dir(&path).await {
            Ok(tmp) => background_purges.spawn(tmp),
            Err(e) => warn!(%branch_id, "Failed to rename stale branch dir {path}: {e}"),
        }
    }
}

/// Open the configured tenant synchronously: preload + attach
/// (open-existing or create-if-missing per `[timeline_init]`) + activate.
/// Returns the fully-active manager; failure is fatal to startup.
pub async fn init_tenant_mgr(
    conf: &'static PageServerConf,
    background_purges: BackgroundPurges,
    resources: TenantSharedResources,
    cancel: CancellationToken,
) -> anyhow::Result<TenantManager> {
    let ctx = RequestContext::todo_child(TaskKind::Startup, DownloadBehavior::Warn);

    // Single-branch pageserver: the identity baked into
    // pageserver.toml by the launcher is the sole source of truth for
    // which branch this process serves and at what
    // generation. The local branches/ dir is just scratch (initdb
    // tarballs, basebackup cache), never authoritative.
    let Some(identity) = conf.timeline_identity else {
        anyhow::bail!(
            "pageserver.toml must set branch_id/generation: this pageserver \
             serves exactly one branch, assigned by the launcher before spawn"
        );
    };
    let attached = HashMap::from([(identity.branch_id, identity.generation)]);

    purge_stale_tenant_dirs(conf, &attached, &background_purges).await;

    TENANT.startup_scheduled.inc();

    let (branch_id, generation) = (identity.branch_id, identity.generation);
    let branch_path = conf.branch_path(&branch_id);
    // branch dir is just a cache (initdb.tar.zst, basebackup_cache).
    // Doesn't need crash-safe sync.
    tokio::fs::create_dir_all(&branch_path)
        .await
        .fatal_err("create branch dir");

    let tenant_conf = pageserver_api::models::BranchConfig::default();

    let tenant = PageServer::open(
        conf,
        branch_id,
        resources,
        AttachedTenantConf::new(conf, tenant_conf, generation),
        &ctx,
    )
    .await?;
    TENANT.startup_complete.inc();

    info!(branch_id = %branch_id, "tenant opened at startup");

    Ok(TenantManager {
        conf,
        tenant,
        cancel,
    })
}

impl TenantManager {
    /// The single (tenant, timeline) identity this process serves,
    /// from pageserver.toml. Infallible: validated at startup.
    pub(crate) fn timeline_identity(&self) -> crate::config::TimelineIdentity {
        self.conf
            .timeline_identity
            .expect("identity is required at startup")
    }

    /// Gets the attached tenant, erroring if `branch_id` doesn't match the
    /// configured identity. The tenant is open and active for the whole process
    /// lifetime (it transitions its timeline to Stopping during shutdown).
    pub(crate) fn get_attached_tenant_shard(
        &self,
        branch_id: BranchId,
    ) -> Result<Arc<PageServer>, GetTenantError> {
        if branch_id != self.timeline_identity().branch_id {
            return Err(GetTenantError::NotFound(branch_id));
        }
        Ok(Arc::clone(&self.tenant))
    }

    /// The attached tenant.
    pub(crate) fn list(&self) -> Vec<(BranchId, Arc<PageServer>)> {
        vec![(self.tenant.branch_id(), Arc::clone(&self.tenant))]
    }

    /// Shut down the tenant. This runs as part of pageserver shutdown.
    ///
    /// NB: the tenant stays accessible until process exit, so the management
    /// API keeps answering rather than 404ing mid-shutdown.
    #[instrument(skip_all)]
    pub(crate) async fn shutdown(&self) {
        self.cancel.cancel();

        let tenant = Arc::clone(&self.tenant);
        tenant
            .shutdown(ShutdownMode::Flush)
            .instrument(info_span!("shutdown", branch_id = %tenant.branch_id()))
            .await;
        info!("tenant successfully stopped");
    }

    /// This pageserver's contribution to utilization metrics: the number of bytes
    /// of local disk space the tenant wants, and the shard count (always 1).
    pub(crate) fn calculate_utilization(&self) -> (u64, u32) {
        (self.tenant.local_storage_wanted(), 1)
    }

    #[instrument(skip_all, fields(branch_id=%branch_id))]
    pub(crate) async fn immediate_gc(
        &self,
        branch_id: BranchId,
        gc_req: TimelineGcRequest,
        cancel: CancellationToken,
        ctx: &RequestContext,
    ) -> Result<GcResult, ApiError> {
        let tenant = self
            .get_attached_tenant_shard(branch_id)
            .map_err(|e| ApiError::NotFound(anyhow::anyhow!(e).into()))?;

        let gc_horizon = gc_req.gc_horizon.unwrap_or_else(|| tenant.get_gc_horizon());
        // Use tenant's pitr setting
        let pitr = tenant.get_pitr_interval();

        let ctx: RequestContext =
            ctx.detached_child(TaskKind::GarbageCollector, DownloadBehavior::Download);

        let _gate_guard = tenant.gate.enter().map_err(|_| ApiError::ShuttingDown)?;

        fail::fail_point!("immediate_gc_task_pre");

        #[allow(unused_mut)]
        let mut result = tenant
            .gc_iteration(Some(branch_id), gc_horizon, pitr, &cancel, &ctx)
            .await;
        // FIXME: `gc_iteration` can return an error for multiple reasons; we should handle it
        // better once the types support it.

        result.map_err(|e| match e {
            GcError::TenantCancelled | GcError::TimelineCancelled => ApiError::ShuttingDown,
            GcError::TimelineNotFound => {
                ApiError::NotFound(anyhow::anyhow!("Timeline not found").into())
            }
            other => ApiError::InternalServerError(anyhow::anyhow!(other)),
        })
    }
}

#[derive(Debug, thiserror::Error)]
pub(crate) enum GetTenantError {
    #[error("Tenant {0} not found")]
    NotFound(BranchId),
}
