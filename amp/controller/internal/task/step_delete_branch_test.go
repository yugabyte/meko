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

// TestDeleteBranchStepDeployerOnlyForEndpoints pins where the deployer is
// actually required: retiring endpoint *instances*. A branch with none can be
// deleted without one, which is what lets a deployer-less controller
// (yb_amp_local, which spawns and stops its own processes) delete branches
// through the product API instead of a second surface (#515). A branch that
// still has endpoints must keep failing — dropping a record while its instance
// runs is the leak this whole area exists to prevent.
func TestDeleteBranchStepDeployerOnlyForEndpoints(t *testing.T) {
	projectID, branchID := uuid.New(), uuid.New()

	t.Run("no endpoints: succeeds without a deployer", func(t *testing.T) {
		state := newFakeStateStore()
		state.seedBranch(projectID, branchID)
		step := NewDeleteBranchStep(DeleteBranchArgs{ProjectID: projectID, BranchID: branchID},
			Deps{State: state}) // no deployer
		if res := step.Execute(t.Context()); res.Failed() {
			t.Fatalf("Execute failed with nothing to retire: %v", res.Err)
		}
	})

	t.Run("endpoints present: refuses without a deployer", func(t *testing.T) {
		state := newFakeStateStore()
		state.seedBranch(projectID, branchID)
		state.seedEndpoint(uuid.New(), projectID, branchID)
		step := NewDeleteBranchStep(DeleteBranchArgs{ProjectID: projectID, BranchID: branchID},
			Deps{State: state}) // no deployer
		if res := step.Execute(t.Context()); !res.Failed() {
			t.Fatal("Execute succeeded with endpoints to retire and no deployer, want a failure")
		}
	})
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
