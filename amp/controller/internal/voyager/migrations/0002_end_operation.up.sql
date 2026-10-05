-- Idempotent: safe to replay when the DROP committed but the ADD did not.
-- YB DDL is not transactional, so both statements must tolerate partial prior runs.
ALTER TABLE amp_voyager_control.migration_run
    DROP CONSTRAINT IF EXISTS migration_run_operation_check;

ALTER TABLE amp_voyager_control.migration_run
    ADD CONSTRAINT migration_run_operation_check
    CHECK (operation IN ('assess', 'migrate', 'end'));
