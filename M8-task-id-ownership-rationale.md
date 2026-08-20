# M8: Why Voyager migrations persist task IDs

## Decision

M8 persists the task ID alongside asynchronous Voyager migration work:

- `migration.target_task_id` identifies the task currently provisioning a migration target.
- `migration_run.task_id` identifies the task that owns one assess or migrate attempt.

The task ID is an **ownership fence**, not merely diagnostic metadata. A migration write is accepted only when it comes from the task that created or claimed that specific unit of work.

## Context

Voyager work is asynchronous and runs through the controller's durable task framework:

1. An API request creates a task with persisted, ordered steps.
2. A worker later executes those steps and may replay them after a crash.
3. Steps create external resources: a target compute endpoint or a Voyager job.
4. The controller records the resulting state in migration tables.

The task itself is short-lived: it plans and launches work. The migration and its runs are longer-lived domain records used to report assessment, migration, and Voyager progress.

M8 is where these two systems become connected. Earlier milestones could store migration state, but did not require a durable task to claim and mutate that state while performing external side effects.

## What problem this solves

An API-level “no active run” check is not sufficient to serialize concurrent requests. Two requests can both observe no active run before either creates one. Task replay adds another source of duplication: a worker can execute the same persisted task steps again after a failure.

Without an owner field, any task that knows a run ID could update its status or deployment metadata. For example:

```text
Request A creates launcher task T1 for assess attempt 1.
Request B concurrently creates launcher task T2 for the same attempt.
T1 persists and launches run R1.
T2 later attempts to mark R1 running or failed.
```

The run ID is deterministic for `(migration ID, operation, attempt)`, so both tasks resolve to `R1`. That prevents duplicate run rows, but by itself does not establish which task is permitted to mutate `R1`.

With `migration_run.task_id`, the first task to persist the run becomes its owner:

```text
R1 = { migration: M1, operation: assess, attempt: 1, task_id: T1 }
```

Later transitions are conditional:

```sql
UPDATE migration_run
SET status = 'running', ...
WHERE id = :run_id
  AND task_id = :task_id
  AND status IN ('pending', 'running');
```

`T1` may transition `R1`; `T2` cannot. This gives the database an explicit, enforceable answer to “which task owns this run?”

## How it works

### Assess or migrate

For `POST /migrations/{id}/assess` or `/migrate`:

1. The API calculates the next attempt number.
2. `VoyagerStartMigrationRun` allocates a task ID and deterministic run ID before task persistence.
3. It persists one launcher task with two steps:
   - `ensure_migration_run`
   - `launch_voyager_job`
4. `ensure_migration_run` creates the pending run and writes the task ID onto it.
5. `launch_voyager_job` starts the external Voyager job, then marks the run running with the same task ID.
6. On launch failure or later deployer observation, terminal state is also recorded using that same task ID.

For a retry, a new task and a new run are created:

```text
Assess attempt 1: R1 owned by T1
Assess attempt 2: R2 owned by T3
```

Attempts remain immutable history. The migration API reports the latest attempt for each operation.

### Target provisioning

Target provisioning does not create a Voyager run. It provisions project, branch, and endpoint resources, then attaches the endpoint as the migration target.

For `POST /migrations/{id}/target`:

1. The handler allocates task `T4`.
2. It stores `migration.target_task_id = T4` as an atomic provisioning claim.
3. The task provisions compute and executes `attach_target_to_migration`.
4. The attach step succeeds only when the migration's claim is still held by `T4`.
5. Attaching the target clears `target_task_id`.

This prevents a competing or stale provisioning task from attaching a different target and leaving the compute created by the legitimate task orphaned.

## Why the task ID is allocated before execution

The task ID must be known when the task plan is written, not when a worker begins executing:

```text
task.id = T1
ensure_migration_run arguments include task_id = T1
launch_voyager_job arguments include task_id = T1
migration_run.task_id becomes T1
```

Because the ID is in persisted step arguments, a restarted worker reconstructs the same plan and uses the same owner identity. Generating an ID during execution would make replay inconsistent: the ensure and launch steps could disagree about ownership, and target provisioning could not establish its claim before resource creation begins.

## Scope and limitation

This is a **task-level** ownership fence. It prevents one task from mutating work claimed by a different task.

It is not a per-worker-process lock. If a task is replayed, both the original and replay execution use the same task ID. Correctness for that case also relies on:

- task-worker lease management,
- idempotent task steps,
- deterministic Voyager job naming, and
- status transitions that do not move a terminal run back to running.

## Outcome

Persisted task IDs make asynchronous migration operations safely attributable and conditionally writable. They prevent competing tasks from overwriting each other's migration state while preserving durable replay, idempotent external resource creation, and clear migration history.
