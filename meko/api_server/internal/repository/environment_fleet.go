package repository

import (
	"context"
	"database/sql"
	"fmt"
	"time"
)

// FleetRow is the slice of an environment the control plane's fleet metrics read.
type FleetRow struct {
	LastHeartbeatAt *time.Time
	UUID            string
	Kind            string
	Type            string
	State           string
	AgentVersion    string
}

// FleetSnapshot reads every environment, deregistered ones included, for the fleet
// metrics. The whole table rather than SQL aggregates: a fleet is hundreds of rows
// read once a minute, and grouping in Go keeps the series shapes testable without a
// database.
func (r *EnvironmentRepo) FleetSnapshot(ctx context.Context) ([]FleetRow, error) {
	rows, err := r.db.QueryContext(ctx, `
		SELECT e.uuid, COALESCE(e.kind, 'meko'), COALESCE(e.type, 'BYOC'), e.state,
		       e.agent_version, e.last_heartbeat_at
		FROM meko_system.environments e`)
	if err != nil {
		return nil, fmt.Errorf("fleet snapshot: %w", err)
	}
	defer rows.Close()
	var out []FleetRow
	for rows.Next() {
		var f FleetRow
		var agentVersion sql.NullString
		var hb sql.NullTime
		if err := rows.Scan(&f.UUID, &f.Kind, &f.Type, &f.State, &agentVersion, &hb); err != nil {
			return nil, fmt.Errorf("scan fleet row: %w", err)
		}
		f.AgentVersion = agentVersion.String
		if hb.Valid {
			t := hb.Time
			f.LastHeartbeatAt = &t
		}
		out = append(out, f)
	}
	return out, rows.Err()
}

// CountByStatus counts relayed tasks by status across the fleet. Terminal statuses
// count only what PruneTerminal has not yet removed.
func (r *EnvironmentTaskRepo) CountByStatus(ctx context.Context) (map[string]int64, error) {
	rows, err := r.db.QueryContext(ctx,
		`SELECT status, COUNT(*) FROM meko_system.environment_tasks GROUP BY status`)
	if err != nil {
		return nil, fmt.Errorf("count environment tasks: %w", err)
	}
	defer rows.Close()
	out := map[string]int64{}
	for rows.Next() {
		var status string
		var n int64
		if err := rows.Scan(&status, &n); err != nil {
			return nil, fmt.Errorf("scan task count: %w", err)
		}
		out[status] = n
	}
	return out, rows.Err()
}
