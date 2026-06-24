package repository

import (
	"database/sql"
	"fmt"
)

// FreeTierUsageRepo wraps reads of the `meko_system.free_tier_usage`
// counter table that meko-mcp-server's free-tier-quota gate
// atomically increments (see meko-mcp-server PR #151). The table is
// the single source of truth for per-user / per-tool request counts;
// this repo only reads (writes happen from MCP).
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
