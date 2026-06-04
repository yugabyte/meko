//! This module acts as a switchboard to access different repositories managed by this
//! page server.

use std::borrow::Cow;
use std::cmp::Ordering;
use std::collections::{BTreeMap, HashMap};
use std::ops::Deref;
use std::sync::Arc;
use std::time::Duration;

use anyhow::Context;
use camino::{Utf8DirEntry, Utf8Path, Utf8PathBuf};
use futures::StreamExt;
use pageserver_api::key::Key;
use pageserver_api::shard::{
    ShardIdentity, ShardIndex, ShardNumber, ShardStripeSize, TenantShardId,
};
use pageserver_api::upcall_api::ReAttachResponseTenant;
use rand::Rng;
use rand::distr::Alphanumeric;
use remote_storage::TimeoutOrCancel;
use tokio::fs;
use tokio::task::JoinSet;
use tokio_util::sync::CancellationToken;
use tracing::*;
use utils::crashsafe::path_with_suffix_extension;
use utils::fs_ext::PathExt;
use utils::generation::Generation;
use utils::id::{TenantId, TimelineId};
use utils::{backoff, completion};

use super::remote_timeline_client::remote_tenant_path;
use super::{GlobalShutDown, TenantSharedResources};
use crate::config::PageServerConf;
use crate::context::{DownloadBehavior, RequestContext};
use crate::controller_upcall_client::{
    RetryForeverError, StorageControllerUpcallApi, StorageControllerUpcallClient,
};
use crate::deletion_queue::DeletionQueueClient;
use crate::http::routes::ACTIVE_TENANT_TIMEOUT;
use crate::metrics::{LOCAL_DATA_LOSS_SUSPECTED, TENANT, TENANT_MANAGER as METRICS};
use crate::task_mgr::{BACKGROUND_RUNTIME, TaskKind};
use crate::tenant::config::LocationConf;
use crate::tenant::span::debug_assert_current_span_has_tenant_id;
use crate::tenant::timeline::ShutdownMode;
use crate::tenant::{
    AttachedTenantConf, GcError, LoadConfigError, SpawnMode, TenantShard, TenantState,
};
use crate::virtual_file::MaybeFatalIo;
use crate::{InitializationOrder, TEMP_FILE_SUFFIX};

/// For a tenant that appears in TenantsMap, it may either be
/// - `Attached`: has a full Tenant object, is elegible to service
///   reads and ingest WAL.
/// - `InProgress`: other administrative operations acting on the TenantId
///   should block, or return a retry indicator equivalent to HTTP 503.
#[derive(Clone)]
pub(crate) enum TenantSlot {
    Attached(Arc<TenantShard>),
    InProgress(utils::completion::Barrier),
}

impl std::fmt::Debug for TenantSlot {
    fn fmt(&self, f: &mut std::fmt::Formatter<'_>) -> std::fmt::Result {
        match self {
            Self::Attached(tenant) => write!(f, "Attached({})", tenant.current_state()),
            Self::InProgress(_) => write!(f, "InProgress"),
        }
    }
}

impl TenantSlot {
    /// Return the `Tenant` in this slot if attached, else None
    fn get_attached(&self) -> Option<&Arc<TenantShard>> {
        match self {
            Self::Attached(t) => Some(t),
            Self::InProgress(_) => None,
        }
    }
}

/// The tenants known to the pageserver.
/// The enum variants are used to distinguish the different states that the pageserver can be in.
pub(crate) enum TenantsMap {
    /// [`init_tenant_mgr`] is not done yet.
    Initializing,
    /// [`init_tenant_mgr`] is done, all on-disk tenants have been loaded.
    /// New tenants can be added using [`TenantManager::tenant_map_acquire_slot`].
    Open(BTreeMap<TenantShardId, TenantSlot>),
    /// The pageserver has entered shutdown mode via [`TenantManager::shutdown`].
    /// Existing tenants are still accessible, but no new tenants can be created.
    ShuttingDown(BTreeMap<TenantShardId, TenantSlot>),
}

/// When resolving a TenantId to a shard, we may be looking for the 0th
/// shard, or we might be looking for whichever shard holds a particular page.
#[derive(Copy, Clone)]
pub(crate) enum ShardSelector {
    /// Only return the 0th shard, if it is present.  If a non-0th shard is present,
    /// ignore it.
    Zero,
    /// Pick the shard that holds this key
    Page(Key),
    /// The shard ID is known: pick the given shard
    Known(ShardIndex),
}

/// A convenience for use with the re_attach ControllerUpcallClient function: rather
/// than the serializable struct, we build this enum that encapsulates
/// the invariant that attached tenants always have generations.
///
/// This represents the subset of a LocationConfig that we receive during re-attach.
pub(crate) enum TenantStartupMode {
    Attached(Generation),
}

impl TenantStartupMode {
    /// Return the generation that should be used when starting this
    /// tenant, or `None` if the re-attach response says the tenant is
    /// not attached here.
    fn from_reattach_tenant(rart: ReAttachResponseTenant) -> Option<Self> {
        rart.r#gen.map(|g| Self::Attached(Generation::new(g)))
    }
}

/// Result type for looking up a TenantId to a specific shard
pub(crate) enum ShardResolveResult {
    NotFound,
    Found(Arc<TenantShard>),
    // Wait for this barrrier, then query again
    InProgress(utils::completion::Barrier),
}

impl TenantsMap {
    /// Convenience function for typical usage, where we want to get a `Tenant` object, for
    /// working with attached tenants.  If the TenantId is in the map but in Secondary state,
    /// None is returned.
    pub(crate) fn get(&self, tenant_shard_id: &TenantShardId) -> Option<&Arc<TenantShard>> {
        match self {
            TenantsMap::Initializing => None,
            TenantsMap::Open(m) | TenantsMap::ShuttingDown(m) => {
                m.get(tenant_shard_id).and_then(|slot| slot.get_attached())
            }
        }
    }

    #[cfg(all(debug_assertions, not(test)))]
    pub(crate) fn len(&self) -> usize {
        match self {
            TenantsMap::Initializing => 0,
            TenantsMap::Open(m) | TenantsMap::ShuttingDown(m) => m.len(),
        }
    }
}

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

/// Responsible for storing and mutating the collection of all tenants
/// that this pageserver has state for.
///
/// Every Tenant and SecondaryTenant instance lives inside the TenantManager.
///
/// The most important role of the TenantManager is to prevent conflicts: e.g. trying to attach
/// the same tenant twice concurrently, or trying to configure the same tenant into secondary
/// and attached modes concurrently.
pub struct TenantManager {
    conf: &'static PageServerConf,
    tenants: std::sync::RwLock<TenantsMap>,
    resources: TenantSharedResources,

    // Long-running operations that happen outside of a [`Tenant`] lifetime should respect this token.
    // This is for edge cases like tenant deletion.  In normal cases (within a Tenant lifetime),
    // tenants have their own cancellation tokens, which we fire individually in [`Self::shutdown`], or
    // when the tenant detaches.
    cancel: CancellationToken,

    background_purges: BackgroundPurges,
}

fn emergency_generations(
    tenant_confs: &HashMap<TenantShardId, Result<LocationConf, LoadConfigError>>,
) -> HashMap<TenantShardId, TenantStartupMode> {
    tenant_confs
        .iter()
        .filter_map(|(tid, lc)| {
            let lc = match lc {
                Ok(lc) => lc,
                Err(_) => return None,
            };
            Some((*tid, TenantStartupMode::Attached(lc.generation)))
        })
        .collect()
}

async fn init_load_generations(
    conf: &'static PageServerConf,
    tenant_confs: &HashMap<TenantShardId, Result<LocationConf, LoadConfigError>>,
    resources: &TenantSharedResources,
    cancel: &CancellationToken,
) -> anyhow::Result<Option<HashMap<TenantShardId, TenantStartupMode>>> {
    let generations = if conf.control_plane_emergency_mode {
        error!(
            "Emergency mode!  Tenants will be attached unsafely using their last known generation"
        );
        emergency_generations(tenant_confs)
    } else {
        let client = StorageControllerUpcallClient::new(conf, cancel);
        info!("Calling {} API to re-attach tenants", client.base_url());
        // If we are configured to use the control plane API, then it is the source of truth for what tenants to load.
        let empty_local_disk = tenant_confs.is_empty();
        match client.re_attach(conf, empty_local_disk).await {
            Ok(tenants) => tenants
                .into_iter()
                .flat_map(|(id, rart)| {
                    TenantStartupMode::from_reattach_tenant(rart).map(|tsm| (id, tsm))
                })
                .collect(),
            Err(RetryForeverError::ShuttingDown) => {
                anyhow::bail!("Shut down while waiting for control plane re-attach response")
            }
        }
    };

    // The deletion queue needs to know about the startup attachment state to decide which (if any) stored
    // deletion list entries may still be valid.  We provide that by pushing a recovery operation into
    // the queue. Sequential processing of te queue ensures that recovery is done before any new tenant deletions
    // are processed, even though we don't block on recovery completing here.
    let attached_tenants = generations
        .iter()
        .map(|(id, start_mode)| {
            let TenantStartupMode::Attached(generation) = start_mode;
            (*id, *generation)
        })
        .collect();
    resources.deletion_queue_client.recover(attached_tenants)?;

    Ok(Some(generations))
}

/// Given a directory discovered in the pageserver's tenants/ directory, attempt
/// to load a tenant config from it.
///
/// If we cleaned up something expected (like an empty dir or a temp dir), return None.
fn load_tenant_config(
    conf: &'static PageServerConf,
    tenant_shard_id: TenantShardId,
    dentry: Utf8DirEntry,
) -> Option<Result<LocationConf, LoadConfigError>> {
    let tenant_dir_path = dentry.path().to_path_buf();
    if crate::is_temporary(&tenant_dir_path) {
        info!("Found temporary tenant directory, removing: {tenant_dir_path}");
        // No need to use safe_remove_tenant_dir_all because this is already
        // a temporary path
        std::fs::remove_dir_all(&tenant_dir_path).fatal_err("delete temporary tenant dir");
        return None;
    }

    // This case happens if we crash during attachment before writing a config into the dir
    let is_empty = tenant_dir_path
        .is_empty_dir()
        .fatal_err("Checking for empty tenant dir");
    if is_empty {
        info!("removing empty tenant directory {tenant_dir_path:?}");
        std::fs::remove_dir(&tenant_dir_path).fatal_err("delete empty tenant dir");
        return None;
    }

    Some(TenantShard::load_tenant_config(conf, &tenant_shard_id))
}

/// Initial stage of load: walk the local tenants directory, clean up any temp files,
/// and load configurations for the tenants we found.
///
/// Do this in parallel, because we expect 10k+ tenants, so serial execution can take
/// seconds even on reasonably fast drives.
async fn init_load_tenant_configs(
    conf: &'static PageServerConf,
) -> HashMap<TenantShardId, Result<LocationConf, LoadConfigError>> {
    let tenants_dir = conf.tenants_path();

    let dentries = tokio::task::spawn_blocking(move || -> Vec<Utf8DirEntry> {
        let context = format!("read tenants dir {tenants_dir}");
        let dir_entries = tenants_dir.read_dir_utf8().fatal_err(&context);

        dir_entries
            .collect::<Result<Vec<_>, std::io::Error>>()
            .fatal_err(&context)
    })
    .await
    .expect("Config load task panicked");

    let mut configs = HashMap::new();

    let mut join_set = JoinSet::new();
    for dentry in dentries {
        let tenant_shard_id = match dentry.file_name().parse::<TenantShardId>() {
            Ok(id) => id,
            Err(_) => {
                warn!(
                    "Invalid tenant path (garbage in our repo directory?): '{}'",
                    dentry.file_name()
                );
                continue;
            }
        };

        join_set.spawn_blocking(move || {
            (
                tenant_shard_id,
                load_tenant_config(conf, tenant_shard_id, dentry),
            )
        });
    }

    while let Some(r) = join_set.join_next().await {
        let (tenant_shard_id, tenant_config) = r.expect("Panic in config load task");
        if let Some(tenant_config) = tenant_config {
            configs.insert(tenant_shard_id, tenant_config);
        }
    }

    configs
}

#[derive(Debug, thiserror::Error)]
pub(crate) enum DeleteTenantError {
    #[error("Tenant map slot error {0}")]
    SlotError(#[from] TenantSlotError),

    #[error("Cancelled")]
    Cancelled,

    #[error(transparent)]
    Other(#[from] anyhow::Error),
}

/// Initialize repositories at `Initializing` state.
pub fn init(
    conf: &'static PageServerConf,
    background_purges: BackgroundPurges,
    resources: TenantSharedResources,
    cancel: CancellationToken,
) -> TenantManager {
    TenantManager {
        conf,
        tenants: std::sync::RwLock::new(TenantsMap::Initializing),
        resources,
        cancel,
        background_purges,
    }
}

/// Transition repositories from `Initializing` state to `Open` state with locally available timelines.
/// Timelines that are only partially available locally (remote storage has more data than this pageserver)
/// are scheduled for download and added to the tenant once download is completed.
#[instrument(skip_all)]
pub async fn init_tenant_mgr(
    tenant_manager: Arc<TenantManager>,
    init_order: InitializationOrder,
) -> anyhow::Result<()> {
    debug_assert!(matches!(
        *tenant_manager.tenants.read().unwrap(),
        TenantsMap::Initializing
    ));
    let mut tenants = BTreeMap::new();

    let ctx = RequestContext::todo_child(TaskKind::Startup, DownloadBehavior::Warn);

    let conf = tenant_manager.conf;
    let resources = &tenant_manager.resources;
    let cancel = &tenant_manager.cancel;
    let background_purges = &tenant_manager.background_purges;

    // yb-mode: no ephemeral layers, no dirty-byte budget to wire up.

    // Scan local filesystem for attached tenants
    let tenant_configs = init_load_tenant_configs(conf).await;

    // Determine which tenants are to be secondary or attached, and in which generation
    let tenant_modes = init_load_generations(conf, &tenant_configs, resources, cancel).await?;

    // Hadron local SSD check: Raise an alert if our local filesystem does not contain any tenants but the re-attach request returned tenants.
    // This can happen if the PS suffered a Kubernetes node failure resulting in loss of all local data, but recovered quickly on another node
    // so the Storage Controller has not had the time to move tenants out.
    let data_loss_suspected = if let Some(tenant_modes) = &tenant_modes {
        tenant_configs.is_empty() && !tenant_modes.is_empty()
    } else {
        false
    };
    if data_loss_suspected {
        tracing::error!(
            "Local data loss suspected: no tenants found on local filesystem, but re-attach request returned tenants"
        );
    }
    LOCAL_DATA_LOSS_SUSPECTED.set(if data_loss_suspected { 1 } else { 0 });

    tracing::info!(
        "Attaching {} tenants at startup, warming up {} at a time",
        tenant_configs.len(),
        conf.concurrent_tenant_warmup.initial_permits()
    );
    TENANT.startup_scheduled.inc_by(tenant_configs.len() as u64);

    // Accumulate futures for writing tenant configs, so that we can execute in parallel
    let mut config_write_futs = Vec::new();

    // Update the location configs according to the re-attach response and persist them to disk
    tracing::info!("Updating {} location configs", tenant_configs.len());
    for (tenant_shard_id, location_conf) in tenant_configs {
        let tenant_dir_path = conf.tenant_path(&tenant_shard_id);

        let mut location_conf = match location_conf {
            Ok(l) => l,
            Err(e) => {
                // This should only happen in the case of a serialization bug or critical local I/O error: we cannot load this tenant
                error!(tenant_id=%tenant_shard_id.tenant_id, shard_id=%tenant_shard_id.shard_slug(), "Failed to load tenant config, failed to {e:#}");
                continue;
            }
        };

        if let Some(tenant_modes) = &tenant_modes {
            // We have a generation map: treat it as the authority for whether
            // this tenant is really attached.
            match tenant_modes.get(&tenant_shard_id) {
                None => {
                    info!(tenant_id=%tenant_shard_id.tenant_id, shard_id=%tenant_shard_id.shard_slug(), "Detaching tenant, control plane omitted it in re-attach response");

                    match safe_rename_tenant_dir(&tenant_dir_path).await {
                        Ok(tmp_path) => {
                            background_purges.spawn(tmp_path);
                        }
                        Err(e) => {
                            error!(tenant_id=%tenant_shard_id.tenant_id, shard_id=%tenant_shard_id.shard_slug(),
                            "Failed to move detached tenant directory '{tenant_dir_path}': {e:?}");
                        }
                    };

                    // We deleted local content: move on to next tenant, don't try and spawn this one.
                    continue;
                }
                Some(TenantStartupMode::Attached(generation)) => {
                    if &location_conf.generation > generation {
                        tracing::error!(tenant_id=%tenant_shard_id.tenant_id, shard_id=%tenant_shard_id.shard_slug(),
                            "Control plane gave decreasing generation ({generation:?}) in re-attach response for tenant that was attached in generation {:?}; skipping",
                            location_conf.generation
                        );
                        continue;
                    }
                    location_conf.attach_in_generation(*generation);
                }
            }
        } else {
            // Legacy mode: no generation information, any tenant present
            // on local disk may activate
            info!(tenant_id=%tenant_shard_id.tenant_id, shard_id=%tenant_shard_id.shard_slug(), "Starting tenant in legacy mode, no generation",);
        };

        // Presence of a generation number implies attachment: attach the tenant
        // if it wasn't already, and apply the generation number.
        config_write_futs.push(async move {
            let r =
                TenantShard::persist_tenant_config(conf, &tenant_shard_id, &location_conf).await;
            (tenant_shard_id, location_conf, r)
        });
    }

    // Execute config writes with concurrency, to avoid bottlenecking on local FS write latency
    tracing::info!(
        "Writing {} location config files...",
        config_write_futs.len()
    );
    let config_write_results = futures::stream::iter(config_write_futs)
        .buffer_unordered(16)
        .collect::<Vec<_>>()
        .await;

    tracing::info!(
        "Spawning {} tenant shard locations...",
        config_write_results.len()
    );
    // For those shards that have live configurations, construct `Tenant` objects and start them running
    for (tenant_shard_id, location_conf, config_write_result) in config_write_results {
        // Writing a config to local disk is foundational to startup up tenants: panic if we can't.
        config_write_result.fatal_err("write tenant shard config file");

        let tenant_dir_path = conf.tenant_path(&tenant_shard_id);
        let shard_identity = pageserver_api::shard::ShardIdentity::unsharded();
        let generation = location_conf.generation;
        // Task 17.c: refresh `tenant_conf` from the YB-side
        // `pageserver.tenant_config` table. The local `config-v1`
        // copy is a stale snapshot; storage_controller is the
        // source of truth and may have updated it since the last
        // location_config notification. If the YB row is missing
        // (e.g. a tenant created before this migration), fall back
        // to the disk copy.
        let tenant_conf = match resources
            .fpu_page_store
            .load_tenant_config(tenant_shard_id.tenant_id)
            .await
        {
            Ok(Some(yb_conf)) => yb_conf,
            Ok(None) => {
                tracing::info!(
                    tenant_id = %tenant_shard_id.tenant_id,
                    "no pageserver.tenant_config row, using default tenant_conf"
                );
                pageserver_api::models::TenantConfig::default()
            }
            Err(e) => {
                tracing::warn!(
                    tenant_id = %tenant_shard_id.tenant_id,
                    error = %e,
                    "failed to load tenant_config from YB, using default tenant_conf"
                );
                pageserver_api::models::TenantConfig::default()
            }
        };
        let slot = TenantSlot::Attached(
            tenant_spawn(
                conf,
                tenant_shard_id,
                &tenant_dir_path,
                resources.clone(),
                AttachedTenantConf::new(conf, tenant_conf, generation),
                shard_identity,
                Some(init_order.clone()),
                SpawnMode::Lazy,
                &ctx,
            )
            .expect("global shutdown during init_tenant_mgr cannot happen"),
        );

        METRICS.slot_inserted(&slot);
        tenants.insert(tenant_shard_id, slot);
    }

    info!("Processed {} local tenants at startup", tenants.len());

    let mut tenant_map = tenant_manager.tenants.write().unwrap();
    *tenant_map = TenantsMap::Open(tenants);

    Ok(())
}

/// Wrapper for Tenant::spawn that checks invariants before running
#[allow(clippy::too_many_arguments)]
fn tenant_spawn(
    conf: &'static PageServerConf,
    tenant_shard_id: TenantShardId,
    tenant_path: &Utf8Path,
    resources: TenantSharedResources,
    location_conf: AttachedTenantConf,
    shard_identity: ShardIdentity,
    init_order: Option<InitializationOrder>,
    mode: SpawnMode,
    ctx: &RequestContext,
) -> Result<Arc<TenantShard>, GlobalShutDown> {
    // All these conditions should have been satisfied by our caller: the tenant dir exists, is a well formed
    // path, and contains a configuration file.  Assertions that do synchronous I/O are limited to debug mode
    // to avoid impacting prod runtime performance.
    assert!(!crate::is_temporary(tenant_path));
    debug_assert!(tenant_path.is_dir());
    debug_assert!(
        conf.tenant_location_config_path(&tenant_shard_id)
            .try_exists()
            .unwrap()
    );

    TenantShard::spawn(
        conf,
        tenant_shard_id,
        resources,
        location_conf,
        shard_identity,
        init_order,
        mode,
        ctx,
    )
}

#[derive(thiserror::Error, Debug)]
pub(crate) enum UpsertLocationError {
    #[error("Bad config request: {0}")]
    BadRequest(anyhow::Error),

    #[error("Cannot change config in this state: {0}")]
    Unavailable(#[from] TenantMapError),

    #[error("Tenant is already being modified")]
    InProgress,

    #[error("Failed to flush: {0}")]
    Flush(anyhow::Error),

    /// This error variant is for unexpected situations (soft assertions) where the system is in an unexpected state.
    #[error("Internal error: {0}")]
    InternalError(anyhow::Error),
}

impl TenantManager {
    /// Gets the attached tenant from the in-memory data, erroring if it's absent or currently
    /// undergoing a state change (i.e. slot is InProgress).
    ///
    /// The return TenantShard is not guaranteed to be active: check its status after obtaing it, or
    /// use [`TenantShard::wait_to_become_active`] before using it if you will do I/O on it.
    pub(crate) fn get_attached_tenant_shard(
        &self,
        tenant_shard_id: TenantShardId,
    ) -> Result<Arc<TenantShard>, GetTenantError> {
        let locked = self.tenants.read().unwrap();

        let peek_slot = tenant_map_peek_slot(&locked, &tenant_shard_id, TenantSlotPeekMode::Read)?;

        match peek_slot {
            Some(TenantSlot::Attached(tenant)) => Ok(Arc::clone(tenant)),
            Some(TenantSlot::InProgress(_)) => Err(GetTenantError::NotActive(tenant_shard_id)),
            None => Err(GetTenantError::ShardNotFound(tenant_shard_id)),
        }
    }

    /// Returns whether a local shard exists that's a child of the given tenant shard. Note that
    /// this just checks for any shard with a larger shard count, and it may not be a direct child
    /// of the given shard (their keyspace may not overlap).
    pub(crate) fn has_child_shard(&self, tenant_id: TenantId, shard_index: ShardIndex) -> bool {
        match &*self.tenants.read().unwrap() {
            TenantsMap::Initializing => false,
            TenantsMap::Open(slots) | TenantsMap::ShuttingDown(slots) => slots
                .range(TenantShardId::tenant_range(tenant_id))
                .any(|(tsid, _)| tsid.shard_count > shard_index.shard_count),
        }
    }

    #[instrument(skip_all, fields(tenant_id=%tenant_shard_id.tenant_id, shard_id=%tenant_shard_id.shard_slug()))]
    pub(crate) async fn upsert_location(
        &self,
        tenant_shard_id: TenantShardId,
        new_location_config: LocationConf,
        flush: Option<Duration>,
        mut spawn_mode: SpawnMode,
        ctx: &RequestContext,
    ) -> Result<Option<Arc<TenantShard>>, UpsertLocationError> {
        debug_assert_current_span_has_tenant_id();

        // Task 17.c: `LocationConfig` no longer carries `tenant_conf`
        // on the wire. Source of truth for the tenant's `TenantConfig`
        // is `pageserver.tenant_config` in YB; storage_controller
        // writes the row before sending us this PUT. Fall back to
        // the default when the row hasn't propagated yet
        // (e.g. first attach right after tenant_create).
        let tenant_conf = match self
            .resources
            .fpu_page_store
            .load_tenant_config(tenant_shard_id.tenant_id)
            .await
        {
            Ok(Some(yb_conf)) => yb_conf,
            Ok(None) => {
                tracing::info!(
                    "no pageserver.tenant_config row yet, using default tenant_conf"
                );
                pageserver_api::models::TenantConfig::default()
            }
            Err(e) => {
                tracing::warn!(
                    error = %e,
                    "failed to load tenant_config from YB, using default tenant_conf"
                );
                pageserver_api::models::TenantConfig::default()
            }
        };

        info!("configuring tenant location to state {new_location_config:?}");

        enum FastPathModified {
            Attached(Arc<TenantShard>),
        }

        // Special case fast-path for updates to existing slots: if our upsert is only updating configuration,
        // then we do not need to set the slot to InProgress, we can just call into the
        // existng tenant.
        let fast_path_taken = {
            let locked = self.tenants.read().unwrap();
            let peek_slot =
                tenant_map_peek_slot(&locked, &tenant_shard_id, TenantSlotPeekMode::Write)?;
            match peek_slot {
                Some(TenantSlot::Attached(tenant)) => {
                    match new_location_config.generation.cmp(&tenant.generation) {
                        Ordering::Equal => {
                            // A transition from Attached to Attached in the same generation, we may
                            // take our fast path and just provide the updated configuration
                            // to the tenant.
                            tenant.set_new_location_config(
                                AttachedTenantConf::try_from(
                                    self.conf,
                                    new_location_config.clone(),
                                    tenant_conf.clone(),
                                )
                                .map_err(UpsertLocationError::BadRequest)?,
                            );

                            Some(FastPathModified::Attached(tenant.clone()))
                        }
                        Ordering::Less => {
                            return Err(UpsertLocationError::BadRequest(anyhow::anyhow!(
                                "Generation {:?} is less than existing {:?}",
                                new_location_config.generation,
                                tenant.generation
                            )));
                        }
                        Ordering::Greater => {
                            // Generation advanced, fall through to general case of replacing `Tenant` object
                            None
                        }
                    }
                }
                _ => {
                    // Not an Attached->Attached transition, fall through to general case
                    None
                }
            }
        };

        // Fast-path continued: having dropped out of the self.tenants lock, do the async
        // phase of writing config and/or waiting for flush, before returning.
        match fast_path_taken {
            Some(FastPathModified::Attached(tenant)) => {
                TenantShard::persist_tenant_config(
                    self.conf,
                    &tenant_shard_id,
                    &new_location_config,
                )
                .await
                .fatal_err("write tenant shard config");

                let _ = flush; // AttachedStale-flush dance was dead in yb-mode
                return Ok(Some(tenant));
            }
            None => {
                // Proceed with the general case procedure, where we will shutdown & remove any existing
                // slot contents and replace with a fresh one
            }
        };

        // General case for upserts to TenantsMap, excluding the case above: we will substitute an
        // InProgress value to the slot while we make whatever changes are required.  The state for
        // the tenant is inaccessible to the outside world while we are doing this, but that is sensible:
        // the state is ill-defined while we're in transition.  Transitions are async, but fast: we do
        // not do significant I/O, and shutdowns should be prompt via cancellation tokens.
        let mut slot_guard = self
            .tenant_map_acquire_slot(&tenant_shard_id, TenantSlotAcquireMode::Any)
            .map_err(|e| match e {
                TenantSlotError::NotFound(_) => {
                    unreachable!("Called with mode Any")
                }
                TenantSlotError::InProgress => UpsertLocationError::InProgress,
                TenantSlotError::MapState(s) => UpsertLocationError::Unavailable(s),
            })?;

        match slot_guard.get_old_value() {
            Some(TenantSlot::Attached(tenant)) => {
                // The case where we keep a Tenant alive was covered above in the special case
                // for Attached->Attached transitions in the same generation.  By this point,
                // if we see an attached tenant we know it will be discarded and should be
                // shut down.
                let (_guard, progress) = utils::completion::channel();
                // Before we leave our state as the presumed holder of the latest generation,
                // flush any outstanding deletions to reduce the risk of leaking objects.
                self.resources.deletion_queue_client.flush_advisory();

                info!("Shutting down attached tenant");
                match tenant.shutdown(progress, ShutdownMode::Hard).await {
                    Ok(()) => {}
                    Err(barrier) => {
                        info!("Shutdown already in progress, waiting for it to complete");
                        barrier.wait().await;
                    }
                }
                slot_guard.drop_old_value().expect("We just shut it down");

                // Edge case: if we were called with SpawnMode::Create, but a Tenant already existed, then
                // the caller thinks they're creating but the tenant already existed.  We must switch to
                // Eager mode so that when starting this Tenant we properly probe remote storage for timelines,
                // rather than assuming it to be empty.
                spawn_mode = SpawnMode::Eager;
            }
            Some(TenantSlot::InProgress(_)) => {
                // This should never happen: acquire_slot should error out
                // if the contents of a slot were InProgress.
                return Err(UpsertLocationError::InternalError(anyhow::anyhow!(
                    "Acquired an InProgress slot, this is a bug."
                )));
            }
            None => {
                // Slot was vacant, nothing needs shutting down.
            }
        }

        let tenant_path = self.conf.tenant_path(&tenant_shard_id);
        let timelines_path = self.conf.timelines_path(&tenant_shard_id);

        // Directory structure is the same for attached and secondary modes:
        // create it if it doesn't exist.  Timeline load/creation expects the
        // timelines/ subdir to already exist.
        //
        // Does not need to be fsync'd because local storage is just a cache.
        tokio::fs::create_dir_all(&timelines_path)
            .await
            .fatal_err("create timelines/ dir");

        // Before activating either secondary or attached mode, persist the
        // configuration, so that on restart we will re-attach (or re-start
        // secondary) on the tenant.
        TenantShard::persist_tenant_config(self.conf, &tenant_shard_id, &new_location_config)
            .await
            .fatal_err("write tenant shard config");

        let new_slot = {
            let shard_identity = pageserver_api::shard::ShardIdentity::unsharded();

            // Testing hack: if we are configured with no control plane, then drop the generation
            // from upserts.  This enables creating generation-less tenants even though neon_local
            // always uses generations when calling the location conf API.
            let attached_conf =
                AttachedTenantConf::try_from(self.conf, new_location_config, tenant_conf.clone())
                    .map_err(UpsertLocationError::BadRequest)?;

            let tenant = tenant_spawn(
                self.conf,
                tenant_shard_id,
                &tenant_path,
                self.resources.clone(),
                attached_conf,
                shard_identity,
                None,
                spawn_mode,
                ctx,
            )
            .map_err(|_: GlobalShutDown| {
                UpsertLocationError::Unavailable(TenantMapError::ShuttingDown)
            })?;

            TenantSlot::Attached(tenant)
        };

        let attached_tenant = if let TenantSlot::Attached(tenant) = &new_slot {
            Some(tenant.clone())
        } else {
            None
        };

        match slot_guard.upsert(new_slot) {
            Err(TenantSlotUpsertError::InternalError(e)) => {
                Err(UpsertLocationError::InternalError(anyhow::anyhow!(e)))
            }
            Err(TenantSlotUpsertError::MapState(e)) => Err(UpsertLocationError::Unavailable(e)),
            Err(TenantSlotUpsertError::ShuttingDown((new_slot, _completion))) => {
                // If we just called tenant_spawn() on a new tenant, and can't insert it into our map, then
                // we must not leak it: this would violate the invariant that after shutdown_all_tenants, all tenants
                // are shutdown.
                //
                // We must shut it down inline here.
                match new_slot {
                    TenantSlot::InProgress(_) => {
                        // Unreachable because we never insert an InProgress
                        unreachable!()
                    }
                    TenantSlot::Attached(tenant) => {
                        let (_guard, progress) = utils::completion::channel();
                        info!(
                            "Shutting down just-spawned tenant, because tenant manager is shut down"
                        );
                        match tenant.shutdown(progress, ShutdownMode::Hard).await {
                            Ok(()) => {
                                info!("Finished shutting down just-spawned tenant");
                            }
                            Err(barrier) => {
                                info!("Shutdown already in progress, waiting for it to complete");
                                barrier.wait().await;
                            }
                        }
                    }
                }

                Err(UpsertLocationError::Unavailable(
                    TenantMapError::ShuttingDown,
                ))
            }
            Ok(()) => Ok(attached_tenant),
        }
    }

    fn tenant_map_acquire_slot(
        &self,
        tenant_shard_id: &TenantShardId,
        mode: TenantSlotAcquireMode,
    ) -> Result<SlotGuard, TenantSlotError> {
        use TenantSlotAcquireMode::*;
        METRICS.tenant_slot_writes.inc();

        let mut locked = self.tenants.write().unwrap();
        let span = tracing::info_span!("acquire_slot", tenant_id=%tenant_shard_id.tenant_id, shard_id = %tenant_shard_id.shard_slug());
        let _guard = span.enter();

        let m = match &mut *locked {
            TenantsMap::Initializing => return Err(TenantMapError::StillInitializing.into()),
            TenantsMap::ShuttingDown(_) => return Err(TenantMapError::ShuttingDown.into()),
            TenantsMap::Open(m) => m,
        };

        use std::collections::btree_map::Entry;

        let entry = m.entry(*tenant_shard_id);

        match entry {
            Entry::Vacant(v) => match mode {
                MustExist => {
                    tracing::debug!("Vacant && MustExist: return NotFound");
                    Err(TenantSlotError::NotFound(*tenant_shard_id))
                }
                _ => {
                    let (completion, barrier) = utils::completion::channel();
                    let inserting = TenantSlot::InProgress(barrier);
                    METRICS.slot_inserted(&inserting);
                    v.insert(inserting);
                    tracing::debug!("Vacant, inserted InProgress");
                    Ok(SlotGuard::new(
                        *tenant_shard_id,
                        None,
                        completion,
                        &self.tenants,
                    ))
                }
            },
            Entry::Occupied(mut o) => {
                // Apply mode-driven checks
                match (o.get(), mode) {
                    (TenantSlot::InProgress(_), _) => {
                        tracing::debug!("Occupied, failing for InProgress");
                        Err(TenantSlotError::InProgress)
                    }
                    _ => {
                        // Happy case: the slot was not in any state that violated our mode
                        let (completion, barrier) = utils::completion::channel();
                        let in_progress = TenantSlot::InProgress(barrier);
                        METRICS.slot_inserted(&in_progress);
                        let old_value = o.insert(in_progress);
                        METRICS.slot_removed(&old_value);
                        tracing::debug!("Occupied, replaced with InProgress");
                        Ok(SlotGuard::new(
                            *tenant_shard_id,
                            Some(old_value),
                            completion,
                            &self.tenants,
                        ))
                    }
                }
            }
        }
    }

    /// Resetting a tenant is equivalent to detaching it, then attaching it again with the same
    /// LocationConf that was last used to attach it.  Optionally, the local file cache may be
    /// dropped before re-attaching.
    ///
    /// This is not part of a tenant's normal lifecycle: it is used for debug/support, in situations
    /// where an issue is identified that would go away with a restart of the tenant.
    ///
    /// This does not have any special "force" shutdown of a tenant: it relies on the tenant's tasks
    /// to respect the cancellation tokens used in normal shutdown().
    #[instrument(skip_all, fields(tenant_id=%tenant_shard_id.tenant_id, shard_id=%tenant_shard_id.shard_slug(), %drop_cache))]
    pub(crate) async fn reset_tenant(
        &self,
        tenant_shard_id: TenantShardId,
        drop_cache: bool,
        ctx: &RequestContext,
    ) -> anyhow::Result<()> {
        let mut slot_guard =
            self.tenant_map_acquire_slot(&tenant_shard_id, TenantSlotAcquireMode::Any)?;
        let Some(old_slot) = slot_guard.get_old_value() else {
            anyhow::bail!("Tenant not found when trying to reset");
        };

        let Some(tenant) = old_slot.get_attached() else {
            slot_guard.revert();
            anyhow::bail!("Tenant is not in attached state");
        };

        let (_guard, progress) = utils::completion::channel();
        match tenant.shutdown(progress, ShutdownMode::Hard).await {
            Ok(()) => {
                slot_guard.drop_old_value()?;
            }
            Err(_barrier) => {
                slot_guard.revert();
                anyhow::bail!("Cannot reset Tenant, already shutting down");
            }
        }

        let tenant_path = self.conf.tenant_path(&tenant_shard_id);
        let timelines_path = self.conf.timelines_path(&tenant_shard_id);
        let config = TenantShard::load_tenant_config(self.conf, &tenant_shard_id)?;

        if drop_cache {
            tracing::info!("Dropping local file cache");

            match tokio::fs::read_dir(&timelines_path).await {
                Err(e) => {
                    tracing::warn!("Failed to list timelines while dropping cache: {}", e);
                }
                Ok(mut entries) => {
                    while let Some(entry) = entries.next_entry().await? {
                        tokio::fs::remove_dir_all(entry.path()).await?;
                    }
                }
            }
        }

        let shard_identity = pageserver_api::shard::ShardIdentity::unsharded();
        // Task 17.c: tenant_conf source of truth is YB.
        let tenant_conf = self
            .resources
            .fpu_page_store
            .load_tenant_config(tenant_shard_id.tenant_id)
            .await
            .ok()
            .flatten()
            .unwrap_or_default();
        let tenant = tenant_spawn(
            self.conf,
            tenant_shard_id,
            &tenant_path,
            self.resources.clone(),
            AttachedTenantConf::try_from(self.conf, config, tenant_conf)?,
            shard_identity,
            None,
            SpawnMode::Eager,
            ctx,
        )?;

        slot_guard.upsert(TenantSlot::Attached(tenant))?;

        Ok(())
    }

    /// Total list of all tenant slots: this includes attached and InProgress.
    pub(crate) fn list(&self) -> Vec<(TenantShardId, TenantSlot)> {
        let locked = self.tenants.read().unwrap();
        match &*locked {
            TenantsMap::Initializing => Vec::new(),
            TenantsMap::Open(map) | TenantsMap::ShuttingDown(map) => {
                map.iter().map(|(k, v)| (*k, v.clone())).collect()
            }
        }
    }

    pub(crate) fn get(&self, tenant_shard_id: TenantShardId) -> Option<TenantSlot> {
        let locked = self.tenants.read().unwrap();
        match &*locked {
            TenantsMap::Initializing => None,
            TenantsMap::Open(map) | TenantsMap::ShuttingDown(map) => {
                map.get(&tenant_shard_id).cloned()
            }
        }
    }

    /// If a tenant is attached, detach it.  Then remove its data from remote storage.
    ///
    /// A tenant is considered deleted once it is gone from remote storage.  It is the caller's
    /// responsibility to avoid trying to attach the tenant again or use it any way once deletion
    /// has started: this operation is not atomic, and must be retried until it succeeds.
    ///
    /// As a special case, if an unsharded tenant ID is given for a sharded tenant, it will remove
    /// all tenant shards in remote storage (removing all paths with the tenant prefix). The storage
    /// controller uses this to purge all remote tenant data, including any stale parent shards that
    /// may remain after splits. Ideally, this special case would be handled elsewhere. See:
    /// <https://github.com/neondatabase/neon/pull/9394>.
    pub(crate) async fn delete_tenant(
        &self,
        tenant_shard_id: TenantShardId,
    ) -> Result<(), DeleteTenantError> {
        super::span::debug_assert_current_span_has_tenant_id();

        async fn delete_local(
            conf: &PageServerConf,
            background_purges: &BackgroundPurges,
            tenant_shard_id: &TenantShardId,
        ) -> anyhow::Result<()> {
            let local_tenant_directory = conf.tenant_path(tenant_shard_id);
            let tmp_dir = safe_rename_tenant_dir(&local_tenant_directory)
                .await
                .with_context(|| {
                    format!("local tenant directory {local_tenant_directory:?} rename")
                })?;
            background_purges.spawn(tmp_dir);
            Ok(())
        }

        let slot_guard =
            self.tenant_map_acquire_slot(&tenant_shard_id, TenantSlotAcquireMode::Any)?;
        match &slot_guard.old_value {
            Some(TenantSlot::Attached(tenant)) => {
                // Legacy deletion flow: the tenant remains attached, goes to Stopping state, and
                // deletion will be resumed across restarts.
                let tenant = tenant.clone();
                let (_guard, progress) = utils::completion::channel();
                match tenant.shutdown(progress, ShutdownMode::Hard).await {
                    Ok(()) => {}
                    Err(barrier) => {
                        info!("Shutdown already in progress, waiting for it to complete");
                        barrier.wait().await;
                    }
                }
                delete_local(self.conf, &self.background_purges, &tenant_shard_id).await?;
            }
            Some(TenantSlot::InProgress(_)) => unreachable!(),
            None => {}
        };

        // Fall through: local state for this tenant is no longer present, proceed with remote delete.
        // - We use a retry wrapper here so that common transient S3 errors (e.g. 503, 429) do not result
        //   in 500 responses to delete requests.
        // - We keep the `SlotGuard` during this I/O, so that if a concurrent delete request comes in, it will
        //   503/retry, rather than kicking off a wasteful concurrent deletion.
        // NB: this also deletes partial prefixes, i.e. a <tenant_id> path will delete all
        // <tenant_id>_<shard_id>/* objects. See method comment for why.
        backoff::retry(
            || async move {
                self.resources
                    .remote_storage
                    .delete_prefix(&remote_tenant_path(&tenant_shard_id), &self.cancel)
                    .await
            },
            |_| false, // backoff::retry handles cancellation
            1,
            3,
            &format!("delete_tenant[tenant_shard_id={tenant_shard_id}]"),
            &self.cancel,
        )
        .await
        .unwrap_or(Err(TimeoutOrCancel::Cancel.into()))
        .map_err(|err| {
            if TimeoutOrCancel::caused_by_cancel(&err) {
                return DeleteTenantError::Cancelled;
            }
            DeleteTenantError::Other(err)
        })
    }


    ///
    /// Shut down all tenants. This runs as part of pageserver shutdown.
    ///
    /// NB: We leave the tenants in the map, so that they remain accessible through
    /// the management API until we shut it down. If we removed the shut-down tenants
    /// from the tenants map, the management API would return 404 for these tenants,
    /// because TenantsMap::get() now returns `None`.
    /// That could be easily misinterpreted by control plane, the consumer of the
    /// management API. For example, it could attach the tenant on a different pageserver.
    /// We would then be in split-brain once this pageserver restarts.
    #[instrument(skip_all)]
    pub(crate) async fn shutdown(&self) {
        self.cancel.cancel();

        self.shutdown_all_tenants0().await
    }

    async fn shutdown_all_tenants0(&self) {
        let mut join_set = JoinSet::new();

        #[cfg(all(debug_assertions, not(test)))]
        {
            // Check that our metrics properly tracked the size of the tenants map.  This is a convenient location to check,
            // as it happens implicitly at the end of tests etc.
            let m = self.tenants.read().unwrap();
            debug_assert_eq!(METRICS.slots_total(), m.len() as u64);
        }

        // Atomically, 1. create the shutdown tasks and 2. prevent creation of new tenants.
        let (total_in_progress, total_attached) = {
            let mut m = self.tenants.write().unwrap();
            match &mut *m {
                TenantsMap::Initializing => {
                    *m = TenantsMap::ShuttingDown(BTreeMap::default());
                    info!("tenants map is empty");
                    return;
                }
                TenantsMap::Open(tenants) => {
                    let mut shutdown_state = BTreeMap::new();
                    let mut total_in_progress = 0;
                    let mut total_attached = 0;

                    for (tenant_shard_id, v) in std::mem::take(tenants).into_iter() {
                        match v {
                            TenantSlot::Attached(t) => {
                                shutdown_state
                                    .insert(tenant_shard_id, TenantSlot::Attached(t.clone()));
                                join_set.spawn(
                                    async move {
                                        let res = {
                                            let (_guard, shutdown_progress) = completion::channel();
                                            t.shutdown(shutdown_progress, ShutdownMode::FreezeAndFlush).await
                                        };

                                        if let Err(other_progress) = res {
                                            // join the another shutdown in progress
                                            other_progress.wait().await;
                                        }

                                        // we cannot afford per tenant logging here, because if s3 is degraded, we are
                                        // going to log too many lines
                                        debug!("tenant successfully stopped");
                                    }
                                    .instrument(info_span!("shutdown", tenant_id=%tenant_shard_id.tenant_id, shard_id=%tenant_shard_id.shard_slug())),
                                );

                                total_attached += 1;
                            }
                            TenantSlot::InProgress(notify) => {
                                // InProgress tenants are not visible in TenantsMap::ShuttingDown: we will
                                // wait for their notifications to fire in this function.
                                join_set.spawn(async move {
                                    notify.wait().await;
                                });

                                total_in_progress += 1;
                            }
                        }
                    }
                    *m = TenantsMap::ShuttingDown(shutdown_state);
                    (total_in_progress, total_attached)
                }
                TenantsMap::ShuttingDown(_) => {
                    error!(
                        "already shutting down, this function isn't supposed to be called more than once"
                    );
                    return;
                }
            }
        };

        let started_at = std::time::Instant::now();

        info!(
            "Waiting for {} InProgress tenants and {} Attached tenants to shut down",
            total_in_progress, total_attached
        );

        let total = join_set.len();
        let mut panicked = 0;
        let mut buffering = true;
        const BUFFER_FOR: std::time::Duration = std::time::Duration::from_millis(500);
        let mut buffered = std::pin::pin!(tokio::time::sleep(BUFFER_FOR));

        while !join_set.is_empty() {
            tokio::select! {
                Some(joined) = join_set.join_next() => {
                    match joined {
                        Ok(()) => {},
                        Err(join_error) if join_error.is_cancelled() => {
                            unreachable!("we are not cancelling any of the tasks");
                        }
                        Err(join_error) if join_error.is_panic() => {
                            // cannot really do anything, as this panic is likely a bug
                            panicked += 1;
                        }
                        Err(join_error) => {
                            warn!("unknown kind of JoinError: {join_error}");
                        }
                    }
                    if !buffering {
                        // buffer so that every 500ms since the first update (or starting) we'll log
                        // how far away we are; this is because we will get SIGKILL'd at 10s, and we
                        // are not able to log *then*.
                        buffering = true;
                        buffered.as_mut().reset(tokio::time::Instant::now() + BUFFER_FOR);
                    }
                },
                _ = &mut buffered, if buffering => {
                    buffering = false;
                    info!(remaining = join_set.len(), total, elapsed_ms = started_at.elapsed().as_millis(), "waiting for tenants to shutdown");
                }
            }
        }

        if panicked > 0 {
            warn!(
                panicked,
                total, "observed panicks while shutting down tenants"
            );
        }

        // caller will log how long we took
    }

    /// Detaches a tenant, and removes its local files asynchronously.
    ///
    /// File removal is idempotent: even if the tenant has already been removed, this will still
    /// remove any local files. This is used during shard splits, where we leave the parent shard's
    /// files around in case we have to roll back the split.
    pub(crate) async fn detach_tenant(
        &self,
        conf: &'static PageServerConf,
        tenant_shard_id: TenantShardId,
        deletion_queue_client: &DeletionQueueClient,
    ) -> Result<(), TenantStateError> {
        if let Some(tmp_path) = self
            .detach_tenant0(conf, tenant_shard_id, deletion_queue_client)
            .await?
        {
            self.background_purges.spawn(tmp_path);
        }

        Ok(())
    }

    /// Detaches a tenant. This renames the tenant directory to a temporary path and returns it,
    /// allowing the caller to delete it asynchronously. Returns None if the dir is already removed.
    async fn detach_tenant0(
        &self,
        conf: &'static PageServerConf,
        tenant_shard_id: TenantShardId,
        deletion_queue_client: &DeletionQueueClient,
    ) -> Result<Option<Utf8PathBuf>, TenantStateError> {
        let tenant_dir_rename_operation = |tenant_id_to_clean: TenantShardId| async move {
            let local_tenant_directory = conf.tenant_path(&tenant_id_to_clean);
            if !tokio::fs::try_exists(&local_tenant_directory).await? {
                // If the tenant directory doesn't exist, it's already cleaned up.
                return Ok(None);
            }
            safe_rename_tenant_dir(&local_tenant_directory)
                .await
                .with_context(|| {
                    format!("local tenant directory {local_tenant_directory:?} rename")
                })
                .map(Some)
        };

        let mut removal_result = self
            .remove_tenant_from_memory(
                tenant_shard_id,
                tenant_dir_rename_operation(tenant_shard_id),
            )
            .await;

        // If the tenant was not found, it was likely already removed. Attempt to remove the tenant
        // directory on disk anyway. For example, during shard splits, we shut down and remove the
        // parent shard, but leave its directory on disk in case we have to roll back the split.
        //
        // TODO: it would be better to leave the parent shard attached until the split is committed.
        // This will be needed by the gRPC page service too, such that a compute can continue to
        // read from the parent shard until it's notified about the new child shards. See:
        // <https://github.com/neondatabase/neon/issues/11728>.
        if let Err(TenantStateError::SlotError(TenantSlotError::NotFound(_))) = removal_result {
            removal_result = tenant_dir_rename_operation(tenant_shard_id)
                .await
                .map_err(TenantStateError::Other);
        }

        // Flush pending deletions, so that they have a good chance of passing validation
        // before this tenant is potentially re-attached elsewhere.
        deletion_queue_client.flush_advisory();

        removal_result
    }

    pub(crate) fn list_tenants(
        &self,
    ) -> Result<Vec<(TenantShardId, TenantState, Generation)>, TenantMapListError> {
        let tenants = self.tenants.read().unwrap();
        let m = match &*tenants {
            TenantsMap::Initializing => return Err(TenantMapListError::Initializing),
            TenantsMap::Open(m) | TenantsMap::ShuttingDown(m) => m,
        };
        Ok(m.iter()
            .filter_map(|(id, tenant)| match tenant {
                TenantSlot::Attached(tenant) => {
                    Some((*id, tenant.current_state(), tenant.generation()))
                }
                TenantSlot::InProgress(_) => None,
            })
            .collect())
    }


    /// A page service client sends a TenantId, and to look up the correct Tenant we must
    /// resolve this to a fully qualified TenantShardId.
    ///
    /// During shard splits: we shall see parent shards in InProgress state and skip them, and
    /// instead match on child shards which should appear in Attached state.  Very early in a shard
    /// split, or in other cases where a shard is InProgress, we will return our own InProgress result
    /// to instruct the caller to wait for that to finish before querying again.
    pub(crate) fn resolve_attached_shard(
        &self,
        tenant_id: &TenantId,
        selector: ShardSelector,
    ) -> ShardResolveResult {
        let tenants = self.tenants.read().unwrap();
        let mut want_shard: Option<ShardIndex> = None;
        let mut any_in_progress = None;

        match &*tenants {
            TenantsMap::Initializing => ShardResolveResult::NotFound,
            TenantsMap::Open(m) | TenantsMap::ShuttingDown(m) => {
                for slot in m.range(TenantShardId::tenant_range(*tenant_id)) {
                    // Ignore all slots that don't contain an attached tenant
                    let tenant = match &slot.1 {
                        TenantSlot::Attached(t) => t,
                        TenantSlot::InProgress(barrier) => {
                            // We might still find a usable shard, but in case we don't, remember that
                            // we saw at least one InProgress slot, so that we can distinguish this case
                            // from a simple NotFound in our return value.
                            any_in_progress = Some(barrier.clone());
                            continue;
                        }
                    };

                    match selector {
                        ShardSelector::Zero if slot.0.shard_number == ShardNumber(0) => {
                            return ShardResolveResult::Found(tenant.clone());
                        }
                        ShardSelector::Page(key) => {
                            // Each time we find an attached slot with a different shard count,
                            // recompute the expected shard number: during shard splits we might
                            // have multiple shards with the old shard count.
                            if want_shard.is_none()
                                || want_shard.unwrap().shard_count != tenant.shard_identity.count
                            {
                                want_shard = Some(ShardIndex {
                                    shard_number: tenant.shard_identity.get_shard_number(&key),
                                    shard_count: tenant.shard_identity.count,
                                });
                            }

                            if Some(ShardIndex {
                                shard_number: tenant.shard_identity.number,
                                shard_count: tenant.shard_identity.count,
                            }) == want_shard
                            {
                                return ShardResolveResult::Found(tenant.clone());
                            }
                        }
                        ShardSelector::Known(shard)
                            if tenant.shard_identity.shard_index() == shard =>
                        {
                            return ShardResolveResult::Found(tenant.clone());
                        }
                        _ => continue,
                    }
                }

                // Fall through: we didn't find a slot that was in Attached state & matched our selector.  If
                // we found one or more InProgress slot, indicate to caller that they should retry later.  Otherwise
                // this requested shard simply isn't found.
                if let Some(barrier) = any_in_progress {
                    ShardResolveResult::InProgress(barrier)
                } else {
                    ShardResolveResult::NotFound
                }
            }
        }
    }

    /// Calculate the tenant shards' contributions to this pageserver's utilization metrics.  The
    /// returned values are:
    ///  - the number of bytes of local disk space this pageserver's shards are requesting, i.e.
    ///    how much space they would use if not impacted by disk usage eviction.
    ///  - the number of tenant shards currently on this pageserver, including attached
    ///    and secondary.
    ///
    /// This function is quite expensive: callers are expected to cache the result and
    /// limit how often they call it.
    pub(crate) fn calculate_utilization(&self) -> Result<(u64, u32), TenantMapListError> {
        let tenants = self.tenants.read().unwrap();
        let m = match &*tenants {
            TenantsMap::Initializing => return Err(TenantMapListError::Initializing),
            TenantsMap::Open(m) | TenantsMap::ShuttingDown(m) => m,
        };
        let shard_count = m.len();
        let mut wanted_bytes = 0;

        for tenant_slot in m.values() {
            match tenant_slot {
                TenantSlot::InProgress(_barrier) => {
                    // While a slot is being changed, we can't know how much storage it wants.  This
                    // means this function's output can fluctuate if a lot of changes are going on
                    // (such as transitions from secondary to attached).
                    //
                    // We could wait for the barrier and retry, but it's important that the utilization
                    // API is responsive, and the data quality impact is not very significant.
                    continue;
                }
                TenantSlot::Attached(tenant) => {
                    wanted_bytes += tenant.local_storage_wanted();
                }
            }
        }

        Ok((wanted_bytes, shard_count as u32))
    }

    #[instrument(skip_all, fields(tenant_id=%tenant_shard_id.tenant_id, shard_id=%tenant_shard_id.shard_slug(), %timeline_id))]
    pub(crate) async fn immediate_gc(
        &self,
        tenant_shard_id: TenantShardId,
        timeline_id: TimelineId,
        gc_req: TimelineGcRequest,
        cancel: CancellationToken,
        ctx: &RequestContext,
    ) -> Result<GcResult, ApiError> {
        let tenant = {
            let guard = self.tenants.read().unwrap();
            guard
                .get(&tenant_shard_id)
                .cloned()
                .with_context(|| format!("tenant {tenant_shard_id}"))
                .map_err(|e| ApiError::NotFound(e.into()))?
        };

        let gc_horizon = gc_req.gc_horizon.unwrap_or_else(|| tenant.get_gc_horizon());
        // Use tenant's pitr setting
        let pitr = tenant.get_pitr_interval();

        tenant.wait_to_become_active(ACTIVE_TENANT_TIMEOUT).await?;

        // Run in task_mgr to avoid race with tenant_detach operation
        let ctx: RequestContext =
            ctx.detached_child(TaskKind::GarbageCollector, DownloadBehavior::Download);

        let _gate_guard = tenant.gate.enter().map_err(|_| ApiError::ShuttingDown)?;

        fail::fail_point!("immediate_gc_task_pre");

        #[allow(unused_mut)]
        let mut result = tenant
            .gc_iteration(Some(timeline_id), gc_horizon, pitr, &cancel, &ctx)
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

    /// Stops and removes the tenant from memory, if it's not [`TenantState::Stopping`] already, bails otherwise.
    /// Allows to remove other tenant resources manually, via `tenant_cleanup`.
    /// If the cleanup fails, tenant will stay in memory in [`TenantState::Broken`] state, and another removal
    async fn remove_tenant_from_memory<V, F>(
        &self,
        tenant_shard_id: TenantShardId,
        tenant_cleanup: F,
    ) -> Result<V, TenantStateError>
    where
        F: std::future::Future<Output = anyhow::Result<V>>,
    {
        let mut slot_guard =
            self.tenant_map_acquire_slot(&tenant_shard_id, TenantSlotAcquireMode::MustExist)?;

        // allow pageserver shutdown to await for our completion
        let (_guard, progress) = completion::channel();

        // The SlotGuard allows us to manipulate the Tenant object without fear of some
        // concurrent API request doing something else for the same tenant ID.
        let attached_tenant = match slot_guard.get_old_value() {
            Some(TenantSlot::Attached(tenant)) => {
                // whenever we remove a tenant from memory, we don't want to flush and wait for upload
                let shutdown_mode = ShutdownMode::Hard;

                // shutdown is sure to transition tenant to stopping, and wait for all tasks to complete, so
                // that we can continue safely to cleanup.
                match tenant.shutdown(progress, shutdown_mode).await {
                    Ok(()) => {}
                    Err(_other) => {
                        // if pageserver shutdown or other detach/ignore is already ongoing, we don't want to
                        // wait for it but return an error right away because these are distinct requests.
                        slot_guard.revert();
                        return Err(TenantStateError::IsStopping(tenant_shard_id));
                    }
                }
                Some(tenant)
            }
            Some(TenantSlot::InProgress(_)) => {
                // Acquiring a slot guarantees its old value was not InProgress
                unreachable!();
            }
            None => None,
        };

        match tenant_cleanup
            .await
            .with_context(|| format!("Failed to run cleanup for tenant {tenant_shard_id}"))
        {
            Ok(hook_value) => {
                // Success: drop the old TenantSlot::Attached.
                slot_guard
                    .drop_old_value()
                    .expect("We just called shutdown");

                Ok(hook_value)
            }
            Err(e) => {
                // If we had a Tenant, set it to Broken and put it back in the TenantsMap
                if let Some(attached_tenant) = attached_tenant {
                    attached_tenant.set_broken(e.to_string()).await;
                }
                // Leave the broken tenant in the map
                slot_guard.revert();

                Err(TenantStateError::Other(e))
            }
        }
    }
}

#[derive(Debug, thiserror::Error)]
pub(crate) enum GetTenantError {
    /// NotFound is a TenantId rather than TenantShardId, because this error type is used from
    /// getters that use a TenantId and a ShardSelector, not just getters that target a specific shard.
    #[error("Tenant {0} not found")]
    NotFound(TenantId),

    #[error("Tenant {0} not found")]
    ShardNotFound(TenantShardId),

    #[error("Tenant {0} is not active")]
    NotActive(TenantShardId),

    // Initializing or shutting down: cannot authoritatively say whether we have this tenant
    #[error("Tenant map is not available: {0}")]
    MapState(#[from] TenantMapError),
}

#[derive(thiserror::Error, Debug)]
pub(crate) enum GetActiveTenantError {
    /// We may time out either while TenantSlot is InProgress, or while the Tenant
    /// is in a non-Active state
    #[error(
        "Timed out waiting {wait_time:?} for tenant active state. Latest state: {latest_state:?}"
    )]
    WaitForActiveTimeout {
        latest_state: Option<TenantState>,
        wait_time: Duration,
    },

    /// The TenantSlot is absent, or in secondary mode
    #[error(transparent)]
    NotFound(#[from] GetTenantError),

    /// Cancellation token fired while we were waiting
    #[error("cancelled")]
    Cancelled,

    /// Tenant exists, but is in a state that cannot become active (e.g. Stopping, Broken)
    #[error("will not become active.  Current state: {0}")]
    WillNotBecomeActive(TenantState),

    /// Broken is logically a subset of WillNotBecomeActive, but a distinct error is useful as
    /// WillNotBecomeActive is a permitted error under some circumstances, whereas broken should
    /// never happen.
    #[error("Tenant is broken: {0}")]
    Broken(String),

    #[error("reconnect to switch tenant id")]
    SwitchedTenant,
}

#[derive(Debug, thiserror::Error)]
pub(crate) enum DeleteTimelineError {
    #[error("Tenant {0}")]
    Tenant(#[from] GetTenantError),

    #[error("Timeline {0}")]
    Timeline(#[from] crate::tenant::DeleteTimelineError),
}

#[derive(Debug, thiserror::Error)]
pub(crate) enum TenantStateError {
    #[error("Tenant {0} is stopping")]
    IsStopping(TenantShardId),
    #[error(transparent)]
    SlotError(#[from] TenantSlotError),
    #[error(transparent)]
    SlotUpsertError(#[from] TenantSlotUpsertError),
    #[error(transparent)]
    Other(#[from] anyhow::Error),
}

#[derive(Debug, thiserror::Error)]
pub(crate) enum TenantMapListError {
    #[error("tenant map is still initiailizing")]
    Initializing,
}

#[derive(Debug, thiserror::Error)]
pub(crate) enum TenantMapInsertError {
    #[error(transparent)]
    SlotError(#[from] TenantSlotError),
    #[error(transparent)]
    SlotUpsertError(#[from] TenantSlotUpsertError),
    #[error(transparent)]
    Other(#[from] anyhow::Error),
}

/// Superset of TenantMapError: issues that can occur when acquiring a slot
/// for a particular tenant ID.
#[derive(Debug, thiserror::Error)]
pub(crate) enum TenantSlotError {
    /// When acquiring a slot with the expectation that the tenant already exists.
    #[error("Tenant {0} not found")]
    NotFound(TenantShardId),

    // Tried to read a slot that is currently being mutated by another administrative
    // operation.
    #[error("tenant has a state change in progress, try again later")]
    InProgress,

    #[error(transparent)]
    MapState(#[from] TenantMapError),
}

/// Superset of TenantMapError: issues that can occur when using a SlotGuard
/// to insert a new value.
#[derive(thiserror::Error)]
pub(crate) enum TenantSlotUpsertError {
    /// An error where the slot is in an unexpected state, indicating a code bug
    #[error("Internal error updating Tenant")]
    InternalError(Cow<'static, str>),

    #[error(transparent)]
    MapState(TenantMapError),

    // If we encounter TenantManager shutdown during upsert, we must carry the Completion
    // from the SlotGuard, so that the caller can hold it while they clean up: otherwise
    // TenantManager shutdown might race ahead before we're done cleaning up any Tenant that
    // was protected by the SlotGuard.
    #[error("Shutting down")]
    ShuttingDown((TenantSlot, utils::completion::Completion)),
}

impl std::fmt::Debug for TenantSlotUpsertError {
    fn fmt(&self, f: &mut std::fmt::Formatter) -> std::fmt::Result {
        match self {
            Self::InternalError(reason) => write!(f, "Internal Error {reason}"),
            Self::MapState(map_error) => write!(f, "Tenant map state: {map_error:?}"),
            Self::ShuttingDown(_completion) => write!(f, "Tenant map shutting down"),
        }
    }
}

#[derive(Debug, thiserror::Error)]
enum TenantSlotDropError {
    /// It is only legal to drop a TenantSlot if its contents are fully shut down
    #[error("Tenant was not shut down")]
    NotShutdown,
}

/// Errors that can happen any time we are walking the tenant map to try and acquire
/// the TenantSlot for a particular tenant.
#[derive(Debug, thiserror::Error)]
pub(crate) enum TenantMapError {
    // Tried to read while initializing
    #[error("tenant map is still initializing")]
    StillInitializing,

    // Tried to read while shutting down
    #[error("tenant map is shutting down")]
    ShuttingDown,
}

/// Guards a particular tenant_id's content in the TenantsMap.
///
/// While this structure exists, the TenantsMap will contain a [`TenantSlot::InProgress`]
/// for this tenant, which acts as a marker for any operations targeting
/// this tenant to retry later, or wait for the InProgress state to end.
///
/// This structure enforces the important invariant that we do not have overlapping
/// tasks that will try to use local storage for a the same tenant ID: we enforce that
/// the previous contents of a slot have been shut down before the slot can be
/// left empty or used for something else
///
/// Holders of a SlotGuard should explicitly dispose of it, using either `upsert`
/// to provide a new value, or `revert` to put the slot back into its initial
/// state.  If the SlotGuard is dropped without calling either of these, then
/// we will leave the slot empty if our `old_value` is already shut down, else
/// we will replace the slot with `old_value` (equivalent to doing a revert).
///
/// The `old_value` may be dropped before the SlotGuard is dropped, by calling
/// `drop_old_value`.  It is an error to call this without shutting down
/// the conents of `old_value`.
pub(crate) struct SlotGuard<'a> {
    tenant_shard_id: TenantShardId,
    old_value: Option<TenantSlot>,
    upserted: bool,

    /// [`TenantSlot::InProgress`] carries the corresponding Barrier: it will
    /// release any waiters as soon as this SlotGuard is dropped.
    completion: utils::completion::Completion,

    tenants: &'a std::sync::RwLock<TenantsMap>,
}

impl<'a> SlotGuard<'a> {
    fn new(
        tenant_shard_id: TenantShardId,
        old_value: Option<TenantSlot>,
        completion: utils::completion::Completion,
        tenants: &'a std::sync::RwLock<TenantsMap>,
    ) -> Self {
        Self {
            tenant_shard_id,
            old_value,
            upserted: false,
            completion,
            tenants,
        }
    }

    /// Get any value that was present in the slot before we acquired ownership
    /// of it: in state transitions, this will be the old state.
    ///
    // FIXME: get_ prefix
    // FIXME: this should be .as_ref() -- unsure why no clippy
    fn get_old_value(&self) -> &Option<TenantSlot> {
        &self.old_value
    }

    /// Emplace a new value in the slot.  This consumes the guard, and after
    /// returning, the slot is no longer protected from concurrent changes.
    fn upsert(mut self, new_value: TenantSlot) -> Result<(), TenantSlotUpsertError> {
        if !self.old_value_is_shutdown() {
            // This is a bug: callers should never try to drop an old value without
            // shutting it down
            return Err(TenantSlotUpsertError::InternalError(
                "Old TenantSlot value not shut down".into(),
            ));
        }

        let replaced: Option<TenantSlot> = {
            let mut locked = self.tenants.write().unwrap();

            if let TenantSlot::InProgress(_) = new_value {
                // It is never expected to try and upsert InProgress via this path: it should
                // only be written via the tenant_map_acquire_slot path.  If we hit this it's a bug.
                return Err(TenantSlotUpsertError::InternalError(
                    "Attempt to upsert an InProgress state".into(),
                ));
            }

            let m = match &mut *locked {
                TenantsMap::Initializing => {
                    return Err(TenantSlotUpsertError::MapState(
                        TenantMapError::StillInitializing,
                    ));
                }
                TenantsMap::ShuttingDown(_) => {
                    return Err(TenantSlotUpsertError::ShuttingDown((
                        new_value,
                        self.completion.clone(),
                    )));
                }
                TenantsMap::Open(m) => m,
            };

            METRICS.slot_inserted(&new_value);

            let replaced = m.insert(self.tenant_shard_id, new_value);
            self.upserted = true;
            if let Some(replaced) = replaced.as_ref() {
                METRICS.slot_removed(replaced);
            }

            replaced
        };

        // Sanity check: on an upsert we should always be replacing an InProgress marker
        match replaced {
            Some(TenantSlot::InProgress(_)) => {
                // Expected case: we find our InProgress in the map: nothing should have
                // replaced it because the code that acquires slots will not grant another
                // one for the same TenantId.
                Ok(())
            }
            None => {
                METRICS.unexpected_errors.inc();
                error!(
                    tenant_shard_id = %self.tenant_shard_id,
                    "Missing InProgress marker during tenant upsert, this is a bug."
                );
                Err(TenantSlotUpsertError::InternalError(
                    "Missing InProgress marker during tenant upsert".into(),
                ))
            }
            Some(slot) => {
                METRICS.unexpected_errors.inc();
                error!(tenant_shard_id=%self.tenant_shard_id, "Unexpected contents of TenantSlot during upsert, this is a bug.  Contents: {:?}", slot);
                Err(TenantSlotUpsertError::InternalError(
                    "Unexpected contents of TenantSlot".into(),
                ))
            }
        }
    }

    /// Replace the InProgress slot with whatever was in the guard when we started
    fn revert(mut self) {
        if let Some(value) = self.old_value.take() {
            match self.upsert(value) {
                Err(TenantSlotUpsertError::InternalError(_)) => {
                    // We already logged the error, nothing else we can do.
                }
                Err(
                    TenantSlotUpsertError::MapState(_) | TenantSlotUpsertError::ShuttingDown(_),
                ) => {
                    // If the map is shutting down, we need not replace anything
                }
                Ok(()) => {}
            }
        }
    }

    /// We may never drop our old value until it is cleanly shut down: otherwise we might leave
    /// rogue background tasks that would write to the local tenant directory that this guard
    /// is responsible for protecting
    fn old_value_is_shutdown(&self) -> bool {
        match self.old_value.as_ref() {
            Some(TenantSlot::Attached(tenant)) => tenant.gate.close_complete(),
            Some(TenantSlot::InProgress(_)) => {
                // A SlotGuard cannot be constructed for a slot that was already InProgress
                unreachable!()
            }
            None => true,
        }
    }

    /// The guard holder is done with the old value of the slot: they are obliged to already
    /// shut it down before we reach this point.
    fn drop_old_value(&mut self) -> Result<(), TenantSlotDropError> {
        if !self.old_value_is_shutdown() {
            Err(TenantSlotDropError::NotShutdown)
        } else {
            self.old_value.take();
            Ok(())
        }
    }
}

impl<'a> Drop for SlotGuard<'a> {
    fn drop(&mut self) {
        if self.upserted {
            return;
        }
        // Our old value is already shutdown, or it never existed: it is safe
        // for us to fully release the TenantSlot back into an empty state

        let mut locked = self.tenants.write().unwrap();

        let m = match &mut *locked {
            TenantsMap::Initializing => {
                // There is no map, this should never happen.
                return;
            }
            TenantsMap::ShuttingDown(_) => {
                // When we transition to shutdown, InProgress elements are removed
                // from the map, so we do not need to clean up our Inprogress marker.
                // See [`shutdown_all_tenants0`]
                return;
            }
            TenantsMap::Open(m) => m,
        };

        use std::collections::btree_map::Entry;
        match m.entry(self.tenant_shard_id) {
            Entry::Occupied(mut entry) => {
                if !matches!(entry.get(), TenantSlot::InProgress(_)) {
                    METRICS.unexpected_errors.inc();
                    error!(tenant_shard_id=%self.tenant_shard_id, "Unexpected contents of TenantSlot during drop, this is a bug.  Contents: {:?}", entry.get());
                }

                if self.old_value_is_shutdown() {
                    METRICS.slot_removed(entry.get());
                    entry.remove();
                } else {
                    let inserting = self.old_value.take().unwrap();
                    METRICS.slot_inserted(&inserting);
                    let replaced = entry.insert(inserting);
                    METRICS.slot_removed(&replaced);
                }
            }
            Entry::Vacant(_) => {
                METRICS.unexpected_errors.inc();
                error!(
                    tenant_shard_id = %self.tenant_shard_id,
                    "Missing InProgress marker during SlotGuard drop, this is a bug."
                );
            }
        }
    }
}

enum TenantSlotPeekMode {
    /// In Read mode, peek will be permitted to see the slots even if the pageserver is shutting down
    Read,
    /// In Write mode, trying to peek at a slot while the pageserver is shutting down is an error
    Write,
}

fn tenant_map_peek_slot<'a>(
    tenants: &'a std::sync::RwLockReadGuard<'a, TenantsMap>,
    tenant_shard_id: &TenantShardId,
    mode: TenantSlotPeekMode,
) -> Result<Option<&'a TenantSlot>, TenantMapError> {
    match tenants.deref() {
        TenantsMap::Initializing => Err(TenantMapError::StillInitializing),
        TenantsMap::ShuttingDown(m) => match mode {
            TenantSlotPeekMode::Read => Ok(Some(
                // When reading in ShuttingDown state, we must translate None results
                // into a ShuttingDown error, because absence of a tenant shard ID in the map
                // isn't a reliable indicator of the tenant being gone: it might have been
                // InProgress when shutdown started, and cleaned up from that state such
                // that it's now no longer in the map.  Callers will have to wait until
                // we next start up to get a proper answer.  This avoids incorrect 404 API responses.
                m.get(tenant_shard_id).ok_or(TenantMapError::ShuttingDown)?,
            )),
            TenantSlotPeekMode::Write => Err(TenantMapError::ShuttingDown),
        },
        TenantsMap::Open(m) => Ok(m.get(tenant_shard_id)),
    }
}

enum TenantSlotAcquireMode {
    /// Acquire the slot irrespective of current state, or whether it already exists
    Any,
    /// Return an error if trying to acquire a slot and it doesn't already exist
    MustExist,
}

use http_utils::error::ApiError;
use pageserver_api::models::TimelineGcRequest;

use crate::tenant::gc_result::GcResult;

