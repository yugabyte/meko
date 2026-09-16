package task

import (
	"testing"

	"github.com/google/uuid"

	"github.com/yugabyte/amp/controller/internal/core"
)

// The point of the child: it re-renders the WHOLE spec from the stored record,
// unchanged. Anything the record carries that the assembler drops would come
// back as a default and quietly reshape a live endpoint, so the fixture makes
// every field distinguishable from its zero value.
func TestRerenderEndpointStepRendersTheStoredRecord(t *testing.T) {
	state, dep := newFakeStateStore(), &fakeDeployer{}
	epID, clusterID, branchID := uuid.New(), uuid.New(), uuid.New()
	state.seedBranchExtensions(clusterID, branchID, "pgvector")
	state.seedEndpointDeploy(clusterID, core.EndpointDeploy{
		ID: epID, BranchID: branchID,
		PgVersion: 15, CPU: "8", Memory: "16Gi", Version: "ep-v1",
		Connectivity: "nlb_internal", AllowedCIDRs: []string{"10.0.0.0/8"},
	})
	state.endpointAddresses[epID] = "yb-ep-old.endpoints.svc:55433"

	step := NewRerenderEndpointStep(RerenderEndpointArgs{EndpointID: epID}, upgradeDeps(state, dep))
	if res := step.Execute(t.Context()); res.Failed() {
		t.Fatalf("Execute: %v", res.Err)
	}

	if len(dep.created) != 1 {
		t.Fatalf("deploy calls = %d, want 1", len(dep.created))
	}
	spec := dep.created[0]
	if spec.Name != EndpointDeployName(epID) || spec.EndpointID != epID.String() {
		t.Errorf("spec identity = %q/%q, want %q/%s", spec.Name, spec.EndpointID, EndpointDeployName(epID), epID)
	}
	// Everything the record carried survives the round trip. A re-render that
	// dropped Connectivity would delete the endpoint's load balancer.
	if spec.Version != "ep-v1" {
		t.Errorf("spec version = %q, want the stored ep-v1", spec.Version)
	}
	if spec.CPU != "8" || spec.Memory != "16Gi" || spec.PgVersion != 15 {
		t.Errorf("spec shape = %s/%s/pg%d, want 8/16Gi/pg15", spec.CPU, spec.Memory, spec.PgVersion)
	}
	if spec.Connectivity != "nlb_internal" || len(spec.AllowedCIDRs) != 1 {
		t.Errorf("spec exposure = %q/%v, want nlb_internal with one CIDR", spec.Connectivity, spec.AllowedCIDRs)
	}
	if len(spec.Extensions) != 1 || spec.Extensions[0].Name != "pgvector" {
		t.Errorf("spec extensions = %+v, want the branch's pgvector", spec.Extensions)
	}
	// The pod moved, so the address is re-recorded.
	if got := state.endpointAddresses[epID]; got == "yb-ep-old.endpoints.svc:55433" {
		t.Error("the endpoint's address was not re-recorded after the redeploy")
	}
}

// An endpoint deleted between the planner's enumeration and the child running is
// a skipped success. Failing would trip fail-fast and halt every endpoint queued
// behind it, which a concurrent delete during a long roll would do routinely.
func TestRerenderEndpointStepSkipsAnEndpointThatIsGone(t *testing.T) {
	state, dep := newFakeStateStore(), &fakeDeployer{}
	step := NewRerenderEndpointStep(RerenderEndpointArgs{EndpointID: uuid.New()}, upgradeDeps(state, dep))

	res := step.Execute(t.Context())
	if res.Failed() {
		t.Fatalf("a missing endpoint must not fail the child: %v", res.Err)
	}
	if len(dep.created) != 0 {
		t.Errorf("deploy calls = %d, want 0 for an endpoint that is gone", len(dep.created))
	}
}

// A replay with no deployer fails rather than running half-wired.
func TestRerenderEndpointStepRequiresADeployer(t *testing.T) {
	step := NewRerenderEndpointStep(RerenderEndpointArgs{EndpointID: uuid.New()},
		Deps{State: newFakeStateStore()})
	if res := step.Execute(t.Context()); !res.Failed() {
		t.Fatal("Execute succeeded with no deployer, want a failure")
	}
}
