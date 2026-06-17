package process

import (
	"context"
	"log/slog"
	"sync"
)

// Manager supervises a set of child processes for the lifetime of a Run call.
// It is the controller's process-supervision entry point: register processes
// with Add, then call Run from the serve loop alongside the API server and the
// task worker (cmd/amp/main.go).
type Manager struct {
	logger      *slog.Logger
	supervisors []*Supervisor
}

// NewManager returns an empty manager.
func NewManager(logger *slog.Logger) *Manager {
	return &Manager{logger: logger}
}

// Add registers a process to supervise and returns its supervisor (whose Ready
// channel callers may wait on). Call Add before Run.
func (m *Manager) Add(spec Spec) *Supervisor {
	s := NewSupervisor(spec, m.logger)
	m.supervisors = append(m.supervisors, s)
	return s
}

// Len reports how many processes are registered.
func (m *Manager) Len() int { return len(m.supervisors) }

// Run supervises every registered process until ctx is cancelled, then waits
// for them all to stop before returning. With no registered processes it
// returns immediately, so it is safe to call unconditionally.
func (m *Manager) Run(ctx context.Context) {
	var wg sync.WaitGroup
	for _, s := range m.supervisors {
		wg.Add(1)
		go func(s *Supervisor) {
			defer wg.Done()
			s.Run(ctx)
		}(s)
	}
	wg.Wait()
}
