package task

import (
	"context"
	"encoding/json"
	"errors"
	"testing"
	"time"

	"github.com/google/uuid"
)

// The gaps a review of this change found in its own tests: three worker paths that
// leak a lease or stall the fleet if they regress, and nothing exercised any of
// them. A leaked pglease.Held renews FOREVER, so a leak here holds its key for the
// life of the process rather than for a TTL.

// A task that FAILS must still give back its endpoint lock. Otherwise the lock is
// held by a renewing Held for the life of the pod, and every later create, resize
// or delete on that endpoint waits out --task-lock-wait (10m) and then fails.
func TestWorkerReleasesEndpointLockWhenTheTaskFails(t *testing.T) {
	store := NewMemStore()
	endpoint := EndpointLock(uuid.NewString())
	step := &lockingStep{
		fakeStep: fakeStep{kind: "boom", failWith: errors.New("boom")},
		lock:     endpoint,
	}
	reg := NewRegistry()
	reg.Register("boom", func(json.RawMessage, Deps) (Step, error) { return step, nil })

	locks := store.Locks(ownerID())
	w := testWorkerMax(store, locks, reg, 1)

	task, steps := pendingTask("boom")
	store.Seed(task, steps)
	claimAndExecute(t, w)

	if got, _ := store.Get(t.Context(), task.ID); got.Status != StatusFailed {
		t.Fatalf("task status = %q, want failed", got.Status)
	}
	if got := locks.ResourceHolder(endpoint); got != "" {
		t.Errorf("endpoint still locked by %q after the task failed; a later resize would "+
			"wait out --task-lock-wait and fail for the life of the process", got)
	}
}

// The same, for a task interrupted by shutdown rather than failed.
func TestWorkerReleasesEndpointLockOnShutdown(t *testing.T) {
	store := NewMemStore()
	endpoint := EndpointLock(uuid.NewString())
	g := &lockingGateStep{lock: endpoint, entered: make(chan struct{}), release: make(chan struct{})}
	reg := NewRegistry()
	reg.Register(g.Kind(), func(json.RawMessage, Deps) (Step, error) { return g, nil })

	locks := store.Locks(ownerID())
	w := testWorkerMax(store, locks, reg, 1)

	task, steps := pendingTask(g.Kind())
	store.Seed(task, steps)

	ctx, cancel := context.WithCancel(t.Context())
	done := make(chan struct{})
	go func() { w.Run(ctx); close(done) }()

	<-g.entered // holding the endpoint
	cancel()
	close(g.release)
	select {
	case <-done:
	case <-time.After(5 * time.Second):
		t.Fatal("Run did not return after cancel")
	}

	if got := locks.ResourceHolder(endpoint); got != "" {
		t.Errorf("endpoint still locked by %q after shutdown, want released", got)
	}
}

// claim gives the task lease back when the transition finds the task gone. Without
// it every occurrence leaks a Held whose renewal never stops.
func TestWorkerReleasesTaskLeaseWhenClaimFindsNothing(t *testing.T) {
	store := NewMemStore()
	locks := store.Locks(ownerID())
	w := testWorkerMax(store, locks, NewRegistry(), 1)

	task, steps := pendingTask("k0")
	store.Seed(task, steps)

	// The task reaches terminal between the candidate read and the transition, which
	// is what Claim returning nil means.
	store.now = func() time.Time { return time.Now() }
	if err := store.CompleteTask(t.Context(), task.ID, "someone-else"); err != nil {
		t.Fatalf("seed terminal: %v", err)
	}
	// CompleteTask is owner-fenced, so force the state Seed cannot produce.
	seeded, _ := store.Get(t.Context(), task.ID)
	seeded.Status = StatusSucceeded
	store.Seed(seeded, nil)

	claimed, lease, err := w.claim(t.Context())
	if err != nil {
		t.Fatalf("claim: %v", err)
	}
	if claimed != nil || lease != nil {
		t.Fatalf("claimed a terminal task: %v", claimed)
	}
	if got := store.locks.heldBy(taskLockKey(task.ID)); got != "" {
		t.Errorf("task lease still held by %q after Claim found nothing; that Held would "+
			"renew for the life of the process", got)
	}
}

// The branch that replaced FOR UPDATE SKIP LOCKED: a candidate somebody else holds
// is walked PAST, not waited on and not treated as an error. Turning this into an
// early return stalls every instance for a poll interval whenever a peer is
// mid-claim; turning it into an error puts the worker into exponential backoff
// under ordinary contention.
//
// Reaching it needs a lease on a PENDING task, which Candidates does not filter —
// exactly the state a process leaves behind if it dies between AcquireTask and
// Claim.
func TestWorkerWalksPastACandidateSomebodyElseHolds(t *testing.T) {
	store := NewMemStore()
	h := &fakeStep{kind: "k0"}
	w := testWorker(store, regWith(h))

	blocked, bSteps := pendingTask("k0")
	free, fSteps := pendingTask("k0")
	blocked.CreatedAt = time.Now().Add(-time.Hour) // the older, so it is tried first
	store.Seed(blocked, bSteps)
	store.Seed(free, fSteps)

	if _, err := store.Locks("another-instance").AcquireTask(t.Context(), blocked.ID); err != nil {
		t.Fatalf("seeding the peer's lease: %v", err)
	}

	claimed, lease, err := w.claim(t.Context())
	if err != nil {
		t.Fatalf("claim returned an error for a held candidate: %v — it must walk past, "+
			"not back off", err)
	}
	if claimed == nil {
		t.Fatal("claim returned nothing; it stopped at the held candidate instead of trying the next")
	}
	defer func() { _ = lease.Release(t.Context()) }()
	if claimed.ID != free.ID {
		t.Errorf("claimed %s, want the unheld task %s", claimed.ID, free.ID)
	}
}

// A task that cannot get its endpoint lock inside the bound fails cleanly rather
// than hanging. This is the path an operator meets when a lock is stuck, and the
// only one that durably fails a customer's task on a lock.
func TestWorkerFailsATaskThatCannotGetItsLock(t *testing.T) {
	store := NewMemStore()
	endpoint := EndpointLock(uuid.NewString())
	step := &lockingStep{fakeStep: fakeStep{kind: "wants-lock"}, lock: endpoint}
	reg := NewRegistry()
	reg.Register("wants-lock", func(json.RawMessage, Deps) (Step, error) { return step, nil })

	locks := store.Locks(ownerID())
	// The bound has to be the LOCKS' own, not the caller's context: exhausting the
	// caller's ctx is a shutdown, which the worker deliberately leaves reclaimable.
	// PgLocks bounds it with --task-lock-wait the same way.
	locks.Wait = 100 * time.Millisecond
	w := testWorkerMax(store, locks, reg, 1)

	// Somebody else holds the endpoint and never gives it back.
	if _, err := locks.AcquireResource(t.Context(), endpoint, uuid.New(), KindCreateEndpoint); err != nil {
		t.Fatalf("seeding the holder: %v", err)
	}

	task, steps := pendingTask("wants-lock")
	store.Seed(task, steps)

	claimed, lease, err := w.claim(t.Context())
	if err != nil || claimed == nil {
		t.Fatalf("claim: task=%v err=%v", claimed, err)
	}
	defer func() { _ = lease.Release(context.WithoutCancel(t.Context())) }()
	_ = w.execute(t.Context(), w.logger, claimed)

	if step.callCount() != 0 {
		t.Errorf("the step ran %d time(s) without its lock", step.callCount())
	}
	got, _ := store.Get(t.Context(), task.ID)
	if got.Status != StatusFailed {
		t.Errorf("task status = %q after exhausting the lock wait, want failed cleanly", got.Status)
	}
}

// lockingGateStep declares a lock and blocks in Execute, so a test can hold the
// lock open across a shutdown.
type lockingGateStep struct {
	lock    ResourceLock
	entered chan struct{}
	release chan struct{}
	once    bool
}

func (*lockingGateStep) Kind() string         { return "locking-gate" }
func (s *lockingGateStep) Lock() ResourceLock { return s.lock }

func (s *lockingGateStep) Execute(ctx context.Context) StepResult {
	if !s.once {
		s.once = true
		close(s.entered)
	}
	select {
	case <-s.release:
	case <-ctx.Done():
	}
	return Ok(nil)
}
