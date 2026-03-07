package service

import (
	"fmt"

	"meko/api_server/internal/models"
	"meko/api_server/internal/repository"
)

// AgentService orchestrates agent lifecycle operations.
type AgentService struct {
	repo *repository.AgentRepo
}

// NewAgentService creates a new AgentService.
func NewAgentService(repo *repository.AgentRepo) *AgentService {
	return &AgentService{repo: repo}
}

// CreateAgentRequest holds the fields for creating an agent.
type CreateAgentRequest struct {
	AgentName string `json:"agent_name"`
	Metadata  string `json:"agent_metadata,omitempty"`
}

// Create validates and creates a new agent under the given datapack.
func (s *AgentService) Create(
	datapackName string, req CreateAgentRequest,
) (string, error) {
	if datapackName == "" {
		return "", fmt.Errorf("datapack name is required")
	}
	if req.AgentName == "" {
		return "", fmt.Errorf("agent_name is required")
	}
	return s.repo.Create(datapackName, req.AgentName, req.Metadata)
}

// List returns all agents for a datapack.
func (s *AgentService) List(
	datapackName string,
) ([]models.Agent, error) {
	if datapackName == "" {
		return nil, fmt.Errorf("datapack name is required")
	}
	return s.repo.ListByDatapack(datapackName)
}

// Delete removes an agent from a datapack.
func (s *AgentService) Delete(datapackName, agentName string) error {
	if datapackName == "" {
		return fmt.Errorf("datapack name is required")
	}
	if agentName == "" {
		return fmt.Errorf("agent name is required")
	}
	return s.repo.Delete(datapackName, agentName)
}
