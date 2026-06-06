//! Helper functions to download files from remote storage with a RemoteStorage
//!
//! The functions in this module retry failed operations automatically, according
//! to the FAILED_DOWNLOAD_RETRIES constant.

use std::collections::HashSet;
use std::future::Future;
use std::str::FromStr;
use std::time::SystemTime;

use anyhow::Context;
use camino::Utf8PathBuf;
use remote_storage::{
    DownloadError, DownloadOpts, GenericRemoteStorage, ListingMode, RemotePath,
};
use tokio::fs::{File, OpenOptions};
use tokio::io::AsyncSeekExt;
use tokio_util::io::StreamReader;
use tokio_util::sync::CancellationToken;
use tracing::warn;
use utils::id::{TenantId, TimelineId};
use utils::backoff;

use super::{
    FAILED_DOWNLOAD_WARN_THRESHOLD, FAILED_REMOTE_OP_RETRIES, INITDB_PATH, remote_initdb_archive_path,
    remote_initdb_preserved_archive_path,
};
use crate::TEMP_FILE_SUFFIX;
use crate::config::PageServerConf;
use crate::span::{
    debug_assert_current_span_has_tenant_and_timeline_id, debug_assert_current_span_has_tenant_id,
};
use crate::tenant::Generation;

async fn list_identifiers<T>(
    storage: &GenericRemoteStorage,
    prefix: RemotePath,
    cancel: CancellationToken,
) -> anyhow::Result<(HashSet<T>, HashSet<String>)>
where
    T: FromStr + Eq + std::hash::Hash,
{
    let listing = download_retry_forever(
        || storage.list(Some(&prefix), ListingMode::WithDelimiter, None, &cancel),
        &format!("list identifiers in prefix {prefix}"),
        &cancel,
    )
    .await?;

    let mut parsed_ids = HashSet::new();
    let mut other_prefixes = HashSet::new();

    for id_remote_storage_key in listing.prefixes {
        let object_name = id_remote_storage_key.object_name().ok_or_else(|| {
            anyhow::anyhow!("failed to get object name for key {id_remote_storage_key}")
        })?;

        match object_name.parse::<T>() {
            Ok(t) => parsed_ids.insert(t),
            Err(_) => other_prefixes.insert(object_name.to_string()),
        };
    }

    for object in listing.keys {
        let object_name = object
            .key
            .object_name()
            .ok_or_else(|| anyhow::anyhow!("object name for key {}", object.key))?;
        other_prefixes.insert(object_name.to_string());
    }

    Ok((parsed_ids, other_prefixes))
}

async fn do_download_remote_path_retry_forever(
    storage: &GenericRemoteStorage,
    remote_path: &RemotePath,
    download_opts: DownloadOpts,
    cancel: &CancellationToken,
) -> Result<(Vec<u8>, SystemTime), DownloadError> {
    download_retry_forever(
        || async {
            let download = storage
                .download(remote_path, &download_opts, cancel)
                .await?;

            let mut bytes = Vec::new();

            let stream = download.download_stream;
            let mut stream = StreamReader::new(stream);

            tokio::io::copy_buf(&mut stream, &mut bytes).await?;

            Ok((bytes, download.last_modified))
        },
        &format!("download {remote_path:?}"),
        cancel,
    )
    .await
}

/// Metadata objects are "generationed", meaning that they include a generation suffix.  This
/// function downloads the object with the highest generation <= `my_generation`.
///
/// Data objects (layer files) also include a generation in their path, but there is no equivalent
/// search process, because their reference from an index includes the generation.
///
/// An expensive object listing operation is only done if necessary: the typical fast path is to issue two
/// GET operations, one to our own generation (stale attachment case), and one to the immediately preceding
/// generation (normal case when migrating/restarting).  Only if both of these return 404 do we fall back
/// to listing objects.
///
/// * `my_generation`: the value of `[crate::tenant::TenantShard::generation]`
/// * `what`: for logging, what object are we downloading
/// * `prefix`: when listing objects, use this prefix (i.e. the part of the object path before the generation)
/// * `do_download`: a GET of the object in a particular generation, which should **retry indefinitely** unless
///                  `cancel`` has fired.  This function does not do its own retries of GET operations, and relies
///                  on the function passed in to do so.
/// * `parse_path`: parse a fully qualified remote storage path to get the generation of the object.
#[allow(clippy::too_many_arguments)]
#[tracing::instrument(skip_all, fields(generation=?my_generation))]
pub(crate) async fn download_generation_object<'a, T, DF, DFF, PF>(
    storage: &'a GenericRemoteStorage,
    tenant_shard_id: &'a TenantId,
    timeline_id: Option<&'a TimelineId>,
    my_generation: Generation,
    what: &str,
    prefix: RemotePath,
    do_download: DF,
    parse_path: PF,
    cancel: &'a CancellationToken,
) -> Result<(T, Generation, SystemTime), DownloadError>
where
    DF: Fn(
        &'a GenericRemoteStorage,
        &'a TenantId,
        Option<&'a TimelineId>,
        Generation,
        &'a CancellationToken,
    ) -> DFF,
    DFF: Future<Output = Result<(T, Generation, SystemTime), DownloadError>>,
    PF: Fn(RemotePath) -> Option<Generation>,
    T: 'static,
{
    debug_assert_current_span_has_tenant_id();

    if my_generation.is_none() {
        // Operating without generations: just fetch the generation-less path
        return do_download(storage, tenant_shard_id, timeline_id, my_generation, cancel).await;
    }

    // Stale case: If we were intentionally attached in a stale generation, the remote object may already
    // exist in our generation.
    //
    // This is an optimization to avoid doing the listing for the general case below.
    let res = do_download(storage, tenant_shard_id, timeline_id, my_generation, cancel).await;
    match res {
        Ok(decoded) => {
            tracing::debug!("Found {what} from current generation (this is a stale attachment)");
            return Ok(decoded);
        }
        Err(DownloadError::NotFound) => {}
        Err(e) => return Err(e),
    };

    // Typical case: the previous generation of this tenant was running healthily, and had uploaded the object
    // we are seeking in that generation.  We may safely start from this index without doing a listing, because:
    //  - We checked for current generation case above
    //  - generations > my_generation are to be ignored
    //  - any other objects that exist would have an older generation than `previous_gen`, and
    //    we want to find the most recent object from a previous generation.
    //
    // This is an optimization to avoid doing the listing for the general case below.
    let res = do_download(
        storage,
        tenant_shard_id,
        timeline_id,
        my_generation.previous(),
        cancel,
    )
    .await;
    match res {
        Ok(decoded) => {
            tracing::debug!("Found {what} from previous generation");
            return Ok(decoded);
        }
        Err(DownloadError::NotFound) => {
            tracing::debug!("No {what} found from previous generation, falling back to listing");
        }
        Err(e) => {
            return Err(e);
        }
    }

    // General case/fallback: if there is no index at my_generation or prev_generation, then list all index_part.json
    // objects, and select the highest one with a generation <= my_generation.  Constructing the prefix is equivalent
    // to constructing a full index path with no generation, because the generation is a suffix.
    let paths = download_retry(
        || async {
            storage
                .list(Some(&prefix), ListingMode::NoDelimiter, None, cancel)
                .await
        },
        "list index_part files",
        cancel,
    )
    .await?
    .keys;

    // General case logic for which index to use: the latest index whose generation
    // is <= our own.  See "Finding the remote indices for timelines" in docs/rfcs/025-generation-numbers.md
    let max_previous_generation = paths
        .into_iter()
        .filter_map(|o| parse_path(o.key))
        .filter(|g| g <= &my_generation)
        .max();

    match max_previous_generation {
        Some(g) => {
            tracing::debug!("Found {what} in generation {g:?}");
            do_download(storage, tenant_shard_id, timeline_id, g, cancel).await
        }
        None => {
            // Migration from legacy pre-generation state: we have a generation but no prior
            // attached pageservers did.  Try to load from a no-generation path.
            tracing::debug!("No {what}* found");
            do_download(
                storage,
                tenant_shard_id,
                timeline_id,
                Generation::none(),
                cancel,
            )
            .await
        }
    }
}

pub(crate) async fn download_initdb_tar_zst(
    conf: &'static PageServerConf,
    storage: &GenericRemoteStorage,
    tenant_shard_id: &TenantId,
    timeline_id: &TimelineId,
    cancel: &CancellationToken,
) -> Result<(Utf8PathBuf, File), DownloadError> {
    debug_assert_current_span_has_tenant_and_timeline_id();

    let remote_path = remote_initdb_archive_path(&tenant_shard_id, timeline_id);

    let remote_preserved_path =
        remote_initdb_preserved_archive_path(&tenant_shard_id, timeline_id);

    let timeline_path = conf.timelines_path(tenant_shard_id);

    if !timeline_path.exists() {
        tokio::fs::create_dir_all(&timeline_path)
            .await
            .with_context(|| format!("timeline dir creation {timeline_path}"))
            .map_err(DownloadError::Other)?;
    }
    let temp_path = timeline_path.join(format!(
        "{INITDB_PATH}.download-{timeline_id}.{TEMP_FILE_SUFFIX}"
    ));

    let file = download_retry(
        || async {
            let file = OpenOptions::new()
                .create(true)
                .truncate(true)
                .read(true)
                .write(true)
                .open(&temp_path)
                .await
                .with_context(|| format!("tempfile creation {temp_path}"))
                .map_err(DownloadError::Other)?;

            let download = match storage
                .download(&remote_path, &DownloadOpts::default(), cancel)
                .await
            {
                Ok(dl) => dl,
                Err(DownloadError::NotFound) => {
                    storage
                        .download(&remote_preserved_path, &DownloadOpts::default(), cancel)
                        .await?
                }
                Err(other) => Err(other)?,
            };
            let mut download = tokio_util::io::StreamReader::new(download.download_stream);
            let mut writer = tokio::io::BufWriter::with_capacity(super::BUFFER_SIZE, file);

            tokio::io::copy_buf(&mut download, &mut writer).await?;

            let mut file = writer.into_inner();

            file.seek(std::io::SeekFrom::Start(0))
                .await
                .with_context(|| format!("rewinding initdb.tar.zst at: {remote_path:?}"))
                .map_err(DownloadError::Other)?;

            Ok(file)
        },
        &format!("download {remote_path}"),
        cancel,
    )
    .await
    .inspect_err(|_e| {
        // Do a best-effort attempt at deleting the temporary file upon encountering an error.
        // We don't have async here nor do we want to pile on any extra errors.
        if let Err(e) = std::fs::remove_file(&temp_path) {
            if e.kind() != std::io::ErrorKind::NotFound {
                warn!("error deleting temporary file {temp_path}: {e}");
            }
        }
    })?;

    Ok((temp_path, file))
}

/// Helper function to handle retries for a download operation.
///
/// Remote operations can fail due to rate limits (S3), spurious network
/// problems, or other external reasons. Retry FAILED_DOWNLOAD_RETRIES times,
/// with backoff.
///
/// (See similar logic for uploads in `perform_upload_task`)
pub(super) async fn download_retry<T, O, F>(
    op: O,
    description: &str,
    cancel: &CancellationToken,
) -> Result<T, DownloadError>
where
    O: FnMut() -> F,
    F: Future<Output = Result<T, DownloadError>>,
{
    backoff::retry(
        op,
        DownloadError::is_permanent,
        FAILED_DOWNLOAD_WARN_THRESHOLD,
        FAILED_REMOTE_OP_RETRIES,
        description,
        cancel,
    )
    .await
    .ok_or_else(|| DownloadError::Cancelled)
    .and_then(|x| x)
}

pub(crate) async fn download_retry_forever<T, O, F>(
    op: O,
    description: &str,
    cancel: &CancellationToken,
) -> Result<T, DownloadError>
where
    O: FnMut() -> F,
    F: Future<Output = Result<T, DownloadError>>,
{
    backoff::retry(
        op,
        DownloadError::is_permanent,
        FAILED_DOWNLOAD_WARN_THRESHOLD,
        u32::MAX,
        description,
        cancel,
    )
    .await
    .ok_or_else(|| DownloadError::Cancelled)
    .and_then(|x| x)
}
