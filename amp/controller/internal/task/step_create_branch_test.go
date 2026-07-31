package task

import (
	"testing"

	"github.com/google/uuid"
)

func TestCreateBranchStep(t *testing.T) {
	state := newFakeStateStore()
	branchID := uuid.New()
	step := NewCreateBranchStep(CreateBranchArgs{
		ParentBranchID: uuid.New(), ProjectID: uuid.New(),
		BranchID: branchID, Name: "feature", ForkLSN: "0/16ABCD0", PgVersion: 18,
	}, Deps{State: state})

	if res := step.Execute(t.Context()); res.Failed() {
		t.Fatalf("Execute: %v", res.Err)
	}
	if res := step.Execute(t.Context()); res.Failed() {
		t.Fatalf("replay Execute: %v", res.Err)
	}
	if state.ensureBranchCalls != 2 || len(state.createdBranches) != 1 {
		t.Errorf("branch calls = %d, created = %d; want 2 and 1 (idempotent)", state.ensureBranchCalls, len(state.createdBranches))
	}
}
