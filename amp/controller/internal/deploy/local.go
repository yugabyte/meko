package deploy

import "context"

// LocalDeployer is the Deployer for topologies where the compute runs locally,
// started by whoever asked for it rather than placed by the controller. The
// controller still keeps the endpoint record — the row, the single-writer slot,
// the branch it hangs off — but the process that row names is not its to place,
// so every method here is a no-op that reports success.
//
// yb_amp_local is the only such topology: it spawns compute-ctl itself, with
// its own ports and data directory, and a controller that also tried to deploy
// would be fighting it for the same endpoint. Selecting this driver is what lets
// the launcher use the product endpoint API (§11) instead of a second surface.
//
// It is NOT the stub for an unconfigured controller. Leaving the deployer off
// entirely (`--deploy` unset) still makes endpoint create/retire/resize answer
// 501, which is what keeps a misconfigured production controller loud;
// `--deploy-driver=local` is the explicit opt-in that says "records only, on
// purpose".
type LocalDeployer struct{}

var _ Deployer = LocalDeployer{}

// CreateEndpoint launches nothing and returns no address: the compute this
// record names is started locally by whoever owns it. The id echoes the
// requested deploy name so the record's deploy handle keeps the same shape as
// the launching drivers'.
//
// The version echoes back for the same reason: there is no image to override, and
// what the launcher actually built is not visible from here, so the requested
// version is the only one this driver can name. Echoing keeps the record
// convergent — a version recorded is a version a later roll will not re-select.
func (LocalDeployer) CreateEndpoint(_ context.Context, spec EndpointSpec) (Endpoint, error) {
	return Endpoint{ID: spec.Name, Version: spec.Version}, nil
}

// DeleteEndpoint retires nothing; the record's owner stops its own process. The
// delete_endpoint step drops the row after this returns.
func (LocalDeployer) DeleteEndpoint(_ context.Context, _ string) error { return nil }

// ResizeEndpoint reports unsupported: there is no pod to resize in place. The
// step still persists the new reservation to the endpoint's config, so the size
// is durable even though nothing acts on it.
func (LocalDeployer) ResizeEndpoint(_ context.Context, _ ResizeSpec) (ResizeResult, error) {
	return ResizeResult{
		Outcome: ResizeUnsupported,
		Message: "the compute is started locally by its owner; resize it there",
	}, nil
}

// EndpointSafekeeperURL returns the empty string: the safekeeper is co-located
// with a compute the controller did not place, on an address it has no way to
// derive. Callers that need it must be gated on a reachable node (see
// api.Provisioning.Node, which this driver leaves nil).
func (LocalDeployer) EndpointSafekeeperURL(_ string) string { return "" }

// EndpointSupervisorURL is likewise empty: the launcher owns these endpoints and
// reaches their supervisors over the port it allocated, not through this driver.
func (LocalDeployer) EndpointSupervisorURL(_ string) string { return "" }
