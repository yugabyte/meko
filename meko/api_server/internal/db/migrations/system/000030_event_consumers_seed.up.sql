/*
Seeds the 'analytics' consumer with one row per shard (0 to 15), positioned at the time of this
migration so events written before it are not replayed. Its own file because a migration that
writes rows must be the only statement in its file (TestBackfillMigrationsAreSingleStatement).

Keep MEKO_EVENTS_EMIT off until this and the event log migration before it have both run: an event
inserted between the two sits below the offset set here and is never read.
*/
INSERT INTO meko_system.event_consumers (consumer, shard, offset_ts, offset_id)
SELECT 'analytics', s, now(), '00000000-0000-0000-0000-000000000000'::uuid
FROM generate_series(0, 15) AS s
ON CONFLICT (consumer, shard) DO NOTHING;
