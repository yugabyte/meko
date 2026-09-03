package task

import (
	"testing"

	"github.com/google/uuid"

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
	state.seedBranchExtensions(branchID, "pgvector")
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

// The redeploy renders the release from core.EndpointDeploy alone, so it must
// carry the endpoint's exposure. Without it service.connectivity renders empty,
// service-lb.yaml's gate is false, helm prunes the -lb Service and the load
// balancer is deleted — silently reverting an nlb_* endpoint to cluster_ip and
// invalidating the address every client holds.
func TestAddBranchExtensionsStepPreservesConnectivity(t *testing.T) {
	state, dep := newFakeStateStore(), &fakeDeployer{}
	deps := endpointDeps(state, nil, dep)
	deps.Extensions = extensions.NewCatalog("quay.io/yugabyte")

	clusterID, branchID := uuid.New(), uuid.New()
	nlb, plain := uuid.New(), uuid.New()
	cidrs := []string{"10.0.0.0/16"}
	state.seedEndpointWithConnectivity(nlb, clusterID, branchID, "nlb_internal", cidrs)
	state.seedEndpoint(plain, clusterID, branchID)

	step := NewAddBranchExtensionsStep(AddBranchExtensionsArgs{
		ClusterID: clusterID, BranchID: branchID, Extensions: []string{"pg_cron"},
	}, deps)
	if res := step.Execute(t.Context()); res.Failed() {
		t.Fatalf("Execute: %v", res.Err)
	}

	var sawNLB bool
	for _, spec := range dep.created {
		switch spec.EndpointID {
		case nlb.String():
			sawNLB = true
			if spec.Connectivity != "nlb_internal" {
				t.Errorf("redeploy Connectivity = %q, want nlb_internal (an empty value deletes the load balancer)", spec.Connectivity)
			}
			if len(spec.AllowedCIDRs) != 1 || spec.AllowedCIDRs[0] != cidrs[0] {
				t.Errorf("redeploy AllowedCIDRs = %v, want %v", spec.AllowedCIDRs, cidrs)
			}
		case plain.String():
			// A cluster_ip endpoint must stay unexposed, not inherit its neighbour's.
			if spec.Connectivity != "" {
				t.Errorf("cluster_ip endpoint redeployed with Connectivity = %q, want empty", spec.Connectivity)
			}
		}
	}
	if !sawNLB {
		t.Fatalf("the nlb_internal endpoint was never redeployed; specs = %+v", dep.created)
	}
}
