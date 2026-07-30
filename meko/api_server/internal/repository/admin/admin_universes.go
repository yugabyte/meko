package admin

import (
	"database/sql"
	"errors"
	"fmt"
)

// UniverseRow is the meko_system.yb_universes info needed to query YBA for a
// universe's live configuration and metrics.
type UniverseRow struct {
	UniverseUUID  string
	ManagementURL string
	Name          string
}

// GetDefaultUniverse returns the oldest registered universe — the single
// deployment universe today. Returns (nil, nil) when none is registered so
// the caller can degrade gracefully rather than error.
func (r *AdminRepo) GetDefaultUniverse() (*UniverseRow, error) {
	var u UniverseRow
	err := r.systemDB.QueryRow(
		`SELECT yb_universe_uuid::text, yb_management_url, yb_universe_name
		 FROM meko_system.yb_universes
		 ORDER BY created_at ASC
		 LIMIT 1`,
	).Scan(&u.UniverseUUID, &u.ManagementURL, &u.Name)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, nil
	}
	if err != nil {
		return nil, fmt.Errorf("get default universe: %w", err)
	}
	return &u, nil
}
