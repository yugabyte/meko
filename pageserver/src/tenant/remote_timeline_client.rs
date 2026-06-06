//! This module manages synchronizing local FS with remote storage.
//!
//! # Overview
//!
//! * [`RemoteTimelineClient`] provides functions related to upload/download of a particular timeline.
//!   It contains a queue of pending uploads, and manages the queue, performing uploads in parallel
//!   when it's safe to do so.
//!
//! * Stand-alone function, [`list_remote_timelines`], to get list of timelines of a tenant.
//!
//! These functions use the low-level remote storage client, [`remote_storage::RemoteStorage`].
//!
//! # APIs & How To Use Them
//!
//! There is a [RemoteTimelineClient] for each [Timeline][`crate::tenant::Timeline`] in the system,
//! unless the pageserver is configured without remote storage.
//!
//! We allocate the client instance in [Timeline][`crate::tenant::Timeline`], i.e.,
//! either in [`crate::tenant::mgr`] during startup or when creating a new
//! timeline.
//! However, the client does not become ready for use until we've initialized its upload queue:
//!
//! - For timelines that already have some state on the remote storage, we use
//!   [`RemoteTimelineClient::init_upload_queue`] .
//! - For newly created timelines, we use
//!   [`RemoteTimelineClient::init_upload_queue_for_empty_remote`].
//!
//! The former takes the remote's [`IndexPart`] as an argument, possibly retrieved
//! using [`list_remote_timelines`]. We'll elaborate on [`IndexPart`] in the next section.
//!
//! Whenever we've created/updated/deleted a file in a timeline directory, we schedule
//! the corresponding remote operation with the timeline's [`RemoteTimelineClient`]:
//!
//! - [`RemoteTimelineClient::schedule_layer_file_upload`]  when we've created a new layer file.
//! - [`RemoteTimelineClient::schedule_index_upload_for_metadata_update`] when we've updated the timeline metadata file.
//! - [`RemoteTimelineClient::schedule_index_upload_for_file_changes`] to upload an updated index file, after we've scheduled file uploads
//! - [`RemoteTimelineClient::schedule_layer_file_deletion`] when we've deleted one or more layer files.
//!
//! Internally, these functions create [`UploadOp`]s and put them in a queue.
//!
//! There are also APIs for downloading files.
//! These are not part of the aforementioned queuing and will not be discussed
//! further here, except in the section covering tenant attach.
//!
//! # Remote Storage Structure & [`IndexPart`] Index File
//!
//! The "directory structure" in the remote storage mirrors the local directory structure, with paths
//! like `tenants/<tenant_id>/timelines/<timeline_id>/<layer filename>`.
//! Yet instead of keeping the `metadata` file remotely, we wrap it with more
//! data in an "index file" aka [`IndexPart`], containing the list of **all** remote
//! files for a given timeline.
//! If a file is not referenced from [`IndexPart`], it's not part of the remote storage state.
//!
//! Having the `IndexPart` also avoids expensive and slow `S3 list` commands.
//!
//! # Consistency
//!
//! To have a consistent remote structure, it's important that uploads and
//! deletions are performed in the right order. For example, the index file
//! contains a list of layer files, so it must not be uploaded until all the
//! layer files that are in its list have been successfully uploaded.
//!
//! The contract between client and its user is that the user is responsible of
//! scheduling operations in an order that keeps the remote consistent as
//! described above.
//!
//! From the user's perspective, the operations are executed sequentially.
//! Internally, the client knows which operations can be performed in parallel,
//! and which operations act like a "barrier" that require preceding operations
//! to finish. The calling code just needs to call the schedule-functions in the
//! correct order, and the client will parallelize the operations in a way that
//! is safe. For more details, see `UploadOp::can_bypass`.
//!
//! All of this relies on the following invariants:
//!
//! - We rely on read-after write consistency in the remote storage.
//! - Layer files are immutable.
//!
//! NB: Pageserver assumes that it has exclusive write access to the tenant in remote
//! storage. Different tenants can be attached to different pageservers, but if the
//! same tenant is attached to two pageservers at the same time, they will overwrite
//! each other's index file updates, and confusion will ensue. There's no interlock or
//! mechanism to detect that in the pageserver, we rely on the control plane to ensure
//! that that doesn't happen.
//!
//! ## Implementation Note
//!
//! The *actual* remote state lags behind the *desired* remote state while
//! there are in-flight operations.
//! We keep track of the desired remote state in [`UploadQueueInitialized::dirty`].
//! It is initialized based on the [`IndexPart`] that was passed during init
//! and updated with every `schedule_*` function call.
//! All this is necessary necessary to compute the future [`IndexPart`]s
//! when scheduling an operation while other operations that also affect the
//! remote [`IndexPart`] are in flight.
//!
//! # Retries & Error Handling
//!
//! The client retries operations indefinitely, using exponential back-off.
//! There is no way to force a retry, i.e., interrupt the back-off.
//! This could be built easily.
//!
//! # Cancellation
//!
//! The operations execute as plain [`task_mgr`] tasks, scoped to
//! the client's tenant and timeline.
//! Dropping the client will drop queued operations but not executing operations.
//! These will complete unless the `task_mgr` tasks are cancelled using `task_mgr`
//! APIs, e.g., during pageserver shutdown, timeline delete, or tenant detach.
//!
//! # Completion
//!
//! Once an operation has completed, we update [`UploadQueueInitialized::clean`] immediately,
//! and submit a request through the DeletionQueue to update
//! [`UploadQueueInitialized::visible_remote_consistent_lsn`] after it has
//! validated that our generation is not stale.  It is this visible value
//! that is advertized to safekeepers as a signal that that they can
//! delete the WAL up to that LSN.
//!
//! The [`RemoteTimelineClient::wait_completion`] method can be used to wait
//! for all pending operations to complete. It does not prevent more
//! operations from getting scheduled.
//!
//! # Crash Consistency
//!
//! We do not persist the upload queue state.
//! If we drop the client, or crash, all unfinished operations are lost.
//!
//! To recover, the following steps need to be taken:
//! - Retrieve the current remote [`IndexPart`]. This gives us a
//!   consistent remote state, assuming the user scheduled the operations in
//!   the correct order.
//! - Initiate upload queue with that [`IndexPart`].
//! - Reschedule all lost operations by comparing the local filesystem state
//!   and remote state as per [`IndexPart`]. This is done in
//!   [`TenantShard::timeline_init_and_sync`].
//!
//! Note that if we crash during file deletion between the index update
//! that removes the file from the list of files, and deleting the remote file,
//! the file is leaked in the remote storage. Similarly, if a new file is created
//! and uploaded, but the pageserver dies permanently before updating the
//! remote index file, the new file is leaked in remote storage. We accept and
//! tolerate that for now.
//! Note further that we cannot easily fix this by scheduling deletes for every
//! file that is present only on the remote, because we cannot distinguish the
//! following two cases:
//! - (1) We had the file locally, deleted it locally, scheduled a remote delete,
//!   but crashed before it finished remotely.
//! - (2) We never had the file locally because we haven't on-demand downloaded
//!   it yet.
//!
//! # Downloads
//!
//! In addition to the upload queue, [`RemoteTimelineClient`] has functions for
//! downloading files from the remote storage. Downloads are performed immediately
//! against the `RemoteStorage`, independently of the upload queue.
//!
//! When we attach a tenant, we perform the following steps:
//! - create `Tenant` object in `TenantState::Attaching` state
//! - List timelines that are present in remote storage, and for each:
//!   - download their remote [`IndexPart`]s
//!   - create `Timeline` struct and a `RemoteTimelineClient`
//!   - initialize the client's upload queue with its `IndexPart`
//!   - schedule uploads for layers that are only present locally.
//! - After the above is done for each timeline, open the tenant for business by
//!   transitioning it from `TenantState::Attaching` to `TenantState::Active` state.
//!   This starts the timelines' WAL-receivers and the tenant's GC & Compaction loops.
//!
//! # Operating Without Remote Storage
//!
//! If no remote storage configuration is provided, the [`RemoteTimelineClient`] is
//! not created and the uploads are skipped.
//!
//! [`TenantShard::timeline_init_and_sync`]: super::TenantShard::timeline_init_and_sync
//! [`Timeline::load_layer_map`]: super::Timeline::load_layer_map

pub(crate) mod download;
pub mod index;
pub mod manifest;
pub(crate) mod upload;

use std::sync::OnceLock;

use camino::Utf8Path;
pub(crate) use download::download_initdb_tar_zst;
use regex::Regex;
use remote_storage::RemotePath;
pub(crate) use upload::upload_initdb_dir;
use utils::id::{TenantId, TimelineId};

use self::index::IndexPart;
use super::Generation;
use crate::metrics::MeasureRemoteOp;
use crate::tenant::TIMELINES_SEGMENT_NAME;

// Occasional network issues and such can cause remote operations to fail, and
// that's expected. If a download fails, we log it at info-level, and retry.
// But after FAILED_DOWNLOAD_WARN_THRESHOLD retries, we start to log it at WARN
// level instead, as repeated failures can mean a more serious problem. If it
// fails more than FAILED_DOWNLOAD_RETRIES times, we give up
pub(crate) const FAILED_DOWNLOAD_WARN_THRESHOLD: u32 = 3;
pub(crate) const FAILED_REMOTE_OP_RETRIES: u32 = 10;

// Similarly log failed uploads and deletions at WARN level, after this many
// retries. Uploads and deletions are retried forever, though.
pub(crate) const FAILED_UPLOAD_WARN_THRESHOLD: u32 = 3;

pub(crate) const INITDB_PATH: &str = "initdb.tar.zst";

pub(crate) const INITDB_PRESERVED_PATH: &str = "initdb-preserved.tar.zst";

/// Default buffer size when interfacing with [`tokio::fs::File`].
pub(crate) const BUFFER_SIZE: usize = 32 * 1024;

pub enum MaybeDeletedIndexPart {
    IndexPart(IndexPart),
    Deleted(IndexPart),
}

pub fn remote_tenant_path(tenant_shard_id: &TenantId) -> RemotePath {
    let path = format!("tenants/{tenant_shard_id}");
    RemotePath::from_string(&path).expect("Failed to construct path")
}

pub fn remote_tenant_manifest_path(
    tenant_shard_id: &TenantId,
    generation: Generation,
) -> RemotePath {
    let path = format!(
        "tenants/{tenant_shard_id}/tenant-manifest{}.json",
        generation.get_suffix()
    );
    RemotePath::from_string(&path).expect("Failed to construct path")
}

/// Prefix to all generations' manifest objects in a tenant shard
pub fn remote_tenant_manifest_prefix(tenant_shard_id: &TenantId) -> RemotePath {
    let path = format!("tenants/{tenant_shard_id}/tenant-manifest",);
    RemotePath::from_string(&path).expect("Failed to construct path")
}

pub fn remote_timelines_path(tenant_shard_id: &TenantId) -> RemotePath {
    let path = format!("tenants/{tenant_shard_id}/{TIMELINES_SEGMENT_NAME}");
    RemotePath::from_string(&path).expect("Failed to construct path")
}

pub fn remote_timeline_path(
    tenant_shard_id: &TenantId,
    timeline_id: &TimelineId,
) -> RemotePath {
    remote_timelines_path(tenant_shard_id).join(Utf8Path::new(&timeline_id.to_string()))
}

/// Obtains the path of the given Layer in the remote
///
pub fn remote_initdb_archive_path(tenant_id: &TenantId, timeline_id: &TimelineId) -> RemotePath {
    RemotePath::from_string(&format!(
        "tenants/{tenant_id}/{TIMELINES_SEGMENT_NAME}/{timeline_id}/{INITDB_PATH}"
    ))
    .expect("Failed to construct path")
}

pub fn remote_initdb_preserved_archive_path(
    tenant_id: &TenantId,
    timeline_id: &TimelineId,
) -> RemotePath {
    RemotePath::from_string(&format!(
        "tenants/{tenant_id}/{TIMELINES_SEGMENT_NAME}/{timeline_id}/{INITDB_PRESERVED_PATH}"
    ))
    .expect("Failed to construct path")
}

pub fn remote_index_path(
    tenant_shard_id: &TenantId,
    timeline_id: &TimelineId,
    generation: Generation,
) -> RemotePath {
    RemotePath::from_string(&format!(
        "tenants/{tenant_shard_id}/{TIMELINES_SEGMENT_NAME}/{timeline_id}/{0}{1}",
        IndexPart::FILE_NAME,
        generation.get_suffix()
    ))
    .expect("Failed to construct path")
}

/// Given the key of an index, parse out the generation part of the name
pub fn parse_remote_index_path(path: RemotePath) -> Option<Generation> {
    let file_name = match path.get_path().file_name() {
        Some(f) => f,
        None => {
            // Unexpected: we should be seeing index_part.json paths only
            tracing::warn!("Malformed index key {}", path);
            return None;
        }
    };

    match file_name.split_once('-') {
        Some((_, gen_suffix)) => Generation::parse_suffix(gen_suffix),
        None => None,
    }
}

/// Given the key of a tenant manifest, parse out the generation number
pub fn parse_remote_tenant_manifest_path(path: RemotePath) -> Option<Generation> {
    static RE: OnceLock<Regex> = OnceLock::new();
    let re = RE.get_or_init(|| Regex::new(r".*tenant-manifest-([0-9a-f]{8}).json").unwrap());
    re.captures(path.get_path().as_str())
        .and_then(|c| c.get(1))
        .and_then(|m| Generation::parse_suffix(m.as_str()))
}

