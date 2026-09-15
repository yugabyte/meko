package features

import (
	"context"
	"fmt"
	"net/http"
	"time"

	"github.com/yugabyte/amp/test/functional/framework/apps/controller"
	"github.com/yugabyte/amp/test/functional/framework/core"
)

func init() {
	core.Register(core.Test{
		Name:        "features/amp_task_redpath_duplicate_names",
		Suite:       "features",
		Category:    core.Functional,
		Env:         core.EnvSpec{Sharing: core.Exclusive, Shape: core.SharedControlPlane},
		Tags:        []string{"regression"},
		OwningTeam:  core.TeamControlPlane,
		Description: "Create a cluster, then a branch, twice under one name: each duplicate must be refused, synchronously with a 409 or through a task that fails naming the clash, exactly one row of each must exist afterwards, the bystander cluster must keep serving, and the same create under a free name must succeed.",
		Params:      []string{"functional.redpath", "functional", "env.ampstack", "apps.ybastack", "apps.yba", "apps.ybk8s"},
		Timeout:     60 * time.Minute,
		Run:         runAmpTaskRedpathDuplicateNames,
	})
}

func runAmpTaskRedpathDuplicateNames(t *core.T) error {
	return runAmpFeature(t, func(a *ampFeatureRun) []phase {
		return []phase{{"redpath_duplicate_names", a.redpathDuplicateNames}}
	})
}

func (a *ampFeatureRun) redpathDuplicateNames(ctx context.Context) error {
	r, err := a.newRedpath(ctx)
	if err != nil {
		return err
	}
	taken, err := findClusterCtl(ctx, r.cl, a.proj.poolID, "rp-"+a.f.suffix())
	if err != nil {
		return err
	}
	name := taken.Name
	if !r.cfg.Inject {
		name = "rp-free-" + a.f.suffix()
	}
	task, taskLoc, dupErr := r.cl.CreateCluster(ctx, a.proj.poolID, controller.CreateClusterRequest{Name: name, PgVersion: a.f.cfg.PgVersion})
	if err := r.assertDuplicateRefused(ctx, task, taskLoc, dupErr, "cluster", name); err != nil {
		return err
	}
	n := countClusters(ctx, r.cl, a.proj.poolID, name)
	if n < 0 {
		return core.AsEnvFailure(fmt.Errorf("the cluster list could not be read back after the duplicate create of %s", name))
	}
	if n != 1 {
		return fmt.Errorf("%d clusters named %s exist (failed ones included) after the duplicate create, want 1: a duplicate must leave nothing half-created", n, name)
	}

	conn, err := a.f.endpointSQL(ctx, r.bystander)
	if err != nil {
		return fmt.Errorf("connect the writer the fork point comes from: %w", err)
	}
	lsn, err := currentLSN(ctx, conn)
	conn.Close()
	if err != nil {
		return err
	}
	branch := "rp-dup-" + a.f.suffix()
	req := controller.CreateBranchRequest{Name: branch, ParentBranchID: a.proj.branchID, ForkPointLSN: lsn}
	if err := createBranchRetrying(ctx, r.cl, a.proj.poolID, a.proj.projID, req); err != nil {
		return fmt.Errorf("first fork %s: %w", branch, err)
	}
	second := req
	if !r.cfg.Inject {
		second.Name = "rp-dup-free-" + a.f.suffix()
	}
	btask, btaskLoc, dupBranchErr := r.cl.CreateBranch(ctx, a.proj.poolID, a.proj.projID, second)
	if err := r.assertDuplicateRefused(ctx, btask, btaskLoc, dupBranchErr, "branch", second.Name); err != nil {
		return err
	}
	if n := countBranches(ctx, r.cl, a.proj, second.Name); n != 1 {
		return fmt.Errorf("%d branches named %s exist on cluster %s after the duplicate create, want 1", n, second.Name, a.proj.projID)
	}
	if err := r.stillServing(ctx); err != nil {
		return err
	}
	return r.retrySucceeds(ctx, "create a cluster under a free name", func(ctx context.Context) error {
		free := "rp-after-" + a.f.suffix()
		if _, err := r.cl.CreateClusterAndWait(ctx, a.proj.poolID, controller.CreateClusterRequest{Name: free, PgVersion: a.f.cfg.PgVersion}); err != nil {
			return err
		}
		cl, err := findClusterCtl(ctx, r.cl, a.proj.poolID, free)
		if err != nil {
			return err
		}
		r.a.f.register("delete the redpath retry cluster", func(ctx context.Context) error {
			_, e := r.cl.DeleteClusterAndWait(ctx, a.proj.poolID, cl.ID)
			if apiErrorStatus(e) == http.StatusNotFound {
				return nil
			}
			return e
		})
		return nil
	})
}

var duplicateNameReasons = []string{"conflict", "already exists", "duplicate"}

func (r *redpathRun) assertDuplicateRefused(ctx context.Context, task controller.Task, loc string, err error, kind, name string) error {
	if err != nil {
		if status := apiErrorStatus(err); status != http.StatusConflict {
			return fmt.Errorf("a duplicate %s name returned HTTP %d (%v), want 409: a caller has to be able to tell a name clash from a request that is wrong or a dependency that is away", kind, status, err)
		}
		return nil
	}
	what := fmt.Sprintf("the duplicate %s create for %s", kind, name)
	done, werr := r.taskFailedWithin(ctx, task, loc, redpathJoinBudget, what)
	if werr != nil {
		return werr
	}
	return taskErrorNames(done, what, duplicateNameReasons)
}
