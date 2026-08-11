-- 0004: YCP identity — the durable enrollment identity the controller resumes on
-- restart, so a restart reports against the existing environment rather than
-- enrolling again with the control plane.
--
-- The controller is configured with a single YCP environment name (--ycp-env-name)
-- and enrolls once, exchanging its one-time enrollment token for a long-lived,
-- environment-scoped status token. That identity is persisted here so a pod
-- restart loads it and reports directly, without needing the enrollment token
-- (which is short-lived) to still be valid.
--
-- It lives in the controller's own schema (the one --db-schema names) alongside
-- the task log, written unqualified, because it is operational state of the
-- controller — not a resource a user addresses. env_name is the PRIMARY KEY: at
-- most one identity per configured environment name.
CREATE TABLE ycp_identity (
    env_name       text        PRIMARY KEY,
    environment_id text        NOT NULL,
    status_token   text        NOT NULL,
    enrolled_at    timestamptz NOT NULL DEFAULT now(),
    updated_at     timestamptz NOT NULL DEFAULT now()
);
