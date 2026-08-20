package task

import (
	"context"
	"fmt"
	"log/slog"
	"math/rand/v2"
	"os"
	"runtime/debug"
	"sync"
	"time"

	"github.com/google/uuid"

	"github.com/yugabyte/amp/controller/internal/config"
	"github.com/yugabyte/amp/controller/internal/db"
	applog "github.com/yugabyte/amp/controller/internal/log"
)

// maxClaimBackoff caps the exponential backoff between failed claim attempts, so
// a persistent claim error (e.g. the DB is down) is retried at a steady, sane
// rate rather than every poll interval.
const maxClaimBackoff = 30 * time.Second

// A step whose failure carries SQLSTATE 40001 is re-run rather than failing its
// task: the abort means the transaction rolled back in full, so nothing landed and
// the condition clears as soon as the writer that won commits. Why the controller
// provokes these deliberately, and which writers collide, is in
// controller/architecture/ac-tasks.md.
//
// Two bounds, because they bound different things. The unit re-run is the whole
// STEP, not the transaction that aborted, so a step whose external effects precede
// its retryable write repeats them once per attempt — delete_branch retires
// endpoint instances before tombstoning the branch row. They are idempotent (the
// Step contract), so that is waste, not damage; but an attempt count limits how
// many re-runs happen, not what they cost, which is what the wall-clock budget is
// for.
//
// Sound only because an operation that can surface such an abort is atomic — see
// the invariant on the ports in deps.go.
const (
	stepRetryAttempts = 6
	// Doubled per further attempt and jittered.
	stepRetryBackoff = 15 * time.Millisecond
	// Also sorts the cheap case from the expensive one: an abort from a short
	// transaction surfaces in milliseconds, so a step that took longer than this to
	// lose the interlock lost it after doing real work.
	stepRetryBudget = 2 * time.Second
)

// jitter spreads a backoff over [d/2, d], so several steps that lost the same
// interlock do not come back in lockstep and collide with each other again.
func jitter(d time.Duration) time.Duration {
	if d <= 0 {
		return 0
	}
	return d/2 + time.Duration(rand.Int64N(int64(d/2)+1))
}

// Worker claims runnable tasks under a lease and drives each to completion via a
// resume-safe step loop (§8.1). Claiming and execution are decoupled: a single
// claim loop hands every claimed task to its own goroutine, with at most
// cfg.MaxConcurrent running at once. The loop acquires an execution slot *before*
// it claims, so it never holds a lease on a task it can't immediately run.
// Structured for the HA reclaim described in §8.2.
// MetricsRecorder receives task-lifecycle events for metrics. It is deliberately
// small and lives here (not an import of internal/metrics) so the task package
// stays free of that dependency and a worker can run uninstrumented in tests. A
// worker with no recorder skips instrumentation entirely.
type MetricsRecorder interface {
	// TaskStarted marks a task entering execution (inflight++).
	TaskStarted()
	// TaskFinished marks a task leaving execution (inflight--) with its outcome:
	// status is one of "succeeded", "failed", or "interrupted"; dur is the
	// execution wall-clock time.
	TaskFinished(kind, status string, dur time.Duration)
	// StepRetried counts one re-run of a step after it lost a write interlock.
	// stepKind is the task_step.kind (e.g. record_branch_recipe), NOT the task
	// kind — they are different name spaces.
	StepRetried(stepKind string)
}

type Worker struct {
	store    Store
	registry *Registry
	cfg      config.TaskConfig
	logger   *slog.Logger
	owner    string
	metrics  MetricsRecorder
	// retryBudget is stepRetryBudget, held per-worker so a test can shorten it.
	retryBudget time.Duration
}

// Option configures an optional Worker collaborator.
type Option func(*Worker)

// WithMetrics attaches a metrics recorder to the worker.
func WithMetrics(r MetricsRecorder) Option { return func(w *Worker) { w.metrics = r } }

// NewWorker builds a worker. registry rebuilds each persisted step fully wired
// with its deps (it carries them). owner identifies this instance for lease
// claims. Optional collaborators (e.g. metrics) are supplied via Options.
func NewWorker(store Store, registry *Registry, cfg config.TaskConfig, logger *slog.Logger, opts ...Option) *Worker {
	w := &Worker{
		store:    store,
		registry: registry,
		cfg:      cfg,
		logger:   logger,
		owner:    ownerID(),

		retryBudget: stepRetryBudget,
	}
	for _, opt := range opts {
		opt(w)
	}
	return w
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
// An interruption — the worker's context was cancelled by shutdown, or this
// instance lost the lease (see heartbeat) — is not a failure: execute leaves the
// task reclaimable (§8.2), so it is logged as such rather than as an error;
// otherwise a graceful shutdown (or a reclaim) would emit one spurious "task
// failed" per in-flight task.
//
// A per-task context carries a lease heartbeat that renews the lease while the
// step loop runs, so a step that outlives LeaseTTL is not reclaimed and run
// concurrently by a second goroutine (§8.2). If the lease is lost anyway (the
// heartbeat can't renew, e.g. another instance already reclaimed it), the
// heartbeat cancels this context so the in-flight step stops rather than racing
// the new owner.
func (w *Worker) runTask(ctx context.Context, t *Task) {
	logger := w.logger.With(applog.KeyTaskID, t.ID.String(), "kind", t.Kind)
	logger.Debug("task claimed")

	if w.metrics != nil {
		w.metrics.TaskStarted()
	}
	start := time.Now()
	// status is the outcome reported to metrics; the interrupted case is not a
	// terminal failure (the task stays reclaimable, §8.2), so it is tracked
	// distinctly rather than as a failure. Default to failed so a panic that
	// unwinds past the switch isn't miscounted as a success.
	status := string(StatusFailed)
	// Record via defer so tasksInflight is always decremented and the outcome
	// recorded even if execute panics (a store call outside runStep's recover).
	defer func() {
		if w.metrics != nil {
			w.metrics.TaskFinished(t.Kind, status, time.Since(start))
		}
	}()

	taskCtx, cancel := context.WithCancel(ctx)
	defer cancel()
	var hb sync.WaitGroup
	hb.Add(1)
	go func() {
		defer hb.Done()
		w.heartbeat(taskCtx, logger, t.ID, cancel)
	}()

	err := w.execute(taskCtx, logger, t)
	// Classify before cancelling the heartbeat: a cancelled taskCtx (shutdown or
	// a lost lease) is an interruption, not a failure. Capture it here because
	// our own cancel() below would otherwise make taskCtx.Err() always non-nil.
	interrupted := taskCtx.Err() != nil
	cancel()  // stop the heartbeat
	hb.Wait() // and wait for it to exit before returning

	switch {
	case err == nil:
		status = string(StatusSucceeded)
		logger.Debug("task succeeded")
	case interrupted:
		status = "interrupted"
		logger.Info("task interrupted (shutdown or lost lease); left reclaimable", "error", err)
	default:
		status = string(StatusFailed)
		logger.Error("task failed", "error", err)
	}
}

// heartbeat renews the task's lease every RenewInterval until ctx is cancelled
// (the task finished or the worker is shutting down). If the lease can no longer
// be renewed — RenewLease reports it is no longer held, meaning another instance
// reclaimed the task after its lease lapsed — it cancels the task via cancel so
// the in-flight step stops instead of running concurrently with the new owner. A
// transient renewal error is logged and retried on the next tick (the lease has
// slack: RenewInterval < LeaseTTL), so a brief DB blip doesn't abandon the task.
func (w *Worker) heartbeat(ctx context.Context, logger *slog.Logger, taskID uuid.UUID, cancel context.CancelFunc) {
	ticker := time.NewTicker(w.cfg.RenewInterval)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			held, err := w.store.RenewLease(ctx, taskID, w.owner, w.cfg.LeaseTTL)
			if ctx.Err() != nil {
				return // finished/shutting down; a renewal racing that is not interesting
			}
			if err != nil {
				logger.Warn("task lease renewal failed; will retry", "error", err)
				continue
			}
			if !held {
				logger.Warn("task lease lost (reclaimed by another owner); cancelling execution")
				cancel()
				return
			}
		}
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
			_ = w.store.FailStep(ctx, t.ID, st.ID, w.owner, err)
			return fmt.Errorf("build step %d (%s): %w", i, st.Kind, err)
		}
		if err := w.store.StartStep(ctx, t.ID, st.ID, w.owner); err != nil {
			return fmt.Errorf("mark step running: %w", err)
		}
		// Stamp the step-scoped logger onto the context so steps log through
		// applog.FromContext with the task/step keys already attached
		// (controller/AGENTS.md: log through internal/log, not slog directly).
		stepCtx := applog.IntoContext(ctx, stepLog)
		res := w.runStepWithRetry(stepCtx, stepLog, step)
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
			_ = w.store.FailStep(ctx, t.ID, st.ID, w.owner, res.Err)
			return fmt.Errorf("step %d (%s): %w", i, st.Kind, res.Err)
		}
		if err := w.store.CompleteStep(ctx, t.ID, st.ID, w.owner, res.Output); err != nil {
			return fmt.Errorf("mark step succeeded: %w", err)
		}
		stepLog.Debug("step succeeded")
	}
	return w.store.CompleteTask(ctx, t.ID, w.owner)
}

// runStepWithRetry runs one step, re-running it while its failure is a
// serialization abort, within the bounds above. It returns the last result, so an
// exhausted retry fails the task through the ordinary path.
//
// A cancelled context stops the loop at once and returns the failure as-is:
// execute treats a cancelled ctx as an interruption and leaves the task
// reclaimable, which is what a shutdown mid-backoff should do.
func (w *Worker) runStepWithRetry(ctx context.Context, logger *slog.Logger, step Step) StepResult {
	var res StepResult
	start := time.Now()
	for attempt := 0; ; attempt++ {
		res = w.runStep(ctx, logger, step)
		if !res.Failed() || !db.IsAbortedTransaction(res.Err) {
			return res
		}
		if attempt+1 >= stepRetryAttempts || ctx.Err() != nil {
			return res
		}
		if elapsed := time.Since(start); elapsed > w.retryBudget {
			logger.Warn("step lost a write interlock but has spent its retry budget; failing it",
				"elapsed", elapsed.String(), "budget", w.retryBudget.String(),
				"attempt", attempt+1, "error", res.Err)
			return res
		}
		if w.metrics != nil {
			w.metrics.StepRetried(step.Kind())
		}
		logger.Warn("step lost a write interlock; re-running",
			"attempt", attempt+1, "attempts", stepRetryAttempts, "error", res.Err)

		timer := time.NewTimer(jitter(stepRetryBackoff << attempt))
		select {
		case <-ctx.Done():
			timer.Stop()
			return res
		case <-timer.C:
		}
	}
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
