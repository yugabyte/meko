/*
 * Require environments.state to be non-empty -- control-plane-mode migration set.
 *
 * The column has been NOT NULL DEFAULT 'PENDING' with no CHECK since 000001, so
 * anything the service wrote landed verbatim, the DEFAULT never firing because ''
 * is not NULL. CLOUDGA-36984 exploited exactly that: a status report whose body was
 * dropped in transit bound to a zero-valued struct and wrote state = '', on a row
 * whose heartbeat was refreshed in the same statement -- an environment that read
 * as live and recently heard from, rendering as a blank chip and vanishing from
 * every state filter in the console.
 *
 * Deliberately NOT a CHECK against the eleven lifecycle states. The agent is
 * upgraded on the customer's schedule, so it can report a state added after the
 * running control plane shipped; an enum constraint would fail that UPDATE and take
 * the agent's whole fleet stale, which is the compatibility rule REVIEW.md states
 * for EnvState. Empty is the failure worth foreclosing, because it is the only one
 * that is invisible.
 *
 * The backfill runs BEFORE the constraint or it cannot be added to a fleet that
 * already holds a row the bug wrote.
 *
 * Portable SQL only -- this set has to run against a plain PostgreSQL.
 */

-- DEGRADED, not the column's own PENDING default: these were live environments in
-- an unknown condition, and PENDING would read as "never came up". A real heartbeat
-- overwrites it on the agent's next tick, which is every 10s. States that are merely
-- unfamiliar are left alone -- see above, they are legitimate.
UPDATE meko_system.environments
   SET state = 'DEGRADED'
 WHERE state IS NULL OR btrim(state) = '';

-- Repairs rows whose components/resources hold SQL NULL or the JSON literal `null`
-- where every reader expects an array. NOT written by the report above: ApplyStatus
-- has always guarded both fields on nil. This covers rows from earlier paths, and
-- pairs with the jsonArray coercion now applied on the way in.
UPDATE meko_system.environments
   SET components = '[]'::jsonb
 WHERE components IS NULL OR components = 'null'::jsonb;

UPDATE meko_system.environments
   SET resources = '[]'::jsonb
 WHERE resources IS NULL OR resources = 'null'::jsonb;

-- Guarded like every other statement in this set (000001's IF NOT EXISTS, 000005's
-- ADD COLUMN IF NOT EXISTS): migrations run on every boot, and ADD CONSTRAINT has no
-- IF NOT EXISTS of its own, so an unguarded one fails the control plane's boot
-- rather than being a no-op once a version has been force-reset after a dirty apply.
DO $$
BEGIN
    IF NOT EXISTS (
        SELECT 1 FROM pg_constraint WHERE conname = 'environments_state_check'
    ) THEN
        ALTER TABLE meko_system.environments
            ADD CONSTRAINT environments_state_check CHECK (btrim(state) <> '');
    END IF;
END
$$;
