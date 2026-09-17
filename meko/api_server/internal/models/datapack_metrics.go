package models

import "time"

// DatapackMetrics holds all per-datapack analytics metrics for the dashboard.
type DatapackMetrics struct {
	DatapackID string `json:"datapack_id"`

	// M-02 token savings (lifetime counters from token_savings_summary)
	TotalRequests    int     `json:"total_requests"`
	TokensSavedTotal int     `json:"tokens_saved_total"`
	TokensIfRawTotal int     `json:"tokens_if_raw_total"`
	SavingsPct       float64 `json:"savings_pct"`

	// M-03 score bands
	ScoreHighTotal int `json:"score_high_total"`
	ScoreMidTotal  int `json:"score_mid_total"`
	ScoreLowTotal  int `json:"score_low_total"`

	// M-05 access counts from agent_access
	AgentCount     int        `json:"agent_count"`
	HumanCount     int        `json:"human_count"`
	LastAccessedAt *time.Time `json:"last_accessed_at,omitempty"`

	// M-06 daily query activity (last 30 days, oldest first)
	DailyActivity []DailyActivityPoint `json:"daily_activity"`

	// M-08 conversation and memory extraction counts
	ConversationCount     int     `json:"conversation_count"`
	MemoryExtractionCount int     `json:"memory_extraction_count"`
	ExtractionRate        float64 `json:"extraction_rate"`

	// M-09 chunk counts per content type (for token and size estimates)
	// Each chunk is assumed to be ~1024 tokens.
	KBChunkCount           int64 `json:"kb_chunk_count"`
	KBFileCount            int64 `json:"kb_file_count"`
	MemoryChunkCount       int64 `json:"memory_chunk_count"`
	ConversationChunkCount int64 `json:"conversation_chunk_count"`

	// M-10 storage size estimates (bytes of stored text content)
	KBBytes           int64 `json:"kb_bytes"`
	MemoryBytes       int64 `json:"memory_bytes"`
	ConversationBytes int64 `json:"conversation_bytes"`
}

// DailyActivityPoint is one day's query breakdown from datapack_daily_activity.
type DailyActivityPoint struct {
	Date   string `json:"date"`
	Hybrid int    `json:"hybrid"`
	KB     int    `json:"kb"`
	Memory int    `json:"memory"`
}
