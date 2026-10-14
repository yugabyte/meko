/*
 * Small, operator-managed settings that services must pick up without a
 * rollout. Values remain text so each consumer owns parsing, validation, and
 * a safe fallback for its setting.
 */
CREATE TABLE IF NOT EXISTS meko_system.runtime_config (
    config_key   VARCHAR     NOT NULL,
    config_value VARCHAR     NOT NULL,
    updated_at   TIMESTAMPTZ NOT NULL DEFAULT NOW(),
    PRIMARY KEY (config_key HASH)
);
