-- Rollback caveat: this deletes end-run rows and their task provenance is
-- orphaned. A migration that was ended will derive as "completed" again after
-- rollback, but the export directory has already been cleaned up by Voyager.
DELETE FROM amp_voyager_control.migration_run
WHERE operation = 'end';

ALTER TABLE amp_voyager_control.migration_run
    DROP CONSTRAINT IF EXISTS migration_run_operation_check;

ALTER TABLE amp_voyager_control.migration_run
    ADD CONSTRAINT migration_run_operation_check
    CHECK (operation IN ('assess', 'migrate'));
