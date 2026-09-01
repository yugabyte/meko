-- 0014: first-writer-wins for add_branch_extensions.
--
-- SEPARATE from 0013 on purpose, and the separation is load-bearing. golang-migrate
-- runs each migration file in one transaction, and on YugabyteDB a CREATE INDEX
-- inside a transaction block is downgraded to a non-concurrent build ("making
-- create index for table ... nonconcurrent", yugabyte-db#6240) whose backfill does
-- not see rows written earlier in that same transaction. Kept together with 0013's
-- target_ref repair, this index would therefore be built without the very rows the
-- repair exists to cover — silently, since the DDL succeeds. Rows committed before
-- the migration begins are backfilled normally, which is what the split buys.
--
-- First-writer-wins for add_branch_extensions, the twin of
-- tasks_one_active_resize_per_endpoint (0001). Needed because the agent now
-- re-issues an update until the row confirms it: the extensions task redeploys
-- every endpoint on the branch, so an agent restart mid-pass would otherwise plan
-- concurrent helm upgrades of the same release. Same self-releasing shape — a
-- terminal status drops the row out of the predicate.
--
-- The two predicates are kind-DISJOINT, which is what keeps
-- taskLockViolation's structural classification exact: a unique violation from
-- Create's task INSERT can still only be the index for that task's own kind.
CREATE UNIQUE INDEX IF NOT EXISTS tasks_one_active_ext_add_per_branch
    ON tasks (target_ref)
    WHERE kind = 'add_branch_extensions' AND status IN ('pending', 'running');
