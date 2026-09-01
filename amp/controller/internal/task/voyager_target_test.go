package task

import (
	"context"
	"encoding/json"
	"strconv"
	"testing"

	"github.com/google/uuid"

	"github.com/yugabyte/amp/controller/internal/voyager/migration"
)

// fakeAttachStore records the AttachTarget calls the attach step makes, fenced
// on the task holding the provisioning claim.
type fakeAttachStore struct {
	migration.Store
	attachments []attachedTarget
}

type attachedTarget struct {
	migrationID uuid.UUID
	taskID      uuid.UUID
	target      migration.TargetConfig
}

func (s *fakeAttachStore) AttachTarget(_ context.Context, migrationID, taskID uuid.UUID, target migration.TargetConfig) error {
	s.attachments = append(s.attachments, attachedTarget{migrationID: migrationID, taskID: taskID, target: target})
	return nil
}

type fakeMigrationEndpointLookup struct {
	address string
	err     error
	calls   int
	ids     [4]uuid.UUID
}

func (f *fakeMigrationEndpointLookup) EndpointAddress(
	_ context.Context,
	poolID, projectID, branchID, endpointID uuid.UUID,
) (string, error) {
	f.calls++
	f.ids = [4]uuid.UUID{poolID, projectID, branchID, endpointID}
	return f.address, f.err
}

func TestAttachTargetToMigrationStepParsesAddressAndReplays(t *testing.T) {
	args := VoyagerAttachTargetArgs{
		MigrationID:        uuid.New(),
		TaskID:             uuid.New(),
		PoolID:             uuid.New(),
		ClusterID:          uuid.New(),
		BranchID:           uuid.New(),
		EndpointID:         uuid.New(),
		TargetDatabaseName: "production",
	}
	store := &fakeAttachStore{}
	endpoints := &fakeMigrationEndpointLookup{address: "[2001:db8::1]:55433"}
	step := NewVoyagerAttachTargetStep(args, Deps{
		VoyagerMigrations:         store,
		VoyagerMigrationEndpoints: endpoints,
	})

	for i := 0; i < 2; i++ {
		result := step.Execute(t.Context())
		if result.Failed() {
			t.Fatalf("Execute replay %d: %v", i, result.Err)
		}
		var output map[string]any
		if err := json.Unmarshal(result.Output, &output); err != nil {
			t.Fatalf("decode result: %v", err)
		}
		if _, exposed := output["password"]; exposed {
			t.Fatal("step result exposed target password")
		}
	}
	if endpoints.calls != 2 || endpoints.ids != [4]uuid.UUID{
		args.PoolID, args.ClusterID, args.BranchID, args.EndpointID,
	} {
		t.Errorf("endpoint lookup calls/ids = (%d, %v)", endpoints.calls, endpoints.ids)
	}
	if len(store.attachments) != 2 {
		t.Fatalf("AttachTarget calls = %d, want 2 replay-safe calls", len(store.attachments))
	}
	for _, attached := range store.attachments {
		if attached.migrationID != args.MigrationID || attached.taskID != args.TaskID {
			t.Errorf("attach identity = (%s, %s), want migration %s / task %s",
				attached.migrationID, attached.taskID, args.MigrationID, args.TaskID)
		}
		target := attached.target
		if target.Host != "2001:db8::1" || target.Port != 55433 ||
			target.Username != "cloud_admin" || target.Password != "cloud_admin" ||
			target.Database != args.TargetDatabaseName || target.EndpointID != args.EndpointID {
			t.Errorf("attached target = %+v", target)
		}
	}
}

func TestAttachTargetToMigrationStepRejectsInvalidAddress(t *testing.T) {
	for _, address := range []string{"endpoint.internal", ":5433", "endpoint.internal:http", "endpoint.internal:70000"} {
		t.Run(address, func(t *testing.T) {
			store := &fakeAttachStore{}
			step := NewVoyagerAttachTargetStep(
				VoyagerAttachTargetArgs{MigrationID: uuid.New(), TaskID: uuid.New()},
				Deps{
					VoyagerMigrations:         store,
					VoyagerMigrationEndpoints: &fakeMigrationEndpointLookup{address: address},
				},
			)
			if result := step.Execute(t.Context()); !result.Failed() {
				t.Fatalf("Execute with address %q succeeded, want failure", address)
			}
			if len(store.attachments) != 0 {
				t.Fatal("invalid address was attached")
			}
		})
	}
}

func TestAttachTargetToMigrationStepFailsWithoutDependencies(t *testing.T) {
	if result := NewVoyagerAttachTargetStep(
		VoyagerAttachTargetArgs{},
		Deps{},
	).Execute(t.Context()); !result.Failed() {
		t.Fatal("attach step succeeded without dependencies")
	}
}

func TestServiceCreateMigrationTargetPlan(t *testing.T) {
	mem := NewMemStore()
	store := &capturingTaskStore{Store: mem}
	svc := NewService(store)
	in := VoyagerCreateMigrationTargetInput{
		MigrationID:        uuid.New(),
		PoolID:             uuid.New(),
		ProjectName:        "production-migration",
		TargetDatabaseName: "production",
		VCPU:               16,
	}

	tk, err := svc.VoyagerCreateMigrationTarget(t.Context(), in)
	if err != nil {
		t.Fatalf("VoyagerCreateMigrationTarget: %v", err)
	}
	if tk.Kind != KindVoyagerCreateMigrationTarget.String() || tk.ID == uuid.Nil {
		t.Fatalf("task = (%q, %s), want %q with stable id", tk.Kind, tk.ID, KindVoyagerCreateMigrationTarget)
	}
	if len(store.specs) != 1 || store.specs[0].ID != tk.ID {
		t.Fatalf("persisted TaskSpec id = %v, want returned task id %s", store.specs, tk.ID)
	}
	if tk.ClusterID == nil || tk.TargetRef == nil || *tk.TargetRef != in.MigrationID.String() {
		t.Fatalf("task project/target = (%v, %v), want allocated project and migration target", tk.ClusterID, tk.TargetRef)
	}

	steps, err := mem.Steps(t.Context(), tk.ID)
	if err != nil {
		t.Fatalf("Steps: %v", err)
	}
	wantKinds := []string{
		StepCreateCluster,
		StepRecordBranchRecipe,
		StepCreateEndpoint,
		StepVoyagerAttachTarget,
	}
	if !equalKinds(stepKinds(steps), wantKinds) {
		t.Fatalf("steps = %v, want %v", stepKinds(steps), wantKinds)
	}

	var project CreateClusterArgs
	decodeArgs(t, steps[0], &project)
	var recipe RecordBranchRecipeArgs
	decodeArgs(t, steps[1], &recipe)
	var endpoint CreateEndpointArgs
	decodeArgs(t, steps[2], &endpoint)
	var attach VoyagerAttachTargetArgs
	decodeArgs(t, steps[3], &attach)
	if project.PoolID != in.PoolID || project.Name != in.ProjectName || project.PgVersion != voyagerTargetPgVersion {
		t.Errorf("create project args = %+v", project)
	}
	if recipe.ClusterID != project.ClusterID || recipe.BranchID != project.BranchID ||
		recipe.BranchName != defaultBranchName || recipe.PgVersion != voyagerTargetPgVersion {
		t.Errorf("recipe args = %+v, want ids from create project and default main branch", recipe)
	}
	if endpoint.ClusterID != project.ClusterID || endpoint.BranchID != project.BranchID ||
		endpoint.Role != roleReadWrite || endpoint.PgVersion != voyagerTargetPgVersion ||
		endpoint.CPU != strconv.Itoa(in.VCPU) || endpoint.EndpointID == uuid.Nil {
		t.Errorf("endpoint args = %+v, want vCPU-sized writer on allocated project/branch", endpoint)
	}
	if attach.MigrationID != in.MigrationID || attach.TaskID != tk.ID || attach.PoolID != in.PoolID ||
		attach.ClusterID != project.ClusterID || attach.BranchID != project.BranchID ||
		attach.EndpointID != endpoint.EndpointID || attach.TargetDatabaseName != in.TargetDatabaseName {
		t.Errorf("attach args = %+v, want all write-ahead ids threaded through", attach)
	}

	var params VoyagerCreateMigrationTargetInput
	if err := json.Unmarshal(tk.Params, &params); err != nil {
		t.Fatalf("decode params: %v", err)
	}
	// TaskID is json:"-" (not persisted in params); compare the rest.
	params.TaskID = in.TaskID
	if params != in {
		t.Errorf("params = %+v, want %+v (including vcpu)", params, in)
	}
}

func TestServiceCreateMigrationTargetSizingIsOptional(t *testing.T) {
	mem := NewMemStore()
	svc := NewService(mem)
	tk, err := svc.VoyagerCreateMigrationTarget(t.Context(), VoyagerCreateMigrationTargetInput{
		MigrationID:        uuid.New(),
		PoolID:             uuid.New(),
		ProjectName:        "unsized",
		TargetDatabaseName: "postgres",
	})
	if err != nil {
		t.Fatalf("CreateMigrationTarget: %v", err)
	}
	steps, err := mem.Steps(t.Context(), tk.ID)
	if err != nil {
		t.Fatalf("Steps: %v", err)
	}
	// Sizing stays optional for the caller, but the shape no longer is: leaving
	// both empty would drop the target on the deployer's fallback, which is the
	// memory-starved pair the floor exists to eliminate. An unsized target gets
	// the default cores and the memory those cores are owed, spelled out.
	var endpoint CreateEndpointArgs
	decodeArgs(t, steps[2], &endpoint)
	if endpoint.CPU != "2" || endpoint.Memory != "16Gi" {
		t.Errorf("endpoint size = (%q, %q), want (2, 16Gi) when VCPU is zero", endpoint.CPU, endpoint.Memory)
	}
}

func TestServiceCreateMigrationTargetUsesClaimedTaskID(t *testing.T) {
	store := NewMemStore()
	svc := NewService(store)
	taskID := uuid.New()
	got, err := svc.VoyagerCreateMigrationTarget(t.Context(), VoyagerCreateMigrationTargetInput{
		TaskID:             taskID,
		MigrationID:        uuid.New(),
		PoolID:             uuid.New(),
		ProjectName:        "claimed-target",
		TargetDatabaseName: "postgres",
		VCPU:               4,
	})
	if err != nil {
		t.Fatalf("VoyagerCreateMigrationTarget: %v", err)
	}
	if got.ID != taskID {
		t.Fatalf("task id = %s, want preclaimed %s", got.ID, taskID)
	}
}
