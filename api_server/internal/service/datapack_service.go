// Package service implements business logic for the datapack API.
package service

import (
	"fmt"

	"meko/api_server/internal/models"
	"meko/api_server/internal/repository"
)

// DatapackService orchestrates datapack lifecycle operations.
type DatapackService struct {
	repo   *repository.DatapackRepo
	ybaSvc *YBAService
}

// NewDatapackService creates a new DatapackService.
func NewDatapackService(repo *repository.DatapackRepo, ybaSvc *YBAService) *DatapackService {
	return &DatapackService{repo: repo, ybaSvc: ybaSvc}
}

// CreateRequest holds the fields for creating a datapack.
type CreateRequest struct {
	Name string `json:"name"`
	AccountID string `json:"account_id"`
}

// MCPServerInfo holds the MCP server connection details returned after
// datapack creation.
type MCPServerInfo struct {
	URL    string `json:"url"`
	APIKey string `json:"api_key"`
}

// DBAccessInfo holds the direct database access credentials.
type DBAccessInfo struct {
	UserName string `json:"user_name"`
	Password string `json:"password"`
	ConnStr  string `json:"conn_str"`
}

// CreateResponse holds the result of creating a datapack.
type CreateResponse struct {
	DatapackID string        `json:"datapack_id"`
	AgentID    string        `json:"agent_id"`
	Name       string        `json:"name"`
	MCPServer  MCPServerInfo `json:"mcp_server"`
	DBAccess   DBAccessInfo  `json:"db_access"`
}

// Create validates the request, assigns a YBA universe, and creates a new datapack.
func (s *DatapackService) Create(req CreateRequest) (*CreateResponse, error) {
	if req.Name == "" {
		return nil, fmt.Errorf("name is required")
	}
	if req.AccountID == "" {
		return nil, fmt.Errorf("account_id is required")
	}

	universe, err := s.ybaSvc.AssignUniverse()
	if err != nil {
		return nil, fmt.Errorf("failed to assign universe: %w", err)
	}

	res, err := s.repo.Create(repository.CreateDatapackParams{
		Name:         req.Name,
		AccountID:    req.AccountID,
		YBUniverseID: universe.UniverseID,
		YBHostname:   universe.Hostname,
		YBPort:       universe.Port,
		YBAdminConnStr: universe.AdminConnStr,
	})
	if err != nil {
		_ = s.ybaSvc.ReleaseUniverse(universe.UniverseID)
		return nil, fmt.Errorf("failed to create datapack: %w", err)
	}

	return &CreateResponse{
		DatapackID: res.DatapackID,
		AgentID:    res.AgentID,
		Name:       req.Name,
		MCPServer: MCPServerInfo{
			URL:    fmt.Sprintf("https://mekodev.com/%s/mcp", res.AccountID),
			APIKey: res.MCPPassword,
		},
		DBAccess: DBAccessInfo{
			UserName: res.DBUserName,
			Password: res.DBPassword,
			ConnStr:  res.DBConnStr,
		},
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
	YBUniverseConnStr string `json:"datapack_ysql_connection_string,omitempty"`
}

// Update modifies a datapack's configuration.
func (s *DatapackService) Update(
	name string, req UpdateRequest,
) error {
	if name == "" {
		return fmt.Errorf("name is required")
	}
	return s.repo.Update(name, req.YBUniverseConnStr)
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



