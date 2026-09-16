package task

import (
	"context"
	"encoding/json"
	"fmt"

	"github.com/google/uuid"

	"github.com/yugabyte/amp/controller/internal/deploy"
)

// StepDeleteCluster is the task_step.kind for the delete_cluster step.
const StepDeleteCluster = "delete_cluster"

// DeleteClusterArgs are the inputs to the delete_cluster step.
type DeleteClusterArgs struct {
	ClusterID uuid.UUID `json:"cluster_id"`
}

// deleteClusterStep is the single, self-contained cascade step for deleting a
// cluster: it enumerates ALL of the cluster's endpoints at Execute time (robust
// to a concurrently added endpoint on replay), retires each instance + record,
// then deletes the cluster (which tombstones it and all its branches — see
// StateStore.DeleteCluster). Every effect
// is idempotent, so a replay after a crash converges. A deployer is required
// (mirroring delete_endpoint); only the embedded args are serialized.
type deleteClusterStep struct {
	DeleteClusterArgs
	state    StateStore
	deployer deploy.Deployer
	ingress  IngressAllocator
}

// NewDeleteClusterStep binds delete_cluster args + its deps into an executable step.
func NewDeleteClusterStep(args DeleteClusterArgs, deps Deps) Step {
	return &deleteClusterStep{DeleteClusterArgs: args, state: deps.State, deployer: deps.Deployer, ingress: deps.Ingress}
}

func buildDeleteClusterStep(raw json.RawMessage, deps Deps) (Step, error) {
	var args DeleteClusterArgs
	if err := json.Unmarshal(raw, &args); err != nil {
		return nil, fmt.Errorf("decode delete_cluster args: %w", err)
	}
	return NewDeleteClusterStep(args, deps), nil
}

func (*deleteClusterStep) Kind() string { return StepDeleteCluster }

func (s *deleteClusterStep) Execute(ctx context.Context) StepResult {
	if s.deployer == nil {
		return Fail(fmt.Errorf("delete_cluster step requires a deployer, but none is configured"))
	}
	epIDs, err := s.state.EndpointIDsForCluster(ctx, s.ClusterID)
	if err != nil {
		return Fail(fmt.Errorf("enumerate cluster endpoints: %w", err))
	}
	for _, id := range epIDs {
		if err := s.deployer.DeleteEndpoint(ctx, EndpointDeployName(id)); err != nil {
			return Fail(fmt.Errorf("retire endpoint instance %s: %w", id, err))
		}
		// Release with the same reasoning as delete_endpoint: after the release is
		// torn down, and before the row goes. An endpoint is hard-deleted and the
		// reclaim sweep only ages RELEASED rows, so skipping this here strands the
		// slot and its name forever — a cascade is exactly where that goes unnoticed.
		if s.ingress != nil {
			if err := s.ingress.Release(ctx, id); err != nil {
				return Fail(fmt.Errorf("release ingress slot for endpoint %s: %w", id, err))
			}
		}
		if err := s.state.DeleteEndpoint(ctx, id); err != nil {
			return Fail(fmt.Errorf("delete endpoint record %s: %w", id, err))
		}
	}
	if err := s.state.DeleteCluster(ctx, s.ClusterID); err != nil {
		return Fail(fmt.Errorf("delete cluster record: %w", err))
	}
	return Ok(mustResult(map[string]any{
		"cluster_id":        s.ClusterID.String(),
		"endpoints_retired": len(epIDs),
	}))
}
