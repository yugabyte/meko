package task

import (
	"context"
	"encoding/json"
	"fmt"

	"github.com/google/uuid"

	"github.com/yugabyte/yb-amp/controller/internal/deploy"
)

// StepDeleteBranch is the task_step.kind for the delete_branch step.
const StepDeleteBranch = "delete_branch"

// DeleteBranchArgs are the inputs to the delete_branch step.
type DeleteBranchArgs struct {
	ProjectID uuid.UUID `json:"project_id"`
	BranchID  uuid.UUID `json:"branch_id"`
}

// deleteBranchStep is the single, self-contained cascade step for deleting a
// branch: the worker has no dynamic sub-step expansion, so this one step
// enumerates the branch's endpoints at Execute time (robust to a concurrently
// added endpoint on replay), retires each instance + record, then deletes the
// branch row. Every effect is idempotent (a missing instance / row is a no-op),
// so a replay after a crash converges. Its deps are bound at construction (a
// deployer is required, mirroring delete_endpoint); only the embedded args are
// serialized.
type deleteBranchStep struct {
	DeleteBranchArgs
	state    StateStore
	deployer deploy.Deployer
}

// NewDeleteBranchStep binds delete_branch args + its deps into an executable step.
func NewDeleteBranchStep(args DeleteBranchArgs, deps Deps) Step {
	return &deleteBranchStep{DeleteBranchArgs: args, state: deps.State, deployer: deps.Deployer}
}

func buildDeleteBranchStep(raw json.RawMessage, deps Deps) (Step, error) {
	var args DeleteBranchArgs
	if err := json.Unmarshal(raw, &args); err != nil {
		return nil, fmt.Errorf("decode delete_branch args: %w", err)
	}
	return NewDeleteBranchStep(args, deps), nil
}

func (*deleteBranchStep) Kind() string { return StepDeleteBranch }

func (s *deleteBranchStep) Execute(ctx context.Context) StepResult {
	if s.deployer == nil {
		return Fail(fmt.Errorf("delete_branch step requires a deployer, but none is configured"))
	}
	// Pre-check for child forks BEFORE retiring any endpoints: the parent FK is
	// RESTRICT, so a branch with children cannot be deleted. Failing first means
	// no endpoints are retired and nothing is half-deleted (the branch row would
	// otherwise survive with its compute already gone, and replays would re-hit
	// the same FK error forever).
	children, err := s.state.ChildBranchIDs(ctx, s.ProjectID, s.BranchID)
	if err != nil {
		return Fail(fmt.Errorf("check child branches: %w", err))
	}
	if len(children) > 0 {
		return Fail(fmt.Errorf("cannot delete branch %s: it has %d child branch(es); delete them first", s.BranchID, len(children)))
	}
	epIDs, err := s.state.EndpointIDsForBranch(ctx, s.ProjectID, s.BranchID)
	if err != nil {
		return Fail(fmt.Errorf("enumerate branch endpoints: %w", err))
	}
	for _, id := range epIDs {
		if err := s.deployer.DeleteEndpoint(ctx, EndpointDeployName(id)); err != nil {
			return Fail(fmt.Errorf("retire endpoint instance %s: %w", id, err))
		}
		if err := s.state.DeleteEndpoint(ctx, id); err != nil {
			return Fail(fmt.Errorf("delete endpoint record %s: %w", id, err))
		}
	}
	if err := s.state.DeleteBranch(ctx, s.ProjectID, s.BranchID); err != nil {
		return Fail(fmt.Errorf("delete branch record: %w", err))
	}
	return Ok(mustResult(map[string]any{
		"branch_id":         s.BranchID.String(),
		"endpoints_retired": len(epIDs),
	}))
}
