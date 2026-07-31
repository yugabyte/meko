// Package sched holds the packing scheduler and the Resource Lease Manager
// (architecture §4.4) — pure coordination logic with no infrastructure. The
// lease manager bounds how many environments exist at once; the scheduler packs
// SHARED tests onto shared environments and gives EXCLUSIVE tests their own.
package sched

import (
	"context"
	"fmt"
	"sync"
)

// Manager bounds concurrent environments by a total capacity of weight units.
// Acquire blocks until enough capacity frees, serving waiters strict-FIFO so a
// large request is never starved by a stream of small ones behind it.
type Manager struct {
	mu       sync.Mutex
	capacity int
	used     int
	peak     int
	waiters  []*waiter
}

type waiter struct {
	weight  int
	ready   chan struct{}
	granted bool // set under mu when dispatched, so a cancel knows to release
}

// Lease is a held allocation of capacity. Release returns it; it is idempotent.
type Lease struct {
	mgr      *Manager
	weight   int
	pinned   bool
	released bool
	mu       sync.Mutex
}

// NewManager returns a lease manager with the given total capacity (min 1).
func NewManager(capacity int) *Manager {
	if capacity < 1 {
		capacity = 1
	}
	return &Manager{capacity: capacity}
}

// Acquire blocks until weight units are free (strict-FIFO behind earlier
// waiters) or ctx is done. A weight above total capacity is an error — it could
// never be satisfied.
func (m *Manager) Acquire(ctx context.Context, weight int) (*Lease, error) {
	if weight < 1 {
		weight = 1
	}
	m.mu.Lock()
	if weight > m.capacity {
		m.mu.Unlock()
		return nil, fmt.Errorf("lease weight %d exceeds capacity %d", weight, m.capacity)
	}
	if len(m.waiters) == 0 && m.used+weight <= m.capacity {
		m.grantLocked(weight)
		m.mu.Unlock()
		return &Lease{mgr: m, weight: weight}, nil
	}
	w := &waiter{weight: weight, ready: make(chan struct{})}
	m.waiters = append(m.waiters, w)
	m.mu.Unlock()

	select {
	case <-w.ready:
		return &Lease{mgr: m, weight: weight}, nil
	case <-ctx.Done():
		m.cancelWaiter(w)
		return nil, ctx.Err()
	}
}

// grantLocked reserves weight and updates the high-water mark. Caller holds mu.
func (m *Manager) grantLocked(weight int) {
	m.used += weight
	if m.used > m.peak {
		m.peak = m.used
	}
}

// dispatchLocked grants waiters from the front while the head fits. Strict FIFO:
// it stops at the first waiter that does not fit rather than skipping ahead, so
// a big request at the head is not starved. Caller holds mu.
func (m *Manager) dispatchLocked() {
	for len(m.waiters) > 0 {
		w := m.waiters[0]
		if m.used+w.weight > m.capacity {
			return
		}
		m.grantLocked(w.weight)
		w.granted = true
		m.waiters = m.waiters[1:]
		close(w.ready)
	}
}

func (m *Manager) release(weight int) {
	m.mu.Lock()
	m.used -= weight
	m.dispatchLocked()
	m.mu.Unlock()
}

// cancelWaiter removes a still-queued waiter, or — if it was granted between the
// select firing on ctx and this call — releases the capacity it received.
func (m *Manager) cancelWaiter(w *waiter) {
	m.mu.Lock()
	for i, x := range m.waiters {
		if x == w {
			m.waiters = append(m.waiters[:i], m.waiters[i+1:]...)
			m.mu.Unlock()
			return
		}
	}
	granted := w.granted
	m.mu.Unlock()
	if granted {
		m.release(w.weight)
	}
}

// Capacity is the total capacity in weight units.
func (m *Manager) Capacity() int { return m.capacity }

// Used is the currently reserved weight.
func (m *Manager) Used() int {
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.used
}

// Peak is the high-water mark of reserved weight over the manager's lifetime.
func (m *Manager) Peak() int {
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.peak
}

// Waiting is the number of blocked waiters.
func (m *Manager) Waiting() int {
	m.mu.Lock()
	defer m.mu.Unlock()
	return len(m.waiters)
}

// Release returns the lease's capacity. It is idempotent and safe to call from
// any goroutine. A PINNED lease is exempt: Release is a no-op on it, so a
// standing PERSISTENT environment's capacity is never returned to the pool by
// the ordinary end-of-run release path — only an explicit Unpin frees it.
func (l *Lease) Release() {
	l.mu.Lock()
	if l.released || l.pinned {
		l.mu.Unlock()
		return
	}
	l.released = true
	l.mu.Unlock()
	l.mgr.release(l.weight)
}

// Pin marks the lease as pinned (for a PERSISTENT / standing environment): it
// keeps counting against capacity for the environment's whole life and is
// excluded from reclamation — ordinary Release leaves it held. Only Unpin
// returns its capacity. Idempotent.
func (l *Lease) Pin() {
	l.mu.Lock()
	l.pinned = true
	l.mu.Unlock()
}

// Unpin clears the pin and returns the lease's capacity to the pool — the
// explicit teardown path for a PERSISTENT lease (invoked when the standing
// environment is finally reaped). Idempotent; a no-op once released.
func (l *Lease) Unpin() {
	l.mu.Lock()
	if l.released {
		l.mu.Unlock()
		return
	}
	l.pinned = false
	l.released = true
	l.mu.Unlock()
	l.mgr.release(l.weight)
}

// Pinned reports whether the lease has been pinned.
func (l *Lease) Pinned() bool {
	l.mu.Lock()
	defer l.mu.Unlock()
	return l.pinned
}

// Weight is the capacity this lease holds.
func (l *Lease) Weight() int { return l.weight }
