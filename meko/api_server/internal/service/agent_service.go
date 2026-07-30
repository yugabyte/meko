package service

import (
	"encoding/json"
	"fmt"
	"strings"

	"meko/api_server/internal/models"
	"meko/api_server/internal/repository"
)

// AgentService orchestrates agent lifecycle operations.
//
// The service exposes two surfaces:
//   - User-scoped CRUD on meko_system.agents (CreateUserAgent /
//     ListUserAgents / DeleteUserAgent).
//   - Link-only operations on meko_system.agent_access
//     (LinkAgentToDatapack, UnlinkAgentFromDatapack,
//     ListAgentsForDatapack).
type AgentService struct {
	repo *repository.AgentRepo
}

// NewAgentService creates a new AgentService.
func NewAgentService(repo *repository.AgentRepo) *AgentService {
	return &AgentService{repo: repo}
}

// CreateUserAgentRequest is the body for POST /api/v1/agents.
type CreateUserAgentRequest struct {
	AgentID string          `json:"agent_id"`
	Details json.RawMessage `json:"agent_details,omitempty"`
}

// CreateUserAgent strictly creates a user-scoped agent. agent_id is
// required. Returns repository.ErrAgentExists (wrapped) when a row
// with the same (user_id, agent_id) already exists so handlers can
// map it to a 409 Conflict; updating an existing agent is not
// supported by this endpoint by design.
func (s *AgentService) CreateUserAgent(
	userID string, req CreateUserAgentRequest,
) (*models.Agent, error) {
	if userID == "" {
		return nil, fmt.Errorf("user_id is required")
	}
	if strings.TrimSpace(req.AgentID) == "" {
		return nil, fmt.Errorf("agent_id is required")
	}

	if err := s.repo.CreateAgent(userID, req.AgentID, req.Details); err != nil {
		return nil, err
	}
	return s.repo.GetAgent(userID, req.AgentID)
}

// ListUserAgents returns all agents registered to the user.
func (s *AgentService) ListUserAgents(userID string) ([]models.Agent, error) {
	if userID == "" {
		return nil, fmt.Errorf("user_id is required")
	}
	return s.repo.ListAgentsForUser(userID)
}

// DeleteUserAgent removes a user-scoped agent and any datapack links.
func (s *AgentService) DeleteUserAgent(userID, agentID string) error {
	if userID == "" {
		return fmt.Errorf("user_id is required")
	}
	if agentID == "" {
		return fmt.Errorf("agent_id is required")
	}
	return s.repo.DeleteAgent(userID, agentID)
}

// LinkAgentRequest is the body for POST /api/v1/datapacks/:datapack_id/agents.
// The agent must already exist for this user (strict link-only model).
type LinkAgentRequest struct {
	AgentID string `json:"agent_id"`
}

// LinkAgentToDatapack inserts a row into meko_system.agent_access.
// Returns false when the agent does not exist for the user (so handlers
// can map this to a 404).
func (s *AgentService) LinkAgentToDatapack(
	userID, agentID, datapackID string,
) (bool, error) {
	if userID == "" {
		return false, fmt.Errorf("user_id is required")
	}
	if agentID == "" {
		return false, fmt.Errorf("agent_id is required")
	}
	if datapackID == "" {
		return false, fmt.Errorf("datapack_id is required")
	}
	return s.repo.LinkAgentToDatapack(userID, agentID, datapackID)
}

// UnlinkAgentFromDatapack removes only the agent_access row; the
// agent itself remains.
func (s *AgentService) UnlinkAgentFromDatapack(
	userID, agentID, datapackID string,
) error {
	if userID == "" {
		return fmt.Errorf("user_id is required")
	}
	if agentID == "" {
		return fmt.Errorf("agent_id is required")
	}
	if datapackID == "" {
		return fmt.Errorf("datapack_id is required")
	}
	return s.repo.UnlinkAgentFromDatapack(userID, agentID, datapackID)
}

// ListAgentsForDatapack returns the agents linked to the datapack for
// the given user.
func (s *AgentService) ListAgentsForDatapack(
	userID, datapackID string,
) ([]models.Agent, error) {
	if userID == "" {
		return nil, fmt.Errorf("user_id is required")
	}
	if datapackID == "" {
		return nil, fmt.Errorf("datapack_id is required")
	}
	return s.repo.ListAgentsForDatapack(userID, datapackID)
}
