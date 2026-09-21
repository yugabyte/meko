package reconcile

import (
	"context"
	"errors"
	"testing"
	"time"

	"meko/ycp/internal/api"
)

// These cover reconciling an environment the backend could only discover in
// part: what an Actual carrying a DiscoveryError suppresses, and what it deliberately
// does not.

var errUnread = errors.New("the controller is offline")

// The case the field exists for. A backend that cannot reach its remote reports
// the declared resources as unread rather than returning a short list, and
// nothing is created for them — a create here would duplicate whatever the
// unreachable remote is still holding.
func TestUnreadableResourceIsNotCreated(t *testing.T) {
	f := newFake()
	f.actual = []Actual{
		{Key: key("cluster", "c"), DiscoveryError: errUnread},
		{Key: key("pool", "p"), Ready: true},
	}
	desired := []api.DesiredResource{d("pool", "p", 1), d("cluster", "c", 1)}

	res, err := New(f, Options{}).Reconcile(context.Background(), desired, true)
	if err != nil {
		t.Fatalf("a partial reading must not abort the tick: %v", err)
	}
	if len(f.creates) != 0 {
		t.Errorf("created %v; an unread resource may already exist", f.creates)
	}
	if got := res.Unreadable[key("cluster", "c")]; !errors.Is(got, errUnread) {
		t.Errorf("Unreadable[cluster/c] = %v, want %v", got, errUnread)
	}
	if len(res.Failed) != 0 {
		t.Errorf("Failed = %v; nothing was attempted, so nothing failed", res.Failed)
	}
}

// An unread row carries no state to diff against, so an update is written blind
// — it could re-apply a spec the resource already has, and the engine would
// record the generation as applied on the strength of a call it could not check.
func TestUnreadableResourceIsNotUpdated(t *testing.T) {
	f := newFake()
	f.actual = []Actual{{Key: key("cluster", "c"), ObservedGen: 1, DiscoveryError: errUnread}}
	desired := []api.DesiredResource{d("cluster", "c", 2)}

	res, err := New(f, Options{}).Reconcile(context.Background(), desired, true)
	if err != nil {
		t.Fatal(err)
	}
	if len(f.updates) != 0 {
		t.Errorf("updated %v; the row reports no state to update from", f.updates)
	}
	if _, ok := res.Unreadable[key("cluster", "c")]; !ok {
		t.Errorf("Unreadable = %v, want cluster/c", res.Unreadable)
	}
}

// The destructive direction, and the one mistake that cannot be undone. A row
// the backend could not read is not evidence the resource exists, so it is
// never GC'd for being undeclared.
func TestUnreadableResourceIsNeverDeleted(t *testing.T) {
	f := newFake()
	f.actual = []Actual{
		{Key: key("cluster", "gone"), DiscoveryError: errUnread},
		{Key: key("cluster", "really-gone"), Ready: true},
	}

	// Neither is desired. The one that was READ is collectable; the unread one
	// is not.
	res, err := New(f, Options{}).Reconcile(context.Background(), nil, true)
	if err != nil {
		t.Fatal(err)
	}
	if len(f.deletes) != 1 || f.deletes[0] != key("cluster", "really-gone") {
		t.Fatalf("deletes = %v, want only cluster/really-gone", f.deletes)
	}
	if res.Deleted != 1 {
		t.Errorf("Deleted = %d, want 1", res.Deleted)
	}
	// And it is REPORTED, not silently dropped. A withdrawn resource whose
	// collection nobody could confirm leaves the tick unconverged; without this
	// the environment reads READY and posts the revision as applied while the
	// resource is still on the remote.
	if got := res.Unreadable[key("cluster", "gone")]; !errors.Is(got, errUnread) {
		t.Errorf("Unreadable[cluster/gone] = %v, want %v", got, errUnread)
	}
}

// The three prunes below key off "nothing planned this tick", and an unreadable
// resource has nothing planned — but it has not converged either. Left
// unguarded, an unread tick wipes the backoff ladder of the very resource whose
// verbs the same outage just failed, restarts the wedged-task clock, and retires
// a permanent verdict without its spec changing.
func TestUnreadableTickDoesNotResetPerResourceState(t *testing.T) {
	f := newFake()
	k := key("cluster", "c")
	desired := []api.DesiredResource{d("cluster", "c", 1)}

	// Tick 1: readable and the create fails, which lays down a backoff ladder.
	f.failCall[k] = 1
	e := New(f, Options{})
	if _, err := e.Reconcile(context.Background(), desired, true); err != nil {
		t.Fatal(err)
	}
	e.mu.Lock()
	_, hadLadder := e.failures[k]
	e.mu.Unlock()
	if !hadLadder {
		t.Fatal("no backoff ladder after a failed create; the fixture is not exercising the prune")
	}

	// Tick 2: the remote is unreachable — the same outage that failed the verb.
	f.actual = []Actual{{Key: k, DiscoveryError: errUnread}}
	res, err := e.Reconcile(context.Background(), desired, true)
	if err != nil {
		t.Fatal(err)
	}
	if len(res.Unreadable) != 1 {
		t.Fatalf("Unreadable = %v, want cluster/c", res.Unreadable)
	}
	e.mu.Lock()
	_, keptLadder := e.failures[k]
	e.mu.Unlock()
	if !keptLadder {
		t.Error("the unread tick wiped the backoff ladder; a repeatedly-restarting remote " +
			"would be pinned at rung 1 and its work re-issued at full rate")
	}
}

// A resource the backend never mentions is absent, not unread — the short-list
// reading has to stay available, or a backend could not report a genuine
// deletion. Only an explicit Error row suppresses the create.
func TestOmittedResourceIsStillCreated(t *testing.T) {
	f := newFake()
	f.actual = []Actual{{Key: key("pool", "p"), Ready: true}}
	desired := []api.DesiredResource{d("pool", "p", 1), d("cluster", "c", 1)}

	if _, err := New(f, Options{}).Reconcile(context.Background(), desired, true); err != nil {
		t.Fatal(err)
	}
	if len(f.creates) != 1 || f.creates[0] != key("cluster", "c") {
		t.Fatalf("creates = %v, want cluster/c: absence still means create", f.creates)
	}
}

// Ready on a row nobody could read says nothing, so a child is gated on it
// anyway. Without this a backend that left Ready set on an error row would have
// children created against a parent whose state is unknown.
func TestUnreadableParentGatesItsChildren(t *testing.T) {
	f := newFake()
	f.parents[key("branch", "b")] = key("cluster", "c")
	f.actual = []Actual{{Key: key("cluster", "c"), Ready: true, DiscoveryError: errUnread}}
	desired := []api.DesiredResource{d("cluster", "c", 1), d("branch", "b", 1)}

	res, err := New(f, Options{}).Reconcile(context.Background(), desired, true)
	if err != nil {
		t.Fatal(err)
	}
	if len(f.creates) != 0 {
		t.Errorf("created %v; the parent's readiness is not knowable", f.creates)
	}
	if res.Deferred != 1 {
		t.Errorf("Deferred = %d, want 1 (the branch)", res.Deferred)
	}
}

// Everything the backend COULD read still converges. This is the whole point of
// reporting per resource rather than failing the tick: the half of the
// environment that is reachable is reconciled while the other half is dark.
func TestReadableResourcesConvergeAlongsideUnreadableOnes(t *testing.T) {
	f := newFake()
	f.actual = []Actual{
		{Key: key("pool", "p"), ObservedGen: 1, Ready: true},
		{Key: key("cluster", "c"), DiscoveryError: errUnread},
	}
	desired := []api.DesiredResource{d("pool", "p", 2), d("cluster", "c", 1)}

	res, err := New(f, Options{}).Reconcile(context.Background(), desired, true)
	if err != nil {
		t.Fatal(err)
	}
	if len(f.updates) != 1 || f.updates[0] != key("pool", "p") {
		t.Fatalf("updates = %v, want pool/p: a readable resource still converges", f.updates)
	}
	if res.Updated != 1 {
		t.Errorf("Updated = %d, want 1", res.Updated)
	}
	if len(res.Unreadable) != 1 {
		t.Errorf("Unreadable = %v, want just cluster/c", res.Unreadable)
	}
}

// An unread resource is reported through Unreadable, not timed as wedged: its
// State is not something the backend observed, so ageing it would name the
// wrong cause.
func TestUnreadableResourceIsNotReportedStuck(t *testing.T) {
	f := newFake()
	f.actual = []Actual{{Key: key("cluster", "c"), DiscoveryError: errUnread}}
	desired := []api.DesiredResource{d("cluster", "c", 1)}

	now := time.Now()
	e := New(f, Options{Now: func() time.Time { return now }, StuckAfter: time.Minute})
	if _, err := e.Reconcile(context.Background(), desired, true); err != nil {
		t.Fatal(err)
	}
	now = now.Add(10 * time.Minute)
	res, err := e.Reconcile(context.Background(), desired, true)
	if err != nil {
		t.Fatal(err)
	}
	if len(res.Stuck) != 0 {
		t.Errorf("Stuck = %v; an unread resource has its own channel", res.Stuck)
	}
	if len(res.Unreadable) != 1 {
		t.Errorf("Unreadable = %v, want cluster/c", res.Unreadable)
	}
}
