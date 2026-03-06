// Package models defines shared data structures used across the API server.
package models

import "time"

// Datapack represents a registered datapack in the meko system.
type Datapack struct {
	CreatedAt      time.Time `json:"created_at"`
	UpdatedAt      time.Time `json:"updated_at"`
	ID             string    `json:"id"`
	Name           string    `json:"name"`
	AccountID      string    `json:"account_id"`
	LLMModel       string    `json:"llm_model"`
	EmbeddingModel string    `json:"embedding_model"`
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

// KnowledgeBase represents a knowledge base linked to a datapack.
type KnowledgeBase struct {
	CreatedAt  time.Time `json:"created_at"`
	UpdatedAt  time.Time `json:"updated_at"`
	ID         string    `json:"id"`
	DatapackID string    `json:"datapack_id"`
	URL        string    `json:"knowledge_base_url"`
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
