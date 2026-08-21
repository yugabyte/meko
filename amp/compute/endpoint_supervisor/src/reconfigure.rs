//! Applying a compute settings change without bouncing Postgres.
//!
//! Rewriting the spec is not what costs anything — restarting `compute_ctl` is,
//! because that takes Postgres down and every connection with it. A settings
//! change did that unconditionally, which made tuning one GUC on a branch an
//! outage on every endpoint of that branch.
//!
//! Postgres already answers which settings need a restart. `pg_settings.context`
//! says when each one can change, and only three of its values mean "while the
//! server is up": `sighup`, `superuser` and `user`. So the decision is not a
//! hand-kept list that drifts from the server — it is a question put to the
//! running server about the settings that actually changed.
//!
//! Reloadable is a whitelist, deliberately. The other contexts are not all
//! `postmaster`: `internal` settings like `block_size` are compiled in and can
//! never change, and `backend` / `superuser-backend` are fixed for the life of a
//! connection. Treating "not postmaster" as reloadable would report those as
//! applied when nothing can apply them.
//!
//! When every changed setting is reloadable, `compute_ctl` gets a SIGHUP, re-reads
//! the spec, rewrites `postgresql.conf` and calls `pg_reload_conf()`. Connections
//! stay up.
//!
//! Some settings nothing applies, and those are left alone rather than restarted
//! for. `internal` settings like `block_size` are compiled in, and a name
//! Postgres does not report at all — a typo, or one from an extension that is
//! not loaded — cannot be applied by any means. Restarting on their account
//! would be an interruption that changes nothing, so they are reported as not
//! taking effect instead.

use std::collections::BTreeMap;

use anyhow::{Context, Result};
use serde_json::Value;

/// What applying a compute change requires.
#[derive(Debug, PartialEq, Eq)]
pub enum Apply {
    /// Nothing changed for the compute.
    Nothing,
    /// Reload: the changed settings all take effect without a restart.
    Reload,
    /// Restart, and why — the settings that are fixed at server start.
    Restart { because: Vec<String> },
}

/// The `{name: value}` a spec's settings list amounts to.
///
/// `compute_ctl` writes them into `postgresql.conf` in order and Postgres takes
/// the last of a repeated name, so the last occurrence is the effective value —
/// which is what a diff has to compare.
pub fn settings_of(spec: &Value) -> BTreeMap<String, String> {
    let mut out = BTreeMap::new();
    if let Some(list) = spec["spec"]["cluster"]["settings"].as_array() {
        for s in list {
            if let Some(name) = s["name"].as_str() {
                out.insert(
                    name.to_string(),
                    s["value"].as_str().unwrap_or_default().to_string(),
                );
            }
        }
    }
    out
}

/// The settings whose effective value differs between two specs, including ones
/// added or removed — dropping a setting changes what Postgres runs with just
/// as much as changing it.
pub fn changed_settings(old: &Value, new: &Value) -> Vec<String> {
    let (a, b) = (settings_of(old), settings_of(new));
    let mut names: Vec<String> = a
        .keys()
        .chain(b.keys())
        .filter(|k| a.get(*k) != b.get(*k))
        .cloned()
        .collect();
    names.sort();
    names.dedup();
    names
}

/// Decide from the changed settings and the set Postgres calls `postmaster`.
///
/// Split from the query so the rule is testable without a running Postgres —
/// the rule is the part worth pinning, and the query is one statement.
pub fn decide(changed: &[String], sorted: &Classified) -> Apply {
    if changed.is_empty() {
        return Apply::Nothing;
    }
    if sorted.restart.is_empty() {
        Apply::Reload
    } else {
        Apply::Restart {
            because: sorted.restart.clone(),
        }
    }
}

/// Ask the running Postgres what would apply each of `names`.
pub fn classify_settings(
    psql: &camino::Utf8Path,
    port: u16,
    names: &[String],
) -> Result<Classified> {
    if names.is_empty() {
        return Ok(Classified::default());
    }
    Ok(classify(names, &contexts(psql, port)?))
}

/// The contexts a setting can change in while the server is running, or at the
/// next connection — either way a reload of the config file applies it.
const RELOADABLE: [&str; 5] = [
    "sighup",
    "superuser",
    "user",
    "backend",
    "superuser-backend",
];

/// What a set of changed settings needs.
#[derive(Debug, Default, PartialEq, Eq)]
pub struct Classified {
    /// Fixed at server start: only a restart applies these.
    pub restart: Vec<String>,
    /// Nothing applies these, so nothing is done for them. `internal` settings
    /// are compiled in, and a name Postgres does not report at all cannot be
    /// applied by any means — restarting for either would be an interruption
    /// that changes nothing.
    pub inapplicable: Vec<String>,
}

/// Sort changed settings by what would actually apply them.
///
/// Split from the query so the rule is testable without a running Postgres —
/// it is the part that decides whether an endpoint gets interrupted.
pub fn classify(names: &[String], known: &BTreeMap<String, String>) -> Classified {
    let mut out = Classified::default();
    for name in names {
        match known.get(name) {
            Some(c) if RELOADABLE.contains(&c.as_str()) => {}
            Some(c) if c == "postmaster" => out.restart.push(name.clone()),
            // `internal` (compiled in), or a context Postgres grows later that
            // this does not know: not reloadable, and not shown to be fixable by
            // a restart either.
            Some(_) => out.inapplicable.push(name.clone()),
            None => out.inapplicable.push(name.clone()),
        }
    }
    out
}

/// Every setting Postgres knows, and when it can change.
///
/// The whole table rather than a filtered query: it is a few hundred small rows
/// read once per settings change, and having every name means an unknown one is
/// distinguishable from a reloadable one — which is the distinction the caller
/// turns into "restart" rather than "assume it is fine".
fn contexts(psql: &camino::Utf8Path, port: u16) -> Result<BTreeMap<String, String>> {
    let mut cmd = std::process::Command::new(psql);
    cmd.args([
        "-h",
        "127.0.0.1",
        "-p",
        &port.to_string(),
        "-U",
        "cloud_admin",
        "-d",
        "postgres",
        "-v",
        "ON_ERROR_STOP=1",
        "--no-psqlrc",
        "-tAF",
        "\t",
        "-c",
        "SELECT name, context FROM pg_settings",
    ]);
    // Bounded on both axes: this runs while deciding whether to interrupt the
    // endpoint, so a wedged Postgres must not hold that decision open.
    cmd.env("PGCONNECT_TIMEOUT", "3");
    cmd.env("PGOPTIONS", "-c statement_timeout=5000");
    let out = cmd.output().context("run psql for pg_settings")?;
    if !out.status.success() {
        anyhow::bail!(
            "reading pg_settings failed: {}",
            String::from_utf8_lossy(&out.stderr).trim()
        );
    }
    Ok(String::from_utf8_lossy(&out.stdout)
        .lines()
        .filter_map(|line| {
            let (name, context) = line.split_once('\t')?;
            Some((name.trim().to_string(), context.trim().to_string()))
        })
        .collect())
}

#[cfg(test)]
mod tests {
    use super::*;
    use serde_json::json;

    fn spec(settings: &[(&str, &str)]) -> Value {
        json!({"spec": {"cluster": {"settings":
            settings.iter().map(|(n, v)| json!({"name": n, "value": v})).collect::<Vec<_>>()}}})
    }

    #[test]
    fn the_last_value_of_a_repeated_setting_is_the_effective_one() {
        // compute_ctl writes them in order and Postgres takes the last, so a
        // diff that read the first would compare something nothing runs with.
        let s = spec(&[("work_mem", "16MB"), ("work_mem", "64MB")]);
        assert_eq!(settings_of(&s)["work_mem"], "64MB");
    }

    #[test]
    fn a_changed_added_or_removed_setting_all_count() {
        let a = spec(&[("work_mem", "16MB"), ("gone", "1")]);
        let b = spec(&[("work_mem", "64MB"), ("added", "2")]);
        assert_eq!(changed_settings(&a, &b), vec!["added", "gone", "work_mem"]);
    }

    #[test]
    fn an_identical_spec_changes_nothing() {
        let a = spec(&[("work_mem", "64MB")]);
        assert!(changed_settings(&a, &a).is_empty());
        assert_eq!(decide(&[], &Classified::default()), Apply::Nothing);
    }

    #[test]
    fn reloadable_settings_do_not_restart() {
        let changed = vec!["work_mem".to_string()];
        assert_eq!(decide(&changed, &Classified::default()), Apply::Reload);
    }

    fn known() -> BTreeMap<String, String> {
        [
            ("work_mem", "user"),
            ("log_min_duration_statement", "superuser"),
            ("autovacuum_naptime", "sighup"),
            ("post_auth_delay", "backend"),
            ("jit_debugging_support", "superuser-backend"),
            ("shared_buffers", "postmaster"),
            ("block_size", "internal"),
        ]
        .into_iter()
        .map(|(k, v)| (k.to_string(), v.to_string()))
        .collect()
    }

    /// A reload puts five of the seven contexts in force: sighup/superuser/user
    /// take effect in the running server, and backend/superuser-backend take
    /// effect for connections opened after it. None of those is worth an
    /// interruption.
    #[test]
    fn only_postmaster_settings_force_a_restart() {
        let known = known();
        let all: Vec<String> = known.keys().cloned().collect();
        assert_eq!(
            classify(&all, &known).restart,
            vec!["shared_buffers".to_string()]
        );
    }

    /// `block_size` is compiled into the binary, so a restart would not put a
    /// new value in force either. Restarting for it would cost an interruption
    /// and change nothing.
    #[test]
    fn a_compiled_in_setting_is_not_restarted_for() {
        let sorted = classify(&["block_size".to_string()], &known());
        assert!(sorted.restart.is_empty());
        assert_eq!(sorted.inapplicable, vec!["block_size".to_string()]);
    }

    /// A name Postgres does not report — a typo, or one from an extension that
    /// is not loaded. Nothing applies it, so nothing is done for it.
    #[test]
    fn a_setting_postgres_does_not_know_is_not_restarted_for() {
        let sorted = classify(&["yb_amp.nonsense".to_string()], &known());
        assert!(sorted.restart.is_empty());
        assert_eq!(sorted.inapplicable, vec!["yb_amp.nonsense".to_string()]);
    }

    /// A context this code does not recognise — one Postgres grows later — is
    /// not shown to be reloadable, and is not shown to be fixable by a restart
    /// either, so it is reported rather than acted on.
    #[test]
    fn an_unrecognised_context_is_reported_not_restarted_for() {
        let known: BTreeMap<String, String> =
            [("future_knob".to_string(), "something-new".to_string())]
                .into_iter()
                .collect();
        let sorted = classify(&["future_knob".to_string()], &known);
        assert!(sorted.restart.is_empty());
        assert_eq!(sorted.inapplicable, vec!["future_knob".to_string()]);
    }

    /// One postmaster setting in the batch decides the whole batch: they are
    /// applied together, and a reload would leave that one not in force while
    /// reporting success.
    #[test]
    fn one_postmaster_setting_forces_the_restart() {
        let changed = vec!["work_mem".to_string(), "shared_buffers".to_string()];
        assert_eq!(
            decide(&changed, &classify(&changed, &known())),
            Apply::Restart {
                because: vec!["shared_buffers".into()]
            }
        );
    }

    /// A batch of nothing-applies-them settings still reloads rather than
    /// restarting: the reload is what writes the file, and the reply names them
    /// so the operator is not told an edit took when it did not.
    #[test]
    fn inapplicable_settings_alone_do_not_restart() {
        let changed = vec!["block_size".to_string(), "yb_amp.nonsense".to_string()];
        let sorted = classify(&changed, &known());
        assert_eq!(decide(&changed, &sorted), Apply::Reload);
        assert_eq!(sorted.inapplicable, changed);
    }
}
