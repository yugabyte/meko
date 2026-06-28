use std::collections::HashSet;
use std::str::FromStr;
use std::sync::Arc;
use std::time::Duration;

use crate::heartbeater::SafekeeperState;
use crate::metrics;
use crate::persistence::{DatabaseError, TimelinePersistence, TimelineUpdate};
use crate::safekeeper::Safekeeper;
use crate::safekeeper_client::SafekeeperClient;
use crate::timeline_import::TimelineImportFinalizeError;
use anyhow::Context;
use http_utils::error::ApiError;
use pageserver_api::controller_api::{
    SafekeeperDescribeResponse, SkSchedulingPolicy, TimelineImportRequest,
};
use pageserver_api::models::{SafekeeperInfo, SafekeepersInfo, TimelineInfo};
use safekeeper_api::PgVersionId;
use safekeeper_api::membership::{MemberSet, SafekeeperGeneration};
use safekeeper_api::models::TimelineLocateResponse;
use safekeeper_client::mgmt_api;
use tokio::task::JoinSet;
use tokio_util::sync::CancellationToken;
use utils::id::{BranchId, NodeId};
use utils::logging::SecretString;

use super::Service;

impl Service {
    fn make_member_set(safekeepers: &[Safekeeper]) -> Result<MemberSet, anyhow::Error> {
        let members = safekeepers
            .iter()
            .map(|sk| sk.get_safekeeper_id())
            .collect::<Vec<_>>();

        MemberSet::new(members)
    }

    fn get_safekeepers(&self, ids: &[i64]) -> Result<Vec<Safekeeper>, ApiError> {
        let safekeepers = {
            let locked = self.inner.read().unwrap();
            locked.safekeepers.clone()
        };

        ids.iter()
            .map(|&id| {
                let node_id = NodeId(id as u64);
                safekeepers.get(&node_id).cloned().ok_or_else(|| {
                    ApiError::InternalServerError(anyhow::anyhow!(
                        "safekeeper {node_id} is not registered"
                    ))
                })
            })
            .collect::<Result<Vec<_>, _>>()
    }

    /// Timeline creation on safekeepers
    ///
    /// Returns `Ok(left)` if the timeline has been created on a quorum of safekeepers,
    /// where `left` contains the list of safekeepers that didn't have a successful response.
    /// Assumes branch lock is held while calling this function.
    pub(super) async fn tenant_timeline_create_safekeepers_quorum(
        &self,
        branch_id: BranchId,
        pg_version: PgVersionId,
        timeline_persistence: &TimelinePersistence,
    ) -> Result<Vec<NodeId>, ApiError> {
        let safekeepers = self.get_safekeepers(&timeline_persistence.sk_set)?;

        let mset = Self::make_member_set(&safekeepers).map_err(ApiError::InternalServerError)?;
        let mconf = safekeeper_api::membership::Configuration::new(mset);

        let req = safekeeper_api::models::TimelineCreateRequest {
            commit_lsn: None,
            mconf,
            pg_version,
            start_lsn: timeline_persistence.start_lsn.0,
            system_id: None,
            branch_id,
            wal_seg_size: None,
        };

        const SK_CREATE_TIMELINE_RECONCILE_TIMEOUT: Duration = Duration::from_secs(30);

        let results = self
            .tenant_timeline_safekeeper_op_quorum(
                &safekeepers,
                move |client| {
                    let req = req.clone();
                    async move { client.create_timeline(&req).await }
                },
                SK_CREATE_TIMELINE_RECONCILE_TIMEOUT,
            )
            .await?;

        Ok(results
            .into_iter()
            .enumerate()
            .filter_map(|(idx, res)| {
                if res.is_ok() {
                    None // Success, don't return this safekeeper
                } else {
                    Some(safekeepers[idx].get_id()) // Failure, return this safekeeper
                }
            })
            .collect::<Vec<_>>())
    }

    /// Perform an operation on a list of safekeepers in parallel with retries.
    ///
    /// Return the results of the operation on each safekeeper in the input order.
    async fn tenant_timeline_safekeeper_op<T, O, F>(
        &self,
        safekeepers: &[Safekeeper],
        op: O,
        timeout: Duration,
    ) -> Result<Vec<mgmt_api::Result<T>>, ApiError>
    where
        O: FnMut(SafekeeperClient) -> F + Send + 'static,
        O: Clone,
        F: std::future::Future<Output = mgmt_api::Result<T>> + Send + 'static,
        T: Sync + Send + 'static,
    {
        let jwt = self
            .config
            .safekeeper_jwt_token
            .clone()
            .map(SecretString::from);
        let mut joinset = JoinSet::new();

        for (idx, sk) in safekeepers.iter().enumerate() {
            let sk = sk.clone();
            let http_client = self.http_client.clone();
            let jwt = jwt.clone();
            let op = op.clone();
            joinset.spawn(async move {
                let res = sk
                    .with_client_retries(
                        op,
                        &http_client,
                        &jwt,
                        3,
                        3,
                        // TODO(diko): This is a wrong timeout.
                        // It should be scaled to the retry count.
                        timeout,
                        &CancellationToken::new(),
                    )
                    .await;
                (idx, res)
            });
        }

        // Initialize results with timeout errors in case we never get a response.
        let mut results: Vec<mgmt_api::Result<T>> = safekeepers
            .iter()
            .map(|_| {
                Err(mgmt_api::Error::Timeout(
                    "safekeeper operation timed out".to_string(),
                ))
            })
            .collect();

        // After we have built the joinset, we now wait for the tasks to complete,
        // but with a specified timeout to make sure we return swiftly, either with
        // a failure or success.
        let reconcile_deadline = tokio::time::Instant::now() + timeout;

        // Wait until all tasks finish or timeout is hit, whichever occurs
        // first.
        let mut result_count = 0;
        loop {
            if let Ok(res) = tokio::time::timeout_at(reconcile_deadline, joinset.join_next()).await
            {
                let Some(res) = res else { break };
                match res {
                    Ok((idx, res)) => {
                        let sk = &safekeepers[idx];
                        tracing::info!(
                            "response from safekeeper id:{} at {}: {:?}",
                            sk.get_id(),
                            sk.skp.host,
                            // Only print errors, as there is no Debug trait for T.
                            res.as_ref().map(|_| ()),
                        );
                        results[idx] = res;
                        result_count += 1;
                    }
                    Err(join_err) => {
                        tracing::info!("join_err for task in joinset: {join_err}");
                    }
                }
            } else {
                tracing::info!("timeout for operation call after {result_count} responses",);
                break;
            }
        }

        Ok(results)
    }

    /// Perform an operation on a list of safekeepers in parallel with retries,
    /// and validates that we reach a quorum of successful responses.
    ///
    /// Return the results of the operation on each safekeeper in the input order.
    /// It's guaranteed that at least a quorum of the responses are successful.
    async fn tenant_timeline_safekeeper_op_quorum<T, O, F>(
        &self,
        safekeepers: &[Safekeeper],
        op: O,
        timeout: Duration,
    ) -> Result<Vec<mgmt_api::Result<T>>, ApiError>
    where
        O: FnMut(SafekeeperClient) -> F,
        O: Clone + Send + 'static,
        F: std::future::Future<Output = mgmt_api::Result<T>> + Send + 'static,
        T: Sync + Send + 'static,
    {
        let target_sk_count = safekeepers.len();

        if target_sk_count == 0 {
            return Err(ApiError::InternalServerError(anyhow::anyhow!(
                "timeline configured without any safekeepers"
            )));
        }

        if target_sk_count < self.config.timeline_safekeeper_count {
            tracing::warn!(
                "running a quorum operation with {} safekeepers, which is less than configured {} safekeepers per timeline",
                target_sk_count,
                self.config.timeline_safekeeper_count
            );
        }

        let results = self
            .tenant_timeline_safekeeper_op(safekeepers, op, timeout)
            .await?;

        // Now check if quorum was reached in results.

        let quorum_size = target_sk_count / 2 + 1;

        let success_count = results.iter().filter(|res| res.is_ok()).count();
        if success_count < quorum_size {
            // Failure
            return Err(ApiError::InternalServerError(anyhow::anyhow!(
                "not enough successful reconciliations to reach quorum size: {success_count} of {quorum_size} of total {target_sk_count}"
            )));
        }

        Ok(results)
    }

    /// Create timeline in controller database and on safekeepers.
    /// `timeline_info` is result of timeline creation on pageserver.
    ///
    /// All actions must be idempotent as the call is retried until success. It
    /// tries to create timeline in the db and on at least majority of
    /// safekeepers + queue creation for safekeepers which missed it in the db
    /// for infinite retries; after that, call returns Ok.
    ///
    /// The idea is that once this is reached as long as we have alive majority
    /// of safekeepers it is expected to get eventually operational as storcon
    /// will be able to seed timeline on nodes which missed creation by making
    /// pull_timeline from peers. On the other hand we don't want to fail
    /// timeline creation if one safekeeper is down.
    pub(super) async fn tenant_timeline_create_safekeepers(
        self: &Arc<Self>,
        timeline_info: &TimelineInfo,
        read_only: bool,
    ) -> Result<SafekeepersInfo, ApiError> {
        let branch_id = timeline_info.branch_id;
        let pg_version = PgVersionId::from(timeline_info.pg_version);
        // Initially start_lsn is determined by last_record_lsn in pageserver
        // response as it does initdb. However, later we persist it and in sk
        // creation calls replace with the value from the timeline row if it
        // previously existed as on retries in theory endpoint might have
        // already written some data and advanced last_record_lsn, while we want
        // safekeepers to have consistent start_lsn.
        let start_lsn = timeline_info.last_record_lsn;

        // Choose initial set of safekeepers respecting affinity
        let sks = if !read_only {
            self.safekeepers_for_new_timeline().await?
        } else {
            Vec::new()
        };
        let sks_persistence = sks.iter().map(|sk| sk.id.0 as i64).collect::<Vec<_>>();
        // Add timeline to db
        let mut timeline_persist = TimelinePersistence {
            branch_id: branch_id.to_string(),
            start_lsn: start_lsn.into(),
            generation: 1,
            sk_set: sks_persistence.clone(),
            deleted_at: None,
            branch_name: None,
        };
        let inserted = self
            .persistence
            .insert_timeline(timeline_persist.clone())
            .await?;
        if !inserted {
            if let Some(existent_persist) = self.persistence.get_timeline(branch_id).await? {
                // Replace with what we have in the db, to get stuff like the generation right.
                // We do still repeat the http calls to the safekeepers. After all, we could have
                // crashed right after the wrote to the DB.
                timeline_persist = existent_persist;
            } else {
                return Err(ApiError::InternalServerError(anyhow::anyhow!(
                    "insertion said timeline already in db, but looking it up, it was gone"
                )));
            }
        }
        let ret = SafekeepersInfo {
            generation: timeline_persist.generation as u32,
            safekeepers: sks.clone(),
            branch_id,
        };
        if read_only {
            return Ok(ret);
        }

        // Create the timeline on the (single) safekeeper. yb-light is single-SK so
        // quorum == 1; a non-empty `remaining` means the SK did not respond and
        // there is nothing to fall back to.
        let remaining = self
            .tenant_timeline_create_safekeepers_quorum(branch_id, pg_version, &timeline_persist)
            .await?;
        if !remaining.is_empty() {
            return Err(ApiError::InternalServerError(anyhow::anyhow!(
                "safekeeper(s) {remaining:?} failed to create timeline {branch_id}",
            )));
        }

        Ok(ret)
    }

    /// Register a launcher-created timeline in the `timelines` table
    /// (per-endpoint topology: the launcher provisioned the branch's
    /// pageserver + safekeeper itself; this records the result for
    /// cluster bookkeeping — branch names, deletion flows). Idempotent:
    /// an existing row is left untouched.
    pub(crate) async fn timeline_register(
        self: &Arc<Self>,
        branch_id: BranchId,
        start_lsn: utils::lsn::Lsn,
        sk_set: Vec<utils::id::NodeId>,
        branch_name: Option<String>,
    ) -> Result<(), ApiError> {
        // `branch_name` is a non-unique label here: in the flat model
        // independent branches commonly share a name (e.g. every project's
        // root branch is "main"), so the controller does NOT enforce global
        // name uniqueness. Disambiguation by name is the launcher's local
        // concern; callers that need an exact branch pass `branch_id`.
        let timeline_persist = TimelinePersistence {
            branch_id: branch_id.to_string(),
            start_lsn: start_lsn.into(),
            generation: 1,
            sk_set: sk_set.iter().map(|id| id.0 as i64).collect(),
            deleted_at: None,
            branch_name,
        };
        let inserted = self.persistence.insert_timeline(timeline_persist).await?;
        if !inserted {
            tracing::info!("branch {branch_id} already registered, leaving row untouched");
        }
        Ok(())
    }

    pub(crate) async fn tenant_timeline_create_safekeepers_until_success(
        self: &Arc<Self>,
        timeline_info: TimelineInfo,
    ) -> Result<(), TimelineImportFinalizeError> {
        const BACKOFF: Duration = Duration::from_secs(5);

        loop {
            if self.cancel.is_cancelled() {
                return Err(TimelineImportFinalizeError::ShuttingDown);
            }

            // This function is only used in non-read-only scenarios
            let read_only = false;
            let res = self
                .tenant_timeline_create_safekeepers(&timeline_info, read_only)
                .await;

            match res {
                Ok(_) => {
                    tracing::info!("Timeline created on safekeepers");
                    break;
                }
                Err(err) => {
                    tracing::error!("Failed to create timeline on safekeepers: {err}");
                    tokio::select! {
                        _ = self.cancel.cancelled() => {
                            return Err(TimelineImportFinalizeError::ShuttingDown);
                        },
                        _ = tokio::time::sleep(BACKOFF) => {}
                    };
                }
            }
        }

        Ok(())
    }

    /// Directly insert the timeline into the database without reconciling it with safekeepers.
    ///
    /// Useful if the timeline already exists on the specified safekeepers,
    /// but we want to make it storage controller managed.
    pub(crate) async fn timeline_import(&self, req: TimelineImportRequest) -> Result<(), ApiError> {
        let persistence = TimelinePersistence {
            branch_id: req.branch_id.to_string(),
            start_lsn: req.start_lsn.into(),
            generation: 1,
            sk_set: req.sk_set.iter().map(|sk_id| sk_id.0 as i64).collect(),
            deleted_at: None,
            branch_name: None,
        };
        let inserted = self
            .persistence
            .insert_timeline(persistence.clone())
            .await?;
        if inserted {
            tracing::info!("imported timeline into db");
            return Ok(());
        }
        tracing::info!("timeline already present in db, updating");

        let update = TimelineUpdate {
            branch_id: persistence.branch_id,
            start_lsn: persistence.start_lsn,
            sk_set: persistence.sk_set,
        };
        self.persistence.update_timeline_unsafe(update).await?;
        tracing::info!("timeline updated");

        Ok(())
    }

    /// Locate safekeepers for a timeline.
    /// Return the generation and sk_set.
    /// If the timeline is not storcon-managed, return NotFound.
    pub(crate) async fn tenant_timeline_locate(
        &self,
        branch_id: BranchId,
    ) -> Result<TimelineLocateResponse, ApiError> {
        let timeline = self.persistence.get_timeline(branch_id).await?;

        let Some(timeline) = timeline else {
            return Err(ApiError::NotFound(
                anyhow::anyhow!("Timeline {} not found", branch_id).into(),
            ));
        };

        Ok(TimelineLocateResponse {
            generation: SafekeeperGeneration::new(timeline.generation as u32),
            sk_set: timeline
                .sk_set
                .iter()
                .map(|id| NodeId(*id as u64))
                .collect(),
            new_sk_set: None,
        })
    }

    /// Perform timeline deletion on the safekeeper(s), then remove it from the db.
    ///
    /// yb-light is single-SK with no background safekeeper reconciler, so the
    /// deletion is done synchronously inline (with retries inside the op helper).
    pub(super) async fn tenant_timeline_delete_safekeepers(
        self: &Arc<Self>,
        branch_id: BranchId,
    ) -> Result<(), ApiError> {
        let tl = self.persistence.get_timeline(branch_id).await?;
        let Some(tl) = tl else {
            tracing::info!(
                "timeline {branch_id} doesn't exist in timelines table, no deletions on safekeepers needed"
            );
            return Ok(());
        };
        self.persistence.timeline_set_deleted_at(branch_id).await?;

        let safekeepers = self.get_safekeepers(&tl.sk_set)?;
        if !safekeepers.is_empty() {
            const SK_DELETE_TIMELINE_TIMEOUT: Duration = Duration::from_secs(30);
            let results = self
                .tenant_timeline_safekeeper_op(
                    &safekeepers,
                    move |client| async move { client.delete_timeline(branch_id).await },
                    SK_DELETE_TIMELINE_TIMEOUT,
                )
                .await?;
            for (sk, res) in safekeepers.iter().zip(results) {
                if let Err(err) = res {
                    return Err(ApiError::InternalServerError(anyhow::anyhow!(
                        "failed to delete timeline {branch_id} on safekeeper {}: {err}",
                        sk.get_id()
                    )));
                }
                tracing::info!("deleted timeline from safekeeper {}", sk.get_id());
            }
        }

        if let Err(err) = self.persistence.delete_timeline(branch_id).await {
            tracing::warn!(%branch_id, "couldn't delete timeline from db: {err}");
        }
        Ok(())
    }

    /// Perform branch deletion on the safekeeper(s), then remove its timeline row from the db.
    ///
    /// yb-light is single-SK with no background safekeeper reconciler, so the
    /// deletion is done synchronously inline (with retries inside the op helper).
    pub(super) async fn branch_delete_safekeepers(
        self: &Arc<Self>,
        branch_id: BranchId,
    ) -> Result<(), ApiError> {
        let timeline_list = self
            .persistence
            .list_timelines_for_branch(branch_id)
            .await?;

        if timeline_list.is_empty() {
            // Early exit: the branch is either empty or not migrated to the storcon yet
            tracing::info!("Skipping branch delete as the timeline doesn't exist in db");
            return Ok(());
        }

        for timeline in timeline_list.iter() {
            let row_branch_id = BranchId::from_str(&timeline.branch_id)
                .context("branch id loaded from db")
                .map_err(ApiError::InternalServerError)?;
            self.persistence
                .timeline_set_deleted_at(row_branch_id)
                .await?;
        }

        // The set of safekeepers that have any of the branch's timelines.
        let mut sk_set = HashSet::new();
        for timeline in timeline_list.iter() {
            sk_set.extend(timeline.sk_set.iter().copied());
        }
        let sk_ids = sk_set.into_iter().collect::<Vec<_>>();
        let safekeepers = self.get_safekeepers(&sk_ids)?;

        if !safekeepers.is_empty() {
            const SK_DELETE_BRANCH_TIMEOUT: Duration = Duration::from_secs(30);
            let results = self
                .tenant_timeline_safekeeper_op(
                    &safekeepers,
                    move |client| async move { client.delete_timeline(branch_id).await },
                    SK_DELETE_BRANCH_TIMEOUT,
                )
                .await?;
            for (sk, res) in safekeepers.iter().zip(results) {
                if let Err(err) = res {
                    return Err(ApiError::InternalServerError(anyhow::anyhow!(
                        "failed to delete branch {branch_id} on safekeeper {}: {err}",
                        sk.get_id()
                    )));
                }
                tracing::info!(%branch_id, "deleted branch from safekeeper {}", sk.get_id());
            }
        }

        for timeline in timeline_list.iter() {
            let row_branch_id = BranchId::from_str(&timeline.branch_id)
                .context("branch id loaded from db")
                .map_err(ApiError::InternalServerError)?;
            if let Err(err) = self.persistence.delete_timeline(row_branch_id).await {
                tracing::warn!(%row_branch_id, "couldn't delete timeline from db: {err}");
            }
        }
        Ok(())
    }

    /// Choose safekeepers for the new timeline in different azs.
    /// 3 are choosen by default, but may be configured via config (for testing).
    pub(crate) async fn safekeepers_for_new_timeline(
        &self,
    ) -> Result<Vec<SafekeeperInfo>, ApiError> {
        let mut all_safekeepers = {
            let locked = self.inner.read().unwrap();
            locked
                .safekeepers
                .iter()
                .filter_map(|sk| {
                    if sk.1.scheduling_policy() != SkSchedulingPolicy::Active {
                        // If we don't want to schedule stuff onto the safekeeper, respect that.
                        return None;
                    }
                    let utilization_opt = if let SafekeeperState::Available {
                        last_seen_at: _,
                        utilization,
                    } = sk.1.availability()
                    {
                        Some(utilization)
                    } else {
                        // non-available safekeepers still get a chance for new timelines,
                        // but put them last in the list.
                        None
                    };
                    let info = SafekeeperInfo {
                        hostname: sk.1.skp.host.clone(),
                        id: NodeId(sk.1.skp.id as u64),
                    };
                    Some((utilization_opt, info, sk.1.skp.availability_zone_id.clone()))
                })
                .collect::<Vec<_>>()
        };
        all_safekeepers.sort_by_key(|sk| {
            (
                sk.0.as_ref()
                    .map(|ut| ut.timeline_count)
                    .unwrap_or(u64::MAX),
                // Use the id to decide on equal scores for reliability
                sk.1.id.0,
            )
        });
        // Number of safekeepers in different AZs we are looking for
        let wanted_count = self.config.timeline_safekeeper_count;

        let mut sks = Vec::new();
        let mut azs = HashSet::new();
        for (_sk_util, sk_info, az_id) in all_safekeepers.iter() {
            if !azs.insert(az_id) {
                continue;
            }
            sks.push(sk_info.clone());
            if sks.len() == wanted_count {
                break;
            }
        }
        if sks.len() == wanted_count {
            Ok(sks)
        } else {
            Err(ApiError::InternalServerError(anyhow::anyhow!(
                "couldn't find {wanted_count} safekeepers in different AZs for new timeline (found: {}, total active: {})",
                sks.len(),
                all_safekeepers.len(),
            )))
        }
    }

    pub(crate) async fn safekeepers_list(
        &self,
    ) -> Result<Vec<SafekeeperDescribeResponse>, DatabaseError> {
        let locked = self.inner.read().unwrap();
        let mut list = locked
            .safekeepers
            .iter()
            .map(|sk| sk.1.describe_response())
            .collect::<Result<Vec<_>, _>>()?;
        list.sort_by_key(|v| v.id);
        Ok(list)
    }

    pub(crate) async fn get_safekeeper(
        &self,
        id: i64,
    ) -> Result<SafekeeperDescribeResponse, DatabaseError> {
        let locked = self.inner.read().unwrap();
        let sk = locked
            .safekeepers
            .get(&NodeId(id as u64))
            .ok_or(diesel::result::Error::NotFound)?;
        sk.describe_response()
    }

    pub(crate) async fn upsert_safekeeper(
        self: &Arc<Service>,
        record: crate::persistence::SafekeeperUpsert,
    ) -> Result<(), ApiError> {
        let node_id = NodeId(record.id as u64);
        let use_https = self.config.use_https_safekeeper_api;

        if use_https && record.https_port.is_none() {
            return Err(ApiError::PreconditionFailed(
                format!(
                    "cannot upsert safekeeper {node_id}: \
                    https is enabled, but https port is not specified"
                )
                .into(),
            ));
        }

        self.persistence.safekeeper_upsert(record.clone()).await?;
        {
            let mut locked = self.inner.write().unwrap();
            let mut safekeepers = (*locked.safekeepers).clone();
            match safekeepers.entry(node_id) {
                std::collections::hash_map::Entry::Occupied(mut entry) => entry
                    .get_mut()
                    .update_from_record(record)
                    .expect("all preconditions should be checked before upsert to database"),
                std::collections::hash_map::Entry::Vacant(entry) => {
                    entry.insert(
                        Safekeeper::from_persistence(
                            crate::persistence::SafekeeperPersistence::from_upsert(
                                record,
                                SkSchedulingPolicy::Activating,
                            ),
                            CancellationToken::new(),
                            use_https,
                        )
                        .expect("all preconditions should be checked before upsert to database"),
                    );
                }
            }
            locked.safekeepers = Arc::new(safekeepers);
            metrics::METRICS_REGISTRY
                .metrics_group
                .storage_controller_safekeeper_nodes
                .set(locked.safekeepers.len() as i64);
            metrics::METRICS_REGISTRY
                .metrics_group
                .storage_controller_https_safekeeper_nodes
                .set(
                    locked
                        .safekeepers
                        .values()
                        .filter(|s| s.has_https_port())
                        .count() as i64,
                );
        }
        Ok(())
    }

    pub(crate) async fn set_safekeeper_scheduling_policy(
        self: &Arc<Service>,
        id: i64,
        scheduling_policy: SkSchedulingPolicy,
    ) -> Result<(), DatabaseError> {
        self.persistence
            .set_safekeeper_scheduling_policy(id, scheduling_policy)
            .await?;
        let node_id = NodeId(id as u64);
        // After the change has been persisted successfully, update the in-memory state
        self.set_safekeeper_scheduling_policy_in_mem(node_id, scheduling_policy)
            .await
    }

    pub(crate) async fn set_safekeeper_scheduling_policy_in_mem(
        self: &Arc<Service>,
        node_id: NodeId,
        scheduling_policy: SkSchedulingPolicy,
    ) -> Result<(), DatabaseError> {
        let mut locked = self.inner.write().unwrap();
        let mut safekeepers = (*locked.safekeepers).clone();
        let sk = safekeepers
            .get_mut(&node_id)
            .ok_or(DatabaseError::Logical("Not found".to_string()))?;
        sk.set_scheduling_policy(scheduling_policy);

        locked.safekeepers = Arc::new(safekeepers);
        Ok(())
    }
}
