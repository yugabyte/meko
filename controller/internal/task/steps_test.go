package task

import (
	"encoding/json"
	"errors"
	"testing"

	"github.com/google/uuid"
)

// These tests exercise each step handler (steps.go) against the in-memory ports,
// focusing on the property the framework rests on: idempotency. Every handler is
// run, then run a second time, and must converge — no duplicate effect, no error
// — so the worker can safely replay the in-flight step after a crash (§8.1). They
// also cover error propagation and the fork-vs-default-branch deploy spec.

// stepInput wraps marshaled args as the StepInput a handler receives.
func stepInput(t *testing.T, args any) StepInput {
	t.Helper()
	raw, err := json.Marshal(args)
	if err != nil {
		t.Fatalf("marshal args: %v", err)
	}
	return StepInput{Step: Step{Args: raw}}
}

func decodeResult(t *testing.T, raw json.RawMessage) map[string]any {
	t.Helper()
	var m map[string]any
	if err := json.Unmarshal(raw, &m); err != nil {
		t.Fatalf("decode result: %v", err)
	}
	return m
}

// --- create_project ---------------------------------------------------------

func TestCreateProjectHandler(t *testing.T) {
	state := newFakeStateStore()
	h := CreateProjectHandler{State: state}
	branchID := uuid.New()
	in := stepInput(t, CreateProjectArgs{
		PoolID: uuid.New(), ProjectID: uuid.New(), BranchID: branchID, Name: "main", PgVersion: 17,
	})

	res, err := h.Run(t.Context(), in)
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if decodeResult(t, res)["branch_id"] != branchID.String() {
		t.Errorf("result branch_id = %v, want %s", decodeResult(t, res)["branch_id"], branchID)
	}

	// Idempotent replay: a second run converges (no error, no duplicate create).
	if _, err := h.Run(t.Context(), in); err != nil {
		t.Fatalf("replay Run: %v", err)
	}
	if state.ensureProjectCalls != 2 {
		t.Errorf("ensure calls = %d, want 2 (handler ran twice)", state.ensureProjectCalls)
	}
	if len(state.createdBranches) != 1 {
		t.Errorf("distinct branches created = %d, want 1 (idempotent)", len(state.createdBranches))
	}
	if h.Kind() != StepCreateProject {
		t.Errorf("Kind() = %q, want %q", h.Kind(), StepCreateProject)
	}
}

func TestCreateProjectHandlerPropagatesError(t *testing.T) {
	state := newFakeStateStore()
	state.errEnsureProject = errors.New("name taken")
	h := CreateProjectHandler{State: state}
	_, err := h.Run(t.Context(), stepInput(t, CreateProjectArgs{ProjectID: uuid.New(), BranchID: uuid.New(), Name: "main", PgVersion: 17}))
	if err == nil {
		t.Fatal("Run returned nil, want the state-store error propagated")
	}
}

func TestStepHandlersRejectBadArgs(t *testing.T) {
	bad := StepInput{Step: Step{Args: json.RawMessage(`not json`)}}
	handlers := []Handler{
		CreateProjectHandler{State: newFakeStateStore()},
		CreateBranchHandler{State: newFakeStateStore()},
		RecordBranchRecipeHandler{Recipes: newFakeRecipeStore()},
		CreateEndpointHandler{State: newFakeStateStore(), Deployer: &fakeDeployer{}},
		DeleteEndpointHandler{State: newFakeStateStore(), Deployer: &fakeDeployer{}},
	}
	for _, h := range handlers {
		if _, err := h.Run(t.Context(), bad); err == nil {
			t.Errorf("%s: Run on bad args returned nil, want a decode error", h.Kind())
		}
	}
}

// --- create_branch ----------------------------------------------------------

func TestCreateBranchHandler(t *testing.T) {
	state := newFakeStateStore()
	h := CreateBranchHandler{State: state}
	branchID := uuid.New()
	in := stepInput(t, CreateBranchArgs{
		ParentBranchID: uuid.New(), ProjectID: uuid.New(),
		BranchID: branchID, Name: "feature", ForkLSN: "0/16ABCD0", PgVersion: 17,
	})

	if _, err := h.Run(t.Context(), in); err != nil {
		t.Fatalf("Run: %v", err)
	}
	if _, err := h.Run(t.Context(), in); err != nil {
		t.Fatalf("replay Run: %v", err)
	}
	if state.ensureBranchCalls != 2 || len(state.createdBranches) != 1 {
		t.Errorf("branch calls = %d, created = %d; want 2 and 1 (idempotent)", state.ensureBranchCalls, len(state.createdBranches))
	}
}

// --- record_branch_recipe ---------------------------------------------------

func TestRecordBranchRecipeHandlerDefault(t *testing.T) {
	recipes := newFakeRecipeStore()
	h := RecordBranchRecipeHandler{Recipes: recipes}
	projectID, branchID := uuid.New(), uuid.New()
	in := stepInput(t, RecordBranchRecipeArgs{
		ProjectID: projectID, BranchID: branchID, BranchName: "main", PgVersion: 17, StartLSN: "0/0",
	})
	if _, err := h.Run(t.Context(), in); err != nil {
		t.Fatalf("Run: %v", err)
	}
	if len(recipes.recorded) != 1 {
		t.Fatalf("recorded %d recipes, want 1", len(recipes.recorded))
	}
	got := recipes.recorded[0]
	if got.branchID != hex32(branchID) {
		t.Errorf("recipe branch id = %s, want hex32 %s", got.branchID, hex32(branchID))
	}
	if got.recipe.AncestorBranchID != "" {
		t.Errorf("default-branch recipe ancestor = %q, want empty", got.recipe.AncestorBranchID)
	}
	if got.recipe.StartLSN != "0/0" {
		t.Errorf("default-branch recipe start_lsn = %q, want 0/0", got.recipe.StartLSN)
	}
}

func TestRecordBranchRecipeHandlerFork(t *testing.T) {
	recipes := newFakeRecipeStore()
	h := RecordBranchRecipeHandler{Recipes: recipes}
	ancestor := uuid.New()
	in := stepInput(t, RecordBranchRecipeArgs{
		ProjectID: uuid.New(), BranchID: uuid.New(), BranchName: "feature", PgVersion: 17,
		AncestorBranchID: &ancestor, StartLSN: "0/16ABCD0",
	})
	if _, err := h.Run(t.Context(), in); err != nil {
		t.Fatalf("Run: %v", err)
	}
	got := recipes.recorded[0].recipe
	if got.AncestorBranchID != hex32(ancestor) {
		t.Errorf("fork recipe ancestor = %q, want hex32 %q", got.AncestorBranchID, hex32(ancestor))
	}
	if got.StartLSN != "0/16ABCD0" {
		t.Errorf("fork recipe start_lsn = %q, want the fork lsn", got.StartLSN)
	}
}

func TestRecordBranchRecipeHandlerNoStorageController(t *testing.T) {
	// With no storage controller (Recipes nil) the step no-ops with a warning
	// rather than failing — mirroring the synchronous handler.
	h := RecordBranchRecipeHandler{Recipes: nil}
	res, err := h.Run(t.Context(), stepInput(t, RecordBranchRecipeArgs{ProjectID: uuid.New(), BranchID: uuid.New(), StartLSN: "0/0"}))
	if err != nil {
		t.Fatalf("Run with nil recipes returned error, want no-op success: %v", err)
	}
	if decodeResult(t, res)["recorded"] != false {
		t.Errorf("result recorded = %v, want false", decodeResult(t, res)["recorded"])
	}
}

func TestRecordBranchRecipeHandlerPropagatesError(t *testing.T) {
	recipes := newFakeRecipeStore()
	recipes.errRecord = errors.New("sc down")
	h := RecordBranchRecipeHandler{Recipes: recipes}
	if _, err := h.Run(t.Context(), stepInput(t, RecordBranchRecipeArgs{ProjectID: uuid.New(), BranchID: uuid.New(), StartLSN: "0/0"})); err == nil {
		t.Fatal("Run returned nil, want the record error propagated")
	}
}

// --- create_endpoint --------------------------------------------------------

func newCreateEndpointHandler(state *fakeStateStore, recipes *fakeRecipeStore, dep *fakeDeployer, bootstrap bool) CreateEndpointHandler {
	h := CreateEndpointHandler{State: state, Deployer: dep, SCURL: "http://sc:50051", Bootstrap: bootstrap}
	if recipes != nil {
		h.Recipes = recipes
	}
	return h
}

func TestCreateEndpointHandlerDefaultBranchBootstrap(t *testing.T) {
	state, recipes, dep := newFakeStateStore(), newFakeRecipeStore(), &fakeDeployer{}
	h := newCreateEndpointHandler(state, recipes, dep, true)
	epID, projectID, branchID := uuid.New(), uuid.New(), uuid.New()
	in := stepInput(t, CreateEndpointArgs{EndpointID: epID, ProjectID: projectID, BranchID: branchID, Role: "read_write", PgVersion: 17})

	res, err := h.Run(t.Context(), in)
	if err != nil {
		t.Fatalf("Run: %v", err)
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
	// The deploy spec speaks storage ids: project->tenant, branch->timeline.
	if spec.TenantID != hex32(projectID) || spec.TimelineID != hex32(branchID) {
		t.Errorf("deploy spec storage ids = (%s,%s), want hex32(project),hex32(branch)", spec.TenantID, spec.TimelineID)
	}
	if !spec.Bootstrap {
		t.Error("default-branch endpoint spec Bootstrap = false, want true (handler bootstrap set, no ancestor)")
	}
	if spec.AncestorTimelineID != "" {
		t.Errorf("default-branch endpoint spec ancestor = %q, want empty", spec.AncestorTimelineID)
	}
	wantAddr := EndpointDeployName(epID) + ".endpoints.svc:55433"
	if state.endpointAddresses[epID] != wantAddr {
		t.Errorf("recorded address = %q, want %q", state.endpointAddresses[epID], wantAddr)
	}
	if decodeResult(t, res)["address"] != wantAddr {
		t.Errorf("result address = %v, want %q", decodeResult(t, res)["address"], wantAddr)
	}
	if len(recipes.cleared) != 1 {
		t.Errorf("recipe clears = %d, want 1 (endpoint materializes the branch)", len(recipes.cleared))
	}
}

func TestCreateEndpointHandlerForkNoBootstrap(t *testing.T) {
	state, recipes, dep := newFakeStateStore(), newFakeRecipeStore(), &fakeDeployer{}
	h := newCreateEndpointHandler(state, recipes, dep, true) // bootstrap enabled...
	ancestor := uuid.New()
	in := stepInput(t, CreateEndpointArgs{
		EndpointID: uuid.New(), ProjectID: uuid.New(), BranchID: uuid.New(), Role: "read_write", PgVersion: 17,
		AncestorBranchID: &ancestor, AncestorLSN: "0/16ABCD0",
	})
	if _, err := h.Run(t.Context(), in); err != nil {
		t.Fatalf("Run: %v", err)
	}
	spec := dep.created[0]
	if spec.Bootstrap {
		t.Error("fork endpoint spec Bootstrap = true, want false (a fork clones from its ancestor)")
	}
	if spec.AncestorTimelineID != hex32(ancestor) || spec.AncestorLSN != "0/16ABCD0" {
		t.Errorf("fork spec ancestor = (%s, %s), want (hex32 %s, 0/16ABCD0)", spec.AncestorTimelineID, spec.AncestorLSN, hex32(ancestor))
	}
}

func TestCreateEndpointHandlerIdempotent(t *testing.T) {
	state, recipes, dep := newFakeStateStore(), newFakeRecipeStore(), &fakeDeployer{}
	h := newCreateEndpointHandler(state, recipes, dep, false)
	epID := uuid.New()
	in := stepInput(t, CreateEndpointArgs{EndpointID: epID, ProjectID: uuid.New(), BranchID: uuid.New(), Role: "read_write", PgVersion: 17})

	if _, err := h.Run(t.Context(), in); err != nil {
		t.Fatalf("Run: %v", err)
	}
	if _, err := h.Run(t.Context(), in); err != nil {
		t.Fatalf("replay Run: %v", err)
	}
	if len(state.createdEndpoints) != 1 {
		t.Errorf("distinct endpoints recorded = %d, want 1 (idempotent on the same id)", len(state.createdEndpoints))
	}
}

func TestCreateEndpointHandlerDeployFailureStops(t *testing.T) {
	state, dep := newFakeStateStore(), &fakeDeployer{createErr: errors.New("deploy failed")}
	h := newCreateEndpointHandler(state, newFakeRecipeStore(), dep, false)
	epID := uuid.New()
	_, err := h.Run(t.Context(), stepInput(t, CreateEndpointArgs{EndpointID: epID, ProjectID: uuid.New(), BranchID: uuid.New(), Role: "read_only", PgVersion: 17}))
	if err == nil {
		t.Fatal("Run returned nil, want the deploy error propagated")
	}
	if _, ok := state.endpointAddresses[epID]; ok {
		t.Error("address was recorded despite the deploy failing")
	}
}

func TestCreateEndpointHandlerRecordConflictSkipsDeploy(t *testing.T) {
	state, dep := newFakeStateStore(), &fakeDeployer{}
	state.errEnsureEndpoint = errConflict
	h := newCreateEndpointHandler(state, newFakeRecipeStore(), dep, false)
	_, err := h.Run(t.Context(), stepInput(t, CreateEndpointArgs{EndpointID: uuid.New(), ProjectID: uuid.New(), BranchID: uuid.New(), Role: "read_write", PgVersion: 17}))
	if err == nil {
		t.Fatal("Run returned nil, want the record conflict propagated")
	}
	if len(dep.created) != 0 {
		t.Errorf("deployer was called %d time(s) despite the record failing, want 0", len(dep.created))
	}
}

func TestCreateEndpointHandlerRecipeClearIsBestEffort(t *testing.T) {
	// A failure clearing the (harmless) recipe must NOT fail an otherwise-
	// successful endpoint creation.
	state, dep := newFakeStateStore(), &fakeDeployer{}
	recipes := newFakeRecipeStore()
	recipes.errClear = errors.New("clear failed")
	h := newCreateEndpointHandler(state, recipes, dep, false)
	epID := uuid.New()
	if _, err := h.Run(t.Context(), stepInput(t, CreateEndpointArgs{EndpointID: epID, ProjectID: uuid.New(), BranchID: uuid.New(), Role: "read_write", PgVersion: 17})); err != nil {
		t.Fatalf("Run failed on a best-effort recipe-clear error: %v", err)
	}
	if _, ok := state.endpointAddresses[epID]; !ok {
		t.Error("endpoint address not recorded, but the only failure was the best-effort recipe clear")
	}
	// The clear must have been *attempted* (and tolerated) — not silently skipped.
	if recipes.clearAttempts != 1 {
		t.Errorf("recipe clear attempts = %d, want 1 (best-effort clear must still be tried)", recipes.clearAttempts)
	}
}

func TestCreateEndpointHandlerRequiresDeployer(t *testing.T) {
	h := CreateEndpointHandler{State: newFakeStateStore()} // no deployer
	_, err := h.Run(t.Context(), stepInput(t, CreateEndpointArgs{EndpointID: uuid.New(), ProjectID: uuid.New(), BranchID: uuid.New(), Role: "read_write"}))
	if err == nil {
		t.Fatal("Run returned nil with no deployer configured, want an error")
	}
}

// --- delete_endpoint --------------------------------------------------------

func TestDeleteEndpointHandler(t *testing.T) {
	state, dep := newFakeStateStore(), &fakeDeployer{}
	h := DeleteEndpointHandler{State: state, Deployer: dep}
	epID := uuid.New()
	in := stepInput(t, DeleteEndpointArgs{EndpointID: epID})

	if _, err := h.Run(t.Context(), in); err != nil {
		t.Fatalf("Run: %v", err)
	}
	if len(dep.deleted) != 1 || dep.deleted[0] != EndpointDeployName(epID) {
		t.Errorf("deployer deletes = %v, want [%s]", dep.deleted, EndpointDeployName(epID))
	}
	if _, ok := state.deletedEndpoints[epID]; !ok {
		t.Error("endpoint record was not deleted")
	}
	// Idempotent replay.
	if _, err := h.Run(t.Context(), in); err != nil {
		t.Fatalf("replay Run: %v", err)
	}
}

func TestDeleteEndpointHandlerRetireFailureSkipsRecordDelete(t *testing.T) {
	state, dep := newFakeStateStore(), &fakeDeployer{deleteErr: errors.New("retire failed")}
	h := DeleteEndpointHandler{State: state, Deployer: dep}
	epID := uuid.New()
	_, err := h.Run(t.Context(), stepInput(t, DeleteEndpointArgs{EndpointID: epID}))
	if err == nil {
		t.Fatal("Run returned nil, want the retire error propagated")
	}
	if state.deleteEndpointCalls != 0 {
		t.Error("endpoint record was deleted despite the instance retire failing (would orphan a running instance)")
	}
}

func TestDeleteEndpointHandlerRequiresDeployer(t *testing.T) {
	h := DeleteEndpointHandler{State: newFakeStateStore()} // no deployer
	if _, err := h.Run(t.Context(), stepInput(t, DeleteEndpointArgs{EndpointID: uuid.New()})); err == nil {
		t.Fatal("Run returned nil with no deployer configured, want an error")
	}
}
