package task

import (
	"errors"
	"testing"

	"github.com/google/uuid"
)

func TestRecordBranchRecipeStepDefault(t *testing.T) {
	recipes := newFakeRecipeStore()
	projectID, branchID := uuid.New(), uuid.New()
	step := NewRecordBranchRecipeStep(RecordBranchRecipeArgs{
		ProjectID: projectID, BranchID: branchID, BranchName: "main", PgVersion: 17, StartLSN: "0/0",
	}, Deps{Recipes: recipes})
	if res := step.Execute(t.Context()); res.Failed() {
		t.Fatalf("Execute: %v", res.Err)
	}
	if len(recipes.recorded) != 1 {
		t.Fatalf("recorded %d recipes, want 1", len(recipes.recorded))
	}
	got := recipes.recorded[0]
	if got.branchID != hex32(branchID) {
		t.Errorf("recipe branch id = %s, want hex32(branch) %s", got.branchID, hex32(branchID))
	}
	if got.recipe.AncestorBranchID != "" {
		t.Errorf("default-branch recipe ancestor = %q, want empty", got.recipe.AncestorBranchID)
	}
	if got.recipe.StartLSN != "0/0" {
		t.Errorf("default-branch recipe start_lsn = %q, want 0/0", got.recipe.StartLSN)
	}
}

func TestRecordBranchRecipeStepFork(t *testing.T) {
	recipes := newFakeRecipeStore()
	ancestor := uuid.New()
	step := NewRecordBranchRecipeStep(RecordBranchRecipeArgs{
		ProjectID: uuid.New(), BranchID: uuid.New(), BranchName: "feature", PgVersion: 17,
		AncestorBranchID: &ancestor, StartLSN: "0/16ABCD0",
	}, Deps{Recipes: recipes})
	if res := step.Execute(t.Context()); res.Failed() {
		t.Fatalf("Execute: %v", res.Err)
	}
	got := recipes.recorded[0].recipe
	if got.AncestorBranchID != hex32(ancestor) {
		t.Errorf("fork recipe ancestor = %q, want hex32 %q", got.AncestorBranchID, hex32(ancestor))
	}
	if got.StartLSN != "0/16ABCD0" {
		t.Errorf("fork recipe start_lsn = %q, want the fork lsn", got.StartLSN)
	}
}

func TestRecordBranchRecipeStepNoStorageController(t *testing.T) {
	// With no storage controller (recipes nil) the step no-ops with a warning
	// rather than failing — mirroring the synchronous handler.
	step := NewRecordBranchRecipeStep(RecordBranchRecipeArgs{ProjectID: uuid.New(), BranchID: uuid.New(), StartLSN: "0/0"}, Deps{})
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
	step := NewRecordBranchRecipeStep(RecordBranchRecipeArgs{ProjectID: uuid.New(), BranchID: uuid.New(), StartLSN: "0/0"}, Deps{Recipes: recipes})
	if res := step.Execute(t.Context()); !res.Failed() {
		t.Fatal("Execute succeeded, want the record error propagated")
	}
}
