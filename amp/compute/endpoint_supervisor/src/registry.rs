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

use anyhow::{Context, Result};
use serde_json::Value;
use ysql_client::{YbConnConfig, YsqlClient};

use crate::config::OverrideValue;
use crate::retry::Backoff;

/// Everything the registry can say about one endpoint's configuration.
#[derive(Debug, Default, Clone, PartialEq)]
pub struct Overrides {
    /// Branch baseline with the endpoint's own layered over it.
    pub pg: BTreeMap<String, String>,
    pub pageserver: BTreeMap<String, OverrideValue>,
    pub safekeeper: BTreeMap<String, OverrideValue>,
    /// How this endpoint's supervisor holds the branch's write lease. Off the
    /// branch, not the endpoint: the lease is one row per branch, so this is
    /// not a per-endpoint override and there is no endpoint column to merge.
    pub write_token: BTreeMap<String, OverrideValue>,
}

/// The lookup, with the endpoint id bound rather than interpolated.
///
/// `e.id` is `uuid` and the parameter arrives as text, so the cast is on the
/// parameter — casting the column instead would cost the primary-key index on
/// a table with a row per endpoint in the fleet.
///
/// `::text` on the result keeps [`parse`] the one place the document is
/// interpreted.
///
/// `write_token_settings` is read through `to_jsonb(b) ->` rather than named
/// directly. Its definition lives in migration `0002` and there is no repair
/// migration for databases already past it, and naming a column that is not
/// there is a parse error — in **one** statement, which would take the
/// pageserver, safekeeper and pg overrides down with it, on every endpoint,
/// over a key that is merely absent. Through `to_jsonb` a missing column is a
/// missing key, which is what an unset override already looks like.
const QUERY: &str = "SELECT jsonb_build_object(\
     'pg', coalesce(b.pg_settings,'{}'::jsonb) || coalesce(e.pg_settings,'{}'::jsonb),\
     'pageserver', coalesce(e.pageserver_settings,'{}'::jsonb),\
     'safekeeper', coalesce(e.safekeeper_settings,'{}'::jsonb),\
     'write_token', coalesce(to_jsonb(b) -> 'write_token_settings','{}'::jsonb))::text \
     FROM amp_registry.endpoints e \
     LEFT JOIN amp_registry.branches b ON b.branch_id = e.branch_id \
     WHERE e.id = $1::uuid";

/// How long opening the connection may take. Without a bound this blocks
/// indefinitely against an address that accepts nothing, and this read happens
/// before any daemon starts — so an unbounded wait here is an endpoint that
/// never comes up and has nothing listening for a probe to fail against.
///
/// Tighter than [`ysql_client::DEFAULT_CONNECT_TIMEOUT_MS`]: this one is on the
/// boot path, and its failure mode is "carry on with derived defaults".
const CONNECT_TIMEOUT_MS: u64 = 3_000;

/// Bound on a peer that stops answering at the network level. The statement
/// ceiling below cannot cover it — a node that dies without closing its sockets
/// leaves nothing to enforce a server-side timeout — and without this the kernel
/// waits on a two-hour keepalive idle. Inherited from the caller's block it
/// would be zero, i.e. off.
const TCP_USER_TIMEOUT_MS: u64 = 3_000;

/// Ceiling on the query itself, for the case where the connection opens but the
/// statement never returns. Set here rather than inherited, because the client
/// default is off — the daemons' bulk paths cannot carry a statement bound, and
/// this one indexed single-row read can.
const STATEMENT_TIMEOUT_MS: u64 = 5_000;

/// The endpoint's own connection, with both bounds applied.
fn bounded(yb: &YbConnConfig) -> YbConnConfig {
    YbConnConfig {
        connect_timeout_ms: CONNECT_TIMEOUT_MS,
        statement_timeout_ms: STATEMENT_TIMEOUT_MS,
        tcp_user_timeout_ms: TCP_USER_TIMEOUT_MS,
        ..yb.clone()
    }
}

/// Runs the lookup. `Ok(None)` means "nothing to apply" (no row); `Err` means
/// the lookup itself failed and the caller should warn and continue.
///
/// The read is retried, because losing an operator's settings to a YB leader
/// election means an endpoint silently running on derived defaults. Its one
/// deterministic failure — an unparseable document — is handled outside the
/// retry, since repeating it only delays the same error.
///
/// A fresh client per attempt, deliberately: reconnecting is most of what makes
/// a retry worth taking, and this is not on a hot path.
///
/// Called on the boot path, before any daemon starts — which is why every bound
/// in [`bounded`] matters: an unbounded read here is an endpoint that never
/// comes up, with nothing listening for a probe to fail against.
pub fn fetch(yb: &YbConnConfig, endpoint_id: &str) -> Result<Option<Overrides>> {
    let yb = bounded(yb);
    let id = endpoint_id.trim().to_string();
    let rt = tokio::runtime::Builder::new_current_thread()
        .enable_all()
        .build()
        .context("building the registry-read runtime")?;
    let doc = crate::retry::retry("registry override read", &Backoff::registry(), || {
        rt.block_on(read(&yb, &id))
    })?;
    match doc {
        Some(d) => parse(&d),
        None => Ok(None),
    }
}

/// One connect-and-read. `None` is no row for this endpoint.
async fn read(yb: &YbConnConfig, id: &str) -> Result<Option<String>> {
    let client = YsqlClient::from_yb_config(yb, "endpoint-supervisor", false)
        .await
        .context("connecting to amp_registry")?;
    let row = client
        .query_opt(QUERY, &[&id])
        .await
        .context("override lookup")?;
    Ok(row.map(|r| r.get::<_, String>(0)))
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
        ("write_token", &mut out.write_token),
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
        // are flat {key: value} maps, and a nested toml table is addressed by a
        // dotted KEY (`tenant_config.enable_gc`), never by a nested value.
        _ => None,
    }
}

#[cfg(test)]
mod tests {
    use super::*;

    const ID: &str = "3f2b1c4e-0000-4000-8000-000000000001";

    fn conn(host_ports: &str) -> YbConnConfig {
        toml_edit::de::from_str(&format!(
            "host_ports = \"{host_ports}\"\nuser = \"u\"\ndbname = \"d\"\n"
        ))
        .unwrap()
    }

    /// The SQL is the contract with the schema: the operand order of `||` is what
    /// makes the endpoint win, and swapping it would silently invert precedence.
    #[test]
    fn the_query_merges_both_rows_endpoint_last() {
        assert!(
            QUERY.contains(
                "coalesce(b.pg_settings,'{}'::jsonb) || coalesce(e.pg_settings,'{}'::jsonb)"
            ),
            "{QUERY}"
        );
        assert!(
            QUERY.contains("LEFT JOIN amp_registry.branches b"),
            "{QUERY}"
        );
    }

    /// A database that has not re-run `0002` has no `write_token_settings`, and
    /// naming a missing column would fail the whole read — taking the daemon and
    /// pg overrides with it, on every endpoint, over a key that is merely unset.
    #[test]
    fn the_lease_settings_survive_a_database_without_the_column() {
        assert!(
            QUERY.contains("to_jsonb(b) -> 'write_token_settings'"),
            "{QUERY}"
        );
        assert!(!QUERY.contains("b.write_token_settings"), "{QUERY}");
    }

    /// The id is bound, not interpolated — which is what makes it impossible
    /// for an id to change the statement, and why this module no longer checks
    /// the id's shape before using it.
    #[test]
    fn the_endpoint_id_is_a_bind_parameter() {
        assert!(QUERY.contains("WHERE e.id = $1::uuid"), "{QUERY}");
        // The cast is on the parameter, so the primary-key index still applies.
        assert!(!QUERY.contains("e.id::text"), "{QUERY}");
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
    /// blocks the client forever, and this runs before any daemon starts — so
    /// the pod would hang with nothing listening for a probe to fail against.
    ///
    /// Both are asked for rather than inherited: the client's statement default
    /// is off, because the daemons' bulk paths cannot carry one.
    #[test]
    fn the_lookup_is_bounded_by_a_connect_and_statement_timeout() {
        let yb = bounded(&conn("127.0.0.1:5433"));
        assert_eq!(yb.connect_timeout_ms, CONNECT_TIMEOUT_MS);
        assert_eq!(yb.statement_timeout_ms, STATEMENT_TIMEOUT_MS);
        // Zero would mean "no bound" for either, which is the thing this read
        // must never inherit.
        assert_ne!(CONNECT_TIMEOUT_MS, 0);
        assert_ne!(STATEMENT_TIMEOUT_MS, 0);

        // Whole-read worst case: every attempt stalls the full connect timeout,
        // plus the sleeps between them. This is startup latency before Postgres
        // exists, so it must stay small.
        let b = Backoff::registry();
        let sleeps: std::time::Duration = (1..b.attempts).map(|a| b.delay_after(a)).sum();
        let worst = std::time::Duration::from_millis(CONNECT_TIMEOUT_MS) * b.attempts + sleeps;
        assert!(
            worst <= std::time::Duration::from_secs(30),
            "worst-case read is {worst:?}"
        );
    }

    /// An unreachable host must surface an error after a bounded number of
    /// attempts rather than hanging, and must not be mistaken for "no
    /// overrides" — an endpoint quietly running on derived defaults looks
    /// exactly like one that has none configured.
    #[test]
    fn an_unreachable_registry_is_an_error_not_an_empty_result() {
        // Port 1: nothing listens, so every attempt fails to connect.
        let started = std::time::Instant::now();
        let err = fetch(&conn("127.0.0.1:1"), ID).expect_err("must not report success");
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
