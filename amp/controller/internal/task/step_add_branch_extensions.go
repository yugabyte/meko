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
// durable set and re-renders the endpoints on the branch that are behind it so the changed
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
	want := extensionNames(exts)
	redeployed := 0
	for _, d := range deploys {
		// Skip an endpoint already mounting this set. The record is written below,
		// only after its deploy returned, so it means "last successfully deployed
		// with" — which makes a replay after a partial failure redeploy exactly the
		// endpoints the first run never reached, and nothing else.
		//
		// Anything unknown reads as behind: an endpoint with no record, or one whose
		// record could not be parsed, is redeployed. The cost of being wrong that way
		// is an identical manifest; the cost of the other way is an endpoint that
		// never mounts what its branch says it has.
		if sameExtensionSet(d.Extensions, want) {
			continue
		}
		redeployed++
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

	logger.Info("branch extensions added", "added", s.Extensions,
		"endpoints_redeployed", redeployed, "endpoints_already_current", len(deploys)-redeployed)
	return Ok(mustResult(map[string]any{
		"cluster_id":           s.ClusterID.String(),
		"branch_id":            s.BranchID.String(),
		"extensions":           s.Extensions,
		"endpoints_redeployed": redeployed,
	}))
}

// sameExtensionSet reports whether an endpoint's recorded mount set is already the
// one we would deploy.
//
// Compared as a set: the recorded side went through marshalExtensions, which sorts
// and dedups, while the wanted side comes out of the catalog in catalog order, so
// the two agree on membership but not on order. Names only is all there is to
// compare — a catalog entry's version rides its image, not its name — so a version
// bump does not land through this path; the fleet upgrade re-renders for that.
func sameExtensionSet(have, want []string) bool {
	if len(have) != len(want) {
		return false
	}
	seen := make(map[string]struct{}, len(have))
	for _, n := range have {
		seen[n] = struct{}{}
	}
	for _, n := range want {
		if _, ok := seen[n]; !ok {
			return false
		}
	}
	return true
}
