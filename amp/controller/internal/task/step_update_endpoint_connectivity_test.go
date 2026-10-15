package task

import (
	"errors"
	"slices"
	"testing"

	"github.com/google/uuid"

	"github.com/yugabyte/amp/controller/internal/core"
	"github.com/yugabyte/amp/controller/internal/deploy"
)

func seedConnectivityEndpoint(state *fakeStateStore, mode string) uuid.UUID {
	epID := uuid.New()
	state.seedEndpointDeploy(uuid.New(), core.EndpointDeploy{
		ID: epID, BranchID: uuid.New(), PgVersion: 15, CPU: "8", Memory: "16Gi",
		Connectivity: mode, AllowedCIDRs: []string{"103.200.215.98/32"},
	})
	return epID
}

// The list goes to the live Service and then the row, and the release is NOT
// re-rendered: a re-render would also apply a resize the pod template has not
// seen, and roll the pod.
func TestUpdateEndpointConnectivityStepPatchesTheServiceOnly(t *testing.T) {
	state, dep := newFakeStateStore(), &fakeDeployer{}
	epID := seedConnectivityEndpoint(state, deploy.ConnectivityNLBExternal)
	want := []string{"103.200.215.98/32", "192.0.2.10/32"}

	step := NewUpdateEndpointConnectivityStep(
		UpdateEndpointConnectivityArgs{EndpointID: epID, AllowedCIDRs: want}, upgradeDeps(state, dep))
	if res := step.Execute(t.Context()); res.Failed() {
		t.Fatalf("Execute: %v", res.Err)
	}

	if len(dep.created) != 0 {
		t.Errorf("deploy calls = %d, want none — the release must not be re-rendered", len(dep.created))
	}
	if len(dep.sourceRanges) != 1 {
		t.Fatalf("source-range patches = %d, want 1", len(dep.sourceRanges))
	}
	if got := dep.sourceRanges[0]; got.name != EndpointDeployName(epID) || !slices.Equal(got.cidrs, want) {
		t.Errorf("patched %s with %v, want %s with %v", got.name, got.cidrs, EndpointDeployName(epID), want)
	}
	if got := state.endpointCIDRs[epID]; !slices.Equal(got, want) {
		t.Errorf("persisted allowed cidrs = %v, want %v", got, want)
	}
}

// A failed patch must not write the row: GET reports it and the reconcile agent
// compares its spec against it, so a written row would read as applied.
func TestUpdateEndpointConnectivityStepDoesNotPersistWhenThePatchFails(t *testing.T) {
	state, dep := newFakeStateStore(), &fakeDeployer{sourceRangesErr: errors.New("forbidden")}
	epID := seedConnectivityEndpoint(state, deploy.ConnectivityNLBInternal)

	step := NewUpdateEndpointConnectivityStep(
		UpdateEndpointConnectivityArgs{EndpointID: epID, AllowedCIDRs: []string{"192.0.2.10/32"}}, upgradeDeps(state, dep))
	if res := step.Execute(t.Context()); !res.Failed() {
		t.Fatal("Execute succeeded with a failing patch, want a failure")
	}
	if _, wrote := state.endpointCIDRs[epID]; wrote {
		t.Error("allowed cidrs were persisted although the patch failed")
	}
}

// Without a dedicated load balancer there is no Service to patch — the API takes
// only an empty list for one — so the row is the whole change.
func TestUpdateEndpointConnectivityStepWithoutALoadBalancerOnlyPersists(t *testing.T) {
	state, dep := newFakeStateStore(), &fakeDeployer{}
	epID := seedConnectivityEndpoint(state, deploy.ConnectivityClusterIP)

	step := NewUpdateEndpointConnectivityStep(
		UpdateEndpointConnectivityArgs{EndpointID: epID, AllowedCIDRs: []string{}}, upgradeDeps(state, dep))
	if res := step.Execute(t.Context()); res.Failed() {
		t.Fatalf("Execute: %v", res.Err)
	}
	if len(dep.sourceRanges) != 0 {
		t.Errorf("source-range patches = %d, want none for cluster_ip", len(dep.sourceRanges))
	}
	if _, wrote := state.endpointCIDRs[epID]; !wrote {
		t.Error("allowed cidrs were not persisted")
	}
}

// Unlike a re-render roll, this was asked for one endpoint by name, so its
// disappearance is the caller's failure rather than a skip.
func TestUpdateEndpointConnectivityStepFailsForAnEndpointThatIsGone(t *testing.T) {
	state, dep := newFakeStateStore(), &fakeDeployer{}
	step := NewUpdateEndpointConnectivityStep(
		UpdateEndpointConnectivityArgs{EndpointID: uuid.New()}, upgradeDeps(state, dep))
	if res := step.Execute(t.Context()); !res.Failed() {
		t.Fatal("Execute succeeded for a missing endpoint, want a failure")
	}
}

// An ingress endpoint's allowlist is its middleware, not a Service field, and is
// patched in place for the same reason the nlb_* Service is.
func TestUpdateEndpointConnectivityStepPatchesTheIngressMiddleware(t *testing.T) {
	for _, mode := range []string{deploy.ConnectivityIngressInternal, deploy.ConnectivityIngressExternal} {
		state, dep := newFakeStateStore(), &fakeDeployer{}
		epID := seedConnectivityEndpoint(state, mode)
		want := []string{"192.0.2.10/32"}

		step := NewUpdateEndpointConnectivityStep(
			UpdateEndpointConnectivityArgs{EndpointID: epID, AllowedCIDRs: want}, upgradeDeps(state, dep))
		if res := step.Execute(t.Context()); res.Failed() {
			t.Fatalf("%s: Execute: %v", mode, res.Err)
		}
		if len(dep.created) != 0 || len(dep.sourceRanges) != 0 {
			t.Errorf("%s: deploys = %d, Service patches = %d, want neither", mode, len(dep.created), len(dep.sourceRanges))
		}
		if len(dep.ingressAllowlists) != 1 || !slices.Equal(dep.ingressAllowlists[0].cidrs, want) {
			t.Fatalf("%s: middleware patches = %v, want one with %v", mode, dep.ingressAllowlists, want)
		}
		if got := state.endpointCIDRs[epID]; !slices.Equal(got, want) {
			t.Errorf("%s: persisted allowed cidrs = %v, want %v", mode, got, want)
		}
	}
}

// An endpoint that predates its middleware cannot enforce a list, so the row must
// not claim one.
func TestUpdateEndpointConnectivityStepDoesNotPersistWhenTheMiddlewareIsMissing(t *testing.T) {
	state, dep := newFakeStateStore(), &fakeDeployer{ingressAllowlistErr: errors.New("no allowlist middleware")}
	epID := seedConnectivityEndpoint(state, deploy.ConnectivityIngressInternal)

	step := NewUpdateEndpointConnectivityStep(
		UpdateEndpointConnectivityArgs{EndpointID: epID, AllowedCIDRs: []string{"192.0.2.10/32"}}, upgradeDeps(state, dep))
	if res := step.Execute(t.Context()); !res.Failed() {
		t.Fatal("Execute succeeded without a middleware, want a failure")
	}
	if _, wrote := state.endpointCIDRs[epID]; wrote {
		t.Error("allowed cidrs were persisted although nothing enforces them")
	}
}
