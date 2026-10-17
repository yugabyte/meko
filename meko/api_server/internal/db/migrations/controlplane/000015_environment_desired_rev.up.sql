/*
 * A change token for an environment's desired state -- control-plane-mode migration set.
 *
 * The agent long-polls GET /agent/desired-state?since=<token> and the server holds the
 * request until rev moves. The response's revision cannot serve: it is the max
 * per-resource generation, which a create or a delete does not move.
 *
 * The repository bumps rev after each desired-state write commits
 * (EnvironmentRepo.bumpDesiredRev), not a trigger inside the write: inside, the row
 * stays locked until commit, serializing every writer to the environment and letting
 * two multi-environment storage-pool PATCHes deadlock on it. Its own table rather
 * than an environments column, so a bump does not contend with heartbeats for that
 * row. No row reads as 0.
 *
 * Portable SQL only -- this set has to run against a plain PostgreSQL.
 */
CREATE TABLE IF NOT EXISTS meko_system.environment_desired_revs (
    env_uuid UUID NOT NULL PRIMARY KEY REFERENCES meko_system.environments (uuid) ON DELETE CASCADE,
    rev      BIGINT NOT NULL
);
