//! Per-tenant configuration, persisted to `<workdir>/tenants/<id>/config-v1`.
//!
//! In yb-mode a tenant is either *attached* to this pageserver (with a
//! storage_controller-assigned generation number used for fencing YB
//! writes) or it isn't here at all. Detachment is represented by
//! absence of the on-disk config file, so [`LocationConf`] only models
//! the attached state.

use pageserver_api::models;
use pageserver_api::shard::{ShardCount, ShardIdentity, ShardNumber, ShardStripeSize};
use serde::{Deserialize, Serialize};
use utils::critical;
use utils::generation::Generation;

/// Per-tenant, per-pageserver attachment record. All pageservers use
/// the same `tenant_conf`; the generation is per-attachment.
#[derive(Clone, PartialEq, Eq, Serialize, Deserialize)]
pub(crate) struct LocationConf {
    /// storage_controller-assigned generation, used to fence YB writes
    /// against stale pageservers during failover.
    pub(crate) generation: Generation,

    /// Full shard identity. Already implied by `TenantShardId`, but
    /// the explicit `ShardIdentity` lets us compute key→shard mappings
    /// and agree with storage_controller on the intended stripe size.
    pub(crate) shard: ShardIdentity,

    /// Pan-cluster tenant tunables; identical on every pageserver
    /// that attaches the tenant. Will eventually move to YB
    /// (task 17.c).
    pub(crate) tenant_conf: pageserver_api::models::TenantConfig,
}

impl std::fmt::Debug for LocationConf {
    fn fmt(&self, f: &mut std::fmt::Formatter<'_>) -> std::fmt::Result {
        write!(f, "Attached gen={:?}", self.generation)
    }
}

impl LocationConf {
    /// Construct an attachment record. Equivalent to the old
    /// `attached_single` — there is no other attachment mode in yb-mode.
    pub(crate) fn attached(
        tenant_conf: pageserver_api::models::TenantConfig,
        generation: Generation,
        shard_params: models::ShardParameters,
    ) -> Self {
        Self {
            generation,
            shard: ShardIdentity::from_params(ShardNumber(0), shard_params),
            tenant_conf,
        }
    }

    /// Update the generation stored in this record. Called on
    /// attach / re-attach.
    pub(crate) fn attach_in_generation(
        &mut self,
        generation: Generation,
        stripe_size: ShardStripeSize,
    ) {
        self.generation = generation;
        // This should never happen.
        // TODO: turn this into a proper assertion.
        if stripe_size != self.shard.stripe_size {
            critical!(
                "stripe size mismatch: {} != {}",
                self.shard.stripe_size,
                stripe_size,
            );
        }
        self.shard.stripe_size = stripe_size;
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

        let shard = if conf.shard_count == 0 {
            // NB: carry over the persisted stripe size instead of
            // using the default. Unsharded tenants don't use the
            // stripe size, but storage_controller and the pageserver
            // need to agree on it for future splits.
            ShardIdentity::unsharded_with_stripe_size(ShardStripeSize(conf.shard_stripe_size))
        } else {
            ShardIdentity::new(
                ShardNumber(conf.shard_number),
                ShardCount::new(conf.shard_count),
                ShardStripeSize(conf.shard_stripe_size),
            )?
        };

        Ok(Self {
            generation,
            shard,
            tenant_conf: conf.tenant_conf.clone(),
        })
    }
}

impl Default for LocationConf {
    // TODO: this should be removed once tenant loading can guarantee
    // that we are never loading from a directory without a configuration.
    // => tech debt since https://github.com/neondatabase/neon/issues/1555
    fn default() -> Self {
        Self {
            generation: Generation::none(),
            tenant_conf: pageserver_api::models::TenantConfig::default(),
            shard: ShardIdentity::unsharded(),
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
