\set ON_ERROR_STOP on
\timing on

\set seed_shops 40
\set seed_orders 1200
\set seed_tokens 800

INSERT INTO public.merchants (id, name, account_id, channel_id, created_at, updated_at, deleted_at)
SELECT i,
       'shop-' || i,
       ((i - 1) / 50) + 1,
       i,
       now() - ((i % 86400) || ' seconds')::interval,
       now() - ((i % 3600) || ' seconds')::interval,
       NULL
FROM generate_series(1, :seed_shops) AS i;

INSERT INTO public.orders (
  id, scope, shop_id, amount, status, order_id,
  created_at, updated_at, gateway_order_id, gateway_order_scope, deleted_at
)
SELECT i,
       'default'::public.scope_t,
       ((i - 1) % :seed_shops) + 1,
       100 + (i % 500000),
       CASE (i % 4)
         WHEN 0 THEN 'created'
         WHEN 1 THEN 'processing'
         WHEN 2 THEN 'success'
         ELSE 'failed'
       END,
       'ord-' || i,
       now() - ((i % 172800) || ' seconds')::interval,
       now() - ((i % 7200) || ' seconds')::interval,
       i,
       'default'::public.scope_t,
       NULL
FROM generate_series(1, :seed_orders) AS i;

INSERT INTO public.order_meta (
  id, scope, order_ref_id, order_ref_scope, payment_type,
  retry_attempt, user_agent, browser, is_verified
)
SELECT i,
       'default'::public.scope_t,
       i,
       'default'::public.scope_t,
       CASE WHEN i % 2 = 0 THEN 'card' ELSE 'token' END,
       (i % 3)::smallint,
       'Mozilla/5.0 benchmark',
       'Chrome',
       (i % 2 = 0)
FROM generate_series(1, :seed_orders) AS i
WHERE i % 2 = 0;

INSERT INTO public.devices (
  id, scope, order_ref_id, order_ref_scope,
  fingerprint, user_agent, device, device_type,
  language, time_zone, time_zone_offset
)
SELECT i,
       'default'::public.scope_t,
       i,
       'default'::public.scope_t,
       'fp-' || i,
       'Mozilla/5.0 benchmark',
       CASE WHEN i % 2 = 0 THEN 'desktop' ELSE 'mobile' END,
       CASE WHEN i % 2 = 0 THEN 'desktop' ELSE 'phone' END,
       'en-US',
       'UTC',
       0
FROM generate_series(1, :seed_orders) AS i
WHERE i % 3 = 0;

INSERT INTO public.order_data (
  order_ref_id, order_ref_scope, type, data, created_at, updated_at
)
SELECT i,
       'default'::public.scope_t,
       'checkout',
       jsonb_build_object('invoice', 'inv-' || i, 'merchant', 'm-' || (i % 1000)),
       now() - ((i % 86400) || ' seconds')::interval,
       now() - ((i % 3600) || ' seconds')::interval
FROM generate_series(1, :seed_orders) AS i
WHERE i % 4 = 0;

INSERT INTO public.risk_events (
  order_ref_id, order_ref_scope, phase, decision, risk_score,
  finance_fee_amount, finance_fee_currency, created_at, updated_at
)
SELECT i,
       'default'::public.scope_t,
       'precheck',
       CASE WHEN i % 7 = 0 THEN 'decline' ELSE 'approve' END,
       (i % 100),
       0,
       'USD',
       now() - ((i % 86400) || ' seconds')::interval,
       now() - ((i % 3600) || ' seconds')::interval
FROM generate_series(1, :seed_orders) AS i;

INSERT INTO public.payment_profiles (
  id, scope, bank, bin, card_brand, card_country,
  masked_card_number, recurring_token, card_holder,
  created_at, updated_at
)
SELECT i,
       'default'::public.scope_t,
       'Bank-' || (i % 50),
       400000 + (i % 999),
       CASE WHEN i % 2 = 0 THEN 'VISA' ELSE 'MASTERCARD' END,
       CASE WHEN i % 2 = 0 THEN 'US' ELSE 'GB' END,
       '411111XXXXXX' || lpad((i % 10000)::text, 4, '0'),
       'rtok-' || i,
       'Holder ' || i,
       now() - ((i % 172800) || ' seconds')::interval,
       now() - ((i % 7200) || ' seconds')::interval
FROM generate_series(1, :seed_orders) AS i;

INSERT INTO public.order_events (
  id, scope, order_id, order_scope,
  pay_profile_id, pay_profile_scope,
  action, amount, status, type, code,
  shop_notified_at, created_at, updated_at
)
SELECT i,
       'default'::public.scope_t,
       i,
       'default'::public.scope_t,
       i,
       'default'::public.scope_t,
       CASE WHEN i % 10 = 0 THEN 'refund' ELSE 'charge' END,
       100 + (i % 500000),
       CASE (i % 4)
         WHEN 0 THEN 'created'
         WHEN 1 THEN 'processing'
         WHEN 2 THEN 'success'
         ELSE 'failed'
       END,
       CASE WHEN i % 10 = 0 THEN 'refund' ELSE 'charge' END,
       'ev-' || i,
       NULL,
       now() - ((i % 172800) || ' seconds')::interval,
       now() - ((i % 7200) || ' seconds')::interval
FROM generate_series(1, :seed_orders) AS i;

INSERT INTO public.gateway_orders (
  id, scope, event_ref_id, event_ref_scope,
  gateway_order_id, gateway_payment_id, status,
  amount, currency_id, actual_amount, actual_currency_id,
  created_at, updated_at, deleted_at, synced_at, shop_id
)
SELECT i,
       'default'::public.scope_t,
       i,
       'default'::public.scope_t,
       'go-' || i,
       'gp-' || i,
       CASE (i % 4)
         WHEN 0 THEN 'created'
         WHEN 1 THEN 'processing'
         WHEN 2 THEN 'success'
         ELSE 'failed'
       END,
       100 + (i % 500000),
       840,
       100 + (i % 500000),
       840,
       now() - ((i % 172800) || ' seconds')::interval,
       now() - ((i % 7200) || ' seconds')::interval,
       NULL,
       now() - ((i % 1800) || ' seconds')::interval,
       ((i - 1) % :seed_shops) + 1
FROM generate_series(1, :seed_orders) AS i;

INSERT INTO public.gateway_events (
  id, scope, event_ref_id, event_ref_scope,
  gateway_order_id, gateway_order_scope,
  gateway_event_id, public_gateway_event_id,
  action, amount, status, type,
  payment_type, decline_type,
  currency_id, fee_amount, fee_currency_id,
  revenue_amount, revenue_currency_id,
  is_secured, forced_3d, event_link_id,
  created_at, updated_at, deleted_at
)
SELECT i,
       'default'::public.scope_t,
       i,
       'default'::public.scope_t,
       i,
       'default'::public.scope_t,
       'ge-' || i,
       'pge-' || i,
       CASE WHEN i % 10 = 0 THEN 'refund' ELSE 'charge' END,
       100 + (i % 500000),
       CASE (i % 4)
         WHEN 0 THEN 'created'
         WHEN 1 THEN 'processing'
         WHEN 2 THEN 'success'
         ELSE 'failed'
       END,
       CASE WHEN i % 10 = 0 THEN 'refund' ELSE 'charge' END,
       CASE WHEN i % 2 = 0 THEN 'card' ELSE 'wallet' END,
       CASE WHEN i % 7 = 0 THEN 'hard' ELSE 'none' END,
       840,
       ((i % 500)::numeric / 100),
       840,
       ((i % 900)::numeric / 100),
       840,
       (i % 2 = 0),
       (i % 3 = 0),
       'link-' || i,
       now() - ((i % 172800) || ' seconds')::interval,
       now() - ((i % 7200) || ' seconds')::interval,
       NULL
FROM generate_series(1, :seed_orders) AS i;

INSERT INTO public.payment_methods (
  id, scope, event_ref_id, event_ref_scope,
  gateway_event_id, gateway_event_scope,
  method_ref, gateway_method, expiration_date, is_enabled,
  bank, bin, card_brand, card_country, masked_card_number,
  card_holder, shop_id, recurring_count, metadata,
  created_at, updated_at
)
SELECT i,
       'default'::public.scope_t,
       i,
       'default'::public.scope_t,
       i,
       'default'::public.scope_t,
       ('tok-' || i)::char(100),
       'gwm-' || i,
       current_date + ((i % 1825) || ' days')::interval,
       true,
       'Bank-' || (i % 50),
       400000 + (i % 999),
       CASE WHEN i % 2 = 0 THEN 'VISA' ELSE 'MASTERCARD' END,
       CASE WHEN i % 2 = 0 THEN 'USA' ELSE 'GBR' END,
       '411111XXXXXX' || lpad((i % 10000)::text, 4, '0'),
       'Holder ' || i,
       ((i - 1) % :seed_shops) + 1,
       (i % 20),
       jsonb_build_object('seed', true, 'i', i),
       now() - ((i % 172800) || ' seconds')::interval,
       now() - ((i % 7200) || ' seconds')::interval
FROM generate_series(1, :seed_tokens) AS i;

INSERT INTO public.auth_events (
  id, order_id, gateway_order_id, is_active, code,
  verify_url, return_url, visited, returned, type_of_secure,
  fingerprint_collection_started_at, created_at, updated_at, deleted_at
)
SELECT i,
       i,
       CASE WHEN i % 5 = 0 THEN NULL ELSE i END,
       true,
       'auth-' || i,
       'https://example.invalid/verify/' || i,
       'https://example.invalid/return/' || i,
       (i % 2 = 0),
       (i % 3 = 0),
       '3ds2',
       now() - ((i % 3600) || ' seconds')::interval,
       now() - ((i % 172800) || ' seconds')::interval,
       now() - ((i % 7200) || ' seconds')::interval,
       NULL
FROM generate_series(1, :seed_orders) AS i
WHERE i % 3 = 0;

INSERT INTO public.settlement_links (
  id, order_ref_id, order_ref_scope,
  gateway_event_id, gateway_event_scope,
  link_ref, created_at, updated_at, deleted_at
)
SELECT i,
       i,
       'default'::public.scope_t,
       i,
       'default'::public.scope_t,
       'arn-' || i,
       now() - ((i % 172800) || ' seconds')::interval,
       now() - ((i % 7200) || ' seconds')::interval,
       NULL
FROM generate_series(1, :seed_orders) AS i
WHERE i % 2 = 0;

ANALYZE public.merchants;
ANALYZE public.orders;
ANALYZE public.order_meta;
ANALYZE public.devices;
ANALYZE public.order_data;
ANALYZE public.risk_events;
ANALYZE public.payment_profiles;
ANALYZE public.order_events;
ANALYZE public.gateway_orders;
ANALYZE public.gateway_events;
ANALYZE public.payment_methods;
ANALYZE public.auth_events;
ANALYZE public.settlement_links;
