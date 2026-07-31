package features

import (
	"context"
	"fmt"

	"github.com/yugabyte/amp/test/functional/framework/apps/controller"
	"github.com/yugabyte/amp/test/functional/framework/core"
)

const branchDeleteTable = "amp_branch_del"

func init() {
	core.Register(core.Test{
		Name:        "features/amp_branch_delete",
		Suite:       "features",
		Category:    core.Stress,
		Env:         core.EnvSpec{Sharing: core.Exclusive, Shape: core.SharedControlPlane},
		Tags:        []string{"regression", "nightly"},
		OwningTeam:  core.TeamControlPlane,
		Description: "Delete a forked branch (endpoint first, then branch) and assert it is gone from the list, the parent's data and endpoint are untouched, and no task is left failed.",
		Params:      []string{"functional", "env.ampstack", "apps.ybastack", "apps.yba", "apps.ybk8s"},
		Run:         runAmpBranchDelete,
	})
}

func runAmpBranchDelete(t *core.T) error {
	return runAmpFeature(t, func(a *ampFeatureRun) []phase {
		return []phase{{"branch_delete", a.branchDelete}}
	})
}

func (a *ampFeatureRun) branchDelete(ctx context.Context) error {
	cl, err := a.f.controllerClient(ctx)
	if err != nil {
		return err
	}
	return branchDeleteSteps(ctx, cl, a.proj, "branch-del-"+a.f.suffix(), a.f.endpointSQL)
}

func branchDeleteSteps(ctx context.Context, cl ampCtl, proj ampProject, childName string, connect endpointConnector) error {
	baseEP, err := createEndpoint(ctx, cl, proj.poolID, proj.projID, proj.branchID)
	if err != nil {
		return fmt.Errorf("create base endpoint: %w", err)
	}
	base, err := connect(ctx, baseEP)
	if err != nil {
		return fmt.Errorf("connect base endpoint: %w", err)
	}
	defer base.Close()

	if _, err := base.Exec(ctx, "CREATE TABLE "+branchDeleteTable+" (id int primary key, val text)"); err != nil {
		return fmt.Errorf("create table: %w", err)
	}
	if _, err := base.Exec(ctx, "INSERT INTO "+branchDeleteTable+" (id, val) VALUES (1, 'parent')"); err != nil {
		return fmt.Errorf("seed parent: %w", err)
	}
	forkLSN, err := currentLSN(ctx, base)
	if err != nil {
		return err
	}

	child, err := forkChildAt(ctx, cl, proj, proj.branchID, forkLSN, childName, connect)
	if err != nil {
		return err
	}
	if _, err := child.Exec(ctx, "INSERT INTO "+branchDeleteTable+" (id, val) VALUES (2, 'child')"); err != nil {
		child.Close()
		return fmt.Errorf("write child marker: %w", err)
	}
	if n, err := child.QueryInt(ctx, "SELECT count(*) FROM "+branchDeleteTable); err != nil {
		child.Close()
		return fmt.Errorf("read child: %w", err)
	} else if n != 2 {
		child.Close()
		return fmt.Errorf("child branch has %d rows before its endpoint+branch delete, want 2 (the inherited parent row plus the child's own marker)", n)
	}
	child.Close()

	// Delete endpoint(s) first, then the branch.
	childBranch, err := findBranchCtl(ctx, cl, proj.poolID, proj.projID, childName)
	if err != nil {
		return err
	}
	eps, err := cl.ListAllEndpoints(ctx, proj.poolID, proj.projID, childBranch.BranchID)
	if err != nil {
		return fmt.Errorf("list child endpoints: %w", err)
	}
	for _, ep := range eps {
		if _, err := cl.DeleteEndpointAndWait(ctx, proj.poolID, proj.projID, childBranch.BranchID, ep.ID); err != nil {
			return fmt.Errorf("delete child endpoint %s: %w", ep.ID, err)
		}
	}
	if _, err := cl.DeleteBranchAndWait(ctx, proj.poolID, proj.projID, childBranch.BranchID); err != nil {
		return fmt.Errorf("delete branch %s: %w", childBranch.BranchID, err)
	}

	// The branch is gone from the list.
	gone, err := branchAbsent(ctx, cl, proj.poolID, proj.projID, childBranch.BranchID)
	if err != nil {
		return err
	}
	if !gone {
		return fmt.Errorf("deleted branch %s is still listed", childBranch.BranchID)
	}

	// The parent's data is intact and its endpoint still serves reads+writes.
	if n, err := base.QueryInt(ctx, "SELECT count(*) FROM "+branchDeleteTable+" WHERE val = 'parent'"); err != nil {
		return fmt.Errorf("re-read parent: %w", err)
	} else if n != 1 {
		return fmt.Errorf("parent row damaged after branch delete (count=%d, want 1)", n)
	}
	if n, err := base.QueryInt(ctx, "SELECT count(*) FROM "+branchDeleteTable+" WHERE val = 'child'"); err != nil {
		return fmt.Errorf("check child leak on parent: %w", err)
	} else if n != 0 {
		return fmt.Errorf("child marker leaked to parent (count=%d, want 0)", n)
	}
	if _, err := base.Exec(ctx, "INSERT INTO "+branchDeleteTable+" (id, val) VALUES (3, 'post-delete')"); err != nil {
		return fmt.Errorf("parent write after branch delete: %w", err)
	}

	// No task in the project ended up failed.
	tasks, err := cl.ListTasks(ctx, controller.ListTasksOptions{Status: controller.TaskFailed, ProjectID: proj.projID, Limit: 100})
	if err != nil {
		return fmt.Errorf("list failed tasks: %w", err)
	}
	if len(tasks.Tasks) != 0 {
		first := tasks.Tasks[0]
		msg := ""
		if first.Error != nil {
			msg = *first.Error
		}
		return fmt.Errorf("%d failed task(s) after branch delete; first: %s (%s): %s", len(tasks.Tasks), first.ID, first.Kind, msg)
	}
	return nil
}

// branchAbsent pages ListBranches and reports whether branchID is gone.
func branchAbsent(ctx context.Context, cl ampCtl, poolID, projID, branchID string) (bool, error) {
	const page = 100
	for offset := 0; ; offset += page {
		list, err := cl.ListBranches(ctx, poolID, projID, page, offset)
		if err != nil {
			return false, fmt.Errorf("list branches: %w", err)
		}
		for _, b := range list.Branches {
			if b.BranchID == branchID {
				return false, nil
			}
		}
		if list.Count < page || offset+len(list.Branches) >= list.Total {
			return true, nil
		}
	}
}
