ALTER TABLE meko_system.account_registry
    ADD COLUMN IF NOT EXISTS cognito_sub VARCHAR(255) UNIQUE,
    ADD COLUMN IF NOT EXISTS email_verified_at TIMESTAMPTZ;

CREATE INDEX IF NOT EXISTS idx_account_registry_cognito_sub
    ON meko_system.account_registry (cognito_sub);
