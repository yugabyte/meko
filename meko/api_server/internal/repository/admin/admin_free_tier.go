package admin

import (
	"database/sql"
	"errors"
	"fmt"
	"time"
)

// CountAuditEventsInRange counts free_tier_audit_log events for one user +
// metric within [start, endExclusive). The audit log is the append-only per-tool
// event log (one row per request), so unlike the free_tier_usage summary it can
// be windowed by date; idx_free_tier_audit_user_metric (user_id, metric_name,
// created_at) serves this. nil bounds mean unbounded on that side.
func (r *AdminRepo) CountAuditEventsInRange(
	userID, metric string, start, endExclusive *time.Time,
) (int64, error) {
	pred, rangeArgs := rangePredicate("created_at", start, endExclusive, 3)
	args := append([]any{userID, metric}, rangeArgs...)
	var n int64
	if err := r.systemDB.QueryRow(
		`SELECT COUNT(*) FROM meko_system.free_tier_audit_log
		 WHERE user_id = $1 AND metric_name = $2`+pred,
		args...,
	).Scan(&n); err != nil {
		return 0, fmt.Errorf("count audit events (%s): %w", metric, err)
	}
	return n, nil
}

// FreeTierSummaryCount returns the exact lifetime counter from the
// free_tier_usage summary for one (user, metric) — the source of truth for the
// all-time case, where the audit log's best-effort writes could undercount. 0
// when the user has no row for the metric yet.
func (r *AdminRepo) FreeTierSummaryCount(userID, metric string) (int64, error) {
	var n int64
	err := r.systemDB.QueryRow(
		`SELECT request_count FROM meko_system.free_tier_usage
		 WHERE user_id = $1 AND metric_name = $2`,
		userID, metric,
	).Scan(&n)
	if errors.Is(err, sql.ErrNoRows) {
		return 0, nil
	}
	if err != nil {
		return 0, fmt.Errorf("free tier summary count (%s): %w", metric, err)
	}
	return n, nil
}
