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

	// The endpoint pass, kept in fields of its own so a cluster assertion still
	// counts only clusters. A fake with no endpoints seeded answers the endpoint
	// pass with an empty batch, which is what makes the cluster-only tests below
	// unaffected by there being a second pass at all.
	endpoints         []core.OrphanEndpoint
	endpointSelectErr error
	endpointSelects   int
	endpointPrechecks int
	// order is the resource kinds selected, in the order the sweep asked for them.
	order []string
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
	f.order = append(f.order, resourceCluster)
	f.gotCutoff, f.gotLimit = failedBefore, limit
	if f.selectErr != nil {
		return nil, f.selectErr
	}
	out := make([]core.OrphanCluster, len(f.batch))
	copy(out, f.batch)
	return out, nil
}

func (f *fakeOrphans) FailedCreateEndpoints(_ context.Context, failedBefore time.Time, limit int) ([]core.OrphanEndpoint, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.endpointSelects++
	f.order = append(f.order, resourceEndpoint)
	f.gotCutoff, f.gotLimit = failedBefore, limit
	if f.endpointSelectErr != nil {
		return nil, f.endpointSelectErr
	}
	out := make([]core.OrphanEndpoint, len(f.endpoints))
	copy(out, f.endpoints)
	return out, nil
}

func (f *fakeOrphans) IsFailedCreateEndpoint(_ context.Context, id uuid.UUID) (bool, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.endpointPrechecks++
	if f.precheckErr != nil {
		return false, f.precheckErr
	}
	return f.reclaimable[id] && !f.deleting[id], nil
}

// withEndpoints seeds the endpoint pass, returning f so it composes with
// newFakeOrphans in one expression.
func (f *fakeOrphans) withEndpoints(ids ...uuid.UUID) *fakeOrphans {
	for _, id := range ids {
		f.endpoints = append(f.endpoints, core.OrphanEndpoint{
			EndpointID: id, BranchID: uuid.New(), ClusterID: uuid.New(), FailedAt: time.Unix(0, 0),
		})
		f.reclaimable[id] = true
	}
	return f
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
	mu            sync.Mutex
	db            *fakeOrphans
	calls         []uuid.UUID
	endpointCalls []uuid.UUID
	err           error
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

func (f *fakeTasks) DeleteEndpoint(_ context.Context, in task.DeleteEndpointInput) (*task.Task, error) {
	if f.hold != nil {
		<-f.hold
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.err != nil {
		return nil, f.err
	}
	f.endpointCalls = append(f.endpointCalls, in.EndpointID)
	if f.db != nil {
		f.db.mu.Lock()
		f.db.deleting[in.EndpointID] = true
		f.db.mu.Unlock()
	}
	return &task.Task{ID: uuid.New()}, nil
}

func (f *fakeTasks) endpointsEnqueued() []uuid.UUID {
	f.mu.Lock()
	defer f.mu.Unlock()
	out := make([]uuid.UUID, len(f.endpointCalls))
	copy(out, f.endpointCalls)
	return out
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

// fakeMeter records the per-resource counts, so the metric's resource label is
// asserted rather than assumed.
type fakeMeter struct {
	sweeps  int
	orphans map[string][3]int // resource -> {enqueued, skipped, failed}
}

func newFakeMeter() *fakeMeter {
	return &fakeMeter{orphans: map[string][3]int{}}
}

func (m *fakeMeter) GCSwept(time.Duration) { m.sweeps++ }

func (m *fakeMeter) GCOrphans(resource string, enqueued, skipped, failed int) {
	c := m.orphans[resource]
	m.orphans[resource] = [3]int{c[0] + enqueued, c[1] + skipped, c[2] + failed}
}

// The endpoint pass is the second half of the reclaim: an endpoint whose creating
// task failed before it was ever addressable is torn down with an ordinary
// delete_endpoint, addressed by (cluster, endpoint) the way the API's own delete is.
func TestSweepReclaimsEndpointOrphans(t *testing.T) {
	a, b := uuid.New(), uuid.New()
	db := newFakeOrphans().withEndpoints(a, b)
	tasks := &fakeTasks{db: db}

	if err := newSweeper(db, tasks, Config{}).Sweep(context.Background()); err != nil {
		t.Fatalf("Sweep: %v", err)
	}
	// Assert the *ids*, not just the count: each orphan's teardown closure must
	// carry its own, so a batch cannot enqueue one endpoint twice and miss another.
	got := map[uuid.UUID]int{}
	for _, id := range tasks.endpointsEnqueued() {
		got[id]++
	}
	if got[a] != 1 || got[b] != 1 || len(got) != 2 {
		t.Fatalf("endpoint teardowns = %v, want exactly one each for %s and %s", got, a, b)
	}
	if got := tasks.enqueued(); len(got) != 0 {
		t.Errorf("enqueued %v cluster teardowns, want none: no cluster orphans were seeded", got)
	}
}

// Every pass runs on every sweep, and clusters go first: a cluster teardown
// cascades to the endpoints under it, and the endpoint selection excludes what a
// failed-create cluster already covers, so the order is what stops one sweep
// spending two tasks on the same endpoint.
func TestSweepRunsClusterPassBeforeEndpointPass(t *testing.T) {
	db := newFakeOrphans(uuid.New()).withEndpoints(uuid.New())
	if err := newSweeper(db, &fakeTasks{db: db}, Config{}).Sweep(context.Background()); err != nil {
		t.Fatalf("Sweep: %v", err)
	}
	want := []string{resourceCluster, resourceEndpoint}
	if len(db.order) != 2 || db.order[0] != want[0] || db.order[1] != want[1] {
		t.Errorf("selection order = %v, want %v", db.order, want)
	}
}

// One kind's selection being broken must not cost the other kind its reclaim. The
// error is still reported, so a permanently failing query is visible rather than
// silently swallowed.
func TestSweepEndpointPassRunsWhenClusterSelectionFails(t *testing.T) {
	id := uuid.New()
	db := newFakeOrphans().withEndpoints(id)
	db.selectErr = errors.New("clusters query broken")
	tasks := &fakeTasks{db: db}

	err := newSweeper(db, tasks, Config{}).Sweep(context.Background())
	if err == nil {
		t.Fatal("Sweep should report the cluster selection error")
	}
	got := tasks.endpointsEnqueued()
	if len(got) != 1 || got[0] != id {
		t.Errorf("endpoint teardowns = %v, want [%s]: a broken cluster query must not stop the endpoint pass", got, id)
	}
}

// The precheck guards the endpoint pass the same way it guards the cluster one.
func TestSweepSkipsEndpointThatIsNoLongerReclaimable(t *testing.T) {
	id := uuid.New()
	db := newFakeOrphans().withEndpoints(id)
	db.reclaimable[id] = false
	tasks := &fakeTasks{db: db}

	if err := newSweeper(db, tasks, Config{}).Sweep(context.Background()); err != nil {
		t.Fatalf("Sweep: %v", err)
	}
	if got := tasks.endpointsEnqueued(); len(got) != 0 {
		t.Fatalf("enqueued %v, want none: the precheck said it was not reclaimable", got)
	}
	if db.endpointPrechecks != 1 {
		t.Errorf("endpoint prechecks = %d, want 1", db.endpointPrechecks)
	}
}

// BatchLimit is per kind: a backlog of clusters must not leave endpoints unswept.
func TestSweepAppliesBatchLimitPerResource(t *testing.T) {
	db := newFakeOrphans(uuid.New()).withEndpoints(uuid.New())
	s := newSweeper(db, &fakeTasks{db: db}, Config{BatchLimit: 3})
	if err := s.Sweep(context.Background()); err != nil {
		t.Fatalf("Sweep: %v", err)
	}
	if db.selects != 1 || db.endpointSelects != 1 {
		t.Errorf("selects = %d cluster / %d endpoint, want 1 each", db.selects, db.endpointSelects)
	}
	if db.gotLimit != 3 {
		t.Errorf("limit = %d, want the full 3 handed to each pass, not a share of it", db.gotLimit)
	}
}

// The counts are metered per resource, which is what makes "endpoints are piling
// up while clusters are clean" answerable from the metric alone.
func TestSweepMetersEachResourceSeparately(t *testing.T) {
	skipped := uuid.New()
	db := newFakeOrphans(uuid.New()).withEndpoints(uuid.New(), skipped)
	db.reclaimable[skipped] = false
	meter := newFakeMeter()
	s := New(db, &fakeTasks{db: db}, Config{BatchLimit: 10}, quietLogger(), meter)
	s.now = func() time.Time { return time.Unix(1_000_000, 0) }

	if err := s.Sweep(context.Background()); err != nil {
		t.Fatalf("Sweep: %v", err)
	}
	if meter.sweeps != 1 {
		t.Errorf("sweeps = %d, want 1 for the whole pass, not one per resource", meter.sweeps)
	}
	if got, want := meter.orphans[resourceCluster], [3]int{1, 0, 0}; got != want {
		t.Errorf("cluster counts = %v, want %v (enqueued, skipped, failed)", got, want)
	}
	if got, want := meter.orphans[resourceEndpoint], [3]int{1, 1, 0}; got != want {
		t.Errorf("endpoint counts = %v, want %v (enqueued, skipped, failed)", got, want)
	}
}
