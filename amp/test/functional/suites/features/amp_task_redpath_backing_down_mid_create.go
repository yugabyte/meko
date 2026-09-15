package features

import (
	"context"
	"fmt"
	"time"

	"github.com/yugabyte/amp/test/functional/framework/apps/controller"
	"github.com/yugabyte/amp/test/functional/framework/core"
)

func init() {
	core.Register(core.Test{
		Name:        "features/amp_task_redpath_backing_down_mid_create",
		Suite:       "features",
		Category:    core.Functional,
		Env:         core.EnvSpec{Sharing: core.Exclusive, Shape: core.SharedControlPlane, FaultsSharedInfra: true},
		Tags:        []string{"regression"},
		OwningTeam:  core.TeamControlPlane,
		Description: "Cut the controller off from its backing while a create_endpoint task is running, restore it, and assert the task reaches a terminal status rather than wedging, leaves no compute behind if it failed, leaves the bystander cluster serving, and that the same create succeeds on retry.",
		Params:      []string{"functional.redpath", "functional", "env.ampstack", "apps.ybastack", "apps.yba", "apps.ybk8s"},
		Timeout:     90 * time.Minute,
		Run:         runAmpTaskRedpathBackingDown,
	})
}

func runAmpTaskRedpathBackingDown(t *core.T) error {
	return runAmpFeature(t, func(a *ampFeatureRun) []phase {
		return []phase{{"redpath_backing_down_mid_create", a.redpathBackingDownMidCreate}}
	})
}

func (a *ampFeatureRun) redpathBackingDownMidCreate(ctx context.Context) error {
	r, err := a.newRedpath(ctx)
	if err != nil {
		return err
	}
	task, taskLoc, err := r.cl.CreateEndpoint(ctx, r.target.poolID, r.target.projID, r.target.branchID,
		controller.CreateEndpointRequest{Role: roleReadWrite})
	if err != nil {
		return fmt.Errorf("plan the create the fault interrupts: %w", err)
	}
	taskRef := controller.TaskRef(task, taskLoc)
	if err := r.waitTaskRunning(ctx, taskRef); err != nil {
		return err
	}

	if r.cfg.Inject {
		cut, err := r.cutControllerFromBacking(ctx)
		if err != nil {
			return core.AsEnvFailure(fmt.Errorf("cut the controller off from the backing: %w", err))
		}
		a.f.register("heal the controller-to-backing cut", cut.teardown)
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-time.After(r.cfg.FaultHold):
		}
		if err := cut.heal(ctx); err != nil {
			return core.AsEnvFailure(fmt.Errorf("restore the controller's route to the backing: %w", err))
		}
	}

	done, err := r.waitTaskTerminal(ctx, taskRef)
	if err != nil {
		return err
	}
	r.logf("create_endpoint task %s settled %q after the backing came back", taskRef, done.Status)
	if done.Status == controller.TaskFailed {
		if err := r.noOrphans(ctx, r.target.branchID, r.cfg.OrphanTimeout); err != nil {
			return err
		}
	}
	if err := r.stillServing(ctx); err != nil {
		return err
	}
	return r.retrySucceeds(ctx, "create_endpoint", func(ctx context.Context) error {
		return r.writerServes(ctx, controller.CreateEndpointRequest{Role: roleReadWrite})
	})
}

func (r *redpathRun) waitTaskRunning(ctx context.Context, taskID string) error {
	return r.a.f.t.WaitFor("the create task to start running", r.cfg.TaskStartTimeout, 2*time.Second, func() (bool, error) {
		t, err := r.cl.GetTask(ctx, taskID)
		if err != nil {
			return false, nil
		}
		return t.Status != controller.TaskPending, nil
	})
}

func (r *redpathRun) waitTaskTerminal(ctx context.Context, taskID string) (controller.Task, error) {
	var last controller.Task
	start := time.Now()
	err := r.a.f.t.WaitFor("the create task to settle", r.cfg.WedgeDeadline, 10*time.Second, func() (bool, error) {
		t, err := r.cl.GetTask(ctx, taskID)
		if err != nil {
			return false, nil
		}
		last = t
		return t.Status == controller.TaskSucceeded || t.Status == controller.TaskFailed, nil
	})
	r.a.f.t.Metric("task_settle_seconds", time.Since(start).Seconds(), "s")
	if err != nil && last.Status == "" {
		return last, core.AsEnvFailure(fmt.Errorf("create_endpoint task %s never reported a status within %s, so whether it settled was never observed: %w", taskID, r.cfg.WedgeDeadline, err))
	}
	if err != nil {
		return last, fmt.Errorf("create_endpoint task %s was still %q %s after the backing came back: a task that lost its backing must finish or fail, not sit non-terminal, or it blocks every later roll on this control plane (meko#1007): %w",
			taskID, last.Status, r.cfg.WedgeDeadline, err)
	}
	return last, nil
}
