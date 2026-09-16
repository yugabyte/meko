//! The thin lane's reaction to a renewed certificate: rebuild the generation.
//!
//! The shim reads the three paths inside [`crate::TsClient::connect`], so a
//! rebuild is all it takes. [`Recycling::new`] starts the watch, so a lane
//! configured with TLS reloads without its caller arranging anything.
//! `yb_tls` does the watching; the YSQL lane reacts differently on the same
//! signal.

use std::sync::Weak;
use std::time::Duration;

use crate::recycle::Recycling;

/// Rebuild `pool`'s generation whenever one of `paths` is rewritten. Holds
/// the pool weakly, so the watch ends when the pool is dropped rather than
/// keeping it alive.
pub(crate) fn spawn<T: Send + Sync + 'static>(
    pool: Weak<Recycling<T>>,
    paths: Vec<String>,
    what: String,
    interval: Duration,
) {
    yb_tls::watch(paths, interval, move |changed| {
        let Some(pool) = pool.upgrade() else {
            return false;
        };
        if !changed.is_empty() {
            tracing::info!("TLS reload ({what}): {changed:?} changed; rebuilding on next use");
            pool.recycle_now();
        }
        true
    });
}
