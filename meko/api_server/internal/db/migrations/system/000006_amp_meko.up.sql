/*
 * AMP (Agentic Multitenant Postgres) integration.
 *
 * Two tables, created together because they ship as one feature:
 *
 *  - amp_registration: the AMP control-plane endpoint for this Meko deployment
 *    (global singleton). An admin registers it once via meko_admin, which writes
 *    this row directly; api_server resolves the AMP client from it on every AMP
 *    call — so the controller URL is configured at runtime by an operator, not
 *    baked into an env var.
 *
 *  - amp_datapack_pool: the 1:1 mapping from a Meko datapack to its AMP shared
 *    pool, recorded when a user first creates an AMP database inside a datapack.
 *
 * NOTE: amp_registration.password is stored as-is for now (the AMP controller
 * has no auth yet) and is never returned to clients. Move it to a secret store /
 * encrypt at rest before AMP enforces auth.
 */

-- AMP controller registration (global, one per Meko deployment). Enforced
-- singleton: `singleton` is always TRUE and is the primary key, so the table
-- holds at most one row; re-registering upserts it.
CREATE TABLE IF NOT EXISTS meko_system.amp_registration (
    singleton   BOOLEAN     NOT NULL DEFAULT TRUE,
    name        TEXT        NOT NULL,
    base_url    TEXT        NOT NULL,
    username    TEXT        NOT NULL DEFAULT '',
    password    TEXT        NOT NULL DEFAULT '',
    created_at  TIMESTAMPTZ NOT NULL DEFAULT now(),
    updated_at  TIMESTAMPTZ NOT NULL DEFAULT now(),
    PRIMARY KEY (singleton),
    CONSTRAINT amp_registration_singleton CHECK (singleton)
);

-- Maps a Meko datapack to its AMP shared pool (1:1). The pool id IS the
-- datapack_id by construction, so the two systems share one identifier and there
-- is nothing to reconcile; shared_pool_id is stored explicitly so the mapping is
-- self-describing and can diverge later without a schema change. owner_user_id is
-- the Meko user id (which is also the AMP user id). status tracks the
-- provisioning saga (pending -> ready | failed) so a partial failure can be
-- retried or reconciled.
CREATE TABLE IF NOT EXISTS meko_system.amp_datapack_pool (
    datapack_id     UUID        NOT NULL,                     -- backs meko_system.datapacks(datapack_id)
    shared_pool_id  UUID        NOT NULL,                     -- AMP shared pool id (= datapack_id by construction)
    owner_user_id   UUID        NOT NULL,                     -- owning Meko user id (= AMP user id)
    status          TEXT        NOT NULL DEFAULT 'ready',     -- pending | ready | failed
    created_at      TIMESTAMPTZ NOT NULL DEFAULT now(),
    updated_at      TIMESTAMPTZ NOT NULL DEFAULT now(),
    PRIMARY KEY (datapack_id HASH)
);

-- Per-user lookup (cleanup / reconcile). UNIQUE keeps the (owner, datapack)
-- pair one-to-one and lets the query serve datapack_id straight from the index.
CREATE UNIQUE INDEX IF NOT EXISTS idx_amp_datapack_pool_owner
    ON meko_system.amp_datapack_pool (owner_user_id HASH, datapack_id ASC);
