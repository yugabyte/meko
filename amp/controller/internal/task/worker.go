package task

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"math/rand/v2"
	"runtime/debug"
	"sync"
	"time"

	"github.com/google/uuid"

	"github.com/yugabyte/amp/controller/internal/config"
	"github.com/yugabyte/amp/controller/internal/db"
	applog "github.com/yugabyte/amp/controller/internal/log"
	"github.com/yugabyte/amp/controller/pkg/pglease"
)

// maxClaimBackoff caps the exponential backoff between failed claim attempts, so
// a persistent claim error (e.g. the DB is down) is retried at a steady, sane
// rate rather than every poll interval.
const maxClaimBackoff = 30 * time.Second

// claimCandidates is how many tasks one claim attempt tries the lease on before
// idling. It replaces what FOR UPDATE SKIP LOCKED did — spreading instances over
// different rows — so it need only exceed the instance count, not the backlog.
const claimCandidates = 10

// lockWaitLogThreshold is how long a resource-lock wait must run to be worth a log
// line. Below it, contention is ordinary.
const lockWaitLogThreshold = time.Second

// releaseTimeout bounds giving a lease back. WithoutCancel is right there — a
// cancelled release costs the next holder a whole TTL — but unbounded it would let
// a wedged pool park a finishing task inside Release while it still holds its
// execution slot.
const releaseTimeout = 10 * time.Second

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
	locks    Locks
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
// with its deps (it carries them); locks hands out the task and resource leases;
// subtasks backs the runner a fan-out parent drives its children with. Optional
// collaborators (e.g. metrics) are supplied via Options.
//
// The owner comes from locks rather than being generated here, so the fence on
// every task and step mutation names the same thing the lease does.
//
// The runner is built here, after w exists, because the cycle is unavoidable: a
// runner needs the Worker, the Worker needs the Registry, and the Registry
// carries the Deps a step is built from. Writing Deps.Subtasks beforehand cannot
// work — the Registry holds a Deps by VALUE and hands a copy to every builder.
func NewWorker(store Store, subtasks SubtaskStore, registry *Registry, locks Locks, cfg config.TaskConfig, logger *slog.Logger, opts ...Option) *Worker {
	w := &Worker{
		store:    store,
		registry: registry,
		locks:    locks,
		cfg:      cfg,
		logger:   logger,
		owner:    locks.Owner(),

		retryBudget: stepRetryBudget,
	}
	registry.deps.Subtasks = NewSubtaskRunner(w, subtasks)
	for _, opt := range opts {
		opt(w)
	}
	return w
}

// ownerID identifies this worker for lease claims and for the owner fence on
// every task and step mutation.
//
// An owner must identify one PROCESS: a lease is granted unconditionally to its
// current holder, so a restarted pod that recycles a hostname and pid would
// otherwise read as the same owner and be handed a lease it never held.
// NewOwner's nonce is what makes it a different one.
func ownerID() string {
	return pglease.NewOwner("task-worker")
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

		t, lease, err := w.claim(ctx)
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
			go func(t *Task, lease Lease) {
				defer wg.Done()
				defer func() { <-sem }()
				w.runTask(ctx, t, lease)
			}(t, lease)
		}
	}

	w.drainInFlight(&wg)
}

// claim takes the execution lease on one task and transitions it to running,
// returning nil when nothing is claimable.
//
// The acquire IS the claim and comes first, so a task never reaches `running`
// unowned. Candidates only narrows what to try, and a denial is not worth waiting
// on — somebody is already running that task.
// releaseLease gives a lease back, detached from ctx but bounded: see
// releaseTimeout.
func releaseLease(ctx context.Context, lease Lease) error {
	ctx, cancel := context.WithTimeout(context.WithoutCancel(ctx), releaseTimeout)
	defer cancel()
	return lease.Release(ctx)
}

func (w *Worker) claim(ctx context.Context) (*Task, Lease, error) {
	cands, err := w.store.Candidates(ctx, claimCandidates)
	if err != nil {
		return nil, nil, fmt.Errorf("read claim candidates: %w", err)
	}
	for _, c := range cands {
		lease, err := w.locks.AcquireTask(ctx, c.ID)
		if errors.Is(err, ErrLockHeld) {
			// "Somebody" is usually another instance, but it can also be this one:
			// ErrLockHeld folds pglease's ErrAlreadyHeld, which a candidate read
			// racing our own transition produces. Neither is worth more than a debug
			// line — walking past a task somebody is running is the mechanism working,
			// not an event.
			w.logger.Debug("task lease already held; trying the next candidate",
				"task_id", c.ID, "kind", c.Kind, "error", err)
			continue
		}
		if err != nil {
			return nil, nil, fmt.Errorf("acquire task lease: %w", err)
		}
		t, err := w.store.Claim(ctx, c.ID, w.owner)
		if err != nil || t == nil {
			// Either the write failed or the task moved on between the candidate read
			// and now. Give the lease back so the key is free immediately rather than
			// for a TTL.
			_ = releaseLease(ctx, lease)
			if err != nil {
				return nil, nil, fmt.Errorf("claim task %s: %w", c.ID, err)
			}
			continue
		}
		return t, lease, nil
	}
	return nil, nil, nil
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

// runTask drives one claimed task to completion and logs the outcome; execute has
// already recorded a genuine failure in the store. A shutdown interruption is not a
// failure — the task is left reclaimable (§8.2) — or a graceful stop would emit one
// spurious "task failed" per in-flight task.
//
// Nothing here watches the lease, deliberately: losing one exits the process
// (architecture/ac-locking.md), so recovery code could never run. Releasing when
// the task leaves execution is what frees the key at once instead of a TTL later,
// which is what makes a graceful restart cheap.
func (w *Worker) runTask(ctx context.Context, t *Task, lease Lease) {
	logger := w.logger.With(applog.KeyTaskID, t.ID.String(), "kind", t.Kind)
	logger.Debug("task claimed")

	// Under a context of its own: on the shutdown path ctx is already cancelled, and
	// a release that never reaches the database costs the next holder a whole TTL.
	defer func() {
		if err := releaseLease(ctx, lease); err != nil {
			logger.Warn("releasing task lease", "error", err)
		}
	}()

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

	err := w.execute(ctx, logger, t)

	switch {
	case err == nil:
		status = string(StatusSucceeded)
		logger.Debug("task succeeded")
	case ctx.Err() != nil:
		status = "interrupted"
		logger.Info("task interrupted by shutdown; left reclaimable", "error", err)
	default:
		status = string(StatusFailed)
		logger.Error("task failed", "error", err)
	}
}

// running is the identity of the task a step is executing under, stamped on the
// step's context by execute.
//
// A step normally needs neither field — its args name what it acts on and the
// worker records its outcome. A FAN-OUT parent needs both: it creates children
// under its own task id and runs them under the owner every task write is fenced
// on (ac-endpoint-upgrade.md §5.3), and neither can be an arg (the owner is
// per-execution, and a reclaimed parent must use the NEW one).
type running struct {
	taskID uuid.UUID
	owner  string
}

type runningCtxKey struct{}

func intoRunning(ctx context.Context, r running) context.Context {
	return context.WithValue(ctx, runningCtxKey{}, r)
}

// runningFrom returns the executing task's identity. The second result is false
// when the step was not run by a worker, which a step that needs it must fail on
// rather than guess at.
func runningFrom(ctx context.Context) (running, bool) {
	r, ok := ctx.Value(runningCtxKey{}).(running)
	return r, ok
}

// execute walks a task's steps, skipping already-succeeded ones so a reclaimed
// task resumes at the first unfinished step rather than from the top.
func (w *Worker) execute(ctx context.Context, logger *slog.Logger, t *Task) error {
	steps, err := w.store.Steps(ctx, t.ID)
	if err != nil {
		return fmt.Errorf("load steps: %w", err)
	}
	// Held from the step that declared it until the task leaves execution, not
	// released between steps: a later step may still be on the same resource.
	var resource Lease
	var resourceKey string
	defer func() {
		if resource != nil {
			if err := releaseLease(ctx, resource); err != nil {
				logger.Warn("releasing resource lock", "lock", resourceKey, "error", err)
			}
		}
	}()

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
		if lease, key, err := w.lockFor(ctx, stepLog, t, step, resource, resourceKey); err != nil {
			if ctx.Err() != nil {
				return fmt.Errorf("step %d (%s) interrupted: %w", i, st.Kind, ctx.Err())
			}
			_ = w.store.FailStep(ctx, t.ID, st.ID, w.owner, err)
			return fmt.Errorf("step %d (%s): %w", i, st.Kind, err)
		} else if lease != nil {
			resource, resourceKey = lease, key
		}
		if err := w.store.StartStep(ctx, t.ID, st.ID, w.owner); err != nil {
			return fmt.Errorf("mark step running: %w", err)
		}
		// Stamp the step-scoped logger onto the context so steps log through
		// applog.FromContext with the task/step keys already attached
		// (controller/AGENTS.md: log through internal/log, not slog directly), plus
		// the executing task's identity for a fan-out parent (see running).
		stepCtx := intoRunning(applog.IntoContext(ctx, stepLog), running{taskID: t.ID, owner: w.owner})
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
	// Every step succeeded, so the generation the task declared has actually been
	// applied — stamp it before the task goes terminal. This is the whole reason a
	// failed async task no longer latches as applied (meko#985), and it is here
	// rather than in a per-task step so that no planner can wire the task and forget
	// the stamp.
	if err := w.stampYCPGeneration(ctx, logger, t); err != nil {
		return err
	}
	return w.store.CompleteTask(ctx, t.ID, w.owner)
}

// stampYCPGeneration records the applied YCP desired-state generation on the
// resource the task named, for a task planned from YCP desired state; a no-op for
// every other task.
//
// Not inside CompleteTask's transaction, and it does not need to be: a crash
// between the two leaves the task reclaimable, and the reclaim skips every
// already-succeeded step and re-runs this, whose write is an absolute monotonic
// UPDATE. An unknown resource is a task failure rather than a silent skip — the
// value is only trustworthy if every stamp that was supposed to happen did.
func (w *Worker) stampYCPGeneration(ctx context.Context, logger *slog.Logger, t *Task) error {
	if t.YCP == nil {
		return nil
	}
	state := w.registry.deps.State
	if state == nil {
		return fmt.Errorf("task declares a ycp generation but no state store is configured")
	}
	var err error
	switch t.YCP.Resource {
	case YCPResourceEndpoint:
		err = state.SetEndpointYCPGeneration(ctx, t.YCP.ID, t.YCP.Generation)
	case YCPResourceBranch:
		err = state.SetBranchYCPGeneration(ctx, t.YCP.ID, t.YCP.Generation)
	default:
		return fmt.Errorf("unknown ycp resource %q on task %s", t.YCP.Resource, t.ID)
	}
	if err != nil {
		return fmt.Errorf("stamp ycp generation on %s %s: %w", t.YCP.Resource, t.YCP.ID, err)
	}
	logger.Debug("ycp generation stamped",
		"ycp_resource", t.YCP.Resource, "ycp_resource_id", t.YCP.ID.String(), "ycp_generation", t.YCP.Generation)
	return nil
}

// lockFor takes the resource lock a step declares, waiting out a holder. It returns
// a nil lease when the step needs none or the holder already holds the right one,
// and refuses a SECOND, different lock within one unit of work.
//
// One lock per unit of work is no longer the whole deadlock argument: a parent
// holding `fleet:endpoints` runs children that each take `endpoint:<id>`, so the
// process holds two at once. What keeps that safe is the ORDER — `fleet` before
// `endpoint`, never the reverse (ac-locking.md).
func (w *Worker) lockFor(ctx context.Context, logger *slog.Logger, holder Holder, step Step, held Lease, heldKey string) (Lease, string, error) {
	locking, ok := step.(Locking)
	if !ok {
		return nil, "", nil
	}
	lock := locking.Lock()
	if lock.Zero() {
		return nil, "", nil
	}
	if held != nil {
		if heldKey == lock.Key() {
			return nil, "", nil
		}
		return nil, "", fmt.Errorf("step %s wants lock %s while %s is held: one unit of work may hold at most one",
			step.Kind(), lock, heldKey)
	}

	start := time.Now()
	lease, err := w.locks.AcquireResource(ctx, lock, holder.HolderID(), holder.HolderKind())
	if err != nil {
		return nil, "", err
	}
	if waited := time.Since(start); waited > lockWaitLogThreshold {
		logger.Info("waited for a resource lock", "lock", lock.Key(), "waited", waited.String())
	}
	logger.Debug("resource lock held", "lock", lock.Key())
	return lease, lock.Key(), nil
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
