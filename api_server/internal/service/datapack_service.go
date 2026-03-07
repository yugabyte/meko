// Package service implements business logic for the datapack API.
package service

import (
	"fmt"

	"meko/api_server/internal/models"
	"meko/api_server/internal/repository"
)

// DatapackService orchestrates datapack lifecycle operations.
type DatapackService struct {
	repo *repository.DatapackRepo
}

// NewDatapackService creates a new DatapackService.
func NewDatapackService(repo *repository.DatapackRepo) *DatapackService {
	return &DatapackService{repo: repo}
}

// CreateRequest holds the fields for creating a datapack. Only name and
// seed_knowledge_base are required; embed_model and inference_model can be
// configured later via `meko config select_*` commands.
type CreateRequest struct {
	Name           string `json:"name"`
	EmbedModel     string `json:"embed_model,omitempty"`
	InferenceModel string `json:"inference_model,omitempty"`
	KnowledgeBase  string `json:"seed_knowledge_base"`
}

// CreateResponse holds the result of creating a datapack.
type CreateResponse struct {
	DatapackID string `json:"datapack_id"`
	AgentID    string `json:"agent_id"`
	Name       string `json:"name"`
	ConnectURL string `json:"connect_url"`
}

// Create validates the request and creates a new datapack.
func (s *DatapackService) Create(req CreateRequest) (*CreateResponse, error) {
	if req.Name == "" {
		return nil, fmt.Errorf("name is required")
	}
	if req.KnowledgeBase == "" {
		return nil, fmt.Errorf("seed_knowledge_base is required")
	}

	dpID, agentID, err := s.repo.Create(
		req.Name, req.EmbedModel, req.InferenceModel, req.KnowledgeBase,
	)
	if err != nil {
		return nil, fmt.Errorf("failed to create datapack: %w", err)
	}

	return &CreateResponse{
		DatapackID: dpID,
		AgentID:    agentID,
		Name:       req.Name,
		ConnectURL: fmt.Sprintf(
			"https://localhost:5435/mcp/%s", req.Name,
		),
	}, nil
}

// List returns all registered datapacks.
func (s *DatapackService) List() ([]models.Datapack, error) {
	return s.repo.List()
}

// Describe returns details for a single datapack by name.
func (s *DatapackService) Describe(name string) (*models.Datapack, error) {
	if name == "" {
		return nil, fmt.Errorf("name is required")
	}
	dp, err := s.repo.GetByName(name)
	if err != nil {
		return nil, err
	}
	if dp == nil {
		return nil, fmt.Errorf("datapack %q not found", name)
	}
	return dp, nil
}

// UpdateRequest holds the mutable fields for updating a datapack.
type UpdateRequest struct {
	LLMModel       string `json:"llm_model,omitempty"`
	EmbeddingModel string `json:"embedding_model,omitempty"`
}

// Update modifies a datapack's configuration.
func (s *DatapackService) Update(
	name string, req UpdateRequest,
) error {
	if name == "" {
		return fmt.Errorf("name is required")
	}
	return s.repo.Update(name, req.LLMModel, req.EmbeddingModel)
}

// Delete removes a datapack by name.
func (s *DatapackService) Delete(name string) error {
	if name == "" {
		return fmt.Errorf("name is required")
	}
	return s.repo.Delete(name)
}

// Status returns summary information about a datapack.
func (s *DatapackService) Status(
	name string,
) (*repository.DatapackStatus, error) {
	if name == "" {
		return nil, fmt.Errorf("name is required")
	}
	st, err := s.repo.GetStatus(name)
	if err != nil {
		return nil, err
	}
	if st == nil {
		return nil, fmt.Errorf("datapack %q not found", name)
	}
	return st, nil
}
