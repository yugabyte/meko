package admin

import (
	"context"
	"fmt"
	"time"
)

// CountDatapacksAll returns the total number of datapacks across all users.
// start/endExclusive optionally window by created_at.
func (r *AdminRepo) CountDatapacksAll(start, endExclusive *time.Time) (int64, error) {
	return r.CountDatapacksAllContext(context.Background(), start, endExclusive)
}

// CountDatapacksAllContext is CountDatapacksAll under the caller's context, so a
// metrics scrape can bound the query with a deadline the pool has no
// statement_timeout to enforce on its own.
func (r *AdminRepo) CountDatapacksAllContext(ctx context.Context, start, endExclusive *time.Time) (int64, error) {
	pred, args := rangePredicate("created_at", start, endExclusive, 1)
	where := ""
	if pred != "" {
		where = " WHERE TRUE" + pred
	}
	q := `SELECT COUNT(*) FROM meko_system.datapacks` + where
	var n int64
	if err := r.systemDB.QueryRowContext(ctx, q, args...).Scan(&n); err != nil {
		return 0, fmt.Errorf("count all datapacks: %w", err)
	}
	return n, nil
}

// DatapackWeekCount is one week's datapack-creation count for the executive
// dashboard's week-over-week chart. WeekStart is the Monday of the ISO week
// (YYYY-MM-DD); only weeks with at least one datapack are returned (the UI
// gap-fills the rest across the selected range).
type DatapackWeekCount struct {
	WeekStart string
	Count     int64
}

// CountDatapacksByWeek groups datapack creation into ISO weeks (Monday-start)
// within the optional [start, endExclusive) window, oldest-first, for the
// week-over-week chart.
//
// Weeks are bucketed in UTC (`created_at AT TIME ZONE 'UTC'`), not the session
// TimeZone: the handler parses the range bounds as UTC and the meko_admin chart
// gap-fills its axis in UTC, so pinning the bucket boundary to UTC keeps all
// three aligned regardless of where api_server runs. A session-zone bucket would
// drift the Monday and drop the first partial week (MEKO-605 review).
func (r *AdminRepo) CountDatapacksByWeek(start, endExclusive *time.Time) ([]DatapackWeekCount, error) {
	pred, args := rangePredicate("created_at", start, endExclusive, 1)
	rows, err := r.systemDB.Query(
		`SELECT to_char(date_trunc('week', created_at AT TIME ZONE 'UTC'), 'YYYY-MM-DD') AS week_start,
		        COUNT(*)::bigint
		 FROM meko_system.datapacks
		 WHERE TRUE`+pred+`
		 GROUP BY 1
		 ORDER BY 1`,
		args...,
	)
	if err != nil {
		return nil, fmt.Errorf("count datapacks by week: %w", err)
	}
	defer rows.Close()

	out := []DatapackWeekCount{}
	for rows.Next() {
		var w DatapackWeekCount
		if err := rows.Scan(&w.WeekStart, &w.Count); err != nil {
			return nil, fmt.Errorf("scan datapacks by week: %w", err)
		}
		out = append(out, w)
	}
	return out, rows.Err()
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
