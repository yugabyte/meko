/*
 * Rollback for 000009_standard_tier.up.sql.
 *
 * marketplace_entitlements is dropped cleanly. The `standard` enum
 * member cannot be removed -- Postgres does not support DROP VALUE on
 * an enum. Any rows currently on `standard` are collapsed to `free`
 * first so the enum member is unreferenced (harmless to leave in
 * place). If a real rollback ever needs to purge the value entirely,
 * that requires the same fresh-type-swap dance 000004 avoided in the
 * first place, and would have to be done in a follow-up migration.
 */

-- Any current 'standard' rows collapse to 'free' before we drop the
-- table so no dangling references to the retired plan_id remain.
UPDATE meko_system.users
   SET user_type = 'free'::meko_system.account_type
 WHERE user_type::text = 'standard';

UPDATE meko_system.billing_subscriptions
   SET plan_id = 'free'::meko_system.account_type
 WHERE plan_id::text = 'standard';

DROP INDEX IF EXISTS meko_system.marketplace_entitlements_aws_customer_idx;
DROP TABLE IF EXISTS meko_system.marketplace_entitlements;
