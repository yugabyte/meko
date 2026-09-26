//! The compute spec `compute_ctl` consumes, written before anything starts.
//!
//! The compute spec's setting order is load-bearing and is the one thing to be
//! careful about when reading this: `compute_ctl` writes the settings into
//! `postgresql.conf` in order (`pg_helpers.rs`'s `as_pg_settings`), and Postgres
//! keeps the **last** occurrence of a name. So later entries override earlier
//! ones, and the precedence is
//!
//!   static defaults < resource-derived < chart extension paths < branch < endpoint
//!     < server certificate paths
//!
//! The certificate paths sit past the operator's own settings deliberately: they
//! name files the chart mounted, and a wrong one is a compute that will not start.
//!
//! This is the opposite of the daemons' toml, where a duplicated key is a parse
//! error rather than an override (see [`crate::config`]).

use std::collections::BTreeMap;

use serde_json::{Value, json};

use crate::pg_settings::Setting;

/// `md5<md5(password + user)>`, the form `password_encryption = md5` expects.
pub fn md5_role_password(password: &str, user: &str) -> String {
    let digest = md5::compute(format!("{password}{user}").as_bytes());
    format!("md5{digest:x}")
}

/// The server certificate the compute presents to clients, as the paths the
/// files are mounted at. Paths rather than contents: cert-manager rewrites the
/// files under a running postmaster, and Postgres re-reads both on reload — see
/// [`crate::compute_tls`].
pub struct ComputeTls<'a> {
    pub cert_file: &'a str,
    pub key_file: &'a str,
}

/// The settings the certificate wiring owns, and which an operator therefore
/// cannot set.
///
/// They are rendered last, so an operator's value for any of them is stored and
/// then overridden — which is exactly the case `NotApplied` exists for. Without
/// naming them, `/v1/settings/check` accepts the edit (the value parses),
/// `settings_of` takes the last occurrence so nothing moves, `changed_settings`
/// comes back empty, and the reload answers 200 with nothing not-applied: an edit
/// that silently did nothing, which is the outcome `http.rs` says naming a
/// setting is the only way to prevent.
pub const COMPUTE_TLS_OWNED: [&str; 3] = ["ssl", "ssl_cert_file", "ssl_key_file"];

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
    /// `None` leaves the compute plaintext-only, which is Postgres' own default
    /// and what every endpoint without a certificate gets.
    pub compute_tls: Option<ComputeTls<'a>>,
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
    // Backpressure. Both default to -1, which lets the compute run unboundedly
    // ahead of the pageserver: every byte of that lag is replayed by the next
    // start's basebackup, and pins `amp_storage.wals` above its watermark.
    // Neon's production pair; a larger write lag lengthens the GetPage wait for
    // a recently modified page, a smaller one throttles ordinary bursts.
    push("max_replication_write_lag", "500MB".into(), "string");
    push("max_replication_flush_lag", "10GB".into(), "string");
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
    // 4. The operator's settings, over everything derived.
    for (k, v) in registry {
        push(k, v.clone(), "string");
    }
    // 5. The server certificate, after the operator's settings and so beyond
    // their reach. These are wiring, not tuning: the paths are where the chart
    // mounted the Secret, so an override pointing `ssl_cert_file` elsewhere makes
    // the postmaster refuse to start, and `ssl = off` would silently drop TLS for
    // every client holding a verify-full DSN. `pg_settings` is screened only for
    // what Postgres will parse (`guc::screen`), and a path to nothing parses fine
    // — so position is what protects them.
    //
    // `ssl = on` OFFERS TLS rather than requiring it: pg_hba decides, and
    // compute_ctl writes a `host` rule (`PG_HBA_ALL_SCRAM`), not `hostssl`,
    // and it is the KEYWORD that decides this, not the auth method beside it.
    // A client that
    // asks for TLS gets it and every plaintext client keeps working, which is
    // what makes this safe to turn on under a live endpoint.
    if let Some(tls) = &p.compute_tls {
        push("ssl", "on".into(), "bool");
        push("ssl_cert_file", tls.cert_file.into(), "string");
        push("ssl_key_file", tls.key_file.into(), "string");
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
            compute_tls: None,
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

    /// No certificate means no `ssl` at all, rather than `ssl = off`: Postgres'
    /// own default is off, and an explicit entry would be one more name a
    /// registry override collides with.
    #[test]
    fn a_plaintext_compute_says_nothing_about_ssl() {
        let spec = render_compute_spec(&params(), &[], &BTreeMap::new(), &BTreeMap::new());
        assert!(!names(&spec).iter().any(|n| n.starts_with("ssl")));
    }

    /// The paths reach Postgres verbatim, because they are where the chart
    /// mounted the Secret — deriving or rewriting them would point the postmaster
    /// at a file that is not there, and it refuses to start.
    #[test]
    fn a_mounted_certificate_turns_ssl_on_with_its_own_paths() {
        let mut p = params();
        p.compute_tls = Some(ComputeTls {
            cert_file: "/etc/amp/tls/compute/tls.crt",
            key_file: "/etc/amp/tls/compute/tls.key",
        });
        let spec = render_compute_spec(&p, &[], &BTreeMap::new(), &BTreeMap::new());
        assert_eq!(last_value(&spec, "ssl").as_deref(), Some("on"));
        assert_eq!(
            last_value(&spec, "ssl_cert_file").as_deref(),
            Some("/etc/amp/tls/compute/tls.crt")
        );
        assert_eq!(
            last_value(&spec, "ssl_key_file").as_deref(),
            Some("/etc/amp/tls/compute/tls.key")
        );
    }

    /// The certificate paths are the one thing the operator's own settings do not
    /// win against. `pg_settings` is screened for what Postgres will parse, and a
    /// path to a file that is not there parses fine — so an override would be a
    /// compute that refuses to start, or a silent loss of TLS for every client
    /// holding a verify-full DSN.
    #[test]
    fn an_override_cannot_move_or_disable_the_certificate() {
        let mut p = params();
        p.compute_tls = Some(ComputeTls {
            cert_file: "/etc/amp/tls/compute/tls.crt",
            key_file: "/etc/amp/tls/compute/tls.key",
        });
        let registry = BTreeMap::from([
            ("ssl".to_string(), "off".to_string()),
            ("ssl_cert_file".to_string(), "/tmp/mine.crt".to_string()),
            ("ssl_key_file".to_string(), "/tmp/mine.key".to_string()),
            // A real tuning knob beside them still wins, which is the point:
            // this is a position, not a deny-list.
            ("work_mem".to_string(), "64MB".to_string()),
        ]);
        let spec = render_compute_spec(&p, &[], &BTreeMap::new(), &registry);
        assert_eq!(last_value(&spec, "ssl").as_deref(), Some("on"));
        assert_eq!(
            last_value(&spec, "ssl_cert_file").as_deref(),
            Some("/etc/amp/tls/compute/tls.crt")
        );
        assert_eq!(
            last_value(&spec, "ssl_key_file").as_deref(),
            Some("/etc/amp/tls/compute/tls.key")
        );
        assert_eq!(last_value(&spec, "work_mem").as_deref(), Some("64MB"));
    }

    /// Winning by position is only half the contract: an operator who set one of
    /// these must be TOLD, because the value parses, is stored, and then does
    /// nothing. `COMPUTE_TLS_OWNED` is the list the render and the check both
    /// report from, so the two cannot drift.
    #[test]
    fn the_certificate_settings_are_named_as_the_ones_an_operator_cannot_set() {
        let mut p = params();
        p.compute_tls = Some(ComputeTls {
            cert_file: "/etc/amp/tls/compute/tls.crt",
            key_file: "/etc/amp/tls/compute/tls.key",
        });
        let spec = render_compute_spec(&p, &[], &BTreeMap::new(), &BTreeMap::new());
        let rendered: Vec<String> = names(&spec)
            .into_iter()
            .filter(|n| n.starts_with("ssl"))
            .collect();
        for owned in COMPUTE_TLS_OWNED {
            assert!(
                rendered.iter().any(|n| n == owned),
                "{owned} is declared un-settable but is not rendered"
            );
        }
        assert_eq!(
            rendered.len(),
            COMPUTE_TLS_OWNED.len(),
            "the render puts out {rendered:?} but only {COMPUTE_TLS_OWNED:?} are reported to the \
             caller as not-applied"
        );
    }

    /// The md5 form is what `password_encryption = md5` expects; a wrong digest
    /// means the compute comes up with a password nobody can authenticate with.
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

    /// Backpressure is armed by default, and stays a default: an endpoint that
    /// wants more burst headroom raises it through `pg_settings`.
    #[test]
    fn backpressure_is_a_default_the_operator_can_override() {
        let spec = render_compute_spec(&params(), &[], &BTreeMap::new(), &BTreeMap::new());
        assert_eq!(
            last_value(&spec, "max_replication_write_lag").as_deref(),
            Some("500MB")
        );
        assert_eq!(
            last_value(&spec, "max_replication_flush_lag").as_deref(),
            Some("10GB")
        );

        let registry =
            BTreeMap::from([("max_replication_write_lag".to_string(), "1GB".to_string())]);
        let spec = render_compute_spec(&params(), &[], &BTreeMap::new(), &registry);
        assert_eq!(
            last_value(&spec, "max_replication_write_lag").as_deref(),
            Some("1GB")
        );
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
