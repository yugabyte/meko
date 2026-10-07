package foundation

import (
	"context"
	"errors"
	"testing"
	"time"
)

// replaced scripts a Foundation that is deleted and re-applied: the same name,
// namespace and generation, a different uid.
func replaced(t *testing.T) *Recorder {
	t.Helper()
	return &Recorder{Responses: []Response{
		{Match: "get foundations", Result: Result{Stdout: crObject(t, 1, nil)}, Once: true},
		{Match: "get foundations", Result: Result{Stdout: crObject(t, 1, func(m map[string]any) {
			meta(m)["uid"] = "uid-2"
		})}},
	}}
}

// A deleted-and-re-applied Foundation is a NEW object, and the loop has to
// treat it as one.
//
// Retain is the default deletion policy, so `kubectl delete foundation` is a
// supported thing to do: the object goes and the universe stays. Re-applying
// the same file then yields a fresh object at generation 1 — the same name, the
// same namespace, the same generation the FIRST object once had. Everything the
// loop carries between ticks is about the old object, and the start gate is
// `gen != lastGen`, so without the uid check the re-apply reads as already
// converged and nothing ever runs again.
func TestAReAppliedFoundationIsConvergedAgain(t *testing.T) {
	f := &fakeConverger{}
	r := testReconciler(t, replaced(t), f)

	r.tick(context.Background())
	r.settle(t)
	if applies, _ := f.count(); applies != 1 {
		t.Fatalf("the first object converged %d times, want 1", applies)
	}

	r.tick(context.Background())
	r.settle(t)
	if applies, _ := f.count(); applies != 2 {
		t.Errorf("the replacement converged %d times in total, want 2 — it came back at "+
			"generation 1, which the object before it had also reached", applies)
	}
}

// The replacement does not inherit the backoff either. A foundation whose
// converge was failing every half hour is exactly the one a user deletes and
// re-applies, and making them wait out the old object's backoff for an object
// that has never been tried is the opposite of what the re-apply asked for.
func TestAReAppliedFoundationDoesNotInheritTheBackoff(t *testing.T) {
	f := &fakeConverger{applyErr: errors.New("yba is not up")}
	r := testReconciler(t, replaced(t), f)
	r.WithIntervals(0, time.Hour, time.Hour, 0)

	r.tick(context.Background())
	r.settle(t)
	r.reap()
	if r.nextAttempt.IsZero() {
		t.Fatal("a failed converge set no backoff; this test is not testing what it says")
	}

	r.tick(context.Background())
	r.settle(t)
	if applies, _ := f.count(); applies != 2 {
		t.Errorf("the replacement converged %d times in total, want 2 — it is waiting out a "+
			"backoff earned by an object that no longer exists", applies)
	}
}

// And a converge still running for the old object is not credited to the new
// one. Its result describes a spec nothing is reading any more, so attributing
// it would mark the replacement converged without anything having converged it.
func TestTheOldObjectsConvergeIsNotCreditedToItsReplacement(t *testing.T) {
	f := &fakeConverger{block: make(chan struct{})}
	r := testReconciler(t, replaced(t), f)

	r.tick(context.Background()) // starts a converge for uid-1 and leaves it running
	old := r.active
	if old == nil {
		t.Fatal("no converge in flight")
	}

	r.tick(context.Background()) // sees uid-2 and interrupts the old converge

	// reap() collects only a converge that has already finished, so the tick
	// that discards the old result is the one AFTER it has actually stopped.
	select {
	case <-old.done:
	case <-time.After(5 * time.Second):
		t.Fatal("the superseded converge ignored its interrupt")
	}
	r.tick(context.Background()) // discards it, and starts the replacement's own

	if r.lastGen != 0 {
		t.Errorf("lastGen = %d, want 0 — the replacement's own converge is still running, "+
			"and the one that finished belonged to the object before it", r.lastGen)
	}
	if applies, _ := f.count(); applies != 2 {
		t.Errorf("Apply ran %d times, want 2 — the replacement never got its own converge", applies)
	}

	close(f.block)
	r.settle(t)
}
