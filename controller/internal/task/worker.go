package task

import (
	"context"
	"fmt"
	"log/slog"
	"os"
	"time"

	"github.com/yugabyte/yb-amp/controller/internal/config"
	applog "github.com/yugabyte/yb-amp/controller/internal/log"
)

// Worker polls for runnable tasks, claims one under a lease, and drives it to
// completion via a resume-safe step loop (§8.1). It is single-instance for now
// but structured for the HA reclaim described in §8.2.
type Worker struct {
	store    Store
	registry *Registry
	cfg      config.TaskConfig
	logger   *slog.Logger
	owner    string
}

// NewWorker builds a worker. owner identifies this instance for lease claims.
func NewWorker(store Store, registry *Registry, cfg config.TaskConfig, logger *slog.Logger) *Worker {
	return &Worker{
		store:    store,
		registry: registry,
		cfg:      cfg,
		logger:   logger,
		owner:    ownerID(),
	}
}

func ownerID() string {
	host, err := os.Hostname()
	if err != nil || host == "" {
		host = "unknown"
	}
	return fmt.Sprintf("%s/%d", host, os.Getpid())
}

// Run drives the worker loop until ctx is cancelled.
func (w *Worker) Run(ctx context.Context) {
	w.logger.Info("task worker started", "owner", w.owner, "poll_interval", w.cfg.PollInterval.String())
	ticker := time.NewTicker(w.cfg.PollInterval)
	defer ticker.Stop()
	w.drain(ctx) // process anything already runnable before the first tick
	for {
		select {
		case <-ctx.Done():
			w.logger.Info("task worker stopping")
			return
		case <-ticker.C:
			w.drain(ctx)
		}
	}
}

// drain runs claimable tasks until none remain or the context is cancelled.
func (w *Worker) drain(ctx context.Context) {
	for ctx.Err() == nil {
		ran, err := w.runOne(ctx)
		if err != nil {
			w.logger.Error("task claim/run error", "error", err)
			return
		}
		if !ran {
			return
		}
	}
}

func (w *Worker) runOne(ctx context.Context) (bool, error) {
	t, err := w.store.ClaimNext(ctx, w.owner, w.cfg.LeaseTTL)
	if err != nil {
		return false, err
	}
	if t == nil {
		return false, nil
	}
	logger := w.logger.With(applog.KeyTaskID, t.ID.String(), "kind", t.Kind)
	logger.Info("task claimed")
	if err := w.execute(ctx, logger, t); err != nil {
		logger.Error("task failed", "error", err)
	} else {
		logger.Info("task succeeded")
	}
	return true, nil
}

// execute walks a task's steps, skipping already-succeeded ones so a reclaimed
// task resumes at the first unfinished step rather than from the top.
func (w *Worker) execute(ctx context.Context, logger *slog.Logger, t *Task) error {
	steps, err := w.store.Steps(ctx, t.ID)
	if err != nil {
		return fmt.Errorf("load steps: %w", err)
	}
	for _, st := range steps {
		if st.Status == StatusSucceeded {
			continue
		}
		stepLog := logger.With(applog.KeyStep, st.Ordinal, "step_kind", st.Kind)

		handler, ok := w.registry.Lookup(st.Kind)
		if !ok {
			err := fmt.Errorf("no handler registered for step kind %q", st.Kind)
			_ = w.store.FailStep(ctx, t.ID, st.Ordinal, err)
			return err
		}
		if err := w.store.StartStep(ctx, t.ID, st.Ordinal); err != nil {
			return fmt.Errorf("mark step running: %w", err)
		}
		// Stamp the step-scoped logger onto the context so step handlers log
		// through applog.FromContext with the task/step keys already attached
		// (controller/AGENTS.md: log through internal/log, not slog directly).
		stepCtx := applog.IntoContext(ctx, stepLog)
		result, err := handler.Run(stepCtx, StepInput{Task: *t, Step: st})
		if err != nil {
			// A cancelled worker context means we are shutting down (or the parent
			// was cancelled), not that the step genuinely failed. Leave the step and
			// task `running` so the lease expires and another instance (or this one
			// after restart) reclaims and replays the step (§8.2) — durably failing
			// a task over a shutdown would strand it. (Only triggers when ctx is
			// already cancelled; a real step error on a live ctx still fails below.)
			if ctx.Err() != nil {
				return fmt.Errorf("step %d (%s) interrupted: %w", st.Ordinal, st.Kind, ctx.Err())
			}
			_ = w.store.FailStep(ctx, t.ID, st.Ordinal, err)
			return fmt.Errorf("step %d (%s): %w", st.Ordinal, st.Kind, err)
		}
		if err := w.store.CompleteStep(ctx, t.ID, st.Ordinal, result); err != nil {
			return fmt.Errorf("mark step succeeded: %w", err)
		}
		stepLog.Info("step succeeded")
	}
	return w.store.CompleteTask(ctx, t.ID)
}
