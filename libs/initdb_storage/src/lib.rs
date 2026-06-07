//! Upload and download the per-timeline `initdb.tar.zst` archive.
//!
//! Upstream Neon used remote storage as the durable home for all pageserver
//! state. After the yb-light slim only the initdb tarball remains there:
//! the pageserver runs `initdb` once when a new timeline is created, ships the
//! result here, and later branches reuse it via `download_initdb_tar_zst`
//! instead of paying the `initdb` cost again. Everything else
//! (timeline state, index_part, tenant manifest) lives in YB rows now.
//!
//! Path layout in remote storage:
//! ```text
//! tenants/<tenant_id>/timelines/<timeline_id>/initdb.tar.zst
//! tenants/<tenant_id>/timelines/<timeline_id>/initdb-preserved.tar.zst
//! ```

mod download;
mod upload;

pub use download::download_initdb_tar_zst;
pub use upload::upload_initdb_dir;

use camino::Utf8Path;
use remote_storage::RemotePath;
use utils::id::{TenantId, TimelineId};

pub const INITDB_PATH: &str = "initdb.tar.zst";
pub const INITDB_PRESERVED_PATH: &str = "initdb-preserved.tar.zst";

/// Streaming buffer size used by both upload and download.
pub const BUFFER_SIZE: usize = 32 * 1024;

/// Warn at this many failed download attempts (but keep retrying).
pub const FAILED_DOWNLOAD_WARN_THRESHOLD: u32 = 3;
/// Cap on retry attempts for download/upload before bailing.
pub const FAILED_REMOTE_OP_RETRIES: u32 = 10;

/// Tempfile suffix used by the download path so partial fetches are easy
/// to recognise and clean up on the next attempt.
const TEMP_FILE_SUFFIX: &str = "___temp";

const TIMELINES_SEGMENT_NAME: &str = "timelines";

/// Prefix of all objects belonging to a tenant in remote storage. Used by
/// the tenant-delete path to wipe `tenants/<tenant_id>/*` in one shot.
pub fn remote_tenant_path(tenant_shard_id: &TenantId) -> RemotePath {
    let path = format!("tenants/{tenant_shard_id}");
    RemotePath::from_string(&path).expect("Failed to construct path")
}

pub fn remote_timelines_path(tenant_shard_id: &TenantId) -> RemotePath {
    let path = format!("tenants/{tenant_shard_id}/{TIMELINES_SEGMENT_NAME}");
    RemotePath::from_string(&path).expect("Failed to construct path")
}

pub fn remote_timeline_path(tenant_shard_id: &TenantId, timeline_id: &TimelineId) -> RemotePath {
    remote_timelines_path(tenant_shard_id).join(Utf8Path::new(&timeline_id.to_string()))
}

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
