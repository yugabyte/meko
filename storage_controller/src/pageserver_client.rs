use std::time::Duration;

use pageserver_api::models::detach_ancestor::AncestorDetached;
use pageserver_api::models::{
    DetachBehavior, LsnLease, PageserverUtilization, TenantScanRemoteStorageResponse,
    TimelineArchivalConfigRequest, TimelineInfo, TopTenantShardsRequest,
    TopTenantShardsResponse,
};
use pageserver_client::BlockUnblock;
use pageserver_client::mgmt_api::{Client, Result};
use reqwest::StatusCode;
use utils::id::{NodeId, TenantId, TimelineId};
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

    pub(crate) async fn tenant_delete(&self, tenant_shard_id: TenantId) -> Result<StatusCode> {
        measured_request!(
            "tenant",
            crate::metrics::Method::Delete,
            &self.node_id_label,
            self.inner.tenant_delete(tenant_shard_id).await
        )
    }

    pub(crate) async fn tenant_time_travel_remote_storage(
        &self,
        tenant_shard_id: TenantId,
        timestamp: &str,
        done_if_after: &str,
    ) -> Result<()> {
        measured_request!(
            "tenant_time_travel_remote_storage",
            crate::metrics::Method::Put,
            &self.node_id_label,
            self.inner
                .tenant_time_travel_remote_storage(tenant_shard_id, timestamp, done_if_after)
                .await
        )
    }

    #[expect(dead_code)]
    pub(crate) async fn tenant_timeline_compact(
        &self,
        tenant_shard_id: TenantId,
        timeline_id: TimelineId,
        force_image_layer_creation: bool,
        wait_until_done: bool,
    ) -> Result<()> {
        measured_request!(
            "tenant_timeline_compact",
            crate::metrics::Method::Put,
            &self.node_id_label,
            self.inner
                .tenant_timeline_compact(
                    tenant_shard_id,
                    timeline_id,
                    force_image_layer_creation,
                    true,
                    false,
                    wait_until_done,
                )
                .await
        )
    }

    pub(crate) async fn tenant_timeline_describe(
        &self,
        tenant_shard_id: &TenantId,
        timeline_id: &TimelineId,
    ) -> Result<TimelineInfo> {
        measured_request!(
            "tenant_timeline_describe",
            crate::metrics::Method::Get,
            &self.node_id_label,
            self.inner
                .tenant_timeline_describe(tenant_shard_id, timeline_id,)
                .await
        )
    }

    pub(crate) async fn tenant_scan_remote_storage(
        &self,
        tenant_id: TenantId,
    ) -> Result<TenantScanRemoteStorageResponse> {
        measured_request!(
            "tenant_scan_remote_storage",
            crate::metrics::Method::Get,
            &self.node_id_label,
            self.inner.tenant_scan_remote_storage(tenant_id).await
        )
    }

    pub(crate) async fn timeline_delete(
        &self,
        tenant_shard_id: TenantId,
        timeline_id: TimelineId,
    ) -> Result<StatusCode> {
        measured_request!(
            "timeline",
            crate::metrics::Method::Delete,
            &self.node_id_label,
            self.inner
                .timeline_delete(tenant_shard_id, timeline_id)
                .await
        )
    }

    pub(crate) async fn timeline_lease_lsn(
        &self,
        tenant_shard_id: TenantId,
        timeline_id: TimelineId,
        lsn: Lsn,
    ) -> Result<LsnLease> {
        measured_request!(
            "timeline_lease_lsn",
            crate::metrics::Method::Post,
            &self.node_id_label,
            self.inner
                .timeline_init_lsn_lease(tenant_shard_id, timeline_id, lsn)
                .await
        )
    }

    #[allow(unused)]
    pub(crate) async fn timeline_detail(
        &self,
        tenant_shard_id: TenantId,
        timeline_id: TimelineId,
    ) -> Result<TimelineInfo> {
        measured_request!(
            "timeline_detail",
            crate::metrics::Method::Get,
            &self.node_id_label,
            self.inner
                .timeline_detail(tenant_shard_id, timeline_id)
                .await
        )
    }

    pub(crate) async fn timeline_archival_config(
        &self,
        tenant_shard_id: TenantId,
        timeline_id: TimelineId,
        req: &TimelineArchivalConfigRequest,
    ) -> Result<()> {
        measured_request!(
            "timeline_archival_config",
            crate::metrics::Method::Put,
            &self.node_id_label,
            self.inner
                .timeline_archival_config(tenant_shard_id, timeline_id, req)
                .await
        )
    }

    pub(crate) async fn timeline_detach_ancestor(
        &self,
        tenant_shard_id: TenantId,
        timeline_id: TimelineId,
        behavior: Option<DetachBehavior>,
    ) -> Result<AncestorDetached> {
        measured_request!(
            "timeline_detach_ancestor",
            crate::metrics::Method::Put,
            &self.node_id_label,
            self.inner
                .timeline_detach_ancestor(tenant_shard_id, timeline_id, behavior)
                .await
        )
    }

    pub(crate) async fn timeline_block_unblock_gc(
        &self,
        tenant_shard_id: TenantId,
        timeline_id: TimelineId,
        dir: BlockUnblock,
    ) -> Result<()> {
        // measuring these makes no sense because we synchronize with the gc loop and remote
        // storage on block_gc so there should be huge outliers
        measured_request!(
            "timeline_block_unblock_gc",
            crate::metrics::Method::Post,
            &self.node_id_label,
            self.inner
                .timeline_block_unblock_gc(tenant_shard_id, timeline_id, dir)
                .await
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
    pub(crate) async fn top_tenant_shards(
        &self,
        request: TopTenantShardsRequest,
    ) -> Result<TopTenantShardsResponse> {
        measured_request!(
            "top_tenants",
            crate::metrics::Method::Post,
            &self.node_id_label,
            self.inner.top_tenant_shards(request).await
        )
    }

    pub(crate) async fn activate_post_import(
        &self,
        tenant_shard_id: TenantId,
        timeline_id: TimelineId,
        timeline_activate_timeout: Duration,
    ) -> Result<TimelineInfo> {
        measured_request!(
            "activate_post_import",
            crate::metrics::Method::Put,
            &self.node_id_label,
            self.inner
                .activate_post_import(tenant_shard_id, timeline_id, timeline_activate_timeout)
                .await
        )
    }
}
