//! A set of generic storage abstractions for the page server to use when backing up and restoring its state from the external storage.
//! No other modules from this tree are supposed to be used directly by the external code.
//!
//! [`RemoteStorage`] trait a CRUD-like generic abstraction over external storages:
//!   * [`yb_store`] uses a YugabyteDB table over the Postgres wire protocol
//!     (the only production / dev backend for yb-pageserver)
//!
//! Cloud-storage backends (S3, Azure Blob, GCS) were removed in lane E
//! along with their integration tests and SDK dependencies — yb-light
//! never reads or writes to a cloud object store. The dev-only LocalFs
//! backend was dropped subsequently; everything writes through YbStore.
//!
#![deny(unsafe_code)]
#![deny(clippy::undocumented_unsafe_blocks)]

mod config;
mod error;
mod metrics;
mod simulate_failures;
mod support;
mod yb_store;

pub use ysql_client::{YbConnConfig, YsqlClient, build_yb_rustls_connector};

use std::collections::HashMap;
use std::fmt::Debug;
use std::num::NonZeroU32;
use std::ops::Bound;
use std::pin::{Pin, pin};
use std::sync::Arc;
use std::time::SystemTime;

use anyhow::Context;
use bytes::Bytes;
use camino::{Utf8Path, Utf8PathBuf};
pub use config::TypedRemoteStorageKind;
pub use error::{DownloadError, TimeTravelError, TimeoutOrCancel};
use futures::StreamExt;
use futures::stream::Stream;
use itertools::Itertools as _;
use metrics::RequestKind;
use serde::{Deserialize, Serialize};
use tokio::sync::Semaphore;
use tokio_util::sync::CancellationToken;
use tracing::info;

pub use self::simulate_failures::UnreliableWrapper;
pub use self::yb_store::YbStore;
pub use crate::config::{RemoteStorageConfig, RemoteStorageKind};

/// Entity tag — opaque string identifying a particular object
/// version, returned with downloads and head responses. We used to
/// re-export `azure_core::Etag` here; that was the only consumer of
/// the azure_core dependency. After lane E we own the type.
#[derive(Debug, Clone, PartialEq, Eq)]
pub struct Etag(pub String);

impl From<String> for Etag {
    fn from(s: String) -> Self {
        Etag(s)
    }
}

impl From<&str> for Etag {
    fn from(s: &str) -> Self {
        Etag(s.to_string())
    }
}

impl std::fmt::Display for Etag {
    fn fmt(&self, f: &mut std::fmt::Formatter<'_>) -> std::fmt::Result {
        std::fmt::Display::fmt(&self.0, f)
    }
}

impl AsRef<str> for Etag {
    fn as_ref(&self) -> &str {
        &self.0
    }
}

/// Default concurrency cap for backends that don't enforce one themselves.
///
/// Also bounds the upload queue concurrency. Some tests create thousands of
/// uploads, which slows down the quadratic scheduling of the upload
/// queue, and there's no point spawning so many Tokio tasks.
pub const DEFAULT_REMOTE_STORAGE_CONCURRENCY_LIMIT: usize = 100;

/// No client-side limit by default.
pub const DEFAULT_MAX_KEYS_PER_LIST_RESPONSE: Option<i32> = None;

const REMOTE_STORAGE_PREFIX_SEPARATOR: char = '/';

/// Path on the remote storage, relative to some inner prefix.
/// The prefix is an implementation detail, that allows representing local paths
/// as the remote ones, stripping the local storage prefix away.
#[derive(Debug, Clone, PartialEq, Eq, PartialOrd, Ord, Hash)]
pub struct RemotePath(Utf8PathBuf);

impl Serialize for RemotePath {
    fn serialize<S>(&self, serializer: S) -> Result<S::Ok, S::Error>
    where
        S: serde::Serializer,
    {
        serializer.collect_str(self)
    }
}

impl<'de> Deserialize<'de> for RemotePath {
    fn deserialize<D>(deserializer: D) -> Result<Self, D::Error>
    where
        D: serde::Deserializer<'de>,
    {
        let str = String::deserialize(deserializer)?;
        Ok(Self(Utf8PathBuf::from(&str)))
    }
}

impl std::fmt::Display for RemotePath {
    fn fmt(&self, f: &mut std::fmt::Formatter<'_>) -> std::fmt::Result {
        std::fmt::Display::fmt(&self.0, f)
    }
}

impl RemotePath {
    pub fn new(relative_path: &Utf8Path) -> anyhow::Result<Self> {
        anyhow::ensure!(
            relative_path.is_relative(),
            "Path {relative_path:?} is not relative"
        );
        Ok(Self(relative_path.to_path_buf()))
    }

    pub fn from_string(relative_path: &str) -> anyhow::Result<Self> {
        Self::new(Utf8Path::new(relative_path))
    }

    pub fn with_base(&self, base_path: &Utf8Path) -> Utf8PathBuf {
        base_path.join(&self.0)
    }

    pub fn object_name(&self) -> Option<&str> {
        self.0.file_name()
    }

    pub fn join(&self, path: impl AsRef<Utf8Path>) -> Self {
        Self(self.0.join(path))
    }

    pub fn get_path(&self) -> &Utf8PathBuf {
        &self.0
    }

    pub fn strip_prefix(&self, p: &RemotePath) -> Result<&Utf8Path, std::path::StripPrefixError> {
        self.0.strip_prefix(&p.0)
    }

    pub fn add_trailing_slash(&self) -> Self {
        // Unwrap safety inputs are guararnteed to be valid UTF-8
        Self(format!("{}/", self.0).try_into().unwrap())
    }
}

/// We don't need callers to be able to pass arbitrary delimiters: just control
/// whether listings will use a '/' separator or not.
///
/// The WithDelimiter mode will populate `prefixes` and `keys` in the result.  The
/// NoDelimiter mode will only populate `keys`.
#[derive(Copy, Clone)]
pub enum ListingMode {
    WithDelimiter,
    NoDelimiter,
}

#[derive(PartialEq, Eq, Debug, Clone)]
pub struct ListingObject {
    pub key: RemotePath,
    pub last_modified: SystemTime,
    pub size: u64,
}

#[derive(Default)]
pub struct Listing {
    pub prefixes: Vec<RemotePath>,
    pub keys: Vec<ListingObject>,
}

#[derive(Default)]
pub struct VersionListing {
    pub versions: Vec<Version>,
}

#[derive(Debug)]
pub struct Version {
    pub key: RemotePath,
    pub last_modified: SystemTime,
    pub kind: VersionKind,
}

impl Version {
    pub fn version_id(&self) -> Option<&VersionId> {
        match &self.kind {
            VersionKind::Version(id) => Some(id),
            VersionKind::DeletionMarker => None,
        }
    }
}

#[derive(Debug)]
pub enum VersionKind {
    DeletionMarker,
    Version(VersionId),
}

// I was going to do an `enum GenericVersion` but this feels cleaner.
#[derive(Default)]
pub struct GCSVersionListing {
    pub versions: Vec<GCSVersion>,
}

#[derive(Debug)]
pub struct GCSVersion {
    pub key: RemotePath,
    pub last_modified: SystemTime,
    pub id: VersionId,
    pub time_deleted: Option<SystemTime>,
}

impl From<GCSVersionListing> for VersionListing {
    fn from(gcs_listing: GCSVersionListing) -> Self {
        let version_listing = gcs_listing
            .versions
            .into_iter()
            .map(
                |GCSVersion {
                     key,
                     last_modified,
                     id,
                     ..
                 }| {
                    Version {
                        key,
                        last_modified,
                        kind: VersionKind::Version(VersionId(id.0)),
                    }
                },
            )
            .collect::<Vec<Version>>();

        VersionListing {
            versions: version_listing,
        }
    }
}

/// Options for downloads. The default value is a plain GET.
pub struct DownloadOpts {
    /// If given, returns [`DownloadError::Unmodified`] if the object still has
    /// the same ETag (using If-None-Match).
    pub etag: Option<Etag>,
    /// The start of the byte range to download, or unbounded.
    pub byte_start: Bound<u64>,
    /// The end of the byte range to download, or unbounded. Must be after the
    /// start bound.
    pub byte_end: Bound<u64>,
    /// Optionally request a specific version of a key
    pub version_id: Option<VersionId>,
    /// Indicate whether we're downloading something small or large: this indirectly controls
    /// timeouts: for something like an index/manifest/heatmap, we should time out faster than
    /// for layer files
    pub kind: DownloadKind,
}

pub enum DownloadKind {
    Large,
    Small,
}

#[derive(Debug, Clone)]
pub struct VersionId(pub String);

impl Default for DownloadOpts {
    fn default() -> Self {
        Self {
            etag: Default::default(),
            byte_start: Bound::Unbounded,
            byte_end: Bound::Unbounded,
            version_id: None,
            kind: DownloadKind::Large,
        }
    }
}

impl DownloadOpts {
    /// Returns the byte range with inclusive start and exclusive end, or None
    /// if unbounded.
    pub fn byte_range(&self) -> Option<(u64, Option<u64>)> {
        if self.byte_start == Bound::Unbounded && self.byte_end == Bound::Unbounded {
            return None;
        }
        let start = match self.byte_start {
            Bound::Excluded(i) => i + 1,
            Bound::Included(i) => i,
            Bound::Unbounded => 0,
        };
        let end = match self.byte_end {
            Bound::Excluded(i) => Some(i),
            Bound::Included(i) => Some(i + 1),
            Bound::Unbounded => None,
        };
        if let Some(end) = end {
            assert!(start < end, "range end {end} at or before start {start}");
        }
        Some((start, end))
    }

    /// Returns the byte range as an RFC 2616 Range header value with inclusive
    /// bounds, or None if unbounded.
    pub fn byte_range_header(&self) -> Option<String> {
        self.byte_range()
            .map(|(start, end)| (start, end.map(|end| end - 1))) // make end inclusive
            .map(|(start, end)| match end {
                Some(end) => format!("bytes={start}-{end}"),
                None => format!("bytes={start}-"),
            })
    }
}

/// Storage (potentially remote) API to manage its state.
/// This storage tries to be unaware of any layered repository context,
/// providing basic CRUD operations for storage files.
#[allow(async_fn_in_trait)]
pub trait RemoteStorage: Send + Sync + 'static {
    /// List objects in remote storage, with semantics matching AWS S3's [`ListObjectsV2`].
    ///
    /// The stream is guaranteed to return at least one element, even in the case of errors
    /// (in that case it's an `Err()`), or an empty `Listing`.
    ///
    /// The stream is not ending if it returns an error, as long as [`is_permanent`] returns false on the error.
    /// The `next` function can be retried, and maybe in a future retry, there will be success.
    ///
    /// Note that the prefix is relative to any `prefix_in_bucket` configured for the client, not
    /// from the absolute root of the bucket.
    ///
    /// `mode` configures whether to use a delimiter.  Without a delimiter, all keys
    /// within the prefix are listed in the `keys` of the result.  With a delimiter, any "directories" at the top level of
    /// the prefix are returned in the `prefixes` of the result, and keys in the top level of the prefix are
    /// returned in `keys` ().
    ///
    /// `max_keys` controls the maximum number of keys that will be returned.  If this is None, this function
    /// will iteratively call listobjects until it runs out of keys.  Note that this is not safe to use on
    /// unlimted size buckets, as the full list of objects is allocated into a monolithic data structure.
    ///
    /// [`ListObjectsV2`]: <https://docs.aws.amazon.com/AmazonS3/latest/API/API_ListObjectsV2.html>
    /// [`is_permanent`]: DownloadError::is_permanent
    fn list_streaming(
        &self,
        prefix: Option<&RemotePath>,
        mode: ListingMode,
        max_keys: Option<NonZeroU32>,
        cancel: &CancellationToken,
    ) -> impl Stream<Item = Result<Listing, DownloadError>> + Send;

    async fn list(
        &self,
        prefix: Option<&RemotePath>,
        mode: ListingMode,
        max_keys: Option<NonZeroU32>,
        cancel: &CancellationToken,
    ) -> Result<Listing, DownloadError> {
        let mut stream = pin!(self.list_streaming(prefix, mode, max_keys, cancel));
        let mut combined = stream.next().await.expect("At least one item required")?;
        while let Some(list) = stream.next().await {
            let list = list?;
            combined.keys.extend(list.keys.into_iter());
            combined.prefixes.extend_from_slice(&list.prefixes);
        }
        Ok(combined)
    }

    async fn list_versions(
        &self,
        prefix: Option<&RemotePath>,
        mode: ListingMode,
        max_keys: Option<NonZeroU32>,
        cancel: &CancellationToken,
    ) -> Result<VersionListing, DownloadError>;

    /// Obtain metadata information about an object.
    async fn head_object(
        &self,
        key: &RemotePath,
        cancel: &CancellationToken,
    ) -> Result<ListingObject, DownloadError>;

    /// Streams the local file contents into remote into the remote storage entry.
    ///
    /// If the operation fails because of timeout or cancellation, the root cause of the error will be
    /// set to `TimeoutOrCancel`.
    async fn upload(
        &self,
        from: impl Stream<Item = std::io::Result<Bytes>> + Send + Sync + 'static,
        // S3 PUT request requires the content length to be specified,
        // otherwise it starts to fail with the concurrent connection count increasing.
        data_size_bytes: usize,
        to: &RemotePath,
        metadata: Option<StorageMetadata>,
        cancel: &CancellationToken,
    ) -> anyhow::Result<()>;

    /// Streams the remote storage entry contents.
    ///
    /// The returned download stream will obey initial timeout and cancellation signal by erroring
    /// on whichever happens first. Only one of the reasons will fail the stream, which is usually
    /// enough for `tokio::io::copy_buf` usage. If needed the error can be filtered out.
    ///
    /// Returns the metadata, if any was stored with the file previously.
    async fn download(
        &self,
        from: &RemotePath,
        opts: &DownloadOpts,
        cancel: &CancellationToken,
    ) -> Result<Download, DownloadError>;

    /// Delete a single path from remote storage.
    ///
    /// If the operation fails because of timeout or cancellation, the root cause of the error will be
    /// set to `TimeoutOrCancel`. In such situation it is unknown if the deletion went through.
    async fn delete(&self, path: &RemotePath, cancel: &CancellationToken) -> anyhow::Result<()>;

    /// Delete a multiple paths from remote storage.
    ///
    /// If the operation fails because of timeout or cancellation, the root cause of the error will be
    /// set to `TimeoutOrCancel`. In such situation it is unknown which deletions, if any, went
    /// through.
    async fn delete_objects(
        &self,
        paths: &[RemotePath],
        cancel: &CancellationToken,
    ) -> anyhow::Result<()>;

    /// Returns the maximum number of keys that a call to [`Self::delete_objects`] can delete without chunking
    ///
    /// The value returned is only an optimization hint, One can pass larger number of objects to
    /// `delete_objects` as well.
    ///
    /// The value is guaranteed to be >= 1.
    fn max_keys_per_delete(&self) -> usize;

    /// Deletes all objects matching the given prefix.
    ///
    /// NB: this uses NoDelimiter and will match partial prefixes. For example, the prefix /a/b will
    /// delete /a/b, /a/b/*, /a/bc, /a/bc/*, etc.
    ///
    /// If the operation fails because of timeout or cancellation, the root cause of the error will
    /// be set to `TimeoutOrCancel`. In such situation it is unknown which deletions, if any, went
    /// through.
    async fn delete_prefix(
        &self,
        prefix: &RemotePath,
        cancel: &CancellationToken,
    ) -> anyhow::Result<()> {
        let mut stream =
            pin!(self.list_streaming(Some(prefix), ListingMode::NoDelimiter, None, cancel));
        while let Some(result) = stream.next().await {
            let keys = match result {
                Ok(listing) if listing.keys.is_empty() => continue,
                Ok(listing) => listing.keys.into_iter().map(|o| o.key).collect_vec(),
                Err(DownloadError::Cancelled) => return Err(TimeoutOrCancel::Cancel.into()),
                Err(DownloadError::Timeout) => return Err(TimeoutOrCancel::Timeout.into()),
                Err(err) => return Err(err.into()),
            };
            tracing::info!("Deleting {} keys from remote storage", keys.len());
            self.delete_objects(&keys, cancel).await?;
        }
        Ok(())
    }

    /// Copy a remote object inside a bucket from one path to another.
    async fn copy(
        &self,
        from: &RemotePath,
        to: &RemotePath,
        cancel: &CancellationToken,
    ) -> anyhow::Result<()>;

    /// Resets the content of everything with the given prefix to the given state
    async fn time_travel_recover(
        &self,
        prefix: Option<&RemotePath>,
        timestamp: SystemTime,
        done_if_after: SystemTime,
        cancel: &CancellationToken,
        complexity_limit: Option<NonZeroU32>,
    ) -> Result<(), TimeTravelError>;
}

/// Data part of an ongoing [`Download`].
///
/// `DownloadStream` is sensitive to the timeout and cancellation used with the original
/// [`RemoteStorage::download`] request. The type yields `std::io::Result<Bytes>` to be compatible
/// with `tokio::io::copy_buf`.
// This has 'static because safekeepers do not use cancellation tokens (yet)
pub type DownloadStream =
    Pin<Box<dyn Stream<Item = std::io::Result<Bytes>> + Send + Sync + 'static>>;

pub struct Download {
    pub download_stream: DownloadStream,
    /// The last time the file was modified (`last-modified` HTTP header)
    pub last_modified: SystemTime,
    /// A way to identify this specific version of the resource (`etag` HTTP header)
    pub etag: Etag,
    /// Extra key-value data, associated with the current remote file.
    pub metadata: Option<StorageMetadata>,
}

impl Debug for Download {
    fn fmt(&self, f: &mut std::fmt::Formatter<'_>) -> std::fmt::Result {
        f.debug_struct("Download")
            .field("metadata", &self.metadata)
            .finish()
    }
}

/// Every storage backend that yb-light supports.
/// Serves as a simple way to pass around the [`RemoteStorage`] without
/// dealing with generics. Post lane E + LocalFs removal, only YbStore
/// (real backend) + Unreliable (test wrapper) survive — the cloud
/// backends and their SDK dependencies were deleted, and the dev-only
/// LocalFs backend was removed afterward.
#[derive(Clone)]
pub enum GenericRemoteStorage<Other: Clone = Arc<UnreliableWrapper>> {
    Unreliable(Other),
    YbStore(Arc<YbStore>),
}

impl<Other: RemoteStorage> GenericRemoteStorage<Arc<Other>> {
    // See [`RemoteStorage::list`].
    pub async fn list(
        &self,
        prefix: Option<&RemotePath>,
        mode: ListingMode,
        max_keys: Option<NonZeroU32>,
        cancel: &CancellationToken,
    ) -> Result<Listing, DownloadError> {
        match self {
            Self::Unreliable(s) => s.list(prefix, mode, max_keys, cancel).await,
            Self::YbStore(s) => s.list(prefix, mode, max_keys, cancel).await,
        }
    }

    // See [`RemoteStorage::list_streaming`].
    pub fn list_streaming<'a>(
        &'a self,
        prefix: Option<&'a RemotePath>,
        mode: ListingMode,
        max_keys: Option<NonZeroU32>,
        cancel: &'a CancellationToken,
    ) -> impl Stream<Item = Result<Listing, DownloadError>> + 'a + Send {
        match self {
            Self::Unreliable(s) => Box::pin(s.list_streaming(prefix, mode, max_keys, cancel))
                as Pin<Box<dyn Stream<Item = Result<Listing, DownloadError>> + Send>>,
            Self::YbStore(s) => Box::pin(s.list_streaming(prefix, mode, max_keys, cancel)),
        }
    }

    // See [`RemoteStorage::list_versions`].
    pub async fn list_versions<'a>(
        &'a self,
        prefix: Option<&'a RemotePath>,
        mode: ListingMode,
        max_keys: Option<NonZeroU32>,
        cancel: &'a CancellationToken,
    ) -> Result<VersionListing, DownloadError> {
        match self {
            Self::Unreliable(s) => s.list_versions(prefix, mode, max_keys, cancel).await,
            Self::YbStore(s) => s.list_versions(prefix, mode, max_keys, cancel).await,
        }
    }

    // See [`RemoteStorage::head_object`].
    pub async fn head_object(
        &self,
        key: &RemotePath,
        cancel: &CancellationToken,
    ) -> Result<ListingObject, DownloadError> {
        match self {
            Self::Unreliable(s) => s.head_object(key, cancel).await,
            Self::YbStore(s) => s.head_object(key, cancel).await,
        }
    }

    /// See [`RemoteStorage::upload`]
    pub async fn upload(
        &self,
        from: impl Stream<Item = std::io::Result<Bytes>> + Send + Sync + 'static,
        data_size_bytes: usize,
        to: &RemotePath,
        metadata: Option<StorageMetadata>,
        cancel: &CancellationToken,
    ) -> anyhow::Result<()> {
        match self {
            Self::Unreliable(s) => s.upload(from, data_size_bytes, to, metadata, cancel).await,
            Self::YbStore(s) => s.upload(from, data_size_bytes, to, metadata, cancel).await,
        }
    }

    /// See [`RemoteStorage::download`]
    pub async fn download(
        &self,
        from: &RemotePath,
        opts: &DownloadOpts,
        cancel: &CancellationToken,
    ) -> Result<Download, DownloadError> {
        match self {
            Self::Unreliable(s) => s.download(from, opts, cancel).await,
            Self::YbStore(s) => s.download(from, opts, cancel).await,
        }
    }

    /// See [`RemoteStorage::delete`]
    pub async fn delete(
        &self,
        path: &RemotePath,
        cancel: &CancellationToken,
    ) -> anyhow::Result<()> {
        match self {
            Self::Unreliable(s) => s.delete(path, cancel).await,
            Self::YbStore(s) => s.delete(path, cancel).await,
        }
    }

    /// See [`RemoteStorage::delete_objects`]
    pub async fn delete_objects(
        &self,
        paths: &[RemotePath],
        cancel: &CancellationToken,
    ) -> anyhow::Result<()> {
        match self {
            Self::Unreliable(s) => s.delete_objects(paths, cancel).await,
            Self::YbStore(s) => s.delete_objects(paths, cancel).await,
        }
    }

    /// [`RemoteStorage::max_keys_per_delete`]
    pub fn max_keys_per_delete(&self) -> usize {
        match self {
            Self::Unreliable(s) => s.max_keys_per_delete(),
            Self::YbStore(s) => s.max_keys_per_delete(),
        }
    }

    /// See [`RemoteStorage::delete_prefix`]
    pub async fn delete_prefix(
        &self,
        prefix: &RemotePath,
        cancel: &CancellationToken,
    ) -> anyhow::Result<()> {
        match self {
            Self::Unreliable(s) => s.delete_prefix(prefix, cancel).await,
            Self::YbStore(s) => s.delete_prefix(prefix, cancel).await,
        }
    }

    /// See [`RemoteStorage::copy`]
    pub async fn copy_object(
        &self,
        from: &RemotePath,
        to: &RemotePath,
        cancel: &CancellationToken,
    ) -> anyhow::Result<()> {
        match self {
            Self::Unreliable(s) => s.copy(from, to, cancel).await,
            Self::YbStore(s) => s.copy(from, to, cancel).await,
        }
    }

    /// See [`RemoteStorage::time_travel_recover`].
    pub async fn time_travel_recover(
        &self,
        prefix: Option<&RemotePath>,
        timestamp: SystemTime,
        done_if_after: SystemTime,
        cancel: &CancellationToken,
        complexity_limit: Option<NonZeroU32>,
    ) -> Result<(), TimeTravelError> {
        match self {
            Self::Unreliable(s) => {
                s.time_travel_recover(prefix, timestamp, done_if_after, cancel, complexity_limit)
                    .await
            }
            Self::YbStore(s) => {
                s.time_travel_recover(prefix, timestamp, done_if_after, cancel, complexity_limit)
                    .await
            }
        }
    }
}

impl GenericRemoteStorage {
    pub async fn from_storage_kind(kind: TypedRemoteStorageKind) -> anyhow::Result<Self> {
        Self::from_config(&RemoteStorageConfig {
            storage: kind.into(),
            timeout: RemoteStorageConfig::DEFAULT_TIMEOUT,
            small_timeout: RemoteStorageConfig::DEFAULT_SMALL_TIMEOUT,
        })
        .await
    }

    pub async fn from_config(storage_config: &RemoteStorageConfig) -> anyhow::Result<Self> {
        info!("RemoteStorageConfig: {:?}", storage_config);

        let timeout = storage_config.timeout;

        // If someone overrides timeout to be small without adjusting small_timeout, then adjust it automatically
        let small_timeout = std::cmp::min(storage_config.small_timeout, timeout);

        info!(
            "RemoteStorageConfig's storage attribute: {:?}",
            storage_config.storage
        );

        let _ = small_timeout; // reserved; the cloud backends used it
        Ok(match &storage_config.storage {
            RemoteStorageKind::YbStore(yb_config) => {
                info!(
                    "Using YugabyteDB blob store at {} (db {}) as remote storage",
                    yb_config.host_ports, yb_config.dbname
                );
                Self::YbStore(Arc::new(YbStore::new(yb_config, timeout).await?))
            }
        })
    }

    pub fn unreliable_wrapper(s: Self, fail_first: u64, fail_probability: u64) -> Self {
        Self::Unreliable(Arc::new(UnreliableWrapper::new(
            s,
            fail_first,
            fail_probability,
        )))
    }

    /// See [`RemoteStorage::upload`], which this method calls with `None` as metadata.
    pub async fn upload_storage_object(
        &self,
        from: impl Stream<Item = std::io::Result<Bytes>> + Send + Sync + 'static,
        from_size_bytes: usize,
        to: &RemotePath,
        cancel: &CancellationToken,
    ) -> anyhow::Result<()> {
        self.upload(from, from_size_bytes, to, None, cancel)
            .await
            .with_context(|| {
                format!("Failed to upload data of length {from_size_bytes} to storage path {to:?}")
            })
    }

    /// The name of the bucket/container/etc.
    pub fn bucket_name(&self) -> Option<&str> {
        match self {
            Self::Unreliable(_s) => None,
            Self::YbStore(s) => Some(s.table_name()),
        }
    }
}

/// Extra set of key-value pairs that contain arbitrary metadata about the storage entry.
/// Immutable, cannot be changed once the file is created.
#[derive(Debug, Clone, PartialEq, Eq)]
pub struct StorageMetadata(HashMap<String, String>);

impl<const N: usize> From<[(&str, &str); N]> for StorageMetadata {
    fn from(arr: [(&str, &str); N]) -> Self {
        let map: HashMap<String, String> = arr
            .iter()
            .map(|(k, v)| (k.to_string(), v.to_string()))
            .collect();
        Self(map)
    }
}

struct ConcurrencyLimiter {
    // Every request to S3 can be throttled or cancelled, if a certain number of requests per second is exceeded.
    // Same goes to IAM, which is queried before every S3 request, if enabled. IAM has even lower RPS threshold.
    // The helps to ensure we don't exceed the thresholds.
    write: Arc<Semaphore>,
    read: Arc<Semaphore>,
}

impl ConcurrencyLimiter {
    fn for_kind(&self, kind: RequestKind) -> &Arc<Semaphore> {
        match kind {
            RequestKind::Get => &self.read,
            RequestKind::Put => &self.write,
            RequestKind::List => &self.read,
            RequestKind::Delete => &self.write,
            RequestKind::Copy => &self.write,
            RequestKind::Head => &self.read,
        }
    }

    async fn acquire(
        &self,
        kind: RequestKind,
    ) -> Result<tokio::sync::SemaphorePermit<'_>, tokio::sync::AcquireError> {
        self.for_kind(kind).acquire().await
    }

    fn new(limit: usize) -> ConcurrencyLimiter {
        Self {
            read: Arc::new(Semaphore::new(limit)),
            write: Arc::new(Semaphore::new(limit)),
        }
    }
}

#[cfg(test)]
mod tests {
    use super::*;

    /// DownloadOpts::byte_range() should generate (inclusive, exclusive) ranges
    /// with optional end bound, or None when unbounded.
    #[test]
    fn download_opts_byte_range() {
        // Consider using test_case or a similar table-driven test framework.
        let cases = [
            // (byte_start, byte_end, expected)
            (Bound::Unbounded, Bound::Unbounded, None),
            (Bound::Unbounded, Bound::Included(7), Some((0, Some(8)))),
            (Bound::Unbounded, Bound::Excluded(7), Some((0, Some(7)))),
            (Bound::Included(3), Bound::Unbounded, Some((3, None))),
            (Bound::Included(3), Bound::Included(7), Some((3, Some(8)))),
            (Bound::Included(3), Bound::Excluded(7), Some((3, Some(7)))),
            (Bound::Excluded(3), Bound::Unbounded, Some((4, None))),
            (Bound::Excluded(3), Bound::Included(7), Some((4, Some(8)))),
            (Bound::Excluded(3), Bound::Excluded(7), Some((4, Some(7)))),
            // 1-sized ranges are fine, 0 aren't and will panic (separate test).
            (Bound::Included(3), Bound::Included(3), Some((3, Some(4)))),
            (Bound::Included(3), Bound::Excluded(4), Some((3, Some(4)))),
        ];

        for (byte_start, byte_end, expect) in cases {
            let opts = DownloadOpts {
                byte_start,
                byte_end,
                ..Default::default()
            };
            let result = opts.byte_range();
            assert_eq!(
                result, expect,
                "byte_start={byte_start:?} byte_end={byte_end:?}"
            );

            // Check generated HTTP header, which uses an inclusive range.
            let expect_header = expect.map(|(start, end)| match end {
                Some(end) => format!("bytes={start}-{}", end - 1), // inclusive end
                None => format!("bytes={start}-"),
            });
            assert_eq!(
                opts.byte_range_header(),
                expect_header,
                "byte_start={byte_start:?} byte_end={byte_end:?}"
            );
        }
    }

    /// DownloadOpts::byte_range() zero-sized byte range should panic.
    #[test]
    #[should_panic]
    fn download_opts_byte_range_zero() {
        DownloadOpts {
            byte_start: Bound::Included(3),
            byte_end: Bound::Excluded(3),
            ..Default::default()
        }
        .byte_range();
    }

    /// DownloadOpts::byte_range() negative byte range should panic.
    #[test]
    #[should_panic]
    fn download_opts_byte_range_negative() {
        DownloadOpts {
            byte_start: Bound::Included(3),
            byte_end: Bound::Included(2),
            ..Default::default()
        }
        .byte_range();
    }

    #[test]
    fn test_object_name() {
        let k = RemotePath::new(Utf8Path::new("a/b/c")).unwrap();
        assert_eq!(k.object_name(), Some("c"));

        let k = RemotePath::new(Utf8Path::new("a/b/c/")).unwrap();
        assert_eq!(k.object_name(), Some("c"));

        let k = RemotePath::new(Utf8Path::new("a/")).unwrap();
        assert_eq!(k.object_name(), Some("a"));

        // XXX is it impossible to have an empty key?
        let k = RemotePath::new(Utf8Path::new("")).unwrap();
        assert_eq!(k.object_name(), None);
    }

    #[test]
    fn rempte_path_cannot_be_created_from_absolute_ones() {
        let err = RemotePath::new(Utf8Path::new("/")).expect_err("Should fail on absolute paths");
        assert_eq!(err.to_string(), "Path \"/\" is not relative");
    }
}
