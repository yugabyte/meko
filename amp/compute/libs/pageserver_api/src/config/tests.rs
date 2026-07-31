use super::*;

#[test]
fn test_node_metadata_v1_backward_compatibilty() {
    let v1 = serde_json::to_vec(&serde_json::json!({
        "host": "localhost",
        "port": 23,
        "http_host": "localhost",
        "http_port": 42,
    }));

    assert_eq!(
        serde_json::from_slice::<NodeMetadata>(&v1.unwrap()).unwrap(),
        NodeMetadata {
            postgres_host: "localhost".to_string(),
            postgres_port: 23,
            grpc_host: None,
            grpc_port: None,
            http_host: "localhost".to_string(),
            http_port: 42,
            https_port: None,
            other: HashMap::new(),
        }
    )
}

#[test]
fn test_node_metadata_v2_backward_compatibilty() {
    let v2 = serde_json::to_vec(&serde_json::json!({
        "host": "localhost",
        "port": 23,
        "http_host": "localhost",
        "http_port": 42,
        "https_port": 123,
    }));

    assert_eq!(
        serde_json::from_slice::<NodeMetadata>(&v2.unwrap()).unwrap(),
        NodeMetadata {
            postgres_host: "localhost".to_string(),
            postgres_port: 23,
            grpc_host: None,
            grpc_port: None,
            http_host: "localhost".to_string(),
            http_port: 42,
            https_port: Some(123),
            other: HashMap::new(),
        }
    )
}

#[test]
fn test_node_metadata_v3_backward_compatibilty() {
    let v3 = serde_json::to_vec(&serde_json::json!({
        "host": "localhost",
        "port": 23,
        "grpc_host": "localhost",
        "grpc_port": 51,
        "http_host": "localhost",
        "http_port": 42,
        "https_port": 123,
    }));

    assert_eq!(
        serde_json::from_slice::<NodeMetadata>(&v3.unwrap()).unwrap(),
        NodeMetadata {
            postgres_host: "localhost".to_string(),
            postgres_port: 23,
            grpc_host: Some("localhost".to_string()),
            grpc_port: Some(51),
            http_host: "localhost".to_string(),
            http_port: 42,
            https_port: Some(123),
            other: HashMap::new(),
        }
    )
}

/// yb-amp ships SLRU on demand, not in basebackup. A revert to the
/// upstream default (false) reintroduces the 2026-07 compute-startup
/// stall: basebackup packs the whole (workload-bloated) SLRU and the
/// compute reads it one page per RPC, overrunning the Init timeout.
#[test]
fn lazy_slru_download_defaults_on() {
    assert!(
        BranchConfigToml::default().lazy_slru_download,
        "yb-amp default_tenant_conf must keep lazy_slru_download = true"
    );
}

/// `materialize_inflight_bytes_limit` is a memory safety ceiling, so it
/// must resolve verbatim and NEVER be multiplied by `scale_factor` — the
/// opposite of the read-cache / backpressure / queue budgets, which do
/// scale. Scaling a per-instance memory cap on big boxes is exactly the
/// mistake that let the materialiser heap run to OOM; this test pins the
/// invariant so a future refactor can't quietly re-scale it.
#[test]
fn materialize_inflight_bytes_limit_is_not_scaled() {
    let base = YbPageWriterConfig::unscaled_defaults();
    let toml = YbPageWriterConfigToml::default(); // all fields absent
    let scale = 4.0;
    let resolved = toml.resolve(scale);

    assert_eq!(
        resolved.materialize_inflight_bytes_limit, base.materialize_inflight_bytes_limit,
        "inflight-bytes ceiling must be scale-independent"
    );
    // Sanity: the budgets that SHOULD scale still do, so this test is
    // actually exercising a resolve() that applies scale_factor.
    assert_eq!(
        resolved.read_cache_bytes,
        scale_count(base.read_cache_bytes, scale),
        "read_cache_bytes must scale with scale_factor"
    );
    assert_eq!(
        resolved.backpressure_bytes,
        scale_count(base.backpressure_bytes, scale),
        "backpressure_bytes must scale with scale_factor"
    );
}

/// `0` disables the byte cap, and an explicit value is honored verbatim
/// (never scaled) — mirroring the `backpressure_bytes` "0 = off" convention.
#[test]
fn materialize_inflight_bytes_limit_explicit_value_is_verbatim() {
    let toml = YbPageWriterConfigToml {
        materialize_inflight_bytes_limit: Some(0),
        ..Default::default()
    };
    assert_eq!(toml.resolve(4.0).materialize_inflight_bytes_limit, 0);

    let toml = YbPageWriterConfigToml {
        materialize_inflight_bytes_limit: Some(256 * 1024 * 1024),
        ..Default::default()
    };
    assert_eq!(
        toml.resolve(4.0).materialize_inflight_bytes_limit,
        256 * 1024 * 1024
    );
}
