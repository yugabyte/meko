package service

import (
	"context"
	"fmt"
	"log/slog"
	"net/url"
	"strconv"
	"strings"

	"meko/api_server/internal/k8s"
	"meko/api_server/internal/repository"
)

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

// ProvisioningConfig holds the static configuration for account
// provisioning. Image URIs and replica counts come from the
// tier_config table at runtime.
type ProvisioningConfig struct {
	CRDNamespace string
}

// ProvisioningService orchestrates post-signup provisioning: creating a
// default datapack and applying a MekoUserAccount CRD.
type ProvisioningService struct {
	dpCreator  DatapackCreator
	crdCreator CRDCreator
	tierStore  TierConfigStore
	config     ProvisioningConfig
}

// NewProvisioningService creates a new ProvisioningService.
func NewProvisioningService(
	dpCreator DatapackCreator,
	crdCreator CRDCreator,
	tierStore TierConfigStore,
	config ProvisioningConfig,
) *ProvisioningService {
	return &ProvisioningService{
		dpCreator:  dpCreator,
		crdCreator: crdCreator,
		tierStore:  tierStore,
		config:     config,
	}
}

// ProvisionNewAccount creates a default datapack for the account and
// then applies a MekoUserAccount CRD to the cluster using the DB
// credentials from the datapack. Errors are logged but not returned
// (best-effort, fire-and-forget).
func (s *ProvisioningService) ProvisionNewAccount(ctx context.Context, accountID string) {
	slog.Info("Starting account provisioning", "account_id", accountID)

	const defaultDatapackName = "meko_default_datapack"

	// PostgreSQL limits identifiers to 63 bytes. The backing DB name
	// is "{accountID}_{datapackName}", so verify it fits.
	const maxYBIdentLen = 63
	dbIdentLen := len(accountID) + 1 + len(defaultDatapackName)
	if dbIdentLen > maxYBIdentLen {
		slog.Error("Default datapack's DB name too long",
			"account_id", accountID,
			"db_ident_len", dbIdentLen,
			"max", maxYBIdentLen,
		)
		return
	}

	dpResp, err := s.dpCreator.Create(CreateRequest{
		Name:      defaultDatapackName,
		AccountID: accountID,
	})
	if err != nil {
		slog.Error("Failed to create default datapack",
			"account_id", accountID,
			"datapack_name", defaultDatapackName,
			"error", err,
		)
		return
	}

	slog.Info("Default datapack created",
		"account_id", accountID,
		"datapack_id", dpResp.DatapackID,
		"datapack_name", defaultDatapackName,
	)

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
		LangfuseImage:  tierCfg.LangfuseImage,
		YugabyteDBHost: host,
		YugabyteDBPort: port,
		YugabyteDBUser: dpResp.MCPUserName,
		YugabyteDBPass: dpResp.MCPServer.APIKey,
		YugabyteDBName: dbName,
		MCPReplicas:    tierCfg.MCPReplicas,
		RAGReplicas:    tierCfg.RAGReplicas,
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
