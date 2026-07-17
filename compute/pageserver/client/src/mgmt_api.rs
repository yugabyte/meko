use std::collections::HashMap;
use std::error::Error as _;
use std::time::Duration;

use bytes::Bytes;
use detach_ancestor::AncestorDetached;
use http_utils::error::HttpErrorBody;
use pageserver_api::models::*;
use postgres_versioninfo::PgMajorVersion;
pub use reqwest::Body as ReqwestBody;
use reqwest::{IntoUrl, Method, StatusCode, Url};
use utils::id::BranchId;
use utils::lsn::Lsn;

use crate::BlockUnblock;

pub mod util;

#[derive(Debug, Clone)]
pub struct Client {
    mgmt_api_endpoint: String,
    authorization_header: Option<String>,
    client: reqwest::Client,
}

#[derive(thiserror::Error, Debug)]
pub enum Error {
    #[error("send request: {0}{}", .0.source().map(|e| format!(": {e}")).unwrap_or_default())]
    SendRequest(reqwest::Error),

    #[error("receive body: {0}{}", .0.source().map(|e| format!(": {e}")).unwrap_or_default())]
    ReceiveBody(reqwest::Error),

    #[error("receive error body: {0}")]
    ReceiveErrorBody(String),

    #[error("pageserver API: {1}")]
    ApiError(StatusCode, String),

    #[error("Cancelled")]
    Cancelled,

    #[error("request timed out: {0}")]
    Timeout(String),
}

pub type Result<T> = std::result::Result<T, Error>;

pub trait ResponseErrorMessageExt: Sized {
    fn error_from_body(self) -> impl std::future::Future<Output = Result<Self>> + Send;
}

impl ResponseErrorMessageExt for reqwest::Response {
    async fn error_from_body(self) -> Result<Self> {
        let status = self.status();
        if !(status.is_client_error() || status.is_server_error()) {
            return Ok(self);
        }

        let url = self.url().to_owned();
        Err(match self.json::<HttpErrorBody>().await {
            Ok(HttpErrorBody { msg }) => Error::ApiError(status, msg),
            Err(_) => {
                Error::ReceiveErrorBody(format!("Http error ({}) at {}.", status.as_u16(), url))
            }
        })
    }
}

pub enum ForceAwaitLogicalSize {
    Yes,
    No,
}

impl Client {
    pub fn new(client: reqwest::Client, mgmt_api_endpoint: String, jwt: Option<&str>) -> Self {
        Self {
            mgmt_api_endpoint,
            authorization_header: jwt.map(|jwt| format!("Bearer {jwt}")),
            client,
        }
    }

    pub async fn list_branches(&self) -> Result<Vec<pageserver_api::models::BranchInfo>> {
        let uri = format!("{}/v1/branch", self.mgmt_api_endpoint);
        let resp = self.get(&uri).await?;
        resp.json().await.map_err(Error::ReceiveBody)
    }

    /// Send an HTTP request to an arbitrary path with a desired HTTP method and returning a streaming
    /// Response.  This function is suitable for pass-through/proxy use cases where we don't care
    /// what the response content looks like.
    ///
    /// Use/add one of the properly typed methods below if you know aren't proxying, and
    /// know what kind of response you expect.
    pub async fn op_raw(&self, method: Method, path: String) -> Result<reqwest::Response> {
        debug_assert!(path.starts_with('/'));
        let uri = format!("{}{}", self.mgmt_api_endpoint, path);

        let mut req = self.client.request(method, uri);
        if let Some(value) = &self.authorization_header {
            req = req.header(reqwest::header::AUTHORIZATION, value);
        }
        req.send().await.map_err(Error::ReceiveBody)
    }

    pub async fn branch_details(
        &self,
        branch_id: BranchId,
    ) -> Result<pageserver_api::models::BranchDetails> {
        let uri = format!("{}/v1/branch/{branch_id}", self.mgmt_api_endpoint);
        self.get(uri)
            .await?
            .json()
            .await
            .map_err(Error::ReceiveBody)
    }

    pub async fn timeline_info(
        &self,
        branch_id: BranchId,
        force_await_logical_size: ForceAwaitLogicalSize,
    ) -> Result<pageserver_api::models::TimelineInfo> {
        let uri = format!("{}/v1/branch/{branch_id}", self.mgmt_api_endpoint);

        let uri = match force_await_logical_size {
            ForceAwaitLogicalSize::Yes => format!("{}?force-await-logical-size={}", uri, true),
            ForceAwaitLogicalSize::No => uri,
        };

        self.get(&uri)
            .await?
            .json()
            .await
            .map_err(Error::ReceiveBody)
    }

    pub async fn keyspace(
        &self,
        branch_id: BranchId,
    ) -> Result<pageserver_api::models::partitioning::Partitioning> {
        let uri = format!("{}/v1/branch/{branch_id}/keyspace", self.mgmt_api_endpoint);
        self.get(&uri)
            .await?
            .json()
            .await
            .map_err(Error::ReceiveBody)
    }

    async fn get<U: IntoUrl>(&self, uri: U) -> Result<reqwest::Response> {
        self.request(Method::GET, uri, ()).await
    }

    fn start_request<U: reqwest::IntoUrl>(
        &self,
        method: Method,
        uri: U,
    ) -> reqwest::RequestBuilder {
        let req = self.client.request(method, uri);
        if let Some(value) = &self.authorization_header {
            req.header(reqwest::header::AUTHORIZATION, value)
        } else {
            req
        }
    }

    async fn request_noerror<B: serde::Serialize, U: reqwest::IntoUrl>(
        &self,
        method: Method,
        uri: U,
        body: B,
    ) -> Result<reqwest::Response> {
        self.start_request(method, uri)
            .json(&body)
            .send()
            .await
            .map_err(Error::ReceiveBody)
    }

    async fn request<B: serde::Serialize, U: reqwest::IntoUrl>(
        &self,
        method: Method,
        uri: U,
        body: B,
    ) -> Result<reqwest::Response> {
        let res = self.request_noerror(method, uri, body).await?;
        let response = res.error_from_body().await?;
        Ok(response)
    }

    pub async fn status(&self) -> Result<()> {
        let uri = format!("{}/v1/status", self.mgmt_api_endpoint);
        self.get(&uri).await?;
        Ok(())
    }

    /// The branch deletion API can return 202 if deletion is incomplete, or
    /// 404 if it is complete.  Callers are responsible for checking the status
    /// code and retrying.  Error codes other than 404 will return Err().
    pub async fn branch_delete(&self, branch_id: BranchId) -> Result<StatusCode> {
        let uri = format!("{}/v1/branch/{branch_id}", self.mgmt_api_endpoint);

        match self.request(Method::DELETE, &uri, ()).await {
            Err(Error::ApiError(status_code, msg)) => {
                if status_code == StatusCode::NOT_FOUND {
                    Ok(StatusCode::NOT_FOUND)
                } else {
                    Err(Error::ApiError(status_code, msg))
                }
            }
            Err(e) => Err(e),
            Ok(response) => Ok(response.status()),
        }
    }

    pub async fn branch_time_travel_remote_storage(
        &self,
        branch_id: BranchId,
        timestamp: &str,
        done_if_after: &str,
    ) -> Result<()> {
        let uri = format!(
            "{}/v1/branch/{branch_id}/time_travel_remote_storage?travel_to={timestamp}&done_if_after={done_if_after}",
            self.mgmt_api_endpoint
        );
        self.request(Method::PUT, &uri, ()).await?;
        Ok(())
    }

    pub async fn branch_compact(
        &self,
        branch_id: BranchId,
        force_image_layer_creation: bool,
        must_force_image_layer_creation: bool,
        scheduled: bool,
        wait_until_done: bool,
    ) -> Result<()> {
        let mut path = reqwest::Url::parse(&format!(
            "{}/v1/branch/{branch_id}/compact",
            self.mgmt_api_endpoint
        ))
        .expect("Cannot build URL");

        if force_image_layer_creation {
            path.query_pairs_mut()
                .append_pair("force_image_layer_creation", "true");
        }

        if must_force_image_layer_creation {
            path.query_pairs_mut()
                .append_pair("must_force_image_layer_creation", "true");
        }

        if scheduled {
            path.query_pairs_mut().append_pair("scheduled", "true");
        }
        if wait_until_done {
            path.query_pairs_mut()
                .append_pair("wait_until_scheduled_compaction_done", "true");
            path.query_pairs_mut()
                .append_pair("wait_until_uploaded", "true");
        }
        self.request(Method::PUT, path, ()).await?;
        Ok(())
    }

    pub async fn branch_describe(&self, branch_id: &BranchId) -> Result<TimelineInfo> {
        let mut path =
            reqwest::Url::parse(&format!("{}/v1/branch/{branch_id}", self.mgmt_api_endpoint))
                .expect("Cannot build URL");
        path.query_pairs_mut()
            .append_pair("include-image-consistent-lsn", "true");

        let response: reqwest::Response = self.request(Method::GET, path, ()).await?;
        let body = response.json().await.map_err(Error::ReceiveBody)?;
        Ok(body)
    }

    pub async fn branch_scan_remote_storage(
        &self,
        branch_id: BranchId,
    ) -> Result<BranchScanRemoteStorageResponse> {
        let uri = format!(
            "{}/v1/branch/{branch_id}/scan_remote_storage",
            self.mgmt_api_endpoint
        );
        let response = self.request(Method::GET, &uri, ()).await?;
        let body = response.json().await.map_err(Error::ReceiveBody)?;
        Ok(body)
    }

    pub async fn set_branch_config(&self, req: &BranchConfigRequest) -> Result<()> {
        let uri = format!("{}/v1/branch/config", self.mgmt_api_endpoint);
        self.request(Method::PUT, &uri, req).await?;
        Ok(())
    }

    pub async fn patch_branch_config(&self, req: &BranchConfigPatchRequest) -> Result<()> {
        let uri = format!("{}/v1/branch/config", self.mgmt_api_endpoint);
        self.request(Method::PATCH, &uri, req).await?;
        Ok(())
    }

    pub async fn branch_secondary_download(
        &self,
        branch_id: BranchId,
        wait: Option<std::time::Duration>,
    ) -> Result<(StatusCode, SecondaryProgress)> {
        let mut path = reqwest::Url::parse(&format!(
            "{}/v1/branch/{}/secondary/download",
            self.mgmt_api_endpoint, branch_id
        ))
        .expect("Cannot build URL");

        if let Some(wait) = wait {
            path.query_pairs_mut()
                .append_pair("wait_ms", &format!("{}", wait.as_millis()));
        }

        let response = self.request(Method::POST, path, ()).await?;
        let status = response.status();
        let progress: SecondaryProgress = response.json().await.map_err(Error::ReceiveBody)?;
        Ok((status, progress))
    }

    pub async fn branch_secondary_status(&self, branch_id: BranchId) -> Result<SecondaryProgress> {
        let path = reqwest::Url::parse(&format!(
            "{}/v1/branch/{}/secondary/status",
            self.mgmt_api_endpoint, branch_id
        ))
        .expect("Cannot build URL");

        self.request(Method::GET, path, ())
            .await?
            .json()
            .await
            .map_err(Error::ReceiveBody)
    }

    pub async fn branch_heatmap_upload(&self, branch_id: BranchId) -> Result<()> {
        let path = reqwest::Url::parse(&format!(
            "{}/v1/branch/{}/heatmap_upload",
            self.mgmt_api_endpoint, branch_id
        ))
        .expect("Cannot build URL");

        self.request(Method::POST, path, ()).await?;
        Ok(())
    }

    pub async fn location_config(
        &self,
        branch_id: BranchId,
        config: LocationConfig,
        flush_ms: Option<std::time::Duration>,
        lazy: bool,
    ) -> Result<()> {
        let req_body = BranchLocationConfigRequest { config };

        let mut path = reqwest::Url::parse(&format!(
            "{}/v1/branch/{}/location_config",
            self.mgmt_api_endpoint, branch_id
        ))
        // Should always work: mgmt_api_endpoint is configuration, not user input.
        .expect("Cannot build URL");

        if lazy {
            path.query_pairs_mut().append_pair("lazy", "true");
        }

        if let Some(flush_ms) = flush_ms {
            path.query_pairs_mut()
                .append_pair("flush_ms", &format!("{}", flush_ms.as_millis()));
        }

        self.request(Method::PUT, path, &req_body).await?;
        Ok(())
    }

    pub async fn list_location_config(&self) -> Result<LocationConfigListResponse> {
        let path = format!("{}/v1/location_config", self.mgmt_api_endpoint);
        self.request(Method::GET, &path, ())
            .await?
            .json()
            .await
            .map_err(Error::ReceiveBody)
    }

    pub async fn get_location_config(&self, branch_id: BranchId) -> Result<Option<LocationConfig>> {
        let path = format!("{}/v1/location_config/{branch_id}", self.mgmt_api_endpoint);
        self.request(Method::GET, &path, ())
            .await?
            .json()
            .await
            .map_err(Error::ReceiveBody)
    }

    pub async fn branch_create(&self, req: &TimelineCreateRequest) -> Result<TimelineInfo> {
        let uri = format!("{}/v1/branch", self.mgmt_api_endpoint);
        self.request(Method::POST, &uri, req)
            .await?
            .json()
            .await
            .map_err(Error::ReceiveBody)
    }

    pub async fn branch_detail(&self, branch_id: BranchId) -> Result<TimelineInfo> {
        let uri = format!("{}/v1/branch/{branch_id}", self.mgmt_api_endpoint);

        self.request(Method::GET, &uri, ())
            .await?
            .json()
            .await
            .map_err(Error::ReceiveBody)
    }

    pub async fn branch_archival_config(
        &self,
        branch_id: BranchId,
        req: &TimelineArchivalConfigRequest,
    ) -> Result<()> {
        let uri = format!(
            "{}/v1/branch/{branch_id}/archival_config",
            self.mgmt_api_endpoint
        );

        self.request(Method::PUT, &uri, req)
            .await?
            .json()
            .await
            .map_err(Error::ReceiveBody)
    }

    pub async fn branch_detach_ancestor(
        &self,
        branch_id: BranchId,
        behavior: Option<DetachBehavior>,
    ) -> Result<AncestorDetached> {
        let uri = format!(
            "{}/v1/branch/{branch_id}/detach_ancestor",
            self.mgmt_api_endpoint
        );
        let mut uri = Url::parse(&uri)
            .map_err(|e| Error::ApiError(StatusCode::INTERNAL_SERVER_ERROR, format!("{e}")))?;

        if let Some(behavior) = behavior {
            uri.query_pairs_mut()
                .append_pair("detach_behavior", &behavior.to_string());
        }

        self.request(Method::PUT, uri, ())
            .await?
            .json()
            .await
            .map_err(Error::ReceiveBody)
    }

    pub async fn branch_block_unblock_gc(
        &self,
        branch_id: BranchId,
        dir: BlockUnblock,
    ) -> Result<()> {
        let uri = format!("{}/v1/branch/{branch_id}/{dir}_gc", self.mgmt_api_endpoint,);

        self.request(Method::POST, &uri, ()).await.map(|_| ())
    }

    /// Flush the branch's pages to YB, advancing its durable
    /// `consistent_lsn`. With `wait_lsn`, the pageserver first waits for WAL
    /// ingest to reach that LSN so the post-flush `consistent_lsn` covers it —
    /// used to flush an ancestor at branch time so the branch's durability
    /// barrier clears without waiting for the periodic flush.
    pub async fn branch_flush(&self, branch_id: BranchId, wait_lsn: Option<Lsn>) -> Result<()> {
        let mut url = reqwest::Url::parse(&format!(
            "{}/v1/branch/{branch_id}/flush",
            self.mgmt_api_endpoint,
        ))
        .expect("Cannot build URL");
        if let Some(lsn) = wait_lsn {
            url.query_pairs_mut().append_pair("lsn", &lsn.to_string());
        }
        self.request(Method::PUT, url, ()).await.map(|_| ())
    }

    pub async fn branch_download_heatmap_layers(
        &self,
        branch_id: BranchId,
        concurrency: Option<usize>,
        recurse: bool,
    ) -> Result<()> {
        let mut path = reqwest::Url::parse(&format!(
            "{}/v1/branch/{}/download_heatmap_layers",
            self.mgmt_api_endpoint, branch_id
        ))
        .expect("Cannot build URL");

        path.query_pairs_mut()
            .append_pair("recurse", &format!("{recurse}"));

        if let Some(concurrency) = concurrency {
            path.query_pairs_mut()
                .append_pair("concurrency", &format!("{concurrency}"));
        }

        self.request(Method::POST, path, ()).await.map(|_| ())
    }

    pub async fn branch_reset(&self, branch_id: BranchId) -> Result<()> {
        let uri = format!("{}/v1/branch/{}/reset", self.mgmt_api_endpoint, branch_id);
        self.request(Method::POST, &uri, ())
            .await?
            .json()
            .await
            .map_err(Error::ReceiveBody)
    }

    pub async fn branch_synthetic_size(&self, branch_id: BranchId) -> Result<BranchHistorySize> {
        let uri = format!(
            "{}/v1/branch/{}/synthetic_size",
            self.mgmt_api_endpoint, branch_id
        );
        self.get(&uri)
            .await?
            .json()
            .await
            .map_err(Error::ReceiveBody)
    }

    pub async fn get_utilization(&self) -> Result<PageserverUtilization> {
        let uri = format!("{}/v1/utilization", self.mgmt_api_endpoint);
        self.get(uri)
            .await?
            .json()
            .await
            .map_err(Error::ReceiveBody)
    }

    pub async fn top_branches(&self, request: TopBranchesRequest) -> Result<TopBranchesResponse> {
        let uri = format!("{}/v1/top_branches", self.mgmt_api_endpoint);
        self.request(Method::POST, uri, request)
            .await?
            .json()
            .await
            .map_err(Error::ReceiveBody)
    }

    pub async fn ingest_aux_files(
        &self,
        branch_id: BranchId,
        aux_files: HashMap<String, String>,
    ) -> Result<bool> {
        let uri = format!(
            "{}/v1/branch/{}/ingest_aux_files",
            self.mgmt_api_endpoint, branch_id
        );
        let resp = self
            .request_noerror(Method::POST, &uri, IngestAuxFilesRequest { aux_files })
            .await?;
        match resp.status() {
            StatusCode::OK => Ok(true),
            status => Err(match resp.json::<HttpErrorBody>().await {
                Ok(HttpErrorBody { msg }) => Error::ApiError(status, msg),
                Err(_) => {
                    Error::ReceiveErrorBody(format!("Http error ({}) at {}.", status.as_u16(), uri))
                }
            }),
        }
    }

    pub async fn list_aux_files(
        &self,
        branch_id: BranchId,
        lsn: Lsn,
    ) -> Result<HashMap<String, Bytes>> {
        let uri = format!(
            "{}/v1/branch/{}/list_aux_files",
            self.mgmt_api_endpoint, branch_id
        );
        let resp = self
            .request_noerror(Method::POST, &uri, ListAuxFilesRequest { lsn })
            .await?;
        match resp.status() {
            StatusCode::OK => {
                let resp: HashMap<String, Bytes> = resp.json().await.map_err(|e| {
                    Error::ApiError(StatusCode::INTERNAL_SERVER_ERROR, format!("{e}"))
                })?;
                Ok(resp)
            }
            status => Err(match resp.json::<HttpErrorBody>().await {
                Ok(HttpErrorBody { msg }) => Error::ApiError(status, msg),
                Err(_) => {
                    Error::ReceiveErrorBody(format!("Http error ({}) at {}.", status.as_u16(), uri))
                }
            }),
        }
    }

    pub async fn import_basebackup(
        &self,
        branch_id: BranchId,
        base_lsn: Lsn,
        end_lsn: Lsn,
        pg_version: PgMajorVersion,
        basebackup_tarball: ReqwestBody,
    ) -> Result<()> {
        let pg_version = pg_version.major_version_num();

        let uri = format!(
            "{}/v1/branch/{branch_id}/import_basebackup?base_lsn={base_lsn}&end_lsn={end_lsn}&pg_version={pg_version}",
            self.mgmt_api_endpoint,
        );
        self.start_request(Method::PUT, uri)
            .body(basebackup_tarball)
            .send()
            .await
            .map_err(Error::SendRequest)?
            .error_from_body()
            .await?
            .json()
            .await
            .map_err(Error::ReceiveBody)
    }

    pub async fn import_wal(
        &self,
        branch_id: BranchId,
        start_lsn: Lsn,
        end_lsn: Lsn,
        wal_tarball: ReqwestBody,
    ) -> Result<()> {
        let uri = format!(
            "{}/v1/branch/{branch_id}/import_wal?start_lsn={start_lsn}&end_lsn={end_lsn}",
            self.mgmt_api_endpoint,
        );
        self.start_request(Method::PUT, uri)
            .body(wal_tarball)
            .send()
            .await
            .map_err(Error::SendRequest)?
            .error_from_body()
            .await?
            .json()
            .await
            .map_err(Error::ReceiveBody)
    }

    pub async fn branch_init_lsn_lease(&self, branch_id: BranchId, lsn: Lsn) -> Result<LsnLease> {
        let uri = format!("{}/v1/branch/{branch_id}/lsn_lease", self.mgmt_api_endpoint,);

        self.request(Method::POST, &uri, LsnLeaseRequest { lsn })
            .await?
            .json()
            .await
            .map_err(Error::ReceiveBody)
    }

    pub async fn wait_lsn(&self, request: BranchWaitLsnRequest) -> Result<StatusCode> {
        let uri = format!("{}/v1/branch/wait_lsn", self.mgmt_api_endpoint,);

        self.request_noerror(Method::POST, uri, request)
            .await
            .map(|resp| resp.status())
    }

    pub async fn activate_post_import(
        &self,
        branch_id: BranchId,
        activate_timeline_timeout: Duration,
    ) -> Result<TimelineInfo> {
        let uri = format!(
            "{}/v1/branch/{}/activate_post_import?timeline_activate_timeout_ms={}",
            self.mgmt_api_endpoint,
            branch_id,
            activate_timeline_timeout.as_millis()
        );

        self.request(Method::PUT, uri, ())
            .await?
            .json()
            .await
            .map_err(Error::ReceiveBody)
    }
}
