/*
 * Per-resource generation on desired state.
 *
 * The control plane bumps a resource's generation when its spec changes, so the
 * reconcile-agent can tell an unchanged resource (skip) from a mutated one
 * (re-apply) without keeping its own store. See ReplaceDesired.
 *
 * Defaults to 0 (not 1): a backend that cannot echo the applied generation back
 * on List relies on the engine's in-process cache to skip no-op ticks, and that
 * cache is empty after a restart — at generation 0 the diff is a clean no-op
 * cache-cold. IF NOT EXISTS so this stays a no-op on a DB that already ran the
 * control-plane migration set (which adds the same column).
 */
ALTER TABLE meko_system.environment_desired_resources
    ADD COLUMN IF NOT EXISTS generation BIGINT NOT NULL DEFAULT 0;
