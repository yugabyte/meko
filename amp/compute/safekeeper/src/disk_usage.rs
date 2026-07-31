//! Filesystem-capacity reporting + the global "out of disk" flag.

use std::sync::atomic::AtomicBool;

use anyhow::{Result, anyhow};
use once_cell::sync::Lazy;
use utils::{critical_timeline, id::BranchId};

/// true if the last background scan found total usage > limit
pub static GLOBAL_DISK_LIMIT_EXCEEDED: Lazy<AtomicBool> = Lazy::new(|| AtomicBool::new(false));

/// Returns filesystem usage in bytes for the filesystem containing the given path.
// Need to suppress the clippy::unnecessary_cast warning because the casts on the block count and the
// block size are required on macOS (they are 32-bit integers on macOS, apparantly).
#[allow(clippy::unnecessary_cast)]
pub fn get_filesystem_usage(path: &std::path::Path) -> u64 {
    // Allow overriding disk usage via failpoint for tests
    fail::fail_point!("sk-global-disk-usage", |val| {
        // val is Option<String>; parse payload if present
        val.and_then(|s| s.parse::<u64>().ok()).unwrap_or(0)
    });

    // Call statvfs(3) for filesystem usage
    use nix::sys::statvfs::statvfs;
    match statvfs(path) {
        Ok(stat) => {
            // fragment size (f_frsize) if non-zero else block size (f_bsize)
            let frsize = stat.fragment_size();
            let blocksz = if frsize > 0 {
                frsize
            } else {
                stat.block_size()
            };
            // used blocks = total blocks - available blocks for unprivileged
            let used_blocks = stat.blocks().saturating_sub(stat.blocks_available());
            used_blocks as u64 * blocksz as u64
        }
        Err(e) => {
            // The global disk usage watcher isn't associated with a branch, so we just
            // pass a placeholder (all-zero) branch ID to the critical!() macro.
            let placeholder_branch_id = BranchId::from_array([0u8; 16]);
            critical_timeline!(
                placeholder_branch_id,
                None::<&AtomicBool>,
                "Global disk usage watcher failed to read filesystem usage: {:?}",
                e
            );
            0
        }
    }
}

/// Returns the total capacity of the current working directory's filesystem in bytes.
#[allow(clippy::unnecessary_cast)]
pub fn get_filesystem_capacity(path: &std::path::Path) -> Result<u64> {
    // Call statvfs(3) for filesystem stats
    use nix::sys::statvfs::statvfs;
    match statvfs(path) {
        Ok(stat) => {
            // fragment size (f_frsize) if non-zero else block size (f_bsize)
            let frsize = stat.fragment_size();
            let blocksz = if frsize > 0 {
                frsize
            } else {
                stat.block_size()
            };
            Ok(stat.blocks() as u64 * blocksz as u64)
        }
        Err(e) => Err(anyhow!("Failed to read filesystem capacity: {:?}", e)),
    }
}
