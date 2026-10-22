package memory

import (
	"context"
	"time"

	"github.com/yugabyte/meko/backend/internal/store"
)

// CallRetention is how long the call log keeps a call.
const CallRetention = 90 * 24 * time.Hour

// pruneEvery is how often RunCallPruner prunes after its first run.
var pruneEvery = 24 * time.Hour

// RecordCall adds a call to the call log. The call already happened, so a failure to record it is
// logged, not returned.
func (s *Service) RecordCall(ctx context.Context, c store.Call) {
	// The caller's request may be over (its context canceled) by the time it's recorded.
	ctx, cancel := context.WithTimeout(context.WithoutCancel(ctx), 5*time.Second)
	defer cancel()
	if err := s.st.AddCall(ctx, c); err != nil && s.log != nil {
		s.log.Warn("couldn't record a call", "tool", c.Tool, "error", err)
	}
}

// RunCallPruner deletes calls older than CallRetention now and then every day, until ctx ends.
func (s *Service) RunCallPruner(ctx context.Context) {
	t := time.NewTicker(pruneEvery)
	defer t.Stop()
	for {
		n, err := s.st.PruneCalls(ctx, time.Now().Add(-CallRetention))
		switch {
		case err != nil && ctx.Err() == nil && s.log != nil:
			s.log.Warn("couldn't prune the call log", "error", err)
		case n > 0 && s.log != nil:
			s.log.Info("pruned the call log", "calls", n)
		}
		select {
		case <-ctx.Done():
			return
		case <-t.C:
		}
	}
}
