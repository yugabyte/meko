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
)

// These cover the endpoint lock: the one a TASK holds, so two blocking tasks
// cannot touch one endpoint whichever process owns them. The task lock's own
// tests are in worker_test.go. Both run against MemLocks, which models the
// decision rule but not the clock — the timings are pglease's own tests, and the
// SQL is the live-database ones (architecture/ac-locking.md).

// lockingStep is a fakeStep that declares a resource lock, plus a gate so a test
// can hold the lock while a second task tries for it.
type lockingStep struct {
	fakeStep
	lock    ResourceLock
	entered chan struct{}
	release chan struct{}
	once    sync.Once
}

func (s *lockingStep) Lock() ResourceLock { return s.lock }

func (s *lockingStep) Execute(ctx context.Context) StepResult {
	if s.entered != nil {
		s.once.Do(func() { close(s.entered) })
	}
	if s.release != nil {
		select {
		case <-s.release:
		case <-ctx.Done():
			return Fail(ctx.Err())
		}
	}
	return s.fakeStep.Execute(ctx)
}

func TestEndpointLockAdmitsOneTaskAtATime(t *testing.T) {
	store := NewMemStore()
	locks := store.Locks(ownerID())
	lock := EndpointLock(uuid.NewString())
	first, second := uuid.New(), uuid.New()

	held, err := locks.AcquireResource(t.Context(), lock, first, KindResizeEndpoint.String())
	if err != nil {
		t.Fatalf("first task was refused the endpoint lock: %v", err)
	}

	// A second task wanting the same endpoint waits rather than being refused
	// outright — the contended endpoint is a queue you join.
	waitCtx, cancel := context.WithTimeout(t.Context(), 50*time.Millisecond)
	defer cancel()
	if _, err := locks.AcquireResource(waitCtx, lock, second, KindDeleteEndpoint.String()); !errors.Is(err, ErrLockHeld) {
		t.Fatalf("second task's bounded wait ended with err=%v, want ErrLockHeld", err)
	}

	// It is admitted once the holder is done.
	if err := held.Release(t.Context()); err != nil {
		t.Fatalf("Release: %v", err)
	}
	if _, err := locks.AcquireResource(t.Context(), lock, second, KindDeleteEndpoint.String()); err != nil {
		t.Fatalf("after release the second task was still refused: %v", err)
	}
}

func TestEndpointLockIsPerEndpoint(t *testing.T) {
	store := NewMemStore()
	locks := store.Locks(ownerID())
	a, b := EndpointLock(uuid.NewString()), EndpointLock(uuid.NewString())

	if _, err := locks.AcquireResource(t.Context(), a, uuid.New(), KindResizeEndpoint.String()); err != nil {
		t.Fatalf("lock on endpoint a: %v", err)
	}
	if _, err := locks.AcquireResource(t.Context(), b, uuid.New(), KindResizeEndpoint.String()); err != nil {
		t.Errorf("lock on a different endpoint was refused: %v — the lock must not be global", err)
	}
}

// The owner names the task, not the process. Two tasks in ONE process are the
// case that breaks if it named the process: a lease is granted unconditionally to
// its current holder, so both would be granted and the database would stop being
// what decides.
func TestEndpointLockOwnerNamesTheTask(t *testing.T) {
	store := NewMemStore()
	locks := store.Locks("one-process")
	lock := EndpointLock(uuid.NewString())
	holder := uuid.New()

	if _, err := locks.AcquireResource(t.Context(), lock, holder, KindResizeEndpoint.String()); err != nil {
		t.Fatalf("AcquireResource: %v", err)
	}
	waitCtx, cancel := context.WithTimeout(t.Context(), 50*time.Millisecond)
	defer cancel()
	if _, err := locks.AcquireResource(waitCtx, lock, uuid.New(), KindDeleteEndpoint.String()); !errors.Is(err, ErrLockHeld) {
		t.Fatalf("a second task in the same process was granted the lock (err=%v); the owner must name the task", err)
	}

	// And the row says which task holds it, which is what "why is this endpoint
	// busy?" needs — resource_token has no column for it.
	if got := locks.ResourceHolder(lock); !strings.Contains(got, holder.String()) {
		t.Errorf("holder = %q, want it to name task %s", got, holder)
	}
}

// overlapStep records the peak number of concurrent executions across every task
// that runs it, which is what "these two never touch the endpoint at once" means.
// Which task goes first is deliberately not asserted: the lock orders nothing, it
// only excludes.
type overlapStep struct {
	mu      sync.Mutex
	running int
	peak    int
	lock    ResourceLock
	locked  bool
}

func (*overlapStep) Kind() string { return "endpoint-work" }

func (s *overlapStep) Lock() ResourceLock {
	if !s.locked {
		return ResourceLock{}
	}
	return s.lock
}

func (s *overlapStep) Execute(context.Context) StepResult {
	s.mu.Lock()
	s.running++
	s.peak = max(s.peak, s.running)
	s.mu.Unlock()

	time.Sleep(20 * time.Millisecond) // long enough for a rival to overlap if it can

	s.mu.Lock()
	s.running--
	s.mu.Unlock()
	return Ok(nil)
}

func (s *overlapStep) peakConcurrency() int {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.peak
}

// Two tasks, one endpoint: they must never run against it at the same time. The
// unlocked half is the control — it proves the harness can actually see an
// overlap, so the locked half's peak of 1 means the lock did it.
func TestWorkerSerializesTasksOnOneEndpoint(t *testing.T) {
	for _, tc := range []struct {
		name     string
		locked   bool
		wantPeak int
	}{
		{name: "locked", locked: true, wantPeak: 1},
		{name: "unlocked control", locked: false, wantPeak: 2},
	} {
		t.Run(tc.name, func(t *testing.T) {
			store := NewMemStore()
			step := &overlapStep{lock: EndpointLock(uuid.NewString()), locked: tc.locked}
			reg := NewRegistry()
			reg.Register(step.Kind(), func(json.RawMessage, Deps) (Step, error) { return step, nil })

			// Two slots, so the only thing that can hold a task back is the lock.
			w := testWorkerMax(store, store.Locks(ownerID()), reg, 2)

			var ids []uuid.UUID
			for range 2 {
				task, steps := pendingTask(step.Kind())
				store.Seed(task, steps)
				ids = append(ids, task.ID)
			}

			ctx, cancel := context.WithCancel(t.Context())
			done := make(chan struct{})
			go func() { w.Run(ctx); close(done) }()

			for _, id := range ids {
				waitFor(t, 5*time.Second, func() bool {
					got, _ := store.Get(t.Context(), id)
					return got != nil && got.Status == StatusSucceeded
				})
			}
			if got := step.peakConcurrency(); got != tc.wantPeak {
				t.Errorf("peak concurrent executions on one endpoint = %d, want %d", got, tc.wantPeak)
			}

			cancel()
			select {
			case <-done:
			case <-time.After(2 * time.Second):
				t.Fatal("Run did not return after cancel")
			}
		})
	}
}

// A task whose steps want two different locks is failed rather than allowed to
// reserve both: holding two is what deadlock needs, and nothing here detects one.
func TestWorkerRefusesATaskWantingTwoLocks(t *testing.T) {
	store := NewMemStore()
	one := &lockingStep{fakeStep: fakeStep{kind: "one"}, lock: EndpointLock(uuid.NewString())}
	two := &lockingStep{fakeStep: fakeStep{kind: "two"}, lock: EndpointLock(uuid.NewString())}
	reg := NewRegistry()
	reg.Register("one", func(json.RawMessage, Deps) (Step, error) { return one, nil })
	reg.Register("two", func(json.RawMessage, Deps) (Step, error) { return two, nil })
	w := testWorker(store, reg)

	task, steps := pendingTask("one", "two")
	store.Seed(task, steps)

	claimAndExecute(t, w)

	if two.callCount() != 0 {
		t.Errorf("the second locking step ran %d time(s), want 0", two.callCount())
	}
	got, _ := store.Get(t.Context(), task.ID)
	if got.Status != StatusFailed {
		t.Fatalf("task status = %q, want failed", got.Status)
	}
	if got.Error == nil || !strings.Contains(*got.Error, "at most one") {
		t.Errorf("task error = %v, want it to name the one-lock-per-task rule", got.Error)
	}
}

// A reclaim after a crash is not deadlocked by the lock the dead owner left
// behind: the lock excludes distinct tasks, not two executions of one.
//
// The re-run is a DIFFERENT process, which is what makes this the real scenario —
// resourceOwner carries a per-process nonce, so the re-run's owner string differs
// and it takes the lapsed row over rather than renewing it (locks.go, Locking).
// An earlier version of this test re-acquired under the same owner and passed only
// because MemLocks modelled no reserve guard.
func TestEndpointLockIsRetakenAfterTheHolderDies(t *testing.T) {
	store := NewMemStore()
	dead, alive := store.Locks("proc-a"), store.Locks("proc-b")
	lock := EndpointLock(uuid.NewString())
	task := uuid.New()

	if _, err := dead.AcquireResource(t.Context(), lock, task, KindResizeEndpoint.String()); err != nil {
		t.Fatalf("AcquireResource: %v", err)
	}
	store.locks.expire(lock.Key()) // proc-a died holding it

	if _, err := alive.AcquireResource(t.Context(), lock, task, KindResizeEndpoint.String()); err != nil {
		t.Fatalf("the same task re-run elsewhere was refused its own lock: %v", err)
	}
}

// The other half, and the guard that made the test above wrong: one process may
// not hold two leases over one row, even after the row lapses. pglease refuses
// that in Client.reserve, because two Held would each believe they own it and
// either one's Release would un-fence the other.
func TestEndpointLockRefusesASecondLeaseToItsOwnHolder(t *testing.T) {
	store := NewMemStore()
	locks := store.Locks(ownerID())
	lock := EndpointLock(uuid.NewString())
	task := uuid.New()

	held, err := locks.AcquireResource(t.Context(), lock, task, KindResizeEndpoint.String())
	if err != nil {
		t.Fatalf("AcquireResource: %v", err)
	}
	for _, when := range []string{"while live", "after the row lapsed"} {
		if when == "after the row lapsed" {
			store.locks.expire(lock.Key())
		}
		ctx, cancel := context.WithTimeout(t.Context(), 50*time.Millisecond)
		_, err := locks.AcquireResource(ctx, lock, task, KindResizeEndpoint.String())
		cancel()
		if !errors.Is(err, ErrLockHeld) {
			t.Errorf("second acquire %s got err=%v, want ErrLockHeld", when, err)
		}
	}
	// And once given back it is takeable again, so the guard is not a deadlock.
	if err := held.Release(t.Context()); err != nil {
		t.Fatalf("Release: %v", err)
	}
	if _, err := locks.AcquireResource(t.Context(), lock, task, KindResizeEndpoint.String()); err != nil {
		t.Errorf("after release the holder was still refused: %v", err)
	}
}

// A lost lease must not delete the row on release: the successor may hold it under
// the same owner string, and release matches on the owner alone.
func TestReleaseAfterLossDoesNotUnfenceTheSuccessor(t *testing.T) {
	store := NewMemStore()
	locks := store.Locks("one-process")
	lock := EndpointLock(uuid.NewString())
	first, second := uuid.New(), uuid.New()

	loser, err := locks.AcquireResource(t.Context(), lock, first, KindResizeEndpoint.String())
	if err != nil {
		t.Fatalf("AcquireResource: %v", err)
	}
	store.locks.expire(lock.Key())
	if _, err := locks.AcquireResource(t.Context(), lock, second, KindDeleteEndpoint.String()); err != nil {
		t.Fatalf("takeover: %v", err)
	}

	// The loser tidies up. The row is the successor's now and must survive it.
	if err := loser.Release(t.Context()); err != nil {
		t.Fatalf("Release: %v", err)
	}
	if got := locks.ResourceHolder(lock); got == "" {
		t.Error("the loser's release freed the successor's lock")
	} else if !strings.Contains(got, second.String()) {
		t.Errorf("holder = %q, want the successor task %s", got, second)
	}
}

// The four endpoint-mutating steps all lock the endpoint they touch, and the
// cascade deletes deliberately lock nothing. This is a rule about which kinds
// serialize, so it is asserted rather than left to a reader to notice.
func TestEndpointStepsDeclareTheirLock(t *testing.T) {
	id := uuid.New()
	want := EndpointLock(id.String())

	for _, s := range []Locking{
		&createEndpointStep{CreateEndpointArgs: CreateEndpointArgs{EndpointID: id}},
		&resizeEndpointStep{ResizeEndpointArgs: ResizeEndpointArgs{EndpointID: id}},
		&upgradeEndpointStep{UpgradeEndpointArgs: UpgradeEndpointArgs{EndpointID: id}},
		&deleteEndpointStep{DeleteEndpointArgs: DeleteEndpointArgs{EndpointID: id}},
	} {
		if got := s.Lock(); got != want {
			t.Errorf("%T.Lock() = %v, want %v", s, got, want)
		}
	}

	for _, s := range []Step{
		&deleteBranchStep{},
		&deleteClusterStep{},
	} {
		if _, ok := s.(Locking); ok {
			t.Errorf("%T declares a resource lock; the cascade deletes touch many endpoints and need intent locks, not one lock", s)
		}
	}
}
