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

pub use crate::protected_keys::PROTECTED_KEYS;

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

impl Daemon {
    fn file_name(self) -> &'static str {
        match self {
            Daemon::Pageserver => "pageserver.toml",
            Daemon::Safekeeper => "safekeeper.toml",
        }
    }
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
        let root = key.split('.').next().unwrap_or(key.as_str());
        if PROTECTED_KEYS.contains(&root) {
            // Refused, not merged: an override must not be able to repoint an
            // endpoint at another branch's data.
            tracing::warn!(key = %key, "refusing override of an identity/wiring key");
            continue;
        }
        if let Err(e) = insert_path(&mut doc, key, value.clone().into_value()) {
            // Dropped rather than fatal: emitting it anyway would produce a
            // document the daemon refuses to parse.
            tracing::warn!(key = %key, error = %e, "ignoring override");
            continue;
        }
        tracing::info!(key = %key, "applied override");
    }
    let rendered = doc.to_string();

    // Type-check against the struct the daemon actually parses, so a wrong-typed
    // override fails here rather than in the daemon's startup path.
    //
    // Deserializing alone is NOT enough: neither config struct denies unknown
    // fields, so a key the daemon has never heard of parses cleanly here and
    // then aborts its startup — the pageserver diffs the on-disk keys against
    // its own re-serialization and refuses to boot on any that do not round
    // trip. So round-trip the keys here too, and fail in the supervisor's tests
    // rather than in a pod that will not come up.
    let round_tripped = match daemon {
        Daemon::Pageserver => {
            let parsed: pageserver_api::config::ConfigToml = toml_edit::de::from_str(&rendered)
                .context("rendered pageserver.toml does not deserialize as ConfigToml")?;
            toml_edit::ser::to_document(&parsed).context("re-serializing pageserver.toml")?
        }
        Daemon::Safekeeper => {
            let parsed: safekeeper_api::config::SafekeeperConfigToml = toml_edit::de::from_str(
                &rendered,
            )
            .context("rendered safekeeper.toml does not deserialize as SafekeeperConfigToml")?;
            toml_edit::ser::to_document(&parsed).context("re-serializing safekeeper.toml")?
        }
    };
    let unknown: Vec<&str> = doc
        .iter()
        .map(|(k, _)| k)
        .filter(|k| !round_tripped.contains_key(k))
        .collect();
    if !unknown.is_empty() {
        bail!(
            "rendered {} has key(s) the daemon does not define: {}. \
             A key it cannot parse aborts its startup, so this must not ship.",
            daemon.file_name(),
            unknown.join(", ")
        );
    }
    if rendered.trim().is_empty() {
        bail!("rendered an empty config document");
    }
    Ok(rendered)
}

/// Inserts `value` at a dotted `path`, creating intermediate tables.
///
/// Not `Table::insert`: that treats `tenant_config.enable_gc` as one literal key
/// and emits it quoted, which the daemon then drops as unknown.
///
/// Intermediate tables are marked dotted so the output stays `a.b = v` rather
/// than growing a `[a]` header — the derived keys are written first, and a header
/// after them would swallow everything following it into that table.
fn insert_path(doc: &mut DocumentMut, path: &str, value: Value) -> Result<()> {
    if path.is_empty() {
        bail!("empty override key");
    }
    let mut segments = path.split('.').peekable();
    let mut table = doc.as_table_mut();
    while let Some(segment) = segments.next() {
        if segment.is_empty() {
            bail!("empty segment in override key `{path}`");
        }
        if segments.peek().is_none() {
            table.insert(segment, Item::Value(value));
            return Ok(());
        }
        let entry = table.entry(segment).or_insert_with(|| {
            let mut intermediate = toml_edit::Table::new();
            intermediate.set_dotted(true);
            Item::Table(intermediate)
        });
        table = entry.as_table_mut().with_context(|| {
            format!("`{segment}` is already a value, so `{path}` cannot be set")
        })?;
    }
    bail!("override key `{path}` has no final segment")
}

#[cfg(test)]
mod tests {
    use super::*;

    /// The check that would have caught writing `require_write_token` into
    /// `pageserver.toml` before the pageserver had a field for it: the daemon
    /// aborts startup on a key it cannot parse, and deserializing here does not
    /// notice because neither config struct denies unknown fields.
    #[test]
    fn a_key_the_daemon_does_not_define_is_refused() {
        let err = render(
            Daemon::Pageserver,
            vec![
                ("branch_id", Value::from("77ffde89f43d4ca08ef0a0ed1f089eaa")),
                ("not_a_real_pageserver_key", Value::from(true)),
            ],
            &BTreeMap::new(),
        )
        .expect_err("an unknown key must not render");
        let msg = format!("{err:#}");
        assert!(msg.contains("not_a_real_pageserver_key"), "{msg}");
        assert!(msg.contains("pageserver.toml"), "{msg}");

        // Same for the safekeeper, whose struct is equally permissive.
        let err = render(
            Daemon::Safekeeper,
            vec![("not_a_real_safekeeper_key", Value::from(1))],
            &BTreeMap::new(),
        )
        .expect_err("an unknown key must not render");
        assert!(format!("{err:#}").contains("not_a_real_safekeeper_key"));

        // And the guard must not fire on a real key at either value — the
        // supervisor writes `require_write_token = false` when fencing is off,
        // and a `skip_serializing_if` on the field would drop it from the round
        // trip and make this reject a config that is perfectly valid.
        for on in [true, false] {
            render(
                Daemon::Safekeeper,
                vec![("require_write_token", Value::from(on))],
                &BTreeMap::new(),
            )
            .unwrap_or_else(|e| panic!("require_write_token = {on} must render: {e:#}"));
        }
    }

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

    /// The motivating case: the GC knobs live in `ConfigToml.tenant_config`, so a
    /// dotted path is the only way to reach them.
    #[test]
    fn a_dotted_override_lands_in_the_nested_table() {
        let out = render(
            Daemon::Pageserver,
            ps_base(),
            &overrides(&[
                ("tenant_config.enable_gc", OverrideValue::Bool(true)),
                ("tenant_config.gc_horizon", OverrideValue::Int(1024)),
            ]),
        )
        .unwrap();

        // Not quoted as a single key: `"tenant_config.enable_gc" = true` parses
        // as a top-level key whose name contains a dot, which is exactly the
        // stray key the pageserver bails on.
        assert!(!out.contains(r#""tenant_config"#), "{out}");

        let parsed: pageserver_api::config::ConfigToml = toml_edit::de::from_str(&out).unwrap();
        assert!(parsed.tenant_config.enable_gc, "{out}");
        assert_eq!(parsed.tenant_config.gc_horizon, 1024, "{out}");
    }

    /// Two paths under one root coexist: the second finds the table the first
    /// created rather than redefining it, which toml would reject.
    #[test]
    fn dotted_overrides_sharing_a_root_do_not_duplicate_it() {
        let out = render(
            Daemon::Pageserver,
            ps_base(),
            &overrides(&[
                ("tenant_config.enable_gc", OverrideValue::Bool(true)),
                ("tenant_config.gc_horizon", OverrideValue::Int(7)),
            ]),
        )
        .unwrap();
        let k = keys(&out);
        let mut sorted = k.clone();
        sorted.sort();
        sorted.dedup();
        assert_eq!(k.len(), sorted.len(), "duplicate key in {out}");
    }

    /// Protection is decided on the first segment, so a dotted path cannot be
    /// used to smuggle an identity key past the deny-list.
    #[test]
    fn a_dotted_path_under_a_protected_root_is_refused() {
        let out = render(
            Daemon::Pageserver,
            ps_base(),
            &overrides(&[(
                "branch_id.sneaky",
                OverrideValue::Str("someone-elses".into()),
            )]),
        )
        .unwrap();
        assert!(out.contains(BRANCH), "{out}");
        assert!(!out.contains("someone-elses"), "{out}");
    }

    /// Defining a name as both a value and a table is a document toml_edit
    /// rejects, so the override is dropped rather than taking the daemon down.
    #[test]
    fn a_dotted_path_colliding_with_a_scalar_is_dropped() {
        let out = render(
            Daemon::Pageserver,
            ps_base(),
            &overrides(&[("scale_factor.nested", OverrideValue::Int(1))]),
        )
        .unwrap();
        assert!(out.contains("scale_factor = 4.0"), "{out}");
        assert!(!out.contains("nested"), "{out}");
    }
}
