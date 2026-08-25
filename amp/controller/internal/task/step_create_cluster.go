package task

import (
	"context"
	"encoding/json"
	"fmt"

	"github.com/google/uuid"
)

// StepCreateCluster is the task_step.kind for the create_cluster step.
const StepCreateCluster = "create_cluster"

// CreateClusterArgs are the inputs to the create_cluster step: the cluster + its
// default branch ids (assigned write-ahead at plan time) and the pool they live
// under.
type CreateClusterArgs struct {
	PoolID    uuid.UUID `json:"pool_id"`
	ClusterID uuid.UUID `json:"cluster_id"`
	BranchID  uuid.UUID `json:"branch_id"`
	Name      string    `json:"name"`
	PgVersion int       `json:"pg_version"`
	// Extensions is the cluster-wide externalized extension set (catalog names,
	// pre-validated by the handler), written onto the default branch row so every
	// endpoint on the cluster resolves against it. Empty when none were requested.
	Extensions []string `json:"extensions,omitempty"`
}

// createClusterStep creates a cluster and its default branch. Its only exported
// (serialized) fields are the embedded args; state is bound at construction, so
// json.Marshal of it is exactly the persisted task_step.args.
type createClusterStep struct {
	CreateClusterArgs
	state StateStore
}

// NewCreateClusterStep binds create_cluster args + its deps into an executable step.
func NewCreateClusterStep(args CreateClusterArgs, deps Deps) Step {
	return &createClusterStep{CreateClusterArgs: args, state: deps.State}
}

func buildCreateClusterStep(raw json.RawMessage, deps Deps) (Step, error) {
	var args CreateClusterArgs
	if err := json.Unmarshal(raw, &args); err != nil {
		return nil, fmt.Errorf("decode create_cluster args: %w", err)
	}
	return NewCreateClusterStep(args, deps), nil
}

func (*createClusterStep) Kind() string { return StepCreateCluster }

func (s *createClusterStep) Execute(ctx context.Context) StepResult {
	if err := s.state.EnsureCluster(ctx, s.PoolID, s.ClusterID, s.BranchID, s.Name, s.PgVersion, s.Extensions); err != nil {
		return Fail(fmt.Errorf("ensure cluster+default branch: %w", err))
	}
	return Ok(mustResult(map[string]string{"cluster_id": s.ClusterID.String(), "branch_id": s.BranchID.String()}))
}
