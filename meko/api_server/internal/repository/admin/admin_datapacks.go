package admin

import (
	"fmt"
	"time"
)

// CountDatapacksAll returns the total number of datapacks across all users.
// start/endExclusive optionally window by created_at.
func (r *AdminRepo) CountDatapacksAll(start, endExclusive *time.Time) (int64, error) {
	pred, args := rangePredicate("created_at", start, endExclusive, 1)
	where := ""
	if pred != "" {
		where = " WHERE TRUE" + pred
	}
	var n int64
	if err := r.systemDB.QueryRow(
		`SELECT COUNT(*) FROM meko_system.datapacks`+where,
		args...,
	).Scan(&n); err != nil {
		return 0, fmt.Errorf("count all datapacks: %w", err)
	}
	return n, nil
}

// CountDatapacksByOwner returns the all-time number of datapacks owned by
// `userID` (per-user admin metric). Shared-in datapacks are NOT counted.
func (r *AdminRepo) CountDatapacksByOwner(userID string) (int64, error) {
	var n int64
	if err := r.systemDB.QueryRow(
		`SELECT COUNT(*) FROM meko_system.datapacks WHERE user_id = $1`,
		userID,
	).Scan(&n); err != nil {
		return 0, fmt.Errorf("count datapacks for user %s: %w", userID, err)
	}
	return n, nil
}
