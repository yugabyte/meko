package task

import (
	"context"
	"encoding/json"
	"testing"

	"github.com/google/uuid"

	"github.com/yugabyte/amp/controller/internal/voyager/migration"
)

// capturingTaskStore wraps a real Store and records the TaskSpecs the planner
// hands to Create, so a test can assert the write-ahead id was threaded through.
type capturingTaskStore struct {
	Store
	specs []TaskSpec
}

func (s *capturingTaskStore) Create(ctx context.Context, spec TaskSpec) (*Task, error) {
	s.specs = append(s.specs, spec)
	return s.Store.Create(ctx, spec)
}

func TestMemStoreUsesPreassignedTaskID(t *testing.T) {
	store := NewMemStore()
	id := uuid.New()
	first, err := store.Create(t.Context(), TaskSpec{
		ID: id, Kind: "first", Steps: []StepSpec{{Kind: KindNoop}},
	})
	if err != nil {
		t.Fatalf("Create: %v", err)
	}
	if first.ID != id {
		t.Fatalf("task id = %s, want preassigned %s", first.ID, id)
	}

	// Reusing the stable id returns the original write-ahead plan, not a second
	// set of steps under the same task.
	replayed, err := store.Create(t.Context(), TaskSpec{
		ID: id, Kind: "replacement", Steps: []StepSpec{{Kind: "replacement"}},
	})
	if err != nil {
		t.Fatalf("replayed Create: %v", err)
	}
	if replayed.Kind != "first" || len(replayed.StepIDs) != 1 || replayed.StepIDs[0] != first.StepIDs[0] {
		t.Fatalf("replayed task = %+v, want original durable plan %+v", replayed, first)
	}

	generated, err := store.Create(t.Context(), TaskSpec{Kind: KindNoop})
	if err != nil {
		t.Fatalf("Create with zero id: %v", err)
	}
	if generated.ID == uuid.Nil || generated.ID == id {
		t.Fatalf("generated id = %s, want a fresh non-zero id", generated.ID)
	}
}

func TestServiceStartMigrationRunPlan(t *testing.T) {
	mem := NewMemStore()
	store := &capturingTaskStore{Store: mem}
	svc := NewService(store)
	in := VoyagerStartMigrationRunInput{
		MigrationID:  uuid.New(),
		Operation:    migration.OperationMigrate,
		Attempt:      3,
		ParallelJobs: 8,
	}

	tk, err := svc.VoyagerStartMigrationRun(t.Context(), in)
	if err != nil {
		t.Fatalf("VoyagerStartMigrationRun: %v", err)
	}
	if tk.Kind != KindVoyagerStartMigrationRun || tk.ClusterID != nil ||
		tk.TargetRef == nil || *tk.TargetRef != in.MigrationID.String() {
		t.Fatalf("task = %+v, want project-less migration-targeted launcher", tk)
	}
	if len(store.specs) != 1 || store.specs[0].ID != tk.ID {
		t.Fatalf("TaskSpec did not carry stable returned task id %s", tk.ID)
	}

	steps, err := mem.Steps(t.Context(), tk.ID)
	if err != nil {
		t.Fatalf("Steps: %v", err)
	}
	wantKinds := []string{StepVoyagerEnsureRun, StepVoyagerLaunchJob}
	if !equalKinds(stepKinds(steps), wantKinds) {
		t.Fatalf("steps = %v, want %v", stepKinds(steps), wantKinds)
	}
	var ensure VoyagerEnsureRunArgs
	decodeArgs(t, steps[0], &ensure)
	var launch VoyagerLaunchJobArgs
	decodeArgs(t, steps[1], &launch)
	if ensure.TaskID != tk.ID || ensure.RunID == uuid.Nil || ensure.MigrationID != in.MigrationID ||
		ensure.Operation != in.Operation || ensure.Attempt != in.Attempt {
		t.Errorf("ensure args = %+v, want stable task/run identities and input", ensure)
	}
	if launch.RunID != ensure.RunID || launch.TaskID != tk.ID || launch.MigrationID != in.MigrationID ||
		launch.Operation != in.Operation || launch.Attempt != in.Attempt ||
		launch.ParallelJobs != in.ParallelJobs {
		t.Errorf("launch args = %+v, want matching run/task and input", launch)
	}

	var params map[string]any
	if err := json.Unmarshal(tk.Params, &params); err != nil {
		t.Fatalf("decode params: %v", err)
	}
	if len(params) != 4 {
		t.Errorf("params keys = %v, want only migration_id/operation/attempt/parallel_jobs", params)
	}
}

func TestServiceStartMigrationRunUsesStableRunID(t *testing.T) {
	store := NewMemStore()
	svc := NewService(store)
	in := VoyagerStartMigrationRunInput{
		MigrationID: uuid.New(),
		Operation:   migration.OperationAssess,
		Attempt:     1,
	}
	first, err := svc.VoyagerStartMigrationRun(t.Context(), in)
	if err != nil {
		t.Fatalf("first VoyagerStartMigrationRun: %v", err)
	}
	second, err := svc.VoyagerStartMigrationRun(t.Context(), in)
	if err != nil {
		t.Fatalf("second VoyagerStartMigrationRun: %v", err)
	}
	firstSteps, err := store.Steps(t.Context(), first.ID)
	if err != nil {
		t.Fatalf("first Steps: %v", err)
	}
	secondSteps, err := store.Steps(t.Context(), second.ID)
	if err != nil {
		t.Fatalf("second Steps: %v", err)
	}
	var firstRun, secondRun VoyagerEnsureRunArgs
	decodeArgs(t, firstSteps[0], &firstRun)
	decodeArgs(t, secondSteps[0], &secondRun)
	if firstRun.RunID == uuid.Nil || firstRun.RunID != secondRun.RunID {
		t.Fatalf("run ids = %s and %s, want the same non-zero id", firstRun.RunID, secondRun.RunID)
	}
}
