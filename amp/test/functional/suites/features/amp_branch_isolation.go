package features

import (
	"context"
	"fmt"

	"github.com/yugabyte/amp/test/functional/framework/apps/ampstack"
	"github.com/yugabyte/amp/test/functional/framework/apps/controller"
	"github.com/yugabyte/amp/test/functional/framework/apps/pg"
	"github.com/yugabyte/amp/test/functional/framework/core"
	"github.com/yugabyte/amp/test/functional/framework/exec/k8s"
)

// isolationTable is the table the branch-isolation test seeds on the base branch
// and reads across the fork.
const isolationTable = "amp_iso"

func init() {
	core.Register(core.Test{
		Name:        "features/amp_branch_isolation",
		Suite:       "features",
		Category:    core.Stress,
		Env:         core.EnvSpec{Sharing: core.Exclusive, Shape: core.SharedControlPlane},
		Tags:        []string{"regression"},
		OwningTeam:  core.TeamControlPlane,
		Description: "Prove copy-on-write branch isolation: a forked child sees base data but writes diverge on each side.",
		Params:      []string{"functional", "env.ampstack", "apps.ybastack", "apps.yba", "apps.ybk8s"},
		Run:         runAmpBranchIsolation,
	})
}

// runAmpBranchIsolation proves amp's headline branching guarantee — copy-on-write
// isolation. On a stood-up amp control plane it seeds a known row through an
// endpoint on the cluster's default (base) branch, forks a CHILD branch off it,
// and asserts: the child SEES the base's pre-branch data (copy-on-write from the
// fork point) AND writes on either side are invisible to the other (the branches
// diverge — a child write never appears on the base, and a base write made after
// the fork never appears on the child).
func runAmpBranchIsolation(t *core.T) error {
	return runAmpFeature(t, func(a *ampFeatureRun) []phase {
		return []phase{{"branch_isolation", a.branchIsolation}}
	})
}

// branchIsolation is the live feature phase: it wires the real endpoint connector
// (endpoint address → pg.ConnInfo → pooled pgx) into the seam-pure assertion
// logic.
func (a *ampFeatureRun) branchIsolation(ctx context.Context) error {
	cl, err := a.f.controllerClient(ctx)
	if err != nil {
		return err
	}
	connect := a.f.endpointSQL
	return branchIsolationSteps(ctx, cl, a.proj, "child-"+a.f.suffix(), connect)
}

// branchIsolationSteps drives the copy-on-write assertions over the ampCtl +
// endpoint-connector seams, so the whole feature (fork, divergent writes,
// bidirectional isolation) is unit-testable with fakes.
func branchIsolationSteps(ctx context.Context, cl ampCtl, proj ampCluster, childName string, connect endpointConnector) error {
	// Seed a known row on the base (default) branch.
	baseEP, err := createEndpoint(ctx, cl, proj.poolID, proj.projID, proj.branchID)
	if err != nil {
		return fmt.Errorf("create base endpoint: %w", err)
	}
	base, err := connect(ctx, baseEP)
	if err != nil {
		return fmt.Errorf("connect base endpoint: %w", err)
	}
	defer base.Close()
	if _, err := base.Exec(ctx, "CREATE TABLE "+isolationTable+" (id int primary key, val text)"); err != nil {
		return fmt.Errorf("create table on base: %w", err)
	}
	if _, err := base.Exec(ctx, "INSERT INTO "+isolationTable+" (id, val) VALUES (1, 'base-seed')"); err != nil {
		return fmt.Errorf("seed base row: %w", err)
	}

	// Fork a child branch off the base at its latest LSN (copy-on-write).
	if _, err := cl.CreateBranchAndWait(ctx, proj.poolID, proj.projID, controller.CreateBranchRequest{
		Name:           childName,
		ParentBranchID: proj.branchID,
	}); err != nil {
		return fmt.Errorf("create child branch: %w", err)
	}
	child, err := findBranchCtl(ctx, cl, proj.poolID, proj.projID, childName)
	if err != nil {
		return err
	}
	childEP, err := createEndpoint(ctx, cl, proj.poolID, proj.projID, child.BranchID)
	if err != nil {
		return fmt.Errorf("create child endpoint: %w", err)
	}
	kid, err := connect(ctx, childEP)
	if err != nil {
		return fmt.Errorf("connect child endpoint: %w", err)
	}
	defer kid.Close()

	// The child sees the base's pre-branch data (copy-on-write from the fork).
	if n, err := kid.QueryInt(ctx, "SELECT count(*) FROM "+isolationTable+" WHERE val = 'base-seed'"); err != nil {
		return fmt.Errorf("read base-seed row on child: %w", err)
	} else if n != 1 {
		return fmt.Errorf("copy-on-write broken: child does not see the base's pre-branch row (count=%d, want 1)", n)
	}

	// Diverge: a write on each side, made AFTER the fork.
	if _, err := kid.Exec(ctx, "INSERT INTO "+isolationTable+" (id, val) VALUES (2, 'child-only')"); err != nil {
		return fmt.Errorf("write child-only row: %w", err)
	}
	if _, err := base.Exec(ctx, "INSERT INTO "+isolationTable+" (id, val) VALUES (3, 'base-after')"); err != nil {
		return fmt.Errorf("write base-after row: %w", err)
	}

	// Isolation, both directions.
	if n, err := base.QueryInt(ctx, "SELECT count(*) FROM "+isolationTable+" WHERE val = 'child-only'"); err != nil {
		return fmt.Errorf("check child write on base: %w", err)
	} else if n != 0 {
		return fmt.Errorf("branch isolation broken: the child's write leaked to the base (count=%d, want 0)", n)
	}
	if n, err := kid.QueryInt(ctx, "SELECT count(*) FROM "+isolationTable+" WHERE val = 'base-after'"); err != nil {
		return fmt.Errorf("check base post-fork write on child: %w", err)
	} else if n != 0 {
		return fmt.Errorf("branch isolation broken: the base's post-fork write is visible on the child (count=%d, want 0)", n)
	}
	if n, err := base.QueryInt(ctx, "SELECT count(*) FROM "+isolationTable+" WHERE val = 'base-seed'"); err != nil {
		return fmt.Errorf("re-read base-seed row on base: %w", err)
	} else if n != 1 {
		return fmt.Errorf("base lost its seed row (count=%d, want 1)", n)
	}
	return nil
}

// endpointSQL opens a pooled SQL connection to an amp endpoint with the
// configured cloud_admin/postgres credentials, waiting for YSQL readiness first.
//
// A self-provisioned endpoint is reached through a kubectl port-forward of its
// Service, the same way the sharedk8s provider reaches one. The controller
// reports a cluster-internal address, and on a cluster whose pod range is not
// routable from the coordinator (amp-dev's is shadowed by an unrelated tailnet
// route) dialling it does not refuse — it times out, burning the whole readiness
// budget before failing. Only a run with no kubectl at all falls back to the
// address as given.
func (f *flow) endpointSQL(ctx context.Context, ep controller.Endpoint) (sqlConn, error) {
	if f.ampEnv != nil {
		return f.ampEnv.ConnectEndpoint(ctx, ep)
	}
	ci, err := f.endpointConnInfo(ep)
	if err != nil {
		return nil, err
	}
	kube := f.endpointKube()
	if kube == nil {
		return connectEndpoint(ctx, ci)
	}
	// kubectl port-forward exits the instant the Service has no ready backing
	// pod, so the workload is waited on first: a start-up race becomes a bounded
	// wait, and a compute that never comes up a clear failure.
	if err := ampstack.WaitEndpointWorkloadReady(ctx, kube, ep.ID, endpointReadyTimeout); err != nil {
		return nil, fmt.Errorf("endpoint %s compute never became ready: %w", ep.ID, err)
	}
	pf, err := kube.PortForward(ctx, "svc/"+ampstack.EndpointWorkloadName(ep.ID), 0, ci.Port)
	if err != nil {
		return nil, fmt.Errorf("port-forward endpoint %s: %w", ep.ID, err)
	}
	ci.Host, ci.Port = "127.0.0.1", pf.LocalPort()
	conn, err := connectEndpoint(ctx, ci)
	if err != nil {
		_ = pf.Close()
		return nil, err
	}
	return forwardedConn{sqlConn: conn, pf: pf}, nil
}

// endpointKube is kubectl in the namespace the amp stack this run deployed lives
// in. It is taken from the stack itself rather than from the flow, because a
// test that reinstalls the control plane moves it to a new namespace.
func (f *flow) endpointKube() *k8s.Kubectl {
	if f.ampApp != nil {
		return f.ampApp.Kube()
	}
	return f.ampKube()
}

// forwardedConn ties a connection to the port-forward carrying it, so closing
// the connection closes the forward. Every caller already defers Close; a
// forward left behind would outlive the endpoint it points at.
type forwardedConn struct {
	sqlConn
	pf *k8s.PortForward
}

func (c forwardedConn) Close() {
	c.sqlConn.Close()
	_ = c.pf.Close()
}

// connectEndpoint waits for YSQL readiness then opens a pg client (returned as the
// sqlConn seam).
func connectEndpoint(ctx context.Context, ci pg.ConnInfo) (sqlConn, error) {
	return connectPG(ctx, ci)
}
