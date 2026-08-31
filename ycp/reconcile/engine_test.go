package reconcile

import (
	"context"
	"encoding/json"
	"errors"
	"sync"
	"testing"
	"time"

	"meko/ycp/internal/api"
)

func d(kind, name string, gen int64) api.DesiredResource {
	return api.DesiredResource{Kind: kind, Name: name, Generation: gen, Spec: json.RawMessage(`{}`)}
}

func key(kind, name string) ResourceKey { return ResourceKey{Kind: kind, Name: name} }

func actionsByKey(as []Action) map[ResourceKey]Action {
	m := map[ResourceKey]Action{}
	for _, a := range as {
		m[a.Key] = a
	}
	return m
}

func TestDiff(t *testing.T) {
	desired := []api.DesiredResource{
		d("cluster", "keep", 3), // unchanged: cache says applied 3
		d("cluster", "bump", 5), // changed: cache says applied 4
		d("cluster", "new", 1),  // create
	}
	actual := []Actual{
		{Key: key("cluster", "keep"), ObservedGen: 3},
		{Key: key("cluster", "bump"), ObservedGen: 4},
		{Key: key("cluster", "stale")}, // withdrawn: delete
	}
	cache := map[ResourceKey]int64{}

	got := actionsByKey(Diff(desired, actual, cache))

	want := map[ResourceKey]ActionType{
		key("cluster", "keep"):  ActionNoop,
		key("cluster", "bump"):  ActionUpdate,
		key("cluster", "new"):   ActionCreate,
		key("cluster", "stale"): ActionDelete,
	}
	if len(got) != len(want) {
		t.Fatalf("got %d actions, want %d: %+v", len(got), len(want), got)
	}
	for k, wt := range want {
		if got[k].Type != wt {
			t.Errorf("%s: got %s, want %s", k, got[k].Type, wt)
		}
	}
}

// A backend that echoes gen 0 (does not report observed generation) must still
// reach a no-op once the engine's own cache records the applied generation.
func TestDiffFallsBackToCache(t *testing.T) {
	desired := []api.DesiredResource{d("cluster", "c", 7)}
	actual := []Actual{{Key: key("cluster", "c"), ObservedGen: 0}}

	// No cache yet -> update.
	if got := Diff(desired, actual, map[ResourceKey]int64{}); got[0].Type != ActionUpdate {
		t.Fatalf("without cache: got %s, want update", got[0].Type)
	}
	// Cache records applied 7 -> no-op even though backend echoes 0.
	cache := map[ResourceKey]int64{key("cluster", "c"): 7}
	if got := Diff(desired, actual, cache); got[0].Type != ActionNoop {
		t.Fatalf("with cache: got %s, want noop", got[0].Type)
	}
}

// fakeBackend records calls and lets a test script readiness, parentage, and
// failures.
type fakeBackend struct {
	mu       sync.Mutex
	actual   []Actual
	parents  map[ResourceKey]ResourceKey
	failCall map[ResourceKey]int  // remaining forced failures per key
	perm     map[ResourceKey]bool // keys whose verb returns a PermanentError

	attempts map[ResourceKey]int // every verb call, success or not
	creates  []ResourceKey
	updates  []ResourceKey
	deletes  []ResourceKey
}

func newFake() *fakeBackend {
	return &fakeBackend{
		parents:  map[ResourceKey]ResourceKey{},
		failCall: map[ResourceKey]int{},
		perm:     map[ResourceKey]bool{},
		attempts: map[ResourceKey]int{},
	}
}

func (f *fakeBackend) List(context.Context) ([]Actual, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	out := make([]Actual, len(f.actual))
	copy(out, f.actual)
	return out, nil
}

func (f *fakeBackend) Parent(r api.DesiredResource) (ResourceKey, bool) {
	p, ok := f.parents[ResourceKey{r.Kind, r.Name}]
	return p, ok
}

// maybeReject records the attempt and returns a permanent or forced-transient
// error if the key is scripted for one. Caller holds f.mu.
func (f *fakeBackend) maybeReject(k ResourceKey) error {
	f.attempts[k]++
	if f.perm[k] {
		return Permanent(errors.New("unsupported"))
	}
	if f.failCall[k] > 0 {
		f.failCall[k]--
		return errors.New("forced failure")
	}
	return nil
}

func (f *fakeBackend) Create(_ context.Context, r api.DesiredResource) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	k := ResourceKey{r.Kind, r.Name}
	if err := f.maybeReject(k); err != nil {
		return err
	}
	f.creates = append(f.creates, k)
	return nil
}

func (f *fakeBackend) Update(_ context.Context, r api.DesiredResource) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	k := ResourceKey{r.Kind, r.Name}
	if err := f.maybeReject(k); err != nil {
		return err
	}
	f.updates = append(f.updates, k)
	return nil
}

func (f *fakeBackend) Delete(_ context.Context, k ResourceKey) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	if err := f.maybeReject(k); err != nil {
		return err
	}
	f.deletes = append(f.deletes, k)
	return nil
}

// A child whose parent is absent/not-ready is deferred, not created; once the
// parent is present and Ready, the child is created on a later tick.
func TestReconcileGatesChildOnParentReadiness(t *testing.T) {
	f := newFake()
	f.parents[key("branch", "b")] = key("cluster", "c")
	desired := []api.DesiredResource{d("cluster", "c", 1), d("branch", "b", 1)}

	// Tick 1: cluster absent -> only cluster created, branch deferred.
	e := New(f, Options{})
	res, err := e.Reconcile(context.Background(), desired, true)
	if err != nil {
		t.Fatal(err)
	}
	if res.Created != 1 || res.Deferred != 1 {
		t.Fatalf("tick1: created=%d deferred=%d, want 1/1", res.Created, res.Deferred)
	}

	// Cluster now exists but is not Ready -> branch still deferred.
	f.actual = []Actual{{Key: key("cluster", "c"), ObservedGen: 1, Ready: false}}
	res, _ = e.Reconcile(context.Background(), desired, true)
	if res.Deferred != 1 || res.Created != 0 {
		t.Fatalf("tick2: created=%d deferred=%d, want 0/1", res.Created, res.Deferred)
	}

	// Cluster Ready -> branch created.
	f.actual = []Actual{{Key: key("cluster", "c"), ObservedGen: 1, Ready: true}}
	res, _ = e.Reconcile(context.Background(), desired, true)
	if res.Created != 1 {
		t.Fatalf("tick3: created=%d, want 1", res.Created)
	}
}

// Deletes only fire on a fresh desired read; a stale tick must never GC.
func TestReconcileNoDeleteOnStaleDesired(t *testing.T) {
	f := newFake()
	f.actual = []Actual{{Key: key("cluster", "gone")}}
	e := New(f, Options{})

	res, _ := e.Reconcile(context.Background(), nil /*desired*/, false /*stale*/)
	if res.Deleted != 0 || len(f.deletes) != 0 {
		t.Fatalf("stale: deleted=%d, want 0", res.Deleted)
	}
	res, _ = e.Reconcile(context.Background(), nil, true /*fresh*/)
	if res.Deleted != 1 {
		t.Fatalf("fresh: deleted=%d, want 1", res.Deleted)
	}
}

// Only the top of a withdrawn subtree is deleted; the backend cascades the rest.
func TestReconcileDeletesTopOfWithdrawnSubtree(t *testing.T) {
	f := newFake()
	f.actual = []Actual{
		{Key: key("cluster", "c")},
		{Key: key("branch", "b"), Parent: key("cluster", "c")},
	}
	e := New(f, Options{})
	res, _ := e.Reconcile(context.Background(), nil, true)
	if res.Deleted != 1 {
		t.Fatalf("deleted=%d, want 1 (only the cluster)", res.Deleted)
	}
	if len(f.deletes) != 1 || f.deletes[0] != key("cluster", "c") {
		t.Fatalf("deletes=%v, want [cluster/c]", f.deletes)
	}
}

// A failed apply backs off: it is retried only after the backoff window.
func TestReconcileBackoff(t *testing.T) {
	f := newFake()
	f.failCall[key("cluster", "c")] = 1 // fail the first create
	desired := []api.DesiredResource{d("cluster", "c", 1)}

	now := time.Unix(0, 0)
	e := New(f, Options{Now: func() time.Time { return now }, BaseBackoff: time.Minute})

	res, _ := e.Reconcile(context.Background(), desired, true)
	if len(res.Failed) != 1 {
		t.Fatalf("tick1: failed=%d, want 1", len(res.Failed))
	}
	// Immediately retry: still inside backoff -> deferred, no new attempt.
	res, _ = e.Reconcile(context.Background(), desired, true)
	if res.Created != 0 || res.Deferred != 1 {
		t.Fatalf("tick2: created=%d deferred=%d, want 0/1", res.Created, res.Deferred)
	}
	// Advance past backoff -> retried and (no more forced failure) succeeds.
	now = now.Add(2 * time.Minute)
	res, _ = e.Reconcile(context.Background(), desired, true)
	if res.Created != 1 {
		t.Fatalf("tick3: created=%d, want 1", res.Created)
	}
}

// A permanent (unsupported) verb is surfaced as drift and never re-issued: the
// environment must not flap on an endless backoff of a call that can't succeed.
func TestReconcilePermanentNotRetried(t *testing.T) {
	f := newFake()
	// A withdrawn pool whose delete the backend rejects permanently.
	f.actual = []Actual{{Key: key("storage-pool", "p")}}
	f.perm[key("storage-pool", "p")] = true

	e := New(f, Options{})

	// Tick 1: the delete is attempted once and comes back permanent.
	res, _ := e.Reconcile(context.Background(), nil, true)
	if len(res.Permanent) != 1 || len(res.Failed) != 0 || res.Deleted != 0 {
		t.Fatalf("tick1: permanent=%d failed=%d deleted=%d, want 1/0/0", len(res.Permanent), len(res.Failed), res.Deleted)
	}
	if f.attempts[key("storage-pool", "p")] != 1 {
		t.Fatalf("tick1: attempts=%d, want 1", f.attempts[key("storage-pool", "p")])
	}

	// Tick 2+: still reported as drift, but not re-issued (no backoff flap).
	res, _ = e.Reconcile(context.Background(), nil, true)
	if len(res.Permanent) != 1 || res.Deferred != 0 {
		t.Fatalf("tick2: permanent=%d deferred=%d, want 1/0", len(res.Permanent), res.Deferred)
	}
	if f.attempts[key("storage-pool", "p")] != 1 {
		t.Fatalf("tick2: attempts=%d, want still 1 (not retried)", f.attempts[key("storage-pool", "p")])
	}

	// Re-declaring the pool clears the drift (the delete is no longer planned).
	res, _ = e.Reconcile(context.Background(), []api.DesiredResource{d("storage-pool", "p", 0)}, true)
	if len(res.Permanent) != 0 {
		t.Fatalf("tick3: permanent=%d, want 0 after re-declaring", len(res.Permanent))
	}
}

// A create the backend refuses for the spec as declared must not wedge the
// resource: its action stays planned every tick (the resource never appears in
// List), so a mark keyed on the resource alone would never expire and the
// corrected spec would never be applied — with the environment still reporting
// READY, since permanent drift does not degrade it.
func TestReconcilePermanentRetiredByNewGeneration(t *testing.T) {
	f := newFake()
	k := key("branch", "p/c/b")
	f.perm[k] = true

	e := New(f, Options{})
	bad := []api.DesiredResource{d("branch", "p/c/b", 0)}

	res, _ := e.Reconcile(context.Background(), bad, true)
	if len(res.Permanent) != 1 || res.Created != 0 {
		t.Fatalf("tick1: permanent=%d created=%d, want 1/0", len(res.Permanent), res.Created)
	}
	// Same spec, same generation: reported, not re-issued.
	res, _ = e.Reconcile(context.Background(), bad, true)
	if len(res.Permanent) != 1 || f.attempts[k] != 1 {
		t.Fatalf("tick2: permanent=%d attempts=%d, want 1/1", len(res.Permanent), f.attempts[k])
	}

	// The operator fixes the spec. The bumped generation retires the mark, so the
	// create is asked again — and now succeeds.
	f.perm[k] = false
	res, _ = e.Reconcile(context.Background(), []api.DesiredResource{d("branch", "p/c/b", 1)}, true)
	if res.Created != 1 || len(res.Permanent) != 0 {
		t.Fatalf("tick3: created=%d permanent=%d, want 1/0 — a corrected spec must un-stick the resource",
			res.Created, len(res.Permanent))
	}
}

// A resource that sits non-Ready in the same state past StuckAfter is flagged,
// and the flag clears once it makes progress.
func TestReconcileStuck(t *testing.T) {
	f := newFake()
	ep := key("endpoint", "p/c/main/rw")
	f.actual = []Actual{{Key: ep, Ready: false, State: "provisioning"}}
	desired := []api.DesiredResource{d("endpoint", "p/c/main/rw", 0)} // exists at gen 0 -> noop

	now := time.Unix(0, 0)
	e := New(f, Options{Now: func() time.Time { return now }, StuckAfter: time.Minute})

	// First sighting: not stuck yet (just entered the state).
	if res, _ := e.Reconcile(context.Background(), desired, true); len(res.Stuck) != 0 {
		t.Fatalf("tick1: stuck=%d, want 0", len(res.Stuck))
	}
	// Same non-Ready state past the deadline: stuck.
	now = now.Add(2 * time.Minute)
	res, _ := e.Reconcile(context.Background(), desired, true)
	if len(res.Stuck) != 1 || res.Stuck[ep] < time.Minute {
		t.Fatalf("tick2: stuck=%v, want the endpoint flagged", res.Stuck)
	}
	// It becomes Ready: no longer stuck.
	f.actual = []Actual{{Key: ep, Ready: true, State: "ready"}}
	now = now.Add(time.Minute)
	if res, _ := e.Reconcile(context.Background(), desired, true); len(res.Stuck) != 0 {
		t.Fatalf("tick3: stuck=%d, want 0 after becoming ready", len(res.Stuck))
	}
}
