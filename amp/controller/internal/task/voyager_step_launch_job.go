package task

import (
	"context"
	"encoding/json"
	"fmt"

	"github.com/google/uuid"

	"github.com/yugabyte/amp/controller/internal/deploy"
	"github.com/yugabyte/amp/controller/internal/voyager/migration"
)

// StepVoyagerLaunchJob is the task_step.kind that idempotently launches a
// Voyager operation and marks its durable run as running. Any failure lands on
// the run row (MarkRunTerminal failed) before the step itself fails, so the run
// never lingers pending.
const StepVoyagerLaunchJob = "launch_voyager_job"

// VoyagerLaunchJobArgs identifies the immutable migration attempt to launch. The
// task id fences the run transitions to the launcher's owning task, matching the
// run written by the ensure step.
type VoyagerLaunchJobArgs struct {
	RunID        uuid.UUID           `json:"run_id"`
	MigrationID  uuid.UUID           `json:"migration_id"`
	Operation    migration.Operation `json:"operation"`
	Attempt      int                 `json:"attempt"`
	TaskID       uuid.UUID           `json:"task_id"`
	ParallelJobs int                 `json:"parallel_jobs"`
}

type voyagerLaunchJobStep struct {
	VoyagerLaunchJobArgs
	migrations migration.Store
	runner     deploy.VoyagerJobRunner
}

// NewVoyagerLaunchJobStep binds launch args, migration persistence, and the
// Voyager runner into an executable step.
func NewVoyagerLaunchJobStep(args VoyagerLaunchJobArgs, deps Deps) Step {
	return &voyagerLaunchJobStep{
		VoyagerLaunchJobArgs: args,
		migrations:           deps.VoyagerMigrations,
		runner:               deps.VoyagerRunner,
	}
}

func buildVoyagerLaunchJobStep(raw json.RawMessage, deps Deps) (Step, error) {
	var args VoyagerLaunchJobArgs
	if err := json.Unmarshal(raw, &args); err != nil {
		return nil, fmt.Errorf("decode launch_voyager_job args: %w", err)
	}
	return NewVoyagerLaunchJobStep(args, deps), nil
}

func (*voyagerLaunchJobStep) Kind() string { return StepVoyagerLaunchJob }

func (s *voyagerLaunchJobStep) Execute(ctx context.Context) StepResult {
	if s.migrations == nil {
		return Fail(fmt.Errorf("launch_voyager_job step requires a migration store, but none is configured"))
	}
	if s.runner == nil {
		return Fail(fmt.Errorf("launch_voyager_job step requires a Voyager runner, but none is configured"))
	}
	if s.TaskID == uuid.Nil {
		return Fail(fmt.Errorf("launch_voyager_job step requires a task id"))
	}

	m, err := s.migrations.GetMigration(ctx, s.MigrationID)
	if err != nil {
		return s.failRun(ctx, fmt.Errorf("load migration for Voyager run: %w", err))
	}

	spec := deploy.VoyagerRunSpec{
		MigrationID: s.MigrationID,
		Operation:   deploy.VoyagerOperation(s.Operation),
		Attempt:     s.Attempt,
		Source: deploy.VoyagerSourceConfig{
			DBType:   m.Source.DBType,
			Host:     m.Source.Host,
			Port:     m.Source.Port,
			Username: m.Source.Username,
			Password: m.Source.Password,
			Database: m.Source.Database,
			Schemas:  append([]string(nil), m.Source.Schemas...),
		},
		ParallelJobs: s.ParallelJobs,
	}
	if m.Target != nil {
		spec.Target = &deploy.VoyagerTargetConfig{
			Host:     m.Target.Host,
			Port:     m.Target.Port,
			Username: m.Target.Username,
			Password: m.Target.Password,
			Database: m.Target.Database,
		}
	}
	if s.Operation == migration.OperationMigrate && spec.Target == nil {
		return s.failRun(ctx, fmt.Errorf("launch_voyager_job cannot start migrate without an attached target"))
	}

	ref, err := s.runner.StartRun(ctx, spec)
	if err != nil {
		return s.failRun(ctx, fmt.Errorf("start Voyager run: %w", err))
	}
	// RunRef carries only runtime identity metadata. Credentials remain solely in
	// the transient VoyagerRunSpec passed to StartRun, never persisted.
	metadata, err := json.Marshal(ref)
	if err != nil {
		return s.failRun(ctx, fmt.Errorf("encode Voyager run metadata: %w", err))
	}
	if err := s.migrations.MarkRunRunning(ctx, s.RunID, s.TaskID, ref.DeployerType, metadata); err != nil {
		return s.failRun(ctx, fmt.Errorf("mark migration run running: %w", err))
	}

	return Ok(mustResult(map[string]any{
		"run_id":            s.RunID.String(),
		"deployer_type":     ref.DeployerType,
		"deployer_metadata": json.RawMessage(metadata),
	}))
}

func (s *voyagerLaunchJobStep) failRun(ctx context.Context, cause error) StepResult {
	summary := fmt.Sprintf("Voyager %s launch failed", s.Operation)
	if err := s.migrations.MarkRunTerminal(ctx, s.RunID, s.TaskID, migration.RunFailed, summary); err != nil {
		return Fail(fmt.Errorf("%v; mark migration run failed: %w", cause, err))
	}
	return Fail(cause)
}
