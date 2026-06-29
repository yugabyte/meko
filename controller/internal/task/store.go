package task

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"

	"github.com/yugabyte/yb-amp/controller/internal/db"
)

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

	// ClaimNext atomically transitions one runnable task pending->running under
	// a lease, returning nil if none is runnable. Tasks whose lease has expired
	// are reclaimable (§8.2).
	ClaimNext(ctx context.Context, owner string, lease time.Duration) (*Task, error)

	// Steps returns a task's step records in execution order (task.step_ids).
	Steps(ctx context.Context, taskID uuid.UUID) ([]StepRecord, error)

	// StartStep marks a step running.
	StartStep(ctx context.Context, stepID uuid.UUID) error

	// CompleteStep marks a step succeeded and records its result.
	CompleteStep(ctx context.Context, stepID uuid.UUID, result json.RawMessage) error

	// FailStep marks a step and its task failed, recording the cause.
	FailStep(ctx context.Context, taskID, stepID uuid.UUID, cause error) error

	// CompleteTask marks a task succeeded.
	CompleteTask(ctx context.Context, taskID uuid.UUID) error

	// Get returns a task by id, or nil if it does not exist.
	Get(ctx context.Context, id uuid.UUID) (*Task, error)
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

const taskColumns = `id::text, kind, status::text, project_id::text, target_ref,
	params, step_ids::text[], error, owner, lease_expiry, created_at, updated_at`

func (s *PgStore) Create(ctx context.Context, spec TaskSpec) (*Task, error) {
	taskID := uuid.New()
	params := spec.Params
	if len(params) == 0 {
		params = json.RawMessage("{}")
	}
	var projectArg any
	if spec.ProjectID != nil {
		projectArg = spec.ProjectID.String()
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
	if _, err := tx.Exec(ctx,
		`INSERT INTO task (id, kind, status, project_id, target_ref, params, step_ids)
		 VALUES ($1::uuid, $2, 'pending', $3::uuid, $4, $5::jsonb, $6::uuid[])
		 ON CONFLICT (id) DO NOTHING`,
		taskID.String(), spec.Kind, projectArg, spec.TargetRef, []byte(params), stepIDs); err != nil {
		return nil, fmt.Errorf("insert task: %w", err)
	}

	for i, sp := range spec.Steps {
		args := sp.Args
		if len(args) == 0 {
			args = json.RawMessage("{}")
		}
		if _, err := tx.Exec(ctx,
			`INSERT INTO task_step (id, task_id, kind, status, args)
			 VALUES ($1::uuid, $2::uuid, $3, 'pending', $4::jsonb)
			 ON CONFLICT (id) DO NOTHING`,
			stepIDs[i], taskID.String(), sp.Kind, []byte(args)); err != nil {
			return nil, fmt.Errorf("insert step %d (%s): %w", i, sp.Kind, err)
		}
	}

	if err := tx.Commit(ctx); err != nil {
		return nil, err
	}
	return s.Get(ctx, taskID)
}

func (s *PgStore) ClaimNext(ctx context.Context, owner string, lease time.Duration) (*Task, error) {
	row := s.pool.QueryRow(ctx,
		`UPDATE task
		 SET status = 'running', owner = $1,
		     lease_expiry = now() + make_interval(secs => $2),
		     updated_at = now()
		 WHERE id = (
		     SELECT id FROM task
		     WHERE status = 'pending'
		        OR (status = 'running' AND lease_expiry < now())
		     ORDER BY created_at
		     FOR UPDATE SKIP LOCKED
		     LIMIT 1
		 )
		 RETURNING `+taskColumns,
		owner, lease.Seconds())
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
		 FROM task_step st
		 JOIN task t ON t.id = st.task_id
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

func (s *PgStore) StartStep(ctx context.Context, stepID uuid.UUID) error {
	_, err := s.pool.Exec(ctx,
		`UPDATE task_step SET status = 'running', updated_at = now()
		 WHERE id = $1::uuid`,
		stepID.String())
	return err
}

func (s *PgStore) CompleteStep(ctx context.Context, stepID uuid.UUID, result json.RawMessage) error {
	var arg any
	if len(result) > 0 {
		arg = []byte(result)
	}
	_, err := s.pool.Exec(ctx,
		`UPDATE task_step SET status = 'succeeded', result = $2::jsonb, error = NULL, updated_at = now()
		 WHERE id = $1::uuid`,
		stepID.String(), arg)
	return err
}

func (s *PgStore) FailStep(ctx context.Context, taskID, stepID uuid.UUID, cause error) error {
	msg := cause.Error()
	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return err
	}
	defer tx.Rollback(ctx)

	if _, err := tx.Exec(ctx,
		`UPDATE task_step SET status = 'failed', error = $2, updated_at = now()
		 WHERE id = $1::uuid`,
		stepID.String(), msg); err != nil {
		return err
	}
	if _, err := tx.Exec(ctx,
		`UPDATE task SET status = 'failed', error = $2, updated_at = now()
		 WHERE id = $1::uuid`,
		taskID.String(), msg); err != nil {
		return err
	}
	return tx.Commit(ctx)
}

func (s *PgStore) CompleteTask(ctx context.Context, taskID uuid.UUID) error {
	_, err := s.pool.Exec(ctx,
		`UPDATE task SET status = 'succeeded', error = NULL, updated_at = now()
		 WHERE id = $1::uuid`,
		taskID.String())
	return err
}

func (s *PgStore) Get(ctx context.Context, id uuid.UUID) (*Task, error) {
	row := s.pool.QueryRow(ctx, `SELECT `+taskColumns+` FROM task WHERE id = $1::uuid`, id.String())
	t, err := scanTask(row)
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, nil
	}
	return t, err
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
		projectStr *string
		params     []byte
		stepIDStrs []string
		t          Task
	)
	if err := r.Scan(&idStr, &t.Kind, &statusStr, &projectStr, &t.TargetRef,
		&params, &stepIDStrs, &t.Error, &t.Owner, &t.LeaseExpiry, &t.CreatedAt, &t.UpdatedAt); err != nil {
		return nil, err
	}
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
	if projectStr != nil {
		pid, err := uuid.Parse(*projectStr)
		if err != nil {
			return nil, fmt.Errorf("parse project id: %w", err)
		}
		t.ProjectID = &pid
	}
	return &t, nil
}
