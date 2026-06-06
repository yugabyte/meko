//! Helper functions to upload files to remote storage with a RemoteStorage

use std::io::SeekFrom;

use anyhow::Context;
use remote_storage::GenericRemoteStorage;
use tokio::fs::File;
use tokio::io::AsyncSeekExt;
use tokio_util::sync::CancellationToken;
use utils::id::{TenantId, TimelineId};

use crate::tenant::remote_timeline_client::remote_initdb_archive_path;

/// Uploads the given `initdb` data to the remote storage. Called once on
/// first-time bootstrap so future branches can reuse the tarball via
/// `download_initdb_tar_zst` instead of re-running `initdb`.
pub(crate) async fn upload_initdb_dir(
    storage: &GenericRemoteStorage,
    tenant_id: &TenantId,
    timeline_id: &TimelineId,
    mut initdb_tar_zst: File,
    size: u64,
    cancel: &CancellationToken,
) -> anyhow::Result<()> {
    tracing::trace!("uploading initdb dir");

    // We might have read somewhat into the file already in the prior retry attempt
    initdb_tar_zst.seek(SeekFrom::Start(0)).await?;

    let file = tokio_util::io::ReaderStream::with_capacity(initdb_tar_zst, super::BUFFER_SIZE);

    let remote_path = remote_initdb_archive_path(tenant_id, timeline_id);
    storage
        .upload_storage_object(file, size as usize, &remote_path, cancel)
        .await
        .with_context(|| format!("upload initdb dir for '{tenant_id} / {timeline_id}'"))
}
