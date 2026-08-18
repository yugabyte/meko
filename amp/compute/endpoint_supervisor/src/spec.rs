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

/// `md5<md5(password + user)>`, the form `password_encryption = md5` expects.
pub fn md5_role_password(password: &str, user: &str) -> String {
    let digest = md5::compute(format!("{password}{user}").as_bytes());
    format!("md5{digest:x}")
}

/// Everything the compute spec needs that is not a Postgres setting.
pub struct ComputeSpecParams<'a> {
    pub compute_port: u16,
    pub branch_id: &'a str,
    pub safekeeper_addr: &'a str,
    pub pageserver_connstring: &'a str,
    pub shared_preload_libraries: &'a str,
    /// Already md5-hashed; see [`md5_role_password`].
    pub cloud_admin_password_md5: &'a str,
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
    push("wal_level", "logical".into(), "enum");
    push("wal_log_hints", "on".into(), "bool");
    push("port", p.compute_port.to_string(), "integer");
    push("listen_addresses", "0.0.0.0".into(), "string");
    push("max_connections", "100".into(), "integer");
    push("shared_buffers", "128MB".into(), "string");
    push("max_wal_senders", "10".into(), "integer");
    push("max_replication_slots", "10".into(), "integer");
    push("password_encryption", "md5".into(), "enum");
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
                "roles": [{
                    "name": "cloud_admin",
                    "encrypted_password": p.cloud_admin_password_md5,
                    "options": null,
                }],
                "databases": [],
                "settings": settings,
            },
            "skip_pg_catalog_updates": true,
        },
        "compute_ctl_config": {"jwks": {"keys": []}},
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
            cloud_admin_password_md5: "md5deadbeef",
            endpoint_id: Some("3f2b1c4e-0000-4000-8000-000000000001"),
            yb_storage_conn: json!({"type": "YbStore", "host_ports": "h:5433",
                                    "user": "yugabyte", "dbname": "yb_storage"}),
            autoprewarm: false,
            offload_lfc_interval_seconds: None,
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

    /// The md5 form is what `password_encryption = md5` expects; a wrong digest
    /// means the compute comes up with a password nobody can authenticate with.
    #[test]
    fn md5_role_password_matches_postgres() {
        // Same construction Postgres uses: md5(password || username).
        assert_eq!(
            md5_role_password("cloud_admin", "cloud_admin"),
            format!("md5{:x}", md5::compute(b"cloud_admincloud_admin"))
        );
        assert!(md5_role_password("a", "b").starts_with("md5"));
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
        assert_eq!(
            spec["spec"]["cluster"]["roles"][0]["encrypted_password"],
            "md5deadbeef"
        );
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
        let spec = render_compute_spec(&params(), &derived, &BTreeMap::new(), &BTreeMap::new());
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
    }
}
