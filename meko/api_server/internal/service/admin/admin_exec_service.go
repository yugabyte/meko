// Package admin: admin_exec_service assembles the executive dashboard for the
// meko_admin console. The dashboard is split across four endpoints — growth
// (users + datapacks), engagement (conversations + promoted memories), knowledge
// (KB docs/chunks/by-type), and savings (token burn) — so the frontend fetches
// them in parallel and each panel renders on its own timeline instead of the
// whole page waiting on the slowest (KB pipeline / Langfuse) query. Every count
// is windowed to the dashboard's [start, end] range.
package admin

import (
	"context"
	"log/slog"
	"time"

	adminrepo "meko/api_server/internal/repository/admin"
	"meko/api_server/internal/service"
)

// Executive leaderboard tuning: show the top 10 users by tokens saved. There is
// deliberately no minimum-request threshold — any user who saved tokens in the
// window is eligible.
const execTopUsersLimit = 10

// ExecUserSavings is one user's rolled-up token savings for the leaderboard,
// attributed to the user who ran the searches. RequestCount and TokensSaved
// are raw counters; the UI derives the efficiency % and dollars saved from
// them (same math as meko_ui).
type ExecUserSavings struct {
	UserID       string `json:"user_id"`
	Email        string `json:"email"`
	FirstName    string `json:"first_name"`
	LastName     string `json:"last_name"`
	RequestCount int64  `json:"request_count"`
	TokensSaved  int64  `json:"tokens_saved"`
}

// ExecDocTypeCount is one file-type bucket for the "indexed documents by file
// type" chart. Type is the stored MIME type; the UI maps it to a friendly label.
type ExecDocTypeCount struct {
	Type  string `json:"type"`
	Count int64  `json:"count"`
}

// ExecWeekCount is one week's datapack-creation count for the week-over-week
// chart. WeekStart is the Monday of the ISO week (YYYY-MM-DD).
type ExecWeekCount struct {
	WeekStart string `json:"week_start"`
	Count     int64  `json:"count"`
}

// ExecGrowth is the GET /admin/executive/growth payload: users and datapacks
// created in range, plus the week-over-week datapack trend. Control plane, fast.
type ExecGrowth struct {
	UsersCreated     int64           `json:"users_created"`
	DatapacksCreated int64           `json:"datapacks_created"`
	DatapacksByWeek  []ExecWeekCount `json:"datapacks_by_week"`
}

// ExecEngagement is the GET /admin/executive/engagement payload: conversations
// (total + by agent, Langfuse) and promoted memories (data plane), in range.
type ExecEngagement struct {
	Conversations        int64                              `json:"conversations"`
	ConversationsByAgent []adminrepo.AgentConversationCount `json:"conversations_by_agent"`
	PromotedMemories     int64                              `json:"promoted_memories"`
	LangfuseEnabled      bool                               `json:"langfuse_enabled"`
}

// ExecKnowledge is the GET /admin/executive/knowledge payload: indexed KB
// documents + chunks, and indexed docs grouped by file type, in range.
type ExecKnowledge struct {
	KBDocuments       int64              `json:"kb_documents"`
	KBChunks          int64              `json:"kb_chunks"`
	KBDocumentsByType []ExecDocTypeCount `json:"kb_documents_by_type"`
}

// ExecSavings is the GET /admin/executive/savings payload: account-wide
// contextual-search requests + tokens saved (feed the hero % / dollars) and the
// top savers leaderboard, in range.
type ExecSavings struct {
	TotalRequests    int64             `json:"total_requests"`
	TotalTokensSaved int64             `json:"total_tokens_saved"`
	TopUsers         []ExecUserSavings `json:"top_users"`
}

// ExecutiveSummary is the legacy combined payload for GET /admin/token-savings.
// It was shaped for the meko_ui in-app admin console, removed in MEKO-655, and
// has no consumer now — meko_admin uses the four /admin/executive/* panels. The
// shim assembles its subset from the same methods those panels use.
type ExecutiveSummary struct {
	TotalUsers         int64             `json:"total_users"`
	TotalDatapacks     int64             `json:"total_datapacks"`
	TotalConversations int64             `json:"total_conversations"`
	LangfuseEnabled    bool              `json:"langfuse_enabled"`
	TotalRequests      int64             `json:"total_requests"`
	TotalTokensSaved   int64             `json:"total_tokens_saved"`
	TopUsers           []ExecUserSavings `json:"top_users"`
}

// Growth: users + datapacks created in [start, end], plus the weekly trend.
// The two counts are required (control plane); the trend is best-effort.
func (s *AdminMetricsService) Growth(ctx context.Context, start, end *time.Time) (ExecGrowth, error) {
	out := ExecGrowth{DatapacksByWeek: []ExecWeekCount{}}
	endExclusive := endExclusiveBound(end)

	total, _, _, _, err := s.repo.CountUsersByTier(ctx, start, endExclusive)
	if err != nil {
		return out, err
	}
	out.UsersCreated = total

	dpCount, err := s.repo.CountDatapacksAll(start, endExclusive)
	if err != nil {
		return out, err
	}
	out.DatapacksCreated = dpCount

	if weeks, wErr := s.repo.CountDatapacksByWeek(start, endExclusive); wErr != nil {
		slog.Error("exec growth: datapacks by week", "error", wErr)
	} else {
		for _, w := range weeks {
			out.DatapacksByWeek = append(out.DatapacksByWeek, ExecWeekCount{WeekStart: w.WeekStart, Count: w.Count})
		}
	}
	return out, nil
}

// Engagement: conversations (Langfuse, scoped to this deployment's users) and
// promoted memories (data plane) in range. All best-effort — a Langfuse/data
// outage zeroes only this panel.
func (s *AdminMetricsService) Engagement(ctx context.Context, start, end *time.Time) (ExecEngagement, error) {
	out := ExecEngagement{ConversationsByAgent: []adminrepo.AgentConversationCount{}}
	endExclusive := endExclusiveBound(end)

	if n, pErr := s.repo.CountAllPromoted(ctx, start, endExclusive); pErr != nil {
		slog.Error("exec engagement: promoted memories", "error", pErr)
	} else {
		out.PromotedMemories = n
	}

	// Conversations: scoped to this deployment's users so the shared Langfuse DB
	// never leaks other deployments' traces.
	if s.repo.LangfuseConfigured() {
		out.LangfuseEnabled = true
		if userIDs, uErr := s.repo.ListUserIDs(ctx); uErr != nil {
			slog.Error("exec engagement: list user ids; skipping conversations", "error", uErr)
		} else if len(userIDs) > 0 {
			if n, cErr := s.repo.TotalConversations(userIDs, start, endExclusive); cErr != nil {
				slog.Warn("exec engagement: total conversations", "error", cErr)
			} else {
				out.Conversations = n
			}
			if rows, aErr := s.repo.ConversationsByAgent(userIDs, start, endExclusive); aErr != nil {
				slog.Warn("exec engagement: conversations by agent", "error", aErr)
			} else {
				out.ConversationsByAgent = rows
			}
		}
	}
	return out, nil
}

// Knowledge: indexed KB documents + chunks and the by-file-type breakdown in
// range. Best-effort (data plane). The tiles and the chart come from one
// pipeline scan (CountIndexedDocsBreakdownAll) rather than two, which is what
// keeps this panel from timing out for a heavy deployment.
func (s *AdminMetricsService) Knowledge(_ context.Context, start, end *time.Time) (ExecKnowledge, error) {
	out := ExecKnowledge{KBDocumentsByType: []ExecDocTypeCount{}}
	endExclusive := endExclusiveBound(end)

	kb, err := s.repo.CountIndexedDocsBreakdownAll(
		service.DatapackIndexSchema, service.DatapackIndexName, start, endExclusive,
	)
	if err != nil {
		slog.Error("exec knowledge: kb breakdown", "error", err)
		return out, nil
	}
	out.KBDocuments, out.KBChunks = kb.Documents, kb.Chunks
	for _, t := range kb.ByType {
		out.KBDocumentsByType = append(out.KBDocumentsByType, ExecDocTypeCount{Type: t.DocumentType, Count: t.Count})
	}
	return out, nil
}

// Savings: contextual-search requests + tokens saved and the top savers in
// range. Control plane (token_savings_log); required.
func (s *AdminMetricsService) Savings(_ context.Context, start, end *time.Time) (ExecSavings, error) {
	out := ExecSavings{TopUsers: []ExecUserSavings{}}
	endExclusive := endExclusiveBound(end)

	req, saved, err := s.tokenSavings(start, endExclusive)
	if err != nil {
		return out, err
	}
	out.TotalRequests, out.TotalTokensSaved = req, saved

	rows, err := s.repo.TopUserTokenSavings(start, endExclusive, execTopUsersLimit)
	if err != nil {
		return out, err
	}
	for _, r := range rows {
		out.TopUsers = append(out.TopUsers, ExecUserSavings{
			UserID:       r.UserID,
			Email:        r.Email,
			FirstName:    r.FirstName,
			LastName:     r.LastName,
			RequestCount: r.RequestCount,
			TokensSaved:  r.TokensSaved,
		})
	}
	return out, nil
}

// tokenSavings reads the cheaper maintained summary counters for the all-time
// (unbounded) case, and aggregates the append-only log when a range is set.
func (s *AdminMetricsService) tokenSavings(start, endExclusive *time.Time) (requests, tokensSaved int64, err error) {
	if start == nil && endExclusive == nil {
		return s.repo.TotalTokenSavings()
	}
	return s.repo.TotalTokenSavingsInRange(start, endExclusive)
}

// Executive is the legacy combined rollup behind GET /admin/token-savings (see
// ExecutiveSummary for why it still exists). It fetches ONLY
// the fields ExecutiveSummary exposes — the user + datapack totals directly (not
// the full Growth panel, which also runs the week-over-week CountDatapacksByWeek
// aggregate ExecutiveSummary has no field for), the Savings panel, and the
// conversation total + langfuse flag. It skips the Engagement panel entirely
// (CountAllPromoted / ConversationsByAgent), so no wasted data-plane or Langfuse
// hit fires on every meko_ui load.
func (s *AdminMetricsService) Executive(ctx context.Context, start, end *time.Time) (ExecutiveSummary, error) {
	out := ExecutiveSummary{TopUsers: []ExecUserSavings{}}

	endExclusive := endExclusiveBound(end)
	users, _, _, _, err := s.repo.CountUsersByTier(ctx, start, endExclusive)
	if err != nil {
		return out, err
	}
	dpCount, err := s.repo.CountDatapacksAll(start, endExclusive)
	if err != nil {
		return out, err
	}
	savings, err := s.Savings(ctx, start, end)
	if err != nil {
		return out, err
	}

	out.TotalUsers = users
	out.TotalDatapacks = dpCount
	out.TotalRequests = savings.TotalRequests
	out.TotalTokensSaved = savings.TotalTokensSaved
	out.TopUsers = savings.TopUsers

	// Conversation total only (best-effort), scoped to this deployment's users.
	if s.repo.LangfuseConfigured() {
		out.LangfuseEnabled = true
		if userIDs, uErr := s.repo.ListUserIDs(ctx); uErr != nil {
			slog.Error("exec (legacy): list user ids; skipping conversations", "error", uErr)
		} else if len(userIDs) > 0 {
			if n, cErr := s.repo.TotalConversations(userIDs, start, endExclusiveBound(end)); cErr != nil {
				slog.Warn("exec (legacy): total conversations", "error", cErr)
			} else {
				out.TotalConversations = n
			}
		}
	}
	return out, nil
}
