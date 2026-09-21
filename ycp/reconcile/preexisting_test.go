package reconcile

import (
	"context"
	"testing"
	"time"

	"meko/ycp/internal/api"
)

// These cover Actual.Preexisting: a resource the backend did not create and
// cannot delete, which is on the remote whether or not YCP ever asked for it.

// A resource the backend did not create is never collected, however long desired
// state goes without naming it. Without this the ordinary GC rule sees
// in-actual-not-in-desired and plans a Delete the backend has to refuse, which
// latches a permanent drift row on an environment where nothing is wrong.
func TestPreexistingResourceIsNeverDeleted(t *testing.T) {
	f := newFake()
	f.actual = []Actual{
		{Key: key("controller", "controller"), Preexisting: true, Ready: true},
		{Key: key("cluster", "ordinary"), Ready: true},
	}

	res, err := New(f, Options{}).Reconcile(context.Background(), nil, true)
	if err != nil {
		t.Fatal(err)
	}
	if len(f.deletes) != 1 || f.deletes[0] != key("cluster", "ordinary") {
		t.Fatalf("deletes = %v, want only cluster/ordinary", f.deletes)
	}
	if res.Deleted != 1 {
		t.Errorf("Deleted = %d, want 1", res.Deleted)
	}
}

// Preexisting suppresses the delete and nothing else: a declared one is created
// and updated like any other resource, or the flag would quietly opt a kind out
// of being managed at all.
func TestPreexistingResourceStillConverges(t *testing.T) {
	f := newFake()
	f.actual = []Actual{{Key: key("controller", "controller"), Preexisting: true, ObservedGen: 1, Ready: true}}
	desired := []api.DesiredResource{d("controller", "controller", 2)}

	res, err := New(f, Options{}).Reconcile(context.Background(), desired, true)
	if err != nil {
		t.Fatal(err)
	}
	if len(f.updates) != 1 || f.updates[0] != key("controller", "controller") {
		t.Fatalf("updates = %v, want the controller updated", f.updates)
	}
	if res.Updated != 1 {
		t.Errorf("Updated = %d, want 1", res.Updated)
	}
}

// An undeclared pre-existing resource sits in one state forever by design, so
// timing it manufactures a "no progress" alarm for something nobody asked about.
// A DECLARED one stays timed — that one is converging, or failing to.
func TestUndeclaredPreexistingResourceIsNotReportedStuck(t *testing.T) {
	f := newFake()
	f.actual = []Actual{
		{Key: key("controller", "controller"), Preexisting: true, State: "unmanaged"},
		{Key: key("cluster", "declared"), State: "creating"},
	}
	desired := []api.DesiredResource{d("cluster", "declared", 1)}

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
	if _, ok := res.Stuck[key("controller", "controller")]; ok {
		t.Error("an undeclared pre-existing resource was reported stuck; nobody is waiting on it")
	}
	if _, ok := res.Stuck[key("cluster", "declared")]; !ok {
		t.Error("a declared resource making no progress was NOT reported stuck")
	}
}
