package task

import (
	"errors"
	"testing"

	"github.com/google/uuid"

	"github.com/yugabyte/amp/controller/internal/extensions"
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

func TestCreateEndpointStepNoDefaultsMountsNothing(t *testing.T) {
	// With a catalog but no opt-in and no branch set, nothing is mounted:
	// pgvector is off by default.
	state, dep := newFakeStateStore(), &fakeDeployer{}
	deps := endpointDeps(state, newFakeRecipeStore(), dep)
	deps.Extensions = extensions.NewCatalog("quay.io/yugabyte")
	epID, projectID, branchID := uuid.New(), uuid.New(), uuid.New()
	step := NewCreateEndpointStep(CreateEndpointArgs{EndpointID: epID, ProjectID: projectID, BranchID: branchID, Role: "read_write", PgVersion: 18}, deps)

	if res := step.Execute(t.Context()); res.Failed() {
		t.Fatalf("Execute: %v", res.Err)
	}
	if got := dep.created[0].Extensions; len(got) != 0 {
		t.Fatalf("spec.Extensions = %+v, want none (no defaults, no opt-in)", got)
	}
}

func TestCreateEndpointStepOptInMountsAndPersists(t *testing.T) {
	// An opt-in via args.Extensions is mounted (resolved to its pinned image)
	// AND persisted onto the branch's durable set, so a recreate/fork inherits it.
	state, dep := newFakeStateStore(), &fakeDeployer{}
	deps := endpointDeps(state, newFakeRecipeStore(), dep)
	deps.Extensions = extensions.NewCatalog("quay.io/yugabyte")
	epID, projectID, branchID := uuid.New(), uuid.New(), uuid.New()
	step := NewCreateEndpointStep(CreateEndpointArgs{
		EndpointID: epID, ProjectID: projectID, BranchID: branchID, Role: "read_write", PgVersion: 18,
		Extensions: []string{"pgvector"},
	}, deps)

	if res := step.Execute(t.Context()); res.Failed() {
		t.Fatalf("Execute: %v", res.Err)
	}
	exts := dep.created[0].Extensions
	if len(exts) != 1 || exts[0].Name != "pgvector" || exts[0].Image != "quay.io/yugabyte/amp-ext-pgvector:0.8.2" {
		t.Fatalf("spec.Extensions = %+v, want the opted-in pgvector", exts)
	}
	// The opt-in is now durable on the branch.
	persisted, _ := state.BranchExtensions(t.Context(), projectID, branchID)
	if len(persisted) != 1 || persisted[0] != "pgvector" {
		t.Fatalf("branch extensions = %v, want [pgvector] persisted", persisted)
	}
}

func TestCreateEndpointStepInheritsBranchExtensions(t *testing.T) {
	// A branch that opted into an extra extension (e.g. inherited across a fork)
	// mounts it in addition to the catalog defaults, de-duplicated and ordered
	// defaults-first.
	state, dep := newFakeStateStore(), &fakeDeployer{}
	branchID := uuid.New()
	// The branch already carries pgvector (a default) plus a hypothetical extra;
	// the extra is unknown to the catalog so it is dropped, proving unknown names
	// can't wedge the deploy while known ones are honored.
	state.seedBranchExtensions(branchID, "pgvector", "totally-unknown-ext")
	deps := endpointDeps(state, newFakeRecipeStore(), dep)
	deps.Extensions = extensions.NewCatalog("r")
	step := NewCreateEndpointStep(CreateEndpointArgs{EndpointID: uuid.New(), ProjectID: uuid.New(), BranchID: branchID, Role: "read_write", PgVersion: 18}, deps)

	if res := step.Execute(t.Context()); res.Failed() {
		t.Fatalf("Execute: %v", res.Err)
	}
	exts := dep.created[0].Extensions
	if len(exts) != 1 || exts[0].Name != "pgvector" {
		t.Fatalf("spec.Extensions = %+v, want just the known pgvector (unknown dropped, deduped)", exts)
	}
}

func TestCreateEndpointStepNoCatalogMountsNothing(t *testing.T) {
	// No catalog (Docker driver / feature off) => no ImageVolume mounts.
	state, dep := newFakeStateStore(), &fakeDeployer{}
	step := NewCreateEndpointStep(CreateEndpointArgs{EndpointID: uuid.New(), ProjectID: uuid.New(), BranchID: uuid.New(), Role: "read_write", PgVersion: 18}, endpointDeps(state, newFakeRecipeStore(), dep))
	if res := step.Execute(t.Context()); res.Failed() {
		t.Fatalf("Execute: %v", res.Err)
	}
	if len(dep.created[0].Extensions) != 0 {
		t.Fatalf("spec.Extensions = %+v, want none without a catalog", dep.created[0].Extensions)
	}
}

func TestCreateEndpointStepRequiresDeployer(t *testing.T) {
	step := NewCreateEndpointStep(CreateEndpointArgs{EndpointID: uuid.New(), ProjectID: uuid.New(), BranchID: uuid.New(), Role: "read_write"}, Deps{State: newFakeStateStore()}) // no deployer
	if res := step.Execute(t.Context()); !res.Failed() {
		t.Fatal("Execute succeeded with no deployer configured, want a failure")
	}
}
