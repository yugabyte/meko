package task

import (
	"errors"
	"testing"

	"github.com/google/uuid"
)

func TestDeleteProjectStep(t *testing.T) {
	state, dep := newFakeStateStore(), &fakeDeployer{}
	projectID := uuid.New()
	br1, br2 := uuid.New(), uuid.New()
	state.seedBranch(projectID, br1)
	state.seedBranch(projectID, br2)
	// Multiple endpoints across two branches of the project.
	ep1, ep2, ep3 := uuid.New(), uuid.New(), uuid.New()
	state.seedEndpoint(ep1, projectID, br1)
	state.seedEndpoint(ep2, projectID, br1)
	state.seedEndpoint(ep3, projectID, br2)
	// A different project must be left untouched.
	otherProject, otherBranch := uuid.New(), uuid.New()
	otherEp := uuid.New()
	state.seedBranch(otherProject, otherBranch)
	state.seedEndpoint(otherEp, otherProject, otherBranch)

	step := NewDeleteProjectStep(DeleteProjectArgs{ProjectID: projectID}, Deps{State: state, Deployer: dep})
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
	if _, ok := state.deletedProjects[projectID]; !ok {
		t.Error("project record not deleted")
	}
	// The unrelated project's endpoint survives.
	if _, ok := state.deletedEndpoints[otherEp]; ok {
		t.Error("unrelated project's endpoint was deleted")
	}
	if _, ok := state.branchProject[otherBranch]; !ok {
		t.Error("unrelated project's branch was deleted")
	}

	// Idempotent replay.
	if res := step.Execute(t.Context()); res.Failed() {
		t.Fatalf("replay Execute: %v", res.Err)
	}
}

func TestDeleteProjectStepTolerateAbsent(t *testing.T) {
	state, dep := newFakeStateStore(), &fakeDeployer{}
	step := NewDeleteProjectStep(DeleteProjectArgs{ProjectID: uuid.New()}, Deps{State: state, Deployer: dep})
	if res := step.Execute(t.Context()); res.Failed() {
		t.Fatalf("Execute on absent project failed: %v", res.Err)
	}
	if len(dep.deleted) != 0 {
		t.Errorf("retired %d instances for an empty project, want 0", len(dep.deleted))
	}
}

func TestDeleteProjectStepRequiresDeployer(t *testing.T) {
	step := NewDeleteProjectStep(DeleteProjectArgs{ProjectID: uuid.New()}, Deps{State: newFakeStateStore()}) // no deployer
	if res := step.Execute(t.Context()); !res.Failed() {
		t.Fatal("Execute succeeded with no deployer configured, want a failure")
	}
}

func TestDeleteProjectStepRetireFailureSkipsProjectDelete(t *testing.T) {
	state := newFakeStateStore()
	dep := &fakeDeployer{deleteErr: errors.New("retire failed")}
	projectID, branchID := uuid.New(), uuid.New()
	state.seedBranch(projectID, branchID)
	state.seedEndpoint(uuid.New(), projectID, branchID)

	step := NewDeleteProjectStep(DeleteProjectArgs{ProjectID: projectID}, Deps{State: state, Deployer: dep})
	if res := step.Execute(t.Context()); !res.Failed() {
		t.Fatal("Execute succeeded despite a retire failure")
	}
	if _, ok := state.deletedProjects[projectID]; ok {
		t.Error("project deleted despite an endpoint retire failing")
	}
}
