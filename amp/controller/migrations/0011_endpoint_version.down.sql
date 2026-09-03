-- Reverse 0011.
--
-- A real drop, unlike `0005`'s no-op down: this migration is the column's
-- definition — no earlier migration creates it — so dropping it cannot destroy
-- another migration's work. The recorded versions go with it, which is what a
-- downgrade to a controller that cannot read them means.

ALTER TABLE amp_registry.endpoints
    DROP COLUMN IF EXISTS version,
    DROP COLUMN IF EXISTS target_version;
