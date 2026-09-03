package task

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"sync"

	"github.com/google/uuid"

	applog "github.com/yugabyte/amp/controller/internal/log"
)

// Subtasks runs a parent's children to completion. No task lease is taken: a
// subtask is not claimable (§5.2), so the parent's lease is its liveness and the
// parent stays non-terminal until every child has finished.
//
// A framework concept rather than an upgrade-local one, because "different
// planners over the same child" is the reason for the shape
// (architecture/ac-endpoint-upgrade.md §9).
type Subtasks interface {
	// Create records a subtask, returning the existing row on a replay so the
	// parent can reconcile against its status.
	Create(ctx context.Context, parentID uuid.UUID, kind string, args json.RawMessage) (*Subtask, error)

	// ByParent returns the children a parent already has, oldest first. A replaying
	// planner reconciles against them BEFORE deciding what to create, which is what
	// keeps a filter over desired state from dropping an endpoint whose child is
	// unfinished (ac-endpoint-upgrade.md §5.5).
	ByParent(ctx context.Context, parentID uuid.UUID) ([]*Subtask, error)

	// Run executes one subtask under owner — the parent's owner, so the child's
	// writes carry the fence every task write has. It runs what it is given:
	// deciding a child is already done is the parent's job.
	Run(ctx context.Context, s *Subtask, owner string) error

	// Abandon marks a child that will never be attempted terminal, with the reason.
	//
	// A fan-out creates every child up front, so a parent that stops early leaves
	// rows nobody will run. It must not leave them PENDING: the parent is about to
	// go terminal, and a terminal parent is never reclaimed, so those rows would sit
	// pending forever and a re-fired roll derives different child ids — nothing ever
	// picks them up. Only for an exit that makes the parent terminal; an interrupted
	// parent stays reclaimable and its children are meant to survive for the replay.
	Abandon(ctx context.Context, s *Subtask, owner, reason string) error
}

// SubtaskRunner is the real Subtasks: a subtask store plus the worker's step
// machinery. It holds the worker rather than restating lockFor and
// runStepWithRetry, so a child gets the same one-lock rule, abort retry and panic
// boundary a task's step gets.
//
// Run is called from up to max_parallel goroutines of one parent step at once,
// which is sound because a Worker's fields and everything they reach are
// read-only by the time anything runs.
type SubtaskRunner struct {
	store SubtaskStore
	w     *Worker

	// live guards one id against being Run twice at once. See reserve.
	mu   sync.Mutex
	live map[uuid.UUID]struct{}
}

// NewSubtaskRunner binds a subtask store to the worker whose step machinery runs
// the children.
func NewSubtaskRunner(w *Worker, store SubtaskStore) *SubtaskRunner {
	return &SubtaskRunner{store: store, w: w, live: make(map[uuid.UUID]struct{})}
}

var _ Subtasks = (*SubtaskRunner)(nil)

// ErrSubtaskRunning means this process is already running that subtask.
var ErrSubtaskRunning = errors.New("task: subtask is already running in this process")

// reserve stops one runner from executing the same subtask twice at once.
//
// Load-bearing, because nothing below it catches this: two concurrent Runs of one
// id compute an identical resourceOwner, and AcquireResource builds a fresh
// pglease.Client per call, so pglease's own per-client guard never fires — both
// would hold a live lease over one row, which is two concurrent helm runs on one
// release. A planner reaches it by appending every Create result to a work list,
// since a duplicate Create returns the same row.
func (r *SubtaskRunner) reserve(id uuid.UUID) error {
	r.mu.Lock()
	defer r.mu.Unlock()
	if _, ok := r.live[id]; ok {
		return fmt.Errorf("%w: %s", ErrSubtaskRunning, id)
	}
	r.live[id] = struct{}{}
	return nil
}

func (r *SubtaskRunner) release(id uuid.UUID) {
	r.mu.Lock()
	defer r.mu.Unlock()
	delete(r.live, id)
}

func (r *SubtaskRunner) Create(ctx context.Context, parentID uuid.UUID, kind string, args json.RawMessage) (*Subtask, error) {
	return r.store.Create(ctx, parentID, kind, args)
}

func (r *SubtaskRunner) ByParent(ctx context.Context, parentID uuid.UUID) ([]*Subtask, error) {
	return r.store.ByParent(ctx, parentID)
}

func (r *SubtaskRunner) Abandon(ctx context.Context, s *Subtask, owner, reason string) error {
	return r.store.Fail(ctx, s.ID, owner, errors.New(reason))
}

func (r *SubtaskRunner) Run(ctx context.Context, s *Subtask, owner string) error {
	if err := r.reserve(s.ID); err != nil {
		return err
	}
	defer r.release(s.ID)

	w := r.w
	logger := w.logger.With(applog.KeyTaskID, s.ParentID.String(),
		applog.KeySubtaskID, s.ID.String(), "step_kind", s.Kind)

	// (1) Build. An unknown kind or undecodable args can never run, so fail the
	// child rather than spin on it.
	step, err := w.registry.Build(s.Kind, s.Args)
	if err != nil {
		_ = r.store.Fail(ctx, s.ID, owner, err)
		return fmt.Errorf("build subtask %s (%s): %w", s.ID, s.Kind, err)
	}

	// (2) Lock. Nothing is held coming in: the parent's `fleet:endpoints` is its
	// own unit of work.
	lease, key, err := w.lockFor(ctx, logger, s, step, nil, "")
	if err != nil {
		// A cancelled context is a shutdown, not a failure: leave the row alone so
		// the reclaimed parent re-creates and re-runs it (§5.5).
		if ctx.Err() != nil {
			return fmt.Errorf("subtask %s (%s) interrupted: %w", s.ID, s.Kind, ctx.Err())
		}
		// Nor is an exhausted lock wait, which AcquireResource bounds on an inner
		// deadline and so reports with the caller's ctx still live. ErrLockHeld says
		// somebody else holds the endpoint, never that the work would fail — and
		// `failed` is terminal, so recording it would be permanent: a replaying
		// parent does not re-attempt a failed child. The row stays non-terminal and
		// the error still wraps ErrLockHeld, which the planner reports as
		// `unattempted` (§5.4).
		if !errors.Is(err, ErrLockHeld) {
			_ = r.store.Fail(ctx, s.ID, owner, err)
		}
		return fmt.Errorf("subtask %s (%s): %w", s.ID, s.Kind, err)
	}
	if lease != nil {
		// Released as soon as THIS child finishes, unlike a task's lock, which is held
		// until the task leaves execution because a later step may want it again.
		// Through releaseLease like every other release: detached from ctx so a
		// shutdown frees the key now rather than a TTL from now, but bounded, so a
		// wedged pool cannot park a finishing child inside Release and stall the roll
		// behind one of its max_parallel slots.
		defer func() {
			if err := releaseLease(ctx, lease); err != nil {
				logger.Warn("releasing resource lock", "lock", key, "error", err)
			}
		}()
	}

	// (3) Run, with the child-scoped logger on the context so a step's own lines
	// carry the parent and subtask ids.
	if err := r.store.Start(ctx, s.ID, owner); err != nil {
		return fmt.Errorf("mark subtask running: %w", err)
	}
	res := w.runStepWithRetry(applog.IntoContext(ctx, logger), logger, step)
	if res.Failed() {
		if ctx.Err() != nil {
			return fmt.Errorf("subtask %s (%s) interrupted: %w", s.ID, s.Kind, ctx.Err())
		}
		_ = r.store.Fail(ctx, s.ID, owner, res.Err)
		return fmt.Errorf("subtask %s (%s): %w", s.ID, s.Kind, res.Err)
	}
	if err := r.store.Complete(ctx, s.ID, owner, res.Output); err != nil {
		return fmt.Errorf("mark subtask succeeded: %w", err)
	}
	logger.Debug("subtask succeeded")
	return nil
}
