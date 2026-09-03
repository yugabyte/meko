package task

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"time"

	"github.com/google/uuid"

	"github.com/yugabyte/amp/controller/internal/db"
	"github.com/yugabyte/amp/controller/pkg/pglease"
)

// Two leases from pkg/pglease over amp_registry.resource_token (migration 0006):
// `task:<id>` held by the process, `endpoint:<id>` held by the task. Both namespaces
// are disjoint from the compute write fence's `branch:<id>` in the same table.
// Design: architecture/ac-locking.md.
const (
	taskKeyPrefix = "task:"

	// ResourceEndpoint's id is the endpoint id, spelled as tasks.target_ref does.
	ResourceEndpoint = "endpoint"
)

// ErrLockHeld means somebody else holds the lock. Not a failure: the claim loop
// treats it as "not my work".
var ErrLockHeld = errors.New("task: lock held by another owner")

// ResourceLock names the thing a step needs exclusively.
type ResourceLock struct {
	Type string
	ID   string
}

// Key is the lease key this lock takes.
func (r ResourceLock) Key() string { return r.Type + ":" + r.ID }

func (r ResourceLock) String() string { return r.Key() }

// Zero reports the lock unset, which is what a step needing nothing returns.
func (r ResourceLock) Zero() bool { return r.Type == "" || r.ID == "" }

// EndpointLock is the lock on one compute endpoint.
//
// An unset id yields the ZERO lock, not a lock on the zero uuid. uuid.Nil.String()
// is 36 characters, not "", so without this a step whose args omit endpoint_id
// would take `endpoint:00000000-...` and serialize against every other such step.
// Taking no lock is the lesser wrong: nothing plans such a step, and one that
// exists fails in its own Execute rather than becoming everyone else's problem.
func EndpointLock(endpointID string) ResourceLock {
	if endpointID == "" || endpointID == uuid.Nil.String() {
		return ResourceLock{}
	}
	return ResourceLock{Type: ResourceEndpoint, ID: endpointID}
}

// Locking is implemented by steps needing a resource exclusively. The worker takes
// the lock before the step and holds it until the task leaves execution.
//
// A task may hold at most ONE: deadlock needs two, so a task whose steps declare
// two different locks fails instead.
//
// It excludes distinct TASKS, not two executions of one, so steps must stay
// idempotent. Re-acquiring is free only within one process — resourceOwner names
// the process too, so a task reclaimed elsewhere waits the lapsed row out. Harmless:
// that row lapses alongside the task lease gating it.
type Locking interface {
	Lock() ResourceLock
}

// Lease is one held lock, to be given back when the work under it is done.
//
// No way to observe it ending, deliberately: losing a lease exits the process, so
// recovery code could never run, and exposing pglease's cancellation would only
// invite someone to write it.
type Lease interface {
	Release(ctx context.Context) error
}

// Locks hands out the leases the worker takes. PgLocks is the real one; MemLocks
// is the in-memory model the unit tests run against.
type Locks interface {
	// Owner identifies this process, and lands in tasks.owner so the fence on every
	// task and step write names what the lease does.
	Owner() string

	// AcquireTask takes this process's lease on a task, reporting ErrLockHeld when
	// anyone already holds it. The claim loop then tries the next candidate rather
	// than waiting: a held task lock means the work is already being done.
	AcquireTask(ctx context.Context, taskID uuid.UUID) (Lease, error)

	// AcquireResource waits for the lease on r, held in the name of the unit of work
	// asking for it rather than this process's, and reports ErrLockHeld when it
	// gives up — on ctx, or on the implementation's own bound (PgLocks:
	// --task-lock-wait; MemLocks: its own). The bound is not part of the contract;
	// pass a ctx deadline to pick one.
	//
	// holderID/kind name that unit: a task and its kind, or a SUBTASK and its step
	// kind (ac-endpoint-upgrade.md §5.3). See resourceOwner.
	AcquireResource(ctx context.Context, r ResourceLock, holderID uuid.UUID, kind string) (Lease, error)
}

var _ Lease = (*pglease.Held)(nil)

// PgLocks is the Locks backed by pkg/pglease. It keeps pglease's default
// AbortProcess, so losing a lease exits the process — by then the database has been
// unreachable long enough that the other in-flight tasks are stuck too
// (architecture/ac-locking.md).
type PgLocks struct {
	store      *pglease.Store
	taskClient *pglease.Client
	owner      string
	cfg        pglease.Config
	wait       time.Duration
}

var _ Locks = (*PgLocks)(nil)

// NewPgLocks builds the lease client over the controller's own pool. wait bounds
// how long a task blocks on a contended resource lock.
//
// Pass a logger: pglease defaults to discarding, and the record it drops is the
// failed-renewal warning that says the database went flaky before a lease was lost.
// The abort writes to stderr directly and does not need it.
func NewPgLocks(pool *db.Pool, logger *slog.Logger, ttl, renewEvery, wait time.Duration) (*PgLocks, error) {
	// Default skew: PgStore.Candidates assumes the same one when it asks
	// token_holder whether a lease is still live, and the two have to agree.
	store, err := pglease.NewStore(pool.Pool, pglease.StoreConfig{})
	if err != nil {
		return nil, fmt.Errorf("task locks: %w", err)
	}
	cfg := leaseConfig(logger, ttl, renewEvery)
	owner := ownerID()
	client, err := pglease.New(store, owner, cfg)
	if err != nil {
		return nil, fmt.Errorf("task locks: %w", err)
	}
	return &PgLocks{store: store, taskClient: client, owner: owner, cfg: cfg, wait: wait}, nil
}

// leaseConfig is the lease timing both key spaces are held under.
//
// Abort is left UNSET on purpose, so pglease substitutes AbortProcess and losing a
// lease exits the process. That omission is the whole fail-fast design, which is
// why it is decided here in one place and pinned by a test rather than being an
// unremarked gap in a struct literal.
func leaseConfig(logger *slog.Logger, ttl, renewEvery time.Duration) pglease.Config {
	return pglease.Config{
		TTL:          ttl,
		RenewEvery:   renewEvery,
		RenewTimeout: pglease.DefaultRenewTimeout,
		SafetyMargin: pglease.DefaultSafetyMargin,
		Logger:       logger,
	}
}

func (l *PgLocks) Owner() string { return l.owner }

func (l *PgLocks) AcquireTask(ctx context.Context, taskID uuid.UUID) (Lease, error) {
	h, err := l.taskClient.Acquire(ctx, taskLockKey(taskID))
	if err != nil {
		return nil, classifyAcquire(err)
	}
	return h, nil
}

func (l *PgLocks) AcquireResource(ctx context.Context, r ResourceLock, holderID uuid.UUID, kind string) (Lease, error) {
	// A client per acquire, because the owner names the unit of work: pglease grants
	// unconditionally to the current holder, so two of them sharing this process's
	// owner id would both be granted and the database would stop deciding. Cheap —
	// a client starts no goroutine and owns no pool; the renewal belongs to the Held.
	c, err := pglease.New(l.store, resourceOwner(holderID, kind, l.owner), l.cfg)
	if err != nil {
		return nil, fmt.Errorf("resource lock %s: %w", r, err)
	}
	waitCtx, cancel := context.WithTimeout(ctx, l.wait)
	defer cancel()
	h, err := c.AcquireWait(waitCtx, r.Key())
	if err != nil {
		return nil, fmt.Errorf("resource lock %s: %w", r, classifyAcquire(err))
	}
	return h, nil
}

// Holder reports who holds a resource lock, or "" when free. For reporting a wait,
// never deciding one: it is a read, and the acquire decides.
func (l *PgLocks) Holder(ctx context.Context, r ResourceLock) (string, error) {
	lease, err := l.store.Holder(ctx, r.Key())
	if err != nil || lease == nil {
		return "", err
	}
	return lease.Owner, nil
}

// classifyAcquire folds pglease's two "somebody has it" answers into ErrLockHeld.
func classifyAcquire(err error) error {
	if errors.Is(err, pglease.ErrNotAcquired) || errors.Is(err, pglease.ErrAlreadyHeld) {
		return fmt.Errorf("%w: %w", ErrLockHeld, err)
	}
	return err
}

func taskLockKey(taskID uuid.UUID) string { return taskKeyPrefix + taskID.String() }

// resourceOwner is the owner string a resource lease is held under. It names the
// unit of work holding it, because that is the answer "what is holding endpoint
// X?" wants and resource_token has no metadata column to put it in otherwise; the
// process is carried too, so the row also says which instance to go and look at.
//
// `task=` is where to look it up for a step of a task. A subtask's id goes in the
// same field (ac-endpoint-upgrade.md §5.3) and is a row in `subtasks`, not
// `tasks` — the parent's step result is what names it.
//
// Naming the process is what stops a cross-process reclaim renewing the row rather
// than taking it over — see Locking. Kept anyway: the diagnostic is worth more than
// a takeover wait already bounded by the task lease.
func resourceOwner(holderID uuid.UUID, kind, procOwner string) string {
	return fmt.Sprintf("task=%s kind=%s proc=%s", holderID, kind, procOwner)
}

// The steps that mutate one endpoint's compute declare their lock together here,
// so what serializes against what is answerable from one place.
//
// STILL UNPROTECTED, and not an oversight: delete_branch, delete_cluster and
// add_branch_extensions each touch EVERY endpoint under a branch or cluster —
// add_branch_extensions through the same deployer create and resize use, so it can
// helm-upgrade a release a locked task is mid-way through. None can take one
// endpoint lock when they need all of them; that wants intent locks.
// https://github.com/yugabyte/meko/issues/976
func (s *createEndpointStep) Lock() ResourceLock  { return EndpointLock(s.EndpointID.String()) }
func (s *resizeEndpointStep) Lock() ResourceLock  { return EndpointLock(s.EndpointID.String()) }
func (s *upgradeEndpointStep) Lock() ResourceLock { return EndpointLock(s.EndpointID.String()) }
func (s *deleteEndpointStep) Lock() ResourceLock  { return EndpointLock(s.EndpointID.String()) }

var (
	_ Locking = (*createEndpointStep)(nil)
	_ Locking = (*resizeEndpointStep)(nil)
	_ Locking = (*upgradeEndpointStep)(nil)
	_ Locking = (*deleteEndpointStep)(nil)
)
