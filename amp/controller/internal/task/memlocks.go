package task

import (
	"context"
	"fmt"
	"sync"
	"sync/atomic"
	"time"

	"github.com/google/uuid"

	"github.com/yugabyte/amp/controller/pkg/pglease"
)

const (
	// memLockPoll is how often a waiting AcquireResource re-checks. Only MemLocks
	// polls: pglease sleeps the denial's own RetryAfter.
	memLockPoll = 2 * time.Millisecond

	// memLockWait bounds a wait, so a test contending a lock nothing releases fails
	// here rather than hanging out the package timeout. MemLocks's own bound, not
	// --task-lock-wait; pass a ctx deadline to pick one (see Locks.AcquireResource).
	memLockWait = 5 * time.Second
)

// memLockTable models amp_registry.resource_token: one row per key, holding the
// current lease only, with takeover overwriting in place.
//
// It models the decision a pglease CALLER sees — token_acquire composed with
// pglease's reserve guard, not token_acquire alone. So a different owner is refused
// until the row frees, and so is the current owner's second acquire, which
// token_acquire would grant as a renewal but pglease refuses a layer up.
//
// The clock is deliberately not modelled: a row is held until released or Expired,
// because a test that outwaits a real lease is slow and proves nothing about the
// SQL. Expire means "that holder is gone"; timing belongs to pglease's tests and
// the live-database ones.
type memLockTable struct {
	mu   sync.Mutex
	rows map[string]*memLockRow
}

type memLockRow struct {
	owner   string
	lapsed  bool // the holder is gone; a different owner may take over
	granted *MemLease
}

func newMemLockTable() *memLockTable {
	return &memLockTable{rows: map[string]*memLockRow{}}
}

// acquire grants, renews or refuses the lease on key in one step, mirroring
// token_acquire. A refusal reports the holder alongside the error, as pglease's
// *Denial does, because a NO-WAIT caller has to tell its own predecessor's row
// from a rival's (locks.go, noWaitDenial).
func (t *memLockTable) acquire(key, owner string) (*MemLease, string, error) {
	t.mu.Lock()
	defer t.mu.Unlock()
	row := t.rows[key]
	switch {
	// LOAD-BEARING, and it must precede the lapsed case: pglease's Client.reserve
	// refuses while this owner still has a live Held for the key, whatever the row
	// says. Without this the model grants a LAPSED row back to its own holder,
	// producing two Held over one row — the double execution the task lock exists to
	// prevent, and a divergence that would hide the same bug in PgLocks.
	case row != nil && row.owner == owner && row.granted.live():
		return nil, owner, fmt.Errorf("%w: %w", ErrLockHeld, &pglease.Denial{Key: key, Holder: owner})
	case row == nil, row.lapsed:
		// Free: takeover. A lapsed row's previous holder learns it lost the lease
		// the same way pglease's does — its lease context is cancelled.
		if row != nil && row.granted != nil {
			row.granted.end(ErrLockHeld)
		}
	case row.owner == owner:
		return nil, owner, fmt.Errorf("%w: %w", ErrLockHeld, &pglease.Denial{Key: key, Holder: owner})
	default:
		return nil, row.owner, fmt.Errorf("%w: %w", ErrLockHeld, &pglease.Denial{Key: key, Holder: row.owner})
	}

	ctx, cancel := context.WithCancelCause(context.Background())
	lease := &MemLease{key: key, owner: owner, table: t, ctx: ctx, cancel: cancel}
	t.rows[key] = &memLockRow{owner: owner, granted: lease}
	return lease, "", nil
}

// release drops the row if owner still holds it; otherwise a no-op, as
// token_release is.
func (t *memLockTable) release(key, owner string) {
	t.mu.Lock()
	defer t.mu.Unlock()
	if row := t.rows[key]; row != nil && row.owner == owner {
		delete(t.rows, key)
	}
}

// expire models the holder going away without releasing: the row stays, since
// nothing reclaims it, but becomes takeable.
func (t *memLockTable) expire(key string) {
	t.mu.Lock()
	defer t.mu.Unlock()
	if row := t.rows[key]; row != nil {
		row.lapsed = true
	}
}

// heldBy reports the owner holding key, or "" when it is free to a new owner.
func (t *memLockTable) heldBy(key string) string {
	t.mu.Lock()
	defer t.mu.Unlock()
	row := t.rows[key]
	if row == nil || row.lapsed {
		return ""
	}
	return row.owner
}

// MemLease is one lease from a MemLocks.
type MemLease struct {
	key   string
	owner string
	table *memLockTable

	ctx    context.Context
	cancel context.CancelCauseFunc
	once   sync.Once
	lost   atomic.Bool
}

func (l *MemLease) Context() context.Context { return l.ctx }

func (l *MemLease) Release(context.Context) error {
	l.once.Do(func() {
		// LOAD-BEARING, mirroring pglease's Held.Release: a LOST lease must not delete
		// the row. The successor may hold it under the same owner string, and release
		// matches on owner alone, so deleting would un-fence a live holder.
		if !l.lost.Load() {
			l.table.release(l.key, l.owner)
		}
		l.cancel(nil)
	})
	return nil
}

// end marks the lease lost and cancels it, without touching the row — how a holder
// learns somebody took over.
func (l *MemLease) end(cause error) {
	l.lost.Store(true)
	l.cancel(cause)
}

// live reports whether this lease has neither been released nor lost. Nil-safe, so
// acquire can ask it of a row whose grant is absent.
func (l *MemLease) live() bool { return l != nil && l.ctx.Err() == nil }

// MemLocks is the in-memory Locks the unit tests run against: one owner's view
// of a MemStore's lock table. Two MemLocks over one store are two controller
// instances contending for the same rows.
type MemLocks struct {
	table *memLockTable
	owner string

	// Wait bounds AcquireResource, defaulting to memLockWait. It is MemLocks's
	// analogue of --task-lock-wait, and settable so a test can exercise the
	// wait-exhausted path without the caller's own context expiring — which the
	// worker would classify as a shutdown rather than a lock failure.
	Wait time.Duration
}

var _ Locks = (*MemLocks)(nil)

// Locks returns this store's lock table as seen by owner. Pass a different owner
// for each simulated instance.
func (s *MemStore) Locks(owner string) *MemLocks {
	return &MemLocks{table: s.locks, owner: owner}
}

// ExpireTaskLease models the holder of a task's lease dying without releasing:
// the row becomes takeable, so the task turns up as a candidate again.
func (s *MemStore) ExpireTaskLease(taskID uuid.UUID) { s.locks.expire(taskLockKey(taskID)) }

func (m *MemLocks) Owner() string { return m.owner }

func (m *MemLocks) AcquireTask(_ context.Context, taskID uuid.UUID) (Lease, error) {
	lease, _, err := m.table.acquire(taskLockKey(taskID), m.owner)
	if err != nil {
		return nil, err
	}
	return lease, nil
}

func (m *MemLocks) AcquireResource(ctx context.Context, r ResourceLock, holderID uuid.UUID, kind string) (Lease, error) {
	owner := resourceOwner(holderID, kind, m.owner)
	if r.NoWait {
		// One attempt, no poll loop: the refusal is the answer, as it is against
		// token_acquire (locks.go). Ahead of the wait bound below, which a no-wait
		// lock reaches only for the one case noWaitDenial sends there — a row this
		// holder itself left behind in a dead process.
		lease, _, err := m.table.acquire(r.Key(), owner)
		if err == nil {
			return lease, nil
		}
		if denial := noWaitDenial(r, err, owner); denial != nil {
			return nil, denial
		}
	}
	wait := m.Wait
	if wait <= 0 {
		wait = memLockWait
	}
	// An INNER bound, as PgLocks does with --task-lock-wait: exhausting it must
	// leave the caller's context live, or the worker reads it as a shutdown.
	waitCtx, cancel := context.WithTimeout(ctx, wait)
	defer cancel()
	for {
		lease, _, err := m.table.acquire(r.Key(), owner)
		if err == nil {
			return lease, nil
		}
		timer := time.NewTimer(memLockPoll)
		select {
		case <-waitCtx.Done():
			timer.Stop()
			return nil, fmt.Errorf("resource lock %s: %w: %w", r, waitCtx.Err(), err)
		case <-timer.C:
		}
	}
}

// ResourceHolder reports the owner holding r, or "" when it is free.
func (m *MemLocks) ResourceHolder(r ResourceLock) string { return m.table.heldBy(r.Key()) }
