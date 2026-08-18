/*
 * Per-resource generation on desired state (control-plane-mode migration set).
 * Same column as system/000011_desired_generation.up.sql, in portable SQL.
 *
 * The control plane bumps a resource's generation when its spec changes, so the
 * reconcile-agent can tell an unchanged resource (skip) from a mutated one
 * (re-apply). Defaults to 0 so a backend that cannot echo the generation on List
 * diffs to a clean no-op cache-cold. IF NOT EXISTS so pointing this set at a DB
 * that already ran system/000011 is a no-op.
 */
ALTER TABLE meko_system.environment_desired_resources
    ADD COLUMN IF NOT EXISTS generation BIGINT NOT NULL DEFAULT 0;
