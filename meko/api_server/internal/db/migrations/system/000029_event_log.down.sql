DROP TABLE IF EXISTS meko_system.event_applied;
DROP TABLE IF EXISTS meko_system.event_dead_letters;
DROP TABLE IF EXISTS meko_system.event_consumers;
-- Dropping the parent drops events_default and every monthly partition with it.
DROP TABLE IF EXISTS meko_system.events;
