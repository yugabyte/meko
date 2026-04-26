// Package service implements business logic for the datapack API.
package service

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"errors"
	"fmt"
	"log/slog"
	"strings"

	_ "github.com/lib/pq"

	"meko/api_server/internal/models"
	"meko/api_server/internal/repository"
)

// DatapackService orchestrates datapack lifecycle operations.
//
// Multi-tenant model: every datapack is just a row in
// meko_system.datapacks. The shared YBA universe is selected via
// YBAService. The user's MCP role lives in
// meko_system.users.user_details and is reused across all of that
// user's datapacks.
type DatapackService struct {
	repo        *repository.DatapackRepo
	ybaSvc      *YBAService
	accountRepo *repository.AccountRepo
}

// NewDatapackService creates a new DatapackService.
func NewDatapackService(
	repo *repository.DatapackRepo,
	ybaSvc *YBAService,
	accountRepo *repository.AccountRepo,
) *DatapackService {
	return &DatapackService{repo: repo, ybaSvc: ybaSvc, accountRepo: accountRepo}
}

// CreateRequest holds the fields for creating a datapack.
//
// AgentID is optional in the API: when empty the service falls back
// to repository.DefaultAgentID (the user's default agent created at
// signup). When set it must reference an existing agent owned by the
// same user, otherwise the create fails with a not-found error.
type CreateRequest struct {
	Name    string `json:"name"`
	UserID  string `json:"user_id"`
	AgentID string `json:"agent_id,omitempty"`
}

// MCPServerInfo holds the MCP server connection details returned after
// datapack creation.
type MCPServerInfo struct {
	URL    string `json:"url"`
	APIKey string `json:"api_key"`
}

// DBAccessInfo holds the YSQL access credentials. These are the
// shared user-level MCP credentials; every datapack for the same user
// reuses them.
type DBAccessInfo struct {
	UserName string `json:"user_name"`
	Password string `json:"password"`
	ConnStr  string `json:"conn_str"`
}

// CreateResponse holds the result of creating a datapack.
type CreateResponse struct {
	DatapackID  string        `json:"datapack_id"`
	AgentID     string        `json:"agent_id"`
	Name        string        `json:"name"`
	MCPUserName string        `json:"mcp_user_name"`
	MCPServer   MCPServerInfo `json:"mcp_server"`
	DBAccess    DBAccessInfo  `json:"db_access"`
}

// Create assigns a universe slot, ensures the user's MCP credentials
// exist, and inserts the datapack + agent_datapacks link in a single
// transaction. No per-datapack database is provisioned.
//
// The agent referenced by AgentID (or DefaultAgentID when empty) must
// already exist in meko_system.agents for this user. The default
// agent is created at signup; additional agents are created via
// POST /agents.
func (s *DatapackService) Create(req CreateRequest) (*CreateResponse, error) {
	if req.Name == "" {
		return nil, fmt.Errorf("name is required")
	}
	if req.UserID == "" {
		return nil, fmt.Errorf("user_id is required")
	}

	agentID := strings.TrimSpace(req.AgentID)
	if agentID == "" {
		agentID = repository.DefaultAgentID
	}

	ctx := context.Background()

	mcpCreds, err := s.accountRepo.GetMCPCredentials(ctx, req.UserID)
	if err != nil {
		return nil, fmt.Errorf("failed to look up MCP credentials: %w", err)
	}

	var mcpUserName, mcpPassword string
	if mcpCreds != nil {
		mcpUserName = mcpCreds.UserName
		mcpPassword = mcpCreds.Password
		slog.Info("reusing existing user MCP credentials",
			"user_id", req.UserID, "mcp_user", mcpUserName)
	} else {
		mcpUserName = fmt.Sprintf("mcp_%s", req.UserID)
		mcpPassword, err = randomServiceHex(16)
		if err != nil {
			return nil, fmt.Errorf("failed to generate MCP password: %w", err)
		}
		if err := s.accountRepo.SetMCPCredentials(
			ctx, req.UserID, mcpUserName, mcpPassword,
		); err != nil {
			return nil, fmt.Errorf("failed to store MCP credentials: %w", err)
		}
		slog.Info("created new user MCP credentials",
			"user_id", req.UserID, "mcp_user", mcpUserName)
	}

	universe, err := s.ybaSvc.AssignUniverse()
	if err != nil {
		return nil, fmt.Errorf("failed to assign universe: %w", err)
	}

	res, err := s.repo.Create(repository.CreateDatapackParams{
		Name:        req.Name,
		UserID:      req.UserID,
		AgentID:     agentID,
		UniverseID:  universe.UniverseID,
		ConnStr:     universe.AdminConnStr,
		MCPUserName: mcpUserName,
		MCPPassword: mcpPassword,
	})
	if err != nil {
		_ = s.ybaSvc.ReleaseUniverse(universe.UniverseID)
		if errors.Is(err, repository.ErrAgentNotFound) {
			return nil, fmt.Errorf(
				"agent %q not found for user; create it via POST /agents first",
				agentID,
			)
		}
		return nil, fmt.Errorf("failed to create datapack: %w", err)
	}

	return &CreateResponse{
		DatapackID:  res.DatapackID,
		AgentID:     res.AgentID,
		Name:        req.Name,
		MCPUserName: res.MCPUserName,
		MCPServer: MCPServerInfo{
			URL:    fmt.Sprintf("https://%s.mcp.mekodev.com/mcp", res.UserID),
			APIKey: res.MCPPassword,
		},
		DBAccess: DBAccessInfo{
			UserName: res.MCPUserName,
			Password: res.MCPPassword,
			ConnStr:  res.ConnStr,
		},
	}, nil
}

// List returns datapacks belonging to the given user.
func (s *DatapackService) List(userID string) ([]models.Datapack, error) {
	if userID == "" {
		return nil, fmt.Errorf("user_id is required")
	}
	return s.repo.List(userID)
}

// Describe returns details for a single datapack by user and name.
func (s *DatapackService) Describe(userID, name string) (*models.Datapack, error) {
	if name == "" {
		return nil, fmt.Errorf("name is required")
	}
	if userID == "" {
		return nil, fmt.Errorf("user_id is required")
	}
	dp, err := s.repo.GetByName(userID, name)
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
	YBUniverseConnStr string `json:"datapack_ysql_connection_string,omitempty"`
}

// Update modifies a datapack's configuration.
func (s *DatapackService) Update(
	userID, name string, req UpdateRequest,
) error {
	if name == "" {
		return fmt.Errorf("name is required")
	}
	if userID == "" {
		return fmt.Errorf("user_id is required")
	}
	return s.repo.Update(userID, name, req.YBUniverseConnStr)
}

// Delete removes the datapack registry entry and releases the universe
// slot. There is no per-datapack database to drop in the multi-tenant
// model.
func (s *DatapackService) Delete(userID, name string) error {
	if name == "" {
		return fmt.Errorf("name is required")
	}
	if userID == "" {
		return fmt.Errorf("user_id is required")
	}

	dp, err := s.repo.GetByName(userID, name)
	if err != nil {
		return fmt.Errorf("failed to look up datapack: %w", err)
	}
	if dp == nil {
		return fmt.Errorf("datapack %q not found", name)
	}

	if err := s.repo.Delete(userID, name); err != nil {
		return fmt.Errorf("failed to delete registry entry: %w", err)
	}

	if err := s.ybaSvc.ReleaseUniverse(dp.UniverseID); err != nil {
		slog.Warn("failed to release universe slot",
			"universe_id", dp.UniverseID, "error", err)
	}

	slog.Info("datapack deleted successfully", "datapack", name)
	return nil
}

// Status returns summary information about a datapack.
func (s *DatapackService) Status(
	userID, name string,
) (*repository.DatapackStatus, error) {
	if name == "" {
		return nil, fmt.Errorf("name is required")
	}
	if userID == "" {
		return nil, fmt.Errorf("user_id is required")
	}
	st, err := s.repo.GetStatus(userID, name)
	if err != nil {
		return nil, err
	}
	if st == nil {
		return nil, fmt.Errorf("datapack %q not found", name)
	}
	return st, nil
}

func randomServiceHex(nBytes int) (string, error) {
	b := make([]byte, nBytes)
	if _, err := rand.Read(b); err != nil {
		return "", err
	}
	return hex.EncodeToString(b), nil
}
