// Package models defines shared data structures used across the API server.
package models

import (
	"encoding/json"
	"strings"
	"time"
)

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
	MemoryCount      int         `json:"memory_count"`
	KnowledgeCount   int         `json:"knowledge_count"`
	LearningsCount   int         `json:"learnings_count"`
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
type Memory struct {
	ID      string `json:"id"`
	Data    string `json:"data"`
	AgentID string `json:"agent_id"`
}

// MemoryListFilter is the filter for the Mem0 collection (datapacks.meko_mem0_collection).
// DatapackID is required; UserID is optional. UserType ("free" | "pro" | "enterprise")
// selects the query path: only "pro" / "enterprise" run the vector-similarity ranking
// (LLM summary + embedding + ORDER BY embeddings); everything else runs the plain SELECT.
type MemoryListFilter struct {
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
	Criteria   MemoryListFilter `json:"criteria"`
	Memories   []Memory         `json:"memories"`
	TopK       int              `json:"top_k"`
	Total      int              `json:"total"`
	Page       int              `json:"page"`
	PageSize   int              `json:"page_size"`
	TotalPages int              `json:"total_pages"`
}

// MemoryCountResponse is the response to the memories count API. It returns
// the total number of rows in datapacks.meko_mem0_collection for a given
// datapack_id, with no additional filtering applied.
type MemoryCountResponse struct {
	DatapackID string `json:"datapack_id"`
	Count      int    `json:"count"`
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
	InviteID           string  `json:"invite_id"`
	GrantRole          string  `json:"grant"`
	RecipientUserID    *string `json:"recipient_user_id"`
	RecipientUserName  *string `json:"recipient_user_name"`
	RecipientEmailID   string  `json:"recipient_email_id"`
}

// OutgoingPendingInvitesResponse is returned by GET /datapacks/:name/outgoing-shares
// (pending invites only; owner or maintainer).
type OutgoingPendingInvitesResponse struct {
	PendingInvites []OutgoingSharePendingInvite `json:"pending_invites"`
}

// DatapackMembersResponse is returned by GET /datapacks/:name/member-list
// (accepted members and metadata; any datapack access).
type DatapackMembersResponse struct {
	Members []OutgoingShareMember `json:"members"`
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
