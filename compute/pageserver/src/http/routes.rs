//!
//! Management HTTP API
//!
use std::str::FromStr;
use std::sync::Arc;
use std::time::Duration;

use anyhow::{Context, Result, anyhow};
use http_utils::endpoint::{
    self, attach_openapi_ui, auth_middleware, check_permission_with, profile_cpu_handler,
    profile_heap_handler, prometheus_metrics_handler, request_span,
};
use http_utils::error::{ApiError, HttpErrorBody};
use http_utils::failpoints::failpoints_handler;
use http_utils::json::{json_request, json_response};
use http_utils::request::{must_get_query_param, parse_query_param, parse_request_param};
use http_utils::{RequestExt, RouterBuilder};
use humantime::format_rfc3339;
use hyper::{Body, Request, Response, StatusCode, Uri, header};
use metrics::launch_timestamp::LaunchTimestamp;
use pageserver_api::models::{
    LsnLease, LsnLeaseRequest, StatusResponse, TimelineGcRequest, TimelineInfo,
};
use remote_storage::GenericRemoteStorage;
use tokio_util::sync::CancellationToken;
use tracing::*;
use utils::auth::SwappableJwtAuth;
use utils::id::BranchId;
use utils::lsn::Lsn;

use crate::config::PageServerConf;
use crate::context::{DownloadBehavior, RequestContext};
use crate::page_server;
use crate::page_server::mgr::{GetTenantError, TenantManager};
use crate::page_server::timeline::Timeline;
use crate::page_server::{GetTimelineError, PageReconstructError};
use crate::pgdatadir_mapping::LsnForTimestamp;
use crate::task_mgr::TaskKind;

pub struct State {
    conf: &'static PageServerConf,
    tenant_manager: Arc<TenantManager>,
    auth: Option<Arc<SwappableJwtAuth>>,
    allowlist_routes: &'static [&'static str],
    #[allow(dead_code)]
    remote_storage: GenericRemoteStorage,
    latest_utilization: tokio::sync::Mutex<Option<(std::time::Instant, bytes::Bytes)>>,
}

impl State {
    pub fn new(
        conf: &'static PageServerConf,
        tenant_manager: Arc<TenantManager>,
        auth: Option<Arc<SwappableJwtAuth>>,
        remote_storage: GenericRemoteStorage,
    ) -> anyhow::Result<Self> {
        let allowlist_routes = &[
            "/v1/status",
            "/v1/doc",
            "/swagger.yml",
            "/metrics",
            "/profile/cpu",
            "/profile/heap",
        ];
        Ok(Self {
            conf,
            tenant_manager,
            auth,
            allowlist_routes,
            remote_storage,
            latest_utilization: Default::default(),
        })
    }
}

#[inline(always)]
fn get_state(request: &Request<Body>) -> &State {
    request
        .data::<Arc<State>>()
        .expect("unknown state type")
        .as_ref()
}

#[inline(always)]
fn get_config(request: &Request<Body>) -> &'static PageServerConf {
    get_state(request).conf
}

/// Check that the requester is authorized to operate on given tenant
fn check_permission(request: &Request<Body>, branch_id: Option<BranchId>) -> Result<(), ApiError> {
    check_permission_with(request, |claims| {
        crate::auth::check_permission(claims, branch_id)
    })
}

impl From<PageReconstructError> for ApiError {
    fn from(pre: PageReconstructError) -> ApiError {
        match pre {
            PageReconstructError::Other(other) => ApiError::InternalServerError(other),
            PageReconstructError::MissingKey(e) => ApiError::InternalServerError(e.into()),
            PageReconstructError::Cancelled => ApiError::Cancelled,
            PageReconstructError::AncestorLsnTimeout(e) => ApiError::Timeout(format!("{e}").into()),
            PageReconstructError::WalRedo(pre) => ApiError::InternalServerError(pre),
        }
    }
}

impl From<GetTenantError> for ApiError {
    fn from(tse: GetTenantError) -> ApiError {
        match tse {
            GetTenantError::NotFound(tid) => ApiError::NotFound(anyhow!("tenant {tid}").into()),
        }
    }
}

impl From<GetTimelineError> for ApiError {
    fn from(gte: GetTimelineError) -> Self {
        // Rationale: tenant is activated only after eligble timelines activate
        ApiError::NotFound(gte.into())
    }
}

// Helper function to construct a TimelineInfo struct for a timeline
async fn build_timeline_info(
    timeline: &Arc<Timeline>,
    include_non_incremental_logical_size: bool,
    force_await_initial_logical_size: bool,
    include_image_consistent_lsn: bool,
    ctx: &RequestContext,
) -> anyhow::Result<TimelineInfo> {
    crate::page_server::debug_assert_current_span_has_branch_id();

    if force_await_initial_logical_size {
        timeline.clone().await_initial_logical_size().await
    }

    let mut info = build_timeline_info_common(
        timeline,
        ctx,
        page_server::timeline::GetLogicalSizePriority::Background,
    )
    .await?;
    if include_non_incremental_logical_size {
        // XXX we should be using spawn_ondemand_logical_size_calculation here.
        // Otherwise, if someone deletes the timeline / detaches the tenant while
        // we're executing this function, we will outlive the timeline on-disk state.
        info.current_logical_size_non_incremental = Some(
            timeline
                .get_current_logical_size_non_incremental(info.last_record_lsn, ctx)
                .await?,
        );
    }
    if include_image_consistent_lsn {
        info.image_consistent_lsn = Some(timeline.compute_image_consistent_lsn().await?);
    }
    Ok(info)
}

async fn build_timeline_info_common(
    timeline: &Arc<Timeline>,
    ctx: &RequestContext,
    logical_size_task_priority: page_server::timeline::GetLogicalSizePriority,
) -> anyhow::Result<TimelineInfo> {
    crate::page_server::debug_assert_current_span_has_branch_id();
    let initdb_lsn = timeline.initdb_lsn;
    let last_record_lsn = timeline.get_last_record_lsn();
    let (wal_source_connstr, last_received_msg_lsn, last_received_msg_ts) = {
        let guard = timeline.last_received_wal.lock().unwrap();
        if let Some(info) = guard.as_ref() {
            (
                Some(format!("{}", info.wal_source_connconf)), // Password is hidden, but it's for statistics only.
                Some(info.last_received_msg_lsn),
                Some(info.last_received_msg_ts),
            )
        } else {
            (None, None, None)
        }
    };

    let ancestor_branch_id = timeline.get_ancestor_branch_id();
    let ancestor_lsn = match timeline.get_ancestor_lsn() {
        Lsn(0) => None,
        lsn @ Lsn(_) => Some(lsn),
    };
    let current_logical_size = timeline.get_current_logical_size(logical_size_task_priority, ctx);
    let current_physical_size = Some(timeline.layer_size_sum().await);
    let state = timeline.current_state();
    // Report is_archived = false if the timeline is still loading
    let is_archived = timeline.is_archived().unwrap_or(false);
    let remote_consistent_lsn_projected = timeline
        .get_remote_consistent_lsn_projected()
        .unwrap_or(Lsn(0));
    let remote_consistent_lsn_visible = timeline
        .get_remote_consistent_lsn_visible()
        .unwrap_or(Lsn(0));
    let is_invisible = timeline
        .remote_state
        .invisible
        .load(std::sync::atomic::Ordering::Relaxed);

    let walreceiver_status = timeline.walreceiver_status();

    let (pitr_history_size, within_ancestor_pitr) = timeline.get_pitr_history_stats();

    // Externally, expose the lowest LSN that can be used to create a branch.
    // Internally we distinguish between the planned GC cutoff (PITR point) and the "applied" GC cutoff (where we
    // actually trimmed data to), which can pass each other when PITR is changed.
    let min_readable_lsn = std::cmp::max(
        timeline.get_gc_cutoff_lsn().unwrap_or_default(),
        *timeline.get_applied_gc_cutoff_lsn(),
    );

    let (rel_size_migration, rel_size_migrated_at) = timeline.get_rel_size_v2_status();

    let info = TimelineInfo {
        branch_id: timeline.branch_id,
        ancestor_branch_id,
        ancestor_lsn,
        // HTTP API field name kept as `disk_consistent_lsn` for back-compat
        // but populated with the durable `consistent_lsn` (gap-free, post-flush).
        // `applied_lsn` is exposed separately via `last_record_lsn`.
        disk_consistent_lsn: timeline.get_consistent_lsn(),
        remote_consistent_lsn: remote_consistent_lsn_projected,
        remote_consistent_lsn_visible,
        initdb_lsn,
        last_record_lsn,
        prev_record_lsn: Some(timeline.get_prev_record_lsn()),
        min_readable_lsn,
        applied_gc_cutoff_lsn: *timeline.get_applied_gc_cutoff_lsn(),
        current_logical_size: current_logical_size.size_dont_care_about_accuracy(),
        current_logical_size_is_accurate: match current_logical_size.accuracy() {
            page_server::timeline::logical_size::Accuracy::Approximate => false,
            page_server::timeline::logical_size::Accuracy::Exact => true,
        },
        directory_entries_counts: timeline.get_directory_metrics().to_vec(),
        current_physical_size,
        current_logical_size_non_incremental: None,
        pitr_history_size,
        within_ancestor_pitr,
        timeline_dir_layer_file_size_sum: None,
        wal_source_connstr,
        last_received_msg_lsn,
        last_received_msg_ts,
        pg_version: timeline.pg_version,

        state,
        is_archived: Some(is_archived),
        rel_size_migration: Some(rel_size_migration),
        rel_size_migrated_at,
        is_invisible: Some(is_invisible),

        walreceiver_status,
        image_consistent_lsn: None,
    };
    Ok(info)
}

// healthcheck handler
async fn status_handler(
    request: Request<Body>,
    _cancel: CancellationToken,
) -> Result<Response<Body>, ApiError> {
    check_permission(&request, None)?;
    json_response(StatusCode::OK, StatusResponse {})
}

async fn reload_auth_validation_keys_handler(
    request: Request<Body>,
    _cancel: CancellationToken,
) -> Result<Response<Body>, ApiError> {
    check_permission(&request, None)?;
    let config = get_config(&request);
    let state = get_state(&request);
    let Some(shared_auth) = &state.auth else {
        return json_response(StatusCode::BAD_REQUEST, ());
    };
    // unwrap is ok because check is performed when creating config, so path is set and exists
    let key_path = config.auth_validation_public_key_path.as_ref().unwrap();
    info!("Reloading public key(s) for verifying JWT tokens from {key_path:?}");

    match utils::auth::JwtAuth::from_key_path(key_path) {
        Ok(new_auth) => {
            shared_auth.swap(new_auth);
            json_response(StatusCode::OK, ())
        }
        Err(e) => {
            let err_msg = "Error reloading public keys";
            warn!("Error reloading public keys from {key_path:?}: {e:}");
            json_response(
                StatusCode::INTERNAL_SERVER_ERROR,
                HttpErrorBody::from_msg(err_msg.to_string()),
            )
        }
    }
}

async fn timeline_list_handler(
    request: Request<Body>,
    _cancel: CancellationToken,
) -> Result<Response<Body>, ApiError> {
    let branch_id: BranchId = parse_request_param(&request, "branch_id")?;
    let include_non_incremental_logical_size: Option<bool> =
        parse_query_param(&request, "include-non-incremental-logical-size")?;
    let force_await_initial_logical_size: Option<bool> =
        parse_query_param(&request, "force-await-initial-logical-size")?;
    let include_image_consistent_lsn: Option<bool> =
        parse_query_param(&request, "include-image-consistent-lsn")?;
    check_permission(&request, Some(branch_id))?;

    let state = get_state(&request);
    let ctx = RequestContext::new(TaskKind::MgmtRequest, DownloadBehavior::Download);

    let response_data = async {
        let tenant = state.tenant_manager.get_attached_tenant_shard(branch_id)?;

        let timelines = tenant.list_timelines();

        let mut response_data = Vec::with_capacity(timelines.len());
        for timeline in timelines {
            let timeline_info = build_timeline_info(
                &timeline,
                include_non_incremental_logical_size.unwrap_or(false),
                force_await_initial_logical_size.unwrap_or(false),
                include_image_consistent_lsn.unwrap_or(false),
                &ctx,
            )
            .instrument(info_span!("build_timeline_info", branch_id = %timeline.branch_id))
            .await
            .context("Failed to build timeline info")
            .map_err(ApiError::InternalServerError)?;

            response_data.push(timeline_info);
        }
        Ok::<Vec<TimelineInfo>, ApiError>(response_data)
    }
    .instrument(info_span!("timeline_list",
                branch_id = %branch_id))
    .await?;

    json_response(StatusCode::OK, response_data)
}

async fn timeline_detail_handler(
    request: Request<Body>,
    _cancel: CancellationToken,
) -> Result<Response<Body>, ApiError> {
    let branch_id: BranchId = parse_request_param(&request, "branch_id")?;
    let include_non_incremental_logical_size: Option<bool> =
        parse_query_param(&request, "include-non-incremental-logical-size")?;
    let force_await_initial_logical_size: Option<bool> =
        parse_query_param(&request, "force-await-initial-logical-size")?;
    let include_image_consistent_lsn: Option<bool> =
        parse_query_param(&request, "include-image-consistent-lsn")?;
    check_permission(&request, Some(branch_id))?;

    // Logical size calculation needs downloading.
    let ctx = RequestContext::new(TaskKind::MgmtRequest, DownloadBehavior::Download);
    let state = get_state(&request);

    let timeline_info = async {
        let tenant = state.tenant_manager.get_attached_tenant_shard(branch_id)?;

        let timeline = tenant.get_timeline(branch_id, false)?;
        let ctx = &ctx.with_scope_timeline(&timeline);

        let timeline_info = build_timeline_info(
            &timeline,
            include_non_incremental_logical_size.unwrap_or(false),
            force_await_initial_logical_size.unwrap_or(false),
            include_image_consistent_lsn.unwrap_or(false),
            ctx,
        )
        .await
        .context("get local timeline info")
        .map_err(ApiError::InternalServerError)?;

        Ok::<_, ApiError>(timeline_info)
    }
    .instrument(info_span!("timeline_detail",
                branch_id = %branch_id,
                %branch_id))
    .await?;

    json_response(StatusCode::OK, timeline_info)
}

async fn get_lsn_by_timestamp_handler(
    request: Request<Body>,
    cancel: CancellationToken,
) -> Result<Response<Body>, ApiError> {
    let branch_id: BranchId = parse_request_param(&request, "branch_id")?;
    check_permission(&request, Some(branch_id))?;
    let state = get_state(&request);

    let branch_id: BranchId = parse_request_param(&request, "branch_id")?;
    let timestamp_raw = must_get_query_param(&request, "timestamp")?;
    let timestamp = humantime::parse_rfc3339(&timestamp_raw)
        .with_context(|| format!("Invalid time: {timestamp_raw:?}"))
        .map_err(ApiError::BadRequest)?;
    let timestamp_pg = postgres_ffi::to_pg_timestamp(timestamp);

    let with_lease = parse_query_param(&request, "with_lease")?.unwrap_or(false);

    let timeline = active_timeline_of_active_tenant(&state.tenant_manager, branch_id).await?;
    let ctx = RequestContext::new(TaskKind::MgmtRequest, DownloadBehavior::Download)
        .with_scope_timeline(&timeline);
    let result = timeline
        .find_lsn_for_timestamp(timestamp_pg, &cancel, &ctx)
        .await?;

    #[derive(serde::Serialize, Debug)]
    struct Result {
        lsn: Lsn,
        kind: &'static str,
        #[serde(default)]
        #[serde(skip_serializing_if = "Option::is_none")]
        #[serde(flatten)]
        lease: Option<LsnLease>,
    }
    let (lsn, kind) = match result {
        LsnForTimestamp::Present(lsn) => (lsn, "present"),
        LsnForTimestamp::Future(lsn) => (lsn, "future"),
        LsnForTimestamp::Past(lsn) => (lsn, "past"),
        LsnForTimestamp::NoData(lsn) => (lsn, "nodata"),
    };

    let lease = if with_lease {
        timeline
            .init_lsn_lease(lsn, timeline.get_lsn_lease_length_for_ts(), &ctx)
            .inspect_err(|_| {
                warn!("fail to grant a lease to {}", lsn);
            })
            .ok()
    } else {
        None
    };

    let result = Result { lsn, kind, lease };
    let valid_until = result
        .lease
        .as_ref()
        .map(|l| humantime::format_rfc3339_millis(l.valid_until).to_string());
    tracing::info!(
        lsn=?result.lsn,
        kind=%result.kind,
        timestamp=%timestamp_raw,
        valid_until=?valid_until,
        "lsn_by_timestamp finished"
    );
    json_response(StatusCode::OK, result)
}

async fn get_timestamp_of_lsn_handler(
    request: Request<Body>,
    _cancel: CancellationToken,
) -> Result<Response<Body>, ApiError> {
    let branch_id: BranchId = parse_request_param(&request, "branch_id")?;
    check_permission(&request, Some(branch_id))?;
    let state = get_state(&request);

    let branch_id: BranchId = parse_request_param(&request, "branch_id")?;

    let lsn_str = must_get_query_param(&request, "lsn")?;
    let lsn = Lsn::from_str(&lsn_str)
        .with_context(|| format!("Invalid LSN: {lsn_str:?}"))
        .map_err(ApiError::BadRequest)?;

    let timeline = active_timeline_of_active_tenant(&state.tenant_manager, branch_id).await?;
    let ctx = RequestContext::new(TaskKind::MgmtRequest, DownloadBehavior::Download)
        .with_scope_timeline(&timeline);
    let result = timeline.get_timestamp_for_lsn(lsn, &ctx).await?;

    match result {
        Some(time) => {
            let time = format_rfc3339(
                postgres_ffi::try_from_pg_timestamp(time).map_err(ApiError::InternalServerError)?,
            )
            .to_string();
            json_response(StatusCode::OK, time)
        }
        None => Err(ApiError::PreconditionFailed(
            format!("Timestamp for lsn {lsn} not found").into(),
        )),
    }
}

pub fn html_response(status: StatusCode, data: String) -> Result<Response<Body>, ApiError> {
    let response = Response::builder()
        .status(status)
        .header(header::CONTENT_TYPE, "text/html")
        .body(Body::from(data.as_bytes().to_vec()))
        .map_err(|e| ApiError::InternalServerError(e.into()))?;
    Ok(response)
}

// Obtains an lsn lease on the given timeline.
async fn lsn_lease_handler(
    mut request: Request<Body>,
    _cancel: CancellationToken,
) -> Result<Response<Body>, ApiError> {
    let branch_id: BranchId = parse_request_param(&request, "branch_id")?;
    check_permission(&request, Some(branch_id))?;
    let lsn = json_request::<LsnLeaseRequest>(&mut request).await?.lsn;

    let ctx = RequestContext::new(TaskKind::MgmtRequest, DownloadBehavior::Download);

    let state = get_state(&request);

    let timeline = active_timeline_of_active_tenant(&state.tenant_manager, branch_id).await?;

    let result = async {
        timeline
            .init_lsn_lease(lsn, timeline.get_lsn_lease_length(), &ctx)
            .map_err(|e| {
                ApiError::InternalServerError(
                    e.context(format!("invalid lsn lease request at {lsn}")),
                )
            })
    }
    .instrument(info_span!("init_lsn_lease", branch_id = %branch_id))
    .await?;

    json_response(StatusCode::OK, result)
}

// Run GC immediately on given timeline.
async fn timeline_gc_handler(
    mut request: Request<Body>,
    cancel: CancellationToken,
) -> Result<Response<Body>, ApiError> {
    let branch_id: BranchId = parse_request_param(&request, "branch_id")?;
    check_permission(&request, Some(branch_id))?;

    let gc_req: TimelineGcRequest = json_request(&mut request).await?;

    let state = get_state(&request);

    let ctx = RequestContext::new(TaskKind::MgmtRequest, DownloadBehavior::Download);
    let gc_result = state
        .tenant_manager
        .immediate_gc(branch_id, gc_req, cancel, &ctx)
        .await?;

    json_response(StatusCode::OK, gc_result)
}
// Flush the timeline's pages to YB, advancing its durable `consistent_lsn`.
// With `?lsn=<lsn>`, first wait for WAL ingest to reach that LSN, so the
// post-flush `consistent_lsn` is `>= lsn` (used at branch time to clear the
// ancestor's `create_branch_row` durability barrier without waiting for the
// periodic flush). In yb-amp "freeze" is a no-op (no in-memory layers) — this
// is just a flush to YB; named accordingly.
async fn timeline_flush_handler(
    request: Request<Body>,
    _cancel: CancellationToken,
) -> Result<Response<Body>, ApiError> {
    let branch_id: BranchId = parse_request_param(&request, "branch_id")?;
    check_permission(&request, Some(branch_id))?;

    let wait_lsn: Option<Lsn> = parse_query_param(&request, "lsn")?;

    let state = get_state(&request);

    async {
        let timeline = active_timeline_of_active_tenant(&state.tenant_manager, branch_id).await?;
        let ctx = RequestContext::new(TaskKind::MgmtRequest, DownloadBehavior::Download)
            .with_scope_timeline(&timeline);

        // Wait for ingest to catch up to the requested LSN before flushing,
        // so the resulting durable `consistent_lsn` covers it.
        if let Some(lsn) = wait_lsn {
            timeline
                .wait_lsn(
                    lsn,
                    page_server::timeline::WaitLsnWaiter::PageService,
                    page_server::timeline::WaitLsnTimeout::Default,
                    &ctx,
                )
                .await
                .map_err(|e| ApiError::InternalServerError(e.into()))?;
        }

        // `force_flush` (not plain `flush`): this endpoint forces the durable
        // consistent_lsn UPDATE through immediately (bypassing the throttle the
        // periodic/bootstrap flushes use), so a branching descendant's
        // durability barrier clears without waiting out the `max_age` window.
        timeline.force_flush().await.map_err(|e| match e {
            page_server::timeline::FlushLayerError::Cancelled => ApiError::ShuttingDown,
            other => ApiError::InternalServerError(other.into()),
        })?;

        json_response(StatusCode::OK, ())
    }
    .instrument(info_span!("flush", branch_id = %branch_id))
    .await
}

async fn active_timeline_of_active_tenant(
    tenant_manager: &TenantManager,
    branch_id: BranchId,
) -> Result<Arc<Timeline>, ApiError> {
    let tenant = tenant_manager.get_attached_tenant_shard(branch_id)?;

    Ok(tenant.get_timeline(branch_id, true)?)
}

async fn always_panic_handler(
    req: Request<Body>,
    _cancel: CancellationToken,
) -> Result<Response<Body>, ApiError> {
    // Deliberately cause a panic to exercise the panic hook registered via std::panic::set_hook().
    // For pageserver, the relevant panic hook is `tracing_panic_hook` , and the `sentry` crate's wrapper around it.
    // Use catch_unwind to ensure that tokio nor hyper are distracted by our panic.
    let query = req.uri().query();
    let _ = std::panic::catch_unwind(|| {
        panic!("unconditional panic for testing panic hook integration; request query: {query:?}")
    });
    json_response(StatusCode::NO_CONTENT, ())
}
async fn handler_404(_: Request<Body>) -> Result<Response<Body>, ApiError> {
    json_response(
        StatusCode::NOT_FOUND,
        HttpErrorBody::from_msg("page not found".to_owned()),
    )
}

async fn post_tracing_event_handler(
    mut r: Request<Body>,
    _cancel: CancellationToken,
) -> Result<Response<Body>, ApiError> {
    #[derive(Debug, serde::Deserialize)]
    #[serde(rename_all = "lowercase")]
    enum Level {
        Error,
        Warn,
        Info,
        Debug,
        Trace,
    }
    #[derive(Debug, serde::Deserialize)]
    struct Request {
        level: Level,
        message: String,
    }
    let body: Request = json_request(&mut r)
        .await
        .map_err(|_| ApiError::BadRequest(anyhow::anyhow!("invalid JSON body")))?;

    match body.level {
        Level::Error => tracing::error!(?body.message),
        Level::Warn => tracing::warn!(?body.message),
        Level::Info => tracing::info!(?body.message),
        Level::Debug => tracing::debug!(?body.message),
        Level::Trace => tracing::trace!(?body.message),
    }

    json_response(StatusCode::OK, ())
}

/// Polled by control plane.
///
/// See [`crate::utilization`].
async fn get_utilization(
    r: Request<Body>,
    _cancel: CancellationToken,
) -> Result<Response<Body>, ApiError> {
    fail::fail_point!("get-utilization-http-handler", |_| {
        Err(ApiError::ResourceUnavailable("failpoint".into()))
    });

    // this probably could be completely public, but lets make that change later.
    check_permission(&r, None)?;

    let state = get_state(&r);
    let mut g = state.latest_utilization.lock().await;

    let regenerate_every = Duration::from_secs(1);
    let still_valid = g
        .as_ref()
        .is_some_and(|(captured_at, _)| captured_at.elapsed() < regenerate_every);

    // avoid needless statvfs calls even though those should be non-blocking fast.
    // regenerate at most 1Hz to allow polling at any rate.
    if !still_valid {
        let path = state.conf.branches_path();
        let doc =
            crate::utilization::regenerate(state.conf, path.as_std_path(), &state.tenant_manager)
                .map_err(ApiError::InternalServerError)?;

        let mut buf = Vec::new();
        serde_json::to_writer(&mut buf, &doc)
            .context("serialize")
            .map_err(ApiError::InternalServerError)?;

        let body = bytes::Bytes::from(buf);

        *g = Some((std::time::Instant::now(), body));
    }

    // hyper 0.14 doesn't yet have Response::clone so this is a bit of extra legwork
    let cached = g.as_ref().expect("just set").1.clone();

    Response::builder()
        .header(hyper::http::header::CONTENT_TYPE, "application/json")
        // thought of using http date header, but that is second precision which does not give any
        // debugging aid
        .status(StatusCode::OK)
        .body(hyper::Body::from(cached))
        .context("build response")
        .map_err(ApiError::InternalServerError)
}

/// Common functionality of all the HTTP API handlers.
///
/// - Adds a tracing span to each request (by `request_span`)
/// - Logs the request depending on the request method (by `request_span`)
/// - Logs the response if it was not successful (by `request_span`
/// - Shields the handler function from async cancellations. Hyper can drop the handler
///   Future if the connection to the client is lost, but most of the pageserver code is
///   not async cancellation safe. This converts the dropped future into a graceful cancellation
///   request with a CancellationToken.
async fn api_handler<R, H>(request: Request<Body>, handler: H) -> Result<Response<Body>, ApiError>
where
    R: std::future::Future<Output = Result<Response<Body>, ApiError>> + Send + 'static,
    H: FnOnce(Request<Body>, CancellationToken) -> R + Send + Sync + 'static,
{
    if request.uri() != &"/v1/failpoints".parse::<Uri>().unwrap() {
        fail::fail_point!("api-503", |_| Err(ApiError::ResourceUnavailable(
            "failpoint".into()
        )));

        fail::fail_point!("api-500", |_| Err(ApiError::InternalServerError(
            anyhow::anyhow!("failpoint")
        )));
    }

    // Spawn a new task to handle the request, to protect the handler from unexpected
    // async cancellations. Most pageserver functions are not async cancellation safe.
    // We arm a drop-guard, so that if Hyper drops the Future, we signal the task
    // with the cancellation token.
    let token = CancellationToken::new();
    let cancel_guard = token.clone().drop_guard();
    let result = request_span(request, move |r| async {
        let handle = tokio::spawn(
            async {
                let token_cloned = token.clone();
                let result = handler(r, token).await;
                if token_cloned.is_cancelled() {
                    // dropguard has executed: we will never turn this result into response.
                    //
                    // at least temporarily do {:?} logging; these failures are rare enough but
                    // could hide difficult errors.
                    match &result {
                        Ok(response) => {
                            let status = response.status();
                            info!(%status, "Cancelled request finished successfully")
                        }
                        Err(e) => match e {
                            ApiError::ShuttingDown | ApiError::ResourceUnavailable(_) => {
                                // Don't log this at error severity: they are normal during lifecycle of tenants/process
                                info!("Cancelled request aborted for shutdown")
                            }
                            _ => {
                                // Log these in a highly visible way, because we have no client to send the response to, but
                                // would like to know that something went wrong.
                                error!("Cancelled request finished with an error: {e:?}")
                            }
                        },
                    }
                }
                // only logging for cancelled panicked request handlers is the tracing_panic_hook,
                // which should suffice.
                //
                // there is still a chance to lose the result due to race between
                // returning from here and the actual connection closing happening
                // before outer task gets to execute. leaving that up for #5815.
                result
            }
            .in_current_span(),
        );

        match handle.await {
            // TODO: never actually return Err from here, always Ok(...) so that we can log
            // spanned errors. Call api_error_handler instead and return appropriate Body.
            Ok(result) => result,
            Err(e) => {
                // The handler task panicked. We have a global panic handler that logs the
                // panic with its backtrace, so no need to log that here. Only log a brief
                // message to make it clear that we returned the error to the client.
                error!("HTTP request handler task panicked: {e:#}");

                // Don't return an Error here, because then fallback error handler that was
                // installed in make_router() will print the error. Instead, construct the
                // HTTP error response and return that.
                Ok(
                    ApiError::InternalServerError(anyhow!("HTTP request handler task panicked"))
                        .into_response(),
                )
            }
        }
    })
    .await;

    cancel_guard.disarm();

    result
}

/// Like api_handler, but returns an error response if the server is built without
/// the 'testing' feature.
async fn testing_api_handler<R, H>(
    desc: &str,
    request: Request<Body>,
    handler: H,
) -> Result<Response<Body>, ApiError>
where
    R: std::future::Future<Output = Result<Response<Body>, ApiError>> + Send + 'static,
    H: FnOnce(Request<Body>, CancellationToken) -> R + Send + Sync + 'static,
{
    if cfg!(feature = "testing") {
        api_handler(request, handler).await
    } else {
        std::future::ready(Err(ApiError::BadRequest(anyhow!(
            "Cannot {desc} because pageserver was compiled without testing APIs",
        ))))
        .await
    }
}

pub fn make_router(
    state: Arc<State>,
    launch_ts: &'static LaunchTimestamp,
    auth: Option<Arc<SwappableJwtAuth>>,
) -> anyhow::Result<RouterBuilder<hyper::Body, ApiError>> {
    let spec = include_bytes!("openapi_spec.yml");
    let mut router = attach_openapi_ui(endpoint::make_router(), spec, "/swagger.yml", "/v1/doc");
    if auth.is_some() {
        router = router.middleware(auth_middleware(|request| {
            let state = get_state(request);
            if state.allowlist_routes.contains(&request.uri().path()) {
                None
            } else {
                state.auth.as_deref()
            }
        }))
    }

    router = router.middleware(
        endpoint::add_response_header_middleware(
            "PAGESERVER_LAUNCH_TIMESTAMP",
            &launch_ts.to_string(),
        )
        .expect("construct launch timestamp header middleware"),
    );

    let force_metric_collection_on_scrape = state.conf.force_metric_collection_on_scrape;

    let prometheus_metrics_handler_wrapper =
        move |req| prometheus_metrics_handler(req, force_metric_collection_on_scrape);

    Ok(router
        .data(state)
        .get("/metrics", move |r| {
            request_span(r, prometheus_metrics_handler_wrapper)
        })
        .get("/profile/cpu", |r| request_span(r, profile_cpu_handler))
        .get("/profile/heap", |r| request_span(r, profile_heap_handler))
        .get("/v1/status", |r| api_handler(r, status_handler))
        .put("/v1/failpoints", |r| {
            testing_api_handler("manage failpoints", r, failpoints_handler)
        })
        .post("/v1/reload_auth_validation_keys", |r| {
            api_handler(r, reload_auth_validation_keys_handler)
        })
        .get("/v1/branch/:branch_id/timeline", |r| {
            api_handler(r, timeline_list_handler)
        })
        .get("/v1/branch/:branch_id", |r| {
            api_handler(r, timeline_detail_handler)
        })
        .get("/v1/branch/:branch_id/get_lsn_by_timestamp", |r| {
            api_handler(r, get_lsn_by_timestamp_handler)
        })
        .get("/v1/branch/:branch_id/get_timestamp_of_lsn", |r| {
            api_handler(r, get_timestamp_of_lsn_handler)
        })
        .post("/v1/branch/:branch_id/lsn_lease", |r| {
            api_handler(r, lsn_lease_handler)
        })
        .put("/v1/branch/:branch_id/do_gc", |r| {
            api_handler(r, timeline_gc_handler)
        })
        .put(
            "/v1/branch/:branch_id/flush",
            // Not testing-gated: the launcher calls this on the ancestor at
            // branch time (with `?lsn=`) to flush its pages to YB so the
            // branch's `create_branch_row` durability barrier clears
            // immediately (otherwise it waits up to one flush interval).
            |r| api_handler(r, timeline_flush_handler),
        )
        .get("/v1/panic", |r| api_handler(r, always_panic_handler))
        .post("/v1/tracing/event", |r| {
            testing_api_handler("emit a tracing event", r, post_tracing_event_handler)
        })
        .get("/v1/utilization", |r| api_handler(r, get_utilization))
        .any(handler_404))
}
