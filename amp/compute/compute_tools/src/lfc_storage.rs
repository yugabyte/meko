//! The LFC state blob, read and written directly against YugabyteDB.
//!
//! LFC prewarm needs one blob per endpoint: the compressed page list Postgres
//! hands over on offload and loads back on prewarm. That blob lives in YB, behind
//! [`remote_storage`], which `compute_ctl` already links for remote extensions.
//!
//! The key layout is `{branch_id}/{endpoint_id}/lfc_state`, unchanged, so a blob
//! written before this code existed is still the one read after it.
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
    storage: GenericRemoteStorage,
}

impl LfcStorage {
    /// Connects using the storage kind the spec carries.
    pub async fn new(kind: TypedRemoteStorageKind) -> Result<Self> {
        let storage = GenericRemoteStorage::from_storage_kind(kind)
            .await
            .context("connecting to the LFC state store")?;
        Ok(Self { storage })
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
        let opts = DownloadOpts::default();
        let download = retry(
            || async { self.storage.download(key, &opts, cancel).await },
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
        retry(
            || async {
                let stream = futures::stream::once(futures::future::ready(Ok(bytes.clone())));
                self.storage.upload(stream, len, key, None, cancel).await
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

/// An id safe to interpolate into a key: non-empty, bounded, and made only of
/// characters that cannot traverse or split a path. Branch ids are 32 hex digits
/// and endpoint ids are UUIDs or `ep-`-prefixed names, so nothing legitimate is
/// excluded by allowing only alphanumerics, `-` and `_`.
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
    #[test]
    fn the_key_matches_the_previous_http_layout() {
        let k = LfcStorage::key("9a054903242f46bca0c9cc6b91a37e4c", "ep-123").unwrap();
        assert_eq!(
            k.get_path().as_str(),
            "9a054903242f46bca0c9cc6b91a37e4c/ep-123/lfc_state"
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

    /// The ids that actually occur must still be accepted: a 32-hex branch id and
    /// a UUID or `ep-`-prefixed endpoint id.
    #[test]
    fn real_ids_are_accepted() {
        for (b, e) in [
            (
                "9a054903242f46bca0c9cc6b91a37e4c",
                "3f2b1c4e-0000-4000-8000-000000000001",
            ),
            ("9a054903242f46bca0c9cc6b91a37e4c", "ep-quiet-forest-12345"),
            ("9a054903242f46bca0c9cc6b91a37e4c", "some_endpoint_1"),
        ] {
            LfcStorage::key(b, e).unwrap_or_else(|err| panic!("rejected {b}/{e}: {err}"));
        }
    }
}
