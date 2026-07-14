// Package admin: admin_metrics_service aggregates control-plane and
// data-plane counters for the meko_admin console. Data-plane reads fail
// independently (log + 0) so a hiccup there never 500s the whole page.
package admin

import (
	"context"
	"database/sql"
	"errors"
	"log/slog"
	"time"

	adminrepo "meko/api_server/internal/repository/admin"
	"meko/api_server/internal/service"
)

// ErrUserNotFound is returned by Metrics when a per-user request names a user
// id that does not exist; the handler maps it to 404.
var ErrUserNotFound = errors.New("user not found")

// tierFree / tierPro are the two collapsed tiers the admin console
// shows: user_type='free' maps to free, everything else to pro.
const (
	tierFree = "free"
	tierPro  = "pro"
)

// AdminUser is one row of GET /admin/users.
type AdminUser struct {
	ID        string    `json:"id"`
	Email     string    `json:"email"`
	FirstName string    `json:"first_name"`
	LastName  string    `json:"last_name"`
	Tier      string    `json:"tier"`
	CreatedAt time.Time `json:"created_at"`
}

// AdminMetrics is the GET /admin/metrics payload for either the overall
// ("all") scope or a single user. Tier is nil for the overall scope.
type AdminMetrics struct {
	Scope              string  `json:"scope"`
	TotalUsers         int64   `json:"total_users"`
	FreeUsers          int64   `json:"free_users"`
	ProUsers           int64   `json:"pro_users"`
	Tier               *string `json:"tier"`
	Datapacks          int64   `json:"datapacks"`
	SharedDatapacks    int64   `json:"shared_datapacks"`
	KBDocuments        int64   `json:"kb_documents"`
	KBChunks           int64   `json:"kb_chunks"`
	PrivateMemories    int64   `json:"private_memories"`
	CollectiveMemories int64   `json:"collective_memories"`

	// Langfuse-derived (best-effort): zeroed/empty when Langfuse DB is
	// unconfigured or a read fails; langfuse_enabled reflects that.
	Conversations        int64                              `json:"conversations"`
	ConversationsByAgent []adminrepo.AgentConversationCount `json:"conversations_by_agent"`
	TokenBurn            int64                              `json:"token_burn"`
	LangfuseEnabled      bool                               `json:"langfuse_enabled"`
}

// AdminMetricsService gathers admin dashboard counters. It reads the
// control plane (users, datapacks, invites) and the data plane (KB
// pipeline, memories, Langfuse) through a single AdminRepo.
type AdminMetricsService struct {
	repo *adminrepo.AdminRepo
}

// NewAdminMetricsService wires the admin repo the admin metrics need.
func NewAdminMetricsService(repo *adminrepo.AdminRepo) *AdminMetricsService {
	return &AdminMetricsService{repo: repo}
}

// tierFor collapses a raw user_type into the admin console's two tiers.
func tierFor(userType string) string {
	if userType == tierFree {
		return tierFree
	}
	return tierPro
}

// ListUsers returns all users (newest first) for the admin console.
func (s *AdminMetricsService) ListUsers(ctx context.Context) ([]AdminUser, error) {
	rows, err := s.repo.ListUsers(ctx)
	if err != nil {
		return nil, err
	}
	out := make([]AdminUser, 0, len(rows))
	for _, r := range rows {
		out = append(out, AdminUser{
			ID:        r.UserID,
			Email:     r.Email,
			FirstName: r.FirstName,
			LastName:  r.LastName,
			Tier:      tierFor(r.UserType),
			CreatedAt: r.CreatedAt,
		})
	}
	return out, nil
}

// Metrics returns the overall dashboard when userID is "" or "all",
// otherwise the per-user breakdown. start/end (date-only, both optional)
// window every counter; nil bounds mean all-time.
func (s *AdminMetricsService) Metrics(
	ctx context.Context, userID string, start, end *time.Time,
) (AdminMetrics, error) {
	// End is inclusive of its whole day: convert to an exclusive upper
	// bound at midnight after `end` so repos filter col < endExclusive.
	endExclusive := endExclusiveBound(end)
	if userID == "" || userID == "all" {
		return s.overall(ctx, start, endExclusive)
	}
	return s.perUser(ctx, userID, start, endExclusive)
}

// endExclusiveBound turns an inclusive end date into the exclusive upper
// bound (end + 24h). Returns nil when end is nil (no upper bound).
func endExclusiveBound(end *time.Time) *time.Time {
	if end == nil {
		return nil
	}
	e := end.Add(24 * time.Hour)
	return &e
}

// overall aggregates global counters. The two data-plane groups (KB and
// memories) each fail independently: an outage there logs + zeroes that
// group without failing the whole request.
func (s *AdminMetricsService) overall(ctx context.Context, start, end *time.Time) (AdminMetrics, error) {
	m := AdminMetrics{Scope: "all"}

	// User headcount + tier split are always all-time, never date-filtered.
	total, free, pro, err := s.repo.CountUsersByTier(ctx, nil, nil)
	if err != nil {
		return m, err
	}
	m.TotalUsers, m.FreeUsers, m.ProUsers = total, free, pro

	// Datapacks are always the current total, never date-filtered.
	dpCount, err := s.repo.CountDatapacksAll(nil, nil)
	if err != nil {
		return m, err
	}
	m.Datapacks = dpCount

	shared, err := s.repo.CountSharingOwners(ctx, start, end)
	if err != nil {
		return m, err
	}
	m.SharedDatapacks = shared

	// Data-plane: KB pipeline aggregates (global).
	if kb, kbErr := s.repo.CountCompletedDocsAndChunksAll(
		service.DatapackIndexSchema, service.DatapackIndexName, start, end,
	); kbErr != nil {
		slog.Error("admin metrics: kb counts (all)", "error", kbErr)
	} else {
		m.KBDocuments, m.KBChunks = kb.Documents, kb.Chunks
	}

	// Data-plane: private + collective memory counts (global).
	if n, mErr := s.repo.CountPrivateMemoriesAll(ctx, start, end); mErr != nil {
		slog.Error("admin metrics: private memories (all)", "error", mErr)
	} else {
		m.PrivateMemories = n
	}
	if n, pErr := s.repo.CountAllPromoted(ctx, start, end); pErr != nil {
		slog.Error("admin metrics: collective memories (all)", "error", pErr)
	} else {
		m.CollectiveMemories = n
	}

	// Scope overall Langfuse aggregates to this deployment's users so the
	// total matches the sum of per-user metrics. The shared Langfuse DB also
	// holds traces from OTHER deployments, so we must NOT query it without a
	// user filter — on error we skip Langfuse entirely rather than leak
	// cross-deployment totals.
	if userIDs, err := s.repo.ListUserIDs(ctx); err != nil {
		slog.Error("admin metrics: list user ids; skipping langfuse", "error", err)
	} else {
		s.populateLangfuse(&m, userIDs, start, end)
	}
	return m, nil
}

// populateLangfuse best-effort fills conversation + token-burn metrics
// from the Langfuse DB, scoped to userIDs. Scoping the overall totals to
// this deployment's users guarantees overall == sum of per-user. Any
// failure logs a warning and leaves zeros/empty slice; it never fails
// Metrics.
func (s *AdminMetricsService) populateLangfuse(m *AdminMetrics, userIDs []string, start, end *time.Time) {
	m.ConversationsByAgent = []adminrepo.AgentConversationCount{}
	if s.repo == nil || !s.repo.LangfuseConfigured() {
		return
	}
	m.LangfuseEnabled = true

	// Defense-in-depth: never hit Langfuse with an empty user list — the shared
	// DB holds other deployments' traces, so an unfiltered query would leak
	// their totals. Empty = nothing to count for this deployment.
	if len(userIDs) == 0 {
		return
	}

	if n, err := s.repo.TotalConversations(userIDs, start, end); err != nil {
		slog.Warn("admin metrics: langfuse conversations", "error", err)
	} else {
		m.Conversations = n
	}
	if rows, err := s.repo.ConversationsByAgent(userIDs, start, end); err != nil {
		slog.Warn("admin metrics: langfuse conversations by agent", "error", err)
	} else {
		m.ConversationsByAgent = rows
	}
	if n, err := s.repo.TokenBurn(userIDs, start, end); err != nil {
		slog.Warn("admin metrics: langfuse token burn", "error", err)
	} else {
		m.TokenBurn = n
	}
}

// perUser aggregates counters scoped to a single user. total_users is
// forced to 1 and free/pro reflect that user's own tier.
func (s *AdminMetricsService) perUser(ctx context.Context, userID string, start, end *time.Time) (AdminMetrics, error) {
	m := AdminMetrics{Scope: userID, TotalUsers: 1}

	// Unknown user id -> ErrUserNotFound so the handler returns 404.
	userType, err := s.repo.GetUserType(ctx, userID)
	if errors.Is(err, sql.ErrNoRows) {
		return m, ErrUserNotFound
	}
	if err != nil {
		return m, err
	}
	tier := tierFor(userType)
	m.Tier = &tier
	if tier == tierFree {
		m.FreeUsers = 1
	} else {
		m.ProUsers = 1
	}

	// Datapacks are always the current total, never date-filtered.
	dpCount, err := s.repo.CountDatapacksByOwner(userID)
	if err != nil {
		return m, err
	}
	m.Datapacks = dpCount

	shared, err := s.repo.CountAcceptedSharesByOwner(ctx, userID, start, end)
	if err != nil {
		return m, err
	}
	m.SharedDatapacks = shared

	// Data-plane: KB pipeline aggregates scoped to the user's datapacks.
	// Skip the data-plane hit entirely when the user owns no KB sources.
	if sourceIDs, sErr := s.repo.GetSourceIDsByOwner(userID); sErr != nil {
		slog.Error("admin metrics: kb source ids", "user_id", userID, "error", sErr)
	} else if len(sourceIDs) > 0 {
		if kb, kbErr := s.repo.CountCompletedDocsAndChunksBySourceIDs(
			service.DatapackIndexSchema, service.DatapackIndexName, sourceIDs, start, end,
		); kbErr != nil {
			slog.Error("admin metrics: kb counts (user)", "user_id", userID, "error", kbErr)
		} else {
			m.KBDocuments, m.KBChunks = kb.Documents, kb.Chunks
		}
	}

	// Data-plane: private + collective memory counts scoped by user id.
	if n, mErr := s.repo.CountPrivateMemoriesByUser(ctx, userID, start, end); mErr != nil {
		slog.Error("admin metrics: private memories (user)", "user_id", userID, "error", mErr)
	} else {
		m.PrivateMemories = n
	}
	if n, pErr := s.repo.CountPromotedByUser(ctx, userID, start, end); pErr != nil {
		slog.Error("admin metrics: collective memories (user)", "user_id", userID, "error", pErr)
	} else {
		m.CollectiveMemories = n
	}

	s.populateLangfuse(&m, []string{userID}, start, end)
	return m, nil
}
