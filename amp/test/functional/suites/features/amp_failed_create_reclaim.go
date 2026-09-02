// meko#486: an endpoint whose create failed kept its row, and with it the
// branch's single read_write slot, so the branch could never get a writer again
// without operator SQL. The sweeper now reclaims failed-create endpoints.
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
	// unschedulableMemory passes the API's floor and fits no node.
	unschedulableMemory = "4000Gi"
	unschedulableCPU    = "500m"
)

func init() {
	core.Register(core.Test{
		Name:        "features/amp_failed_create_reclaim",
		Suite:       "features",
		Category:    core.Functional,
		Env:         core.EnvSpec{Sharing: core.Exclusive, Shape: core.SharedControlPlane},
		Tags:        []string{"regression"},
		OwningTeam:  core.TeamControlPlane,
		Description: "Create an endpoint no node can schedule, wait for its create task to fail, then assert the sweeper removes the failed endpoint and the branch accepts a new writer.",
		Params:      []string{"functional.failed_create_reclaim", "env.ampstack", "apps.ybastack", "apps.yba", "apps.ybk8s"},
		// The create fails only once helm gives up: --wait --timeout is the
		// controller's deploy timeout (35m on the QA control plane), and 51m was
		// observed before the task settled.
		Timeout: 120 * time.Minute,
		Run:     runAmpFailedCreateReclaim,
	})
}

func runAmpFailedCreateReclaim(t *core.T) error {
	return runAmpFeature(t, func(a *ampFeatureRun) []phase {
		return []phase{{"failed_create_reclaim", a.failedCreateReclaim}}
	})
}

func (a *ampFeatureRun) failedCreateReclaim(ctx context.Context) error {
	cl, err := a.f.controllerClient(ctx)
	if err != nil {
		return err
	}
	sweep := a.f.t.Options.GetDuration("functional.failed_create_reclaim.sweep_timeout", 45*time.Minute)
	task, _, err := cl.CreateEndpoint(ctx, a.proj.poolID, a.proj.projID, a.proj.branchID, controller.CreateEndpointRequest{
		Role:      roleReadWrite,
		Resources: &controller.EndpointResources{CPU: unschedulableCPU, Memory: unschedulableMemory},
	})
	if err != nil {
		if apiErrorStatus(err) == http.StatusBadRequest {
			a.f.t.Skip("the controller refuses a " + unschedulableMemory + " endpoint synchronously, so no failed create can be produced this way: " + err.Error())
			return nil
		}
		return fmt.Errorf("plan the unschedulable create: %w", err)
	}
	start := time.Now()
	done, err := cl.WaitTask(ctx, task.ID)
	if err != nil && done.Status == "" {
		return fmt.Errorf("wait for create task %s: %w", task.ID, err)
	}
	a.f.logf("create task %s settled %q after %s", task.ID, done.Status, time.Since(start).Round(time.Second))
	a.f.t.Metric("create_failed_after_seconds", time.Since(start).Seconds(), "s")
	if done.Status == controller.TaskSucceeded {
		return fmt.Errorf("create task %s for a %s endpoint succeeded; no node can hold that, so the deploy reported ready without a running compute", task.ID, unschedulableMemory)
	}
	if done.Status != controller.TaskFailed {
		return fmt.Errorf("create task %s settled at %q, want %q", task.ID, done.Status, controller.TaskFailed)
	}

	// Whether a writer can be created while the failed row still exists is
	// worth knowing either way; only the post-sweep create is the contract.
	if _, _, err := cl.CreateEndpoint(ctx, a.proj.poolID, a.proj.projID, a.proj.branchID, controller.CreateEndpointRequest{Role: roleReadWrite}); err != nil {
		a.f.logf("a create while the failed endpoint row still exists is refused: %v", err)
	} else {
		a.f.logf("a create while the failed endpoint row still exists is accepted")
	}
	sweepStart := time.Now()
	err = a.f.t.WaitFor("the failed-create endpoint to be reclaimed", sweep, 15*time.Second, func() (bool, error) {
		eps, err := cl.ListAllEndpoints(ctx, a.proj.poolID, a.proj.projID, a.proj.branchID)
		if err != nil {
			return false, nil
		}
		for _, e := range eps {
			if e.Resources != nil && e.Resources.Memory == unschedulableMemory {
				return false, nil
			}
		}
		return true, nil
	})
	if err != nil {
		eps, _ := cl.ListAllEndpoints(ctx, a.proj.poolID, a.proj.projID, a.proj.branchID)
		return fmt.Errorf("%d endpoint(s) still listed on branch %s %s after create task %s failed; the sweeper must reclaim a failed-create endpoint or the branch never gets a writer again (meko#486): %w", len(eps), a.proj.branchID, sweep, task.ID, err)
	}
	a.f.t.Metric("reclaim_seconds", time.Since(sweepStart).Seconds(), "s")

	var ep controller.Endpoint
	if last, ok := lastEndpoint(ctx, cl, a.proj.poolID, a.proj.projID, a.proj.branchID); ok {
		ep = last
	} else if ep, err = createEndpoint(ctx, cl, a.proj.poolID, a.proj.projID, a.proj.branchID); err != nil {
		return fmt.Errorf("create a writer after the failed one was reclaimed: %w", err)
	}
	c, err := a.f.endpointSQL(ctx, ep)
	if err != nil {
		return fmt.Errorf("connect the replacement endpoint: %w", err)
	}
	defer c.Close()
	if _, err := c.QueryInt(ctx, "SELECT 1"); err != nil {
		return fmt.Errorf("the replacement endpoint does not answer: %w", err)
	}
	return nil
}
