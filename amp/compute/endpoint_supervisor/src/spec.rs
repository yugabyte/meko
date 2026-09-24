//! The compute spec `compute_ctl` consumes, written before anything starts.
//!
//! The compute spec's setting order is load-bearing and is the one thing to be
//! careful about when reading this: `compute_ctl` writes the settings into
//! `postgresql.conf` in order (`pg_helpers.rs`'s `as_pg_settings`), and Postgres
//! keeps the **last** occurrence of a name. So later entries override earlier
//! ones, and the precedence is
//!
//!   static defaults < resource-derived < chart extension paths < branch < endpoint
//!
//! which is the opposite of the daemons' toml, where a duplicated key is a parse
//! error rather than an override (see [`crate::config`]).

use std::collections::BTreeMap;

use serde_json::{Value, json};

use crate::pg_settings::Setting;

/// Everything the compute spec needs that is not a Postgres setting.
pub struct ComputeSpecParams<'a> {
    pub compute_port: u16,
    pub branch_id: &'a str,
    pub safekeeper_addr: &'a str,
    pub pageserver_connstring: &'a str,
    pub shared_preload_libraries: &'a str,
    /// This endpoint's id, which the LFC state blob is keyed by. Without it
    /// `compute_ctl` cannot address its own state and prewarm is unavailable.
    pub endpoint_id: Option<&'a str>,
    /// The YB blob store `compute_ctl` reads and writes LFC state in, as the
    /// externally tagged object its spec field deserializes.
    pub yb_storage_conn: Value,
    /// Load the previous LFC state on startup.
    pub autoprewarm: bool,
    /// Save the LFC state this often. `None` means never.
    pub offload_lfc_interval_seconds: Option<u64>,
    /// The key set `compute_ctl` validates bearer tokens against on its external
    /// HTTP server, as the `{"keys": [...]}` object its config expects.
    ///
    /// An empty set is the pod's, and is deliberate: the supervisor has no token
    /// to present, so an empty set closes every external authenticated route and
    /// leaves the in-pod server as the control channel (see `compute_tools`'
    /// `http::server`). A caller that does hold a key — `yb_amp_local` mints
    /// status tokens from the local environment's keypair — passes it here, or
    /// its own probes are the ones that get rejected.
    pub compute_ctl_jwks: Value,
}

/// Renders the compute spec, layering settings in precedence order.
///
/// `derived` are the resource-derived GUCs, `chart` the extension paths the chart
/// computed from the ImageVolumes it mounted, and `registry` the branch-merged
/// user settings read from `amp_registry` (branch baseline with the endpoint's own
/// already layered over it). Each layer is appended after the previous, so the
/// later one wins in `postgresql.conf`.
pub fn render_compute_spec(
    p: &ComputeSpecParams<'_>,
    derived: &[Setting],
    chart: &BTreeMap<String, String>,
    registry: &BTreeMap<String, String>,
) -> Value {
    let mut settings: Vec<Value> = Vec::new();
    let mut push = |name: &str, value: String, vartype: &str| {
        settings.push(json!({"name": name, "value": value, "vartype": vartype}));
    };

    // 1. Static defaults: the invariants of a yb-amp compute, not tuning.
    push("fsync", "off".into(), "bool");
    // Durability lives in the YB rows the safekeeper writes, not in fsync'd
    // Postgres pages, so full-page images are pure WAL overhead: every
    // checkpoint re-arms them for the whole working set, and the safekeeper's
    // WAL compression is tuned for a WAL that carries none.
    push("full_page_writes", "off".into(), "bool");
    push("wal_level", "logical".into(), "enum");
    // Hint-bit FPIs are the other full-page images in the WAL, and
    // `XLogSaveBufferForHint` forces them with REGBUF_FORCE_IMAGE, so
    // `full_page_writes = off` above does not suppress them. Everything they
    // make durable -- heap hint bits, index LP_DEAD kill bits, FSM page bits --
    // is recomputed on demand when it is missing, and the VM/FSM truncation
    // clears that `XLogHintBitIsNeeded()` also gates are reproduced by the
    // pageserver itself (`ingest_xlog_smgr_truncate`). Torn pages, the reason
    // upstream Postgres wants this on, cannot happen here: pages reach storage
    // only through the WAL.
    push("wal_log_hints", "off".into(), "bool");
    push("port", p.compute_port.to_string(), "integer");
    push("listen_addresses", "0.0.0.0".into(), "string");
    push("max_connections", "100".into(), "integer");
    push("shared_buffers", "128MB".into(), "string");
    push("max_wal_senders", "10".into(), "integer");
    push("max_replication_slots", "10".into(), "integer");
    // Governs what Postgres does with a PLAINTEXT password — a client's own
    // `ALTER ROLE ... PASSWORD 'x'`, and remote_auth when COMPUTE_PASSWORD
    // carries a plaintext rather than a verifier. It does NOT re-encode a
    // verifier handed over already hashed, and it does not upgrade an existing
    // md5 verifier: that one is keyed on what pg_authid holds, per role, forever.
    push("password_encryption", "scram-sha-256".into(), "enum");
    push("restart_after_crash", "off".into(), "bool");
    push("synchronous_standby_names", "walproposer".into(), "string");
    push(
        "shared_preload_libraries",
        p.shared_preload_libraries.into(),
        "string",
    );
    push("yb_amp.safekeepers", p.safekeeper_addr.into(), "string");
    push("yb_amp.branch_id", p.branch_id.into(), "string");
    push(
        "yb_amp.pageserver_connstring",
        p.pageserver_connstring.into(),
        "string",
    );

    // 2. Resource-derived, overriding the static shared_buffers above. Each
    // carries its own vartype: the worker counts are integer GUCs, and declaring
    // them as strings would misdescribe them to compute_ctl.
    for s in derived {
        push(&s.name, s.value.clone(), s.vartype);
    }
    // 3. Chart-derived extension paths, which cannot come from the database.
    for (k, v) in chart {
        push(k, v.clone(), "string");
    }
    // 4. The operator's settings, last, so they win over everything derived.
    for (k, v) in registry {
        push(k, v.clone(), "string");
    }

    // The file compute_ctl reads is a ComputeConfig -- `{ spec, compute_ctl_config }`
    // (`compute_api::responses::ComputeConfig`, read by bin/compute-ctl.rs) --
    // NOT a bare spec. Emitting the spec fields at the top level produces a file
    // it cannot deserialize, so the compute never starts.
    json!({
        "spec": {
            "format_version": 1.0,
            "timestamp": "2024-01-01T00:00:00.000Z",
            "operation_uuid": "00000000-0000-0000-0000-000000000000",
            // Required: ComputeSpec::suspend_timeout_seconds is i64 with no
            // serde default, so omitting it fails deserialization outright.
            // -1 means never suspend, which is what a pod-per-endpoint wants.
            "suspend_timeout_seconds": -1,
            // LFC prewarm: compute_ctl reads and writes the state blob in YB
            // itself, so the store's connection travels in the spec. endpoint_id
            // keys the blob; without it there is nothing to address.
            "endpoint_id": p.endpoint_id,
            "yb_storage_conn": p.yb_storage_conn,
            "autoprewarm": p.autoprewarm,
            "offload_lfc_interval_seconds": p.offload_lfc_interval_seconds,
            "cluster": {
                "cluster_id": p.branch_id,
                "name": "compute",
                "state": "running",
                // No `encrypted_password`: `skip_pg_catalog_updates` below makes
                // compute_ctl skip the block that would apply one, so the field
                // was inert — and wrong besides, since `Role::to_pg_options`
                // prefixes a literal `md5` to anything not starting with
                // SCRAM-SHA-256, which would have double-hashed the verifier the
                // control plane sends. The password is remote_auth's, and only
                // remote_auth's.
                "roles": [{
                    "name": "cloud_admin",
                    "options": null,
                }],
                "databases": [],
                "settings": settings,
            },
            "skip_pg_catalog_updates": true,
        },
        "compute_ctl_config": {"jwks": p.compute_ctl_jwks},
    })
}

#[cfg(test)]
mod tests {
    use super::*;

    fn params() -> ComputeSpecParams<'static> {
        ComputeSpecParams {
            compute_port: 55433,
            branch_id: "9a054903-242f-46bc-a0c9-cc6b91a37e4c",
            safekeeper_addr: "127.0.0.1:5454",
            pageserver_connstring: "host=127.0.0.1 port=64000",
            shared_preload_libraries: "yb_amp",
            endpoint_id: Some("3f2b1c4e-0000-4000-8000-000000000001"),
            yb_storage_conn: json!({"type": "YbStore", "host_ports": "h:5433",
                                    "user": "yugabyte", "dbname": "yb_storage"}),
            autoprewarm: false,
            offload_lfc_interval_seconds: None,
            compute_ctl_jwks: json!({"keys": []}),
        }
    }

    fn names(spec: &Value) -> Vec<String> {
        spec["spec"]["cluster"]["settings"]
            .as_array()
            .unwrap()
            .iter()
            .map(|s| s["name"].as_str().unwrap().to_string())
            .collect()
    }

    fn last_value(spec: &Value, name: &str) -> Option<String> {
        spec["spec"]["cluster"]["settings"]
            .as_array()
            .unwrap()
            .iter()
            .filter(|s| s["name"] == name)
            .next_back()
            .map(|s| s["value"].as_str().unwrap().to_string())
    }

    /// The GUC only governs a password Postgres hashes ITSELF — a client's
    /// `ALTER ROLE ... PASSWORD 'plain'`, and remote_auth when COMPUTE_PASSWORD
    /// carries a plaintext. Left at md5 those would keep producing md5
    /// verifiers, which the `scram-sha-256` hba line then refuses.
    #[test]
    fn a_password_postgres_hashes_itself_is_scram() {
        let spec = render_compute_spec(&params(), &[], &BTreeMap::new(), &BTreeMap::new());
        assert_eq!(
            last_value(&spec, "password_encryption").as_deref(),
            Some("scram-sha-256")
        );
    }

    /// The spec carries no password at all. It used to, inertly
    /// (`skip_pg_catalog_updates`), and the value was an md5 digest that
    /// `Role::to_pg_options` would have prefixed with another literal `md5`. The
    /// role's password is remote_auth's alone.
    #[test]
    fn the_spec_carries_no_role_password() {
        let spec = render_compute_spec(&params(), &[], &BTreeMap::new(), &BTreeMap::new());
        let role = &spec["spec"]["cluster"]["roles"][0];
        assert_eq!(role["name"], "cloud_admin");
        assert!(
            role.get("encrypted_password").is_none(),
            "the compute spec must not carry a role password: {role}"
        );
        assert_eq!(spec["spec"]["skip_pg_catalog_updates"], true);
    }

    /// Precedence is positional, because `compute_ctl` writes these in order and
    /// Postgres keeps the last occurrence. This is the property the whole layering
    /// rests on, and it is the opposite of the daemons' toml.
    #[test]
    fn later_layers_win_on_a_shared_key() {
        let derived = vec![Setting {
            name: "shared_buffers".into(),
            value: "4096MB".into(),
            vartype: "string",
        }];
        let chart = BTreeMap::from([("extension_control_path".to_string(), "$system".to_string())]);
        let registry = BTreeMap::from([
            ("shared_buffers".to_string(), "1GB".to_string()),
            ("work_mem".to_string(), "64MB".to_string()),
        ]);
        let spec = render_compute_spec(&params(), &derived, &chart, &registry);

        // Three occurrences of shared_buffers; the registry's is last and wins.
        let n = names(&spec);
        assert_eq!(n.iter().filter(|x| *x == "shared_buffers").count(), 3);
        assert_eq!(last_value(&spec, "shared_buffers").as_deref(), Some("1GB"));
        assert_eq!(last_value(&spec, "work_mem").as_deref(), Some("64MB"));
        assert_eq!(
            last_value(&spec, "extension_control_path").as_deref(),
            Some("$system")
        );

        // And the derived value still beats the static default when the registry
        // says nothing about it.
        let spec = render_compute_spec(&params(), &derived, &chart, &BTreeMap::new());
        assert_eq!(
            last_value(&spec, "shared_buffers").as_deref(),
            Some("4096MB")
        );
    }

    #[test]
    fn the_spec_carries_the_wiring_the_compute_needs() {
        let spec = render_compute_spec(&params(), &[], &BTreeMap::new(), &BTreeMap::new());
        assert_eq!(
            last_value(&spec, "yb_amp.branch_id").as_deref(),
            Some("9a054903-242f-46bc-a0c9-cc6b91a37e4c")
        );
        assert_eq!(
            last_value(&spec, "yb_amp.pageserver_connstring").as_deref(),
            Some("host=127.0.0.1 port=64000")
        );
        assert_eq!(last_value(&spec, "port").as_deref(), Some("55433"));
        assert_eq!(spec["spec"]["cluster"]["roles"][0]["name"], "cloud_admin");
    }

    /// The WAL-shape invariants: no full-page images from either source, because
    /// durability is in the YB rows the safekeeper writes rather than in fsync'd
    /// Postgres pages. `full_page_writes` is the one this block used to leave
    /// unset, so k8s endpoints took Postgres' `on` and carried images the design
    /// has no use for -- ~12x the WAL volume on random-update workloads --
    /// and `wal_log_hints` was the second source, unaffected by the first being
    /// off. Both stay defaults, not locks: a workload that wants either can set
    /// it per endpoint.
    #[test]
    fn the_wal_invariants_are_defaults_the_operator_can_override() {
        let spec = render_compute_spec(&params(), &[], &BTreeMap::new(), &BTreeMap::new());
        assert_eq!(last_value(&spec, "fsync").as_deref(), Some("off"));
        assert_eq!(
            last_value(&spec, "full_page_writes").as_deref(),
            Some("off")
        );
        assert_eq!(last_value(&spec, "wal_log_hints").as_deref(), Some("off"));
        assert_eq!(last_value(&spec, "wal_level").as_deref(), Some("logical"));

        let registry = BTreeMap::from([
            ("full_page_writes".to_string(), "on".to_string()),
            ("wal_log_hints".to_string(), "on".to_string()),
        ]);
        let spec = render_compute_spec(&params(), &[], &BTreeMap::new(), &registry);
        assert_eq!(last_value(&spec, "full_page_writes").as_deref(), Some("on"));
        assert_eq!(last_value(&spec, "wal_log_hints").as_deref(), Some("on"));
    }

    /// `compute_ctl` gates its external routes on this, and an empty set rejects
    /// every request — a debug build panics on the assertion rather than
    /// answering 401, which surfaces as a dropped connection. So a caller that
    /// holds a key has to see it reach the spec; local endpoints timed out
    /// probing their own `/status` when it did not.
    #[test]
    fn the_callers_jwks_reaches_compute_ctl() {
        let spec = render_compute_spec(&params(), &[], &BTreeMap::new(), &BTreeMap::new());
        assert_eq!(spec["compute_ctl_config"]["jwks"], json!({"keys": []}));

        let key = json!({"keys": [{"kty": "OKP", "crv": "Ed25519", "x": "abc"}]});
        let mut p = params();
        p.compute_ctl_jwks = key.clone();
        let spec = render_compute_spec(&p, &[], &BTreeMap::new(), &BTreeMap::new());
        assert_eq!(spec["compute_ctl_config"]["jwks"], key);
        // And it must survive the typed parse, not just the json! macro.
        let parsed: compute_api::responses::ComputeConfig =
            serde_json::from_value(spec).expect("must deserialize as ComputeConfig");
        assert_eq!(parsed.compute_ctl_config.jwks.keys.len(), 1);
    }

    /// The guard for the shape itself: the rendered file must deserialize into the
    /// very type `compute_ctl` reads it with. Emitting the spec fields at the top
    /// level instead of under "spec" produced a file it could not parse, and only a
    /// check against the real type catches that.
    #[test]
    fn the_rendered_file_deserializes_as_computeconfig() {
        let derived = crate::pg_settings::derive(&crate::resources::Resources {
            memory_bytes: Some(8 << 30),
            vcpu: Some(4.0),
            data_volume_bytes: Some(12 << 30),
        })
        .0;
        // A name the derived layer also sets: this is the payload of a settings
        // reload, and it has to survive the typed parse *and* still be the
        // occurrence that wins.
        assert!(
            derived.iter().any(|s| s.name == "max_parallel_workers"),
            "max_parallel_workers must be derived for this to test precedence"
        );
        let registry = [("max_parallel_workers".to_string(), "3".to_string())]
            .into_iter()
            .collect();
        let spec = render_compute_spec(&params(), &derived, &BTreeMap::new(), &registry);
        let parsed: compute_api::responses::ComputeConfig =
            serde_json::from_value(spec).expect("must deserialize as ComputeConfig");
        let inner = parsed.spec.expect("spec must be present");
        assert_eq!(inner.suspend_timeout_seconds, -1);
        let settings = inner.cluster.settings.expect("settings must be present");
        // The integer GUCs must be declared integer, not string.
        let wp = settings
            .iter()
            .find(|s| s.name == "max_worker_processes")
            .expect("max_worker_processes");
        assert_eq!(wp.vartype, "integer");
        // Postgres takes the last occurrence, so the operator's value has to be
        // the last one here too.
        let last = settings
            .iter()
            .rev()
            .find(|s| s.name == "max_parallel_workers")
            .expect("max_parallel_workers");
        assert_eq!(last.value.as_deref(), Some("3"));
    }
}
