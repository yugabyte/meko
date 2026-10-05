// Package service implements business logic for the datapack API.
package service

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"errors"
	"fmt"
	"log/slog"
	"regexp"
	"strings"
	"unicode/utf8"

	_ "github.com/lib/pq"

	"meko/api_server/internal/models"
	"meko/api_server/internal/repository"
)

const defaultDatapackName = "meko_default_datapack"

const (
	minDatapackNameLength = 3
	maxDatapackNameLength = 60
)

var htmlTagPattern = regexp.MustCompile(`<[^>]*>`)

var ErrInvalidDatapackName = errors.New("invalid datapack name")

type invalidDatapackNameError struct {
	message string
}

func (e invalidDatapackNameError) Error() string {
	return e.message
}

func (e invalidDatapackNameError) Is(target error) bool {
	return target == ErrInvalidDatapackName
}

func newInvalidDatapackNameError(message string) error {
	return invalidDatapackNameError{message: message}
}

func validateDatapackName(name string) error {
	// Datapack names are mirrored to Langfuse project names during create and
	// rename, so this intentionally mirrors Langfuse's projectNameSchema:
	// StringNoHTML.min(3, "Must have at least 3 characters").max(60, ...),
	// where StringNoHTML uses /<[^>]*>/g.
	if name == "" {
		return newInvalidDatapackNameError("name is required")
	}
	nameLength := utf8.RuneCountInString(name)
	if nameLength < minDatapackNameLength {
		return newInvalidDatapackNameError("Must have at least 3 characters")
	}
	if nameLength > maxDatapackNameLength {
		return newInvalidDatapackNameError("Must have at most 60 characters")
	}
	if htmlTagPattern.MatchString(name) {
		return newInvalidDatapackNameError("Text cannot contain HTML tags")
	}
	return nil
}

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
	langfuseSvc *LangfuseService
	// freeTierRepo reads the lifetime `datapack_creation` counter the
	// Create transaction increments; the free-tier cap gates on it so
	// deleting a datapack never refunds quota. Optional: when nil the
	// cap is not enforced (matches early-bootstrap minimal wiring).
	freeTierRepo *repository.FreeTierUsageRepo
	// convSearchOptOutRepo backs the conversation-search opt-out toggle.
	// Deliberately NOT part of UpdateRequest/Update: that endpoint is
	// owner-only (RequireDatapackOwnership), while the opt-out toggle must
	// also allow a shared maintainer — see SetConversationSearchOptOut and
	// its dedicated route/middleware in router.go.
	convSearchOptOutRepo *repository.ConversationSearchOptOutRepo
	// ampSvc, when set (AMP feature enabled), tears down the datapack's AMP
	// clusters + pool mapping on delete. Nil leaves deletion AMP-blind.
	ampSvc *AMPService
}

// SetAMPService wires the optional AMP cleanup run on datapack deletion. Called
// from router setup only when --show-pg-amp is on; nil otherwise.
func (s *DatapackService) SetAMPService(amp *AMPService) { s.ampSvc = amp }

// NewDatapackService creates a new DatapackService.
func NewDatapackService(
	repo *repository.DatapackRepo,
	ybaSvc *YBAService,
	accountRepo *repository.AccountRepo,
	langfuseSvc *LangfuseService,
	freeTierRepo *repository.FreeTierUsageRepo,
	convSearchOptOutRepo *repository.ConversationSearchOptOutRepo,
) *DatapackService {
	return &DatapackService{
		repo:                 repo,
		ybaSvc:               ybaSvc,
		accountRepo:          accountRepo,
		langfuseSvc:          langfuseSvc,
		freeTierRepo:         freeTierRepo,
		convSearchOptOutRepo: convSearchOptOutRepo,
	}
}

// ConversationSearchOptedOut reports whether datapackID has opted out of
// conversation-search embedding. Readable by anyone with datapack access
// (see the route's RequireDatapackAccess-only gate) — only *changing* it is
// restricted to owner/maintainer.
func (s *DatapackService) ConversationSearchOptedOut(datapackID string) (bool, error) {
	return s.convSearchOptOutRepo.IsOptedOut(datapackID)
}

// SetConversationSearchOptOut sets or clears the opt-out for datapackID.
// userID is the acting account, stamped as opted_out_by on opt-out. Callers
// must already have verified the caller is the datapack's owner or a
// maintainer (see RequireDatapackOwnerOrMaintainerGrant on this method's route)
// — this method itself does not re-check access.
func (s *DatapackService) SetConversationSearchOptOut(userID, datapackID string, optedOut bool) error {
	if optedOut {
		return s.convSearchOptOutRepo.SetOptedOut(datapackID, userID)
	}
	return s.convSearchOptOutRepo.ClearOptedOut(datapackID)
}

// CreateRequest holds the fields for creating a datapack.
//
// AgentID is optional in the API: when empty the service falls back
// to repository.DefaultAgentID (the user's default agent created at
// signup). When set it must reference an existing agent owned by the
// same user, otherwise the create fails with a not-found error.
type CreateRequest struct {
	Name        string `json:"name"`
	UserID      string `json:"user_id"`
	AgentID     string `json:"agent_id,omitempty"`
	Description string `json:"description,omitempty"`
	// UserType is the requester's `meko_system.users.user_type` ("free"
	// / "pro" / "enterprise"). Populated by the handler from the auth
	// context; paid tiers skip the free-tier datapack count cap.
	UserType string `json:"-"`
	// FolderID files the new datapack into a project folder. Required once the
	// Projects feature is on and the caller has at least one project — see
	// ProjectService.ValidateNewDatapackFolder. Ignored when the feature is
	// off, so the field is additive for existing clients.
	FolderID string `json:"folder_id,omitempty"`
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
// exist, and inserts the datapack + agent_access link in a single
// transaction. No per-datapack database is provisioned.
//
// The agent referenced by AgentID (or DefaultAgentID when empty) must
// already exist in meko_system.agents for this user. The default
// agent is created at signup; additional agents are created via
// POST /agents.
func (s *DatapackService) Create(req CreateRequest) (*CreateResponse, error) {
	req.Name = strings.TrimSpace(req.Name)
	if err := validateDatapackName(req.Name); err != nil {
		return nil, err
	}
	if req.UserID == "" {
		return nil, fmt.Errorf("user_id is required")
	}

	// Free-tier cap. Applies ONLY when user_type is explicitly "free";
	// every other tier (Pro / Enterprise / unknown) skips this block.
	// Up front so the failure is cheap for free tier.
	//
	// Gates on the LIFETIME `datapack_creation` counter (incremented by
	// DatapackRepo.Create, never decremented) rather than the live
	// datapack count, so delete-and-recreate cannot reset the quota.
	// When the counter is below the cap, take the max with the live
	// count so legacy users whose datapacks predate the counter can't
	// under-gate; the extra query is skipped once the lifetime cap is
	// already hit.
	// Tier cap: applies to free + standard; pro / enterprise skip.
	// Standard has a higher cap but the same enforcement path.
	if IsGatedUserType(req.UserType) && s.freeTierRepo != nil {
		limits := LimitsFor(req.UserType)
		n, err := s.freeTierRepo.RequestCount(req.UserID, repository.MetricDatapackCreation)
		if err != nil {
			return nil, fmt.Errorf("tier cap check: %w", err)
		}
		if n < limits.MaxDatapacks {
			liveCount, liveErr := s.repo.CountByOwner(req.UserID)
			if liveErr != nil {
				return nil, fmt.Errorf("tier cap check (live count): %w", liveErr)
			}
			if liveCount > n {
				n = liveCount
			}
		}
		if n >= limits.MaxDatapacks {
			slog.Info("datapack create blocked by tier cap",
				"user_id", req.UserID, "user_type", req.UserType,
				"lifetime_creations", n, "cap", limits.MaxDatapacks)
			return nil, ErrFreeTierDatapackLimit
		}
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
		Description: req.Description,
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
	Name              string `json:"name,omitempty"`
	Description       string `json:"description,omitempty"`
}

// Update modifies a datapack's configuration identified by its stable UUID.
// When the name is being changed and Langfuse is configured, the corresponding
// Langfuse project is renamed as a best-effort operation: failure is logged but
// does not roll back the local DB rename.
func (s *DatapackService) Update(
	userID, datapackID string, req UpdateRequest,
) error {
	if datapackID == "" {
		return fmt.Errorf("datapack_id is required")
	}
	if userID == "" {
		return fmt.Errorf("user_id is required")
	}

	// Resolve the current row so rename guards/no-op detection compare
	// against the datapack's existing name rather than a URL segment.
	dp, err := s.repo.GetByID(userID, datapackID)
	if err != nil {
		return fmt.Errorf("get datapack before update: %w", err)
	}
	if dp == nil {
		return fmt.Errorf("datapack %q not found", datapackID)
	}

	if req.Name != "" {
		req.Name = strings.TrimSpace(req.Name)
		if err := validateDatapackName(req.Name); err != nil {
			return err
		}
		// Normalise: if the client sends the same name back, treat it as no-op
		// for the rename path so the default-datapack guard and Langfuse sync
		// are not triggered unnecessarily.
		if req.Name == dp.Name {
			req.Name = ""
		}
	}
	if req.Name != "" && dp.Name == defaultDatapackName {
		return fmt.Errorf("%s cannot be renamed", defaultDatapackName)
	}

	if err := s.repo.UpdateByID(userID, datapackID, repository.UpdateDatapackParams{
		YBConnStr:   req.YBUniverseConnStr,
		Name:        req.Name,
		Description: req.Description,
	}); err != nil {
		return err
	}

	if req.Name != "" && s.langfuseSvc != nil {
		if err := s.langfuseSvc.RenameProject(userID, datapackID, req.Name); err != nil {
			slog.Warn("langfuse project rename failed after datapack rename",
				"datapack_id", datapackID,
				"new_name", req.Name,
				"error", err,
			)
		}
	}
	return nil
}

// Delete removes the Langfuse project (when configured), deletes the
// datapack registry entry, and releases the universe slot. There is no
// per-datapack database to drop in the multi-tenant model.
func (s *DatapackService) Delete(userID, datapackID string) error {
	if datapackID == "" {
		return fmt.Errorf("datapack_id is required")
	}
	if userID == "" {
		return fmt.Errorf("user_id is required")
	}

	dp, err := s.repo.GetByID(userID, datapackID)
	if err != nil {
		return fmt.Errorf("failed to look up datapack: %w", err)
	}
	if dp == nil {
		return fmt.Errorf("datapack %q not found", datapackID)
	}

	if s.langfuseSvc != nil {
		if err := s.langfuseSvc.CleanupForDeletedDatapack(userID, dp.ID); err != nil {
			return fmt.Errorf("failed to cleanup langfuse project before deleting datapack: %w", err)
		}
	}

	// Tear down the datapack's AMP clusters + pool mapping (best-effort, before
	// the registry row goes so the pool is still resolvable). AMP being down
	// must not block datapack deletion, so a failure is logged, not fatal.
	if s.ampSvc != nil {
		if err := s.ampSvc.CleanupForDeletedDatapack(context.Background(), dp.ID); err != nil {
			slog.Warn("amp cleanup failed during datapack deletion; resources may be orphaned",
				"datapack_id", dp.ID, "error", err)
		}
	}

	if err := s.repo.DeleteByID(userID, datapackID); err != nil {
		return fmt.Errorf("failed to delete registry entry: %w", err)
	}

	if s.langfuseSvc != nil {
		slog.Info("langfuse cleanup completed for datapack deletion",
			"user_id", userID,
			"datapack_id", dp.ID,
			"datapack_name", dp.Name,
		)
	}

	if err := s.ybaSvc.ReleaseUniverse(dp.UniverseID); err != nil {
		slog.Warn("failed to release universe slot",
			"universe_id", dp.UniverseID, "error", err)
	}

	slog.Info("datapack deleted successfully", "datapack_id", datapackID, "datapack", dp.Name)
	return nil
}

// Status returns summary information about a datapack by stable UUID.
func (s *DatapackService) Status(
	userID, datapackID string,
) (*repository.DatapackStatus, error) {
	if datapackID == "" {
		return nil, fmt.Errorf("datapack_id is required")
	}
	if userID == "" {
		return nil, fmt.Errorf("user_id is required")
	}
	st, err := s.repo.GetStatusByID(userID, datapackID)
	if err != nil {
		return nil, err
	}
	if st == nil {
		return nil, fmt.Errorf("datapack %q not found", datapackID)
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
