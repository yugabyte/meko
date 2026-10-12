//! The LFC prewarm state, served to a compute that holds no YugabyteDB
//! credential.
//!
//! `compute_ctl` keeps one blob per endpoint, `{branch}/{endpoint}/lfc_state` in
//! `amp_storage.blobs`. With the users split it cannot reach YugabyteDB, so it
//! reads and writes the blob here instead, and this process does the storage
//! call with the storage credentials.
//!
//! A child of the supervisor running as the storage user (`endpoint-supervisor
//! serve-lfc-state`), not a thread of the supervisor: the compute's requests are
//! then parsed by a process with the pageserver's privileges rather than by root
//! with `SETUID`/`CHOWN`, and the supervisor restarts it like any other child. The branch is this endpoint's and never
//! comes from the request. A read may name another endpoint on the branch, which
//! `POST /lfc/prewarm?from_endpoint=` needs; a write may name only this one, or
//! the compute could replace a sibling's state with pages of its choosing.
//!
//! Bound to loopback only: the caller is the compute in this pod.
//!
//! Configured through the environment the supervisor gives it:
//! `LFC_STATE_PORT`, `LFC_STATE_BRANCH_ID`, `LFC_STATE_ENDPOINT_ID` (absent
//! refuses every write) and `LFC_STATE_STORAGE` (the `YbStore` connection as
//! JSON, naming the password variable rather than holding it).

use std::sync::Arc;

use anyhow::{Context, Result};
use bytes::Bytes;
use http_utils::endpoint;
use http_utils::error::ApiError;
use hyper0::{Body, Request, Response, StatusCode};
use remote_storage::{
    DownloadError, DownloadOpts, GenericRemoteStorage, RemotePath, TypedRemoteStorageKind,
};
use routerify::RouterBuilder;
use routerify::ext::RequestExt;
use tokio::io::AsyncReadExt;
use tokio::sync::OnceCell;
use tokio_util::sync::CancellationToken;

/// The cap `compute_ctl` applies before uploading: a compressed page list this
/// large means something is wrong upstream.
const MAX_STATE_BYTES: u64 = 256 * 1024 * 1024;

struct State {
    branch_id: String,
    /// The only id a write may name. `None` refuses every write, as the compute
    /// offloads nothing without an endpoint id.
    endpoint_id: Option<String>,
    kind: TypedRemoteStorageKind,
    /// Connected on first use: an endpoint that never prewarms never connects.
    storage: OnceCell<GenericRemoteStorage>,
}

impl State {
    async fn storage(&self) -> Result<&GenericRemoteStorage, ApiError> {
        self.storage
            .get_or_try_init(|| GenericRemoteStorage::from_storage_kind(self.kind.clone()))
            .await
            .map_err(|e| {
                ApiError::InternalServerError(e.context("connecting to the LFC state store"))
            })
    }

    fn key(&self, endpoint_id: &str) -> Result<RemotePath, ApiError> {
        let ok = !endpoint_id.is_empty()
            && endpoint_id.len() <= 128
            && endpoint_id
                .chars()
                .all(|c| c.is_ascii_alphanumeric() || c == '-' || c == '_');
        if !ok {
            return Err(ApiError::BadRequest(anyhow::anyhow!(
                "endpoint id {endpoint_id:?} is not allowed in a storage key"
            )));
        }
        let path = camino::Utf8PathBuf::from(format!("{}/{endpoint_id}/lfc_state", self.branch_id));
        RemotePath::new(&path).map_err(ApiError::InternalServerError)
    }
}

fn endpoint_id(req: &Request<Body>) -> String {
    req.param("endpoint_id").cloned().unwrap_or_default()
}

async fn get_handler(req: Request<Body>) -> Result<Response<Body>, ApiError> {
    let state = req.data::<Arc<State>>().expect("state").clone();
    let key = state.key(&endpoint_id(&req))?;
    let storage = state.storage().await?;
    let cancel = CancellationToken::new();
    let mut download = match storage
        .download(&key, &DownloadOpts::default(), &cancel)
        .await
    {
        Ok(d) => d,
        Err(DownloadError::NotFound) => {
            return Err(ApiError::NotFound(
                anyhow::anyhow!("no stored LFC state").into(),
            ));
        }
        Err(e) => {
            return Err(ApiError::InternalServerError(
                anyhow::Error::new(e).context("downloading LFC state"),
            ));
        }
    };
    let mut buf = Vec::new();
    tokio_util::io::StreamReader::new(&mut download.download_stream)
        .read_to_end(&mut buf)
        .await
        .map_err(|e| {
            ApiError::InternalServerError(anyhow::Error::new(e).context("reading LFC state"))
        })?;
    Ok(Response::builder()
        .status(StatusCode::OK)
        .body(Body::from(buf))
        .expect("static response"))
}

async fn put_handler(mut req: Request<Body>) -> Result<Response<Body>, ApiError> {
    let state = req.data::<Arc<State>>().expect("state").clone();
    let id = endpoint_id(&req);
    let key = state.key(&id)?;
    if state.endpoint_id.as_deref() != Some(id.as_str()) {
        return Err(ApiError::Forbidden(format!(
            "endpoint {id:?} is not this endpoint; only its own LFC state is writable"
        )));
    }
    let body = read_capped(req.body_mut()).await?;
    let len = body.len();
    let storage = state.storage().await?;
    let cancel = CancellationToken::new();
    let stream = futures::stream::once(futures::future::ready(Ok(body)));
    storage
        .upload(stream, len, &key, None, &cancel)
        .await
        .map_err(|e| ApiError::InternalServerError(e.context("uploading LFC state")))?;
    Ok(Response::builder()
        .status(StatusCode::OK)
        .body(Body::empty())
        .expect("static response"))
}

/// The request body, refused past [`MAX_STATE_BYTES`] as it streams in rather
/// than after it is buffered: the declared length is the caller's to choose.
async fn read_capped(body: &mut Body) -> Result<Bytes, ApiError> {
    use hyper0::body::HttpBody;
    let mut buf = Vec::new();
    while let Some(chunk) = body.data().await {
        let chunk = chunk.map_err(|e| ApiError::BadRequest(anyhow::Error::new(e)))?;
        if (buf.len() + chunk.len()) as u64 > MAX_STATE_BYTES {
            return Err(ApiError::BadRequest(anyhow::anyhow!(
                "LFC state over the {MAX_STATE_BYTES}-byte limit"
            )));
        }
        buf.extend_from_slice(&chunk);
    }
    Ok(Bytes::from(buf))
}

fn make_router(state: Arc<State>) -> RouterBuilder<Body, ApiError> {
    endpoint::make_router()
        .data(state)
        .get("/v1/lfc_state/:endpoint_id", |r| {
            endpoint::request_span(r, get_handler)
        })
        .put("/v1/lfc_state/:endpoint_id", |r| {
            endpoint::request_span(r, put_handler)
        })
}

/// The argument that runs this binary as the LFC state server.
pub const SUBCOMMAND: &str = "serve-lfc-state";

/// The child's entry point. Returns only on failure, so the process exits
/// non-zero and the supervisor restarts it.
pub fn run_from_env() -> Result<std::convert::Infallible> {
    let port: u16 = crate::env_opt("LFC_STATE_PORT")
        .context("LFC_STATE_PORT is required")?
        .parse()
        .context("LFC_STATE_PORT is not a port")?;
    let branch_id =
        crate::env_opt("LFC_STATE_BRANCH_ID").context("LFC_STATE_BRANCH_ID is required")?;
    let endpoint_id = crate::env_opt("LFC_STATE_ENDPOINT_ID");
    let kind = serde_json::from_str(
        &crate::env_opt("LFC_STATE_STORAGE").context("LFC_STATE_STORAGE is required")?,
    )
    .context("LFC_STATE_STORAGE is not a storage connection")?;
    let listener = bind(std::net::SocketAddr::from(([127, 0, 0, 1], port)))?;
    serve(listener, branch_id, endpoint_id, kind)
}

fn bind(addr: std::net::SocketAddr) -> Result<std::net::TcpListener> {
    anyhow::ensure!(
        addr.ip().is_loopback(),
        "the LFC state service must bind loopback, not {addr}"
    );
    std::net::TcpListener::bind(addr).with_context(|| format!("bind {addr}"))
}

/// Serves until the server fails, which is always an error: nothing stops it on
/// purpose but a signal, and that ends the process.
fn serve(
    listener: std::net::TcpListener,
    branch_id: String,
    endpoint_id: Option<String>,
    kind: TypedRemoteStorageKind,
) -> Result<std::convert::Infallible> {
    let addr = listener.local_addr()?;
    let state = Arc::new(State {
        branch_id,
        endpoint_id,
        kind,
        storage: OnceCell::new(),
    });
    let router = make_router(state)
        .build()
        .map_err(|e| anyhow::anyhow!("building the LFC state router: {e}"))?;
    let service = routerify::RequestServiceBuilder::new(router)
        .map_err(|e| anyhow::anyhow!("building the LFC state request service: {e}"))?;
    let rt = tokio::runtime::Builder::new_current_thread()
        .enable_all()
        .build()
        .context("building the LFC state service's runtime")?;
    rt.block_on(async move {
        let server = http_utils::server::Server::new(Arc::new(service), listener, None)?;
        tracing::info!(%addr, "LFC state service listening");
        server.serve(CancellationToken::new()).await
    })?;
    anyhow::bail!("the LFC state service stopped")
}

#[cfg(test)]
mod tests {
    use super::*;

    fn state() -> State {
        State {
            branch_id: "9a054903-242f-46bc-a0c9-cc6b91a37e4c".into(),
            endpoint_id: Some("ep-123".into()),
            kind: serde_json::from_value(serde_json::json!({
                "type": "YbStore", "host_ports": "127.0.0.1:1", "user": "u", "dbname": "d"
            }))
            .unwrap(),
            storage: OnceCell::new(),
        }
    }

    /// The key layout `compute_ctl` uses, or an endpoint switched to the proxy
    /// cannot read back the state it offloaded before.
    #[test]
    fn the_key_matches_compute_ctl_s_layout() {
        let k = state().key("ep-123").unwrap();
        assert_eq!(
            k.get_path().as_str(),
            "9a054903-242f-46bc-a0c9-cc6b91a37e4c/ep-123/lfc_state"
        );
    }

    /// The branch is fixed here, so the only way out of it is a traversing id.
    #[test]
    fn a_traversing_endpoint_id_is_refused() {
        for bad in ["..", "a/../b", "x/lfc_state", "", "ep 1", "ep\u{0}1"] {
            assert!(state().key(bad).is_err(), "accepted {bad:?}");
        }
    }

    /// Sends one raw request to a freshly spawned service and returns the reply.
    fn ask(own: Option<&str>, request: &str) -> String {
        use std::io::{Read, Write};
        let kind = state().kind;
        let listener = bind("127.0.0.1:0".parse().unwrap()).unwrap();
        let addr = listener.local_addr().unwrap();
        let own = own.map(String::from);
        std::thread::spawn(move || serve(listener, "b".into(), own, kind));
        let mut reply = String::new();
        for _ in 0..50 {
            if let Ok(mut s) = std::net::TcpStream::connect(addr) {
                s.write_all(request.as_bytes()).unwrap();
                s.read_to_string(&mut reply).unwrap();
                break;
            }
            std::thread::sleep(std::time::Duration::from_millis(20));
        }
        reply
    }

    /// The service has to come up and route, which compiling does not prove. A
    /// bad id is answered before any storage call, so this needs no YugabyteDB.
    #[test]
    fn the_service_answers_once_spawned() {
        let reply = ask(
            Some("ep-1"),
            "GET /v1/lfc_state/bad%20id HTTP/1.1\r\nHost: x\r\nConnection: close\r\n\r\n",
        );
        assert!(reply.starts_with("HTTP/1.1 400"), "{reply}");
    }

    /// Refused before any storage call, so this too needs no YugabyteDB.
    #[test]
    fn a_write_to_another_endpoint_is_forbidden() {
        let put = |id: &str| {
            format!(
                "PUT /v1/lfc_state/{id} HTTP/1.1\r\nHost: x\r\nContent-Length: 1\r\n\
                 Connection: close\r\n\r\nx"
            )
        };
        let reply = ask(Some("ep-1"), &put("ep-2"));
        assert!(reply.starts_with("HTTP/1.1 403"), "{reply}");
        let reply = ask(None, &put("ep-1"));
        assert!(reply.starts_with("HTTP/1.1 403"), "{reply}");
    }

    #[test]
    fn only_loopback_binds() {
        let err = bind("0.0.0.0:0".parse().unwrap()).unwrap_err();
        assert!(err.to_string().contains("loopback"), "{err}");
    }
}
