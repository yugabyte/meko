// POST /tasks/{id}/cancel (meko PR #1361).
package features

import (
	"context"
	"fmt"
	"net/http"
	"strings"
	"time"

	"github.com/yugabyte/amp/test/functional/framework/apps/controller"
	"github.com/yugabyte/amp/test/functional/framework/core"
)

const (
	cancelTaskBudget   = 20 * time.Minute
	cancelUnknownTask  = "00000000-0000-4000-8000-00000000c0de"
	cancelledErrorWord = "cancel"
)

func init() {
	core.Register(core.Test{
		Name:        "features/amp_task_cancel",
		Suite:       "features",
		Category:    core.Functional,
		Env:         core.EnvSpec{Sharing: core.Exclusive, Shape: core.SharedControlPlane, SharedLock: core.LockEndpointFleetUpgrade},
		Tags:        []string{"regression"},
		OwningTeam:  core.TeamControlPlane,
		Description: "Task cancellation: cancelling an unknown task is 404, a malformed id 400, a finished task 409. A two-endpoint fleet roll cancelled as soon as it is planned ends failed with the cancellation as its error and cancel_requested_at set, a repeated cancel keeps the first timestamp, no child is left pending or running, both endpoints still serve their rows, and a fresh roll of the same endpoints is accepted and succeeds rather than being blocked by the cancelled one. A writer create and a with_writer cluster create cancelled the moment they are accepted either lose the race and serve, or end cancelled leaving no compute behind, after which a writer create on that branch and a same-named cluster create succeed.",
		Params:      []string{"functional", "env.ampstack", "apps.ybastack", "apps.yba", "apps.ybk8s"},
		Timeout:     60 * time.Minute,
		Run:         runAmpTaskCancel,
	})
}

type taskCancelRun struct {
	a   *ampFeatureRun
	r   *redpathRun
	cl  *controller.Client
	eps []controller.Endpoint
}

func runAmpTaskCancel(t *core.T) error {
	return runAmpFeature(t, func(a *ampFeatureRun) []phase {
		c := &taskCancelRun{a: a}
		return []phase{
			{"cancel_refusals", c.refusals},
			{"cancel_fleet_roll", c.cancelRoll},
			{"cancel_endpoint_create", c.cancelEndpointCreate},
			{"cancel_cluster_create", c.cancelClusterCreate},
			{"bystander_still_serving", func(ctx context.Context) error { return c.r.stillServing(ctx) }},
		}
	})
}

func (c *taskCancelRun) refusals(ctx context.Context) error {
	r, err := c.a.newRedpath(ctx)
	if err != nil {
		return err
	}
	c.r, c.cl = r, r.cl
	if _, err := c.cl.CancelTask(ctx, cancelUnknownTask); apiErrorStatus(err) != http.StatusNotFound {
		return fmt.Errorf("cancelling an unknown task answered %v, want 404", err)
	}
	if _, err := c.cl.CancelTask(ctx, "not-a-uuid"); apiErrorStatus(err) != http.StatusBadRequest {
		return fmt.Errorf("cancelling a malformed task id answered %v, want 400", err)
	}
	done, err := c.cl.ListTasks(ctx, controller.ListTasksOptions{ClusterID: c.r.target.projID, Kind: "create_cluster", Status: controller.TaskSucceeded, Limit: 1})
	if err != nil {
		return fmt.Errorf("find the target cluster's create task: %w", err)
	}
	if len(done.Tasks) == 0 {
		return fmt.Errorf("no succeeded create_cluster task for cluster %s", c.r.target.projID)
	}
	if _, err := c.cl.CancelTask(ctx, done.Tasks[0].ID); apiErrorStatus(err) != http.StatusConflict {
		return fmt.Errorf("cancelling succeeded task %s answered %v, want 409", done.Tasks[0].ID, err)
	}
	after, err := c.cl.GetTask(ctx, done.Tasks[0].ID)
	if err != nil {
		return err
	}
	if after.Status != controller.TaskSucceeded || after.CancelRequestedAt != nil {
		return fmt.Errorf("a refused cancel changed finished task %s to status %q cancel_requested_at %v", after.ID, after.Status, after.CancelRequestedAt)
	}
	return nil
}

func (c *taskCancelRun) requestCancel(ctx context.Context, ref, what string) (time.Time, error) {
	first, err := c.cl.CancelTask(ctx, ref)
	if apiErrorStatus(err) == http.StatusConflict {
		return time.Time{}, nil
	}
	if err != nil {
		return time.Time{}, fmt.Errorf("cancel %s task %s: %w", what, ref, err)
	}
	if first.CancelRequestedAt == nil {
		return time.Time{}, fmt.Errorf("the 202 for cancelling %s task %s carries no cancel_requested_at", what, ref)
	}
	second, err := c.cl.CancelTask(ctx, ref)
	if apiErrorStatus(err) == http.StatusConflict {
		return *first.CancelRequestedAt, nil
	}
	if err != nil {
		return time.Time{}, fmt.Errorf("repeat the cancel of %s task %s: %w", what, ref, err)
	}
	if second.CancelRequestedAt == nil || !second.CancelRequestedAt.Equal(*first.CancelRequestedAt) {
		return time.Time{}, fmt.Errorf("repeating the cancel of %s task %s moved cancel_requested_at from %v to %v, want the original kept", what, ref, first.CancelRequestedAt, second.CancelRequestedAt)
	}
	return *first.CancelRequestedAt, nil
}

func (c *taskCancelRun) settle(ctx context.Context, ref, what string) (controller.Task, error) {
	wctx, cancel := context.WithTimeout(ctx, cancelTaskBudget)
	defer cancel()
	done, err := c.cl.WaitTask(wctx, ref)
	if done.Status == controller.TaskSucceeded || done.Status == controller.TaskFailed {
		return done, nil
	}
	if ctx.Err() != nil {
		return done, ctx.Err()
	}
	return done, fmt.Errorf("%s task %s was still %q %s after it was cancelled: a cancelled task must reach a terminal status (%v)", what, ref, done.Status, cancelTaskBudget, err)
}

func cancelledProperly(t controller.Task, what string) error {
	msg := ""
	if t.Error != nil {
		msg = *t.Error
	}
	if !strings.Contains(strings.ToLower(msg), cancelledErrorWord) {
		return fmt.Errorf("cancelled %s task %s failed with %q, which does not say it was cancelled: an operator cannot tell a stopped task from a broken one", what, t.ID, msg)
	}
	if t.CancelRequestedAt == nil {
		return fmt.Errorf("cancelled %s task %s ended failed with no cancel_requested_at", what, t.ID)
	}
	return nil
}

func (c *taskCancelRun) cancelRoll(ctx context.Context) error {
	proj := c.r.target
	base, err := createEndpoint(ctx, c.cl, proj.poolID, proj.projID, proj.branchID)
	if err != nil {
		return fmt.Errorf("create the roll's base endpoint: %w", err)
	}
	conn, err := c.a.f.endpointSQL(ctx, base)
	if err != nil {
		return fmt.Errorf("connect the base endpoint: %w", err)
	}
	if err := seedRollTable(ctx, conn); err != nil {
		conn.Close()
		return err
	}
	conn.Close()
	childName := "cancel-fork-" + c.a.f.suffix()
	childConn, err := forkChildAt(ctx, c.cl, proj, proj.branchID, "", childName, c.a.f.endpointSQL)
	if err != nil {
		return err
	}
	childConn.Close()
	child, err := findBranchCtl(ctx, c.cl, proj.poolID, proj.projID, childName)
	if err != nil {
		return err
	}
	childEP, ok := lastEndpoint(ctx, c.cl, proj.poolID, proj.projID, child.BranchID)
	if !ok {
		return fmt.Errorf("the fork %s has no endpoint", child.BranchID)
	}
	c.eps = []controller.Endpoint{base, childEP}
	ids := []string{base.ID, childEP.ID}

	task, loc, err := c.cl.UpgradeEndpoints(ctx, controller.UpgradeEndpointsRequest{EndpointIDs: ids, MaxParallel: 1})
	if err != nil {
		return fmt.Errorf("plan a roll of %v: %w", ids, err)
	}
	ref := controller.TaskRef(task, loc)
	stamped, err := c.requestCancel(ctx, ref, "endpoint upgrade roll")
	if err != nil {
		return err
	}
	done, err := c.settle(ctx, ref, "endpoint upgrade roll")
	if err != nil {
		return err
	}
	subs, err := c.cl.ListSubtasks(ctx, ref)
	if err != nil {
		return fmt.Errorf("list the roll's children: %w", err)
	}
	for _, s := range subs.Subtasks {
		if s.Status == controller.TaskPending || s.Status == controller.TaskRunning {
			return fmt.Errorf("child %s (%s) of cancelled roll %s is still %q: a terminal parent is never reclaimed, so it is work nothing will ever run", s.ID, s.EndpointID(), ref, s.Status)
		}
	}
	c.a.f.logf("roll %s ended %q with %d children after a cancel stamped %v", ref, done.Status, len(subs.Subtasks), stamped)
	switch {
	case stamped.IsZero():
		return fmt.Errorf("roll %s was already terminal (%q) when cancelled a moment after it was planned: two endpoints at max_parallel 1 cannot roll that fast", ref, done.Status)
	case done.Status == controller.TaskSucceeded:
		return fmt.Errorf("roll %s was cancelled at %v, before its first child could have finished, and still succeeded rolling %d children: the cancel was ignored", ref, stamped, len(subs.Subtasks))
	}
	if err := cancelledProperly(done, "endpoint upgrade roll"); err != nil {
		return err
	}
	if _, err := c.cl.CancelTask(ctx, ref); apiErrorStatus(err) != http.StatusConflict {
		return fmt.Errorf("cancelling the already-cancelled roll %s answered %v, want 409", ref, err)
	}
	for _, ep := range c.eps {
		if err := c.servesRollRows(ctx, ep); err != nil {
			return fmt.Errorf("after the cancelled roll: %w", err)
		}
	}

	again, loc, err := c.cl.UpgradeEndpoints(ctx, controller.UpgradeEndpointsRequest{EndpointIDs: ids, MaxParallel: 1})
	if err != nil {
		return fmt.Errorf("a roll of the same endpoints after the cancelled one was refused (%w): a cancelled roll must not block the next", err)
	}
	wctx, cancel := context.WithTimeout(ctx, rollTaskBudget)
	defer cancel()
	if _, err := c.cl.WaitTask(wctx, controller.TaskRef(again, loc)); err != nil {
		return fmt.Errorf("the roll after the cancelled one: %w", err)
	}
	for _, ep := range c.eps {
		if err := c.servesRollRows(ctx, ep); err != nil {
			return fmt.Errorf("after the follow-up roll: %w", err)
		}
	}
	return nil
}

func (c *taskCancelRun) servesRollRows(ctx context.Context, ep controller.Endpoint) error {
	conn, err := c.a.f.endpointSQL(ctx, ep)
	if err != nil {
		return fmt.Errorf("endpoint %s does not accept connections: %w", ep.ID, err)
	}
	defer conn.Close()
	n, err := conn.QueryInt(ctx, "SELECT count(*) FROM "+rollTable)
	if err != nil {
		return fmt.Errorf("endpoint %s cannot read %s: %w", ep.ID, rollTable, err)
	}
	if n != rollRows {
		return fmt.Errorf("endpoint %s serves %d rows of %s, want %d", ep.ID, n, rollTable, rollRows)
	}
	return nil
}

func (c *taskCancelRun) cancelEndpointCreate(ctx context.Context) error {
	proj := c.r.target
	name := "cancel-ep-" + c.a.f.suffix()
	if _, err := c.cl.CreateBranchAndWait(ctx, proj.poolID, proj.projID, controller.CreateBranchRequest{Name: name, ParentBranchID: proj.branchID}); err != nil {
		return fmt.Errorf("create the branch for the cancelled create: %w", err)
	}
	b, err := findBranchCtl(ctx, c.cl, proj.poolID, proj.projID, name)
	if err != nil {
		return err
	}
	task, loc, err := c.cl.CreateEndpoint(ctx, proj.poolID, proj.projID, b.BranchID, controller.CreateEndpointRequest{Role: roleReadWrite})
	if err != nil {
		return fmt.Errorf("plan the writer create: %w", err)
	}
	ref := controller.TaskRef(task, loc)
	if _, err := c.requestCancel(ctx, ref, "create_endpoint"); err != nil {
		return err
	}
	done, err := c.settle(ctx, ref, "create_endpoint")
	if err != nil {
		return err
	}
	if done.Status == controller.TaskFailed {
		if err := cancelledProperly(done, "create_endpoint"); err != nil {
			return err
		}
		if err := c.r.noOrphans(ctx, b.BranchID, c.r.cfg.OrphanTimeout); err != nil {
			return err
		}
	} else {
		c.a.f.logf("create_endpoint %s (created %v, cancel stamped %v, finished %v) started its step before the cancel landed", ref, done.CreatedAt, done.CancelRequestedAt, done.UpdatedAt)
	}
	if _, err := c.cl.CancelTask(ctx, ref); apiErrorStatus(err) != http.StatusConflict {
		return fmt.Errorf("cancelling finished create_endpoint %s answered %v, want 409", ref, err)
	}
	return c.r.retrySucceeds(ctx, "create_endpoint after a cancelled one", func(ctx context.Context) error {
		if ep, ok := lastEndpoint(ctx, c.cl, proj.poolID, proj.projID, b.BranchID); ok {
			if c.r.serves(ctx, ep) == nil {
				return nil
			}
			c.a.f.logf("the cancelled create left endpoint row %s on branch %s; deleting it before the retry (meko#486)", ep.ID, b.BranchID)
			if _, err := c.cl.DeleteEndpointAndWait(ctx, proj.poolID, proj.projID, b.BranchID, ep.ID); err != nil {
				return fmt.Errorf("delete the endpoint the cancelled create left: %w", err)
			}
		}
		ep, err := createEndpoint(ctx, c.cl, proj.poolID, proj.projID, b.BranchID)
		if err != nil {
			return fmt.Errorf("a writer create on the branch whose create was cancelled: %w", err)
		}
		return c.r.serves(ctx, ep)
	})
}

func (c *taskCancelRun) cancelClusterCreate(ctx context.Context) error {
	name := "cancel-cl-" + c.a.f.suffix()
	pool := c.a.proj.poolID
	task, loc, err := c.cl.CreateCluster(ctx, pool, controller.CreateClusterRequest{Name: name, PgVersion: c.a.f.cfg.PgVersion, WithWriter: true})
	if err != nil {
		return fmt.Errorf("plan the with_writer cluster create: %w", err)
	}
	ref := controller.TaskRef(task, loc)
	c.a.f.register("delete cluster "+name, func(ctx context.Context) error {
		cls, err := c.cl.ListClustersIncludingFailed(ctx, pool, 200, 0)
		if err != nil {
			return err
		}
		for _, cl := range cls.Clusters {
			if cl.Name == name {
				if _, err := c.cl.DeleteClusterAndWait(ctx, pool, cl.ID); err != nil {
					return err
				}
			}
		}
		return nil
	})
	if _, err := c.requestCancel(ctx, ref, "create_cluster"); err != nil {
		return err
	}
	done, err := c.settle(ctx, ref, "create_cluster")
	if err != nil {
		return err
	}
	if done.Status == controller.TaskSucceeded {
		c.a.f.logf("create_cluster %s (created %v, cancel stamped %v, finished %v) passed every step boundary before the cancel landed", ref, done.CreatedAt, done.CancelRequestedAt, done.UpdatedAt)
		proj, err := findClusterCtl(ctx, c.cl, pool, name)
		if err != nil {
			return err
		}
		ep, ok := lastEndpoint(ctx, c.cl, pool, proj.ID, proj.DefaultBranch.BranchID)
		if !ok {
			return fmt.Errorf("with_writer cluster %s succeeded with no writer", name)
		}
		conn, err := c.a.f.endpointSQL(ctx, ep)
		if err != nil {
			return fmt.Errorf("the writer of the cluster whose cancel lost the race: %w", err)
		}
		conn.Close()
		return nil
	}
	if err := cancelledProperly(done, "create_cluster"); err != nil {
		return err
	}
	if done.ClusterID != nil {
		if err := c.clusterHasNoCompute(ctx, pool, *done.ClusterID); err != nil {
			return err
		}
	}
	return c.r.retrySucceeds(ctx, "a same-named cluster create after a cancelled one", func(ctx context.Context) error {
		_, err := c.cl.CreateClusterAndWait(ctx, pool, controller.CreateClusterRequest{Name: name, PgVersion: c.a.f.cfg.PgVersion, WithWriter: true})
		if err != nil {
			return fmt.Errorf("%w (a cancelled create must not squat on its cluster's name)", err)
		}
		return nil
	})
}

func (c *taskCancelRun) clusterHasNoCompute(ctx context.Context, pool, clusterID string) error {
	cl, err := c.cl.GetCluster(ctx, pool, clusterID)
	if err != nil {
		if apiErrorStatus(err) == http.StatusNotFound {
			return nil
		}
		return fmt.Errorf("read the cancelled cluster %s: %w", clusterID, err)
	}
	var left map[string]string
	err = pollUntil(ctx, c.r.cfg.OrphanTimeout, 15*time.Second, func() (bool, error) {
		pods, err := c.a.f.branchComputePods(ctx, cl.DefaultBranch.BranchID)
		left = pods
		return len(pods) == 0, err
	})
	if err != nil && len(left) > 0 {
		return fmt.Errorf("the cancelled with_writer create of cluster %s left compute %v running %s later: a cancel stops the remaining steps, so nothing may be serving", clusterID, left, c.r.cfg.OrphanTimeout)
	}
	return err
}
