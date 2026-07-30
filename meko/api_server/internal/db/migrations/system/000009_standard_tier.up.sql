/*
 * Add the `standard` tier to meko_system.account_type and create the
 * marketplace_entitlements table that Standard subscribers land in.
 *
 * Enum extension: mirrors the pattern 000004_billing already uses for
 * starter/growth/scale/enterprise -- ALTER TYPE ADD VALUE IF NOT EXISTS,
 * which is idempotent and does not rewrite any existing rows. No fresh
 * type + swap + drop is needed: starter/growth/scale are inert in
 * practice (no rows on those tiers) and Postgres cannot DROP enum
 * values anyway, so leaving them in place has zero cost.
 *
 * marketplace_entitlements: parallel table to stripe_customers /
 * billing_subscriptions for users who subscribe via AWS Marketplace
 * instead of the Stripe checkout. Kept separate (not one polymorphic
 * `subscriptions` table) so the Stripe-specific columns / Marketplace-
 * specific columns don't sprawl in one row and so tier resolution can
 * prefer whichever source is authoritative for each user.
 *
 * Flow:
 *   1. Buyer subscribes on AWS Marketplace.
 *   2. AWS POSTs a form-encoded `x-amzn-marketplace-token` to our
 *      `POST /marketplace/register` fulfillment URL.
 *   3. api_server exchanges the token via ResolveCustomer, then
 *      GetEntitlements to learn which dimension (standard today) the
 *      buyer picked.
 *   4. Row upserted here; users.user_type updated to match.
 *
 * Entitlement changes (upgrade / downgrade / cancel) arrive later
 * over SNS at `POST /marketplace/sns`; the handler re-runs
 * GetEntitlements and updates this row.
 *
 * PK design: ((user_id) HASH). Lookups are always user_id -> tier;
 * the reverse path (aws_customer_id -> user_id) is needed in the SNS
 * handler so it gets a secondary unique index.
 */

ALTER TYPE meko_system.account_type ADD VALUE IF NOT EXISTS 'standard';

CREATE TABLE IF NOT EXISTS meko_system.marketplace_entitlements (
    user_id             UUID NOT NULL,                    -- meko_system.users(user_id)
    -- AWS-issued identifier returned by ResolveCustomer. Stable per
    -- (buyer AWS account × product), so the SNS handler uses it to
    -- find the mapped Meko user without needing the token again.
    aws_customer_id     VARCHAR(255) NOT NULL,
    -- Buyer's AWS account id (12 digits). Kept for support/debug and
    -- to distinguish two subscriptions from the same organisation.
    aws_account_id      VARCHAR(32),
    -- Product code AWS assigns when your listing goes live. Cached
    -- here so the SNS handler can filter by product cheaply when we
    -- eventually list more than one Meko product.
    product_code        VARCHAR(255) NOT NULL,
    -- Which dimension/tier the buyer picked. Populated from the
    -- entitlement's Dimension field.
    plan_id             meko_system.account_type NOT NULL,
    -- ENTITLED | EXPIRED. Anything other than ENTITLED collapses to
    -- free in the user_type sync path.
    status              VARCHAR(32) NOT NULL,
    -- Entitlement expiry as reported by AWS; used to enforce hard
    -- caps once the contract lapses without an SNS event landing.
    expires_at          TIMESTAMPTZ,
    created_at          TIMESTAMPTZ NOT NULL DEFAULT NOW(),
    updated_at          TIMESTAMPTZ NOT NULL DEFAULT NOW(),
    PRIMARY KEY ((user_id) HASH)
) SPLIT INTO 4 TABLETS;

CREATE UNIQUE INDEX IF NOT EXISTS marketplace_entitlements_aws_customer_idx
    ON meko_system.marketplace_entitlements (aws_customer_id);
