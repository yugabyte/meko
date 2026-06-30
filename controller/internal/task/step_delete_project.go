package task

import (
	"context"
	"encoding/json"
	"fmt"

	"github.com/google/uuid"

	"github.com/yugabyte/yb-amp/controller/internal/deploy"
)

// StepDeleteProject is the task_step.kind for the delete_project step.
const StepDeleteProject = "delete_project"

// DeleteProjectArgs are the inputs to the delete_project step.
type DeleteProjectArgs struct {
	ProjectID uuid.UUID `json:"project_id"`
}

// deleteProjectStep is the single, self-contained cascade step for deleting a
// project: it enumerates ALL of the project's endpoints at Execute time (robust
// to a concurrently added endpoint on replay), retires each instance + record,
// then deletes the project (which drops all its branches, nulling their
// in-project parent pointers first — see StateStore.DeleteProject). Every effect
// is idempotent, so a replay after a crash converges. A deployer is required
// (mirroring delete_endpoint); only the embedded args are serialized.
type deleteProjectStep struct {
	DeleteProjectArgs
	state    StateStore
	deployer deploy.Deployer
}

// NewDeleteProjectStep binds delete_project args + its deps into an executable step.
func NewDeleteProjectStep(args DeleteProjectArgs, deps Deps) Step {
	return &deleteProjectStep{DeleteProjectArgs: args, state: deps.State, deployer: deps.Deployer}
}

func buildDeleteProjectStep(raw json.RawMessage, deps Deps) (Step, error) {
	var args DeleteProjectArgs
	if err := json.Unmarshal(raw, &args); err != nil {
		return nil, fmt.Errorf("decode delete_project args: %w", err)
	}
	return NewDeleteProjectStep(args, deps), nil
}

func (*deleteProjectStep) Kind() string { return StepDeleteProject }

func (s *deleteProjectStep) Execute(ctx context.Context) StepResult {
	if s.deployer == nil {
		return Fail(fmt.Errorf("delete_project step requires a deployer, but none is configured"))
	}
	epIDs, err := s.state.EndpointIDsForProject(ctx, s.ProjectID)
	if err != nil {
		return Fail(fmt.Errorf("enumerate project endpoints: %w", err))
	}
	for _, id := range epIDs {
		if err := s.deployer.DeleteEndpoint(ctx, EndpointDeployName(id)); err != nil {
			return Fail(fmt.Errorf("retire endpoint instance %s: %w", id, err))
		}
		if err := s.state.DeleteEndpoint(ctx, id); err != nil {
			return Fail(fmt.Errorf("delete endpoint record %s: %w", id, err))
		}
	}
	if err := s.state.DeleteProject(ctx, s.ProjectID); err != nil {
		return Fail(fmt.Errorf("delete project record: %w", err))
	}
	return Ok(mustResult(map[string]any{
		"project_id":        s.ProjectID.String(),
		"endpoints_retired": len(epIDs),
	}))
}
