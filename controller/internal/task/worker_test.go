package task

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"log/slog"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/yugabyte/amp/controller/internal/config"
)

// These tests cover the Worker (worker.go) against the in-memory MemStore and
// fakeSteps. The per-task execution scenarios (amp-controller.md §8) drive
// execute directly after a real claim — a task runs its ordered steps to
// completion, a reclaimed task resumes at the first non-succeeded step (skipping
// finished ones), an in-flight running step is replayed, a step error fails the
// task and halts later steps, an unknown step kind fails it, and a cancelled
// context leaves it reclaimable. The claim loop itself (concurrency, backpressure,
// graceful stop) is covered by the Run tests at the bottom.

func testWorker(store Store, reg *Registry) *Worker {
	return testWorkerMax(store, reg, 8)
}

func testWorkerMax(store Store, reg *Registry, maxConcurrent int) *Worker {
	cfg := config.TaskConfig{
		PollInterval:  time.Millisecond,
		LeaseTTL:      30 * time.Second,
		RenewInterval: 10 * time.Second,
		MaxConcurrent: maxConcurrent,
		ShutdownGrace: 5 * time.Second,
	}
	return NewWorker(store, reg, cfg, slog.New(slog.NewTextHandler(io.Discard, nil)))
}

// regWith registers each fakeStep under its kind. The builder ignores args/deps
// and hands back the same instance the test holds, so the worker's per-execution
// rebuild still funnels calls into one recorder.
func regWith(steps ...*fakeStep) *Registry {
	r := NewRegistry()
	for _, s := range steps {
		r.Register(s.kind, func(json.RawMessage, Deps) (Step, error) { return s, nil })
	}
	return r
}

// pendingTask builds a pending task with steps of the given kinds, all pending.
// Each step gets an id; the task lists them in order (StepIDs).
func pendingTask(kinds ...string) (*Task, []*StepRecord) {
	id := uuid.New()
	now := time.Now()
	t := &Task{ID: id, Kind: "test", Status: StatusPending, CreatedAt: now, UpdatedAt: now}
	var steps []*StepRecord
	for _, k := range kinds {
		stID := uuid.New()
		t.StepIDs = append(t.StepIDs, stID)
		steps = append(steps, &StepRecord{ID: stID, TaskID: id, Kind: k, Status: StatusPending})
	}
	return t, steps
}

// claimAndExecute claims the next runnable task and runs it synchronously, for
// the per-task scenarios. It fails the test if nothing is claimable. The
// resulting task/step state is read back from the store by each test.
func claimAndExecute(t *testing.T, w *Worker, store Store) {
	t.Helper()
	claimed, err := store.ClaimNext(t.Context(), w.owner, w.cfg.LeaseTTL)
	if err != nil || claimed == nil {
		t.Fatalf("ClaimNext: claimed=%v err=%v, want a task", claimed, err)
	}
	_ = w.execute(t.Context(), w.logger, claimed)
}

// waitFor polls cond until it holds or the timeout elapses (then fails). Used by
// the Run tests, which observe the loop's effects on the store asynchronously.
func waitFor(t *testing.T, timeout time.Duration, cond func() bool) {
	t.Helper()
	deadline := time.Now().Add(timeout)
	for time.Now().Before(deadline) {
		if cond() {
			return
		}
		time.Sleep(time.Millisecond)
	}
	t.Fatal("condition not met within timeout")
}

func TestWorkerRunsAllStepsAndCompletes(t *testing.T) {
	store := NewMemStore()
	h0 := &fakeStep{kind: "k0"}
	h1 := &fakeStep{kind: "k1"}
	w := testWorker(store, regWith(h0, h1))

	task, steps := pendingTask("k0", "k1")
	store.Seed(task, steps)

	claimAndExecute(t, w, store)

	if h0.callCount() != 1 || h1.callCount() != 1 {
		t.Fatalf("handler calls = (k0:%d, k1:%d), want (1,1)", h0.callCount(), h1.callCount())
	}
	got, _ := store.Get(t.Context(), task.ID)
	if got.Status != StatusSucceeded {
		t.Fatalf("task status = %q, want succeeded", got.Status)
	}
	stepRows, _ := store.Steps(t.Context(), task.ID)
	for i, st := range stepRows {
		if st.Status != StatusSucceeded {
			t.Errorf("step %d status = %q, want succeeded", i, st.Status)
		}
		if len(st.Result) == 0 {
			t.Errorf("step %d has no result recorded", i)
		}
	}
}

func TestWorkerResumesSkippingSucceededSteps(t *testing.T) {
	store := NewMemStore()
	h0 := &fakeStep{kind: "k0"}
	h1 := &fakeStep{kind: "k1"}
	w := testWorker(store, regWith(h0, h1))

	// A task previously claimed by a now-dead owner: running, lease expired,
	// step 0 already succeeded, step 1 still pending. The worker must reclaim it
	// and resume at step 1 without re-running step 0 (§8.1/§8.2).
	id := uuid.New()
	s0, s1 := uuid.New(), uuid.New()
	expired := time.Now().Add(-time.Minute)
	owner := "dead-owner"
	task := &Task{ID: id, Kind: "test", Status: StatusRunning, Owner: &owner, LeaseExpiry: &expired, CreatedAt: time.Now(), StepIDs: []uuid.UUID{s0, s1}}
	steps := []*StepRecord{
		{ID: s0, TaskID: id, Kind: "k0", Status: StatusSucceeded},
		{ID: s1, TaskID: id, Kind: "k1", Status: StatusPending},
	}
	store.Seed(task, steps)

	claimAndExecute(t, w, store)

	if h0.callCount() != 0 {
		t.Errorf("already-succeeded step 0 was re-run %d time(s), want 0", h0.callCount())
	}
	if h1.callCount() != 1 {
		t.Errorf("step 1 ran %d time(s), want 1", h1.callCount())
	}
	got, _ := store.Get(t.Context(), id)
	if got.Status != StatusSucceeded {
		t.Fatalf("task status = %q, want succeeded", got.Status)
	}
}

func TestWorkerReplaysInFlightRunningStep(t *testing.T) {
	store := NewMemStore()
	h := &fakeStep{kind: "k0"}
	w := testWorker(store, regWith(h))

	// The canonical §8.2 crash case: the owner died with the step left RUNNING
	// (started, never completed). The worker skips only *succeeded* steps, so it
	// must re-invoke this one — exactly once — and idempotency makes the replay
	// safe. A regression that skipped a running step (treating it as done) would
	// leave callCount at 0.
	id := uuid.New()
	s0 := uuid.New()
	expired := time.Now().Add(-time.Minute)
	owner := "dead-owner"
	task := &Task{ID: id, Kind: "test", Status: StatusRunning, Owner: &owner, LeaseExpiry: &expired, CreatedAt: time.Now(), StepIDs: []uuid.UUID{s0}}
	store.Seed(task, []*StepRecord{{ID: s0, TaskID: id, Kind: "k0", Status: StatusRunning}})

	claimAndExecute(t, w, store)

	if h.callCount() != 1 {
		t.Errorf("in-flight running step ran %d time(s) on reclaim, want exactly 1 (re-run, not skipped)", h.callCount())
	}
	got, _ := store.Get(t.Context(), id)
	if got.Status != StatusSucceeded {
		t.Fatalf("task status = %q, want succeeded", got.Status)
	}
}

func TestWorkerLeavesTaskReclaimableOnCancellation(t *testing.T) {
	store := NewMemStore()
	h := &fakeStep{kind: "k0"}
	w := testWorker(store, regWith(h))

	task, steps := pendingTask("k0")
	store.Seed(task, steps)
	claimed, err := store.ClaimNext(t.Context(), "me", time.Minute)
	if err != nil || claimed == nil {
		t.Fatalf("ClaimNext: %v (claimed=%v)", err, claimed)
	}

	// The worker is shutting down mid-step: the step ctx is cancelled, so the
	// step returns a cancellation failure. That must NOT durably fail the task —
	// it must be left running/reclaimable so §8.2 resume picks it back up.
	ctx, cancel := context.WithCancel(t.Context())
	cancel()
	if err := w.execute(ctx, w.logger, claimed); err == nil {
		t.Fatal("execute on a cancelled ctx returned nil, want an interruption error")
	}
	got, _ := store.Get(t.Context(), task.ID)
	if got.Status == StatusFailed {
		t.Fatal("a task interrupted by shutdown was durably FAILED; want left running/reclaimable for §8.2 resume")
	}
}

func TestWorkerFailsTaskAndHaltsLaterSteps(t *testing.T) {
	store := NewMemStore()
	hFail := &fakeStep{kind: "kfail", failWith: errors.New("boom")}
	hNext := &fakeStep{kind: "knext"}
	w := testWorker(store, regWith(hFail, hNext))

	task, steps := pendingTask("kfail", "knext")
	store.Seed(task, steps)

	claimAndExecute(t, w, store)

	if hNext.callCount() != 0 {
		t.Errorf("step after the failed one ran %d time(s), want 0", hNext.callCount())
	}
	got, _ := store.Get(t.Context(), task.ID)
	if got.Status != StatusFailed {
		t.Fatalf("task status = %q, want failed", got.Status)
	}
	if got.Error == nil {
		t.Error("failed task has no error recorded")
	}
	stepRows, _ := store.Steps(t.Context(), task.ID)
	if stepRows[0].Status != StatusFailed {
		t.Errorf("failed step status = %q, want failed", stepRows[0].Status)
	}
	if stepRows[1].Status != StatusPending {
		t.Errorf("step after the failure = %q, want pending (never started)", stepRows[1].Status)
	}
}

func TestWorkerFailsOnUnknownStepKind(t *testing.T) {
	store := NewMemStore()
	w := testWorker(store, NewRegistry()) // empty registry: no builder for "mystery"

	task, steps := pendingTask("mystery")
	store.Seed(task, steps)

	claimAndExecute(t, w, store)

	got, _ := store.Get(t.Context(), task.ID)
	if got.Status != StatusFailed {
		t.Fatalf("task status = %q, want failed (no step registered for kind)", got.Status)
	}
}

func TestWorkerIgnoresUnexpiredRunningTask(t *testing.T) {
	store := NewMemStore()

	// A task held by a live owner (lease in the future) must not be claimable.
	id := uuid.New()
	s0 := uuid.New()
	future := time.Now().Add(time.Hour)
	owner := "live-owner"
	task := &Task{ID: id, Kind: "test", Status: StatusRunning, Owner: &owner, LeaseExpiry: &future, CreatedAt: time.Now(), StepIDs: []uuid.UUID{s0}}
	store.Seed(task, []*StepRecord{{ID: s0, TaskID: id, Kind: "k0", Status: StatusPending}})

	claimed, err := store.ClaimNext(t.Context(), "me", time.Minute)
	if err != nil {
		t.Fatalf("ClaimNext: %v", err)
	}
	if claimed != nil {
		t.Errorf("claimed a live-owned (unexpired-lease) task %s, want nothing claimable", claimed.ID)
	}
}

func TestWorkerNothingRunnableWhenEmpty(t *testing.T) {
	store := NewMemStore()
	claimed, err := store.ClaimNext(t.Context(), "me", time.Minute)
	if err != nil {
		t.Fatalf("ClaimNext: %v", err)
	}
	if claimed != nil {
		t.Errorf("claimed %s from an empty store, want nothing", claimed.ID)
	}
}

func TestWorkerNoopStepEndToEnd(t *testing.T) {
	// The DefaultRegistry's noop step should drive a task to completion — the
	// minimal end-to-end exercise of the executor.
	store := NewMemStore()
	w := testWorker(store, DefaultRegistry(Deps{}))
	task, steps := pendingTask(KindNoop)
	store.Seed(task, steps)

	claimAndExecute(t, w, store)

	got, _ := store.Get(t.Context(), task.ID)
	if got.Status != StatusSucceeded {
		t.Fatalf("noop task status = %q, want succeeded", got.Status)
	}
}

// --- the claim loop (Run) -------------------------------------------------

func TestWorkerRunDrainsBacklogConcurrently(t *testing.T) {
	store := NewMemStore()
	h := &fakeStep{kind: "k"}
	w := testWorker(store, regWith(h))

	var ids []uuid.UUID
	for range 5 {
		task, steps := pendingTask("k")
		store.Seed(task, steps)
		ids = append(ids, task.ID)
	}

	ctx, cancel := context.WithCancel(t.Context())
	done := make(chan struct{})
	go func() { w.Run(ctx); close(done) }()

	waitFor(t, 2*time.Second, func() bool {
		for _, id := range ids {
			tk, _ := store.Get(t.Context(), id)
			if tk == nil || tk.Status != StatusSucceeded {
				return false
			}
		}
		return true
	})
	if h.callCount() != 5 {
		t.Errorf("step ran %d time(s), want 5 (one per task)", h.callCount())
	}

	cancel()
	select {
	case <-done:
	case <-time.After(2 * time.Second):
		t.Fatal("Run did not return after cancel")
	}
}

func TestWorkerRunRespectsMaxConcurrent(t *testing.T) {
	store := NewMemStore()
	g := &gateStep{release: make(chan struct{})}
	reg := NewRegistry()
	reg.Register("gate", func(json.RawMessage, Deps) (Step, error) { return g, nil })
	w := testWorkerMax(store, reg, 2) // cap concurrency at 2

	var ids []uuid.UUID
	for range 4 {
		task, steps := pendingTask("gate")
		store.Seed(task, steps)
		ids = append(ids, task.ID)
	}

	ctx, cancel := context.WithCancel(t.Context())
	done := make(chan struct{})
	go func() { w.Run(ctx); close(done) }()

	// With a cap of 2, exactly two steps run (and block) at once.
	waitFor(t, 2*time.Second, func() bool { return g.cur() == 2 })
	// Give the loop a beat to (wrongly) start a third if the cap weren't honored.
	time.Sleep(20 * time.Millisecond)
	if peak := g.peak(); peak != 2 {
		t.Errorf("peak concurrent executions = %d, want 2 (MaxConcurrent)", peak)
	}

	close(g.release) // let them all finish; the remaining two then run
	waitFor(t, 2*time.Second, func() bool {
		for _, id := range ids {
			tk, _ := store.Get(t.Context(), id)
			if tk == nil || tk.Status != StatusSucceeded {
				return false
			}
		}
		return true
	})
	if g.peak() != 2 {
		t.Errorf("peak concurrent executions = %d, want 2 (never exceeded the cap)", g.peak())
	}

	cancel()
	select {
	case <-done:
	case <-time.After(2 * time.Second):
		t.Fatal("Run did not return after cancel")
	}
}

func TestMemStoreRenewLeaseIsOwnerAndStatusFenced(t *testing.T) {
	store := NewMemStore()
	task, steps := pendingTask("k0")
	store.Seed(task, steps)

	claimed, err := store.ClaimNext(t.Context(), "owner-a", time.Second)
	if err != nil || claimed == nil {
		t.Fatalf("ClaimNext: %v (claimed=%v)", err, claimed)
	}
	before := *claimed.LeaseExpiry

	// The holder renews: reported held, lease extended.
	held, err := store.RenewLease(t.Context(), task.ID, "owner-a", 10*time.Second)
	if err != nil || !held {
		t.Fatalf("RenewLease by holder = (held=%v, err=%v), want (true, nil)", held, err)
	}
	got, _ := store.Get(t.Context(), task.ID)
	if !got.LeaseExpiry.After(before) {
		t.Errorf("lease_expiry not extended: before=%v after=%v", before, *got.LeaseExpiry)
	}

	// A different owner cannot renew (it does not hold the lease).
	if held, _ := store.RenewLease(t.Context(), task.ID, "owner-b", 10*time.Second); held {
		t.Error("RenewLease by a non-holder reported held=true, want false")
	}

	// A terminal task cannot be renewed (nothing to keep alive).
	_ = store.CompleteTask(t.Context(), task.ID, "owner-a")
	if held, _ := store.RenewLease(t.Context(), task.ID, "owner-a", 10*time.Second); held {
		t.Error("RenewLease on a succeeded task reported held=true, want false")
	}

	// A missing task reports not-held rather than erroring.
	if held, err := store.RenewLease(t.Context(), uuid.New(), "owner-a", time.Second); err != nil || held {
		t.Errorf("RenewLease on a missing task = (held=%v, err=%v), want (false, nil)", held, err)
	}
}

func TestMemStoreMutationsAreOwnerFenced(t *testing.T) {
	store := NewMemStore()
	task, steps := pendingTask("k0")
	store.Seed(task, steps)
	stepID := steps[0].ID

	claimed, err := store.ClaimNext(t.Context(), "owner-a", time.Minute)
	if err != nil || claimed == nil {
		t.Fatalf("ClaimNext: %v (claimed=%v)", err, claimed)
	}

	// A worker that no longer owns the task (owner-b) must not advance or finish
	// it — every mutation is a no-op, so a partitioned/stalled ex-owner cannot
	// clobber the state of a task another instance now owns (#90).
	_ = store.StartStep(t.Context(), task.ID, stepID, "owner-b")
	_ = store.CompleteStep(t.Context(), task.ID, stepID, "owner-b", nil)
	_ = store.CompleteTask(t.Context(), task.ID, "owner-b")
	_ = store.FailStep(t.Context(), task.ID, stepID, "owner-b", errors.New("boom"))

	got, _ := store.Get(t.Context(), task.ID)
	if got.Status != StatusRunning {
		t.Errorf("task status = %q after non-owner mutations, want unchanged (running)", got.Status)
	}
	stepRows, _ := store.Steps(t.Context(), task.ID)
	if stepRows[0].Status != StatusPending {
		t.Errorf("step status = %q after non-owner mutations, want unchanged (pending)", stepRows[0].Status)
	}

	// The real owner still advances it.
	if err := store.CompleteTask(t.Context(), task.ID, "owner-a"); err != nil {
		t.Fatalf("CompleteTask by owner: %v", err)
	}
	got, _ = store.Get(t.Context(), task.ID)
	if got.Status != StatusSucceeded {
		t.Errorf("task status = %q after owner CompleteTask, want succeeded", got.Status)
	}
}

func TestWorkerRenewsLeaseSoLongStepIsNotReclaimed(t *testing.T) {
	store := NewMemStore()
	g := &gateStep{release: make(chan struct{})}
	reg := NewRegistry()
	reg.Register("gate", func(json.RawMessage, Deps) (Step, error) { return g, nil })

	// A short lease with faster renewal, and free slots so a reclaim *could*
	// happen if the lease lapsed. Renewal must keep the running task's lease
	// alive so the claim loop never reclaims and re-runs it.
	cfg := config.TaskConfig{PollInterval: 5 * time.Millisecond, LeaseTTL: 100 * time.Millisecond, RenewInterval: 25 * time.Millisecond, MaxConcurrent: 4, ShutdownGrace: time.Second}
	w := NewWorker(store, reg, cfg, slog.New(slog.NewTextHandler(io.Discard, nil)))

	task, steps := pendingTask("gate")
	store.Seed(task, steps)

	ctx, cancel := context.WithCancel(t.Context())
	done := make(chan struct{})
	go func() { w.Run(ctx); close(done) }()

	waitFor(t, 2*time.Second, func() bool { return g.cur() == 1 }) // claimed and running
	// Hold the step across several lease periods. Without renewal the loop would
	// reclaim the lease-expired running task and run it again (a second call).
	time.Sleep(400 * time.Millisecond)
	if c := g.callCount(); c != 1 {
		t.Errorf("gate step ran %d time(s); want exactly 1 (renewal must prevent a lease-expiry reclaim)", c)
	}
	got, _ := store.Get(t.Context(), task.ID)
	if got.LeaseExpiry == nil || !got.LeaseExpiry.After(time.Now()) {
		t.Errorf("running task's lease not kept alive: lease_expiry=%v", got.LeaseExpiry)
	}

	close(g.release)
	cancel()
	select {
	case <-done:
	case <-time.After(2 * time.Second):
		t.Fatal("Run did not return after cancel")
	}
}

func TestWorkerCancelsExecutionWhenLeaseLost(t *testing.T) {
	store := &leaseLostStore{MemStore: NewMemStore()}
	step := &cancelAwareStep{started: make(chan struct{})}
	reg := NewRegistry()
	reg.Register("cancelaware", func(json.RawMessage, Deps) (Step, error) { return step, nil })

	rec := &capturingHandler{}
	cfg := config.TaskConfig{PollInterval: time.Millisecond, LeaseTTL: 100 * time.Millisecond, RenewInterval: 10 * time.Millisecond, MaxConcurrent: 1, ShutdownGrace: 2 * time.Second}
	w := NewWorker(store, reg, cfg, slog.New(rec))

	task, steps := pendingTask("cancelaware")
	store.Seed(task, steps)

	ctx, cancel := context.WithCancel(t.Context())
	defer cancel()
	done := make(chan struct{})
	go func() { w.Run(ctx); close(done) }()

	<-step.started // the step is running and blocked on its context

	// The store reports the lease as lost on the first renewal; the heartbeat must
	// cancel the step's context (it returns Fail(ctx.Err())) rather than let it run
	// concurrently with the (simulated) new owner. That is an interruption, not a
	// durable failure: the task must be left reclaimable.
	waitFor(t, 2*time.Second, func() bool { return rec.has(slog.LevelWarn, "lease lost") })
	waitFor(t, 2*time.Second, func() bool {
		got, _ := store.Get(t.Context(), task.ID)
		return got != nil && got.Status != StatusFailed
	})
	if rec.has(slog.LevelError, "task failed") {
		t.Error(`a task cancelled by lease loss was logged as ERROR "task failed"; want an interruption`)
	}

	cancel()
	select {
	case <-done:
	case <-time.After(2 * time.Second):
		t.Fatal("Run did not return after cancel")
	}
}

// leaseLostStore is a MemStore that always reports a running task's lease as no
// longer held, modeling another instance having reclaimed it — so the worker's
// heartbeat exercises its lease-loss cancellation path.
type leaseLostStore struct {
	*MemStore
}

func (s *leaseLostStore) RenewLease(context.Context, uuid.UUID, string, time.Duration) (bool, error) {
	return false, nil
}

// gateStep blocks in Execute until release is closed, tracking the peak number of
// concurrent executions — so a test can assert the worker honors MaxConcurrent.
type gateStep struct {
	mu         sync.Mutex
	running    int
	maxRunning int
	calls      int
	release    chan struct{}
}

func (*gateStep) Kind() string { return "gate" }

func (g *gateStep) Execute(context.Context) StepResult {
	g.mu.Lock()
	g.running++
	g.calls++
	if g.running > g.maxRunning {
		g.maxRunning = g.running
	}
	g.mu.Unlock()

	<-g.release // block until the test releases all in-flight executions

	g.mu.Lock()
	g.running--
	g.mu.Unlock()
	return Ok(json.RawMessage(`{"ok":true}`))
}

func (g *gateStep) cur() int {
	g.mu.Lock()
	defer g.mu.Unlock()
	return g.running
}

func (g *gateStep) peak() int {
	g.mu.Lock()
	defer g.mu.Unlock()
	return g.maxRunning
}

func (g *gateStep) callCount() int {
	g.mu.Lock()
	defer g.mu.Unlock()
	return g.calls
}

func TestWorkerRecoversFromStepPanic(t *testing.T) {
	store := NewMemStore()
	reg := NewRegistry()
	reg.Register("boom", func(json.RawMessage, Deps) (Step, error) { return panicStep{}, nil })
	w := testWorker(store, reg)

	task, steps := pendingTask("boom")
	store.Seed(task, steps)

	// A panicking step must not crash the worker — it fails its task instead. (If
	// the recover regressed, the panic would propagate here and crash the test.)
	claimAndExecute(t, w, store)

	got, _ := store.Get(t.Context(), task.ID)
	if got.Status != StatusFailed {
		t.Fatalf("task status = %q, want failed (panicking step)", got.Status)
	}
	stepRows, _ := store.Steps(t.Context(), task.ID)
	if stepRows[0].Status != StatusFailed || stepRows[0].Error == nil {
		t.Errorf("panicking step = {status:%q err:%v}, want failed with a recorded error", stepRows[0].Status, stepRows[0].Error)
	}
}

func TestWorkerLogsShutdownInterruptionNotFailure(t *testing.T) {
	store := NewMemStore()
	step := &cancelAwareStep{started: make(chan struct{})}
	reg := NewRegistry()
	reg.Register("cancelaware", func(json.RawMessage, Deps) (Step, error) { return step, nil })

	rec := &capturingHandler{}
	cfg := config.TaskConfig{PollInterval: time.Millisecond, LeaseTTL: 30 * time.Second, RenewInterval: 10 * time.Second, MaxConcurrent: 1, ShutdownGrace: 2 * time.Second}
	w := NewWorker(store, reg, cfg, slog.New(rec))

	task, steps := pendingTask("cancelaware")
	store.Seed(task, steps)

	ctx, cancel := context.WithCancel(t.Context())
	done := make(chan struct{})
	go func() { w.Run(ctx); close(done) }()

	<-step.started // the step is running and blocked on ctx
	cancel()       // shut down mid-task

	select {
	case <-done:
	case <-time.After(2 * time.Second):
		t.Fatal("Run did not return after cancel")
	}

	// A graceful shutdown must NOT log a spurious failure for an interrupted task.
	if rec.has(slog.LevelError, "task failed") {
		t.Error(`shutdown interruption was logged as ERROR "task failed", want a non-error interruption log`)
	}
	if !rec.has(slog.LevelInfo, "interrupted") {
		t.Error(`expected an info-level "interrupted" log, got none`)
	}
}

func TestWorkerBoundedShutdown(t *testing.T) {
	store := NewMemStore()
	g := &gateStep{release: make(chan struct{})} // blocks forever; ignores ctx
	reg := NewRegistry()
	reg.Register("gate", func(json.RawMessage, Deps) (Step, error) { return g, nil })
	cfg := config.TaskConfig{PollInterval: time.Millisecond, LeaseTTL: 30 * time.Second, RenewInterval: 10 * time.Second, MaxConcurrent: 1, ShutdownGrace: 50 * time.Millisecond}
	w := NewWorker(store, reg, cfg, slog.New(slog.NewTextHandler(io.Discard, nil)))

	task, steps := pendingTask("gate")
	store.Seed(task, steps)

	ctx, cancel := context.WithCancel(t.Context())
	done := make(chan struct{})
	go func() { w.Run(ctx); close(done) }()

	waitFor(t, 2*time.Second, func() bool { return g.cur() == 1 }) // step running and wedged
	cancel()

	// The step never returns (it ignores ctx), so wg.Wait would block forever; the
	// grace bound must let Run return anyway.
	select {
	case <-done:
	case <-time.After(time.Second): // >> ShutdownGrace (50ms)
		t.Fatal("Run did not return within the shutdown grace; bounded drain regressed")
	}
	close(g.release) // release the abandoned goroutine so it doesn't linger
}

// panicStep panics in Execute — to verify the worker recovers and fails the task
// rather than crashing.
type panicStep struct{}

func (panicStep) Kind() string                       { return "boom" }
func (panicStep) Execute(context.Context) StepResult { panic("boom in a step") }

// cancelAwareStep blocks until its context is cancelled, then reports the
// cancellation as a failure — modeling a step interrupted by shutdown.
type cancelAwareStep struct {
	once    sync.Once
	started chan struct{}
}

func (*cancelAwareStep) Kind() string { return "cancelaware" }

func (s *cancelAwareStep) Execute(ctx context.Context) StepResult {
	s.once.Do(func() { close(s.started) })
	<-ctx.Done()
	return Fail(ctx.Err())
}

// capturingHandler records each log record's level and message, for asserting
// what the worker logged.
type capturingHandler struct {
	mu      sync.Mutex
	records []slog.Record
}

func (*capturingHandler) Enabled(context.Context, slog.Level) bool { return true }

func (h *capturingHandler) Handle(_ context.Context, r slog.Record) error {
	h.mu.Lock()
	defer h.mu.Unlock()
	h.records = append(h.records, r.Clone())
	return nil
}

func (h *capturingHandler) WithAttrs([]slog.Attr) slog.Handler { return h }
func (h *capturingHandler) WithGroup(string) slog.Handler      { return h }

func (h *capturingHandler) has(level slog.Level, substr string) bool {
	h.mu.Lock()
	defer h.mu.Unlock()
	for _, r := range h.records {
		if r.Level == level && strings.Contains(r.Message, substr) {
			return true
		}
	}
	return false
}
