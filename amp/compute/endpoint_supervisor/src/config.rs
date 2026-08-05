//! Rendering `pageserver.toml` and `safekeeper.toml`, with operator overrides
//! merged in.
//!
//! Two properties hold by construction:
//!
//! 1. **A duplicated key is unrepresentable.** The document is a `toml_edit`
//!    table keyed by name, so an override replaces the derived value in place.
//!    This matters because toml forbids defining a key twice — `toml_edit`, which
//!    both daemons parse with, rejects such a document outright, so a key emitted
//!    twice would stop the daemon starting rather than letting the last one win.
//!    (`postgresql.conf` is the opposite: there, appending overrides.)
//! 2. **The result is type-checked against what the daemon parses.** Each render
//!    deserializes its own output into the daemon's config struct
//!    (`pageserver_api::config::ConfigToml`,
//!    `safekeeper_api::config::SafekeeperConfigToml`) before returning it, so a
//!    wrong-typed override — `scale_factor = "8"` instead of `8`, which is what
//!    hand-written jsonb produces — fails here with a clear error instead of at
//!    daemon startup.
//!
//! Rendering stays minimal rather than serializing a full defaulted struct: the
//! daemons fill their own defaults, and emitting every field would freeze today's
//! defaults into every endpoint's config file.

use std::collections::BTreeMap;

use anyhow::{Context, Result, bail};
use toml_edit::{DocumentMut, Item, Value};

/// Keys an override may never touch: the pod's identity and wiring, not tuning.
/// Overriding `branch_id` would point the endpoint at another branch's data;
/// overriding a listen address or the YB connection would unbind it from the rest
/// of the pod. Tuning knobs are open by design — this is the whole deny-list.
pub const PROTECTED_KEYS: &[&str] = &[
    "branch_id",
    "pg_distrib_dir",
    "listen_pg_addr",
    "listen_http_addr",
    "listen_grpc_addr",
    "listen_pg",
    "listen_http",
    "safekeeper_pg_addr",
    "control_plane_api",
    "yb_storage_conn",
];

/// An override value as it arrives from the registry's jsonb.
#[derive(Debug, Clone, PartialEq)]
pub enum OverrideValue {
    Int(i64),
    Float(f64),
    Bool(bool),
    Str(String),
}

impl OverrideValue {
    fn into_value(self) -> Value {
        match self {
            // Numbers and bools go in bare: a quoted scale_factor would not
            // deserialize as f64.
            Self::Int(v) => Value::from(v),
            Self::Float(v) => Value::from(v),
            Self::Bool(v) => Value::from(v),
            Self::Str(v) => Value::from(v),
        }
    }
}

/// Which daemon a document is for. Decides the type the render is checked
/// against, and so which keys are even legal.
#[derive(Debug, Clone, Copy, PartialEq, Eq)]
pub enum Daemon {
    Pageserver,
    Safekeeper,
}

/// Applies `overrides` over `base` and renders the document.
///
/// `base` is what the supervisor derived, as already-typed toml values. Both maps
/// are keyed by name, so no key can appear twice in the output.
pub fn render(
    daemon: Daemon,
    base: Vec<(&str, Value)>,
    overrides: &BTreeMap<String, OverrideValue>,
) -> Result<String> {
    let mut doc = DocumentMut::new();
    for (key, value) in base {
        doc.insert(key, Item::Value(value));
    }
    for (key, value) in overrides {
        if PROTECTED_KEYS.contains(&key.as_str()) {
            // Refused, not merged: an override must not be able to repoint an
            // endpoint at another branch's data.
            tracing::warn!(key = %key, "refusing override of an identity/wiring key");
            continue;
        }
        let replaced = doc.contains_key(key);
        doc.insert(key, Item::Value(value.clone().into_value()));
        tracing::info!(key = %key, replaced, "applied override");
    }
    let rendered = doc.to_string();

    // Type-check against the struct the daemon actually parses, so a wrong-typed
    // override fails here rather than in the daemon's startup path.
    match daemon {
        Daemon::Pageserver => {
            toml_edit::de::from_str::<pageserver_api::config::ConfigToml>(&rendered)
                .context("rendered pageserver.toml does not deserialize as ConfigToml")?;
        }
        Daemon::Safekeeper => {
            toml_edit::de::from_str::<safekeeper_api::config::SafekeeperConfigToml>(&rendered)
                .context("rendered safekeeper.toml does not deserialize as SafekeeperConfigToml")?;
        }
    }
    if rendered.trim().is_empty() {
        bail!("rendered an empty config document");
    }
    Ok(rendered)
}

#[cfg(test)]
mod tests {
    use super::*;

    /// A real 32-hex BranchId: ConfigToml validates the format, so a made-up
    /// value fails the render (which the type check correctly caught).
    const BRANCH: &str = "9a054903242f46bca0c9cc6b91a37e4c";

    fn overrides(pairs: &[(&str, OverrideValue)]) -> BTreeMap<String, OverrideValue> {
        pairs
            .iter()
            .map(|(k, v)| ((*k).to_string(), v.clone()))
            .collect()
    }

    fn ps_base() -> Vec<(&'static str, Value)> {
        vec![
            ("branch_id", Value::from(BRANCH)),
            ("pg_distrib_dir", Value::from("/usr/local")),
            ("listen_pg_addr", Value::from("127.0.0.1:64000")),
            ("scale_factor", Value::from(4.0)),
        ]
    }

    fn keys(rendered: &str) -> Vec<String> {
        rendered
            .lines()
            .filter(|l| l.contains('='))
            .map(|l| l.split('=').next().unwrap().trim().to_string())
            .collect()
    }

    /// An override replaces the derived value in place. toml forbids a duplicated
    /// key, so appending would produce a document the daemon refuses to parse.
    #[test]
    fn override_replaces_and_cannot_duplicate_a_key() {
        let out = render(
            Daemon::Pageserver,
            ps_base(),
            &overrides(&[("scale_factor", OverrideValue::Int(8))]),
        )
        .unwrap();
        let k = keys(&out);
        let mut sorted = k.clone();
        sorted.sort();
        sorted.dedup();
        assert_eq!(k.len(), sorted.len(), "duplicate key in {out}");
        assert!(out.contains("scale_factor = 8"), "{out}");
        assert!(!out.contains("scale_factor = 4.0"), "{out}");
    }

    #[test]
    fn identity_and_wiring_keys_are_refused() {
        let out = render(
            Daemon::Pageserver,
            ps_base(),
            &overrides(&[
                ("branch_id", OverrideValue::Str("someone-elses".into())),
                ("listen_pg_addr", OverrideValue::Str("0.0.0.0:1".into())),
            ]),
        )
        .unwrap();
        assert!(out.contains(BRANCH), "{out}");
        assert!(
            out.contains("listen_pg_addr = \"127.0.0.1:64000\""),
            "{out}"
        );
        assert!(!out.contains("someone-elses"), "{out}");
    }

    /// A new tuning key the base did not set is added, not dropped.
    #[test]
    fn an_unseen_tuning_key_is_added() {
        let out = render(
            Daemon::Pageserver,
            ps_base(),
            &overrides(&[("wait_lsn_timeout", OverrideValue::Str("30s".into()))]),
        )
        .unwrap();
        assert!(out.contains("wait_lsn_timeout = \"30s\""), "{out}");
    }

    /// The type check earns its place here: a quoted number is what an operator
    /// writing jsonb by hand produces, and the daemon would only fail at startup.
    #[test]
    fn a_wrong_typed_override_is_rejected_at_render_time() {
        let err = render(
            Daemon::Pageserver,
            ps_base(),
            &overrides(&[("scale_factor", OverrideValue::Str("8".into()))]),
        )
        .expect_err("a string scale_factor must not render");
        let msg = format!("{err:#}");
        assert!(msg.contains("ConfigToml"), "unexpected error: {msg}");
    }

    #[test]
    fn safekeeper_documents_are_checked_against_their_own_type() {
        let base = vec![
            ("branch_id", Value::from(BRANCH)),
            ("listen_pg", Value::from("127.0.0.1:5454")),
            ("listen_http", Value::from("127.0.0.1:7676")),
        ];
        let out = render(
            Daemon::Safekeeper,
            base.clone(),
            &overrides(&[("wal_cache_size_mib", OverrideValue::Int(512))]),
        )
        .unwrap();
        assert!(out.contains("wal_cache_size_mib = 512"), "{out}");

        // A pageserver-only key is not silently accepted into safekeeper.toml:
        // SafekeeperConfigToml denies unknown fields.
        let err = render(
            Daemon::Safekeeper,
            base,
            &overrides(&[("walredo_pool_size", OverrideValue::Int(4))]),
        )
        .expect_err("an unknown safekeeper key must be rejected");
        assert!(format!("{err:#}").contains("SafekeeperConfigToml"));
    }

    #[test]
    fn bools_and_floats_render_bare() {
        let out = render(
            Daemon::Safekeeper,
            vec![("branch_id", Value::from(BRANCH))],
            &overrides(&[
                ("current_thread_runtime", OverrideValue::Bool(true)),
                ("max_global_disk_usage_ratio", OverrideValue::Float(0.5)),
            ]),
        )
        .unwrap();
        assert!(out.contains("current_thread_runtime = true"), "{out}");
        assert!(out.contains("max_global_disk_usage_ratio = 0.5"), "{out}");
    }
}
