// Package service implements business logic for the datapack API.
package service

import (
	"context"
	"database/sql"
	"crypto/rand"
	"encoding/hex"
	"fmt"
	"log/slog"

	_ "github.com/lib/pq"

	"meko/api_server/internal/models"
	"meko/api_server/internal/repository"
)

// DatapackService orchestrates datapack lifecycle operations.
type DatapackService struct {
	repo        *repository.DatapackRepo
	ybaSvc      *YBAService
	accountRepo *repository.AccountRepo
}

// NewDatapackService creates a new DatapackService.
func NewDatapackService(repo *repository.DatapackRepo, ybaSvc *YBAService, accountRepo *repository.AccountRepo) *DatapackService {
	return &DatapackService{repo: repo, ybaSvc: ybaSvc, accountRepo: accountRepo}
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

	ctx := context.Background()

	mcpCreds, err := s.accountRepo.GetMCPCredentials(ctx, req.AccountID)
	if err != nil {
		return nil, fmt.Errorf("failed to look up MCP credentials: %w", err)
	}

	mcpUserExists := mcpCreds != nil
	var mcpUserName, mcpPassword string
	if mcpUserExists {
		mcpUserName = mcpCreds.UserName
		mcpPassword = mcpCreds.Password
		slog.Info("reusing existing account MCP user",
			"account_id", req.AccountID, "mcp_user", mcpUserName)
	} else {
		mcpUserName = fmt.Sprintf("mcp_%s", req.AccountID)
		mcpPassword, err = randomServiceHex(16)
		if err != nil {
			return nil, fmt.Errorf("failed to generate MCP password: %w", err)
		}
		if err := s.accountRepo.SetMCPCredentials(ctx, req.AccountID, mcpUserName, mcpPassword); err != nil {
			return nil, fmt.Errorf("failed to store MCP credentials: %w", err)
		}
		slog.Info("created new account MCP user",
			"account_id", req.AccountID, "mcp_user", mcpUserName)
	}

	universe, err := s.ybaSvc.AssignUniverse()
	if err != nil {
		return nil, fmt.Errorf("failed to assign universe: %w", err)
	}

	res, err := s.repo.Create(repository.CreateDatapackParams{
		Name:           req.Name,
		AccountID:      req.AccountID,
		YBUniverseID:   universe.UniverseID,
		YBHostname:     universe.Hostname,
		YBPort:         universe.Port,
		YBAdminConnStr: universe.AdminConnStr,
		MCPUserName:    mcpUserName,
		MCPPassword:    mcpPassword,
		MCPUserExists:  mcpUserExists,
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
			URL:    fmt.Sprintf("https://%s.mcp.mekodev.com/mcp", res.AccountID),
			APIKey: res.MCPPassword,
		},
		DBAccess: DBAccessInfo{
			UserName: res.DBUserName,
			Password: res.DBPassword,
			ConnStr:  res.DBConnStr,
		},
	}, nil
}

// List returns datapacks belonging to the given account.
func (s *DatapackService) List(accountID string) ([]models.Datapack, error) {
	if accountID == "" {
		return nil, fmt.Errorf("account_id is required")
	}
	return s.repo.List(accountID)
}

// Describe returns details for a single datapack by account and name.
func (s *DatapackService) Describe(accountID, name string) (*models.Datapack, error) {
	if name == "" {
		return nil, fmt.Errorf("name is required")
	}
	if accountID == "" {
		return nil, fmt.Errorf("account_id is required")
	}
	dp, err := s.repo.GetByName(accountID, name)
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
	accountID, name string, req UpdateRequest,
) error {
	if name == "" {
		return fmt.Errorf("name is required")
	}
	if accountID == "" {
		return fmt.Errorf("account_id is required")
	}
	return s.repo.Update(accountID, name, req.YBUniverseConnStr)
}

// Delete tears down the underlying database, removes the registry
// entry, and releases the universe slot.
func (s *DatapackService) Delete(accountID, name string) error {
	if name == "" {
		return fmt.Errorf("name is required")
	}
	if accountID == "" {
		return fmt.Errorf("account_id is required")
	}

	dp, err := s.repo.GetByName(accountID, name)
	if err != nil {
		return fmt.Errorf("failed to look up datapack: %w", err)
	}
	if dp == nil {
		return fmt.Errorf("datapack %q not found", name)
	}

	adminConnStr, err := s.ybaSvc.GetAdminConnStr(dp.YBUniverseID)
	if err != nil {
		return fmt.Errorf("failed to get admin connection: %w", err)
	}

	dbName := fmt.Sprintf("%s_%s", dp.AccountID, dp.Name)

	slog.Info("dropping datapack database",
		"datapack", name, "db_name", dbName)

	adminDB, err := sql.Open("postgres", adminConnStr)
	if err != nil {
		return fmt.Errorf("failed to connect to universe: %w", err)
	}
	defer adminDB.Close()

	if err := repository.DropDatapackDB(adminDB, dbName, dp.DBUserName); err != nil {
		return fmt.Errorf("failed to drop datapack database: %w", err)
	}

	slog.Info("datapack database dropped, removing registry entry",
		"datapack", name)

	if err := s.repo.Delete(accountID, name); err != nil {
		return fmt.Errorf("failed to delete registry entry: %w", err)
	}

	if err := s.ybaSvc.ReleaseUniverse(dp.YBUniverseID); err != nil {
		slog.Warn("failed to release universe slot",
			"universe_id", dp.YBUniverseID, "error", err)
	}

	slog.Info("datapack deleted successfully", "datapack", name)
	return nil
}

// Status returns summary information about a datapack.
func (s *DatapackService) Status(
	accountID, name string,
) (*repository.DatapackStatus, error) {
	if name == "" {
		return nil, fmt.Errorf("name is required")
	}
	if accountID == "" {
		return nil, fmt.Errorf("account_id is required")
	}
	st, err := s.repo.GetStatus(accountID, name)
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
