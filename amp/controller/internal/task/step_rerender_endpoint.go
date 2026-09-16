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

// StepRerenderEndpoint is the task_step.kind for the rerender_endpoint step, the
// child of a re-render roll.
const StepRerenderEndpoint = "rerender_endpoint"

// RerenderEndpointArgs name the endpoint to re-render. There is nothing else to
// say: everything that changes comes from the endpoint's stored record and the
// deployer's current configuration.
type RerenderEndpointArgs struct {
	EndpointID uuid.UUID `json:"endpoint_id"`
}

// rerenderEndpointStep redeploys one endpoint from its stored desired state,
// unchanged, so that deployer-level configuration reaches a release rendered
// before it. TLS is the case it exists for: the mode, issuer and CA bundle live
// on the controller's config rather than on the endpoint, so an endpoint created
// before TLS was turned on keeps running without a certificate until something
// re-renders it (ac-endpoint-tls.md).
//
// It is `upgrade_endpoint` with no mutation, and shares its idempotency: the
// deployer's create-or-upgrade is keyed by the endpoint's stable name, and the
// address is re-recorded because a pod recreate can move it. An endpoint deleted
// since the planner enumerated is a skipped success, so a concurrent delete
// during a long roll does not trip fail-fast and halt everything behind it.
type rerenderEndpointStep struct {
	RerenderEndpointArgs
	state      StateStore
	deployer   deploy.Deployer
	yb         deploy.YBConn
	scURL      string
	extensions *extensions.Service
}

// NewRerenderEndpointStep binds rerender_endpoint args + its deps into an
// executable step.
func NewRerenderEndpointStep(args RerenderEndpointArgs, deps Deps) Step {
	return &rerenderEndpointStep{
		RerenderEndpointArgs: args,
		state:                deps.State,
		deployer:             deps.Deployer,
		yb:                   deps.EndpointYB,
		scURL:                deps.ControllerURL,
		extensions:           deps.Extensions,
	}
}

func buildRerenderEndpointStep(raw json.RawMessage, deps Deps) (Step, error) {
	var args RerenderEndpointArgs
	if err := json.Unmarshal(raw, &args); err != nil {
		return nil, fmt.Errorf("decode rerender_endpoint args: %w", err)
	}
	return NewRerenderEndpointStep(args, deps), nil
}

func (*rerenderEndpointStep) Kind() string { return StepRerenderEndpoint }

func (s *rerenderEndpointStep) Execute(ctx context.Context) StepResult {
	if s.deployer == nil {
		return Fail(fmt.Errorf("rerender_endpoint step requires a deployer, but none is configured"))
	}
	logger := applog.FromContext(ctx).With(applog.KeyEndpointID, s.EndpointID.String())

	desired, err := s.state.EndpointDeploy(ctx, s.EndpointID)
	if errors.Is(err, core.ErrNotFound) {
		logger.Info("endpoint is gone; nothing to re-render")
		return Ok(mustResult(map[string]string{
			"endpoint_id": s.EndpointID.String(),
			"outcome":     "skipped",
			"reason":      "the endpoint no longer exists",
		}))
	}
	if err != nil {
		return Fail(fmt.Errorf("read endpoint desired state: %w", err))
	}
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
	logger.Info("endpoint re-rendered", "address", deployed.Address)
	return Ok(mustResult(map[string]string{
		"endpoint_id": s.EndpointID.String(),
		"outcome":     "rerendered",
		"address":     deployed.Address,
	}))
}
