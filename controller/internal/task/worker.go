package task

import (
	"context"
	"fmt"
	"log/slog"
	"os"
	"runtime/debug"
	"sync"
	"time"

	"github.com/yugabyte/yb-amp/controller/internal/config"
	applog "github.com/yugabyte/yb-amp/controller/internal/log"
)

// maxClaimBackoff caps the exponential backoff between failed claim attempts, so
// a persistent claim error (e.g. the DB is down) is retried at a steady, sane
// rate rather than every poll interval.
const maxClaimBackoff = 30 * time.Second

// Worker claims runnable tasks under a lease and drives each to completion via a
// resume-safe step loop (§8.1). Claiming and execution are decoupled: a single
// claim loop hands every claimed task to its own goroutine, with at most
// cfg.MaxConcurrent running at once. The loop acquires an execution slot *before*
// it claims, so it never holds a lease on a task it can't immediately run.
// Structured for the HA reclaim described in §8.2.
type Worker struct {
	store    Store
	registry *Registry
	cfg      config.TaskConfig
	logger   *slog.Logger
	owner    string
}

// NewWorker builds a worker. registry rebuilds each persisted step fully wired
// with its deps (it carries them). owner identifies this instance for lease
// claims.
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

// Run drives the claim loop until ctx is cancelled, then waits for in-flight
// tasks to unwind. A free execution slot (a buffered "semaphore" sized to
// cfg.MaxConcurrent) gates claiming: the loop blocks for a slot, claims one task,
// and runs it in its own goroutine that frees the slot when done — so claiming
// and execution proceed independently, up to MaxConcurrent tasks at once, and a
// task is never claimed without somewhere to run. When nothing is claimable it
// idles a poll interval before retrying.
func (w *Worker) Run(ctx context.Context) {
	w.logger.Info("task worker started",
		"owner", w.owner,
		"poll_interval", w.cfg.PollInterval.String(),
		"max_concurrent", w.cfg.MaxConcurrent)

	sem := make(chan struct{}, w.cfg.MaxConcurrent)
	var wg sync.WaitGroup
	claimErrs := 0 // consecutive claim failures, for backoff

	// Loop until ctx is cancelled. The shutdown paths use `continue` (which, unlike
	// `break`, targets the for loop even from inside the select/switch below), so
	// the condition does the exiting — no labeled break needed.
	for ctx.Err() == nil {
		// Acquire an execution slot before claiming, so a claimed task always has
		// somewhere to run (we never hold a lease on a task we can't start).
		select {
		case <-ctx.Done():
			continue
		case sem <- struct{}{}:
		}

		t, err := w.store.ClaimNext(ctx, w.owner, w.cfg.LeaseTTL)
		switch {
		case err != nil:
			<-sem
			if ctx.Err() == nil { // a real error, not shutdown
				claimErrs++
				w.logger.Error("task claim error", "error", err, "consecutive", claimErrs)
				w.backoff(ctx, claimErrs)
			}
		case t == nil:
			// Nothing runnable; release the slot and idle until the next poll.
			claimErrs = 0
			<-sem
			w.idle(ctx)
		default:
			claimErrs = 0
			wg.Add(1)
			go func(t *Task) {
				defer wg.Done()
				defer func() { <-sem }()
				w.runTask(ctx, t)
			}(t)
		}
	}

	w.drainInFlight(&wg)
}

// drainInFlight waits for in-flight task goroutines to finish, up to the
// configured shutdown grace. Past that it returns and lets the process exit,
// leaving any still-running tasks to be reclaimed once their leases expire (§8.2)
// — so a step that ignores cancellation can't wedge shutdown indefinitely.
func (w *Worker) drainInFlight(wg *sync.WaitGroup) {
	w.logger.Info("task worker stopping; waiting for in-flight tasks", "grace", w.cfg.ShutdownGrace.String())
	done := make(chan struct{})
	go func() { wg.Wait(); close(done) }()
	timer := time.NewTimer(w.cfg.ShutdownGrace)
	defer timer.Stop()
	select {
	case <-done:
		w.logger.Info("task worker stopped")
	case <-timer.C:
		w.logger.Warn("task worker shutdown grace elapsed; abandoning in-flight tasks (they remain reclaimable)")
	}
}

// wait sleeps for d, or returns early if ctx is cancelled (the loop's condition
// then exits).
func (w *Worker) wait(ctx context.Context, d time.Duration) {
	timer := time.NewTimer(d)
	defer timer.Stop()
	select {
	case <-ctx.Done():
	case <-timer.C:
	}
}

// idle waits one poll interval — used when there is nothing to claim.
func (w *Worker) idle(ctx context.Context) {
	w.logger.Debug("task worker idle", "poll_interval", w.cfg.PollInterval.String())
	w.wait(ctx, w.cfg.PollInterval)
}

// backoff waits before the next claim attempt, growing the delay exponentially
// from PollInterval with consecutive failures and capping it at maxClaimBackoff,
// so a persistent claim error doesn't hammer the store every poll interval.
func (w *Worker) backoff(ctx context.Context, attempt int) {
	d := w.cfg.PollInterval
	for i := 1; i < attempt && d < maxClaimBackoff; i++ {
		d *= 2
	}
	if d > maxClaimBackoff {
		d = maxClaimBackoff
	}
	w.wait(ctx, d)
}

// runTask drives one claimed task to completion, logging the outcome. A genuine
// failure is already recorded in the store by execute; here it is only logged.
// An interruption (the worker's context was cancelled mid-task by shutdown) is
// not a failure: execute leaves the task reclaimable (§8.2), so it is logged as
// such rather than as an error — otherwise a graceful shutdown would emit one
// spurious "task failed" per in-flight task.
func (w *Worker) runTask(ctx context.Context, t *Task) {
	logger := w.logger.With(applog.KeyTaskID, t.ID.String(), "kind", t.Kind)
	logger.Debug("task claimed")
	switch err := w.execute(ctx, logger, t); {
	case err == nil:
		logger.Debug("task succeeded")
	case ctx.Err() != nil:
		logger.Info("task interrupted by shutdown; left reclaimable", "error", err)
	default:
		logger.Error("task failed", "error", err)
	}
}

// execute walks a task's steps, skipping already-succeeded ones so a reclaimed
// task resumes at the first unfinished step rather than from the top.
func (w *Worker) execute(ctx context.Context, logger *slog.Logger, t *Task) error {
	steps, err := w.store.Steps(ctx, t.ID)
	if err != nil {
		return fmt.Errorf("load steps: %w", err)
	}
	for i, st := range steps {
		if st.Status == StatusSucceeded {
			continue
		}
		stepLog := logger.With(applog.KeyStep, i, "step_kind", st.Kind, "step_id", st.ID.String())

		// Reconstruct the executable step from its persisted kind+args. An unknown
		// kind or undecodable args can never run, so fail the task rather than spin
		// on it.
		step, err := w.registry.Build(st.Kind, st.Args)
		if err != nil {
			_ = w.store.FailStep(ctx, t.ID, st.ID, err)
			return fmt.Errorf("build step %d (%s): %w", i, st.Kind, err)
		}
		if err := w.store.StartStep(ctx, st.ID); err != nil {
			return fmt.Errorf("mark step running: %w", err)
		}
		// Stamp the step-scoped logger onto the context so steps log through
		// applog.FromContext with the task/step keys already attached
		// (controller/AGENTS.md: log through internal/log, not slog directly).
		stepCtx := applog.IntoContext(ctx, stepLog)
		res := w.runStep(stepCtx, stepLog, step)
		if res.Failed() {
			// A cancelled worker context means we are shutting down (or the parent
			// was cancelled), not that the step genuinely failed. Leave the step and
			// task `running` so the lease expires and another instance (or this one
			// after restart) reclaims and replays the step (§8.2) — durably failing
			// a task over a shutdown would strand it. (Only triggers when ctx is
			// already cancelled; a real step error on a live ctx still fails below.)
			if ctx.Err() != nil {
				return fmt.Errorf("step %d (%s) interrupted: %w", i, st.Kind, ctx.Err())
			}
			_ = w.store.FailStep(ctx, t.ID, st.ID, res.Err)
			return fmt.Errorf("step %d (%s): %w", i, st.Kind, res.Err)
		}
		if err := w.store.CompleteStep(ctx, st.ID, res.Output); err != nil {
			return fmt.Errorf("mark step succeeded: %w", err)
		}
		stepLog.Debug("step succeeded")
	}
	return w.store.CompleteTask(ctx, t.ID)
}

// runStep executes one step, converting a panic into a step failure so a single
// bad handler fails its task instead of crashing the worker (and taking every
// other in-flight task down with it). The full stack is logged; the returned
// failure carries a concise cause (it lands in task_step.error).
func (w *Worker) runStep(ctx context.Context, logger *slog.Logger, step Step) (res StepResult) {
	defer func() {
		if p := recover(); p != nil {
			logger.Error("step panicked", "panic", fmt.Sprint(p), "stack", string(debug.Stack()))
			res = Fail(fmt.Errorf("step %s panicked: %v", step.Kind(), p))
		}
	}()
	return step.Execute(ctx)
}
