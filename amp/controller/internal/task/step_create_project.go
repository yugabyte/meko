package task

import (
	"context"
	"encoding/json"
	"fmt"

	"github.com/google/uuid"
)

// StepCreateProject is the task_step.kind for the create_project step.
const StepCreateProject = "create_project"

// CreateProjectArgs are the inputs to the create_project step: the project + its
// default branch ids (assigned write-ahead at plan time) and the pool they live
// under.
type CreateProjectArgs struct {
	PoolID    uuid.UUID `json:"pool_id"`
	ProjectID uuid.UUID `json:"project_id"`
	BranchID  uuid.UUID `json:"branch_id"`
	Name      string    `json:"name"`
	PgVersion int       `json:"pg_version"`
}

// createProjectStep creates a project and its default branch. Its only exported
// (serialized) fields are the embedded args; state is bound at construction, so
// json.Marshal of it is exactly the persisted task_step.args.
type createProjectStep struct {
	CreateProjectArgs
	state StateStore
}

// NewCreateProjectStep binds create_project args + its deps into an executable step.
func NewCreateProjectStep(args CreateProjectArgs, deps Deps) Step {
	return &createProjectStep{CreateProjectArgs: args, state: deps.State}
}

func buildCreateProjectStep(raw json.RawMessage, deps Deps) (Step, error) {
	var args CreateProjectArgs
	if err := json.Unmarshal(raw, &args); err != nil {
		return nil, fmt.Errorf("decode create_project args: %w", err)
	}
	return NewCreateProjectStep(args, deps), nil
}

func (*createProjectStep) Kind() string { return StepCreateProject }

func (s *createProjectStep) Execute(ctx context.Context) StepResult {
	if err := s.state.EnsureProject(ctx, s.PoolID, s.ProjectID, s.BranchID, s.Name, s.PgVersion); err != nil {
		return Fail(fmt.Errorf("ensure project+default branch: %w", err))
	}
	return Ok(mustResult(map[string]string{"project_id": s.ProjectID.String(), "branch_id": s.BranchID.String()}))
}
