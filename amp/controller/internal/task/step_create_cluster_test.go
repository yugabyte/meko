package task

import (
	"errors"
	"testing"

	"github.com/google/uuid"
)

func TestCreateClusterStep(t *testing.T) {
	state := newFakeStateStore()
	branchID := uuid.New()
	step := NewCreateClusterStep(CreateClusterArgs{
		PoolID: uuid.New(), ClusterID: uuid.New(), BranchID: branchID, Name: "main", PgVersion: 18,
	}, Deps{State: state})

	res := step.Execute(t.Context())
	if res.Failed() {
		t.Fatalf("Execute: %v", res.Err)
	}
	if decodeResult(t, res.Output)["branch_id"] != branchID.String() {
		t.Errorf("result branch_id = %v, want %s", decodeResult(t, res.Output)["branch_id"], branchID)
	}

	// Idempotent replay: a second run converges (no error, no duplicate create).
	if res := step.Execute(t.Context()); res.Failed() {
		t.Fatalf("replay Execute: %v", res.Err)
	}
	if state.ensureClusterCalls != 2 {
		t.Errorf("ensure calls = %d, want 2 (step ran twice)", state.ensureClusterCalls)
	}
	if len(state.createdBranches) != 1 {
		t.Errorf("distinct branches created = %d, want 1 (idempotent)", len(state.createdBranches))
	}
	if step.Kind() != StepCreateCluster {
		t.Errorf("Kind() = %q, want %q", step.Kind(), StepCreateCluster)
	}
}

func TestCreateClusterStepPropagatesError(t *testing.T) {
	state := newFakeStateStore()
	state.errEnsureCluster = errors.New("name taken")
	step := NewCreateClusterStep(CreateClusterArgs{ClusterID: uuid.New(), BranchID: uuid.New(), Name: "main", PgVersion: 18}, Deps{State: state})
	if res := step.Execute(t.Context()); !res.Failed() {
		t.Fatal("Execute succeeded, want the state-store error propagated")
	}
}
