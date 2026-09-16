package task

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"

	"github.com/yugabyte/amp/controller/internal/db"
	"github.com/yugabyte/amp/controller/pkg/pglease"
)

// ErrResizeInProgress is returned by Create when a resize_endpoint task is
// planned for an endpoint that already has a non-terminal one. It is the
// first-writer-wins lock: the in-flight resize holds the endpoint until it
// reaches a terminal status, and a concurrent resize gets this error (which the
// API maps to 409 Conflict). Enforced atomically by the partial unique index
// tasks_one_active_resize_per_endpoint (PgStore) / a guarded check (MemStore).
var ErrResizeInProgress = errors.New("a resize is already in progress for this endpoint")

// ErrBranchExtensionsInProgress is the add_branch_extensions twin: the in-flight
// add holds the branch until it reaches a terminal status. It matters more than a
// duplicate add's own cost, because two concurrent tasks redeploy overlapping sets
// of the branch's endpoints — concurrent helm upgrades of the same release. The
// step skipping endpoints already mounting the set narrows that overlap but does
// not remove it: two adds of different names both leave every endpoint behind.
var ErrBranchExtensionsInProgress = errors.New("an extension add is already in progress for this branch")

// ErrTaskNotFound and ErrTaskTerminal are what RequestCancel — the one unfenced,
// API-originated write — reports when it matched no row. They are distinguished
// because they are different answers to the caller: nothing to act on (404)
// versus the work is already over (409).
var (
	ErrTaskNotFound = errors.New("task not found")
	ErrTaskTerminal = errors.New("task has already finished")
)

// taskLockViolation reports the typed conflict when inserting spec failed because
// that kind's first-writer-wins lock rejected it, or nil when the error is
// something else.
//
// It identifies the lock by SQLSTATE and the KIND being inserted, never by the
// constraint's name, because there is no portable field to read that name from:
// upstream PostgreSQL raises the unique violation with errtableconstraint(),
// which fills the wire protocol's `C` field that pgx exposes as ConstraintName,
// while YSQL builds the same error in pg_yb_utils.c and only formats the name
// into the message, leaving ConstraintName and Detail empty. Keying on that field
// is what made every concurrent resize a 500 instead of a 409 (#400), and keying
// on the message text instead would just trade a silent break for a brittle one.
//
// Classifying by kind stays exact with more than one lock because the indexes'
// predicates are kind-DISJOINT — each names one kind — so a unique violation from
// this INSERT can only be the index for the kind being inserted. The only other
// unique constraint on tasks is its primary key, whose conflict
// ON CONFLICT (id) DO NOTHING already absorbs. TestPgUniqueConstraintsOnTask
// holds that inventory and the kinds' declared lockIndex names in agreement
// against the live schema, in both directions.
func taskLockViolation(spec TaskSpec, err error) error {
	if spec.Kind.lockIndex == "" || spec.TargetRef == nil {
		return nil
	}
	var pgErr *pgconn.PgError
	if !errors.As(err, &pgErr) || pgErr.Code != "23505" { // 23505: unique_violation
		return nil
	}
	return spec.Kind.inProgress
}

// ycpResourceArg, ycpResourceIDArg and ycpGenerationArg render a task's optional
// YCP stamp as three nullable bind parameters. Nil target -> three NULLs, i.e. a
// task YCP did not drive.
func ycpResourceArg(y *YCPTarget) *string {
	if y == nil {
		return nil
	}
	return &y.Resource
}

func ycpResourceIDArg(y *YCPTarget) *string {
	if y == nil {
		return nil
	}
	id := y.ID.String()
	return &id
}

func ycpGenerationArg(y *YCPTarget) *int64 {
	if y == nil {
		return nil
	}
	return &y.Generation
}

// scanYCPTarget reassembles the stamp from its three columns. They are written
// together and only together, so a partially-set row is corruption rather than a
// state to tolerate.
func scanYCPTarget(resource, resourceID *string, generation *int64) (*YCPTarget, error) {
	if resource == nil && resourceID == nil && generation == nil {
		return nil, nil
	}
	if resource == nil || resourceID == nil || generation == nil {
		return nil, fmt.Errorf("task has a partial ycp stamp (resource=%v id=%v generation=%v)",
			resource, resourceID, generation)
	}
	id, err := uuid.Parse(*resourceID)
	if err != nil {
		return nil, fmt.Errorf("parse task ycp_resource_id: %w", err)
	}
	return &YCPTarget{Resource: *resource, ID: id, Generation: *generation}, nil
}

// Store persists tasks and their steps. Every method is written to be
// idempotent per the YB retry contract (AGENTS.md): absolute UPDATEs and
// INSERT ... ON CONFLICT, never read-modify-write.
type Store interface {
	// Create write-ahead-records a task and its steps, all pending, before any
	// step runs: it assigns the task id and a step id per step, writes the
	// task_step rows, and records the ordered step ids on the task. It returns the
	// persisted Task. This is the one entry point the CreateXTask builders (tasks.go)
	// use; callers never construct executable Steps.
	Create(ctx context.Context, spec TaskSpec) (*Task, error)

	// Candidates returns up to limit maybe-claimable tasks, oldest first: pending
	// ones, plus running ones whose lease nobody holds because the owner died.
	//
	// A HINT only — the acquire decides, so a candidate whose read raced a takeover
	// is refused and the caller moves on. Returning a set rather than one row is
	// what replaces FOR UPDATE SKIP LOCKED (architecture/ac-locking.md).
	Candidates(ctx context.Context, limit int) ([]*Task, error)

	// Claim transitions a task to running under owner, after the caller has taken
	// its lease. It returns nil when the task is no longer claimable — it reached
	// a terminal status or was deleted between the candidate read and now.
	//
	// Re-stamping owner is what fences out a previous owner's writes, so this is
	// also the reclaim path: a running task whose lease lapsed is claimed the same
	// way a pending one is.
	Claim(ctx context.Context, taskID uuid.UUID, owner string) (*Task, error)

	// Steps returns a task's step records in execution order (task.step_ids).
	Steps(ctx context.Context, taskID uuid.UUID) ([]StepRecord, error)

	// StartStep marks a step running. It is owner-fenced on the parent task: a
	// worker that has lost the lease (the task is now owned by someone else) does
	// not mutate it.
	StartStep(ctx context.Context, taskID, stepID uuid.UUID, owner string) error

	// CompleteStep marks a step succeeded and records its result. Owner-fenced on
	// the parent task (see StartStep).
	CompleteStep(ctx context.Context, taskID, stepID uuid.UUID, owner string, result json.RawMessage) error

	// FailStep marks a step and its task failed, recording the cause. Owner-fenced
	// so a worker that has lost the lease cannot durably fail a task another
	// instance now owns.
	FailStep(ctx context.Context, taskID, stepID uuid.UUID, owner string, cause error) error

	// CompleteTask marks a task succeeded. Owner-fenced so a worker that has lost
	// the lease cannot overwrite the state of a task another instance now owns.
	CompleteTask(ctx context.Context, taskID uuid.UUID, owner string) error

	// RequestCancel stamps cancel_requested_at on a non-terminal task, so the
	// worker stops it at its next step boundary. ErrTaskTerminal when the task has
	// already finished, ErrTaskNotFound when there is no such task; a repeat
	// request keeps the first timestamp.
	//
	// UNFENCED, unlike every worker-side write that takes a task terminal. The
	// fence is `AND owner = $currentOwner`, and it exists so a worker that lost its
	// lease writes nothing — but this arrives from an API caller who is not the
	// owner and never will be, and a task that is still `pending` has no owner at
	// all, so fencing it would make it a permanent no-op on exactly the cases that
	// matter. (`Claim` above is unfenced too, for its own reason: re-stamping
	// `owner` IS the reclaim.) What this must still refuse is a terminal task, and
	// it never writes `status`, `owner` or `error`: taking a task terminal stays
	// the fenced worker's write.
	RequestCancel(ctx context.Context, id uuid.UUID) error

	// Get returns a task by id, or nil if it does not exist.
	Get(ctx context.Context, id uuid.UUID) (*Task, error)

	// List returns a page of tasks matching the filter, ordered by created_at
	// DESC (newest first), plus the total number of matching tasks (ignoring the
	// page window). A zero-value TaskFilter is unfiltered. Setting
	// TaskFilter.UpdatedSince orders updated_at ASC instead -- see that field.
	List(ctx context.Context, f TaskFilter, limit, offset int) ([]*Task, int, error)

	// ActiveByKind returns the oldest non-terminal task of kind — pending or
	// running — or nil when there is none.
	//
	// It is the plan-time half of "one fleet roll at a time"
	// (ac-endpoint-upgrade.md §5.6): a fast refusal in front of the fleet lock,
	// never the authority, since the read and the insert are two statements and a
	// roll can be planned between them. A QUERY over `tasks` rather than a probe
	// of the `fleet:endpoints` lease, deliberately — a roll that is still
	// `pending` has not been claimed, so it holds no lease at all and a probe
	// would miss it entirely.
	ActiveByKind(ctx context.Context, kind string) (*Task, error)
}

// TaskFilter narrows a List query. Zero-value fields are unfiltered: an empty
// Status or Kind matches any, and a nil ClusterID matches any cluster (including
// cluster-less tasks).
type TaskFilter struct {
	Status    Status     // "" => any
	Kind      string     // "" => any
	ClusterID *uuid.UUID // nil => any

	// UpdatedSince turns the query into a change feed: only tasks whose
	// updated_at is at or after it, ordered updated_at ASC instead of the default
	// created_at DESC. nil => any, with the default ordering.
	//
	// The ordering has to change with the filter, which is why this is one field
	// and not two. `updated_at` is written on every status transition, so it is
	// the only column an incremental reader can page by: ordered created_at DESC,
	// a task created an hour ago that just reached a terminal state sits below
	// rows the reader has already consumed, so a watermark over that ordering
	// skips it. Ascending, so paging forward through a backlog cannot skip a row
	// as newer ones land at the far end.
	//
	// Inclusive (>=, not >) because two tasks can share a timestamp: an exclusive
	// bound set to the last row's updated_at would drop its same-instant twin.
	// Callers therefore re-read the boundary rows and must tolerate the repeat.
	UpdatedSince *time.Time
}

// PgStore is the YBDB-backed Store.
type PgStore struct {
	pool *db.Pool
}

// NewPgStore returns a Store backed by pool.
func NewPgStore(pool *db.Pool) *PgStore {
	return &PgStore{pool: pool}
}

// Compile-time assertion that PgStore satisfies the Store interface (MemStore is
// the in-memory implementation used by unit tests; see mem.go).
var _ Store = (*PgStore)(nil)

// tasks.lease_expiry is absent: ownership is a lease in amp_registry.resource_token
// (architecture/ac-locking.md), so the column is DEPRECATED — read and written by
// nothing. Left in the schema rather than dropped, because a migration would make
// every fresh install create it only to lose it.
// Cleanup: https://github.com/yugabyte/meko/issues/975
const taskColumns = `id::text, kind, status::text, cluster_id::text, target_ref,
	params, step_ids::text[], error, owner, ycp_resource, ycp_resource_id::text, ycp_generation,
	created_at, updated_at, cancel_requested_at`

func (s *PgStore) Create(ctx context.Context, spec TaskSpec) (*Task, error) {
	taskID := spec.ID
	if taskID == uuid.Nil {
		taskID = uuid.New()
	}
	params := spec.Params
	if len(params) == 0 {
		params = json.RawMessage("{}")
	}
	var clusterArg any
	if spec.ClusterID != nil {
		clusterArg = spec.ClusterID.String()
	}

	// Assign a stable id per step up front; their order is the task's step_ids.
	stepIDs := make([]string, len(spec.Steps))
	for i := range spec.Steps {
		stepIDs[i] = uuid.New().String()
	}

	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return nil, err
	}
	defer tx.Rollback(ctx)

	// The task first (the steps' task_id FK points at it), carrying the ordered
	// step ids; then the steps themselves.
	tag, err := tx.Exec(ctx,
		`INSERT INTO tasks (id, kind, status, cluster_id, target_ref, params, step_ids,
		                    ycp_resource, ycp_resource_id, ycp_generation)
		 VALUES ($1::uuid, $2, 'pending', $3::uuid, $4, $5::jsonb, $6::uuid[], $7, $8::uuid, $9)
		 ON CONFLICT (id) DO NOTHING`,
		taskID.String(), spec.Kind.name, clusterArg, spec.TargetRef, []byte(params), stepIDs,
		ycpResourceArg(spec.YCP), ycpResourceIDArg(spec.YCP), ycpGenerationArg(spec.YCP))
	if err != nil {
		// A first-writer-wins lock: a second non-terminal task of this kind for the
		// same target_ref violates the kind's partial unique index, which we surface
		// as a typed conflict for the API to turn into a 409.
		if lockErr := taskLockViolation(spec, err); lockErr != nil {
			return nil, lockErr
		}
		return nil, fmt.Errorf("insert task: %w", err)
	}
	// A replay of Create with a preassigned id returns the original durable plan;
	// it must not add unreferenced steps under freshly generated ids.
	if tag.RowsAffected() == 0 {
		if err := tx.Commit(ctx); err != nil {
			return nil, err
		}
		return s.Get(ctx, taskID)
	}

	for i, sp := range spec.Steps {
		args := sp.Args
		if len(args) == 0 {
			args = json.RawMessage("{}")
		}
		if _, err := tx.Exec(ctx,
			`INSERT INTO task_steps (id, task_id, kind, status, args)
			 VALUES ($1::uuid, $2::uuid, $3, 'pending', $4::jsonb)
			 ON CONFLICT (id) DO NOTHING`,
			stepIDs[i], taskID.String(), sp.Kind, []byte(args)); err != nil {
			return nil, fmt.Errorf("insert step %d (%s): %w", i, sp.Kind, err)
		}
	}

	// Past the commit the plan is durable, so a failure reading it back must not
	// look replayable to the API: the caller would plan the same work twice under
	// a fresh task id. The commit itself is the same case — it may have landed.
	if err := tx.Commit(ctx); err != nil {
		return nil, db.AfterWrite(err)
	}
	t, err := s.Get(ctx, taskID)
	if err != nil {
		return nil, db.AfterWrite(err)
	}
	return t, nil
}

// Candidates asks token_holder whether a running task's lease is live, and only for
// running rows — pending is the common path. "No row" already means free to a new
// owner, so this reuses migration 0006's definition instead of restating it.
//
// The skew must match the one PgLocks acquires under, or the two disagree about when
// a lapsed lease is takeable; both take pglease's default.
func (s *PgStore) Candidates(ctx context.Context, limit int) ([]*Task, error) {
	rows, err := s.pool.Query(ctx,
		`SELECT `+taskColumns+`
		 FROM tasks
		 WHERE status = 'pending'
		    OR (status = 'running'
		        AND NOT EXISTS (
		            SELECT 1 FROM amp_registry.token_holder('`+taskKeyPrefix+`' || id::text, $1)))
		 ORDER BY created_at
		 LIMIT $2`,
		pglease.DefaultSkew.Milliseconds(), limit)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	var out []*Task
	for rows.Next() {
		t, err := scanTask(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, t)
	}
	return out, rows.Err()
}

func (s *PgStore) Claim(ctx context.Context, taskID uuid.UUID, owner string) (*Task, error) {
	row := s.pool.QueryRow(ctx,
		`UPDATE tasks
		 SET status = 'running', owner = $2, updated_at = now()
		 WHERE id = $1::uuid AND status IN ('pending', 'running')
		 RETURNING `+taskColumns,
		taskID.String(), owner)
	t, err := scanTask(row)
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, nil
	}
	return t, err
}

func (s *PgStore) Steps(ctx context.Context, taskID uuid.UUID) ([]StepRecord, error) {
	// Order by each step's position in the task's step_ids list (execution order).
	rows, err := s.pool.Query(ctx,
		`SELECT st.id::text, st.task_id::text, st.kind, st.status::text, st.args, st.result, st.error, st.updated_at
		 FROM task_steps st
		 JOIN tasks t ON t.id = st.task_id
		 WHERE st.task_id = $1::uuid
		 ORDER BY array_position(t.step_ids, st.id)`,
		taskID.String())
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	var steps []StepRecord
	for rows.Next() {
		var (
			idStr     string
			tidStr    string
			st        StepRecord
			args      []byte
			result    []byte
			statusStr string
		)
		if err := rows.Scan(&idStr, &tidStr, &st.Kind, &statusStr, &args, &result, &st.Error, &st.UpdatedAt); err != nil {
			return nil, err
		}
		if st.ID, err = uuid.Parse(idStr); err != nil {
			return nil, fmt.Errorf("parse step id: %w", err)
		}
		if st.TaskID, err = uuid.Parse(tidStr); err != nil {
			return nil, fmt.Errorf("parse task id: %w", err)
		}
		st.Status = Status(statusStr)
		st.Args = json.RawMessage(args)
		if result != nil {
			st.Result = json.RawMessage(result)
		}
		steps = append(steps, st)
	}
	return steps, rows.Err()
}

func (s *PgStore) StartStep(ctx context.Context, taskID, stepID uuid.UUID, owner string) error {
	_, err := s.pool.Exec(ctx,
		`UPDATE task_steps SET status = 'running', updated_at = now()
		 WHERE id = $1::uuid
		   AND EXISTS (SELECT 1 FROM tasks WHERE id = $2::uuid AND owner = $3)`,
		stepID.String(), taskID.String(), owner)
	return err
}

func (s *PgStore) CompleteStep(ctx context.Context, taskID, stepID uuid.UUID, owner string, result json.RawMessage) error {
	var arg any
	if len(result) > 0 {
		arg = []byte(result)
	}
	_, err := s.pool.Exec(ctx,
		`UPDATE task_steps SET status = 'succeeded', result = $2::jsonb, error = NULL, updated_at = now()
		 WHERE id = $1::uuid
		   AND EXISTS (SELECT 1 FROM tasks WHERE id = $3::uuid AND owner = $4)`,
		stepID.String(), arg, taskID.String(), owner)
	return err
}

func (s *PgStore) FailStep(ctx context.Context, taskID, stepID uuid.UUID, owner string, cause error) error {
	msg := cause.Error()
	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return err
	}
	defer tx.Rollback(ctx)

	if _, err := tx.Exec(ctx,
		`UPDATE task_steps SET status = 'failed', error = $2, updated_at = now()
		 WHERE id = $1::uuid
		   AND EXISTS (SELECT 1 FROM tasks WHERE id = $3::uuid AND owner = $4)`,
		stepID.String(), msg, taskID.String(), owner); err != nil {
		return err
	}
	if _, err := tx.Exec(ctx,
		`UPDATE tasks SET status = 'failed', error = $2, updated_at = now()
		 WHERE id = $1::uuid AND owner = $3`,
		taskID.String(), msg, owner); err != nil {
		return err
	}
	return tx.Commit(ctx)
}

func (s *PgStore) CompleteTask(ctx context.Context, taskID uuid.UUID, owner string) error {
	_, err := s.pool.Exec(ctx,
		`UPDATE tasks SET status = 'succeeded', error = NULL, updated_at = now()
		 WHERE id = $1::uuid AND owner = $2`,
		taskID.String(), owner)
	return err
}

func (s *PgStore) RequestCancel(ctx context.Context, id uuid.UUID) error {
	// COALESCE, not a plain assignment: the stamp records when the FIRST request
	// landed, so re-issuing a cancel while the worker is still unwinding does not
	// keep moving it forward.
	tag, err := s.pool.Exec(ctx,
		`UPDATE tasks SET cancel_requested_at = COALESCE(cancel_requested_at, now()), updated_at = now()
		 WHERE id = $1::uuid AND status IN ('pending', 'running')`,
		id.String())
	if err != nil {
		return fmt.Errorf("request task cancel: %w", err)
	}
	if tag.RowsAffected() == 0 {
		return s.whyNoRow(ctx, id)
	}
	return nil
}

// whyNoRow separates the two reasons a status-guarded write matched nothing.
// Read after the write, never before it: the write is the decision, and a
// pre-check would just be a wider window for the task to go terminal in.
func (s *PgStore) whyNoRow(ctx context.Context, id uuid.UUID) error {
	t, err := s.Get(ctx, id)
	if err != nil {
		return err
	}
	if t == nil {
		return fmt.Errorf("%w: %s", ErrTaskNotFound, id)
	}
	return fmt.Errorf("%w: %s is %s", ErrTaskTerminal, id, t.Status)
}

func (s *PgStore) Get(ctx context.Context, id uuid.UUID) (*Task, error) {
	row := s.pool.QueryRow(ctx, `SELECT `+taskColumns+` FROM tasks WHERE id = $1::uuid`, id.String())
	t, err := scanTask(row)
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, nil
	}
	return t, err
}

// List returns a page of tasks matching f, ordered created_at DESC -- or
// updated_at ASC when f.UpdatedSince is set -- plus the total matching count.
// Filters are applied via optional, positionally-numbered predicates so an unset
// field widens the result rather than excluding rows.
func (s *PgStore) List(ctx context.Context, f TaskFilter, limit, offset int) ([]*Task, int, error) {
	var (
		where string
		args  []any
	)
	add := func(pred string, arg any) {
		args = append(args, arg)
		clause := fmt.Sprintf(pred, len(args))
		if where == "" {
			where = " WHERE " + clause
		} else {
			where += " AND " + clause
		}
	}
	if f.Status != "" {
		add("status = $%d::task_status", string(f.Status))
	}
	if f.Kind != "" {
		add("kind = $%d", f.Kind)
	}
	if f.ClusterID != nil {
		add("cluster_id = $%d::uuid", f.ClusterID.String())
	}
	orderBy := "created_at DESC, id DESC"
	if f.UpdatedSince != nil {
		add("updated_at >= $%d", *f.UpdatedSince)
		orderBy = "updated_at ASC, id ASC"
	}

	var total int
	if err := s.pool.QueryRow(ctx, `SELECT count(*) FROM tasks`+where, args...).Scan(&total); err != nil {
		return nil, 0, fmt.Errorf("count tasks: %w", err)
	}

	// The page args follow the filter args positionally.
	limitArg := len(args) + 1
	offsetArg := len(args) + 2
	args = append(args, limit, offset)
	rows, err := s.pool.Query(ctx,
		fmt.Sprintf(`SELECT %s FROM tasks%s ORDER BY %s LIMIT $%d OFFSET $%d`,
			taskColumns, where, orderBy, limitArg, offsetArg),
		args...)
	if err != nil {
		return nil, 0, fmt.Errorf("list tasks: %w", err)
	}
	defer rows.Close()

	var tasks []*Task
	for rows.Next() {
		t, err := scanTask(rows)
		if err != nil {
			return nil, 0, err
		}
		tasks = append(tasks, t)
	}
	if err := rows.Err(); err != nil {
		return nil, 0, fmt.Errorf("iterate tasks: %w", err)
	}
	return tasks, total, nil
}

func (s *PgStore) ActiveByKind(ctx context.Context, kind string) (*Task, error) {
	t, err := scanTask(s.pool.QueryRow(ctx,
		`SELECT `+taskColumns+`
		 FROM tasks
		 WHERE kind = $1 AND status IN ('pending', 'running')
		 ORDER BY created_at
		 LIMIT 1`, kind))
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, nil
	}
	if err != nil {
		return nil, fmt.Errorf("active %s task: %w", kind, err)
	}
	return t, nil
}

// row is satisfied by both pgx.Row and a single pgx.Rows.
type row interface {
	Scan(dest ...any) error
}

// scanTask scans the taskColumns projection (uuid columns cast to text).
func scanTask(r row) (*Task, error) {
	var (
		idStr      string
		statusStr  string
		clusterStr *string
		params     []byte
		stepIDStrs []string
		ycpRes     *string
		ycpResID   *string
		ycpGen     *int64
		t          Task
	)
	if err := r.Scan(&idStr, &t.Kind, &statusStr, &clusterStr, &t.TargetRef,
		&params, &stepIDStrs, &t.Error, &t.Owner, &ycpRes, &ycpResID, &ycpGen,
		&t.CreatedAt, &t.UpdatedAt, &t.CancelRequestedAt); err != nil {
		return nil, err
	}
	ycp, err := scanYCPTarget(ycpRes, ycpResID, ycpGen)
	if err != nil {
		return nil, err
	}
	t.YCP = ycp
	id, err := uuid.Parse(idStr)
	if err != nil {
		return nil, fmt.Errorf("parse task id: %w", err)
	}
	t.ID = id
	t.Status = Status(statusStr)
	t.Params = json.RawMessage(params)
	for _, sid := range stepIDStrs {
		stepID, err := uuid.Parse(sid)
		if err != nil {
			return nil, fmt.Errorf("parse step id: %w", err)
		}
		t.StepIDs = append(t.StepIDs, stepID)
	}
	if clusterStr != nil {
		pid, err := uuid.Parse(*clusterStr)
		if err != nil {
			return nil, fmt.Errorf("parse cluster id: %w", err)
		}
		t.ClusterID = &pid
	}
	return &t, nil
}
