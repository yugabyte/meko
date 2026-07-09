package task

import (
	"errors"
	"testing"

	"github.com/google/uuid"
)

// endpointDeps bundles the create_endpoint collaborators a test wants the step
// constructed with. A nil recipes is left as a nil Deps.Recipes (the no-storage-
// controller case), not a typed-nil interface.
func endpointDeps(state *fakeStateStore, recipes *fakeRecipeStore, dep *fakeDeployer, bootstrap bool) Deps {
	d := Deps{State: state, Deployer: dep, SCEndpointURL: "http://sc:50051", Bootstrap: bootstrap}
	if recipes != nil {
		d.Recipes = recipes
	}
	return d
}

func TestCreateEndpointStepDefaultBranchBootstrap(t *testing.T) {
	state, recipes, dep := newFakeStateStore(), newFakeRecipeStore(), &fakeDeployer{}
	epID, projectID, branchID := uuid.New(), uuid.New(), uuid.New()
	step := NewCreateEndpointStep(CreateEndpointArgs{EndpointID: epID, ProjectID: projectID, BranchID: branchID, Role: "read_write", PgVersion: 18}, endpointDeps(state, recipes, dep, true))

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
	if !spec.Bootstrap {
		t.Error("default-branch endpoint spec Bootstrap = false, want true (deps bootstrap set, no ancestor)")
	}
	if spec.AncestorBranchID != "" {
		t.Errorf("default-branch endpoint spec ancestor = %q, want empty", spec.AncestorBranchID)
	}
	wantAddr := EndpointDeployName(epID) + ".endpoints.svc:55433"
	if state.endpointAddresses[epID] != wantAddr {
		t.Errorf("recorded address = %q, want %q", state.endpointAddresses[epID], wantAddr)
	}
	if decodeResult(t, res.Output)["address"] != wantAddr {
		t.Errorf("result address = %v, want %q", decodeResult(t, res.Output)["address"], wantAddr)
	}
	if len(recipes.cleared) != 1 {
		t.Errorf("recipe clears = %d, want 1 (endpoint materializes the branch)", len(recipes.cleared))
	}
}

func TestCreateEndpointStepForkNoBootstrap(t *testing.T) {
	state, recipes, dep := newFakeStateStore(), newFakeRecipeStore(), &fakeDeployer{}
	ancestor := uuid.New()
	step := NewCreateEndpointStep(CreateEndpointArgs{
		EndpointID: uuid.New(), ProjectID: uuid.New(), BranchID: uuid.New(), Role: "read_write", PgVersion: 18,
		AncestorBranchID: &ancestor, AncestorLSN: "0/16ABCD0",
	}, endpointDeps(state, recipes, dep, true)) // bootstrap enabled...
	if res := step.Execute(t.Context()); res.Failed() {
		t.Fatalf("Execute: %v", res.Err)
	}
	spec := dep.created[0]
	if spec.Bootstrap {
		t.Error("fork endpoint spec Bootstrap = true, want false (a fork clones from its ancestor)")
	}
	if spec.AncestorBranchID != hex32(ancestor) || spec.AncestorLSN != "0/16ABCD0" {
		t.Errorf("fork spec ancestor = (%s, %s), want (hex32 %s, 0/16ABCD0)", spec.AncestorBranchID, spec.AncestorLSN, hex32(ancestor))
	}
}

func TestCreateEndpointStepIdempotent(t *testing.T) {
	state, recipes, dep := newFakeStateStore(), newFakeRecipeStore(), &fakeDeployer{}
	step := NewCreateEndpointStep(CreateEndpointArgs{EndpointID: uuid.New(), ProjectID: uuid.New(), BranchID: uuid.New(), Role: "read_write", PgVersion: 18}, endpointDeps(state, recipes, dep, false))

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
	step := NewCreateEndpointStep(CreateEndpointArgs{EndpointID: epID, ProjectID: uuid.New(), BranchID: uuid.New(), Role: "read_only", PgVersion: 18}, endpointDeps(state, newFakeRecipeStore(), dep, false))
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
	step := NewCreateEndpointStep(CreateEndpointArgs{EndpointID: uuid.New(), ProjectID: uuid.New(), BranchID: uuid.New(), Role: "read_write", PgVersion: 18}, endpointDeps(state, newFakeRecipeStore(), dep, false))
	if res := step.Execute(t.Context()); !res.Failed() {
		t.Fatal("Execute succeeded, want the record conflict propagated")
	}
	if len(dep.created) != 0 {
		t.Errorf("deployer was called %d time(s) despite the record failing, want 0", len(dep.created))
	}
}

func TestCreateEndpointStepRecipeClearIsBestEffort(t *testing.T) {
	// A failure clearing the (harmless) recipe must NOT fail an otherwise-
	// successful endpoint creation.
	state, dep := newFakeStateStore(), &fakeDeployer{}
	recipes := newFakeRecipeStore()
	recipes.errClear = errors.New("clear failed")
	epID := uuid.New()
	step := NewCreateEndpointStep(CreateEndpointArgs{EndpointID: epID, ProjectID: uuid.New(), BranchID: uuid.New(), Role: "read_write", PgVersion: 18}, endpointDeps(state, recipes, dep, false))
	if res := step.Execute(t.Context()); res.Failed() {
		t.Fatalf("Execute failed on a best-effort recipe-clear error: %v", res.Err)
	}
	if _, ok := state.endpointAddresses[epID]; !ok {
		t.Error("endpoint address not recorded, but the only failure was the best-effort recipe clear")
	}
	// The clear must have been *attempted* (and tolerated) — not silently skipped.
	if recipes.clearAttempts != 1 {
		t.Errorf("recipe clear attempts = %d, want 1 (best-effort clear must still be tried)", recipes.clearAttempts)
	}
}

func TestCreateEndpointStepRequiresDeployer(t *testing.T) {
	step := NewCreateEndpointStep(CreateEndpointArgs{EndpointID: uuid.New(), ProjectID: uuid.New(), BranchID: uuid.New(), Role: "read_write"}, Deps{State: newFakeStateStore()}) // no deployer
	if res := step.Execute(t.Context()); !res.Failed() {
		t.Fatal("Execute succeeded with no deployer configured, want a failure")
	}
}
