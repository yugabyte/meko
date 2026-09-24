/*
 * Distinguish a BYOC environment from a MANAGED one -- control-plane-mode migration
 * set.
 *
 * Modelled on the kind column (000001): TEXT NOT NULL with a DEFAULT, no CHECK, and
 * validated in Go (service.normalizeType). No CHECK because the value is entirely
 * control-plane-minted, so the Go gate is the single source of truth -- and it avoids
 * the idempotent-ADD-CONSTRAINT dance 000011 needed.
 *
 * Default 'BYOC' so every existing row is correct with no backfill: YCP is the BYOC
 * control plane, so an environment predating this is a BYOC one.
 *
 * Guarded with IF NOT EXISTS like every other statement in this set -- migrations run
 * on every boot, so an unguarded ADD COLUMN would fail the second boot rather than
 * being a no-op.
 */
ALTER TABLE meko_system.environments
    ADD COLUMN IF NOT EXISTS type TEXT NOT NULL DEFAULT 'BYOC'; -- BYOC | MANAGED
