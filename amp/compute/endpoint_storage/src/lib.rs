use axum::extract::{FromRequestParts, Path};
use axum::response::{IntoResponse, Response};
use axum::{RequestPartsExt, http::StatusCode, http::request::Parts};
use camino::Utf8PathBuf;
use remote_storage::{GenericRemoteStorage, RemotePath};
use serde::{Deserialize, Serialize};
use std::fmt::Display;
use std::result::Result as StdResult;
use std::sync::Arc;
use tokio_util::sync::CancellationToken;
use tracing::{debug, error};
use utils::id::{BranchId, EndpointId};

fn normalize_key(key: &str) -> StdResult<Utf8PathBuf, String> {
    let key = clean_utf8(&Utf8PathBuf::from(key));
    if key.starts_with("..") || key == "." || key == "/" {
        return Err(format!("invalid key {key}"));
    }
    match key.strip_prefix("/").map(Utf8PathBuf::from) {
        Ok(p) => Ok(p),
        _ => Ok(key),
    }
}

// Copied from path_clean crate with PathBuf->Utf8PathBuf
fn clean_utf8(path: &camino::Utf8Path) -> Utf8PathBuf {
    use camino::Utf8Component as Comp;
    let mut out = Vec::new();
    for comp in path.components() {
        match comp {
            Comp::CurDir => (),
            Comp::ParentDir => match out.last() {
                Some(Comp::RootDir) => (),
                Some(Comp::Normal(_)) => {
                    out.pop();
                }
                None | Some(Comp::CurDir) | Some(Comp::ParentDir) | Some(Comp::Prefix(_)) => {
                    out.push(comp)
                }
            },
            comp => out.push(comp),
        }
    }
    if !out.is_empty() {
        out.iter().collect()
    } else {
        Utf8PathBuf::from(".")
    }
}

pub struct Storage {
    pub storage: GenericRemoteStorage,
    pub cancel: CancellationToken,
    pub max_upload_file_limit: usize,
}

#[derive(Deserialize, Serialize)]
struct KeyRequest {
    branch_id: BranchId,
    endpoint_id: EndpointId,
    path: String,
}

#[derive(Debug, PartialEq)]
pub struct S3Path {
    pub path: RemotePath,
}

impl TryFrom<&KeyRequest> for S3Path {
    type Error = String;
    fn try_from(req: &KeyRequest) -> StdResult<Self, Self::Error> {
        let KeyRequest {
            branch_id,
            endpoint_id,
            path,
        } = &req;
        let prefix = format!("{branch_id}/{endpoint_id}",);
        let path = Utf8PathBuf::from(prefix).join(normalize_key(path)?);
        let path = RemotePath::new(&path).unwrap(); // unwrap() because the path is already relative
        Ok(S3Path { path })
    }
}

pub fn bad_request(err: impl Display, desc: &'static str) -> Response {
    debug!(%err, desc);
    (StatusCode::BAD_REQUEST, err.to_string()).into_response()
}

pub fn ok() -> Response {
    StatusCode::OK.into_response()
}

pub fn internal_error(err: impl Display, path: impl Display, desc: &'static str) -> Response {
    error!(%err, %path, desc);
    StatusCode::INTERNAL_SERVER_ERROR.into_response()
}

pub fn not_found(key: impl ToString) -> Response {
    (StatusCode::NOT_FOUND, key.to_string()).into_response()
}

/// The route is the whole request: `{branch_id}/{endpoint_id}` is the prefix a
/// key is confined to, and [`normalize_key`] keeps the rest of the path from
/// escaping it.
impl FromRequestParts<Arc<Storage>> for S3Path {
    type Rejection = Response;
    async fn from_request_parts(
        parts: &mut Parts,
        _state: &Arc<Storage>,
    ) -> Result<Self, Self::Rejection> {
        let Path(path): Path<KeyRequest> = parts
            .extract()
            .await
            .map_err(|e| bad_request(e, "invalid route"))?;
        (&path)
            .try_into()
            .map_err(|e| bad_request(e, "invalid route"))
    }
}

#[cfg(test)]
mod tests {
    use super::*;

    #[test]
    fn normalize_key() {
        let f = super::normalize_key;
        assert_eq!(f("hello/world/..").unwrap(), Utf8PathBuf::from("hello"));
        assert_eq!(
            f("ololo/1/../../not_ololo").unwrap(),
            Utf8PathBuf::from("not_ololo")
        );
        assert!(f("ololo/1/../../../").is_err());
        assert!(f(".").is_err());
        assert!(f("../").is_err());
        assert!(f("").is_err());
        assert_eq!(f("/1/2/3").unwrap(), Utf8PathBuf::from("1/2/3"));
        assert!(f("/1/2/3/../../../").is_err());
        assert!(f("/1/2/3/../../../../").is_err());
    }

    const BRANCH_ID: BranchId =
        BranchId::from_array([1, 1, 2, 3, 4, 5, 6, 7, 8, 9, 1, 2, 3, 4, 5, 7]);
    const ENDPOINT_ID: &str = "ep-winter-frost-a662z3vg";

    #[test]
    fn s3_path() {
        let s3_path = |key| {
            let path = &format!("{BRANCH_ID}/{ENDPOINT_ID}/{key}");
            let path = RemotePath::from_string(path).unwrap();
            S3Path { path }
        };

        let mut key_path = KeyRequest {
            branch_id: BRANCH_ID,
            endpoint_id: ENDPOINT_ID.into(),
            path: "cache_key".to_string(),
        };
        assert_eq!(S3Path::try_from(&key_path).unwrap(), s3_path(key_path.path));

        key_path.path = "we/can/have/nested/paths".to_string();
        assert_eq!(S3Path::try_from(&key_path).unwrap(), s3_path(key_path.path));

        // A key cannot climb out of its endpoint's prefix.
        key_path.path = "../error/hello/../".to_string();
        assert!(S3Path::try_from(&key_path).is_err());
    }
}
