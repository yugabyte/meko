package features

import (
	"context"
	"fmt"

	"github.com/yugabyte/amp/test/functional/framework/apps/controller"
	"github.com/yugabyte/amp/test/functional/framework/core"
)

const forkLSNTable = "amp_fork_lsn"

func init() {
	core.Register(core.Test{
		Name:        "features/amp_fork_at_lsn",
		Suite:       "features",
		Category:    core.Stress,
		Env:         core.EnvSpec{Sharing: core.Exclusive, Shape: core.SharedControlPlane},
		Tags:        []string{"regression", "nightly"},
		OwningTeam:  core.TeamControlPlane,
		Description: "Fork a branch at a captured LSN and assert the child sees data committed at or before that LSN but not data written after it.",
		Params:      []string{"functional", "env.ampstack", "apps.ybastack", "apps.yba", "apps.ybk8s"},
		Run:         runAmpForkAtLSN,
	})
}

func runAmpForkAtLSN(t *core.T) error {
	return runAmpFeature(t, func(a *ampFeatureRun) []phase {
		return []phase{{"fork_at_lsn", a.forkAtLSN}}
	})
}

func (a *ampFeatureRun) forkAtLSN(ctx context.Context) error {
	cl, err := a.f.controllerClient(ctx)
	if err != nil {
		return err
	}
	return forkAtLSNSteps(ctx, cl, a.proj, "fork-lsn-"+a.f.suffix(), a.f.endpointSQL)
}

func forkAtLSNSteps(ctx context.Context, cl ampCtl, proj ampProject, childName string, connect endpointConnector) error {
	baseEP, err := createEndpoint(ctx, cl, proj.poolID, proj.projID, proj.branchID)
	if err != nil {
		return fmt.Errorf("create base endpoint: %w", err)
	}
	base, err := connect(ctx, baseEP)
	if err != nil {
		return fmt.Errorf("connect base endpoint: %w", err)
	}
	defer base.Close()

	if _, err := base.Exec(ctx, "CREATE TABLE "+forkLSNTable+" (id int primary key, val text)"); err != nil {
		return fmt.Errorf("create table: %w", err)
	}
	if _, err := base.Exec(ctx, "INSERT INTO "+forkLSNTable+" (id, val) VALUES (1, 'before-lsn')"); err != nil {
		return fmt.Errorf("write pre-fork row: %w", err)
	}

	// Capture the WAL position AFTER the pre-fork row commits and BEFORE the
	// post-fork row: everything <= this LSN must be on the child, nothing after.
	forkLSN, err := base.QueryString(ctx, "SELECT pg_current_wal_lsn()")
	if err != nil {
		return fmt.Errorf("capture fork LSN: %w", err)
	}
	if forkLSN == "" {
		return fmt.Errorf("pg_current_wal_lsn() returned empty")
	}

	if _, err := base.Exec(ctx, "INSERT INTO "+forkLSNTable+" (id, val) VALUES (2, 'after-lsn')"); err != nil {
		return fmt.Errorf("write post-fork row: %w", err)
	}

	if _, err := cl.CreateBranchAndWait(ctx, proj.poolID, proj.projID, controller.CreateBranchRequest{
		Name:           childName,
		ParentBranchID: proj.branchID,
		ForkPointLSN:   forkLSN,
	}); err != nil {
		return fmt.Errorf("fork branch at LSN %s: %w", forkLSN, err)
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

	if n, err := kid.QueryInt(ctx, "SELECT count(*) FROM "+forkLSNTable+" WHERE val = 'before-lsn'"); err != nil {
		return fmt.Errorf("read pre-fork row on child: %w", err)
	} else if n != 1 {
		return fmt.Errorf("LSN fork lost data committed before the fork LSN (before-lsn count=%d, want 1)", n)
	}
	if n, err := kid.QueryInt(ctx, "SELECT count(*) FROM "+forkLSNTable+" WHERE val = 'after-lsn'"); err != nil {
		return fmt.Errorf("read post-fork row on child: %w", err)
	} else if n != 0 {
		return fmt.Errorf("LSN fork included data written after the fork LSN (after-lsn count=%d, want 0)", n)
	}
	if n, err := base.QueryInt(ctx, "SELECT count(*) FROM "+forkLSNTable); err != nil {
		return fmt.Errorf("re-read base: %w", err)
	} else if n != 2 {
		return fmt.Errorf("base lost rows after the fork (count=%d, want 2)", n)
	}
	return nil
}
