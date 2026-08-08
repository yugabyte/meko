package task

import (
	"context"
	"encoding/json"
	"fmt"

	"github.com/google/uuid"

	applog "github.com/yugabyte/amp/controller/internal/log"
)

// StepRecordBranchRecipe is the task_step.kind for the record_branch_recipe step.
const StepRecordBranchRecipe = "record_branch_recipe"

// RecordBranchRecipeArgs are the inputs to the record_branch_recipe step: the
// recipe the controller holds until an endpoint materializes the branch.
// AncestorBranchID is nil for a cluster's default branch, set for a branch off
// another (with StartLSN = the fork point).
type RecordBranchRecipeArgs struct {
	ClusterID        uuid.UUID  `json:"cluster_id"`
	BranchID         uuid.UUID  `json:"branch_id"`
	BranchName       string     `json:"branch_name"`
	PgVersion        int        `json:"pg_version"`
	AncestorBranchID *uuid.UUID `json:"ancestor_branch_id,omitempty"`
	StartLSN         string     `json:"start_lsn"`
	// PgSettings are the caller's settings for the new branch. They merge OVER
	// what it inherits from its ancestor and win on a key both define; nil
	// inherits unchanged.
	PgSettings map[string]string `json:"pg_settings,omitempty"`
}

// recordBranchRecipeStep records the controller's pending-init recipe.
// With no controller (recipes nil) it no-ops with a warning; the recipe
// then lives only in the control DB's branch row. Only the embedded args are
// serialized; recipes is bound at construction.
type recordBranchRecipeStep struct {
	RecordBranchRecipeArgs
	recipes RecipeStore
}

// NewRecordBranchRecipeStep binds record_branch_recipe args + its deps into an
// executable step.
func NewRecordBranchRecipeStep(args RecordBranchRecipeArgs, deps Deps) Step {
	return &recordBranchRecipeStep{RecordBranchRecipeArgs: args, recipes: deps.Recipes}
}

func buildRecordBranchRecipeStep(raw json.RawMessage, deps Deps) (Step, error) {
	var args RecordBranchRecipeArgs
	if err := json.Unmarshal(raw, &args); err != nil {
		return nil, fmt.Errorf("decode record_branch_recipe args: %w", err)
	}
	return NewRecordBranchRecipeStep(args, deps), nil
}

func (*recordBranchRecipeStep) Kind() string { return StepRecordBranchRecipe }

func (s *recordBranchRecipeStep) Execute(ctx context.Context) StepResult {
	// What a missing store costs depends on which branch this is, because this step
	// plays two roles:
	//
	//   - For a FORK it is the branch's creator: the row, its
	//     amp_storage.branch_state seed, and the fork/GC-cutoff interlock commit in
	//     one transaction (that atomicity is why a rejected fork leaves no orphan).
	//     With no store there is no branch at all, so Ok would be a task reporting
	//     success having created nothing. Fail.
	//   - For a cluster's DEFAULT branch there is no ancestor and no interlock, and
	//     the row was already written by create_cluster. Only the branch_state seed
	//     is missed, which degrades exactly as it always has: the branch exists in
	//     the control plane but nothing can materialize it.
	if s.recipes == nil {
		if s.AncestorBranchID != nil {
			return Fail(fmt.Errorf(
				"cannot declare branch %s: no storage-layer store is configured", s.BranchID))
		}
		applog.FromContext(ctx).Warn("no storage-layer store; branch declared in the control plane but not seeded for attach",
			applog.KeyClusterID, s.ClusterID.String(), applog.KeyBranchID, s.BranchID.String())
		return Ok(mustResult(map[string]bool{"recorded": false}))
	}
	ancestor := ""
	if s.AncestorBranchID != nil {
		ancestor = hex32(*s.AncestorBranchID)
	}
	if err := s.recipes.RecordBranchRecipe(ctx, s.ClusterID, hex32(s.BranchID), BranchRecipe{
		BranchName:       s.BranchName,
		PgVersion:        s.PgVersion,
		AncestorBranchID: ancestor,
		StartLSN:         s.StartLSN,
		PgSettings:       s.PgSettings,
	}); err != nil {
		return Fail(fmt.Errorf("record branch recipe: %w", err))
	}
	return Ok(mustResult(map[string]bool{"recorded": true}))
}
