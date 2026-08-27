package task

import (
	"context"
	"errors"
	"io"
	"log/slog"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"
)

// locksContract is the behaviour BOTH Locks implementations must share.
//
// It exists because MemLocks and PgLocks were otherwise exercised by disjoint
// tests, which is how the in-memory model came to permit things pglease refuses —
// a divergence no test could detect by construction. Anything asserted here is
// asserted against both (TestMemLocksContract, TestPgLocksContract).
func locksContract(t *testing.T, newLocks func(owner string) Locks) {
	t.Helper()

	t.Run("task lease admits one owner", func(t *testing.T) {
		a, b := newLocks("owner-a"), newLocks("owner-b")
		id := uuid.New()

		held, err := a.AcquireTask(t.Context(), id)
		if err != nil {
			t.Fatalf("first owner refused: %v", err)
		}
		t.Cleanup(func() { _ = held.Release(context.WithoutCancel(t.Context())) })

		if _, err := b.AcquireTask(t.Context(), id); !errors.Is(err, ErrLockHeld) {
			t.Errorf("second owner got err=%v, want ErrLockHeld", err)
		}
		// Nor may the holder take a second lease over the one row: two would each
		// believe they own it, and either one's Release would un-fence the other.
		if _, err := a.AcquireTask(t.Context(), id); !errors.Is(err, ErrLockHeld) {
			t.Errorf("holder's second acquire got err=%v, want ErrLockHeld", err)
		}
		if err := held.Release(t.Context()); err != nil {
			t.Fatalf("Release: %v", err)
		}
		taken, err := b.AcquireTask(t.Context(), id)
		if err != nil {
			t.Fatalf("after release the second owner was refused: %v", err)
		}
		t.Cleanup(func() { _ = taken.Release(context.WithoutCancel(t.Context())) })
	})

	t.Run("endpoint lease admits one task", func(t *testing.T) {
		// ONE process, two tasks: the case that breaks if the owner names the process
		// rather than the task, because pglease grants unconditionally to the holder.
		locks := newLocks("one-process")
		lock := EndpointLock(uuid.NewString())
		first, second := uuid.New(), uuid.New()

		held, err := locks.AcquireResource(t.Context(), lock, first, KindResizeEndpoint)
		if err != nil {
			t.Fatalf("first task refused: %v", err)
		}
		t.Cleanup(func() { _ = held.Release(context.WithoutCancel(t.Context())) })

		ctx, cancel := context.WithTimeout(t.Context(), 300*time.Millisecond)
		_, err = locks.AcquireResource(ctx, lock, second, KindDeleteEndpoint)
		cancel()
		if !errors.Is(err, ErrLockHeld) {
			t.Errorf("second task got err=%v, want ErrLockHeld", err)
		}

		if err := held.Release(t.Context()); err != nil {
			t.Fatalf("Release: %v", err)
		}
		taken, err := locks.AcquireResource(t.Context(), lock, second, KindDeleteEndpoint)
		if err != nil {
			t.Fatalf("after release the second task was refused: %v", err)
		}
		t.Cleanup(func() { _ = taken.Release(context.WithoutCancel(t.Context())) })
	})

	t.Run("endpoint lease is per endpoint", func(t *testing.T) {
		locks := newLocks("one-process")
		a, b := EndpointLock(uuid.NewString()), EndpointLock(uuid.NewString())

		ha, err := locks.AcquireResource(t.Context(), a, uuid.New(), KindResizeEndpoint)
		if err != nil {
			t.Fatalf("lock on endpoint a: %v", err)
		}
		t.Cleanup(func() { _ = ha.Release(context.WithoutCancel(t.Context())) })

		hb, err := locks.AcquireResource(t.Context(), b, uuid.New(), KindResizeEndpoint)
		if err != nil {
			t.Fatalf("lock on a different endpoint was refused: %v", err)
		}
		t.Cleanup(func() { _ = hb.Release(context.WithoutCancel(t.Context())) })
	})

	t.Run("the row names the holding task", func(t *testing.T) {
		// resource_token has no metadata column, so the owner string is the only place
		// "what is holding endpoint X, and why" can live.
		locks := newLocks("one-process")
		lock := EndpointLock(uuid.NewString())
		task := uuid.New()

		held, err := locks.AcquireResource(t.Context(), lock, task, KindResizeEndpoint)
		if err != nil {
			t.Fatalf("AcquireResource: %v", err)
		}
		t.Cleanup(func() { _ = held.Release(context.WithoutCancel(t.Context())) })

		got := resourceHolder(t, locks, lock)
		if !strings.Contains(got, task.String()) {
			t.Errorf("holder = %q, want it to name task %s", got, task)
		}
		if !strings.Contains(got, KindResizeEndpoint) {
			t.Errorf("holder = %q, want it to name the kind %s", got, KindResizeEndpoint)
		}
	})
}

// resourceHolder reads the holder through whichever probe the implementation has.
func resourceHolder(t *testing.T, locks Locks, r ResourceLock) string {
	t.Helper()
	switch l := locks.(type) {
	case *MemLocks:
		return l.ResourceHolder(r)
	case *PgLocks:
		got, err := l.Holder(t.Context(), r)
		if err != nil {
			t.Fatalf("Holder: %v", err)
		}
		return got
	default:
		t.Fatalf("no holder probe for %T", locks)
		return ""
	}
}

func TestMemLocksContract(t *testing.T) {
	store := NewMemStore()
	locksContract(t, func(owner string) Locks { return store.Locks(owner) })
}

// The fail-fast design rests on pglease.Config.Abort being UNSET, so that New
// substitutes AbortProcess. Setting it — a one-line change that reads as an
// improvement, and exactly what the perf test does deliberately — would leave the
// controller running with a lost task lease: two instances executing one task,
// which is the split-brain the locks exist to prevent.
func TestLeaseConfigKeepsTheProcessAbortDefault(t *testing.T) {
	cfg := leaseConfig(slog.New(slog.NewTextHandler(io.Discard, nil)), 30*time.Second, 10*time.Second)

	if cfg.Abort != nil {
		t.Error("leaseConfig sets Abort; it must stay nil so pglease substitutes AbortProcess " +
			"(architecture/ac-locking.md — losing a lease exits the process)")
	}
	if cfg.Logger == nil {
		t.Error("leaseConfig leaves Logger nil, which discards the failed-renewal warning " +
			"that precedes a lost lease")
	}
	// And the timings the controller ships must actually be renewable, or every
	// lease aborts one safety margin after the first missed renewal.
	if err := cfg.Validate(); err != nil {
		t.Errorf("the shipped lease timings are invalid: %v", err)
	}
}
