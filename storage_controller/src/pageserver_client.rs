use std::time::Duration;

use pageserver_api::models::detach_ancestor::AncestorDetached;
use pageserver_api::models::{
    BranchScanRemoteStorageResponse, DetachBehavior, LsnLease, PageserverUtilization,
    TimelineArchivalConfigRequest, TimelineInfo, TopBranchesRequest, TopBranchesResponse,
};
use pageserver_client::BlockUnblock;
use pageserver_client::mgmt_api::{Client, Result};
use reqwest::StatusCode;
use utils::id::{BranchId, NodeId};
use utils::lsn::Lsn;

/// Thin wrapper around [`pageserver_client::mgmt_api::Client`]. It allows the storage
/// controller to collect metrics in a non-intrusive manner.
#[derive(Debug, Clone)]
pub(crate) struct PageserverClient {
    inner: Client,
    node_id_label: String,
}

macro_rules! measured_request {
    ($name:literal, $method:expr, $node_id: expr, $invoke:expr) => {{
        let labels = crate::metrics::PageserverRequestLabelGroup {
            pageserver_id: $node_id,
            path: $name,
            method: $method,
        };

        let latency = &crate::metrics::METRICS_REGISTRY
            .metrics_group
            .storage_controller_pageserver_request_latency;
        let _timer_guard = latency.start_timer(labels.clone());

        let res = $invoke;

        if res.is_err() {
            let error_counters = &crate::metrics::METRICS_REGISTRY
                .metrics_group
                .storage_controller_pageserver_request_error;
            error_counters.inc(labels)
        }

        res
    }};
}

impl PageserverClient {
    pub(crate) fn new(
        node_id: NodeId,
        raw_client: reqwest::Client,
        mgmt_api_endpoint: String,
        jwt: Option<&str>,
    ) -> Self {
        Self {
            inner: Client::new(raw_client, mgmt_api_endpoint, jwt),
            node_id_label: node_id.0.to_string(),
        }
    }

    pub(crate) async fn branch_delete(&self, branch_id: BranchId) -> Result<StatusCode> {
        measured_request!(
            "branch",
            crate::metrics::Method::Delete,
            &self.node_id_label,
            self.inner.branch_delete(branch_id).await
        )
    }

    pub(crate) async fn branch_time_travel_remote_storage(
        &self,
        branch_id: BranchId,
        timestamp: &str,
        done_if_after: &str,
    ) -> Result<()> {
        measured_request!(
            "branch_time_travel_remote_storage",
            crate::metrics::Method::Put,
            &self.node_id_label,
            self.inner
                .branch_time_travel_remote_storage(branch_id, timestamp, done_if_after)
                .await
        )
    }

    #[expect(dead_code)]
    pub(crate) async fn branch_compact(
        &self,
        branch_id: BranchId,
        force_image_layer_creation: bool,
        wait_until_done: bool,
    ) -> Result<()> {
        measured_request!(
            "branch_compact",
            crate::metrics::Method::Put,
            &self.node_id_label,
            self.inner
                .branch_compact(
                    branch_id,
                    force_image_layer_creation,
                    true,
                    false,
                    wait_until_done,
                )
                .await
        )
    }

    pub(crate) async fn branch_describe(&self, branch_id: &BranchId) -> Result<TimelineInfo> {
        measured_request!(
            "branch_describe",
            crate::metrics::Method::Get,
            &self.node_id_label,
            self.inner.branch_describe(branch_id).await
        )
    }

    pub(crate) async fn branch_scan_remote_storage(
        &self,
        branch_id: BranchId,
    ) -> Result<BranchScanRemoteStorageResponse> {
        measured_request!(
            "branch_scan_remote_storage",
            crate::metrics::Method::Get,
            &self.node_id_label,
            self.inner.branch_scan_remote_storage(branch_id).await
        )
    }

    pub(crate) async fn branch_lease_lsn(&self, branch_id: BranchId, lsn: Lsn) -> Result<LsnLease> {
        measured_request!(
            "branch_lease_lsn",
            crate::metrics::Method::Post,
            &self.node_id_label,
            self.inner.branch_init_lsn_lease(branch_id, lsn).await
        )
    }

    #[allow(unused)]
    pub(crate) async fn branch_detail(&self, branch_id: BranchId) -> Result<TimelineInfo> {
        measured_request!(
            "branch_detail",
            crate::metrics::Method::Get,
            &self.node_id_label,
            self.inner.branch_detail(branch_id).await
        )
    }

    pub(crate) async fn branch_archival_config(
        &self,
        branch_id: BranchId,
        req: &TimelineArchivalConfigRequest,
    ) -> Result<()> {
        measured_request!(
            "branch_archival_config",
            crate::metrics::Method::Put,
            &self.node_id_label,
            self.inner.branch_archival_config(branch_id, req).await
        )
    }

    pub(crate) async fn branch_detach_ancestor(
        &self,
        branch_id: BranchId,
        behavior: Option<DetachBehavior>,
    ) -> Result<AncestorDetached> {
        measured_request!(
            "branch_detach_ancestor",
            crate::metrics::Method::Put,
            &self.node_id_label,
            self.inner.branch_detach_ancestor(branch_id, behavior).await
        )
    }

    pub(crate) async fn branch_block_unblock_gc(
        &self,
        branch_id: BranchId,
        dir: BlockUnblock,
    ) -> Result<()> {
        // measuring these makes no sense because we synchronize with the gc loop and remote
        // storage on block_gc so there should be huge outliers
        measured_request!(
            "branch_block_unblock_gc",
            crate::metrics::Method::Post,
            &self.node_id_label,
            self.inner.branch_block_unblock_gc(branch_id, dir).await
        )
    }

    pub(crate) async fn get_utilization(&self) -> Result<PageserverUtilization> {
        measured_request!(
            "utilization",
            crate::metrics::Method::Get,
            &self.node_id_label,
            self.inner.get_utilization().await
        )
    }

    #[allow(dead_code)]
    pub(crate) async fn top_branches(
        &self,
        request: TopBranchesRequest,
    ) -> Result<TopBranchesResponse> {
        measured_request!(
            "top_branches",
            crate::metrics::Method::Post,
            &self.node_id_label,
            self.inner.top_branches(request).await
        )
    }

    pub(crate) async fn activate_post_import(
        &self,
        branch_id: BranchId,
        timeline_activate_timeout: Duration,
    ) -> Result<TimelineInfo> {
        measured_request!(
            "activate_post_import",
            crate::metrics::Method::Put,
            &self.node_id_label,
            self.inner
                .activate_post_import(branch_id, timeline_activate_timeout)
                .await
        )
    }
}
