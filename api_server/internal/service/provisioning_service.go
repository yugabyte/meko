package service

import (
	"context"
	"fmt"
	"log/slog"
	"strings"

	"meko/api_server/internal/models"
)

// DefaultDatapackName is the name assigned to the default datapack
// created at signup. The (user_id, datapack_name) UNIQUE constraint on
// meko_system.datapacks lets us look it up by name later.
const DefaultDatapackName = "meko_default_datapack"

// DatapackCreator abstracts the datapack creation operation, enabling
// test doubles.
type DatapackCreator interface {
	Create(req CreateRequest) (*CreateResponse, error)
}

// AccountLookup abstracts account profile lookups for provisioning.
type AccountLookup interface {
	GetByID(ctx context.Context, id string) (*models.Account, error)
}

// LangfuseProvisioner abstracts Langfuse project/org provisioning.
// ProvisionForDatapack ensures a langfuse_project_mapping row for the
// (account, datapack) pair. Best-effort: returns nil when Langfuse is
// disabled or upstream provisioning fails.
type LangfuseProvisioner interface {
	Enabled() bool
	ProvisionForDatapack(
		account *models.Account,
		datapackID, datapackName string,
	) *ProjectKeys
	GetOrgKeysForAccount(accountID string) (publicKey, secretKey string, err error)
	Host() string
}

// ProvisioningService orchestrates post-signup provisioning. In the
// multi-tenant model this is just three things:
//  1. Insert default datapack + default agent + agent_access link
//     atomically via DatapackCreator.
//  2. Provision Langfuse project keys (best-effort, fire-and-forget).
//
// CRD application and per-datapack DB provisioning have been removed.
type ProvisioningService struct {
	dpCreator    DatapackCreator
	accountStore AccountLookup
	langfuse     LangfuseProvisioner
}

// NewProvisioningService creates a new ProvisioningService.
func NewProvisioningService(
	dpCreator DatapackCreator,
	accountStore AccountLookup,
	langfuse LangfuseProvisioner,
) *ProvisioningService {
	return &ProvisioningService{
		dpCreator:    dpCreator,
		accountStore: accountStore,
		langfuse:     langfuse,
	}
}

// ProvisionNewAccount creates the default datapack (and its default
// agent) for the given user, then triggers Langfuse provisioning.
// Errors are logged but not returned (best-effort, fire-and-forget).
func (s *ProvisioningService) ProvisionNewAccount(ctx context.Context, userID string) {
	slog.Info("Starting account provisioning", "user_id", userID)

	dpResp, err := s.dpCreator.Create(CreateRequest{
		Name:   DefaultDatapackName,
		UserID: userID,
	})
	if err != nil {
		slog.Error("Failed to create default datapack",
			"user_id", userID,
			"datapack_name", DefaultDatapackName,
			"error", err,
		)
		return
	}

	datapackName := strings.TrimSpace(dpResp.Name)
	if datapackName == "" {
		datapackName = DefaultDatapackName
	}

	slog.Info("Default datapack created",
		"user_id", userID,
		"datapack_id", dpResp.DatapackID,
		"datapack_name", datapackName,
		"agent_id", dpResp.AgentID,
	)

	s.ensureLangfuseProvisioning(ctx, userID, dpResp.DatapackID, datapackName)

	slog.Info("Account provisioning completed",
		"user_id", userID,
		"datapack_id", dpResp.DatapackID,
	)
}

// langfuseProvisionResult carries the Langfuse credentials produced
// during provisioning.
type langfuseProvisionResult struct {
	SecretKey string
	PublicKey string
	BaseURL   string
}

func (s *ProvisioningService) ensureLangfuseProvisioning(
	ctx context.Context,
	userID string,
	datapackID string,
	datapackName string,
) *langfuseProvisionResult {
	if s.langfuse == nil || !s.langfuse.Enabled() {
		return nil
	}
	if s.accountStore == nil {
		slog.Warn("langfuse provisioning skipped: account store is nil",
			"user_id", userID,
		)
		return nil
	}
	acct, err := s.accountStore.GetByID(ctx, userID)
	if err != nil {
		slog.Error("Failed to load account for langfuse provisioning",
			"user_id", userID,
			"error", err,
		)
		return nil
	}
	if acct == nil {
		slog.Warn("langfuse provisioning skipped: account not found",
			"user_id", userID,
		)
		return nil
	}
	projectKeys := s.langfuse.ProvisionForDatapack(acct, datapackID, datapackName)
	if projectKeys == nil {
		// ProvisionForDatapack already logs upstream errors; nothing
		// further to do for the signup flow.
		return nil
	}
	s.logLangfuseProvisioningDetails(userID, datapackID, projectKeys)

	baseURL := strings.TrimSpace(s.langfuse.Host())
	if baseURL == "" {
		baseURL = "http://localhost:3000"
	}

	slog.Info("langfuse is provisioned for account",
		"user_id", userID,
		"datapack_id", datapackID,
	)
	return &langfuseProvisionResult{
		SecretKey: projectKeys.SecretKey,
		PublicKey: projectKeys.PublicKey,
		BaseURL:   baseURL,
	}
}

func (s *ProvisioningService) logLangfuseProvisioningDetails(
	userID string,
	datapackID string,
	projectKeys *ProjectKeys,
) {
	if projectKeys != nil {
		slog.Info("langfuse project keys created",
			"user_id", userID,
			"datapack_id", datapackID,
			"project_id", projectKeys.ProjectID,
			"project_name", projectKeys.ProjectName,
			"project_public_key", projectKeys.PublicKey,
			"project_secret_key", projectKeys.SecretKey,
		)
	}

	orgPublic, orgSecret, err := s.langfuse.GetOrgKeysForAccount(userID)
	if err != nil {
		slog.Warn("failed to fetch org keys after langfuse provisioning",
			"user_id", userID,
			"error", err,
		)
	} else {
		slog.Info("langfuse org keys available",
			"user_id", userID,
			"org_public_key", orgPublic,
			"org_secret_key", orgSecret,
		)
	}

	baseURL := strings.TrimSpace(s.langfuse.Host())
	if baseURL == "" {
		baseURL = "http://localhost:3000"
	}

	if projectKeys != nil {
		secretExport := fmt.Sprintf("export LANGFUSE_SECRET_KEY=%q", projectKeys.SecretKey)
		publicExport := fmt.Sprintf("export LANGFUSE_PUBLIC_KEY=%q", projectKeys.PublicKey)
		slog.Info(secretExport)
		slog.Info(publicExport)
		fmt.Println(secretExport)
		fmt.Println(publicExport)
	}
	baseURLExport := fmt.Sprintf("export LANGFUSE_BASE_URL=%q", baseURL)
	hostExport := fmt.Sprintf("export LANGFUSE_HOST=%q", baseURL)
	slog.Info(baseURLExport)
	slog.Info(hostExport)
	fmt.Println(baseURLExport)
	fmt.Println(hostExport)
}
