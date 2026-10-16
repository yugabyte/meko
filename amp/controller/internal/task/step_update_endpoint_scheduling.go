package task

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"

	"github.com/google/uuid"

	"github.com/yugabyte/amp/controller/internal/core"
	"github.com/yugabyte/amp/controller/internal/deploy"
	"github.com/yugabyte/amp/controller/internal/extensions"
	applog "github.com/yugabyte/amp/controller/internal/log"
)

// StepUpdateEndpointScheduling is the task_step.kind for the
// update_endpoint_scheduling step.
const StepUpdateEndpointScheduling = "update_endpoint_scheduling"

// UpdateEndpointSchedulingArgs are the inputs to the update_endpoint_scheduling
// step. A nil list is left as stored; an empty one clears it.
type UpdateEndpointSchedulingArgs struct {
	EndpointID                uuid.UUID       `json:"endpoint_id"`
	Tolerations               json.RawMessage `json:"tolerations,omitempty"`
	TopologySpreadConstraints json.RawMessage `json:"topology_spread_constraints,omitempty"`
}

// updateEndpointSchedulingStep replaces an endpoint's tolerations and/or
// topology spread constraints. Both are pod-spec fields, so unlike connectivity
// there is no live object to patch: the release is re-rendered from the stored
// record with the new lists, which recreates the pod.
//
// Render-before-persist, as in update_endpoint_connectivity: the row is what GET
// reports and what the reconcile agent compares its spec against, so writing it
// first would let a refused render read as applied. It does not cover a render
// the API server accepts whose pod then never becomes ready: helm has no
// rollback here, so the new lists stay on the Deployment while the row keeps the
// old ones. Both halves are idempotent, so a replay after a crash between them
// converges.
type updateEndpointSchedulingStep struct {
	UpdateEndpointSchedulingArgs
	state      StateStore
	deployer   deploy.Deployer
	yb         deploy.YBConn
	scURL      string
	extensions *extensions.Service
}

// NewUpdateEndpointSchedulingStep binds update_endpoint_scheduling args + its
// deps into an executable step.
func NewUpdateEndpointSchedulingStep(args UpdateEndpointSchedulingArgs, deps Deps) Step {
	return &updateEndpointSchedulingStep{
		UpdateEndpointSchedulingArgs: args,
		state:                        deps.State,
		deployer:                     deps.Deployer,
		yb:                           deps.EndpointYB,
		scURL:                        deps.ControllerURL,
		extensions:                   deps.Extensions,
	}
}

func buildUpdateEndpointSchedulingStep(raw json.RawMessage, deps Deps) (Step, error) {
	var args UpdateEndpointSchedulingArgs
	if err := json.Unmarshal(raw, &args); err != nil {
		return nil, fmt.Errorf("decode update_endpoint_scheduling args: %w", err)
	}
	return NewUpdateEndpointSchedulingStep(args, deps), nil
}

func (*updateEndpointSchedulingStep) Kind() string { return StepUpdateEndpointScheduling }

func (s *updateEndpointSchedulingStep) Execute(ctx context.Context) StepResult {
	if s.deployer == nil {
		return Fail(fmt.Errorf("update_endpoint_scheduling step requires a deployer, but none is configured"))
	}
	logger := applog.FromContext(ctx).With(applog.KeyEndpointID, s.EndpointID.String())

	desired, err := s.state.EndpointDeploy(ctx, s.EndpointID)
	if errors.Is(err, core.ErrNotFound) {
		return Fail(fmt.Errorf("endpoint %s no longer exists", s.EndpointID))
	}
	if err != nil {
		return Fail(fmt.Errorf("read endpoint desired state: %w", err))
	}
	patch := core.Scheduling{Tolerations: s.Tolerations, TopologySpreadConstraints: s.TopologySpreadConstraints}
	desired.Scheduling = desired.Scheduling.Apply(patch)

	exts, err := resolveBranchExtensions(ctx, s.extensions, s.state, desired.BranchID)
	if err != nil {
		return Fail(err)
	}
	deployed, err := s.deployer.CreateEndpoint(ctx, deploy.EndpointSpecFrom(desired, s.yb, s.scURL, exts))
	if err != nil {
		return Fail(fmt.Errorf("re-render endpoint: %w", err))
	}
	if err := s.state.SetEndpointAddress(ctx, s.EndpointID, deployed.Address, deployed.TailnetAddress); err != nil {
		return Fail(fmt.Errorf("record endpoint address: %w", err))
	}
	if err := s.state.UpdateEndpointScheduling(ctx, s.EndpointID, patch); err != nil {
		return Fail(fmt.Errorf("persist endpoint scheduling: %w", err))
	}

	logger.Info("endpoint scheduling updated",
		"tolerations", string(desired.Scheduling.Tolerations),
		"topology_spread_constraints", string(desired.Scheduling.TopologySpreadConstraints))
	return Ok(mustResult(map[string]string{
		"endpoint_id": s.EndpointID.String(),
		"address":     deployed.Address,
	}))
}
