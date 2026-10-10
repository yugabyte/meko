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
	// a storage pool owns, past and present.
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

// clusterPoolRefresh is how stale a live cluster's observed_at may get before a
// heartbeat rewrites it. It has to stay well under TaskRetention: PruneClusterPools
// treats observed_at as "last reported", and a live cluster's pairing must never look
// old enough to prune.
const clusterPoolRefresh = 24 * time.Hour

// RecordClusterPools remembers which storage pool each of an environment's clusters
// belongs to, keyed by amp cluster UUID (meko_system.environment_cluster_pools).
// clusterIDs and pools are parallel. It only ever adds or corrects a pairing: a
// cluster missing from this report is not removed, because outliving the inventory is
// the reason the table exists.
//
// One statement for the whole set, since this runs on every 10s heartbeat; an
// unchanged pairing is rewritten only once per clusterPoolRefresh.
func (r *EnvironmentTaskRepo) RecordClusterPools(envUUID string, clusterIDs, pools []string) error {
	if len(clusterIDs) == 0 {
		return nil
	}
	_, err := r.db.Exec(`
		INSERT INTO meko_system.environment_cluster_pools (env_uuid, cluster_id, storage_pool)
		SELECT $1, u.cluster_id, u.storage_pool
		  FROM unnest($2::uuid[], $3::text[]) AS u(cluster_id, storage_pool)
		ON CONFLICT (env_uuid, cluster_id) DO UPDATE
		  SET storage_pool = EXCLUDED.storage_pool, observed_at = NOW()
		  WHERE meko_system.environment_cluster_pools.storage_pool <> EXCLUDED.storage_pool
		     OR meko_system.environment_cluster_pools.observed_at < $4`,
		envUUID, pq.Array(clusterIDs), pq.Array(pools), time.Now().UTC().Add(-clusterPoolRefresh))
	if err != nil {
		return fmt.Errorf("record cluster pools: %w", err)
	}
	return nil
}

// ClusterIDsForPool returns the amp UUIDs of every cluster ever reported under a
// storage pool in one environment, including ones since deleted.
func (r *EnvironmentTaskRepo) ClusterIDsForPool(envUUID, pool string) ([]string, error) {
	rows, err := r.db.Query(
		`SELECT cluster_id FROM meko_system.environment_cluster_pools
		 WHERE env_uuid = $1 AND storage_pool = $2`, envUUID, pool)
	if err != nil {
		return nil, fmt.Errorf("list pool clusters: %w", err)
	}
	defer rows.Close()
	var ids []string
	for rows.Next() {
		var id string
		if err := rows.Scan(&id); err != nil {
			return nil, fmt.Errorf("scan pool cluster: %w", err)
		}
		ids = append(ids, id)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("iterate pool clusters: %w", err)
	}
	return ids, nil
}

// PruneClusterPools deletes pairings no task refers to any more whose cluster has not
// been reported for olderThan. A live cluster's observed_at is never older than
// clusterPoolRefresh, so its pairing is kept even once its tasks have aged out -- the
// agent may relay its delete_cluster task only after the inventory has dropped it.
func (r *EnvironmentTaskRepo) PruneClusterPools(olderThan time.Duration) (int64, error) {
	res, err := r.db.Exec(
		`DELETE FROM meko_system.environment_cluster_pools p
		 WHERE p.observed_at < $1
		   AND NOT EXISTS (SELECT 1 FROM meko_system.environment_tasks t
		                   WHERE t.env_uuid = p.env_uuid AND t.cluster_id = p.cluster_id)`,
		time.Now().UTC().Add(-olderThan))
	if err != nil {
		return 0, fmt.Errorf("prune cluster pools: %w", err)
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
