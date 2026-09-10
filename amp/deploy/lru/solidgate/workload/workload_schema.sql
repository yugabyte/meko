SET statement_timeout = 0;
SET lock_timeout = 0;
SET idle_in_transaction_session_timeout = 0;
SET client_encoding = UTF8;
SET standard_conforming_strings = on;
SET check_function_bodies = false;
SET xmloption = content;
SET client_min_messages = warning;
SET row_security = off;

DROP TYPE IF EXISTS public.scope_t CASCADE;
CREATE TYPE public.scope_t AS ENUM ('default');

CREATE TABLE public.merchants (
  id bigint NOT NULL,
  name text NOT NULL,
  account_id bigint NOT NULL,
  channel_id bigint NOT NULL,
  created_at timestamp without time zone NOT NULL,
  updated_at timestamp without time zone NOT NULL,
  deleted_at timestamp without time zone,
  PRIMARY KEY (id HASH)
) SPLIT INTO 3 TABLETS;

CREATE TABLE public.orders (
  id bigint NOT NULL,
  scope public.scope_t NOT NULL,
  shop_id bigint NOT NULL,
  amount bigint NOT NULL,
  status varchar(32) NOT NULL,
  order_id varchar(100) NOT NULL,
  created_at timestamp without time zone NOT NULL,
  updated_at timestamp without time zone NOT NULL,
  gateway_order_id bigint,
  gateway_order_scope public.scope_t,
  deleted_at timestamp without time zone,
  PRIMARY KEY (id HASH, scope ASC)
) SPLIT INTO 3 TABLETS;

CREATE TABLE public.order_meta (
  id bigint NOT NULL,
  scope public.scope_t NOT NULL,
  order_ref_id bigint NOT NULL,
  order_ref_scope public.scope_t NOT NULL,
  payment_type varchar(32),
  retry_attempt smallint,
  user_agent text,
  browser text,
  is_verified boolean,
  PRIMARY KEY (id HASH, scope ASC)
) SPLIT INTO 3 TABLETS;

CREATE TABLE public.devices (
  id bigint NOT NULL,
  scope public.scope_t NOT NULL,
  order_ref_id bigint NOT NULL,
  order_ref_scope public.scope_t NOT NULL,
  fingerprint text,
  user_agent text,
  device text,
  device_type text,
  language text,
  time_zone text,
  time_zone_offset integer,
  PRIMARY KEY (id HASH, scope ASC)
) SPLIT INTO 3 TABLETS;

CREATE TABLE public.order_data (
  order_ref_id bigint NOT NULL,
  order_ref_scope public.scope_t NOT NULL,
  type varchar(64),
  data jsonb,
  created_at timestamp without time zone NOT NULL,
  updated_at timestamp without time zone NOT NULL,
  PRIMARY KEY (order_ref_id HASH, order_ref_scope ASC)
) SPLIT INTO 3 TABLETS;

CREATE TABLE public.risk_events (
  order_ref_id bigint NOT NULL,
  order_ref_scope public.scope_t NOT NULL,
  phase varchar(64) NOT NULL,
  decision varchar(32),
  risk_score integer,
  finance_fee_amount bigint,
  finance_fee_currency varchar(8),
  created_at timestamp without time zone NOT NULL,
  updated_at timestamp without time zone NOT NULL,
  PRIMARY KEY (order_ref_id HASH, order_ref_scope ASC, phase ASC)
) SPLIT INTO 3 TABLETS;

CREATE TABLE public.payment_profiles (
  id bigint NOT NULL,
  scope public.scope_t NOT NULL,
  bank text,
  bin bigint,
  card_brand text,
  card_country text,
  masked_card_number text,
  recurring_token text,
  card_holder text,
  created_at timestamp without time zone NOT NULL,
  updated_at timestamp without time zone NOT NULL,
  PRIMARY KEY (id HASH, scope ASC)
) SPLIT INTO 3 TABLETS;

CREATE TABLE public.order_events (
  id bigint NOT NULL,
  scope public.scope_t NOT NULL,
  order_id bigint NOT NULL,
  order_scope public.scope_t NOT NULL,
  pay_profile_id bigint,
  pay_profile_scope public.scope_t,
  action varchar(32),
  amount bigint,
  status varchar(32),
  type varchar(32),
  code varchar(120),
  shop_notified_at timestamp without time zone,
  created_at timestamp without time zone NOT NULL,
  updated_at timestamp without time zone NOT NULL,
  PRIMARY KEY (id HASH, scope ASC)
) SPLIT INTO 3 TABLETS;

CREATE TABLE public.gateway_orders (
  id bigint NOT NULL,
  scope public.scope_t NOT NULL,
  event_ref_id bigint,
  event_ref_scope public.scope_t,
  gateway_order_id varchar(128),
  gateway_payment_id varchar(128),
  status varchar(32) NOT NULL,
  amount bigint,
  currency_id bigint,
  actual_amount bigint,
  actual_currency_id bigint,
  created_at timestamp without time zone NOT NULL,
  updated_at timestamp without time zone NOT NULL,
  deleted_at timestamp without time zone,
  synced_at timestamp without time zone,
  shop_id bigint,
  PRIMARY KEY (id HASH, scope ASC)
) SPLIT INTO 3 TABLETS;

CREATE TABLE public.gateway_events (
  id bigint NOT NULL,
  scope public.scope_t NOT NULL,
  event_ref_id bigint,
  event_ref_scope public.scope_t,
  gateway_order_id bigint,
  gateway_order_scope public.scope_t,
  gateway_event_id varchar(128),
  public_gateway_event_id varchar(128),
  action varchar(32),
  amount bigint,
  status varchar(32),
  type varchar(32),
  payment_type varchar(32),
  decline_type varchar(32),
  currency_id bigint,
  fee_amount numeric(18,2),
  fee_currency_id bigint,
  revenue_amount numeric(18,2),
  revenue_currency_id bigint,
  is_secured boolean,
  forced_3d boolean,
  event_link_id varchar(128),
  created_at timestamp without time zone NOT NULL,
  updated_at timestamp without time zone NOT NULL,
  deleted_at timestamp without time zone,
  PRIMARY KEY (id HASH, scope ASC)
) SPLIT INTO 3 TABLETS;

CREATE TABLE public.payment_methods (
  id bigint NOT NULL,
  scope public.scope_t NOT NULL,
  event_ref_id bigint,
  event_ref_scope public.scope_t,
  gateway_event_id bigint,
  gateway_event_scope public.scope_t,
  method_ref char(100),
  gateway_method text,
  expiration_date timestamp without time zone,
  is_enabled boolean,
  bank text,
  bin bigint,
  card_brand text,
  card_country text,
  masked_card_number text,
  card_holder text,
  shop_id bigint,
  recurring_count integer,
  metadata jsonb,
  created_at timestamp without time zone NOT NULL,
  updated_at timestamp without time zone NOT NULL,
  PRIMARY KEY (id HASH, scope ASC)
) SPLIT INTO 3 TABLETS;

CREATE TABLE public.auth_events (
  id bigint NOT NULL,
  order_id bigint,
  gateway_order_id bigint,
  is_active boolean,
  code varchar(120),
  verify_url text,
  return_url text,
  visited boolean,
  returned boolean,
  type_of_secure varchar(32),
  fingerprint_collection_started_at timestamp without time zone,
  created_at timestamp without time zone NOT NULL,
  updated_at timestamp without time zone NOT NULL,
  deleted_at timestamp without time zone,
  PRIMARY KEY (id HASH)
) SPLIT INTO 3 TABLETS;

CREATE TABLE public.settlement_links (
  id bigint NOT NULL,
  order_ref_id bigint NOT NULL,
  order_ref_scope public.scope_t NOT NULL,
  gateway_event_id bigint,
  gateway_event_scope public.scope_t,
  link_ref varchar(255) NOT NULL,
  created_at timestamp without time zone NOT NULL,
  updated_at timestamp without time zone NOT NULL,
  deleted_at timestamp without time zone,
  PRIMARY KEY (id HASH)
) SPLIT INTO 3 TABLETS;

CREATE INDEX NONCONCURRENTLY ON public.merchants USING btree (channel_id HASH) SPLIT INTO 3 TABLETS;

CREATE INDEX NONCONCURRENTLY ON public.orders USING btree (created_at ASC, status ASC);
CREATE INDEX NONCONCURRENTLY ON public.orders USING btree (order_id HASH, shop_id ASC, status ASC, deleted_at ASC) SPLIT INTO 3 TABLETS;
CREATE INDEX NONCONCURRENTLY ON public.orders USING btree (shop_id HASH, updated_at ASC) SPLIT INTO 3 TABLETS;
CREATE INDEX NONCONCURRENTLY ON public.orders USING btree (updated_at ASC);

CREATE INDEX NONCONCURRENTLY ON public.order_meta USING btree (order_ref_id HASH, order_ref_scope ASC) SPLIT INTO 3 TABLETS;
CREATE INDEX NONCONCURRENTLY ON public.devices USING btree (order_ref_id HASH, order_ref_scope ASC) SPLIT INTO 3 TABLETS;

CREATE INDEX NONCONCURRENTLY ON public.order_events USING btree (order_id HASH, order_scope ASC) SPLIT INTO 3 TABLETS;
CREATE INDEX NONCONCURRENTLY ON public.order_events USING btree (pay_profile_id HASH, pay_profile_scope ASC) SPLIT INTO 3 TABLETS;

CREATE INDEX NONCONCURRENTLY ON public.gateway_orders USING btree (event_ref_id HASH, event_ref_scope ASC) SPLIT INTO 3 TABLETS;
CREATE INDEX NONCONCURRENTLY ON public.gateway_orders USING btree (gateway_order_id HASH, status ASC, deleted_at ASC, synced_at ASC) SPLIT INTO 3 TABLETS;
CREATE INDEX NONCONCURRENTLY ON public.gateway_orders USING btree (updated_at ASC);

CREATE INDEX NONCONCURRENTLY ON public.gateway_events USING btree (event_ref_id HASH, event_ref_scope ASC) SPLIT INTO 3 TABLETS;
CREATE INDEX NONCONCURRENTLY ON public.gateway_events USING btree (gateway_order_id HASH, gateway_order_scope ASC) SPLIT INTO 3 TABLETS;
CREATE INDEX NONCONCURRENTLY ON public.gateway_events USING btree (updated_at ASC);

CREATE INDEX NONCONCURRENTLY ON public.payment_methods USING btree (method_ref HASH) SPLIT INTO 3 TABLETS;
CREATE INDEX NONCONCURRENTLY ON public.payment_methods USING btree (event_ref_id HASH, event_ref_scope ASC) SPLIT INTO 3 TABLETS;
CREATE INDEX NONCONCURRENTLY ON public.payment_methods USING btree (gateway_event_id HASH, gateway_event_scope ASC) SPLIT INTO 3 TABLETS;
CREATE INDEX NONCONCURRENTLY ON public.payment_methods USING btree (updated_at ASC);

CREATE INDEX NONCONCURRENTLY ON public.auth_events USING btree (order_id HASH) SPLIT INTO 3 TABLETS;

CREATE UNIQUE INDEX NONCONCURRENTLY ON public.settlement_links USING btree (COALESCE(gateway_event_id, 0) HASH, COALESCE(gateway_event_scope, 'default'::public.scope_t) ASC, link_ref ASC) SPLIT INTO 3 TABLETS;
CREATE INDEX NONCONCURRENTLY ON public.settlement_links USING btree (gateway_event_id HASH, gateway_event_scope ASC) SPLIT INTO 3 TABLETS;
CREATE INDEX NONCONCURRENTLY ON public.settlement_links USING btree (order_ref_id HASH, order_ref_scope ASC) SPLIT INTO 3 TABLETS;

CREATE INDEX NONCONCURRENTLY ON public.order_data USING btree (updated_at ASC);
CREATE INDEX NONCONCURRENTLY ON public.risk_events USING btree (order_ref_id HASH, order_ref_scope ASC) SPLIT INTO 3 TABLETS;
