package service

import "strings"

// IsProUserType reports whether the given `meko_system.users.user_type`
// is the pro tier. Mirrors IsFreeUserType's trim/fold semantics.
func IsProUserType(userType string) bool {
	return strings.EqualFold(strings.TrimSpace(userType), "pro")
}

// Pro-tier caps. Pro is uncapped except for the metrics listed here.
// Like the free-tier caps in free_tier.go, these mirror env names read
// by meko-mcp-server — the enforcing side, which counts usage in
// meko_system.free_tier_usage — so ops tunes one env name and api_server,
// the UI, and the MCP server all agree.
var (
	// Conversation-cache embedding cap: one unit per turn embedded into
	// datapacks.conversation_history_search (live index_for_search=True
	// or conversation_index_history backfill), per account across
	// datapacks (metric: conversation_search_embedding).
	ProTierMaxConversationEmbeddings = envInt64("MEKO_PRO_TIER_CONVERSATION_EMBEDDINGS", 10_000)
)
