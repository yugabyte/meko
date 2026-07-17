package sched_test

import (
	"context"
	"runtime"
	"testing"
	"time"

	"github.com/yugabyte/yb-amp/test/functional/framework/sched"
)

func TestLeaseAcquireBlocksAndReleaseUnblocks(t *testing.T) {
	m := sched.NewManager(2)
	l1, err := m.Acquire(context.Background(), 1)
	if err != nil {
		t.Fatalf("acquire 1: %v", err)
	}
	l2, err := m.Acquire(context.Background(), 1)
	if err != nil {
		t.Fatalf("acquire 2: %v", err)
	}
	if m.Used() != 2 {
		t.Fatalf("used = %d, want 2", m.Used())
	}

	granted := make(chan struct{})
	go func() {
		l3, _ := m.Acquire(context.Background(), 1)
		_ = l3
		close(granted)
	}()

	// The third acquire must block until capacity frees.
	waitFor(t, func() bool { return m.Waiting() == 1 })
	select {
	case <-granted:
		t.Fatalf("third acquire granted while at capacity")
	default:
	}

	l1.Release()
	select {
	case <-granted:
	case <-time.After(2 * time.Second):
		t.Fatalf("third acquire did not unblock after release")
	}
	l2.Release()
}

func TestLeaseCtxCancelWhileBlocked(t *testing.T) {
	m := sched.NewManager(1)
	l1, err := m.Acquire(context.Background(), 1)
	if err != nil {
		t.Fatalf("acquire: %v", err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	errc := make(chan error, 1)
	go func() {
		_, err := m.Acquire(ctx, 1)
		errc <- err
	}()
	waitFor(t, func() bool { return m.Waiting() == 1 })
	cancel()
	select {
	case err := <-errc:
		if err == nil {
			t.Fatalf("want ctx error, got nil")
		}
	case <-time.After(2 * time.Second):
		t.Fatalf("cancelled acquire did not return")
	}
	// The cancelled waiter must not have consumed capacity.
	waitFor(t, func() bool { return m.Waiting() == 0 })
	if m.Used() != 1 {
		t.Fatalf("used = %d, want 1 (cancel must not leak capacity)", m.Used())
	}
	l1.Release()
	if m.Used() != 0 {
		t.Fatalf("used = %d, want 0 after release", m.Used())
	}
}

func TestLeasePinHoldsCapacity(t *testing.T) {
	m := sched.NewManager(1)
	l, err := m.Acquire(context.Background(), 1)
	if err != nil {
		t.Fatalf("acquire: %v", err)
	}
	l.Pin()
	if !l.Pinned() {
		t.Fatalf("lease not pinned")
	}

	ctx, cancel := context.WithTimeout(context.Background(), 100*time.Millisecond)
	defer cancel()
	if _, err := m.Acquire(ctx, 1); err == nil {
		t.Fatalf("acquire succeeded while a pinned lease held all capacity")
	}
	// A pinned lease is NOT freed by ordinary Release — a PERSISTENT env holds
	// its slot until it is explicitly reaped.
	l.Release()
	if m.Used() != 1 {
		t.Fatalf("used = %d, want 1 (pinned lease survives Release)", m.Used())
	}
	ctx2, cancel2 := context.WithTimeout(context.Background(), 100*time.Millisecond)
	defer cancel2()
	if _, err := m.Acquire(ctx2, 1); err == nil {
		t.Fatalf("acquire succeeded after Release of a pinned lease (should still be held)")
	}
	// Unpin is the explicit teardown path that finally returns the capacity.
	l.Unpin()
	if m.Used() != 0 {
		t.Fatalf("used = %d, want 0 after Unpin", m.Used())
	}
	l2, err := m.Acquire(context.Background(), 1)
	if err != nil {
		t.Fatalf("acquire after unpin: %v", err)
	}
	l2.Release()
}

func TestLeaseReleaseIdempotent(t *testing.T) {
	m := sched.NewManager(2)
	l, _ := m.Acquire(context.Background(), 2)
	l.Release()
	l.Release() // must not double-free
	if m.Used() != 0 {
		t.Fatalf("used = %d, want 0", m.Used())
	}
}

func TestLeaseWeightAboveCapacityErrors(t *testing.T) {
	m := sched.NewManager(2)
	if _, err := m.Acquire(context.Background(), 3); err == nil {
		t.Fatalf("want error for weight > capacity")
	}
}

// TestLeaseFIFOFairness proves a large request at the head of the queue is not
// starved by a smaller one behind it: even when the small request would fit, it
// waits until the large one is served first.
func TestLeaseFIFOFairness(t *testing.T) {
	m := sched.NewManager(2)
	a, _ := m.Acquire(context.Background(), 1)
	b, _ := m.Acquire(context.Background(), 1) // used = 2, full

	order := make(chan string, 2)
	// big (weight 2) enqueues first...
	bigDone := make(chan *sched.Lease, 1)
	go func() {
		l, _ := m.Acquire(context.Background(), 2)
		order <- "big"
		bigDone <- l
	}()
	waitFor(t, func() bool { return m.Waiting() == 1 })
	// ...small (weight 1) enqueues behind it.
	smallDone := make(chan *sched.Lease, 1)
	go func() {
		l, _ := m.Acquire(context.Background(), 1)
		order <- "small"
		smallDone <- l
	}()
	waitFor(t, func() bool { return m.Waiting() == 2 })

	// Free one unit (used 2→1). small would fit (1+1<=2) but big is ahead and
	// needs 2 — strict FIFO must serve neither yet.
	a.Release()
	waitFor(t, func() bool { return m.Waiting() == 2 })

	// Free the other unit (used→0): big fits and is served first.
	b.Release()
	if got := <-order; got != "big" {
		t.Fatalf("first served = %q, want big (FIFO fairness)", got)
	}
	big := <-bigDone
	big.Release()
	if got := <-order; got != "small" {
		t.Fatalf("second served = %q, want small", got)
	}
	(<-smallDone).Release()
}

// waitFor spins on cond using Gosched (a synchronization point on manager state,
// not a timed sleep) with a generous failsafe deadline.
func waitFor(t *testing.T, cond func() bool) {
	t.Helper()
	deadline := time.Now().Add(2 * time.Second)
	for !cond() {
		if time.Now().After(deadline) {
			t.Fatalf("condition not met within deadline")
		}
		runtime.Gosched()
	}
}
