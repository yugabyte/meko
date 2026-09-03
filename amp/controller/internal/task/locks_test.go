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

	"github.com/yugabyte/amp/controller/pkg/pglease"
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

	// observe, when set, is asked who holds the lock at execute time — for the
	// subtask runner tests, which assert the runner took the declared lock and
	// took it in the child's name.
	observe *MemLocks
	mu      sync.Mutex
	holder  string
}

func (s *lockingStep) Lock() ResourceLock { return s.lock }

// heldBy reports who the lock table named as the holder when the step last ran,
// or "" if it was free (or the step never ran).
func (s *lockingStep) heldBy() string {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.holder
}

func (s *lockingStep) Execute(ctx context.Context) StepResult {
	if s.observe != nil {
		s.mu.Lock()
		s.holder = s.observe.ResourceHolder(s.lock)
		s.mu.Unlock()
	}
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
		t.Fatalf("second task's bounded wait ended with err=%v, want an error wrapping ErrLockHeld", err)
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
			t.Errorf("second acquire %s got err=%v, want an error wrapping ErrLockHeld", when, err)
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

// The four endpoint-mutating steps all lock the endpoint they touch, the fleet
// roll locks the fleet instead, and the cascade deletes deliberately lock
// nothing. This is a rule about which kinds serialize, so it is asserted rather
// than left to a reader to notice.
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

// TestFleetRollDeclaresTheFleetLock is what makes it one roll at a time (§5.6).
// Two concurrent rolls have no sensible interleaving; without the declaration
// they would both fan out over the same fleet with only the per-endpoint locks
// between them.
func TestFleetRollDeclaresTheFleetLock(t *testing.T) {
	var step Step = &planEndpointUpgradesStep{}
	locking, ok := step.(Locking)
	if !ok {
		t.Fatal("plan_endpoint_upgrades declares no resource lock; nothing then serializes two fleet rolls")
	}
	got := locking.Lock()
	if want := FleetEndpointsLock(); got != want {
		t.Fatalf("plan_endpoint_upgrades locks %v, want %v", got, want)
	}
	// A third namespace in resource_token, disjoint from endpoint:<id> and
	// task:<id> as they are from the compute fence's branch:<id>.
	if got.Key() != "fleet:endpoints" {
		t.Errorf("fleet lock key = %q, want fleet:endpoints", got.Key())
	}
	if got.Type == ResourceEndpoint {
		t.Error("the fleet roll took an endpoint lock; it holds one lock covering work its children do")
	}
	// And it is the one lock that REFUSES rather than queues (§5.6). Declaring it
	// without NoWait would silently restore the old contract: two rolls at
	// different versions, both accepted, the fleet flipped twice.
	if !got.NoWait {
		t.Error("the fleet lock queues; a second roll must be refused, not made to wait for its turn")
	}
}

// TestFleetLockRefusesTheSecondRoll is layer 2 of "one roll at a time" (§5.6),
// and the authority: the plan-time query in front of it is two statements away
// from the insert, so this is what actually stops two rolls naming different
// versions from flipping the fleet twice.
func TestFleetLockRefusesTheSecondRoll(t *testing.T) {
	store := NewMemStore()
	locks := store.Locks(ownerID())
	first, second := uuid.New(), uuid.New()

	held, err := locks.AcquireResource(t.Context(), FleetEndpointsLock(), first, KindUpgradeEndpoints.String())
	if err != nil {
		t.Fatalf("first roll was refused the fleet lock: %v", err)
	}

	// No deadline on the context, deliberately: the refusal has to come from the
	// LOCK, not from the caller's patience running out. A lock that queued would
	// sit here for MemLocks' own bound and then report the timeout instead.
	start := time.Now()
	_, err = locks.AcquireResource(t.Context(), FleetEndpointsLock(), second, KindUpgradeEndpoints.String())
	waited := time.Since(start)
	if err == nil {
		t.Fatal("two rolls held the fleet lock at once")
	}
	if !errors.Is(err, ErrLockHeld) {
		t.Errorf("the second roll failed with %v, want an error wrapping ErrLockHeld", err)
	}
	if waited > memLockWait/2 {
		t.Errorf("the second roll waited %s for the fleet lock; it must be refused at once, not queued", waited)
	}

	// Refused, not poisoned: the next roll after this one finishes still gets it.
	if err := held.Release(t.Context()); err != nil {
		t.Fatalf("Release: %v", err)
	}
	if _, err := locks.AcquireResource(t.Context(), FleetEndpointsLock(), second, KindUpgradeEndpoints.String()); err != nil {
		t.Fatalf("a roll starting after the first finished was refused: %v", err)
	}
}

// TestFleetLockRefusesRatherThanQueueing: the two ways a lock can be denied mean
// different things — a refusal is work that was never attempted, a bounded wait
// that ran out is a queue that did not clear in time — and both wrap ErrLockHeld,
// so ErrLockRefused is what separates them. Naming the WORK behind the lock is the
// service's job (ErrFleetRollInProgress), not this layer's.
func TestFleetLockRefusesRatherThanQueueing(t *testing.T) {
	store := NewMemStore()
	locks := store.Locks(ownerID())

	if _, err := locks.AcquireResource(t.Context(), FleetEndpointsLock(), uuid.New(), KindUpgradeEndpoints.String()); err != nil {
		t.Fatalf("first roll was refused the fleet lock: %v", err)
	}
	_, err := locks.AcquireResource(t.Context(), FleetEndpointsLock(), uuid.New(), KindUpgradeEndpoints.String())
	if err == nil {
		t.Fatal("the second roll was granted the fleet lock")
	}
	if !errors.Is(err, ErrLockRefused) {
		t.Errorf("refusal = %v, want an error wrapping ErrLockRefused", err)
	}

	// Ordinary endpoint contention waits instead, so it is never a refusal.
	lock := EndpointLock(uuid.NewString())
	if _, err := locks.AcquireResource(t.Context(), lock, uuid.New(), KindResizeEndpoint.String()); err != nil {
		t.Fatalf("endpoint lock: %v", err)
	}
	waitCtx, cancel := context.WithTimeout(t.Context(), 50*time.Millisecond)
	defer cancel()
	if _, err := locks.AcquireResource(waitCtx, lock, uuid.New(), KindDeleteEndpoint.String()); errors.Is(err, ErrLockRefused) {
		t.Errorf("a contended endpoint reported %v, which names the wrong contention", err)
	}
}

// TestEndpointLockStillWaitsForItsTurn is the other half of the asymmetry, and
// the reason NoWait is per-lock rather than per-acquire: only the FLEET refuses.
// A child whose endpoint a resize is holding must join the queue — refusing it
// would turn ordinary contention into a failed roll (§5.4).
func TestEndpointLockStillWaitsForItsTurn(t *testing.T) {
	if EndpointLock(uuid.NewString()).NoWait {
		t.Fatal("the endpoint lock refuses instead of queueing; a contended endpoint must wait")
	}

	store := NewMemStore()
	locks := store.Locks(ownerID())
	lock := EndpointLock(uuid.NewString())

	held, err := locks.AcquireResource(t.Context(), lock, uuid.New(), KindResizeEndpoint.String())
	if err != nil {
		t.Fatalf("first task was refused the endpoint lock: %v", err)
	}
	// The holder finishes shortly. The waiter has to still be there to take the
	// lock: one that had been refused on its first attempt is long gone.
	go func() {
		time.Sleep(20 * time.Millisecond)
		_ = held.Release(context.WithoutCancel(t.Context()))
	}()
	if _, err := locks.AcquireResource(t.Context(), lock, uuid.New(), KindDeleteEndpoint.String()); err != nil {
		t.Fatalf("a task on a busy endpoint was refused rather than queued: %v", err)
	}
}

// TestFleetLockWaitsOutItsOwnLapsedRow is the case a bare refusal gets wrong. The
// task lease is what makes a crashed roll claimable (Candidates probes
// `task:<id>` and nothing else), and `fleet:endpoints` is a separate row on its
// own renewal — so the reclaiming worker can find the fleet still held by the very
// execution it is replacing, for up to one renewal interval past the task lease.
//
// Refusing there fails the roll durably, saying "another fleet upgrade is already
// running" about its own dead predecessor, and strands the subtasks that execution
// created: a re-fired roll derives child ids from a NEW parent id, so nothing ever
// picks them up.
func TestFleetLockWaitsOutItsOwnLapsedRow(t *testing.T) {
	store := NewMemStore()
	dead, alive := store.Locks("proc-a"), store.Locks("proc-b")
	roll := uuid.New()

	if _, err := dead.AcquireResource(t.Context(), FleetEndpointsLock(), roll, KindUpgradeEndpoints.String()); err != nil {
		t.Fatalf("the first execution was refused the fleet lock: %v", err)
	}

	// proc-a is gone but its row has not lapsed yet. The assertion is that the
	// reclaim WAITED: a refusal would come back with ErrLockRefused well inside
	// this deadline, so the deadline expiring is the evidence.
	waitCtx, cancel := context.WithTimeout(t.Context(), 50*time.Millisecond)
	_, err := alive.AcquireResource(waitCtx, FleetEndpointsLock(), roll, KindUpgradeEndpoints.String())
	cancel()
	if errors.Is(err, ErrLockRefused) {
		t.Fatalf("the reclaiming execution was refused its own predecessor's lock: %v", err)
	}
	if !errors.Is(err, ErrLockHeld) {
		t.Fatalf("bounded wait ended with err=%v, want an error wrapping ErrLockHeld", err)
	}

	// And once that row lapses the reclaim proceeds, which is the whole point of
	// waiting rather than refusing.
	store.locks.expire(FleetEndpointsLock().Key())
	if _, err := alive.AcquireResource(t.Context(), FleetEndpointsLock(), roll, KindUpgradeEndpoints.String()); err != nil {
		t.Errorf("after the predecessor's row lapsed the reclaim was still refused: %v", err)
	}
}

// The other half: the carve-out above must not turn "refused" into "wait for
// whoever has it". A different roll, in a different process — the shape a
// self-reclaim is easiest to confuse with — is still refused at once.
func TestFleetLockRefusesADifferentRollFromAnotherProcess(t *testing.T) {
	store := NewMemStore()
	first, second := store.Locks("proc-a"), store.Locks("proc-b")

	if _, err := first.AcquireResource(t.Context(), FleetEndpointsLock(), uuid.New(), KindUpgradeEndpoints.String()); err != nil {
		t.Fatalf("first roll was refused the fleet lock: %v", err)
	}
	// No deadline: the refusal must come from the lock, not from patience.
	start := time.Now()
	_, err := second.AcquireResource(t.Context(), FleetEndpointsLock(), uuid.New(), KindUpgradeEndpoints.String())
	waited := time.Since(start)
	if !errors.Is(err, ErrLockHeld) {
		t.Fatalf("the second roll failed with %v, want an error wrapping ErrLockHeld", err)
	}
	if waited > memLockWait/2 {
		t.Errorf("the second roll waited %s; a different roll must be refused at once, not queued", waited)
	}
}

// noWaitDenial is where the three answers are decided, so it is asserted directly:
// the wording is chosen from the CLASSIFIED error, not from the fact that an
// acquire failed.
func TestNoWaitDenialSeparatesContentionFromFailure(t *testing.T) {
	r := FleetEndpointsLock()
	roll := uuid.New()
	self := resourceOwner(roll, KindUpgradeEndpoints.String(), "proc-b")
	predecessor := resourceOwner(roll, KindUpgradeEndpoints.String(), "proc-a")
	rival := resourceOwner(uuid.New(), KindUpgradeEndpoints.String(), "proc-c")

	denial := func(holder string) error {
		return &pglease.Denial{Key: r.Key(), Holder: holder}
	}

	// An infrastructure failure is not contention, and must not be reported as a
	// lock somebody holds.
	for name, boom := range map[string]error{
		"backend error":     errors.New("dial tcp 10.0.0.1:5433: connect: connection refused"),
		"cancelled context": context.Canceled,
	} {
		err := noWaitDenial(r, boom, self)
		switch {
		case err == nil:
			t.Errorf("%s: noWaitDenial returned nil; only a self-reclaim may wait", name)
		case errors.Is(err, ErrLockHeld):
			t.Errorf("%s: reported as contention: %v", name, err)
		case !errors.Is(err, boom):
			t.Errorf("%s: the cause was dropped: %v", name, err)
		}
	}

	// Contention from a different roll: refused, and named.
	if err := noWaitDenial(r, denial(rival), self); !errors.Is(err, ErrLockHeld) {
		t.Errorf("a rival roll's denial = %v, want an error wrapping ErrLockHeld", err)
	}

	// Contention from this roll's own predecessor: nil, meaning wait it out.
	if err := noWaitDenial(r, denial(predecessor), self); err != nil {
		t.Errorf("our own predecessor's denial = %v, want nil (wait it out)", err)
	}

	// A denial that could not name a holder is refused, not waited on: "unknown"
	// must never be read as "mine".
	if err := noWaitDenial(r, denial(""), self); !errors.Is(err, ErrLockHeld) {
		t.Errorf("a denial with no visible holder = %v, want an error wrapping ErrLockHeld", err)
	}
}

func TestHolderIDOf(t *testing.T) {
	id := uuid.New()
	for name, tc := range map[string]struct {
		owner string
		want  uuid.UUID
		ok    bool
	}{
		"a task's owner":    {owner: resourceOwner(id, KindResizeEndpoint.String(), "proc-a"), want: id, ok: true},
		"a subtask's owner": {owner: resourceOwner(id, StepUpgradeEndpoint, "proc-a"), want: id, ok: true},
		// The compute write fence keys the same table, so its owners turn up in a
		// Holder read. They carry no task field and must not parse as one.
		"a foreign owner": {owner: "compute-ctl@host/7/abc"},
		"empty":           {owner: ""},
		"not a uuid":      {owner: "task=not-a-uuid kind=x proc=y"},
	} {
		got, ok := holderIDOf(tc.owner)
		if ok != tc.ok || got != tc.want {
			t.Errorf("%s: holderIDOf(%q) = %v, %v; want %v, %v", name, tc.owner, got, ok, tc.want, tc.ok)
		}
	}
}

func TestHeldByOwnPredecessor(t *testing.T) {
	roll, other := uuid.New(), uuid.New()
	self := resourceOwner(roll, KindUpgradeEndpoints.String(), "proc-b")

	for name, tc := range map[string]struct {
		holder string
		want   bool
	}{
		"same unit of work, dead process": {holder: resourceOwner(roll, KindUpgradeEndpoints.String(), "proc-a"), want: true},
		"a different unit of work":        {holder: resourceOwner(other, KindUpgradeEndpoints.String(), "proc-a")},
		// Byte-identical means pglease would have granted it as a renewal, so a
		// refusal here is one to honour rather than to wait out.
		"this very acquire": {holder: self},
		"no visible holder": {holder: ""},
		"a foreign owner":   {holder: "compute-ctl@host/7/abc"},
	} {
		if got := heldByOwnPredecessor(tc.holder, self); got != tc.want {
			t.Errorf("%s: heldByOwnPredecessor(%q, self) = %v, want %v", name, tc.holder, got, tc.want)
		}
	}
}
