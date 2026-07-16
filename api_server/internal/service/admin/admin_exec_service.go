// Package admin: admin_exec_service assembles the executive dashboard payload
// for the meko_admin console — high-level adoption + efficiency numbers a
// VP/manager wants at a glance: total users/datapacks/conversations, the
// account-wide contextual-search token savings, and a top-N leaderboard of the
// users saving the most tokens (attributed to the user who ran each search).
// Account-wide totals are read from the maintained token_savings_summary
// counters; the per-user leaderboard from token_savings_log. The % saved and
// dollars saved are derived UI-side, mirroring meko_ui's TOKEN BURN tile so the
// numbers reconcile across the two consoles.
package admin

import (
	"context"
	"log/slog"
)

// Executive leaderboard tuning: show the top 10 users, and only count users
// with sustained adoption (>= 20 contextual-search requests) so the board
// reflects real usage rather than one-off spikes.
const (
	execTopUsersLimit = 10
	execMinRequests   = 20
)

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

// ExecutiveSummary is the GET /admin/token-savings payload. The account-wide
// TotalRequests/TotalTokensSaved feed the hero band's % + dollars-saved; the
// three headcount totals feed the top strip; TopUsers feeds the leaderboard.
type ExecutiveSummary struct {
	TotalUsers         int64             `json:"total_users"`
	TotalDatapacks     int64             `json:"total_datapacks"`
	TotalConversations int64             `json:"total_conversations"`
	LangfuseEnabled    bool              `json:"langfuse_enabled"`
	TotalRequests      int64             `json:"total_requests"`
	TotalTokensSaved   int64             `json:"total_tokens_saved"`
	TopUsers           []ExecUserSavings `json:"top_users"`
}

// Executive gathers the executive dashboard payload. Control-plane counts
// (users, datapacks, token savings) are required; the Langfuse conversation
// total is best-effort (zeroed + logged on failure, like the metrics service).
func (s *AdminMetricsService) Executive(ctx context.Context) (ExecutiveSummary, error) {
	out := ExecutiveSummary{TopUsers: []ExecUserSavings{}}

	total, _, _, err := s.repo.CountUsersByTier(ctx, nil, nil)
	if err != nil {
		return out, err
	}
	out.TotalUsers = total

	dpCount, err := s.repo.CountDatapacksAll(nil, nil)
	if err != nil {
		return out, err
	}
	out.TotalDatapacks = dpCount

	req, saved, err := s.repo.TotalTokenSavings()
	if err != nil {
		return out, err
	}
	out.TotalRequests, out.TotalTokensSaved = req, saved

	rows, err := s.repo.TopUserTokenSavings(execMinRequests, execTopUsersLimit)
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

	// Total conversations: best-effort, scoped to this deployment's users so
	// the shared Langfuse DB never leaks other deployments' traces. Any failure
	// leaves it at zero rather than failing the whole dashboard.
	if s.repo.LangfuseConfigured() {
		out.LangfuseEnabled = true
		if userIDs, uErr := s.repo.ListUserIDs(ctx); uErr != nil {
			slog.Error("exec metrics: list user ids; skipping conversations", "error", uErr)
		} else if len(userIDs) > 0 {
			if n, cErr := s.repo.TotalConversations(userIDs, nil, nil); cErr != nil {
				slog.Warn("exec metrics: total conversations", "error", cErr)
			} else {
				out.TotalConversations = n
			}
		}
	}

	return out, nil
}
