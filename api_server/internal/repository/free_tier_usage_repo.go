package repository

import (
	"database/sql"
	"fmt"
)

// MetricDatapackCreation is the free_tier_usage metric_name for
// lifetime datapack creations. Incremented by DatapackRepo.Create
// (never decremented on delete) so the free-tier datapack cap counts
// creations, not currently-held datapacks — deleting a datapack does
// not refund quota.
const MetricDatapackCreation = "datapack_creation"

// FreeTierUsageRepo wraps the `meko_system.free_tier_usage` counter
// table. Most metrics are atomically incremented by meko-mcp-server's
// free-tier-quota gate (see meko-mcp-server PR #151); api_server
// writes only MetricDatapackCreation (via IncrementRequestCountTx
// inside DatapackRepo.Create's transaction). The table is the single
// source of truth for per-user / per-tool lifetime request counts.
type FreeTierUsageRepo struct {
	db *sql.DB
}

func NewFreeTierUsageRepo(db *sql.DB) *FreeTierUsageRepo {
	return &FreeTierUsageRepo{db: db}
}

// RequestCount returns the lifetime request count for a specific
// (user_id, metric_name). PK index → O(1) lookup. Returns 0 when no
// row exists yet (user has never invoked the metric).
//
// metric_name examples: "knowledgebase_search", "memory_search",
// "memory_add", "conversation_add_message".
func (r *FreeTierUsageRepo) RequestCount(userID, metricName string) (int64, error) {
	var count int64
	err := r.db.QueryRow(
		`SELECT request_count
		 FROM   meko_system.free_tier_usage
		 WHERE  user_id = $1 AND metric_name = $2`,
		userID, metricName,
	).Scan(&count)
	if err == sql.ErrNoRows {
		return 0, nil
	}
	if err != nil {
		return 0, fmt.Errorf("free_tier_usage: read %s for %s: %w", metricName, userID, err)
	}
	return count, nil
}

// IncrementRequestCountTx bumps the lifetime counter for
// (user_id, metric_name) by one inside the caller's transaction, so
// the increment commits or rolls back atomically with the operation
// being counted. Package-level (not a method) because callers hold a
// *sql.Tx, not this repo.
func IncrementRequestCountTx(tx *sql.Tx, userID, metricName string) error {
	if tx == nil {
		return fmt.Errorf("free_tier_usage: transaction is nil")
	}
	_, err := tx.Exec(
		`INSERT INTO meko_system.free_tier_usage (user_id, metric_name, request_count, updated_at)
		 VALUES ($1, $2, 1, now())
		 ON CONFLICT (user_id, metric_name)
		 DO UPDATE SET request_count = meko_system.free_tier_usage.request_count + 1,
		               updated_at    = now()`,
		userID, metricName,
	)
	if err != nil {
		return fmt.Errorf("free_tier_usage: increment %s for %s: %w", metricName, userID, err)
	}
	return nil
}
