package task

import (
	"context"
	"encoding/json"
	"fmt"

	"github.com/google/uuid"

	"github.com/yugabyte/amp/controller/internal/deploy"
	"github.com/yugabyte/amp/controller/internal/extensions"
	applog "github.com/yugabyte/amp/controller/internal/log"
)

// StepAddBranchExtensions is the task_step.kind for the add_branch_extensions step.
const StepAddBranchExtensions = "add_branch_extensions"

// AddBranchExtensionsArgs are the inputs to the add_branch_extensions step: the
// branch (with its cluster) and the catalog extension names to enable on it
// (pre-validated by the handler).
type AddBranchExtensionsArgs struct {
	ClusterID  uuid.UUID `json:"cluster_id"`
	BranchID   uuid.UUID `json:"branch_id"`
	Extensions []string  `json:"extensions"`
}

// addBranchExtensionsStep unions the requested extensions into the branch's
// durable set and re-renders every endpoint on the branch so the changed
// ImageVolume mounts take effect. It is one self-contained cascade step
// (mirroring delete_branch): it enumerates the branch's endpoints at Execute
// time — robust to a concurrently-added endpoint on replay — and each effect is
// idempotent (absolute union, then a helm-upgrade-shaped deploy keyed by the
// endpoint's stable name), so a replay after a crash converges. A deployer +
// catalog are required; only the embedded args are serialized.
type addBranchExtensionsStep struct {
	AddBranchExtensionsArgs
	state      StateStore
	deployer   deploy.Deployer
	yb         deploy.YBConn
	scURL      string
	extensions *extensions.Catalog
}

// NewAddBranchExtensionsStep binds add_branch_extensions args + its deps into an
// executable step.
func NewAddBranchExtensionsStep(args AddBranchExtensionsArgs, deps Deps) Step {
	return &addBranchExtensionsStep{
		AddBranchExtensionsArgs: args,
		state:                   deps.State,
		deployer:                deps.Deployer,
		yb:                      deps.EndpointYB,
		scURL:                   deps.ControllerURL,
		extensions:              deps.Extensions,
	}
}

func buildAddBranchExtensionsStep(raw json.RawMessage, deps Deps) (Step, error) {
	var args AddBranchExtensionsArgs
	if err := json.Unmarshal(raw, &args); err != nil {
		return nil, fmt.Errorf("decode add_branch_extensions args: %w", err)
	}
	return NewAddBranchExtensionsStep(args, deps), nil
}

func (*addBranchExtensionsStep) Kind() string { return StepAddBranchExtensions }

func (s *addBranchExtensionsStep) Execute(ctx context.Context) StepResult {
	if s.deployer == nil {
		return Fail(fmt.Errorf("add_branch_extensions step requires a deployer, but none is configured"))
	}
	// Unlike create_endpoint, a missing catalog is fatal here rather than "no
	// mounts": proceeding would strip the mounts off every endpoint on the branch.
	if s.extensions == nil {
		return Fail(fmt.Errorf("add_branch_extensions step requires an extension catalog, but none is configured"))
	}
	logger := applog.FromContext(ctx).With("cluster_id", s.ClusterID.String(), "branch_id", s.BranchID.String())

	// Union the request into the branch's durable set (absolute union, so a replay
	// is idempotent): adding is per branch.
	if err := s.state.AddBranchExtensions(ctx, s.ClusterID, s.BranchID, s.Extensions); err != nil {
		return Fail(fmt.Errorf("add branch extensions: %w", err))
	}

	deploys, err := s.state.BranchEndpointDeploys(ctx, s.ClusterID, s.BranchID)
	if err != nil {
		return Fail(fmt.Errorf("enumerate branch endpoints: %w", err))
	}
	// One read for the whole roll, before the first deploy: every endpoint on the
	// branch renders from that one snapshot. Resolving per endpoint would let a
	// concurrent AddBranchExtensions hand the later ones a larger set than the
	// earlier ones, and a read that failed partway would abort with some endpoints
	// already redeployed.
	exts, err := resolveBranchExtensions(ctx, s.extensions, s.state, s.BranchID)
	if err != nil {
		return Fail(err)
	}
	for _, d := range deploys {
		spec := deploy.EndpointSpecFrom(d, s.yb, s.scURL, exts)
		deployed, err := s.deployer.CreateEndpoint(ctx, spec)
		if err != nil {
			return Fail(fmt.Errorf("redeploy endpoint %s: %w", d.ID, err))
		}
		// A helm upgrade that changes ImageVolume mounts recreates the pod, so with
		// a ClusterIP address (the pod IP) the endpoint's recorded address is now
		// stale. Re-record it, same as create_endpoint does after its own deploy —
		// otherwise clients and YCP's readiness check consume the dead address.
		if err := s.state.SetEndpointAddress(ctx, d.ID, deployed.Address, deployed.TailnetAddress); err != nil {
			return Fail(fmt.Errorf("record endpoint address %s: %w", d.ID, err))
		}
		// Record what the endpoint now mounts (audit / recreate spec). Idempotent.
		// An endpoint that mounts nothing records nil rather than an empty slice;
		// both sinks treat the two alike (marshalExtensions writes "[]" for either,
		// extensionValuesFor collapses len==0 to nil).
		if err := s.state.UpdateEndpointExtensions(ctx, d.ID, extensionNames(spec.Extensions)); err != nil {
			return Fail(fmt.Errorf("record endpoint extensions %s: %w", d.ID, err))
		}
		// The deployer just answered with the tag it ran, so take it: for an endpoint
		// whose version is still unknown (created before the column, or by a deployer
		// that reports none) this redeploy is where it becomes known. Same guard as
		// create_endpoint — an empty report is no information and must not overwrite
		// a known version with "unknown".
		if deployed.Version != "" {
			if err := s.state.SetEndpointVersion(ctx, d.ID, deployed.Version); err != nil {
				return Fail(fmt.Errorf("record endpoint version %s: %w", d.ID, err))
			}
		}
	}

	logger.Info("branch extensions added", "added", s.Extensions, "endpoints_redeployed", len(deploys))
	return Ok(mustResult(map[string]any{
		"cluster_id":           s.ClusterID.String(),
		"branch_id":            s.BranchID.String(),
		"extensions":           s.Extensions,
		"endpoints_redeployed": len(deploys),
	}))
}
