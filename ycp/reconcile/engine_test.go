package reconcile

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
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

// A backend that cannot echo an observed generation (amp's pools and clusters
// report 0) must still reach a no-op once the engine's own cache records the
// applied generation. The cache is only a floor for work that COMPLETED — a
// backend planning async work reports Pending, which records nothing — so it can
// never mask a stale resource.
func TestDiffFallsBackToCache(t *testing.T) {
	desired := []api.DesiredResource{d("cluster", "c", 7)}
	actual := []Actual{{Key: key("cluster", "c")}} // no echo

	// No cache yet -> update.
	if got := Diff(desired, actual, map[ResourceKey]int64{}); got[0].Type != ActionUpdate {
		t.Fatalf("without cache: got %s, want update", got[0].Type)
	}
	// Cache records applied 7 -> no-op.
	cache := map[ResourceKey]int64{key("cluster", "c"): 7}
	if got := Diff(desired, actual, cache); got[0].Type != ActionNoop {
		t.Fatalf("with cache: got %s, want noop", got[0].Type)
	}
}

// The echo is what makes an update self-heal across a restart: with the cache
// cold, a resource the backend reports as up to date is a no-op, and one it
// reports behind is re-issued. Before the echo existed the first case cost a
// redundant pass on every restart and the second never happened at all.
func TestDiffUsesTheEchoWithAColdCache(t *testing.T) {
	desired := []api.DesiredResource{d("endpoint", "e", 4)}
	cold := map[ResourceKey]int64{}

	behind := []Actual{{Key: key("endpoint", "e"), ObservedGen: 1}}
	if got := Diff(desired, behind, cold); got[0].Type != ActionUpdate {
		t.Fatalf("echo behind desired: got %s, want update", got[0].Type)
	}

	current := []Actual{{Key: key("endpoint", "e"), ObservedGen: 4}}
	if got := Diff(desired, current, cold); got[0].Type != ActionNoop {
		t.Fatalf("echo at desired: got %s, want noop", got[0].Type)
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
	// permReason overrides the reason perm returns; zero is ReasonUnsupported.
	permReason map[ResourceKey]PermanentReason
	pending    map[ResourceKey]bool // keys whose verb returns a PendingError

	attempts map[ResourceKey]int // every verb call, success or not
	creates  []ResourceKey
	updates  []ResourceKey
	deletes  []ResourceKey
}

func newFake() *fakeBackend {
	return &fakeBackend{
		parents:    map[ResourceKey]ResourceKey{},
		failCall:   map[ResourceKey]int{},
		perm:       map[ResourceKey]bool{},
		permReason: map[ResourceKey]PermanentReason{},
		pending:    map[ResourceKey]bool{},
		attempts:   map[ResourceKey]int{},
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
		return PermanentAs(f.permReason[k], errors.New("refused"))
	}
	if f.pending[k] {
		return Pending(errors.New("task still running"))
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

// reconsideringFake is a backend that can withdraw its own permanent verdicts.
type reconsideringFake struct {
	*fakeBackend
	reconsider []ResourceKey
}

func (r *reconsideringFake) Reconsider() []ResourceKey {
	out := r.reconsider
	r.reconsider = nil
	return out
}

// A rejection whose cause was a sibling — a branch's read_write slot already
// taken — must be asked again once the backend sees the sibling go, though the
// refused resource's own generation never moves.
func TestReconcilePermanentRetiredByReconsider(t *testing.T) {
	f := &reconsideringFake{fakeBackend: newFake()}
	k := key("endpoint", "p/c/b/rw2")
	f.perm[k] = true
	f.permReason[k] = ReasonRejected
	e := New(f, Options{})
	desired := []api.DesiredResource{d("endpoint", "p/c/b/rw2", 0)}

	e.Reconcile(context.Background(), desired, true)
	if res, _ := e.Reconcile(context.Background(), desired, true); len(res.Permanent) != 1 || f.attempts[k] != 1 {
		t.Fatalf("tick2: permanent=%d attempts=%d, want 1/1", len(res.Permanent), f.attempts[k])
	}

	f.perm[k] = false
	f.reconsider = []ResourceKey{k}
	res, _ := e.Reconcile(context.Background(), desired, true)
	if res.Created != 1 || len(res.Permanent) != 0 {
		t.Fatalf("tick3: created=%d permanent=%d, want 1/0 — a reconsidered key must be asked again",
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

// A backend reporting Pending means the work it was asked for is already in
// flight. That is neither applied nor failed, so the tick must record it as
// updating and touch nothing else: no cache write (the update has not landed, so
// a later tick still has to check), and no backoff (nothing failed, so the
// environment must not go DEGRADED for a task that is converging fine).
func TestReconcilePendingIsNeitherAppliedNorFailed(t *testing.T) {
	f := newFake()
	ep := key("endpoint", "p/c/main/rw")
	f.actual = []Actual{{Key: ep, ObservedGen: 1, Ready: true, State: "ready"}}
	f.pending[ep] = true
	desired := []api.DesiredResource{d("endpoint", "p/c/main/rw", 2)}

	now := time.Unix(0, 0)
	e := New(f, Options{Now: func() time.Time { return now }, StuckAfter: time.Minute})

	res, err := e.Reconcile(context.Background(), desired, true)
	if err != nil {
		t.Fatal(err)
	}
	if len(res.Updating) != 1 || res.Updating[ep] == nil {
		t.Fatalf("updating = %v, want the endpoint reported", res.Updating)
	}
	if len(res.Failed) != 0 || len(res.Permanent) != 0 || res.Updated != 0 {
		t.Fatalf("failed=%v permanent=%v updated=%d, want none of those", res.Failed, res.Permanent, res.Updated)
	}
	// No backoff was recorded, so the next tick re-checks immediately rather than
	// waiting out a penalty for something that did not fail.
	if _, backing := e.failures[ep]; backing {
		t.Error("a pending update recorded a backoff")
	}
	// And nothing was cached as applied: the echo is still behind, so the tick
	// after this one must ask again.
	if gen, cached := e.cache[ep]; cached {
		t.Errorf("a pending update cached generation %d as applied", gen)
	}

	res, _ = e.Reconcile(context.Background(), desired, true)
	if len(res.Updating) != 1 {
		t.Fatalf("tick2 updating = %v, want the endpoint still reported", res.Updating)
	}
	if f.attempts[ep] != 2 {
		t.Errorf("attempts = %d, want 2 — a pending resource is re-checked every tick", f.attempts[ep])
	}
}

// A task that never terminates is the one no-progress case reading the task's
// status cannot detect: the controller keeps answering "running". Past StuckAfter
// the engine reports it through Stuck, so the environment degrades with a reason
// instead of sitting "updating" forever.
func TestReconcilePendingTooLongIsStuck(t *testing.T) {
	f := newFake()
	ep := key("endpoint", "p/c/main/rw")
	// Ready and in a steady state, so the non-Ready stuck rule cannot fire — this
	// has to be caught by how long the update has been pending.
	f.actual = []Actual{{Key: ep, ObservedGen: 1, Ready: true, State: "ready"}}
	f.pending[ep] = true
	desired := []api.DesiredResource{d("endpoint", "p/c/main/rw", 2)}

	now := time.Unix(0, 0)
	e := New(f, Options{Now: func() time.Time { return now }, StuckAfter: time.Minute})

	if res, _ := e.Reconcile(context.Background(), desired, true); len(res.Stuck) != 0 {
		t.Fatalf("tick1: stuck=%v, want none — the update has only just been issued", res.Stuck)
	}
	now = now.Add(2 * time.Minute)
	res, _ := e.Reconcile(context.Background(), desired, true)
	if len(res.Stuck) != 1 || res.Stuck[ep] < time.Minute {
		t.Fatalf("tick2: stuck=%v, want the endpoint flagged for a wedged update", res.Stuck)
	}

	// The task finally lands: the mark clears, so the resource stops being stuck
	// rather than staying flagged on a stale timestamp.
	f.pending[ep] = false
	f.actual = []Actual{{Key: ep, ObservedGen: 2, Ready: true, State: "ready"}}
	now = now.Add(time.Minute)
	res, _ = e.Reconcile(context.Background(), desired, true)
	if len(res.Stuck) != 0 || len(res.Updating) != 0 {
		t.Fatalf("tick3: stuck=%v updating=%v, want both empty once the echo catches up", res.Stuck, res.Updating)
	}
	if res.NoOp != 1 {
		t.Errorf("tick3: noop=%d, want 1 — the echoed generation now matches desired", res.NoOp)
	}
}

// A long update that is still moving — a fleet roll's done/total advancing — is not
// stuck, on either timer: Progress restarts both. One that stops moving is flagged
// again a StuckAfter later.
func TestReconcileProgressRestartsTheStuckTimers(t *testing.T) {
	f := newFake()
	fl := key("fleet-version", "fleet")
	f.pending[fl] = true
	desired := []api.DesiredResource{d("fleet-version", "fleet", 2)}

	now := time.Unix(0, 0)
	e := New(f, Options{Now: func() time.Time { return now }, StuckAfter: time.Minute})
	tick := func(progress string) Result {
		t.Helper()
		f.actual = []Actual{{Key: fl, ObservedGen: 1, State: "rolling", Progress: progress}}
		res, err := e.Reconcile(context.Background(), desired, true)
		if err != nil {
			t.Fatal(err)
		}
		return res
	}

	tick("0/4")
	for _, p := range []string{"1/4", "2/4", "3/4"} {
		now = now.Add(2 * time.Minute)
		if res := tick(p); len(res.Stuck) != 0 {
			t.Fatalf("progress %s: stuck=%v, want none while the roll moves", p, res.Stuck)
		}
	}
	now = now.Add(2 * time.Minute)
	if res := tick("3/4"); len(res.Stuck) != 1 {
		t.Fatalf("no progress: stuck=%v, want the roll flagged", res.Stuck)
	}
}

// The whole point of the echo, end to end through the engine, against a backend
// that behaves like a real async one: it reports Pending when it issues, then the
// task's failure, and the echo only advances once work actually lands.
//
// The failure is what used to be invisible. The 202 was recorded as applied, so no
// later tick revisited the resource and nothing degraded the environment.
func TestReconcileAsyncUpdateFailureSelfHeals(t *testing.T) {
	f := newFake()
	ep := key("endpoint", "p/c/main/rw")
	f.actual = []Actual{{Key: ep, ObservedGen: 1, Ready: true, State: "ready"}}
	desired := []api.DesiredResource{d("endpoint", "p/c/main/rw", 2)}

	now := time.Unix(0, 0)
	e := New(f, Options{Now: func() time.Time { return now }, BaseBackoff: time.Second})

	// Tick 1: the update is issued. Pending, because a 202 is not evidence.
	f.pending[ep] = true
	res, _ := e.Reconcile(context.Background(), desired, true)
	if len(res.Updating) != 1 || res.Updated != 0 {
		t.Fatalf("tick1: updating=%v updated=%d, want the update reported in flight", res.Updating, res.Updated)
	}
	if _, cached := e.cache[ep]; cached {
		t.Fatal("tick1 cached a generation for work that has not landed")
	}

	// Tick 2: the backend reads the task and finds it failed. Now it is a real
	// failure — reported, and backed off.
	now = now.Add(time.Second)
	f.pending[ep] = false
	f.failCall[ep] = 1
	res, _ = e.Reconcile(context.Background(), desired, true)
	if len(res.Failed) != 1 {
		t.Fatalf("tick2: failed=%v, want the task's failure surfaced", res.Failed)
	}
	if len(res.Updating) != 0 {
		t.Errorf("tick2: updating=%v, want empty once the task is terminal", res.Updating)
	}

	// Tick 3, inside the backoff: nothing re-issued, so a permanently-failing task
	// cannot become a task per tick.
	res, _ = e.Reconcile(context.Background(), desired, true)
	if f.attempts[ep] != 2 {
		t.Errorf("attempts inside the backoff = %d, want 2", f.attempts[ep])
	}

	// Tick 4, past the backoff: re-issued, because the echo is still behind.
	now = now.Add(2 * time.Second)
	f.pending[ep] = true
	res, _ = e.Reconcile(context.Background(), desired, true)
	if len(res.Updating) != 1 {
		t.Fatalf("tick4: updating=%v, want the update re-issued", res.Updating)
	}
	if f.attempts[ep] != 3 {
		t.Errorf("attempts = %d, want 3", f.attempts[ep])
	}

	// The task lands: the echo advances and the engine stops.
	now = now.Add(time.Second)
	f.pending[ep] = false
	f.actual = []Actual{{Key: ep, ObservedGen: 2, Ready: true, State: "ready"}}
	res, _ = e.Reconcile(context.Background(), desired, true)
	if res.NoOp != 1 || res.Updated != 0 || len(res.Updating) != 0 {
		t.Fatalf("final tick: noop=%d updated=%d updating=%v, want a clean no-op",
			res.NoOp, res.Updated, res.Updating)
	}
}

// An update that keeps failing must back off exponentially. On an async path every
// failure is preceded by a Pending — the issuing tick reports pending, a later one
// reads the task's failure — so a ladder that resets on the pending would sit on its
// first rung forever and plan a task every BaseBackoff, each add_branch_extensions
// one redeploying every endpoint on its branch.
func TestReconcileAsyncFailureBackoffClimbs(t *testing.T) {
	f := newFake()
	ep := key("endpoint", "p/c/main/rw")
	f.actual = []Actual{{Key: ep, ObservedGen: 1, Ready: true, State: "ready"}}
	desired := []api.DesiredResource{d("endpoint", "p/c/main/rw", 2)}

	now := time.Unix(0, 0)
	base := time.Second
	e := New(f, Options{Now: func() time.Time { return now }, BaseBackoff: base, MaxBackoff: time.Hour})

	// Three issue -> fail cycles, each waiting out the backoff the last one set.
	var waits []time.Duration
	for attempt := 0; attempt < 3; attempt++ {
		f.pending[ep] = true
		if res, _ := e.Reconcile(context.Background(), desired, true); len(res.Updating) != 1 {
			t.Fatalf("attempt %d: updating=%v, want the update issued", attempt, res.Updating)
		}
		now = now.Add(time.Millisecond)
		f.pending[ep] = false
		f.failCall[ep] = 1
		if res, _ := e.Reconcile(context.Background(), desired, true); len(res.Failed) != 1 {
			t.Fatalf("attempt %d: failed=%v, want the task's failure", attempt, res.Failed)
		}

		e.mu.Lock()
		wait := e.failures[ep].nextAttempt.Sub(now)
		e.mu.Unlock()
		waits = append(waits, wait)
		now = now.Add(wait)
	}

	for i := 1; i < len(waits); i++ {
		if waits[i] <= waits[i-1] {
			t.Fatalf("backoff waits = %v, want each longer than the last", waits)
		}
	}
	if waits[0] != base {
		t.Errorf("first wait = %v, want BaseBackoff %v", waits[0], base)
	}
}

// The ladder must not outlive the failure. Once the resource converges nothing is
// dispatched for it — the echo advancing makes it a no-op — so without pruning a
// spent ladder would delay its next update by up to MaxBackoff.
func TestReconcileClearsBackoffOnceConverged(t *testing.T) {
	f := newFake()
	ep := key("endpoint", "p/c/main/rw")
	f.actual = []Actual{{Key: ep, ObservedGen: 1, Ready: true, State: "ready"}}
	desired := []api.DesiredResource{d("endpoint", "p/c/main/rw", 2)}

	now := time.Unix(0, 0)
	e := New(f, Options{Now: func() time.Time { return now }, BaseBackoff: time.Hour})

	f.failCall[ep] = 1
	if res, _ := e.Reconcile(context.Background(), desired, true); len(res.Failed) != 1 {
		t.Fatalf("failed=%v, want the update to fail", res.Failed)
	}
	e.mu.Lock()
	_, backing := e.failures[ep]
	e.mu.Unlock()
	if !backing {
		t.Fatal("no backoff recorded after a failure")
	}

	// The echo catches up: the resource is a no-op and never reaches dispatch.
	f.actual = []Actual{{Key: ep, ObservedGen: 2, Ready: true, State: "ready"}}
	now = now.Add(time.Second)
	if res, _ := e.Reconcile(context.Background(), desired, true); res.NoOp != 1 {
		t.Fatalf("noop=%d, want 1", res.NoOp)
	}
	e.mu.Lock()
	_, stillBacking := e.failures[ep]
	e.mu.Unlock()
	if stillBacking {
		t.Error("the spent backoff ladder outlived the failure it came from")
	}
}

// A rejection and a block are permanent on exactly the same terms as an
// unsupported verb — issued once, never re-issued, and not enough to degrade the
// environment — and each keeps its own reason all the way to the Result, because
// that is what the row's state is read from.
func TestPermanentReasonSurvivesToTheResult(t *testing.T) {
	for _, tc := range []struct {
		name   string
		reason PermanentReason
		state  string
	}{
		{"unsupported", ReasonUnsupported, "drift"},
		{"rejected", ReasonRejected, "rejected"},
		{"blocked", ReasonBlocked, "blocked"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			f := newFake()
			k := key("endpoint", "p/c/b/rw")
			f.actual = []Actual{{Key: k, Ready: true}}
			f.perm[k] = true
			f.permReason[k] = tc.reason

			e := New(f, Options{})
			desired := []api.DesiredResource{d("endpoint", "p/c/b/rw", 1)}

			res, _ := e.Reconcile(context.Background(), desired, true)
			err, ok := res.Permanent[k]
			if !ok || len(res.Failed) != 0 {
				t.Fatalf("tick1: permanent=%v failed=%d, want the update reported permanent", res.Permanent, len(res.Failed))
			}
			if got := PermanentReasonOf(err).State(); got != tc.state {
				t.Errorf("state = %q, want %q", got, tc.state)
			}

			// Not re-issued while the spec stands, whatever the reason.
			res, _ = e.Reconcile(context.Background(), desired, true)
			if _, ok := res.Permanent[k]; !ok {
				t.Errorf("tick2: the verdict was dropped instead of being held")
			}
			if f.attempts[k] != 1 {
				t.Errorf("tick2: attempts=%d, want still 1", f.attempts[k])
			}
		})
	}
}

type oneShotSource struct{ ds api.DesiredState }

func (s oneShotSource) Desired(context.Context) (api.DesiredState, bool, error) {
	return s.ds, true, nil
}

// OnTick sees every tick Run drives, with the result the reporter gets.
func TestRunCallsOnTickWithEachResult(t *testing.T) {
	f := newFake()
	ctx, cancel := context.WithCancel(context.Background())
	var got []Result
	e := New(f, Options{OnTick: func(res Result, err error, took time.Duration) {
		if err != nil {
			t.Errorf("tick error: %v", err)
		}
		if took < 0 {
			t.Errorf("negative tick duration %s", took)
		}
		got = append(got, res)
		cancel()
	}})
	src := oneShotSource{api.DesiredState{Resources: []api.DesiredResource{{Kind: "pool", Name: "p", Generation: 1}}}}
	e.Run(ctx, src, nil, time.Hour, nil)
	if len(got) != 1 || got[0].Created != 1 {
		t.Fatalf("OnTick saw %+v, want one tick that created the pool", got)
	}
}

// Every condition that degrades the environment names its resource in the log: the
// agent's metric counts them but carries no key (CLOUDGA-37501).
func TestRunLogsTheResourcesThatDegrade(t *testing.T) {
	f := newFake()
	pool := key("pool", "p")
	ep := key("endpoint", "p/c/main/rw")
	f.failCall[pool] = 1
	f.actual = []Actual{{Key: ep, Ready: false, State: "provisioning"}}

	now := time.Unix(0, 0)
	ctx, cancel := context.WithCancel(context.Background())
	ticks := 0
	e := New(f, Options{
		Now:        func() time.Time { now = now.Add(2 * time.Minute); return now },
		StuckAfter: time.Minute,
		OnTick: func(Result, error, time.Duration) {
			if ticks++; ticks == 2 {
				cancel()
			}
		},
	})
	src := oneShotSource{api.DesiredState{Resources: []api.DesiredResource{
		{Kind: "pool", Name: "p", Generation: 1}, d("endpoint", "p/c/main/rw", 0),
	}}}
	var logs []string
	e.Run(ctx, src, nil, time.Millisecond, func(format string, args ...any) {
		logs = append(logs, fmt.Sprintf(format, args...))
	})

	all := strings.Join(logs, "\n")
	for _, want := range []string{pool.String() + " failed:", ep.String() + " is stuck:"} {
		if !strings.Contains(all, want) {
			t.Errorf("log lacks %q:\n%s", want, all)
		}
	}
}

// Failing keeps a resource that is waiting out its backoff, which Failed drops on
// every tick its retry does not land on (CLOUDGA-37501), and lets it go on success.
func TestReconcileFailingSpansBackoff(t *testing.T) {
	f := newFake()
	pool := key("pool", "p")
	f.failCall[pool] = 1
	desired := []api.DesiredResource{{Kind: "pool", Name: "p", Generation: 1}}

	now := time.Unix(0, 0)
	e := New(f, Options{Now: func() time.Time { return now }, BaseBackoff: time.Minute, MaxBackoff: time.Minute})

	res, _ := e.Reconcile(context.Background(), desired, true)
	if _, ok := res.Failed[pool]; !ok {
		t.Fatalf("tick1: failed=%v, want the pool", res.Failed)
	}
	if _, ok := res.Failing[pool]; !ok {
		t.Fatalf("tick1: failing=%v, want the pool", res.Failing)
	}
	// Inside the backoff window: not retried, so not Failed, but still Failing.
	now = now.Add(time.Second)
	res, _ = e.Reconcile(context.Background(), desired, true)
	if len(res.Failed) != 0 {
		t.Fatalf("tick2: failed=%v, want nothing retried inside the backoff", res.Failed)
	}
	if _, ok := res.Failing[pool]; !ok {
		t.Fatalf("tick2: failing=%v, want the pool still failing", res.Failing)
	}
	// Past it: the retry succeeds and the pool is no longer failing.
	now = now.Add(2 * time.Minute)
	if res, _ = e.Reconcile(context.Background(), desired, true); len(res.Failing) != 0 {
		t.Fatalf("tick3: failing=%v, want none after the retry succeeded", res.Failing)
	}
}
