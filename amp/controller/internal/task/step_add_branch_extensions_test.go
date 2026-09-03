package task

import (
	"errors"
	"testing"

	"github.com/google/uuid"

	"github.com/yugabyte/amp/controller/internal/core"
	"github.com/yugabyte/amp/controller/internal/deploy"
	"github.com/yugabyte/amp/controller/internal/extensions"
)

// TestAddBranchExtensionsStepUnionsAndRedeploys covers the happy path: the step
// unions the requested names into the branch's set and redeploys every endpoint
// on the branch with the resolved (defaults ∪ opted-in) mount set.
func TestAddBranchExtensionsStepUnionsAndRedeploys(t *testing.T) {
	state, dep := newFakeStateStore(), &fakeDeployer{}
	deps := endpointDeps(state, nil, dep)
	deps.Extensions = extensions.NewCatalog("quay.io/yugabyte")

	clusterID, branchID := uuid.New(), uuid.New()
	state.seedBranchExtensions(clusterID, branchID, "pgvector")
	ep1, ep2 := uuid.New(), uuid.New()
	state.seedEndpoint(ep1, clusterID, branchID)
	state.seedEndpoint(ep2, clusterID, branchID)

	step := NewAddBranchExtensionsStep(AddBranchExtensionsArgs{ClusterID: clusterID, BranchID: branchID, Extensions: []string{"pg_cron"}}, deps)
	if res := step.Execute(t.Context()); res.Failed() {
		t.Fatalf("Execute: %v", res.Err)
	}

	// The branch set is the union of the existing and the added names, deduped.
	got, _ := state.BranchExtensions(t.Context(), clusterID, branchID)
	set := map[string]bool{}
	for _, n := range got {
		set[n] = true
	}
	if len(got) != 2 || !set["pg_cron"] || !set["pgvector"] {
		t.Fatalf("branch extensions = %v, want {pg_cron, pgvector}", got)
	}
	// Both endpoints were redeployed, each mounting both extensions.
	if len(dep.created) != 2 {
		t.Fatalf("deploy calls = %d, want 2 (one per endpoint)", len(dep.created))
	}
	for _, spec := range dep.created {
		if len(spec.Extensions) != 2 {
			t.Fatalf("redeploy spec extensions = %+v, want both pg_cron and pgvector", spec.Extensions)
		}
	}
	// The audit set was recorded on each endpoint.
	if len(state.endpointExts[ep1]) != 2 || len(state.endpointExts[ep2]) != 2 {
		t.Fatalf("recorded endpoint extensions = %v / %v, want 2 each", state.endpointExts[ep1], state.endpointExts[ep2])
	}
}

// A redeploy re-renders each endpoint's whole spec, so every value it does not
// carry across from that endpoint's row is silently replaced by a default: the
// pod comes back on the deployer's default major version or resized to the
// chart's 2 cores/4Gi. The two endpoints here differ in both, so a spec assembled
// from anything but its own record fails.
func TestAddBranchExtensionsStepRedeploysEachEndpointAsItIs(t *testing.T) {
	state, dep := newFakeStateStore(), &fakeDeployer{}
	deps := endpointDeps(state, nil, dep)
	deps.Extensions = extensions.NewCatalog("quay.io/yugabyte")

	clusterID, branchID := uuid.New(), uuid.New()
	state.seedBranchExtensions(clusterID, branchID)
	big, small := uuid.New(), uuid.New()
	state.seedEndpointDeploy(clusterID, core.EndpointDeploy{ID: big, BranchID: branchID, PgVersion: 15, CPU: "8", Memory: "16Gi"})
	state.seedEndpointDeploy(clusterID, core.EndpointDeploy{ID: small, BranchID: branchID, PgVersion: 16, CPU: "1", Memory: "2Gi"})

	step := NewAddBranchExtensionsStep(AddBranchExtensionsArgs{ClusterID: clusterID, BranchID: branchID, Extensions: []string{"pgvector"}}, deps)
	if res := step.Execute(t.Context()); res.Failed() {
		t.Fatalf("Execute: %v", res.Err)
	}

	deployed := map[string]deploy.EndpointSpec{}
	for _, spec := range dep.created {
		deployed[spec.EndpointID] = spec
	}
	if len(deployed) != 2 {
		t.Fatalf("distinct endpoints redeployed = %d, want 2", len(deployed))
	}
	for _, want := range []deploy.EndpointSpec{
		{EndpointID: big.String(), PgVersion: 15, CPU: "8", Memory: "16Gi"},
		{EndpointID: small.String(), PgVersion: 16, CPU: "1", Memory: "2Gi"},
	} {
		got := deployed[want.EndpointID]
		if got.PgVersion != want.PgVersion || got.CPU != want.CPU || got.Memory != want.Memory {
			t.Errorf("endpoint %s redeployed as pg%d %s/%s, want pg%d %s/%s (its own row's desired state)",
				want.EndpointID, got.PgVersion, got.CPU, got.Memory, want.PgVersion, want.CPU, want.Memory)
		}
		// The newly added extension is mounted, which the read only returns when the
		// endpoint's branch id came through on its record.
		if len(got.Extensions) != 1 || got.Extensions[0].Name != "pgvector" {
			t.Errorf("endpoint %s redeployed with extensions %+v, want the branch's pgvector", want.EndpointID, got.Extensions)
		}
	}
}

// One read of the branch's set for the whole roll. Reading per endpoint instead
// would let a concurrent add give the later endpoints a larger set than the
// earlier ones, and would let a read that fails partway abort with endpoints
// already redeployed — which the store here forces by failing every read after
// the first.
func TestAddBranchExtensionsStepReadsTheBranchSetOnce(t *testing.T) {
	state := &recordingExtStore{fakeStateStore: newFakeStateStore(), err: errors.New("boom"), okReads: 1}
	dep := &fakeDeployer{}
	deps := endpointDeps(state, nil, dep)
	deps.Extensions = extensions.NewCatalog("quay.io/yugabyte")

	clusterID, branchID := uuid.New(), uuid.New()
	state.seedBranchExtensions(clusterID, branchID, "pgvector")
	for range 3 {
		state.seedEndpoint(uuid.New(), clusterID, branchID)
	}

	step := NewAddBranchExtensionsStep(AddBranchExtensionsArgs{ClusterID: clusterID, BranchID: branchID, Extensions: []string{"pg_cron"}}, deps)
	if res := step.Execute(t.Context()); res.Failed() {
		t.Fatalf("Execute: %v", res.Err)
	}
	if state.calls != 1 {
		t.Errorf("BranchExtensions reads = %d for a 3-endpoint branch, want 1", state.calls)
	}
	if len(dep.created) != 3 {
		t.Fatalf("endpoints redeployed = %d, want 3", len(dep.created))
	}
	for _, spec := range dep.created {
		if len(spec.Extensions) != 2 {
			t.Errorf("endpoint %s redeployed with %+v, want the same 2-extension snapshot as every other", spec.EndpointID, spec.Extensions)
		}
	}
}

// A branch set that cannot be read aborts the roll before the first endpoint is
// touched, rather than leaving the branch half redeployed.
func TestAddBranchExtensionsStepReadFailureDeploysNothing(t *testing.T) {
	state := &recordingExtStore{fakeStateStore: newFakeStateStore(), err: errors.New("boom")}
	dep := &fakeDeployer{}
	deps := endpointDeps(state, nil, dep)
	deps.Extensions = extensions.NewCatalog("quay.io/yugabyte")

	clusterID, branchID := uuid.New(), uuid.New()
	state.seedBranchExtensions(clusterID, branchID, "pgvector")
	state.seedEndpoint(uuid.New(), clusterID, branchID)
	state.seedEndpoint(uuid.New(), clusterID, branchID)

	step := NewAddBranchExtensionsStep(AddBranchExtensionsArgs{ClusterID: clusterID, BranchID: branchID, Extensions: []string{"pg_cron"}}, deps)
	if res := step.Execute(t.Context()); !res.Failed() {
		t.Fatal("Execute succeeded with an unreadable branch set, want a failure")
	}
	if len(dep.created) != 0 {
		t.Errorf("endpoints redeployed = %d before the failed read aborted, want 0", len(dep.created))
	}
}

// TestAddBranchExtensionsStepRequiresDeployerAndCatalog: the step fails cleanly
// when either the deployer or the catalog is absent (no task is planned without
// them, but a replay must not run half-wired).
func TestAddBranchExtensionsStepRequiresDeployerAndCatalog(t *testing.T) {
	clusterID, branchID := uuid.New(), uuid.New()

	noDeployer := NewAddBranchExtensionsStep(AddBranchExtensionsArgs{ClusterID: clusterID, BranchID: branchID, Extensions: []string{"pgvector"}},
		Deps{State: newFakeStateStore(), Extensions: extensions.NewCatalog("r")})
	if res := noDeployer.Execute(t.Context()); !res.Failed() {
		t.Fatal("Execute succeeded with no deployer, want a failure")
	}

	noCatalog := NewAddBranchExtensionsStep(AddBranchExtensionsArgs{ClusterID: clusterID, BranchID: branchID, Extensions: []string{"pgvector"}},
		Deps{State: newFakeStateStore(), Deployer: &fakeDeployer{}})
	if res := noCatalog.Execute(t.Context()); !res.Failed() {
		t.Fatal("Execute succeeded with no catalog, want a failure")
	}
}
