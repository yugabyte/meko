/*
Durable event log for work that should not run inline in a request.

A producer inserts one row into events. A separate worker reads each shard from its saved
offset, applies the effects and advances the offset in the same transaction, so a batch's
effects are applied once. Late commits are recovered through event_applied (below).

  events              append-only, never updated. `shard` is a stable hash of the datapack
                      modulo 16: the unit of parallelism and ownership, so one datapack's
                      events stay ordered inside one shard. The key is range-sharded,
                      (shard, created_at, id), so a shard's rows are contiguous and in
                      created_at order for the worker's range read, and tablets can split by
                      key range as data grows. A hash key on `shard` would not: it holds only
                      16 values, so a partition would use at most 16 tablets, and DocDB cannot
                      split a tablet whose rows share one hash code. One shard's appends still
                      go to the tail of one tablet; that is the cost of per-shard order.
                      The CHECK keeps `shard` inside the 0 to 15 that 000030 seeds consumers
                      for, so a producer bug is an insert error, not events nobody reads.
  event_consumers     one row per (consumer, shard): the saved position and the lease. A
                      consumer is a group of readers, not a process. Seeded by 000030.
  event_dead_letters  events a handler could not process. The offset moves past them. Keeps the
                      event's own created_at and event_schema_version, so a replay is faithful.
  event_applied       the ids of events a consumer has handled (applied, skipped or dead-lettered),
                      kept for a trailing window and then trimmed. The consumer reads only rows
                      older than a short lag, because created_at is stamped at INSERT but a row is
                      visible only at COMMIT: a commit that lands after the offset has passed it
                      would be skipped for good. The consumer therefore re-reads a trailing window
                      below the offset and applies anything missing from this table. The key
                      leads with `shard`, not `consumer` (one value, so one tablet that never
                      splits), and the table is created with SPLIT AT on shard, like each partition
                      of events: every handled event inserts here and the janitor range-deletes.

events is range-partitioned by month, so expired data is removed with DROP, not DELETE, and
leaves no tombstones. PostgreSQL requires the partition key in the primary key, and created_at
is there. The current month and the six after it are created below, counted from the day this
migration RUNS rather than from fixed dates, so a later install (a BYOC data plane next year)
gets its own months. Later months are added by hand (see the runbook) until they are automated.
A row that matches no partition lands in events_default instead of failing the insert; that is
an alert condition, and PostgreSQL will not create a month whose range already has rows there.

created_at defaults to clock_timestamp(), not now(), which is the transaction start.
*/

CREATE TABLE IF NOT EXISTS meko_system.events (
    shard                SMALLINT    NOT NULL,
    created_at           TIMESTAMPTZ NOT NULL DEFAULT clock_timestamp(),
    id                   UUID        NOT NULL,
    type                 TEXT        NOT NULL,
    event_schema_version SMALLINT    NOT NULL DEFAULT 1,
    payload              JSONB       NOT NULL,
    PRIMARY KEY (shard ASC, created_at ASC, id ASC),
    CONSTRAINT events_shard_chk CHECK (shard BETWEEN 0 AND 15)
) PARTITION BY RANGE (created_at);

COMMENT ON COLUMN meko_system.events.shard IS
    'Which of 16 shards (0 to 15) the event belongs to: a stable hash of the datapack id, so one datapack''s events stay ordered inside one shard.';
COMMENT ON COLUMN meko_system.events.type IS
    'What happened, e.g. search.completed. The consumer picks a handler by this.';
COMMENT ON COLUMN meko_system.events.event_schema_version IS
    'Version of the payload''s JSON shape for this type, starting at 1. Bumped only when a change would break an older reader (a field removed, renamed or changed in meaning); adding a field does not need one. The consumer ignores unknown fields and reads a newer version as it reads this one unless a handler branches on it.';
COMMENT ON COLUMN meko_system.events.payload IS
    'The event itself, JSON, as the producer wrote it. Shape per type and event_schema_version.';

CREATE TABLE IF NOT EXISTS meko_system.events_default
    PARTITION OF meko_system.events DEFAULT;

DO $$
DECLARE
    lo timestamp;
    hi timestamp;
BEGIN
    FOR i IN 0..6 LOOP
        lo := date_trunc('month', now() AT TIME ZONE 'UTC') + make_interval(months => i);
        hi := lo + interval '1 month';
        EXECUTE format(
            'CREATE TABLE IF NOT EXISTS meko_system.%I '
            || 'PARTITION OF meko_system.events FOR VALUES FROM (%L) TO (%L) '
            || 'SPLIT AT VALUES ((4), (8), (12))',
            'events_' || to_char(lo, 'YYYY_MM'),
            (lo AT TIME ZONE 'UTC')::text,
            (hi AT TIME ZONE 'UTC')::text
        );
    END LOOP;
END $$;

CREATE TABLE IF NOT EXISTS meko_system.event_consumers (
    consumer         TEXT        NOT NULL,
    shard            SMALLINT    NOT NULL,
    offset_ts        TIMESTAMPTZ NOT NULL,
    offset_id        UUID        NOT NULL,
    lease_owner      UUID,
    lease_expires_at TIMESTAMPTZ,
    updated_at       TIMESTAMPTZ NOT NULL DEFAULT now(),
    PRIMARY KEY (consumer, shard)
);

CREATE TABLE IF NOT EXISTS meko_system.event_dead_letters (
    id                   UUID        PRIMARY KEY DEFAULT gen_random_uuid(),
    consumer             TEXT        NOT NULL,
    shard                SMALLINT    NOT NULL,
    event_id             UUID        NOT NULL,
    event_created_at     TIMESTAMPTZ NOT NULL,
    type                 TEXT        NOT NULL,
    event_schema_version SMALLINT    NOT NULL,
    payload              JSONB       NOT NULL,
    error                TEXT        NOT NULL,
    created_at           TIMESTAMPTZ NOT NULL DEFAULT now()
);

-- The consumer's janitor deletes dead letters older than DEAD_LETTER_DAYS by created_at.
CREATE INDEX IF NOT EXISTS idx_event_dead_letters_created_at
    ON meko_system.event_dead_letters (created_at ASC);

COMMENT ON COLUMN meko_system.event_dead_letters.event_created_at IS
    'The event''s own created_at, as it was in events. A replay keeps it, so the event counts toward the day it happened. created_at on this table is when it was dead-lettered.';
COMMENT ON COLUMN meko_system.event_dead_letters.event_schema_version IS
    'The event''s event_schema_version, so a replayed event keeps the payload version it was written with.';

CREATE TABLE IF NOT EXISTS meko_system.event_applied (
    consumer    TEXT        NOT NULL,
    shard       SMALLINT    NOT NULL,
    created_at  TIMESTAMPTZ NOT NULL,
    event_id    UUID        NOT NULL,
    PRIMARY KEY (shard ASC, consumer ASC, created_at ASC, event_id ASC)
) SPLIT AT VALUES ((4), (8), (12));

COMMENT ON TABLE meko_system.event_applied IS
    'Events a consumer has handled, for a trailing window (default 5 minutes). Written in the same transaction as the effects. The consumer compares it with the events below its offset to find rows that committed late, and trims it behind the window. created_at is the event''s own created_at.';
