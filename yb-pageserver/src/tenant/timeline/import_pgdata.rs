use std::sync::Arc;

use tokio::task::JoinHandle;
use tokio_util::sync::CancellationToken;
use utils::sync::gate::Gate;

use super::{Timeline, TimelineDeleteProgress};
use crate::context::RequestContext;

pub(crate) mod index_part_format;

/// yb-pageserver: ImportingTimeline is kept as a state-machine
/// placeholder so the tenant struct + activate_post_import HTTP
/// handler still compile. The actual S3-tarball → image-layer-file
/// import flow (`flow.rs`, `importbucket_client.rs`,
/// `importbucket_format.rs`) was deleted in Phase 4d — ybLight
/// users bootstrap via initdb + WAL through PageWriter, not via the
/// Neon cloud "import existing pgdata bucket" path.
pub struct ImportingTimeline {
    pub import_task_handle: JoinHandle<()>,
    pub import_task_gate: Gate,
    pub timeline: Arc<Timeline>,
    pub delete_progress: TimelineDeleteProgress,
}

impl std::fmt::Debug for ImportingTimeline {
    fn fmt(&self, f: &mut std::fmt::Formatter<'_>) -> std::fmt::Result {
        write!(f, "ImportingTimeline<{}>", self.timeline.timeline_id)
    }
}

impl ImportingTimeline {
    pub async fn shutdown(&self) {
        self.import_task_handle.abort();
        self.import_task_gate.close().await;

        self.timeline.remote_client.shutdown().await;
    }
}

/// yb-mode: external pgdata import via S3 is not supported.
pub async fn doit(
    _timeline: &Arc<Timeline>,
    _index_part: index_part_format::Root,
    _ctx: &RequestContext,
    _cancel: CancellationToken,
) -> anyhow::Result<()> {
    anyhow::bail!(
        "yb-pageserver: import-from-pgdata-bucket is not supported. \
         Bootstrap a ybLight tenant via initdb + WAL ingestion instead."
    )
}
