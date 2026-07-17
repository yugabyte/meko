//! Download a previously-uploaded `initdb.tar.zst` archive.

use std::future::Future;

use anyhow::Context;
use camino::{Utf8Path, Utf8PathBuf};
use remote_storage::{DownloadError, DownloadOpts, GenericRemoteStorage};
use tokio::fs::{File, OpenOptions};
use tokio::io::AsyncSeekExt;
use tokio_util::sync::CancellationToken;
use tracing::warn;
use utils::backoff;
use utils::id::BranchId;

use crate::{
    BUFFER_SIZE, FAILED_DOWNLOAD_WARN_THRESHOLD, FAILED_REMOTE_OP_RETRIES, INITDB_PATH,
    TEMP_FILE_SUFFIX, remote_initdb_archive_path, remote_initdb_preserved_archive_path,
};

/// Download the branch's initdb tarball into `branch_path`. Returns the
/// path of the on-disk tarball and an open `File` rewound to byte 0, ready
/// for the caller to feed into `extract_zst_tarball`.
///
/// Falls back to the `initdb-preserved.tar.zst` object if the primary one
/// is gone (we keep a preserved copy alongside the writable one).
pub async fn download_initdb_tar_zst(
    branch_path: &Utf8Path,
    storage: &GenericRemoteStorage,
    branch_id: &BranchId,
    cancel: &CancellationToken,
) -> Result<(Utf8PathBuf, File), DownloadError> {
    let remote_path = remote_initdb_archive_path(branch_id);
    let remote_preserved_path = remote_initdb_preserved_archive_path(branch_id);

    if !branch_path.exists() {
        tokio::fs::create_dir_all(&branch_path)
            .await
            .with_context(|| format!("branch dir creation {branch_path}"))
            .map_err(DownloadError::Other)?;
    }
    let temp_path = branch_path.join(format!(
        "{INITDB_PATH}.download-{branch_id}.{TEMP_FILE_SUFFIX}"
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
            let mut writer = tokio::io::BufWriter::with_capacity(BUFFER_SIZE, file);

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
        if let Err(e) = std::fs::remove_file(&temp_path) {
            if e.kind() != std::io::ErrorKind::NotFound {
                warn!("error deleting temporary file {temp_path}: {e}");
            }
        }
    })?;

    Ok((temp_path, file))
}

/// Retry a download operation: bounded retries with backoff.
async fn download_retry<T, O, F>(
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
