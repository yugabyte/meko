package task

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"sync"
	"sync/atomic"
	"testing"

	"github.com/google/uuid"
)

// These cover SubtaskRunner.Run (subtask_runner.go): build → lock → run → record,
// with no step loop, no resume and no task lease. What the runner borrows from the
// worker (the abort retry, the panic boundary) is asserted here too, because
// borrowing it is the point — a runner that restated either would drift.

// countingLocks counts task-lease acquisitions, so a test can assert the subtask
// runner takes none: a subtask is not claimable, so a task lease on one would be
// a lease on a row that does not exist.
type countingLocks struct {
	*MemLocks
	mu         sync.Mutex
	taskLeases int
}

func (l *countingLocks) AcquireTask(ctx context.Context, taskID uuid.UUID) (Lease, error) {
	l.mu.Lock()
	l.taskLeases++
	l.mu.Unlock()
	return l.MemLocks.AcquireTask(ctx, taskID)
}

func (l *countingLocks) taskLeaseCount() int {
	l.mu.Lock()
	defer l.mu.Unlock()
	return l.taskLeases
}

// regFor registers one arbitrary Step under kind. regWith only takes *fakeStep,
// and these tests need steps that also implement Locking or panic.
func regFor(kind string, step Step) *Registry {
	r := NewRegistry()
	r.Register(kind, func(json.RawMessage, Deps) (Step, error) { return step, nil })
	return r
}

// runnerFixture is a claimed parent task plus a runner over its store.
type runnerFixture struct {
	store  *MemStore
	subs   *MemSubtasks
	locks  *countingLocks
	worker *Worker
	runner *SubtaskRunner
	parent *Task
	owner  string
}

func newRunnerFixture(t *testing.T, reg *Registry) *runnerFixture {
	t.Helper()
	store := NewMemStore()
	locks := &countingLocks{MemLocks: store.Locks(ownerID())}
	f := newRunnerFixtureWith(t, store, reg, locks)
	f.locks = locks
	return f
}

// newRunnerFixtureWith is newRunnerFixture over a caller-supplied lock table, for
// the tests that need an acquire to be refused. f.locks is nil on it.
func newRunnerFixtureWith(t *testing.T, store *MemStore, reg *Registry, locks Locks) *runnerFixture {
	t.Helper()
	w := testWorkerMax(store, locks, reg, 8)
	parent := claimedParent(t, store, w.owner)
	subs := store.Subtasks()
	return &runnerFixture{
		store:  store,
		subs:   subs,
		worker: w,
		runner: NewSubtaskRunner(w, subs),
		parent: parent,
		owner:  w.owner,
	}
}

// contendedLocks reports every acquire of a held key as ErrLockHeld, the way an
// exhausted wait does — with the caller's context still live, since the wait is
// bounded inside AcquireResource.
type contendedLocks struct {
	*MemLocks
	held map[string]struct{}
}

func (c *contendedLocks) AcquireResource(ctx context.Context, lock ResourceLock, holderID uuid.UUID, holderKind string) (Lease, error) {
	if _, contended := c.held[lock.Key()]; contended {
		return nil, fmt.Errorf("resource lock %s: %w", lock, ErrLockHeld)
	}
	return c.MemLocks.AcquireResource(ctx, lock, holderID, holderKind)
}

// TestSubtaskRunLeavesAContendedChildNonTerminal is §5.4's "a child that exhausts
// that wait is unattempted, not failed" at the layer that decides it.
//
// The wait is bounded INSIDE AcquireResource, so it runs out with the caller's
// context still live — which is why the shutdown guard cannot catch this and the
// error has to be classified. Recording `failed` here would be permanent: failed
// is terminal, a replaying parent does not re-attempt it, and Create hands the
// next parent the same row. So one endpoint held by a resize (or by a fenced-out
// predecessor that finished the deploy successfully) would stay unupgraded for
// good, from a lock wait that says nothing about whether the upgrade would work.
func TestSubtaskRunLeavesAContendedChildNonTerminal(t *testing.T) {
	const endpoint = "33333333-3333-3333-3333-333333333333"
	step := &lockingStep{fakeStep: fakeStep{kind: StepUpgradeEndpoint}, lock: EndpointLock(endpoint)}
	store := NewMemStore()
	locks := &contendedLocks{
		MemLocks: store.Locks(ownerID()),
		held:     map[string]struct{}{EndpointLock(endpoint).Key(): {}},
	}
	f := newRunnerFixtureWith(t, store, regFor(StepUpgradeEndpoint, step), locks)
	sub := f.create(t, StepUpgradeEndpoint)

	err := f.runner.Run(t.Context(), sub, f.owner)
	// Still an error, and still ErrLockHeld: that is what the parent classifies as
	// `unattempted` rather than halting the roll on.
	if !errors.Is(err, ErrLockHeld) {
		t.Fatalf("Run over a held endpoint = %v, want an error wrapping ErrLockHeld", err)
	}
	if got := f.reread(t); got.Status != StatusPending {
		t.Errorf("subtask status = %q, want pending: a failed row is terminal, so a replaying "+
			"parent would never re-attempt this endpoint", got.Status)
	}
	if n := step.callCount(); n != 0 {
		t.Errorf("the step ran %d times without its endpoint lock, want 0", n)
	}
}

// create records one child of the fixture's parent.
func (f *runnerFixture) create(t *testing.T, kind string) *Subtask {
	t.Helper()
	sub, err := f.runner.Create(t.Context(), f.parent.ID, kind, json.RawMessage(`{"endpoint_id":"e1"}`))
	if err != nil {
		t.Fatalf("Create subtask: %v", err)
	}
	return sub
}

func (f *runnerFixture) reread(t *testing.T) *Subtask { return onlySubtask(t, f.subs, f.parent.ID) }

// TestSubtaskRunStampsTheParentsOwner: the child's writes are fenced on the parent
// task's owner, so Run must record under the owner it was handed rather than
// anything of its own. Stamping the wrong one is not a cosmetic error — the fence
// refuses the write and the child is left pending with its work already done.
func TestSubtaskRunStampsTheParentsOwner(t *testing.T) {
	step := &fakeStep{kind: KindNoop}
	f := newRunnerFixture(t, regWith(step))
	sub := f.create(t, KindNoop)

	if err := f.runner.Run(t.Context(), sub, f.owner); err != nil {
		t.Fatalf("Run: %v", err)
	}

	got := f.reread(t)
	if got.Status != StatusSucceeded {
		t.Fatalf("subtask status = %q, want succeeded", got.Status)
	}
	if got.Owner == nil || *got.Owner != f.owner {
		t.Fatalf("subtask owner = %v, want the parent's owner %q", got.Owner, f.owner)
	}
	if string(got.Result) != `{"ok":true}` {
		t.Fatalf("subtask result = %q, want the step's output", got.Result)
	}
}

// TestSubtaskRunIsFencedOutByAReclaim: a process still limping along after its
// parent was taken over must not write the children the new owner is now driving.
// The step still runs — nothing here can stop it — but nothing it did is recorded
// under the stale owner.
func TestSubtaskRunIsFencedOutByAReclaim(t *testing.T) {
	step := &fakeStep{kind: KindNoop}
	f := newRunnerFixture(t, regWith(step))
	sub := f.create(t, KindNoop)
	stale := f.owner

	if claimed, _, err := f.store.Claim(t.Context(), f.parent.ID, "another-instance"); err != nil || claimed == nil {
		t.Fatalf("reclaim the parent: task=%v err=%v", claimed, err)
	}
	if err := f.runner.Run(t.Context(), sub, stale); err != nil {
		t.Fatalf("Run under a stale owner = %v, want nil (its writes are silently refused)", err)
	}

	got := f.reread(t)
	if got.Status != StatusPending || got.Owner != nil {
		t.Fatalf("subtask = status %q owner %v, want untouched by the fenced-out process",
			got.Status, got.Owner)
	}
}

// TestSubtaskRunRecordsAStepFailure: a child that fails must land in the store as
// failed with its cause, because the parent's report names the subtask id to go
// and read. Returning the error without recording it would leave the roll's one
// durable account of what went wrong empty.
func TestSubtaskRunRecordsAStepFailure(t *testing.T) {
	step := &fakeStep{kind: KindNoop, failWith: errors.New("helm --wait timed out")}
	f := newRunnerFixture(t, regWith(step))
	sub := f.create(t, KindNoop)

	err := f.runner.Run(t.Context(), sub, f.owner)
	if err == nil {
		t.Fatal("Run returned nil for a failing step, want the failure")
	}
	if !strings.Contains(err.Error(), "helm --wait timed out") {
		t.Errorf("Run error = %v, want it to carry the step's cause", err)
	}

	got := f.reread(t)
	if got.Status != StatusFailed {
		t.Fatalf("subtask status = %q, want failed", got.Status)
	}
	if got.Error == nil || !strings.Contains(*got.Error, "helm --wait timed out") {
		t.Fatalf("subtask error = %v, want the step's cause recorded", got.Error)
	}
	// Fail-fast is the parent step's decision, from this return value; the child
	// must not pre-empt it by failing the parent itself.
	parent, _ := f.store.Get(t.Context(), f.parent.ID)
	if parent.Status != StatusRunning {
		t.Fatalf("parent status = %q, want running", parent.Status)
	}
}

// TestSubtaskRunTakesNoTaskLease: a subtask is not in `tasks`, so a task lease on
// one names a row that does not exist — and taking one per child is the 2N-lease
// shape §5.2 exists to avoid. The parent's lease is the child's liveness.
func TestSubtaskRunTakesNoTaskLease(t *testing.T) {
	step := &fakeStep{kind: KindNoop}
	f := newRunnerFixture(t, regWith(step))
	sub := f.create(t, KindNoop)

	before := f.locks.taskLeaseCount()
	if err := f.runner.Run(t.Context(), sub, f.owner); err != nil {
		t.Fatalf("Run: %v", err)
	}
	if got := f.locks.taskLeaseCount() - before; got != 0 {
		t.Fatalf("task leases acquired by Run = %d, want 0", got)
	}
	if holder := f.store.locks.heldBy(taskLockKey(sub.ID)); holder != "" {
		t.Fatalf("task lease %s is held by %q, want no such lease", taskLockKey(sub.ID), holder)
	}
}

// TestSubtaskRunRecordsAPanicAsFailure: a panicking child must fail its own row,
// not take the process down. It matters more here than for a task's step — the
// runner is called from max_parallel goroutines, so an escaping panic would kill
// the roll and every other task in flight.
func TestSubtaskRunRecordsAPanicAsFailure(t *testing.T) {
	f := newRunnerFixture(t, regFor(panicStep{}.Kind(), panicStep{}))
	sub := f.create(t, panicStep{}.Kind())

	err := f.runner.Run(t.Context(), sub, f.owner)
	if err == nil {
		t.Fatal("Run returned nil for a panicking step, want the failure")
	}
	got := f.reread(t)
	if got.Status != StatusFailed {
		t.Fatalf("subtask status = %q, want failed", got.Status)
	}
	if got.Error == nil || !strings.Contains(*got.Error, "panicked") {
		t.Fatalf("subtask error = %v, want the panic recorded", got.Error)
	}
}

// TestSubtaskRunRetriesSerializationAbort: a child that loses one of the
// controller's write interlocks is re-run, not failed — the same contract a task's
// step has (#578), which the runner gets by borrowing runStepWithRetry rather than
// restating it.
func TestSubtaskRunRetriesSerializationAbort(t *testing.T) {
	step := &fakeStep{kind: KindNoop, failWith: abortErr(), failTimes: 2}
	f := newRunnerFixture(t, regWith(step))
	rec := &fakeRecorder{}
	f.worker.metrics = rec
	sub := f.create(t, KindNoop)

	if err := f.runner.Run(t.Context(), sub, f.owner); err != nil {
		t.Fatalf("Run: %v", err)
	}
	if got := step.callCount(); got != 3 {
		t.Fatalf("step calls = %d, want 3 (two aborts then success)", got)
	}
	if got := f.reread(t); got.Status != StatusSucceeded {
		t.Fatalf("subtask status = %q, want succeeded", got.Status)
	}
	// The retry metric is labelled by STEP kind, which is what subtasks.kind is, so
	// a child's retries are counted with every other run of that step.
	if len(rec.retried) != 2 {
		t.Fatalf("StepRetried calls = %d, want 2", len(rec.retried))
	}
}

// TestSubtaskRunFailsAnUnknownKind: a kind no Registry entry can build will never
// run, so the child fails rather than being retried forever by successive parents.
func TestSubtaskRunFailsAnUnknownKind(t *testing.T) {
	f := newRunnerFixture(t, NewRegistry())
	sub := f.create(t, "no_such_step")

	if err := f.runner.Run(t.Context(), sub, f.owner); err == nil {
		t.Fatal("Run returned nil for an unregistered kind, want the failure")
	}
	if got := f.reread(t); got.Status != StatusFailed {
		t.Fatalf("subtask status = %q, want failed", got.Status)
	}
}

// TestSubtaskRunLeavesAnInterruptedChildReclaimable: a shutdown is not a failure.
// The row must stay non-terminal so the reclaimed parent re-creates it, gets the
// same id back, and re-runs it on the step idempotency contract (§5.5 step 4).
func TestSubtaskRunLeavesAnInterruptedChildReclaimable(t *testing.T) {
	step := &fakeStep{kind: KindNoop}
	f := newRunnerFixture(t, regWith(step))
	sub := f.create(t, KindNoop)

	ctx, cancel := context.WithCancel(t.Context())
	cancel()
	if err := f.runner.Run(ctx, sub, f.owner); err == nil {
		t.Fatal("Run returned nil under a cancelled context, want an interruption error")
	}
	if got := f.reread(t); got.Status == StatusFailed {
		t.Fatal("subtask was durably failed by a shutdown, want it left reclaimable")
	}
}

// TestSubtaskRunRefusesASecondConcurrentRun is the in-process half of §5.2.
//
// The separate table closes the CLAIM path, but not this one: two concurrent Runs of
// one id compute an identical resourceOwner, and AcquireResource builds a fresh
// pglease.Client per call — so pglease's own reserve() is per-Client and cannot fire,
// and BOTH would hold a live lease over one resource_token row. That is two helm runs
// on one release, and a row a third task can then be handed mid-write.
//
// The realistic way in: Create returns the same row for a duplicate, so a planner that
// appends every Create result to its work list gets two entries for one row as soon as
// its enumeration yields an endpoint twice — a repeated id in a caller-supplied list.
func TestSubtaskRunRefusesASecondConcurrentRun(t *testing.T) {
	// inFlight/maxInFlight observe whether two executions ever overlap, so removing
	// the reservation fails this test fast rather than deadlocking it: the second Run
	// gets into the step, maxInFlight reaches 2, and the assertion below fires.
	var mu sync.Mutex
	var inFlight, maxInFlight int
	entered, release := make(chan struct{}), make(chan struct{})
	// Not sync.Once: Once.Do BLOCKS later callers until the first returns, which would
	// deadlock the very mutant this test exists to catch (the second execution would
	// park inside Do while the first waits on release). A counter lets a second
	// execution run straight through and be observed.
	var seq atomic.Int32
	step := &fakeStep{kind: KindNoop, before: func() {
		mu.Lock()
		inFlight++
		if inFlight > maxInFlight {
			maxInFlight = inFlight
		}
		mu.Unlock()
		if seq.Add(1) == 1 {
			close(entered)
			<-release
		}
		mu.Lock()
		inFlight--
		mu.Unlock()
	}}
	f := newRunnerFixture(t, regWith(step))
	sub := f.create(t, KindNoop)

	first := make(chan error, 1)
	go func() { first <- f.runner.Run(t.Context(), sub, f.owner) }()
	<-entered // the first Run is inside the step, holding the reservation

	err := f.runner.Run(t.Context(), sub, f.owner)
	if !errors.Is(err, ErrSubtaskRunning) {
		t.Errorf("second concurrent Run = %v, want ErrSubtaskRunning", err)
	}

	close(release)
	if err := <-first; err != nil {
		t.Fatalf("first Run: %v", err)
	}
	mu.Lock()
	peak := maxInFlight
	mu.Unlock()
	if peak > 1 {
		t.Errorf("%d executions of one subtask overlapped, want 1: two concurrent runs compute "+
			"an identical lease owner and both get a live Held over one resource_token row", peak)
	}
	// The reservation is for the duration of one Run, not the row's lifetime: once the
	// first finished, the id is runnable again (which a replaying parent relies on).
	if err := f.runner.Run(t.Context(), sub, f.owner); err != nil {
		t.Errorf("Run after the first finished = %v, want it runnable again", err)
	}
}
