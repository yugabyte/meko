-- Controller-owned Voyager migration state. Voyager itself owns and migrates
-- ybvoyager_visualizer; controller migrations must never create those objects.

CREATE SCHEMA IF NOT EXISTS amp_voyager_control;

CREATE TABLE amp_voyager_control.migration (
    -- This id is also injected into Voyager as its migration_uuid.
    id              uuid PRIMARY KEY DEFAULT gen_random_uuid(),
    name            text        NOT NULL,
    source_config   jsonb       NOT NULL,
    target_config   jsonb,
    target_task_id  uuid,
    created_at      timestamptz NOT NULL DEFAULT now(),
    updated_at      timestamptz NOT NULL DEFAULT now()
);

CREATE TABLE amp_voyager_control.migration_run (
    id                uuid PRIMARY KEY,
    migration_id      uuid NOT NULL REFERENCES amp_voyager_control.migration (id),
    operation         text NOT NULL
                      CHECK (operation IN ('assess', 'migrate')),
    attempt           int  NOT NULL,
    status            text NOT NULL DEFAULT 'pending'
                      CHECK (status IN ('pending', 'running', 'succeeded', 'failed')),
    error_summary     text,
    deployer_type     text,
    deployer_metadata jsonb,
    task_id           uuid,
    started_at        timestamptz,
    finished_at       timestamptz,
    created_at        timestamptz NOT NULL DEFAULT now(),
    updated_at        timestamptz NOT NULL DEFAULT now(),
    UNIQUE (migration_id, operation, attempt)
);

-- Assess and migrate jobs must never overlap for one migration.
CREATE UNIQUE INDEX one_active_run_per_migration
    ON amp_voyager_control.migration_run (migration_id)
    WHERE status IN ('pending', 'running');
