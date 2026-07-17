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
//! branches/<branch_id>/initdb.tar.zst
//! branches/<branch_id>/initdb-preserved.tar.zst
//! ```

mod download;
mod upload;

pub use download::download_initdb_tar_zst;
pub use upload::upload_initdb_dir;

use remote_storage::RemotePath;
use utils::id::BranchId;

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

/// Prefix of all objects belonging to a branch in remote storage.
pub fn remote_branch_path(branch_id: &BranchId) -> RemotePath {
    let path = format!("branches/{branch_id}");
    RemotePath::from_string(&path).expect("Failed to construct path")
}

pub fn remote_initdb_archive_path(branch_id: &BranchId) -> RemotePath {
    RemotePath::from_string(&format!("branches/{branch_id}/{INITDB_PATH}"))
        .expect("Failed to construct path")
}

pub fn remote_initdb_preserved_archive_path(branch_id: &BranchId) -> RemotePath {
    RemotePath::from_string(&format!("branches/{branch_id}/{INITDB_PRESERVED_PATH}"))
        .expect("Failed to construct path")
}
