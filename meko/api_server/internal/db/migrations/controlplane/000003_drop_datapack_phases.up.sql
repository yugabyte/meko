-- Drop the column 000002 added: YCP no longer models datapacks, so nothing writes or
-- reads it. An environment is the unit; what runs inside Meko is Meko's own concern.
--
-- A forward migration rather than deleting 000002, which is what was tried first and
-- is a mistake worth not repeating: golang-migrate records the applied version, and a
-- database that has run 000002 fails outright when the file is gone —
--
--   migration failed: no migration found for version 2:
--     read down for version 2 migrations/controlplane: file does not exist
--
-- — so every already-migrated control plane crash-loops its migrate init container on
-- the next deploy. A migration that has run anywhere is history: add to it, never
-- rewrite it.
ALTER TABLE meko_system.environments
    DROP COLUMN IF EXISTS datapack_phases;
