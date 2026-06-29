use std::collections::HashMap;

use http_utils::error::ApiError;
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

#[derive(Serialize, Clone, Copy, PartialEq, Eq)]
pub enum MaySchedule {
    Yes,
    No,
}

#[derive(Serialize, PartialEq, Eq)]
pub(crate) struct SchedulerNode {
    /// How many shards are currently scheduled on this node, via their [`crate::branch::IntentState`].
    branch_count: usize,
    /// How many shards are currently attached on this node, via their [`crate::branch::IntentState`].
    attached_branch_count: usize,

    /// Whether this node is currently elegible to have new shards scheduled (this is derived
    /// from a node's availability state and scheduling policy).
    may_schedule: MaySchedule,
}

/// Marker for the kind of location being scheduled. yb-light no longer does
/// utilization/affinity scoring (placement is launcher-pinned, one pageserver
/// per endpoint), so the tag only documents intent at the call site.
pub(crate) trait ShardTag {}

pub(crate) struct AttachedShardTag {}
impl ShardTag for AttachedShardTag {}

pub(crate) struct SecondaryShardTag {}
impl ShardTag for SecondaryShardTag {}

/// This type is responsible for selecting which node is used when a tenant shard needs to choose a pageserver
/// on which to run.
///
/// The type has no persistent state of its own: this is all populated at startup.  The Serialize
/// impl is only for debug dumps.
#[derive(Serialize)]
pub(crate) struct Scheduler {
    nodes: HashMap<NodeId, SchedulerNode>,
}

// Carried between multiple calls to [`Branch::schedule`]. yb-light no longer
// does affinity scoring (placement is launcher-pinned, one pageserver per
// endpoint), so this is now a no-op kept for call-site compatibility.
#[derive(Debug, Default, Clone)]
pub(crate) struct ScheduleContext {}

impl ScheduleContext {
    /// No-op: affinity-based avoidance was removed along with scheduler scoring.
    pub(crate) fn avoid(&mut self, _nodes: &[NodeId]) {}
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

    /// Pick a node to place a location on.
    ///
    /// hard_exclude: it is forbidden to use nodes in this list (e.g. a shard's
    /// existing locations, so we don't pick the same node twice). If no node
    /// outside this list is eligible, returns [`ScheduleError::ImpossibleConstraint`].
    ///
    /// yb-light no longer does utilization/affinity scoring (placement is
    /// launcher-pinned, one pageserver per endpoint). We simply pick the
    /// least-loaded eligible node, breaking ties by node id for determinism.
    /// `Tag`/`context` are kept for call-site compatibility but unused.
    pub(crate) fn schedule_shard<Tag: ShardTag>(
        &mut self,
        hard_exclude: &[NodeId],
        _context: &ScheduleContext,
    ) -> Result<NodeId, ScheduleError> {
        if self.nodes.is_empty() {
            return Err(ScheduleError::NoPageservers);
        }

        let node_id = self
            .nodes
            .iter()
            .filter(|(node_id, node)| {
                !hard_exclude.contains(node_id) && matches!(node.may_schedule, MaySchedule::Yes)
            })
            .min_by_key(|(node_id, node)| (node.branch_count, **node_id))
            .map(|(node_id, _)| *node_id);

        let Some(node_id) = node_id else {
            tracing::info!("Scheduling failure, while excluding {hard_exclude:?}, node states:");
            for (node_id, node) in &self.nodes {
                tracing::info!(
                    "Node {node_id}: may_schedule={} shards={}",
                    !matches!(node.may_schedule, MaySchedule::No),
                    node.branch_count
                );
            }
            return Err(ScheduleError::ImpossibleConstraint);
        };

        tracing::info!("scheduler selected node {node_id} (hard exclude: {hard_exclude:?})");

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
                if matches!(node.may_schedule, MaySchedule::Yes) {
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
}
