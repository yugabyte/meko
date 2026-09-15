package repository

import (
	"context"
	"database/sql"
	"fmt"
	"time"

	"meko/api_server/internal/models"
)

// DatapackMetricsRepo reads all per-datapack analytics metrics in one call.
//
// systemDB serves meko_system.* (token_savings_summary, agent_access,
// datapack_daily_activity). dataDB serves datapacks.* (conversation_history_search,
// meko_mem0_collection) — the same split as the rest of the API server repos.
type DatapackMetricsRepo struct {
	systemDB *sql.DB
	dataDB   *sql.DB
}

// NewDatapackMetricsRepo creates a new DatapackMetricsRepo.
func NewDatapackMetricsRepo(systemDB, dataDB *sql.DB) *DatapackMetricsRepo {
	return &DatapackMetricsRepo{
		systemDB: systemDB,
		dataDB:   dataDB,
	}
}

// GetMetrics returns all analytics metrics for the given datapack.
func (r *DatapackMetricsRepo) GetMetrics(ctx context.Context, datapackID string) (*models.DatapackMetrics, error) {
	return r.fetchMetrics(ctx, datapackID)
}

func (r *DatapackMetricsRepo) fetchMetrics(ctx context.Context, datapackID string) (*models.DatapackMetrics, error) {
	m := &models.DatapackMetrics{DatapackID: datapackID}

	if err := r.fetchSavings(ctx, datapackID, m); err != nil {
		return nil, err
	}
	if err := r.fetchAccess(ctx, datapackID, m); err != nil {
		return nil, err
	}
	if err := r.fetchDailyActivity(ctx, datapackID, m); err != nil {
		return nil, err
	}
	if err := r.fetchConversationCount(ctx, datapackID, m); err != nil {
		return nil, err
	}
	if err := r.fetchMemoryExtractionCount(ctx, datapackID, m); err != nil {
		return nil, err
	}

	if m.TokensIfRawTotal > 0 {
		m.SavingsPct = float64(m.TokensSavedTotal) / float64(m.TokensIfRawTotal)
	}
	if m.ConversationCount > 0 {
		m.ExtractionRate = float64(m.MemoryExtractionCount) / float64(m.ConversationCount)
	}

	return m, nil
}

func (r *DatapackMetricsRepo) fetchSavings(ctx context.Context, datapackID string, m *models.DatapackMetrics) error {
	rows, err := r.systemDB.QueryContext(ctx,
		`SELECT request_count, tokens_saved_total, tokens_if_raw_total,
		        score_high_total, score_mid_total, score_low_total
		 FROM meko_system.token_savings_summary
		 WHERE datapack_id = $1::uuid`,
		datapackID,
	)
	if err != nil {
		return fmt.Errorf("failed to read token savings: %w", err)
	}
	defer rows.Close()

	for rows.Next() {
		var reqCount, saved, ifRaw int
		var hi, mid, lo sql.NullInt64
		if err := rows.Scan(&reqCount, &saved, &ifRaw, &hi, &mid, &lo); err != nil {
			return fmt.Errorf("failed to scan token savings row: %w", err)
		}
		m.TotalRequests += reqCount
		m.TokensSavedTotal += saved
		m.TokensIfRawTotal += ifRaw
		m.ScoreHighTotal += int(hi.Int64)
		m.ScoreMidTotal += int(mid.Int64)
		m.ScoreLowTotal += int(lo.Int64)
	}
	return rows.Err()
}

func (r *DatapackMetricsRepo) fetchAccess(ctx context.Context, datapackID string, m *models.DatapackMetrics) error {
	var lastAccessedAt sql.NullTime
	err := r.systemDB.QueryRowContext(ctx,
		`SELECT COUNT(*) AS agent_count,
		        COUNT(DISTINCT user_id) AS human_count,
		        MAX(created_at) AS last_accessed_at
		 FROM meko_system.agent_access
		 WHERE datapack_id = $1::uuid`,
		datapackID,
	).Scan(&m.AgentCount, &m.HumanCount, &lastAccessedAt)
	if err != nil {
		return fmt.Errorf("failed to read agent access: %w", err)
	}
	if lastAccessedAt.Valid {
		t := lastAccessedAt.Time
		m.LastAccessedAt = &t
	}
	return nil
}

func (r *DatapackMetricsRepo) fetchDailyActivity(ctx context.Context, datapackID string, m *models.DatapackMetrics) error {
	rows, err := r.systemDB.QueryContext(ctx,
		`SELECT activity_date, search_type, query_count
		 FROM meko_system.datapack_daily_activity
		 WHERE datapack_id = $1::uuid
		   AND activity_date >= CURRENT_DATE - INTERVAL '30 days'
		 ORDER BY activity_date ASC`,
		datapackID,
	)
	if err != nil {
		return fmt.Errorf("failed to read daily activity: %w", err)
	}
	defer rows.Close()

	// Accumulate per-day across search_type rows, then emit in order.
	type dayBucket struct {
		index  int
		point  models.DailyActivityPoint
	}
	byDate := make(map[string]*dayBucket)
	var dateOrder []string

	for rows.Next() {
		var date time.Time
		var searchType string
		var count int
		if err := rows.Scan(&date, &searchType, &count); err != nil {
			return fmt.Errorf("failed to scan daily activity row: %w", err)
		}
		key := date.Format("2006-01-02")
		b, exists := byDate[key]
		if !exists {
			b = &dayBucket{index: len(dateOrder), point: models.DailyActivityPoint{Date: key}}
			byDate[key] = b
			dateOrder = append(dateOrder, key)
		}
		switch searchType {
		case "hybrid":
			b.point.Hybrid += count
		case "kb":
			b.point.KB += count
		case "memory":
			b.point.Memory += count
		}
	}
	if err := rows.Err(); err != nil {
		return err
	}

	m.DailyActivity = make([]models.DailyActivityPoint, len(dateOrder))
	for _, key := range dateOrder {
		b := byDate[key]
		m.DailyActivity[b.index] = b.point
	}
	return nil
}

func (r *DatapackMetricsRepo) fetchConversationCount(ctx context.Context, datapackID string, m *models.DatapackMetrics) error {
	err := r.dataDB.QueryRowContext(ctx,
		`SELECT COUNT(DISTINCT conversation_id)
		 FROM datapacks.conversation_history_search
		 WHERE tenant_id = $1::uuid`,
		datapackID,
	).Scan(&m.ConversationCount)
	if err != nil {
		return fmt.Errorf("failed to read conversation count: %w", err)
	}
	return nil
}

func (r *DatapackMetricsRepo) fetchMemoryExtractionCount(ctx context.Context, datapackID string, m *models.DatapackMetrics) error {
	err := r.dataDB.QueryRowContext(ctx,
		`SELECT COUNT(*)
		 FROM datapacks.meko_mem0_collection
		 WHERE meko_datapack_id = $1::uuid`,
		datapackID,
	).Scan(&m.MemoryExtractionCount)
	if err != nil {
		return fmt.Errorf("failed to read memory extraction count: %w", err)
	}
	return nil
}
