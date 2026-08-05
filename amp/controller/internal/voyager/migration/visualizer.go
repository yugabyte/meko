package migration

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
)

// LatestPhases reads the latest invocation_sequence row per phase. Voyager
// owns the schema and may not have created it before the first job; that state
// is treated as an empty result rather than an error.
func (s *YbStore) LatestPhases(ctx context.Context, migrationID uuid.UUID) ([]Phase, error) {
	rows, err := s.pool.Query(ctx,
		`SELECT migration_phase, COALESCE(status, ''), invocation_timestamp
		 FROM (
		     SELECT DISTINCT ON (migration_phase)
		         migration_phase, status, invocation_timestamp
		     FROM ybvoyager_visualizer.ybvoyager_visualizer_metadata
		     WHERE migration_uuid = $1::uuid
		     ORDER BY migration_phase, invocation_sequence DESC
		 ) latest
		 -- Emit phases in workflow order (ac-voyager.md §3.1): assess ->
		 -- export-schema -> analyze-schema -> import-schema -> export-data ->
		 -- import-data. import-schema (5) sorts before export-data (4) because
		 -- the migrate job imports schema before it exports data.
		 ORDER BY CASE migration_phase
		     WHEN 1 THEN 1  -- assess
		     WHEN 2 THEN 2  -- export-schema
		     WHEN 3 THEN 3  -- analyze-schema
		     WHEN 5 THEN 4  -- import-schema
		     WHEN 4 THEN 5  -- export-data
		     WHEN 6 THEN 6  -- import-data
		     ELSE 7
		 END, migration_phase`,
		migrationID.String())
	if isVisualizerAbsent(err) {
		return []Phase{}, nil
	}
	if err != nil {
		return nil, fmt.Errorf("query latest Voyager phases: %w", err)
	}
	defer rows.Close()

	phases := make([]Phase, 0)
	for rows.Next() {
		var (
			number    int
			status    string
			updatedAt *time.Time
		)
		if err := rows.Scan(&number, &status, &updatedAt); err != nil {
			return nil, fmt.Errorf("scan latest Voyager phase: %w", err)
		}
		phaseNumber := PhaseNumber(number)
		phase := Phase{
			Phase:  phaseNumber,
			Name:   phaseNumber.Name(),
			Status: normalizePhaseStatus(status),
		}
		if updatedAt != nil {
			phase.UpdatedAt = *updatedAt
		}
		phases = append(phases, phase)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("iterate latest Voyager phases: %w", err)
	}
	return phases, nil
}

// LatestDataProgress reads Voyager's latest-state metrics table (phases 4 and 6
// only) without creating or modifying it. It returns nil when no metrics exist
// yet or the visualizer schema is absent.
func (s *YbStore) LatestDataProgress(ctx context.Context, migrationID uuid.UUID) (*DataProgress, error) {
	rows, err := s.pool.Query(ctx,
		`SELECT migration_phase,
		        COALESCE(schema_name, ''),
		        COALESCE(table_name, ''),
		        COALESCE(status, 0),
		        COALESCE(count_live_rows, 0),
		        COALESCE(count_total_rows, 0)
		 FROM ybvoyager_visualizer.ybvoyager_visualizer_table_metrics
		 WHERE migration_uuid = $1::uuid
		   AND migration_phase IN (4, 6)
		 ORDER BY migration_phase, schema_name, table_name`,
		migrationID.String())
	if isVisualizerAbsent(err) {
		return nil, nil
	}
	if err != nil {
		return nil, fmt.Errorf("query Voyager table metrics: %w", err)
	}
	defer rows.Close()

	progress := &DataProgress{}
	for rows.Next() {
		var (
			phaseNumber  int
			statusNumber int
			table        TableProgress
		)
		if err := rows.Scan(
			&phaseNumber,
			&table.Schema,
			&table.Table,
			&statusNumber,
			&table.RowsCompleted,
			&table.RowsTotal,
		); err != nil {
			return nil, fmt.Errorf("scan Voyager table metric: %w", err)
		}
		table.Status = normalizeTableStatus(statusNumber)
		switch PhaseNumber(phaseNumber) {
		case PhaseExportData:
			if progress.Export == nil {
				progress.Export = &PhaseDataProgress{Tables: []TableProgress{}}
			}
			addTableProgress(progress.Export, table)
		case PhaseImportData:
			if progress.Import == nil {
				progress.Import = &PhaseDataProgress{Tables: []TableProgress{}}
			}
			addTableProgress(progress.Import, table)
		}
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("iterate Voyager table metrics: %w", err)
	}
	if progress.Export == nil && progress.Import == nil {
		return nil, nil
	}
	return progress, nil
}

// ReadVoyagerStatus composes the two read-only visualizer projections. It
// returns nil when Voyager has produced nothing yet.
func (s *YbStore) ReadVoyagerStatus(ctx context.Context, migrationID uuid.UUID) (*VoyagerStatus, error) {
	phases, err := s.LatestPhases(ctx, migrationID)
	if err != nil {
		return nil, err
	}
	dataProgress, err := s.LatestDataProgress(ctx, migrationID)
	if err != nil {
		return nil, err
	}
	if len(phases) == 0 && dataProgress == nil {
		return nil, nil
	}
	return &VoyagerStatus{
		Phases:       phases,
		DataProgress: dataProgress,
	}, nil
}

// LatestCompletedAssessmentPayload returns the newest completed phase-1
// payload. A missing migration is ErrNotFound; missing visualizer objects and
// missing completed rows both mean an assessment is not available yet
// (ErrAssessmentNotFound).
func (s *YbStore) LatestCompletedAssessmentPayload(
	ctx context.Context,
	migrationID uuid.UUID,
) (json.RawMessage, error) {
	// migrationExists returns ErrNotFound when the migration is unknown.
	if err := s.migrationExists(ctx, migrationID); err != nil {
		return nil, err
	}

	var payload string
	err := s.pool.QueryRow(ctx,
		`SELECT payload
		 FROM ybvoyager_visualizer.ybvoyager_visualizer_metadata
		 WHERE migration_uuid = $1::uuid
		   AND migration_phase = 1
		   AND UPPER(status) = 'COMPLETED'
		   AND payload IS NOT NULL
		 ORDER BY invocation_sequence DESC
		 LIMIT 1`,
		migrationID.String()).Scan(&payload)
	if isVisualizerAbsent(err) || errors.Is(err, pgx.ErrNoRows) {
		return nil, ErrAssessmentNotFound
	}
	if err != nil {
		return nil, fmt.Errorf("get completed Voyager assessment payload: %w", err)
	}
	return append(json.RawMessage(nil), payload...), nil
}

// GetAssessment reads and projects the latest completed Voyager assessment.
func (s *YbStore) GetAssessment(ctx context.Context, migrationID uuid.UUID) (Assessment, error) {
	payload, err := s.LatestCompletedAssessmentPayload(ctx, migrationID)
	if err != nil {
		return Assessment{}, err
	}
	return ParseAssessmentPayload(payload)
}

// addTableProgress accumulates a phase's totals across tables. Voyager keeps one
// row per table and overwrites it with the running count, so we add the current
// number from each distinct table — never old + new snapshots of the same table.
func addTableProgress(progress *PhaseDataProgress, table TableProgress) {
	progress.RowsCompleted += table.RowsCompleted
	progress.RowsTotal += table.RowsTotal
	progress.Tables = append(progress.Tables, table)
}

func normalizePhaseStatus(status string) PhaseStatus {
	switch strings.ToUpper(strings.TrimSpace(status)) {
	case "COMPLETED":
		return PhaseCompleted
	case "IN PROGRESS":
		return PhaseInProgress
	default:
		return PhaseNotStarted
	}
}

func normalizeTableStatus(status int) TableStatus {
	switch status {
	case 1:
		return TableInProgress
	case 2:
		return TableDone
	case 3:
		return TableCompleted
	default:
		return TableNotStarted
	}
}

// isVisualizerAbsent reports whether err is Voyager not having created its
// visualizer objects yet: undefined_table (42P01) or invalid_schema_name
// (3F000).
func isVisualizerAbsent(err error) bool {
	var pgErr *pgconn.PgError
	if !errors.As(err, &pgErr) {
		return false
	}
	return pgErr.Code == "42P01" || pgErr.Code == "3F000"
}
