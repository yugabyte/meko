//! Hard-abort the process with a logged reason.
//!
//! Use [`fatal_abort`] whenever you detect state the process must not
//! continue running with — data corruption, ser/des mismatch, walredo
//! PANIC, broken invariants. The function:
//!
//! 1. Logs the caller-supplied reason at `tracing::error!`.
//! 2. Best-effort flushes `stdout` + `stderr` so the line survives
//!    `abort()` even with line-buffered captured output.
//! 3. Calls [`std::process::abort`] — no destructors, no thread
//!    join, no chance to write any more bytes from this process.
//!
//! The signature takes `impl Display` so callers can pass a `&str`
//! literal, a `String`, or `format_args!(...)` to interleave
//! structured context with the message.
//!
//! ```ignore
//! utils::abort::fatal_abort(format_args!(
//!     "walredo failed: tenant={tid} key={key:?}: {err:#}"
//! ));
//! ```
//!
//! For routine errors that the system can recover from (transient YB
//! write failure, retryable HTTP call) keep using `tracing::error!`
//! + a counter — `fatal_abort` is reserved for "this run is over".

use std::fmt::Display;
use std::io::Write;

/// Log `reason` at error level, flush stdio, then `std::process::abort`.
pub fn fatal_abort(reason: impl Display) -> ! {
    tracing::error!("ABORT: {reason}");
    let _ = std::io::stdout().flush();
    let _ = std::io::stderr().flush();
    std::process::abort()
}
