use std::collections::HashMap;
use std::fmt;
use std::io::Write as _;
use std::str::FromStr;
use std::sync::Arc;

use http_utils::endpoint::{
    self, ChannelWriter, auth_middleware, check_permission_with, profile_cpu_handler,
    profile_heap_handler, prometheus_metrics_handler, request_span,
};
use http_utils::error::ApiError;
use http_utils::failpoints::failpoints_handler;
use http_utils::json::{json_request, json_response};
use http_utils::request::{ensure_no_body, parse_query_param, parse_request_param};
use http_utils::{RequestExt, RouterBuilder};
use hyper::{Body, Request, Response, StatusCode};
use postgres_ffi::WAL_SEGMENT_SIZE;
use safekeeper_api::ServerInfo;
use safekeeper_api::models::{
    AcceptorStateStatus, SafekeeperStatus, TenantDeleteResult, TermSwitchApiEntry,
    TimelineCreateRequest, TimelineDeleteResult, TimelineStatus,
};
use tokio::sync::mpsc;
use tokio_stream::wrappers::ReceiverStream;
use tokio_util::sync::CancellationToken;
use tracing::info_span;
use utils::auth::SwappableJwtAuth;
use utils::id::{TenantId, TenantTimelineId, TimelineId};
use utils::lsn::Lsn;

use crate::debug_dump::TimelineDigestRequest;
use crate::disk_usage::{get_filesystem_capacity, get_filesystem_usage};
use crate::safekeeper::TermLsn;
use crate::timelines_global_map::DeleteOrExclude;
use crate::{GlobalTimelines, SafeKeeperConf, debug_dump, patch_control_file};
use serde_json::json;

/// Healthcheck handler.
async fn status_handler(request: Request<Body>) -> Result<Response<Body>, ApiError> {
    check_permission(&request, None)?;
    let conf = get_conf(&request);
    let status = SafekeeperStatus { id: conf.my_id };
    json_response(StatusCode::OK, status)
}

fn get_conf(request: &Request<Body>) -> &SafeKeeperConf {
    request
        .data::<Arc<SafeKeeperConf>>()
        .expect("unknown state type")
        .as_ref()
}

fn get_global_timelines(request: &Request<Body>) -> Arc<GlobalTimelines> {
    request
        .data::<Arc<GlobalTimelines>>()
        .expect("unknown state type")
        .clone()
}

fn check_permission(request: &Request<Body>, tenant_id: Option<TenantId>) -> Result<(), ApiError> {
    check_permission_with(request, |claims| {
        crate::auth::check_permission(claims, tenant_id)
    })
}

/// Deactivates all timelines for the tenant and removes its data directory.
/// See `timeline_delete_handler`.
async fn tenant_delete_handler(mut request: Request<Body>) -> Result<Response<Body>, ApiError> {
    let tenant_id = parse_request_param(&request, "tenant_id")?;
    let only_local = parse_query_param(&request, "only_local")?.unwrap_or(false);
    check_permission(&request, Some(tenant_id))?;
    ensure_no_body(&mut request).await?;
    let global_timelines = get_global_timelines(&request);
    let action = if only_local {
        DeleteOrExclude::DeleteLocal
    } else {
        DeleteOrExclude::Delete
    };
    let delete_info = global_timelines
        .delete_all_for_tenant(&tenant_id, action)
        .await
        .map_err(ApiError::InternalServerError)?;
    let response_body: TenantDeleteResult = delete_info
        .iter()
        .map(|(ttid, resp)| (format!("{}", ttid.timeline_id), *resp))
        .collect::<HashMap<String, TimelineDeleteResult>>();
    json_response(StatusCode::OK, response_body)
}

async fn timeline_create_handler(mut request: Request<Body>) -> Result<Response<Body>, ApiError> {
    let request_data: TimelineCreateRequest = json_request(&mut request).await?;

    let ttid = TenantTimelineId {
        tenant_id: request_data.tenant_id,
        timeline_id: request_data.timeline_id,
    };
    check_permission(&request, Some(ttid.tenant_id))?;

    let server_info = ServerInfo {
        pg_version: request_data.pg_version,
        system_id: request_data.system_id.unwrap_or(0),
        wal_seg_size: request_data.wal_seg_size.unwrap_or(WAL_SEGMENT_SIZE as u32),
    };
    let global_timelines = get_global_timelines(&request);
    global_timelines
        .create(
            ttid,
            request_data.mconf,
            server_info,
            request_data.start_lsn,
        )
        .await
        .map_err(ApiError::InternalServerError)?;

    json_response(StatusCode::OK, ())
}

async fn utilization_handler(request: Request<Body>) -> Result<Response<Body>, ApiError> {
    check_permission(&request, None)?;
    let global_timelines = get_global_timelines(&request);
    let utilization = global_timelines.get_timeline_counts();
    json_response(StatusCode::OK, utilization)
}

/// Returns filesystem capacity and current utilization for the safekeeper data directory.
async fn filesystem_usage_handler(request: Request<Body>) -> Result<Response<Body>, ApiError> {
    check_permission(&request, None)?;
    let conf = get_conf(&request);
    let path = conf.workdir.as_std_path();
    let capacity = get_filesystem_capacity(path).map_err(ApiError::InternalServerError)?;
    let usage = get_filesystem_usage(path);
    let resp = json!({
        "data_dir": path,
        "capacity_bytes": capacity,
        "usage_bytes": usage,
    });
    json_response(StatusCode::OK, resp)
}

/// List all (not deleted) timelines.
/// Note: it is possible to do the same with debug_dump.
async fn timeline_list_handler(request: Request<Body>) -> Result<Response<Body>, ApiError> {
    check_permission(&request, None)?;
    let global_timelines = get_global_timelines(&request);
    let res: Vec<TenantTimelineId> = global_timelines
        .get_all()
        .iter()
        .map(|tli| tli.ttid)
        .collect();
    json_response(StatusCode::OK, res)
}

impl From<TermSwitchApiEntry> for TermLsn {
    fn from(api_val: TermSwitchApiEntry) -> Self {
        TermLsn {
            term: api_val.term,
            lsn: api_val.lsn,
        }
    }
}

/// Report info about timeline.
async fn timeline_status_handler(request: Request<Body>) -> Result<Response<Body>, ApiError> {
    let ttid = TenantTimelineId::new(
        parse_request_param(&request, "tenant_id")?,
        parse_request_param(&request, "timeline_id")?,
    );
    check_permission(&request, Some(ttid.tenant_id))?;

    let global_timelines = get_global_timelines(&request);
    let tli = global_timelines.get(ttid).map_err(ApiError::from)?;
    let (inmem, state) = tli.get_state().await;
    let flush_lsn = tli.get_flush_lsn().await;

    let last_log_term = state.acceptor_state.get_last_log_term(flush_lsn);
    let term_history = state
        .acceptor_state
        .term_history
        .0
        .into_iter()
        .map(|ts| TermSwitchApiEntry {
            term: ts.term,
            lsn: ts.lsn,
        })
        .collect();
    let acc_state = AcceptorStateStatus {
        term: state.acceptor_state.term,
        epoch: last_log_term,
        term_history,
    };

    let conf = get_conf(&request);
    // Note: we report in memory values which can be lost.
    let status = TimelineStatus {
        tenant_id: ttid.tenant_id,
        timeline_id: ttid.timeline_id,
        mconf: state.mconf,
        acceptor_state: acc_state,
        pg_info: state.server,
        flush_lsn,
        timeline_start_lsn: state.timeline_start_lsn,
        local_start_lsn: state.local_start_lsn,
        // yb-light: commit_lsn ≡ flush_lsn (single-SK quorum).
        commit_lsn: flush_lsn,
        remote_consistent_lsn: inmem.remote_consistent_lsn,
        peers: tli.get_peers(conf).await,
        walsenders: tli.get_walsenders().get_all_public(),
        walreceivers: tli.get_walreceivers().get_all(),
    };
    json_response(StatusCode::OK, status)
}

/// Deactivates the timeline and removes its data directory.
async fn timeline_delete_handler(mut request: Request<Body>) -> Result<Response<Body>, ApiError> {
    let ttid = TenantTimelineId::new(
        parse_request_param(&request, "tenant_id")?,
        parse_request_param(&request, "timeline_id")?,
    );
    let only_local = parse_query_param(&request, "only_local")?.unwrap_or(false);
    check_permission(&request, Some(ttid.tenant_id))?;
    ensure_no_body(&mut request).await?;
    let global_timelines = get_global_timelines(&request);
    let action = if only_local {
        DeleteOrExclude::DeleteLocal
    } else {
        DeleteOrExclude::Delete
    };
    let resp = global_timelines
        .delete_or_exclude(&ttid, action)
        .await
        .map_err(ApiError::InternalServerError)?;
    json_response(StatusCode::OK, resp)
}

async fn patch_control_file_handler(
    mut request: Request<Body>,
) -> Result<Response<Body>, ApiError> {
    check_permission(&request, None)?;

    let ttid = TenantTimelineId::new(
        parse_request_param(&request, "tenant_id")?,
        parse_request_param(&request, "timeline_id")?,
    );

    let global_timelines = get_global_timelines(&request);
    let tli = global_timelines.get(ttid).map_err(ApiError::from)?;

    let patch_request: patch_control_file::Request = json_request(&mut request).await?;
    let response = patch_control_file::handle_request(tli, patch_request)
        .await
        .map_err(ApiError::InternalServerError)?;

    json_response(StatusCode::OK, response)
}

/// Force persist control file.
async fn timeline_checkpoint_handler(request: Request<Body>) -> Result<Response<Body>, ApiError> {
    check_permission(&request, None)?;

    let ttid = TenantTimelineId::new(
        parse_request_param(&request, "tenant_id")?,
        parse_request_param(&request, "timeline_id")?,
    );

    let global_timelines = get_global_timelines(&request);
    let tli = global_timelines.get(ttid)?;
    tli.write_shared_state()
        .await
        .sk
        .state_mut()
        .flush()
        .await
        .map_err(ApiError::InternalServerError)?;
    json_response(StatusCode::OK, ())
}

async fn timeline_digest_handler(request: Request<Body>) -> Result<Response<Body>, ApiError> {
    let ttid = TenantTimelineId::new(
        parse_request_param(&request, "tenant_id")?,
        parse_request_param(&request, "timeline_id")?,
    );
    check_permission(&request, Some(ttid.tenant_id))?;

    let global_timelines = get_global_timelines(&request);
    let from_lsn: Option<Lsn> = parse_query_param(&request, "from_lsn")?;
    let until_lsn: Option<Lsn> = parse_query_param(&request, "until_lsn")?;

    let request = TimelineDigestRequest {
        from_lsn: from_lsn.ok_or(ApiError::BadRequest(anyhow::anyhow!(
            "from_lsn is required"
        )))?,
        until_lsn: until_lsn.ok_or(ApiError::BadRequest(anyhow::anyhow!(
            "until_lsn is required"
        )))?,
    };

    let tli = global_timelines.get(ttid).map_err(ApiError::from)?;
    let tli = tli
        .wal_residence_guard()
        .await
        .map_err(ApiError::InternalServerError)?;

    let response = debug_dump::calculate_digest(&tli, request)
        .await
        .map_err(ApiError::InternalServerError)?;
    json_response(StatusCode::OK, response)
}

/// Unevict timeline and remove uploaded partial segment(s) from the remote storage.
/// Successfull response returns list of segments existed before the deletion.
/// Aimed for one-off usage not normally needed.
async fn timeline_backup_partial_reset(request: Request<Body>) -> Result<Response<Body>, ApiError> {
    let ttid = TenantTimelineId::new(
        parse_request_param(&request, "tenant_id")?,
        parse_request_param(&request, "timeline_id")?,
    );
    check_permission(&request, Some(ttid.tenant_id))?;

    let global_timelines = get_global_timelines(&request);
    let tli = global_timelines.get(ttid).map_err(ApiError::from)?;

    let response = tli
        .backup_partial_reset()
        .await
        .map_err(ApiError::InternalServerError)?;
    json_response(StatusCode::OK, response)
}

fn parse_kv_str<E: fmt::Display, T: FromStr<Err = E>>(k: &str, v: &str) -> Result<T, ApiError> {
    v.parse()
        .map_err(|e| ApiError::BadRequest(anyhow::anyhow!("cannot parse {k}: {e}")))
}

/// Dump debug info about all available safekeeper state.
async fn dump_debug_handler(mut request: Request<Body>) -> Result<Response<Body>, ApiError> {
    check_permission(&request, None)?;
    ensure_no_body(&mut request).await?;

    let mut dump_all: Option<bool> = None;
    let mut dump_control_file: Option<bool> = None;
    let mut dump_memory: Option<bool> = None;
    let mut dump_disk_content: Option<bool> = None;
    let mut dump_term_history: Option<bool> = None;
    let mut dump_wal_last_modified: Option<bool> = None;
    let mut tenant_id: Option<TenantId> = None;
    let mut timeline_id: Option<TimelineId> = None;

    let query = request.uri().query().unwrap_or("");
    let mut values = url::form_urlencoded::parse(query.as_bytes());

    for (k, v) in &mut values {
        match k.as_ref() {
            "dump_all" => dump_all = Some(parse_kv_str(&k, &v)?),
            "dump_control_file" => dump_control_file = Some(parse_kv_str(&k, &v)?),
            "dump_memory" => dump_memory = Some(parse_kv_str(&k, &v)?),
            "dump_disk_content" => dump_disk_content = Some(parse_kv_str(&k, &v)?),
            "dump_term_history" => dump_term_history = Some(parse_kv_str(&k, &v)?),
            "dump_wal_last_modified" => dump_wal_last_modified = Some(parse_kv_str(&k, &v)?),
            "tenant_id" => tenant_id = Some(parse_kv_str(&k, &v)?),
            "timeline_id" => timeline_id = Some(parse_kv_str(&k, &v)?),
            _ => Err(ApiError::BadRequest(anyhow::anyhow!(
                "Unknown query parameter: {}",
                k
            )))?,
        }
    }

    let dump_all = dump_all.unwrap_or(false);
    let dump_control_file = dump_control_file.unwrap_or(dump_all);
    let dump_memory = dump_memory.unwrap_or(dump_all);
    let dump_disk_content = dump_disk_content.unwrap_or(dump_all);
    let dump_term_history = dump_term_history.unwrap_or(true);
    let dump_wal_last_modified = dump_wal_last_modified.unwrap_or(dump_all);

    let global_timelines = get_global_timelines(&request);

    let args = debug_dump::Args {
        dump_all,
        dump_control_file,
        dump_memory,
        dump_disk_content,
        dump_term_history,
        dump_wal_last_modified,
        tenant_id,
        timeline_id,
    };

    let resp = debug_dump::build(args, global_timelines)
        .await
        .map_err(ApiError::InternalServerError)?;

    let started_at = std::time::Instant::now();

    let (tx, rx) = mpsc::channel(1);

    let body = Body::wrap_stream(ReceiverStream::new(rx));

    let mut writer = ChannelWriter::new(128 * 1024, tx);

    let response = Response::builder()
        .status(200)
        .header(hyper::header::CONTENT_TYPE, "application/octet-stream")
        .body(body)
        .unwrap();

    let span = info_span!("blocking");
    tokio::task::spawn_blocking(move || {
        let _span = span.entered();

        let res = serde_json::to_writer(&mut writer, &resp)
            .map_err(std::io::Error::from)
            .and_then(|_| writer.flush());

        match res {
            Ok(()) => {
                tracing::info!(
                    bytes = writer.flushed_bytes(),
                    elapsed_ms = started_at.elapsed().as_millis(),
                    "responded /v1/debug_dump"
                );
            }
            Err(e) => {
                tracing::warn!("failed to write out /v1/debug_dump response: {e:#}");
                // semantics of this error are quite... unclear. we want to error the stream out to
                // abort the response to somehow notify the client that we failed.
                //
                // though, most likely the reason for failure is that the receiver is already gone.
                drop(
                    writer
                        .tx
                        .blocking_send(Err(std::io::ErrorKind::BrokenPipe.into())),
                );
            }
        }
    });

    Ok(response)
}

/// Safekeeper http router.
pub fn make_router(
    conf: Arc<SafeKeeperConf>,
    global_timelines: Arc<GlobalTimelines>,
) -> RouterBuilder<hyper::Body, ApiError> {
    let mut router = endpoint::make_router();
    if conf.http_auth.is_some() {
        router = router.middleware(auth_middleware(|request| {
            const ALLOWLIST_ROUTES: &[&str] =
                &["/v1/status", "/metrics", "/profile/cpu", "/profile/heap"];
            if ALLOWLIST_ROUTES.contains(&request.uri().path()) {
                None
            } else {
                // Option<Arc<SwappableJwtAuth>> is always provided as data below, hence unwrap().
                request
                    .data::<Option<Arc<SwappableJwtAuth>>>()
                    .unwrap()
                    .as_deref()
            }
        }))
    }

    let force_metric_collection_on_scrape = conf.force_metric_collection_on_scrape;

    let prometheus_metrics_handler_wrapper =
        move |req| prometheus_metrics_handler(req, force_metric_collection_on_scrape);

    // NB: on any changes do not forget to update the OpenAPI spec
    // located nearby (/safekeeper/src/http/openapi_spec.yaml).
    let auth = conf.http_auth.clone();
    router
        .data(conf)
        .data(global_timelines)
        .data(auth)
        .get("/metrics", move |r| {
            request_span(r, prometheus_metrics_handler_wrapper)
        })
        .get("/profile/cpu", |r| request_span(r, profile_cpu_handler))
        .get("/profile/heap", |r| request_span(r, profile_heap_handler))
        .get("/v1/status", |r| request_span(r, status_handler))
        .put("/v1/failpoints", |r| {
            request_span(r, move |r| async {
                check_permission(&r, None)?;
                let cancel = CancellationToken::new();
                failpoints_handler(r, cancel).await
            })
        })
        .get("/v1/utilization", |r| request_span(r, utilization_handler))
        .get("/v1/debug/filesystem_usage", |r| {
            request_span(r, filesystem_usage_handler)
        })
        .delete("/v1/tenant/:tenant_id", |r| {
            request_span(r, tenant_delete_handler)
        })
        // Will be used in the future instead of implicit timeline creation
        .post("/v1/tenant/timeline", |r| {
            request_span(r, timeline_create_handler)
        })
        .get("/v1/tenant/timeline", |r| {
            request_span(r, timeline_list_handler)
        })
        .get("/v1/tenant/:tenant_id/timeline/:timeline_id", |r| {
            request_span(r, timeline_status_handler)
        })
        .delete("/v1/tenant/:tenant_id/timeline/:timeline_id", |r| {
            request_span(r, timeline_delete_handler)
        })
        .patch(
            "/v1/tenant/:tenant_id/timeline/:timeline_id/control_file",
            |r| request_span(r, patch_control_file_handler),
        )
        .post(
            "/v1/tenant/:tenant_id/timeline/:timeline_id/checkpoint",
            |r| request_span(r, timeline_checkpoint_handler),
        )
        .get("/v1/tenant/:tenant_id/timeline/:timeline_id/digest", |r| {
            request_span(r, timeline_digest_handler)
        })
        .post(
            "/v1/tenant/:tenant_id/timeline/:timeline_id/backup_partial_reset",
            |r| request_span(r, timeline_backup_partial_reset),
        )
        .get("/v1/debug_dump", |r| request_span(r, dump_debug_handler))
}

#[cfg(test)]
mod tests {
    use super::*;

    #[test]
    fn test_term_switch_entry_api_serialize() {
        let state = AcceptorStateStatus {
            term: 1,
            epoch: 1,
            term_history: vec![TermSwitchApiEntry {
                term: 1,
                lsn: Lsn(0x16FFDDDD),
            }],
        };
        let json = serde_json::to_string(&state).unwrap();
        assert_eq!(
            json,
            "{\"term\":1,\"epoch\":1,\"term_history\":[{\"term\":1,\"lsn\":\"0/16FFDDDD\"}]}"
        );
    }
}
