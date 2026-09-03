package task

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"

	"github.com/yugabyte/amp/controller/internal/db"
)

// The YBDB-backed SubtaskStore, beside PgStore (store.go) because the task
// framework's SQL lives together: `subtasks` is its table, as `tasks` and
// `task_steps` are.

// PgSubtasks is the YBDB-backed SubtaskStore.
type PgSubtasks struct {
	pool *db.Pool
}

// NewPgSubtasks returns a SubtaskStore backed by pool.
func NewPgSubtasks(pool *db.Pool) *PgSubtasks { return &PgSubtasks{pool: pool} }

// MemSubtasks (memsubtasks.go) is the in-memory mirror the unit tests run against.
var _ SubtaskStore = (*PgSubtasks)(nil)

const subtaskColumns = `id::text, parent_id::text, kind, status::text, args, result,
	error, owner, created_at, updated_at`

func (s *PgSubtasks) Create(ctx context.Context, parentID uuid.UUID, kind string, args json.RawMessage) (*Subtask, error) {
	args = subtaskArgs(args)
	id := subtaskID(parentID, kind, args)
	if _, err := s.pool.Exec(ctx,
		`INSERT INTO subtasks (id, parent_id, kind, args)
		 VALUES ($1::uuid, $2::uuid, $3, $4::jsonb)
		 ON CONFLICT (id) DO NOTHING`,
		id.String(), parentID.String(), kind, []byte(args)); err != nil {
		return nil, fmt.Errorf("insert subtask: %w", err)
	}
	// Read back rather than RETURNING: ON CONFLICT DO NOTHING returns no row on
	// conflict, which is the replay case this has to answer. No db.AfterWrite on
	// failure either — the derived id means replaying is the cure, not the hazard.
	sub, err := s.get(ctx, id)
	if err != nil {
		return nil, fmt.Errorf("read back subtask %s: %w", id, err)
	}
	if sub == nil {
		return nil, fmt.Errorf("subtask %s is absent after its insert", id)
	}
	return sub, nil
}

func (s *PgSubtasks) get(ctx context.Context, id uuid.UUID) (*Subtask, error) {
	row := s.pool.QueryRow(ctx, `SELECT `+subtaskColumns+` FROM subtasks WHERE id = $1::uuid`, id.String())
	sub, err := scanSubtask(row)
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, nil
	}
	return sub, err
}

// Ordered on (created_at, id). A parent creates its children in one pass, so
// created_at ties are ordinary and the id is a stable — though arbitrary, being a
// hash — tiebreak. Callers report per-endpoint outcomes and do not depend on the
// order matching creation.
func (s *PgSubtasks) ByParent(ctx context.Context, parentID uuid.UUID) ([]*Subtask, error) {
	rows, err := s.pool.Query(ctx,
		`SELECT `+subtaskColumns+`
		 FROM subtasks
		 WHERE parent_id = $1::uuid
		 ORDER BY created_at, id`,
		parentID.String())
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	var out []*Subtask
	for rows.Next() {
		sub, err := scanSubtask(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, sub)
	}
	return out, rows.Err()
}

// The three transitions share one fence: the row's OWN parent must currently be
// owned by owner. Correlated on `s.parent_id` — which is why the `s` alias is
// load-bearing, since an unqualified parent_id would resolve inside the subquery's
// own scope — so it cannot be pointed at some other task the caller happens to own.

func (s *PgSubtasks) Start(ctx context.Context, id uuid.UUID, owner string) error {
	_, err := s.pool.Exec(ctx,
		`UPDATE subtasks s SET status = 'running', owner = $2, updated_at = now()
		 WHERE s.id = $1::uuid
		   AND EXISTS (SELECT 1 FROM tasks t WHERE t.id = s.parent_id AND t.owner = $2)`,
		id.String(), owner)
	return err
}

func (s *PgSubtasks) Complete(ctx context.Context, id uuid.UUID, owner string, result json.RawMessage) error {
	var arg any
	if len(result) > 0 {
		arg = []byte(result)
	}
	_, err := s.pool.Exec(ctx,
		`UPDATE subtasks s SET status = 'succeeded', result = $2::jsonb, error = NULL, updated_at = now()
		 WHERE s.id = $1::uuid
		   AND EXISTS (SELECT 1 FROM tasks t WHERE t.id = s.parent_id AND t.owner = $3)`,
		id.String(), arg, owner)
	return err
}

func (s *PgSubtasks) Fail(ctx context.Context, id uuid.UUID, owner string, cause error) error {
	_, err := s.pool.Exec(ctx,
		`UPDATE subtasks s SET status = 'failed', error = $2, updated_at = now()
		 WHERE s.id = $1::uuid
		   AND EXISTS (SELECT 1 FROM tasks t WHERE t.id = s.parent_id AND t.owner = $3)`,
		id.String(), cause.Error(), owner)
	return err
}

// scanSubtask scans the subtaskColumns projection (uuid columns cast to text).
func scanSubtask(r row) (*Subtask, error) {
	var (
		idStr     string
		parentStr string
		statusStr string
		args      []byte
		result    []byte
		sub       Subtask
	)
	if err := r.Scan(&idStr, &parentStr, &sub.Kind, &statusStr, &args, &result,
		&sub.Error, &sub.Owner, &sub.CreatedAt, &sub.UpdatedAt); err != nil {
		return nil, err
	}
	id, err := uuid.Parse(idStr)
	if err != nil {
		return nil, fmt.Errorf("parse subtask id: %w", err)
	}
	parentID, err := uuid.Parse(parentStr)
	if err != nil {
		return nil, fmt.Errorf("parse subtask parent id: %w", err)
	}
	sub.ID = id
	sub.ParentID = parentID
	sub.Status = Status(statusStr)
	sub.Args = json.RawMessage(args)
	if result != nil {
		sub.Result = json.RawMessage(result)
	}
	return &sub, nil
}
