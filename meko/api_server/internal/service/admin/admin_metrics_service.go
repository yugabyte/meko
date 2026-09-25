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

	"meko/api_server/internal/repository"
	adminrepo "meko/api_server/internal/repository/admin"
	"meko/api_server/internal/service"
)

// ErrUserNotFound is returned by Metrics when a per-user request names a user
// id that does not exist; the handler maps it to 404.
var ErrUserNotFound = errors.New("user not found")

// The canonical account_type tiers the admin console understands.
const (
	tierFree       = "free"
	tierStandard   = "standard"
	tierPro        = "pro"
	tierEnterprise = "enterprise"
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
//
// FreeUsers / StandardUsers / ProUsers are a partition of TotalUsers:
//
//	free_users     -- user_type='free'
//	standard_users -- user_type='standard' (AWS Marketplace)
//	pro_users      -- user_type IN ('pro','enterprise') (sales-led)
//
// Before Standard existed the split was 2-bucket (free vs. else, all
// counted as pro_users); the field is kept in the response so existing
// consumers (meko_ui, meko_admin) don't break, but it no longer
// includes Standard subscribers.
type AdminMetrics struct {
	Scope              string  `json:"scope"`
	TotalUsers         int64   `json:"total_users"`
	FreeUsers          int64   `json:"free_users"`
	StandardUsers      int64   `json:"standard_users"`
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

// tierFor returns the user's tier as a canonical account_type value,
// defaulting unknown/empty values to free.
func tierFor(userType string) string {
	switch userType {
	case tierFree, tierStandard, tierPro, tierEnterprise:
		return userType
	default:
		return tierFree
	}
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
	total, free, standard, pro, err := s.repo.CountUsersByTier(ctx, nil, nil)
	if err != nil {
		return m, err
	}
	m.TotalUsers, m.FreeUsers, m.StandardUsers, m.ProUsers = total, free, standard, pro

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
	switch tier {
	case tierFree:
		m.FreeUsers = 1
	case tierStandard:
		m.StandardUsers = 1
	default:
		// tierPro / tierEnterprise
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

// The per-user metrics tab fetches the six panels below in parallel (mirroring
// the executive dashboard split) so one slow query — the KB pipeline scan or a
// Langfuse aggregate — no longer blocks the fast control-plane tiles or times
// out the whole tab for a heavy user. Every count is scoped to the user and
// windowed to [start, end] (raw inclusive dates; each method makes its own
// exclusive upper bound); nil bounds mean all-time.

// UserOverview is GET /admin/metrics/overview: the user's tier, owned datapacks
// (all-time) and shares made in range. Control plane, fast; required.
type UserOverview struct {
	Scope           string `json:"scope"`
	Tier            string `json:"tier"`
	Datapacks       int64  `json:"datapacks"`
	SharedDatapacks int64  `json:"shared_datapacks"`
}

// UserMemories is GET /admin/metrics/memories: the user's private and promoted
// (collective) memories in range. Data plane; best-effort.
type UserMemories struct {
	PrivateMemories    int64 `json:"private_memories"`
	CollectiveMemories int64 `json:"collective_memories"`
}

// UserConversations is GET /admin/metrics/conversations: the user's conversation
// total and by-agent breakdown in range. Langfuse; best-effort.
type UserConversations struct {
	Conversations        int64                              `json:"conversations"`
	ConversationsByAgent []adminrepo.AgentConversationCount `json:"conversations_by_agent"`
	LangfuseEnabled      bool                               `json:"langfuse_enabled"`
}

// UserTokens is GET /admin/metrics/tokens: the user's token burn in range.
// Langfuse; best-effort.
type UserTokens struct {
	TokenBurn       int64 `json:"token_burn"`
	LangfuseEnabled bool  `json:"langfuse_enabled"`
}

// UserKnowledge is GET /admin/metrics/knowledge: the user's indexed KB documents
// + chunks and the by-file-type breakdown in range. Data plane; best-effort.
type UserKnowledge struct {
	KBDocuments       int64              `json:"kb_documents"`
	KBChunks          int64              `json:"kb_chunks"`
	KBDocumentsByType []ExecDocTypeCount `json:"kb_documents_by_type"`
}

// EnsureUserExists returns ErrUserNotFound when userID is not a user of this
// deployment. The per-user handlers call this (via userScope) before any panel
// runs, so a foreign/unknown id never reaches the shared Langfuse DB (MG-138).
func (s *AdminMetricsService) EnsureUserExists(ctx context.Context, userID string) error {
	_, err := s.repo.GetUserType(ctx, userID)
	if errors.Is(err, sql.ErrNoRows) {
		return ErrUserNotFound
	}
	return err
}

// UserOverview: tier + owned datapacks (all-time) + shares (in range). An unknown
// user id is ErrUserNotFound so the handler returns 404.
func (s *AdminMetricsService) UserOverview(ctx context.Context, userID string, start, end *time.Time) (UserOverview, error) {
	out := UserOverview{Scope: userID}
	endExclusive := endExclusiveBound(end)

	userType, err := s.repo.GetUserType(ctx, userID)
	if errors.Is(err, sql.ErrNoRows) {
		return out, ErrUserNotFound
	}
	if err != nil {
		return out, err
	}
	out.Tier = tierFor(userType)

	dpCount, err := s.repo.CountDatapacksByOwner(userID)
	if err != nil {
		return out, err
	}
	out.Datapacks = dpCount

	shared, err := s.repo.CountAcceptedSharesByOwner(ctx, userID, start, endExclusive)
	if err != nil {
		return out, err
	}
	out.SharedDatapacks = shared
	return out, nil
}

// UserMemories: private + collective memory counts scoped by user id.
func (s *AdminMetricsService) UserMemories(ctx context.Context, userID string, start, end *time.Time) (UserMemories, error) {
	out := UserMemories{}
	endExclusive := endExclusiveBound(end)

	if n, err := s.repo.CountPrivateMemoriesByUser(ctx, userID, start, endExclusive); err != nil {
		slog.Error("user metrics: private memories", "user_id", userID, "error", err)
	} else {
		out.PrivateMemories = n
	}
	if n, err := s.repo.CountPromotedByUser(ctx, userID, start, endExclusive); err != nil {
		slog.Error("user metrics: collective memories", "user_id", userID, "error", err)
	} else {
		out.CollectiveMemories = n
	}
	return out, nil
}

// UserConversations: conversation total + by-agent breakdown (Langfuse), scoped
// to the single user.
func (s *AdminMetricsService) UserConversations(_ context.Context, userID string, start, end *time.Time) (UserConversations, error) {
	out := UserConversations{ConversationsByAgent: []adminrepo.AgentConversationCount{}}
	if !s.repo.LangfuseConfigured() {
		return out, nil
	}
	out.LangfuseEnabled = true
	endExclusive := endExclusiveBound(end)
	ids := []string{userID}

	if n, err := s.repo.TotalConversations(ids, start, endExclusive); err != nil {
		slog.Warn("user metrics: conversations", "user_id", userID, "error", err)
	} else {
		out.Conversations = n
	}
	if rows, err := s.repo.ConversationsByAgent(ids, start, endExclusive); err != nil {
		slog.Warn("user metrics: conversations by agent", "user_id", userID, "error", err)
	} else {
		out.ConversationsByAgent = rows
	}
	return out, nil
}

// UserTokens: token burn (Langfuse), scoped to the single user.
func (s *AdminMetricsService) UserTokens(_ context.Context, userID string, start, end *time.Time) (UserTokens, error) {
	out := UserTokens{}
	if !s.repo.LangfuseConfigured() {
		return out, nil
	}
	out.LangfuseEnabled = true
	endExclusive := endExclusiveBound(end)

	if n, err := s.repo.TokenBurn([]string{userID}, start, endExclusive); err != nil {
		slog.Warn("user metrics: token burn", "user_id", userID, "error", err)
	} else {
		out.TokenBurn = n
	}
	return out, nil
}

// UserSearches is GET /admin/metrics/searches: the user's knowledge-base, memory
// and context search counts from the meko_system.free_tier_usage counters.
// knowledgebase_search and memory_search are the same counters Meko UI's usage
// tab surfaces (account_usage_service.go); context_search has no Meko UI tile but
// is tracked in the same table, so it's exposed here for admins. context_search
// is a distinct unit — its internal memory/KB lookups bypass the
// memory_search/knowledgebase_search quotas, so the three do not double-count.
type UserSearches struct {
	KBSearches      int64 `json:"kb_searches"`
	MemorySearches  int64 `json:"memory_searches"`
	ContextSearches int64 `json:"context_searches"`
}

// UserSearches: KB + memory + context search counts, windowed to [start, end].
// All best-effort (a read failure logs + leaves zero rather than failing the tab).
func (s *AdminMetricsService) UserSearches(_ context.Context, userID string, start, end *time.Time) (UserSearches, error) {
	out := UserSearches{}
	endExclusive := endExclusiveBound(end)

	if n, err := s.searchCount(userID, repository.MetricKnowledgebaseSearch, start, endExclusive); err != nil {
		slog.Error("user metrics: kb searches", "user_id", userID, "error", err)
	} else {
		out.KBSearches = n
	}
	if n, err := s.searchCount(userID, repository.MetricMemorySearch, start, endExclusive); err != nil {
		slog.Error("user metrics: memory searches", "user_id", userID, "error", err)
	} else {
		out.MemorySearches = n
	}
	if n, err := s.searchCount(userID, repository.MetricContextSearch, start, endExclusive); err != nil {
		slog.Error("user metrics: context searches", "user_id", userID, "error", err)
	} else {
		out.ContextSearches = n
	}
	return out, nil
}

// searchCount windows retrieval counts by date from the append-only
// free_tier_audit_log, but reads the exact free_tier_usage summary for the
// all-time (unbounded) case — the audit log's best-effort writes can undercount.
// Same summary-vs-log split as token savings.
func (s *AdminMetricsService) searchCount(userID, metric string, start, endExclusive *time.Time) (int64, error) {
	if start == nil && endExclusive == nil {
		return s.repo.FreeTierSummaryCount(userID, metric)
	}
	return s.repo.CountAuditEventsInRange(userID, metric, start, endExclusive)
}

// UserKnowledge: indexed KB docs + chunks and the by-type breakdown, scoped to
// the user's rag sources. One pipeline scan (see CountIndexedDocsBreakdown*).
func (s *AdminMetricsService) UserKnowledge(_ context.Context, userID string, start, end *time.Time) (UserKnowledge, error) {
	out := UserKnowledge{KBDocumentsByType: []ExecDocTypeCount{}}
	endExclusive := endExclusiveBound(end)

	sourceIDs, err := s.repo.GetSourceIDsByOwner(userID)
	if err != nil {
		slog.Error("user metrics: kb source ids", "user_id", userID, "error", err)
		return out, nil
	}
	kb, err := s.repo.CountIndexedDocsBreakdownBySourceIDs(
		service.DatapackIndexSchema, service.DatapackIndexName, sourceIDs, start, endExclusive,
	)
	if err != nil {
		slog.Error("user metrics: kb breakdown", "user_id", userID, "error", err)
		return out, nil
	}
	out.KBDocuments, out.KBChunks = kb.Documents, kb.Chunks
	for _, t := range kb.ByType {
		out.KBDocumentsByType = append(out.KBDocumentsByType, ExecDocTypeCount{Type: t.DocumentType, Count: t.Count})
	}
	return out, nil
}
