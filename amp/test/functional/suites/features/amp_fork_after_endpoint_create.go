package features

import (
	"context"
	"fmt"
	"time"

	"github.com/yugabyte/amp/test/functional/framework/apps/controller"
	"github.com/yugabyte/amp/test/functional/framework/core"
)

const forkAfterCreateTable = "amp_fork_after_create"

func init() {
	core.Register(core.Test{
		Name:        "features/amp_fork_after_endpoint_create",
		Suite:       "features",
		Category:    core.Stress,
		Env:         core.EnvSpec{Sharing: core.Exclusive, Shape: core.SharedControlPlane},
		Tags:        []string{"regression", "nightly"},
		OwningTeam:  core.TeamControlPlane,
		Description: "Fork a branch the instant its endpoint's create task reports success, with no wait in between, and assert the controller resolves a fork point instead of rejecting its own just-deployed endpoint.",
		Params:      []string{"functional", "env.ampstack", "apps.ybastack", "apps.yba", "apps.ybk8s"},
		Run:         runAmpForkAfterEndpointCreate,
	})
}

func runAmpForkAfterEndpointCreate(t *core.T) error {
	return runAmpFeature(t, func(a *ampFeatureRun) []phase {
		return []phase{{"fork_after_endpoint_create", a.forkAfterEndpointCreate}}
	})
}

func (a *ampFeatureRun) forkAfterEndpointCreate(ctx context.Context) error {
	cl, err := a.f.controllerClient(ctx)
	if err != nil {
		return err
	}
	return forkAfterEndpointCreateSteps(ctx, cl, a.proj, "fork-fresh-"+a.f.suffix(), a.f.endpointSQL, a.f.logf)
}

// forkAfterEndpointCreateSteps forks a parent whose endpoint was created moments
// ago. The endpoint's own Service is what the controller reads the fork point
// over, and it gains a backend after the pod goes Ready — so a client acting the
// instant create_endpoint succeeds used to get a terminal 400 invalid_request
// (#724). The other fork tests all branch off long-lived parents and never touch
// that window; the only thing that did was perf/multitenant's fork_latency, 70
// minutes into a nightly.
//
// Deliberately no sleep between the create and the fork: the gap IS the test.
func forkAfterEndpointCreateSteps(ctx context.Context, cl ampCtl, proj ampCluster, childName string,
	connect endpointConnector, logf func(string, ...any)) error {
	created := time.Now()
	parentEP, err := createEndpoint(ctx, cl, proj.poolID, proj.projID, proj.branchID)
	if err != nil {
		return fmt.Errorf("create parent endpoint: %w", err)
	}

	// A branch nobody has written to has no determinable fork point even with an
	// endpoint deployed, and would fail this test for an unrelated reason. One row
	// is enough to give it one.
	parent, err := connect(ctx, parentEP)
	if err != nil {
		return fmt.Errorf("connect parent endpoint: %w", err)
	}
	defer parent.Close()
	if _, err := parent.Exec(ctx, "CREATE TABLE "+forkAfterCreateTable+" (id int primary key)"); err != nil {
		return fmt.Errorf("create table: %w", err)
	}
	if _, err := parent.Exec(ctx, "INSERT INTO "+forkAfterCreateTable+" (id) VALUES (1)"); err != nil {
		return fmt.Errorf("seed the fork parent: %w", err)
	}

	elapsed := time.Since(created)
	logf("fork_after_endpoint_create: forking %s at %.1fs after its endpoint create returned",
		proj.branchID, elapsed.Seconds())
	if _, err := cl.CreateBranchAndWait(ctx, proj.poolID, proj.projID, controller.CreateBranchRequest{
		Name:           childName,
		ParentBranchID: proj.branchID,
	}); err != nil {
		return fmt.Errorf("fork %.1fs after the parent's endpoint was created: %w "+
			"(the endpoint is deployed and was just written to, so the fork point should resolve)",
			elapsed.Seconds(), err)
	}

	// A 202 only means the fork point resolved. Read the branch back to confirm it
	// carries the parent and a real fork LSN rather than a silent 0/0.
	child, err := findBranchCtl(ctx, cl, proj.poolID, proj.projID, childName)
	if err != nil {
		return err
	}
	if child.ParentBranchID == nil || *child.ParentBranchID != proj.branchID {
		return fmt.Errorf("forked branch parent_branch_id = %v, want %s", child.ParentBranchID, proj.branchID)
	}
	if child.ForkLSN == nil || *child.ForkLSN == "" || *child.ForkLSN == "0/0" {
		return fmt.Errorf("forked branch fork_lsn = %v, want the parent's flush_lsn", child.ForkLSN)
	}
	return nil
}
