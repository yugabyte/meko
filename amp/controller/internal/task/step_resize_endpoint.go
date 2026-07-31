package task

import (
	"context"
	"encoding/json"
	"fmt"

	"github.com/google/uuid"

	"github.com/yugabyte/amp/controller/internal/deploy"
	applog "github.com/yugabyte/amp/controller/internal/log"
)

// StepResizeEndpoint is the task_step.kind for the resize_endpoint step.
const StepResizeEndpoint = "resize_endpoint"

// ResizeEndpointArgs are the inputs to the resize_endpoint step. CPU/Memory are
// Kubernetes quantities applied as request == limit (Guaranteed QoS preserved).
type ResizeEndpointArgs struct {
	EndpointID uuid.UUID `json:"endpoint_id"`
	CPU        string    `json:"cpu"`
	Memory     string    `json:"memory"`
}

// resizeEndpointStep changes an endpoint's compute reservation. It asks the
// deployer to resize the running pod in place, then — only once the deployer
// accepted it (a deferred/infeasible in-place outcome is not an error) —
// persists the new size to the endpoint's stored config so a controller-driven
// redeploy keeps it. Deployer-before-persist means a size the pod rejected (a
// deployer error) never lands in config. Both effects are idempotent (the resize
// is a no-op at the target size; the config write is an absolute jsonb merge), so
// a replay after a crash converges. The in-place outcome is recorded on the step
// result for the API to surface. Its deps are bound at construction (deployer is
// required); only the embedded args are serialized.
type resizeEndpointStep struct {
	ResizeEndpointArgs
	state    StateStore
	deployer deploy.Deployer
}

// NewResizeEndpointStep binds resize_endpoint args + its deps into an executable step.
func NewResizeEndpointStep(args ResizeEndpointArgs, deps Deps) Step {
	return &resizeEndpointStep{ResizeEndpointArgs: args, state: deps.State, deployer: deps.Deployer}
}

func buildResizeEndpointStep(raw json.RawMessage, deps Deps) (Step, error) {
	var args ResizeEndpointArgs
	if err := json.Unmarshal(raw, &args); err != nil {
		return nil, fmt.Errorf("decode resize_endpoint args: %w", err)
	}
	return NewResizeEndpointStep(args, deps), nil
}

func (*resizeEndpointStep) Kind() string { return StepResizeEndpoint }

func (s *resizeEndpointStep) Execute(ctx context.Context) StepResult {
	if s.deployer == nil {
		return Fail(fmt.Errorf("resize_endpoint step requires a deployer, but none is configured"))
	}
	logger := applog.FromContext(ctx).With(applog.KeyEndpointID, s.EndpointID.String())

	// (1) In-place pod resize, in the deployer. A deferred/infeasible in-place
	// outcome is NOT an error (it is carried on result, err == nil); only a real
	// apply failure (pod absent, k8s rejects the PATCH) returns an error. We must
	// not persist a size the pod never accepted, so this runs before the DB write.
	result, err := s.deployer.ResizeEndpoint(ctx, deploy.ResizeSpec{
		Name:   EndpointDeployName(s.EndpointID),
		CPU:    s.CPU,
		Memory: s.Memory,
	})
	if err != nil {
		return Fail(fmt.Errorf("resize endpoint: %w", err))
	}

	// (2) DB config: persist the new size once the deployer accepted it (including
	// a deferred/infeasible in-place outcome, which is not an error), so a
	// controller-driven redeploy converges to it. Absolute jsonb merge, so a
	// replay after a crash between the resize and this write converges (the
	// deployer resize is idempotent).
	if err := s.state.UpdateEndpointResources(ctx, s.EndpointID, s.CPU, s.Memory); err != nil {
		return Fail(fmt.Errorf("persist endpoint resources: %w", err))
	}

	logger.Info("endpoint resized", "cpu", s.CPU, "memory", s.Memory, "in_place", string(result.Outcome))
	return Ok(mustResult(map[string]string{
		"endpoint_id":      s.EndpointID.String(),
		"cpu":              s.CPU,
		"memory":           s.Memory,
		"in_place_resize":  string(result.Outcome),
		"in_place_message": result.Message,
	}))
}
