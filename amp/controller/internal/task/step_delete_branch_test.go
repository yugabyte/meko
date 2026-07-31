package task

import (
	"errors"
	"testing"

	"github.com/google/uuid"
)

func TestDeleteBranchStep(t *testing.T) {
	state, dep := newFakeStateStore(), &fakeDeployer{}
	projectID, branchID := uuid.New(), uuid.New()
	state.seedBranch(projectID, branchID)
	ep1, ep2 := uuid.New(), uuid.New()
	state.seedEndpoint(ep1, projectID, branchID)
	state.seedEndpoint(ep2, projectID, branchID)
	// An endpoint on another branch must be left untouched.
	other := uuid.New()
	state.seedEndpoint(other, projectID, uuid.New())

	step := NewDeleteBranchStep(DeleteBranchArgs{ProjectID: projectID, BranchID: branchID}, Deps{State: state, Deployer: dep})
	if res := step.Execute(t.Context()); res.Failed() {
		t.Fatalf("Execute: %v", res.Err)
	}

	// Both branch endpoints retired (instance + record), the unrelated one kept.
	if len(dep.deleted) != 2 {
		t.Errorf("deployer retired %d instances, want 2 (%v)", len(dep.deleted), dep.deleted)
	}
	for _, id := range []uuid.UUID{ep1, ep2} {
		if _, ok := state.deletedEndpoints[id]; !ok {
			t.Errorf("endpoint %s record not deleted", id)
		}
	}
	if _, ok := state.deletedEndpoints[other]; ok {
		t.Error("unrelated endpoint was deleted")
	}
	if _, ok := state.deletedBranches[branchID]; !ok {
		t.Error("branch record not deleted")
	}

	// Idempotent replay: endpoints already gone, branch already gone, still succeeds.
	if res := step.Execute(t.Context()); res.Failed() {
		t.Fatalf("replay Execute: %v", res.Err)
	}
}

func TestDeleteBranchStepTolerateAbsent(t *testing.T) {
	state, dep := newFakeStateStore(), &fakeDeployer{}
	// Nothing seeded: a branch with no endpoints and no row converges to success.
	step := NewDeleteBranchStep(DeleteBranchArgs{ProjectID: uuid.New(), BranchID: uuid.New()}, Deps{State: state, Deployer: dep})
	if res := step.Execute(t.Context()); res.Failed() {
		t.Fatalf("Execute on absent branch failed: %v", res.Err)
	}
	if len(dep.deleted) != 0 {
		t.Errorf("retired %d instances for an empty branch, want 0", len(dep.deleted))
	}
}

func TestDeleteBranchStepRequiresDeployer(t *testing.T) {
	step := NewDeleteBranchStep(DeleteBranchArgs{ProjectID: uuid.New(), BranchID: uuid.New()}, Deps{State: newFakeStateStore()}) // no deployer
	if res := step.Execute(t.Context()); !res.Failed() {
		t.Fatal("Execute succeeded with no deployer configured, want a failure")
	}
}

func TestDeleteBranchStepRetireFailureSkipsBranchDelete(t *testing.T) {
	state := newFakeStateStore()
	dep := &fakeDeployer{deleteErr: errors.New("retire failed")}
	projectID, branchID := uuid.New(), uuid.New()
	state.seedBranch(projectID, branchID)
	state.seedEndpoint(uuid.New(), projectID, branchID)

	step := NewDeleteBranchStep(DeleteBranchArgs{ProjectID: projectID, BranchID: branchID}, Deps{State: state, Deployer: dep})
	if res := step.Execute(t.Context()); !res.Failed() {
		t.Fatal("Execute succeeded despite a retire failure")
	}
	if _, ok := state.deletedBranches[branchID]; ok {
		t.Error("branch deleted despite an endpoint retire failing (would orphan a running instance)")
	}
}
