package task

import (
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/yugabyte/amp/controller/internal/core"
)

// These cover the two things the roll gained over a plain bounded pool: it can be
// cancelled while it runs, and it consumes its work set in an order that bounds
// the blast radius.

// A roll is ONE step, and the worker only checks for a cancel between steps — so
// without the poll between children a claimed roll runs the whole fleet no matter
// what an operator asks for. The children already started finish; the rest are
// closed, since the parent is about to go terminal and nothing reclaims it.
func TestRollStopsAtTheNextChildWhenCancelled(t *testing.T) {
	ctl := newRollControl()
	f := newRollFixture(t, ctl, nil, ep1, ep2, ep3)

	// Cancel before the roll starts, so the poll fires on the first child rather
	// than racing the pool.
	if err := f.store.RequestCancel(t.Context(), f.parent.ID); err != nil {
		t.Fatalf("RequestCancel: %v", err)
	}

	res := f.execute(t, PlanEndpointUpgradesArgs{Version: "v2", MaxParallel: 1})
	if res.Err == nil {
		t.Fatal("a cancelled roll succeeded; it must fail so the task is not recorded as converged")
	}
	if !errors.Is(res.Err, ErrCancelled) {
		t.Errorf("roll error = %v, want it to wrap ErrCancelled so the worker can tell "+
			"'an operator stopped this' from 'this broke'", res.Err)
	}
	for _, id := range []uuid.UUID{ep1, ep2, ep3} {
		if n := ctl.runCount(id); n != 0 {
			t.Errorf("endpoint %s ran %d children after the cancel; the poll runs BEFORE a child starts", id, n)
		}
	}

	// Every child row is closed rather than left pending: a terminal parent is
	// never reclaimed, so a pending row under one is work nothing will ever run.
	subs, err := f.subs.ByParent(t.Context(), f.parent.ID)
	if err != nil {
		t.Fatalf("ByParent: %v", err)
	}
	if len(subs) != 3 {
		t.Fatalf("%d children recorded, want 3", len(subs))
	}
	for _, s := range subs {
		if s.Status == StatusPending || s.Status == StatusRunning {
			t.Errorf("child %s left %s under a terminal parent", s.ID, s.Status)
		}
	}
}

// A roll with no TaskReader still converges. Every other roll dep is required
// because the work cannot be done without it; this one only costs the roll its
// cancellability, which is not worth failing a fleet upgrade over.
func TestRollWithoutATaskReaderStillRuns(t *testing.T) {
	ctl := newRollControl()
	f := newRollFixture(t, ctl, nil, ep1, ep2)
	f.deps.Tasks = nil

	if err := f.store.RequestCancel(t.Context(), f.parent.ID); err != nil {
		t.Fatalf("RequestCancel: %v", err)
	}
	res := f.execute(t, PlanEndpointUpgradesArgs{Version: "v2", MaxParallel: 1})
	if res.Err != nil {
		t.Fatalf("roll failed without a task reader: %v", res.Err)
	}
	if got := decodeRoll(t, res); got.Upgraded != 2 {
		t.Errorf("upgraded = %d, want 2: the cancel is unreadable, so the roll runs", got.Upgraded)
	}
}

// A cancel the roll notices mid-flight stops new children but lets the running
// one finish — killing a child mid-`helm --wait` leaves a release the next roll
// has to clean up.
func TestRollCancelledMidFlightLetsTheRunningChildFinish(t *testing.T) {
	ctl := newRollControl()
	ctl.hold = make(chan struct{})
	f := newRollFixture(t, ctl, nil, ep1, ep2, ep3)

	done := make(chan StepResult, 1)
	go func() { done <- f.execute(t, PlanEndpointUpgradesArgs{Version: "v2", MaxParallel: 1}) }()

	// Wait for the first child to be parked inside Execute, then ask to stop.
	waitFor(t, 5*time.Second, func() bool { inFlight, _ := ctl.snapshot(); return inFlight == 1 })
	if err := f.store.RequestCancel(t.Context(), f.parent.ID); err != nil {
		t.Fatalf("RequestCancel: %v", err)
	}
	close(ctl.hold)

	res := <-done
	if !errors.Is(res.Err, ErrCancelled) {
		t.Fatalf("roll error = %v, want ErrCancelled", res.Err)
	}
	if n := ctl.runCount(ep1); n != 1 {
		t.Errorf("the in-flight child ran %d times, want 1: a cancel must not kill work already started", n)
	}
	if n := ctl.runCount(ep2) + ctl.runCount(ep3); n != 0 {
		t.Errorf("%d children started after the cancel, want 0", n)
	}
	// The error says how far it got, because a failed step records its cause and
	// not its result.
	if !strings.Contains(res.Err.Error(), "1 upgraded") {
		t.Errorf("cancel error = %q, want the counts so an operator knows what moved", res.Err)
	}
}

// orderRoll is what keeps a bounded pool from taking down one customer's whole
// cluster at once: the enumeration comes back grouped by cluster, so without it
// the first max_parallel endpoints in flight all belong to the same one.
func TestOrderRollRoundRobinsClustersAndMovesWritersLast(t *testing.T) {
	// Cluster ids are compared as strings, so these sort a, b, c.
	ca := uuid.MustParse("aaaaaaaa-0000-0000-0000-000000000000")
	cb := uuid.MustParse("bbbbbbbb-0000-0000-0000-000000000000")
	id := func(n string) uuid.UUID { return uuid.MustParse(n + "0000000-0000-0000-0000-000000000000") }

	a1, a2 := id("1"), id("2")
	b1, b2 := id("3"), id("4")
	in := []core.EndpointDeploy{
		{ID: a1, ClusterID: ca, Role: core.RoleReadWrite},
		{ID: a2, ClusterID: ca, Role: core.RoleReadOnly},
		{ID: b1, ClusterID: cb, Role: core.RoleReadWrite},
		{ID: b2, ClusterID: cb, Role: core.RoleReadOnly},
	}
	want := []uuid.UUID{a2, b2, a1, b1} // readers first, alternating clusters

	got := orderRoll(in)
	if len(got) != len(want) {
		t.Fatalf("orderRoll returned %d members, want %d", len(got), len(want))
	}
	for i, w := range want {
		if got[i].ID != w {
			t.Fatalf("order = %v, want %v (readers before writers, clusters alternating)", ids(got), want)
		}
	}

	// A reclaimed parent has to re-derive the identical sequence, or a retried roll
	// moves the fleet in a different order than the one that failed.
	shuffled := []core.EndpointDeploy{in[2], in[0], in[3], in[1]}
	for i, m := range orderRoll(shuffled) {
		if m.ID != want[i] {
			t.Fatalf("orderRoll is input-order dependent: %v vs %v", ids(orderRoll(shuffled)), want)
		}
	}
}

func ids(ds []core.EndpointDeploy) []uuid.UUID {
	out := make([]uuid.UUID, 0, len(ds))
	for _, d := range ds {
		out = append(out, d.ID)
	}
	return out
}
