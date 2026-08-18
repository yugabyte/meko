package task

import (
	"errors"
	"testing"

	"github.com/google/uuid"
)

func TestRecordBranchRecipeStepDefault(t *testing.T) {
	recipes := newFakeRecipeStore()
	clusterID, branchID := uuid.New(), uuid.New()
	step := NewRecordBranchRecipeStep(RecordBranchRecipeArgs{
		ClusterID: clusterID, BranchID: branchID, BranchName: "main", PgVersion: 18, StartLSN: "0/0",
	}, Deps{Recipes: recipes})
	if res := step.Execute(t.Context()); res.Failed() {
		t.Fatalf("Execute: %v", res.Err)
	}
	if len(recipes.recorded) != 1 {
		t.Fatalf("recorded %d recipes, want 1", len(recipes.recorded))
	}
	got := recipes.recorded[0]
	if got.branchID != branchID {
		t.Errorf("recipe branch id = %s, want %s", got.branchID, branchID)
	}
	if got.recipe.AncestorBranchID != nil {
		t.Errorf("default-branch recipe ancestor = %v, want nil", got.recipe.AncestorBranchID)
	}
	if got.recipe.StartLSN != "0/0" {
		t.Errorf("default-branch recipe start_lsn = %q, want 0/0", got.recipe.StartLSN)
	}
}

func TestRecordBranchRecipeStepFork(t *testing.T) {
	recipes := newFakeRecipeStore()
	ancestor := uuid.New()
	step := NewRecordBranchRecipeStep(RecordBranchRecipeArgs{
		ClusterID: uuid.New(), BranchID: uuid.New(), BranchName: "feature", PgVersion: 18,
		AncestorBranchID: &ancestor, StartLSN: "0/16ABCD0",
	}, Deps{Recipes: recipes})
	if res := step.Execute(t.Context()); res.Failed() {
		t.Fatalf("Execute: %v", res.Err)
	}
	got := recipes.recorded[0].recipe
	if got.AncestorBranchID == nil || *got.AncestorBranchID != ancestor {
		t.Errorf("fork recipe ancestor = %v, want %s", got.AncestorBranchID, ancestor)
	}
	if got.StartLSN != "0/16ABCD0" {
		t.Errorf("fork recipe start_lsn = %q, want the fork lsn", got.StartLSN)
	}
}

func TestRecordBranchRecipeStepNoController(t *testing.T) {
	// With no controller (recipes nil) the step no-ops with a warning
	// rather than failing — mirroring the synchronous handler.
	step := NewRecordBranchRecipeStep(RecordBranchRecipeArgs{ClusterID: uuid.New(), BranchID: uuid.New(), StartLSN: "0/0"}, Deps{})
	res := step.Execute(t.Context())
	if res.Failed() {
		t.Fatalf("Execute with nil recipes failed, want no-op success: %v", res.Err)
	}
	if decodeResult(t, res.Output)["recorded"] != false {
		t.Errorf("result recorded = %v, want false", decodeResult(t, res.Output)["recorded"])
	}
}

func TestRecordBranchRecipeStepPropagatesError(t *testing.T) {
	recipes := newFakeRecipeStore()
	recipes.errRecord = errors.New("sc down")
	step := NewRecordBranchRecipeStep(RecordBranchRecipeArgs{ClusterID: uuid.New(), BranchID: uuid.New(), StartLSN: "0/0"}, Deps{Recipes: recipes})
	if res := step.Execute(t.Context()); !res.Failed() {
		t.Fatal("Execute succeeded, want the record error propagated")
	}
}
