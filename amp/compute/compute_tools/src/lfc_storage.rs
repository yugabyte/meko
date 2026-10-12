//! The LFC state blob, read and written directly against YugabyteDB.
//!
//! LFC prewarm needs one blob per endpoint: the compressed page list Postgres
//! hands over on offload and loads back on prewarm. That blob lives in YB, behind
//! [`remote_storage`], which `compute_ctl` already links for remote extensions.
//!
//! The key layout is `{branch_id}/{endpoint_id}/lfc_state`. Its *shape* is
//! unchanged, but the branch component now spells the id as a uuid. That does not
//! strand old blobs: `YbStore::parse_key` splits a key into `(namespace,
//! branch_id uuid, key_body)` and strips the id out of the body, and
//! `Uuid::parse_str` reads both spellings to the same 16 bytes — so a blob
//! written under a flat token resolves to the same row when looked up under a
//! hyphenated one.
//!
//! Retries here rather than at the caller: a prewarm that fails because YB was
//! briefly unavailable would leave a cold cache and report `Failed`, which reads
//! as a bug in prewarm rather than a transient. The budget matches what the
//! storage layer's own callers use.

use anyhow::{Context, Result};
use bytes::Bytes;
use remote_storage::{
    DownloadError, DownloadOpts, GenericRemoteStorage, RemotePath, TimeoutOrCancel,
    TypedRemoteStorageKind,
};
use tokio::io::AsyncReadExt;
use tokio_util::sync::CancellationToken;
use tracing::info;
use utils::backoff::retry;

/// Warn once a call has failed this many times, but keep trying.
const WARN_THRESHOLD: u32 = 3;
const MAX_RETRIES: u32 = 10;

/// The one key an endpoint stores. A name, not a path component the caller
/// chooses: nothing else belongs under this prefix.
const LFC_STATE_KEY: &str = "lfc_state";

/// Refuse to upload beyond this. The state is a compressed page list, so a blob
/// this large means something is wrong upstream, and pushing it would put a
/// multi-hundred-megabyte row into YB.
const MAX_STATE_BYTES: usize = 256 * 1024 * 1024;

/// The blob store for one compute's LFC state.
pub struct LfcStorage {
    backend: Backend,
}

enum Backend {
    /// YugabyteDB directly, with the credentials the spec carries.
    Remote(GenericRemoteStorage),
    /// The supervisor's loopback service, for a compute that runs as its own
    /// user and holds no YugabyteDB credential. The supervisor fixes the branch,
    /// so only the endpoint id travels.
    Proxy {
        base_url: String,
        client: reqwest::Client,
    },
}

impl LfcStorage {
    /// Connects using the storage kind the spec carries.
    pub async fn new(kind: TypedRemoteStorageKind) -> Result<Self> {
        let storage = GenericRemoteStorage::from_storage_kind(kind)
            .await
            .context("connecting to the LFC state store")?;
        Ok(Self {
            backend: Backend::Remote(storage),
        })
    }

    pub fn proxy(base_url: &str) -> Result<Self> {
        let client = reqwest::Client::builder()
            .timeout(std::time::Duration::from_secs(120))
            .build()
            .context("building the LFC state client")?;
        Ok(Self {
            backend: Backend::Proxy {
                base_url: base_url.trim_end_matches('/').to_string(),
                client,
            },
        })
    }

    /// `{branch_id}/{endpoint_id}/lfc_state` — the same key the HTTP service
    /// derived from its route, so existing state stays addressable.
    ///
    /// Both ids are validated, not trusted. `from_endpoint` on
    /// `POST /lfc/prewarm` is a query parameter, so an id carrying `/` or `..`
    /// would let a caller address a key outside its own prefix and read another
    /// branch's state.
    pub fn key(branch_id: &str, endpoint_id: &str) -> Result<RemotePath> {
        let branch_id = checked_id(branch_id, "branch_id")?;
        let endpoint_id = checked_id(endpoint_id, "endpoint_id")?;
        let path = camino::Utf8PathBuf::from(format!("{branch_id}/{endpoint_id}/{LFC_STATE_KEY}"));
        RemotePath::new(&path).context("building the LFC state key")
    }

    /// The stored state, or `None` when this endpoint has never offloaded.
    ///
    /// A missing blob is the first-start case, not an error — the caller reports
    /// it as skipped rather than failed.
    pub async fn download(
        &self,
        key: &RemotePath,
        cancel: &CancellationToken,
    ) -> Result<Option<Bytes>> {
        let storage = match &self.backend {
            Backend::Remote(storage) => storage,
            Backend::Proxy { base_url, client } => {
                return proxy_download(client, &proxy_url(base_url, key)?, cancel).await;
            }
        };
        let opts = DownloadOpts::default();
        let download = retry(
            || async { storage.download(key, &opts, cancel).await },
            DownloadError::is_permanent,
            WARN_THRESHOLD,
            MAX_RETRIES,
            "downloading LFC state",
            cancel,
        )
        .await
        .unwrap_or(Err(DownloadError::Cancelled));

        let mut download = match download {
            Ok(d) => d,
            Err(DownloadError::NotFound) => {
                info!(%key, "no stored LFC state");
                return Ok(None);
            }
            Err(e) => return Err(e).context("downloading LFC state"),
        };

        let mut buf = Vec::new();
        let mut reader = tokio_util::io::StreamReader::new(&mut download.download_stream);
        reader
            .read_to_end(&mut buf)
            .await
            .context("reading the LFC state stream")?;
        Ok(Some(Bytes::from(buf)))
    }

    /// Replaces this endpoint's stored state.
    pub async fn upload(
        &self,
        key: &RemotePath,
        bytes: Bytes,
        cancel: &CancellationToken,
    ) -> Result<()> {
        let len = bytes.len();
        anyhow::ensure!(
            len <= MAX_STATE_BYTES,
            "LFC state is {len} bytes, over the {MAX_STATE_BYTES} limit"
        );
        let storage = match &self.backend {
            Backend::Remote(storage) => storage,
            Backend::Proxy { base_url, client } => {
                return proxy_upload(client, &proxy_url(base_url, key)?, bytes, cancel).await;
            }
        };
        retry(
            || async {
                let stream = futures::stream::once(futures::future::ready(Ok(bytes.clone())));
                storage.upload(stream, len, key, None, cancel).await
            },
            TimeoutOrCancel::caused_by_cancel,
            WARN_THRESHOLD,
            MAX_RETRIES,
            "uploading LFC state",
            cancel,
        )
        .await
        .unwrap_or(Err(anyhow::anyhow!("uploading LFC state was cancelled")))
        .context("uploading LFC state")
    }
}

/// `{base}/v1/lfc_state/{endpoint_id}`, from a key [`LfcStorage::key`] built and
/// so already validated.
fn proxy_url(base_url: &str, key: &RemotePath) -> Result<String> {
    let endpoint_id = key
        .get_path()
        .iter()
        .nth(1)
        .context("the LFC state key has no endpoint component")?;
    Ok(format!("{base_url}/v1/lfc_state/{endpoint_id}"))
}

async fn proxy_download(
    client: &reqwest::Client,
    url: &str,
    cancel: &CancellationToken,
) -> Result<Option<Bytes>> {
    match retry(
        || async {
            let resp = client.get(url).send().await?;
            if resp.status() == reqwest::StatusCode::NOT_FOUND {
                return Ok(None);
            }
            let resp = resp.error_for_status()?;
            Ok::<_, reqwest::Error>(Some(resp.bytes().await?))
        },
        |_| false,
        WARN_THRESHOLD,
        MAX_RETRIES,
        "downloading LFC state through the supervisor",
        cancel,
    )
    .await
    {
        Some(got) => got.context("downloading LFC state through the supervisor"),
        None => anyhow::bail!("downloading LFC state was cancelled"),
    }
}

async fn proxy_upload(
    client: &reqwest::Client,
    url: &str,
    bytes: Bytes,
    cancel: &CancellationToken,
) -> Result<()> {
    match retry(
        || async {
            client
                .put(url)
                .body(bytes.clone())
                .send()
                .await?
                .error_for_status()
                .map(|_| ())
        },
        |_| false,
        WARN_THRESHOLD,
        MAX_RETRIES,
        "uploading LFC state through the supervisor",
        cancel,
    )
    .await
    {
        Some(done) => done.context("uploading LFC state through the supervisor"),
        None => anyhow::bail!("uploading LFC state was cancelled"),
    }
}

/// An id safe to interpolate into a key: non-empty, bounded, and made only of
/// characters that cannot traverse or split a path. Branch and endpoint ids are
/// uuids, or `ep-`-prefixed names, so nothing legitimate is excluded by allowing
/// only alphanumerics, `-` and `_`.
fn checked_id<'a>(id: &'a str, what: &str) -> Result<&'a str> {
    anyhow::ensure!(!id.is_empty(), "{what} is empty");
    anyhow::ensure!(id.len() <= 128, "{what} is too long ({} bytes)", id.len());
    anyhow::ensure!(
        id.chars()
            .all(|c| c.is_ascii_alphanumeric() || c == '-' || c == '_'),
        "{what} {id:?} has characters that are not allowed in a storage key"
    );
    Ok(id)
}

#[cfg(test)]
mod tests {
    use super::*;

    /// The key must match what the HTTP service produced from its route, or an
    /// endpoint upgraded to this code cannot read back state it offloaded before.
    /// The branch id's spelling changed with it; see the module doc for why that
    /// does not strand a blob written under the old one.
    #[test]
    fn the_key_matches_the_previous_http_layout() {
        let k = LfcStorage::key("9a054903-242f-46bc-a0c9-cc6b91a37e4c", "ep-123").unwrap();
        assert_eq!(
            k.get_path().as_str(),
            "9a054903-242f-46bc-a0c9-cc6b91a37e4c/ep-123/lfc_state"
        );
    }

    /// The proxy is addressed by endpoint alone; the supervisor fixes the branch.
    #[test]
    fn the_proxy_url_carries_the_endpoint_id() {
        let k = LfcStorage::key("9a054903-242f-46bc-a0c9-cc6b91a37e4c", "ep-123").unwrap();
        assert_eq!(
            proxy_url("http://127.0.0.1:9896", &k).unwrap(),
            "http://127.0.0.1:9896/v1/lfc_state/ep-123"
        );
    }

    /// A key is confined to its own prefix. `from_endpoint` reaches this from a
    /// query parameter on `POST /lfc/prewarm`, so an id that can traverse or
    /// split the path would read another branch's state.
    #[test]
    fn a_traversing_id_is_refused() {
        for bad in [
            "..",
            "../..",
            "a/../b",
            "sibling/lfc_state",
            "",
            ".",
            "ep 1",
            "ep\u{0}1",
        ] {
            assert!(
                LfcStorage::key("branch", bad).is_err(),
                "accepted endpoint_id {bad:?}"
            );
            assert!(
                LfcStorage::key(bad, "ep").is_err(),
                "accepted branch_id {bad:?}"
            );
        }
    }

    /// The ids that actually occur must still be accepted: a uuid branch id and
    /// a uuid or `ep-`-prefixed endpoint id.
    #[test]
    fn real_ids_are_accepted() {
        for (b, e) in [
            (
                "9a054903-242f-46bc-a0c9-cc6b91a37e4c",
                "3f2b1c4e-0000-4000-8000-000000000001",
            ),
            (
                "9a054903-242f-46bc-a0c9-cc6b91a37e4c",
                "ep-quiet-forest-12345",
            ),
            ("9a054903-242f-46bc-a0c9-cc6b91a37e4c", "some_endpoint_1"),
        ] {
            LfcStorage::key(b, e).unwrap_or_else(|err| panic!("rejected {b}/{e}: {err}"));
        }
    }
}
