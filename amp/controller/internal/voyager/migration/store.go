package migration

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"

	"github.com/yugabyte/amp/controller/internal/db"
)

// Store is the controller-owned persistence surface over amp_voyager_control.*. It
// covers the control tables (migration + migration_run); the read-only
// projection over Voyager-owned ybvoyager_visualizer.* is added in a later
// milestone.
type Store interface {
	// CreateMigration inserts a migration with the caller-supplied id in state
	// "created" and returns it. The id is the idempotency key (also injected
	// into Voyager as its migration_uuid), so replaying the same id converges
	// on the existing row instead of inserting a duplicate.
	CreateMigration(ctx context.Context, id uuid.UUID, name string, source SourceConfig) (Migration, error)
	// GetMigration returns the migration with its latest runs and derived
	// status, or ErrNotFound.
	GetMigration(ctx context.Context, id uuid.UUID) (Migration, error)
	// ListMigrations returns a page of summaries (newest first) and the total
	// count across all migrations. limit/offset are clamped to a safe range.
	ListMigrations(ctx context.Context, limit, offset int) ([]MigrationSummary, int, error)

	// GetRun returns a single run by id, or ErrNotFound.
	GetRun(ctx context.Context, id uuid.UUID) (Run, error)
	// LatestRuns returns the newest run per operation for a migration, oldest
	// first, so status derivation sees a stable sequence.
	LatestRuns(ctx context.Context, migrationID uuid.UUID) ([]Run, error)
	// HasActiveRun reports whether a pending or running run exists for the
	// migration.
	HasActiveRun(ctx context.Context, migrationID uuid.UUID) (bool, error)
	// NextAttempt returns MAX(attempt)+1 for (migration, operation) — the
	// attempt number the orchestration layer stamps onto the next EnsureRun.
	NextAttempt(ctx context.Context, migrationID uuid.UUID, operation Operation) (int, error)
	// SetTargetTask atomically claims target provisioning for taskID. A replay
	// by the same task is allowed; a competing claim is ErrConflict.
	SetTargetTask(ctx context.Context, migrationID, taskID uuid.UUID) error
	// ClearTargetTask releases taskID's provisioning claim. It is idempotent if
	// the claim is already gone.
	ClearTargetTask(ctx context.Context, migrationID, taskID uuid.UUID) error
	// AttachTarget writes the resolved target and clears the claim. Only the
	// claim holder (taskID) may attach; replaying the same target converges.
	AttachTarget(ctx context.Context, migrationID, taskID uuid.UUID, target TargetConfig) error
	// EnsureRun write-ahead inserts a run keyed by spec.ID; replaying reads the
	// existing row back. A competing active run or duplicate attempt is
	// ErrConflict.
	EnsureRun(ctx context.Context, spec RunSpec) (Run, error)
	// MarkRunRunning records that taskID submitted the job. It fences on task_id
	// (only the run's owner may transition it) and moves only from
	// pending/running; an already-terminal run is a no-op replay.
	MarkRunRunning(ctx context.Context, runID, taskID uuid.UUID, deployerType string, metadata json.RawMessage) error
	// MarkRunTerminal records a terminal outcome. It fences on task_id and moves
	// only from pending/running; replaying the same terminal status is a no-op
	// success, a conflicting status is ErrConflict.
	MarkRunTerminal(ctx context.Context, runID, taskID uuid.UUID, status RunStatus, errorSummary string) error

	// LatestPhases returns the latest invocation per Voyager phase from the
	// read-only ybvoyager_visualizer.* projection; an absent schema (Voyager has
	// not run yet) yields an empty slice, not an error.
	LatestPhases(ctx context.Context, migrationID uuid.UUID) ([]Phase, error)
	// LatestDataProgress returns Voyager's export/import per-table metrics, or
	// nil when none exist yet (or the visualizer schema is absent).
	LatestDataProgress(ctx context.Context, migrationID uuid.UUID) (*DataProgress, error)
	// ReadVoyagerStatus composes phases + data progress into a VoyagerStatus, or
	// nil when Voyager has produced nothing yet.
	ReadVoyagerStatus(ctx context.Context, migrationID uuid.UUID) (*VoyagerStatus, error)
	// LatestCompletedAssessmentPayload returns the newest completed phase-1
	// payload; ErrNotFound if the migration is unknown, ErrAssessmentNotFound if
	// no assessment has completed yet.
	LatestCompletedAssessmentPayload(ctx context.Context, migrationID uuid.UUID) (json.RawMessage, error)
	// GetAssessment reads and projects the latest completed Voyager assessment
	// into the agent-facing Assessment shape.
	GetAssessment(ctx context.Context, migrationID uuid.UUID) (Assessment, error)
}

// YbStore is the YugabyteDB-backed Store. The control tables live in the
// controller's YugabyteDB (reached over the Postgres wire via pgx), not a PG
// compute. It follows the controller's existing conventions: UUIDs are passed
// as text with an explicit ::uuid cast and projected ::text to avoid a pgx uuid
// codec, and writes use ON CONFLICT (id) DO NOTHING + read-back so replaying the
// same statement converges instead of erroring.
type YbStore struct {
	pool *db.Pool
}

// NewYbStore returns a Store backed by pool. The pool must be pinned to the
// amp_voyager_control schema.
func NewYbStore(pool *db.Pool) *YbStore {
	return &YbStore{pool: pool}
}

var _ Store = (*YbStore)(nil)

// List pagination is clamped to this range so a caller can't pass a negative
// LIMIT/OFFSET (which the driver rejects) or an unbounded page.
const (
	defaultPageSize = 20
	maxPageSize     = 100
)

const migrationColumns = `id::text, name, source_config::text,
	COALESCE(target_config::text, ''), COALESCE(target_task_id::text, ''),
	created_at, updated_at`

const runColumns = `id::text, migration_id::text, operation, attempt, status,
	COALESCE(error_summary, ''), COALESCE(deployer_type, ''),
	COALESCE(deployer_metadata::text, ''), COALESCE(task_id::text, ''),
	started_at, finished_at, created_at, updated_at`

// CreateMigration inserts a migration with the caller-supplied id. Passing the
// id in (rather than generating one here) makes the ON CONFLICT (id) DO NOTHING
// reachable, so a retried create with the same id converges on the existing row
// instead of inserting a duplicate.
func (s *YbStore) CreateMigration(ctx context.Context, id uuid.UUID, name string, source SourceConfig) (Migration, error) {
	if id == uuid.Nil {
		return Migration{}, fmt.Errorf("create migration: id is required")
	}
	sourceJSON, err := json.Marshal(source)
	if err != nil {
		return Migration{}, fmt.Errorf("encode source config: %w", err)
	}
	if _, err := s.pool.Exec(ctx,
		`INSERT INTO migration (id, name, source_config)
		 VALUES ($1::uuid, $2, $3::jsonb)
		 ON CONFLICT (id) DO NOTHING`,
		id.String(), name, string(sourceJSON)); err != nil {
		return Migration{}, mapWriteError("insert migration", err)
	}
	return s.GetMigration(ctx, id)
}

func (s *YbStore) GetMigration(ctx context.Context, id uuid.UUID) (Migration, error) {
	row := s.pool.QueryRow(ctx,
		`SELECT `+migrationColumns+` FROM migration WHERE id = $1::uuid`,
		id.String())
	m, err := scanMigration(row)
	if errors.Is(err, pgx.ErrNoRows) {
		return Migration{}, ErrNotFound
	}
	if err != nil {
		return Migration{}, err
	}
	if err := s.hydrate(ctx, &m); err != nil {
		return Migration{}, err
	}
	return m, nil
}

func (s *YbStore) ListMigrations(ctx context.Context, limit, offset int) ([]MigrationSummary, int, error) {
	// Clamp inputs: a non-positive limit defaults to a page, an oversized one is
	// capped, and a negative offset is floored — the driver rejects negative
	// LIMIT/OFFSET, and limit=0 would otherwise return an empty page.
	if limit <= 0 {
		limit = defaultPageSize
	} else if limit > maxPageSize {
		limit = maxPageSize
	}
	if offset < 0 {
		offset = 0
	}
	var total int
	if err := s.pool.QueryRow(ctx, `SELECT count(*) FROM migration`).Scan(&total); err != nil {
		return nil, 0, fmt.Errorf("count migrations: %w", err)
	}
	rows, err := s.pool.Query(ctx,
		`SELECT `+migrationColumns+`
		 FROM migration ORDER BY created_at DESC, id LIMIT $1 OFFSET $2`,
		limit, offset)
	if err != nil {
		return nil, 0, fmt.Errorf("list migrations: %w", err)
	}
	defer rows.Close()

	var migrations []Migration
	for rows.Next() {
		m, err := scanMigration(rows)
		if err != nil {
			return nil, 0, err
		}
		migrations = append(migrations, m)
	}
	if err := rows.Err(); err != nil {
		return nil, 0, fmt.Errorf("list migrations: %w", err)
	}

	// The list view derives state from persisted rows only (no deployer calls),
	// so it may lag until the next per-migration GET refreshes live job state.
	summaries := make([]MigrationSummary, 0, len(migrations))
	for i := range migrations {
		m := &migrations[i]
		runs, err := s.LatestRuns(ctx, m.ID)
		if err != nil {
			return nil, 0, err
		}
		m.Runs = runs
		status := DeriveStatus(*m)
		summaries = append(summaries, MigrationSummary{
			ID:        m.ID,
			Name:      m.Name,
			State:     status.State,
			Detail:    status.Detail,
			CreatedAt: m.CreatedAt,
			UpdatedAt: m.UpdatedAt,
		})
	}
	return summaries, total, nil
}

func (s *YbStore) GetRun(ctx context.Context, id uuid.UUID) (Run, error) {
	row := s.pool.QueryRow(ctx,
		`SELECT `+runColumns+` FROM migration_run WHERE id = $1::uuid`,
		id.String())
	run, err := scanRun(row)
	if errors.Is(err, pgx.ErrNoRows) {
		return Run{}, ErrNotFound
	}
	return run, err
}

// LatestRuns returns one row per operation, the greatest attempt, ordered by
// creation so status derivation sees a stable sequence.
func (s *YbStore) LatestRuns(ctx context.Context, migrationID uuid.UUID) ([]Run, error) {
	rows, err := s.pool.Query(ctx,
		`SELECT `+runColumns+`
		 FROM (
		     SELECT DISTINCT ON (operation) *
		     FROM migration_run
		     WHERE migration_id = $1::uuid
		     ORDER BY operation, attempt DESC
		 ) latest
		 ORDER BY created_at, operation`,
		migrationID.String())
	if err != nil {
		return nil, fmt.Errorf("list migration runs: %w", err)
	}
	defer rows.Close()

	var runs []Run
	for rows.Next() {
		run, err := scanRun(rows)
		if err != nil {
			return nil, err
		}
		runs = append(runs, run)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("list migration runs: %w", err)
	}
	return runs, nil
}

func (s *YbStore) HasActiveRun(ctx context.Context, migrationID uuid.UUID) (bool, error) {
	var active bool
	if err := s.pool.QueryRow(ctx,
		`SELECT EXISTS (
		     SELECT 1 FROM migration_run
		     WHERE migration_id = $1::uuid AND status IN ('pending', 'running')
		 )`,
		migrationID.String()).Scan(&active); err != nil {
		return false, fmt.Errorf("check active migration run: %w", err)
	}
	return active, nil
}

// NextAttempt returns MAX(attempt)+1 for (migration, operation). The
// orchestration layer (M4+) calls it to pick the attempt number for the next
// EnsureRun, so a retried assess/migrate gets attempt 2, 3, ….
func (s *YbStore) NextAttempt(ctx context.Context, migrationID uuid.UUID, operation Operation) (int, error) {
	var next int
	if err := s.pool.QueryRow(ctx,
		`SELECT COALESCE(MAX(attempt), 0) + 1
		 FROM migration_run WHERE migration_id = $1::uuid AND operation = $2`,
		migrationID.String(), string(operation)).Scan(&next); err != nil {
		return 0, fmt.Errorf("next migration run attempt: %w", err)
	}
	return next, nil
}

// SetTargetTask atomically claims target provisioning for taskID. A replay by
// the same task is allowed; a competing claim (or an already-attached target)
// is a conflict.
func (s *YbStore) SetTargetTask(ctx context.Context, migrationID, taskID uuid.UUID) error {
	tag, err := s.pool.Exec(ctx,
		`UPDATE migration
		 SET target_task_id = $2::uuid, updated_at = now()
		 WHERE id = $1::uuid
		   AND target_config IS NULL
		   AND (target_task_id IS NULL OR target_task_id = $2::uuid)`,
		migrationID.String(), taskID.String())
	if err != nil {
		return mapWriteError("set migration target task", err)
	}
	if tag.RowsAffected() == 1 {
		return nil
	}
	return s.notFoundOrConflict(ctx, migrationID)
}

// ClearTargetTask releases a provisioning claim held by taskID. It is
// idempotent: if the claim is already gone the migration is left untouched.
func (s *YbStore) ClearTargetTask(ctx context.Context, migrationID, taskID uuid.UUID) error {
	tag, err := s.pool.Exec(ctx,
		`UPDATE migration
		 SET target_task_id = NULL, updated_at = now()
		 WHERE id = $1::uuid AND target_task_id = $2::uuid`,
		migrationID.String(), taskID.String())
	if err != nil {
		return mapWriteError("clear migration target task", err)
	}
	if tag.RowsAffected() == 1 {
		return nil
	}
	// Nothing updated: either the migration is gone (surface that) or the claim
	// was already cleared/never held (a no-op replay, which is fine).
	if err := s.migrationExists(ctx, migrationID); err != nil {
		return err
	}
	return nil
}

// AttachTarget writes the resolved target and clears the provisioning claim.
// Only the claim holder (taskID) may attach a new target — guarding on
// target_task_id the way ClearTargetTask does stops a task that never held the
// claim from overwriting the holder's target and orphaning its compute.
// Replaying with the same target still converges, even after the claim was
// cleared, via the second branch.
func (s *YbStore) AttachTarget(ctx context.Context, migrationID, taskID uuid.UUID, target TargetConfig) error {
	targetJSON, err := json.Marshal(target)
	if err != nil {
		return fmt.Errorf("encode target config: %w", err)
	}
	tag, err := s.pool.Exec(ctx,
		`UPDATE migration
		 SET target_config = $3::jsonb, target_task_id = NULL, updated_at = now()
		 WHERE id = $1::uuid
		   AND (
		       (target_task_id = $2::uuid AND target_config IS NULL)
		       OR target_config = $3::jsonb
		   )`,
		migrationID.String(), taskID.String(), string(targetJSON))
	if err != nil {
		return mapWriteError("attach migration target", err)
	}
	if tag.RowsAffected() == 1 {
		return nil
	}
	return s.notFoundOrConflict(ctx, migrationID)
}

// EnsureRun write-ahead inserts the run. The stable spec.ID is the idempotency
// key: replaying reads the existing row back. A competing active run or a
// duplicate (migration, operation, attempt) surfaces as a conflict.
func (s *YbStore) EnsureRun(ctx context.Context, spec RunSpec) (Run, error) {
	if spec.ID == uuid.Nil || spec.MigrationID == uuid.Nil {
		return Run{}, fmt.Errorf("ensure migration run: run id and migration id are required")
	}
	if !spec.Operation.Valid() {
		return Run{}, fmt.Errorf("ensure migration run: invalid operation %q", spec.Operation)
	}
	if spec.Attempt < 1 {
		return Run{}, fmt.Errorf("ensure migration run: attempt must be positive")
	}
	var taskID any
	if spec.TaskID != nil {
		taskID = spec.TaskID.String()
	}
	_, err := s.pool.Exec(ctx,
		`INSERT INTO migration_run (id, migration_id, operation, attempt, task_id)
		 VALUES ($1::uuid, $2::uuid, $3, $4, $5::uuid)
		 ON CONFLICT (id) DO NOTHING`,
		spec.ID.String(), spec.MigrationID.String(), string(spec.Operation), spec.Attempt, taskID)
	if err != nil {
		return Run{}, mapWriteError("ensure migration run", err)
	}
	return s.GetRun(ctx, spec.ID)
}

// MarkRunRunning records that taskID submitted the job. It fences on task_id so
// only the run's owner can transition it — a worker that lost its lease cannot
// write over the instance that now owns the run. It transitions only from
// pending/running; if the run is already terminal (a late replay) it is treated
// as a no-op success.
func (s *YbStore) MarkRunRunning(ctx context.Context, runID, taskID uuid.UUID, deployerType string, metadata json.RawMessage) error {
	var metadataArg any
	if len(metadata) > 0 {
		metadataArg = string(metadata)
	}
	tag, err := s.pool.Exec(ctx,
		`UPDATE migration_run
		 SET status = 'running', deployer_type = $3, deployer_metadata = $4::jsonb,
		     error_summary = NULL, started_at = COALESCE(started_at, now()),
		     finished_at = NULL, updated_at = now()
		 WHERE id = $1::uuid AND task_id = $2::uuid AND status IN ('pending', 'running')`,
		runID.String(), taskID.String(), deployerType, metadataArg)
	if err != nil {
		return mapWriteError("mark migration run running", err)
	}
	if tag.RowsAffected() == 1 {
		return nil
	}
	return s.terminalReplayOrConflict(ctx, runID)
}

// MarkRunTerminal records a terminal outcome. Like MarkRunRunning it fences on
// task_id (only the owner may transition the run) and moves only from
// pending/running; replaying with the same terminal status is a no-op success,
// while a conflicting terminal status is reported.
func (s *YbStore) MarkRunTerminal(ctx context.Context, runID, taskID uuid.UUID, status RunStatus, errorSummary string) error {
	if status != RunSucceeded && status != RunFailed {
		return fmt.Errorf("mark migration run terminal: invalid status %q", status)
	}
	var summaryArg any
	if errorSummary != "" {
		summaryArg = errorSummary
	}
	tag, err := s.pool.Exec(ctx,
		`UPDATE migration_run
		 SET status = $3, error_summary = $4, finished_at = now(), updated_at = now()
		 WHERE id = $1::uuid AND task_id = $2::uuid AND status IN ('pending', 'running')`,
		runID.String(), taskID.String(), string(status), summaryArg)
	if err != nil {
		return mapWriteError("mark migration run terminal", err)
	}
	if tag.RowsAffected() == 1 {
		return nil
	}
	run, err := s.GetRun(ctx, runID)
	if err != nil {
		return err
	}
	if run.Status == status {
		return nil // idempotent replay
	}
	return ErrConflict
}

// hydrate fills the derived, read-time fields of a migration: the latest runs,
// the read-only projection over Voyager's own ybvoyager_visualizer.* progress
// tables, and the status derived from both. Runs is coalesced to a non-nil
// slice so the JSON contract is "runs": [] rather than "runs": null; Voyager is
// left nil when Voyager has produced nothing yet.
func (s *YbStore) hydrate(ctx context.Context, m *Migration) error {
	runs, err := s.LatestRuns(ctx, m.ID)
	if err != nil {
		return err
	}
	if runs == nil {
		runs = []Run{}
	}
	m.Runs = runs
	voyager, err := s.ReadVoyagerStatus(ctx, m.ID)
	if err != nil {
		return err
	}
	m.Voyager = voyager
	status := DeriveStatus(*m)
	m.State, m.Detail = status.State, status.Detail
	return nil
}

// terminalReplayOrConflict decides the outcome of a guarded UPDATE that
// affected no rows: a run that is already terminal means a late replay (ok),
// anything else is a conflict (or the run is missing).
func (s *YbStore) terminalReplayOrConflict(ctx context.Context, runID uuid.UUID) error {
	run, err := s.GetRun(ctx, runID)
	if err != nil {
		return err
	}
	if run.Status.Terminal() {
		return nil
	}
	return ErrConflict
}

// notFoundOrConflict distinguishes a missing migration (ErrNotFound) from a
// guarded UPDATE that matched the row but not its precondition (ErrConflict).
func (s *YbStore) notFoundOrConflict(ctx context.Context, migrationID uuid.UUID) error {
	if err := s.migrationExists(ctx, migrationID); err != nil {
		return err
	}
	return ErrConflict
}

func (s *YbStore) migrationExists(ctx context.Context, migrationID uuid.UUID) error {
	var x int
	err := s.pool.QueryRow(ctx,
		`SELECT 1 FROM migration WHERE id = $1::uuid`, migrationID.String()).Scan(&x)
	if errors.Is(err, pgx.ErrNoRows) {
		return ErrNotFound
	}
	if err != nil {
		return fmt.Errorf("resolve migration: %w", err)
	}
	return nil
}

// mapWriteError translates the constraint violations the control-table writes
// can raise: a unique violation (duplicate attempt, or the single-active-run
// partial index) -> ErrConflict, and a foreign-key violation (unknown
// migration) -> ErrNotFound. It preserves the operation and the offending
// constraint name in the wrapped message (while staying errors.Is-comparable to
// the sentinel) so logs can tell "another run is active" from "duplicate
// attempt".
func mapWriteError(what string, err error) error {
	var pgErr *pgconn.PgError
	if errors.As(err, &pgErr) {
		switch pgErr.Code {
		case "23505": // unique_violation
			return fmt.Errorf("%s: unique violation on %q: %w", what, pgErr.ConstraintName, ErrConflict)
		case "23503": // foreign_key_violation
			return fmt.Errorf("%s: foreign key violation on %q: %w", what, pgErr.ConstraintName, ErrNotFound)
		}
	}
	return fmt.Errorf("%s: %w", what, err)
}

type scanRow interface {
	Scan(dest ...any) error
}

func scanMigration(r scanRow) (Migration, error) {
	var (
		idStr         string
		sourceJSON    string
		targetJSON    string
		targetTaskStr string
		m             Migration
	)
	if err := r.Scan(&idStr, &m.Name, &sourceJSON, &targetJSON, &targetTaskStr,
		&m.CreatedAt, &m.UpdatedAt); err != nil {
		return Migration{}, err
	}
	id, err := uuid.Parse(idStr)
	if err != nil {
		return Migration{}, fmt.Errorf("parse migration id: %w", err)
	}
	m.ID = id
	if err := json.Unmarshal([]byte(sourceJSON), &m.Source); err != nil {
		return Migration{}, fmt.Errorf("decode source config: %w", err)
	}
	if targetJSON != "" {
		var target TargetConfig
		if err := json.Unmarshal([]byte(targetJSON), &target); err != nil {
			return Migration{}, fmt.Errorf("decode target config: %w", err)
		}
		m.Target = &target
	}
	if targetTaskStr != "" {
		taskID, err := uuid.Parse(targetTaskStr)
		if err != nil {
			return Migration{}, fmt.Errorf("parse target task id: %w", err)
		}
		m.TargetTaskID = &taskID
	}
	m.Runs = []Run{}
	return m, nil
}

func scanRun(r scanRow) (Run, error) {
	var (
		idStr      string
		migStr     string
		op         string
		status     string
		errSummary string
		depType    string
		depMeta    string
		taskStr    string
		startedAt  sql.NullTime
		finishedAt sql.NullTime
		run        Run
	)
	if err := r.Scan(&idStr, &migStr, &op, &run.Attempt, &status,
		&errSummary, &depType, &depMeta, &taskStr,
		&startedAt, &finishedAt, &run.CreatedAt, &run.UpdatedAt); err != nil {
		return Run{}, err
	}
	id, err := uuid.Parse(idStr)
	if err != nil {
		return Run{}, fmt.Errorf("parse run id: %w", err)
	}
	migrationID, err := uuid.Parse(migStr)
	if err != nil {
		return Run{}, fmt.Errorf("parse run migration id: %w", err)
	}
	run.ID = id
	run.MigrationID = migrationID
	run.Operation = Operation(op)
	run.Status = RunStatus(status)
	if errSummary != "" {
		run.ErrorSummary = &errSummary
	}
	if depType != "" {
		run.DeployerType = &depType
	}
	if depMeta != "" {
		run.DeployerMetadata = json.RawMessage(depMeta)
	}
	if taskStr != "" {
		taskID, err := uuid.Parse(taskStr)
		if err != nil {
			return Run{}, fmt.Errorf("parse run task id: %w", err)
		}
		run.TaskID = &taskID
	}
	if startedAt.Valid {
		t := startedAt.Time
		run.StartedAt = &t
	}
	if finishedAt.Valid {
		t := finishedAt.Time
		run.FinishedAt = &t
	}
	return run, nil
}
