package task

import (
	"context"
	"encoding/json"
	"fmt"

	"github.com/google/uuid"

	"github.com/yugabyte/yb-amp/controller/internal/deploy"
)

// StepDeleteEndpoint is the task_step.kind for the delete_endpoint step.
const StepDeleteEndpoint = "delete_endpoint"

// DeleteEndpointArgs are the inputs to the delete_endpoint step.
type DeleteEndpointArgs struct {
	EndpointID uuid.UUID `json:"endpoint_id"`
}

// deleteEndpointStep retires the running instance, then deletes the record. Both
// effects are idempotent (a missing instance / row is a no-op), so a replay after
// a crash between them converges. Its deps are bound at construction (deployer is
// required); only the embedded args are serialized.
type deleteEndpointStep struct {
	DeleteEndpointArgs
	state    StateStore
	deployer deploy.Deployer
}

// NewDeleteEndpointStep binds delete_endpoint args + its deps into an executable step.
func NewDeleteEndpointStep(args DeleteEndpointArgs, deps Deps) Step {
	return &deleteEndpointStep{DeleteEndpointArgs: args, state: deps.State, deployer: deps.Deployer}
}

func buildDeleteEndpointStep(raw json.RawMessage, deps Deps) (Step, error) {
	var args DeleteEndpointArgs
	if err := json.Unmarshal(raw, &args); err != nil {
		return nil, fmt.Errorf("decode delete_endpoint args: %w", err)
	}
	return NewDeleteEndpointStep(args, deps), nil
}

func (*deleteEndpointStep) Kind() string { return StepDeleteEndpoint }

func (s *deleteEndpointStep) Execute(ctx context.Context) StepResult {
	if s.deployer == nil {
		return Fail(fmt.Errorf("delete_endpoint step requires a deployer, but none is configured"))
	}
	if err := s.deployer.DeleteEndpoint(ctx, EndpointDeployName(s.EndpointID)); err != nil {
		return Fail(fmt.Errorf("retire endpoint instance: %w", err))
	}
	if err := s.state.DeleteEndpoint(ctx, s.EndpointID); err != nil {
		return Fail(fmt.Errorf("delete endpoint record: %w", err))
	}
	return Ok(mustResult(map[string]string{"endpoint_id": s.EndpointID.String()}))
}
