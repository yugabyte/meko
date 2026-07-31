package task

import (
	"errors"
	"testing"

	"github.com/google/uuid"
)

// endpointDeps bundles the create_endpoint collaborators a test wants the step
// constructed with. A nil recipes is left as a nil Deps.Recipes (the no-storage-
// controller case), not a typed-nil interface.
func endpointDeps(state *fakeStateStore, recipes *fakeRecipeStore, dep *fakeDeployer) Deps {
	d := Deps{State: state, Deployer: dep, ControllerURL: "http://sc:50051"}
	if recipes != nil {
		d.Recipes = recipes
	}
	return d
}

func TestCreateEndpointStepRecordsAndDeploys(t *testing.T) {
	state, recipes, dep := newFakeStateStore(), newFakeRecipeStore(), &fakeDeployer{}
	epID, projectID, branchID := uuid.New(), uuid.New(), uuid.New()
	step := NewCreateEndpointStep(CreateEndpointArgs{EndpointID: epID, ProjectID: projectID, BranchID: branchID, Role: "read_write", PgVersion: 18}, endpointDeps(state, recipes, dep))

	res := step.Execute(t.Context())
	if res.Failed() {
		t.Fatalf("Execute: %v", res.Err)
	}
	// Record + deploy + address + clear recipe.
	if state.createdEndpoints[epID] != "read_write" {
		t.Errorf("endpoint not recorded as read_write: %v", state.createdEndpoints)
	}
	if len(dep.created) != 1 {
		t.Fatalf("deployer CreateEndpoint calls = %d, want 1", len(dep.created))
	}
	spec := dep.created[0]
	if spec.Name != EndpointDeployName(epID) {
		t.Errorf("deploy name = %q, want %q", spec.Name, EndpointDeployName(epID))
	}
	// The deploy spec speaks the storage layer's flat branch id (hex32 of branch).
	if spec.BranchID != hex32(branchID) {
		t.Errorf("deploy spec branch id = %s, want hex32(branch) %s", spec.BranchID, hex32(branchID))
	}
	wantAddr := EndpointDeployName(epID) + ".endpoints.svc:55433"
	if state.endpointAddresses[epID] != wantAddr {
		t.Errorf("recorded address = %q, want %q", state.endpointAddresses[epID], wantAddr)
	}
	if decodeResult(t, res.Output)["address"] != wantAddr {
		t.Errorf("result address = %v, want %q", decodeResult(t, res.Output)["address"], wantAddr)
	}
}

func TestCreateEndpointStepIdempotent(t *testing.T) {
	state, recipes, dep := newFakeStateStore(), newFakeRecipeStore(), &fakeDeployer{}
	step := NewCreateEndpointStep(CreateEndpointArgs{EndpointID: uuid.New(), ProjectID: uuid.New(), BranchID: uuid.New(), Role: "read_write", PgVersion: 18}, endpointDeps(state, recipes, dep))

	if res := step.Execute(t.Context()); res.Failed() {
		t.Fatalf("Execute: %v", res.Err)
	}
	if res := step.Execute(t.Context()); res.Failed() {
		t.Fatalf("replay Execute: %v", res.Err)
	}
	if len(state.createdEndpoints) != 1 {
		t.Errorf("distinct endpoints recorded = %d, want 1 (idempotent on the same id)", len(state.createdEndpoints))
	}
}

func TestCreateEndpointStepDeployFailureStops(t *testing.T) {
	state, dep := newFakeStateStore(), &fakeDeployer{createErr: errors.New("deploy failed")}
	epID := uuid.New()
	step := NewCreateEndpointStep(CreateEndpointArgs{EndpointID: epID, ProjectID: uuid.New(), BranchID: uuid.New(), Role: "read_only", PgVersion: 18}, endpointDeps(state, newFakeRecipeStore(), dep))
	if res := step.Execute(t.Context()); !res.Failed() {
		t.Fatal("Execute succeeded, want the deploy error propagated")
	}
	if _, ok := state.endpointAddresses[epID]; ok {
		t.Error("address was recorded despite the deploy failing")
	}
}

func TestCreateEndpointStepRecordConflictSkipsDeploy(t *testing.T) {
	state, dep := newFakeStateStore(), &fakeDeployer{}
	state.errEnsureEndpoint = errConflict
	step := NewCreateEndpointStep(CreateEndpointArgs{EndpointID: uuid.New(), ProjectID: uuid.New(), BranchID: uuid.New(), Role: "read_write", PgVersion: 18}, endpointDeps(state, newFakeRecipeStore(), dep))
	if res := step.Execute(t.Context()); !res.Failed() {
		t.Fatal("Execute succeeded, want the record conflict propagated")
	}
	if len(dep.created) != 0 {
		t.Errorf("deployer was called %d time(s) despite the record failing, want 0", len(dep.created))
	}
}

func TestCreateEndpointStepRequiresDeployer(t *testing.T) {
	step := NewCreateEndpointStep(CreateEndpointArgs{EndpointID: uuid.New(), ProjectID: uuid.New(), BranchID: uuid.New(), Role: "read_write"}, Deps{State: newFakeStateStore()}) // no deployer
	if res := step.Execute(t.Context()); !res.Failed() {
		t.Fatal("Execute succeeded with no deployer configured, want a failure")
	}
}
