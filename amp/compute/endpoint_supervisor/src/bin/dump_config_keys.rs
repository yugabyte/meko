//! Emits the override keys the two daemons accept, as JSON, for the controller's
//! API to validate against. The result is checked in, so building the controller
//! needs no Rust; CI regenerates and diffs it.
//!
//! Generated rather than hand-maintained in Go because the valid key set is
//! defined by Rust types the controller cannot see, and an allowlist would drift
//! from them silently in either direction.
//!
//! Keys are dotted paths, matching the override column's key form, each with the
//! toml scalar type it takes so the API can reject a wrong-typed value too.
//!
//! Most come from walking each type's serialized `Default`. That misses `Option`
//! fields which skip serializing when `None`, so those are listed in `EXTRA_KEYS`
//! — verified, not trusted; see `verify_extra`. The list is therefore incomplete
//! rather than wrong: an `Option` nobody has needed yet is absent and the API
//! rejects it, which is the safe direction to fail.

use std::collections::BTreeMap;

use anyhow::{Context, Result};
use toml_edit::{DocumentMut, Item, Value};

// Included by path rather than through a lib target, so `PROTECTED_KEYS` keeps a
// single definition without restructuring this crate into a lib.
#[path = "../protected_keys.rs"]
mod protected_keys;

/// One daemon's accepted overrides: dotted path -> the toml scalar type it takes.
type Keys = BTreeMap<String, &'static str>;

/// Which daemon an extra key belongs to.
#[derive(Clone, Copy, PartialEq, Eq)]
enum Daemon {
    Pageserver,
    Safekeeper,
}

/// Real paths the `Default` walk cannot see, because the field is an `Option` that
/// skips serializing when `None`.
///
/// `(daemon, dotted path, json type name, toml probe value)`. The probe is what
/// proves the entry — see `verify_extra`. Add a key when someone needs it.
const EXTRA_KEYS: &[(Daemon, &str, &str, &str)] = &[
    // The recent-WAL cache; `None` derives it from scale_factor.
    (Daemon::Safekeeper, "wal_cache_size_mib", "integer", "512"),
    (Daemon::Safekeeper, "max_delta_for_fanout", "integer", "64"),
    // Durability off for throwaway perf runs; never for anything real.
    (Daemon::Pageserver, "no_sync", "boolean", "true"),
    (
        Daemon::Pageserver,
        "validate_wal_contiguity",
        "boolean",
        "true",
    ),
    (
        Daemon::Pageserver,
        "enable_read_path_debugging",
        "boolean",
        "true",
    ),
];

fn main() -> Result<()> {
    let mut pageserver = keys_of(
        &toml_edit::ser::to_string_pretty(&pageserver_api::config::ConfigToml::default())
            .context("serializing ConfigToml's default")?,
    )
    .context("walking ConfigToml")?;
    let mut safekeeper = keys_of(
        &toml_edit::ser::to_string_pretty(&safekeeper_api::config::SafekeeperConfigToml::default())
            .context("serializing SafekeeperConfigToml's default")?,
    )
    .context("walking SafekeeperConfigToml")?;

    for (daemon, path, ty, probe) in EXTRA_KEYS {
        verify_extra(*daemon, path, probe)
            .with_context(|| format!("EXTRA_KEYS entry {path:?} is not a valid override"))?;
        let target = match daemon {
            Daemon::Pageserver => &mut pageserver,
            Daemon::Safekeeper => &mut safekeeper,
        };
        if target.insert((*path).to_string(), ty).is_some() {
            // It serializes at its default after all, so the walk already had it
            // and the entry is dead weight that will rot unnoticed.
            anyhow::bail!(
                "EXTRA_KEYS entry {path:?} is already found by the Default walk; drop it"
            );
        }
    }

    // Serialized by hand so the output is stable and diffable in review.
    let mut out = String::new();
    out.push_str("{\n");
    // The exact regeneration command, so the CI failure is self-servicing.
    out.push_str(
        "  \"_generated_by\": \"(cd amp && cargo run -q -p endpoint_supervisor \
         --bin dump-config-keys > controller/internal/api/daemon_config_keys.json)\",\n",
    );
    out.push_str("  \"protected\": [\n");
    for (i, key) in protected_keys::PROTECTED_KEYS.iter().enumerate() {
        let comma = if i + 1 == protected_keys::PROTECTED_KEYS.len() {
            ""
        } else {
            ","
        };
        out.push_str(&format!("    {}{}\n", quote(key), comma));
    }
    out.push_str("  ],\n");
    out.push_str(&section("pageserver", &pageserver));
    out.push_str(",\n");
    out.push_str(&section("safekeeper", &safekeeper));
    out.push('\n');
    out.push_str("}\n");
    print!("{out}");
    Ok(())
}

fn section(name: &str, keys: &Keys) -> String {
    let mut out = format!("  {}: {{\n", quote(name));
    for (i, (path, ty)) in keys.iter().enumerate() {
        let comma = if i + 1 == keys.len() { "" } else { "," };
        out.push_str(&format!("    {}: {}{}\n", quote(path), quote(ty), comma));
    }
    out.push_str("  }");
    out
}

fn quote(s: &str) -> String {
    // The key space is Rust identifiers and dots; nothing here needs escaping,
    // but assert that rather than assuming it.
    assert!(
        !s.contains(['"', '\\', '\n']),
        "key needs json escaping: {s:?}"
    );
    format!("\"{s}\"")
}

/// Proves an `EXTRA_KEYS` entry is a real override the daemon accepts.
///
/// Deserializing is the whole check for the safekeeper, whose type denies unknown
/// fields. `ConfigToml` does not, so a bad pageserver key would deserialize
/// happily and be dropped; that is caught the way the daemon catches it at
/// startup — re-serialize and require the path to have survived.
fn verify_extra(daemon: Daemon, path: &str, probe: &str) -> Result<()> {
    let doc = format!("{path} = {probe}\n");
    match daemon {
        Daemon::Safekeeper => {
            toml_edit::de::from_str::<safekeeper_api::config::SafekeeperConfigToml>(&doc)
                .context("rejected by SafekeeperConfigToml")?;
        }
        Daemon::Pageserver => {
            let parsed = toml_edit::de::from_str::<pageserver_api::config::ConfigToml>(&doc)
                .context("rejected by ConfigToml")?;
            let round_tripped = toml_edit::ser::to_string_pretty(&parsed)
                .context("re-serializing the parsed ConfigToml")?;
            if !keys_of(&round_tripped)?.contains_key(path) {
                anyhow::bail!(
                    "did not survive a ConfigToml round trip — it is dropped as an unknown \
                     field, which the pageserver would not report either"
                );
            }
        }
    }
    Ok(())
}

/// Collects every scalar leaf of a rendered toml document as a dotted path.
fn keys_of(rendered: &str) -> Result<Keys> {
    let doc: DocumentMut = rendered.parse()?;
    let mut keys = Keys::new();
    walk_table(doc.as_table(), &mut Vec::new(), &mut keys);
    Ok(keys)
}

fn walk_table(table: &dyn toml_edit::TableLike, stack: &mut Vec<String>, keys: &mut Keys) {
    for (name, item) in table.iter() {
        stack.push(name.to_string());
        walk_item(item, stack, keys);
        stack.pop();
    }
}

fn walk_item(item: &Item, stack: &mut Vec<String>, keys: &mut Keys) {
    match item {
        Item::Table(t) => walk_table(t, stack, keys),
        Item::Value(Value::InlineTable(t)) => walk_table(t, stack, keys),
        Item::Value(v) => {
            if let Some(ty) = scalar_type(v) {
                keys.insert(stack.join("."), ty);
            }
            // Arrays and array-of-tables are deliberately dropped: an override
            // value is a json scalar, so there is no way to express one and the
            // API should reject it rather than list it as settable.
        }
        Item::ArrayOfTables(_) | Item::None => {}
    }
}

fn scalar_type(v: &Value) -> Option<&'static str> {
    match v {
        Value::Boolean(_) => Some("boolean"),
        Value::Integer(_) => Some("integer"),
        Value::Float(_) => Some("float"),
        Value::String(_) => Some("string"),
        // A toml datetime cannot be produced from json either.
        Value::Datetime(_) | Value::Array(_) | Value::InlineTable(_) => None,
    }
}
