package gc

import (
	"context"
	"errors"
	"io"
	"log/slog"
	"sync"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/yugabyte/amp/controller/internal/core"
	"github.com/yugabyte/amp/controller/internal/task"
)

func quietLogger() *slog.Logger {
	return slog.New(slog.NewTextHandler(io.Discard, nil))
}

// fakeOrphans models the database. reclaimable is the set the precheck answers
// true for; deleting records clusters a teardown has been enqueued for, which is
// what makes the precheck answer false afterwards — the behaviour the real
// query's "no pending/running delete_cluster" clause produces.
type fakeOrphans struct {
	mu          sync.Mutex
	batch       []core.OrphanCluster
	reclaimable map[uuid.UUID]bool
	deleting    map[uuid.UUID]bool
	selectErr   error
	precheckErr error

	gotCutoff time.Time
	gotLimit  int
	selects   int
	prechecks int
}

func newFakeOrphans(ids ...uuid.UUID) *fakeOrphans {
	f := &fakeOrphans{
		reclaimable: map[uuid.UUID]bool{},
		deleting:    map[uuid.UUID]bool{},
	}
	for _, id := range ids {
		f.batch = append(f.batch, core.OrphanCluster{
			ClusterID: id, PoolID: uuid.New(), FailedAt: time.Unix(0, 0),
		})
		f.reclaimable[id] = true
	}
	return f
}

func (f *fakeOrphans) FailedCreateClusters(_ context.Context, failedBefore time.Time, limit int) ([]core.OrphanCluster, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.selects++
	f.gotCutoff, f.gotLimit = failedBefore, limit
	if f.selectErr != nil {
		return nil, f.selectErr
	}
	out := make([]core.OrphanCluster, len(f.batch))
	copy(out, f.batch)
	return out, nil
}

func (f *fakeOrphans) IsFailedCreateCluster(_ context.Context, id uuid.UUID) (bool, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.prechecks++
	if f.precheckErr != nil {
		return false, f.precheckErr
	}
	return f.reclaimable[id] && !f.deleting[id], nil
}

// fakeTasks records enqueued teardowns and, like the real store, makes the
// orphan un-reclaimable once one exists.
type fakeTasks struct {
	mu    sync.Mutex
	db    *fakeOrphans
	calls []uuid.UUID
	err   error
	// hold, when non-nil, blocks inside DeleteCluster so a test can interleave
	// two sweepers at the exact point between precheck and enqueue.
	hold chan struct{}
}

func (f *fakeTasks) DeleteCluster(_ context.Context, in task.DeleteClusterInput) (*task.Task, error) {
	if f.hold != nil {
		<-f.hold
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.err != nil {
		return nil, f.err
	}
	f.calls = append(f.calls, in.ClusterID)
	if f.db != nil {
		f.db.mu.Lock()
		f.db.deleting[in.ClusterID] = true
		f.db.mu.Unlock()
	}
	return &task.Task{ID: uuid.New()}, nil
}

func (f *fakeTasks) enqueued() []uuid.UUID {
	f.mu.Lock()
	defer f.mu.Unlock()
	out := make([]uuid.UUID, len(f.calls))
	copy(out, f.calls)
	return out
}

func newSweeper(o Orphans, t Enqueuer, cfg Config) *Sweeper {
	if cfg.BatchLimit == 0 {
		cfg.BatchLimit = 10
	}
	s := New(o, t, cfg, quietLogger(), nil)
	s.now = func() time.Time { return time.Unix(1_000_000, 0) }
	return s
}

func TestSweepEnqueuesTeardownPerOrphan(t *testing.T) {
	a, b := uuid.New(), uuid.New()
	db := newFakeOrphans(a, b)
	tasks := &fakeTasks{db: db}

	if err := newSweeper(db, tasks, Config{}).Sweep(context.Background()); err != nil {
		t.Fatalf("Sweep: %v", err)
	}
	if got := tasks.enqueued(); len(got) != 2 {
		t.Fatalf("enqueued %d teardowns, want 2 (%v)", len(got), got)
	}
}

func TestSweepAppliesGraceAndBatchLimit(t *testing.T) {
	db := newFakeOrphans()
	s := newSweeper(db, &fakeTasks{db: db}, Config{Grace: 30 * time.Minute, BatchLimit: 7})
	if err := s.Sweep(context.Background()); err != nil {
		t.Fatalf("Sweep: %v", err)
	}
	want := time.Unix(1_000_000, 0).Add(-30 * time.Minute)
	if !db.gotCutoff.Equal(want) {
		t.Errorf("cutoff = %v, want now-grace = %v", db.gotCutoff, want)
	}
	if db.gotLimit != 7 {
		t.Errorf("limit = %d, want 7", db.gotLimit)
	}
}

// The precheck is the guard against acting on a stale batch: an orphan selected
// but no longer reclaimable must not be torn down.
func TestSweepSkipsOrphanThatIsNoLongerReclaimable(t *testing.T) {
	id := uuid.New()
	db := newFakeOrphans(id)
	db.reclaimable[id] = false
	tasks := &fakeTasks{db: db}

	if err := newSweeper(db, tasks, Config{}).Sweep(context.Background()); err != nil {
		t.Fatalf("Sweep: %v", err)
	}
	if got := tasks.enqueued(); len(got) != 0 {
		t.Fatalf("enqueued %v, want none: the precheck said it was not reclaimable", got)
	}
	if db.prechecks != 1 {
		t.Errorf("prechecks = %d, want 1", db.prechecks)
	}
}

// A precheck that errors must leave the orphan alone rather than guess. The
// orphan stays selectable, so the next sweep retries it.
func TestSweepLeavesOrphanAloneWhenPrecheckErrors(t *testing.T) {
	db := newFakeOrphans(uuid.New())
	db.precheckErr = errors.New("db down")
	tasks := &fakeTasks{db: db}

	if err := newSweeper(db, tasks, Config{}).Sweep(context.Background()); err != nil {
		t.Fatalf("Sweep should absorb a per-orphan error, got %v", err)
	}
	if got := tasks.enqueued(); len(got) != 0 {
		t.Fatalf("enqueued %v after a failed precheck, want none", got)
	}
}

// One unreclaimable orphan must not abort the batch.
func TestSweepContinuesBatchAfterEnqueueFailure(t *testing.T) {
	db := newFakeOrphans(uuid.New(), uuid.New(), uuid.New())
	tasks := &fakeTasks{db: db, err: errors.New("insert failed")}

	if err := newSweeper(db, tasks, Config{}).Sweep(context.Background()); err != nil {
		t.Fatalf("Sweep should absorb per-orphan errors, got %v", err)
	}
	if db.prechecks != 3 {
		t.Errorf("prechecks = %d, want 3: every orphan should be attempted", db.prechecks)
	}
}

func TestSweepReturnsSelectionError(t *testing.T) {
	db := newFakeOrphans()
	db.selectErr = errors.New("select failed")
	if err := newSweeper(db, &fakeTasks{}, Config{}).Sweep(context.Background()); err == nil {
		t.Fatal("Sweep should return a selection error: there is nothing to iterate")
	}
}

func TestSweepStopsOnCancelledContext(t *testing.T) {
	db := newFakeOrphans(uuid.New(), uuid.New())
	tasks := &fakeTasks{db: db}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()

	if err := newSweeper(db, tasks, Config{}).Sweep(ctx); !errors.Is(err, context.Canceled) {
		t.Fatalf("Sweep err = %v, want context.Canceled", err)
	}
	if got := tasks.enqueued(); len(got) != 0 {
		t.Fatalf("enqueued %v under a cancelled context, want none", got)
	}
}

func TestRunReturnsImmediatelyWhenDisabled(t *testing.T) {
	db := newFakeOrphans(uuid.New())
	s := newSweeper(db, &fakeTasks{db: db}, Config{Enabled: false, Interval: time.Millisecond})

	done := make(chan struct{})
	go func() { s.Run(context.Background()); close(done) }()
	select {
	case <-done:
	case <-time.After(2 * time.Second):
		t.Fatal("Run did not return with Enabled=false")
	}
	if db.selects != 0 {
		t.Errorf("selects = %d, want 0 when disabled", db.selects)
	}
}

// One Sweeper, many concurrent Sweep calls: Sweep holds s.sweeping for the whole
// pass, so each orphan is torn down exactly once however the passes interleave.
// Under -race this also covers the Sweeper's own state for data races.
//
// This assertion is load-bearing: an earlier draft without the mutex passed
// without -race and failed under it, enqueueing some clusters 4 times.
func TestConcurrentSweepsEnqueueEachOrphanOnce(t *testing.T) {
	ids := make([]uuid.UUID, 8)
	for i := range ids {
		ids[i] = uuid.New()
	}
	db := newFakeOrphans(ids...)
	tasks := &fakeTasks{db: db}
	s := newSweeper(db, tasks, Config{})

	const passes = 6
	var wg sync.WaitGroup
	start := make(chan struct{})
	for range passes {
		wg.Add(1)
		go func() {
			defer wg.Done()
			<-start
			if err := s.Sweep(context.Background()); err != nil {
				t.Errorf("Sweep: %v", err)
			}
		}()
	}
	close(start)
	wg.Wait()

	seen := map[uuid.UUID]int{}
	for _, id := range tasks.enqueued() {
		seen[id]++
	}
	for _, id := range ids {
		if seen[id] != 1 {
			t.Errorf("cluster %s enqueued %d times, want exactly 1", id, seen[id])
		}
	}
}

// The window that remains: two *separate* Sweepers — two controller processes —
// share no mutex, so both can pass the precheck before either enqueues. This
// documents the bound rather than asserting it away: delete_cluster is
// idempotent, so the duplicate is wasted work, not damage, and the deployed
// topology is a single controller. If the store ever grows an atomic conditional
// insert for deletes, this is the test that should start failing.
func TestSeparateSweepersCanDuplicateWhenInterleavedAtEnqueue(t *testing.T) {
	id := uuid.New()
	db := newFakeOrphans(id)
	hold := make(chan struct{})
	tasks := &fakeTasks{db: db, hold: hold}

	var wg sync.WaitGroup
	for range 2 {
		wg.Add(1)
		go func() {
			defer wg.Done()
			if err := newSweeper(db, tasks, Config{}).Sweep(context.Background()); err != nil {
				t.Errorf("Sweep: %v", err)
			}
		}()
	}
	// Let both reach DeleteCluster — i.e. both already past the precheck — then
	// release them together.
	time.Sleep(50 * time.Millisecond)
	close(hold)
	wg.Wait()

	if got := len(tasks.enqueued()); got != 2 {
		t.Errorf("enqueued %d, want 2: with both sweepers past the precheck the "+
			"duplicate is expected, and is safe because delete_cluster is idempotent", got)
	}
}
