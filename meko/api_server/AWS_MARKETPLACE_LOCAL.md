# AWS Marketplace Integration Runbook

End-to-end setup for the AWS Marketplace SaaS Contract integration.
Covers local dev, production deploy, and the full request flow so anyone
picking this up can wire it end-to-end.

---

## Overview

Meko's Standard tier is sold via an AWS Marketplace SaaS Contract
listing. Buyers subscribe on AWS, AWS redirects them to our
fulfillment URL with a single-use token, and Meko resolves that token
into an entitlement + flips the user's `user_type` to `standard`.

Two flows:
- **Flow A — Existing Meko user upgrades**: user clicks Upgrade in
  Settings → lands on AWS listing → subscribes → AWS redirects back →
  Meko finalizes with the existing session.
- **Flow B — Brand-new buyer**: buyer discovers Meko on AWS
  Marketplace, subscribes, then Meko presents a signup flow that
  carries the AWS token through → after account creation + login, the
  same finalize step runs.

---

## Local development

### Prerequisites

- Meko `api_server` builds cleanly (`go build ./...`)
- Yugabyte / Meko system DB running locally
- Meko UI dev server available on `http://localhost:3010`
- AWS account with access to the marketplace-seller role
- `ngrok` account (free tier is enough)

### Env vars

#### meko_ui pod env (runtime)

The meko_ui express server reads these from `process.env` at boot
and exposes them via `/runtime-config.js` → `window.__MEKO_RUNTIME__`.
Client code reads them via `readRuntimeEnv()`. Ops can flip either
flag by setting an env on the pod and restarting — no rebuild, no
bundle change.

| Variable | Purpose | Example |
|---|---|---|
| `MEKO_ENABLE_STANDARD_TIER` | Show the Standard tile on the Settings pricing table | `true` |
| `MEKO_AWS_MARKETPLACE_URL` | Deep link the Upgrade button on the Standard tile to your AWS listing | `https://aws.amazon.com/marketplace/pp/prodview-xxxxxx` |

Locally, put them in `meko_ui/.env` and restart the dev server. In
prod, set on the meko_ui deployment (kubernetes env, ECS task def,
etc.) and rollout-restart the pod.

#### Runtime (api_server)

Set on the api_server pod / local shell:

| Variable | Purpose | Example |
|---|---|---|
| `AWS_PROFILE` | (Local dev only) Named AWS profile for SDK credentials. In prod, an IAM role attached to the pod / task supplies credentials — this env var is not set. | `marketplace-seller` |
| `AWS_MARKETPLACE_PRODUCT_CODE` | Product code from your AWS listing overview | `1abc2def3ghi4jkl5mno6pqr7stu` |
| `AWS_MARKETPLACE_REGION` | Always `us-east-1` (Marketplace APIs are only served there) | `us-east-1` |
| `AWS_MARKETPLACE_WELCOME_URL` | Final redirect target after a successful entitlement resolution. Also used to derive the FE origin for the /marketplace/complete bounce. | `http://localhost:3010/settings?source=aws-marketplace` |

Optional Standard-tier limit overrides (defaults exist in
`internal/service/free_tier.go`):

- `MEKO_STANDARD_TIER_DATAPACKS`
- `MEKO_STANDARD_TIER_KB_BYTES`
- `MEKO_STANDARD_TIER_KB_FILE_BYTES`
- `MEKO_STANDARD_TIER_CONVERSATIONS`
- `MEKO_STANDARD_TIER_TOKEN_BURN`

### One-time setup

#### 1. Configure the AWS SSO profile

Marketplace APIs must be called by credentials belonging to the seller
account (not `meko-dev`). Configure SSO once:

```bash
aws configure sso
```

Answer the prompts:

- SSO session name: `meko-marketplace`
- SSO start URL: `https://<your-org>.awsapps.com/start`
- SSO region: `us-west-2` (or your org's Identity Center region)
- SSO registration scopes: `sso:account:access`
- Account: pick `marketplace-seller`
- Role: `AdministratorAccess` (or one with marketplace-entitlement perms)
- CLI default client Region: **`us-east-1`** (Marketplace only lives here)
- Profile name: `marketplace-seller`

Sign in and verify:

```bash
aws sso login --profile marketplace-seller
aws sts get-caller-identity --profile marketplace-seller
```

`Account` must match the marketplace-seller ID shown top-right in the
AWS Marketplace Management Portal.

Confirm Marketplace API is reachable:

```bash
aws marketplace-entitlement get-entitlements \
  --product-code <your-product-code> \
  --profile marketplace-seller
# Expect: { "Entitlements": [] }
```

Required IAM permissions on the SSO role:

- `aws-marketplace:ResolveCustomer`
- `aws-marketplace:GetEntitlements`

#### 2. Install ngrok

```bash
brew install ngrok
ngrok config add-authtoken <your-authtoken>   # from dashboard.ngrok.com
```

Free tier is enough; paid tier ($10/mo) gives a fixed subdomain, so
you don't have to keep updating the AWS Fulfillment URL between
sessions.

#### 3. Configure the AWS listing

Once, in AWS Marketplace Management Portal:

- Create a SaaS product
- **Pricing model**: Contract-based (not "Pay as you go")
- **Dimension API name**: `standard` (lowercase; must match `PlanIDFromDimension`)
- **Rate**: `$25/Month` (auto-clamped to `$0.001` for Limited listings)
- **Fulfillment URL**: your current ngrok tunnel URL (see per-session below)
- **Visibility**: Limited, with your own AWS account allow-listed
- **EULA**: Standard Contract for AWS Marketplace (SCMP)

### Per-session workflow

#### 1. Start ngrok

```bash
ngrok http 25433
```

Copy the `Forwarding https://<random>.ngrok-free.dev` URL. Free-tier
subdomains change every restart.

#### 2. Update the AWS Fulfillment URL

If the ngrok URL changed since last session:

AWS Marketplace Management Portal → your product → **Actions →
Update fulfillment options** → set to
`https://<current-ngrok>.ngrok-free.dev/api/v1/marketplace/register`.
Save; propagation takes a few minutes.

#### 3. Refresh SSO

```bash
aws sso login --profile marketplace-seller
```

Session lasts ~8 hours; re-login when api_server hits
`ExpiredTokenException`.

#### 4. Launch api_server

```bash
lsof -ti :25433 | xargs kill -9 2>/dev/null

export AWS_PROFILE=marketplace-seller
export AWS_MARKETPLACE_PRODUCT_CODE=<your-product-code>
export AWS_MARKETPLACE_REGION=us-east-1
export AWS_MARKETPLACE_WELCOME_URL=http://localhost:3010/settings?source=aws-marketplace

./api_server   # or your usual launch command
```

Expected boot logs:

```
INFO api server starting  port=25433
INFO marketplace: initialized  product_code=... region=us-east-1
INFO marketplace routes registered
```

#### 5. Launch the meko_ui dev server

In another terminal:

```bash
cd meko_ui
npm run dev
```

Should proxy `/api/v1/*` to `http://localhost:25433`.

#### 6. Smoke-test

```bash
# Local health
curl -i http://localhost:25433/api/v1/health

# Through ngrok
curl -i https://<current-ngrok>.ngrok-free.dev/api/v1/health

# Fulfillment endpoint (fake token → expect 303 with token in query)
curl -si -X POST https://<current-ngrok>.ngrok-free.dev/api/v1/marketplace/register \
  -d "x-amzn-marketplace-token=preflight" | grep -i location
# Expect: Location: http://localhost:3010/marketplace/complete?t=preflight
```

The ngrok inspection UI at `http://localhost:4040` shows every request
+ response — useful when debugging.

#### 7. Test the buyer flow

Flow A (existing user upgrade):

1. Sign into Meko in your browser
2. Go to `/settings` → click **Upgrade** on the Standard tile
3. Redirected to the AWS Marketplace listing
4. Click **Continue to Subscribe** → accept EULA → Subscribe
5. Click **Set up your account** on the success page
6. Ngrok interstitial (free tier) → **Visit Site**
7. Browser lands on `/marketplace/complete?t=<token>` → BE resolves →
   redirected to `/settings?source=aws-marketplace&plan=standard`

Flow B (brand-new buyer):

1. In an incognito window, open your AWS Marketplace listing
2. Buy the subscription (same steps 3-6 as Flow A)
3. Since you're not signed into Meko, the browser lands on
   `/marketplace/complete?t=<token>` → `MarketplaceSignInBouncePage`
   redirects to `/signup?source=aws-marketplace&return_to=/marketplace/complete?t=<token>`
4. Sign up (use a fresh email like `foo+aws-test@yugabyte.com`)
5. Enter email verification code → navigates to
   `/signin?return_to=/marketplace/complete?t=<token>` (return_to
   preserved through the flow)
6. Log in with new credentials → the guard honors `return_to` →
   `/marketplace/complete?t=<token>` → same resolution as Flow A →
   `/settings?...`

#### 8. Verify

```sql
SELECT plan_id, status FROM meko_system.marketplace_entitlements
 WHERE user_id = '<your-user-id>';
-- Expect: standard | ENTITLED

SELECT user_type FROM meko_system.users
 WHERE user_id = '<your-user-id>';
-- Expect: standard
```

The `TierBadge` next to your email should now read `STANDARD`. The
Pricing section on `/settings` should show the Current chip on the
Standard tile.

#### 9. Reset between tests

```sql
DELETE FROM meko_system.marketplace_entitlements WHERE user_id = '<user-id>';
UPDATE meko_system.users SET user_type = 'free' WHERE user_id = '<user-id>';
```

AWS side: buyer console → Manage subscriptions → Meko → **Actions →
Cancel subscription** (if you want a completely fresh token). Or from
the same subscription row click **Set up product** to mint a new
token without cancelling — same effect for the fulfillment loop.

### Common issues

- **Route returns 502 with `AWS_ERROR`**: token expired or product
  code doesn't match. Refresh SSO session, verify product code.
- **Route returns 503 `MARKETPLACE_DISABLED`**: `AWS_MARKETPLACE_PRODUCT_CODE`
  isn't set on api_server. Restart with env in scope.
- **Browser lands on `/datapacks` after login (not `/marketplace/complete`)**:
  the `return_to` query param got dropped. Confirm SignUpPage
  navigates to `ROUTES.signin` (not `ROUTES.login`, which triggers a
  `<Navigate>` that strips the query).
- **ngrok changed URL and AWS calls the old one**: update the AWS
  Fulfillment URL to the current tunnel or use paid ngrok for a fixed
  subdomain.

---

## Production

### What's different vs dev

- **No ngrok** — api_server serves at a real HTTPS host (e.g.
  `api.mekodata.ai`). AWS calls that URL directly.
- **AWS_MARKETPLACE_WELCOME_URL** points at the prod FE origin
  (`https://cloud.mekodata.ai/settings?source=aws-marketplace`).
- **AWS creds** are typically an **IAM role attached to the compute**
  (ECS task role / EKS service account / EC2 instance profile), not
  SSO or user access keys. Attach the same
  `aws-marketplace:ResolveCustomer` and `aws-marketplace:GetEntitlements`
  permissions to that role.
- **Listing visibility**: Public, not Limited. Pricing is the real
  `$25/Month` (not the `$0.001` Limited-listing test cap).

### Prod checklist

1. Marketplace Management Portal
   - Confirm the listing is **Public** with real $25 pricing
   - Fulfillment URL: `https://api.mekodata.ai/api/v1/marketplace/register`
   - EULA present, refund policy filled, category selected
2. AWS Marketplace listing has a stable **Product code** — copy it.
3. api_server pod env vars
   - `AWS_MARKETPLACE_PRODUCT_CODE=<prod product code>`
   - `AWS_MARKETPLACE_REGION=us-east-1`
   - `AWS_MARKETPLACE_WELCOME_URL=https://cloud.mekodata.ai/settings?source=aws-marketplace`
4. api_server pod IAM (task role / SA) has
   `aws-marketplace:ResolveCustomer` + `aws-marketplace:GetEntitlements`.
5. meko_ui pod env (runtime — no rebuild required)
   - `MEKO_ENABLE_STANDARD_TIER=true`
   - `MEKO_AWS_MARKETPLACE_URL=https://aws.amazon.com/marketplace/pp/prodview-<listing-id>`
   - Note: `/api/v1/*` routing to api_server is handled by the nginx
     sidecar (`meko-devops` deployment), NOT the meko_ui express
     server. No `MEKO_API_UPSTREAM` needed on the pod in prod.
6. Migrations applied — `meko_system.marketplace_entitlements` table
   exists and the `account_type` enum has `standard`.

### Prod smoke test

Same as dev step 6 but against the prod URL:

```bash
curl -si https://api.mekodata.ai/api/v1/health
# HTTP/1.1 200 OK

curl -si -X POST https://api.mekodata.ai/api/v1/marketplace/register \
  -d "x-amzn-marketplace-token=preflight" 2>&1 | grep -i location
# Location: https://cloud.mekodata.ai/marketplace/complete?t=preflight
```

### SNS entitlement-change subscription (optional — for renewals / cancels)

AWS auto-provisions an SNS topic for the listing. Subscribe your
`/api/v1/marketplace/sns` endpoint to it so entitlement changes
(cancel, upgrade, expire) are pushed back to Meko in near real time.

- SNS Console → your product's topic → Subscriptions → Create
- Protocol: HTTPS
- Endpoint: `https://api.mekodata.ai/api/v1/marketplace/sns`
- AWS sends a `SubscriptionConfirmation` payload the handler must
  acknowledge (the code in `marketplace_handler.go` handles this).

---

## Request flow diagram

Both flows converge on `/marketplace/complete` — the difference is
whether a signup detour happens before it.

```
                     ┌──────────────────────────────────────────┐
                     │      AWS Marketplace listing page        │
                     └──────────────────┬───────────────────────┘
                                        │ buyer clicks Subscribe
                                        ▼
                     ┌──────────────────────────────────────────┐
                     │  AWS collects payment, mints token       │
                     │  "Set up your account" button appears    │
                     └──────────────────┬───────────────────────┘
                                        │ POST x-amzn-marketplace-token
                                        ▼
                     ┌──────────────────────────────────────────┐
                     │  api_server: /api/v1/marketplace/register│
                     │  (PUBLIC — no auth)                      │
                     └──────────────────┬───────────────────────┘
                                        │ 303 See Other
                                        │ Location: <feBase>/marketplace/complete?t=<token>
                                        ▼
                     ┌──────────────────────────────────────────┐
                     │  Browser: /marketplace/complete?t=<token>│
                     └───────┬──────────────────────┬───────────┘
                             │                      │
                    signed in│                 not signed in
                             │                      │
                             ▼                      ▼
       ┌──────────────────────────────┐    ┌────────────────────────────────┐
       │ ProtectedRoutes              │    │ PublicRoutes                   │
       │ → MarketplaceCompletePage    │    │ → MarketplaceSignInBouncePage  │
       │   window.location.assign(    │    │   Navigate("/signup?           │
       │     "/api/v1/marketplace/    │    │     source=aws-marketplace&    │
       │      complete?t=<token>")    │    │     return_to=/marketplace/    │
       └──────────────┬───────────────┘    │     complete?t=<token>")       │
                      │                    └───────────────┬────────────────┘
                      │                                    │ signup + confirm +
                      │                                    │ sign in (return_to
                      │                                    │ preserved through)
                      │                                    ▼
                      │                    ┌───────────────────────────────┐
                      │                    │ /marketplace/complete?t=<token>│
                      │                    │ (now authed → same as left)   │
                      │                    └───────────────┬───────────────┘
                      │                                    │
                      ▼                                    ▼
                     ┌──────────────────────────────────────────┐
                     │  api_server GET /api/v1/marketplace/     │
                     │  complete (AUTHED)                       │
                     │  ─ ResolveCustomer(token)                │
                     │  ─ GetEntitlement(customer_id)           │
                     │  ─ INSERT marketplace_entitlements       │
                     │  ─ UPDATE users.user_type = 'standard'   │
                     └──────────────────┬───────────────────────┘
                                        │ 303 See Other
                                        │ Location: <welcomeURL>
                                        │  ?source=aws-marketplace
                                        │  &plan=standard
                                        ▼
                     ┌──────────────────────────────────────────┐
                     │  /settings — user sees Standard tier     │
                     │  STANDARD tag shows next to email        │
                     └──────────────────────────────────────────┘

                        Later — entitlement change:
                     ┌──────────────────────────────────────────┐
                     │  AWS SNS → POST /api/v1/marketplace/sns  │
                     │  handler re-runs GetEntitlement,         │
                     │  updates row + user_type as needed       │
                     └──────────────────────────────────────────┘
```

---

## Key files

- **BE**
  - `api_server/internal/handlers/marketplace_handler.go` — Register / Complete / SNS
  - `api_server/internal/service/marketplace_service.go` — AWS SDK wrappers
  - `api_server/internal/repository/marketplace_repo.go` — DB access
  - `api_server/internal/db/migrations/system/000004_billing.up.sql` — `marketplace_entitlements` table + `account_type` enum
- **FE**
  - `meko_ui/src/features/billing/pages/MarketplaceCompletePage.tsx` — authed landing
  - `meko_ui/src/features/billing/pages/MarketplaceSignInBouncePage.tsx` — unauth bounce
  - `meko_ui/src/features/auth/pages/SignUpPage.tsx` — carries `return_to` through email confirm
  - `meko_ui/src/features/auth/pages/SignInPage.tsx` — honors `return_to` on login success
  - `meko_ui/src/features/auth/returnTo.ts` — same-origin path validator
  - `meko_ui/src/app/routes/guards.ts` — auth guard that honors `return_to`
  - `meko_ui/src/features/settings/components/PricingSection.tsx` — Standard tile + Upgrade
  - `meko_ui/src/features/layout/AppWorkspaceHeader.tsx` — TierBadge next to email
  - `meko_ui/src/lib/freeTier.ts` — Standard tier limits + resolveTier
