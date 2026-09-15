package features

import (
	"context"
	"fmt"
	"net/http"
	"time"

	"github.com/yugabyte/amp/test/functional/framework/apps/controller"
	"github.com/yugabyte/amp/test/functional/framework/core"
)

const (
	redpathUnschedulableMemory = "4000Gi"
	redpathUnschedulableCPU    = "500m"
	redpathFeasibleMemory      = "6Gi"
)

var unschedulableReasons = []string{"insufficient", "unschedulable", "failedscheduling", "nodes are available"}

func init() {
	core.Register(core.Test{
		Name:        "features/amp_task_redpath_unschedulable",
		Suite:       "features",
		Category:    core.Functional,
		Env:         core.EnvSpec{Sharing: core.Exclusive, Shape: core.SharedControlPlane},
		Tags:        []string{"regression"},
		OwningTeam:  core.TeamControlPlane,
		Description: "Create an endpoint asking for more memory than any node has: the create task must fail naming the scheduling reason rather than a bare deadline, leave no compute behind, leave the bystander cluster serving, and a feasible create on the same branch afterwards must succeed.",
		Params:      []string{"functional.redpath", "functional", "env.ampstack", "apps.ybastack", "apps.yba", "apps.ybk8s"},
		Timeout:     120 * time.Minute,
		Run:         runAmpTaskRedpathUnschedulable,
	})
}

func runAmpTaskRedpathUnschedulable(t *core.T) error {
	return runAmpFeature(t, func(a *ampFeatureRun) []phase {
		return []phase{{"redpath_unschedulable", a.redpathUnschedulable}}
	})
}

func (a *ampFeatureRun) redpathUnschedulable(ctx context.Context) error {
	r, err := a.newRedpath(ctx)
	if err != nil {
		return err
	}
	memory := redpathUnschedulableMemory
	if !r.cfg.Inject {
		memory = redpathFeasibleMemory
	}
	task, taskLoc, err := r.cl.CreateEndpoint(ctx, r.target.poolID, r.target.projID, r.target.branchID,
		controller.CreateEndpointRequest{
			Role:      roleReadWrite,
			Resources: &controller.EndpointResources{CPU: redpathUnschedulableCPU, Memory: memory},
		})
	if err != nil {
		if apiErrorStatus(err) == http.StatusBadRequest {
			a.f.t.Skip("the controller refuses a " + memory + " endpoint synchronously, so no failing create task can be produced this way: " + err.Error())
			return nil
		}
		return fmt.Errorf("plan the unschedulable create: %w", err)
	}
	done, err := r.taskFailedWithin(ctx, task, taskLoc, redpathCreateDeadline, "create_endpoint for "+memory)
	if err != nil {
		return err
	}
	if err := taskErrorNames(done, "create_endpoint for "+memory, unschedulableReasons); err != nil {
		return err
	}
	if err := r.noOrphans(ctx, r.target.branchID, r.cfg.OrphanTimeout); err != nil {
		return err
	}
	if err := r.stillServing(ctx); err != nil {
		return err
	}
	return r.retrySucceeds(ctx, "a feasible create_endpoint", func(ctx context.Context) error {
		return r.writerServes(ctx, controller.CreateEndpointRequest{
			Role:      roleReadWrite,
			Resources: &controller.EndpointResources{CPU: redpathUnschedulableCPU, Memory: redpathFeasibleMemory},
		})
	})
}
