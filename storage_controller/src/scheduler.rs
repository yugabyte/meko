use std::collections::HashMap;
use std::fmt::Debug;

use http_utils::error::ApiError;
use pageserver_api::models::PageserverUtilization;
use serde::Serialize;
use utils::id::NodeId;

use crate::node::Node;
use crate::branch::Branch;

/// Scenarios in which we cannot find a suitable location for a tenant shard
#[derive(thiserror::Error, Debug)]
pub enum ScheduleError {
    #[error("No pageservers found")]
    NoPageservers,
    #[error("No pageserver found matching constraint")]
    ImpossibleConstraint,
}

impl From<ScheduleError> for ApiError {
    fn from(value: ScheduleError) -> Self {
        ApiError::Conflict(format!("Scheduling error: {value}"))
    }
}

#[derive(Serialize)]
pub enum MaySchedule {
    Yes(PageserverUtilization),
    No,
}

#[derive(Serialize)]
pub(crate) struct SchedulerNode {
    /// How many shards are currently scheduled on this node, via their [`crate::branch::IntentState`].
    branch_count: usize,
    /// How many shards are currently attached on this node, via their [`crate::branch::IntentState`].
    attached_branch_count: usize,

    /// Whether this node is currently elegible to have new shards scheduled (this is derived
    /// from a node's availability state and scheduling policy).
    may_schedule: MaySchedule,
}

pub(crate) trait NodeSchedulingScore: Debug + Ord + Copy + Sized {
    fn generate(
        node_id: &NodeId,
        node: &mut SchedulerNode,
        context: &ScheduleContext,
    ) -> Option<Self>;

    fn is_overloaded(&self) -> bool;
    fn node_id(&self) -> NodeId;
}

pub(crate) trait ShardTag {
    type Score: NodeSchedulingScore;
}

pub(crate) struct AttachedShardTag {}
impl ShardTag for AttachedShardTag {
    type Score = NodeAttachmentSchedulingScore;
}

pub(crate) struct SecondaryShardTag {}
impl ShardTag for SecondaryShardTag {
    type Score = NodeSecondarySchedulingScore;
}

/// Scheduling score of a given node for shard attachments.
/// Lower scores indicate more suitable nodes.
/// Ordering is given by member declaration order (top to bottom).
#[derive(Debug, PartialEq, Eq, PartialOrd, Ord, Clone, Copy)]
pub(crate) struct NodeAttachmentSchedulingScore {
    /// The number of shards belonging to the tenant currently being
    /// scheduled that are attached to this node.
    affinity_score: AffinityScore,
    /// Utilisation score that combines shard count and disk utilisation
    utilization_score: u64,
    /// Total number of shards attached to this node. When nodes have identical utilisation, this
    /// acts as an anti-affinity between attached shards.
    total_attached_branch_count: usize,
    /// Convenience to make selection deterministic in tests and empty systems
    node_id: NodeId,
}

impl NodeSchedulingScore for NodeAttachmentSchedulingScore {
    fn generate(
        node_id: &NodeId,
        node: &mut SchedulerNode,
        context: &ScheduleContext,
    ) -> Option<Self> {
        let utilization = match &mut node.may_schedule {
            MaySchedule::Yes(u) => u,
            MaySchedule::No => {
                return None;
            }
        };

        Some(Self {
            affinity_score: context
                .nodes
                .get(node_id)
                .copied()
                .unwrap_or(AffinityScore::FREE),
            utilization_score: utilization.cached_score(),
            total_attached_branch_count: node.attached_branch_count,
            node_id: *node_id,
        })
    }

    fn is_overloaded(&self) -> bool {
        PageserverUtilization::is_overloaded(self.utilization_score)
    }

    fn node_id(&self) -> NodeId {
        self.node_id
    }
}

/// Scheduling score of a given node for shard secondaries.
/// Lower scores indicate more suitable nodes.
/// Ordering is given by member declaration order (top to bottom).
#[derive(Debug, PartialEq, Eq, PartialOrd, Ord, Clone, Copy)]
pub(crate) struct NodeSecondarySchedulingScore {
    /// The number of shards belonging to the tenant currently being
    /// scheduled that are attached to this node.
    affinity_score: AffinityScore,
    /// Utilisation score that combines shard count and disk utilisation
    utilization_score: u64,
    /// Anti-affinity with other locations: this gives the behavior that secondaries
    /// will spread out across the nodes.
    total_branch_count: usize,
    /// Convenience to make selection deterministic in tests and empty systems
    node_id: NodeId,
}

impl NodeSchedulingScore for NodeSecondarySchedulingScore {
    fn generate(
        node_id: &NodeId,
        node: &mut SchedulerNode,
        context: &ScheduleContext,
    ) -> Option<Self> {
        let utilization = match &mut node.may_schedule {
            MaySchedule::Yes(u) => u,
            MaySchedule::No => {
                return None;
            }
        };

        Some(Self {
            affinity_score: context
                .nodes
                .get(node_id)
                .copied()
                .unwrap_or(AffinityScore::FREE),
            utilization_score: utilization.cached_score(),
            total_branch_count: node.branch_count,
            node_id: *node_id,
        })
    }

    fn is_overloaded(&self) -> bool {
        PageserverUtilization::is_overloaded(self.utilization_score)
    }

    fn node_id(&self) -> NodeId {
        self.node_id
    }
}

impl PartialEq for SchedulerNode {
    fn eq(&self, other: &Self) -> bool {
        let may_schedule_matches = matches!(
            (&self.may_schedule, &other.may_schedule),
            (MaySchedule::Yes(_), MaySchedule::Yes(_)) | (MaySchedule::No, MaySchedule::No)
        );

        may_schedule_matches
            && self.branch_count == other.branch_count
            && self.attached_branch_count == other.attached_branch_count
    }
}

impl Eq for SchedulerNode {}

/// This type is responsible for selecting which node is used when a tenant shard needs to choose a pageserver
/// on which to run.
///
/// The type has no persistent state of its own: this is all populated at startup.  The Serialize
/// impl is only for debug dumps.
#[derive(Serialize)]
pub(crate) struct Scheduler {
    nodes: HashMap<NodeId, SchedulerNode>,
}

/// Score for soft constraint scheduling: lower scores are preferred to higher scores.
///
/// For example, we may set an affinity score based on the number of shards from the same
/// tenant already on a node, to implicitly prefer to balance out shards.
#[derive(Copy, Clone, Debug, Eq, PartialEq, PartialOrd, Ord)]
pub(crate) struct AffinityScore(pub(crate) usize);

impl AffinityScore {
    /// If we have no anti-affinity at all toward a node, this is its score.  It means
    /// the scheduler has a free choice amongst nodes with this score, and may pick a node
    /// based on other information such as total utilization.
    pub(crate) const FREE: Self = Self(0);

    pub(crate) fn inc(&mut self) {
        self.0 += 1;
    }
}

impl std::ops::Add for AffinityScore {
    type Output = Self;

    fn add(self, rhs: Self) -> Self::Output {
        Self(self.0 + rhs.0)
    }
}

// For carrying state between multiple calls to [`Branch::schedule`], e.g. when calling
// it for many shards in the same tenant.
#[derive(Debug, Default, Clone)]
pub(crate) struct ScheduleContext {
    /// Sparse map of nodes: omitting a node implicitly makes its affinity [`AffinityScore::FREE`]
    pub(crate) nodes: HashMap<NodeId, AffinityScore>,
}

impl ScheduleContext {
    /// Input is a list of nodes we would like to avoid using again within this context.  The more
    /// times a node is passed into this call, the less inclined we are to use it.
    pub(crate) fn avoid(&mut self, nodes: &[NodeId]) {
        for node_id in nodes {
            let entry = self.nodes.entry(*node_id).or_insert(AffinityScore::FREE);
            entry.inc()
        }
    }

}

pub(crate) enum RefCountUpdate {
    PromoteSecondary,
    Attach,
    Detach,
    DemoteAttached,
    AddSecondary,
    RemoveSecondary,
}

impl Scheduler {
    pub(crate) fn new<'a>(nodes: impl Iterator<Item = &'a Node>) -> Self {
        let mut scheduler_nodes = HashMap::new();
        for node in nodes {
            scheduler_nodes.insert(
                node.get_id(),
                SchedulerNode {
                    branch_count: 0,
                    attached_branch_count: 0,
                    may_schedule: node.may_schedule(),
                },
            );
        }

        Self {
            nodes: scheduler_nodes,
        }
    }

    /// For debug/support: check that our internal statistics are in sync with the state of
    /// the nodes & tenant shards.
    ///
    /// If anything is inconsistent, log details and return an error.
    pub(crate) fn consistency_check<'a>(
        &self,
        nodes: impl Iterator<Item = &'a Node>,
        shards: impl Iterator<Item = &'a Branch>,
    ) -> anyhow::Result<()> {
        let mut expect_nodes: HashMap<NodeId, SchedulerNode> = HashMap::new();
        for node in nodes {
            expect_nodes.insert(
                node.get_id(),
                SchedulerNode {
                    branch_count: 0,
                    attached_branch_count: 0,
                    may_schedule: node.may_schedule(),
                },
            );
        }

        for shard in shards {
            if let Some(node_id) = shard.intent.get_attached() {
                match expect_nodes.get_mut(node_id) {
                    Some(node) => {
                        node.branch_count += 1;
                        node.attached_branch_count += 1;
                    }
                    None => anyhow::bail!(
                        "Branch {} references nonexistent node {}",
                        shard.branch_id,
                        node_id
                    ),
                }
            }

            for node_id in shard.intent.get_secondary() {
                match expect_nodes.get_mut(node_id) {
                    Some(node) => {
                        node.branch_count += 1;
                    }
                    None => anyhow::bail!(
                        "Branch {} references nonexistent node {}",
                        shard.branch_id,
                        node_id
                    ),
                }
            }
        }

        for (node_id, expect_node) in &expect_nodes {
            let Some(self_node) = self.nodes.get(node_id) else {
                anyhow::bail!("Node {node_id} not found in Self")
            };

            if self_node != expect_node {
                tracing::error!("Inconsistency detected in scheduling state for node {node_id}");
                tracing::error!("Expected state: {}", serde_json::to_string(expect_node)?);
                tracing::error!("Self state: {}", serde_json::to_string(self_node)?);

                anyhow::bail!("Inconsistent state on {node_id}");
            }
        }

        if expect_nodes.len() != self.nodes.len() {
            // We just checked that all the expected nodes are present.  If the lengths don't match,
            // it means that we have nodes in Self that are unexpected.
            for node_id in self.nodes.keys() {
                if !expect_nodes.contains_key(node_id) {
                    anyhow::bail!("Node {node_id} found in Self but not in expected nodes");
                }
            }
        }

        Ok(())
    }

    /// Update the reference counts of a node. These reference counts are used to guide scheduling
    /// decisions, not for memory management: they represent the number of tenant shard whose IntentState
    /// targets this node and the number of tenants shars whose IntentState is attached to this
    /// node.
    ///
    /// It is an error to call this for a node that is not known to the scheduler (i.e. passed into
    /// [`Self::new`] or [`Self::node_upsert`])
    pub(crate) fn update_node_ref_counts(&mut self, node_id: NodeId, update: RefCountUpdate) {
        let Some(node) = self.nodes.get_mut(&node_id) else {
            debug_assert!(false);
            tracing::error!("Scheduler missing node {node_id}");
            return;
        };

        match update {
            RefCountUpdate::PromoteSecondary => {
                node.attached_branch_count += 1;
            }
            RefCountUpdate::Attach => {
                node.branch_count += 1;
                node.attached_branch_count += 1;
            }
            RefCountUpdate::Detach => {
                node.branch_count -= 1;
                node.attached_branch_count -= 1;
            }
            RefCountUpdate::DemoteAttached => {
                node.attached_branch_count -= 1;
            }
            RefCountUpdate::AddSecondary => {
                node.branch_count += 1;
            }
            RefCountUpdate::RemoveSecondary => {
                node.branch_count -= 1;
            }
        }
    }

    pub(crate) fn node_upsert(&mut self, node: &Node) {
        use std::collections::hash_map::Entry::*;
        match self.nodes.entry(node.get_id()) {
            Occupied(mut entry) => {
                entry.get_mut().may_schedule = node.may_schedule();
            }
            Vacant(entry) => {
                entry.insert(SchedulerNode {
                    branch_count: 0,
                    attached_branch_count: 0,
                    may_schedule: node.may_schedule(),
                });
            }
        }
    }

    pub(crate) fn node_remove(&mut self, node_id: NodeId) {
        if self.nodes.remove(&node_id).is_none() {
            tracing::warn!(node_id=%node_id, "Removed non-existent node from scheduler");
        }
    }

    /// Compute a schedulling score for each node that the scheduler knows of
    /// minus a set of hard excluded nodes.
    fn compute_node_scores<Score>(
        &mut self,
        hard_exclude: &[NodeId],
        context: &ScheduleContext,
    ) -> Vec<Score>
    where
        Score: NodeSchedulingScore,
    {
        self.nodes
            .iter_mut()
            .filter_map(|(k, v)| {
                if hard_exclude.contains(k) {
                    None
                } else {
                    Score::generate(k, v, context)
                }
            })
            .collect()
    }

    /// hard_exclude: it is forbidden to use nodes in this list, typically becacuse they
    /// are already in use by this shard -- we use this to avoid picking the same node
    /// as both attached and secondary location.  This is a hard constraint: if we cannot
    /// find any nodes that aren't in this list, then we will return a [`ScheduleError::ImpossibleConstraint`].
    ///
    /// context: we prefer to avoid using nodes identified in the context, according
    /// to their anti-affinity score.  We use this to prefeer to avoid placing shards in
    /// the same tenant on the same node.  This is a soft constraint: the context will never
    /// cause us to fail to schedule a shard.
    pub(crate) fn schedule_shard<Tag: ShardTag>(
        &mut self,
        hard_exclude: &[NodeId],
        context: &ScheduleContext,
    ) -> Result<NodeId, ScheduleError> {
        if self.nodes.is_empty() {
            return Err(ScheduleError::NoPageservers);
        }

        let mut scores = self.compute_node_scores::<Tag::Score>(hard_exclude, context);

        // Exclude nodes whose utilization is critically high, if there are alternatives available.  This will
        // cause us to violate affinity rules if it is necessary to avoid critically overloading nodes: for example
        // we may place shards in the same tenant together on the same pageserver if all other pageservers are
        // overloaded.
        let non_overloaded_scores = scores
            .iter()
            .filter(|i| !i.is_overloaded())
            .copied()
            .collect::<Vec<_>>();
        if !non_overloaded_scores.is_empty() {
            scores = non_overloaded_scores;
        }

        // Sort the nodes by score. The one with the lowest scores will be the preferred node.
        // Refer to [`NodeAttachmentSchedulingScore`] for attached locations and
        // [`NodeSecondarySchedulingScore`] for secondary locations to understand how the nodes
        // are ranked.
        scores.sort();

        if scores.is_empty() {
            // After applying constraints, no pageservers were left.
            // Log details to understand why we couldn't schedule: this may help an engineer
            // understand if some nodes are marked offline in a way that's preventing progress.
            tracing::info!("Scheduling failure, while excluding {hard_exclude:?}, node states:");
            for (node_id, node) in &self.nodes {
                tracing::info!(
                    "Node {node_id}: may_schedule={} shards={}",
                    !matches!(node.may_schedule, MaySchedule::No),
                    node.branch_count
                );
            }
            return Err(ScheduleError::ImpossibleConstraint);
        }

        // Lowest score wins
        let node_id = scores.first().unwrap().node_id();

        tracing::info!(
            "scheduler selected node {node_id} (elegible nodes {:?}, hard exclude: {hard_exclude:?}, soft exclude: {context:?})",
            scores.iter().map(|i| i.node_id().0).collect::<Vec<_>>(),
        );

        // Note that we do not update shard count here to reflect the scheduling: that
        // is IntentState's job when the scheduled location is used.

        Ok(node_id)
    }

    /// Selects any available node. This is suitable for performing background work (e.g. S3
    /// deletions).
    pub(crate) fn any_available_node(&mut self) -> Result<NodeId, ScheduleError> {
        self.schedule_shard::<AttachedShardTag>(&[], &ScheduleContext::default())
    }

    /// For use when choosing a preferred secondary location: filter out nodes that are not
    /// available.
    pub(crate) fn filter_usable_nodes(&self, nodes: &[NodeId]) -> Vec<NodeId> {
        nodes
            .iter()
            .filter_map(|node_id| {
                let node = self
                    .nodes
                    .get(node_id)
                    .expect("Referenced nodes always exist");
                if matches!(node.may_schedule, MaySchedule::Yes(_)) {
                    Some(*node_id)
                } else {
                    None
                }
            })
            .collect()
    }

    /// Unit test access to internal state
    #[cfg(test)]
    pub(crate) fn get_node_branch_count(&self, node_id: NodeId) -> usize {
        self.nodes.get(&node_id).unwrap().branch_count
    }

    #[cfg(test)]
    pub(crate) fn get_node_attached_branch_count(&self, node_id: NodeId) -> usize {
        self.nodes.get(&node_id).unwrap().attached_branch_count
    }

}

#[cfg(test)]
pub(crate) mod test_utils {

    use std::collections::HashMap;

    use pageserver_api::controller_api::NodeAvailability;
    use pageserver_api::models::utilization::test_utilization;
    use utils::id::NodeId;

    use crate::node::Node;

    /// Test helper: synthesize the requested number of nodes, all in active state.
    ///
    /// Node IDs start at one.
    pub(crate) fn make_test_nodes(n: u64) -> HashMap<NodeId, Node> {
        (1..n + 1)
            .map(|i| {
                (NodeId(i), {
                    let mut node = Node::new(
                        NodeId(i),
                        format!("httphost-{i}"),
                        80 + i as u16,
                        None,
                        format!("pghost-{i}"),
                        5432 + i as u16,
                        Some(format!("grpchost-{i}")),
                        Some(51051 + i as u16),
                        false,
                    )
                    .unwrap();
                    node.set_availability(NodeAvailability::Active(test_utilization::simple(0)));
                    assert!(node.is_available());
                    node
                })
            })
            .collect()
    }
}

#[cfg(test)]
mod tests {
    use pageserver_api::controller_api::NodeAvailability;
    use pageserver_api::models::utilization::test_utilization;

    use super::*;
    use crate::branch::IntentState;
    #[test]
    fn scheduler_basic() -> anyhow::Result<()> {
        let nodes = test_utils::make_test_nodes(2);

        let mut scheduler = Scheduler::new(nodes.values());
        let mut t1_intent = IntentState::new();
        let mut t2_intent = IntentState::new();

        let context = ScheduleContext::default();

        let scheduled = scheduler.schedule_shard::<AttachedShardTag>(&[], &context)?;
        t1_intent.set_attached(&mut scheduler, Some(scheduled));
        let scheduled = scheduler.schedule_shard::<AttachedShardTag>(&[], &context)?;
        t2_intent.set_attached(&mut scheduler, Some(scheduled));

        assert_eq!(scheduler.get_node_branch_count(NodeId(1)), 1);
        assert_eq!(scheduler.get_node_attached_branch_count(NodeId(1)), 1);

        assert_eq!(scheduler.get_node_branch_count(NodeId(2)), 1);
        assert_eq!(scheduler.get_node_attached_branch_count(NodeId(2)), 1);

        let scheduled = scheduler
            .schedule_shard::<AttachedShardTag>(&t1_intent.all_pageservers(), &context)?;
        t1_intent.push_secondary(&mut scheduler, scheduled);

        assert_eq!(scheduler.get_node_branch_count(NodeId(1)), 1);
        assert_eq!(scheduler.get_node_attached_branch_count(NodeId(1)), 1);

        assert_eq!(scheduler.get_node_branch_count(NodeId(2)), 2);
        assert_eq!(scheduler.get_node_attached_branch_count(NodeId(2)), 1);

        t1_intent.clear(&mut scheduler);
        assert_eq!(scheduler.get_node_branch_count(NodeId(1)), 0);
        assert_eq!(scheduler.get_node_branch_count(NodeId(2)), 1);

        let total_attached = scheduler.get_node_attached_branch_count(NodeId(1))
            + scheduler.get_node_attached_branch_count(NodeId(2));
        assert_eq!(total_attached, 1);

        if cfg!(debug_assertions) {
            // Dropping an IntentState without clearing it causes a panic in debug mode,
            // because we have failed to properly update scheduler shard counts.
            let result = std::panic::catch_unwind(move || {
                drop(t2_intent);
            });
            assert!(result.is_err());
        } else {
            t2_intent.clear(&mut scheduler);

            assert_eq!(scheduler.get_node_branch_count(NodeId(1)), 0);
            assert_eq!(scheduler.get_node_attached_branch_count(NodeId(1)), 0);

            assert_eq!(scheduler.get_node_branch_count(NodeId(2)), 0);
            assert_eq!(scheduler.get_node_attached_branch_count(NodeId(2)), 0);
        }

        Ok(())
    }

    #[test]
    /// Test the PageserverUtilization's contribution to scheduling algorithm
    fn scheduler_utilization() {
        let mut nodes = test_utils::make_test_nodes(3);
        let mut scheduler = Scheduler::new(nodes.values());

        // Need to keep these alive because they contribute to shard counts via RAII
        let mut scheduled_intents = Vec::new();

        let empty_context = ScheduleContext::default();

        fn assert_scheduler_chooses(
            expect_node: NodeId,
            scheduled_intents: &mut Vec<IntentState>,
            scheduler: &mut Scheduler,
            context: &ScheduleContext,
        ) {
            let scheduled = scheduler
                .schedule_shard::<AttachedShardTag>(&[], context)
                .unwrap();
            let mut intent = IntentState::new();
            intent.set_attached(scheduler, Some(scheduled));
            scheduled_intents.push(intent);
            assert_eq!(scheduled, expect_node);
        }

        // Independent schedule calls onto empty nodes should round-robin, because each node's
        // attached branch count is updated inline.  The order is determinsitic because when all other factors are
        // equal, we order by node ID.
        assert_scheduler_chooses(
            NodeId(1),
            &mut scheduled_intents,
            &mut scheduler,
            &empty_context,
        );
        assert_scheduler_chooses(
            NodeId(2),
            &mut scheduled_intents,
            &mut scheduler,
            &empty_context,
        );
        assert_scheduler_chooses(
            NodeId(3),
            &mut scheduled_intents,
            &mut scheduler,
            &empty_context,
        );

        // Manually setting utilization higher should cause schedule calls to round-robin the other nodes
        // which have equal utilization.
        nodes
            .get_mut(&NodeId(1))
            .unwrap()
            .set_availability(NodeAvailability::Active(test_utilization::simple(
                1024 * 1024 * 1024,
            )));
        scheduler.node_upsert(nodes.get(&NodeId(1)).unwrap());

        assert_scheduler_chooses(
            NodeId(2),
            &mut scheduled_intents,
            &mut scheduler,
            &empty_context,
        );
        assert_scheduler_chooses(
            NodeId(3),
            &mut scheduled_intents,
            &mut scheduler,
            &empty_context,
        );
        assert_scheduler_chooses(
            NodeId(2),
            &mut scheduled_intents,
            &mut scheduler,
            &empty_context,
        );
        assert_scheduler_chooses(
            NodeId(3),
            &mut scheduled_intents,
            &mut scheduler,
            &empty_context,
        );

        // The scheduler should prefer nodes with lower affinity score,
        // even if they have higher utilization (as long as they aren't utilized at >100%)
        let mut context_prefer_node1 = ScheduleContext::default();
        context_prefer_node1.avoid(&[NodeId(2), NodeId(3)]);
        assert_scheduler_chooses(
            NodeId(1),
            &mut scheduled_intents,
            &mut scheduler,
            &context_prefer_node1,
        );
        assert_scheduler_chooses(
            NodeId(1),
            &mut scheduled_intents,
            &mut scheduler,
            &context_prefer_node1,
        );

        // If a node is over-utilized, it will not be used even if affinity scores prefer it
        nodes
            .get_mut(&NodeId(1))
            .unwrap()
            .set_availability(NodeAvailability::Active(test_utilization::overloaded()));
        scheduler.node_upsert(nodes.get(&NodeId(1)).unwrap());
        assert_scheduler_chooses(
            NodeId(2),
            &mut scheduled_intents,
            &mut scheduler,
            &context_prefer_node1,
        );
        assert_scheduler_chooses(
            NodeId(3),
            &mut scheduled_intents,
            &mut scheduler,
            &context_prefer_node1,
        );

        for mut intent in scheduled_intents {
            intent.clear(&mut scheduler);
        }
    }
}
