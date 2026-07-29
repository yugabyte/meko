-- marketplace_entitlements is now owned by 000009_standard_tier and
-- dropped in that migration's down file. Kept here as a defensive no-op
-- (nothing else references it, and it may still exist on dev pods that
-- ran the earlier revision of this file).
DROP TABLE IF EXISTS meko_system.marketplace_entitlements;
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
