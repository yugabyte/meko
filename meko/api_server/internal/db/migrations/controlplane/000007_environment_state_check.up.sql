/*
 * Backfill environments.state where it is empty -- control-plane-mode migration set.
 *
 * The column has been NOT NULL DEFAULT 'PENDING' with no CHECK since 000001, so
 * anything the service wrote landed verbatim, the DEFAULT never firing because ''
 * is not NULL. CLOUDGA-36984 exploited exactly that: a status report whose body was
 * dropped in transit bound to a zero-valued struct and wrote state = '', on a row
 * whose heartbeat was refreshed in the same statement -- an environment that read
 * as live and recently heard from, rendering as a blank chip and vanishing from
 * every state filter in the console. 000011 forecloses it.
 *
 * DEGRADED, not the column's own PENDING default: these were live environments in
 * an unknown condition, and PENDING would read as "never came up". A real heartbeat
 * overwrites it on the agent's next tick, which is every 10s. States that are merely
 * unfamiliar are left alone -- the agent is upgraded on the customer's schedule, so
 * a state newer than the running control plane is legitimate.
 *
 * One statement, no DDL beside it -- the batching rule in db/connect.go; the row this
 * rewrites is by definition one whose agent is still reporting, so the conflict is
 * not hypothetical. The CHECK the filename names moved to 000011 with that split, and
 * the name stays because checkmigrations reads a rename of an applied version as a
 * second file claiming it.
 *
 * Portable SQL only -- this set has to run against a plain PostgreSQL.
 */
UPDATE meko_system.environments
   SET state = 'DEGRADED'
 WHERE state IS NULL OR btrim(state) = '';
