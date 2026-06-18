// Package models defines shared data structures used across the API server.
package models

import (
	"encoding/json"
	"strings"
	"time"
)

// WorkbenchAgentID is the Inference Gateway's agent_id. Only its conversations
// populate the conversation cache, so only they can be backfilled.
const WorkbenchAgentID = "WorkbenchLM"

// Datapack represents a registered datapack in the meko system.
//
// Backed by meko_system.datapacks. Identified by (user_id, datapack_id);
// (user_id, datapack_name) is also unique. The connection string and
// YSQL credentials are shared per-user (multi-tenant universe): the
// datapack_ysql_username column carries the same value as the user's
// MCP role and is exposed here as MCPUserName.
type Datapack struct {
	CreatedAt        time.Time `json:"created_at"`
	UpdatedAt        time.Time `json:"updated_at"`
	ID               string    `json:"datapack_id"`
	Name             string    `json:"datapack_name"`
	UserID           string    `json:"user_id"`
	CreatedByAgentID string    `json:"created_by_agent_id,omitempty"`
	UniverseID       string    `json:"universe_id"`
	YBUniverseConn   string    `json:"datapack_ysql_connection_string"`
	MCPUserName      string    `json:"datapack_ysql_username"`
	Description      string    `json:"description,omitempty"`
	OwnerName        string    `json:"datapack_owner_name,omitempty"`
	OwnerEmail       string    `json:"datapack_owner_email_id,omitempty"`
	Grant            string    `json:"grant,omitempty"`
	MemoryCount      int       `json:"memory_count"`
	KnowledgeCount   int       `json:"knowledge_count"`
	// LearningsCount = mem0 rows still awaiting a user decision
	// (meko_promotion_user_action IS NULL). A subset of MemoryCount.
	LearningsCount int `json:"learnings_count"`
	// CollectiveMemoryCount = memories promoted into the shared KB.
	CollectiveMemoryCount int `json:"collective_memory_count"`
}

// Agent represents a user-scoped agent. Backed by meko_system.agents.
// Linked to datapacks via meko_system.agent_access.
type Agent struct {
	CreatedAt time.Time       `json:"created_at"`
	UpdatedAt time.Time       `json:"updated_at"`
	UserID    string          `json:"user_id"`
	AgentID   string          `json:"agent_id"`
	Details   json.RawMessage `json:"agent_details,omitempty"`
}

// Account represents a user account in the meko system. Backed by
// meko_system.users.
type Account struct {
	CreatedAt              time.Time  `json:"created_at"`
	UpdatedAt              time.Time  `json:"updated_at"`
	EmailVerifiedAt        *time.Time `json:"email_verified_at,omitempty"`
	HasCompletedOnboarding bool       `json:"has_completed_onboarding"`
	ID                     string     `json:"user_id"`
	FirstName              string     `json:"user_first_name"`
	LastName               string     `json:"user_last_name"`
	Email                  string     `json:"user_email"`
	AccountType            string     `json:"user_type"`
	UserOrg                *string    `json:"user_org,omitempty"`
	CognitoSub             string     `json:"cognito_sub,omitempty"`
	MCPUserName            *string    `json:"mcp_user_name,omitempty"`
	MCPUserPassword        *string    `json:"-"`
}

// KnowledgeBase represents a row from meko_system.knowledge_bases.
//
// Identified by the composite (DatapackID, RagSourceID) primary key.
// URL points to the S3 prefix where uploaded files for this KB live.
// Details carries free-form metadata as JSONB.
type KnowledgeBase struct {
	CreatedAt   time.Time       `json:"created_at"`
	UpdatedAt   time.Time       `json:"updated_at"`
	DatapackID  string          `json:"datapack_id"`
	RagSourceID string          `json:"rag_source_id"`
	URL         string          `json:"knowledge_base_url"`
	Details     json.RawMessage `json:"knowledge_base_details,omitempty"`
}

// Memory represents one row from datapacks.meko_mem0_collection as exposed
// to the UI. Only the memory text (payload->>'data') and the owning agent
// are surfaced; all other payload fields are intentionally dropped.
//
// UserID + UserName + UserEmail identify which account added the memory.
// In a shared datapack the listing surfaces memories from every member, so
// the UI uses these fields to label the author and to gate per-memory
// actions (e.g. "delete" only on memories the current user owns).
type Memory struct {
	ID             string     `json:"id"`
	Data           string     `json:"data"`
	AgentID        string     `json:"agent_id"`
	ConversationID string     `json:"conversation_id,omitempty"`
	UserID         string     `json:"user_id,omitempty"`
	UserName       string     `json:"user_name,omitempty"`
	UserEmail      string     `json:"user_email,omitempty"`
	CreatedAt      *time.Time `json:"created_at,omitempty"`
}

// MemoryListFilter is the filter for the Mem0 collection (datapacks.meko_mem0_collection).
// DatapackID is required; UserID is optional. UserType ("free" | "pro" | "enterprise")
// selects the query path: only "pro" / "enterprise" run the vector-similarity ranking
// (LLM summary + embedding + ORDER BY embeddings); everything else runs the plain SELECT.
type MemoryListFilter struct {
	// Q is a case-insensitive substring filter over `payload->>'data'`.
	// Empty disables the filter.
	Q string `json:"q,omitempty"`
	// Scope selects which `meko_promotion_user_action` values are
	// included. "learnings" → only NULL (rows still awaiting decision).
	// "memories" → NULL OR 'keep_as_memory'. Empty falls back to
	// "memories" so legacy callers keep working.
	Scope      string `json:"scope,omitempty"`
	DatapackID string `json:"datapack_id"`
	UserID     string `json:"user_id,omitempty"`
	UserType   string `json:"user_type,omitempty"`
	Page       int    `json:"page"`
	PageSize   int    `json:"page_size"`
}

// IsPaidUserType reports whether the account tier should run the
// vector-similarity ranking path. Only "pro" / "enterprise" qualify;
// "free" (or empty/unknown) falls back to the plain SELECT path.
func (f MemoryListFilter) IsPaidUserType() bool {
	switch strings.ToLower(strings.TrimSpace(f.UserType)) {
	case "pro", "enterprise":
		return true
	default:
		return false
	}
}

// MemoryListResponse is the response to the memories list API.
type MemoryListResponse struct {
	Criteria          MemoryListFilter `json:"criteria"`
	Memories          []Memory         `json:"memories"`
	TopK              int              `json:"top_k"`
	Total             int              `json:"total"`
	Page              int              `json:"page"`
	PageSize          int              `json:"page_size"`
	TotalPages        int              `json:"total_pages"`
	LangfuseProjectID string           `json:"langfuse_project_id,omitempty"`
}

// MemoryCountResponse is the response to the memories count API. It returns
// the total number of rows in datapacks.meko_mem0_collection for a given
// datapack_id, with no additional filtering applied.
type MemoryCountResponse struct {
	DatapackID string `json:"datapack_id"`
	Count      int    `json:"count"`
}

// CreateFeedbackRequest is the body for POST /api/v1/feedback.
// user_id is required and must match the authenticated user (non-pointer string;
// JSON null unmarshals to empty and fails validation). Optional pointers may be
// omitted or JSON null for datapack scope when there is no datapack (e.g. install/home).
type CreateFeedbackRequest struct {
	UserID           string     `json:"user_id"`
	CreatedAt        *time.Time `json:"created_at"`
	DatapackID       *string    `json:"datapack_id"`
	DatapackName     *string    `json:"datapack_name"`
	UserPath         *string    `json:"user_path"`
	Category         string     `json:"category"`
	FeedbackText     string     `json:"feedback_text"`
	Source           string     `json:"source"`
	SourceIdentifier *string    `json:"source_identifier"`
	MekoVersion      *string    `json:"meko_version,omitempty"`
}

// CreateFeedbackResponse is the JSON body for a successful POST /api/v1/feedback.
// FeedbackID is the meko_system.feedback.id assigned by the database (string form of BIGINT).
type CreateFeedbackResponse struct {
	FeedbackID string `json:"feedback_id"`
	Status     string `json:"status"`
	Message    string `json:"message"`
}

// ValidFeedbackSource reports whether s is a meko_system.feedback_source value.
func ValidFeedbackSource(s string) bool {
	switch s {
	case "web_app", "docs", "discord", "email", "cli", "mcp_server":
		return true
	}
	return false
}

// ValidFeedbackCategory reports whether s is a meko_system.feedback_category value.
func ValidFeedbackCategory(s string) bool {
	switch s {
	case "bug", "confusion", "feature_request", "other":
		return true
	}
	return false
}

// ValidFeedbackStatus reports whether s is a meko_system.feedback_status value.
func ValidFeedbackStatus(s string) bool {
	switch s {
	case "new", "triaged", "in_progress", "resolved", "wontfix":
		return true
	}
	return false
}

// FeedbackListItem is one row returned by GET /api/v1/feedback.
// Omits internal-only columns (internal_notes, resolved_at) per migration policy.
type FeedbackListItem struct {
	ID               string     `json:"id"`
	CreatedAt        time.Time  `json:"created_at"`
	UpdatedAt        *time.Time `json:"updated_at,omitempty"`
	UserID           *string    `json:"user_id,omitempty"`
	Source           string     `json:"source"`
	SourceIdentifier *string    `json:"source_identifier,omitempty"`
	DatapackID       *string    `json:"datapack_id,omitempty"`
	DatapackName     *string    `json:"datapack_name,omitempty"`
	ContextRoute     *string    `json:"context_route,omitempty"`
	Category         string     `json:"category"`
	Body             string     `json:"body"`
	MekoVersion      *string    `json:"meko_version,omitempty"`
	Status           string     `json:"status"`
}

// PromoteMemoriesRequest is the body for POST /api/v1/promote-memories.
type PromoteMemoriesRequest struct {
	UserID     string   `json:"user_id"`
	AgentID    string   `json:"agent_id,omitempty"`
	DatapackID string   `json:"datapack_id"`
	MemoryIDs  []string `json:"memory_ids"`
}

// PromoteMemoriesResult summarizes what happened when promoting mem0 memories
// into knowledge_bases + promoted_collection (and evicting them from mem0).
type PromoteMemoriesResult struct {
	InsertedIDs []string `json:"inserted_ids"`
	UpdatedIDs  []string `json:"updated_ids"`
	NotFoundIDs []string `json:"not_found_ids"`
}

// KeepAsMemoryRequest is the body for POST /api/v1/keep-as-memory.
// Flags one or more mem0 rows as "user chose to keep, not promote" so
// they vanish from the Learnings list but stay in Memories.
type KeepAsMemoryRequest struct {
	DatapackID string   `json:"datapack_id"`
	MemoryIDs  []string `json:"memory_ids"`
}

// KeepAsMemoryResult reports how many rows actually flipped state. Rows
// already marked, or that don't belong to the datapack, are not counted.
type KeepAsMemoryResult struct {
	Updated int64 `json:"updated"`
}

// PromotedMemory is one row from datapacks.promoted_collection for list APIs.
//
// Per-row enrichment for the SharedKnowledge "Promoted memories" tab:
//   - PromotedByName / PromotedByEmail: identity of the user who
//     promoted this memory. Resolved out-of-line via systemDB's
//     LookupUserDisplays — both fields are nullable because (a)
//     `meko_user_id` on the row can be NULL, and (b) the user may have
//     been deleted since the promotion happened.
//
// Index-level constants (embedding_model / dimensions / ai_provider)
// are NOT on this struct OR on PromotedMemoryListResponse — they live
// on GET /knowledge-bases/summary as the single source of truth for
// properties of the global vector index. Callers that need both the
// list and the index config issue both requests; callers that only
// need the list pay nothing extra.
type PromotedMemory struct {
	ID              string     `json:"id"`
	ConversationID  *string    `json:"conversation_id,omitempty"`
	AgentID         string     `json:"agent_id,omitempty"`
	Data            string     `json:"data"`
	PromotedToKBAt  *time.Time `json:"promoted_to_kb_at,omitempty"`
	PromotedByName  *string    `json:"promoted_by_name,omitempty"`
	PromotedByEmail *string    `json:"promoted_by_email,omitempty"`
}

// PromotedMemoryListFilter is the query input for GET /promoted-memories.
type PromotedMemoryListFilter struct {
	DatapackID string `json:"datapack_id"`
	Page       int    `json:"page"`
	PageSize   int    `json:"page_size"`
}

// PromotedMemoryListResponse is the response body for GET /promoted-memories.
//
// Index-level constants (embedding_model / embedding_dimensions /
// ai_provider) are deliberately NOT on this response. They live on
// GET /knowledge-bases/summary, which is the single source of truth
// for properties of the global vector index. Pages that need both
// (e.g. SharedKnowledge) already fetch /summary; pages that only show
// the promoted list (e.g. the Collective Memory route) do not need
// these constants and would otherwise pay for an extra dist_rag
// roundtrip per page view.
type PromotedMemoryListResponse struct {
	Criteria         PromotedMemoryListFilter `json:"criteria"`
	PromotedMemories []PromotedMemory         `json:"promoted_memories"`
	Total            int                      `json:"total"`
	Page             int                      `json:"page"`
	PageSize         int                      `json:"page_size"`
	TotalPages       int                      `json:"total_pages"`
}

// GrantRole represents the API-level share role. The backend translates
// each role into two agent_access_permissions rows (one per object_type).
type GrantRole string

const (
	GrantViewer      GrantRole = "viewer"
	GrantContributor GrantRole = "contributor"
	GrantMaintainer  GrantRole = "maintainer"
)

// ValidGrantRole reports whether s is a recognised grant role.
func ValidGrantRole(s string) bool {
	switch GrantRole(s) {
	case GrantViewer, GrantContributor, GrantMaintainer:
		return true
	}
	return false
}

// DatapackInvite represents a row from meko_system.datapack_invites.
type DatapackInvite struct {
	InviteID        string    `json:"invite_id"`
	DatapackID      string    `json:"datapack_id"`
	OwnerUserID     string    `json:"owner_user_id"`
	RecipientEmail  string    `json:"recipient_email"`
	RecipientUserID *string   `json:"recipient_user_id,omitempty"`
	GrantRole       string    `json:"grant"`
	Status          string    `json:"status"`
	CreatedAt       time.Time `json:"created_at"`
	UpdatedAt       time.Time `json:"updated_at"`
}

// IncomingShare is returned by GET /datapacks/incoming-shares.
type IncomingShare struct {
	InviteID     string `json:"invite_id"`
	OwnerUserID  string `json:"owner_user_id"`
	OwnerName    string `json:"owner_name"`
	GrantRole    string `json:"grant"`
	DatapackID   string `json:"datapack_id"`
	DatapackName string `json:"datapack_name"`
}

// OutgoingShareMember is a user who has accepted and has active access.
type OutgoingShareMember struct {
	UserID    string `json:"user_id"`
	IsOwner   bool   `json:"is_owner"`
	UserName  string `json:"user_name"`
	UserEmail string `json:"user_email_id"`
	GrantRole string `json:"grant"`
}

// OutgoingSharePendingInvite is an invite that has not been accepted yet.
type OutgoingSharePendingInvite struct {
	InviteID          string  `json:"invite_id"`
	GrantRole         string  `json:"grant"`
	RecipientUserID   *string `json:"recipient_user_id"`
	RecipientUserName *string `json:"recipient_user_name"`
	RecipientEmailID  string  `json:"recipient_email_id"`
}

// OutgoingPendingInvitesResponse is returned by GET /datapacks/:datapack_id/outgoing-shares
// (pending invites only; owner or maintainer).
type OutgoingPendingInvitesResponse struct {
	PendingInvites []OutgoingSharePendingInvite `json:"pending_invites"`
}

// DatapackMembersResponse is returned by GET /datapacks/:datapack_id/member-list
// (accepted members and metadata; any datapack access).
type DatapackMembersResponse struct {
	Members []OutgoingShareMember `json:"members"`
}

// MCPPersonalAccessToken represents one row from
// meko_system.mcp_personal_access_tokens.
//
// The `token_hash` column is intentionally NOT exposed -- it never
// leaves the repo layer. `TokenPrefix` is the first ~16 chars of the
// plaintext token (e.g. "mko_tkn_3KvL9X8z") and is shown in the UI as
// the visual identifier after the one-time create reveal. The full
// plaintext is only available in CreateMCPTokenResponse, returned
// once at create time.
//
// Scopes is the set of permission strings the token was minted with.
// Default vocabulary: "*" (full access), "read", "write". The set is
// always non-empty; rows created before scopes existed are migrated to
// {"*"} by the schema default.
//
// ExpiresAt is mandatory at the storage layer (NOT NULL since
// migration 000004). The non-pointer type makes "no expiry" not
// representable in Go, eliminating an entire class of nil-handling
// bugs in callers.
type MCPPersonalAccessToken struct {
	CreatedAt   time.Time  `json:"created_at"`
	LastUsedAt  *time.Time `json:"last_used_at,omitempty"`
	ExpiresAt   time.Time  `json:"expires_at"`
	RevokedAt   *time.Time `json:"revoked_at,omitempty"`
	ID          string     `json:"id"`
	UserID      string     `json:"-"`
	Name        string     `json:"name"`
	TokenPrefix string     `json:"token_prefix"`
	Scopes      []string   `json:"scopes"`
}

// IsActive reports whether the token can still authenticate. A token
// is active when it has not been revoked and has not expired.
func (t MCPPersonalAccessToken) IsActive(now time.Time) bool {
	if t.RevokedAt != nil {
		return false
	}
	if !t.ExpiresAt.After(now) {
		return false
	}
	return true
}

// CreateMCPTokenRequest is the JSON body for POST /me/meko-tokens.
//
// TTLDays is optional: when zero (or omitted) the service applies the
// default TTL; values larger than the platform max are clamped down.
//
// Scopes is optional: when nil/empty the service defaults to {"*"}.
// Each entry must come from the service-layer allowlist; otherwise the
// request is rejected at create time.
type CreateMCPTokenRequest struct {
	Name    string   `json:"name"`
	TTLDays int      `json:"ttl_days,omitempty"`
	Scopes  []string `json:"scopes,omitempty"`
}

// CreateMCPTokenResponse is the body returned exactly once at token
// creation time. The plaintext `Token` is NEVER stored or returned
// again; clients must persist it themselves (e.g. paste into mcp.json).
type CreateMCPTokenResponse struct {
	Token       string                 `json:"token"`
	TokenRecord MCPPersonalAccessToken `json:"token_record"`
}

// ValidatePATRequest is the JSON body for
// POST /api/v1/internal/meko-tokens/validate. Sent by the MCP server
// once per inbound tool call.
//
// ClientIP / ClientUserAgent are optional forwarding fields: when the
// MCP server populates them, the api_server uses those values for the
// audit row (actor_ip / actor_app_client) instead of the direct HTTP
// connection's RemoteAddr / User-Agent. This is how the originating
// end-user's IP and client identity (e.g. "Cursor/0.42.5 (mcp-client)")
// reach the audit log -- the api_server only sees the MCP server pod
// otherwise.
//
// Both fields are best-effort and additive. Missing values fall back
// to whatever the api_server can read off the inbound HTTP connection,
// so old MCP server builds keep working but produce less useful audit
// rows for revoked/expired token reuse events.
type ValidatePATRequest struct {
	Token           string `json:"token"`
	ClientIP        string `json:"client_ip,omitempty"`
	ClientUserAgent string `json:"client_user_agent,omitempty"`
}

// MCPPATValidationReason is the closed set of outcomes the validator
// returns for a non-active token. Empty when Valid is true.
type MCPPATValidationReason string

const (
	// MCPPATValidationExpired indicates the token row exists but its
	// expires_at is in the past.
	MCPPATValidationExpired MCPPATValidationReason = "expired"

	// MCPPATValidationRevoked indicates the token row exists but
	// revoked_at is set.
	MCPPATValidationRevoked MCPPATValidationReason = "revoked"

	// MCPPATValidationUnknown indicates either the format is wrong
	// (cheap reject) or no row matches the hash. Cannot be
	// distinguished from outside without leaking the existence of a
	// hashed bearer.
	MCPPATValidationUnknown MCPPATValidationReason = "unknown"
)

// ValidatePATResponse is the JSON body returned by the validator.
//
// When Valid is true: TokenID, UserID, ExpiresAt and Scopes are
// populated and Reason is empty. When Valid is false: only Reason is
// populated.
//
// All requests that are syntactically well-formed return HTTP 200;
// the verdict lives in the body so the MCP server can distinguish
// "api_server is up and says no" from "api_server is unreachable".
type ValidatePATResponse struct {
	ExpiresAt *time.Time             `json:"expires_at,omitempty"`
	TokenID   string                 `json:"token_id,omitempty"`
	UserID    string                 `json:"user_id,omitempty"`
	Scopes    []string               `json:"scopes,omitempty"`
	Reason    MCPPATValidationReason `json:"reason,omitempty"`
	Valid     bool                   `json:"valid"`
}

// MCPPATAuditAction is the discrete set of actions captured in
// meko_system.mcp_pat_audit_log.action.
type MCPPATAuditAction string

const (
	// MCPPATAuditCreated is logged when a user mints a new PAT.
	MCPPATAuditCreated MCPPATAuditAction = "created"

	// MCPPATAuditRevoked is logged when a user revokes a PAT from the
	// management UI / API.
	MCPPATAuditRevoked MCPPATAuditAction = "revoked"

	// MCPPATAuditExpiredUsed is logged (rate-limited) by the MCP
	// server when an expired token is presented.
	MCPPATAuditExpiredUsed MCPPATAuditAction = "expired_used"

	// MCPPATAuditRevokedUsed is logged (rate-limited) by the MCP
	// server when a revoked token is presented -- a strong signal of
	// possible token leak.
	MCPPATAuditRevokedUsed MCPPATAuditAction = "revoked_used"
)

// MCPPATActorType is the actor classification for an audit row.
type MCPPATActorType string

const (
	// MCPPATActorUser is the self-service caller acting on their own
	// tokens.
	MCPPATActorUser MCPPATActorType = "user"

	// MCPPATActorAdmin is reserved for admin / support users acting
	// on another user's tokens. Not used by the self-service flow.
	MCPPATActorAdmin MCPPATActorType = "admin"

	// MCPPATActorSystem is reserved for automated processes (e.g. the
	// MCP server logging revoked-used / expired-used).
	MCPPATActorSystem MCPPATActorType = "system"
)

// MCPPATAuditEntry represents one row from meko_system.mcp_pat_audit_log.
//
// ActorIP and ActorAppClient are captured best-effort from the request
// (or forwarded by the MCP server on the validate path) and may be
// empty. ActorAppClient is intentionally broader than HTTP User-Agent:
// it carries browser UAs on user-driven rows and tool identifiers
// (e.g. "Cursor/0.42.5 (mcp-client)") on validator-driven rows.
// Metadata is free-form JSONB for forward-compatibility.
type MCPPATAuditEntry struct {
	CreatedAt      time.Time         `json:"created_at"`
	ID             string            `json:"id"`
	TokenID        string            `json:"token_id"`
	UserID         string            `json:"user_id"`
	Action         MCPPATAuditAction `json:"action"`
	ActorID        string            `json:"actor_id"`
	ActorType      MCPPATActorType   `json:"actor_type"`
	ActorIP        *string           `json:"actor_ip,omitempty"`
	ActorAppClient string            `json:"actor_app_client,omitempty"`
	Metadata       json.RawMessage   `json:"metadata,omitempty"`
}

// EmbeddingDims returns the vector dimension for a given embedding model name.
// Defaults to 1536 for unrecognized models.
func EmbeddingDims(embedModel string) int {
	dims := map[string]int{
		"text-embedding-ada-002": 1536,
		"text-embedding-3-small": 1536,
		"text-embedding-3-large": 3072,
	}
	if d, ok := dims[embedModel]; ok {
		return d
	}
	return 1536
}
