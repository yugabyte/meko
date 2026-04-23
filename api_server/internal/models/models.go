// Package models defines shared data structures used across the API server.
package models

import (
	"encoding/json"
	"time"
)

// Datapack represents a registered datapack in the meko system.
type Datapack struct {
	CreatedAt      time.Time `json:"created_at"`
	UpdatedAt      time.Time `json:"updated_at"`
	ID             string    `json:"id"`
	Name           string    `json:"name"`
	AccountID      string    `json:"account_id"`
	YBUniverseID   string    `json:"yb_universe_id"`
	YBUniverseConn string    `json:"datapack_ysql_connection_string"`
	MCPUserName    string    `json:"mcp_user_name"`
	DBUserName     string    `json:"db_user_name"`
}

// Agent represents an agent within a datapack.
type Agent struct {
	CreatedAt  time.Time `json:"created_at"`
	UpdatedAt  time.Time `json:"updated_at"`
	ID         string    `json:"id"`
	DatapackID string    `json:"datapack_id"`
	Name       string    `json:"agent_name"`
	Metadata   string    `json:"agent_metadata,omitempty"`
}

// Account represents a user account in the meko system.
type Account struct {
	CreatedAt              time.Time  `json:"created_at"`
	UpdatedAt              time.Time  `json:"updated_at"`
	EmailVerifiedAt        *time.Time `json:"email_verified_at,omitempty"`
	HasCompletedOnboarding bool       `json:"has_completed_onboarding"`
	ID                     string     `json:"id"`
	FirstName              string     `json:"first_name"`
	LastName               string     `json:"last_name"`
	Email                  string     `json:"email"`
	AccountType            string     `json:"account_type"`
	CognitoSub             string     `json:"cognito_sub,omitempty"`
	MCPUserName            *string    `json:"mcp_user_name,omitempty"`
	MCPUserPassword        *string    `json:"-"`
}

// KnowledgeBase represents a knowledge base linked to a datapack.
type KnowledgeBase struct {
	CreatedAt    time.Time `json:"created_at"`
	UpdatedAt    time.Time `json:"updated_at"`
	ID           string    `json:"id"`
	DatapackID   string    `json:"datapack_id"`
	URL          string    `json:"knowledge_base_url"`
	RagSourceID  *string   `json:"rag_source_id,omitempty"`
	RagIndexName *string   `json:"rag_index_name,omitempty"`
}

// Memory represents one row from the Mem0 vector collection table.
type Memory struct {
	ID      string          `json:"id"`
	Payload json.RawMessage `json:"payload"`
}

// MemoryListFilter is the filter to filter memories based on user_id, agent_id, and datapack_id in the Mem0 vector collection table.
// FreeUser skips the vector-similarity ranking path (no LLM summary, no embedding, no ORDER BY vector).
type MemoryListFilter struct {
	UserID     string `json:"user_id"`
	AgentID    string `json:"agent_id"`
	DatapackID string `json:"datapack_id"`
	FreeUser   bool   `json:"free_user"`
	Page       int    `json:"page"`
	PageSize   int    `json:"page_size"`
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

// TenantKBSyncRequest is the body for POST /api/v1/tenant-kb/sync.
type TenantKBSyncRequest struct {
	UserID     string   `json:"user_id"`
	AgentID    string   `json:"agent_id,omitempty"`
	DatapackID string   `json:"datapack_id"`
	MemoryIDs  []string `json:"memory_ids"`
}

// TenantKBSyncResult summarizes what happened during mem0 → knowledge_base sync.
type TenantKBSyncResult struct {
	InsertedIDs []string `json:"inserted_ids"`
	UpdatedIDs  []string `json:"updated_ids"`
	NotFoundIDs []string `json:"not_found_ids"`
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
