/*
 * Billing / Stripe integration.
 *
 * Adds the 5-tier plan enum values, a Stripe customer mapping, a
 * per-user subscription record, and a webhook-event dedupe table. The
 * api_server's BillingService is the only writer for the latter three;
 * the meko UI reads `user_type` off meko_system.users (kept in sync by
 * the webhook handler) but never touches the billing_* tables directly.
 *
 * Plan tiers map to meko_system.account_type values:
 *   free       -- default, no subscription row
 *   starter    -- $16/mo Stripe price
 *   growth     -- $64/mo Stripe price  (legacy "pro" rows continue to
 *                 work; the UI maps pro -> growth at render time)
 *   scale      -- $256/mo Stripe price
 *   enterprise -- billed off-platform, no Stripe subscription
 */

ALTER TYPE meko_system.account_type ADD VALUE IF NOT EXISTS 'starter';
ALTER TYPE meko_system.account_type ADD VALUE IF NOT EXISTS 'growth';
ALTER TYPE meko_system.account_type ADD VALUE IF NOT EXISTS 'scale';
ALTER TYPE meko_system.account_type ADD VALUE IF NOT EXISTS 'enterprise';


/*
 * Stripe customer mapping.
 *
 * One row per Meko user that has ever entered a Checkout flow. We
 * create it lazily on the first POST /billing/checkout-session so
 * Free-tier users never hit Stripe at all.
 *
 * PK design: ((user_id) HASH). Lookups are always user_id -> Stripe
 * customer id; the reverse path (customer_id -> user_id) is needed in
 * the webhook handler so it gets a secondary unique index.
 */
CREATE TABLE IF NOT EXISTS meko_system.stripe_customers (
    user_id             UUID NOT NULL,                    -- meko_system.users(user_id)
    stripe_customer_id  VARCHAR(64) NOT NULL,             -- e.g. "cus_NffrFeUfNV2Hib"
    email               VARCHAR NOT NULL,                 -- cached so webhooks don't need a users join
    created_at          TIMESTAMPTZ NOT NULL DEFAULT NOW(),
    updated_at          TIMESTAMPTZ NOT NULL DEFAULT NOW(),
    PRIMARY KEY ((user_id) HASH)
) SPLIT INTO 4 TABLETS;

CREATE UNIQUE INDEX IF NOT EXISTS stripe_customers_customer_id_idx
    ON meko_system.stripe_customers (stripe_customer_id);


/*
 * Active subscription state per user.
 *
 * One row per user. We collapse historical sub rows into a single
 * current-state record because the UI only ever shows "what plan am
 * I on right now / when does it renew"
 *
 * plan_id duplicates the value we set on meko_system.users.user_type
 * so a stale user_type row (race between webhook and UI read) can
 * still be reconciled from this table without parsing the Stripe
 * price_id again.
 */
CREATE TABLE IF NOT EXISTS meko_system.billing_subscriptions (
    user_id                  UUID NOT NULL,                 -- meko_system.users(user_id)
    stripe_customer_id       VARCHAR(64) NOT NULL,
    stripe_subscription_id   VARCHAR(64) NOT NULL,
    stripe_price_id          VARCHAR(64) NOT NULL,
    -- One of the Stripe sub statuses: incomplete, incomplete_expired,
    -- trialing, active, past_due, canceled, unpaid, paused.
    -- We treat (active | trialing) as "paid"; everything else falls
    -- back to free in the user_type sync path.
    status                   VARCHAR(32) NOT NULL,
    plan_id                  meko_system.account_type NOT NULL,
    -- Both timestamps come straight from the Stripe sub object. Used
    -- by the UI to render "Renews May 28, 2026" and to enforce hard
    -- caps once the period rolls over.
    current_period_start     TIMESTAMPTZ,
    current_period_end       TIMESTAMPTZ,
    cancel_at_period_end     BOOLEAN NOT NULL DEFAULT FALSE,
    created_at               TIMESTAMPTZ NOT NULL DEFAULT NOW(),
    updated_at               TIMESTAMPTZ NOT NULL DEFAULT NOW(),
    PRIMARY KEY ((user_id) HASH)
) SPLIT INTO 4 TABLETS;

CREATE UNIQUE INDEX IF NOT EXISTS billing_subscriptions_subscription_id_idx
    ON meko_system.billing_subscriptions (stripe_subscription_id);


/*
 * Webhook event idempotency log.
 *
 * Stripe retries failed webhook deliveries on an exponential schedule;
 * a successful 200 from us is the only signal they consider durable.
 * We insert into this table inside the same transaction as the state
 * change so a retried delivery short-circuits in the handler instead
 * of double-applying.
 *
 * Rows older than the Stripe retry window (~3 days) carry no value;
 * a future maintenance job can prune them, but today the table is
 * small enough that we don't bother.
 *
 * PK design: ((event_id) HASH). All lookups are exact-match by id.
 */
CREATE TABLE IF NOT EXISTS meko_system.billing_webhook_events (
    event_id      VARCHAR(64) NOT NULL,                   -- "evt_..." from Stripe
    event_type    VARCHAR(64) NOT NULL,                   -- "invoice.paid", etc.
    processed_at  TIMESTAMPTZ NOT NULL DEFAULT NOW(),
    PRIMARY KEY ((event_id) HASH)
) SPLIT INTO 2 TABLETS;
