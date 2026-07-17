package features

import (
	"context"

	"github.com/yugabyte/yb-amp/test/functional/framework/apps/controller"
	"github.com/yugabyte/yb-amp/test/functional/framework/apps/pg"
	"github.com/yugabyte/yb-amp/test/functional/framework/core"
	"github.com/yugabyte/yb-amp/test/functional/framework/exec/k8s"
)

// ampControlPlane is the amp control-plane handle a scheduler-provided environment
// exposes so an amp-feature test can ATTACH to a STANDING control plane instead of
// self-provisioning its own backing + amp. env/sharedk8s.Env satisfies it
// structurally (project-per-test on a standing amp control plane); a fake satisfies
// it in unit tests. When t.Env does NOT satisfy it — the fake/none provider, or a
// universe env — the flow falls back to provisionYBK8s + deployAmp (today's
// self-provision path).
//
// The four members are exactly what the flow needs to source the amp handle from
// the environment: a controller client (the ampCtl seam), the pool the
// project-per-test is created on, a way to connect an endpoint's Postgres (through
// the provider's port-forward when the control plane is cluster-internal), and the
// namespace kubectl the compute-restart roller drives. The assertion cores
// (branchIsolationSteps / endpointLifecycleSteps / computeRestartSteps) never see
// this type — only where the flow builds their seams changes.
type ampControlPlane interface {
	// AmpController returns a live amp controller client over the standing control
	// plane (re-establishing any provider-owned forward as needed).
	AmpController(ctx context.Context) (*controller.Client, error)
	// AmpPoolID is the storage pool the project-per-test is created on.
	AmpPoolID() string
	// ConnectEndpoint opens a pooled SQL connection to an amp endpoint over the
	// standing control plane, port-forwarding the endpoint Service when it is
	// cluster-internal. The forward's lifetime is owned by the environment.
	ConnectEndpoint(ctx context.Context, ep controller.Endpoint) (*pg.Client, error)
	// AmpKube is the kubectl scoped to the control-plane namespace (where endpoint
	// compute pods live), or nil when the controller is reached by a direct URL
	// (no kube, so the compute-restart roller cannot run).
	AmpKube() *k8s.Kubectl
}

// ampEnvFrom returns the amp control-plane handle a provided environment exposes,
// and whether it does. A nil / non-amp env yields (nil, false) — the self-provision
// path. This one type-assert is the whole attach-vs-self-provision decision point.
func ampEnvFrom(env core.Environment) (ampControlPlane, bool) {
	if env == nil {
		return nil, false
	}
	cp, ok := env.(ampControlPlane)
	return cp, ok
}
