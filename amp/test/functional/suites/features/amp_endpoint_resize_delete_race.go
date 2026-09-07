// The endpoint lock (https://github.com/yugabyte/meko/pull/971) serializes every
// blocking task that touches one endpoint, not only resize against resize. amp_endpoint_resize_lock
// holds the resize-vs-resize 409; this one races a resize against a DELETE of the
// same endpoint, the pair the resize-only unique index never covered.
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
	resizeDeleteRaceTable = "amp_resize_del_race"
	// resizeDeleteRaceSettle bounds how long both tasks may take to reach a
	// terminal status once planned: a resize that redeploys plus a delete queued
	// behind it on the endpoint lock.
	resizeDeleteRaceSettle = 10 * time.Minute
	// resizeDeleteRacePodSettle bounds the wait for the retired compute to leave
	// the cluster; a Terminating pod is not a leak yet.
	resizeDeleteRacePodSettle = 3 * time.Minute
)

// resizeDeleteRaceDelays: how long after the resize is planned the delete goes
// out. Zero races the two claims; the later ones land the delete while the
// resize step holds the endpoint lock mid-redeploy.
var resizeDeleteRaceDelays = []time.Duration{0, 1 * time.Second, 5 * time.Second}

func init() {
	core.Register(core.Test{
		Name:        "features/amp_endpoint_resize_delete_race",
		Suite:       "features",
		Category:    core.Stress,
		Env:         core.EnvSpec{Sharing: core.Exclusive, Shape: core.SharedControlPlane},
		Tags:        []string{"regression"},
		OwningTeam:  core.TeamControlPlane,
		Description: "Race an endpoint resize against a delete of the same endpoint, three timings, and hold the endpoint lock's contract: both tasks reach a terminal status, the delete is never lost, the endpoint is gone from the API with no compute pod left in the cluster, the cluster has no task stuck non-terminal, and the branch's data survives for a fresh endpoint.",
		Params:      []string{"functional", "env.ampstack", "apps.ybastack", "apps.yba", "apps.ybk8s"},
		Timeout:     40 * time.Minute,
		Run:         runAmpEndpointResizeDeleteRace,
	})
}

func runAmpEndpointResizeDeleteRace(t *core.T) error {
	return runAmpFeature(t, func(a *ampFeatureRun) []phase {
		return []phase{{"endpoint_resize_delete_race", a.endpointResizeDeleteRace}}
	})
}

func (a *ampFeatureRun) endpointResizeDeleteRace(ctx context.Context) error {
	cl, err := a.f.controllerClient(ctx)
	if err != nil {
		return err
	}
	proj := a.proj

	seedEP, err := createEndpointReq(ctx, cl, proj.poolID, proj.projID, proj.branchID,
		controller.CreateEndpointRequest{Role: roleReadWrite, Resources: &controller.EndpointResources{CPU: resizeCreateCPU, Memory: resizeCreateMemory}})
	if err != nil {
		return fmt.Errorf("create the seed endpoint: %w", err)
	}
	conn, err := a.f.endpointSQL(ctx, seedEP)
	if err != nil {
		return fmt.Errorf("connect the seed endpoint: %w", err)
	}
	if _, err := conn.Exec(ctx, "CREATE TABLE "+resizeDeleteRaceTable+" (id int primary key, round int)"); err != nil {
		conn.Close()
		return fmt.Errorf("create table: %w", err)
	}
	if _, err := conn.Exec(ctx, "INSERT INTO "+resizeDeleteRaceTable+" VALUES (0, -1)"); err != nil {
		conn.Close()
		return fmt.Errorf("seed: %w", err)
	}
	conn.Close()

	ep := seedEP
	for i, delay := range resizeDeleteRaceDelays {
		if err := a.resizeDeleteRound(ctx, cl, ep, i, delay); err != nil {
			return fmt.Errorf("round %d (delete fired %s after the resize): %w", i, delay, err)
		}
		// A fresh endpoint on the same branch proves the storage under the raced
		// endpoint is intact, and is the next round's victim.
		ep, err = createEndpointReq(ctx, cl, proj.poolID, proj.projID, proj.branchID,
			controller.CreateEndpointRequest{Role: roleReadWrite, Resources: &controller.EndpointResources{CPU: resizeCreateCPU, Memory: resizeCreateMemory}})
		if err != nil {
			return fmt.Errorf("round %d: create a fresh endpoint on the branch after the race: %w", i, err)
		}
		conn, err := a.f.endpointSQL(ctx, ep)
		if err != nil {
			return fmt.Errorf("round %d: connect the fresh endpoint: %w", i, err)
		}
		n, err := conn.QueryInt(ctx, "SELECT count(*) FROM "+resizeDeleteRaceTable)
		if err == nil && n != int64(i+1) {
			err = fmt.Errorf("%s has %d rows on the fresh endpoint, want %d: a row committed before the race is missing", resizeDeleteRaceTable, n, i+1)
		}
		if err == nil {
			_, err = conn.Exec(ctx, "INSERT INTO "+resizeDeleteRaceTable+" VALUES ($1, $2)", i+1, i)
		}
		conn.Close()
		if err != nil {
			return fmt.Errorf("round %d: %w", i, err)
		}
	}
	return nil
}

// resizeDeleteRound plans the resize, fires the delete after delay, joins both,
// and asserts the endpoint and its compute are gone with nothing left running.
func (a *ampFeatureRun) resizeDeleteRound(ctx context.Context, cl *controller.Client, ep controller.Endpoint, round int, delay time.Duration) error {
	proj := a.proj
	resizeTask, _, err := cl.ResizeEndpoint(ctx, proj.poolID, proj.projID, proj.branchID, ep.ID,
		controller.ResizeEndpointRequest{Resources: controller.EndpointResources{CPU: resizeCPUA}})
	if err != nil {
		return fmt.Errorf("plan the resize: %w", err)
	}
	if delay > 0 {
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-time.After(delay):
		}
	}
	// The delete is always accepted (202): the endpoint lock queues its step
	// behind the resize rather than refusing the request.
	deleteTask, _, err := cl.DeleteEndpoint(ctx, proj.poolID, proj.projID, proj.branchID, ep.ID)
	if err != nil {
		return fmt.Errorf("the delete planned %s after the resize was refused: %w", delay, err)
	}

	settle, cancel := context.WithTimeout(ctx, resizeDeleteRaceSettle)
	defer cancel()
	resizeDone, rerr := cl.WaitTask(settle, resizeTask.ID)
	if resizeDone.Status == "" {
		return fmt.Errorf("resize task %s never reached a terminal status within %s while a delete of the same endpoint was in flight: %v (the endpoint lock must queue, not deadlock)", resizeTask.ID, resizeDeleteRaceSettle, rerr)
	}
	deleteDone, derr := cl.WaitTask(settle, deleteTask.ID)
	if deleteDone.Status == "" {
		return fmt.Errorf("delete task %s never reached a terminal status within %s (resize ended %s): %v", deleteTask.ID, resizeDeleteRaceSettle, resizeDone.Status, derr)
	}
	a.f.logf("round %d: resize %s ended %s, delete %s ended %s", round, resizeTask.ID, resizeDone.Status, deleteTask.ID, deleteDone.Status)
	if deleteDone.Status != controller.TaskSucceeded {
		return fmt.Errorf("the delete task %s ended %s after the resize ended %s: %v; a delete queued behind a resize on the endpoint lock must still retire the endpoint", deleteTask.ID, deleteDone.Status, resizeDone.Status, derr)
	}

	if _, err := cl.GetEndpoint(ctx, proj.poolID, proj.projID, proj.branchID, ep.ID); apiErrorStatus(err) != http.StatusNotFound {
		return fmt.Errorf("endpoint %s is still readable after its delete task succeeded (%v), want 404", ep.ID, err)
	}
	if err := a.assertNoTaskStuck(ctx, cl, proj.projID); err != nil {
		return err
	}
	if kube := a.f.ampKube(); kube != nil {
		sel := "app.kubernetes.io/instance=" + endpointDeployName(ep.ID)
		var left string
		err := pollUntil(ctx, resizeDeleteRacePodSettle, 3*time.Second, func() (bool, error) {
			pods, err := kube.GetSelector(ctx, "pod", sel, "{.items[*].metadata.name}")
			if err != nil {
				return false, fmt.Errorf("list pods for %s: %w", sel, err)
			}
			left = pods
			return pods == "", nil
		})
		if err != nil && left != "" {
			return fmt.Errorf("endpoint %s is gone from the API but its compute pod(s) %q are still in the cluster %s after the delete succeeded: the resize's redeploy landed after the delete's uninstall and left compute nobody owns", ep.ID, left, resizeDeleteRacePodSettle)
		}
		if err != nil {
			return err
		}
	}
	return nil
}

// assertNoTaskStuck: once both racers are terminal, nothing on the cluster may
// still be pending or running — a task the lock never released would sit here.
func (a *ampFeatureRun) assertNoTaskStuck(ctx context.Context, cl *controller.Client, clusterID string) error {
	for _, status := range []string{controller.TaskPending, controller.TaskRunning} {
		list, err := cl.ListTasks(ctx, controller.ListTasksOptions{ClusterID: clusterID, Status: status, Limit: 10})
		if err != nil {
			return fmt.Errorf("list %s tasks of cluster %s: %w", status, clusterID, err)
		}
		if list.Total > 0 {
			kinds := make([]string, 0, len(list.Tasks))
			for _, t := range list.Tasks {
				kinds = append(kinds, t.Kind+" "+t.ID)
			}
			return fmt.Errorf("%d task(s) of cluster %s are still %s after both racers settled: %v", list.Total, clusterID, status, kinds)
		}
	}
	return nil
}
