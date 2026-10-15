package task

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"

	"github.com/google/uuid"

	"github.com/yugabyte/amp/controller/internal/core"
	"github.com/yugabyte/amp/controller/internal/deploy"
	applog "github.com/yugabyte/amp/controller/internal/log"
)

// StepUpdateEndpointConnectivity is the task_step.kind for the
// update_endpoint_connectivity step.
const StepUpdateEndpointConnectivity = "update_endpoint_connectivity"

// UpdateEndpointConnectivityArgs are the inputs to the update_endpoint_connectivity
// step: the endpoint and the source ranges that replace its current ones.
type UpdateEndpointConnectivityArgs struct {
	EndpointID   uuid.UUID `json:"endpoint_id"`
	AllowedCIDRs []string  `json:"allowed_cidrs"`
}

// updateEndpointConnectivityStep replaces an endpoint's allowed source ranges in
// place: it patches the live object that enforces them — loadBalancerSourceRanges
// on an nlb_* endpoint's Service, the source range of an ingress_* endpoint's
// <release>-allow middleware — and then persists the list, so a later redeploy renders the same thing. It does not
// re-render the release, which would apply everything else the stored record
// says too and can roll the pod (deploy.SourceRangesUpdater, deploy.IngressAllowlistUpdater).
//
// Patch-before-persist, as in resize_endpoint: the row is what GET reports and
// what the reconcile agent compares its spec against, so writing it first would
// let a failed patch read as applied. The patch is one synchronous field write, so
// a failure leaves the object as it was. Both halves are idempotent, so a replay
// after a crash between them converges.
//
// A cluster_ip endpoint has nothing to patch; the API accepts only an empty list
// for one, so persisting it is the whole change.
type updateEndpointConnectivityStep struct {
	UpdateEndpointConnectivityArgs
	state    StateStore
	deployer deploy.Deployer
}

// NewUpdateEndpointConnectivityStep binds update_endpoint_connectivity args + its
// deps into an executable step.
func NewUpdateEndpointConnectivityStep(args UpdateEndpointConnectivityArgs, deps Deps) Step {
	return &updateEndpointConnectivityStep{
		UpdateEndpointConnectivityArgs: args,
		state:                          deps.State,
		deployer:                       deps.Deployer,
	}
}

func buildUpdateEndpointConnectivityStep(raw json.RawMessage, deps Deps) (Step, error) {
	var args UpdateEndpointConnectivityArgs
	if err := json.Unmarshal(raw, &args); err != nil {
		return nil, fmt.Errorf("decode update_endpoint_connectivity args: %w", err)
	}
	return NewUpdateEndpointConnectivityStep(args, deps), nil
}

func (*updateEndpointConnectivityStep) Kind() string { return StepUpdateEndpointConnectivity }

func (s *updateEndpointConnectivityStep) Execute(ctx context.Context) StepResult {
	if s.deployer == nil {
		return Fail(fmt.Errorf("update_endpoint_connectivity step requires a deployer, but none is configured"))
	}
	logger := applog.FromContext(ctx).With(applog.KeyEndpointID, s.EndpointID.String())

	desired, err := s.state.EndpointDeploy(ctx, s.EndpointID)
	if errors.Is(err, core.ErrNotFound) {
		// Deleted between the 202 and now. Failing is the honest answer: the
		// caller asked for ranges on an endpoint that no longer has any.
		return Fail(fmt.Errorf("endpoint %s no longer exists", s.EndpointID))
	}
	if err != nil {
		return Fail(fmt.Errorf("read endpoint desired state: %w", err))
	}
	if deploy.IsDedicatedLB(desired.Connectivity) {
		u, ok := s.deployer.(deploy.SourceRangesUpdater)
		if !ok {
			return Fail(fmt.Errorf("this deployer cannot change a load balancer's source ranges"))
		}
		if err := u.UpdateSourceRanges(ctx, EndpointDeployName(s.EndpointID), s.AllowedCIDRs); err != nil {
			return Fail(fmt.Errorf("apply endpoint allowed cidrs: %w", err))
		}
	}
	if deploy.IsIngress(desired.Connectivity) {
		u, ok := s.deployer.(deploy.IngressAllowlistUpdater)
		if !ok {
			return Fail(fmt.Errorf("this deployer cannot change an ingress endpoint's allowlist"))
		}
		if err := u.UpdateIngressAllowlist(ctx, EndpointDeployName(s.EndpointID), s.AllowedCIDRs); err != nil {
			return Fail(fmt.Errorf("apply endpoint allowed cidrs: %w", err))
		}
	}
	if err := s.state.UpdateEndpointAllowedCIDRs(ctx, s.EndpointID, s.AllowedCIDRs); err != nil {
		return Fail(fmt.Errorf("persist endpoint allowed cidrs: %w", err))
	}

	logger.Info("endpoint allowed cidrs updated", "allowed_cidrs", s.AllowedCIDRs)
	return Ok(mustResult(map[string]string{
		"endpoint_id":   s.EndpointID.String(),
		"allowed_cidrs": strings.Join(s.AllowedCIDRs, ","),
	}))
}
