-- Reverse 0008: recreate the 0004 ycp_identity table (see 0004_ycp.up.sql). The
-- controller no longer writes it, so this only restores the schema.
CREATE TABLE IF NOT EXISTS ycp_identity (
    env_name       text        PRIMARY KEY,
    environment_id text        NOT NULL,
    status_token   text        NOT NULL,
    enrolled_at    timestamptz NOT NULL DEFAULT now(),
    updated_at     timestamptz NOT NULL DEFAULT now()
);
