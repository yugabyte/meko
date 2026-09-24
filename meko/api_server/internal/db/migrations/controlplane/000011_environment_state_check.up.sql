/*
 * Require environments.state to be non-empty.
 *
 * Deliberately NOT a CHECK against the eleven lifecycle states. The agent is
 * upgraded on the customer's schedule, so it can report a state added after the
 * running control plane shipped; an enum constraint would fail that UPDATE and take
 * the agent's whole fleet stale, which is the compatibility rule REVIEW.md states
 * for EnvState. Empty is the failure worth foreclosing, because it is the only one
 * that is invisible.
 *
 * NOT VALID, so existing rows are not scanned. 000007 cleared them, but minutes of
 * catalog-bumping DDL separate it from here, and the replicas still serving
 * heartbeats through that gap are the pre-fix ones that write '' in the first place.
 * A validating ADD CONSTRAINT would fail on a row written in the gap and leave the
 * version dirty at 11 -- unrecoverable by the runbook's reset, which re-runs 11 but
 * not 000007's backfill. The constraint's job is to foreclose future writes, and
 * NOT VALID enforces every insert and update from here on; a row that raced the gap
 * stays blank until its agent's next tick overwrites it.
 *
 * Guarded like every other statement in this set (000001's IF NOT EXISTS, 000005's
 * ADD COLUMN IF NOT EXISTS): migrations run on every boot, and ADD CONSTRAINT has no
 * IF NOT EXISTS of its own, so an unguarded one fails the control plane's boot
 * rather than being a no-op once a version has been force-reset after a dirty apply.
 *
 * Its own file because it is DDL: it must not share an implicit transaction with the
 * backfill's writes -- see the batching rule in db/connect.go.
 */
DO $$
BEGIN
    IF NOT EXISTS (
        SELECT 1 FROM pg_constraint WHERE conname = 'environments_state_check'
    ) THEN
        ALTER TABLE meko_system.environments
            ADD CONSTRAINT environments_state_check CHECK (btrim(state) <> '') NOT VALID;
    END IF;
END
$$;
