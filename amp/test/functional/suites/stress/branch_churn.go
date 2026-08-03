// branch_churn is the control-plane churn stress scenario: several lanes run the
// full branch lifecycle — fork a branch, deploy an endpoint on it, write and
// verify a dataset, retire the endpoint, delete the branch — over and over, in
// parallel, against one project. It is the counterpart to the data-plane load
// tests: nothing here is about throughput, it is about whether the controller's
// asynchronous create/delete tasks stay correct and leak nothing when the same
// resource ladder is built and torn down concurrently, hundreds of tasks deep.
//
// It runs on a STANDING amp control plane (the sharedk8s project-per-test env
// exposes one) rather than self-provisioning: churning branches needs the control
// plane, not a bare YugabyteDB backing, and it skips cleanly on any env that does
// not offer one. Everything it creates hangs off a single project, whose delete
// cascade is registered before the first lane starts, so a killed run leaves
// nothing behind.
package stress

import (
	"context"
	"fmt"
	"log"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/yugabyte/amp/test/functional/framework/apps/controller"
	"github.com/yugabyte/amp/test/functional/framework/apps/pg"
	"github.com/yugabyte/amp/test/functional/framework/core"
)

const (
	// churnLanes is how many lifecycles run concurrently against the one project.
	churnLanes = 3
	// churnCycles is how many lifecycles each lane completes in sequence.
	churnCycles = 4
	// churnRows is the dataset each cycle writes to its branch and reads back
	// before retiring the endpoint.
	churnRows = 200
	// churnTable is the per-cycle table name (each cycle owns its own branch, so
	// the name never collides).
	churnTable = "amp_churn"
	// churnTeardownBudget bounds the project-cascade cleanup, independent of the
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
		Env:         core.EnvSpec{Sharing: core.Exclusive, Shape: core.SharedControlPlane},
		Tags:        []string{"stress", "nightly"},
		OwningTeam:  core.TeamControlPlane,
		Description: "Churn the branch lifecycle (fork, endpoint, write, retire, delete) in parallel lanes against one project and assert every cycle converged, nothing was orphaned, and no task failed.",
		Params:      []string{"env.sharedk8s"},
		Run:         runBranchChurn,
	})
}

// ampControlPlaneEnv is the capability this test depends on: a standing amp
// control plane it can create a project on and reach the endpoints of. The
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
}

// runBranchChurn creates one project, runs churnLanes × churnCycles branch
// lifecycles across it, and then asserts the control plane is exactly where it
// started: only the default branch left, no endpoint anywhere, no failed task
// recorded against the project, and the controller still ready.
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

	var proj controller.Project
	if err := t.Step("create_project", func(s *core.StepHandle) error {
		name := "churn-" + suffix(t)
		s.Set("project", name)
		if _, err := cl.CreateProjectAndWait(t.Context, poolID, controller.CreateProjectRequest{Name: name}); err != nil {
			return err
		}
		p, err := findProject(t.Context, cl, poolID, name)
		if err != nil {
			return err
		}
		proj = p
		td.push("delete churn project (cascade)", func(ctx context.Context) error {
			_, e := cl.DeleteProjectAndWait(ctx, poolID, proj.ID)
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
		t.Metric("churn_cycles", float64(churnLanes*churnCycles), "count")
		return nil
	}); err != nil {
		return err
	}

	return t.Step("verify_no_orphans", func(s *core.StepHandle) error {
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
			return fmt.Errorf("after %d branch lifecycles the project lists %d branch(es) (%v), want only the default branch — every churned branch was deleted and waited on, so anything left is an orphan",
				churnLanes*churnCycles, len(branches), names)
		}
		eps, err := cl.ListAllEndpoints(t.Context, poolID, proj.ID, proj.DefaultBranch.BranchID)
		if err != nil {
			return fmt.Errorf("list endpoints after the churn: %w", err)
		}
		if len(eps) != 0 {
			return fmt.Errorf("the default branch lists %d endpoint(s) after the churn, want 0 — the churn only ever created endpoints on its own branches", len(eps))
		}
		failed, err := cl.ListTasks(t.Context, controller.ListTasksOptions{ProjectID: proj.ID, Status: controller.TaskFailed, Limit: 50})
		if err != nil {
			return fmt.Errorf("list failed tasks after the churn: %w", err)
		}
		// Each transient the lanes retried past left its own failed task, so the
		// bar is "no failure the churn did not already observe and recover from".
		retried := int(run.retried.Load())
		s.Set("failed_tasks", failed.Total)
		if failed.Total > retried {
			kinds := make([]string, 0, len(failed.Tasks))
			for _, tk := range failed.Tasks {
				kinds = append(kinds, tk.Kind)
			}
			return fmt.Errorf("the project has %d failed task(s) after the churn (%v) but the lanes only retried past %d transient error(s), so at least one lifecycle failed unobserved", failed.Total, kinds, retried)
		}
		if err := cl.Readyz(t.Context); err != nil {
			return fmt.Errorf("the controller is not ready after the churn: %w", err)
		}
		return nil
	})
}

// runChurnLane runs one lane's cycles in sequence: fork a branch, deploy an
// endpoint on it, write and read back a dataset, retire the endpoint, delete the
// branch — asserting each step converged before moving on. It probes controller
// readiness every cycle so a control plane that degrades under the churn is
// caught while it is happening, not only at the end.
func runChurnLane(ctx context.Context, t *core.T, run *churnRun, env ampControlPlaneEnv, cl *controller.Client, poolID string, proj controller.Project, lane int, logf func(string, ...any)) error {
	for cycle := 0; cycle < churnCycles; cycle++ {
		name := fmt.Sprintf("churn-l%d-c%d-%s", lane, cycle, suffix(t))
		if err := churnCycle(ctx, run, env, cl, poolID, proj, name); err != nil {
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
func churnCycle(ctx context.Context, run *churnRun, env ampControlPlaneEnv, cl *controller.Client, poolID string, proj controller.Project, name string) error {
	if err := withChurnRetry(ctx, run, func() error {
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
	if err := withChurnRetry(ctx, run, func() error {
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

// findProject resolves an asynchronously-created project by name (its id is not
// known until the create task succeeds).
func findProject(ctx context.Context, cl *controller.Client, poolID, name string) (controller.Project, error) {
	const page = 100
	for offset := 0; ; offset += page {
		list, err := cl.ListProjects(ctx, poolID, page, offset)
		if err != nil {
			return controller.Project{}, fmt.Errorf("list projects: %w", err)
		}
		for _, p := range list.Projects {
			if p.Name == name {
				return p, nil
			}
		}
		if list.Count < page || offset+len(list.Projects) >= list.Total {
			return controller.Project{}, fmt.Errorf("project %q not found after create", name)
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
