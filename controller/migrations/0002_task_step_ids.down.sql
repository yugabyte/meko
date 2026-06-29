-- Revert 0002: restore the 0001 shape — task_step keyed by (task_id, ordinal),
-- task without a step_ids list. No data is preserved (the tables are unused).

DROP TABLE IF EXISTS task_step;
DROP TABLE IF EXISTS task;

CREATE TABLE task (
    id           uuid PRIMARY KEY DEFAULT gen_random_uuid(),
    kind         text        NOT NULL,
    status       task_status NOT NULL DEFAULT 'pending',
    project_id   uuid,
    target_ref   text,
    params       jsonb       NOT NULL DEFAULT '{}'::jsonb,
    error        text,
    owner        text,
    lease_expiry timestamptz,
    created_at   timestamptz NOT NULL DEFAULT now(),
    updated_at   timestamptz NOT NULL DEFAULT now()
);

CREATE TABLE task_step (
    task_id    uuid        NOT NULL REFERENCES task (id),
    ordinal    int         NOT NULL,
    kind       text        NOT NULL,
    status     step_status NOT NULL DEFAULT 'pending',
    args       jsonb       NOT NULL DEFAULT '{}'::jsonb,
    result     jsonb,
    error      text,
    updated_at timestamptz NOT NULL DEFAULT now(),
    PRIMARY KEY (task_id, ordinal)
);

CREATE INDEX task_runnable ON task (status) WHERE status IN ('pending', 'running');
