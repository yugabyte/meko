//! Originally a layer-file FD cache (the `VirtualFile` clock-LRU pool)
//! plus a full async read/write API. The yb-pageserver fork moved
//! every page to YugabyteDB, so the layer stack is gone and the only
//! surviving on-disk write is `Tenant::persist_tenant_config_at`
//! calling `VirtualFile::crashsafe_overwrite` (~1 call per tenant per
//! attachment change).
//!
//! What's left here:
//!
//! - [`VirtualFile::crashsafe_overwrite`] — async wrapper around
//!   [`utils::crashsafe::overwrite`], bypasses any FD pool.
//! - [`PAGE_SZ`] — 8 KiB constant still consumed by `walredo::process`.
//! - [`is_fatal_io_error`] / [`on_fatal_io_error`] / [`MaybeFatalIo`]
//!   — IO-error classification + abort helpers used across the
//!   pageserver to terminate on EIO/EACCES/EROFS.

use camino::Utf8PathBuf;
use tokio_epoll_uring::{BoundedBuf, IoBuf};

/// Size of a Postgres page. yb-pageserver does not use the legacy
/// page cache, but this constant still parameterises a few walredo
/// alignment / capacity calls.
pub const PAGE_SZ: usize = 8192;

/// Namespace for the surviving on-disk write call. Kept as a unit
/// struct rather than a free function so the call site
/// `VirtualFile::crashsafe_overwrite(...)` reads identically to its
/// upstream form — easier to spot if a future change re-grows the
/// surface.
pub struct VirtualFile;

impl VirtualFile {
    /// Async wrapper around [`utils::crashsafe::overwrite`]: write
    /// `content` to `tmp_path`, fsync it, rename it over `final_path`,
    /// fsync the parent dir. The rename is atomic; a mid-write crash
    /// leaves `final_path` either untouched or fully updated.
    ///
    /// Fatal IO errors abort the process via [`on_fatal_io_error`].
    pub async fn crashsafe_overwrite<B: BoundedBuf<Buf = Buf> + Send, Buf: IoBuf + Send>(
        final_path: Utf8PathBuf,
        tmp_path: Utf8PathBuf,
        content: B,
    ) -> std::io::Result<()> {
        tokio::task::spawn_blocking(move || {
            let slice_storage;
            let content_len = content.bytes_init();
            let content = if content.bytes_init() > 0 {
                slice_storage = Some(content.slice(0..content_len));
                slice_storage.as_deref().expect("just set it to Some()")
            } else {
                &[]
            };
            utils::crashsafe::overwrite(&final_path, &tmp_path, content)
                .maybe_fatal_err("crashsafe_overwrite")
        })
        .await
        .expect("blocking task is never aborted")
    }
}

/// No-op kept as the call site for any future re-introduction of
/// pageserver-local disk artifacts. yb-mode has nothing to wire up
/// here — the FD pool is gone, the `io_engine` choice is recorded in
/// `PageServerConf` for diagnostics only, and `crashsafe_overwrite`
/// uses blocking `std::fs` directly.
#[cfg(not(test))]
pub fn init() {}

/// Identify error types that should always terminate the process.
/// Other error types may be eligible for retry.
pub(crate) fn is_fatal_io_error(e: &std::io::Error) -> bool {
    use nix::errno::Errno::*;
    match e.raw_os_error().map(nix::errno::Errno::from_raw) {
        // Terminate on EIO because we no longer trust the device to
        // store data safely, or to uphold persistence guarantees on
        // fsync.
        Some(EIO) => true,
        // Terminate on EROFS because a filesystem is usually
        // remounted readonly when it has experienced some critical
        // issue, so the same logic as EIO applies.
        Some(EROFS) => true,
        // Terminate on EACCES because we should always have
        // permissions for our own data dir: if we don't, then we
        // can't do our job and need administrative intervention.
        Some(EACCES) => true,
        // Other errors (ENOSPC, EINVAL, EBADF, Interrupted, …) are
        // retryable from this module's perspective.
        _ => false,
    }
}

/// Call this when the local filesystem gives us an error with an
/// external cause (EIO / EROFS / EACCES): all these indicate bad
/// storage or bad configuration, neither of which we can fix from
/// inside a running process.
pub(crate) fn on_fatal_io_error(e: &std::io::Error, context: &str) -> ! {
    let backtrace = std::backtrace::Backtrace::force_capture();
    tracing::error!("Fatal I/O error: {e}: {context})\n{backtrace}");
    std::process::abort();
}

pub(crate) trait MaybeFatalIo<T> {
    fn maybe_fatal_err(self, context: &str) -> std::io::Result<T>;
    fn fatal_err(self, context: &str) -> T;
}

impl<T> MaybeFatalIo<T> for std::io::Result<T> {
    /// Terminate the process if the result is an error of a fatal
    /// type, else pass it through. Appropriate for writes, where we
    /// die on EIO/ACCES but not on ENOSPC.
    fn maybe_fatal_err(self, context: &str) -> std::io::Result<T> {
        if let Err(e) = &self {
            if is_fatal_io_error(e) {
                on_fatal_io_error(e, context);
            }
        }
        self
    }

    /// Terminate the process on any I/O error. Appropriate for reads
    /// on files that we know exist: they should always work.
    fn fatal_err(self, context: &str) -> T {
        match self {
            Ok(v) => v,
            Err(e) => on_fatal_io_error(&e, context),
        }
    }
}

#[cfg(test)]
mod tests {
    use super::*;

    #[tokio::test]
    async fn test_atomic_overwrite_basic() {
        let testdir = crate::config::PageServerConf::test_repo_dir("test_atomic_overwrite_basic");
        std::fs::create_dir_all(&testdir).unwrap();

        let path = testdir.join("myfile");
        let tmp_path = testdir.join("myfile.tmp");

        VirtualFile::crashsafe_overwrite(path.clone(), tmp_path.clone(), b"foo".to_vec())
            .await
            .unwrap();

        let post = std::fs::read_to_string(&path).unwrap();
        assert_eq!(post, "foo");
        assert!(!tmp_path.exists());

        VirtualFile::crashsafe_overwrite(path.clone(), tmp_path.clone(), b"bar".to_vec())
            .await
            .unwrap();

        let post = std::fs::read_to_string(&path).unwrap();
        assert_eq!(post, "bar");
        assert!(!tmp_path.exists());
    }

    #[tokio::test]
    async fn test_atomic_overwrite_preexisting_tmp() {
        let testdir =
            crate::config::PageServerConf::test_repo_dir("test_atomic_overwrite_preexisting_tmp");
        std::fs::create_dir_all(&testdir).unwrap();

        let path = testdir.join("myfile");
        let tmp_path = testdir.join("myfile.tmp");

        std::fs::write(&tmp_path, "some preexisting junk that should be removed").unwrap();
        assert!(tmp_path.exists());

        VirtualFile::crashsafe_overwrite(path.clone(), tmp_path.clone(), b"foo".to_vec())
            .await
            .unwrap();

        let post = std::fs::read_to_string(&path).unwrap();
        assert_eq!(post, "foo");
        assert!(!tmp_path.exists());
    }
}
