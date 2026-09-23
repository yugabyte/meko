-- 0004: the resource-token (lease) store in `amp_registry`.
--
-- Implements the design in compute/architecture/compute-fencing.md. A token is
-- a point in time: microseconds since the Unix epoch, at which the lease
-- expires. Holders pass it downstream as the write deadline / fence value, so
-- one value is both "you hold the lease" and "stop writing at this instant" —
-- which is what lets a service that was handed a token decide for itself,
-- against its own clock, without talking to anything.
--
-- The thin client will HybridTime-encode it (`<< 12`) at the one point where it
-- becomes `ignore_after_hybrid_time`, which the server compares against
-- `clock_->Now()`. SQL never stores the shifted value: keeping the encoding on
-- the client keeps it in one place rather than two that can drift.
--
-- Rule enforced atomically by token_acquire():
--   * same client       -> may always renew, no waiting
--   * different client   -> may take over only once now > stored token + p_skew_ms
--   * otherwise          -> denied, with the time left until takeover
--
-- The p_skew_ms grace exists because the token comes from the PG node's wall
-- clock, so two nodes can disagree. 500 ms matches YugabyteDB's own
-- max_clock_skew_usec default (src/yb/util/physical_time.cc). It gates only
-- takeover by a NEW client; the holder renews without waiting, because a
-- renewal cannot violate mutual exclusion whoever's clock is right.
--
--
-- Everything is schema-qualified: `amp_registry` is a fixed name (0002), and the
-- migrator's search_path points at the controller's own schema, so the function
-- bodies have to say `amp_registry.` too — a bare `now_us()` would not resolve
-- when a caller runs with a different search_path.

-- Current time in epoch microseconds. clock_timestamp() bottoms out in the same
-- gettimeofday that YB's own wall clock reads (GetCurrentTimeMicros,
-- src/yb/gutil/walltime.h), so the value is comparable to hybrid times
-- cluster-wide once the client shifts it. EXTRACT returns numeric in PG15, so
-- the microsecond conversion is exact rather than routed through a float.
--
-- Use clock_timestamp(), NOT now()/current_timestamp: those return TRANSACTION
-- START time, measured 304 ms stale in a trivial single-statement query, which
-- would silently consume most of the skew grace.
CREATE OR REPLACE FUNCTION amp_registry.now_us() RETURNS bigint
  LANGUAGE sql VOLATILE PARALLEL SAFE AS
$$ SELECT (EXTRACT(EPOCH FROM clock_timestamp()) * 1000000)::bigint $$;

CREATE OR REPLACE FUNCTION amp_registry.us_add_ms(us bigint, ms bigint) RETURNS bigint
  LANGUAGE sql IMMUTABLE PARALLEL SAFE AS
$$ SELECT us + ms * 1000 $$;

CREATE OR REPLACE FUNCTION amp_registry.us_to_ms(us_delta bigint) RETURNS bigint
  LANGUAGE sql IMMUTABLE PARALLEL SAFE AS
$$ SELECT us_delta / 1000 $$;

-- One row per resource, holding the CURRENT lease only. There is no history and
-- no row per client: a takeover overwrites in place, which is what keeps the
-- decision a single-statement one.
CREATE TABLE IF NOT EXISTS amp_registry.resource_token (
    resource_id text   NOT NULL,
    client_id   text   NOT NULL,
    token       bigint NOT NULL,  -- epoch micros at which this lease expires
    granted_us  bigint NOT NULL,  -- epoch micros it was granted at
    PRIMARY KEY (resource_id HASH)
) SPLIT INTO 3 TABLETS;

-- Always returns exactly one row.
--   granted = true  -> token is the new lease expiry; holder = p_client_id; retry_after_ms = 0
--   granted = false -> token is NULL; holder is the current owner; retry_after_ms is the
--                      time until this client may take over (token + p_skew_ms - now)
--
-- LOAD-BEARING: the mutual-exclusion guarantee comes entirely from the one
-- atomic INSERT ... ON CONFLICT DO UPDATE ... WHERE below. RETURNING yields a
-- row only when that WHERE fired, so "granted" is a fact about the write rather
-- than a conclusion drawn from a prior read. Do NOT refactor it into separate
-- SELECT and UPDATE statements — that introduces the read-then-write window the
-- whole design exists to avoid.
CREATE OR REPLACE FUNCTION amp_registry.token_acquire(p_resource_id text,
                                                      p_client_id   text,
                                                      p_ttl_ms      bigint DEFAULT 10000,
                                                      p_skew_ms     bigint DEFAULT 500)
RETURNS TABLE (granted boolean, token bigint, holder text, retry_after_ms bigint)
LANGUAGE plpgsql AS $$
DECLARE
  v_now    bigint;
  v_token  bigint;
  v_holder text;
  v_left   bigint;
BEGIN
  IF p_ttl_ms IS NULL OR p_ttl_ms <= 0 OR p_ttl_ms > 300000 THEN
    RAISE EXCEPTION 'p_ttl_ms must be in (0, 300000], got %', p_ttl_ms;
  END IF;
  IF p_skew_ms IS NULL OR p_skew_ms < 0 OR p_skew_ms > 3000 THEN
    RAISE EXCEPTION 'p_skew_ms must be in [0, 3000], got %', p_skew_ms;
  END IF;

  v_now := amp_registry.now_us();

  INSERT INTO amp_registry.resource_token AS rt (resource_id, client_id, token, granted_us)
  VALUES (p_resource_id, p_client_id, amp_registry.us_add_ms(v_now, p_ttl_ms), v_now)
  ON CONFLICT (resource_id) DO UPDATE
     SET client_id  = EXCLUDED.client_id,
         token      = EXCLUDED.token,
         granted_us = EXCLUDED.granted_us
   WHERE amp_registry.us_add_ms(rt.token, p_skew_ms) <= v_now  -- expired + skew grace: free for anyone
      OR rt.client_id = EXCLUDED.client_id                     -- holder renews, no grace needed
  RETURNING rt.token INTO v_token;

  IF v_token IS NOT NULL THEN
    RETURN QUERY SELECT true, v_token, p_client_id, 0::bigint;
    RETURN;
  END IF;

  -- Denied: report who holds it and how long until takeover is permitted.
  -- Re-read rather than trusting the rejected upsert, and tolerate a concurrent
  -- release (coalesce to 0 below, i.e. "try again now").
  --
  -- Rounded UP: `/ 1000` truncates, so a plain divide would tell the caller to
  -- wait fractionally less than the grace actually has left and it would come
  -- straight back for another denial. Floored at 1ms rather than 0 because this
  -- re-read races the lease it is reporting on — an expiry between the rejected
  -- upsert and this SELECT makes the remainder <= 0, and a raw-SQL caller with
  -- no wait of its own would spin on a 0.
  SELECT rt.client_id,
         greatest((amp_registry.us_add_ms(rt.token, p_skew_ms) - v_now + 999) / 1000, 1)
    INTO v_holder, v_left
    FROM amp_registry.resource_token rt
   WHERE rt.resource_id = p_resource_id;

  RETURN QUERY SELECT false, NULL::bigint, v_holder, coalesce(v_left, 0);
END $$;

-- Owner-only release. True if a row was removed. Bypasses the skew grace on
-- purpose: an explicit handoff needs no clock agreement.
CREATE OR REPLACE FUNCTION amp_registry.token_release(p_resource_id text, p_client_id text)
RETURNS boolean
LANGUAGE plpgsql AS $$
DECLARE v_n int;
BEGIN
  DELETE FROM amp_registry.resource_token
   WHERE resource_id = p_resource_id AND client_id = p_client_id;
  GET DIAGNOSTICS v_n = ROW_COUNT;
  RETURN v_n > 0;
END $$;

-- Reports the row while takeover is still blocked, i.e. through the skew grace.
-- expires_in_ms is the real lease budget (0 once lapsed); takeover_in_ms is how
-- much longer a different client is kept out. Returns no rows once the grace has
-- also passed, so "no row" means "free".
CREATE OR REPLACE FUNCTION amp_registry.token_holder(p_resource_id text, p_skew_ms bigint DEFAULT 500)
RETURNS TABLE (client_id text, token bigint, expires_in_ms bigint, takeover_in_ms bigint)
LANGUAGE sql AS $$
  -- now_us() is VOLATILE, so it has to be read ONCE and joined in: calling it
  -- per-expression would let the three answers disagree with each other.
  SELECT rt.client_id,
         rt.token,
         greatest(amp_registry.us_to_ms(rt.token - n.now), 0),
         greatest(amp_registry.us_to_ms(amp_registry.us_add_ms(rt.token, p_skew_ms) - n.now), 0)
    FROM amp_registry.resource_token rt,
         LATERAL (SELECT amp_registry.now_us() AS now) n
   WHERE rt.resource_id = p_resource_id
     AND amp_registry.us_add_ms(rt.token, p_skew_ms) > n.now
$$;
