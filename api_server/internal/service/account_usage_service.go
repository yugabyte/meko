package service

import (
	"log/slog"

	"meko/api_server/internal/repository"
)

// AgentIDWorkbenchLM is the literal stamped on WorkbenchLM traces by
// the inference gateway. Mirror of `WORKBENCH_LM_AGENT_ID` in the FE.
// Lives in service (not handler) because the SUM-by-agent query uses it.
const AgentIDWorkbenchLM = "WorkbenchLM"

// AccountUsage is the four-counter response shape returned by
// /auth/me/usage. Storage is bytes; conversations and tokens are
// scoped to WorkbenchLM specifically (see field comments).
type AccountUsage struct {
	DatapackCount     int64 `json:"datapack_count"`
	KBStorageFileSize int64 `json:"kb_storage_file_size"`
	// NumConversations counts `conversation_add_message` invocations
	// from `meko_system.free_tier_usage` — the same counter MCP's
	// free-tier-quota gate increments. Field name kept for FE compat.
	NumConversations int64 `json:"num_conversations"`
	// TokenBurn sums Langfuse `usage_details.total` across WorkbenchLM
	// GENERATION observations written by inference_gateway via the
	// MCP `track_token_usage` tool.
	TokenBurn int64 `json:"token_burn"`
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

	// num_conversations source: meko_system.free_tier_usage counter
	// table, written by meko-mcp-server's quota gate. Indexed PK
	// lookup. Best-effort: failure logs + zero rather than abort.
	if s.freeTierRepo != nil {
		nMsgs, msgsErr := s.freeTierRepo.RequestCount(userID, "conversation_add_message")
		if msgsErr != nil {
			slog.Warn("usage: read free_tier_usage", "user_id", userID, "error", msgsErr)
		} else {
			usage.NumConversations = nMsgs
		}
	}

	// token_burn source: Langfuse SUM. Best-effort — when Langfuse
	// is unconfigured (dev / minimal stack) we surface 0.
	if s.langfuseRepo != nil && s.langfuseRepo.LangfuseDBConfigured() {
		nTok, tokErr := s.langfuseRepo.SumTokenUsageByUserAndAgent(userID, AgentIDWorkbenchLM)
		if tokErr != nil {
			slog.Warn("usage: sum token burn (langfuse)", "user_id", userID, "error", tokErr)
		} else {
			usage.TokenBurn = nTok
		}
	}

	return usage, nil
}
