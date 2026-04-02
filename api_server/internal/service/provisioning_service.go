package service

import (
	"context"
	"fmt"
	"log/slog"
	"net/url"
	"strconv"
	"strings"

	"meko/api_server/internal/k8s"
	"meko/api_server/internal/models"
	"meko/api_server/internal/repository"
)

const DefaultDatapackName = "meko_default_datapack"

// DatapackCreator abstracts the datapack creation operation, enabling
// test doubles.
type DatapackCreator interface {
	Create(req CreateRequest) (*CreateResponse, error)
}

// CRDCreator abstracts the Kubernetes CRD creation operation, enabling
// test doubles.
type CRDCreator interface {
	CreateMekoUserAccount(ctx context.Context, p k8s.MekoUserAccountParams) error
}

// TierConfigStore abstracts tier configuration lookups, enabling test
// doubles.
type TierConfigStore interface {
	GetByTier(ctx context.Context, tier string) (*repository.TierConfig, error)
}

// AccountLookup abstracts account profile lookups for provisioning.
type AccountLookup interface {
	GetByID(ctx context.Context, id string) (*models.Account, error)
}

// LangfuseProvisioner abstracts Langfuse project/org provisioning.
type LangfuseProvisioner interface {
	Enabled() bool
	GenerateProjectKeysForDatapack(
		accountID string,
		accountEmail string,
		firstName string,
		lastName string,
		datapackID string,
		datapackName string,
	) (*ProjectKeys, error)
	GetOrgKeysForAccount(accountID string) (publicKey, secretKey string, err error)
	Host() string
}

// ProvisioningConfig holds the static configuration for account
// provisioning. Image URIs and replica counts come from the
// tier_config table at runtime.
type ProvisioningConfig struct {
	CRDNamespace string
}

// ProvisioningService orchestrates post-signup provisioning: creating a
// default datapack and applying a MekoUserAccount CRD.
type ProvisioningService struct {
	dpCreator    DatapackCreator
	crdCreator   CRDCreator
	tierStore    TierConfigStore
	accountStore AccountLookup
	langfuse     LangfuseProvisioner
	config       ProvisioningConfig
}

// NewProvisioningService creates a new ProvisioningService.
func NewProvisioningService(
	dpCreator DatapackCreator,
	crdCreator CRDCreator,
	tierStore TierConfigStore,
	accountStore AccountLookup,
	langfuse LangfuseProvisioner,
	config ProvisioningConfig,
) *ProvisioningService {
	return &ProvisioningService{
		dpCreator:    dpCreator,
		crdCreator:   crdCreator,
		tierStore:    tierStore,
		accountStore: accountStore,
		langfuse:     langfuse,
		config:       config,
	}
}

// ProvisionNewAccount creates a default datapack for the account and
// then applies a MekoUserAccount CRD to the cluster using the DB
// credentials from the datapack. Errors are logged but not returned
// (best-effort, fire-and-forget).
func (s *ProvisioningService) ProvisionNewAccount(ctx context.Context, accountID string) {
	slog.Info("Starting account provisioning", "account_id", accountID)

	// PostgreSQL limits identifiers to 63 bytes. The backing DB name
	// is "{accountID}_{datapackName}", so verify it fits.
	const maxYBIdentLen = 63
	dbIdentLen := len(accountID) + 1 + len(DefaultDatapackName)
	if dbIdentLen > maxYBIdentLen {
		slog.Error("Default datapack's DB name too long",
			"account_id", accountID,
			"db_ident_len", dbIdentLen,
			"max", maxYBIdentLen,
		)
		return
	}

	dpResp, err := s.dpCreator.Create(CreateRequest{
		Name:      DefaultDatapackName,
		AccountID: accountID,
	})
	if err != nil {
		slog.Error("Failed to create default datapack",
			"account_id", accountID,
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
		"account_id", accountID,
		"datapack_id", dpResp.DatapackID,
		"datapack_name", datapackName,
	)

	lfResult := s.ensureLangfuseProvisioning(ctx, accountID, dpResp.DatapackID, datapackName)

	host, port, dbName, err := parseConnStr(dpResp.DBAccess.ConnStr)
	if err != nil {
		slog.Error("Failed to parse datapack connection string",
			"account_id", accountID,
			"conn_str", dpResp.DBAccess.ConnStr,
			"error", err,
		)
		return
	}

	const defaultTier = "free"
	tierCfg, err := s.tierStore.GetByTier(ctx, defaultTier)
	if err != nil {
		slog.Error("Failed to fetch tier config",
			"account_id", accountID,
			"tier", defaultTier,
			"error", err,
		)
		return
	}

	params := k8s.MekoUserAccountParams{
		AccountID:      accountID,
		Namespace:      s.config.CRDNamespace,
		AccountTier:    tierCfg.Tier,
		MCPImage:       tierCfg.MCPImage,
		RAGImage:       tierCfg.RAGImage,
		YugabyteDBHost: host,
		YugabyteDBPort: port,
		YugabyteDBUser: resolveYugabyteUser(dpResp),
		YugabyteDBPass: resolveYugabytePassword(dpResp),
		YugabyteDBName: dbName,
		MCPReplicas:    tierCfg.MCPReplicas,
		RAGReplicas:    tierCfg.RAGReplicas,
	}
	if lfResult != nil {
		params.LangfuseSecretKey = lfResult.SecretKey
		params.LangfusePublicKey = lfResult.PublicKey
		params.LangfuseBaseURL = lfResult.BaseURL
	}

	if err := s.crdCreator.CreateMekoUserAccount(ctx, params); err != nil {
		slog.Error("Failed to create MekoUserAccount CRD",
			"account_id", accountID,
			"error", err,
		)
		return
	}

	slog.Info("Account provisioning completed",
		"account_id", accountID,
		"datapack_id", dpResp.DatapackID,
	)
}

// langfuseProvisionResult carries the Langfuse credentials produced
// during provisioning so they can be forwarded to the CRD.
type langfuseProvisionResult struct {
	SecretKey string
	PublicKey string
	BaseURL   string
}

func (s *ProvisioningService) ensureLangfuseProvisioning(
	ctx context.Context,
	accountID string,
	datapackID string,
	datapackName string,
) *langfuseProvisionResult {
	if s.langfuse == nil || !s.langfuse.Enabled() {
		return nil
	}
	if s.accountStore == nil {
		slog.Warn("langfuse provisioning skipped: account store is nil",
			"account_id", accountID,
		)
		return nil
	}
	acct, err := s.accountStore.GetByID(ctx, accountID)
	if err != nil {
		slog.Error("Failed to load account for langfuse provisioning",
			"account_id", accountID,
			"error", err,
		)
		return nil
	}
	if acct == nil {
		slog.Warn("langfuse provisioning skipped: account not found",
			"account_id", accountID,
		)
		return nil
	}
	projectKeys, err := s.langfuse.GenerateProjectKeysForDatapack(
		acct.ID,
		acct.Email,
		acct.FirstName,
		acct.LastName,
		datapackID,
		datapackName,
	)
	if err != nil {
		slog.Error("Failed to provision langfuse mapping for default datapack",
			"account_id", accountID,
			"datapack_id", datapackID,
			"error", err,
		)
		return nil
	}
	s.logLangfuseProvisioningDetails(accountID, datapackID, projectKeys)

	baseURL := strings.TrimSpace(s.langfuse.Host())
	if baseURL == "" {
		baseURL = "http://localhost:3000"
	}

	slog.Info("langfuse is provisioned for account",
		"account_id", accountID,
		"datapack_id", datapackID,
	)
	return &langfuseProvisionResult{
		SecretKey: projectKeys.SecretKey,
		PublicKey: projectKeys.PublicKey,
		BaseURL:   baseURL,
	}
}

func (s *ProvisioningService) logLangfuseProvisioningDetails(
	accountID string,
	datapackID string,
	projectKeys *ProjectKeys,
) {
	if projectKeys != nil {
		slog.Info("langfuse project keys created",
			"account_id", accountID,
			"datapack_id", datapackID,
			"project_id", projectKeys.ProjectID,
			"project_name", projectKeys.ProjectName,
			"project_public_key", projectKeys.PublicKey,
			"project_secret_key", projectKeys.SecretKey,
		)
	}

	orgPublic, orgSecret, err := s.langfuse.GetOrgKeysForAccount(accountID)
	if err != nil {
		slog.Warn("failed to fetch org keys after langfuse provisioning",
			"account_id", accountID,
			"error", err,
		)
	} else {
		slog.Info("langfuse org keys available",
			"account_id", accountID,
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

// parseConnStr extracts host, port, and database name from a
// PostgreSQL URI (e.g. "postgresql://user:pass@host:5433/dbname").
func parseConnStr(connStr string) (host string, port int32, dbName string, err error) {
	u, err := url.Parse(connStr)
	if err != nil {
		return "", 0, "", fmt.Errorf("invalid connection string: %w", err)
	}

	host = u.Hostname()
	if host == "" {
		return "", 0, "", fmt.Errorf("missing host in connection string")
	}

	portStr := u.Port()
	if portStr == "" {
		port = 5433
	} else {
		p, err := strconv.ParseInt(portStr, 10, 32)
		if err != nil {
			return "", 0, "", fmt.Errorf("invalid port %q: %w", portStr, err)
		}
		port = int32(p)
	}

	dbName = strings.TrimPrefix(u.Path, "/")
	if dbName == "" {
		return "", 0, "", fmt.Errorf("missing database name in connection string")
	}

	return host, port, dbName, nil
}

func resolveYugabyteUser(dpResp *CreateResponse) string {
	if dpResp == nil {
		return ""
	}
	if user := strings.TrimSpace(dpResp.MCPUserName); user != "" {
		return user
	}
	return strings.TrimSpace(dpResp.DBAccess.UserName)
}

func resolveYugabytePassword(dpResp *CreateResponse) string {
	if dpResp == nil {
		return ""
	}
	if pass := strings.TrimSpace(dpResp.MCPServer.APIKey); pass != "" {
		return pass
	}
	return strings.TrimSpace(dpResp.DBAccess.Password)
}
