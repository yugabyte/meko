-- 0007: write-once YCP binding. Records the single YCP environment this
-- controller is bound to, so a second reconcile-agent pointed at a different YCP
-- cannot quietly take it over — the bind endpoint (POST /api/v1/ycp/binding)
-- rejects a conflicting (ycp_url, environment_id). See ac-ycp.md.
--
-- Singleton by construction: the boolean PRIMARY KEY pinned to true admits at
-- most one row, so "the binding" is unambiguous and the first writer wins.
-- Lives in the controller's own schema (written unqualified) alongside the task
-- log: it is operational state of the controller, not a user-addressable
-- resource.
CREATE TABLE ycp_binding (
    singleton      boolean     PRIMARY KEY DEFAULT true CHECK (singleton),
    ycp_url        text        NOT NULL,
    environment_id text        NOT NULL,
    bound_at       timestamptz NOT NULL DEFAULT now()
);
