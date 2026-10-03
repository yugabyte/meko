package repository

import (
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/lib/pq"

	"meko/api_server/internal/models"
)

// EnvironmentTaskRepo persists the amp task history the reconcile agent relays
// (meko_system.environment_tasks). Control-plane mode only.
//
// Rows are keyed by (env_uuid, task_id) -- by the parent's uuid rather than its id
// slug, since an id is unique only within an account. Everything here is scoped to
// one env_uuid: this repo never resolves an environment, so the caller has to have
// done that (and therefore the account check) first.
type EnvironmentTaskRepo struct {
	db *sql.DB
}

// NewEnvironmentTaskRepo constructs the repo over the system DB.
func NewEnvironmentTaskRepo(db *sql.DB) *EnvironmentTaskRepo {
	return &EnvironmentTaskRepo{db: db}
}

// TaskRetention is how long a terminal task stays readable. It is not an
// implementation detail: the agent's memory dies with its pod and nothing else keeps
// this history, so the window IS the console's history depth -- how far back an
// operator can ask what ran. Documented as such in ycp/DEPLOY.md.
const TaskRetention = 30 * 24 * time.Hour

// EnvironmentTaskListOpts narrows a ListTasks query. Zero-value fields are
// unfiltered.
type EnvironmentTaskListOpts struct {
	Limit  int
	Offset int
	// Statuses is an OR set; empty matches any. The caller is responsible for
	// whitelisting these against models.EnvironmentTaskStatuses -- they reach SQL as
	// bound parameters, but an unknown value would silently return an empty page
	// rather than saying so.
	Statuses  []string
	Kind      string
	ClusterID string
	// ClusterIDs is an OR set of amp cluster UUIDs; empty is unfiltered. It is how the
	// pool-scoped tasks API restricts a shared environment's log to just the clusters
	// a storage pool owns -- the caller resolves that set from observed inventory.
	ClusterIDs []string
}

// EnvironmentTaskListResult is one page plus the count the page was taken from.
type EnvironmentTaskListResult struct {
	Items   []models.EnvironmentTask
	Total   int
	HasMore bool
}

// UpsertTasks records a delta of relayed tasks. Absence is never a delete: what the
// agent does not mention this tick it already reported, and dropping it is what
// would turn this table back into a snapshot.
//
// The task_updated_at guard is load-bearing. The agent re-reads the boundary rows of
// its change feed on every sweep and re-sends anything it could not confirm
// delivered, so the same task arrives repeatedly and can arrive out of order across
// a retry. Without the guard a stale report walks a terminal task back to running,
// and the console shows an operation as in flight forever. Same monotonic reasoning
// as the controller's own GREATEST() generation stamp.
func (r *EnvironmentTaskRepo) UpsertTasks(envUUID string, tasks []models.EnvironmentTask) error {
	if len(tasks) == 0 {
		return nil
	}
	tx, err := r.db.Begin()
	if err != nil {
		return fmt.Errorf("begin: %w", err)
	}
	defer func() { _ = tx.Rollback() }()

	const upsert = `
		INSERT INTO meko_system.environment_tasks
		    (env_uuid, task_id, kind, status, cluster_id, target_ref, error, result,
		     ycp_generation, task_created_at, task_updated_at, reported_at)
		VALUES ($1,$2,$3,$4,$5,$6,$7,$8,$9,$10,$11,NOW())
		ON CONFLICT (env_uuid, task_id) DO UPDATE
		  SET kind            = EXCLUDED.kind,
		      status          = EXCLUDED.status,
		      cluster_id      = EXCLUDED.cluster_id,
		      target_ref      = EXCLUDED.target_ref,
		      error           = EXCLUDED.error,
		      result          = EXCLUDED.result,
		      ycp_generation  = EXCLUDED.ycp_generation,
		      task_created_at = EXCLUDED.task_created_at,
		      task_updated_at = EXCLUDED.task_updated_at,
		      reported_at     = NOW()
		  WHERE EXCLUDED.task_updated_at >= meko_system.environment_tasks.task_updated_at`
	for _, t := range tasks {
		if _, err := tx.Exec(upsert,
			envUUID, t.TaskID, t.Kind, t.Status,
			nullableUUID(t.ClusterID), nullableText(t.TargetRef), nullableText(t.Error),
			nullableJSON(t.Result), t.YCPGeneration, t.CreatedAt, t.UpdatedAt,
		); err != nil {
			return fmt.Errorf("upsert task %s: %w", t.TaskID, err)
		}
	}
	return tx.Commit()
}

// ListTasks returns one page of an environment's task history, newest first.
//
// Ordered task_created_at DESC with a task_id DESC tiebreak: several tasks can share
// a created_at, and without a total order the same row can surface on two pages of
// one listing.
func (r *EnvironmentTaskRepo) ListTasks(envUUID string, opts EnvironmentTaskListOpts) (EnvironmentTaskListResult, error) {
	var out EnvironmentTaskListResult

	where := []string{"env_uuid = $1"}
	args := []any{envUUID}
	add := func(pred string, arg any) {
		args = append(args, arg)
		where = append(where, fmt.Sprintf(pred, len(args)))
	}
	if len(opts.Statuses) > 0 {
		// ANY over a bound array rather than an interpolated IN list, so the number of
		// statuses does not change the statement's shape.
		add("status = ANY($%d)", pq.Array(opts.Statuses))
	}
	if opts.Kind != "" {
		add("kind = $%d", opts.Kind)
	}
	if opts.ClusterID != "" {
		add("cluster_id = $%d::uuid", opts.ClusterID)
	}
	if len(opts.ClusterIDs) > 0 {
		// ANY over a bound uuid[] rather than an interpolated IN list, so the number of
		// clusters does not change the statement's shape. ANDs with ClusterID above.
		add("cluster_id = ANY($%d::uuid[])", pq.Array(opts.ClusterIDs))
	}
	clause := " WHERE " + strings.Join(where, " AND ")

	if err := r.db.QueryRow(
		`SELECT count(*) FROM meko_system.environment_tasks`+clause, args...,
	).Scan(&out.Total); err != nil {
		return out, fmt.Errorf("count environment tasks: %w", err)
	}

	limitArg, offsetArg := len(args)+1, len(args)+2
	args = append(args, opts.Limit, opts.Offset)
	rows, err := r.db.Query(fmt.Sprintf(
		`SELECT task_id, kind, status, cluster_id, target_ref, error,
		        COALESCE(result::text, ''), ycp_generation,
		        task_created_at, task_updated_at, reported_at
		 FROM meko_system.environment_tasks%s
		 ORDER BY task_created_at DESC, task_id DESC
		 LIMIT $%d OFFSET $%d`, clause, limitArg, offsetArg), args...)
	if err != nil {
		return out, fmt.Errorf("list environment tasks: %w", err)
	}
	defer rows.Close()

	// Never nil, so the handler serializes [] rather than null.
	out.Items = make([]models.EnvironmentTask, 0, opts.Limit)
	for rows.Next() {
		t, err := scanEnvironmentTask(rows)
		if err != nil {
			return out, err
		}
		out.Items = append(out.Items, t)
	}
	if err := rows.Err(); err != nil {
		return out, fmt.Errorf("iterate environment tasks: %w", err)
	}
	out.HasMore = opts.Offset+len(out.Items) < out.Total
	return out, nil
}

// GetTask returns one task of an environment, or nil when there is none.
func (r *EnvironmentTaskRepo) GetTask(envUUID, taskID string) (*models.EnvironmentTask, error) {
	row := r.db.QueryRow(
		`SELECT task_id, kind, status, cluster_id, target_ref, error,
		        COALESCE(result::text, ''), ycp_generation,
		        task_created_at, task_updated_at, reported_at
		 FROM meko_system.environment_tasks
		 WHERE env_uuid = $1 AND task_id = $2`, envUUID, taskID)
	t, err := scanEnvironmentTask(row)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	return &t, nil
}

// PruneTerminal deletes terminal tasks last touched more than olderThan ago, across
// every environment, and reports how many went.
//
// Terminal only: a pending or running task is live state however old its timestamp
// looks, and deleting one would make an operation the console is watching vanish
// mid-flight rather than resolve.
func (r *EnvironmentTaskRepo) PruneTerminal(olderThan time.Duration) (int64, error) {
	res, err := r.db.Exec(
		`DELETE FROM meko_system.environment_tasks
		 WHERE status = ANY($1) AND task_updated_at < $2`,
		pq.Array(models.TerminalTaskStatuses), time.Now().UTC().Add(-olderThan))
	if err != nil {
		return 0, fmt.Errorf("prune environment tasks: %w", err)
	}
	n, err := res.RowsAffected()
	if err != nil {
		return 0, nil // the delete happened; the count is not worth an error
	}
	return n, nil
}

// rowScanner is satisfied by both *sql.Row and *sql.Rows, so the single-row and
// paged reads share one scan.
type rowScanner interface {
	Scan(dest ...any) error
}

func scanEnvironmentTask(s rowScanner) (models.EnvironmentTask, error) {
	var (
		t         models.EnvironmentTask
		clusterID sql.NullString
		targetRef sql.NullString
		errText   sql.NullString
		result    string
		gen       sql.NullInt64
	)
	if err := s.Scan(&t.TaskID, &t.Kind, &t.Status, &clusterID, &targetRef, &errText,
		&result, &gen, &t.CreatedAt, &t.UpdatedAt, &t.ReportedAt); err != nil {
		return t, err
	}
	t.ClusterID = clusterID.String
	t.TargetRef = targetRef.String
	t.Error = errText.String
	if result != "" {
		t.Result = json.RawMessage(result)
	}
	if gen.Valid {
		g := gen.Int64
		t.YCPGeneration = &g
	}
	return t, nil
}

// nullableText stores "" as NULL, so an absent optional field reads back absent
// rather than as an empty string the console would render as a blank value.
func nullableText(s string) any {
	if s == "" {
		return nil
	}
	return s
}

// nullableUUID is nullableText for a uuid column, where "" is not a castable value.
func nullableUUID(s string) any {
	if s == "" {
		return nil
	}
	return s
}

func nullableJSON(raw json.RawMessage) any {
	if len(raw) == 0 {
		return nil
	}
	return []byte(raw)
}
