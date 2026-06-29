use std::sync::Arc;

use crate::persistence::{TimelinePersistence, TimelineUpdate};
use http_utils::error::ApiError;
use pageserver_api::controller_api::TimelineImportRequest;
use utils::id::BranchId;

use super::Service;

// In yb-light's co-located topology each branch's endpoint provisions and owns
// its single co-located safekeeper, and the launcher creates the timeline on it
// directly. The storage controller never contacts a safekeeper; it only records
// branch rows in its `timelines` table for cluster bookkeeping (branch names,
// drop flows). See issue #171.
impl Service {
    /// Register a launcher-created timeline in the `timelines` table. Idempotent:
    /// an existing row is left untouched.
    pub(crate) async fn timeline_register(
        self: &Arc<Self>,
        branch_id: BranchId,
        start_lsn: utils::lsn::Lsn,
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
            deleted_at: None,
            branch_name,
        };
        let inserted = self.persistence.insert_timeline(timeline_persist).await?;
        if !inserted {
            tracing::info!("branch {branch_id} already registered, leaving row untouched");
        }
        Ok(())
    }

    /// Directly insert (or update) the branch's timeline row in the database.
    ///
    /// Useful if the timeline already exists on its safekeeper, but we want to
    /// make it storage controller managed.
    pub(crate) async fn timeline_import(&self, req: TimelineImportRequest) -> Result<(), ApiError> {
        let persistence = TimelinePersistence {
            branch_id: req.branch_id.to_string(),
            start_lsn: req.start_lsn.into(),
            generation: 1,
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
        };
        self.persistence.update_timeline_unsafe(update).await?;
        tracing::info!("timeline updated");

        Ok(())
    }
}
