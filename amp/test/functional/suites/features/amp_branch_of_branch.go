package features

import (
	"context"
	"fmt"

	"github.com/yugabyte/amp/test/functional/framework/apps/controller"
	"github.com/yugabyte/amp/test/functional/framework/core"
)

const lineageTable = "amp_lineage"

func init() {
	core.Register(core.Test{
		Name:        "features/amp_branch_of_branch",
		Suite:       "features",
		Category:    core.Stress,
		Env:         core.EnvSpec{Sharing: core.Exclusive, Shape: core.SharedControlPlane},
		Tags:        []string{"regression", "nightly"},
		OwningTeam:  core.TeamControlPlane,
		Description: "Fork a child then a grandchild off it; assert copy-on-write lineage down two levels and isolation between the levels.",
		Params:      []string{"functional", "env.ampstack", "apps.ybastack", "apps.yba", "apps.ybk8s"},
		Run:         runAmpBranchOfBranch,
	})
}

func runAmpBranchOfBranch(t *core.T) error {
	return runAmpFeature(t, func(a *ampFeatureRun) []phase {
		return []phase{{"branch_of_branch", a.branchOfBranch}}
	})
}

func (a *ampFeatureRun) branchOfBranch(ctx context.Context) error {
	cl, err := a.f.controllerClient(ctx)
	if err != nil {
		return err
	}
	sfx := a.f.suffix()
	return branchOfBranchSteps(ctx, cl, a.proj, "child-"+sfx, "grandchild-"+sfx, a.f.endpointSQL)
}

// forkChildAt forks name off parentBranchID at forkLSN, creates an endpoint on
// it, and returns a live connection. An explicit LSN keeps the fork point
// deterministic under concurrency (the "latest" auto fork-point is not settled
// immediately after a write).
func forkChildAt(ctx context.Context, cl ampCtl, proj ampProject, parentBranchID, forkLSN, name string, connect endpointConnector) (sqlConn, error) {
	if _, err := cl.CreateBranchAndWait(ctx, proj.poolID, proj.projID, controller.CreateBranchRequest{
		Name:           name,
		ParentBranchID: parentBranchID,
		ForkPointLSN:   forkLSN,
	}); err != nil {
		return nil, fmt.Errorf("fork branch %s: %w", name, err)
	}
	b, err := findBranchCtl(ctx, cl, proj.poolID, proj.projID, name)
	if err != nil {
		return nil, err
	}
	ep, err := createEndpoint(ctx, cl, proj.poolID, proj.projID, b.BranchID)
	if err != nil {
		return nil, fmt.Errorf("create endpoint on %s: %w", name, err)
	}
	conn, err := connect(ctx, ep)
	if err != nil {
		return nil, fmt.Errorf("connect endpoint on %s: %w", name, err)
	}
	return conn, nil
}

func currentLSN(ctx context.Context, c sqlConn) (string, error) {
	lsn, err := c.QueryString(ctx, "SELECT pg_current_wal_lsn()")
	if err != nil {
		return "", fmt.Errorf("capture LSN: %w", err)
	}
	if lsn == "" {
		return "", fmt.Errorf("pg_current_wal_lsn() returned an empty LSN, want a non-empty WAL position to fork the child branch at")
	}
	return lsn, nil
}

func branchOfBranchSteps(ctx context.Context, cl ampCtl, proj ampProject, childName, grandName string, connect endpointConnector) error {
	baseEP, err := createEndpoint(ctx, cl, proj.poolID, proj.projID, proj.branchID)
	if err != nil {
		return fmt.Errorf("create base endpoint: %w", err)
	}
	base, err := connect(ctx, baseEP)
	if err != nil {
		return fmt.Errorf("connect base endpoint: %w", err)
	}
	defer base.Close()

	if _, err := base.Exec(ctx, "CREATE TABLE "+lineageTable+" (id int primary key, val text)"); err != nil {
		return fmt.Errorf("create table: %w", err)
	}
	if _, err := base.Exec(ctx, "INSERT INTO "+lineageTable+" (id, val) VALUES (1, 'base')"); err != nil {
		return fmt.Errorf("seed base: %w", err)
	}
	baseLSN, err := currentLSN(ctx, base)
	if err != nil {
		return err
	}

	child, err := forkChildAt(ctx, cl, proj, proj.branchID, baseLSN, childName, connect)
	if err != nil {
		return err
	}
	defer child.Close()
	if _, err := child.Exec(ctx, "INSERT INTO "+lineageTable+" (id, val) VALUES (2, 'child')"); err != nil {
		return fmt.Errorf("write child row: %w", err)
	}
	childLSN, err := currentLSN(ctx, child)
	if err != nil {
		return err
	}

	childBranch, err := findBranchCtl(ctx, cl, proj.poolID, proj.projID, childName)
	if err != nil {
		return err
	}
	grand, err := forkChildAt(ctx, cl, proj, childBranch.BranchID, childLSN, grandName, connect)
	if err != nil {
		return err
	}
	defer grand.Close()

	// The grandchild inherits base AND child rows (copy-on-write down the chain).
	if n, err := grand.QueryInt(ctx, "SELECT count(*) FROM "+lineageTable+" WHERE val IN ('base','child')"); err != nil {
		return fmt.Errorf("read inherited rows on grandchild: %w", err)
	} else if n != 2 {
		return fmt.Errorf("copy-on-write broken down the fork chain: grandchild sees %d of the 2 base+child rows it should have inherited from both ancestors, want 2", n)
	}

	// A grandchild write is invisible to both ancestors.
	if _, err := grand.Exec(ctx, "INSERT INTO "+lineageTable+" (id, val) VALUES (3, 'grand')"); err != nil {
		return fmt.Errorf("write grandchild row: %w", err)
	}
	if n, err := child.QueryInt(ctx, "SELECT count(*) FROM "+lineageTable+" WHERE val = 'grand'"); err != nil {
		return fmt.Errorf("check grandchild write on child: %w", err)
	} else if n != 0 {
		return fmt.Errorf("branch isolation broken: the grandchild's write is visible on its parent child branch (count=%d, want 0)", n)
	}
	if n, err := base.QueryInt(ctx, "SELECT count(*) FROM "+lineageTable+" WHERE val IN ('child','grand')"); err != nil {
		return fmt.Errorf("check descendant writes on base: %w", err)
	} else if n != 0 {
		return fmt.Errorf("branch isolation broken: writes from the child or grandchild are visible on the base branch (count=%d, want 0)", n)
	}
	return nil
}
