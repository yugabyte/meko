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
// AncestorBranchID is nil for a project's default branch, set for a branch off
// another (with StartLSN = the fork point).
type RecordBranchRecipeArgs struct {
	ProjectID        uuid.UUID  `json:"project_id"`
	BranchID         uuid.UUID  `json:"branch_id"`
	BranchName       string     `json:"branch_name"`
	PgVersion        int        `json:"pg_version"`
	AncestorBranchID *uuid.UUID `json:"ancestor_branch_id,omitempty"`
	StartLSN         string     `json:"start_lsn"`
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
	if s.recipes == nil {
		applog.FromContext(ctx).Warn("controller not available; branch recipe recorded in the control DB only",
			applog.KeyProjectID, s.ProjectID.String(), applog.KeyBranchID, s.BranchID.String())
		return Ok(mustResult(map[string]bool{"recorded": false}))
	}
	ancestor := ""
	if s.AncestorBranchID != nil {
		ancestor = hex32(*s.AncestorBranchID)
	}
	if err := s.recipes.RecordBranchRecipe(ctx, hex32(s.BranchID), BranchRecipe{
		BranchName:       s.BranchName,
		PgVersion:        s.PgVersion,
		AncestorBranchID: ancestor,
		StartLSN:         s.StartLSN,
	}); err != nil {
		return Fail(fmt.Errorf("record branch recipe: %w", err))
	}
	return Ok(mustResult(map[string]bool{"recorded": true}))
}
