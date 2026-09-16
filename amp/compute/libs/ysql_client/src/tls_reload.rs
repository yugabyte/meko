//! The YSQL lane's reaction to a renewed certificate: bump the pool's reload
//! generation so its slots reopen on next use.
//!
//! `from_yb_config_pooled` starts the watch, so a pool watches its own files
//! with no wiring at the call site. The reconnect is lazy and best-effort, so
//! a reload can only add a connection, never take a working one away.
//! `yb_tls` does the watching; the thin lane reacts differently on the same
//! signal.

use std::sync::{Arc, Weak};

use crate::metrics::YB_CLIENT_CERT_NOT_AFTER_SECONDS;
use crate::{YbConnConfig, YsqlClient};

impl YbConnConfig {
    /// The TLS files this connection block reads from disk, in the order
    /// `sslrootcert`, `sslcert`, `sslkey`; empty when TLS is off.
    pub fn tls_file_paths(&self) -> Vec<String> {
        if !self.tls_enabled() {
            return Vec::new();
        }
        [&self.sslrootcert, &self.sslcert, &self.sslkey]
            .into_iter()
            .flatten()
            .cloned()
            .collect()
    }
}

/// Bump `client`'s reload generation whenever one of its TLS files is
/// rewritten. Holds the generation weakly, so the watch ends when the pool is
/// dropped rather than keeping it alive.
pub(crate) fn watch_for_client(cfg: &YbConnConfig, what: &str, client: &YsqlClient) {
    let generation: Weak<std::sync::atomic::AtomicU64> = Arc::downgrade(&client.reload_generation);
    let cfg = cfg.clone();
    let what = what.to_string();
    yb_tls::watch(
        cfg.tls_file_paths(),
        yb_tls::poll_interval(),
        move |changed| {
            let Some(generation) = generation.upgrade() else {
                return false;
            };
            if !changed.is_empty() {
                report_change(&cfg, &what, changed);
                generation.fetch_add(1, std::sync::atomic::Ordering::AcqRel);
            }
            true
        },
    );
}

fn report_change(cfg: &YbConnConfig, what: &str, changed: &[String]) {
    match cfg.inspect_client_certificate() {
        Ok(Some(info)) => {
            YB_CLIENT_CERT_NOT_AFTER_SECONDS.set(info.not_after_unix as i64);
            tracing::info!(
                "TLS reload ({what}): {changed:?} changed; client certificate now serial \
                 {} CN={} notAfter={}; reconnecting",
                info.serial,
                info.common_name.as_deref().unwrap_or("-"),
                info.not_after_unix
            );
        }
        Ok(None) => tracing::info!("TLS reload ({what}): {changed:?} changed; reconnecting"),
        Err(e) => tracing::warn!(
            "TLS reload ({what}): {changed:?} changed but the client certificate does not \
             check out ({e:#}); reconnecting anyway"
        ),
    }
}
