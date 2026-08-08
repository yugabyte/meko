package task

import (
	"errors"
	"testing"

	"github.com/google/uuid"
)

func TestDeleteClusterStep(t *testing.T) {
	state, dep := newFakeStateStore(), &fakeDeployer{}
	clusterID := uuid.New()
	br1, br2 := uuid.New(), uuid.New()
	state.seedBranch(clusterID, br1)
	state.seedBranch(clusterID, br2)
	// Multiple endpoints across two branches of the cluster.
	ep1, ep2, ep3 := uuid.New(), uuid.New(), uuid.New()
	state.seedEndpoint(ep1, clusterID, br1)
	state.seedEndpoint(ep2, clusterID, br1)
	state.seedEndpoint(ep3, clusterID, br2)
	// A different cluster must be left untouched.
	otherCluster, otherBranch := uuid.New(), uuid.New()
	otherEp := uuid.New()
	state.seedBranch(otherCluster, otherBranch)
	state.seedEndpoint(otherEp, otherCluster, otherBranch)

	step := NewDeleteClusterStep(DeleteClusterArgs{ClusterID: clusterID}, Deps{State: state, Deployer: dep})
	if res := step.Execute(t.Context()); res.Failed() {
		t.Fatalf("Execute: %v", res.Err)
	}

	if len(dep.deleted) != 3 {
		t.Errorf("deployer retired %d instances, want 3 (%v)", len(dep.deleted), dep.deleted)
	}
	for _, id := range []uuid.UUID{ep1, ep2, ep3} {
		if _, ok := state.deletedEndpoints[id]; !ok {
			t.Errorf("endpoint %s record not deleted", id)
		}
	}
	if _, ok := state.deletedClusters[clusterID]; !ok {
		t.Error("cluster record not deleted")
	}
	// The unrelated cluster's endpoint survives.
	if _, ok := state.deletedEndpoints[otherEp]; ok {
		t.Error("unrelated cluster's endpoint was deleted")
	}
	if _, ok := state.branchCluster[otherBranch]; !ok {
		t.Error("unrelated cluster's branch was deleted")
	}

	// Idempotent replay.
	if res := step.Execute(t.Context()); res.Failed() {
		t.Fatalf("replay Execute: %v", res.Err)
	}
}

func TestDeleteClusterStepTolerateAbsent(t *testing.T) {
	state, dep := newFakeStateStore(), &fakeDeployer{}
	step := NewDeleteClusterStep(DeleteClusterArgs{ClusterID: uuid.New()}, Deps{State: state, Deployer: dep})
	if res := step.Execute(t.Context()); res.Failed() {
		t.Fatalf("Execute on absent cluster failed: %v", res.Err)
	}
	if len(dep.deleted) != 0 {
		t.Errorf("retired %d instances for an empty cluster, want 0", len(dep.deleted))
	}
}

func TestDeleteClusterStepRequiresDeployer(t *testing.T) {
	step := NewDeleteClusterStep(DeleteClusterArgs{ClusterID: uuid.New()}, Deps{State: newFakeStateStore()}) // no deployer
	if res := step.Execute(t.Context()); !res.Failed() {
		t.Fatal("Execute succeeded with no deployer configured, want a failure")
	}
}

func TestDeleteClusterStepRetireFailureSkipsClusterDelete(t *testing.T) {
	state := newFakeStateStore()
	dep := &fakeDeployer{deleteErr: errors.New("retire failed")}
	clusterID, branchID := uuid.New(), uuid.New()
	state.seedBranch(clusterID, branchID)
	state.seedEndpoint(uuid.New(), clusterID, branchID)

	step := NewDeleteClusterStep(DeleteClusterArgs{ClusterID: clusterID}, Deps{State: state, Deployer: dep})
	if res := step.Execute(t.Context()); !res.Failed() {
		t.Fatal("Execute succeeded despite a retire failure")
	}
	if _, ok := state.deletedClusters[clusterID]; ok {
		t.Error("cluster deleted despite an endpoint retire failing")
	}
}
