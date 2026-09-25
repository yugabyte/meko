package service

import (
	"log/slog"

	"meko/api_server/internal/repository"
)

// AgentIDWorkbenchLM is the literal stamped on WorkbenchLM traces by
// the inference gateway. Mirror of `WORKBENCH_LM_AGENT_ID` in the FE.
// Lives in service (not handler) because the SUM-by-agent query uses it.
const AgentIDWorkbenchLM = "WorkbenchLM"

// AgentIDReviewAgentLab is the agent_id inference_gateway's chain.py stamps
// on review_agent-persona traces (PERSONA_AGENT_IDS). Metered the same as
// WorkbenchLM -- Labs traffic isn't exempt from the free-tier token cap.
const AgentIDReviewAgentLab = "ReviewAgentLab"

// MeteredTokenBurnAgentIDs are the agent_ids summed into TokenBurn below.
var MeteredTokenBurnAgentIDs = []string{AgentIDWorkbenchLM, AgentIDReviewAgentLab}

// AccountUsage is the four-counter response shape returned by
// /auth/me/usage. Storage is bytes; conversations and tokens are
// scoped to WorkbenchLM specifically (see field comments).
type AccountUsage struct {
	// DatapackCount is the LIVE count of datapacks the user currently
	// owns — display only. Free-tier gating uses DatapackCreations.
	DatapackCount int64 `json:"datapack_count"`
	// DatapackCreations is the lifetime `datapack_creation` counter
	// from `meko_system.free_tier_usage` (incremented on create, never
	// decremented on delete). The FE gates the datapack cap on this so
	// delete-and-recreate cannot reset the quota.
	DatapackCreations int64 `json:"datapack_creations"`
	KBStorageFileSize int64 `json:"kb_storage_file_size"`
	// NumConversations counts `conversation_add_message` invocations
	// from `meko_system.free_tier_usage` — the same counter MCP's
	// free-tier-quota gate increments. Field name kept for FE compat.
	NumConversations int64 `json:"num_conversations"`
	// TokenBurn sums Langfuse `usage_details.total` across WorkbenchLM
	// GENERATION observations written by inference_gateway via the
	// MCP `track_token_usage` tool.
	TokenBurn int64 `json:"token_burn"`
	// MCP-side retrieval / write counters. `meko-mcp-server` increments
	// these on every gated call to `memory_search`, `knowledgebase_search`,
	// and `memory_add`; api_server only reads them so the UI's Settings
	// usage tracker can render "consumed / max" against the tier caps.
	NumMemorySearches int64 `json:"num_memory_searches"`
	NumKBSearches     int64 `json:"num_kb_searches"`
	NumMemoryAdds     int64 `json:"num_memory_adds"`
}

// AccountUsageService aggregates per-account free-tier usage counters
// from three independent sources: the primary `meko_system` DB
// (datapacks, KB sizes, free_tier_usage), and the Langfuse DB (token
// burn). Handlers depend on this service, not the underlying repos.
type AccountUsageService struct {
	datapackRepo *repository.DatapackRepo
	kbRepo       *repository.KnowledgeBaseRepo
	langfuseRepo *repository.LangfuseRepo
	freeTierRepo *repository.FreeTierUsageRepo
}

func NewAccountUsageService(
	datapackRepo *repository.DatapackRepo,
	kbRepo *repository.KnowledgeBaseRepo,
	langfuseRepo *repository.LangfuseRepo,
	freeTierRepo *repository.FreeTierUsageRepo,
) *AccountUsageService {
	return &AccountUsageService{
		datapackRepo: datapackRepo,
		kbRepo:       kbRepo,
		langfuseRepo: langfuseRepo,
		freeTierRepo: freeTierRepo,
	}
}

// Configured reports whether the primary counter repos are wired.
// Used by the handler to fail loud + clean when the registry didn't
// supply them (early-bootstrap minimal wiring).
func (s *AccountUsageService) Configured() bool {
	return s != nil && s.datapackRepo != nil && s.kbRepo != nil
}

// GetUsage returns the four counters for `userID`. Each source fails
// independently — a Langfuse outage drops `token_burn` to 0 but the
// other counters still return real values. Errors on the two primary
// counters propagate (those reads are fast indexed lookups; a failure
// is more likely to be a misconfiguration than transient).
func (s *AccountUsageService) GetUsage(userID string) (AccountUsage, error) {
	usage := AccountUsage{}

	dpCount, err := s.datapackRepo.CountByOwner(userID)
	if err != nil {
		slog.Error("usage: count datapacks", "user_id", userID, "error", err)
		return usage, err
	}
	usage.DatapackCount = dpCount

	kbBytes, err := s.kbRepo.SumDocumentSizesByOwner(userID)
	if err != nil {
		slog.Error("usage: sum kb sizes", "user_id", userID, "error", err)
		return usage, err
	}
	usage.KBStorageFileSize = kbBytes

	// num_conversations + datapack_creations source:
	// meko_system.free_tier_usage counter table (conversation counter
	// written by meko-mcp-server's quota gate, creation counter by
	// DatapackRepo.Create). Indexed PK lookups. Best-effort: failure
	// logs + zero rather than abort.
	if s.freeTierRepo != nil {
		nMsgs, msgsErr := s.freeTierRepo.RequestCount(userID, "conversation_add_message")
		if msgsErr != nil {
			slog.Warn("usage: read free_tier_usage", "user_id", userID, "error", msgsErr)
		} else {
			usage.NumConversations = nMsgs
		}

		nCreations, crErr := s.freeTierRepo.RequestCount(userID, repository.MetricDatapackCreation)
		if crErr != nil {
			slog.Warn("usage: read datapack_creation counter", "user_id", userID, "error", crErr)
		} else {
			usage.DatapackCreations = nCreations
		}

		// MCP-side retrieval counters — metric names match the tool
		// names meko-mcp-server passes to `_enforce_free_tier_limit` /
		// `_enforce_quota_charge`. Best-effort like the others above.
		for _, m := range []struct {
			metric string
			set    func(int64)
		}{
			{repository.MetricMemorySearch, func(n int64) { usage.NumMemorySearches = n }},
			{repository.MetricKnowledgebaseSearch, func(n int64) { usage.NumKBSearches = n }},
			{"memory_add", func(n int64) { usage.NumMemoryAdds = n }},
		} {
			n, e := s.freeTierRepo.RequestCount(userID, m.metric)
			if e != nil {
				slog.Warn("usage: read free_tier_usage", "user_id", userID, "metric", m.metric, "error", e)
				continue
			}
			m.set(n)
		}
	}

	// token_burn source: Langfuse SUM across every metered agent_id.
	// Best-effort — when Langfuse is unconfigured (dev / minimal stack)
	// we surface 0.
	if s.langfuseRepo != nil && s.langfuseRepo.LangfuseDBConfigured() {
		var total int64
		for _, agentID := range MeteredTokenBurnAgentIDs {
			nTok, tokErr := s.langfuseRepo.SumTokenUsageByUserAndAgent(userID, agentID)
			if tokErr != nil {
				slog.Warn("usage: sum token burn (langfuse)", "user_id", userID, "agent_id", agentID, "error", tokErr)
				continue
			}
			total += nTok
		}
		usage.TokenBurn = total
	}

	return usage, nil
}
