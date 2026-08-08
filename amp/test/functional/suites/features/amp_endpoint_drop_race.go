package features

import (
	"context"
	"fmt"
	"strings"
	"time"

	"github.com/yugabyte/amp/test/functional/framework/apps/controller"
	"github.com/yugabyte/amp/test/functional/framework/core"
)

const endpointDropRaceTable = "amp_ep_drop_race"

// endpointDropRaceDelays is one round per entry: how long after the branch drop
// is planned the endpoint create goes out. The window this test aims at opens
// when the delete_branch step finishes enumerating the branch's endpoints and
// closes when it tombstones the branch row — and between those two the step
// retires a running compute, so it is seconds wide, not microseconds. A single
// fixed delay would bet the test on one guess about where inside that window the
// create lands, so the rounds spread across it: just inside, mid-retire, and
// late enough that the tombstone may already have landed (the create then 404s
// on the handler's own pre-check, which is a legal outcome, not a failure).
//
// Deliberately no zero delay. Firing both at once is a coin flip on which task
// the worker claims first, and the create-wins-the-claim ordering exercises a
// DIFFERENT seam: the create's record lands before the drop's enumeration, so
// the drop retires that endpoint and tombstones legitimately — and the create,
// still mid-flight, then deploys the compute the drop just uninstalled
// (`step_create_endpoint` deploys AFTER `EnsureEndpointRecord`, and
// `SetEndpointAddress` on the deleted row is a silent no-op). That leaves the
// same orphaned pod by a route no branch-row interlock can close, so it belongs
// to its own fix and its own test rather than making this one flaky.
var endpointDropRaceDelays = []time.Duration{750 * time.Millisecond, 2 * time.Second, 4 * time.Second}

// endpointDropRaceSettle bounds how long the orphaned-compute check waits for
// the cluster to converge. A pod that is still Terminating is not a leak, so the
// assertion polls rather than sampling once.
const endpointDropRaceSettle = 120 * time.Second

func init() {
	core.Register(core.Test{
		Name:     "features/amp_endpoint_drop_race",
		Suite:    "features",
		Category: core.Stress,
		Env:      core.EnvSpec{Sharing: core.Exclusive, Shape: core.SharedControlPlane},
		Tags:     []string{"regression", "nightly"},
		// Both sides of this race are control-plane statements, and what it
		// prevents is a compute the control plane can no longer see.
		OwningTeam:  core.TeamControlPlane,
		Description: "Race an endpoint create against a drop of its branch; assert a branch the API says is gone never leaves a compute pod running (the create-endpoint <-> branch-drop interlock).",
		Params:      []string{"functional", "env.ampstack", "apps.ybastack", "apps.yba", "apps.ybk8s"},
		Run:         runAmpEndpointDropRace,
	})
}

func runAmpEndpointDropRace(t *core.T) error {
	return runAmpFeature(t, func(a *ampFeatureRun) []phase {
		return []phase{{"endpoint_drop_race", a.endpointDropRace}}
	})
}

func (a *ampFeatureRun) endpointDropRace(ctx context.Context) error {
	cl, err := a.f.controllerClient(ctx)
	if err != nil {
		return err
	}
	return endpointDropRaceSteps(ctx, cl, a.proj, "ep-drop-race-"+a.f.suffix(),
		a.f.endpointSQL, a.f.branchComputePods, a.f.logf)
}

// branchComputeLister reports the compute the CLUSTER still has for a branch,
// keyed by the endpoint's deploy name (`yb-ep-<endpoint id>`) with the pod name
// and phase as the value. Empty means the branch has no compute left.
//
// It reads the cluster rather than the API on purpose: once a branch is
// tombstoned the API cannot see its endpoints at all (every projection joins
// `branches` and filters `dropped_at IS NULL`), so the API is exactly the wrong
// instrument for finding one that leaked.
type branchComputeLister func(ctx context.Context, branchID string) (map[string]string, error)

// endpointDropRaceSteps drives the race over the ampCtl, connector and compute
// lister seams.
//
// WHAT IT DEFENDS. Creating an endpoint and dropping its branch are two
// transactions that must not both commit. The drop does refuse a branch that
// still has endpoints — but it refuses by READING `NOT EXISTS (endpoints)`,
// while the create writes a row in a different table. Neither side writes what
// the other reads, so under snapshot isolation the two used to miss each other
// and both commit, leaving a live endpoint on a tombstoned branch.
// `EnsureEndpointRecord` now WRITES the branch row it depends on, so the two
// serialize and one of them has to lose.
//
// WHY IT NEEDS A REAL ENVIRONMENT. The forbidden state is invisible through the
// API: a leaked endpoint on a tombstoned branch does not appear in
// listEndpoints. It appears as a compute pod nobody owns — still serving, and
// which no cascade will ever enumerate to retire, while branch-drop GC is free
// to reclaim the pages and WAL underneath it. Seeing that takes the cluster.
//
// The assertion is the INVARIANT, not a winner. Either order is legal: the drop
// may lose (its endpoint guard refuses it, the branch stays) or win (the create
// fails), and which one happens is up to scheduling. What must never hold is the
// conjunction — the branch gone from the API AND a compute for it still running.
func endpointDropRaceSteps(
	ctx context.Context,
	cl ampCtl,
	proj ampCluster,
	namePrefix string,
	connect endpointConnector,
	branchCompute branchComputeLister,
	logf func(string, ...any),
) error {
	// Seed the parent: a fork needs a real LSN, and the write proves the branch
	// the rounds fork from is a working one rather than an empty shell.
	baseEP, err := createEndpoint(ctx, cl, proj.poolID, proj.projID, proj.branchID)
	if err != nil {
		return fmt.Errorf("create base endpoint: %w", err)
	}
	base, err := connect(ctx, baseEP)
	if err != nil {
		return fmt.Errorf("connect base endpoint: %w", err)
	}
	defer base.Close()
	if _, err := base.Exec(ctx, "CREATE TABLE "+endpointDropRaceTable+" (id int primary key, val text)"); err != nil {
		return fmt.Errorf("create table: %w", err)
	}
	if _, err := base.Exec(ctx, "INSERT INTO "+endpointDropRaceTable+" (id, val) VALUES (1, 'parent')"); err != nil {
		return fmt.Errorf("seed parent: %w", err)
	}
	forkLSN, err := currentLSN(ctx, base)
	if err != nil {
		return err
	}

	for i, delay := range endpointDropRaceDelays {
		name := fmt.Sprintf("%s-%d", namePrefix, i)
		if err := endpointDropRaceRound(ctx, cl, proj, name, forkLSN, delay, connect, branchCompute, logf); err != nil {
			return fmt.Errorf("round %d (endpoint create fired %s after the drop): %w", i, delay, err)
		}
	}

	// The parent is untouched by all of it: the rounds raced a child branch, and
	// a drop must never take its ancestor's data or compute with it.
	if n, err := base.QueryInt(ctx, "SELECT count(*) FROM "+endpointDropRaceTable+" WHERE val = 'parent'"); err != nil {
		return fmt.Errorf("re-read parent after the rounds: %w", err)
	} else if n != 1 {
		return fmt.Errorf("parent row damaged after the drop races (count=%d, want 1)", n)
	}
	return nil
}

// endpointDropRaceRound runs one race: fork a child that has a compute of its
// own, fire the drop, fire the create `delay` later, join both tasks, then
// assert the invariant and clean up whatever survived.
func endpointDropRaceRound(
	ctx context.Context,
	cl ampCtl,
	proj ampCluster,
	name, forkLSN string,
	delay time.Duration,
	connect endpointConnector,
	branchCompute branchComputeLister,
	logf func(string, ...any),
) error {
	// The child gets a read_write endpoint of its own (forkChildAt deploys one).
	// That is not scenery: it is what makes the drop's window wide enough to aim
	// at, since delete_branch has to retire that running compute between the
	// endpoint set it reads and the tombstone it writes.
	childConn, err := forkChildAt(ctx, cl, proj, proj.branchID, forkLSN, name, connect)
	if err != nil {
		return err
	}
	childConn.Close()

	child, err := findBranchCtl(ctx, cl, proj.poolID, proj.projID, name)
	if err != nil {
		return err
	}

	// Prove the lister can see this branch's compute BEFORE racing anything.
	// Without this the leak check is vacuous: a selector that matches nothing
	// reports "no orphaned compute" for a branch that leaked every pod it had.
	seeded, err := branchCompute(ctx, child.BranchID)
	if err != nil {
		return fmt.Errorf("list compute for the freshly forked branch %s: %w", child.BranchID, err)
	}
	if len(seeded) == 0 {
		return fmt.Errorf("no compute pod found for branch %s right after its endpoint was created: "+
			"the leak check below reads the cluster for pods belonging to a branch, and it cannot see the one "+
			"pod that certainly exists — so it would report any leak as clean", child.BranchID)
	}

	// Fire the drop, then the create. Both return 202 + Task, and the controller
	// executes tasks concurrently (task-max-concurrent defaults to 50), so the two
	// really are in flight together against the same branch row.
	dropTask, _, err := cl.DeleteBranch(ctx, proj.poolID, proj.projID, child.BranchID)
	if err != nil {
		return fmt.Errorf("plan the branch drop: %w", err)
	}
	select {
	case <-ctx.Done():
		return ctx.Err()
	case <-time.After(delay):
	}

	// read_only deliberately: the child already holds the branch's one permitted
	// writer, and a second read_write would be rejected by
	// endpoints_one_writer_per_branch before it ever reached the interlock under
	// test. A synchronous error here is a legal outcome rather than a failure —
	// it means the drop got far enough that the handler's own branch pre-check
	// 404s.
	createTask, _, createErr := cl.CreateEndpoint(ctx, proj.poolID, proj.projID, child.BranchID,
		controller.CreateEndpointRequest{Role: roleReadOnly})
	if createErr != nil {
		logf("endpoint create refused synchronously (the drop won the pre-check): %v", createErr)
	}

	dropStatus := joinRaceTask(ctx, cl, dropTask, "delete_branch", logf)
	createStatus := joinRaceTask(ctx, cl, createTask, "create_endpoint", logf)
	logf("race on branch %s: drop=%s create=%s", child.BranchID, dropStatus, createStatus)

	gone, err := branchAbsent(ctx, cl, proj.poolID, proj.projID, child.BranchID)
	if err != nil {
		return err
	}
	if !gone {
		// The drop lost — legal. The create committed first, the drop's endpoint
		// guard refused it, and the branch is still there with its endpoints.
		// Nothing can have leaked: the branch is visible, so every compute on it
		// is still enumerable and the cascade can retire it.
		return dropRaceCleanup(ctx, cl, proj, child.BranchID, logf)
	}

	// The drop won, so the branch is gone from every reader — and nothing in the
	// control plane can enumerate a compute left behind on it. Every compute for
	// this branch must therefore be gone too: the one the fork deployed, and any
	// the racing create deployed.
	return assertNoOrphanedCompute(ctx, branchCompute, child.BranchID, createStatus)
}

// joinRaceTask waits out a planned task and reports its terminal status. A task
// that FAILS is a legal outcome here — exactly one of the two racers is expected
// to lose — so a failure is logged and returned as a status, not raised. Only
// losing the task itself (transport, timeout, a cancelled context) yields the
// "unknown" status, which the caller reports in its diagnosis.
func joinRaceTask(ctx context.Context, cl ampCtl, task controller.Task, kind string, logf func(string, ...any)) string {
	if task.ID == "" {
		return "not-planned"
	}
	done, err := cl.WaitTask(ctx, task.ID)
	if done.Status != "" {
		if err != nil {
			logf("%s task %s finished %s: %v", kind, task.ID, done.Status, err)
		}
		return done.Status
	}
	logf("could not read %s task %s to completion: %v", kind, task.ID, err)
	return "unknown"
}

// assertNoOrphanedCompute is the invariant: the branch is gone from the API, so
// every compute that belonged to it must be gone from the cluster. It polls
// rather than sampling once, because a pod that is still Terminating is not yet
// a leak.
func assertNoOrphanedCompute(ctx context.Context, branchCompute branchComputeLister, branchID, createStatus string) error {
	var orphans []string
	err := pollUntil(ctx, endpointDropRaceSettle, 3*time.Second, func() (bool, error) {
		live, err := branchCompute(ctx, branchID)
		if err != nil {
			return false, fmt.Errorf("list compute for branch %s: %w", branchID, err)
		}
		orphans = orphans[:0]
		for instance, pod := range live {
			orphans = append(orphans, instance+" ("+pod+")")
		}
		return len(orphans) == 0, nil
	})
	if err == nil {
		return nil
	}
	if len(orphans) == 0 {
		return fmt.Errorf("checking for orphaned compute after the branch drop: %w", err)
	}
	return fmt.Errorf(
		"branch %s is gone from the API but %d compute pod(s) for it are still running %s after the drop "+
			"settled: %s (the racing endpoint create ended %s). This is the create-endpoint <-> branch-drop "+
			"write skew: the create's row landed inside the drop's window and both committed, so the endpoint "+
			"now sits on a tombstoned branch — invisible to every `dropped_at IS NULL` reader, so no cascade "+
			"will ever enumerate it to retire it, while its compute keeps serving and branch-drop GC is free "+
			"to reclaim the pages and WAL underneath it",
		branchID, len(orphans), endpointDropRaceSettle, strings.Join(orphans, ", "), createStatus)
}

// dropRaceCleanup retires what a lost drop left behind, so the next round starts
// from the same shape and the namespace does not accumulate computes. Best
// effort: the cluster-delete cascade in the shared teardown is the backstop, so a
// cleanup failure is logged rather than failing a round that already proved its
// invariant.
func dropRaceCleanup(ctx context.Context, cl ampCtl, proj ampCluster, branchID string, logf func(string, ...any)) error {
	eps, err := cl.ListAllEndpoints(ctx, proj.poolID, proj.projID, branchID)
	if err != nil {
		logf("cleanup: list endpoints on %s: %v", branchID, err)
		return nil
	}
	for _, ep := range eps {
		if _, err := cl.DeleteEndpointAndWait(ctx, proj.poolID, proj.projID, branchID, ep.ID); err != nil {
			logf("cleanup: delete endpoint %s: %v", ep.ID, err)
			return nil
		}
	}
	if _, err := cl.DeleteBranchAndWait(ctx, proj.poolID, proj.projID, branchID); err != nil {
		logf("cleanup: delete branch %s: %v", branchID, err)
	}
	return nil
}

// branchComputePods is the live branchComputeLister: it finds the branch's
// endpoints in the CLUSTER and reports which of them still have a pod.
//
// An endpoint's release carries its branch in its ConfigMap (`BRANCH_ID`, the
// flat 32-hex rendering of the branch uuid), and every object in the release
// carries `app.kubernetes.io/instance: yb-ep-<endpoint id>`. So the branch's
// endpoints are the endpoint ConfigMaps whose BRANCH_ID matches, and their
// compute is whatever pods carry the matching instance label. Both reads use a
// `range` template rather than an index-aligned pair of lists, so an endpoint
// with an empty BRANCH_ID cannot shift every later row onto the wrong branch.
func (f *flow) branchComputePods(ctx context.Context, branchID string) (map[string]string, error) {
	kube := f.ampKube()
	const endpointSelector = "app.kubernetes.io/name=yb-amp-endpoint"
	cms, err := kube.GetSelector(ctx, "configmap", endpointSelector,
		`{range .items[*]}{.metadata.name}={.data.BRANCH_ID}{"\n"}{end}`)
	if err != nil {
		return nil, fmt.Errorf("list endpoint configmaps: %w", err)
	}
	want := flatBranchID(branchID)
	out := map[string]string{}
	for _, line := range strings.Split(cms, "\n") {
		instance, branch, ok := strings.Cut(strings.TrimSpace(line), "=")
		if !ok || instance == "" || flatBranchID(branch) != want {
			continue
		}
		pods, err := kube.GetSelector(ctx, "pod", "app.kubernetes.io/instance="+instance,
			`{range .items[*]}{.metadata.name}={.status.phase}{"\n"}{end}`)
		if err != nil {
			return nil, fmt.Errorf("list pods for endpoint %s: %w", instance, err)
		}
		for _, podLine := range strings.Split(pods, "\n") {
			pod, phase, ok := strings.Cut(strings.TrimSpace(podLine), "=")
			if !ok || pod == "" {
				continue
			}
			out[instance] = pod + " " + phase
		}
	}
	return out, nil
}

// flatBranchID normalizes a branch id for comparison: the control plane's API
// speaks the dashed uuid, the storage layer (and so the endpoint's BRANCH_ID)
// speaks the flat 32-hex rendering of the same 16 bytes.
func flatBranchID(id string) string {
	return strings.ToLower(strings.ReplaceAll(strings.TrimSpace(id), "-", ""))
}
