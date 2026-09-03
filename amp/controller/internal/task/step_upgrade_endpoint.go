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

// StepUpgradeEndpoint is the task_step.kind for the upgrade_endpoint step.
const StepUpgradeEndpoint = "upgrade_endpoint"

// UpgradeEndpointArgs are the inputs to the upgrade_endpoint step: the endpoint
// and the version to move it to — an opaque string the deployer knows how to run
// (today the endpoint image tag).
type UpgradeEndpointArgs struct {
	EndpointID uuid.UUID `json:"endpoint_id"`
	Version    string    `json:"version"`
}

// upgradeEndpointStep moves one endpoint to a target version in three phases:
// record the target, redeploy, then record what is now running. An endpoint whose
// two versions differ is one whose upgrade has not converged; replay finishes it.
//
// Idempotent: both writes are absolute UPDATEs and a release already at the
// target converges to a no-op in the deployer. Only the embedded args are
// serialized; the ports are bound at construction.
type upgradeEndpointStep struct {
	UpgradeEndpointArgs
	state      StateStore
	deployer   deploy.Deployer
	yb         deploy.YBConn
	scURL      string
	extensions *extensions.Catalog
}

// NewUpgradeEndpointStep binds upgrade_endpoint args + its deps into an executable step.
func NewUpgradeEndpointStep(args UpgradeEndpointArgs, deps Deps) Step {
	return &upgradeEndpointStep{
		UpgradeEndpointArgs: args,
		state:               deps.State,
		deployer:            deps.Deployer,
		yb:                  deps.EndpointYB,
		scURL:               deps.ControllerURL,
		extensions:          deps.Extensions,
	}
}

func buildUpgradeEndpointStep(raw json.RawMessage, deps Deps) (Step, error) {
	var args UpgradeEndpointArgs
	if err := json.Unmarshal(raw, &args); err != nil {
		return nil, fmt.Errorf("decode upgrade_endpoint args: %w", err)
	}
	return NewUpgradeEndpointStep(args, deps), nil
}

func (*upgradeEndpointStep) Kind() string { return StepUpgradeEndpoint }

func (s *upgradeEndpointStep) Execute(ctx context.Context) StepResult {
	if s.deployer == nil {
		return Fail(fmt.Errorf("upgrade_endpoint step requires a deployer, but none is configured"))
	}
	// An empty version would hand the endpoint back to whatever the running
	// controller is configured with, on this deploy and every later one.
	if s.Version == "" {
		return Fail(fmt.Errorf("upgrade_endpoint step requires a target version, but none was given"))
	}
	logger := applog.FromContext(ctx).With(applog.KeyEndpointID, s.EndpointID.String())

	// (1) The intent, before anything moves.
	if err := s.state.SetEndpointTargetVersion(ctx, s.EndpointID, s.Version); err != nil {
		return Fail(fmt.Errorf("persist endpoint target version: %w", err))
	}

	// (2) Re-render the endpoint's spec from its stored desired state, with the
	// target version on it. The version comes from the args rather than the row
	// just written, which is a separate statement this read need not observe.
	desired, err := s.state.EndpointDeploy(ctx, s.EndpointID)
	if errors.Is(err, core.ErrNotFound) {
		// Gone between the planner's enumeration and now — deleted, or its branch
		// tombstoned by a cluster delete. Nothing to upgrade, and NOT a failure: the
		// planner reports a missing endpoint as `skipped`, and a roll is never refused
		// because one of its endpoints cannot move (ac-endpoint-upgrade.md §6). Failing
		// here would instead trip fail-fast and halt every endpoint still queued behind
		// it, which a concurrent delete during a long roll would do routinely.
		logger.Info("endpoint is gone; nothing to upgrade", "requested_version", s.Version)
		return Ok(mustResult(map[string]string{
			"endpoint_id": s.EndpointID.String(),
			"outcome":     "skipped",
			"reason":      "the endpoint no longer exists",
		}))
	}
	if err != nil {
		return Fail(fmt.Errorf("read endpoint desired state: %w", err))
	}
	desired.Version = s.Version
	exts, err := resolveBranchExtensions(ctx, s.extensions, s.state, desired.BranchID)
	if err != nil {
		return Fail(err)
	}
	deployed, err := s.deployer.CreateEndpoint(ctx, deploy.EndpointSpecFrom(desired, s.yb, s.scURL, exts))
	if err != nil {
		return Fail(fmt.Errorf("redeploy endpoint: %w", err))
	}

	if err := s.state.SetEndpointAddress(ctx, s.EndpointID, deployed.Address, deployed.TailnetAddress); err != nil {
		return Fail(fmt.Errorf("record endpoint address: %w", err))
	}

	// (3) What is now running. The deployer is the authority on what a version
	// resolved to; an empty report is no information, so fall back to the target
	// rather than recording "unknown" over an endpoint that just deployed.
	version := s.Version
	if deployed.Version != "" {
		version = deployed.Version
	}
	if err := s.state.SetEndpointVersion(ctx, s.EndpointID, version); err != nil {
		return Fail(fmt.Errorf("record endpoint version: %w", err))
	}

	logger.Info("endpoint upgraded", "version", version, "requested_version", s.Version, "address", deployed.Address)
	return Ok(mustResult(map[string]string{
		"endpoint_id":     s.EndpointID.String(),
		"version":         version,
		"address":         deployed.Address,
		"tailnet_address": deployed.TailnetAddress,
	}))
}
