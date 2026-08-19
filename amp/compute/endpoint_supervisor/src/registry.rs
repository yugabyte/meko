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
use uuid::Uuid;
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
/// `$1` carries no cast, and that is load-bearing. `e.id` is `uuid`, so the
/// server infers the parameter as `uuid` and [`read`] binds a `Uuid` for it.
/// Writing `$1::uuid` instead — which reads like it helps — is what broke this
/// read on every endpoint boot (#834): the cast is what *types* the parameter,
/// the server reports `uuid` back in `Describe`, and `&str` has no `ToSql`
/// arm for `uuid`, so the bind fails client-side before the statement is ever
/// sent. A text parameter cast in SQL is a `psql` habit; with typed binds the
/// Rust value's type is the contract.
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
     WHERE e.id = $1";

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
    // Parsed before the retry loop, not inside it: a malformed id fails
    // identically on every attempt, and `retry`'s contract is that only
    // transient failures belong in it. It also has to be a `Uuid` rather than
    // the `&str` this used to bind — see [`QUERY`].
    let id = parse_id(endpoint_id)?;
    let rt = tokio::runtime::Builder::new_current_thread()
        .enable_all()
        .build()
        .context("building the registry-read runtime")?;
    let doc = crate::retry::retry("registry override read", &Backoff::registry(), || {
        rt.block_on(read(&yb, id))
    })?;
    match doc {
        Some(d) => parse(&d),
        None => Ok(None),
    }
}

/// The endpoint id as the `uuid` the column holds.
///
/// `EndpointId` is still a `String` (`utils::id::EndpointId`), so the parse is
/// where a malformed id is caught — and catching it here rather than at the
/// server is the difference between one clear error and a bind that cannot
/// succeed.
fn parse_id(endpoint_id: &str) -> Result<Uuid> {
    let raw = endpoint_id.trim();
    Uuid::parse_str(raw).with_context(|| format!("endpoint id {raw:?} is not a uuid"))
}

/// One connect-and-read. `None` is no row for this endpoint.
async fn read(yb: &YbConnConfig, id: Uuid) -> Result<Option<String>> {
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
    /// for an id to change the statement.
    ///
    /// This deliberately does NOT assert the parameter's spelling. Its previous
    /// form pinned `WHERE e.id = $1::uuid`, which is precisely the bug of #834:
    /// the assertion passed, read as coverage, and the read failed on every
    /// endpoint boot in the fleet. What the parameter must actually do is bind,
    /// and only a live server can say whether it does — `live::` below.
    #[test]
    fn the_endpoint_id_is_a_bind_parameter() {
        assert!(QUERY.contains("WHERE e.id = $1"), "{QUERY}");
        // Casting the column would drop the primary-key index.
        assert!(!QUERY.contains("e.id::text"), "{QUERY}");
    }

    /// A malformed id is rejected before anything connects, so the retry loop
    /// never spends four attempts on an error that cannot change (#834).
    #[test]
    fn a_malformed_endpoint_id_is_rejected_up_front() {
        let err = parse_id("not-a-uuid").unwrap_err().to_string();
        assert!(err.contains("not a uuid"), "{err}");
        // Whitespace around a real id is tolerated — it arrives from an env var.
        let id = "0057d25f-4b16-4c3f-9a0e-2f1d3c4b5a69";
        assert_eq!(parse_id(&format!("  {id}\n")).unwrap().to_string(), id);
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

    /// The half of this module that needs a real `amp_registry`.
    ///
    /// Everything above tests the SQL text and the parser, and #834 proves that
    /// is not enough: the statement was well-formed and the parser was correct,
    /// and the read still failed on every endpoint boot in the fleet, because
    /// the *bind* could not be serialized. Only a server that reports the
    /// parameter's type back can catch that, so these seed their own rows and
    /// run the real [`fetch`].
    ///
    /// Selected by path (`registry::tests::live::`) in
    /// `test/integration/compute/main.rs::unit_tests`, where a migrated cluster
    /// exists, and excluded by path from the clusterless unit job in
    /// `amp-pr-build.yml`. Keep those two in sync.
    mod live {
        use super::super::*;
        use uuid::Uuid;

        fn conn() -> YbConnConfig {
            let host = std::env::var("YB_TEST_HOST").unwrap_or_else(|_| "127.0.0.1".into());
            let port = std::env::var("YB_TEST_PORT").unwrap_or_else(|_| "5433".into());
            let user = std::env::var("YB_TEST_USER").unwrap_or_else(|_| "yugabyte".into());
            let dbname = std::env::var("YB_TEST_DB").unwrap_or_else(|_| "yb_storage".into());
            toml_edit::de::from_str(&format!(
                "host_ports = \"{host}:{port}\"\nuser = \"{user}\"\ndbname = \"{dbname}\"\n"
            ))
            .expect("build YbConnConfig")
        }

        /// One endpoint on one branch, with overrides on both, and the ids to
        /// reach them by. Everything is fresh per call — unique names and
        /// generated uuids — so these tests are safe to run in parallel with
        /// each other and with anything else on the same database.
        struct Seeded {
            endpoint_id: Uuid,
            branch_id: Uuid,
            cluster_id: Uuid,
            pool_id: Uuid,
            user_id: Uuid,
        }

        /// The FK chain the endpoints row needs: user → pool → cluster →
        /// branch → endpoint.
        ///
        /// Every insert is `ON CONFLICT DO NOTHING`, because these go through
        /// `YsqlClient` like everything else: its connection-drop arm replays a
        /// statement whose commit it could not confirm, and a plain `INSERT`
        /// replayed after a lost ack fails on its own primary key. `REVIEW.md`
        /// §1 makes that a rule for fixtures too, and a fixture that can fail
        /// spuriously is worse than no fixture.
        ///
        /// `role` goes in as the literal `'read_write'` rather than a bound
        /// parameter on purpose. Binding it would need `$n::amp_registry.
        /// endpoint_role`, and that cast is the #834 trap all over again — it
        /// types the parameter as the enum, which `&str` cannot serialize to.
        /// A constant that never came from a caller belongs in the SQL.
        async fn seed(client: &YsqlClient) -> Seeded {
            let s = Seeded {
                endpoint_id: Uuid::new_v4(),
                branch_id: Uuid::new_v4(),
                cluster_id: Uuid::new_v4(),
                pool_id: Uuid::new_v4(),
                user_id: Uuid::new_v4(),
            };
            let tag = s.endpoint_id.simple().to_string();
            client
                .execute(
                    "INSERT INTO amp_registry.users (id) VALUES ($1) \
                     ON CONFLICT DO NOTHING",
                    &[&s.user_id],
                )
                .await
                .expect("seed user");
            client
                .execute(
                    "INSERT INTO amp_registry.storage_pools (id, name, owner_user_id) \
                     VALUES ($1, $2, $3) ON CONFLICT DO NOTHING",
                    &[&s.pool_id, &format!("ovr-pool-{tag}"), &s.user_id],
                )
                .await
                .expect("seed storage pool");
            client
                .execute(
                    "INSERT INTO amp_registry.clusters (id, storage_pool_id, name) \
                     VALUES ($1, $2, $3) ON CONFLICT DO NOTHING",
                    &[&s.cluster_id, &s.pool_id, &format!("ovr-cluster-{tag}")],
                )
                .await
                .expect("seed cluster");
            client
                .execute(
                    "INSERT INTO amp_registry.branches \
                       (branch_id, cluster_id, name, pg_version, pg_settings, \
                        write_token_settings) \
                     VALUES ($1, $2, $3, 18, \
                             '{\"work_mem\":\"32MB\",\"shared_buffers\":\"1GB\"}'::jsonb, \
                             '{\"require_write_token\":true}'::jsonb) \
                     ON CONFLICT DO NOTHING",
                    &[&s.branch_id, &s.cluster_id, &format!("ovr-branch-{tag}")],
                )
                .await
                .expect("seed branch");
            client
                .execute(
                    "INSERT INTO amp_registry.endpoints \
                       (id, branch_id, role, pg_settings, pageserver_settings, \
                        safekeeper_settings) \
                     VALUES ($1, $2, 'read_write', \
                             '{\"work_mem\":\"64MB\"}'::jsonb, \
                             '{\"scale_factor\":8}'::jsonb, \
                             '{\"wal_cache_size_mib\":512}'::jsonb) \
                     ON CONFLICT DO NOTHING",
                    &[&s.endpoint_id, &s.branch_id],
                )
                .await
                .expect("seed endpoint");
            s
        }

        /// Best-effort teardown, in FK order. A leaked row would only cost the
        /// next run a little clutter, so a failure here is not the test's
        /// verdict.
        async fn unseed(client: &YsqlClient, s: &Seeded) {
            for (sql, id) in [
                (
                    "DELETE FROM amp_registry.endpoints WHERE id = $1",
                    &s.endpoint_id,
                ),
                (
                    "DELETE FROM amp_registry.branches WHERE branch_id = $1",
                    &s.branch_id,
                ),
                (
                    "DELETE FROM amp_registry.clusters WHERE id = $1",
                    &s.cluster_id,
                ),
                (
                    "DELETE FROM amp_registry.storage_pools WHERE id = $1",
                    &s.pool_id,
                ),
                ("DELETE FROM amp_registry.users WHERE id = $1", &s.user_id),
            ] {
                let _ = client.execute(sql, &[id]).await;
            }
        }

        /// A runtime for the seeding, separate from the one [`fetch`] builds for
        /// itself. `fetch` is synchronous and `block_on`s internally, so it
        /// cannot be called from inside a runtime — which is why these are
        /// `#[test]` and not `#[tokio::test]`.
        fn rt() -> tokio::runtime::Runtime {
            tokio::runtime::Builder::new_current_thread()
                .enable_all()
                .build()
                .expect("build test runtime")
        }

        /// The #834 regression, and the reason this module needs a live server
        /// at all: binding this endpoint id must serialize. Before the fix it
        /// could not, for any id, on any boot — "cannot convert between the Rust
        /// type `&str` and the Postgres type `uuid`" — which is why every
        /// SQL-text assertion above passed while the feature was dead
        /// fleet-wide. The statement was fine; the bind never left the client.
        ///
        /// Also pins what the module doc claims about precedence: the endpoint's
        /// `work_mem` beats the branch's, a branch key the endpoint does not set
        /// survives the merge, the lease settings come off the branch, and a
        /// well-formed id with no row is "nothing to apply" rather than an error.
        ///
        /// Two connections, deliberately, and no more. Both binds run on the
        /// test's own client, and [`fetch`] is called once for the end-to-end
        /// path. Each extra connect is a chance for the smart driver's
        /// process-global failed-host bookkeeping to starve the next one after a
        /// slow connect trips the 3s bound — a flake about the driver's
        /// connection accounting, which is not what this test is for.
        #[test]
        fn the_override_read_binds_its_id_and_merges_both_rows() {
            let cfg = conn();
            let rt = rt();
            let client = rt.block_on(async {
                YsqlClient::from_yb_config(&cfg, "registry_live_test", false)
                    .await
                    .expect("connect to amp_registry; set YB_TEST_HOST/PORT/USER/DB to override")
            });
            let seeded = rt.block_on(seed(&client));

            // The bind, on the connection this test already holds.
            let bound = rt.block_on(async {
                let row = client
                    .query_opt(QUERY, &[&seeded.endpoint_id])
                    .await
                    .expect("binding a uuid endpoint id must serialize (#834)");
                let missing = client
                    .query_opt(QUERY, &[&Uuid::new_v4()])
                    .await
                    .expect("an id with no row still has to bind");
                (row.map(|r| r.get::<_, String>(0)), missing.is_none())
            });
            // And the whole path the supervisor actually calls, once.
            let end_to_end = fetch(&cfg, &seeded.endpoint_id.to_string());
            rt.block_on(unseed(&client, &seeded));

            let (doc, no_row_for_unknown_id) = bound;
            assert!(
                no_row_for_unknown_id,
                "an unknown endpoint id has nothing to apply"
            );
            let got = parse(&doc.expect("the seeded endpoint has a row"))
                .expect("the document parses")
                .expect("and is not empty");
            let via_fetch = end_to_end
                .expect("the override read must succeed against a live registry")
                .expect("the seeded endpoint has a row");
            assert_eq!(got, via_fetch, "fetch must return what the query returns");

            assert_eq!(got.pg.get("work_mem").map(String::as_str), Some("64MB"));
            assert_eq!(
                got.pg.get("shared_buffers").map(String::as_str),
                Some("1GB"),
                "a branch key the endpoint does not set must survive the merge"
            );
            assert_eq!(
                got.pageserver.get("scale_factor"),
                Some(&OverrideValue::Int(8))
            );
            assert_eq!(
                got.safekeeper.get("wal_cache_size_mib"),
                Some(&OverrideValue::Int(512))
            );
            assert_eq!(
                got.write_token.get("require_write_token"),
                Some(&OverrideValue::Bool(true)),
                "the lease settings come off the branch"
            );
        }
    }
}
