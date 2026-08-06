ALTER TABLE meko_system.environments
    ADD COLUMN IF NOT EXISTS datapack_phases JSONB NOT NULL DEFAULT '[]'::jsonb;
