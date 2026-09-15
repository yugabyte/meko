package features

import (
	"context"
	"fmt"
	"net/http"
	"time"

	"github.com/yugabyte/amp/test/functional/framework/apps/controller"
	"github.com/yugabyte/amp/test/functional/framework/core"
)

var quotaReasons = []string{"exceeded quota", "quota", "forbidden"}

func init() {
	core.Register(core.Test{
		Name:        "features/amp_task_redpath_quota",
		Suite:       "features",
		Category:    core.Functional,
		Env:         core.EnvSpec{Sharing: core.Exclusive, Shape: core.SharedControlPlane, FaultsSharedInfra: true},
		Tags:        []string{"regression"},
		OwningTeam:  core.TeamControlPlane,
		Description: "Cap the control-plane namespace at the Deployments it already has, then create an endpoint: the create task must fail naming the quota rather than a deadline, leave no compute behind, leave the bystander cluster serving, and succeed once the quota is lifted.",
		Params:      []string{"functional.redpath", "functional", "env.ampstack", "apps.ybastack", "apps.yba", "apps.ybk8s"},
		Timeout:     120 * time.Minute,
		Run:         runAmpTaskRedpathQuota,
	})
}

func runAmpTaskRedpathQuota(t *core.T) error {
	return runAmpFeature(t, func(a *ampFeatureRun) []phase {
		return []phase{{"redpath_quota", a.redpathQuota}}
	})
}

func (a *ampFeatureRun) redpathQuota(ctx context.Context) error {
	r, err := a.newRedpath(ctx)
	if err != nil {
		return err
	}
	kube := a.f.ampKube()
	if kube == nil {
		a.f.t.Skip("no kubectl for the amp namespace, so no quota can be applied to it")
		return nil
	}

	var quota *resourceQuota
	if r.cfg.Inject {
		if quota, err = applyDeploymentQuota(ctx, kube, a.f.suffix(), r.cfg.QuotaHeadroom); err != nil {
			return err
		}
		a.f.register("remove the redpath Deployment quota", quota.remove)
	}

	task, taskLoc, err := r.cl.CreateEndpoint(ctx, r.target.poolID, r.target.projID, r.target.branchID,
		controller.CreateEndpointRequest{Role: roleReadWrite})
	if err != nil {
		if apiErrorStatus(err) == http.StatusBadRequest || apiErrorStatus(err) == http.StatusConflict {
			r.logf("the create was refused synchronously under the quota: %v", err)
			return r.afterQuotaLifted(ctx, quota)
		}
		return fmt.Errorf("plan the create under the quota: %w", err)
	}
	done, err := r.taskFailedWithin(ctx, task, taskLoc, redpathCreateDeadline, "create_endpoint under a namespace quota")
	if err != nil {
		return err
	}
	if err := taskErrorNames(done, "create_endpoint under a namespace quota", quotaReasons); err != nil {
		return err
	}
	if err := r.noOrphans(ctx, r.target.branchID, r.cfg.OrphanTimeout); err != nil {
		return err
	}
	if err := r.stillServing(ctx); err != nil {
		return err
	}
	return r.afterQuotaLifted(ctx, quota)
}

func (r *redpathRun) afterQuotaLifted(ctx context.Context, quota *resourceQuota) error {
	if quota != nil {
		if err := quota.remove(ctx); err != nil {
			return fmt.Errorf("remove the quota: %w", err)
		}
	}
	return r.retrySucceeds(ctx, "create_endpoint", func(ctx context.Context) error {
		return r.writerServes(ctx, controller.CreateEndpointRequest{Role: roleReadWrite})
	})
}
