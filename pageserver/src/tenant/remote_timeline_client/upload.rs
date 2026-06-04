//! Helper functions to upload files to remote storage with a RemoteStorage

use std::io::SeekFrom;

use anyhow::{Context, bail};
use bytes::Bytes;
use fail::fail_point;
use remote_storage::GenericRemoteStorage;
use tokio::fs::File;
use tokio::io::AsyncSeekExt;
use tokio_util::sync::CancellationToken;
use utils::id::{TenantId, TimelineId};
use utils::pausable_failpoint;

use super::Generation;
use super::manifest::TenantManifest;
use crate::tenant::remote_timeline_client::{
    remote_initdb_archive_path,
    remote_tenant_manifest_path,
};

/// Serializes and uploads the given tenant manifest data to the remote storage.
pub(crate) async fn upload_tenant_manifest(
    storage: &GenericRemoteStorage,
    tenant_shard_id: &TenantId,
    generation: Generation,
    tenant_manifest: &TenantManifest,
    cancel: &CancellationToken,
) -> anyhow::Result<()> {
    tracing::trace!("uploading new tenant manifest");

    fail_point!("before-upload-manifest", |_| {
        bail!("failpoint before-upload-manifest")
    });
    pausable_failpoint!("before-upload-manifest-pausable");

    let serialized = Bytes::from(tenant_manifest.to_json_bytes()?);
    let tenant_manifest_size = serialized.len();
    let remote_path = remote_tenant_manifest_path(tenant_shard_id, generation);

    storage
        .upload_storage_object(
            futures::stream::once(futures::future::ready(Ok(serialized))),
            tenant_manifest_size,
            &remote_path,
            cancel,
        )
        .await
        .with_context(|| format!("upload tenant manifest for '{tenant_shard_id}'"))
}

/// Uploads the given `initdb` data to the remote storage.
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


// The upstream `time_travel_recover_tenant` walked an S3 bucket's
// versioned objects to roll a tenant back to a timestamp. S3 is gone
// (lane E), and the YB-native time-travel design lives on
// `pageserver.pages` + `pageserver.timeline_state` row history rather
// than blob versions. See architecture/yb-pageserver.md "Time travel
// and branching" for the planned shape.
