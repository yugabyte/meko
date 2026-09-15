package features

import (
	"context"
	"fmt"
	"net/http"
	"time"

	"github.com/yugabyte/amp/test/functional/framework/apps/controller"
	"github.com/yugabyte/amp/test/functional/framework/core"
)

const deleteDuringCreateRowWait = 2 * time.Minute

func init() {
	core.Register(core.Test{
		Name:        "features/amp_task_redpath_delete_during_create",
		Suite:       "features",
		Category:    core.Functional,
		Env:         core.EnvSpec{Sharing: core.Exclusive, Shape: core.SharedControlPlane},
		Tags:        []string{"regression"},
		OwningTeam:  core.TeamControlPlane,
		Description: "Delete an endpoint while its create task is still running: assert one of the two wins cleanly, that an endpoint the API says is gone leaves no compute pod behind, that the bystander cluster keeps serving, and that a fresh create on the same branch afterwards succeeds.",
		Params:      []string{"functional.redpath", "functional", "env.ampstack", "apps.ybastack", "apps.yba", "apps.ybk8s"},
		Timeout:     90 * time.Minute,
		Run:         runAmpTaskRedpathDeleteDuringCreate,
	})
}

func runAmpTaskRedpathDeleteDuringCreate(t *core.T) error {
	return runAmpFeature(t, func(a *ampFeatureRun) []phase {
		return []phase{{"redpath_delete_during_create", a.redpathDeleteDuringCreate}}
	})
}

func (a *ampFeatureRun) redpathDeleteDuringCreate(ctx context.Context) error {
	r, err := a.newRedpath(ctx)
	if err != nil {
		return err
	}
	createTask, createLoc, err := r.cl.CreateEndpoint(ctx, r.target.poolID, r.target.projID, r.target.branchID,
		controller.CreateEndpointRequest{Role: roleReadWrite})
	if err != nil {
		return fmt.Errorf("plan the create the delete races: %w", err)
	}
	ep, err := r.awaitEndpointRow(ctx)
	if err != nil {
		return err
	}

	var deleteStatus string
	if r.cfg.Inject {
		deleteTask, deleteLoc, derr := r.cl.DeleteEndpoint(ctx, r.target.poolID, r.target.projID, r.target.branchID, ep.ID)
		if derr != nil {
			if apiErrorStatus(derr) == http.StatusConflict || apiErrorStatus(derr) == http.StatusNotFound {
				r.logf("the delete was refused while the create was running: %v", derr)
			} else {
				return fmt.Errorf("plan the delete of endpoint %s mid-create: %w", ep.ID, derr)
			}
		} else {
			deleteStatus = r.joinTask(ctx, deleteTask, deleteLoc, "delete_endpoint")
		}
	}
	createStatus := r.joinTask(ctx, createTask, createLoc, "create_endpoint")
	r.logf("race on endpoint %s: create=%s delete=%s", ep.ID, createStatus, deleteStatus)

	_, getErr := r.cl.GetEndpoint(ctx, r.target.poolID, r.target.projID, r.target.branchID, ep.ID)
	switch {
	case apiErrorStatus(getErr) == http.StatusNotFound:
		if err := r.noOrphans(ctx, r.target.branchID, r.cfg.OrphanTimeout); err != nil {
			return fmt.Errorf("endpoint %s is gone from the API (create=%s delete=%s) but its compute is not: %w", ep.ID, createStatus, deleteStatus, err)
		}
	case getErr != nil:
		return fmt.Errorf("read endpoint %s back after the race (create=%s delete=%s): %w", ep.ID, createStatus, deleteStatus, getErr)
	default:
		if !r.cfg.Inject && createStatus != controller.TaskSucceeded {
			return fmt.Errorf("with no delete raced at all, create_endpoint ended %q", createStatus)
		}
		live, lerr := r.cl.GetEndpoint(ctx, r.target.poolID, r.target.projID, r.target.branchID, ep.ID)
		if lerr != nil {
			return lerr
		}
		c, err := r.a.f.endpointSQL(ctx, live)
		if err != nil {
			return fmt.Errorf("endpoint %s survived the race (create=%s delete=%s) but does not accept connections, so neither side won cleanly: %w", ep.ID, createStatus, deleteStatus, err)
		}
		defer c.Close()
		if _, err := c.QueryInt(ctx, "SELECT 1"); err != nil {
			return fmt.Errorf("endpoint %s survived the race but does not answer SQL: %w", ep.ID, err)
		}
	}
	if err := r.stillServing(ctx); err != nil {
		return err
	}
	return r.retrySucceeds(ctx, "create_endpoint after the race", func(ctx context.Context) error {
		fresh, err := createEndpoint(ctx, r.cl, r.target.poolID, r.target.projID, r.target.branchID)
		if err != nil {
			return err
		}
		c, err := r.a.f.endpointSQL(ctx, fresh)
		if err != nil {
			return err
		}
		defer c.Close()
		_, err = c.QueryInt(ctx, "SELECT 1")
		return err
	})
}

func (r *redpathRun) awaitEndpointRow(ctx context.Context) (controller.Endpoint, error) {
	var ep controller.Endpoint
	err := r.a.f.t.WaitFor("the create task to write its endpoint row", deleteDuringCreateRowWait, 2*time.Second, func() (bool, error) {
		got, ok := lastEndpoint(ctx, r.cl, r.target.poolID, r.target.projID, r.target.branchID)
		if !ok {
			return false, nil
		}
		ep = got
		return true, nil
	})
	if err != nil {
		return controller.Endpoint{}, fmt.Errorf("no endpoint row appeared within %s of the create being planned, so there is nothing for the delete to race: %w", deleteDuringCreateRowWait, err)
	}
	return ep, nil
}
