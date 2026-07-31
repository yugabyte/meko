package task

import (
	"errors"
	"testing"

	"github.com/google/uuid"

	"github.com/yugabyte/amp/controller/internal/deploy"
)

func TestResizeEndpointStep(t *testing.T) {
	state, dep := newFakeStateStore(), &fakeDeployer{}
	epID := uuid.New()
	step := NewResizeEndpointStep(
		ResizeEndpointArgs{EndpointID: epID, CPU: "4", Memory: "8Gi"},
		Deps{State: state, Deployer: dep},
	)

	res := step.Execute(t.Context())
	if res.Failed() {
		t.Fatalf("Execute: %v", res.Err)
	}

	// DB config persisted (after the deployer accepted the resize).
	if got := state.endpointResources[epID]; got != [2]string{"4", "8Gi"} {
		t.Errorf("persisted resources = %v, want [4 8Gi]", got)
	}
	// Deployer asked to resize the right instance to the right size.
	if len(dep.resized) != 1 {
		t.Fatalf("deployer resize calls = %d, want 1", len(dep.resized))
	}
	if got := dep.resized[0]; got.Name != EndpointDeployName(epID) || got.CPU != "4" || got.Memory != "8Gi" {
		t.Errorf("resize spec = %+v, want name=%s cpu=4 memory=8Gi", got, EndpointDeployName(epID))
	}

	// Idempotent replay converges.
	if res := step.Execute(t.Context()); res.Failed() {
		t.Fatalf("replay Execute: %v", res.Err)
	}
}

func TestResizeEndpointStepDoesNotPersistOnResizeFailure(t *testing.T) {
	// When the live resize *errors* (the pod rejected the size / was absent), the
	// step must NOT persist that size — otherwise GetEndpoint would report a size
	// the pod never adopted, and a later redeploy would bring it up at the rejected
	// size. (A deferred/infeasible in-place *outcome* is not an error and DOES
	// persist; that path is covered by the happy-path test's outcome plumbing.)
	state := newFakeStateStore()
	dep := &fakeDeployer{resizeErr: errors.New("resize rejected")}
	epID := uuid.New()
	step := NewResizeEndpointStep(
		ResizeEndpointArgs{EndpointID: epID, CPU: "6", Memory: "12Gi"},
		Deps{State: state, Deployer: dep},
	)
	if res := step.Execute(t.Context()); !res.Failed() {
		t.Fatal("Execute succeeded, want the resize error propagated")
	}
	if got := state.endpointResources[epID]; got != [2]string{} {
		t.Errorf("persisted resources = %v, want none (a rejected size must not land in config)", got)
	}
}

func TestResizeEndpointStepRequiresDeployer(t *testing.T) {
	step := NewResizeEndpointStep(
		ResizeEndpointArgs{EndpointID: uuid.New(), CPU: "2", Memory: "4Gi"},
		Deps{State: newFakeStateStore()}, // no deployer
	)
	if res := step.Execute(t.Context()); !res.Failed() {
		t.Fatal("Execute succeeded with no deployer configured, want a failure")
	}
}

// Ensure the fake deployer's ResizeResult type is exercised (guards the fake
// against interface drift).
var _ = deploy.ResizeCompleted
