//! Per-tenant location configuration, in-memory only.
//!
//! In yb-mode a tenant is either *attached* to this pageserver (with a
//! storage_controller-assigned generation number used for fencing YB
//! writes) or it isn't here at all. yb-mode tenants are always
//! unsharded — YugabyteDB tablet-shards the keyspace internally — so
//! `LocationConf` has nothing left but the generation.
//!
//! Source of truth for attachment is storage_controller (sent at
//! startup via the re-attach upcall, and at runtime via the
//! location_config PUT). `TenantConfig` lives in
//! `pageserver.tenant_config` (task 17.c). Neither is persisted on
//! the pageserver's local disk.

use pageserver_api::models;
use serde::{Deserialize, Serialize};
use utils::generation::Generation;

#[derive(Clone, PartialEq, Eq, Serialize, Deserialize)]
pub(crate) struct LocationConf {
    /// storage_controller-assigned generation, used to fence YB writes
    /// against stale pageservers during failover.
    pub(crate) generation: Generation,
}

impl std::fmt::Debug for LocationConf {
    fn fmt(&self, f: &mut std::fmt::Formatter<'_>) -> std::fmt::Result {
        write!(f, "Attached gen={:?}", self.generation)
    }
}

impl LocationConf {
    /// Construct an attachment record.
    pub(crate) fn attached(generation: Generation) -> Self {
        Self { generation }
    }

    /// Decode an external `LocationConfig` request into a
    /// `LocationConf`. Fails if `generation` is absent — detachment
    /// is signaled by the absence of a `LocationConf`, not by
    /// decoding an empty one.
    pub(crate) fn try_from(conf: &'_ models::LocationConfig) -> anyhow::Result<Self> {
        let generation = conf
            .generation
            .map(Generation::new)
            .ok_or_else(|| anyhow::anyhow!("Generation must be set when attaching"))?;
        Ok(Self { generation })
    }
}

impl Default for LocationConf {
    // TODO: this should be removed once tenant loading can guarantee
    // that we are never loading from a directory without a configuration.
    // => tech debt since https://github.com/neondatabase/neon/issues/1555
    fn default() -> Self {
        Self {
            generation: Generation::none(),
        }
    }
}

#[cfg(test)]
mod tests {
    #[test]
    fn serde_roundtrip_tenant_conf_opt() {
        let small_conf = pageserver_api::models::TenantConfig {
            gc_horizon: Some(42),
            ..Default::default()
        };

        let toml_form = toml_edit::ser::to_string(&small_conf).unwrap();
        assert_eq!(toml_form, "gc_horizon = 42\n");
        assert_eq!(small_conf, toml_edit::de::from_str(&toml_form).unwrap());

        let json_form = serde_json::to_string(&small_conf).unwrap();
        assert_eq!(json_form, "{\"gc_horizon\":42}");
        assert_eq!(small_conf, serde_json::from_str(&json_form).unwrap());
    }
}
