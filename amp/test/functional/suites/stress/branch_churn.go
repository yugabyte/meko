// branch_churn is the control-plane churn stress scenario: several lanes run the
// full branch lifecycle — fork a branch, deploy an endpoint on it, write and
// verify a dataset, retire the endpoint, delete the branch — over and over, in
// parallel, against one cluster. It is the counterpart to the data-plane load
// tests: nothing here is about throughput, it is about whether the controller's
// asynchronous create/delete tasks stay correct and leak nothing when the same
// resource ladder is built and torn down concurrently, hundreds of tasks deep.
//
// It runs on a STANDING amp control plane (the sharedk8s cluster-per-test env
// exposes one) rather than self-provisioning: churning branches needs the control
// plane, not a bare YugabyteDB backing, and it skips cleanly on any env that does
// not offer one. Everything it creates hangs off a single cluster, whose delete
// cascade is registered before the first lane starts, so a killed run leaves
// nothing behind.
package stress

import (
	"context"
	"errors"
	"fmt"
	"log"
	"net/http"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/yugabyte/amp/test/functional/framework/apps/controller"
	"github.com/yugabyte/amp/test/functional/framework/apps/pg"
	"github.com/yugabyte/amp/test/functional/framework/core"
)

const (
	// churnLanes is how many lifecycles run concurrently against the one cluster.
	churnLanes = 3
	// churnCycles is how many lifecycles each lane completes in sequence.
	churnCycles = 4
	// churnRows is the dataset each cycle writes to its branch and reads back
	// before retiring the endpoint.
	churnRows = 200
	// churnTable is the per-cycle table name (each cycle owns its own branch, so
	// the name never collides).
	churnTable = "amp_churn"
	// churnTeardownBudget bounds the cluster-cascade cleanup, independent of the
	// body's deadline.
	churnTeardownBudget = 15 * time.Minute
	// churnRetries is how many extra attempts one control-plane call makes on a
	// transient controller error (concurrent-DDL deadlock, the endpoint helm race)
	// — the churn drives exactly the concurrency that produces them.
	churnRetries = 4
)

func init() {
	core.Register(core.Test{
		Name:        "stress/branch_churn",
		Suite:       "stress",
		Category:    core.Stress,
		Env:         core.EnvSpec{Sharing: core.Exclusive, Shape: core.DedicatedStack},
		Tags:        []string{"stress"},
		OwningTeam:  core.TeamControlPlane,
		Description: "Churn the branch lifecycle (fork, endpoint, write, retire, delete) in parallel lanes against one cluster, cancelling a branch create or an endpoint create in two cycles of every three, and assert every cancelled task either lost the race or ended failed as cancelled, every cycle converged, nothing was orphaned, and no task failed other than the cancelled and the retried ones.",
		Params:      []string{"env.sharedk8s", "stress.consistency"},
		Run:         runBranchChurn,
	})
}

// ampControlPlaneEnv is the capability this test depends on: a standing amp
// control plane it can create a cluster on and reach the endpoints of. The
// sharedk8s env satisfies it; depending on the capability rather than that
// concrete env is what lets any future control-plane env run this unchanged.
type ampControlPlaneEnv interface {
	AmpController(ctx context.Context) (*controller.Client, error)
	AmpPoolID() string
	ConnectEndpoint(ctx context.Context, ep controller.Endpoint) (*pg.Client, error)
}

// churnRun carries the state the lanes share: the number of transient
// control-plane errors they retried past. The final no-failed-task assertion is
// stated against it — a retried transient DOES leave a failed task behind, so
// asserting a flat zero would be asserting something the retry itself breaks.
type churnRun struct {
	retried atomic.Int64
	// seedEndpoint is the writer seed_parent leaves on the DEFAULT branch. The
	// churn's own endpoints live on its own branches, so this one is the only
	// endpoint the default branch is allowed to have at the end.
	seedEndpoint string
	seedEP       controller.Endpoint
	cancelled    atomic.Int64
	lostRace     atomic.Int64
}

const (
	churnCancelNone = iota
	churnCancelBranch
	churnCancelEndpoint
)

// runBranchChurn creates one cluster, runs churnLanes × churnCycles branch
// lifecycles across it, and then asserts the control plane is exactly where it
// started: only the default branch left, no endpoint anywhere, no failed task
// recorded against the cluster, and the controller still ready.
func runBranchChurn(t *core.T) (err error) {
	env, ok := t.Env.(ampControlPlaneEnv)
	if !ok {
		t.Skip("requires an env exposing a standing amp control plane (sharedk8s)")
		return nil
	}
	logf := t.Masker().Logf(log.Printf)
	cl, err := env.AmpController(t.Context)
	if err != nil {
		return core.AsEnvFailure(fmt.Errorf("amp controller client: %w", err))
	}
	poolID := env.AmpPoolID()

	run := &churnRun{}
	td := &teardownStack{}
	defer func() {
		tctx, cancel := context.WithTimeout(context.Background(), churnTeardownBudget)
		defer cancel()
		if tdErr := td.run(tctx, logf); tdErr != nil && err == nil {
			err = fmt.Errorf("teardown: %w", tdErr)
		}
	}()

	var proj controller.Cluster
	if err := t.Step("create_cluster", func(s *core.StepHandle) error {
		name := "churn-" + suffix(t)
		s.Set("cluster", name)
		if _, err := cl.CreateClusterAndWait(t.Context, poolID, controller.CreateClusterRequest{Name: name}); err != nil {
			return err
		}
		p, err := findCluster(t.Context, cl, poolID, name)
		if err != nil {
			return err
		}
		proj = p
		td.push("delete churn cluster (cascade)", func(ctx context.Context) error {
			_, e := cl.DeleteClusterAndWait(ctx, poolID, proj.ID)
			return e
		})
		return nil
	}); err != nil {
		return err
	}

	// A branch with no endpoint has no determinable fork point (the controller
	// rejects the fork outright), so the parent gets one writer + a seed row
	// before any lane forks from it. It also gives the churn real history to
	// fork over instead of an empty timeline.
	if err := t.Step("seed_parent", func(s *core.StepHandle) error {
		if _, err := cl.CreateEndpointAndWait(t.Context, poolID, proj.ID,
			proj.DefaultBranch.BranchID, controller.CreateEndpointRequest{Role: "read_write"}); err != nil {
			return fmt.Errorf("create parent endpoint: %w", err)
		}
		eps, err := cl.ListAllEndpoints(t.Context, poolID, proj.ID, proj.DefaultBranch.BranchID)
		if err != nil {
			return fmt.Errorf("list parent endpoints: %w", err)
		}
		if len(eps) == 0 {
			return fmt.Errorf("parent endpoint create reported success but the branch lists none")
		}
		ep := eps[len(eps)-1]
		run.seedEndpoint = ep.ID
		run.seedEP = ep
		s.Set("endpoint", ep.ID)
		conn, err := env.ConnectEndpoint(t.Context, ep)
		if err != nil {
			return fmt.Errorf("connect parent endpoint: %w", err)
		}
		defer conn.Close()
		if _, err := conn.Exec(t.Context,
			"CREATE TABLE churn_seed (id int PRIMARY KEY, note text)"); err != nil {
			return err
		}
		_, err = conn.Exec(t.Context,
			"INSERT INTO churn_seed VALUES (1, 'parent history for fork points')")
		return err
	}); err != nil {
		return err
	}

	if err := t.Step("churn", func(s *core.StepHandle) error {
		s.Set("lanes", churnLanes)
		s.Set("cycles_per_lane", churnCycles)
		var wg sync.WaitGroup
		errs := make([]error, churnLanes)
		for lane := 0; lane < churnLanes; lane++ {
			wg.Add(1)
			go func(lane int) {
				defer wg.Done()
				errs[lane] = runChurnLane(t.Context, t, run, env, cl, poolID, proj, lane, logf)
			}(lane)
		}
		wg.Wait()
		for lane, e := range errs {
			if e != nil {
				return fmt.Errorf("churn lane %d: %w", lane, e)
			}
		}
		s.Set("transients_retried", run.retried.Load())
		s.Set("cancels_honoured", run.cancelled.Load())
		s.Set("cancels_lost_race", run.lostRace.Load())
		t.Metric("churn_cycles", float64(churnLanes*churnCycles), "count")
		t.Metric("churn_cancels_honoured", float64(run.cancelled.Load()), "count")
		return nil
	}); err != nil {
		return err
	}

	if err := t.Step("verify_no_orphans", func(s *core.StepHandle) error {
		branches, err := cl.ListAllBranches(t.Context, poolID, proj.ID)
		if err != nil {
			return fmt.Errorf("list branches after the churn: %w", err)
		}
		s.Set("branches_left", len(branches))
		if len(branches) != 1 || branches[0].BranchID != proj.DefaultBranch.BranchID {
			names := make([]string, 0, len(branches))
			for _, b := range branches {
				names = append(names, b.Name)
			}
			return fmt.Errorf("after %d branch lifecycles the cluster lists %d branch(es) (%v), want only the default branch — every churned branch was deleted and waited on, so anything left is an orphan",
				churnLanes*churnCycles, len(branches), names)
		}
		eps, err := cl.ListAllEndpoints(t.Context, poolID, proj.ID, proj.DefaultBranch.BranchID)
		if err != nil {
			return fmt.Errorf("list endpoints after the churn: %w", err)
		}
		// seed_parent's writer is still there on purpose — it is what gives the
		// lanes a determinable fork point, and only the cascade at teardown
		// removes it. So the bar is "exactly that endpoint and nothing else":
		// an extra one, or a different one, still means the churn leaked.
		if len(eps) != 1 || eps[0].ID != run.seedEndpoint {
			ids := make([]string, 0, len(eps))
			for _, e := range eps {
				ids = append(ids, e.ID)
			}
			return fmt.Errorf("the default branch lists %v after the churn, want only seed_parent's writer %s — the churn only ever created endpoints on its own branches",
				ids, run.seedEndpoint)
		}
		failed, err := churnFailedTasks(t.Context, cl, proj.ID)
		if err != nil {
			return fmt.Errorf("list failed tasks after the churn: %w", err)
		}
		// Each transient the lanes retried past left its own failed task, so the
		// bar is "no failure the churn did not already observe and recover from".
		retried := int(run.retried.Load())
		var broken []string
		cancelled := 0
		for _, tk := range failed {
			if churnCancelled(tk) {
				cancelled++
				continue
			}
			broken = append(broken, tk.Kind)
		}
		s.Set("failed_tasks", len(failed))
		s.Set("cancelled_tasks", cancelled)
		if cancelled != int(run.cancelled.Load()) {
			return fmt.Errorf("the cluster has %d failed task(s) carrying cancel_requested_at but the lanes saw %d cancels honoured", cancelled, run.cancelled.Load())
		}
		if len(broken) > retried {
			return fmt.Errorf("the cluster has %d failed task(s) that were not cancelled (%v) but the lanes only retried past %d transient error(s), so at least one lifecycle failed unobserved", len(broken), broken, retried)
		}
		if err := cl.Readyz(t.Context); err != nil {
			return fmt.Errorf("the controller is not ready after the churn: %w", err)
		}
		return nil
	}); err != nil {
		return err
	}

	// The churn deletes every branch it forked, so the parent's own endpoint is
	// all that is left to read pages from.
	return t.Step("physical_consistency", func(s *core.StepHandle) error {
		return verifyEndpointConsistency(t, s, env, []consistencyTarget{{Name: "seed endpoint", Endpoint: run.seedEP}})
	})
}

// runChurnLane runs one lane's cycles in sequence: fork a branch, deploy an
// endpoint on it, write and read back a dataset, retire the endpoint, delete the
// branch — asserting each step converged before moving on. It probes controller
// readiness every cycle so a control plane that degrades under the churn is
// caught while it is happening, not only at the end.
func runChurnLane(ctx context.Context, t *core.T, run *churnRun, env ampControlPlaneEnv, cl *controller.Client, poolID string, proj controller.Cluster, lane int, logf func(string, ...any)) error {
	for cycle := 0; cycle < churnCycles; cycle++ {
		name := fmt.Sprintf("churn-l%d-c%d-%s", lane, cycle, suffix(t))
		if err := churnCycle(ctx, run, env, cl, poolID, proj, name, (lane+cycle)%3); err != nil {
			return fmt.Errorf("cycle %d (branch %s): %w", cycle, name, err)
		}
		if err := cl.Readyz(ctx); err != nil {
			return fmt.Errorf("the controller stopped reporting ready after cycle %d: %w", cycle, err)
		}
		logf("churn lane %d cycle %d done (branch %s)", lane, cycle, name)
	}
	return nil
}

// churnCycle is one full lifecycle. Every create and delete is waited on, so a
// step that never converges fails here rather than surfacing as an orphan later.
func churnCycle(ctx context.Context, run *churnRun, env ampControlPlaneEnv, cl *controller.Client, poolID string, proj controller.Cluster, name string, cancel int) error {
	branchReady := false
	if cancel == churnCancelBranch {
		var ref string
		if err := withChurnRetry(ctx, run, func() error {
			task, loc, err := cl.CreateBranch(ctx, poolID, proj.ID, controller.CreateBranchRequest{Name: name, ParentBranchID: proj.DefaultBranch.BranchID})
			ref = controller.TaskRef(task, loc)
			return err
		}); err != nil {
			return fmt.Errorf("plan the branch create to cancel: %w", err)
		}
		done, err := churnCancel(ctx, run, cl, ref, "branch create")
		if err != nil {
			return err
		}
		if branchReady = done.Status == controller.TaskSucceeded; !branchReady {
			if br, err := findBranch(ctx, cl, poolID, proj.ID, name); err == nil {
				if _, err := cl.DeleteBranchAndWait(ctx, poolID, proj.ID, br.BranchID); err != nil {
					return fmt.Errorf("delete the branch a cancelled create left behind: %w", err)
				}
			}
		}
	}
	if err := withChurnRetry(ctx, run, func() error {
		if branchReady {
			return nil
		}
		_, err := cl.CreateBranchAndWait(ctx, poolID, proj.ID, controller.CreateBranchRequest{
			Name:           name,
			ParentBranchID: proj.DefaultBranch.BranchID,
		})
		return err
	}); err != nil {
		return fmt.Errorf("create branch: %w", err)
	}
	br, err := findBranch(ctx, cl, poolID, proj.ID, name)
	if err != nil {
		return err
	}

	var ep controller.Endpoint
	if cancel == churnCancelEndpoint {
		var ref string
		if err := withChurnRetry(ctx, run, func() error {
			task, loc, err := cl.CreateEndpoint(ctx, poolID, proj.ID, br.BranchID, controller.CreateEndpointRequest{Role: "read_write"})
			ref = controller.TaskRef(task, loc)
			return err
		}); err != nil {
			return fmt.Errorf("plan the endpoint create to cancel: %w", err)
		}
		done, err := churnCancel(ctx, run, cl, ref, "endpoint create")
		if err != nil {
			return err
		}
		eps, err := cl.ListAllEndpoints(ctx, poolID, proj.ID, br.BranchID)
		if err != nil {
			return err
		}
		for _, e := range eps {
			if done.Status == controller.TaskSucceeded {
				ep = e
				continue
			}
			// A cancelled create can leave its row until the sweeper takes it (meko#486).
			if _, err := cl.DeleteEndpointAndWait(ctx, poolID, proj.ID, br.BranchID, e.ID); err != nil {
				return fmt.Errorf("delete endpoint %s a cancelled create left: %w", e.ID, err)
			}
		}
	}
	if err := withChurnRetry(ctx, run, func() error {
		if ep.ID != "" {
			return nil
		}
		if _, err := cl.CreateEndpointAndWait(ctx, poolID, proj.ID, br.BranchID, controller.CreateEndpointRequest{Role: "read_write"}); err != nil {
			return err
		}
		eps, err := cl.ListAllEndpoints(ctx, poolID, proj.ID, br.BranchID)
		if err != nil {
			return err
		}
		if len(eps) == 0 {
			return fmt.Errorf("endpoint create on branch %s reported success but the branch lists no endpoint", name)
		}
		ep = eps[len(eps)-1]
		return nil
	}); err != nil {
		return fmt.Errorf("create endpoint: %w", err)
	}

	if err := churnWrite(ctx, env, ep); err != nil {
		return err
	}

	if _, err := cl.DeleteEndpointAndWait(ctx, poolID, proj.ID, br.BranchID, ep.ID); err != nil {
		return fmt.Errorf("delete endpoint %s: %w", ep.ID, err)
	}
	if _, err := cl.DeleteBranchAndWait(ctx, poolID, proj.ID, br.BranchID); err != nil {
		return fmt.Errorf("delete branch %s: %w", name, err)
	}
	return nil
}

// churnCancel requires a just-cancelled task to end succeeded (lost the race) or failed as cancelled.
func churnCancel(ctx context.Context, run *churnRun, cl *controller.Client, ref, what string) (controller.Task, error) {
	if _, err := cl.CancelTask(ctx, ref); err != nil {
		var apiErr *controller.APIError
		if !errors.As(err, &apiErr) || apiErr.StatusCode != http.StatusConflict {
			return controller.Task{}, fmt.Errorf("cancel the %s %s: %w", what, ref, err)
		}
	}
	done, err := cl.WaitTask(ctx, ref)
	switch done.Status {
	case controller.TaskSucceeded:
		run.lostRace.Add(1)
		return done, nil
	case controller.TaskFailed:
		msg := ""
		if done.Error != nil {
			msg = *done.Error
		}
		if !churnCancelled(done) {
			if transientChurnErr(errors.New(msg)) {
				run.retried.Add(1)
				return done, nil
			}
			return done, fmt.Errorf("the cancelled %s %s failed with %q and cancel_requested_at %v, not as cancelled", what, ref, msg, done.CancelRequestedAt)
		}
		run.cancelled.Add(1)
		return done, nil
	}
	return done, fmt.Errorf("the cancelled %s %s never reached a terminal status: %w", what, ref, err)
}

func churnCancelled(t controller.Task) bool {
	return t.CancelRequestedAt != nil && t.Error != nil && strings.Contains(strings.ToLower(*t.Error), "cancel")
}

func churnFailedTasks(ctx context.Context, cl *controller.Client, clusterID string) ([]controller.Task, error) {
	const page = 100
	var out []controller.Task
	for offset := 0; ; offset += page {
		list, err := cl.ListTasks(ctx, controller.ListTasksOptions{ClusterID: clusterID, Status: controller.TaskFailed, Limit: page, Offset: offset})
		if err != nil {
			return nil, err
		}
		out = append(out, list.Tasks...)
		if len(list.Tasks) < page || len(out) >= list.Total {
			return out, nil
		}
	}
}

// churnWrite writes and verifies the cycle's dataset through the endpoint — the
// proof that the branch this cycle created is a working database, not just a row
// in the registry.
func churnWrite(ctx context.Context, env ampControlPlaneEnv, ep controller.Endpoint) error {
	conn, err := env.ConnectEndpoint(ctx, ep)
	if err != nil {
		return fmt.Errorf("connect endpoint %s: %w", ep.ID, err)
	}
	defer conn.Close()
	if _, err := conn.Exec(ctx, "CREATE TABLE "+churnTable+" (id int primary key, payload text)"); err != nil {
		return fmt.Errorf("create table on endpoint %s: %w", ep.ID, err)
	}
	if _, err := conn.Exec(ctx, "INSERT INTO "+churnTable+" (id, payload) SELECT g, md5(g::text) FROM generate_series(1, $1) g", churnRows); err != nil {
		return fmt.Errorf("seed rows on endpoint %s: %w", ep.ID, err)
	}
	n, err := conn.QueryInt(ctx, "SELECT count(*) FROM "+churnTable)
	if err != nil {
		return fmt.Errorf("count rows on endpoint %s: %w", ep.ID, err)
	}
	if n != churnRows {
		return fmt.Errorf("the branch's endpoint %s serves %d rows, want the %d just written", ep.ID, n, churnRows)
	}
	return nil
}

// withChurnRetry retries the transient control-plane errors the churn's own
// concurrency produces: a registry DDL deadlock, and the non-idempotent endpoint
// helm deploy whose Service is not visible yet. Anything else is returned at once.
func withChurnRetry(ctx context.Context, run *churnRun, fn func() error) error {
	var lastErr error
	for attempt := 0; attempt <= churnRetries; attempt++ {
		if attempt > 0 {
			select {
			case <-ctx.Done():
				return ctx.Err()
			case <-time.After(time.Duration(attempt) * 3 * time.Second):
			}
		}
		lastErr = fn()
		if lastErr == nil || !transientChurnErr(lastErr) {
			return lastErr
		}
		run.retried.Add(1)
	}
	return fmt.Errorf("still failing after %d retries: %w", churnRetries, lastErr)
}

func transientChurnErr(err error) bool {
	s := err.Error()
	// "cannot determine a fork point": the controller resolves it from the
	// parent's safekeeper on :7676, which comes up a beat after PG does — a
	// fresh writer can serve SQL while its safekeeper still refuses HTTP. Under
	// churn the fork often lands in that beat. Retryable within the budget; if
	// it persists, the run still fails and the message names the port to chase.
	return strings.Contains(s, "deadlock") ||
		strings.Contains(s, "cannot determine a fork point") ||
		(strings.Contains(s, "not found") && strings.Contains(s, "yb-ep-"))
}

// findCluster resolves an asynchronously-created cluster by name (its id is not
// known until the create task succeeds).
func findCluster(ctx context.Context, cl *controller.Client, poolID, name string) (controller.Cluster, error) {
	const page = 100
	for offset := 0; ; offset += page {
		list, err := cl.ListClusters(ctx, poolID, page, offset)
		if err != nil {
			return controller.Cluster{}, fmt.Errorf("list clusters: %w", err)
		}
		for _, p := range list.Clusters {
			if p.Name == name {
				return p, nil
			}
		}
		if list.Count < page || offset+len(list.Clusters) >= list.Total {
			return controller.Cluster{}, fmt.Errorf("cluster %q not found after create", name)
		}
	}
}

// findBranch resolves an asynchronously-created branch by name.
func findBranch(ctx context.Context, cl *controller.Client, poolID, projID, name string) (controller.Branch, error) {
	branches, err := cl.ListAllBranches(ctx, poolID, projID)
	if err != nil {
		return controller.Branch{}, fmt.Errorf("list branches: %w", err)
	}
	for _, b := range branches {
		if b.Name == name {
			return b, nil
		}
	}
	return controller.Branch{}, fmt.Errorf("branch %q not found after create", name)
}
