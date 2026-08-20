package task

import (
	"context"
	"encoding/json"
	"errors"
	"strings"
	"testing"

	"github.com/google/uuid"

	"github.com/yugabyte/amp/controller/internal/deploy"
	"github.com/yugabyte/amp/controller/internal/voyager/migration"
)

// fakeMigrationStore is a minimal migration.Store for the launcher-step tests: it
// records the calls the ensure/launch steps make and returns a preloaded
// migration. Only the methods those steps touch are implemented; the rest panic
// via the embedded interface if ever called.
type fakeMigrationStore struct {
	migration.Store

	migration   migration.Migration
	getErr      error
	ensureSpecs []migration.RunSpec
	marks       []markedMigrationRun
	terminal    []terminalMigrationRun
}

type markedMigrationRun struct {
	runID        uuid.UUID
	taskID       uuid.UUID
	deployerType string
	metadata     json.RawMessage
}

type terminalMigrationRun struct {
	runID  uuid.UUID
	taskID uuid.UUID
	status migration.RunStatus
	detail string
}

func (s *fakeMigrationStore) GetMigration(_ context.Context, _ uuid.UUID) (migration.Migration, error) {
	if s.getErr != nil {
		return migration.Migration{}, s.getErr
	}
	return s.migration, nil
}

func (s *fakeMigrationStore) EnsureRun(_ context.Context, spec migration.RunSpec) (migration.Run, error) {
	s.ensureSpecs = append(s.ensureSpecs, spec)
	return migration.Run{
		ID:          spec.ID,
		MigrationID: spec.MigrationID,
		Operation:   spec.Operation,
		Attempt:     spec.Attempt,
		TaskID:      spec.TaskID,
		Status:      migration.RunPending,
	}, nil
}

func (s *fakeMigrationStore) MarkRunRunning(
	_ context.Context,
	runID, taskID uuid.UUID,
	deployerType string,
	metadata json.RawMessage,
) error {
	s.marks = append(s.marks, markedMigrationRun{
		runID:        runID,
		taskID:       taskID,
		deployerType: deployerType,
		metadata:     append(json.RawMessage(nil), metadata...),
	})
	return nil
}

func (s *fakeMigrationStore) MarkRunTerminal(
	_ context.Context,
	runID, taskID uuid.UUID,
	status migration.RunStatus,
	detail string,
) error {
	s.terminal = append(s.terminal, terminalMigrationRun{
		runID: runID, taskID: taskID, status: status, detail: detail,
	})
	return nil
}

type fakeVoyagerRunner struct {
	specs []deploy.VoyagerRunSpec
	ref   deploy.RunRef
	err   error
}

func (f *fakeVoyagerRunner) StartRun(_ context.Context, spec deploy.VoyagerRunSpec) (deploy.RunRef, error) {
	f.specs = append(f.specs, spec)
	return f.ref, f.err
}

func (*fakeVoyagerRunner) RunStatus(context.Context, deploy.RunRef) (deploy.RunState, error) {
	return deploy.RunStateRunning, nil
}

func TestEnsureMigrationRunStepCarriesTaskIDAndReplays(t *testing.T) {
	store := &fakeMigrationStore{}
	args := VoyagerEnsureRunArgs{
		RunID:       uuid.New(),
		MigrationID: uuid.New(),
		Operation:   migration.OperationAssess,
		Attempt:     2,
		TaskID:      uuid.New(),
	}
	step := NewVoyagerEnsureRunStep(args, Deps{VoyagerMigrations: store})
	for i := 0; i < 2; i++ {
		if result := step.Execute(t.Context()); result.Failed() {
			t.Fatalf("Execute replay %d: %v", i, result.Err)
		}
	}
	if len(store.ensureSpecs) != 2 {
		t.Fatalf("EnsureRun calls = %d, want 2", len(store.ensureSpecs))
	}
	for _, spec := range store.ensureSpecs {
		if spec.ID != args.RunID || spec.MigrationID != args.MigrationID ||
			spec.Operation != args.Operation || spec.Attempt != args.Attempt ||
			spec.TaskID == nil || *spec.TaskID != args.TaskID {
			t.Errorf("EnsureRun spec = %+v, want args %+v including task id", spec, args)
		}
	}
}

func TestLaunchVoyagerJobStepConvertsConfigsAndReplays(t *testing.T) {
	migrationID, runID, taskID := uuid.New(), uuid.New(), uuid.New()
	store := &fakeMigrationStore{migration: migration.Migration{
		ID: migrationID,
		Source: migration.SourceConfig{
			DBType: "postgresql", Host: "source.internal", Port: 5432,
			Username: "source_user", Password: "source-secret",
			Database: "production", Schemas: []string{"public", "sales"},
		},
		Target: &migration.TargetConfig{
			Host: "target.internal", Port: 55433, Username: "cloud_admin",
			Password: "target-secret", Database: "production",
		},
	}}
	runner := &fakeVoyagerRunner{ref: deploy.RunRef{
		DeployerType: deploy.VoyagerDeployerKubernetes,
		Namespace:    "voyager",
		JobName:      "voyager-migrate",
		JobUID:       "job-uid",
		Volume:       "voyager-export",
	}}
	step := NewVoyagerLaunchJobStep(VoyagerLaunchJobArgs{
		RunID: runID, MigrationID: migrationID, Operation: migration.OperationMigrate,
		Attempt: 2, TaskID: taskID, ParallelJobs: 8,
	}, Deps{VoyagerMigrations: store, VoyagerRunner: runner})

	for i := 0; i < 2; i++ {
		result := step.Execute(t.Context())
		if result.Failed() {
			t.Fatalf("Execute replay %d: %v", i, result.Err)
		}
		if strings.Contains(string(result.Output), "source-secret") ||
			strings.Contains(string(result.Output), "target-secret") {
			t.Fatal("step result exposed Voyager credentials")
		}
	}
	if len(runner.specs) != 2 || len(store.marks) != 2 {
		t.Fatalf("replay calls: StartRun=%d MarkRunRunning=%d, want 2 each", len(runner.specs), len(store.marks))
	}
	for _, spec := range runner.specs {
		if spec.MigrationID != migrationID || spec.Operation != deploy.VoyagerOperationMigrate ||
			spec.Attempt != 2 || spec.ParallelJobs != 8 ||
			spec.Source.Password != "source-secret" || spec.Target == nil ||
			spec.Target.Password != "target-secret" {
			t.Errorf("VoyagerRunSpec = %+v, want converted source and target configs", spec)
		}
	}
	for _, mark := range store.marks {
		if mark.runID != runID || mark.taskID != taskID || mark.deployerType != deploy.VoyagerDeployerKubernetes {
			t.Errorf("mark = %+v, want run %s/task %s/kubernetes", mark, runID, taskID)
		}
		if strings.Contains(string(mark.metadata), "source-secret") ||
			strings.Contains(string(mark.metadata), "target-secret") ||
			strings.Contains(string(mark.metadata), "deployer_type") {
			t.Errorf("persisted metadata contains credentials or non-metadata fields: %s", mark.metadata)
		}
	}
}

func TestMigrationLauncherStepsFailCleanly(t *testing.T) {
	if result := NewVoyagerEnsureRunStep(
		VoyagerEnsureRunArgs{TaskID: uuid.New()},
		Deps{},
	).Execute(t.Context()); !result.Failed() {
		t.Fatal("ensure step succeeded without a migration store")
	}

	store := &fakeMigrationStore{migration: migration.Migration{
		ID: uuid.New(),
		Source: migration.SourceConfig{
			DBType: "postgresql", Host: "source", Port: 5432,
			Username: "user", Password: "secret", Database: "db", Schemas: []string{"public"},
		},
	}}
	runner := &fakeVoyagerRunner{}
	step := NewVoyagerLaunchJobStep(VoyagerLaunchJobArgs{
		RunID: uuid.New(), MigrationID: store.migration.ID, TaskID: uuid.New(),
		Operation: migration.OperationMigrate, Attempt: 1, ParallelJobs: 1,
	}, Deps{VoyagerMigrations: store, VoyagerRunner: runner})
	result := step.Execute(t.Context())
	if !result.Failed() || !strings.Contains(result.Err.Error(), "without an attached target") {
		t.Fatalf("migrate without target result = %+v, want clean target failure", result)
	}
	if len(runner.specs) != 0 || len(store.marks) != 0 {
		t.Fatal("migrate without target launched or marked a run")
	}
	if len(store.terminal) != 1 || store.terminal[0].status != migration.RunFailed {
		t.Fatalf("terminal marks = %+v, want one failed run", store.terminal)
	}

	store.getErr = errors.New("database unavailable")
	assess := NewVoyagerLaunchJobStep(VoyagerLaunchJobArgs{
		RunID: uuid.New(), MigrationID: store.migration.ID, TaskID: uuid.New(),
		Operation: migration.OperationAssess, Attempt: 1,
	}, Deps{VoyagerMigrations: store, VoyagerRunner: runner})
	if result := assess.Execute(t.Context()); !result.Failed() {
		t.Fatal("launch step ignored migration reload failure")
	}
	if len(store.terminal) != 2 || store.terminal[1].status != migration.RunFailed {
		t.Fatalf("terminal marks = %+v, want failed assess launch", store.terminal)
	}
}
