package task

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"sync"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/yugabyte/amp/controller/internal/config"
	"github.com/yugabyte/amp/controller/internal/db"
	"github.com/yugabyte/amp/controller/pkg/pglease"
)

// The locks against a real database, where MemLocks cannot stand in: it models
// the decision rule but not the clock, and everything below is about the clock —
// a controller that died still holds its task until the lease lapses, and the
// task is reclaimable exactly then. The candidate query's call into
// amp_registry.token_holder only runs here too.
//
// Timings are the production shape at a tenth of the scale: a 5s lease against a
// 30s one in production, so a reclaim costs the suite seconds rather than half a
// minute. What is being asserted is that the wait is the lease, not that it is
// any particular number of seconds.
const (
	pgTestTTL        = 5 * time.Second
	pgTestRenewEvery = time.Second
	// pgReclaimSlack is how much past the lease to allow before calling a reclaim
	// failed: the skew grace plus a poll interval plus room for a slow CI box.
	pgReclaimSlack = 10 * time.Second

	// pgStepDwell is how long a step stays inside Execute, so two tasks racing one
	// endpoint would actually overlap if the lock let them.
	pgStepDwell = 50 * time.Millisecond
)

func pgTestConfig() config.TaskConfig {
	return config.TaskConfig{
		PollInterval:  50 * time.Millisecond,
		LeaseTTL:      pgTestTTL,
		RenewInterval: pgTestRenewEvery,
		MaxConcurrent: 4,
		ShutdownGrace: 2 * time.Second,
		LockWait:      30 * time.Second,
	}
}

// testLeaseLogger discards: these tests assert on task state, not on log lines,
// and the renewal warnings a real controller wants are noise here. It is passed
// explicitly rather than left nil so the wiring is exercised.
func testLeaseLogger() *slog.Logger { return slog.New(slog.NewTextHandler(io.Discard, nil)) }

// controller is one simulated controller process: its own lease client, so its
// owner id (and therefore its fence) is distinct from every other one's.
type controller struct {
	locks *PgLocks
	w     *Worker
	stop  context.CancelFunc
	done  chan struct{}
}

// startController runs a worker against the shared store, as a separate instance
// would. Nothing here is shared with another controller except the database.
func startController(t *testing.T, pool *db.Pool, reg *Registry) *controller {
	t.Helper()
	locks, err := NewPgLocks(pool, testLeaseLogger(), pgTestTTL, pgTestRenewEvery, 30*time.Second)
	if err != nil {
		t.Fatalf("NewPgLocks: %v", err)
	}
	w := NewWorker(NewPgStore(pool), NewPgSubtasks(pool), reg, locks, pgTestConfig(), slog.New(slog.NewTextHandler(io.Discard, nil)))
	ctx, cancel := context.WithCancel(context.WithoutCancel(t.Context()))
	c := &controller{locks: locks, w: w, stop: cancel, done: make(chan struct{})}
	go func() { w.Run(ctx); close(c.done) }()
	t.Cleanup(c.shutdown)
	return c
}

// shutdown stops the controller the way SIGTERM does: in-flight tasks unwind and
// release their leases, so the next instance can take them at once.
func (c *controller) shutdown() {
	c.stop()
	select {
	case <-c.done:
	case <-time.After(10 * time.Second):
	}
}

// killController models a crash rather than a shutdown: the process is gone
// without releasing anything, so its rows sit in resource_token until they lapse.
// The lease is taken straight on the store — no renewal goroutine — which is what
// a dead process's row is.
func killController(t *testing.T, pool *db.Pool, owner string, keys ...string) {
	t.Helper()
	store, err := pglease.NewStore(pool.Pool, pglease.StoreConfig{})
	if err != nil {
		t.Fatalf("lease store: %v", err)
	}
	for _, key := range keys {
		g, err := store.Acquire(context.WithoutCancel(t.Context()), key, owner, pgTestTTL)
		if err != nil {
			t.Fatalf("seeding the dead controller's lease on %s: %v", key, err)
		}
		if !g.Granted {
			t.Fatalf("the dead controller could not take %s: held by %s", key, g.Holder)
		}
	}
}

// resetTasks empties the task log and the lease table. The scratch database is
// shared by every test in this package, and the claim loop looks at every task in
// it, so a leftover row from another test would be work these controllers pick up.
func resetTasks(t *testing.T, pool *db.Pool) {
	t.Helper()
	ctx := context.WithoutCancel(t.Context())
	for _, q := range []string{
		`DELETE FROM task_steps`,
		`DELETE FROM tasks`,
		`DELETE FROM amp_registry.resource_token`,
	} {
		if _, err := pool.Exec(ctx, q); err != nil {
			t.Fatalf("reset %q: %v", q, err)
		}
	}
}

// pgStep records every execution in order, so a test can assert what ran, when,
// and that two tasks never overlapped on one endpoint. It optionally locks an
// endpoint and optionally blocks, to hold a lock open.
type pgStep struct {
	kind     string
	endpoint string
	// fleet makes the step declare `fleet:endpoints` instead of an endpoint lock —
	// what a fleet roll's parent takes.
	fleet bool

	mu      sync.Mutex
	bound   uuid.UUID
	runs    []uuid.UUID
	running int
	peak    int

	block chan struct{} // non-nil: Execute waits on it
}

func (s *pgStep) Kind() string { return s.kind }

func (s *pgStep) Lock() ResourceLock {
	if s.fleet {
		return FleetEndpointsLock()
	}
	if s.endpoint == "" {
		return ResourceLock{}
	}
	return EndpointLock(s.endpoint)
}

func (s *pgStep) Execute(ctx context.Context) StepResult {
	s.mu.Lock()
	s.running++
	s.peak = max(s.peak, s.running)
	s.runs = append(s.runs, s.bound)
	s.mu.Unlock()
	defer func() {
		s.mu.Lock()
		s.running--
		s.mu.Unlock()
	}()

	if s.block != nil {
		select {
		case <-s.block:
		case <-ctx.Done():
			return Fail(ctx.Err())
		}
		return Ok(nil)
	}

	// Dwell, so a concurrent execution is actually observable. Without it the window
	// where running > 0 is nanoseconds wide and peak can never reach 2 — the
	// assertion would hold with the lock removed entirely.
	select {
	case <-time.After(pgStepDwell):
	case <-ctx.Done():
		return Fail(ctx.Err())
	}
	return Ok(nil)
}

// bind records which task this step was reconstructed for. Execute is what appends
// to runs.
func (s *pgStep) bind(id uuid.UUID) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.bound = id
}

func (s *pgStep) runCount() int {
	s.mu.Lock()
	defer s.mu.Unlock()
	return len(s.runs)
}

func (s *pgStep) peakConcurrency() int {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.peak
}

// pgReg registers each step under its kind, binding the task id into the record
// so a test can tell which task ran it.
func pgReg(steps ...*pgStep) *Registry {
	r := NewRegistry()
	for _, s := range steps {
		r.Register(s.kind, func(args json.RawMessage, _ Deps) (Step, error) {
			var a struct {
				TaskID uuid.UUID `json:"task_id"`
			}
			if err := json.Unmarshal(args, &a); err != nil {
				return nil, err
			}
			if a.TaskID == uuid.Nil {
				return nil, fmt.Errorf("%s: step args carry no task_id", s.kind)
			}
			// The id is stamped on the step, NOT recorded here: recording at build
			// time counts reconstructions, and every assertion in this file is about
			// how many times a step actually RAN.
			s.bind(a.TaskID)
			return s, nil
		})
	}
	return r
}

// seedTask writes one task with a single step of the given kind, the way the API
// would plan it.
func seedTask(t *testing.T, s *PgStore, kind Kind, stepKind, endpoint string) *Task {
	t.Helper()
	id := uuid.New()
	args, err := json.Marshal(map[string]string{"task_id": id.String()})
	if err != nil {
		t.Fatalf("marshal step args: %v", err)
	}
	spec := TaskSpec{
		ID:    id,
		Kind:  kind,
		Steps: []StepSpec{{Kind: stepKind, Args: args}},
	}
	if endpoint != "" {
		ref := endpoint
		spec.TargetRef = &ref
	}
	task, err := s.Create(context.WithoutCancel(t.Context()), spec)
	if err != nil {
		t.Fatalf("create %s task: %v", kind, err)
	}
	return task
}

func waitForStatus(t *testing.T, s *PgStore, id uuid.UUID, want Status, within time.Duration) time.Duration {
	t.Helper()
	start := time.Now()
	deadline := start.Add(within)
	for time.Now().Before(deadline) {
		got, err := s.Get(context.WithoutCancel(t.Context()), id)
		if err != nil {
			t.Fatalf("Get task %s: %v", id, err)
		}
		if got != nil && got.Status == want {
			return time.Since(start)
		}
		time.Sleep(50 * time.Millisecond)
	}
	got, _ := s.Get(context.WithoutCancel(t.Context()), id)
	t.Fatalf("task %s did not reach %q within %s (status=%v)", id, want, within, got)
	return 0
}

// TestPgTaskLeaseAdmitsOneInstance is the task lock's whole point against the
// real token_acquire: two controllers, one task, one winner.
func TestPgTaskLeaseAdmitsOneInstance(t *testing.T) {
	pool, _ := migratedPool(t)
	t.Cleanup(pool.Close)
	resetTasks(t, pool)

	a, err := NewPgLocks(pool, testLeaseLogger(), pgTestTTL, pgTestRenewEvery, time.Second)
	if err != nil {
		t.Fatalf("NewPgLocks: %v", err)
	}
	b, err := NewPgLocks(pool, testLeaseLogger(), pgTestTTL, pgTestRenewEvery, time.Second)
	if err != nil {
		t.Fatalf("NewPgLocks: %v", err)
	}
	if a.Owner() == b.Owner() {
		t.Fatalf("two instances in one process share owner id %q; the nonce is what keeps them distinct", a.Owner())
	}

	id := uuid.New()
	held, err := a.AcquireTask(t.Context(), id)
	if err != nil {
		t.Fatalf("first instance was refused: %v", err)
	}
	if _, err := b.AcquireTask(t.Context(), id); !errors.Is(err, ErrLockHeld) {
		t.Fatalf("second instance got err=%v, want ErrLockHeld", err)
	}
	if err := held.Release(t.Context()); err != nil {
		t.Fatalf("Release: %v", err)
	}
	// Release skips the skew grace, so the handover is immediate rather than a
	// TTL away — this is what makes a graceful restart cheap.
	taken, err := b.AcquireTask(t.Context(), id)
	if err != nil {
		t.Fatalf("after release the second instance was still refused: %v", err)
	}
	// Every lease a test takes must be given back before the pool closes under it.
	// A live lease whose renewals start failing is a lost lease, and losing one
	// exits the process — which would take the whole test binary with it.
	t.Cleanup(func() { _ = taken.Release(context.WithoutCancel(t.Context())) })
}

// TestPgCrashedControllerHoldsItsTaskUntilTheLeaseLapses is the reclaim path with
// real timing: a controller dies mid-task, and a survivor picks the task up — but
// not before the dead one's lease has lapsed.
func TestPgCrashedControllerHoldsItsTaskUntilTheLeaseLapses(t *testing.T) {
	pool, _ := migratedPool(t)
	t.Cleanup(pool.Close)
	resetTasks(t, pool)
	store := NewPgStore(pool)

	step := &pgStep{kind: "work"}
	task := seedTask(t, store, Kind{"test", "", "", nil}, step.kind, "")

	// Controller A claimed it and died with the step in flight. Nothing releases
	// the lease and nothing renews it.
	died := time.Now()
	killController(t, pool, "dead-controller-a", taskLockKey(task.ID))
	if _, err := store.Claim(context.WithoutCancel(t.Context()), task.ID, "dead-controller-a"); err != nil {
		t.Fatalf("Claim: %v", err)
	}

	// While that lease is live the task is nobody's to take, which is what stops a
	// second controller running it in parallel.
	cands, err := store.Candidates(t.Context(), claimCandidates)
	if err != nil {
		t.Fatalf("Candidates: %v", err)
	}
	for _, c := range cands {
		if c.ID == task.ID {
			t.Fatal("a task held by a live lease was offered as a claim candidate")
		}
	}

	// Controller B starts. It must wait out the lease and then resume.
	//
	// Measured from killController, not from here: the dead controller's lease clock
	// started there, and the Claim, Candidates and NewPgLocks calls in between would
	// otherwise be charged against the margin (the skew grace is only 500ms).
	startController(t, pool, pgReg(step))
	waitForStatus(t, store, task.ID, StatusSucceeded, pgTestTTL+pgReclaimSlack)
	took := time.Since(died)

	if took < pgTestTTL {
		t.Errorf("the task was reclaimed after %s, before the dead controller's %s lease had lapsed", took, pgTestTTL)
	}
	if got := step.runCount(); got != 1 {
		t.Errorf("the step ran %d time(s) across the restart, want 1", got)
	}
}

// TestPgGracefulRestartHandsTheTaskOverAtOnce is the other half: a controller
// that stops properly releases its leases, so its successor does not wait out a
// TTL for work nobody is doing.
func TestPgGracefulRestartHandsTheTaskOverAtOnce(t *testing.T) {
	pool, _ := migratedPool(t)
	t.Cleanup(pool.Close)
	resetTasks(t, pool)
	store := NewPgStore(pool)

	step := &pgStep{kind: "work", block: make(chan struct{})}
	task := seedTask(t, store, Kind{"test", "", "", nil}, step.kind, "")

	first := startController(t, pool, pgReg(step))
	waitForStatus(t, store, task.ID, StatusRunning, 10*time.Second)

	// SIGTERM, and the lease is given back. Note the step is released first, so it
	// returns Ok and the interruption actually comes from CompleteStep/CompleteTask
	// failing on the just-cancelled context — not from the step noticing the cancel.
	// Either way the task is left running and reclaimable: a shutdown is not a
	// failure. (If those two store writes ever move to WithoutCancel, this
	// controller would complete the task itself and the assertions below would go
	// vacuous.)
	close(step.block)
	first.shutdown()

	if holder, err := first.locks.Holder(t.Context(), ResourceLock{Type: "task", ID: task.ID.String()}); err != nil {
		t.Fatalf("Holder: %v", err)
	} else if holder != "" {
		t.Errorf("after a graceful stop the task lease is still held by %q, want released", holder)
	}

	startController(t, pool, pgReg(step))
	// Well inside one lease: the successor did not have to wait for anything.
	waitForStatus(t, store, task.ID, StatusSucceeded, pgTestTTL)
}

// TestPgEndpointLockSurvivesAControllerRestart is the scenario end to end:
// create an endpoint, lose the controller mid-task, and have the restarted one
// resume it and then run the resize — with the two never overlapping on the
// endpoint, and the resize waiting for the lock the dead controller still holds.
func TestPgEndpointLockSurvivesAControllerRestart(t *testing.T) {
	pool, _ := migratedPool(t)
	t.Cleanup(pool.Close)
	resetTasks(t, pool)
	store := NewPgStore(pool)

	endpoint := uuid.NewString()
	// One step kind for both tasks, so the peak-concurrency count covers the pair:
	// what matters is that nothing touches this endpoint twice at once, not which
	// task is which.
	step := &pgStep{kind: "endpoint-work", endpoint: endpoint}
	reg := pgReg(step)

	create := seedTask(t, store, KindCreateEndpoint, step.kind, endpoint)
	resize := seedTask(t, store, KindResizeEndpoint, step.kind, endpoint)

	// Controller A claimed the create and died holding both the task and the
	// endpoint. The endpoint lock is the interesting one: it is what stops the
	// resize proceeding against an endpoint whose create never finished.
	killController(t, pool, "dead-controller-a",
		taskLockKey(create.ID), EndpointLock(endpoint).Key())
	if _, err := store.Claim(context.WithoutCancel(t.Context()), create.ID, "dead-controller-a"); err != nil {
		t.Fatalf("Claim: %v", err)
	}

	// Controller B restarts and drains both, in whichever order the leases free
	// up. Both must finish, and neither may run while the other holds the endpoint.
	startController(t, pool, reg)
	waitForStatus(t, store, create.ID, StatusSucceeded, pgTestTTL+pgReclaimSlack)
	waitForStatus(t, store, resize.ID, StatusSucceeded, pgTestTTL+pgReclaimSlack)

	if got := step.peakConcurrency(); got != 1 {
		t.Errorf("peak concurrent work on endpoint %s = %d, want 1", endpoint, got)
	}
	if got := step.runCount(); got != 2 {
		t.Errorf("steps ran %d time(s) for 2 tasks, want 2", got)
	}
	// And the lock is given back, so the endpoint is not stranded — polled rather
	// than read once, because the release is the last thing a task does: it happens
	// after CompleteTask, so a task is briefly terminal while still holding its
	// endpoint. Releasing before the terminal write would invert that and leave a
	// non-terminal task with no lock, which is the worse end to crash in.
	locks, err := NewPgLocks(pool, testLeaseLogger(), pgTestTTL, pgTestRenewEvery, time.Second)
	if err != nil {
		t.Fatalf("NewPgLocks: %v", err)
	}
	var lastHolder string
	waitFor(t, 5*time.Second, func() bool {
		holder, err := locks.Holder(context.WithoutCancel(t.Context()), EndpointLock(endpoint))
		if err != nil {
			t.Fatalf("Holder: %v", err)
		}
		lastHolder = holder
		return holder == ""
	})
	if lastHolder != "" {
		t.Errorf("endpoint %s is still locked by %q after both tasks finished", endpoint, lastHolder)
	}
}

// The same contract MemLocks is held to, against the real token_acquire. This is
// what makes the in-memory model's fidelity checkable instead of assumed.
func TestPgLocksContract(t *testing.T) {
	pool, _ := migratedPool(t)
	t.Cleanup(pool.Close)
	resetTasks(t, pool)

	locksContract(t, func(owner string) Locks {
		// One owner per call, as the contract expects; PgLocks generates its own
		// process-unique id, so the argument only has to make them distinct.
		l, err := NewPgLocks(pool, testLeaseLogger(), pgTestTTL, pgTestRenewEvery, time.Second)
		if err != nil {
			t.Fatalf("NewPgLocks(%s): %v", owner, err)
		}
		return l
	})
}

// TestPgFleetLockAdmitsOneRollAtATime is §5.6 over real SQL: `fleet:endpoints` is
// a third namespace in resource_token, and nothing else in `go test ./...` puts
// that key through token_acquire. Two rolls take it in TURN — the second queues
// on the lease rather than being refused, which is the contract change that let
// the fleet-roll unique index and its structural classifier be dropped.
func TestPgFleetLockAdmitsOneRollAtATime(t *testing.T) {
	pool, _ := migratedPool(t)
	t.Cleanup(pool.Close)
	resetTasks(t, pool)
	store := NewPgStore(pool)

	step := &pgStep{kind: "fleet-roll", fleet: true}
	reg := pgReg(step)
	first := seedTask(t, store, KindUpgradeEndpoints, step.kind, "")
	second := seedTask(t, store, KindUpgradeEndpoints, step.kind, "")

	startController(t, pool, reg)
	waitForStatus(t, store, first.ID, StatusSucceeded, pgTestTTL+pgReclaimSlack)
	waitForStatus(t, store, second.ID, StatusSucceeded, pgTestTTL+pgReclaimSlack)

	if got := step.peakConcurrency(); got != 1 {
		t.Errorf("peak concurrent fleet rolls = %d, want 1: two rolls over one fleet have no sensible interleaving", got)
	}
	if got := step.runCount(); got != 2 {
		t.Errorf("rolls ran %d time(s) for 2 tasks, want 2 — the second waits, it is not refused", got)
	}

	locks, err := NewPgLocks(pool, testLeaseLogger(), pgTestTTL, pgTestRenewEvery, time.Second)
	if err != nil {
		t.Fatalf("NewPgLocks: %v", err)
	}
	var lastHolder string
	waitFor(t, 5*time.Second, func() bool {
		holder, err := locks.Holder(context.WithoutCancel(t.Context()), FleetEndpointsLock())
		if err != nil {
			t.Fatalf("Holder: %v", err)
		}
		lastHolder = holder
		return holder == ""
	})
	if lastHolder != "" {
		t.Errorf("fleet:endpoints is still held by %q after both rolls finished", lastHolder)
	}
}
