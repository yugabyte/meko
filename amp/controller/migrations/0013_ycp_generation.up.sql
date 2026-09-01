-- 0013: record the applied YCP desired-state generation, so a failed async task
-- stops latching as applied (meko#985).
--
-- The reconcile agent's engine is level-triggered on EXISTENCE but was
-- generation-latched on CONTENT: a failed create leaves the resource absent and a
-- failed delete leaves it present, so both self-heal on the next diff, but an
-- update's only staleness signal was a generation held in the agent's own process
-- and written from the controller's 202 — which means "task planned", not "task
-- done". A task that failed after that left the resource unconverged with the
-- agent reporting healthy forever. See ac-ycp.md ("Convergence status").
--
-- Two halves, in one migration because they are inseparable: the agent now
-- retries an update until the row confirms it, and retrying is only safe with the
-- interlock.

-- IF NOT EXISTS throughout: migrations are under the same idempotency contract as
-- every other YB write (amp/REVIEW.md §1), and on YugabyteDB an ADD COLUMN that
-- adds nothing does not bump the catalog version, so the guard is free
-- (0005_endpoint_daemon_settings and 0010_cluster_engine document the same).
--
-- (1) The echo. What the agent reads back on the resource: the highest generation
-- FULLY applied to this row, written by the worker once every step of the task
-- carrying it succeeded. Nullable with no default — NULL means "no YCP update has
-- been applied", which is every pre-existing row and every resource YCP does not
-- manage. The write is monotonic (GREATEST), so a task carrying an older
-- generation that completes after a newer one cannot drag it backwards.
--
-- Deliberately NOT copied into a fork: registry.RecordBranch inherits a parent's
-- `extensions`/`pg_settings`, but a new branch is a new YCP resource at
-- generation 0, so its column starts NULL.
ALTER TABLE amp_registry.endpoints ADD COLUMN IF NOT EXISTS ycp_generation bigint;
ALTER TABLE amp_registry.branches  ADD COLUMN IF NOT EXISTS ycp_generation bigint;

-- (2) What a task declares it is applying, so the worker can stamp the row on
-- completion rather than each planner remembering to. NULL means "not
-- YCP-driven", which is every task an operator or the UI plans directly.
-- `ycp_resource` is plain text validated in Go (the worker switches on it anyway,
-- and one rejection site beats two). Unqualified like the rest of the task log:
-- this is the controller's own schema (--db-schema), not amp_registry.
ALTER TABLE tasks ADD COLUMN IF NOT EXISTS ycp_resource    text;    -- 'endpoint' | 'branch'
ALTER TABLE tasks ADD COLUMN IF NOT EXISTS ycp_resource_id uuid;
ALTER TABLE tasks ADD COLUMN IF NOT EXISTS ycp_generation  bigint;

-- add_branch_extensions only started stamping target_ref with this change, so a
-- task already pending/running carries NULL there — and a unique index stores no
-- entry for NULL, so without this backfill the first add after the deploy would be
-- admitted alongside it: exactly the concurrent redeploy of one branch the index
-- exists to prevent. cluster_id is corrected in the same pass because it used to
-- be handed the branch id, so those rows answer GET /tasks?cluster_id=<branch id>.
--
-- Terminal rows are left alone: they sit outside the index predicate, and their
-- columns are a historical record nothing reconciles against.
UPDATE tasks
   SET target_ref = COALESCE(target_ref, params->>'branch_id'),
       cluster_id = COALESCE((params->>'cluster_id')::uuid, cluster_id)
 WHERE kind = 'add_branch_extensions'
   AND status IN ('pending', 'running')
   AND params ? 'branch_id';
