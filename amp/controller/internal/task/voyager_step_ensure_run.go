package task

import (
	"context"
	"encoding/json"
	"fmt"

	"github.com/google/uuid"

	"github.com/yugabyte/amp/controller/internal/voyager/migration"
)

// StepVoyagerEnsureRun is the task_step.kind that write-ahead records a
// migration run before its Voyager job is launched, so the run row exists (and
// is fenced to this task) even if the launch step later fails or replays.
const StepVoyagerEnsureRun = "ensure_migration_run"

// VoyagerEnsureRunArgs carries every stable identity allocated by the
// start-migration-run planner. The task id is threaded through so the run is
// fenced to its owning task and both launcher steps transition the same run.
type VoyagerEnsureRunArgs struct {
	RunID       uuid.UUID           `json:"run_id"`
	MigrationID uuid.UUID           `json:"migration_id"`
	Operation   migration.Operation `json:"operation"`
	Attempt     int                 `json:"attempt"`
	TaskID      uuid.UUID           `json:"task_id"`
}

type voyagerEnsureRunStep struct {
	VoyagerEnsureRunArgs
	migrations migration.Store
}

// NewVoyagerEnsureRunStep binds run args and the migration store into an
// executable step.
func NewVoyagerEnsureRunStep(args VoyagerEnsureRunArgs, deps Deps) Step {
	return &voyagerEnsureRunStep{VoyagerEnsureRunArgs: args, migrations: deps.VoyagerMigrations}
}

func buildVoyagerEnsureRunStep(raw json.RawMessage, deps Deps) (Step, error) {
	var args VoyagerEnsureRunArgs
	if err := json.Unmarshal(raw, &args); err != nil {
		return nil, fmt.Errorf("decode ensure_migration_run args: %w", err)
	}
	return NewVoyagerEnsureRunStep(args, deps), nil
}

func (*voyagerEnsureRunStep) Kind() string { return StepVoyagerEnsureRun }

func (s *voyagerEnsureRunStep) Execute(ctx context.Context) StepResult {
	if s.migrations == nil {
		return Fail(fmt.Errorf("ensure_migration_run step requires a migration store, but none is configured"))
	}
	if s.TaskID == uuid.Nil {
		return Fail(fmt.Errorf("ensure_migration_run step requires a task id"))
	}

	taskID := s.TaskID
	run, err := s.migrations.EnsureRun(ctx, migration.RunSpec{
		ID:          s.RunID,
		MigrationID: s.MigrationID,
		Operation:   s.Operation,
		Attempt:     s.Attempt,
		TaskID:      &taskID,
	})
	if err != nil {
		return Fail(fmt.Errorf("ensure migration run: %w", err))
	}
	return Ok(mustResult(map[string]any{
		"run_id":       run.ID.String(),
		"migration_id": run.MigrationID.String(),
		"operation":    run.Operation,
		"attempt":      run.Attempt,
	}))
}
