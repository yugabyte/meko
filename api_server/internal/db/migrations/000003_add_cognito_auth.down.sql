DROP INDEX IF EXISTS meko_system.idx_account_registry_cognito_sub;

ALTER TABLE meko_system.account_registry
    DROP COLUMN IF EXISTS email_verified_at,
    DROP COLUMN IF EXISTS cognito_sub;
