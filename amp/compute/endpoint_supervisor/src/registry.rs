//! Reading this endpoint's operator overrides out of `amp_registry`.
//!
//! Two rows in one query: the branch carries the settings every compute on it
//! gets, the endpoint carries its own, and `||` on jsonb takes the **right**
//! operand for a key both define — which is the precedence the schema documents
//! ("an endpoint's own pg_settings merge over these"). `LEFT JOIN`, so a missing
//! branch row still yields the endpoint's settings rather than nothing at all.
//!
//! Failure is deliberately **not** fatal. An endpoint that cannot reach the
//! registry must still start on its derived defaults; refusing to boot because an
//! override *might* exist would turn a registry blip into an outage. The caller
//! gets empty maps and a warning.
//!
//! The read is narrow on purpose — one row, four columns, keyed by primary key.
//! `amp_registry` is the controller's own schema and the pod only reaches it
//! because both connect as the same superuser today, so the less of it this
//! depends on, the cheaper a future least-privilege role is to introduce.

use std::collections::BTreeMap;

use anyhow::{Context, Result, bail};
use serde_json::Value;

use crate::config::OverrideValue;
use crate::retry::Backoff;

/// Everything the registry can say about one endpoint's configuration.
#[derive(Debug, Default, Clone, PartialEq)]
pub struct Overrides {
    /// Branch baseline with the endpoint's own layered over it.
    pub pg: BTreeMap<String, String>,
    pub pageserver: BTreeMap<String, OverrideValue>,
    pub safekeeper: BTreeMap<String, OverrideValue>,
}

/// How to reach YugabyteDB with the bundled `psql`.
pub struct PsqlTarget {
    pub psql: camino::Utf8PathBuf,
    pub host: String,
    pub port: String,
    pub user: String,
    pub dbname: String,
    /// Name of the env var holding the password, if any (`YbConnConfig`'s
    /// `password_env` convention). Its value is passed through as `PGPASSWORD`.
    pub password_env: Option<String>,
}

/// A well-formed endpoint id, checked before it is interpolated into SQL.
///
/// `psql -c` takes no bind parameters, so the id is validated rather than
/// trusted: anything that is not a plain UUID is refused.
pub fn valid_endpoint_id(raw: &str) -> Option<String> {
    let s = raw.trim();
    let hex_and_dashes = s.chars().all(|c| c.is_ascii_hexdigit() || c == '-');
    let shape = s.len() == 36
        && s.as_bytes().iter().enumerate().all(|(i, b)| {
            if matches!(i, 8 | 13 | 18 | 23) {
                *b == b'-'
            } else {
                b.is_ascii_hexdigit()
            }
        });
    (hex_and_dashes && shape).then(|| s.to_string())
}

fn query(endpoint_id: &str) -> String {
    format!(
        "SELECT jsonb_build_object(\
         'pg', coalesce(b.pg_settings,'{{}}'::jsonb) || coalesce(e.pg_settings,'{{}}'::jsonb),\
         'pageserver', coalesce(e.pageserver_settings,'{{}}'::jsonb),\
         'safekeeper', coalesce(e.safekeeper_settings,'{{}}'::jsonb)) \
         FROM amp_registry.endpoints e \
         LEFT JOIN amp_registry.branches b ON b.branch_id = e.branch_id \
         WHERE e.id = '{endpoint_id}'"
    )
}

/// How long `psql` may spend opening the connection. Without this it blocks
/// indefinitely against an address that accepts nothing, and this read happens
/// before any daemon starts — so an unbounded wait here is an endpoint that never
/// comes up and has nothing listening for a probe to fail against.
const CONNECT_TIMEOUT_SECS: u32 = 3;

/// Ceiling on the query itself, for the case where the connection opens but the
/// statement never returns.
const STATEMENT_TIMEOUT_MS: u32 = 5_000;

/// Runs the lookup. `Ok(None)` means "nothing to apply" (no row, or no id);
/// `Err` means the lookup itself failed and the caller should warn and continue.
///
/// The psql invocation is retried, because losing an operator's settings to a YB
/// leader election means an endpoint silently running on derived defaults. The two
/// deterministic failures — a malformed id and unparseable output — are checked
/// outside the retry, since repeating them only delays the same error.
pub fn fetch(target: &PsqlTarget, endpoint_id: &str) -> Result<Option<Overrides>> {
    let id = valid_endpoint_id(endpoint_id)
        .with_context(|| format!("endpoint id {endpoint_id:?} is not a UUID"))?;
    let stdout = crate::retry::retry("registry override read", &Backoff::registry(), || {
        run_psql(target, &id)
    })?;
    parse(&stdout)
}

/// One `psql` invocation, returning its stdout.
fn run_psql(target: &PsqlTarget, id: &str) -> Result<String> {
    let out = psql_command(target, id).output().context("run psql")?;
    if !out.status.success() {
        bail!(
            "override lookup failed: {}",
            String::from_utf8_lossy(&out.stderr).trim()
        );
    }
    String::from_utf8(out.stdout).context("psql output is not utf-8")
}

/// Builds the invocation. Split out so the timeouts, which are the difference
/// between a bounded read and a pod that hangs, are assertable in a test.
fn psql_command(target: &PsqlTarget, id: &str) -> std::process::Command {
    let mut cmd = std::process::Command::new(&target.psql);
    cmd.args([
        "-h",
        &target.host,
        "-p",
        &target.port,
        "-U",
        &target.user,
        "-d",
        &target.dbname,
        "-v",
        "ON_ERROR_STOP=1",
        "--no-psqlrc",
        // -t (tuples only) -A (unaligned) so stdout is exactly the JSON document.
        "-t",
        "-A",
        "-c",
        &query(id),
    ]);
    // Both bounds go through libpq's environment rather than the connstring, so
    // they apply without having to rebuild the target's arguments.
    cmd.env("PGCONNECT_TIMEOUT", CONNECT_TIMEOUT_SECS.to_string());
    cmd.env(
        "PGOPTIONS",
        format!("-c statement_timeout={STATEMENT_TIMEOUT_MS}"),
    );
    if let Some(var) = &target.password_env {
        if let Ok(pw) = std::env::var(var) {
            cmd.env("PGPASSWORD", pw);
        }
    }
    cmd
}

/// Parses the single JSON document the query returns.
pub fn parse(stdout: &str) -> Result<Option<Overrides>> {
    let raw = stdout.trim();
    if raw.is_empty() {
        return Ok(None);
    }
    let doc: Value = serde_json::from_str(raw).context("override row is not valid JSON")?;
    let mut out = Overrides::default();
    if let Some(map) = doc.get("pg").and_then(Value::as_object) {
        for (k, v) in map {
            // pg settings are strings by the time they reach postgresql.conf.
            let s = match v {
                Value::String(s) => s.clone(),
                other => other.to_string(),
            };
            out.pg.insert(k.clone(), s);
        }
    }
    for (surface, dest) in [
        ("pageserver", &mut out.pageserver),
        ("safekeeper", &mut out.safekeeper),
    ] {
        if let Some(map) = doc.get(surface).and_then(Value::as_object) {
            for (k, v) in map {
                match to_override(v) {
                    Some(val) => {
                        dest.insert(k.clone(), val);
                    }
                    None => tracing::warn!(
                        surface,
                        key = %k,
                        "ignoring override: only scalar values are supported"
                    ),
                }
            }
        }
    }
    Ok(Some(out))
}

fn to_override(v: &Value) -> Option<OverrideValue> {
    match v {
        Value::Bool(b) => Some(OverrideValue::Bool(*b)),
        Value::String(s) => Some(OverrideValue::Str(s.clone())),
        Value::Number(n) => {
            if let Some(i) = n.as_i64() {
                Some(OverrideValue::Int(i))
            } else {
                n.as_f64().map(OverrideValue::Float)
            }
        }
        // Objects and arrays are refused rather than half-rendered: the columns
        // are documented as flat {key: value} maps of top-level toml keys.
        _ => None,
    }
}

#[cfg(test)]
mod tests {
    use super::*;

    const ID: &str = "3f2b1c4e-0000-4000-8000-000000000001";

    #[test]
    fn only_a_uuid_is_accepted() {
        assert_eq!(valid_endpoint_id(ID).as_deref(), Some(ID));
        assert_eq!(valid_endpoint_id(&format!("  {ID}  ")).as_deref(), Some(ID));
        // The interesting cases: anything that could change the statement.
        for bad in [
            "",
            "not-a-uuid",
            "'; DROP TABLE amp_registry.endpoints; --",
            "3f2b1c4e-0000-4000-8000-00000000000", // too short
            "3f2b1c4e-0000-4000-8000-0000000000011", // too long
            "3f2b1c4e_0000_4000_8000_000000000001", // wrong separators
            "zzzzzzzz-0000-4000-8000-000000000001", // non-hex
        ] {
            assert_eq!(valid_endpoint_id(bad), None, "accepted {bad:?}");
        }
    }

    /// The SQL is the contract with the schema: the operand order of `||` is what
    /// makes the endpoint win, and swapping it would silently invert precedence.
    #[test]
    fn the_query_merges_both_rows_endpoint_last() {
        let q = query(ID);
        assert!(
            q.contains(
                "coalesce(b.pg_settings,'{}'::jsonb) || coalesce(e.pg_settings,'{}'::jsonb)"
            ),
            "{q}"
        );
        assert!(q.contains("LEFT JOIN amp_registry.branches b"), "{q}");
        assert!(q.contains(&format!("WHERE e.id = '{ID}'")), "{q}");
    }

    #[test]
    fn no_row_means_nothing_to_apply() {
        assert_eq!(parse("").unwrap(), None);
        assert_eq!(parse("   \n").unwrap(), None);
    }

    #[test]
    fn the_three_surfaces_are_parsed_with_their_types() {
        let got = parse(
            r#"{"pg":{"work_mem":"64MB"},
                "pageserver":{"scale_factor":8,"yb_payload_compression":"none"},
                "safekeeper":{"wal_cache_size_mib":512,"current_thread_runtime":true}}"#,
        )
        .unwrap()
        .unwrap();
        assert_eq!(got.pg.get("work_mem").unwrap(), "64MB");
        assert_eq!(
            got.pageserver.get("scale_factor"),
            Some(&OverrideValue::Int(8))
        );
        assert_eq!(
            got.pageserver.get("yb_payload_compression"),
            Some(&OverrideValue::Str("none".into()))
        );
        assert_eq!(
            got.safekeeper.get("current_thread_runtime"),
            Some(&OverrideValue::Bool(true))
        );
    }

    /// A nested value cannot be rendered into a top-level toml key, so it is
    /// dropped with a warning rather than half-written.
    #[test]
    fn nested_values_are_refused() {
        let got = parse(r#"{"pageserver":{"nested":{"a":1},"ok":4}}"#)
            .unwrap()
            .unwrap();
        assert!(!got.pageserver.contains_key("nested"));
        assert_eq!(got.pageserver.get("ok"), Some(&OverrideValue::Int(4)));
    }

    #[test]
    fn malformed_json_is_an_error_not_a_silent_empty() {
        assert!(parse("{not json").is_err());
    }

    /// The read must be bounded on both axes. Without these an unreachable YB
    /// blocks `psql` forever, and this runs before any daemon starts — so the pod
    /// would hang with nothing listening for a probe to fail against.
    #[test]
    fn the_lookup_is_bounded_by_a_connect_and_statement_timeout() {
        let target = PsqlTarget {
            psql: "psql".into(),
            host: "h".into(),
            port: "5433".into(),
            user: "u".into(),
            dbname: "d".into(),
            password_env: None,
        };
        let cmd = psql_command(&target, ID);
        let env: std::collections::BTreeMap<_, _> = cmd
            .get_envs()
            .filter_map(|(k, v)| Some((k.to_str()?, v?.to_str()?)))
            .collect();
        assert_eq!(
            env.get("PGCONNECT_TIMEOUT").copied(),
            Some(CONNECT_TIMEOUT_SECS.to_string().as_str())
        );
        assert_eq!(
            env.get("PGOPTIONS").copied(),
            Some(format!("-c statement_timeout={STATEMENT_TIMEOUT_MS}").as_str())
        );
        // Whole-read worst case: every attempt stalls the full connect timeout,
        // plus the sleeps between them. This is startup latency before Postgres
        // exists, so it must stay small.
        let b = Backoff::registry();
        let sleeps: std::time::Duration = (1..b.attempts).map(|a| b.delay_after(a)).sum();
        let worst =
            std::time::Duration::from_secs(u64::from(CONNECT_TIMEOUT_SECS)) * b.attempts + sleeps;
        assert!(
            worst <= std::time::Duration::from_secs(30),
            "worst-case read is {worst:?}"
        );
    }

    /// An unreachable host must surface an error after a bounded number of
    /// attempts rather than hanging, and must not be mistaken for "no overrides".
    #[test]
    fn an_unreachable_registry_is_an_error_not_an_empty_result() {
        let target = PsqlTarget {
            // Any path that is not an executable: this exercises the retry and
            // error plumbing without needing a psql or a server.
            psql: "/nonexistent/psql".into(),
            host: "127.0.0.1".into(),
            port: "1".into(),
            user: "yugabyte".into(),
            dbname: "yb_storage".into(),
            password_env: None,
        };
        let started = std::time::Instant::now();
        let err = fetch(&target, ID).expect_err("must not report success");
        assert!(
            format!("{err:#}").contains("failed after"),
            "should report the exhausted budget: {err:#}"
        );
        assert!(
            started.elapsed() < std::time::Duration::from_secs(30),
            "took {:?}",
            started.elapsed()
        );
    }

    /// A malformed id is deterministic, so it must fail once rather than be
    /// retried — and it must not reach the shell at all.
    #[test]
    fn a_bad_id_fails_without_retrying() {
        let target = PsqlTarget {
            psql: "/nonexistent/psql".into(),
            host: "127.0.0.1".into(),
            port: "1".into(),
            user: "u".into(),
            dbname: "d".into(),
            password_env: None,
        };
        let err = fetch(&target, "'; DROP TABLE x; --").expect_err("must reject");
        let msg = format!("{err:#}");
        assert!(msg.contains("is not a UUID"), "{msg}");
        assert!(
            !msg.contains("failed after"),
            "must not have retried: {msg}"
        );
    }

    #[test]
    fn a_float_override_keeps_its_type() {
        let got = parse(r#"{"pageserver":{"scale_factor":2.5}}"#)
            .unwrap()
            .unwrap();
        assert_eq!(
            got.pageserver.get("scale_factor"),
            Some(&OverrideValue::Float(2.5))
        );
    }
}
