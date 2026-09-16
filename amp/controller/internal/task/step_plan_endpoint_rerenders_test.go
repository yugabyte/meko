package task

import (
	"encoding/json"
	"errors"
	"testing"

	"github.com/google/uuid"

	"github.com/yugabyte/amp/controller/internal/core"
	"github.com/yugabyte/amp/controller/internal/extensions"
)

// rerenderFixture drives the planner over the REAL rerender_endpoint child and
// the real SubtaskRunner, so the whole path a roll takes — a child per endpoint,
// each taking its own endpoint lock — is exercised rather than stubbed.
type rerenderFixture struct {
	store  *MemStore
	parent *Task
	owner  string
	state  *fakeStateStore
	dep    *fakeDeployer
	deps   Deps
}

func newRerenderFixture(t *testing.T, seed ...uuid.UUID) *rerenderFixture {
	t.Helper()
	store := NewMemStore()
	state, dep := newFakeStateStore(), &fakeDeployer{}

	// The registry the runner rebuilds each child through. The child needs no
	// subtask runner of its own, so this deps is the planner's minus that.
	childDeps := Deps{State: state, Deployer: dep, Extensions: extensions.NewService(extensions.NewCatalog("quay.io/yugabyte"), nil)}
	w := testWorkerMax(store, store.Locks(ownerID()), ProductionRegistry(childDeps), 8)
	parent := claimedParent(t, store, w.owner)

	cluster, branch := uuid.New(), uuid.New()
	for _, id := range seed {
		state.seedEndpointDeploy(cluster, core.EndpointDeploy{ID: id, BranchID: branch, PgVersion: 18})
	}
	deps := childDeps
	deps.Subtasks = NewSubtaskRunner(w, store.Subtasks())
	return &rerenderFixture{store: store, parent: parent, owner: w.owner, state: state, dep: dep, deps: deps}
}

func (f *rerenderFixture) execute(t *testing.T, args PlanEndpointRerendersArgs) StepResult {
	t.Helper()
	step := NewPlanEndpointRerendersStep(args, f.deps)
	ctx := intoRunning(t.Context(), running{taskID: f.parent.ID, owner: f.owner})
	return step.Execute(ctx)
}

type decodedRerender struct {
	Endpoints   []endpointRoll `json:"endpoints"`
	Rerendered  int            `json:"rerendered"`
	Skipped     int            `json:"skipped"`
	Failed      int            `json:"failed"`
	Unattempted int            `json:"unattempted"`
}

func decodeRerender(t *testing.T, res StepResult) decodedRerender {
	t.Helper()
	if res.Failed() {
		t.Fatalf("roll failed: %v", res.Err)
	}
	var out decodedRerender
	if err := json.Unmarshal(res.Output, &out); err != nil {
		t.Fatalf("decode roll result %s: %v", res.Output, err)
	}
	return out
}

// THE regression test. An earlier attempt made each endpoint its own locked
// STEP of one task, which the worker refuses at the second step: a task may hold
// one resource lock. Only the subtask fan-out gets a lock per endpoint, so a roll
// of more than one endpoint is the thing worth pinning.
func TestRerenderRollCoversEveryEndpoint(t *testing.T) {
	ep1, ep2, ep3 := uuid.New(), uuid.New(), uuid.New()
	f := newRerenderFixture(t, ep1, ep2, ep3)

	got := decodeRerender(t, f.execute(t, PlanEndpointRerendersArgs{MaxParallel: 2}))
	if got.Rerendered != 3 || got.Failed != 0 || got.Unattempted != 0 {
		t.Fatalf("roll = %d rerendered, %d failed, %d unattempted; want 3/0/0", got.Rerendered, got.Failed, got.Unattempted)
	}
	if len(f.dep.created) != 3 {
		t.Fatalf("deploy calls = %d, want one per endpoint", len(f.dep.created))
	}
	for _, id := range []uuid.UUID{ep1, ep2, ep3} {
		if line := lineFor(t, got.Endpoints, id); line.Outcome != rollRerendered {
			t.Errorf("endpoint %s: outcome %q (%s), want %s", id, line.Outcome, line.Reason, rollRerendered)
		}
	}
	// Every child is a subtask row under the parent, not a task of its own.
	kids, err := f.store.Subtasks().ByParent(t.Context(), f.parent.ID)
	if err != nil {
		t.Fatalf("read children: %v", err)
	}
	if len(kids) != 3 {
		t.Fatalf("subtasks = %d, want 3", len(kids))
	}
}

// A named subset rolls only those endpoints; the rest of the fleet is untouched.
func TestRerenderRollHonoursANamedSubset(t *testing.T) {
	ep1, ep2 := uuid.New(), uuid.New()
	f := newRerenderFixture(t, ep1, ep2)

	got := decodeRerender(t, f.execute(t, PlanEndpointRerendersArgs{EndpointIDs: []uuid.UUID{ep2}, MaxParallel: 4}))
	if got.Rerendered != 1 {
		t.Fatalf("rerendered = %d, want 1", got.Rerendered)
	}
	if len(f.dep.created) != 1 || f.dep.created[0].EndpointID != ep2.String() {
		t.Fatalf("deployed %d endpoint(s), first %v; want only %s", len(f.dep.created), f.dep.created, ep2)
	}
}

// An id that resolves to nothing is a skipped line, never a refusal of the roll.
func TestRerenderRollSkipsAnUnknownEndpoint(t *testing.T) {
	ep1, gone := uuid.New(), uuid.New()
	f := newRerenderFixture(t, ep1)

	got := decodeRerender(t, f.execute(t, PlanEndpointRerendersArgs{
		EndpointIDs: []uuid.UUID{ep1, gone}, MaxParallel: 4,
	}))
	if got.Rerendered != 1 || got.Skipped != 1 {
		t.Fatalf("roll = %d rerendered, %d skipped; want 1/1", got.Rerendered, got.Skipped)
	}
	if line := lineFor(t, got.Endpoints, gone); line.Outcome != rollSkipped {
		t.Errorf("missing endpoint: outcome %q, want %s", line.Outcome, rollSkipped)
	}
}

// max_parallel is validated in the step as well as the handler, so a replayed
// task with a bad value fails cleanly instead of running unbounded.
func TestRerenderRollRejectsAnOutOfRangeParallelism(t *testing.T) {
	f := newRerenderFixture(t, uuid.New())
	for _, n := range []int{0, -1, MaxRollParallel + 1} {
		if res := f.execute(t, PlanEndpointRerendersArgs{MaxParallel: n}); !res.Failed() {
			t.Errorf("max_parallel %d was accepted, want a failure", n)
		}
	}
}

// A roll refuses to run without the collaborators every child needs, rather than
// fanning out N children that each fail on the same missing dep.
func TestRerenderRollRequiresItsCollaborators(t *testing.T) {
	f := newRerenderFixture(t, uuid.New())

	noDeployer := f.deps
	noDeployer.Deployer = nil
	if res := NewPlanEndpointRerendersStep(PlanEndpointRerendersArgs{MaxParallel: 4}, noDeployer).
		Execute(intoRunning(t.Context(), running{taskID: f.parent.ID, owner: f.owner})); !res.Failed() {
		t.Error("planned a roll with no deployer, want a failure")
	}
	noRunner := f.deps
	noRunner.Subtasks = nil
	if res := NewPlanEndpointRerendersStep(PlanEndpointRerendersArgs{MaxParallel: 4}, noRunner).
		Execute(intoRunning(t.Context(), running{taskID: f.parent.ID, owner: f.owner})); !res.Failed() {
		t.Error("planned a roll with no subtask runner, want a failure")
	}
}

// The two roll kinds contend for one fleet lock, so the service refuses a
// re-render while an upgrade is in flight. Without this the two would
// helm-upgrade the same release from two parents.
func TestRerenderEndpointsRefusedWhileAnUpgradeRolls(t *testing.T) {
	store := NewMemStore()
	svc := NewService(store, store.Subtasks())

	if _, err := svc.UpgradeEndpoints(t.Context(), UpgradeEndpointsInput{Version: "ep-v2", MaxParallel: 4}); err != nil {
		t.Fatalf("plan the upgrade: %v", err)
	}
	_, err := svc.RerenderEndpoints(t.Context(), RerenderEndpointsInput{MaxParallel: 4})
	if err == nil {
		t.Fatal("a re-render was planned while an upgrade rolls, want ErrFleetRollInProgress")
	}
	if !errors.Is(err, ErrFleetRollInProgress) {
		t.Fatalf("err = %v, want ErrFleetRollInProgress", err)
	}
}

// And the other direction: the gate has to be symmetric, or an upgrade
// requested during a re-render gets a 202 and then dies in the worker on the
// no-wait fleet lock.
func TestUpgradeEndpointsRefusedWhileARerenderRolls(t *testing.T) {
	store := NewMemStore()
	svc := NewService(store, store.Subtasks())

	if _, err := svc.RerenderEndpoints(t.Context(), RerenderEndpointsInput{MaxParallel: 4}); err != nil {
		t.Fatalf("plan the re-render: %v", err)
	}
	_, err := svc.UpgradeEndpoints(t.Context(), UpgradeEndpointsInput{Version: "ep-v2", MaxParallel: 4})
	if err == nil {
		t.Fatal("an upgrade was planned while a re-render rolls, want ErrFleetRollInProgress")
	}
	if !errors.Is(err, ErrFleetRollInProgress) {
		t.Fatalf("err = %v, want ErrFleetRollInProgress", err)
	}
}
