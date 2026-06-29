package task

import (
	"errors"
	"testing"

	"github.com/google/uuid"
)

func TestCreateProjectStep(t *testing.T) {
	state := newFakeStateStore()
	branchID := uuid.New()
	step := NewCreateProjectStep(CreateProjectArgs{
		PoolID: uuid.New(), ProjectID: uuid.New(), BranchID: branchID, Name: "main", PgVersion: 17,
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
	if state.ensureProjectCalls != 2 {
		t.Errorf("ensure calls = %d, want 2 (step ran twice)", state.ensureProjectCalls)
	}
	if len(state.createdBranches) != 1 {
		t.Errorf("distinct branches created = %d, want 1 (idempotent)", len(state.createdBranches))
	}
	if step.Kind() != StepCreateProject {
		t.Errorf("Kind() = %q, want %q", step.Kind(), StepCreateProject)
	}
}

func TestCreateProjectStepPropagatesError(t *testing.T) {
	state := newFakeStateStore()
	state.errEnsureProject = errors.New("name taken")
	step := NewCreateProjectStep(CreateProjectArgs{ProjectID: uuid.New(), BranchID: uuid.New(), Name: "main", PgVersion: 17}, Deps{State: state})
	if res := step.Execute(t.Context()); !res.Failed() {
		t.Fatal("Execute succeeded, want the state-store error propagated")
	}
}
