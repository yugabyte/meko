mod safekeeper_service;

use std::borrow::Cow;
use std::collections::{BTreeMap, HashMap};
use std::error::Error;
use std::num::NonZeroU32;
use std::sync::Arc;
use std::time::{Duration, Instant, SystemTime};

use anyhow::Context;
use control_plane::storage_controller::{
    AttachHookRequest, AttachHookResponse, InspectRequest, InspectResponse,
};
use diesel::result::DatabaseErrorKind;
use futures::StreamExt;
use futures::stream::FuturesUnordered;
use http_utils::error::ApiError;
use itertools::Itertools;
use pageserver_api::controller_api::{
    BranchCreateRequest, BranchCreateResponse, BranchCreateResponseShard, BranchDescribeResponse,
    BranchDescribeResponseShard, BranchLocateResponse, BranchPolicyRequest,
    BranchTimelineDescribeResponse, ComputeEndpoint, MetadataHealthRecord,
    MetadataHealthUpdateRequest, NodeAvailability, NodeRegisterRequest, NodeShard,
    NodeShardResponse, PendingTimelineInit, PendingTimelineInitRequest, PlacementPolicy,
    ShardSchedulingPolicy, TimelineBranchName,
};
use pageserver_api::models::{
    self, BranchConfig, BranchImportStatus, DetachBehavior, LsnLease, PageserverUtilization,
    TimelineArchivalConfigRequest, TimelineCreateRequest, TimelineCreateResponseStorcon,
    TimelineInfo,
};
use pageserver_api::upcall_api::{PutTimelineImportStatusRequest, TimelineImportStatusRequest};
use pageserver_client::{BlockUnblock, mgmt_api};
use reqwest::{Certificate, StatusCode};
use safekeeper_api::PgMajorVersion;
use tokio_util::sync::CancellationToken;
use tracing::instrument;
use utils::completion::Barrier;
use utils::generation::Generation;
use utils::id::{BranchId, NodeId};
use utils::lsn::Lsn;
use utils::sync::gate::Gate;
use utils::{failpoint_support, pausable_failpoint};

use crate::branch::{Branch, IntentState};
use crate::heartbeater::{Heartbeater, PageserverState};
use crate::id_lock_map::{
    IdLockMap, TracingExclusiveGuard, trace_exclusive_lock, trace_shared_lock,
};
use crate::metrics;
use crate::node::{AvailabilityTransition, Node};
use crate::pageserver_client::PageserverClient;
use crate::persistence::split_state::SplitState;
use crate::persistence::{
    BranchFilter, BranchGenerationState, BranchPersistence, DatabaseError,
    MetadataHealthPersistence, PendingTimelineInitPersistence, Persistence,
};
use crate::scheduler::{ScheduleContext, ScheduleError, Scheduler};
use crate::timeline_import::{
    FinalizingImport, ImportResult, ShardImportStatuses, TimelineImport,
    TimelineImportFinalizeError, TimelineImportState, UpcallClient,
};

// For operations that should be quick, like attaching a new branch
const SHORT_RECONCILE_TIMEOUT: Duration = Duration::from_secs(5);

// For operations that might be slow, like migrating a branch with
// some data in it.
pub const RECONCILE_TIMEOUT: Duration = Duration::from_secs(30);

// If we receive a call using Secondary mode initially, it will omit generation.  We will initialize
// branches into this generation, and as long as it remains in this generation, we will accept
// input generation from future requests as authoritative.
const INITIAL_GENERATION: Generation = Generation::new(0);

/// How long [`Service::startup_reconcile`] is allowed to take before it should give
/// up on unresponsive pageservers and proceed.
pub(crate) const STARTUP_RECONCILE_TIMEOUT: Duration = Duration::from_secs(30);

/// How long a node may be unresponsive to heartbeats before we declare it offline.
/// This must be long enough to cover node restarts as well as normal operations: in future
pub const MAX_OFFLINE_INTERVAL_DEFAULT: Duration = Duration::from_secs(30);

/// How long a node may be unresponsive to heartbeats during start up before we declare it
/// offline.
///
/// This is much more lenient than [`MAX_OFFLINE_INTERVAL_DEFAULT`] since the pageserver's
/// handling of the re-attach response may take a long time and blocks heartbeats from
/// being handled on the pageserver side.
pub const MAX_WARMING_UP_INTERVAL_DEFAULT: Duration = Duration::from_secs(300);

/// How often to send heartbeats to registered nodes?
pub const HEARTBEAT_INTERVAL_DEFAULT: Duration = Duration::from_secs(5);

#[derive(Clone, strum_macros::Display)]
enum BranchOperations {
    Create,
    TimeTravelRemoteStorage,
    Delete,
    UpdatePolicy,
    TimelineCreate,
    TimelineDelete,
    AttachHook,
    TimelineArchivalConfig,
    TimelineDetachAncestor,
    TimelineGcBlockUnblock,
    TimelineLsnLease,
}

#[derive(Clone, strum_macros::Display)]
enum NodeOperations {
    Register,
    Configure,
    DeleteTombstone,
}

enum BranchGenerationValidity {
    Valid,
    Mismatched {
        claimed: Generation,
        actual: Option<Generation>,
    },
}

// Top level state available to all HTTP handlers
struct ServiceState {
    branches: BTreeMap<BranchId, Branch>,

    nodes: Arc<HashMap<NodeId, Node>>,

    scheduler: Scheduler,

    /// Tracks ongoing timeline import finalization tasks
    imports_finalizing: BTreeMap<BranchId, FinalizingImport>,
}

/// Transform an error from a pageserver into an error to return to callers of a storage
/// controller API.
fn passthrough_api_error(node: &Node, e: mgmt_api::Error) -> ApiError {
    match e {
        mgmt_api::Error::SendRequest(e) => {
            // Presume errors sending requests are connectivity/availability issues
            ApiError::ResourceUnavailable(format!("{node} error sending request: {e}").into())
        }
        mgmt_api::Error::ReceiveErrorBody(str) => {
            // Presume errors receiving body are connectivity/availability issues
            ApiError::ResourceUnavailable(
                format!("{node} error receiving error body: {str}").into(),
            )
        }
        mgmt_api::Error::ReceiveBody(err) if err.is_decode() => {
            // Return 500 for decoding errors.
            ApiError::InternalServerError(anyhow::Error::from(err).context("error decoding body"))
        }
        mgmt_api::Error::ReceiveBody(err) => {
            // Presume errors receiving body are connectivity/availability issues except for decoding errors
            let src_str = err.source().map(|e| e.to_string()).unwrap_or_default();
            ApiError::ResourceUnavailable(
                format!("{node} error receiving error body: {err} {src_str}").into(),
            )
        }
        mgmt_api::Error::ApiError(StatusCode::NOT_FOUND, msg) => {
            ApiError::NotFound(anyhow::anyhow!(format!("{node}: {msg}")).into())
        }
        mgmt_api::Error::ApiError(StatusCode::SERVICE_UNAVAILABLE, msg) => {
            ApiError::ResourceUnavailable(format!("{node}: {msg}").into())
        }
        mgmt_api::Error::ApiError(status @ StatusCode::UNAUTHORIZED, msg)
        | mgmt_api::Error::ApiError(status @ StatusCode::FORBIDDEN, msg) => {
            // Auth errors talking to a pageserver are not auth errors for the caller: they are
            // internal server errors, showing that something is wrong with the pageserver or
            // storage controller's auth configuration.
            ApiError::InternalServerError(anyhow::anyhow!("{node} {status}: {msg}"))
        }
        mgmt_api::Error::ApiError(status @ StatusCode::TOO_MANY_REQUESTS, msg) => {
            // Pass through 429 errors: if pageserver is asking us to wait + retry, we in
            // turn ask our clients to wait + retry
            ApiError::Conflict(format!("{node} {status}: {status} {msg}"))
        }
        mgmt_api::Error::ApiError(status, msg) => {
            // Presume general case of pageserver API errors is that we tried to do something
            // that can't be done right now.
            ApiError::Conflict(format!("{node} {status}: {status} {msg}"))
        }
        mgmt_api::Error::Cancelled => ApiError::ShuttingDown,
        mgmt_api::Error::Timeout(e) => ApiError::Timeout(e.into()),
    }
}

impl ServiceState {
    fn new(
        nodes: HashMap<NodeId, Node>,
        branches: BTreeMap<BranchId, Branch>,
        scheduler: Scheduler,
    ) -> Self {
        Self {
            branches,
            nodes: Arc::new(nodes),
            scheduler,
            imports_finalizing: Default::default(),
        }
    }

    fn parts_mut(
        &mut self,
    ) -> (
        &mut Arc<HashMap<NodeId, Node>>,
        &mut BTreeMap<BranchId, Branch>,
        &mut Scheduler,
    ) {
        (&mut self.nodes, &mut self.branches, &mut self.scheduler)
    }
}

#[derive(Clone)]
pub struct Config {
    // All pageservers managed by one instance of this service must have
    // the same public key.  This JWT token will be used to authenticate
    // this service to the pageservers it manages.
    pub pageserver_jwt_token: Option<String>,

    // All safekeepers managed by one instance of this service must have
    // the same public key. This JWT token will be used to authenticate
    // this service to the safekeepers it manages.
    pub safekeeper_jwt_token: Option<String>,

    // This JWT token will be used to authenticate this service to the control plane.
    pub control_plane_jwt_token: Option<String>,

    /// Prefix for storage API endpoints of the control plane. We use this prefix to compute
    /// URLs that we use to send pageserver and safekeeper attachment locations.
    pub control_plane_url: Option<String>,

    /// Grace period within which a pageserver does not respond to heartbeats, but is still
    /// considered active. Once the grace period elapses, the next heartbeat failure will
    /// mark the pagseserver offline.
    pub max_offline_interval: Duration,

    /// Extended grace period within which pageserver may not respond to heartbeats.
    /// This extended grace period kicks in after the node has been drained for restart
    /// and/or upon handling the re-attach request from a node.
    pub max_warming_up_interval: Duration,

    /// How many API requests per second to allow per branch, across all
    /// branch-scoped API endpoints. Further API requests queue until ready.
    pub tenant_rate_limit: NonZeroU32,

    pub heartbeat_interval: Duration,

    pub use_https_pageserver_api: bool,

    pub use_https_safekeeper_api: bool,

    pub ssl_ca_certs: Vec<Certificate>,

    pub timelines_onto_safekeepers: bool,

    /// Number of safekeepers to choose for a timeline when creating it.
    /// Safekeepers will be choosen from different availability zones.
    pub timeline_safekeeper_count: usize,
}

impl From<DatabaseError> for ApiError {
    fn from(err: DatabaseError) -> ApiError {
        match err {
            DatabaseError::Query(e) => ApiError::InternalServerError(e.into()),
            // FIXME: ApiError doesn't have an Unavailable variant, but ShuttingDown maps to 503.
            DatabaseError::Connection(_) | DatabaseError::ConnectionPool(_) => {
                ApiError::ShuttingDown
            }
            DatabaseError::Logical(reason) | DatabaseError::Migration(reason) => {
                ApiError::InternalServerError(anyhow::anyhow!(reason))
            }
        }
    }
}

enum InitialShardScheduleOutcome {
    Scheduled(BranchCreateResponseShard),
    NotScheduled,
    ShardScheduleError(ScheduleError),
}

pub struct Service {
    inner: Arc<std::sync::RwLock<ServiceState>>,
    config: Config,
    persistence: Arc<Persistence>,

    heartbeater_ps: Heartbeater<Node, PageserverState>,

    // Locking on a branch granularity:
    // - Take exclusively for rare operations that mutate the branch's persistent state (e.g. create/delete)
    // - Take in shared mode for operations that need the branch to stay the same to complete reliably (e.g. timeline CRUD)
    branch_op_locks: IdLockMap<BranchId, BranchOperations>,

    // Locking for node-mutating operations: take exclusively for operations that modify the node's persistent state, or
    // that transition it to/from Active.
    node_op_locks: IdLockMap<NodeId, NodeOperations>,

    // Process shutdown will fire this token
    cancel: CancellationToken,

    // Background tasks will hold this gate
    gate: Gate,

    /// This waits for initial sync with pageservers to complete.  Until this barrier
    /// passes, it isn't safe to do any actions that mutate branches.
    pub(crate) startup_complete: Barrier,

    /// HTTP client with proper CA certs.
    http_client: reqwest::Client,
}

// yb-light: branches are single-shard; shard-split scaffolding
// (ShardSplitParams / ShardSplitAction / ShardSplitTarget /
// BranchShardSplitAbort / ShardSplitInputs and the compute_split_shards
// helper) has been removed.

#[derive(Clone)]
pub(crate) struct MutationLocation {
    pub(crate) node: Node,
    pub(crate) generation: Generation,
}

#[derive(Clone)]
pub(crate) struct ShardMutationLocations {
    pub(crate) latest: MutationLocation,
}

#[derive(Default, Clone)]
pub(crate) struct BranchMutationLocations(pub BTreeMap<BranchId, ShardMutationLocations>);

impl Service {
    pub fn get_config(&self) -> &Config {
        &self.config
    }

    pub fn get_http_client(&self) -> &reqwest::Client {
        &self.http_client
    }

    /// Called once on startup: heartbeat all known nodes/safekeepers so that ones which are
    /// already responsive come up Active immediately, schedule loaded branches, and resume any
    /// in-flight timeline import finalization.
    #[instrument(skip_all)]
    async fn startup_reconcile(self: &Arc<Service>) {
        // Send initial heartbeat requests to all nodes loaded from the database
        let all_nodes = {
            let locked = self.inner.read().unwrap();
            locked.nodes.clone()
        };
        let mut nodes_online = self.initial_heartbeat_round(all_nodes.keys()).await;

        let branch_count = {
            let mut locked = self.inner.write().unwrap();
            let (nodes, branches, scheduler) = locked.parts_mut();

            // Mark nodes online if they responded to us: nodes are offline by default after a restart.
            let mut new_nodes = (**nodes).clone();
            for (node_id, node) in new_nodes.iter_mut() {
                if let Some(utilization) = nodes_online.remove(node_id) {
                    node.set_availability(NodeAvailability::Active(utilization));
                    scheduler.node_upsert(node);
                }
            }
            *nodes = Arc::new(new_nodes);

            // Populate each branch's intent state: intent was pre-seeded from
            // generation_pageserver in [`Self::spawn`]; scheduling fills in the rest.
            for (branch_id, branch) in branches.iter_mut() {
                let mut schedule_context = ScheduleContext::default();
                if let Err(e) = branch.schedule(scheduler, &mut schedule_context) {
                    // Non-fatal error: we are unable to properly schedule the branch, perhaps because
                    // not enough pageservers are available.  The branch may well still be available
                    // to clients.
                    tracing::error!("Failed to schedule branch {branch_id} at startup: {e}");
                }
            }

            branches.len()
        };

        // Resume timeline imports:
        // 1. Mark each branch with an importing timeline as importing.
        // 2. Finalize the completed imports in the background. This handles the case where
        //    the previous storage controller instance shut down whilst finalizing imports.
        let imports = self.persistence.list_timeline_imports().await;
        match imports {
            Ok(mut imports) => {
                {
                    let mut locked = self.inner.write().unwrap();
                    for import in &imports {
                        locked
                            .branches
                            .range_mut(import.branch_id..=import.branch_id)
                            .for_each(|(_id, branch)| {
                                branch.importing = TimelineImportState::Importing
                            });
                    }
                }

                imports.retain(|import| import.is_complete());
                tokio::task::spawn({
                    let finalize_imports_self = self.clone();
                    async move {
                        finalize_imports_self
                            .finalize_timeline_imports(imports)
                            .await
                    }
                });
            }
            Err(err) => {
                tracing::error!("Could not retrieve completed imports from database: {err}");
            }
        }

        tracing::info!("Startup complete ({branch_count} branches total)");
    }

    async fn initial_heartbeat_round<'a>(
        &self,
        node_ids: impl Iterator<Item = &'a NodeId>,
    ) -> HashMap<NodeId, PageserverUtilization> {
        assert!(!self.startup_complete.is_ready());

        let all_nodes = {
            let locked = self.inner.read().unwrap();
            locked.nodes.clone()
        };

        let mut nodes_to_heartbeat = HashMap::new();
        for node_id in node_ids {
            match all_nodes.get(node_id) {
                Some(node) => {
                    nodes_to_heartbeat.insert(*node_id, node.clone());
                }
                None => {
                    tracing::warn!("Node {node_id} was removed during start-up");
                }
            }
        }

        tracing::info!("Sending initial heartbeats...");
        let res_ps = self
            .heartbeater_ps
            .heartbeat(Arc::new(nodes_to_heartbeat))
            .await;

        let mut online_nodes = HashMap::new();
        if let Ok(deltas) = res_ps {
            for (node_id, status) in deltas.0 {
                match status {
                    PageserverState::Available { utilization, .. } => {
                        online_nodes.insert(node_id, utilization);
                    }
                    PageserverState::Offline => {}
                    PageserverState::WarmingUp { .. } => {
                        unreachable!("Nodes are never marked warming-up during startup reconcile")
                    }
                }
            }
        }

        online_nodes
    }

    /// Heartbeat all storage nodes once in a while.
    #[instrument(skip_all)]
    async fn spawn_heartbeat_driver(self: &Arc<Self>) {
        self.startup_complete.clone().wait().await;

        let mut interval = tokio::time::interval(self.config.heartbeat_interval);
        while !self.cancel.is_cancelled() {
            tokio::select! {
              _ = interval.tick() => { }
              _ = self.cancel.cancelled() => return
            };

            let nodes = {
                let locked = self.inner.read().unwrap();
                locked.nodes.clone()
            };

            let res_ps = self.heartbeater_ps.heartbeat(nodes).await;

            if let Ok(deltas) = res_ps {
                let mut to_handle = Vec::default();

                for (node_id, state) in deltas.0 {
                    let new_availability = match state {
                        PageserverState::Available { utilization, .. } => {
                            NodeAvailability::Active(utilization)
                        }
                        PageserverState::WarmingUp { started_at } => {
                            NodeAvailability::WarmingUp(started_at)
                        }
                        PageserverState::Offline => {
                            // The node might have been placed in the WarmingUp state
                            // while the heartbeat round was on-going. Hence, filter out
                            // offline transitions for WarmingUp nodes that are still within
                            // their grace period.
                            if let Ok(NodeAvailability::WarmingUp(started_at)) = self
                                .get_node(node_id)
                                .await
                                .as_ref()
                                .map(|n| n.get_availability())
                            {
                                let now = Instant::now();
                                if now - *started_at >= self.config.max_warming_up_interval {
                                    NodeAvailability::Offline
                                } else {
                                    NodeAvailability::WarmingUp(*started_at)
                                }
                            } else {
                                NodeAvailability::Offline
                            }
                        }
                    };

                    let node_lock = trace_exclusive_lock(
                        &self.node_op_locks,
                        node_id,
                        NodeOperations::Configure,
                    )
                    .await;

                    pausable_failpoint!("heartbeat-pre-node-state-configure");

                    // This is the code path for geniune availability transitions (i.e node
                    // goes unavailable and/or comes back online).
                    let res = self
                        .node_state_configure(node_id, Some(new_availability), &node_lock)
                        .await;

                    match res {
                        Ok(transition) => {
                            // Keep hold of the lock until the availability transitions
                            // have been handled in
                            // [`Service::handle_node_availability_transitions`] in order avoid
                            // racing with [`Service::external_node_configure`].
                            to_handle.push((node_id, node_lock, transition));
                        }
                        Err(ApiError::NotFound(_)) => {
                            // This should be rare, but legitimate since the heartbeats are done
                            // on a snapshot of the nodes.
                            tracing::info!("Node {} was not found after heartbeat round", node_id);
                        }
                        Err(ApiError::ShuttingDown) => {
                            // No-op: we're shutting down, no need to try and update any nodes' statuses
                        }
                        Err(err) => {
                            // Transition to active involves reconciling: if a node responds to a heartbeat then
                            // becomes unavailable again, we may get an error here.
                            tracing::error!(
                                "Failed to update node state {} after heartbeat round: {}",
                                node_id,
                                err
                            );
                        }
                    }
                }

                // We collected all the transitions above and now we handle them.
                let res = self.handle_node_availability_transitions(to_handle).await;
                if let Err(errs) = res {
                    for (node_id, err) in errs {
                        match err {
                            ApiError::NotFound(_) => {
                                // This should be rare, but legitimate since the heartbeats are done
                                // on a snapshot of the nodes.
                                tracing::info!(
                                    "Node {} was not found after heartbeat round",
                                    node_id
                                );
                            }
                            err => {
                                tracing::error!(
                                    "Failed to handle availability transition for {} after heartbeat round: {}",
                                    node_id,
                                    err
                                );
                            }
                        }
                    }
                }
            }
        }
    }

    pub async fn spawn(config: Config, persistence: Arc<Persistence>) -> anyhow::Result<Arc<Self>> {
        // Apply the migrations before reading from the database.
        persistence.migration_run().await?;

        tracing::info!("Loading nodes from database...");
        let nodes = persistence
            .list_nodes()
            .await?
            .into_iter()
            .map(|x| Node::from_persistent(x, config.use_https_pageserver_api))
            .collect::<anyhow::Result<Vec<Node>>>()?;
        let nodes: HashMap<NodeId, Node> = nodes.into_iter().map(|n| (n.get_id(), n)).collect();
        tracing::info!("Loaded {} nodes from database.", nodes.len());
        metrics::METRICS_REGISTRY
            .metrics_group
            .storage_controller_pageserver_nodes
            .set(nodes.len() as i64);
        metrics::METRICS_REGISTRY
            .metrics_group
            .storage_controller_https_pageserver_nodes
            .set(nodes.values().filter(|n| n.has_https_port()).count() as i64);

        tracing::info!("Loading branches from database...");
        let branch_persistence = persistence.load_active_branches().await?;
        tracing::info!(
            "Loaded {} branches from database.",
            branch_persistence.len()
        );

        // yb-light: shard splits don't exist; the recovery dance for
        // mid-split states is gone with the split machinery.

        let mut branches = BTreeMap::new();

        let mut scheduler = Scheduler::new(nodes.values());

        #[cfg(feature = "testing")]
        {
            use std::collections::HashSet;

            // Hack: insert scheduler state for all nodes referenced by branches, as compatibility
            // tests only store the branches, not the nodes.  The nodes will be loaded shortly
            // after when pageservers start up and register.
            let mut node_ids = HashSet::new();
            for tsp in &branch_persistence {
                if let Some(node_id) = tsp.generation_pageserver {
                    node_ids.insert(node_id);
                }
            }
            for node_id in node_ids {
                tracing::info!("Creating node {} in scheduler for tests", node_id);
                let node = Node::new(
                    NodeId(node_id as u64),
                    "".to_string(),
                    123,
                    None,
                    "".to_string(),
                    123,
                    None,
                    None,
                    false,
                )
                .unwrap();

                scheduler.node_upsert(&node);
            }
        }
        for tsp in branch_persistence {
            let branch_id = tsp.get_branch_id()?;

            // We will populate intent properly later in [`Self::startup_reconcile`], initially populate
            // it with what we can infer: the node for which a generation was most recently issued.
            let mut intent = IntentState::new();
            if let Some(generation_pageserver) = tsp.generation_pageserver.map(|n| NodeId(n as u64))
            {
                if nodes.contains_key(&generation_pageserver) {
                    intent.set_attached(&mut scheduler, Some(generation_pageserver));
                } else {
                    // If a node was removed before being completely drained, it is legal for it to leave behind a `generation_pageserver` referring
                    // to a non-existent node, because node deletion doesn't block on completing the reconciliations that will issue new generations
                    // on different pageservers.
                    tracing::warn!(
                        "Branch {branch_id} references non-existent node {generation_pageserver} in database, will be rescheduled"
                    );
                }
            }
            let new_branch = Branch::from_persistent(tsp, intent)?;

            branches.insert(branch_id, new_branch);
        }

        let (startup_completion, startup_complete) = utils::completion::channel();

        let cancel = CancellationToken::new();

        let mut http_client = reqwest::Client::builder();
        // We intentionally disable the connection pool, so every request will create its own TCP connection.
        // It's especially important for heartbeaters to notice more network problems.
        //
        // TODO: It makes sense to use this client only in heartbeaters and create a second one with
        // connection pooling for everything else. But reqwest::Client may create a connection without
        // ever using it (it uses hyper's Client under the hood):
        // https://github.com/hyperium/hyper-util/blob/d51318df3461d40e5f5e5ca163cb3905ac960209/src/client/legacy/client.rs#L415
        //
        // Because of a bug in hyper0::Connection::graceful_shutdown such connections hang during
        // graceful server shutdown: https://github.com/hyperium/hyper/issues/2730
        //
        // The bug has been fixed in hyper v1, so keep alive may be enabled only after we migrate to hyper1.
        http_client = http_client.pool_max_idle_per_host(0);
        for ssl_ca_cert in &config.ssl_ca_certs {
            http_client = http_client.add_root_certificate(ssl_ca_cert.clone());
        }
        let http_client = http_client.build()?;

        let heartbeater_ps = Heartbeater::new(
            http_client.clone(),
            config.pageserver_jwt_token.clone(),
            config.max_offline_interval,
            config.max_warming_up_interval,
            cancel.clone(),
        );

        let this = Arc::new(Self {
            inner: Arc::new(std::sync::RwLock::new(ServiceState::new(
                nodes, branches, scheduler,
            ))),
            config: config.clone(),
            persistence,
            heartbeater_ps,
            startup_complete: startup_complete.clone(),
            cancel,
            gate: Gate::default(),
            branch_op_locks: Default::default(),
            node_op_locks: Default::default(),
            http_client,
        });

        tokio::task::spawn({
            let this = this.clone();
            async move {
                if let Ok(_gate) = this.gate.enter() {
                    loop {
                        tokio::select! {
                            _ = this.cancel.cancelled() => {
                                break;
                            },
                            _ = tokio::time::sleep(Duration::from_secs(60)) => {}
                        };
                        this.branch_op_locks.housekeeping();
                    }
                }
            }
        });

        tokio::task::spawn({
            let this = this.clone();
            // We will block the [`Service::startup_complete`] barrier until [`Self::startup_reconcile`]
            // is done.
            let startup_completion = startup_completion.clone();
            async move {
                // Block shutdown until we're done (we must respect self.cancel)
                let Ok(_gate) = this.gate.enter() else {
                    return;
                };

                this.startup_reconcile().await;

                drop(startup_completion);
            }
        });

        tokio::task::spawn({
            let this = this.clone();
            let startup_complete = startup_complete.clone();
            async move {
                startup_complete.wait().await;
                this.spawn_heartbeat_driver().await;
            }
        });

        Ok(this)
    }

    pub(crate) async fn attach_hook(
        &self,
        attach_req: AttachHookRequest,
    ) -> anyhow::Result<AttachHookResponse> {
        let branch_id = attach_req.branch_id;
        let _branch_lock = trace_exclusive_lock(
            &self.branch_op_locks,
            branch_id,
            BranchOperations::AttachHook,
        )
        .await;

        // This is a test hook.  To enable using it on branches that were created directly with
        // the pageserver API (not via this service), we will auto-create any missing branch
        // with default state.
        let insert = {
            match self.maybe_load_branch(branch_id, &_branch_lock).await {
                Ok(_) => false,
                Err(ApiError::NotFound(_)) => true,
                Err(e) => return Err(e.into()),
            }
        };

        if insert {
            let config = attach_req.config.clone().unwrap_or_default();
            let tsp = BranchPersistence {
                branch_id: branch_id.to_string(),
                generation: attach_req.generation_override.or(Some(0)),
                generation_pageserver: None,
                placement_policy: serde_json::to_string(&PlacementPolicy::Attached(0)).unwrap(),
                config: serde_json::to_value(&config).unwrap(),
                scheduling_policy: serde_json::to_string(&ShardSchedulingPolicy::default())
                    .unwrap(),
            };

            match self.persistence.insert_branches(vec![tsp]).await {
                Err(e) => match e {
                    DatabaseError::Query(diesel::result::Error::DatabaseError(
                        DatabaseErrorKind::UniqueViolation,
                        _,
                    )) => {
                        tracing::info!("Raced with another request to insert branch {}", branch_id)
                    }
                    _ => return Err(e.into()),
                },
                Ok(()) => {
                    tracing::info!("Inserted branch {} in database", branch_id);

                    let mut branch = Branch::new(branch_id, PlacementPolicy::Attached(0));
                    branch.config = config;

                    let mut locked = self.inner.write().unwrap();
                    locked.branches.insert(branch_id, branch);
                    tracing::info!("Inserted branch {} in memory", branch_id);
                }
            }
        }

        let new_generation = if let Some(req_node_id) = attach_req.node_id {
            let maybe_branch_conf = {
                let locked = self.inner.write().unwrap();
                locked.branches.get(&branch_id).map(|t| t.config.clone())
            };

            match maybe_branch_conf {
                Some(conf) => {
                    let new_generation = self
                        .persistence
                        .increment_generation(branch_id, req_node_id)
                        .await?;

                    // Persist the placement policy update. This is required
                    // when we reattaching a detached branch.
                    self.persistence
                        .update_branch(
                            BranchFilter::Branch(branch_id),
                            Some(PlacementPolicy::Attached(0)),
                            Some(conf),
                            None,
                            None,
                        )
                        .await?;
                    Some(new_generation)
                }
                None => {
                    anyhow::bail!("Attach hook handling raced with branch removal")
                }
            }
        } else {
            self.persistence.detach(branch_id).await?;
            None
        };

        let mut locked = self.inner.write().unwrap();
        let (_nodes, branches, scheduler) = locked.parts_mut();

        let branch = branches
            .get_mut(&branch_id)
            .expect("Checked for existence above");

        if let Some(new_generation) = new_generation {
            branch.generation = Some(new_generation);
            branch.policy = PlacementPolicy::Attached(0);
        } else {
            // This is a detach notification.  We must update placement policy to avoid re-attaching
            // during background scheduling/reconciliation, or during storage controller restart.
            assert!(attach_req.node_id.is_none());
            branch.policy = PlacementPolicy::Detached;
        }

        if let Some(attaching_pageserver) = attach_req.node_id.as_ref() {
            tracing::info!(
                branch_id = %branch_id,
                ps_id = %attaching_pageserver,
                generation = ?branch.generation,
                "issuing",
            );
        } else if let Some(ps_id) = branch.intent.get_attached() {
            tracing::info!(
                branch_id = %branch_id,
                %ps_id,
                generation = ?branch.generation,
                "dropping",
            );
        } else {
            tracing::info!(
            branch_id = %branch_id,
            "no-op: branch already has no pageserver");
        }
        branch.intent.set_attached(scheduler, attach_req.node_id);

        tracing::info!(
            "attach_hook: branch {} set generation {:?}, pageserver {}, config {:?}",
            branch_id,
            branch.generation,
            // TODO: this is an odd number of 0xf's
            attach_req.node_id.unwrap_or(utils::id::NodeId(0xfffffff)),
            attach_req.config,
        );

        // Record the attached location as observed, so that tests which manually configure a
        // branch on the pageserver and then call this attach hook see a consistent state.
        #[cfg(feature = "testing")]
        {
            if let Some(node_id) = attach_req.node_id {
                branch.observed.locations = HashMap::from([(
                    node_id,
                    crate::branch::ObservedStateLocation {
                        conf: Some(models::LocationConfig {
                            generation: branch.generation.unwrap().into(),
                        }),
                    },
                )]);
            } else {
                branch.observed.locations.clear();
            }
        }

        Ok(AttachHookResponse {
            generation: attach_req
                .node_id
                .map(|_| branch.generation.expect("Test hook, not used on branches that are mid-onboarding with a NULL generation").into().unwrap()),
        })
    }

    pub(crate) fn inspect(&self, inspect_req: InspectRequest) -> InspectResponse {
        let locked = self.inner.read().unwrap();

        let branch = locked.branches.get(&inspect_req.branch_id);

        InspectResponse {
            attachment: branch.and_then(|s| {
                s.intent
                    .get_attached()
                    .map(|ps| (s.generation.expect("Test hook, not used on branches that are mid-onboarding with a NULL generation").into().unwrap(), ps))
            }),
        }
    }

    pub(crate) async fn tenant_create(
        &self,
        create_req: BranchCreateRequest,
    ) -> Result<BranchCreateResponse, ApiError> {
        let branch_id = create_req.new_branch_id;

        // Exclude any concurrent attempts to create/access the same branch ID
        let _branch_lock =
            trace_exclusive_lock(&self.branch_op_locks, branch_id, BranchOperations::Create).await;
        self.do_tenant_create(create_req).await
    }

    pub(crate) async fn do_tenant_create(
        &self,
        create_req: BranchCreateRequest,
    ) -> Result<BranchCreateResponse, ApiError> {
        let placement_policy = create_req
            .placement_policy
            .clone()
            // As a default, zero secondaries is convenient for tests that don't choose a policy.
            .unwrap_or(PlacementPolicy::Attached(0));

        let branch_id = create_req.new_branch_id;

        tracing::info!("Creating branch {}", create_req.new_branch_id);

        // yb-light: always single shard regardless of requested count.
        let create_ids = vec![branch_id];

        // If the caller specifies a None generation, it means "start from default".
        let initial_generation = if matches!(placement_policy, PlacementPolicy::Secondary) {
            tracing::info!(
                "tenant_create: secondary mode, generation is_some={}",
                create_req.generation.is_some()
            );
            create_req.generation.map(Generation::new)
        } else {
            tracing::info!(
                "tenant_create: not secondary mode, generation is_some={}",
                create_req.generation.is_some()
            );
            Some(
                create_req
                    .generation
                    .map(Generation::new)
                    .unwrap_or(INITIAL_GENERATION),
            )
        };

        // Ordering: we persist branches before creating them on the pageserver.  This enables a caller
        // to clean up after themselves by issuing a branch deletion if something goes wrong and we restart
        // during the creation, rather than risking leaving orphan objects in S3.
        let persist_branches = create_ids
            .iter()
            .map(|branch_id| BranchPersistence {
                branch_id: branch_id.to_string(),
                generation: initial_generation.map(|g| g.into().unwrap() as i32),
                // The pageserver is not known until scheduling happens: we will set this column when
                // incrementing the generation the first time we attach to a pageserver.
                generation_pageserver: None,
                placement_policy: serde_json::to_string(&placement_policy).unwrap(),
                config: serde_json::to_value(&create_req.config).unwrap(),
                scheduling_policy: serde_json::to_string(&ShardSchedulingPolicy::default())
                    .unwrap(),
            })
            .collect();

        match self.persistence.insert_branches(persist_branches).await {
            Ok(_) => {}
            Err(DatabaseError::Query(diesel::result::Error::DatabaseError(
                DatabaseErrorKind::UniqueViolation,
                _,
            ))) => {
                // Unique key violation: this is probably a retry, and the branch id is the unique
                // key, so a violation means the same branch is being created again.
                tracing::info!(
                    "Branch already present in database, proceeding with idempotent creation..."
                );
            }
            // Any other database error is unexpected and a bug.
            Err(e) => return Err(ApiError::InternalServerError(anyhow::anyhow!(e))),
        };

        let mut schedule_context = ScheduleContext::default();
        let mut schedule_error = None;
        let mut response_shards = Vec::new();
        for branch_id in create_ids {
            tracing::info!("Creating branch {branch_id}...");

            let outcome = self
                .do_initial_shard_scheduling(
                    branch_id,
                    initial_generation,
                    create_req.config.clone(),
                    placement_policy.clone(),
                    &mut schedule_context,
                )
                .await;

            match outcome {
                InitialShardScheduleOutcome::Scheduled(resp) => response_shards.push(resp),
                InitialShardScheduleOutcome::NotScheduled => {}
                InitialShardScheduleOutcome::ShardScheduleError(err) => {
                    schedule_error = Some(err);
                }
            }
        }

        // If we failed to schedule the branch, then it is still created in the controller,
        // but we return an error to the requester to avoid a silent failure when someone
        // tries to e.g. create a branch whose placement policy requires more nodes than
        // are present in the system.
        if let Some(e) = schedule_error {
            return Err(ApiError::Conflict(format!(
                "Failed to schedule branch(es): {e}"
            )));
        }

        Ok(BranchCreateResponse {
            shards: response_shards,
        })
    }

    /// Helper for branch creation that does the scheduling for the branch. Covers both the
    /// case of a new branch and a pre-existing one.
    #[allow(clippy::too_many_arguments)]
    async fn do_initial_shard_scheduling(
        &self,
        branch_id: BranchId,
        initial_generation: Option<Generation>,
        config: BranchConfig,
        placement_policy: PlacementPolicy,
        schedule_context: &mut ScheduleContext,
    ) -> InitialShardScheduleOutcome {
        let mut locked = self.inner.write().unwrap();
        let (_nodes, branches, scheduler) = locked.parts_mut();

        use std::collections::btree_map::Entry;
        match branches.entry(branch_id) {
            Entry::Occupied(mut entry) => {
                tracing::info!("Branch {branch_id} already exists while creating");

                if let Err(err) = entry.get_mut().schedule(scheduler, schedule_context) {
                    return InitialShardScheduleOutcome::ShardScheduleError(err);
                }

                if let Some(node_id) = entry.get().intent.get_attached() {
                    let generation = entry
                        .get()
                        .generation
                        .expect("Generation is set when in attached mode");
                    InitialShardScheduleOutcome::Scheduled(BranchCreateResponseShard {
                        branch_id,
                        node_id: *node_id,
                        generation: generation.into().unwrap(),
                    })
                } else {
                    InitialShardScheduleOutcome::NotScheduled
                }
            }
            Entry::Vacant(entry) => {
                let state = entry.insert(Branch::new(branch_id, placement_policy));

                state.generation = initial_generation;
                state.config = config;
                if let Err(e) = state.schedule(scheduler, schedule_context) {
                    return InitialShardScheduleOutcome::ShardScheduleError(e);
                }

                // Only include the branch in result if we are attaching: the purpose
                // of the response is to tell the caller where the branch is attached.
                if let Some(node_id) = state.intent.get_attached() {
                    let generation = state
                        .generation
                        .expect("Generation is set when in attached mode");
                    InitialShardScheduleOutcome::Scheduled(BranchCreateResponseShard {
                        branch_id,
                        node_id: *node_id,
                        generation: generation.into().unwrap(),
                    })
                } else {
                    InitialShardScheduleOutcome::NotScheduled
                }
            }
        }
    }

    /// For APIs that might act on branches with [`PlacementPolicy::Detached`], first check if
    /// the branch is present in memory. If not, load it from the database.  If it is found
    /// in neither location, return a NotFound error.
    ///
    /// Caller must demonstrate they hold a lock guard, as otherwise two callers might try and load
    /// it at the same time.
    async fn maybe_load_branch(
        &self,
        branch_id: BranchId,
        _guard: &TracingExclusiveGuard<BranchOperations>,
    ) -> Result<(), ApiError> {
        // Check if the branch is present in memory, and select an AZ to use when loading
        // if we will load it.
        {
            let locked = self.inner.read().unwrap();
            let existing = locked.branches.range(branch_id..=branch_id).next();

            if existing.is_some() {
                // We already have this branch in memory
                return Ok(());
            }

            if locked.nodes.is_empty() {
                return Err(ApiError::BadRequest(anyhow::anyhow!(
                    "No nodes found to load branch"
                )));
            }
        };

        let branches = self.persistence.load_branch(branch_id).await?;
        if branches.is_empty() {
            return Err(ApiError::NotFound(
                anyhow::anyhow!("Branch {} not found", branch_id).into(),
            ));
        }

        let mut locked = self.inner.write().unwrap();
        tracing::info!("Loaded {} entries for branch {}", branches.len(), branch_id);

        locked.branches.extend(branches.into_iter().map(|p| {
            let intent = IntentState::new();
            let branch =
                Branch::from_persistent(p, intent).expect("Corrupt branch row in database");

            // Sanity check: when loading on-demand, we should always be loaded something Detached
            debug_assert!(branch.policy == PlacementPolicy::Detached);
            if branch.policy != PlacementPolicy::Detached {
                tracing::error!(
                    "Branch {} loaded on-demand, but has non-Detached policy {:?}",
                    branch.branch_id,
                    branch.policy
                );
            }

            (branch.branch_id, branch)
        }));

        Ok(())
    }

    pub(crate) async fn tenant_time_travel_remote_storage(
        &self,
        branch_id: BranchId,
        timestamp: Cow<'_, str>,
        done_if_after: Cow<'_, str>,
    ) -> Result<(), ApiError> {
        let _branch_lock = trace_exclusive_lock(
            &self.branch_op_locks,
            branch_id,
            BranchOperations::TimeTravelRemoteStorage,
        )
        .await;

        let node = {
            let mut locked = self.inner.write().unwrap();
            // Just a sanity check to prevent misuse: the API expects that the branch is fully
            // detached everywhere, and nothing writes to S3 storage. Here, we verify that,
            // but only at the start of the process, so it's really just to prevent operator
            // mistakes.
            for (id, branch) in locked.branches.range(branch_id..=branch_id) {
                if branch.intent.get_attached().is_some()
                    || !branch.intent.get_secondary().is_empty()
                {
                    return Err(ApiError::InternalServerError(anyhow::anyhow!(
                        "We want branch to be detached, but it is attached: branch_id={id}"
                    )));
                }
                let maybe_attached =
                    branch
                        .observed
                        .locations
                        .iter()
                        .find(|(_, observed_location)| {
                            observed_location
                                .conf
                                .as_ref()
                                .is_some_and(|loc| loc.generation.is_some())
                        });
                if let Some((node_id, _observed_location)) = maybe_attached {
                    return Err(ApiError::InternalServerError(anyhow::anyhow!(
                        "Observed attached branch on node_id={node_id} branch_id={id}"
                    )));
                }
            }
            let scheduler = &mut locked.scheduler;
            // Right now we only perform the operation on a single node without parallelization
            // TODO fan out the operation to multiple nodes for better performance
            let node_id = scheduler.any_available_node()?;
            let node = locked
                .nodes
                .get(&node_id)
                .expect("Pageservers may not be deleted while lock is active");
            node.clone()
        };

        // yb-light is always single-shard, so the recovery runs once against the chosen node.
        let client = PageserverClient::new(
            node.get_id(),
            self.http_client.clone(),
            node.base_url(),
            self.config.pageserver_jwt_token.as_deref(),
        );

        tracing::info!("Doing time travel recovery for branch {branch_id}",);

        client
            .branch_time_travel_remote_storage(branch_id, &timestamp, &done_if_after)
            .await
            .map_err(|e| {
                ApiError::InternalServerError(anyhow::anyhow!(
                    "Error doing time travel recovery for branch {branch_id} on node {}: {e}",
                    node
                ))
            })?;
        Ok(())
    }

    pub(crate) async fn tenant_delete(
        self: &Arc<Self>,
        branch_id: BranchId,
    ) -> Result<StatusCode, ApiError> {
        let _branch_lock =
            trace_exclusive_lock(&self.branch_op_locks, branch_id, BranchOperations::Delete).await;

        self.maybe_load_branch(branch_id, &_branch_lock).await?;

        // Detach the branch: update its intent to remove all attachments.
        let node = {
            let mut locked = self.inner.write().unwrap();
            let (nodes, branches, scheduler) = locked.parts_mut();
            for (_, branch) in branches.range_mut(branch_id..=branch_id) {
                branch.policy = PlacementPolicy::Detached;
                branch
                    .schedule(scheduler, &mut ScheduleContext::default())
                    .expect("De-scheduling is infallible");
                debug_assert!(branch.intent.get_attached().is_none());
                debug_assert!(branch.intent.get_secondary().is_empty());
            }

            // Pick an arbitrary node to use for remote deletions (does not have to be where the branch
            // was attached, just has to be able to see the S3 content)
            let node_id = scheduler.any_available_node()?;
            let node = nodes
                .get(&node_id)
                .expect("Pageservers may not be deleted while lock is active");
            node.clone()
        };

        // Delete the entire branch from remote storage via a random pageserver.
        // The branch id prefix causes the pageserver to remove all remote paths with
        // that prefix.
        match node
            .with_client_retries(
                |client| async move { client.branch_delete(branch_id).await },
                &self.http_client,
                &self.config.pageserver_jwt_token,
                1,
                3,
                RECONCILE_TIMEOUT,
                &self.cancel,
            )
            .await
            .unwrap_or(Err(mgmt_api::Error::Cancelled))
        {
            Ok(_) => {}
            Err(mgmt_api::Error::Cancelled) => {
                return Err(ApiError::ShuttingDown);
            }
            Err(e) => {
                // This is unexpected: remote deletion should be infallible, unless the object store
                // at large is unavailable.
                tracing::error!("Error deleting via node {node}: {e}");
                return Err(ApiError::InternalServerError(anyhow::anyhow!(e)));
            }
        }

        // Fall through: deletion of the branch on pageservers is complete, we may proceed to drop
        // our in-memory state and database state.

        // Ordering: we delete persistent state first: if we then
        // crash, we will drop the in-memory state.

        // Drop persistent state.
        self.persistence.delete_branch(branch_id).await?;

        // Drop in-memory state
        {
            let mut locked = self.inner.write().unwrap();
            let (_nodes, branches, scheduler) = locked.parts_mut();

            // Dereference Scheduler from the branch before dropping it
            for (_branch_id, branch) in branches.range_mut(branch_id..=branch_id) {
                branch.intent.clear(scheduler);
            }

            branches.retain(|id, _branch| *id != branch_id);
            tracing::info!(
                "Deleted branch {branch_id}, now have {} branches",
                locked.branches.len()
            );
        };

        // The branch's safekeeper data is owned by the launcher and reaped
        // separately (see #171); the controller only drops its own records.

        // Success is represented as 404, to imitate the existing pageserver deletion API
        Ok(StatusCode::NOT_FOUND)
    }

    /// Modifies the branch's policies (placement/scheduling configuration) within the
    /// storage controller; nothing is pushed to pageservers.
    pub(crate) async fn tenant_update_policy(
        &self,
        branch_id: BranchId,
        req: BranchPolicyRequest,
    ) -> Result<(), ApiError> {
        // We require an exclusive lock, because we are updating persistent and in-memory state
        let _branch_lock = trace_exclusive_lock(
            &self.branch_op_locks,
            branch_id,
            BranchOperations::UpdatePolicy,
        )
        .await;

        self.maybe_load_branch(branch_id, &_branch_lock).await?;

        failpoint_support::sleep_millis_async!("tenant-update-policy-exclusive-lock");

        let BranchPolicyRequest {
            placement,
            mut scheduling,
        } = req;

        if let Some(PlacementPolicy::Detached | PlacementPolicy::Secondary) = placement {
            // When someone configures a branch to detach, we force the scheduling policy to enable
            // this to take effect.
            if scheduling.is_none() {
                scheduling = Some(ShardSchedulingPolicy::Active);
            }
        }

        self.persistence
            .update_branch(
                BranchFilter::Branch(branch_id),
                placement.clone(),
                None,
                None,
                scheduling,
            )
            .await?;

        let mut schedule_context = ScheduleContext::default();
        let mut locked = self.inner.write().unwrap();
        let (_nodes, branches, scheduler) = locked.parts_mut();
        for (id, branch) in branches.range_mut(branch_id..=branch_id) {
            if let Some(placement) = &placement {
                branch.policy = placement.clone();

                tracing::info!(branch_id=%id,
                               "Updated placement policy to {placement:?}");
            }

            if let Some(scheduling) = &scheduling {
                branch.set_scheduling_policy(*scheduling);

                tracing::info!(branch_id=%id,
                               "Updated scheduling policy to {scheduling:?}");
            }

            // In case scheduling is being switched back on, try it now.
            branch.schedule(scheduler, &mut schedule_context).ok();
        }

        Ok(())
    }

    /// yb-light: the pageserver creates its single timeline ITSELF at
    /// process startup, driven by the `[timeline_init]` block the
    /// launcher wrote into its `pageserver.toml` (the pageserver-side
    /// timeline-create HTTP API no longer exists). This function
    /// therefore only *observes*: it polls the attached pageserver's
    /// timeline_detail until the configured timeline shows up Active
    /// (bootstrap initdb+import can take tens of seconds on debug
    /// builds), then returns its TimelineInfo — the safekeeper leg
    /// consumes `last_record_lsn` from it.
    pub(crate) async fn tenant_timeline_create_pageservers(
        &self,
        branch_id: BranchId,
        // The pageserver creates its single timeline itself at startup, so the
        // request body is not consumed here; we only observe the result.
        _create_req: TimelineCreateRequest,
    ) -> Result<TimelineInfo, ApiError> {
        const OBSERVE_TIMEOUT: Duration = Duration::from_secs(180);
        const POLL_INTERVAL: Duration = Duration::from_millis(500);

        tracing::info!("Waiting for branch {branch_id} to become active on its pageserver",);

        self.tenant_remote_mutation(branch_id, move |mut targets| async move {
            if targets.0.is_empty() {
                return Err(ApiError::NotFound(
                    anyhow::anyhow!("Branch not found").into(),
                ));
            };

            let (shard_zero_bid, shard_zero_locations) = targets
                .0
                .pop_first()
                .expect("Must have at least one branch");

            let node = shard_zero_locations.latest.node;
            let client = PageserverClient::new(
                node.get_id(),
                self.http_client.clone(),
                node.base_url(),
                self.config.pageserver_jwt_token.as_deref(),
            );

            let deadline = tokio::time::Instant::now() + OBSERVE_TIMEOUT;
            loop {
                match client.branch_detail(shard_zero_bid).await {
                    Ok(info) if matches!(info.state, models::TimelineState::Active) => {
                        return Ok(info);
                    }
                    Ok(info) => {
                        tracing::info!(
                            state = ?info.state,
                            "branch present on pageserver, waiting for Active",
                        );
                    }
                    Err(e) => {
                        // Bootstrap still running (404), branch still
                        // attaching (503), or the process is mid-start
                        // (transport error) — all retryable until the
                        // deadline. Genuine failures surface as a
                        // timeout with the last error attached.
                        tracing::info!("branch not ready on {node}: {e}");
                        if tokio::time::Instant::now() >= deadline {
                            return Err(ApiError::Timeout(
                                format!(
                                    "timed out waiting for branch \
                                     {branch_id} on {node}: {e}"
                                )
                                .into(),
                            ));
                        }
                    }
                }
                if tokio::time::Instant::now() >= deadline {
                    return Err(ApiError::Timeout(
                        format!(
                            "timed out waiting for branch {branch_id} \
                             to become Active on {node}"
                        )
                        .into(),
                    ));
                }
                tokio::time::sleep(POLL_INTERVAL).await;
            }
        })
        .await?
    }

    pub(crate) async fn tenant_timeline_create(
        self: &Arc<Self>,
        branch_id: BranchId,
        create_req: TimelineCreateRequest,
    ) -> Result<TimelineCreateResponseStorcon, ApiError> {
        tracing::info!(
            mode=%create_req.mode_tag(),
            "Creating branch {}",
            branch_id,
        );

        let _branch_lock = trace_shared_lock(
            &self.branch_op_locks,
            branch_id,
            BranchOperations::TimelineCreate,
        )
        .await;
        failpoint_support::sleep_millis_async!("tenant-create-timeline-shared-lock");
        let is_import = create_req.is_import();

        if is_import {
            // Ensure that there is no split on-going.
            let locked = self.inner.read().unwrap();
            let splitting = locked
                .branches
                .range(branch_id..=branch_id)
                .any(|(_id, branch)| branch.splitting != SplitState::Idle);

            if splitting {
                return Err(ApiError::Conflict("Branch is splitting".to_string()));
            }
        }

        let timeline_info = self
            .tenant_timeline_create_pageservers(branch_id, create_req)
            .await?;

        // The controller never creates a timeline on the safekeeper (the
        // launcher owns the branch's single co-located safekeeper, see #171).
        // It only tracks the in-progress import here so finalization can run.
        if is_import {
            let import = TimelineImport {
                branch_id,
                shard_statuses: ShardImportStatuses::new(),
            };

            let inserted = self
                .persistence
                .insert_timeline_import(import.to_persistent())
                .await
                .context("timeline import insert")
                .map_err(ApiError::InternalServerError)?;

            // Set the importing flag on the branch
            self.inner
                .write()
                .unwrap()
                .branches
                .range_mut(branch_id..=branch_id)
                .for_each(|(_id, branch)| branch.importing = TimelineImportState::Importing);

            match inserted {
                true => {
                    tracing::info!(%branch_id, "Inserted timeline import");
                }
                false => {
                    tracing::info!(%branch_id, "Timeline import entry already present");
                }
            }
        }

        Ok(TimelineCreateResponseStorcon { timeline_info })
    }

    #[instrument(skip_all, fields(
        branch_id=%req.branch_id,
    ))]
    pub(crate) async fn handle_timeline_shard_import_progress(
        self: &Arc<Self>,
        req: TimelineImportStatusRequest,
    ) -> Result<BranchImportStatus, ApiError> {
        let validity = self
            .validate_shard_generation(req.branch_id, req.generation)
            .await?;
        match validity {
            BranchGenerationValidity::Valid => {
                // fallthrough
            }
            BranchGenerationValidity::Mismatched { claimed, actual } => {
                tracing::info!(
                    claimed=?claimed.into(),
                    actual=?actual.and_then(|g| g.into()),
                    "Rejecting import progress fetch from stale generation"
                );

                return Err(ApiError::BadRequest(anyhow::anyhow!("Invalid generation")));
            }
        }

        let maybe_import = self.persistence.get_timeline_import(req.branch_id).await?;

        let import = maybe_import.ok_or_else(|| {
            ApiError::NotFound(format!("import for {} not found", req.branch_id).into())
        })?;

        Ok(import.shard_statuses.0.clone())
    }

    #[instrument(skip_all, fields(
        branch_id=%req.branch_id,
    ))]
    pub(crate) async fn handle_timeline_shard_import_progress_upcall(
        self: &Arc<Self>,
        req: PutTimelineImportStatusRequest,
    ) -> Result<(), ApiError> {
        let validity = self
            .validate_shard_generation(req.branch_id, req.generation)
            .await?;
        match validity {
            BranchGenerationValidity::Valid => {
                // fallthrough
            }
            BranchGenerationValidity::Mismatched { claimed, actual } => {
                tracing::info!(
                    claimed=?claimed.into(),
                    actual=?actual.and_then(|g| g.into()),
                    "Rejecting import progress update from stale generation"
                );

                return Err(ApiError::PreconditionFailed("Invalid generation".into()));
            }
        }

        let res = self
            .persistence
            .update_timeline_import(req.branch_id, req.status)
            .await;
        let timeline_import = match res {
            Ok(Ok(Some(timeline_import))) => timeline_import,
            Ok(Ok(None)) => {
                // Idempotency: we've already seen and handled this update.
                return Ok(());
            }
            Ok(Err(logical_err)) => {
                return Err(logical_err.into());
            }
            Err(db_err) => {
                return Err(db_err.into());
            }
        };

        tracing::info!(
            branch_id=%req.branch_id,
            "Updated timeline import status to: {timeline_import:?}");

        if timeline_import.is_complete() {
            tokio::task::spawn({
                let this = self.clone();
                async move { this.finalize_timeline_import(timeline_import).await }
            });
        }

        Ok(())
    }

    /// Check that a provided generation for some branch is the most recent one.
    ///
    /// Validate with the in-mem state first, and, if that passes, validate with the
    /// database state which is authoritative.
    async fn validate_shard_generation(
        self: &Arc<Self>,
        branch_id: BranchId,
        generation: Generation,
    ) -> Result<BranchGenerationValidity, ApiError> {
        {
            let locked = self.inner.read().unwrap();
            let branch = locked
                .branches
                .get(&branch_id)
                .ok_or(ApiError::InternalServerError(anyhow::anyhow!(
                    "{} branch not found",
                    branch_id
                )))?;

            if branch.generation != Some(generation) {
                return Ok(BranchGenerationValidity::Mismatched {
                    claimed: generation,
                    actual: branch.generation,
                });
            }
        }

        let mut db_generations = self
            .persistence
            .branches_generations(std::iter::once(&branch_id))
            .await?;
        let (_bid, db_generation) =
            db_generations
                .pop()
                .ok_or(ApiError::InternalServerError(anyhow::anyhow!(
                    "{} branch not found",
                    branch_id
                )))?;

        if db_generation != Some(generation) {
            return Ok(BranchGenerationValidity::Mismatched {
                claimed: generation,
                actual: db_generation,
            });
        }

        Ok(BranchGenerationValidity::Valid)
    }

    /// Finalize the import of a timeline
    ///
    /// This method should be called once all shards have reported that the import is complete.
    /// Firstly, it polls the post import timeline activation endpoint exposed by the pageserver.
    /// Once the timeline is active on all shards, the timeline also gets created on the
    /// safekeepers. Finally, notify cplane of the import completion (whether failed or
    /// successful), and remove the import from the database and in-memory.
    ///
    /// If this method gets pre-empted by shut down, it will be called again at start-up (on-going
    /// imports are stored in the database).
    ///
    /// # Cancel-Safety
    /// Not cancel safe.
    /// If the caller stops polling, the import will not be removed from
    /// [`ServiceState::imports_finalizing`].
    #[instrument(skip_all, fields(
        branch_id=%import.branch_id,
    ))]

    async fn finalize_timeline_import(
        self: &Arc<Self>,
        import: TimelineImport,
    ) -> Result<(), TimelineImportFinalizeError> {
        let import_branch_id = import.branch_id;

        let (_finalize_import_guard, cancel) = {
            let mut locked = self.inner.write().unwrap();
            let gate = Gate::default();
            let cancel = CancellationToken::default();

            let guard = gate.enter().unwrap();

            locked.imports_finalizing.insert(
                import_branch_id,
                FinalizingImport {
                    gate,
                    cancel: cancel.clone(),
                },
            );

            (guard, cancel)
        };

        let res = tokio::select! {
            res = self.finalize_timeline_import_impl(import) => {
                res
            },
            _ = cancel.cancelled() => {
                Err(TimelineImportFinalizeError::Cancelled)
            }
        };

        let mut locked = self.inner.write().unwrap();
        locked.imports_finalizing.remove(&import_branch_id);

        res
    }

    async fn finalize_timeline_import_impl(
        self: &Arc<Self>,
        import: TimelineImport,
    ) -> Result<(), TimelineImportFinalizeError> {
        tracing::info!("Finalizing timeline import");

        pausable_failpoint!("timeline-import-pre-cplane-notification");

        let branch_id = import.branch_id;

        let import_error = import.completion_error();
        match import_error {
            Some(err) => {
                self.notify_cplane_and_delete_import(branch_id, Err(err))
                    .await?;
                tracing::warn!("Timeline import completed with errors");
                Ok(())
            }
            None => match self.activate_timeline_post_import(&import).await {
                Ok(_timeline_info) => {
                    tracing::info!("Post import timeline activation complete");

                    // The branch's single safekeeper is owned by the launcher,
                    // which already created the timeline on it; the controller
                    // does not create it on safekeepers (see #171).

                    self.notify_cplane_and_delete_import(branch_id, Ok(()))
                        .await?;

                    tracing::info!("Timeline import completed successfully");
                    Ok(())
                }
                Err(TimelineImportFinalizeError::ShuttingDown) => {
                    // We got pre-empted by shut down and will resume after the restart.
                    Err(TimelineImportFinalizeError::ShuttingDown)
                }
                Err(err) => {
                    // Any finalize error apart from shut down is permanent and requires us to notify
                    // cplane such that it can clean up.
                    tracing::error!("Import finalize failed with permanent error: {err}");
                    self.notify_cplane_and_delete_import(branch_id, Err(err.to_string()))
                        .await?;
                    Err(err)
                }
            },
        }
    }

    async fn notify_cplane_and_delete_import(
        self: &Arc<Self>,
        branch_id: BranchId,
        import_result: ImportResult,
    ) -> Result<(), TimelineImportFinalizeError> {
        let import_failed = import_result.is_err();
        tracing::info!(%import_failed, "Notifying cplane of import completion");

        let client = UpcallClient::new(self.get_config(), self.cancel.child_token());
        client
            .notify_import_complete(branch_id, import_result)
            .await
            .map_err(|_err| TimelineImportFinalizeError::ShuttingDown)?;

        if let Err(err) = self.persistence.delete_timeline_import(branch_id).await {
            tracing::warn!("Failed to delete timeline import entry from database: {err}");
        }

        self.inner
            .write()
            .unwrap()
            .branches
            .range_mut(branch_id..=branch_id)
            .for_each(|(_id, branch)| branch.importing = TimelineImportState::Idle);

        Ok(())
    }

    /// Activate an imported timeline on its branch once the import is complete.
    /// Returns the [`TimelineInfo`] reported by shard zero.
    async fn activate_timeline_post_import(
        self: &Arc<Self>,
        import: &TimelineImport,
    ) -> Result<TimelineInfo, TimelineImportFinalizeError> {
        const TIMELINE_ACTIVATE_TIMEOUT: Duration = Duration::from_millis(128);

        let mut shard_zero_timeline_info = None;

        while shard_zero_timeline_info.is_none() {
            if self.cancel.is_cancelled() {
                return Err(TimelineImportFinalizeError::ShuttingDown);
            }

            let targets = {
                let locked = self.inner.read().unwrap();
                let mut targets = Vec::new();

                for (branch_id, branch) in
                    locked.branches.range(import.branch_id..=import.branch_id)
                {
                    if let Some(node_id) = branch.intent.get_attached() {
                        let node = locked
                            .nodes
                            .get(node_id)
                            .expect("Pageservers may not be deleted while referenced");
                        targets.push((*branch_id, node.clone()));
                    }
                }

                targets
            };

            let results = self
                .tenant_for_shards_api(
                    targets,
                    |branch_id, client| async move {
                        client
                            .activate_post_import(branch_id, TIMELINE_ACTIVATE_TIMEOUT)
                            .await
                    },
                    1,
                    1,
                    SHORT_RECONCILE_TIMEOUT,
                    &self.cancel,
                )
                .await;

            let mut failed = 0;
            for (_, result) in results.into_iter() {
                match result {
                    Ok(ok) => {
                        shard_zero_timeline_info = Some(ok);
                    }
                    Err(_err) => {
                        failed += 1;
                    }
                }
            }

            if failed > 0 {
                tracing::info!(
                    "Failed to activate timeline on {failed} shards post import. Will retry"
                );
            }

            tokio::select! {
                _ = tokio::time::sleep(Duration::from_millis(250)) => {},
                _ = self.cancel.cancelled() => {
                    return Err(TimelineImportFinalizeError::ShuttingDown);
                }
            }
        }

        Ok(shard_zero_timeline_info.expect("All shards replied"))
    }

    async fn finalize_timeline_imports(self: &Arc<Self>, imports: Vec<TimelineImport>) {
        futures::future::join_all(
            imports
                .into_iter()
                .map(|import| self.finalize_timeline_import(import)),
        )
        .await;
    }

    /// Delete a timeline import if it exists
    ///
    /// Firstly, delete the entry from the database. Any updates
    /// from pageservers after the update will fail with a 404, so the
    /// import cannot progress into finalizing state if it's not there already.
    /// Secondly, cancel the finalization if one is in progress.
    pub(crate) async fn maybe_delete_timeline_import(
        self: &Arc<Self>,
        branch_id: BranchId,
    ) -> Result<(), DatabaseError> {
        let branch_has_ongoing_import = {
            let locked = self.inner.read().unwrap();
            locked
                .branches
                .range(branch_id..=branch_id)
                .any(|(_bid, branch)| branch.importing == TimelineImportState::Importing)
        };

        if !branch_has_ongoing_import {
            return Ok(());
        }

        self.persistence.delete_timeline_import(branch_id).await?;

        let maybe_finalizing = {
            let mut locked = self.inner.write().unwrap();
            locked.imports_finalizing.remove(&branch_id)
        };

        if let Some(finalizing) = maybe_finalizing {
            finalizing.cancel.cancel();
            finalizing.gate.close().await;
        }

        Ok(())
    }

    pub(crate) async fn tenant_timeline_archival_config(
        &self,
        branch_id: BranchId,
        req: TimelineArchivalConfigRequest,
    ) -> Result<(), ApiError> {
        tracing::info!(
            "Setting archival config of branch {branch_id} to '{:?}'",
            req.state
        );

        let _branch_lock = trace_shared_lock(
            &self.branch_op_locks,
            branch_id,
            BranchOperations::TimelineArchivalConfig,
        )
        .await;

        self.tenant_remote_mutation(branch_id, move |targets| async move {
            if targets.0.is_empty() {
                return Err(ApiError::NotFound(
                    anyhow::anyhow!("Branch not found").into(),
                ));
            }
            async fn config_one(
                branch_id: BranchId,
                node: Node,
                http_client: reqwest::Client,
                jwt: Option<String>,
                req: TimelineArchivalConfigRequest,
            ) -> Result<(), ApiError> {
                tracing::info!(
                    "Setting archival config of branch {branch_id}, attached to node {node}",
                );

                let client = PageserverClient::new(
                    node.get_id(),
                    http_client,
                    node.base_url(),
                    jwt.as_deref(),
                );

                client
                    .branch_archival_config(branch_id, &req)
                    .await
                    .map_err(|e| match e {
                        mgmt_api::Error::ApiError(StatusCode::PRECONDITION_FAILED, msg) => {
                            ApiError::PreconditionFailed(msg.into_boxed_str())
                        }
                        _ => passthrough_api_error(&node, e),
                    })
            }

            // no shard needs to go first/last; the operation should be idempotent
            // TODO: it would be great to ensure that all shards return the same error
            let locations = targets
                .0
                .iter()
                .map(|t| (*t.0, t.1.latest.node.clone()))
                .collect();
            let results = self
                .tenant_for_shards(locations, |branch_id, node| {
                    futures::FutureExt::boxed(config_one(
                        branch_id,
                        node,
                        self.http_client.clone(),
                        self.config.pageserver_jwt_token.clone(),
                        req.clone(),
                    ))
                })
                .await?;
            assert!(!results.is_empty(), "must have at least one result");

            Ok(())
        })
        .await?
    }

    pub(crate) async fn tenant_timeline_detach_ancestor(
        &self,
        branch_id: BranchId,
        behavior: Option<DetachBehavior>,
    ) -> Result<models::detach_ancestor::AncestorDetached, ApiError> {
        tracing::info!("Detaching branch {branch_id}",);

        let _branch_lock = trace_shared_lock(
            &self.branch_op_locks,
            branch_id,
            BranchOperations::TimelineDetachAncestor,
        )
        .await;

        self.tenant_remote_mutation(branch_id, move |targets| async move {
            if targets.0.is_empty() {
                return Err(ApiError::NotFound(
                    anyhow::anyhow!("Branch not found").into(),
                ));
            }

            async fn detach_one(
                branch_id: BranchId,
                node: Node,
                http_client: reqwest::Client,
                jwt: Option<String>,
                behavior: Option<DetachBehavior>,
            ) -> Result<models::detach_ancestor::AncestorDetached, ApiError> {
                tracing::info!(
                    "Detaching branch {branch_id}, attached to node {node}",
                );

                let client = PageserverClient::new(node.get_id(), http_client, node.base_url(), jwt.as_deref());

                client
                    .branch_detach_ancestor(branch_id, behavior)
                    .await
                    .map_err(|e| {
                        use mgmt_api::Error;

                        match e {
                            // no ancestor (ever)
                            Error::ApiError(StatusCode::CONFLICT, msg) => ApiError::Conflict(format!(
                                "{node}: {}",
                                msg.strip_prefix("Conflict: ").unwrap_or(&msg)
                            )),
                            // too many ancestors
                            Error::ApiError(StatusCode::BAD_REQUEST, msg) => {
                                ApiError::BadRequest(anyhow::anyhow!("{node}: {msg}"))
                            }
                            Error::ApiError(StatusCode::INTERNAL_SERVER_ERROR, msg) => {
                                // avoid turning these into conflicts to remain compatible with
                                // pageservers, 500 errors are sadly retryable with timeline ancestor
                                // detach
                                ApiError::InternalServerError(anyhow::anyhow!("{node}: {msg}"))
                            }
                            // rest can be mapped as usual
                            other => passthrough_api_error(&node, other),
                        }
                    })
            }

            // no shard needs to go first/last; the operation should be idempotent
            let locations = targets.0.iter().map(|t| (*t.0, t.1.latest.node.clone())).collect();
            let mut results = self
                .tenant_for_shards(locations, |branch_id, node| {
                    futures::FutureExt::boxed(detach_one(
                        branch_id,
                        node,
                        self.http_client.clone(),
                        self.config.pageserver_jwt_token.clone(),
                        behavior,
                    ))
                })
                .await?;

            let any = results.pop().expect("we must have at least one response");

            let mismatching = results
                .iter()
                .filter(|res| *res != &any)
                .collect::<Vec<_>>();
            if !mismatching.is_empty() {
                // this can be hit by races which should not happen because operation lock on cplane
                let matching = results.len() - mismatching.len();
                tracing::error!(
                    matching,
                    compared_against=?any,
                    ?mismatching,
                    "shards returned different results"
                );

                return Err(ApiError::InternalServerError(anyhow::anyhow!("pageservers returned mixed results for ancestor detach; manual intervention is required.")));
            }

            Ok(any)
        }).await?
    }

    pub(crate) async fn tenant_timeline_block_unblock_gc(
        &self,
        branch_id: BranchId,
        dir: BlockUnblock,
    ) -> Result<(), ApiError> {
        let _branch_lock = trace_shared_lock(
            &self.branch_op_locks,
            branch_id,
            BranchOperations::TimelineGcBlockUnblock,
        )
        .await;

        self.tenant_remote_mutation(branch_id, move |targets| async move {
            if targets.0.is_empty() {
                return Err(ApiError::NotFound(
                    anyhow::anyhow!("Branch not found").into(),
                ));
            }

            async fn do_one(
                branch_id: BranchId,
                node: Node,
                http_client: reqwest::Client,
                jwt: Option<String>,
                dir: BlockUnblock,
            ) -> Result<(), ApiError> {
                let client = PageserverClient::new(
                    node.get_id(),
                    http_client,
                    node.base_url(),
                    jwt.as_deref(),
                );

                client
                    .branch_block_unblock_gc(branch_id, dir)
                    .await
                    .map_err(|e| passthrough_api_error(&node, e))
            }

            // no shard needs to go first/last; the operation should be idempotent
            let locations = targets
                .0
                .iter()
                .map(|t| (*t.0, t.1.latest.node.clone()))
                .collect();
            self.tenant_for_shards(locations, |branch_id, node| {
                futures::FutureExt::boxed(do_one(
                    branch_id,
                    node,
                    self.http_client.clone(),
                    self.config.pageserver_jwt_token.clone(),
                    dir,
                ))
            })
            .await
        })
        .await??;
        Ok(())
    }

    pub(crate) fn is_branch_not_found_error(body: &str, branch_id: BranchId) -> bool {
        body.contains(&format!("branch {branch_id}"))
    }

    fn process_result_and_passthrough_errors<T>(
        &self,
        branch_id: BranchId,
        results: Vec<(Node, Result<T, mgmt_api::Error>)>,
    ) -> Result<Vec<(Node, T)>, ApiError> {
        let mut processed_results: Vec<(Node, T)> = Vec::with_capacity(results.len());
        for (node, res) in results {
            match res {
                Ok(res) => processed_results.push((node, res)),
                Err(mgmt_api::Error::ApiError(StatusCode::NOT_FOUND, body))
                    if Self::is_branch_not_found_error(&body, branch_id) =>
                {
                    // If there's a branch not found, we are still in the process of attaching the branch.
                    // Return 503 so that the client can retry.
                    return Err(ApiError::ResourceUnavailable(
                        format!(
                            "Branch is not attached to the pageserver {} yet, please retry",
                            node.get_id()
                        )
                        .into(),
                    ));
                }
                Err(e) => return Err(passthrough_api_error(&node, e)),
            }
        }
        Ok(processed_results)
    }

    pub(crate) async fn tenant_timeline_lsn_lease(
        &self,
        branch_id: BranchId,
        lsn: Lsn,
    ) -> Result<LsnLease, ApiError> {
        let _branch_lock = trace_shared_lock(
            &self.branch_op_locks,
            branch_id,
            BranchOperations::TimelineLsnLease,
        )
        .await;

        self.tenant_remote_mutation(branch_id, |locations| async move {
            if locations.0.is_empty() {
                return Err(ApiError::NotFound(
                    anyhow::anyhow!("Branch not found").into(),
                ));
            }

            let results = self
                .tenant_for_shards_api(
                    locations
                        .0
                        .iter()
                        .map(|(branch_id, ShardMutationLocations { latest, .. })| {
                            (*branch_id, latest.node.clone())
                        })
                        .collect(),
                    |branch_id, client| async move {
                        client
                            .branch_lease_lsn(branch_id, lsn)
                            .await
                    },
                    1,
                    1,
                    SHORT_RECONCILE_TIMEOUT,
                    &self.cancel,
                )
                .await;

            let leases = self.process_result_and_passthrough_errors(branch_id, results)?;
            let mut valid_until = None;
            for (_, lease) in leases {
                if let Some(ref mut valid_until) = valid_until {
                    *valid_until = std::cmp::min(*valid_until, lease.valid_until);
                } else {
                    valid_until = Some(lease.valid_until);
                }
            }
            Ok(LsnLease {
                valid_until: valid_until.unwrap_or_else(SystemTime::now),
            })
        })
        .await?
    }

    /// Helper for concurrently calling a pageserver API on a number of shards, such as timeline creation.
    ///
    /// On success, the returned vector contains exactly the same number of elements as the input `locations`
    /// and returned element at index `i` is the result for `req_fn(op(locations[i])`.
    async fn tenant_for_shards<F, R>(
        &self,
        locations: Vec<(BranchId, Node)>,
        mut req_fn: F,
    ) -> Result<Vec<R>, ApiError>
    where
        F: FnMut(
            BranchId,
            Node,
        )
            -> std::pin::Pin<Box<dyn futures::Future<Output = Result<R, ApiError>> + Send>>,
    {
        let mut futs = FuturesUnordered::new();
        let mut results = Vec::with_capacity(locations.len());

        for (idx, (branch_id, node)) in locations.into_iter().enumerate() {
            let fut = req_fn(branch_id, node);
            futs.push(async move { (idx, fut.await) });
        }

        while let Some((idx, r)) = futs.next().await {
            results.push((idx, r?));
        }

        results.sort_by_key(|(idx, _)| *idx);
        Ok(results.into_iter().map(|(_, r)| r).collect())
    }

    /// Concurrently invoke a pageserver API call on many shards at once.
    ///
    /// The returned Vec has the same length as the `locations` Vec,
    /// and returned element at index `i` is the result for `op(locations[i])`.
    pub(crate) async fn tenant_for_shards_api<T, O, F>(
        &self,
        locations: Vec<(BranchId, Node)>,
        op: O,
        warn_threshold: u32,
        max_retries: u32,
        timeout: Duration,
        cancel: &CancellationToken,
    ) -> Vec<(Node, mgmt_api::Result<T>)>
    where
        O: Fn(BranchId, PageserverClient) -> F + Copy,
        F: std::future::Future<Output = mgmt_api::Result<T>>,
    {
        let mut futs = FuturesUnordered::new();
        let mut results = Vec::with_capacity(locations.len());

        for (idx, (branch_id, node)) in locations.into_iter().enumerate() {
            futs.push(async move {
                let r = node
                    .with_client_retries(
                        |client| op(branch_id, client),
                        &self.http_client,
                        &self.config.pageserver_jwt_token,
                        warn_threshold,
                        max_retries,
                        timeout,
                        cancel,
                    )
                    .await;
                (idx, node, r)
            });
        }

        while let Some((idx, node, r)) = futs.next().await {
            results.push((idx, node, r.unwrap_or(Err(mgmt_api::Error::Cancelled))));
        }

        results.sort_by_key(|(idx, _, _)| *idx);
        results.into_iter().map(|(_, node, r)| (node, r)).collect()
    }

    /// Helper for safely working with a branch remotely on pageservers, for example
    /// when creating and deleting timelines:
    /// - Makes sure the branch is attached somewhere if it weren't already
    /// - Looks up the branch and the node where it was most recently attached
    /// - Guarantees that after the inner function returns, the branch's generation hasn't moved on: this
    ///   ensures that the remote operation acted on the most recent generation, and is therefore durable.
    pub(crate) async fn tenant_remote_mutation<R, O, F>(
        &self,
        branch_id: BranchId,
        op: O,
    ) -> Result<R, ApiError>
    where
        O: FnOnce(BranchMutationLocations) -> F,
        F: std::future::Future<Output = R>,
    {
        let mutation_locations = {
            let mut locations = BranchMutationLocations::default();

            // Load the currently attached pageservers for the latest generation of the branch.  This can
            // run concurrently with reconciliations, and it is not guaranteed that the node we find here
            // will still be the latest when we're done: we will check generations again at the end of
            // this function to handle that.
            let generations = self
                .persistence
                .branch_generations(branch_id)
                .await?
                .into_iter()
                .filter(|i| i.branch_id == branch_id)
                .collect::<Vec<_>>();

            if generations
                .iter()
                .any(|i| i.generation.is_none() || i.generation_pageserver.is_none())
            {
                let branch_generations = generations
                    .into_iter()
                    .map(|i| (i.branch_id, (i.generation, i.generation_pageserver)))
                    .collect::<HashMap<_, _>>();

                // The branch has not been attached to a pageserver.  Check if this is because it's configured
                // to be detached (409: caller should give up), or because it's meant to be attached but isn't yet (503: caller should retry)
                let locked = self.inner.read().unwrap();
                let branches = locked
                    .branches
                    .range(branch_id..=branch_id)
                    .collect::<Vec<_>>();
                for (id, branch) in branches {
                    match branch.policy {
                        PlacementPolicy::Attached(_) => {
                            // This branch is meant to be attached: the caller is not wrong to try and
                            // use this function, but we can't service the request right now.
                            let Some(generation) = branch_generations.get(id) else {
                                // This can only happen if there is a split brain controller modifying the database.  This should
                                // never happen when testing, and if it happens in production we can only log the issue.
                                debug_assert!(false);
                                tracing::error!(
                                    "Branch {id} not found in generation state!  Is another rogue controller running?"
                                );
                                continue;
                            };
                            let (generation, generation_pageserver) = generation;
                            if let Some(generation) = generation {
                                if generation_pageserver.is_none() {
                                    // This is legitimate only in a very narrow window where the branch was only just configured into
                                    // Attached mode after being created in Secondary or Detached mode: generation_pageserver is set
                                    // alongside the generation by attach_hook/re-attach.
                                    tracing::warn!(
                                        "Branch {id} generation is set ({generation:?}) but generation_pageserver is None, not attached yet?"
                                    );
                                }
                            } else {
                                // This should never happen: a branch with no generation is only permitted when it was created in some state
                                // other than PlacementPolicy::Attached (and generation is always written to DB before setting Attached in memory)
                                debug_assert!(false);
                                tracing::error!(
                                    "Branch {id} generation is None, but it is in PlacementPolicy::Attached mode!"
                                );
                                continue;
                            }
                        }
                        PlacementPolicy::Secondary | PlacementPolicy::Detached => {
                            return Err(ApiError::Conflict(format!(
                                "Branch {id} has policy {:?}",
                                branch.policy
                            )));
                        }
                    }
                }

                return Err(ApiError::ResourceUnavailable(
                    "Branch is not yet attached".into(),
                ));
            }

            let locked = self.inner.read().unwrap();
            for BranchGenerationState {
                branch_id,
                generation,
                generation_pageserver,
            } in generations
            {
                let node_id = generation_pageserver.expect("We checked for None above");
                let node = locked
                    .nodes
                    .get(&node_id)
                    .ok_or(ApiError::Conflict(format!(
                        "Raced with removal of node {node_id}"
                    )))?;
                let generation = generation.expect("Checked above");

                let location = ShardMutationLocations {
                    latest: MutationLocation {
                        node: node.clone(),
                        generation,
                    },
                };
                locations.0.insert(branch_id, location);
            }

            locations
        };

        let result = op(mutation_locations.clone()).await;

        // Post-check: are all the generations of the branch the same as they were initially?  This proves that
        // our remote operation executed on the latest generation and is therefore persistent.
        {
            let latest_generations = self
                .persistence
                .branch_generations(branch_id)
                .await?
                .into_iter()
                .filter(|i| i.branch_id == branch_id)
                .collect::<Vec<_>>();

            if latest_generations
                .into_iter()
                .map(
                    |BranchGenerationState {
                         branch_id,
                         generation,
                         generation_pageserver: _,
                     }| (branch_id, generation),
                )
                .collect::<Vec<_>>()
                != mutation_locations
                    .0
                    .into_iter()
                    .map(|i| (i.0, Some(i.1.latest.generation)))
                    .collect::<Vec<_>>()
            {
                // We raced with something that incremented the generation, and therefore cannot be
                // confident that our actions are persistent (they might have hit an old generation).
                //
                // This is safe but requires a retry: ask the client to do that by giving them a 503 response.
                return Err(ApiError::ResourceUnavailable(
                    "Branch attachment changed, please retry".into(),
                ));
            }
        }

        Ok(result)
    }

    pub(crate) async fn tenant_timeline_delete(
        self: &Arc<Self>,
        branch_id: BranchId,
    ) -> Result<StatusCode, ApiError> {
        tracing::info!("Deleting branch {}", branch_id,);
        let _branch_lock = trace_shared_lock(
            &self.branch_op_locks,
            branch_id,
            BranchOperations::TimelineDelete,
        )
        .await;

        // The branch's safekeeper data is owned by the launcher and reaped
        // separately (see #171); the controller deletes only its own records
        // here (via the pageserver mutation below).
        self.tenant_remote_mutation(branch_id, move |mut targets| async move {
            if targets.0.is_empty() {
                return Err(ApiError::NotFound(
                    anyhow::anyhow!("Branch not found").into(),
                ));
            }

            let (shard_zero_bid, shard_zero_locations) = targets
                .0
                .pop_first()
                .expect("Must have at least one branch");

            async fn delete_one(
                branch_id: BranchId,
                node: Node,
                http_client: reqwest::Client,
                jwt: Option<String>,
            ) -> Result<StatusCode, ApiError> {
                tracing::info!("Deleting branch {branch_id}, attached to node {node}",);

                let client = PageserverClient::new(
                    node.get_id(),
                    http_client,
                    node.base_url(),
                    jwt.as_deref(),
                );
                let res = client.branch_delete(branch_id).await;

                match res {
                    Ok(ok) => Ok(ok),
                    Err(mgmt_api::Error::ApiError(StatusCode::CONFLICT, _)) => {
                        Ok(StatusCode::CONFLICT)
                    }
                    Err(mgmt_api::Error::ApiError(StatusCode::PRECONDITION_FAILED, msg))
                        if msg.contains("Requested branch is missing") =>
                    {
                        Err(ApiError::ResourceUnavailable(
                            "Branch migration in progress".into(),
                        ))
                    }
                    Err(mgmt_api::Error::ApiError(StatusCode::SERVICE_UNAVAILABLE, msg)) => {
                        Err(ApiError::ResourceUnavailable(msg.into()))
                    }
                    Err(e) => Err(ApiError::InternalServerError(anyhow::anyhow!(
                        "Error deleting branch {branch_id} on node {node}: {e}",
                    ))),
                }
            }

            let locations = targets
                .0
                .iter()
                .map(|t| (*t.0, t.1.latest.node.clone()))
                .collect();
            let statuses = self
                .tenant_for_shards(locations, |branch_id: BranchId, node: Node| {
                    Box::pin(delete_one(
                        branch_id,
                        node,
                        self.http_client.clone(),
                        self.config.pageserver_jwt_token.clone(),
                    ))
                })
                .await?;

            // If any extra targets haven't finished deletion yet, don't start deletion on shard zero.
            // We return 409 (Conflict) if deletion was already in progress on any of them
            // and 202 (Accepted) if deletion was not already in progress on any of them.
            if statuses.iter().any(|s| s == &StatusCode::CONFLICT) {
                return Ok(StatusCode::CONFLICT);
            }

            if statuses.iter().any(|s| s != &StatusCode::NOT_FOUND) {
                return Ok(StatusCode::ACCEPTED);
            }

            // Delete shard zero last: this is not strictly necessary, but since a caller's GET on a branch will be routed
            // to shard zero, it gives a more obvious behavior that a GET returns 404 once the deletion is done.
            let shard_zero_status = delete_one(
                shard_zero_bid,
                shard_zero_locations.latest.node,
                self.http_client.clone(),
                self.config.pageserver_jwt_token.clone(),
            )
            .await?;
            Ok(shard_zero_status)
        })
        .await?
    }
    /// When you know the BranchId, get the node holding the branch.
    ///
    /// Returns the node and branch id.
    pub(crate) async fn tenant0_node(
        &self,
        branch_id: BranchId,
    ) -> Result<(Node, BranchId), ApiError> {
        let branch_id = {
            let locked = self.inner.read().unwrap();
            let Some((branch_id, _branch)) = locked.branches.range(branch_id..=branch_id).next()
            else {
                return Err(ApiError::NotFound(
                    anyhow::anyhow!("Branch {branch_id} not found").into(),
                ));
            };

            *branch_id
        };

        self.tenant_node(branch_id)
            .await
            .map(|node| (node, branch_id))
    }

    /// When you need to send an HTTP request to the pageserver that holds a branch, this
    /// function looks up and returns node. If the branch isn't found, returns Err(ApiError::NotFound)
    ///
    /// Returns the intent node and whether it is consistent with the observed state.
    pub(crate) async fn tenant_node(&self, branch_id: BranchId) -> Result<Node, ApiError> {
        // Look up in-memory state and maybe use the node from there.
        {
            let locked = self.inner.read().unwrap();
            let Some(branch) = locked.branches.get(&branch_id) else {
                return Err(ApiError::NotFound(
                    anyhow::anyhow!("Branch {branch_id} not found").into(),
                ));
            };

            let Some(intent_node_id) = branch.intent.get_attached() else {
                tracing::warn!(
                    branch_id=%branch_id,
                    "Branch not scheduled (policy {:?}), cannot generate pass-through URL",
                    branch.policy
                );
                return Err(ApiError::Conflict(
                    "Cannot call timeline API on non-attached branch".to_string(),
                ));
            };

            // The in-memory state is authoritative: no reconciler can be in flight.
            let Some(node) = locked.nodes.get(intent_node_id) else {
                // This should never happen
                return Err(ApiError::InternalServerError(anyhow::anyhow!(
                    "Branch refers to nonexistent node"
                )));
            };
            Ok(node.clone())
        }
    }

    pub(crate) fn tenant_locate(
        &self,
        branch_id: BranchId,
    ) -> Result<BranchLocateResponse, ApiError> {
        let locked = self.inner.read().unwrap();
        tracing::info!("Locating branch {branch_id}");

        let mut result = Vec::new();

        for (branch_id, branch) in locked.branches.range(branch_id..=branch_id) {
            let node_id =
                branch
                    .intent
                    .get_attached()
                    .ok_or(ApiError::BadRequest(anyhow::anyhow!(
                        "Cannot locate a branch that is not attached"
                    )))?;

            let node = locked
                .nodes
                .get(&node_id)
                .expect("Pageservers may not be deleted while referenced");

            result.push(node.branch_location(*branch_id));
        }

        if result.is_empty() {
            return Err(ApiError::NotFound(
                anyhow::anyhow!("No locations for this branch ID found").into(),
            ));
        }
        tracing::info!(
            "Located branch {} on {}",
            branch_id,
            result
                .iter()
                .map(|s| format!("{s:?}"))
                .collect::<Vec<_>>()
                .join(",")
        );

        Ok(BranchLocateResponse { shards: result })
    }

    /// Returns None if the input iterator is empty.
    fn tenant_describe_impl<'a>(
        &self,
        branches: impl Iterator<Item = &'a Branch>,
    ) -> Option<BranchDescribeResponse> {
        let mut shard_zero = None;
        let mut describe_shards = Vec::new();

        for branch in branches {
            // yb-light: every branch has exactly one shard, which is shard zero.
            shard_zero = Some(branch);

            describe_shards.push(BranchDescribeResponseShard {
                branch_id: branch.branch_id,
                node_attached: *branch.intent.get_attached(),
                node_secondary: branch.intent.get_secondary().to_vec(),
                last_error: String::new(),
                is_reconciling: false,
                is_pending_compute_notification: false,
                is_splitting: matches!(branch.splitting, SplitState::Splitting),
                is_importing: branch.importing == TimelineImportState::Importing,
                scheduling_policy: branch.get_scheduling_policy(),
            })
        }

        let shard_zero = shard_zero?;

        Some(BranchDescribeResponse {
            branch_id: shard_zero.branch_id,
            shards: describe_shards,
            policy: shard_zero.policy.clone(),
            config: shard_zero.config.clone(),
        })
    }

    pub(crate) fn tenant_describe(
        &self,
        branch_id: BranchId,
    ) -> Result<BranchDescribeResponse, ApiError> {
        let locked = self.inner.read().unwrap();

        self.tenant_describe_impl(
            locked
                .branches
                .range(branch_id..=branch_id)
                .map(|(_k, v)| v),
        )
        .ok_or_else(|| ApiError::NotFound(anyhow::anyhow!("Branch {branch_id} not found").into()))
    }

    pub(crate) async fn tenant_timeline_describe(
        &self,
        branch_id: BranchId,
    ) -> Result<BranchTimelineDescribeResponse, ApiError> {
        self.tenant_remote_mutation(branch_id, |locations| async move {
            if locations.0.is_empty() {
                return Err(ApiError::NotFound(
                    anyhow::anyhow!("Branch not found").into(),
                ));
            };

            let locations: Vec<(BranchId, Node)> = locations
                .0
                .iter()
                .map(|t| (*t.0, t.1.latest.node.clone()))
                .collect();
            let mut futs = FuturesUnordered::new();

            for (branch_id, node) in locations {
                futs.push({
                    async move {
                        let result = node
                            .with_client_retries(
                                |client| async move { client.branch_describe(&branch_id).await },
                                &self.http_client,
                                &self.config.pageserver_jwt_token,
                                3,
                                3,
                                Duration::from_secs(30),
                                &self.cancel,
                            )
                            .await;
                        (result, branch_id, node.get_id())
                    }
                });
            }

            let mut results: Vec<TimelineInfo> = Vec::new();
            while let Some((result, branch_id, node_id)) = futs.next().await {
                match result {
                    Some(Ok(timeline_info)) => results.push(timeline_info),
                    Some(Err(e)) => {
                        tracing::warn!(
                            "Failed to describe branch {} for pageserver {}: {e}",
                            branch_id,
                            node_id,
                        );
                        return Err(ApiError::ResourceUnavailable(format!("{e}").into()));
                    }
                    None => return Err(ApiError::Cancelled),
                }
            }
            let mut image_consistent_lsn: Option<Lsn> = Some(Lsn::MAX);
            for timeline_info in &results {
                if let Some(tline_image_consistent_lsn) = timeline_info.image_consistent_lsn {
                    image_consistent_lsn = Some(std::cmp::min(
                        image_consistent_lsn.unwrap(),
                        tline_image_consistent_lsn,
                    ));
                } else {
                    tracing::warn!(
                        "Branch {} does not have image consistent lsn",
                        timeline_info.branch_id,
                    );
                    image_consistent_lsn = None;
                    break;
                }
            }

            Ok(BranchTimelineDescribeResponse {
                shards: results,
                image_consistent_lsn,
            })
        })
        .await?
    }

    /// Set (or clear) the human-readable branch name attached to a branch.
    pub(crate) async fn set_timeline_branch_name(
        &self,
        branch_id: BranchId,
        branch_name: Option<String>,
    ) -> Result<(), ApiError> {
        let existed = self
            .persistence
            .set_timeline_branch_name(branch_id, branch_name)
            .await?;
        if !existed {
            return Err(ApiError::NotFound(
                anyhow::anyhow!("Branch {branch_id} not found").into(),
            ));
        }
        Ok(())
    }

    /// List every branch that has a branch_name set.
    pub(crate) async fn list_timeline_branch_names(
        &self,
    ) -> Result<Vec<TimelineBranchName>, ApiError> {
        let rows = self.persistence.list_timeline_branch_names().await?;
        let mut out = Vec::with_capacity(rows.len());
        for (branch_id, branch_name) in rows {
            out.push(TimelineBranchName {
                branch_id: branch_id
                    .parse()
                    .map_err(|e| ApiError::InternalServerError(anyhow::anyhow!("{e}")))?,
                branch_name,
            });
        }
        Ok(out)
    }

    /// Record (or overwrite) a pending timeline init recipe. `branch create`
    /// allocates a branch's id + branch name before its pageserver +
    /// safekeeper exist; the recipe says how `endpoint create` should later
    /// bring the branch up. Idempotent on the branch key.
    pub(crate) async fn record_pending_timeline_init(
        &self,
        branch_id: BranchId,
        req: PendingTimelineInitRequest,
    ) -> Result<(), ApiError> {
        let entry = PendingTimelineInitPersistence {
            branch_id: branch_id.to_string(),
            branch_name: req.branch_name,
            pg_version: req.pg_version.major_version_num() as i32,
            ancestor_branch_id: req.ancestor_branch_id.map(|id| id.to_string()),
            start_lsn: req.start_lsn.into(),
        };
        self.persistence.insert_pending_timeline_init(entry).await?;
        Ok(())
    }

    /// List the pending timeline inits recorded for a branch. `endpoint
    /// create` uses this both to resolve a branch name to its (not-yet-
    /// materialized) branch and to read the recipe for materializing it.
    pub(crate) async fn list_pending_timeline_inits(
        &self,
    ) -> Result<Vec<PendingTimelineInit>, ApiError> {
        let rows = self.persistence.list_all_pending_timeline_inits().await?;
        let mut out = Vec::with_capacity(rows.len());
        for r in rows {
            let pg_version = PgMajorVersion::ALL
                .iter()
                .copied()
                .find(|v| v.major_version_num() as i32 == r.pg_version)
                .ok_or_else(|| {
                    ApiError::InternalServerError(anyhow::anyhow!(
                        "unsupported pg_version {} in pending_timeline_inits",
                        r.pg_version
                    ))
                })?;
            out.push(PendingTimelineInit {
                branch_id: r
                    .branch_id
                    .parse()
                    .map_err(|e| ApiError::InternalServerError(anyhow::anyhow!("{e}")))?,
                branch_name: r.branch_name,
                pg_version,
                ancestor_branch_id: r
                    .ancestor_branch_id
                    .map(|s| s.parse())
                    .transpose()
                    .map_err(|e| ApiError::InternalServerError(anyhow::anyhow!("{e}")))?,
                start_lsn: r.start_lsn.0,
            });
        }
        Ok(out)
    }

    /// Delete a pending timeline init once its pageserver + safekeeper have
    /// been materialized. No-op if the row is already gone.
    pub(crate) async fn delete_pending_timeline_init(
        &self,
        branch_id: BranchId,
    ) -> Result<(), ApiError> {
        self.persistence
            .delete_pending_timeline_init(branch_id)
            .await?;
        Ok(())
    }

    /// limit & offset are pagination parameters. Since we are walking an in-memory HashMap, `offset` does not
    /// avoid traversing data, it just avoid returning it. This is suitable for our purposes, since our in memory
    /// maps are small enough to traverse fast, our pagination is just to avoid serializing huge JSON responses
    /// in our external API.
    pub(crate) fn tenant_list(
        &self,
        limit: Option<usize>,
        start_after: Option<BranchId>,
    ) -> Vec<BranchDescribeResponse> {
        let locked = self.inner.read().unwrap();

        // Apply start_from parameter
        let branch_range = match start_after {
            None => locked.branches.range(..),
            Some(branch_id) => locked.branches.range((
                std::ops::Bound::Excluded(branch_id),
                std::ops::Bound::Unbounded,
            )),
        };

        let mut result = Vec::new();
        for (_branch_id, branches) in &branch_range.group_by(|(id, _branch)| **id) {
            result.push(
                self.tenant_describe_impl(branches.map(|(_k, v)| v))
                    .expect("Groups are always non-empty"),
            );

            // Enforce `limit` parameter
            if let Some(limit) = limit {
                if result.len() >= limit {
                    break;
                }
            }
        }

        result
    }

    /// This is for debug/support only: we simply drop all state for a branch, without
    /// detaching or deleting it on pageservers.
    pub(crate) async fn tenant_drop(&self, branch_id: BranchId) -> Result<(), ApiError> {
        self.persistence.delete_branch(branch_id).await?;

        let mut locked = self.inner.write().unwrap();
        let (_nodes, branches, scheduler) = locked.parts_mut();
        let mut ids = Vec::new();
        for (id, _) in branches.range(branch_id..=branch_id) {
            ids.push(*id);
        }

        for id in ids {
            if let Some(mut branch) = branches.remove(&id) {
                branch.intent.clear(scheduler);
            }
        }

        Ok(())
    }

    /// This is for debug/support only: assuming branch data is already present in S3, we "create" a
    /// branch with a very high generation number so that it will see the existing data.
    /// It does not create timelines on safekeepers, because they might already exist on some
    /// safekeeper set. So, the timelines are not storcon-managed after the import.
    pub(crate) async fn tenant_import(
        &self,
        branch_id: BranchId,
    ) -> Result<BranchCreateResponse, ApiError> {
        // Pick an arbitrary available pageserver to use for scanning the branch in remote storage
        let maybe_node = {
            self.inner
                .read()
                .unwrap()
                .nodes
                .values()
                .find(|n| n.is_available())
                .cloned()
        };
        let Some(node) = maybe_node else {
            return Err(ApiError::BadRequest(anyhow::anyhow!("No nodes available")));
        };

        let client = PageserverClient::new(
            node.get_id(),
            self.http_client.clone(),
            node.base_url(),
            self.config.pageserver_jwt_token.as_deref(),
        );

        let scan_result = client
            .branch_scan_remote_storage(branch_id)
            .await
            .map_err(|e| passthrough_api_error(&node, e))?;

        if scan_result.shards.is_empty() {
            return Err(ApiError::NotFound(
                anyhow::anyhow!("No shards found").into(),
            ));
        }

        // Ideally we would set each newly imported shard's generation independently, but for correctness it is sufficient
        // to
        let generation = scan_result
            .shards
            .iter()
            .map(|s| s.generation)
            .max()
            .expect("We already validated >0 shards");

        let response = self
            .do_tenant_create(BranchCreateRequest {
                new_branch_id: branch_id,
                generation,

                placement_policy: Some(PlacementPolicy::Attached(0)), // No secondaries, for convenient debug/hacking
                config: BranchConfig::default(),
            })
            .await?;

        Ok(response)
    }

    /// For debug/support: a full JSON dump of Branches.  Returns a response so that
    /// we don't have to make Branch clonable in the return path.
    pub(crate) fn tenants_dump(&self) -> Result<hyper::Response<hyper::Body>, ApiError> {
        let serialized = {
            let locked = self.inner.read().unwrap();
            let result = locked.branches.values().collect::<Vec<_>>();
            serde_json::to_string(&result).map_err(|e| ApiError::InternalServerError(e.into()))?
        };

        hyper::Response::builder()
            .status(hyper::StatusCode::OK)
            .header(hyper::header::CONTENT_TYPE, "application/json")
            .body(hyper::Body::from(serialized))
            .map_err(|e| ApiError::InternalServerError(e.into()))
    }

    /// Check the consistency of in-memory state vs. persistent state, and check that the
    /// scheduler's statistics are up to date.
    ///
    /// These consistency checks expect an **idle** system.  If changes are going on while
    /// we run, then we can falsely indicate a consistency issue.  This is sufficient for end-of-test
    /// checks, but not suitable for running continuously in the background in the field.
    pub(crate) async fn consistency_check(&self) -> Result<(), ApiError> {
        let (mut expect_nodes, mut expect_branches) = {
            let locked = self.inner.read().unwrap();

            locked
                .scheduler
                .consistency_check(locked.nodes.values(), locked.branches.values())
                .context("Scheduler checks")
                .map_err(ApiError::InternalServerError)?;

            let expect_nodes = locked
                .nodes
                .values()
                .map(|n| n.to_persistent())
                .collect::<Vec<_>>();

            let expect_branches = locked
                .branches
                .values()
                .map(|t| t.to_persistent())
                .collect::<Vec<_>>();

            (expect_nodes, expect_branches)
        };

        let mut nodes = self.persistence.list_nodes().await?;
        expect_nodes.sort_by_key(|n| n.node_id);
        nodes.sort_by_key(|n| n.node_id);

        // Errors relating to nodes are deferred so that we don't skip the branch checks below if we have a node error
        let node_result = if nodes != expect_nodes {
            tracing::error!("Consistency check failed on nodes.");
            tracing::error!(
                "Nodes in memory: {}",
                serde_json::to_string(&expect_nodes)
                    .map_err(|e| ApiError::InternalServerError(e.into()))?
            );
            tracing::error!(
                "Nodes in database: {}",
                serde_json::to_string(&nodes)
                    .map_err(|e| ApiError::InternalServerError(e.into()))?
            );
            Err(ApiError::InternalServerError(anyhow::anyhow!(
                "Node consistency failure"
            )))
        } else {
            Ok(())
        };

        let mut persistent_branches = self.persistence.load_active_branches().await?;
        persistent_branches.sort_by_key(|tsp| tsp.branch_id.clone());

        expect_branches.sort_by_key(|tsp| tsp.branch_id.clone());

        // Because JSON contents of persistent branches might disagree with the fields in current `BranchConfig`
        // definition, we will do an encode/decode cycle to ensure any legacy fields are dropped and any new
        // fields are added, before doing a comparison.
        for tsp in &mut persistent_branches {
            let config: BranchConfig = serde_json::from_value(tsp.config.clone())
                .map_err(|e| ApiError::InternalServerError(e.into()))?;
            tsp.config = serde_json::to_value(&config).expect("Encoding config is infallible");
        }

        if persistent_branches != expect_branches {
            tracing::error!("Consistency check failed on branches.");

            tracing::error!(
                "Branches in memory: {}",
                serde_json::to_string(&expect_branches)
                    .map_err(|e| ApiError::InternalServerError(e.into()))?
            );
            tracing::error!(
                "Branches in database: {}",
                serde_json::to_string(&persistent_branches)
                    .map_err(|e| ApiError::InternalServerError(e.into()))?
            );

            // The total dump log lines above are useful in testing but in the field grafana will
            // usually just drop them because they're so large. So we also do some explicit logging
            // of just the diffs.
            let persistent_branches = persistent_branches
                .into_iter()
                .map(|tsp| (tsp.get_branch_id().unwrap(), tsp))
                .collect::<HashMap<_, _>>();
            let expect_branches = expect_branches
                .into_iter()
                .map(|tsp| (tsp.get_branch_id().unwrap(), tsp))
                .collect::<HashMap<_, _>>();
            for (branch_id, persistent_tsp) in &persistent_branches {
                match expect_branches.get(branch_id) {
                    None => {
                        tracing::error!("Branch {} found in database but not in memory", branch_id);
                    }
                    Some(expect_tsp) => {
                        if expect_tsp != persistent_tsp {
                            tracing::error!(
                                "Branch {} is inconsistent.  In memory: {}, database has: {}",
                                branch_id,
                                serde_json::to_string(expect_tsp).unwrap(),
                                serde_json::to_string(&persistent_tsp).unwrap()
                            );
                        }
                    }
                }
            }

            // Having already logged any differences, log any branches that simply aren't present in the database
            for (branch_id, memory_tsp) in &expect_branches {
                if !persistent_branches.contains_key(branch_id) {
                    tracing::error!(
                        "Branch {} found in memory but not in database: {}",
                        branch_id,
                        serde_json::to_string(memory_tsp)
                            .map_err(|e| ApiError::InternalServerError(e.into()))?
                    );
                }
            }

            return Err(ApiError::InternalServerError(anyhow::anyhow!(
                "Branch consistency failure"
            )));
        }

        node_result
    }

    /// For debug/support: a JSON dump of the [`Scheduler`].  Returns a response so that
    /// we don't have to make Branch clonable in the return path.
    pub(crate) fn scheduler_dump(&self) -> Result<hyper::Response<hyper::Body>, ApiError> {
        let serialized = {
            let locked = self.inner.read().unwrap();
            serde_json::to_string(&locked.scheduler)
                .map_err(|e| ApiError::InternalServerError(e.into()))?
        };

        hyper::Response::builder()
            .status(hyper::StatusCode::OK)
            .header(hyper::header::CONTENT_TYPE, "application/json")
            .body(hyper::Body::from(serialized))
            .map_err(|e| ApiError::InternalServerError(e.into()))
    }

    /// This is for debug/support only: we simply drop the node, without
    /// detaching or deleting branches on pageservers.  We do not try and re-schedule any
    /// branches that were on this node.
    pub(crate) async fn node_drop(&self, node_id: NodeId) -> Result<(), ApiError> {
        self.persistence.set_tombstone(node_id).await?;

        let mut locked = self.inner.write().unwrap();

        for branch in locked.branches.values_mut() {
            branch.deref_node(node_id);
            branch.observed.locations.remove(&node_id);
        }

        let mut nodes = (*locked.nodes).clone();
        nodes.remove(&node_id);
        locked.nodes = Arc::new(nodes);
        metrics::METRICS_REGISTRY
            .metrics_group
            .storage_controller_pageserver_nodes
            .set(locked.nodes.len() as i64);
        metrics::METRICS_REGISTRY
            .metrics_group
            .storage_controller_https_pageserver_nodes
            .set(locked.nodes.values().filter(|n| n.has_https_port()).count() as i64);

        locked.scheduler.node_remove(node_id);

        Ok(())
    }

    pub(crate) async fn node_list(&self) -> Result<Vec<Node>, ApiError> {
        let nodes = {
            self.inner
                .read()
                .unwrap()
                .nodes
                .values()
                .cloned()
                .collect::<Vec<_>>()
        };

        Ok(nodes)
    }

    /// Persist (create or replace) a compute endpoint record on behalf of
    /// `yb_light_local`. The controller only stores the record; it does not
    /// interpret the endpoint `spec`.
    pub(crate) async fn endpoint_create(&self, req: ComputeEndpoint) -> Result<(), ApiError> {
        self.persistence.insert_endpoint(req.into()).await?;
        Ok(())
    }

    /// Return all persisted compute endpoint records.
    pub(crate) async fn endpoint_list(&self) -> Result<Vec<ComputeEndpoint>, ApiError> {
        Ok(self
            .persistence
            .list_endpoints()
            .await?
            .into_iter()
            .map(ComputeEndpoint::from)
            .collect())
    }

    /// Delete a persisted compute endpoint record. Idempotent.
    pub(crate) async fn endpoint_delete(&self, endpoint_id: &str) -> Result<(), ApiError> {
        self.persistence.delete_endpoint(endpoint_id).await?;
        Ok(())
    }

    /// Drop a branch: delete every compute endpoint record for the branch,
    /// discard any un-materialized init recipe, and mark the branch as
    /// dropped (sets `deleted_at`) so it no longer surfaces as a live branch
    /// and is not brought back on restart. The actual pageserver/safekeeper
    /// data + the `timelines` row itself are left for a later GC pass. Compute
    /// processes are stopped by `yb_light_local` before it calls this; the
    /// durable cleanup here is idempotent and order-independent.
    pub(crate) async fn timeline_drop(&self, branch_id: BranchId) -> Result<(), ApiError> {
        self.persistence
            .delete_endpoints_for_branch(&branch_id.to_string())
            .await?;
        self.persistence
            .delete_pending_timeline_init(branch_id)
            .await?;
        self.persistence.timeline_set_deleted_at(branch_id).await?;
        Ok(())
    }

    pub(crate) async fn tombstone_list(&self) -> Result<Vec<Node>, ApiError> {
        self.persistence
            .list_tombstones()
            .await?
            .into_iter()
            .map(|np| Node::from_persistent(np, false))
            .collect::<Result<Vec<_>, _>>()
            .map_err(ApiError::InternalServerError)
    }

    pub(crate) async fn tombstone_delete(&self, node_id: NodeId) -> Result<(), ApiError> {
        let _node_lock = trace_exclusive_lock(
            &self.node_op_locks,
            node_id,
            NodeOperations::DeleteTombstone,
        )
        .await;

        if matches!(self.get_node(node_id).await, Err(ApiError::NotFound(_))) {
            self.persistence.delete_node(node_id).await?;
            Ok(())
        } else {
            Err(ApiError::Conflict(format!(
                "Node {node_id} is in use, consider using tombstone API first"
            )))
        }
    }

    pub(crate) async fn get_node(&self, node_id: NodeId) -> Result<Node, ApiError> {
        self.inner
            .read()
            .unwrap()
            .nodes
            .get(&node_id)
            .cloned()
            .ok_or(ApiError::NotFound(
                format!("Node {node_id} not registered").into(),
            ))
    }

    pub(crate) async fn get_node_shards(
        &self,
        node_id: NodeId,
    ) -> Result<NodeShardResponse, ApiError> {
        let locked = self.inner.read().unwrap();
        let mut shards = Vec::new();
        for (bid, branch) in locked.branches.iter() {
            let is_intended_secondary = match (
                branch.intent.get_attached() == &Some(node_id),
                branch.intent.get_secondary().contains(&node_id),
            ) {
                (true, true) => {
                    return Err(ApiError::InternalServerError(anyhow::anyhow!(
                        "{} attached as primary+secondary on the same node",
                        bid
                    )));
                }
                (true, false) => Some(false),
                (false, true) => Some(true),
                (false, false) => None,
            };
            // yb-light has no Secondary location concept; the observed
            // side is always either attached (`generation: Some`) or
            // absent. Always emit `Some(false)` when we have an
            // observation so the response shape stays stable.
            let is_observed_secondary = branch
                .observed
                .locations
                .get(&node_id)
                .and_then(|loc| loc.conf.as_ref())
                .map(|_| false);
            if is_intended_secondary.is_some() || is_observed_secondary.is_some() {
                shards.push(NodeShard {
                    branch_id: *bid,
                    is_intended_secondary,
                    is_observed_secondary,
                });
            }
        }
        Ok(NodeShardResponse { node_id, shards })
    }

    pub(crate) async fn node_register(
        &self,
        register_req: NodeRegisterRequest,
    ) -> Result<(), ApiError> {
        let _node_lock = trace_exclusive_lock(
            &self.node_op_locks,
            register_req.node_id,
            NodeOperations::Register,
        )
        .await;

        #[derive(PartialEq)]
        enum RegistrationStatus {
            UpToDate,
            NeedUpdate,
            Mismatched,
            New,
        }

        let registration_status = {
            let locked = self.inner.read().unwrap();
            if let Some(node) = locked.nodes.get(&register_req.node_id) {
                if node.registration_match(&register_req) {
                    if node.need_update(&register_req) {
                        RegistrationStatus::NeedUpdate
                    } else {
                        RegistrationStatus::UpToDate
                    }
                } else {
                    RegistrationStatus::Mismatched
                }
            } else {
                RegistrationStatus::New
            }
        };

        match registration_status {
            RegistrationStatus::UpToDate => {
                tracing::info!(
                    "Node {} re-registered with matching address and is up to date",
                    register_req.node_id
                );

                return Ok(());
            }
            RegistrationStatus::Mismatched => {
                // TODO: decide if we want to allow modifying node addresses without removing and re-adding
                // the node.  Safest/simplest thing is to refuse it, and usually we deploy with
                // a fixed address through the lifetime of a node.
                tracing::warn!(
                    "Node {} tried to register with different address",
                    register_req.node_id
                );
                return Err(ApiError::Conflict(
                    "Node is already registered with different address".to_string(),
                ));
            }
            RegistrationStatus::New | RegistrationStatus::NeedUpdate => {
                // fallthrough
            }
        }

        // We do not require that a node is actually online when registered (it will start life
        // with it's  availability set to Offline), but we _do_ require that its DNS record exists. We're
        // therefore not immune to asymmetric L3 connectivity issues, but we are protected against nodes
        // that register themselves with a broken DNS config.  We check only the HTTP hostname, because
        // the postgres hostname might only be resolvable to clients (e.g. if we're on a different VPC than clients).
        if tokio::net::lookup_host(format!(
            "{}:{}",
            register_req.listen_http_addr, register_req.listen_http_port
        ))
        .await
        .is_err()
        {
            // If we have a transient DNS issue, it's up to the caller to retry their registration.  Because
            // we can't robustly distinguish between an intermittent issue and a totally bogus DNS situation,
            // we return a soft 503 error, to encourage callers to retry past transient issues.
            return Err(ApiError::ResourceUnavailable(
                format!(
                    "Node {} tried to register with unknown DNS name '{}'",
                    register_req.node_id, register_req.listen_http_addr
                )
                .into(),
            ));
        }

        if self.config.use_https_pageserver_api && register_req.listen_https_port.is_none() {
            return Err(ApiError::PreconditionFailed(
                format!(
                    "Node {} has no https port, but use_https is enabled",
                    register_req.node_id
                )
                .into(),
            ));
        }

        if register_req.listen_grpc_addr.is_some() != register_req.listen_grpc_port.is_some() {
            return Err(ApiError::BadRequest(anyhow::anyhow!(
                "must specify both gRPC address and port"
            )));
        }

        // Ordering: we must persist the new node _before_ adding it to in-memory state.
        // This ensures that before we use it for anything or expose it via any external
        // API, it is guaranteed to be available after a restart.
        let new_node = Node::new(
            register_req.node_id,
            register_req.listen_http_addr,
            register_req.listen_http_port,
            register_req.listen_https_port,
            register_req.listen_pg_addr,
            register_req.listen_pg_port,
            register_req.listen_grpc_addr.clone(),
            register_req.listen_grpc_port,
            self.config.use_https_pageserver_api,
        );
        let new_node = match new_node {
            Ok(new_node) => new_node,
            Err(error) => return Err(ApiError::InternalServerError(error)),
        };

        match registration_status {
            RegistrationStatus::New => {
                self.persistence.insert_node(&new_node).await.map_err(|e| {
                    if matches!(
                        e,
                        crate::persistence::DatabaseError::Query(
                            diesel::result::Error::DatabaseError(
                                diesel::result::DatabaseErrorKind::UniqueViolation,
                                _,
                            )
                        )
                    ) {
                        // The node can be deleted by tombstone API, and not show up in the list of nodes.
                        // If you see this error, check tombstones first.
                        ApiError::Conflict(format!("Node {} is already exists", new_node.get_id()))
                    } else {
                        ApiError::from(e)
                    }
                })?;
            }
            RegistrationStatus::NeedUpdate => {
                self.persistence
                    .update_node_on_registration(
                        register_req.node_id,
                        register_req.listen_https_port,
                        register_req.listen_grpc_addr,
                        register_req.listen_grpc_port,
                    )
                    .await?
            }
            _ => unreachable!("Other statuses have been processed earlier"),
        }

        let mut locked = self.inner.write().unwrap();
        let mut new_nodes = (*locked.nodes).clone();

        locked.scheduler.node_upsert(&new_node);
        new_nodes.insert(register_req.node_id, new_node);

        locked.nodes = Arc::new(new_nodes);

        metrics::METRICS_REGISTRY
            .metrics_group
            .storage_controller_pageserver_nodes
            .set(locked.nodes.len() as i64);
        metrics::METRICS_REGISTRY
            .metrics_group
            .storage_controller_https_pageserver_nodes
            .set(locked.nodes.values().filter(|n| n.has_https_port()).count() as i64);

        match registration_status {
            RegistrationStatus::New => {
                tracing::info!(
                    "Registered pageserver {}, now have {} pageservers",
                    register_req.node_id,
                    locked.nodes.len()
                );
            }
            RegistrationStatus::NeedUpdate => {
                tracing::info!("Re-registered and updated node {}", register_req.node_id,);
            }
            _ => unreachable!("Other statuses have been processed earlier"),
        }
        Ok(())
    }

    /// Configure in-memory and persistent state of a node as requested
    ///
    /// Note that this function does not trigger any immediate side effects in response
    /// to the changes. That part is handled by [`Self::handle_node_availability_transition`].
    async fn node_state_configure(
        &self,
        node_id: NodeId,
        availability: Option<NodeAvailability>,
        _node_lock: &TracingExclusiveGuard<NodeOperations>,
    ) -> Result<AvailabilityTransition, ApiError> {
        // The transition we calculate here remains valid later in the function because we hold the op lock on the node:
        // nothing else can mutate its availability while we run.
        let availability_transition = if let Some(input_availability) = availability.as_ref() {
            let locked = self.inner.read().unwrap();
            let Some(node) = locked.nodes.get(&node_id) else {
                return Err(ApiError::NotFound(
                    anyhow::anyhow!("Node {} not registered", node_id).into(),
                ));
            };

            node.get_availability_transition(input_availability)
        } else {
            AvailabilityTransition::Unchanged
        };

        // Apply changes from the request to our in-memory state for the Node
        let mut locked = self.inner.write().unwrap();
        let (nodes, _branches, scheduler) = locked.parts_mut();

        let mut new_nodes = (**nodes).clone();

        let Some(node) = new_nodes.get_mut(&node_id) else {
            return Err(ApiError::NotFound(
                anyhow::anyhow!("Node not registered").into(),
            ));
        };

        if let Some(availability) = availability {
            node.set_availability(availability);
        }

        // Update the scheduler, in case the elegibility of the node for new shards has changed
        scheduler.node_upsert(node);

        let new_nodes = Arc::new(new_nodes);
        locked.nodes = new_nodes;

        Ok(availability_transition)
    }

    /// Handle availability transition of one node
    ///
    /// Note that you should first call [`Self::node_state_configure`] to update
    /// the in-memory state referencing that node. If you need to handle more than one transition
    /// consider using [`Self::handle_node_availability_transitions`].
    async fn handle_node_availability_transition(
        &self,
        node_id: NodeId,
        transition: AvailabilityTransition,
        _node_lock: &TracingExclusiveGuard<NodeOperations>,
    ) -> Result<(), ApiError> {
        // Keep observed state bookkeeping consistent with the node's availability state.
        match transition {
            AvailabilityTransition::ToOffline => {
                tracing::info!("Node {} transition to offline", node_id);

                let mut locked = self.inner.write().unwrap();
                let (_nodes, branches, _scheduler) = locked.parts_mut();

                for branch in branches.values_mut() {
                    if let Some(observed_loc) = branch.observed.locations.get_mut(&node_id) {
                        // When a node goes offline, we set its observed configuration to None, indicating unknown: we will
                        // not assume our knowledge of the node's configuration is accurate until it comes back online
                        observed_loc.conf = None;
                    }
                }
            }
            AvailabilityTransition::ToActive => {
                tracing::info!("Node {} transition to active", node_id);
            }
            // No action required for the intermediate unavailable state.
            // When we transition into active or offline from the unavailable state,
            // the correct handling above will kick in.
            AvailabilityTransition::ToWarmingUpFromActive => {
                tracing::info!("Node {} transition to unavailable from active", node_id);
            }
            AvailabilityTransition::ToWarmingUpFromOffline => {
                tracing::info!("Node {} transition to unavailable from offline", node_id);
            }
            AvailabilityTransition::Unchanged => {
                tracing::debug!("Node {} no availability change during config", node_id);
            }
        }

        Ok(())
    }

    /// Handle availability transition for multiple nodes
    ///
    /// Note that you should first call [`Self::node_state_configure`] for
    /// all nodes being handled here for the handling to use fresh in-memory state.
    async fn handle_node_availability_transitions(
        &self,
        transitions: Vec<(
            NodeId,
            TracingExclusiveGuard<NodeOperations>,
            AvailabilityTransition,
        )>,
    ) -> Result<(), Vec<(NodeId, ApiError)>> {
        let mut errors = Vec::default();
        for (node_id, node_lock, transition) in transitions {
            let res = self
                .handle_node_availability_transition(node_id, transition, &node_lock)
                .await;
            if let Err(err) = res {
                errors.push((node_id, err));
            }
        }

        if errors.is_empty() {
            Ok(())
        } else {
            Err(errors)
        }
    }

    pub(crate) async fn node_configure(
        &self,
        node_id: NodeId,
        availability: Option<NodeAvailability>,
    ) -> Result<(), ApiError> {
        let node_lock =
            trace_exclusive_lock(&self.node_op_locks, node_id, NodeOperations::Configure).await;

        let transition = self
            .node_state_configure(node_id, availability, &node_lock)
            .await?;
        self.handle_node_availability_transition(node_id, transition, &node_lock)
            .await
    }

    /// Wrapper around [`Self::node_configure`] for the external HTTP api.
    pub(crate) async fn external_node_configure(
        &self,
        node_id: NodeId,
        availability: Option<NodeAvailability>,
    ) -> Result<(), ApiError> {
        self.node_configure(node_id, availability).await
    }

    // yb-light: automatic shard-splitting is gone (branches are
    // single-shard). The upstream `compute_split_shards` /
    // `tenant_shard_split` orchestration has been removed.

    pub async fn shutdown(&self) {
        // Background tasks hold gate guards: this notifies them of the cancellation and
        // waits for them all to complete.
        tracing::info!("Shutting down: cancelling and waiting for background tasks to exit");
        self.cancel.cancel();
        self.gate.close().await;
    }

    /// Updates scrubber metadata health check results.
    pub(crate) async fn metadata_health_update(
        &self,
        update_req: MetadataHealthUpdateRequest,
    ) -> Result<(), ApiError> {
        let now = chrono::offset::Utc::now();
        let (healthy_records, unhealthy_records) = {
            let locked = self.inner.read().unwrap();
            let healthy_records = update_req
                .healthy_branches
                .into_iter()
                // Retain only health records associated with branches managed by storage controller.
                .filter(|branch_id| locked.branches.contains_key(branch_id))
                .map(|branch_id| MetadataHealthPersistence::new(branch_id, true, now))
                .collect();
            let unhealthy_records = update_req
                .unhealthy_branches
                .into_iter()
                .filter(|branch_id| locked.branches.contains_key(branch_id))
                .map(|branch_id| MetadataHealthPersistence::new(branch_id, false, now))
                .collect();

            (healthy_records, unhealthy_records)
        };

        self.persistence
            .update_metadata_health_records(healthy_records, unhealthy_records, now)
            .await?;
        Ok(())
    }

    /// Lists the branches that have unhealthy metadata status.
    pub(crate) async fn metadata_health_list_unhealthy(&self) -> Result<Vec<BranchId>, ApiError> {
        let result = self
            .persistence
            .list_unhealthy_metadata_health_records()
            .await?
            .iter()
            .map(|p| p.get_branch_id().unwrap())
            .collect();

        Ok(result)
    }

    /// Lists the branches that have not been scrubbed for some duration.
    pub(crate) async fn metadata_health_list_outdated(
        &self,
        not_scrubbed_for: Duration,
    ) -> Result<Vec<MetadataHealthRecord>, ApiError> {
        let earlier = chrono::offset::Utc::now() - not_scrubbed_for;
        let result = self
            .persistence
            .list_outdated_metadata_health_records(earlier)
            .await?
            .into_iter()
            .map(|record| record.into())
            .collect();
        Ok(result)
    }
}
