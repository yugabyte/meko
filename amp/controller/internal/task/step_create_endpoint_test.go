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
func endpointDeps(state StateStore, recipes *fakeRecipeStore, dep *fakeDeployer) Deps {
	d := Deps{State: state, Deployer: dep, ControllerURL: "http://sc:50051"}
	if recipes != nil {
		d.Recipes = recipes
	}
	return d
}

func TestCreateEndpointStepRecordsAndDeploys(t *testing.T) {
	state, recipes, dep := newFakeStateStore(), newFakeRecipeStore(), &fakeDeployer{}
	epID, clusterID, branchID := uuid.New(), uuid.New(), uuid.New()
	step := NewCreateEndpointStep(CreateEndpointArgs{EndpointID: epID, ClusterID: clusterID, BranchID: branchID, Role: "read_write", PgVersion: 18}, endpointDeps(state, recipes, dep))

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
	// The deploy spec carries the branch id verbatim.
	if spec.BranchID != branchID {
		t.Errorf("deploy spec branch id = %s, want %s", spec.BranchID, branchID)
	}
	wantAddr := EndpointDeployName(epID) + ".endpoints.svc:55433"
	if state.endpointAddresses[epID] != wantAddr {
		t.Errorf("recorded address = %q, want %q", state.endpointAddresses[epID], wantAddr)
	}
	if decodeResult(t, res.Output)["address"] != wantAddr {
		t.Errorf("result address = %v, want %q", decodeResult(t, res.Output)["address"], wantAddr)
	}
}

// The args are the only place this endpoint's desired state exists, and the spec
// handed to the deployer is where a value the step failed to carry across goes
// missing — silently, since every one of them has a default downstream. So they
// are asserted on the deployed spec, not on the struct the step passes the
// assembler: a dropped PgVersion deploys the deployer's default major version, a
// dropped size deploys the chart's 2 cores/4Gi, and a dropped branch id resolves
// the opted-in extensions of no branch at all, mounting nothing.
func TestCreateEndpointStepDeploysTheArgsDesiredState(t *testing.T) {
	state, dep := newFakeStateStore(), &fakeDeployer{}
	deps := endpointDeps(state, newFakeRecipeStore(), dep)
	deps.Extensions = extensions.NewCatalog("quay.io/yugabyte")
	epID, clusterID, branchID := uuid.New(), uuid.New(), uuid.New()
	state.seedBranchExtensions(clusterID, branchID, "pgvector")

	step := NewCreateEndpointStep(CreateEndpointArgs{
		EndpointID: epID, ClusterID: clusterID, BranchID: branchID, Role: "read_write",
		PgVersion: 15, CPU: "8", Memory: "16Gi",
	}, deps)
	if res := step.Execute(t.Context()); res.Failed() {
		t.Fatalf("Execute: %v", res.Err)
	}

	if len(dep.created) != 1 {
		t.Fatalf("deployer CreateEndpoint calls = %d, want 1", len(dep.created))
	}
	spec := dep.created[0]
	if spec.PgVersion != 15 {
		t.Errorf("deployed PgVersion = %d, want 15 (the args' pinned version)", spec.PgVersion)
	}
	if spec.CPU != "8" || spec.Memory != "16Gi" {
		t.Errorf("deployed CPU/Memory = %q/%q, want 8/16Gi (the args' size)", spec.CPU, spec.Memory)
	}
	if len(spec.Extensions) != 1 || spec.Extensions[0].Name != "pgvector" {
		t.Errorf("deployed Extensions = %+v, want the branch's pgvector", spec.Extensions)
	}
}

// Pins the argument order of EnsureEndpointRecord, whose four trailing []byte the
// compiler cannot tell apart, so the two documents are made distinguishable.
func TestCreateEndpointStepWritesDaemonSettings(t *testing.T) {
	state, recipes, dep := newFakeStateStore(), newFakeRecipeStore(), &fakeDeployer{}
	epID := uuid.New()
	step := NewCreateEndpointStep(CreateEndpointArgs{
		EndpointID: epID, ClusterID: uuid.New(), BranchID: uuid.New(),
		Role: "read_write", PgVersion: 18,
		PageserverSettings: map[string]any{"tenant_config.enable_gc": true},
		SafekeeperSettings: map[string]any{"wal_cache_size_mib": 512},
	}, endpointDeps(state, recipes, dep))

	if res := step.Execute(t.Context()); res.Failed() {
		t.Fatalf("Execute: %v", res.Err)
	}
	if got, want := state.endpointPageserverSettings[epID], `{"tenant_config.enable_gc":true}`; got != want {
		t.Errorf("pageserver_settings written = %s, want %s", got, want)
	}
	if got, want := state.endpointSafekeeperSettings[epID], `{"wal_cache_size_mib":512}`; got != want {
		t.Errorf("safekeeper_settings written = %s, want %s", got, want)
	}
}

// A nil map must still write `{}`: the columns are NOT NULL, and a JSON null
// would fail the insert rather than mean "no overrides".
func TestCreateEndpointStepWritesEmptyDaemonSettings(t *testing.T) {
	state, recipes, dep := newFakeStateStore(), newFakeRecipeStore(), &fakeDeployer{}
	epID := uuid.New()
	step := NewCreateEndpointStep(CreateEndpointArgs{
		EndpointID: epID, ClusterID: uuid.New(), BranchID: uuid.New(),
		Role: "read_write", PgVersion: 18,
	}, endpointDeps(state, recipes, dep))

	if res := step.Execute(t.Context()); res.Failed() {
		t.Fatalf("Execute: %v", res.Err)
	}
	if got := state.endpointPageserverSettings[epID]; got != "{}" {
		t.Errorf("pageserver_settings written = %q, want {}", got)
	}
	if got := state.endpointSafekeeperSettings[epID]; got != "{}" {
		t.Errorf("safekeeper_settings written = %q, want {}", got)
	}
}

func TestCreateEndpointStepIdempotent(t *testing.T) {
	state, recipes, dep := newFakeStateStore(), newFakeRecipeStore(), &fakeDeployer{}
	step := NewCreateEndpointStep(CreateEndpointArgs{EndpointID: uuid.New(), ClusterID: uuid.New(), BranchID: uuid.New(), Role: "read_write", PgVersion: 18}, endpointDeps(state, recipes, dep))

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
	step := NewCreateEndpointStep(CreateEndpointArgs{EndpointID: epID, ClusterID: uuid.New(), BranchID: uuid.New(), Role: "read_only", PgVersion: 18}, endpointDeps(state, newFakeRecipeStore(), dep))
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
	step := NewCreateEndpointStep(CreateEndpointArgs{EndpointID: uuid.New(), ClusterID: uuid.New(), BranchID: uuid.New(), Role: "read_write", PgVersion: 18}, endpointDeps(state, newFakeRecipeStore(), dep))
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
	epID, clusterID, branchID := uuid.New(), uuid.New(), uuid.New()
	step := NewCreateEndpointStep(CreateEndpointArgs{EndpointID: epID, ClusterID: clusterID, BranchID: branchID, Role: "read_write", PgVersion: 18}, deps)

	if res := step.Execute(t.Context()); res.Failed() {
		t.Fatalf("Execute: %v", res.Err)
	}
	if got := dep.created[0].Extensions; len(got) != 0 {
		t.Fatalf("spec.Extensions = %+v, want none (no defaults, no opt-in)", got)
	}
}

func TestCreateEndpointStepMountsClusterExtension(t *testing.T) {
	// A branch opt-in (pgvector) is mounted on the endpoint, resolved to its
	// pinned image. The set is read from the endpoint's own branch, not passed
	// per endpoint.
	state, dep := newFakeStateStore(), &fakeDeployer{}
	deps := endpointDeps(state, newFakeRecipeStore(), dep)
	deps.Extensions = extensions.NewCatalog("quay.io/yugabyte")
	epID, clusterID, branchID := uuid.New(), uuid.New(), uuid.New()
	state.seedBranchExtensions(clusterID, branchID, "pgvector")
	step := NewCreateEndpointStep(CreateEndpointArgs{
		EndpointID: epID, ClusterID: clusterID, BranchID: branchID, Role: "read_write", PgVersion: 18,
	}, deps)

	if res := step.Execute(t.Context()); res.Failed() {
		t.Fatalf("Execute: %v", res.Err)
	}
	exts := dep.created[0].Extensions
	if len(exts) != 1 || exts[0].Name != "pgvector" || exts[0].Image != "quay.io/yugabyte/amp-ext-pgvector:0.8.2" {
		t.Fatalf("spec.Extensions = %+v, want the branch's pgvector", exts)
	}
}

func TestCreateEndpointStepMountsClusterPreloadExtension(t *testing.T) {
	// A preload extension (pg_cron) opted in on the branch is resolved to its
	// pinned image with Preload:true carried onto the spec (so the chart extends
	// shared_preload_libraries).
	state, dep := newFakeStateStore(), &fakeDeployer{}
	deps := endpointDeps(state, newFakeRecipeStore(), dep)
	deps.Extensions = extensions.NewCatalog("quay.io/yugabyte")
	epID, clusterID, branchID := uuid.New(), uuid.New(), uuid.New()
	state.seedBranchExtensions(clusterID, branchID, "pg_cron")
	step := NewCreateEndpointStep(CreateEndpointArgs{
		EndpointID: epID, ClusterID: clusterID, BranchID: branchID, Role: "read_write", PgVersion: 18,
	}, deps)

	if res := step.Execute(t.Context()); res.Failed() {
		t.Fatalf("Execute: %v", res.Err)
	}
	exts := dep.created[0].Extensions
	if len(exts) != 1 || exts[0].Name != "pg_cron" ||
		exts[0].Image != "quay.io/yugabyte/amp-ext-pg_cron:1.6.7" || !exts[0].Preload {
		t.Fatalf("spec.Extensions = %+v, want the branch's pg_cron with Preload:true", exts)
	}
}

func TestCreateEndpointStepMountsMultipleClusterExtensions(t *testing.T) {
	// Two non-preload extensions in the branch set (pgvector + age) are both
	// resolved to their pinned images and mounted — the co-mounted case the
	// local/smoke tests exercise end-to-end.
	state, dep := newFakeStateStore(), &fakeDeployer{}
	deps := endpointDeps(state, newFakeRecipeStore(), dep)
	deps.Extensions = extensions.NewCatalog("quay.io/yugabyte")
	epID, clusterID, branchID := uuid.New(), uuid.New(), uuid.New()
	state.seedBranchExtensions(clusterID, branchID, "pgvector", "age")
	step := NewCreateEndpointStep(CreateEndpointArgs{
		EndpointID: epID, ClusterID: clusterID, BranchID: branchID, Role: "read_write", PgVersion: 18,
	}, deps)

	if res := step.Execute(t.Context()); res.Failed() {
		t.Fatalf("Execute: %v", res.Err)
	}
	var pgvOK, ageOK bool
	for _, e := range dep.created[0].Extensions {
		switch e.Name {
		case "pgvector":
			pgvOK = e.Image == "quay.io/yugabyte/amp-ext-pgvector:0.8.2" && !e.Preload
		case "age":
			ageOK = e.Image == "quay.io/yugabyte/amp-ext-age:1.8.0" && !e.Preload
		}
	}
	if len(dep.created[0].Extensions) != 2 || !pgvOK || !ageOK {
		t.Fatalf("spec.Extensions = %+v, want pgvector + age both Preload:false", dep.created[0].Extensions)
	}
}

func TestCreateEndpointStepMountsClusterExtensions(t *testing.T) {
	// An endpoint mounts its branch's opted-in set unioned with the catalog
	// defaults, de-duplicated. An unknown name in the set is dropped, proving it
	// can't wedge the deploy while known ones are honored.
	state, dep := newFakeStateStore(), &fakeDeployer{}
	clusterID, branchID := uuid.New(), uuid.New()
	state.seedBranchExtensions(clusterID, branchID, "pgvector", "totally-unknown-ext")
	deps := endpointDeps(state, newFakeRecipeStore(), dep)
	deps.Extensions = extensions.NewCatalog("r")
	step := NewCreateEndpointStep(CreateEndpointArgs{EndpointID: uuid.New(), ClusterID: clusterID, BranchID: branchID, Role: "read_write", PgVersion: 18}, deps)

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
	step := NewCreateEndpointStep(CreateEndpointArgs{EndpointID: uuid.New(), ClusterID: uuid.New(), BranchID: uuid.New(), Role: "read_write", PgVersion: 18}, endpointDeps(state, newFakeRecipeStore(), dep))
	if res := step.Execute(t.Context()); res.Failed() {
		t.Fatalf("Execute: %v", res.Err)
	}
	if len(dep.created[0].Extensions) != 0 {
		t.Fatalf("spec.Extensions = %+v, want none without a catalog", dep.created[0].Extensions)
	}
}

func TestCreateEndpointStepRequiresDeployer(t *testing.T) {
	step := NewCreateEndpointStep(CreateEndpointArgs{EndpointID: uuid.New(), ClusterID: uuid.New(), BranchID: uuid.New(), Role: "read_write"}, Deps{State: newFakeStateStore()}) // no deployer
	if res := step.Execute(t.Context()); !res.Failed() {
		t.Fatal("Execute succeeded with no deployer configured, want a failure")
	}
}
