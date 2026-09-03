package task

import (
	"context"
	"encoding/json"
	"errors"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/yugabyte/amp/controller/internal/core"
)

// These cover the fleet roll (step_plan_endpoint_upgrades.go): what it selects,
// what it creates, how it runs the children, and what it reports. The children
// are a stand-in for upgrade_endpoint rather than the real step — the roll's job
// is deciding WHICH endpoints move and driving them, and a double is what lets a
// test fail, block or contend one endpoint at a time.
//
// The step is executed directly rather than through Worker.Run: it needs only a
// claimed parent for the child write fence, and driving the claim loop would add
// timing to tests whose point is ordering.

// Fixed, ordered endpoint ids: the enumeration is sorted, so a roll with
// max_parallel 1 starts them in this order and a test can name "the first".
var (
	ep1 = uuid.MustParse("11111111-1111-1111-1111-111111111111")
	ep2 = uuid.MustParse("22222222-2222-2222-2222-222222222222")
	ep3 = uuid.MustParse("33333333-3333-3333-3333-333333333333")
)

// rollControl is what the children of one roll report into: which endpoints ran
// and how often, how many overlapped, and what each one should do.
type rollControl struct {
	mu       sync.Mutex
	runs     map[uuid.UUID]int
	inFlight int
	peak     int

	// failOn fails a named endpoint's child; hold, when non-nil, parks every child
	// inside Execute until it is closed.
	failOn map[uuid.UUID]error
	hold   chan struct{}
}

func newRollControl() *rollControl {
	return &rollControl{runs: map[uuid.UUID]int{}, failOn: map[uuid.UUID]error{}}
}

func (c *rollControl) enter(id uuid.UUID) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.runs[id]++
	c.inFlight++
	if c.inFlight > c.peak {
		c.peak = c.inFlight
	}
}

func (c *rollControl) exit() {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.inFlight--
}

func (c *rollControl) runCount(id uuid.UUID) int {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.runs[id]
}

func (c *rollControl) snapshot() (inFlight, peak int) {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.inFlight, c.peak
}

// rollStep stands in for upgrade_endpoint. It declares the same endpoint lock the
// real step does, so the runner's locking is exercised for real.
type rollStep struct {
	args UpgradeEndpointArgs
	ctl  *rollControl
}

func (*rollStep) Kind() string         { return StepUpgradeEndpoint }
func (s *rollStep) Lock() ResourceLock { return EndpointLock(s.args.EndpointID.String()) }
func (s *rollStep) Execute(ctx context.Context) StepResult {
	id := s.args.EndpointID
	s.ctl.enter(id)
	defer s.ctl.exit()
	if s.ctl.hold != nil {
		select {
		case <-s.ctl.hold:
		case <-ctx.Done():
			return Fail(ctx.Err())
		}
	}
	s.ctl.mu.Lock()
	err := s.ctl.failOn[id]
	s.ctl.mu.Unlock()
	if err != nil {
		return Fail(err)
	}
	return Ok(mustResult(map[string]string{"endpoint_id": id.String(), "version": s.args.Version}))
}

// rollRegistry builds one rollStep per child from that child's own args, the way
// the production registry builds the real step.
func rollRegistry(ctl *rollControl) *Registry {
	r := NewRegistry()
	r.Register(StepUpgradeEndpoint, func(raw json.RawMessage, _ Deps) (Step, error) {
		var args UpgradeEndpointArgs
		if err := json.Unmarshal(raw, &args); err != nil {
			return nil, err
		}
		return &rollStep{args: args, ctl: ctl}, nil
	})
	return r
}

type rollFixture struct {
	store  *MemStore
	subs   *MemSubtasks
	parent *Task
	owner  string
	state  *fakeStateStore
	deps   Deps
	ctl    *rollControl
}

// newRollFixture wires a claimed parent, a real SubtaskRunner over MemStore, and
// the seeded endpoints. locks may be nil for the store's own.
func newRollFixture(t *testing.T, ctl *rollControl, locks Locks, seed ...uuid.UUID) *rollFixture {
	t.Helper()
	store := NewMemStore()
	if locks == nil {
		locks = store.Locks(ownerID())
	}
	w := testWorkerMax(store, locks, rollRegistry(ctl), 8)
	parent := claimedParent(t, store, w.owner)
	state := newFakeStateStore()
	cluster, branch := uuid.New(), uuid.New()
	for _, id := range seed {
		state.seedEndpointDeploy(cluster, core.EndpointDeploy{ID: id, BranchID: branch, PgVersion: 18})
	}
	return &rollFixture{
		store:  store,
		subs:   store.Subtasks(),
		parent: parent,
		owner:  w.owner,
		state:  state,
		ctl:    ctl,
		deps: Deps{
			State:    state,
			Deployer: &fakeDeployer{},
			Subtasks: NewSubtaskRunner(w, store.Subtasks()),
		},
	}
}

// setVersion records what an endpoint's row claims it runs — desired state, which
// is what the whole-fleet filter reads.
func (f *rollFixture) setVersion(id uuid.UUID, version string) {
	if err := f.state.SetEndpointVersion(context.Background(), id, version); err != nil {
		panic(err)
	}
}

func (f *rollFixture) execute(t *testing.T, args PlanEndpointUpgradesArgs) StepResult {
	t.Helper()
	step := NewPlanEndpointUpgradesStep(args, f.deps)
	ctx := intoRunning(t.Context(), running{taskID: f.parent.ID, owner: f.owner})
	return step.Execute(ctx)
}

// rollReportOf decodes a succeeded roll's step result.
type decodedRoll struct {
	Version     string         `json:"version"`
	Endpoints   []endpointRoll `json:"endpoints"`
	Upgraded    int            `json:"upgraded"`
	Skipped     int            `json:"skipped"`
	Failed      int            `json:"failed"`
	Unattempted int            `json:"unattempted"`
}

func decodeRoll(t *testing.T, res StepResult) decodedRoll {
	t.Helper()
	if res.Failed() {
		t.Fatalf("roll failed: %v", res.Err)
	}
	var out decodedRoll
	if err := json.Unmarshal(res.Output, &out); err != nil {
		t.Fatalf("decode roll result %s: %v", res.Output, err)
	}
	return out
}

// lineFor returns the report line for one endpoint.
func lineFor(t *testing.T, lines []endpointRoll, id uuid.UUID) endpointRoll {
	t.Helper()
	for _, l := range lines {
		if l.EndpointID == id.String() {
			return l
		}
	}
	t.Fatalf("no report line for endpoint %s in %+v", id, lines)
	return endpointRoll{}
}

// TestFleetRollDedupesCallerSuppliedEndpointIDs.
//
// Create converges on a derived id, so a repeated id yields two work-list entries
// pointing at ONE subtask row. The runner refuses the second (ErrSubtaskRunning,
// §5.2) — which the roll would then report as a failure — and without that guard
// both would hold a live lease over one resource_token row and run two helms on
// one release. Deduping before creating is what makes the repetition harmless.
func TestFleetRollDedupesCallerSuppliedEndpointIDs(t *testing.T) {
	f := newRollFixture(t, newRollControl(), nil, ep1)

	got := decodeRoll(t, f.execute(t, PlanEndpointUpgradesArgs{
		Version:     "v2",
		EndpointIDs: []uuid.UUID{ep1, ep1, ep1},
		MaxParallel: 4,
	}))

	if len(got.Endpoints) != 1 || got.Upgraded != 1 {
		t.Fatalf("roll reported %d lines / %d upgraded for one endpoint named three times: %+v",
			len(got.Endpoints), got.Upgraded, got.Endpoints)
	}
	if n := f.ctl.runCount(ep1); n != 1 {
		t.Errorf("endpoint %s was deployed %d times, want 1", ep1, n)
	}
	rows, err := f.subs.ByParent(t.Context(), f.parent.ID)
	if err != nil {
		t.Fatalf("ByParent: %v", err)
	}
	if len(rows) != 1 {
		t.Errorf("subtask rows = %d, want 1 (the derived id converges; the work list must too)", len(rows))
	}
}

// TestFleetRollNeverSkipsAnExplicitlyNamedEndpoint is §5.1's carve-out, and it is
// load-bearing.
//
// `endpoints.version` records what was ASKED FOR, not what is running: the
// upgrade persists the target BEFORE deploying, so a child that failed its deploy
// leaves the row claiming the target with the pod still on the old version.
// Naming the endpoint is the operator's remedy after a partial roll — skipping it
// would drop exactly the endpoint that needs re-running.
func TestFleetRollNeverSkipsAnExplicitlyNamedEndpoint(t *testing.T) {
	f := newRollFixture(t, newRollControl(), nil, ep1)
	f.setVersion(ep1, "v2")

	got := decodeRoll(t, f.execute(t, PlanEndpointUpgradesArgs{
		Version:     "v2",
		EndpointIDs: []uuid.UUID{ep1},
		MaxParallel: 4,
	}))

	if line := lineFor(t, got.Endpoints, ep1); line.Outcome != rollUpgraded {
		t.Errorf("named endpoint already recorded at the target = %q (%s), want upgraded: the column says "+
			"what was asked for, not what is running", line.Outcome, line.Reason)
	}
	if n := f.ctl.runCount(ep1); n != 1 {
		t.Errorf("named endpoint was deployed %d times, want 1", n)
	}
}

// TestFleetRollSkipsFleetEndpointsAlreadyAtTheTarget: the same filter, where it
// does apply. A whole-fleet roll must not redeploy the endpoints already recorded
// at the target — that is a pod replacement each, for nothing.
func TestFleetRollSkipsFleetEndpointsAlreadyAtTheTarget(t *testing.T) {
	f := newRollFixture(t, newRollControl(), nil, ep1, ep2)
	f.setVersion(ep1, "v2")
	f.setVersion(ep2, "v1")

	got := decodeRoll(t, f.execute(t, PlanEndpointUpgradesArgs{Version: "v2", MaxParallel: 4}))

	if line := lineFor(t, got.Endpoints, ep1); line.Outcome != rollSkipped {
		t.Errorf("endpoint already at the target = %q, want skipped", line.Outcome)
	} else if !strings.Contains(line.Reason, "v2") {
		t.Errorf("skip reason = %q, want it to name the version", line.Reason)
	}
	if line := lineFor(t, got.Endpoints, ep2); line.Outcome != rollUpgraded {
		t.Errorf("endpoint behind the target = %q, want upgraded", line.Outcome)
	}
	if n := f.ctl.runCount(ep1); n != 0 {
		t.Errorf("the already-at-target endpoint was deployed %d times, want 0", n)
	}
	if got.Skipped != 1 || got.Upgraded != 1 {
		t.Errorf("counts = %d skipped / %d upgraded, want 1/1", got.Skipped, got.Upgraded)
	}
}

// TestFleetRollDoesNotHaltOnAContendedEndpoint is §5.4's exception to fail-fast.
//
// ErrLockHeld says somebody else holds the endpoint, not that the upgrade would
// fail — and the specific bad case is real: a predecessor process fenced out of a
// reclaimed parent keeps renewing its endpoint lease, finishes the deploy
// successfully, and the successor times out on the lock for an endpoint that IS
// at the target. Halting the whole fleet roll on that would amplify a narrow
// window into a stopped rollout.
func TestFleetRollDoesNotHaltOnAContendedEndpoint(t *testing.T) {
	store := NewMemStore()
	locks := &contendedLocks{
		MemLocks: store.Locks(ownerID()),
		held:     map[string]struct{}{EndpointLock(ep1.String()).Key(): {}},
	}
	f := newRollFixture(t, newRollControl(), locks, ep1, ep2, ep3)

	// max_parallel 1, so the contended endpoint is reached first and the two behind
	// it can only run if the roll did not halt.
	res := f.execute(t, PlanEndpointUpgradesArgs{Version: "v2", MaxParallel: 1})
	if !res.Failed() {
		t.Fatal("a roll that left an endpoint unattempted reported success")
	}

	for _, id := range []uuid.UUID{ep2, ep3} {
		if n := f.ctl.runCount(id); n != 1 {
			t.Errorf("endpoint %s was deployed %d times, want 1: a contended endpoint is unattempted, "+
				"not failed, and must not stop the roll", id, n)
		}
	}
	if n := f.ctl.runCount(ep1); n != 0 {
		t.Errorf("the contended endpoint was deployed %d times, want 0", n)
	}
}

// TestFleetRollStopsStartingChildrenAfterAFailure is fail-fast itself. Without
// the probe §7 drops, a typo'd version surfaces as a helm --wait timeout minutes
// in — stopping after the first is what keeps that from costing N × timeout and
// taking a slice of the fleet down on the way.
func TestFleetRollStopsStartingChildrenAfterAFailure(t *testing.T) {
	ctl := newRollControl()
	ctl.failOn[ep1] = errors.New("helm --wait timed out")
	f := newRollFixture(t, ctl, nil, ep1, ep2, ep3)

	res := f.execute(t, PlanEndpointUpgradesArgs{Version: "v2", MaxParallel: 1})
	if !res.Failed() {
		t.Fatal("a roll with a failed child reported success")
	}
	if !strings.Contains(res.Err.Error(), "1 failed") {
		t.Errorf("roll error = %v, want it to tally the outcomes", res.Err)
	}

	for _, id := range []uuid.UUID{ep2, ep3} {
		if n := f.ctl.runCount(id); n != 0 {
			t.Errorf("endpoint %s was deployed %d times after an earlier child failed, want 0", id, n)
		}
	}
	// The children behind the failure exist as pending rows: the roll's account has
	// to distinguish "did not work" from "was never tried".
	for _, row := range mustByParent(t, f.subs, f.parent.ID) {
		if row.Status == StatusRunning {
			t.Errorf("subtask %s is still running after the roll returned", row.ID)
		}
	}
}

// TestFleetRollRunsAtMostMaxParallelChildren: the bound is the whole safety
// argument for a fleet roll — an upgrade replaces the pod, so max_parallel is how
// much of the fleet is down at once. It is also the ONLY bound, since the
// children run in-process off the parent's single execution slot.
func TestFleetRollRunsAtMostMaxParallelChildren(t *testing.T) {
	const parallel = 2
	ctl := newRollControl()
	ctl.hold = make(chan struct{})
	ids := []uuid.UUID{ep1, ep2, ep3, uuid.MustParse("44444444-4444-4444-4444-444444444444")}
	f := newRollFixture(t, ctl, nil, ids...)

	done := make(chan StepResult, 1)
	go func() {
		done <- f.execute(t, PlanEndpointUpgradesArgs{Version: "v2", MaxParallel: parallel})
	}()

	// Wait for the pool to fill, then hold it there: an unbounded roll would have
	// every child inside Execute within the same window.
	deadline := time.Now().Add(5 * time.Second)
	for {
		if inFlight, _ := ctl.snapshot(); inFlight >= parallel {
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("the roll never reached max_parallel children in flight")
		}
		time.Sleep(time.Millisecond)
	}
	for until := time.Now().Add(100 * time.Millisecond); time.Now().Before(until); {
		if _, peak := ctl.snapshot(); peak > parallel {
			t.Fatalf("%d children ran at once, want at most max_parallel=%d", peak, parallel)
		}
		time.Sleep(time.Millisecond)
	}
	if _, peak := ctl.snapshot(); peak != parallel {
		t.Fatalf("peak children = %d, want exactly max_parallel=%d", peak, parallel)
	}

	close(ctl.hold)
	if got := decodeRoll(t, <-done); got.Upgraded != len(ids) {
		t.Errorf("upgraded = %d, want all %d", got.Upgraded, len(ids))
	}
}

// TestFleetRollSkipsAnAlreadySucceededChildOnReplay is §5.5: progress within a
// roll comes from the CHILD's status, never from the version column.
//
// The endpoints are named explicitly, so the already-at-target filter is off
// entirely — the only thing that can stop a second deploy here is the subtask row
// the first pass left succeeded. A reclaimed parent that re-filtered on the column
// instead would also drop the endpoints whose children FAILED, and report clean.
func TestFleetRollSkipsAnAlreadySucceededChildOnReplay(t *testing.T) {
	f := newRollFixture(t, newRollControl(), nil, ep1, ep2)
	args := PlanEndpointUpgradesArgs{
		Version:     "v2",
		EndpointIDs: []uuid.UUID{ep1, ep2},
		MaxParallel: 4,
	}

	if got := decodeRoll(t, f.execute(t, args)); got.Upgraded != 2 {
		t.Fatalf("first pass upgraded %d, want 2", got.Upgraded)
	}

	// The parent is reclaimed and its step re-runs from the top.
	got := decodeRoll(t, f.execute(t, args))
	if got.Upgraded != 2 {
		t.Errorf("replay upgraded = %d, want 2 (each child already succeeded)", got.Upgraded)
	}
	for _, id := range []uuid.UUID{ep1, ep2} {
		if n := f.ctl.runCount(id); n != 1 {
			t.Errorf("endpoint %s was deployed %d times across two passes, want 1", id, n)
		}
	}
	if rows := mustByParent(t, f.subs, f.parent.ID); len(rows) != 2 {
		t.Errorf("subtask rows after a replay = %d, want 2 (the derived id converges)", len(rows))
	}
}

// TestFleetRollWaitsForInFlightChildren is the orphan invariant (§5.5), which the
// schema cannot enforce: a step that returned without waiting would leave a child
// running under a task the worker is about to mark terminal — work nothing will
// ever finish or account for.
func TestFleetRollWaitsForInFlightChildren(t *testing.T) {
	ctl := newRollControl()
	ctl.failOn[ep1] = errors.New("helm --wait timed out")
	f := newRollFixture(t, ctl, nil, ep1, ep2, ep3)

	// max_parallel 3, so the two behind the failure are already in flight when it
	// halts the roll; the step must still wait them out.
	if res := f.execute(t, PlanEndpointUpgradesArgs{Version: "v2", MaxParallel: 3}); !res.Failed() {
		t.Fatal("a roll with a failed child reported success")
	}
	for _, row := range mustByParent(t, f.subs, f.parent.ID) {
		if row.Status == StatusRunning {
			t.Errorf("subtask %s is still running after the roll returned: a parent must not reach "+
				"terminal with a live child", row.ID)
		}
	}
}

// TestFleetRollRejectsAnOutOfRangeMaxParallel: 0 is rejected rather than read as
// unbounded, which would fire the whole fleet at once.
func TestFleetRollRejectsAnOutOfRangeMaxParallel(t *testing.T) {
	f := newRollFixture(t, newRollControl(), nil, ep1)
	for _, n := range []int{0, -1, MaxUpgradeParallel + 1} {
		res := f.execute(t, PlanEndpointUpgradesArgs{Version: "v2", MaxParallel: n})
		if !res.Failed() {
			t.Errorf("max_parallel %d was accepted, want a failure", n)
		}
		if n := f.ctl.runCount(ep1); n != 0 {
			t.Fatalf("an endpoint was deployed %d times by a rejected roll", n)
		}
	}
}

// TestFleetRollRefusesWithoutItsDeps: the roll fails as a whole rather than
// fanning out N children that each fail on the same missing collaborator.
func TestFleetRollRefusesWithoutItsDeps(t *testing.T) {
	f := newRollFixture(t, newRollControl(), nil, ep1)
	args := PlanEndpointUpgradesArgs{Version: "v2", MaxParallel: 4}

	for name, deps := range map[string]Deps{
		"no deployer": {State: f.state, Subtasks: f.deps.Subtasks},
		"no runner":   {State: f.state, Deployer: &fakeDeployer{}},
	} {
		step := NewPlanEndpointUpgradesStep(args, deps)
		ctx := intoRunning(t.Context(), running{taskID: f.parent.ID, owner: f.owner})
		if res := step.Execute(ctx); !res.Failed() {
			t.Errorf("%s: the roll succeeded, want a failure", name)
		}
	}

	// An empty version would clear every endpoint's pinned desired state.
	if res := f.execute(t, PlanEndpointUpgradesArgs{MaxParallel: 4}); !res.Failed() {
		t.Error("a roll with no target version succeeded, want a failure")
	}
	// Outside a worker there is no task to hang children off and no owner to fence
	// their writes on, so the step must refuse rather than guess.
	step := NewPlanEndpointUpgradesStep(args, f.deps)
	if res := step.Execute(t.Context()); !res.Failed() {
		t.Error("the roll ran with no executing task on the context, want a failure")
	}
}

// TestFleetRollSkipsAnEndpointThatNoLongerExists: the handler validates the
// request, not the fleet, so a stale id reaches the planner. A roll is never
// refused because one of its endpoints cannot move (§6).
func TestFleetRollSkipsAnEndpointThatNoLongerExists(t *testing.T) {
	f := newRollFixture(t, newRollControl(), nil, ep1)
	gone := uuid.New()

	got := decodeRoll(t, f.execute(t, PlanEndpointUpgradesArgs{
		Version:     "v2",
		EndpointIDs: []uuid.UUID{ep1, gone},
		MaxParallel: 4,
	}))

	if line := lineFor(t, got.Endpoints, gone); line.Outcome != rollSkipped {
		t.Errorf("a stale endpoint id = %q, want skipped", line.Outcome)
	}
	if line := lineFor(t, got.Endpoints, ep1); line.Outcome != rollUpgraded {
		t.Errorf("the live endpoint = %q, want upgraded", line.Outcome)
	}
}

// TestFleetRollReportsTheSubtaskToGoAndRead: the parent's line is a pointer, not
// the whole story — a failed roll records no step result at all, so the subtask id
// is how an operator reaches what actually happened.
func TestFleetRollReportsTheSubtaskToGoAndRead(t *testing.T) {
	f := newRollFixture(t, newRollControl(), nil, ep1)

	got := decodeRoll(t, f.execute(t, PlanEndpointUpgradesArgs{Version: "v2", MaxParallel: 4}))
	line := lineFor(t, got.Endpoints, ep1)
	row := onlySubtask(t, f.subs, f.parent.ID)
	if line.SubtaskID != row.ID.String() {
		t.Errorf("report names subtask %q, want the child's row %s", line.SubtaskID, row.ID)
	}
	if row.Kind != StepUpgradeEndpoint {
		t.Errorf("subtask kind = %q, want the STEP kind %q", row.Kind, StepUpgradeEndpoint)
	}
}

func mustByParent(t *testing.T, subs SubtaskStore, parentID uuid.UUID) []*Subtask {
	t.Helper()
	rows, err := subs.ByParent(t.Context(), parentID)
	if err != nil {
		t.Fatalf("ByParent: %v", err)
	}
	return rows
}
