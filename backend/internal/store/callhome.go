package store

import (
	"context"
	"time"
)

// UsageTotals is what the install holds, counted for the usage stats (internal/callhome): never
// what any of it says or is called.
type UsageTotals struct {
	Datapacks        int
	Memories         int // learnings and kept
	Learnings        int // memories waiting for a decision
	PromotedMemories int
	Artifacts        int
	Conversations    int
	KBDocuments      int
	KBBytes          int64
	DBBytes          int64
}

// UsageTotals counts the install's data.
func (s *Store) UsageTotals(ctx context.Context) (UsageTotals, error) {
	var t UsageTotals
	err := s.r.QueryRowContext(ctx, "SELECT "+
		"(SELECT count(*) FROM datapacks), "+
		"(SELECT count(*) FROM memories), "+
		"(SELECT count(*) FROM memories WHERE promotion_action = ''), "+
		"(SELECT count(*) FROM promoted_memories), "+
		"(SELECT count(*) FROM artifacts), "+
		"(SELECT count(*) FROM conversations), "+
		"(SELECT count(*) FROM kb_documents), "+
		"(SELECT COALESCE(sum(size_bytes), 0) FROM kb_documents), "+
		"(SELECT page_count FROM pragma_page_count()) * (SELECT page_size FROM pragma_page_size())").
		Scan(&t.Datapacks, &t.Memories, &t.Learnings, &t.PromotedMemories, &t.Artifacts, &t.Conversations,
			&t.KBDocuments, &t.KBBytes, &t.DBBytes)
	return t, err
}

// CallDay is the call log's calls on one UTC day by one tool and one agent, summed.
type CallDay struct {
	Day         string // YYYY-MM-DD
	Tool        string
	AgentID     string
	Calls       int
	Errors      int
	Ms          int64
	TokensSent  int64
	TokensSaved int64
}

// CallDays sums the calls since a time by UTC day, tool and agent, oldest day first.
func (s *Store) CallDays(ctx context.Context, since time.Time) ([]CallDay, error) {
	// at is RFC 3339 in UTC, so its first 10 characters are the UTC day.
	rows, err := s.r.QueryContext(ctx, "SELECT substr(at, 1, 10) AS day, tool, agent_id, count(*), "+
		"sum(CASE WHEN ok THEN 0 ELSE 1 END), sum(ms), sum(tokens_sent), sum(tokens_saved) "+
		"FROM call_log WHERE at >= ? GROUP BY day, tool, agent_id ORDER BY day, tool, agent_id",
		since.UTC().Format(timeLayout))
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []CallDay{}
	for rows.Next() {
		var d CallDay
		if err := rows.Scan(&d.Day, &d.Tool, &d.AgentID, &d.Calls, &d.Errors, &d.Ms, &d.TokensSent,
			&d.TokensSaved); err != nil {
			return nil, err
		}
		out = append(out, d)
	}
	return out, rows.Err()
}
