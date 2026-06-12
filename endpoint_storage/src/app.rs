use anyhow::anyhow;
use axum::body::{Body, Bytes};
use axum::response::{IntoResponse, Response};
use axum::{Router, http::StatusCode};
use endpoint_storage::{PrefixS3Path, S3Path, Storage, bad_request, internal_error, not_found, ok};
use remote_storage::TimeoutOrCancel;
use remote_storage::{DownloadError, DownloadOpts};
use std::sync::Arc;
use tracing::info;
use utils::backoff::retry;

pub fn app(state: Arc<Storage>) -> Router<()> {
    use axum::routing::{delete as _delete, get as _get};
    let delete_prefix = _delete(delete_prefix);
    // NB: On any changes do not forget to update the OpenAPI spec
    // in /endpoint_storage/src/openapi_spec.yml.
    Router::new()
        .route(
            "/{tenant_id}/{timeline_id}/{endpoint_id}/{*path}",
            _get(get).put(set).delete(delete),
        )
        .route(
            "/{tenant_id}/{timeline_id}/{endpoint_id}",
            delete_prefix.clone(),
        )
        .route("/{tenant_id}/{timeline_id}", delete_prefix.clone())
        .route("/{tenant_id}", delete_prefix)
        .route("/metrics", _get(metrics))
        .route("/status", _get(async || StatusCode::OK.into_response()))
        .with_state(state)
}

type Result = anyhow::Result<Response, Response>;
type State = axum::extract::State<Arc<Storage>>;

const CONTENT_TYPE: &str = "content-type";
const APPLICATION_OCTET_STREAM: &str = "application/octet-stream";
const WARN_THRESHOLD: u32 = 3;
const MAX_RETRIES: u32 = 10;

async fn metrics() -> Result {
    prometheus::TextEncoder::new()
        .encode_to_string(&prometheus::gather())
        .map(|s| s.into_response())
        .map_err(|e| internal_error(e, "/metrics", "collecting metrics"))
}

async fn get(S3Path { path }: S3Path, state: State) -> Result {
    info!(%path, "downloading");
    let download_err = |err| {
        if let DownloadError::NotFound = err {
            info!(%path, %err, "downloading"); // 404 is not an issue of _this_ service
            return not_found(&path);
        }
        internal_error(err, &path, "downloading")
    };
    let cancel = state.cancel.clone();
    let opts = &DownloadOpts::default();

    let stream = retry(
        async || state.storage.download(&path, opts, &cancel).await,
        DownloadError::is_permanent,
        WARN_THRESHOLD,
        MAX_RETRIES,
        "downloading",
        &cancel,
    )
    .await
    .unwrap_or(Err(DownloadError::Cancelled))
    .map_err(download_err)?
    .download_stream;

    Response::builder()
        .status(StatusCode::OK)
        .header(CONTENT_TYPE, APPLICATION_OCTET_STREAM)
        .body(Body::from_stream(stream))
        .map_err(|e| internal_error(e, path, "reading response"))
}

// Best solution for files is multipart upload, but remote_storage doesn't support it,
// so we can either read Bytes in memory and push at once or forward BodyDataStream to
// remote_storage. The latter may seem more peformant, but BodyDataStream doesn't have a
// guaranteed size() which may produce issues while uploading to s3.
// So, currently we're going with an in-memory copy plus a boundary to prevent uploading
// very large files.
async fn set(S3Path { path }: S3Path, state: State, bytes: Bytes) -> Result {
    info!(%path, "uploading");
    let request_len = bytes.len();
    let max_len = state.max_upload_file_limit;
    if request_len > max_len {
        return Err(bad_request(
            anyhow!("File size {request_len} exceeds max {max_len}"),
            "uploading",
        ));
    }

    let cancel = state.cancel.clone();
    let fun = async || {
        let stream = bytes_to_stream(bytes.clone());
        state
            .storage
            .upload(stream, request_len, &path, None, &cancel)
            .await
    };
    retry(
        fun,
        TimeoutOrCancel::caused_by_cancel,
        WARN_THRESHOLD,
        MAX_RETRIES,
        "uploading",
        &cancel,
    )
    .await
    .unwrap_or(Err(anyhow!("uploading cancelled")))
    .map_err(|e| internal_error(e, path, "reading response"))?;
    Ok(ok())
}

async fn delete(S3Path { path }: S3Path, state: State) -> Result {
    info!(%path, "deleting");
    let cancel = state.cancel.clone();
    retry(
        async || state.storage.delete(&path, &cancel).await,
        TimeoutOrCancel::caused_by_cancel,
        WARN_THRESHOLD,
        MAX_RETRIES,
        "deleting",
        &cancel,
    )
    .await
    .unwrap_or(Err(anyhow!("deleting cancelled")))
    .map_err(|e| internal_error(e, path, "deleting"))?;
    Ok(ok())
}

async fn delete_prefix(PrefixS3Path { path }: PrefixS3Path, state: State) -> Result {
    info!(%path, "deleting prefix");
    let cancel = state.cancel.clone();
    retry(
        async || state.storage.delete_prefix(&path, &cancel).await,
        TimeoutOrCancel::caused_by_cancel,
        WARN_THRESHOLD,
        MAX_RETRIES,
        "deleting prefix",
        &cancel,
    )
    .await
    .unwrap_or(Err(anyhow!("deleting prefix cancelled")))
    .map_err(|e| internal_error(e, path, "deleting prefix"))?;
    Ok(ok())
}

fn bytes_to_stream(bytes: Bytes) -> impl futures::Stream<Item = std::io::Result<Bytes>> {
    futures::stream::once(futures::future::ready(Ok(bytes)))
}
