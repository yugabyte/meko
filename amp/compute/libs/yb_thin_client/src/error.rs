//! Error taxonomy, mirroring the shim's `ybthin_status_code` and the
//! `ysql_client` retry classes. A read-restart is kept distinct so a
//! paged scan can be restarted (the shim reports it; it can't be healed
//! by re-issuing the same continuation).

use crate::ffi;

#[derive(Debug, Clone, thiserror::Error)]
pub enum YbError {
    /// Bad arguments / misuse — never retry.
    #[error("yb_thin_client invalid: {0}")]
    Invalid(String),
    /// Transport loss, timeout, or lost session — retry after reconnect.
    #[error("yb_thin_client network: {0}")]
    Network(String),
    /// Transient server condition (leader not ready, busy) — retry with backoff.
    #[error("yb_thin_client try-again: {0}")]
    TryAgain(String),
    /// `restart_read_time`: restart the whole scan on a fresh read point.
    #[error("yb_thin_client read-restart: {0}")]
    ReadRestart(String),
    /// Table schema-version mismatch — reopen the table.
    #[error("yb_thin_client schema mismatch: {0}")]
    Schema(String),
    /// Anything else — fatal.
    #[error("yb_thin_client error: {0}")]
    Other(String),
    /// The row's `ignore_after_hybrid_time` fence had passed, so its leader
    /// refused it. That says at least one row was refused, not that nothing
    /// landed: a batch spanning tablets is judged per tablet and may be partly
    /// applied, and `upsert` can return this after an earlier attempt already
    /// committed but failed with `Network`. Never retried here: re-sending
    /// under the same deadline is refused again, and whether a renewed lease
    /// makes a new deadline safe is the caller's call (`utils::write_fence`).
    #[error("yb_thin_client fenced: {0}")]
    Fenced(String),
}

impl YbError {
    pub fn is_retryable(&self) -> bool {
        matches!(
            self,
            YbError::Network(_) | YbError::TryAgain(_) | YbError::ReadRestart(_)
        )
    }

    pub fn is_read_restart(&self) -> bool {
        matches!(self, YbError::ReadRestart(_))
    }

    /// The leader refused the write because its deadline had passed.
    pub fn is_fenced(&self) -> bool {
        matches!(self, YbError::Fenced(_))
    }
}

/// Consume a status returned by the shim: free its owned message and map
/// it to `Ok(())` / `Err(YbError)`.
///
/// # Safety
/// `status` must be a valid `YbStatus` produced by the shim (its
/// `message`, if non-null, is a shim-owned C string freed here exactly
/// once).
pub(crate) unsafe fn check(status: ffi::YbStatus) -> Result<(), YbError> {
    let message = if status.message.is_null() {
        String::new()
    } else {
        let s = unsafe { std::ffi::CStr::from_ptr(status.message) }
            .to_string_lossy()
            .into_owned();
        unsafe { ffi::ybthin_string_free(status.message) };
        s
    };
    use ffi::YbStatusCode::*;
    match status.code {
        Ok => Result::Ok(()),
        Invalid => Err(YbError::Invalid(message)),
        Network => Err(YbError::Network(message)),
        TryAgain => Err(YbError::TryAgain(message)),
        ReadRestart => Err(YbError::ReadRestart(message)),
        Schema => Err(YbError::Schema(message)),
        Other => Err(YbError::Other(message)),
        Fenced => Err(YbError::Fenced(message)),
    }
}
