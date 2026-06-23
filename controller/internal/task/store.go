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
	// Plan write-ahead-records a task and its ordered steps, all pending,
	// before any step runs.
	Plan(ctx context.Context, kind string, params json.RawMessage, steps []StepPlan) (*Task, error)

	// ClaimNext atomically transitions one runnable task pending->running under
	// a lease, returning nil if none is runnable. Tasks whose lease has expired
	// are reclaimable (§8.2).
	ClaimNext(ctx context.Context, owner string, lease time.Duration) (*Task, error)

	// Steps returns a task's steps ordered by ordinal.
	Steps(ctx context.Context, taskID uuid.UUID) ([]Step, error)

	// StartStep marks a step running.
	StartStep(ctx context.Context, taskID uuid.UUID, ordinal int) error

	// CompleteStep marks a step succeeded and records its result.
	CompleteStep(ctx context.Context, taskID uuid.UUID, ordinal int, result json.RawMessage) error

	// FailStep marks a step and its task failed, recording the cause.
	FailStep(ctx context.Context, taskID uuid.UUID, ordinal int, cause error) error

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

const taskColumns = `id::text, kind, status::text, project_id::text, target_ref,
	params, error, owner, lease_expiry, created_at, updated_at`

func (s *PgStore) Plan(ctx context.Context, kind string, params json.RawMessage, steps []StepPlan) (*Task, error) {
	id := uuid.New()
	if len(params) == 0 {
		params = json.RawMessage("{}")
	}

	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return nil, err
	}
	defer tx.Rollback(ctx)

	if _, err := tx.Exec(ctx,
		`INSERT INTO task (id, kind, status, params)
		 VALUES ($1::uuid, $2, 'pending', $3::jsonb)
		 ON CONFLICT (id) DO NOTHING`,
		id.String(), kind, []byte(params)); err != nil {
		return nil, fmt.Errorf("insert task: %w", err)
	}

	for i, sp := range steps {
		args := sp.Args
		if len(args) == 0 {
			args = json.RawMessage("{}")
		}
		if _, err := tx.Exec(ctx,
			`INSERT INTO task_step (task_id, ordinal, kind, status, args)
			 VALUES ($1::uuid, $2, $3, 'pending', $4::jsonb)
			 ON CONFLICT (task_id, ordinal) DO NOTHING`,
			id.String(), i, sp.Kind, []byte(args)); err != nil {
			return nil, fmt.Errorf("insert step %d: %w", i, err)
		}
	}

	if err := tx.Commit(ctx); err != nil {
		return nil, err
	}
	return s.Get(ctx, id)
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

func (s *PgStore) Steps(ctx context.Context, taskID uuid.UUID) ([]Step, error) {
	rows, err := s.pool.Query(ctx,
		`SELECT task_id::text, ordinal, kind, status::text, args, result, error, updated_at
		 FROM task_step WHERE task_id = $1::uuid ORDER BY ordinal`,
		taskID.String())
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	var steps []Step
	for rows.Next() {
		var (
			tidStr    string
			st        Step
			args      []byte
			result    []byte
			statusStr string
		)
		if err := rows.Scan(&tidStr, &st.Ordinal, &st.Kind, &statusStr, &args, &result, &st.Error, &st.UpdatedAt); err != nil {
			return nil, err
		}
		tid, err := uuid.Parse(tidStr)
		if err != nil {
			return nil, fmt.Errorf("parse task id: %w", err)
		}
		st.TaskID = tid
		st.Status = Status(statusStr)
		st.Args = json.RawMessage(args)
		if result != nil {
			st.Result = json.RawMessage(result)
		}
		steps = append(steps, st)
	}
	return steps, rows.Err()
}

func (s *PgStore) StartStep(ctx context.Context, taskID uuid.UUID, ordinal int) error {
	_, err := s.pool.Exec(ctx,
		`UPDATE task_step SET status = 'running', updated_at = now()
		 WHERE task_id = $1::uuid AND ordinal = $2`,
		taskID.String(), ordinal)
	return err
}

func (s *PgStore) CompleteStep(ctx context.Context, taskID uuid.UUID, ordinal int, result json.RawMessage) error {
	var arg any
	if len(result) > 0 {
		arg = []byte(result)
	}
	_, err := s.pool.Exec(ctx,
		`UPDATE task_step SET status = 'succeeded', result = $3::jsonb, error = NULL, updated_at = now()
		 WHERE task_id = $1::uuid AND ordinal = $2`,
		taskID.String(), ordinal, arg)
	return err
}

func (s *PgStore) FailStep(ctx context.Context, taskID uuid.UUID, ordinal int, cause error) error {
	msg := cause.Error()
	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return err
	}
	defer tx.Rollback(ctx)

	if _, err := tx.Exec(ctx,
		`UPDATE task_step SET status = 'failed', error = $3, updated_at = now()
		 WHERE task_id = $1::uuid AND ordinal = $2`,
		taskID.String(), ordinal, msg); err != nil {
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
		t          Task
	)
	if err := r.Scan(&idStr, &t.Kind, &statusStr, &projectStr, &t.TargetRef,
		&params, &t.Error, &t.Owner, &t.LeaseExpiry, &t.CreatedAt, &t.UpdatedAt); err != nil {
		return nil, err
	}
	id, err := uuid.Parse(idStr)
	if err != nil {
		return nil, fmt.Errorf("parse task id: %w", err)
	}
	t.ID = id
	t.Status = Status(statusStr)
	t.Params = json.RawMessage(params)
	if projectStr != nil {
		pid, err := uuid.Parse(*projectStr)
		if err != nil {
			return nil, fmt.Errorf("parse project id: %w", err)
		}
		t.ProjectID = &pid
	}
	return &t, nil
}
