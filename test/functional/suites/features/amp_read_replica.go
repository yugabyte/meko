package features

import (
	"context"
	"fmt"
	"strings"
	"time"

	"github.com/yugabyte/amp/test/functional/framework/apps/controller"
	"github.com/yugabyte/amp/test/functional/framework/core"
)

const replicaTable = "amp_replica"
const roleReadOnly = "read_only"

func init() {
	core.Register(core.Test{
		Name:        "features/amp_read_replica",
		Suite:       "features",
		Category:    core.Stress,
		Env:         core.EnvSpec{Sharing: core.Exclusive, Shape: core.SharedControlPlane},
		Tags:        []string{"regression", "nightly"},
		OwningTeam:  core.TeamControlPlane,
		Description: "Create a read_only endpoint alongside the read_write one; assert it serves writes made on the primary and rejects writes itself.",
		Params:      []string{"functional", "env.ampstack", "apps.ybastack", "apps.yba", "apps.ybk8s"},
		Run:         runAmpReadReplica,
	})
}

func runAmpReadReplica(t *core.T) error {
	return runAmpFeature(t, func(a *ampFeatureRun) []phase {
		return []phase{{"read_replica", a.readReplica}}
	})
}

func (a *ampFeatureRun) readReplica(ctx context.Context) error {
	cl, err := a.f.controllerClient(ctx)
	if err != nil {
		return err
	}
	return readReplicaSteps(ctx, cl, a.proj, a.f.endpointSQL)
}

func readReplicaSteps(ctx context.Context, cl ampCtl, proj ampProject, connect endpointConnector) error {
	rwEP, err := createRoleEndpoint(ctx, cl, proj.poolID, proj.projID, proj.branchID, roleReadWrite)
	if err != nil {
		return fmt.Errorf("create read_write endpoint: %w", err)
	}
	rw, err := connect(ctx, rwEP)
	if err != nil {
		return fmt.Errorf("connect read_write endpoint: %w", err)
	}
	defer rw.Close()

	if _, err := rw.Exec(ctx, "CREATE TABLE "+replicaTable+" (id int primary key, val text)"); err != nil {
		return fmt.Errorf("create table on primary: %w", err)
	}
	if _, err := rw.Exec(ctx, "INSERT INTO "+replicaTable+" (id, val) VALUES (1, 'primary-write')"); err != nil {
		return fmt.Errorf("write on primary: %w", err)
	}

	roEP, err := createRoleEndpoint(ctx, cl, proj.poolID, proj.projID, proj.branchID, roleReadOnly)
	if err != nil {
		return fmt.Errorf("create read_only endpoint: %w", err)
	}
	ro, err := connect(ctx, roEP)
	if err != nil {
		return fmt.Errorf("connect read_only endpoint: %w", err)
	}
	defer ro.Close()

	// The replica catches up asynchronously; poll until it serves the write.
	if err := pollUntil(ctx, 90*time.Second, 2*time.Second, func() (bool, error) {
		n, err := ro.QueryInt(ctx, "SELECT count(*) FROM "+replicaTable+" WHERE val = 'primary-write'")
		return n == 1, err
	}); err != nil {
		return fmt.Errorf("replica did not catch up: the row inserted on the read_write endpoint never became visible on the read_only endpoint within 90s (a read replica must serve the primary's committed writes): %w", err)
	}

	// The replica must refuse writes.
	if _, err := ro.Exec(ctx, "INSERT INTO "+replicaTable+" (id, val) VALUES (2, 'replica-write')"); err == nil {
		return fmt.Errorf("read-only endpoint is not enforcing its role: an INSERT sent to the read_only endpoint succeeded, but a read replica must reject all client writes (expected the INSERT to fail with a read-only error)")
	}
	return nil
}

// createRoleEndpoint creates an endpoint of the given role, converging on the
// controller's non-idempotent create: create_endpoint can fail its helm deploy
// ("services yb-ep-… not found") yet leave a partial endpoint record whose
// deploy finishes moments later. So on a transient we back off and REUSE the
// role's endpoint if it is now present, rather than re-create — mirroring
// sharedk8s.createEndpoint.
func createRoleEndpoint(ctx context.Context, cl ampCtl, poolID, projID, branchID, role string) (controller.Endpoint, error) {
	var lastErr error
	for attempt := 0; attempt <= 5; attempt++ {
		if attempt > 0 {
			select {
			case <-ctx.Done():
				return controller.Endpoint{}, ctx.Err()
			case <-time.After(time.Duration(attempt) * 3 * time.Second):
			}
			if ep, ok := existingRoleEndpoint(ctx, cl, poolID, projID, branchID, role); ok {
				return ep, nil
			}
		}
		if _, err := cl.CreateEndpointAndWait(ctx, poolID, projID, branchID, controller.CreateEndpointRequest{Role: role}); err != nil {
			lastErr = err
			if transientDeploy(err) {
				continue
			}
			return controller.Endpoint{}, err
		}
		if ep, ok := existingRoleEndpoint(ctx, cl, poolID, projID, branchID, role); ok {
			return ep, nil
		}
		return controller.Endpoint{}, fmt.Errorf("create %s endpoint reported success but the branch lists no %s endpoint afterwards", role, role)
	}
	return controller.Endpoint{}, fmt.Errorf("create %s endpoint failed after retries: %w", role, lastErr)
}

func existingRoleEndpoint(ctx context.Context, cl ampCtl, poolID, projID, branchID, role string) (controller.Endpoint, bool) {
	eps, err := cl.ListAllEndpoints(ctx, poolID, projID, branchID)
	if err != nil {
		return controller.Endpoint{}, false
	}
	for i := len(eps) - 1; i >= 0; i-- {
		if eps[i].Role == role {
			return eps[i], true
		}
	}
	return controller.Endpoint{}, false
}

// transientDeploy reports the non-idempotent helm-deploy race ("services
// yb-ep-… not found") that clears once the deploy finishes.
func transientDeploy(err error) bool {
	s := err.Error()
	return strings.Contains(s, "services") && strings.Contains(s, "not found")
}

// pollUntil calls fn every interval until it returns true, the deadline passes,
// or fn errors.
func pollUntil(ctx context.Context, timeout, interval time.Duration, fn func() (bool, error)) error {
	deadline := time.Now().Add(timeout)
	for {
		ok, err := fn()
		if err != nil {
			return err
		}
		if ok {
			return nil
		}
		if time.Now().After(deadline) {
			return fmt.Errorf("condition not met within %s", timeout)
		}
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-time.After(interval):
		}
	}
}
