package features

import (
	"context"
	"fmt"
	"time"

	"github.com/yugabyte/yb-amp/test/functional/framework/apps/controller"
	"github.com/yugabyte/yb-amp/test/functional/framework/apps/pg"
	"github.com/yugabyte/yb-amp/test/functional/framework/core"
)

const (
	// lifecycleTable is the table the endpoint-lifecycle test writes and then
	// re-reads through a recreated endpoint.
	lifecycleTable = "amp_life"
	// lifecycleRows is the row count seeded, then asserted to survive the
	// endpoint delete + recreate (proving storage/compute separation).
	lifecycleRows = 50
	// endpointRefusedTimeout bounds the connect attempt to a retired endpoint: a
	// deleted endpoint's compute is gone, so the connect must fail within this
	// budget (a success would mean the endpoint was not actually retired).
	endpointRefusedTimeout = 30 * time.Second
)

func init() {
	core.Register(core.Test{
		Name:        "features/amp_endpoint_lifecycle",
		Suite:       "features",
		Category:    core.Stress,
		Env:         core.EnvSpec{Sharing: core.Exclusive, Shape: core.SharedControlPlane},
		Tags:        []string{"nightly"},
		OwningTeam:  core.TeamControlPlane,
		Description: "Prove storage/compute separation: delete an endpoint and recreate it on the same branch, asserting the dataset survives.",
		Params:      []string{"functional", "env.ampstack", "apps.ybk8s"},
		Run:         runAmpEndpointLifecycle,
	})
}

// runAmpEndpointLifecycle exercises the endpoint lifecycle and proves
// storage/compute separation. It creates an endpoint on the default branch,
// writes a known dataset, deletes the endpoint (asserting it disappears from the
// branch's endpoint list AND a connection to it is refused), then recreates an
// endpoint on the SAME branch and asserts the data is still there — proving the
// data lives in shared YB storage, not in the ephemeral compute.
func runAmpEndpointLifecycle(t *core.T) error {
	return runAmpFeature(t, func(a *ampFeatureRun) []phase {
		return []phase{{"endpoint_lifecycle", a.endpointLifecycle}}
	})
}

// endpointLifecycle is the live feature phase: it wires the real endpoint
// connector and a real "connection refused" probe into the seam-pure lifecycle
// logic.
func (a *ampFeatureRun) endpointLifecycle(ctx context.Context) error {
	cl, err := a.f.controllerClient(ctx)
	if err != nil {
		return err
	}
	return endpointLifecycleSteps(ctx, cl, a.proj, a.f.endpointSQL, a.f.endpointConnectRefused)
}

// endpointLifecycleSteps drives the lifecycle assertions over the ampCtl,
// endpoint-connector, and connection-probe seams, so create → write → delete →
// (gone + refused) → recreate → (data survives) is unit-testable with fakes.
func endpointLifecycleSteps(ctx context.Context, cl ampCtl, proj ampProject, connect endpointConnector, refused endpointProbe) error {
	// Create an endpoint and write a known dataset.
	e1, err := createEndpoint(ctx, cl, proj.poolID, proj.projID, proj.branchID)
	if err != nil {
		return fmt.Errorf("create endpoint: %w", err)
	}
	c1, err := connect(ctx, e1)
	if err != nil {
		return fmt.Errorf("connect endpoint: %w", err)
	}
	if _, err := c1.Exec(ctx, "CREATE TABLE "+lifecycleTable+" (id int primary key)"); err != nil {
		c1.Close()
		return fmt.Errorf("create table: %w", err)
	}
	if _, err := c1.Exec(ctx, "INSERT INTO "+lifecycleTable+" (id) SELECT generate_series(1, $1)", lifecycleRows); err != nil {
		c1.Close()
		return fmt.Errorf("seed rows: %w", err)
	}
	seeded, err := c1.QueryInt(ctx, "SELECT count(*) FROM "+lifecycleTable)
	c1.Close()
	if err != nil {
		return fmt.Errorf("count seeded rows: %w", err)
	}
	if seeded != lifecycleRows {
		return fmt.Errorf("seeded %d rows, expected %d", seeded, lifecycleRows)
	}

	// Delete the endpoint: it must vanish from the list and refuse connections.
	if _, err := cl.DeleteEndpointAndWait(ctx, proj.poolID, proj.projID, proj.branchID, e1.ID); err != nil {
		return fmt.Errorf("delete endpoint: %w", err)
	}
	eps, err := cl.ListAllEndpoints(ctx, proj.poolID, proj.projID, proj.branchID)
	if err != nil {
		return fmt.Errorf("list endpoints after delete: %w", err)
	}
	for _, e := range eps {
		if e.ID == e1.ID {
			return fmt.Errorf("deleted endpoint %s is still listed", e1.ID)
		}
	}
	if err := refused(ctx, e1); err == nil {
		return fmt.Errorf("expected the retired endpoint to refuse connections, but it accepted one")
	}

	// Recreate an endpoint on the SAME branch: the data survives (shared storage).
	e2, err := createEndpoint(ctx, cl, proj.poolID, proj.projID, proj.branchID)
	if err != nil {
		return fmt.Errorf("recreate endpoint: %w", err)
	}
	c2, err := connect(ctx, e2)
	if err != nil {
		return fmt.Errorf("connect recreated endpoint: %w", err)
	}
	defer c2.Close()
	survived, err := c2.QueryInt(ctx, "SELECT count(*) FROM "+lifecycleTable)
	if err != nil {
		return fmt.Errorf("count rows on recreated endpoint: %w", err)
	}
	if survived != seeded {
		return fmt.Errorf("storage/compute separation violated: %d rows before delete, %d after recreate", seeded, survived)
	}
	return nil
}

// endpointProbe attempts a connection to an endpoint and returns the connect
// error (nil on a successful connect). The lifecycle test uses it to assert a
// retired endpoint refuses connections.
type endpointProbe func(ctx context.Context, ep controller.Endpoint) error

// endpointConnectRefused attempts one bounded connection to an endpoint's
// address, returning the connect error (nil if it unexpectedly succeeded). A
// retired endpoint's compute pod is gone, so the connect fails (refused / no
// route) within endpointRefusedTimeout.
func (f *flow) endpointConnectRefused(ctx context.Context, ep controller.Endpoint) error {
	cctx, cancel := context.WithTimeout(ctx, endpointRefusedTimeout)
	defer cancel()
	if f.ampEnv != nil {
		// Attach mode: reaching the endpoint goes through the provider's Service
		// port-forward; a retired endpoint's Service is gone, so ConnectEndpoint
		// fails — which is the "refused" the caller asserts on. A success means the
		// endpoint was not actually retired.
		c, err := f.ampEnv.ConnectEndpoint(cctx, ep)
		if err != nil {
			return err
		}
		c.Close()
		return nil
	}
	ci, err := f.endpointConnInfo(ep)
	if err != nil {
		return err
	}
	c, err := pg.Connect(cctx, ci)
	if err != nil {
		return err
	}
	c.Close()
	return nil
}
