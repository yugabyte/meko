package task

import (
	"errors"
	"testing"

	"github.com/google/uuid"
)

func TestDeleteEndpointStep(t *testing.T) {
	state, dep := newFakeStateStore(), &fakeDeployer{}
	epID := uuid.New()
	step := NewDeleteEndpointStep(DeleteEndpointArgs{EndpointID: epID}, Deps{State: state, Deployer: dep})

	if res := step.Execute(t.Context()); res.Failed() {
		t.Fatalf("Execute: %v", res.Err)
	}
	if len(dep.deleted) != 1 || dep.deleted[0] != EndpointDeployName(epID) {
		t.Errorf("deployer deletes = %v, want [%s]", dep.deleted, EndpointDeployName(epID))
	}
	if _, ok := state.deletedEndpoints[epID]; !ok {
		t.Error("endpoint record was not deleted")
	}
	// Idempotent replay.
	if res := step.Execute(t.Context()); res.Failed() {
		t.Fatalf("replay Execute: %v", res.Err)
	}
}

func TestDeleteEndpointStepRetireFailureSkipsRecordDelete(t *testing.T) {
	state, dep := newFakeStateStore(), &fakeDeployer{deleteErr: errors.New("retire failed")}
	step := NewDeleteEndpointStep(DeleteEndpointArgs{EndpointID: uuid.New()}, Deps{State: state, Deployer: dep})
	if res := step.Execute(t.Context()); !res.Failed() {
		t.Fatal("Execute succeeded, want the retire error propagated")
	}
	if state.deleteEndpointCalls != 0 {
		t.Error("endpoint record was deleted despite the instance retire failing (would orphan a running instance)")
	}
}

func TestDeleteEndpointStepRequiresDeployer(t *testing.T) {
	step := NewDeleteEndpointStep(DeleteEndpointArgs{EndpointID: uuid.New()}, Deps{State: newFakeStateStore()}) // no deployer
	if res := step.Execute(t.Context()); !res.Failed() {
		t.Fatal("Execute succeeded with no deployer configured, want a failure")
	}
}
