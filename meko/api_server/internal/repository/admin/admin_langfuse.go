package admin

import (
	"fmt"
	"time"

	"github.com/lib/pq"
)

// AgentConversationCount is one row of ConversationsByAgent: distinct
// conversations attributed to a single agent_id.
type AgentConversationCount struct {
	Agent         string `json:"agent"`
	Conversations int64  `json:"conversations"`
}

// ConversationsByAgent counts distinct conversations per agent_id, scoped
// to userIDs (no user filter when the slice is empty). Traces without an
// agent_id collapse into "unknown". start/endExclusive optionally window
// by the trace timestamp.
func (r *AdminRepo) ConversationsByAgent(userIDs []string, start, endExclusive *time.Time) ([]AgentConversationCount, error) {
	if !r.LangfuseConfigured() {
		return []AgentConversationCount{}, nil
	}
	query := `
		SELECT COALESCE(NULLIF(t.metadata->>'agent_id', ''), 'unknown') AS agent,
		       COUNT(DISTINCT t.session_id)
		FROM   clickhouse.traces t
		WHERE  t.is_deleted = false`
	var args []any
	if len(userIDs) > 0 {
		query += `
		  AND  t.user_id = ANY($1)`
		args = append(args, pq.Array(userIDs))
	}
	pred, rangeArgs := rangePredicate(`t."timestamp"`, start, endExclusive, len(args)+1)
	query += pred
	args = append(args, rangeArgs...)
	query += `
		GROUP BY 1
		ORDER BY 2 DESC`

	rows, err := r.langfuseDB.Query(query, args...)
	if err != nil {
		return nil, fmt.Errorf("langfuse db: conversations by agent: %w", err)
	}
	defer func() { _ = rows.Close() }()

	out := make([]AgentConversationCount, 0)
	for rows.Next() {
		var row AgentConversationCount
		if err := rows.Scan(&row.Agent, &row.Conversations); err != nil {
			return nil, fmt.Errorf("langfuse db: scan conversations by agent: %w", err)
		}
		out = append(out, row)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("langfuse db: iterate conversations by agent: %w", err)
	}
	return out, nil
}

// TokenBurn sums GENERATION `usage_details.total` across all agents,
// scoped to userIDs (no user filter when the slice is empty).
// start/endExclusive optionally window by the trace timestamp.
func (r *AdminRepo) TokenBurn(userIDs []string, start, endExclusive *time.Time) (int64, error) {
	if !r.LangfuseConfigured() {
		return 0, nil
	}
	query := `
		SELECT COALESCE(SUM(
		           CASE WHEN o.type = 'GENERATION'
		                THEN (o.usage_details ->> 'total')::bigint
		                ELSE 0 END
		       ), 0)
		FROM   clickhouse.traces       t
		JOIN   clickhouse.observations o
		  ON   o.project_id = t.project_id
		  AND  o.trace_id   = t.id
		WHERE  t.is_deleted = false`
	var args []any
	if len(userIDs) > 0 {
		query += `
		  AND  t.user_id = ANY($1)`
		args = append(args, pq.Array(userIDs))
	}
	pred, rangeArgs := rangePredicate(`t."timestamp"`, start, endExclusive, len(args)+1)
	query += pred
	args = append(args, rangeArgs...)
	var total int64
	if err := r.langfuseDB.QueryRow(query, args...).Scan(&total); err != nil {
		return 0, fmt.Errorf("langfuse db: token burn: %w", err)
	}
	return total, nil
}

// TotalConversations counts distinct conversations, scoped to userIDs
// (no user filter when the slice is empty). start/endExclusive optionally
// window by the trace timestamp.
func (r *AdminRepo) TotalConversations(userIDs []string, start, endExclusive *time.Time) (int64, error) {
	if !r.LangfuseConfigured() {
		return 0, nil
	}
	query := `
		SELECT COUNT(DISTINCT t.session_id)
		FROM   clickhouse.traces t
		WHERE  t.is_deleted = false`
	var args []any
	if len(userIDs) > 0 {
		query += `
		  AND  t.user_id = ANY($1)`
		args = append(args, pq.Array(userIDs))
	}
	pred, rangeArgs := rangePredicate(`t."timestamp"`, start, endExclusive, len(args)+1)
	query += pred
	args = append(args, rangeArgs...)
	var total int64
	if err := r.langfuseDB.QueryRow(query, args...).Scan(&total); err != nil {
		return 0, fmt.Errorf("langfuse db: total conversations: %w", err)
	}
	return total, nil
}
