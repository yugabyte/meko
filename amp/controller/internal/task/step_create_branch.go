package task

import (
	"context"
	"encoding/json"
	"fmt"

	"github.com/google/uuid"
)

// StepCreateBranch is the task_step.kind for the create_branch step.
const StepCreateBranch = "create_branch"

// CreateBranchArgs are the inputs to the create_branch step. The branch stays in
// its parent's project; ForkLSN is the storage layer's "X/X" pg_lsn form,
// resolved at plan time.
type CreateBranchArgs struct {
	ParentBranchID uuid.UUID `json:"parent_branch_id"`
	ProjectID      uuid.UUID `json:"project_id"`
	BranchID       uuid.UUID `json:"branch_id"`
	Name           string    `json:"name"`
	ForkLSN        string    `json:"fork_lsn"`
	PgVersion      int       `json:"pg_version"`
}

// createBranchStep forks an existing branch into a new branch. Only the embedded
// args are serialized; state is bound at construction.
type createBranchStep struct {
	CreateBranchArgs
	state StateStore
}

// NewCreateBranchStep binds create_branch args + its deps into an executable step.
func NewCreateBranchStep(args CreateBranchArgs, deps Deps) Step {
	return &createBranchStep{CreateBranchArgs: args, state: deps.State}
}

func buildCreateBranchStep(raw json.RawMessage, deps Deps) (Step, error) {
	var args CreateBranchArgs
	if err := json.Unmarshal(raw, &args); err != nil {
		return nil, fmt.Errorf("decode create_branch args: %w", err)
	}
	return NewCreateBranchStep(args, deps), nil
}

func (*createBranchStep) Kind() string { return StepCreateBranch }

func (s *createBranchStep) Execute(ctx context.Context) StepResult {
	if err := s.state.EnsureBranch(ctx, s.ParentBranchID, s.ProjectID, s.BranchID, s.Name, s.ForkLSN, s.PgVersion); err != nil {
		return Fail(fmt.Errorf("ensure branch: %w", err))
	}
	return Ok(mustResult(map[string]string{"project_id": s.ProjectID.String(), "branch_id": s.BranchID.String()}))
}
