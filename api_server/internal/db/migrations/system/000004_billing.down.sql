DROP TABLE IF EXISTS meko_system.billing_webhook_events;
DROP TABLE IF EXISTS meko_system.billing_subscriptions;
DROP TABLE IF EXISTS meko_system.stripe_customers;

-- Note: ADD VALUE on an enum cannot be cleanly reverted in postgres.
-- The 'starter' / 'growth' / 'scale' enum values stay; any rows on
-- those tiers must be manually migrated back to a legacy value
-- before this migration is rolled back. We intentionally do not
-- attempt a DROP TYPE / CREATE TYPE swap here -- that would require
-- rewriting every meko_system.users row and is far more invasive
-- than a billing-feature rollback warrants.
